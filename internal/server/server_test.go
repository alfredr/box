package server

import (
	"bytes"
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"connectrpc.com/connect"

	"github.com/alfredr/box/internal/box"
	"github.com/alfredr/box/internal/client"
	boxv1 "github.com/alfredr/box/internal/gen/box/v1"
	"github.com/alfredr/box/internal/gen/box/v1/boxv1connect"
	"github.com/alfredr/box/internal/sign"
)

// start creates an HTTP test server with isolated storage and a matching signed client. It does
// not prepare Docker resources or start background polling.
func start(t *testing.T) (*Server, *httptest.Server, boxv1connect.BoxServiceClient, string) {
	t.Helper()
	t.Setenv("BOX_ROOT", t.TempDir())
	key := box.NewSecret()
	if err := box.SetAPIKey(key); err != nil {
		t.Fatal(err)
	}

	s, err := New(Options{Listen: "127.0.0.1:9311", Version: "test"})
	if err != nil {
		t.Fatal(err)
	}

	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts, client.New(client.Profile{URL: ts.URL, Key: key}), key
}

func code(t *testing.T, err error, want connect.Code) {
	t.Helper()
	if got := connect.CodeOf(err); err == nil || got != want {
		t.Errorf("got %v (%v), want %v", err, got, want)
	}
}

func TestVersionAndAuth(t *testing.T) {
	_, ts, c, _ := start(t)
	ctx := context.Background()

	v, err := c.Version(ctx, &boxv1.VersionRequest{})
	if err != nil || v.Version != "test" {
		t.Fatalf("version = %v, %v", v, err)
	}

	wrong := client.New(client.Profile{URL: ts.URL, Key: box.NewSecret()})
	_, err = wrong.Version(ctx, &boxv1.VersionRequest{})
	code(t, err, connect.CodeUnauthenticated)
	if !strings.Contains(client.Message(err), "wrong key") {
		t.Errorf("message = %q", client.Message(err))
	}

	resp, err := http.Post(ts.URL+boxv1connect.BoxServiceVersionProcedure, "application/json", strings.NewReader("{}"))
	if err != nil {
		t.Fatal(err)
	}

	resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Errorf("unsigned: %d", resp.StatusCode)
	}
}

func TestReplayRefused(t *testing.T) {
	_, ts, _, key := start(t)
	body := []byte("{}")
	req, _ := http.NewRequest(http.MethodPost, ts.URL+boxv1connect.BoxServiceGetConfigProcedure, bytes.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	sign.Sign(req, body, key, time.Now())

	var codes []int
	for range 2 {
		again := req.Clone(context.Background())
		again.Body, again.ContentLength = io.NopCloser(bytes.NewReader(body)), int64(len(body))
		resp, err := http.DefaultClient.Do(again)
		if err != nil {
			t.Fatal(err)
		}

		resp.Body.Close()
		codes = append(codes, resp.StatusCode)
	}

	if !slices.Equal(codes, []int{http.StatusOK, http.StatusUnauthorized}) {
		t.Errorf("codes = %v", codes)
	}
}

func TestConfig(t *testing.T) {
	_, _, c, _ := start(t)
	ctx := context.Background()

	_, err := c.SetConfig(ctx, &boxv1.SetConfigRequest{Key: "poll", Value: "30s"})
	code(t, err, connect.CodeInvalidArgument)

	if _, err := c.SetConfig(ctx, &boxv1.SetConfigRequest{Key: "domain", Value: "box.example.com"}); err != nil {
		t.Fatal(err)
	}

	out, err := c.GetConfig(ctx, &boxv1.GetConfigRequest{})
	if err != nil {
		t.Fatal(err)
	}

	values := map[string]string{}
	var keys []string
	for _, s := range out.Settings {
		values[s.Key] = s.Value
		keys = append(keys, s.Key)
	}

	if !slices.Equal(keys, box.ConfigKeys) || values["domain"] != "box.example.com" || values["poll"] != box.DefaultPoll {
		t.Errorf("config = %v", out.Settings)
	}

	caddyfile, err := os.ReadFile(filepath.Join(box.Root(), "proxy", "caddy", "Caddyfile"))
	if err != nil || !strings.Contains(string(caddyfile), "box.example.com {") || !strings.Contains(string(caddyfile), "reverse_proxy 127.0.0.1:9311") {
		t.Errorf("Caddyfile = %s, %v", caddyfile, err)
	}
}

func TestHooks(t *testing.T) {
	s, ts, c, key := start(t)
	ctx := context.Background()

	_, err := c.GetHook(ctx, &boxv1.GetHookRequest{Site: "blog"})
	code(t, err, connect.CodeNotFound)

	site := box.NewSite{Name: "blog", Image: "nginx:alpine", Domains: []string{"example.com"}, Port: 80, Policy: box.Auto}
	if err := box.CreateSite(site); err != nil {
		t.Fatal(err)
	}

	_, err = c.GetHook(ctx, &boxv1.GetHookRequest{Site: "blog"})
	code(t, err, connect.CodeInvalidArgument)

	if _, err := c.SetConfig(ctx, &boxv1.SetConfigRequest{Key: "domain", Value: "box.example.com"}); err != nil {
		t.Fatal(err)
	}

	got, err := c.GetHook(ctx, &boxv1.GetHookRequest{Site: "blog"})
	if err != nil {
		t.Fatal(err)
	}

	h := got.Hook
	if h.Url != "https://box.example.com/deploy/blog" || len(h.Secret) != 64 {
		t.Errorf("hook = %v", h)
	}

	rotated, err := c.RotateHook(ctx, &boxv1.RotateHookRequest{Site: "blog"})
	if err != nil || rotated.Hook.Secret == h.Secret {
		t.Fatalf("rotate = %v, %v", rotated, err)
	}

	list, err := c.ListHooks(ctx, &boxv1.ListHooksRequest{})
	if err != nil || len(list.Hooks) != 1 || list.Hooks[0].Secret != "" {
		t.Errorf("list = %v, %v", list, err)
	}

	// Replace the check function so authentication can be tested without invoking Docker.
	ran := make(chan string, 1)
	s.hooks.run = func(site string) { ran <- site }
	post := func(token string) int {
		req, _ := http.NewRequest(http.MethodPost, ts.URL+"/deploy/blog", strings.NewReader("{}"))
		req.Header.Set("Authorization", "Bearer "+token)
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}

		resp.Body.Close()
		return resp.StatusCode
	}

	if code := post(key); code != http.StatusUnauthorized {
		t.Errorf("API key on the webhook: %d", code)
	}

	if code := post(h.Secret); code != http.StatusUnauthorized {
		t.Errorf("old secret: %d", code)
	}

	if code := post(rotated.Hook.Secret); code != http.StatusAccepted {
		t.Errorf("webhook: %d", code)
	}

	if site := <-ran; site != "blog" {
		t.Errorf("ran %q", site)
	}

	s.hooks.wait()
}

