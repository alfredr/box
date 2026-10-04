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
		{Name: "db", Image: "postgres:17", Policy: Pinned, Keep: -1, Budget: -1},
		{Name: "web", Image: "ghcr.io/alfredr/thirty-phantom:latest", Policy: Auto, Keep: -1, Budget: -1},
		{Name: "worker", Image: "ghcr.io/alfredr/worker:main", Policy: Manual, Keep: -1, Budget: -1},
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

	if !slices.Equal(site.Services, []Service{{Name: "app", Image: n.Image, Policy: Auto, Keep: -1, Budget: -1}}) {
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

func TestReclaimedFrom(t *testing.T) {
	cases := map[string]string{
		"Deleted Images:\ndeleted: sha256:abc\n\nTotal reclaimed space: 19.22MB\n": "19.22MB",
		"Total reclaimed space: 0B\n": "0B",
		"":                            "0B",
	}
	for out, want := range cases {
		if got := reclaimedFrom(out); got != want {
			t.Errorf("reclaimedFrom(%q) = %q, want %q", out, got, want)
		}
	}
}

func TestServiceLimits(t *testing.T) {
	site, err := ParseSite("game", []byte(`
x-box:
  update: {web: auto}
  keep: {web: 10, db: 0}
  budget: {web: 500MB}
services:
  web: {image: "ghcr.io/alfredr/thirty-phantom:latest"}
  db: {image: "postgres:17"}
  worker: {image: "ghcr.io/alfredr/worker:main"}
`))
	if err != nil {
		t.Fatal(err)
	}

	cfg := Config{Keep: "5", Budget: "2GB"}
	want := map[string]Limits{
		"db":     {Keep: 0, Budget: 2_000_000_000},
		"web":    {Keep: 10, Budget: 500_000_000},
		"worker": {Keep: 5, Budget: 2_000_000_000},
	}
	for _, s := range site.Services {
		if got := cfg.Limits(s); got != want[s.Name] {
			t.Errorf("%s limits = %+v, want %+v", s.Name, got, want[s.Name])
		}
	}

	if got := (Config{}).Limits(site.Services[2]); got != (Limits{Keep: DefaultKeep}) {
		t.Errorf("default limits = %+v", got)
	}

	bad := map[string]string{
		"negative keep":   "x-box: {keep: {app: -1}}\nservices: {app: {image: nginx}}\n",
		"wordy keep":      "x-box: {keep: {app: lots}}\nservices: {app: {image: nginx}}\n",
		"huge keep":       "x-box: {keep: {app: 1000}}\nservices: {app: {image: nginx}}\n",
		"bad budget":      "x-box: {budget: {app: huge}}\nservices: {app: {image: nginx}}\n",
		"unknown service": "x-box: {budget: {web: 1GB}}\nservices: {app: {image: nginx}}\n",
	}
	for name, src := range bad {
		if _, err := ParseSite("s", []byte(src)); !IsInvalid(err) {
			t.Errorf("%s: got %v, want an invalid error", name, err)
		}
	}
}

func TestLimitSettings(t *testing.T) {
	var c Config
	if v, _ := c.Get("keep"); v != "3" {
		t.Errorf("keep default = %q", v)
	}

	if v, _ := c.Get("budget"); v != "off" {
		t.Errorf("budget default = %q", v)
	}

	for _, kv := range [][2]string{{"keep", "-1"}, {"keep", "x"}, {"budget", "lots"}} {
		if err := c.Set(kv[0], kv[1]); !IsInvalid(err) {
			t.Errorf("Set(%q, %q) = %v", kv[0], kv[1], err)
		}
	}

	for _, kv := range [][2]string{{"keep", "10"}, {"budget", "1.5GB"}, {"budget", "off"}} {
		if err := c.Set(kv[0], kv[1]); err != nil {
			t.Errorf("Set(%q, %q): %v", kv[0], kv[1], err)
		}
	}
}

func TestSizes(t *testing.T) {
	cases := map[string]int64{
		"0B": 0, "512": 512, "94.71MB": 94_710_000, "2GB": 2_000_000_000, "1.5 GB": 1_500_000_000,
		"10kB": 10_000, "1KiB": 1024, "1GiB": 1 << 30, "3TB": 3_000_000_000_000,
	}
	for in, want := range cases {
		if got, err := ParseSize(in); err != nil || got != want {
			t.Errorf("ParseSize(%q) = %d, %v, want %d", in, got, err, want)
		}
	}

	for _, in := range []string{"", "MB", "1XB", "-5MB"} {
		if _, err := ParseSize(in); err == nil {
			t.Errorf("ParseSize(%q) accepted", in)
		}
	}

	formats := map[int64]string{0: "0B", 999: "999B", 94_710_000: "94.7MB", 2_000_000_000: "2.0GB"}
	for in, want := range formats {
		if got := FormatSize(in); got != want {
			t.Errorf("FormatSize(%d) = %q, want %q", in, got, want)
		}
	}
}

func TestOverLimits(t *testing.T) {
	hist := []Kept{{ID: "h1"}, {ID: "h2"}, {ID: "h3"}, {ID: "h4"}}
	sizes := map[string]imageSize{
		"cur": {Full: 100, Unique: 10},
		"h1":  {Full: 100, Unique: 20},
		"h2":  {Full: 100, Unique: 30},
		"h3":  {Full: 100, Unique: 40},
		"h4":  {Full: 100, Unique: 50},
	}
	ids := func(ks []Kept) []string {
		var out []string
		for _, k := range ks {
			out = append(out, k.ID)
		}

		return out
	}

	cases := []struct {
		name          string
		lim           Limits
		kept, dropped []string
	}{
		{"within", Limits{Keep: 10}, []string{"h1", "h2", "h3", "h4"}, nil},
		{"keep", Limits{Keep: 2}, []string{"h1", "h2"}, []string{"h4", "h3"}},
		{"keep none", Limits{Keep: 0}, nil, []string{"h4", "h3", "h2", "h1"}},
		{"budget", Limits{Keep: 10, Budget: 160}, []string{"h1", "h2"}, []string{"h4", "h3"}},
		{"budget below running", Limits{Keep: 10, Budget: 50}, nil, []string{"h4", "h3", "h2", "h1"}},
		{"both", Limits{Keep: 3, Budget: 200}, []string{"h1", "h2", "h3"}, []string{"h4"}},
	}
	for _, c := range cases {
		kept, dropped := overLimits("cur", hist, sizes, c.lim)
		if !slices.Equal(ids(kept), c.kept) || !slices.Equal(ids(dropped), c.dropped) {
			t.Errorf("%s: kept %v dropped %v, want %v and %v", c.name, ids(kept), ids(dropped), c.kept, c.dropped)
		}
	}

	if len(hist) != 4 {
		t.Error("overLimits changed its input")
	}
}

func TestPickTarget(t *testing.T) {
	hist := []Kept{{ID: "sha256:aaaa1111"}, {ID: "sha256:bbbb2222"}, {ID: "sha256:cccc3333"}}
	cases := map[string]string{
		"": "sha256:aaaa1111", "1": "sha256:aaaa1111", "3": "sha256:cccc3333",
		"bbbb": "sha256:bbbb2222", "sha256:cccc": "sha256:cccc3333", "BBBB2": "sha256:bbbb2222",
	}
	for to, want := range cases {
		if k, ok := pickTarget(hist, to); !ok || k.ID != want {
			t.Errorf("pickTarget(%q) = %v, %v, want %s", to, k, ok, want)
		}
	}

	for _, to := range []string{"0", "4", "-1", "abc", "dddd"} {
		if k, ok := pickTarget(hist, to); ok {
			t.Errorf("pickTarget(%q) = %v", to, k)
		}
	}

	if _, ok := pickTarget(nil, ""); ok {
		t.Error("an empty history has no target")
	}
}

func TestAssess(t *testing.T) {
	small := []Usage{{Site: "blog", Service: "app", Kept: 50, Running: 40, Worst: 200}}
	if c := assess(small, 1000); len(c.Warnings) != 0 || c.Kept != 50 || c.Worst != 200 {
		t.Errorf("small = %+v", c)
	}

	big := []Usage{
		{Site: "game", Service: "app", Kept: 100, Running: 100, Worst: 900},
		{Site: "blog", Service: "app", Kept: 50, Running: 40, Worst: 400},
	}
	c := assess(big, 1000)
	if len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "could reach 1.3kB") || !strings.Contains(c.Warnings[0], "game/app up to 900B, blog/app") {
		t.Errorf("big = %+v", c.Warnings)
	}

	tight := []Usage{{Site: "game", Service: "app", Kept: 100, Running: 100, Budget: 60, Worst: 160}}
	if c := assess(tight, 10_000); len(c.Warnings) != 1 || !strings.Contains(c.Warnings[0], "keeps no history") {
		t.Errorf("tight = %+v", c.Warnings)
	}

	if w := serviceWorst(100, Limits{Keep: 3}); w != 500 {
		t.Errorf("worst with keep 3 = %d", w)
	}

	if w := serviceWorst(100, Limits{Keep: 3, Budget: 250}); w != 350 {
		t.Errorf("worst with a budget = %d", w)
	}
}
