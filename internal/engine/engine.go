// Package engine implements the Docker Engine API calls used to install and maintain the box
// server. Connections may use a local Unix socket or an SSH-forwarded socket.
package engine

import (
	"archive/tar"
	"bytes"
	"context"
	"encoding/base64"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"path"
	"strconv"
	"strings"
	"time"
)

// apiVersion includes support for health-check start intervals, which the server container uses
// during startup.
const apiVersion = "v1.44"

// Client sends HTTP requests to one Docker daemon over a caller-supplied connection.
type Client struct {
	http *http.Client
}

// New creates a client whose HTTP transport obtains connections through dial. Requests have no
// client-wide timeout, so callers must set deadlines on their contexts when needed.
func New(dial func(ctx context.Context) (net.Conn, error)) *Client {
	tr := &http.Transport{DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) { return dial(ctx) }}
	return &Client{http: &http.Client{Transport: tr}}
}

// Unix creates a client connected to the Unix socket at path.
func Unix(path string) *Client {
	return New(func(ctx context.Context) (net.Conn, error) {
		var d net.Dialer
		return d.DialContext(ctx, "unix", path)
	})
}

// Error reports an HTTP error response from the Docker daemon.
type Error struct {
	// Status is the HTTP response status code.
	Status  int
	Message string
}

// Error returns the daemon response message.
func (e *Error) Error() string { return e.Message }

// IsNotFound reports whether err wraps a Docker API response with HTTP status 404.
func IsNotFound(err error) bool {
	var e *Error
	return errors.As(err, &e) && e.Status == http.StatusNotFound
}

func (c *Client) send(ctx context.Context, method, p string, query url.Values, body io.Reader, header http.Header) (*http.Response, error) {
	u := "http://docker/" + apiVersion + p
	if len(query) > 0 {
		u += "?" + query.Encode()
	}

	req, err := http.NewRequestWithContext(ctx, method, u, body)
	if err != nil {
		return nil, err
	}

	for k, v := range header {
		req.Header[k] = v
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return nil, err
	}

	if resp.StatusCode < 400 {
		return resp, nil
	}

	defer resp.Body.Close()

	b, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	var m struct {
		Message string `json:"message"`
	}
	if json.Unmarshal(b, &m) != nil || m.Message == "" {
		m.Message = resp.Status + ": " + strings.TrimSpace(string(b))
	}

	return nil, &Error{Status: resp.StatusCode, Message: m.Message}
}

// call encodes a non-nil input as JSON. It decodes into out when provided, otherwise drains the
// response body. The response body is always closed.
func (c *Client) call(ctx context.Context, method, p string, query url.Values, in, out any) error {
	var body io.Reader
	header := http.Header{}
	if in != nil {
		b, err := json.Marshal(in)
		if err != nil {
			return err
		}

		body = bytes.NewReader(b)
		header.Set("Content-Type", "application/json")
	}

	resp, err := c.send(ctx, method, p, query, body, header)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	if out == nil {
		_, err := io.Copy(io.Discard, resp.Body)
		return err
	}

	return json.NewDecoder(resp.Body).Decode(out)
}

// Ping checks whether the daemon accepts a request using this client API version.
func (c *Client) Ping(ctx context.Context) error {
	return c.call(ctx, http.MethodGet, "/_ping", nil, nil, nil)
}

// ImageStore returns the daemon's storage directory for filesystem storage drivers. It returns
// an empty path for containerd and other backends whose image filesystem the Engine API does
// not identify. In particular, DockerRootDir need not contain containerd image data.
func (c *Client) ImageStore(ctx context.Context) (string, error) {
	var info struct {
		Driver, DockerRootDir string
		DriverStatus          [][2]string
	}
	if err := c.call(ctx, http.MethodGet, "/info", nil, nil, &info); err != nil {
		return "", err
	}

	for _, pair := range info.DriverStatus {
		if pair[0] == "driver-type" && strings.HasPrefix(pair[1], "io.containerd.") {
			return "", nil
		}
	}

	switch info.Driver {
	case "overlay2", "aufs", "vfs", "btrfs", "zfs":
		return info.DockerRootDir, nil
	default:
		return "", nil
	}
}

// Version holds the daemon release and its advertised maximum Engine API version.
type Version struct {
	Version    string
	APIVersion string `json:"ApiVersion"`
}