func TestKeyRotation(t *testing.T) {
	_, ts, c, _ := start(t)
	ctx := context.Background()
	next := box.NewSecret()
	if _, err := c.RotateKey(ctx, &boxv1.RotateKeyRequest{Key: next}); err != nil {
		t.Fatal(err)
	}

	_, err := c.Version(ctx, &boxv1.VersionRequest{})
	code(t, err, connect.CodeUnauthenticated)

	fresh := client.New(client.Profile{URL: ts.URL, Key: next})
	if _, err := fresh.Version(ctx, &boxv1.VersionRequest{}); err != nil {
		t.Errorf("new key: %v", err)
	}

	if k, _ := box.APIKey(); k != next {
		t.Error("the new key wasn't saved")
	}

	_, err = fresh.RotateKey(ctx, &boxv1.RotateKeyRequest{Key: "short"})
	code(t, err, connect.CodeInvalidArgument)
}

func TestCreateSiteRefuses(t *testing.T) {
	_, _, c, _ := start(t)
	_, err := c.CreateSite(context.Background(), &boxv1.CreateSiteRequest{
		Name: "Blog", Image: "nginx", Domains: []string{"example.com"}, Port: 80, Policy: "auto",
	})
	code(t, err, connect.CodeInvalidArgument)

	_, err = c.CreateSite(context.Background(), &boxv1.CreateSiteRequest{
		Name: "blog", Image: "nginx", Domains: []string{"example.com"}, Port: 80, Policy: "sometimes",
	})
	code(t, err, connect.CodeInvalidArgument)
}

func TestAuthorized(t *testing.T) {
	body := []byte(`{"action":"published"}`)
	mac := hmac.New(sha256.New, []byte("s3cret"))
	mac.Write(body)
	sig := "sha256=" + hex.EncodeToString(mac.Sum(nil))

	cases := []struct {
		name   string
		header map[string]string
		want   bool
	}{
		{"bearer", map[string]string{"Authorization": "Bearer s3cret"}, true},
		{"wrong bearer", map[string]string{"Authorization": "Bearer s3cre"}, false},
		{"basic", map[string]string{"Authorization": "Basic s3cret"}, false},
		{"github", map[string]string{"X-Hub-Signature-256": sig}, true},
		{"github wrong", map[string]string{"X-Hub-Signature-256": "sha256=00ff"}, false},
		{"github not hex", map[string]string{"X-Hub-Signature-256": "sha256=zz"}, false},
		{"nothing", nil, false},
	}
	for _, c := range cases {
		h := http.Header{}
		for k, v := range c.header {
			h.Set(k, v)
		}

		if got := Authorized(h, body, "s3cret"); got != c.want {
			t.Errorf("%s: got %v", c.name, got)
		}
	}
}

func TestPokeCoalesces(t *testing.T) {
	release := make(chan struct{})
	started := make(chan struct{}, 10)
	var runs atomic.Int32
	r := &runner{run: func(string) {
		runs.Add(1)
		started <- struct{}{}
		<-release
	}}

	r.poke("blog")
	<-started
	r.poke("blog")
	r.poke("blog")
	r.poke("blog")
	// Requests received during the blocked first check must produce exactly one follow-up
	// check.
	release <- struct{}{}
	<-started
	release <- struct{}{}
	r.wait()

	if n := runs.Load(); n != 2 {
		t.Errorf("runs = %d, want 2", n)
	}
}

func TestPokeRunsSitesInParallel(t *testing.T) {
	var wg sync.WaitGroup
	wg.Add(2)
	both := make(chan struct{})
	r := &runner{run: func(string) {
		wg.Done()
		<-both
	}}

	r.poke("a")
	r.poke("b")
	wg.Wait()
	close(both)
	r.wait()
}

func TestBodyLimit(t *testing.T) {
	_, _, c, _ := start(t)
	_, err := c.SetConfig(context.Background(), &boxv1.SetConfigRequest{Key: "notify", Value: strings.Repeat("x", 5<<20)})
	code(t, err, connect.CodeResourceExhausted)
}
