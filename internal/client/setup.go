package client

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"path"
	"time"

	"github.com/alfredr/box/internal/box"
	"github.com/alfredr/box/internal/engine"
)

const (
	// RemoteRoot is the host directory used by the standard server installation.
	RemoteRoot = "/srv/box"
	// DefaultImage is the server image selected by setup unless overridden.
	DefaultImage = "ghcr.io/alfredr/box:latest"
	// ServerListen is the default loopback address forwarded by Caddy to the command API.
	ServerListen = "127.0.0.1:9311"
	// ServerName is the container name used by setup and server maintenance commands.
	ServerName      = "box"
	imageStoreMount = "/box-image-store"
)

// Server defines the box container and the host resources it uses.
type Server struct {
	Name  string
	Image string
	// Root is the host storage directory. It is mounted at the same path inside the container
	// so Compose bind mounts resolve on the host.
	Root string
	// Listen is the host:port address used by the box HTTP server inside the container.
	Listen string
	// DaemonDir is the host Docker configuration directory. The server mounts it read-only at
	// /etc/docker, while installation uses a separate writable mount. An empty value omits
	// these mounts and disables daemon configuration during installation.
	DaemonDir string
	// ImageStore is the daemon host's image storage directory. Installation mounts it
	// read-only to measure free space. An empty value preserves an existing mount or detects
	// the path for classic filesystem drivers. Containerd storage requires an explicit path.
	ImageStore string
	// Publish uses bridge networking and publishes the Listen port on a free host loopback
	// port. Otherwise the container uses host networking. Local Docker Desktop tests use this
	// to reach the server outside the VM.
	Publish bool
	// Env appends environment entries after the standard BOX_ROOT, BOX_LISTEN, and
	// DOCKER_CONFIG entries.
	Env []string
}

// DefaultServer returns the standard host-networked server configuration for image, using
// /srv/box and the host /etc/docker directory.
func DefaultServer(image string) Server {
	return Server{Name: ServerName, Image: image, Root: RemoteRoot, Listen: ServerListen, DaemonDir: "/etc/docker"}
}

func (s Server) container() engine.Container {
	c := engine.Container{
		Name:  s.Name,
		Image: s.Image,
		Cmd:   []string{"serve"},
		Env: append([]string{
			"BOX_ROOT=" + s.Root,
			"BOX_LISTEN=" + s.Listen,
			"DOCKER_CONFIG=" + path.Join(s.Root, "docker"),
		}, s.Env...),
		Labels: map[string]string{"box.role": "server"},
		Binds: []string{
			"/var/run/docker.sock:/var/run/docker.sock",
			s.Root + ":" + s.Root,
		},
		// Host networking lets Caddy reach the command API through loopback.
		NetworkMode:   "host",
		RestartPolicy: "unless-stopped",
		Healthcheck: &engine.Healthcheck{
			Test:          []string{"CMD", "box", "health"},
			Interval:      30 * time.Second,
			StartPeriod:   60 * time.Second,
			StartInterval: 2 * time.Second,
		},
		// Allow time for background checks to finish before Docker forces the server to stop.
		StopTimeout: 5 * time.Minute,
	}
	if s.DaemonDir != "" {
		c.Binds = append(c.Binds, s.DaemonDir+":/etc/docker:ro")
	}

	if s.ImageStore != "" {
		c.ReadOnlyBinds = map[string]string{imageStoreMount: s.ImageStore}
		c.Env = append(c.Env, "BOX_IMAGE_STORE="+imageStoreMount)
	}

	if s.Publish {
		c.NetworkMode = ""
		_, port, _ := cutLast(s.Listen, ":")
		c.Publish = map[string]string{port + "/tcp": "127.0.0.1:"}
	}

	return c
}

func cutLast(s, sep string) (string, string, bool) {
	for i := len(s) - len(sep); i >= 0; i-- {
		if s[i:i+len(sep)] == sep {
			return s[:i], s[i+len(sep):], true
		}
	}

	return s, "", false
}

