package box

import (
	"errors"
	"fmt"
	"os"
	"regexp"
)

// NewSite supplies the values for a Compose project with one app service behind Caddy.
type NewSite struct {
	Name  string
	Image string
	// Domains lists the names served by the app. At least one is required, and the first is the
	// redirect destination.
	Domains []string
	// Redirects lists domains that receive permanent redirects to the first entry in Domains.
	Redirects []string
	// Port is the app container port Caddy connects to. It must be between 1 and 65535.
	Port   int
	Policy Policy
}

var imagePattern = regexp.MustCompile(`^[a-z0-9][A-Za-z0-9._/:@-]*$`)

// Compose validates n and renders the site template. Validation failures are marked with
// Invalid. It does not write files or start containers.
func (n NewSite) Compose() ([]byte, error) {
	data, err := n.compose()
	if err != nil {
		return nil, Invalid(err)
	}

	return data, nil
}

func (n NewSite) compose() ([]byte, error) {
	if err := ValidName(n.Name); err != nil {
		return nil, err
	}

	if !imagePattern.MatchString(n.Image) {
		return nil, fmt.Errorf("image %q: want a reference like ghcr.io/you/app:latest", n.Image)
	}

	if len(n.Domains) == 0 {
		return nil, errors.New("a site needs at least one --domain")
	}

	for _, d := range append(append([]string{}, n.Domains...), n.Redirects...) {
		if err := validDomain(d); err != nil {
			return nil, err
		}
	}

	if n.Port < 1 || n.Port > 65535 {
		return nil, fmt.Errorf("port %d: want 1 to 65535", n.Port)
	}

	if _, err := ParsePolicy(string(n.Policy)); err != nil {
		return nil, err
	}

	return render("site.compose.yml.tmpl", struct {
		NewSite
		Network string
	}{n, Network})
}

// CreateSite writes a new Compose file and ensures the site has a webhook secret. It rejects an
// existing Compose file and does not deploy the site. If secret creation fails, the Compose
// file remains on disk.
func CreateSite(n NewSite) error {
	data, err := n.Compose()
	if err != nil {
		return err
	}

	if _, err := ParseSite(n.Name, data); err != nil {
		return fmt.Errorf("generated compose file doesn't parse: %w", err)
	}

	if _, err := os.Stat(ComposePath(n.Name)); err == nil {
		return Invalid(fmt.Errorf("site %s exists (box edit %s changes it)", n.Name, n.Name))
	}

	if err := writeFileAtomic(ComposePath(n.Name), data, 0o644); err != nil {
		return err
	}

	_, err = EnsureHookSecret(n.Name)
	return err
}
