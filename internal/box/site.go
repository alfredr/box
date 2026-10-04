package box

import (
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"

	"go.yaml.in/yaml/v3"
)

// Policy determines how an image check handles updates for a service.
type Policy string

const (
	// Auto deploys changed images and allows a check to start a site that has never been deployed.
	Auto Policy = "auto"
	// Manual records a changed image for an explicit deployment.
	Manual Policy = "manual"
	// Pinned excludes the service from checks and explicit deployment pulls. Compose may still
	// start or recreate its container.
	Pinned Policy = "pinned"
)

// ParsePolicy accepts only "auto", "manual", or "pinned".
func ParsePolicy(s string) (Policy, error) {
	switch p := Policy(s); p {
	case Auto, Manual, Pinned:
		return p, nil
	}

	return "", fmt.Errorf("update policy %q: want auto, manual or pinned", s)
}

// Service contains the image reference, update policy, and retention overrides read from a
// Compose service definition. Use negative Keep and Budget values to inherit server settings.
type Service struct {
	Name   string
	Image  string
	Policy Policy
	// Keep limits earlier images. A negative value inherits the server setting.
	Keep int
	// Budget limits estimated image storage in bytes. A negative value inherits the server
	// setting, and zero disables the byte limit.
	Budget int64
}

// Site contains the Compose metadata used to manage one site. Its file is stored at
// sites/<name>/compose.yml under Root.
type Site struct {
	Name string
	// Services is sorted by service name when populated by ParseSite.
	Services []Service
	// Domains contains distinct values from caddy and caddy_<number> labels, including redirect
	// domains.
	Domains []string
}