// InstallOptions controls file installation, optional daemon configuration, and replacement of
// the box server container.
type InstallOptions struct {
	Server Server
	Domain string
	Email  string
	// Key is written as the server API key. An empty value generates a new key rather than
	// reading an existing key from the host.
	Key string
	// Pull fetches the server image before installation. If false, the image must already be
	// available on the daemon.
	Pull bool
	// Auth authenticates the image pull and replaces the server Docker config with this single
	// registry login. Nil preserves the existing config file.
	Auth *engine.Auth
	// Daemon contains settings to merge into daemon.json. A zero value or an empty
	// Server.DaemonDir skips this step.
	Daemon box.DaemonSettings
	// RestartDocker is called only when daemon settings change. Nil leaves restarting to the
	// caller.
	RestartDocker func(context.Context) error
	// Log receives progress messages and must not be nil.
	Log func(string)
}

// Install writes the server settings and API key, applies requested daemon settings, and
// replaces the server container. It preserves site files and existing settings except Domain
// and a supplied Email. It returns the installed key even if starting the replacement container
// fails.
func Install(ctx context.Context, eng *engine.Client, o InstallOptions) (string, error) {
	s := o.Server
	if o.Pull {
		o.Log("pulling " + s.Image)
		if err := eng.Pull(ctx, s.Image, o.Auth); err != nil {
			return "", err
		}
	}

	// Use an unstarted helper container to read and write host files through Docker bind
	// mounts.
	daemon := o.Daemon != (box.DaemonSettings{}) && s.DaemonDir != ""
	binds := []string{s.Root + ":" + s.Root}
	if daemon {
		binds = append(binds, s.DaemonDir+":/host/etc/docker")
	}

	helper, err := eng.Create(ctx, engine.Container{Name: s.Name + "-setup", Image: s.Image, Binds: binds})
	if err != nil {
		return "", err
	}

	defer eng.Remove(context.WithoutCancel(ctx), helper, true)

	key, err := writeFiles(ctx, eng, helper, o)
	if err != nil {
		return "", err
	}

	if daemon {
		changed, err := mergeDaemon(ctx, eng, helper, "/host/etc/docker", o.Daemon)
		if err != nil {
			return "", err
		}

		if changed && o.RestartDocker != nil {
			o.Log("Docker settings changed, restarting Docker")
			if err := o.RestartDocker(ctx); err != nil {
				return "", err
			}

			if err := waitFor(ctx, 60*time.Second, func() error { return eng.Ping(ctx) }); err != nil {
				return "", fmt.Errorf("Docker didn't come back after restarting: %w", err)
			}
		}
	}

	o.Log("starting the server")
	return key, replace(ctx, eng, s)
}

// writeFiles preserves existing settings while replacing the domain, any supplied email, and
// the API key. Registry credentials are replaced only when Auth is provided.
func writeFiles(ctx context.Context, eng *engine.Client, helper string, o InstallOptions) (string, error) {
	root := o.Server.Root
	var cfg box.Config
	current, err := eng.GetFile(ctx, helper, path.Join(root, "box.json"))
	switch {
	case err == nil:
		if err := json.Unmarshal(current, &cfg); err != nil {
			return "", fmt.Errorf("the server's box.json: %w", err)
		}
	case !engine.IsNotFound(err):
		return "", err
	}

	if err := cfg.Set("domain", o.Domain); err != nil {
		return "", err
	}

	if o.Email != "" {
		if err := cfg.Set("email", o.Email); err != nil {
			return "", err
		}
	}

	cfgJSON, err := json.MarshalIndent(cfg, "", "  ")
	if err != nil {
		return "", err
	}

	key := o.Key
	if key == "" {
		key = box.NewSecret()
	}

	files := []engine.File{
		{Name: "secrets", Dir: true, Mode: 0o700},
		{Name: "secrets/api.key", Data: []byte(key + "\n"), Mode: 0o600},
		{Name: "box.json", Data: append(cfgJSON, '\n'), Mode: 0o600},
		{Name: "docker", Dir: true, Mode: 0o700},
	}

	if o.Auth != nil {
		login, err := dockerConfig(o.Auth)
		if err != nil {
			return "", err
		}

		files = append(files, engine.File{Name: "docker/config.json", Data: login, Mode: 0o600})
	}

	o.Log("writing " + root + " (settings and API key)")
	return key, eng.PutFiles(ctx, helper, root, files)
}

