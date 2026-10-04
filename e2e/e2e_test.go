//go:build e2e

// Package e2e tests server installation and site operations against local Docker. Tests require
// the e2e build tag and leave daemon settings unchanged. Cleanup attempts to remove test
// resources but retains cached base images.
package e2e

import (
	"bytes"
	"context"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/alfredr/box/internal/box"
	"github.com/alfredr/box/internal/client"
	"github.com/alfredr/box/internal/engine"
	boxv1 "github.com/alfredr/box/internal/gen/box/v1"
)

func docker(t *testing.T, args ...string) string {
	t.Helper()
	out, err := exec.Command("docker", args...).CombinedOutput()
	if err != nil {
		t.Fatalf("docker %s: %v\n%s", strings.Join(args, " "), err, out)
	}

	return strings.TrimSpace(string(out))
}

// quiet ignores cleanup failures so later cleanup steps still run.
func quiet(args ...string) { exec.Command("docker", args...).Run() }

// localSocket uses DOCKER_HOST or the active Docker context. The test requires a local Unix
// socket and skips other transports.
func localSocket(t *testing.T) string {
	t.Helper()
	host := os.Getenv("DOCKER_HOST")
	if host == "" {
		host = docker(t, "context", "inspect", "--format", "{{.Endpoints.docker.Host}}")
	}

	path, ok := strings.CutPrefix(host, "unix://")
	if !ok {
		t.Skipf("the test needs a local Docker socket, not %s", host)
	}

	return path
}

// pushImage changes the fixture VERSION label to produce a distinct image, pushes it under ref,
// and returns its local image ID.
func pushImage(t *testing.T, ref, dir, version string) string {
	t.Helper()
	docker(t, "build", "--quiet", "--build-arg", "VERSION="+version, "--tag", ref, filepath.Join("testdata", dir))
	docker(t, "push", "--quiet", ref)
	return docker(t, "image", "inspect", "--format", "{{.Id}}", ref)
}

func freePort(t *testing.T) string {
	t.Helper()
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}

	defer l.Close()

	return strconv.Itoa(l.Addr().(*net.TCPAddr).Port)
}

func waitFor(t *testing.T, f func() error) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for {
		err := f()
		if err == nil {
			return
		}

		if time.Now().After(deadline) {
			t.Fatalf("gave up waiting: %v", err)
		}

		time.Sleep(200 * time.Millisecond)
	}
}

