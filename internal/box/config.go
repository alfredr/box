// Package box manages Compose sites, deployment records, and proxy configuration on one Docker
// host.
package box

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/mail"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"time"
)

// Root returns BOX_ROOT when set, or /srv/box otherwise. The server container and Docker host
// must use the same path so Compose bind mounts resolve correctly.
func Root() string {
	if r := os.Getenv("BOX_ROOT"); r != "" {
		return r
	}

	return "/srv/box"
}

func configPath() string         { return filepath.Join(Root(), "box.json") }
func sitesDir() string           { return filepath.Join(Root(), "sites") }
func siteDir(name string) string { return filepath.Join(sitesDir(), name) }
func secretsDir() string         { return filepath.Join(Root(), "secrets") }
func stateDir() string           { return filepath.Join(Root(), "state") }
func proxyDir() string           { return filepath.Join(Root(), "proxy") }
func removedDir() string         { return filepath.Join(Root(), "removed") }

// Config holds the server settings stored in box.json under Root.
type Config struct {
	// Domain is the DNS name used for the command API and site webhooks.
	Domain string `json:"domain,omitempty"`
	// Email is the ACME account address. An empty value omits it from the Caddy configuration.
	Email string `json:"email,omitempty"`
	// Notify is the HTTP endpoint for plain-text notifications. An empty value disables
	// delivery.
	Notify string `json:"notify,omitempty"`
	// Poll is a Go duration string or "off". An empty value uses DefaultPoll.
	Poll   string `json:"poll,omitempty"`
	Keep   string `json:"keep,omitempty"`
	Budget string `json:"budget,omitempty"`
}

// ConfigKeys lists supported setting names in CLI display order.
var ConfigKeys = []string{"domain", "email", "notify", "poll", "keep", "budget"}

// DefaultPoll is the interval used when no polling schedule is configured.
const DefaultPoll = "5m"

// LoadConfig reads box.json under Root. A missing file returns the zero Config without an
// error.
func LoadConfig() (Config, error) {
	var c Config
	b, err := os.ReadFile(configPath())
	if errors.Is(err, os.ErrNotExist) {
		return c, nil
	}

	if err != nil {
		return c, err
	}

	if err := json.Unmarshal(b, &c); err != nil {
		return c, fmt.Errorf("%s: %w", configPath(), err)
	}

	return c, nil
}

// Save replaces box.json with c encoded as JSON and sets the file mode to 0600.
func (c Config) Save() error {
	b, err := json.MarshalIndent(c, "", "  ")
	if err != nil {
		return err
	}

	return writeFileAtomic(configPath(), append(b, '\n'), 0o600)
}

// PollEvery parses the polling interval. It returns zero for "off" and five minutes when the
// value cannot be parsed.
func (c Config) PollEvery() time.Duration {
	if c.Poll == "off" {
		return 0
	}

	if d, err := time.ParseDuration(c.Poll); err == nil {
		return d
	}

	return 5 * time.Minute
}

// HookURL returns the HTTPS webhook URL for site. It fails if Domain is empty and does not
// validate the site name.
func (c Config) HookURL(site string) (string, error) {
	if c.Domain == "" {
		return "", errors.New("the server has no domain yet: box config domain box.example.com")
	}

	return "https://" + c.Domain + "/deploy/" + site, nil
}

// Get returns a setting by name. Reading an unset poll setting returns DefaultPoll.
func (c Config) Get(key string) (string, error) {
	switch key {
	case "domain":
		return c.Domain, nil
	case "email":
		return c.Email, nil
	case "notify":
		return c.Notify, nil
	case "poll":
		if c.Poll == "" {
			return DefaultPoll, nil
		}

		return c.Poll, nil
	case "keep":
		if c.Keep == "" {
			return strconv.Itoa(DefaultKeep), nil
		}

		return c.Keep, nil
	case "budget":
		if c.Budget == "" {
			return "off", nil
		}

		return c.Budget, nil
	}

	return "", Invalid(fmt.Errorf("unknown setting %q (one of %v)", key, ConfigKeys))
}

// Set validates and changes one setting in memory. It does not save the configuration. Empty
// values clear email and notify. Poll intervals must be at least one minute unless polling is
// off.
func (c *Config) Set(key, value string) error {
	switch key {
	case "domain":
		if err := validDomain(value); err != nil {
			return err
		}

		c.Domain = value
	case "email":
		if value != "" {
			if _, err := mail.ParseAddress(value); err != nil {
				return Invalid(fmt.Errorf("email: %w", err))
			}
		}

		c.Email = value
	case "notify":
		if value != "" {
			u, err := url.Parse(value)
			if err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
				return Invalid(fmt.Errorf("notify: want an http(s) URL, got %q", value))
			}
		}

		c.Notify = value
	case "poll":
		if value != "off" {
			d, err := time.ParseDuration(value)
			if err != nil || d < time.Minute {
				return Invalid(fmt.Errorf("poll: want a duration of at least 1m, or off, got %q", value))
			}
		}

		c.Poll = value
	case "keep":
		if _, err := parseKeep(value); err != nil {
			return Invalid(err)
		}

		c.Keep = value
	case "budget":
		if _, err := parseBudget(value); err != nil {
			return Invalid(fmt.Errorf("budget: %w", err))
		}

		c.Budget = value
	default:
		return Invalid(fmt.Errorf("unknown setting %q (one of %v)", key, ConfigKeys))
	}

	return nil
}

// writeFileAtomic writes a temporary file beside path and renames it over the destination. The
// temporary file uses perm, and missing parent directories are created with mode 0755.
func writeFileAtomic(path string, data []byte, perm os.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}

	tmp, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".*")
	if err != nil {
		return err
	}

	defer os.Remove(tmp.Name())

	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}

	if err := tmp.Chmod(perm); err != nil {
		tmp.Close()
		return err
	}

	if err := tmp.Close(); err != nil {
		return err
	}

	return os.Rename(tmp.Name(), path)
}