// dockerConfig encodes a Docker CLI config containing only the supplied registry login.
func dockerConfig(a *engine.Auth) ([]byte, error) {
	auth := base64.StdEncoding.EncodeToString([]byte(a.Username + ":" + a.Password))
	return json.MarshalIndent(map[string]any{"auths": map[string]any{a.ServerAddress: map[string]string{"auth": auth}}}, "", "  ")
}

// mergeDaemon reads and updates daemon.json through the helper container. It reports whether a
// write was needed.
func mergeDaemon(ctx context.Context, eng *engine.Client, helper, dir string, s box.DaemonSettings) (bool, error) {
	current, err := eng.GetFile(ctx, helper, path.Join(dir, "daemon.json"))
	if err != nil && !engine.IsNotFound(err) {
		return false, err
	}

	next, changed, err := box.MergeDaemon(current, s)
	if err != nil || !changed {
		return false, err
	}

	return true, eng.PutFiles(ctx, helper, dir, []engine.File{{Name: "daemon.json", Data: next, Mode: 0o644}})
}

// replace stops and removes the existing container before creating its replacement. It waits up
// to two minutes for health status, and does not restore the old container if replacement
// fails. The image-storage path is preserved or detected and validated before the old server
// is stopped.
func replace(ctx context.Context, eng *engine.Client, s Server) error {
	old, err := eng.Inspect(ctx, s.Name)
	if err != nil && !engine.IsNotFound(err) {
		return err
	}

	exists := err == nil
	if s.ImageStore == "" {
		s.ImageStore = old.Mounts[imageStoreMount]
	}

	if s.ImageStore == "" {
		s.ImageStore, err = eng.ImageStore(ctx)
		if err != nil {
			return err
		}
	}

	if s.ImageStore != "" {
		if !path.IsAbs(s.ImageStore) {
			return fmt.Errorf("image storage must be an absolute host path: %q", s.ImageStore)
		}

		// Validate the host path through Docker before stopping a working server. Structured
		// bind mounts reject missing paths rather than creating an empty directory.
		probe, err := eng.Create(ctx, engine.Container{
			Name: s.Name + "-storage-" + box.NewSecret()[:8], Image: s.Image,
			ReadOnlyBinds: map[string]string{imageStoreMount: s.ImageStore},
		})
		if err != nil {
			return fmt.Errorf("mounting image storage: %w", err)
		}

		defer eng.Remove(context.WithoutCancel(ctx), probe, true)
	}

	spec := s.container()
	if exists {
		if err := eng.Stop(ctx, s.Name, spec.StopTimeout); err != nil {
			return err
		}

		if err := eng.Remove(ctx, s.Name, true); err != nil {
			return err
		}
	}

	id, err := eng.Create(ctx, spec)
	if err != nil {
		return err
	}

	if err := eng.Start(ctx, id); err != nil {
		return err
	}

	return waitFor(ctx, 2*time.Minute, func() error {
		st, err := eng.Inspect(ctx, id)
		switch {
		case err != nil:
			return err
		case st.Health == "healthy":
			return nil
		case st.Status != "running":
			return stop{fmt.Errorf("the server container is %s (box server logs shows why)", st.Status)}
		}

		return fmt.Errorf("the server isn't healthy yet (%s)", st.Health)
	})
}

// stop marks an error that should end waitFor without another retry.
type stop struct{ error }