// Supported reports whether the advertised API version is at least the version used by this
// client.
func (v Version) Supported() bool {
	return compareAPI(v.APIVersion, strings.TrimPrefix(apiVersion, "v")) >= 0
}

func compareAPI(a, b string) int {
	am, an, _ := strings.Cut(a, ".")
	bm, bn, _ := strings.Cut(b, ".")
	for _, pair := range [][2]string{{am, bm}, {an, bn}} {
		x, _ := strconv.Atoi(pair[0])
		y, _ := strconv.Atoi(pair[1])
		if x != y {
			return x - y
		}
	}

	return 0
}

// MinimumDocker is the required Docker release reported in setup diagnostics.
const MinimumDocker = "25"

// Version queries the unversioned Docker endpoint so it can inspect daemons that reject this
// client API version.
func (c *Client) Version(ctx context.Context) (Version, error) {
	var v Version
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://docker/version", nil)
	if err != nil {
		return v, err
	}

	resp, err := c.http.Do(req)
	if err != nil {
		return v, err
	}

	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		return v, fmt.Errorf("docker version: %s", resp.Status)
	}

	return v, json.NewDecoder(resp.Body).Decode(&v)
}

// Auth holds credentials sent to Docker for a registry image pull.
type Auth struct {
	Username      string `json:"username"`
	Password      string `json:"password"`
	ServerAddress string `json:"serveraddress"`
}

// Pull downloads ref through the daemon and waits for the progress stream to end. A non-nil
// auth supplies registry credentials for this request. Errors reported inside the stream are
// returned even after a successful HTTP status.
func (c *Client) Pull(ctx context.Context, ref string, auth *Auth) error {
	header := http.Header{}
	if auth != nil {
		b, err := json.Marshal(auth)
		if err != nil {
			return err
		}

		header.Set("X-Registry-Auth", base64.URLEncoding.EncodeToString(b))
	}

	resp, err := c.send(ctx, http.MethodPost, "/images/create", url.Values{"fromImage": {ref}}, nil, header)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	// Pull failures can arrive as progress records after Docker has sent the HTTP response
	// headers.
	dec := json.NewDecoder(resp.Body)
	for {
		var msg struct {
			Error string `json:"error"`
		}
		if err := dec.Decode(&msg); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}

		if msg.Error != "" {
			return fmt.Errorf("pulling %s: %s", ref, msg.Error)
		}
	}
}

// ImageExists inspects a local image reference. A missing image returns false without an error,
// while other inspection failures are returned.
func (c *Client) ImageExists(ctx context.Context, ref string) (bool, error) {
	err := c.call(ctx, http.MethodGet, "/images/"+ref+"/json", nil, nil, nil)
	if IsNotFound(err) {
		return false, nil
	}

	return err == nil, err
}

// Healthcheck defines the command and timing fields sent to Docker. Create serializes all
// durations as nanoseconds without applying local defaults.
type Healthcheck struct {
	Test          []string
	Interval      time.Duration
	StartPeriod   time.Duration
	StartInterval time.Duration
}

// Container describes the image, process, host mounts, and networking for a Docker container.
// Create forwards these values to the daemon without pulling the image.
type Container struct {
	Name   string
	Image  string
	Cmd    []string
	Env    []string
	Labels map[string]string
	// Binds contains Docker bind specifications with paths resolved on the daemon host.
	Binds []string
	// ReadOnlyBinds maps container destinations to existing host directories. Unlike Binds,
	// these mounts fail if the source is missing instead of creating a host directory.
	ReadOnlyBinds map[string]string
	// NetworkMode selects Docker networking. "host" shares the host network, and an empty value
	// uses the daemon default.
	NetworkMode string
	// Publish maps a container port and protocol, such as "9311/tcp", to an IPv4 host:port
	// address. An empty host port, as in "127.0.0.1:", lets Docker allocate a port.
	Publish       map[string]string
	RestartPolicy string
	Healthcheck   *Healthcheck
	// StopTimeout is sent in whole seconds when positive. Zero leaves the daemon default
	// unchanged.
	StopTimeout time.Duration
}

