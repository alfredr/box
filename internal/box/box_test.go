package box

import (
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestConfigSetRefusesBadValues(t *testing.T) {
	var c Config
	bad := [][2]string{
		{"notify", "ntfy.sh/topic"},
		{"poll", "30s"},
		{"poll", "often"},
		{"domain", "Box.Example.com"},
		{"domain", ""},
		{"email", "not an address"},
		{"colour", "blue"},
	}
	for _, kv := range bad {
		if err := c.Set(kv[0], kv[1]); err == nil {
			t.Errorf("Set(%q, %q) accepted", kv[0], kv[1])
		}
	}

	good := [][2]string{
		{"notify", "https://ntfy.sh/topic"},
		{"poll", "2m"},
		{"domain", "box.example.com"},
		{"email", "me@example.com"},
	}
	for _, kv := range good {
		if err := c.Set(kv[0], kv[1]); err != nil {
			t.Errorf("Set(%q, %q): %v", kv[0], kv[1], err)
		}
	}

	if c.PollEvery() != 2*time.Minute {
		t.Errorf("PollEvery = %v", c.PollEvery())
	}

	if err := c.Set("poll", "off"); err != nil || c.PollEvery() != 0 {
		t.Errorf("poll off: %v, every %v", err, c.PollEvery())
	}

	if err := c.Set("colour", "blue"); !IsInvalid(err) {
		t.Errorf("unknown setting should be invalid, got %v", err)
	}
}

func TestConfigDefaults(t *testing.T) {
	var c Config
	if got, _ := c.Get("poll"); got != DefaultPoll {
		t.Errorf("poll = %q", got)
	}

	if _, err := c.HookURL("blog"); err == nil {
		t.Error("HookURL without a domain should fail")
	}

	c.Domain = "box.example.com"
	if u, _ := c.HookURL("blog"); u != "https://box.example.com/deploy/blog" {
		t.Errorf("HookURL = %q", u)
	}
}

func TestConfigRoundTrip(t *testing.T) {
	t.Setenv("BOX_ROOT", t.TempDir())
	c := Config{Email: "me@example.com", Domain: "box.example.com"}
	if err := c.Save(); err != nil {
		t.Fatal(err)
	}

	got, err := LoadConfig()
	if err != nil || got != c {
		t.Fatalf("LoadConfig = %+v, %v", got, err)
	}
}

func TestParseSite(t *testing.T) {
	site, err := ParseSite("game", []byte(`
x-box:
  update:
    web: auto
    db: pinned
services:
  web:
    image: ghcr.io/alfredr/thirty-phantom:latest
    labels:
      caddy_0: "game.example.com, play.example.com"
      caddy_0.reverse_proxy: "{{upstreams 80}}"
      caddy_1: www.game.example.com
      caddy_1.redir: "https://game.example.com{uri}"
  db:
    image: postgres:17
  worker:
    image: ghcr.io/alfredr/worker:main
    labels:
      - "caddy=api.example.com"
      - "com.example.other=1"
`))
	if err != nil {
		t.Fatal(err)
	}

	want := []Service{
		{Name: "db", Image: "postgres:17", Policy: Pinned},
		{Name: "web", Image: "ghcr.io/alfredr/thirty-phantom:latest", Policy: Auto},
		{Name: "worker", Image: "ghcr.io/alfredr/worker:main", Policy: Manual},
	}
	if !slices.Equal(site.Services, want) {
		t.Errorf("services = %+v", site.Services)
	}

	domains := []string{"game.example.com", "play.example.com", "www.game.example.com", "api.example.com"}
	if !slices.Equal(site.Domains, domains) {
		t.Errorf("domains = %v", site.Domains)
	}
}

func TestParseSiteRefuses(t *testing.T) {
	cases := map[string]string{
		"no services":     "x-box: {}\n",
		"unknown service": "x-box: {update: {web: auto}}\nservices: {app: {image: nginx}}\n",
		"bad policy":      "x-box: {update: {app: sometimes}}\nservices: {app: {image: nginx}}\n",
		"build only":      "services: {app: {build: .}}\n",
		"interpolated":    "services: {app: {image: \"nginx:${TAG}\"}}\n",
	}
	for name, src := range cases {
		if _, err := ParseSite("s", []byte(src)); !IsInvalid(err) {
			t.Errorf("%s: got %v, want an invalid error", name, err)
		}
	}
}

func TestScaffoldParses(t *testing.T) {
	n := NewSite{
		Name:      "blog",
		Image:     "ghcr.io/alfredr/blog:latest",
		Domains:   []string{"example.com"},
		Redirects: []string{"www.example.com"},
		Port:      8080,
		Policy:    Auto,
	}
	data, err := n.Compose()
	if err != nil {
		t.Fatal(err)
	}

	site, err := ParseSite("blog", data)
	if err != nil {
		t.Fatalf("%v\n%s", err, data)
	}

	if !slices.Equal(site.Services, []Service{{Name: "app", Image: n.Image, Policy: Auto}}) {
		t.Errorf("services = %+v", site.Services)
	}

	if !slices.Equal(site.Domains, []string{"example.com", "www.example.com"}) {
		t.Errorf("domains = %v", site.Domains)
	}

	for _, want := range []string{`"{{upstreams 8080}}"`, `"https://example.com{uri} permanent"`, "external: true"} {
		if !strings.Contains(string(data), want) {
			t.Errorf("compose file lacks %s:\n%s", want, data)
		}
	}
}

func TestScaffoldRefuses(t *testing.T) {
	ok := NewSite{Name: "blog", Image: "nginx", Domains: []string{"example.com"}, Port: 80, Policy: Auto}
	bad := []func(*NewSite){
		func(n *NewSite) { n.Name = "Blog" },
		func(n *NewSite) { n.Image = "nginx latest" },
		func(n *NewSite) { n.Domains = nil },
		func(n *NewSite) { n.Domains = []string{"example.com\"; evil"} },
		func(n *NewSite) { n.Redirects = []string{"www.example.com/"} },
		func(n *NewSite) { n.Port = 0 },
		func(n *NewSite) { n.Policy = "sometimes" },
	}
	for i, change := range bad {
		n := ok
		change(&n)
		if _, err := n.Compose(); err == nil {
			t.Errorf("case %d accepted %+v", i, n)
		}
	}
}

func TestCreateSite(t *testing.T) {
	t.Setenv("BOX_ROOT", t.TempDir())
	n := NewSite{Name: "blog", Image: "nginx:alpine", Domains: []string{"example.com"}, Port: 80, Policy: Manual}
	if err := CreateSite(n); err != nil {
		t.Fatal(err)
	}

	if err := CreateSite(n); !IsInvalid(err) {
		t.Errorf("creating a site twice: got %v, want an invalid error", err)
	}

	if _, err := LoadSite("nope"); !errors.Is(err, ErrNoSite) {
		t.Errorf("LoadSite of a missing site: %v", err)
	}

	names, err := ListSites()
	if err != nil || !slices.Equal(names, []string{"blog"}) {
		t.Errorf("ListSites = %v, %v", names, err)
	}

	secret, err := HookSecret("blog")
	if err != nil || len(secret) != 64 {
		t.Errorf("secret = %q, %v", secret, err)
	}

	info, err := os.Stat(secretPath("blog"))
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("secret file mode = %v, %v", info.Mode(), err)
	}

	rotated, err := RotateHookSecret("blog")
	if err != nil || rotated == secret {
		t.Errorf("rotate gave %q, %v", rotated, err)
	}
}

