package box

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
)

const (
	// ProxyImage is the Caddy image that discovers routes from Docker labels.
	ProxyImage = "lucaslorentz/caddy-docker-proxy:2.13-alpine"
	// Network is the shared Docker network used to reach site containers.
	Network      = "web"
	proxyProject = "box-proxy"
)

func proxyComposePath() string { return filepath.Join(proxyDir(), "compose.yml") }
func caddyfilePath() string    { return filepath.Join(proxyDir(), "caddy", "Caddyfile") }

// Caddyfile renders the ACME email and command API route. The listen argument is the upstream
// host:port reachable from the proxy. Site routes are supplied separately by container labels.
func Caddyfile(cfg Config, listen string) ([]byte, error) {
	return render("Caddyfile.tmpl", struct {
		Email, Domain, Listen string
	}{cfg.Email, cfg.Domain, listen})
}

// WriteProxy replaces the proxy Compose file and base Caddyfile under Root. It does not start
// or restart the proxy.
func WriteProxy(cfg Config, listen string) error {
	compose, err := render("proxy.compose.yml.tmpl", struct {
		Project, Image, Network string
	}{proxyProject, ProxyImage, Network})
	if err != nil {
		return err
	}

	caddyfile, err := Caddyfile(cfg, listen)
	if err != nil {
		return err
	}

	if err := writeFileAtomic(proxyComposePath(), compose, 0o644); err != nil {
		return err
	}

	return writeFileAtomic(caddyfilePath(), caddyfile, 0o644)
}

// ProxyUp applies the proxy Compose file and waits for its container to start.
func ProxyUp(ctx context.Context) error {
	_, err := docker(ctx, composeArgs(proxyProject, proxyComposePath(), "up", "--detach", "--wait")...)
	return err
}

// ProxyState returns the first proxy container state, "not created" when absent, or an error
// description if inspection fails.
func ProxyState(ctx context.Context) string {
	cs, err := projectContainers(ctx, proxyProject, proxyComposePath())
	if err != nil {
		return "unknown (" + err.Error() + ")"
	}

	if len(cs) == 0 {
		return "not created"
	}

	return cs[0].State
}

// Prepare creates storage directories, ensures the shared network exists, and writes proxy
// configuration. It also starts the proxy when proxy is true.
func Prepare(ctx context.Context, cfg Config, listen string, proxy bool) error {
	for _, d := range []string{sitesDir(), stateDir(), proxyDir()} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			return err
		}
	}

	if err := os.MkdirAll(secretsDir(), 0o700); err != nil {
		return err
	}

	if err := EnsureNetwork(ctx); err != nil {
		return err
	}

	if err := WriteProxy(cfg, listen); err != nil {
		return err
	}

	if proxy {
		if err := ProxyUp(ctx); err != nil {
			return fmt.Errorf("starting the proxy: %w", err)
		}
	}

	if pruneEnabled() {
		dropStaleProxies(ctx)
		tidy(ctx)
	}

	return nil
}