var (
	// ErrNoSite identifies a missing site Compose file. LoadSite wraps it with the requested
	// name.
	ErrNoSite = errors.New("no such site")

	siteNamePattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,62}$`)
	domainPattern   = regexp.MustCompile(`^(?:[a-z0-9](?:[a-z0-9-]{0,61}[a-z0-9])?\.)+[a-z][a-z0-9-]{0,62}$`)
	caddyKey        = regexp.MustCompile(`^caddy(?:_\d+)?$`)
)

type invalidError struct{ err error }

// Error returns the underlying validation error message.
func (e *invalidError) Error() string { return e.err.Error() }

// Unwrap exposes the underlying validation error.
func (e *invalidError) Unwrap() error { return e.err }

// Invalid marks an error as a validation failure for conversion to an invalid-argument API
// response.
func Invalid(err error) error { return &invalidError{err} }

// IsInvalid reports whether err or an error it wraps was marked by Invalid.
func IsInvalid(err error) bool {
	var e *invalidError
	return errors.As(err, &e)
}

// ValidName accepts 1 to 63 lowercase letters, digits, or hyphens. The first character must be
// a letter or digit.
func ValidName(name string) error {
	if !siteNamePattern.MatchString(name) {
		return Invalid(fmt.Errorf("site name %q: use lowercase letters, digits and dashes", name))
	}

	return nil
}

func validDomain(d string) error {
	if !domainPattern.MatchString(d) {
		return Invalid(fmt.Errorf("domain %q: want a lowercase name like example.com", d))
	}

	return nil
}

// ComposePath returns the site Compose file path under Root. It does not validate name.
func ComposePath(name string) string { return filepath.Join(siteDir(name), "compose.yml") }

// composeFile reads only the service fields and extension data used by box. Docker Compose
// validates the rest of an edited file.
type composeFile struct {
	Box struct {
		Update map[string]string `yaml:"update"`
		Keep   map[string]string `yaml:"keep"`
		Budget map[string]string `yaml:"budget"`
	} `yaml:"x-box"`
	Services map[string]struct {
		Image  string    `yaml:"image"`
		Labels yaml.Node `yaml:"labels"`
	} `yaml:"services"`
}

// ParseSite reads service images, update policies, retention limits, and Caddy domains from
// Compose YAML. Image references must be literal. Services without a policy use Manual, and
// omitted limits inherit server settings. It does not perform full Compose validation or
// validate name.
func ParseSite(name string, data []byte) (Site, error) {
	site, err := parseSite(name, data)
	if err != nil {
		return site, Invalid(err)
	}

	return site, nil
}

func parseSite(name string, data []byte) (Site, error) {
	site := Site{Name: name}
	var f composeFile
	if err := yaml.Unmarshal(data, &f); err != nil {
		return site, fmt.Errorf("%s: %w", name, err)
	}

	if len(f.Services) == 0 {
		return site, fmt.Errorf("%s: no services", name)
	}

	for block, m := range map[string]map[string]string{"update": f.Box.Update, "keep": f.Box.Keep, "budget": f.Box.Budget} {
		for svc := range m {
			if _, ok := f.Services[svc]; !ok {
				return site, fmt.Errorf("%s: x-box %s names %q, which isn't a service", name, block, svc)
			}
		}
	}

	for _, svc := range slices.Sorted(maps.Keys(f.Services)) {
		def := f.Services[svc]
		if def.Image == "" {
			return site, fmt.Errorf("%s: service %s has no image (box runs images, it doesn't build them)", name, svc)
		}

		if strings.Contains(def.Image, "$") {
			return site, fmt.Errorf("%s: service %s: box reads image names as written, without ${...}", name, svc)
		}

		policy := Manual
		if p, ok := f.Box.Update[svc]; ok {
			var err error
			if policy, err = ParsePolicy(p); err != nil {
				return site, fmt.Errorf("%s: service %s: %w", name, svc, err)
			}
		}

		service := Service{Name: svc, Image: def.Image, Policy: policy, Keep: -1, Budget: -1}
		var err error
		if v, ok := f.Box.Keep[svc]; ok {
			if service.Keep, err = parseKeep(v); err != nil {
				return site, fmt.Errorf("%s: service %s: %w", name, svc, err)
			}
		}

		if v, ok := f.Box.Budget[svc]; ok {
			if service.Budget, err = parseBudget(v); err != nil {
				return site, fmt.Errorf("%s: service %s: budget: %w", name, svc, err)
			}
		}

		site.Services = append(site.Services, service)

		for _, kv := range labels(&def.Labels) {
			if !caddyKey.MatchString(kv[0]) {
				continue
			}

			for _, d := range strings.FieldsFunc(kv[1], func(r rune) bool { return r == ',' || r == ' ' }) {
				if !slices.Contains(site.Domains, d) {
					site.Domains = append(site.Domains, d)
				}
			}
		}
	}

	return site, nil
}

// labels accepts both Compose label formats: a mapping or a list of key=value strings.
func labels(n *yaml.Node) [][2]string {
	var pairs [][2]string
	switch n.Kind {
	case yaml.MappingNode:
		for i := 0; i+1 < len(n.Content); i += 2 {
			pairs = append(pairs, [2]string{n.Content[i].Value, n.Content[i+1].Value})
		}
	case yaml.SequenceNode:
		for _, item := range n.Content {
			k, v, _ := strings.Cut(item.Value, "=")
			pairs = append(pairs, [2]string{k, v})
		}
	}

	return pairs
}

// LoadSite validates name and reads its Compose file. A missing file returns an error wrapping
// ErrNoSite.
func LoadSite(name string) (Site, error) {
	if err := ValidName(name); err != nil {
		return Site{Name: name}, err
	}

	data, err := os.ReadFile(ComposePath(name))
	if errors.Is(err, os.ErrNotExist) {
		return Site{Name: name}, fmt.Errorf("%w: %s (box status lists them)", ErrNoSite, name)
	}

	if err != nil {
		return Site{Name: name}, err
	}

	return ParseSite(name, data)
}

// ListSites returns sorted names of valid site directories that contain compose.yml. A missing
// sites directory returns an empty list.
func ListSites() ([]string, error) {
	entries, err := os.ReadDir(sitesDir())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	if err != nil {
		return nil, err
	}

	var names []string
	for _, e := range entries {
		if !e.IsDir() || ValidName(e.Name()) != nil {
			continue
		}

		if _, err := os.Stat(ComposePath(e.Name())); err == nil {
			names = append(names, e.Name())
		}
	}

	return names, nil
}

// pick returns all services when names is empty. Otherwise it preserves the requested order and
// rejects unknown service names.
func (s Site) pick(names []string) ([]Service, error) {
	if len(names) == 0 {
		return s.Services, nil
	}

	var out []Service
	for _, n := range names {
		i := slices.IndexFunc(s.Services, func(svc Service) bool { return svc.Name == n })
		if i < 0 {
			return nil, Invalid(fmt.Errorf("%s has no service %q", s.Name, n))
		}

		out = append(out, s.Services[i])
	}

	return out, nil
}

func serviceNames(svcs []Service) []string {
	names := make([]string, len(svcs))
	for i, s := range svcs {
		names[i] = s.Name
	}

	return names
}