func TestParseContainers(t *testing.T) {
	lines := `{"ID":"a1","Service":"web","State":"running","Health":"healthy"}
{"ID":"b2","Service":"db","State":"exited","Health":""}
`
	want := []container{{"a1", "web", "running", "healthy"}, {"b2", "db", "exited", ""}}
	if got, err := parseContainers(lines); err != nil || !slices.Equal(got, want) {
		t.Errorf("parseContainers = %+v, %v", got, err)
	}

	if got, err := parseContainers("\n"); err != nil || got != nil {
		t.Errorf("empty = %+v, %v", got, err)
	}
}

func TestMergeDaemon(t *testing.T) {
	current := []byte(`{"registry-mirrors": ["https://mirror.example.com"], "log-opts": {"labels": "x"}}`)
	on := true
	out, changed, err := MergeDaemon(current, DaemonSettings{LogSize: "10m", LogFiles: 3, LiveRestore: &on})
	if err != nil || !changed {
		t.Fatalf("changed = %v, err = %v", changed, err)
	}

	var m map[string]any
	if err := json.Unmarshal(out, &m); err != nil {
		t.Fatal(err)
	}

	opts := m["log-opts"].(map[string]any)
	if opts["max-size"] != "10m" || opts["max-file"] != "3" || opts["labels"] != "x" {
		t.Errorf("log-opts = %v", opts)
	}

	if m["live-restore"] != true || m["registry-mirrors"] == nil {
		t.Errorf("daemon.json = %s", out)
	}

	if _, changed, _ := MergeDaemon(out, DaemonSettings{LogSize: "10m"}); changed {
		t.Error("setting the same size again reported a change")
	}

	if _, _, err := MergeDaemon(nil, DaemonSettings{LogSize: "10 MB"}); err == nil {
		t.Error("accepted a bad size")
	}

	if _, _, err := MergeDaemon([]byte("{"), DaemonSettings{LogSize: "10m"}); err == nil {
		t.Error("accepted broken JSON")
	}
}

