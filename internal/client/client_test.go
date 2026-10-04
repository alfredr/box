package client

import (
	"encoding/base64"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"github.com/alfredr/box/internal/engine"
)

func TestProfiles(t *testing.T) {
	path := filepath.Join(t.TempDir(), "box", "servers.json")
	t.Setenv("BOX_CONFIG", path)
	t.Setenv("BOX_SERVER", "")

	p, err := LoadProfiles()
	if err != nil {
		t.Fatal(err)
	}

	if _, _, err := p.Pick(""); err == nil || !strings.Contains(err.Error(), "box setup") {
		t.Errorf("no servers: %v", err)
	}

	p.Put("prod", Profile{URL: "https://box.example.com", Key: "k1"})
	p.Put("test", Profile{URL: "https://box.test.example.com", Key: "k2"})
	if err := p.Save(); err != nil {
		t.Fatal(err)
	}

	info, err := os.Stat(path)
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("servers.json mode = %v, %v", info.Mode(), err)
	}

	p, err = LoadProfiles()
	if err != nil {
		t.Fatal(err)
	}

	if name, _, _ := p.Pick(""); name != "prod" {
		t.Errorf("default = %q, want the first saved", name)
	}

	if name, pr, _ := p.Pick("test"); name != "test" || pr.Key != "k2" {
		t.Errorf("Pick(test) = %q, %+v", name, pr)
	}

	t.Setenv("BOX_SERVER", "test")
	if name, _, _ := p.Pick(""); name != "test" {
		t.Errorf("BOX_SERVER: %q", name)
	}

	if _, _, err := p.Pick("other"); err == nil {
		t.Error("an unknown server should fail")
	}
}

func TestServerContainer(t *testing.T) {
	c := DefaultServer("ghcr.io/alfredr/box:latest").container()
	if c.Name != "box" || c.Image != "ghcr.io/alfredr/box:latest" || c.NetworkMode != "host" || c.Publish != nil {
		t.Errorf("container = %+v", c)
	}

	for _, want := range []string{"/var/run/docker.sock:/var/run/docker.sock", "/srv/box:/srv/box", "/etc/docker:/etc/docker:ro"} {
		if !slices.Contains(c.Binds, want) {
			t.Errorf("binds lack %s: %v", want, c.Binds)
		}
	}

	for _, want := range []string{"BOX_ROOT=/srv/box", "BOX_LISTEN=127.0.0.1:9311", "DOCKER_CONFIG=/srv/box/docker"} {
		if !slices.Contains(c.Env, want) {
			t.Errorf("env lacks %s: %v", want, c.Env)
		}
	}

	s := DefaultServer("box:test")
	s.Listen, s.Publish = "0.0.0.0:9400", true
	c = s.container()
	if c.NetworkMode != "" || c.Publish["9400/tcp"] != "127.0.0.1:" {
		t.Errorf("published container = %+v", c)
	}
}

func TestDockerConfig(t *testing.T) {
	b, err := dockerConfig(&engine.Auth{Username: "me", Password: "pw", ServerAddress: "ghcr.io"})
	if err != nil {
		t.Fatal(err)
	}

	if !strings.Contains(string(b), `"ghcr.io"`) || !strings.Contains(string(b), base64.StdEncoding.EncodeToString([]byte("me:pw"))) {
		t.Errorf("config.json = %s", b)
	}
}