// Create creates an unstarted container and returns its ID. The image must already be available
// to the daemon.
func (c *Client) Create(ctx context.Context, spec Container) (string, error) {
	type portBinding struct{ HostIp, HostPort string }
	type mount struct {
		Type, Source, Target string
		ReadOnly             bool
	}
	type hostConfig struct {
		Binds         []string                 `json:",omitempty"`
		Mounts        []mount                  `json:",omitempty"`
		NetworkMode   string                   `json:",omitempty"`
		PortBindings  map[string][]portBinding `json:",omitempty"`
		RestartPolicy struct{ Name string }    `json:",omitempty"`
	}
	type healthcheck struct {
		Test                                 []string
		Interval, StartPeriod, StartInterval int64
	}
	body := struct {
		Image        string
		Cmd          []string            `json:",omitempty"`
		Env          []string            `json:",omitempty"`
		Labels       map[string]string   `json:",omitempty"`
		ExposedPorts map[string]struct{} `json:",omitempty"`
		Healthcheck  *healthcheck        `json:",omitempty"`
		StopTimeout  *int                `json:",omitempty"`
		HostConfig   hostConfig
	}{Image: spec.Image, Cmd: spec.Cmd, Env: spec.Env, Labels: spec.Labels}

	body.HostConfig.Binds = spec.Binds
	for target, source := range spec.ReadOnlyBinds {
		body.HostConfig.Mounts = append(body.HostConfig.Mounts, mount{Type: "bind", Source: source, Target: target, ReadOnly: true})
	}
	body.HostConfig.NetworkMode = spec.NetworkMode
	body.HostConfig.RestartPolicy.Name = spec.RestartPolicy
	for port, addr := range spec.Publish {
		host, hostPort, _ := strings.Cut(addr, ":")
		if body.ExposedPorts == nil {
			body.ExposedPorts, body.HostConfig.PortBindings = map[string]struct{}{}, map[string][]portBinding{}
		}

		body.ExposedPorts[port] = struct{}{}
		body.HostConfig.PortBindings[port] = []portBinding{{HostIp: host, HostPort: hostPort}}
	}

	if h := spec.Healthcheck; h != nil {
		body.Healthcheck = &healthcheck{h.Test, int64(h.Interval), int64(h.StartPeriod), int64(h.StartInterval)}
	}

	if spec.StopTimeout > 0 {
		s := int(spec.StopTimeout.Seconds())
		body.StopTimeout = &s
	}

	var out struct{ Id string }
	err := c.call(ctx, http.MethodPost, "/containers/create", url.Values{"name": {spec.Name}}, body, &out)
	return out.Id, err
}

// Start asks Docker to start an existing container. It does not wait for application readiness
// or health checks.
func (c *Client) Start(ctx context.Context, id string) error {
	return c.call(ctx, http.MethodPost, "/containers/"+id+"/start", nil, nil, nil)
}

// Stop asks Docker to stop a container, allowing timeout before forced termination. The timeout
// is passed in whole seconds.
func (c *Client) Stop(ctx context.Context, id string, timeout time.Duration) error {
	q := url.Values{"t": {strconv.Itoa(int(timeout.Seconds()))}}
	return c.call(ctx, http.MethodPost, "/containers/"+id+"/stop", q, nil, nil)
}

// Remove deletes a container while retaining its volumes. Setting force allows Docker to remove
// a running container.
func (c *Client) Remove(ctx context.Context, id string, force bool) error {
	return c.call(ctx, http.MethodDelete, "/containers/"+id, url.Values{"force": {strconv.FormatBool(force)}}, nil, nil)
}

// State contains the container fields needed for server maintenance and readiness checks.
type State struct {
	ID string
	// Image is the configured image reference, not the resolved image ID.
	Image string
	// Status is the Docker container state. Health is empty when no health check is configured.
	Status, Health string
	// Ports maps container port/protocol values to IPv4 host addresses. When a port has
	// multiple IPv4 bindings, Inspect retains the last one.
	Ports map[string]string
	// Mounts maps bind-mount destinations to their source paths on the daemon host.
	Mounts map[string]string
}