func TestDaemonSummary(t *testing.T) {
	got := strings.Join(DaemonSummary([]byte(`{"log-opts":{"max-size":"10m","max-file":"3"},"live-restore":true}`)), "\n")
	for _, want := range []string{"max-size 10m", "max-file 3", "live-restore  on"} {
		if !strings.Contains(got, want) {
			t.Errorf("summary lacks %q:\n%s", want, got)
		}
	}

	if got := strings.Join(DaemonSummary(nil), "\n"); !strings.Contains(got, "no rotation") {
		t.Errorf("empty summary = %s", got)
	}
}

func TestCaddyfile(t *testing.T) {
	b, err := Caddyfile(Config{Email: "me@example.com", Domain: "box.example.com"}, "127.0.0.1:9311")
	if err != nil {
		t.Fatal(err)
	}

	got := string(b)
	for _, want := range []string{"email me@example.com", "box.example.com {", "reverse_proxy 127.0.0.1:9311"} {
		if !strings.Contains(got, want) {
			t.Errorf("Caddyfile lacks %q:\n%s", want, got)
		}
	}

	if b, _ := Caddyfile(Config{}, "127.0.0.1:9311"); strings.Contains(string(b), "{") {
		t.Errorf("empty config should give no blocks:\n%s", got)
	}
}

func TestRecordRoundTrip(t *testing.T) {
	t.Setenv("BOX_ROOT", t.TempDir())
	var r Record
	put(&r.Images, "web", "sha256:abc")
	r.Result = "ok"
	if err := r.save("blog"); err != nil {
		t.Fatal(err)
	}

	got, err := LoadRecord("blog")
	if err != nil || got.Images["web"] != "sha256:abc" || got.Result != "ok" {
		t.Errorf("LoadRecord = %+v, %v", got, err)
	}

	b, _ := os.ReadFile(filepath.Join(stateDir(), "blog.json"))
	if strings.Contains(string(b), "deployed_at") {
		t.Errorf("zero times should be left out:\n%s", b)
	}
}

func TestAPIKey(t *testing.T) {
	t.Setenv("BOX_ROOT", t.TempDir())
	if k, err := APIKey(); k != "" || err != nil {
		t.Errorf("before setup: %q, %v", k, err)
	}

	if err := SetAPIKey("short"); !IsInvalid(err) {
		t.Errorf("short key: %v", err)
	}

	key := NewSecret()
	if err := SetAPIKey(key); err != nil {
		t.Fatal(err)
	}

	if k, err := APIKey(); k != key || err != nil {
		t.Errorf("APIKey = %q, %v", k, err)
	}

	info, err := os.Stat(apiKeyPath())
	if err != nil || info.Mode().Perm() != 0o600 {
		t.Errorf("key file mode = %v, %v", info.Mode(), err)
	}
}