// waitFor calls f immediately, then retries every 500 milliseconds until success, a stop error,
// cancellation, or the time limit. The limit does not interrupt a call to f that is still
// running.
func waitFor(ctx context.Context, limit time.Duration, f func() error) error {
	deadline := time.Now().Add(limit)
	for {
		err := f()
		var s stop
		switch {
		case err == nil:
			return nil
		case errors.As(err, &s):
			return s.error
		case time.Now().After(deadline):
			return err
		}

		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(500 * time.Millisecond):
		}
	}
}

// SetupOptions adds SSH access and Docker installation choices to InstallOptions.
type SetupOptions struct {
	InstallOptions
	// Target is passed to the system SSH client as the destination.
	Target string
	// InstallDocker permits an installation attempt without confirmation when the forwarded
	// Docker socket cannot be reached.
	InstallDocker bool
	// Confirm authorizes a Docker installation attempt. It must be non-nil when InstallDocker
	// is false and the daemon is unreachable.
	Confirm func(question string) bool
}

// Setup connects to Docker over SSH, offers installation if the socket is unavailable, and
// installs the box server. It returns an HTTPS profile without saving it or waiting for the
// public endpoint to become reachable.
func Setup(ctx context.Context, o SetupOptions) (Profile, error) {
	host := Host{Target: o.Target}
	o.Log("connecting to " + o.Target)
	eng, closeFn, err := host.Docker(ctx)
	if errors.Is(err, ErrNoDocker) {
		if !o.InstallDocker && !o.Confirm(err.Error()+"\nInstall Docker from Docker's package repository with the system's package manager?") {
			return Profile{}, err
		}

		o.Log("installing Docker")
		if err := host.InstallDocker(ctx); err != nil {
			return Profile{}, err
		}

		eng, closeFn, err = host.Docker(ctx)
	}

	if err != nil {
		return Profile{}, fmt.Errorf("%w (box needs root or a login in the docker group)", err)
	}

	defer closeFn()

	version, err := eng.Version(ctx)
	if err != nil {
		return Profile{}, err
	}

	if !version.Supported() {
		return Profile{}, fmt.Errorf("%s has Docker %s (API %s), and box needs Docker %s or later", o.Target, version.Version, version.APIVersion, engine.MinimumDocker)
	}

	o.Log("docker " + version.Version)
	o.RestartDocker = host.RestartDocker
	key, err := Install(ctx, eng, o.InstallOptions)
	if err != nil {
		return Profile{}, err
	}

	return Profile{URL: "https://" + o.Domain, Key: key, SSH: o.Target}, nil
}

// Upgrade pulls image and replaces the standard box server container. An empty image uses the
// existing container image reference. Auth is used only for this pull and is not saved to the
// server registry configuration.
func Upgrade(ctx context.Context, eng *engine.Client, image string, auth *engine.Auth) error {
	if image == "" {
		st, err := eng.Inspect(ctx, ServerName)
		if err != nil {
			return err
		}

		image = st.Image
	}

	if err := eng.Pull(ctx, image, auth); err != nil {
		return err
	}

	return replace(ctx, eng, DefaultServer(image))
}

// ConfigureDaemon merges settings into the host /etc/docker/daemon.json through a temporary
// container. It reports whether the file changed and does not restart Docker.
func ConfigureDaemon(ctx context.Context, eng *engine.Client, s box.DaemonSettings) (bool, error) {
	st, err := eng.Inspect(ctx, ServerName)
	if err != nil {
		return false, err
	}

	helper, err := eng.Create(ctx, engine.Container{
		Name:  ServerName + "-docker-config",
		Image: st.Image,
		Binds: []string{"/etc/docker:/host/etc/docker"},
	})
	if err != nil {
		return false, err
	}

	defer eng.Remove(context.WithoutCancel(ctx), helper, true)

	return mergeDaemon(ctx, eng, helper, "/host/etc/docker", s)
}