// Inspect reads container state, published IPv4 port bindings, and bind mounts by name or ID.
func (c *Client) Inspect(ctx context.Context, id string) (State, error) {
	var out struct {
		Id     string
		Config struct{ Image string }
		State  struct {
			Status string
			Health *struct{ Status string }
		}
		NetworkSettings struct {
			Ports map[string][]struct{ HostIp, HostPort string }
		}
		Mounts []struct{ Type, Source, Destination string }
	}
	if err := c.call(ctx, http.MethodGet, "/containers/"+id+"/json", nil, nil, &out); err != nil {
		return State{}, err
	}

	st := State{ID: out.Id, Image: out.Config.Image, Status: out.State.Status, Ports: map[string]string{}}
	if out.State.Health != nil {
		st.Health = out.State.Health.Status
	}

	for port, bindings := range out.NetworkSettings.Ports {
		for _, b := range bindings {
			// The local client uses IPv4 bindings even when Docker also publishes an IPv6
			// address.
			if !strings.Contains(b.HostIp, ":") {
				st.Ports[port] = net.JoinHostPort(b.HostIp, b.HostPort)
			}
		}
	}

	st.Mounts = map[string]string{}
	for _, mount := range out.Mounts {
		if mount.Type == "bind" {
			st.Mounts[mount.Destination] = mount.Source
		}
	}

	return st, nil
}

// File describes an archive entry to copy into a container. PutFiles assigns UID and GID zero
// to each entry.
type File struct {
	// Name is a relative path below the destination directory. PutFiles does not validate it.
	Name string
	Data []byte
	// Mode contains the Unix permission bits used in the tar header.
	Mode int64
	// Dir selects a directory entry. Data must be empty for directories.
	Dir bool
}

// PutFiles writes an archive into dir in a container, which may be stopped. Writes through bind
// mounts affect the daemon host. File ownership is UID and GID zero, and all file data is
// buffered before upload.
func (c *Client) PutFiles(ctx context.Context, id, dir string, files []File) error {
	var b bytes.Buffer
	tw := tar.NewWriter(&b)
	for _, f := range files {
		h := &tar.Header{Name: f.Name, Mode: f.Mode, Size: int64(len(f.Data)), Typeflag: tar.TypeReg, ModTime: time.Now()}
		if f.Dir {
			h.Typeflag, h.Size, h.Name = tar.TypeDir, 0, strings.TrimSuffix(f.Name, "/")+"/"
		}

		if err := tw.WriteHeader(h); err != nil {
			return err
		}

		if _, err := tw.Write(f.Data); err != nil {
			return err
		}
	}

	if err := tw.Close(); err != nil {
		return err
	}

	header := http.Header{"Content-Type": {"application/x-tar"}}
	resp, err := c.send(ctx, http.MethodPut, "/containers/"+id+"/archive", url.Values{"path": {dir}}, &b, header)
	if err != nil {
		return err
	}

	return resp.Body.Close()
}

// GetFile reads a regular file from a container archive response. Missing paths return an error
// recognized by IsNotFound. The file contents are read fully into memory.
func (c *Client) GetFile(ctx context.Context, id, p string) ([]byte, error) {
	resp, err := c.send(ctx, http.MethodGet, "/containers/"+id+"/archive", url.Values{"path": {p}}, nil, nil)
	if err != nil {
		return nil, err
	}

	defer resp.Body.Close()

	tr := tar.NewReader(resp.Body)
	for {
		h, err := tr.Next()
		if err != nil {
			return nil, fmt.Errorf("reading %s: %w", p, err)
		}

		if h.Typeflag == tar.TypeReg && path.Base(h.Name) == path.Base(p) {
			return io.ReadAll(tr)
		}
	}
}

// Logs writes recent container stdout and stderr to w. The tail argument is the requested line
// count, and follow keeps the response open for new output. The container must use multiplexed log
// framing rather than a TTY stream. Context cancellation ends the stream without returning an
// error.
func (c *Client) Logs(ctx context.Context, id string, tail int, follow bool, w io.Writer) error {
	q := url.Values{"stdout": {"1"}, "stderr": {"1"}, "tail": {strconv.Itoa(tail)}, "follow": {strconv.FormatBool(follow)}}
	resp, err := c.send(ctx, http.MethodGet, "/containers/"+id+"/logs", q, nil, nil)
	if err != nil {
		return err
	}

	defer resp.Body.Close()

	err = demux(resp.Body, w)
	if ctx.Err() != nil {
		return nil
	}

	return err
}

// demux copies Docker log frame payloads to w in arrival order. Each eight-byte header contains
// the payload length as a big-endian uint32 in its last four bytes. Stream identifiers are
// discarded.
func demux(r io.Reader, w io.Writer) error {
	var header [8]byte
	for {
		if _, err := io.ReadFull(r, header[:]); errors.Is(err, io.EOF) {
			return nil
		} else if err != nil {
			return err
		}

		if _, err := io.CopyN(w, r, int64(binary.BigEndian.Uint32(header[4:]))); err != nil {
			return err
		}
	}
}
