package sign

import (
	"bytes"
	"errors"
	"net/http"
	"testing"
	"time"
)

func signed(t *testing.T, method, url string, body []byte, key string, at time.Time) *http.Request {
	t.Helper()
	req, err := http.NewRequest(method, url, bytes.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}

	Sign(req, body, key, at)
	return req
}

func TestVerify(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	v := &Verifier{Key: func() string { return "k1" }, Now: func() time.Time { return now }}
	body := []byte(`{"services":["web"]}`)
	url := "https://box.example.com/api/v1/sites/blog/deploy?x=1"

	if err := v.Verify(signed(t, "POST", url, body, "k1", now), body); err != nil {
		t.Fatalf("good request: %v", err)
	}

	cases := []struct {
		name string
		req  *http.Request
		body []byte
		want error
	}{
		{"wrong key", signed(t, "POST", url, body, "k2", now), body, ErrBadSignature},
		{"other body", signed(t, "POST", url, body, "k1", now), []byte(`{"services":["db"]}`), ErrBadSignature},
		{"stale", signed(t, "POST", url, body, "k1", now.Add(-6*time.Minute)), body, ErrStale},
		{"future", signed(t, "POST", url, body, "k1", now.Add(6*time.Minute)), body, ErrStale},
	}
	for _, c := range cases {
		if err := v.Verify(c.req, c.body); !errors.Is(err, c.want) {
			t.Errorf("%s: got %v, want %v", c.name, err, c.want)
		}
	}

	req := signed(t, "POST", url, body, "k1", now)
	req.Method = "DELETE"
	if err := v.Verify(req, body); !errors.Is(err, ErrBadSignature) {
		t.Errorf("other method: %v", err)
	}

	req = signed(t, "POST", url, body, "k1", now)
	req.URL.Path = "/api/v1/sites/other/deploy"
	if err := v.Verify(req, body); !errors.Is(err, ErrBadSignature) {
		t.Errorf("other path: %v", err)
	}

	req = signed(t, "GET", url, nil, "k1", now)
	req.Header.Del(SignatureHeader)
	if err := v.Verify(req, nil); !errors.Is(err, ErrUnsigned) {
		t.Errorf("unsigned: %v", err)
	}
}

func TestReplay(t *testing.T) {
	now := time.Unix(1_800_000_000, 0)
	v := &Verifier{Key: func() string { return "k" }, Now: func() time.Time { return now }}
	req := signed(t, "GET", "https://box.example.com/api/v1/status", nil, "k", now)
	if err := v.Verify(req, nil); err != nil {
		t.Fatal(err)
	}

	if err := v.Verify(req, nil); !errors.Is(err, ErrReplay) {
		t.Errorf("replay: %v", err)
	}

	// After the timestamp expires, replay is rejected before the nonce cache is examined.
	now = now.Add(Window + time.Second)
	if err := v.Verify(req, nil); !errors.Is(err, ErrStale) {
		t.Errorf("late replay: %v", err)
	}

	// Nonce cleanup happens during the next authenticated request, not on a timer.
	if err := v.Verify(signed(t, "GET", "https://box.example.com/api/v1/status", nil, "k", now), nil); err != nil {
		t.Fatal(err)
	}

	if len(v.seen) != 1 {
		t.Errorf("nonces kept: %d, want 1", len(v.seen))
	}
}

func TestNoKey(t *testing.T) {
	v := &Verifier{Key: func() string { return "" }}
	req := signed(t, "GET", "https://box.example.com/api/v1/status", nil, "", time.Now())
	if err := v.Verify(req, nil); !errors.Is(err, ErrNoKey) {
		t.Errorf("no key: %v", err)
	}
}