// TestServer installs the server through the Docker API and exercises site operations through
// the signed client. BOX_E2E_IMAGE selects an existing server image instead of building one.
func TestServer(t *testing.T) {
	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skip("needs a running Docker")
	}

	ctx := context.Background()
	id := box.NewSecret()[:6]
	eng := engine.Unix(localSocket(t))

	tmp, err := os.MkdirTemp("", "box-e2e-")
	if err != nil {
		t.Fatal(err)
	}

	// Compose and the host daemon must resolve the same mount path. Resolve macOS
	// temporary-directory symlinks before using the path on both sides.
	root, err := filepath.EvalSymlinks(tmp)
	if err != nil {
		t.Fatal(err)
	}

	hadWeb := exec.Command("docker", "network", "inspect", box.Network).Run() == nil
	site := "e2e-" + id
	serverName, registryName := "box-e2e-server-"+id, "box-e2e-registry-"+id
	image := os.Getenv("BOX_E2E_IMAGE")
	built := image == ""
	if built {
		image = "box-e2e:" + id
	}

	t.Cleanup(func() {
		if t.Failed() {
			var logs bytes.Buffer
			eng.Logs(context.Background(), serverName, 100, false, &logs)
			t.Logf("server logs:\n%s", logs.String())
		}

		quiet("rm", "--force", serverName, serverName+"-setup", registryName)
		out, _ := exec.Command("docker", "ps", "--all", "--quiet", "--filter", "label=com.docker.compose.project="+site).Output()
		for _, c := range strings.Fields(string(out)) {
			quiet("rm", "--force", "--volumes", c)
		}

		quiet("network", "rm", site+"_default")
		if !hadWeb {
			quiet("network", "rm", box.Network)
		}

		// The server creates files as root. Remove them through a container before deleting the
		// host directory.
		quiet("run", "--rm", "--entrypoint", "rm", "--volume", root+":/root-dir", image, "-rf", "/root-dir/box.json",
			"/root-dir/secrets", "/root-dir/docker", "/root-dir/sites", "/root-dir/state", "/root-dir/proxy", "/root-dir/removed")
		os.RemoveAll(root)

		out, _ = exec.Command("docker", "images", "--format", "{{.Repository}}:{{.Tag}}").Output()
		for _, ref := range strings.Fields(string(out)) {
			if strings.Contains(ref, "e2e-"+id) || (built && ref == image) {
				quiet("image", "rm", ref)
			}
		}
	})

	if built {
		build := exec.Command("docker", "build", "--quiet", "--build-arg", "VERSION=e2e", "--tag", image, "..")
		if out, err := build.CombinedOutput(); err != nil {
			t.Fatalf("building the server image: %v\n%s", err, out)
		}
	}

	// Publish an explicit registry port on all interfaces so the host client and Docker Desktop
	// daemon can use the same registry reference.
	port := freePort(t)
	docker(t, "run", "--detach", "--name", registryName, "--publish", port+":5000", "registry:2")
	ref := "127.0.0.1:" + port + "/e2e-" + id + ":latest"
	waitFor(t, func() error {
		resp, err := http.Get("http://127.0.0.1:" + port + "/v2/")
		if err == nil {
			resp.Body.Close()
		}

		return err
	})

	// Publish the API on host loopback and disable Caddy because the test has no public domain.
	// Leave DaemonDir unset so the test does not mount or change host daemon settings.
	server := client.Server{
		Name:    serverName,
		Image:   image,
		Root:    root,
		Listen:  "0.0.0.0:9311",
		Publish: true,
		Env:     []string{"BOX_PROXY=off", "BOX_PRUNE=off"},
	}
	install := client.InstallOptions{Server: server, Domain: "box.e2e.example.com", Log: func(s string) { t.Log(s) }}
	key, err := client.Install(ctx, eng, install)
	if err != nil {
		t.Fatal(err)
	}

	st, err := eng.Inspect(ctx, serverName)
	if err != nil {
		t.Fatal(err)
	}

	profile := client.Profile{URL: "http://" + st.Ports["9311/tcp"], Key: key}
	c := client.New(profile)

	must := func(err error) {
		t.Helper()
		if err != nil {
			t.Fatal(err)
		}
	}

	running := func() string {
		t.Helper()
		out, err := c.GetSite(ctx, &boxv1.GetSiteRequest{Site: site})
		must(err)
		return out.Site.Services[0].Running
	}

	check := func() *boxv1.CheckResult {
		t.Helper()
		out, err := c.Check(ctx, &boxv1.CheckRequest{Sites: []string{site}})
		must(err)
		if r := out.Results[0]; r.Error != "" {
			t.Fatal(r.Error)
		}

		return out.Results[0]
	}

	v1 := pushImage(t, ref, "site", "v1")
	created, err := c.CreateSite(ctx, &boxv1.CreateSiteRequest{
		Name: site, Image: ref, Domains: []string{"e2e.example.com"}, Port: 80, Policy: "auto",
	})
	must(err)
	if created.Hook == nil || running() != "" {
		t.Fatalf("created = %v, running %q, want nothing yet", created, running())
	}

	if r := check(); len(r.Deployed) != 1 || running() != v1 {
		t.Fatalf("first check = %v, running %s, want %s", r, running(), v1)
	}

	v2 := pushImage(t, ref, "site", "v2")
	if r := check(); len(r.Deployed) != 1 || running() != v2 {
		t.Fatalf("check after v2 = %v, running %s", r, running())
	}

	_, err = c.Rollback(ctx, &boxv1.RollbackRequest{Site: site})
	must(err)
	if running() != v1 {
		t.Fatalf("after rollback running %s, want v1 %s", running(), v1)
	}

	if r := check(); len(r.Deployed)+len(r.Waiting) != 0 || running() != v1 {
		t.Fatalf("check after rollback = %v, running %s", r, running())
	}

	_, err = c.ApplyCompose(ctx, &boxv1.ApplyComposeRequest{Site: site, Text: "services: {}\n"})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("bad edit: %v", err)
	}

	// The preceding check pulled the rejected image under the same tag. Changing only the
	// update policy must preserve the container image selected by rollback.
	compose, err := c.GetCompose(ctx, &boxv1.GetComposeRequest{Site: site})
	must(err)
	edited := strings.Replace(compose.Text, "app: auto", "app: manual", 1)
	_, err = c.ApplyCompose(ctx, &boxv1.ApplyComposeRequest{Site: site, Text: edited})
	must(err)
	if running() != v1 {
		t.Fatalf("after the edit running %s, want v1 %s", running(), v1)
	}

	v3 := pushImage(t, ref, "site", "v3")
	if r := check(); len(r.Waiting) != 1 || running() != v1 {
		t.Fatalf("check after v3 = %v, running %s", r, running())
	}

	_, err = c.Deploy(ctx, &boxv1.DeployRequest{Site: site})
	must(err)
	if running() != v3 {
		t.Fatalf("after deploy running %s, want v3 %s", running(), v3)
	}

	// A failing health check must trigger recovery to the image used before deployment.
	pushImage(t, ref, "unhealthy", "v4")
	_, err = c.Deploy(ctx, &boxv1.DeployRequest{Site: site})
	if err == nil || !strings.Contains(client.Message(err), "unhealthy") || running() != v3 {
		t.Fatalf("failing deploy: %v, running %s, want v3 %s", err, running(), v3)
	}

	if out := docker(t, "image", "inspect", "--format", "{{.Id}}", "box-keep/"+site+":app-rejected"); out == v3 || out == "" {
		t.Fatalf("rejected image tag = %q", out)
	}

	history := func() []string {
		t.Helper()
		out, err := c.GetSite(ctx, &boxv1.GetSiteRequest{Site: site})
		must(err)
		var ids []string
		for _, k := range out.Site.Services[0].History {
			ids = append(ids, k.Id)
		}

		return ids
	}

	if h := history(); !slices.Equal(h, []string{v1, v2}) {
		t.Fatalf("history after v3 = %v, want v1 then v2", h)
	}

	_, err = c.Rollback(ctx, &boxv1.RollbackRequest{Site: site, To: box.ShortID(v1)[:6]})
	must(err)
	if running() != v1 || !slices.Equal(history(), []string{v3, v2}) {
		t.Fatalf("after rollback to v1: running %s, history %v", running(), history())
	}

	_, err = c.Rollback(ctx, &boxv1.RollbackRequest{Site: site, To: "1"})
	must(err)
	if running() != v3 || !slices.Equal(history(), []string{v1, v2}) {
		t.Fatalf("after rolling forward: running %s, history %v", running(), history())
	}

	_, err = c.Rollback(ctx, &boxv1.RollbackRequest{Site: site, To: "9"})
	if connect.CodeOf(err) != connect.CodeInvalidArgument {
		t.Fatalf("rollback to a missing image: %v", err)
	}

	compose, err = c.GetCompose(ctx, &boxv1.GetComposeRequest{Site: site})
	must(err)
	limited := strings.Replace(compose.Text, "    app: manual\n", "    app: manual\n  keep:\n    app: 1\n", 1)
	_, err = c.ApplyCompose(ctx, &boxv1.ApplyComposeRequest{Site: site, Text: limited})
	must(err)
	if h := history(); !slices.Equal(h, []string{v1}) || running() != v3 {
		t.Fatalf("after keep 1: history %v, running %s", h, running())
	}

	if out := docker(t, "images", "--filter", "reference=box-keep/"+site+":app-"+box.ShortID(v2), "--format", "{{.ID}}"); out != "" {
		t.Fatalf("the dropped image is still tagged: %s", out)
	}

	huge := strings.Replace(limited, "  keep:\n", "  budget:\n    app: 100TB\n  keep:\n", 1)
	applied, err := c.ApplyCompose(ctx, &boxv1.ApplyComposeRequest{Site: site, Text: huge})
	must(err)
	if len(applied.Warnings) == 0 || !strings.Contains(applied.Warnings[0], "could reach") {
		t.Fatalf("warnings for a 100TB budget = %v", applied.Warnings)
	}

	logs, err := c.Logs(ctx, &boxv1.LogsRequest{Site: site, Tail: 5})
	must(err)
	for logs.Receive() {
	}

	must(logs.Err())
	logs.Close()

	hook, err := c.GetHook(ctx, &boxv1.GetHookRequest{Site: site})
	must(err)
	req, _ := http.NewRequest(http.MethodPost, profile.URL+"/deploy/"+site, nil)
	req.Header.Set("Authorization", "Bearer "+hook.Hook.Secret)
	resp, err := http.DefaultClient.Do(req)
	must(err)
	resp.Body.Close()
	if resp.StatusCode != http.StatusAccepted {
		t.Fatalf("webhook: %s", resp.Status)
	}

	info, err := c.DockerInfo(ctx, &boxv1.DockerInfoRequest{})
	must(err)
	if info.Engine == "" || info.Compose == "" || !info.Network {
		t.Fatalf("docker info = %v", info)
	}

	_, err = c.SetConfig(ctx, &boxv1.SetConfigRequest{Key: "poll", Value: "off"})
	must(err)
	// Pass the existing key explicitly. Installation otherwise generates a new key.
	install.Key = key
	if _, err := client.Install(ctx, eng, install); err != nil {
		t.Fatal(err)
	}

	st, err = eng.Inspect(ctx, serverName)
	must(err)
	profile.URL = "http://" + st.Ports["9311/tcp"]
	c = client.New(profile)
	cfg, err := c.GetConfig(ctx, &boxv1.GetConfigRequest{})
	must(err)
	for _, s := range cfg.Settings {
		if s.Key == "poll" && s.Value != "off" {
			t.Fatalf("poll after reinstall = %q", s.Value)
		}
	}

	if running() != v3 {
		t.Fatalf("after reinstall running %s, want v3 %s", running(), v3)
	}

	var serverLogs bytes.Buffer
	must(eng.Logs(ctx, serverName, 20, false, &serverLogs))
	if !strings.Contains(serverLogs.String(), "box server listening") {
		t.Fatalf("server logs:\n%s", serverLogs.String())
	}

	_, err = c.RemoveSite(ctx, &boxv1.RemoveSiteRequest{Site: site, Purge: true})
	must(err)
	if _, err := c.GetSite(ctx, &boxv1.GetSiteRequest{Site: site}); connect.CodeOf(err) != connect.CodeNotFound {
		t.Fatalf("after remove: %v", err)
	}

	if out := docker(t, "images", "--filter", "reference=box-keep/"+site+":*", "--format", "{{.Repository}}:{{.Tag}}"); out != "" {
		t.Fatalf("images left after remove: %s", out)
	}

	next := box.NewSecret()
	_, err = c.RotateKey(ctx, &boxv1.RotateKeyRequest{Key: next})
	must(err)
	if _, err := c.Version(ctx, &boxv1.VersionRequest{}); connect.CodeOf(err) != connect.CodeUnauthenticated {
		t.Fatalf("old key after rotation: %v", err)
	}

	profile.Key = next
	_, err = client.New(profile).Version(ctx, &boxv1.VersionRequest{})
	must(err)
}
