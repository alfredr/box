package box

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
)

// docker captures stdout separately from stderr so command output can be parsed. On failure,
// stderr is included in the returned error.
func docker(ctx context.Context, args ...string) (string, error) {
	cmd := exec.CommandContext(ctx, "docker", args...)
	var out, errb bytes.Buffer
	cmd.Stdout = &out
	cmd.Stderr = &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}

		return out.String(), fmt.Errorf("docker %s: %s", strings.Join(args, " "), msg)
	}

	return out.String(), nil
}

func composeArgs(project, file string, args ...string) []string {
	return append([]string{"compose", "-p", project, "-f", file}, args...)
}

func siteArgs(name string, args ...string) []string {
	return composeArgs(name, ComposePath(name), args...)
}

// container holds the fields read from each JSON record produced by docker compose ps.
type container struct {
	ID      string `json:"ID"`
	Service string `json:"Service"`
	State   string `json:"State"`
	Health  string `json:"Health"`
}

// parseContainers reads newline-delimited JSON records from Compose. Whitespace-only output
// represents a project with no containers.
func parseContainers(out string) ([]container, error) {
	out = strings.TrimSpace(out)
	if out == "" {
		return nil, nil
	}

	var cs []container
	for line := range strings.Lines(out) {
		var c container
		if err := json.Unmarshal([]byte(line), &c); err != nil {
			return nil, err
		}

		cs = append(cs, c)
	}

	return cs, nil
}

func projectContainers(ctx context.Context, project, file string) ([]container, error) {
	out, err := docker(ctx, composeArgs(project, file, "ps", "--all", "--format", "json")...)
	if err != nil {
		return nil, err
	}

	return parseContainers(out)
}

// inspectSite returns all project containers, including stopped ones, and their creation image
// IDs by service. If a service has multiple containers, its last inspected image ID is
// retained.
func inspectSite(ctx context.Context, name string) (map[string]string, []container, error) {
	cs, err := projectContainers(ctx, name, ComposePath(name))
	if err != nil || len(cs) == 0 {
		return map[string]string{}, cs, err
	}

	args := []string{"inspect", "--format", "{{.Image}}"}
	for _, c := range cs {
		args = append(args, c.ID)
	}

	out, err := docker(ctx, args...)
	if err != nil {
		return nil, nil, err
	}

	images := map[string]string{}
	// Docker returns image IDs in the same order as the container IDs passed to inspect.
	lines := strings.Fields(out)
	for i, c := range cs {
		if i < len(lines) {
			images[c.Service] = lines[i]
		}
	}

	return images, cs, nil
}

// imageID returns an empty string if Docker cannot inspect ref, including failures other than a
// missing image.
func imageID(ctx context.Context, ref string) string {
	out, err := docker(ctx, "image", "inspect", "--format", "{{.Id}}", ref)
	if err != nil {
		return ""
	}

	return strings.TrimSpace(out)
}

// ShortID removes the sha256: prefix and returns at most the first 12 characters for display.
func ShortID(id string) string {
	id = strings.TrimPrefix(id, "sha256:")
	return id[:min(12, len(id))]
}

// NetworkExists reports whether Docker can inspect the shared web network. Inspection failures
// return false.
func NetworkExists(ctx context.Context) bool {
	_, err := docker(ctx, "network", "inspect", Network)
	return err == nil
}

// EnsureNetwork creates the shared web network if NetworkExists returns false.
func EnsureNetwork(ctx context.Context) error {
	if NetworkExists(ctx) {
		return nil
	}

	_, err := docker(ctx, "network", "create", Network)
	return err
}

// DockerVersions returns the daemon and Compose CLI versions. The daemon version is still
// returned if the Compose query fails.
func DockerVersions(ctx context.Context) (engine, compose string, err error) {
	out, err := docker(ctx, "version", "--format", "{{.Server.Version}}")
	if err != nil {
		return "", "", err
	}

	engine = strings.TrimSpace(out)
	out, err = docker(ctx, "compose", "version", "--short")
	if err != nil {
		return engine, "", err
	}

	return engine, strings.TrimSpace(out), nil
}

// DiskUsage returns the unmodified stdout from docker system df.
func DiskUsage(ctx context.Context) (string, error) {
	return docker(ctx, "system", "df")
}

// Logs writes recent stdout and stderr for the selected site services to w. An empty services
// list includes all services. The tail argument is the line count per container. With follow
// set, output streams until the command ends or ctx is cancelled. Cancellation returns nil.
func Logs(ctx context.Context, w io.Writer, name string, services []string, tail int, follow bool) error {
	if _, err := LoadSite(name); err != nil {
		return err
	}

	args := []string{"logs", "--no-color", "--tail", strconv.Itoa(tail)}
	if follow {
		args = append(args, "--follow")
	}

	cmd := exec.CommandContext(ctx, "docker", siteArgs(name, append(args, services...)...)...)
	// Using the same writer for both streams makes os/exec serialize writes to it.
	cmd.Stdout, cmd.Stderr = w, w
	err := cmd.Run()
	if ctx.Err() != nil {
		return nil
	}

	return err
}

// DockerLogin authenticates the server Docker CLI to a registry and saves its credentials. The
// password is passed through stdin rather than command-line arguments. Login failures are
// marked with Invalid.
func DockerLogin(ctx context.Context, registry, user, password string) error {
	cmd := exec.CommandContext(ctx, "docker", "login", "--username", user, "--password-stdin", registry)
	cmd.Stdin = strings.NewReader(password)
	if out, err := cmd.CombinedOutput(); err != nil {
		return Invalid(fmt.Errorf("docker login %s: %s", registry, strings.TrimSpace(string(out))))
	}

	return nil
}

// DockerInfo contains the version, configuration, and disk usage information shown by box
// docker check.
type DockerInfo struct {
	Engine, Compose string
	// Network reports whether the shared web network could be inspected.
	Network    bool
	Proxy      string
	Daemon     []string
	Registries []string
	Disk       string
}

// DockerReport collects daemon and Compose versions, network and proxy state, daemon settings,
// registry names, and disk usage. It returns any error from the required queries.
func DockerReport(ctx context.Context) (DockerInfo, error) {
	var info DockerInfo
	var err error
	if info.Engine, info.Compose, err = DockerVersions(ctx); err != nil {
		return info, err
	}

	info.Network = NetworkExists(ctx)
	info.Proxy = ProxyState(ctx)
	daemon, err := ReadDaemon()
	if err != nil {
		return info, err
	}

	info.Daemon = DaemonSummary(daemon)
	info.Registries = Registries()
	info.Disk, err = DiskUsage(ctx)
	return info, err
}
