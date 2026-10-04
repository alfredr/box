package server

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/hex"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/alfredr/box/internal/box"
)

// hook authenticates a site webhook and queues a check. A successful response confirms
// acceptance, not deployment success. Authenticated GitHub ping events return without
// scheduling work.
func (s *Server) hook(w http.ResponseWriter, r *http.Request) {
	site := r.PathValue("site")
	// Use the same unauthorized response for unknown sites and invalid secrets to avoid
	// disclosing site names.
	secret := ""
	if box.ValidName(site) == nil {
		secret, _ = box.HookSecret(site)
	}

	body, err := io.ReadAll(http.MaxBytesReader(w, r.Body, 1<<20))
	if err != nil {
		http.Error(w, "body too large", http.StatusRequestEntityTooLarge)
		return
	}

	if secret == "" || !Authorized(r.Header, body, secret) {
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	if r.Header.Get("X-GitHub-Event") == "ping" {
		io.WriteString(w, "pong\n")
		return
	}

	s.hooks.poke(site)
	w.WriteHeader(http.StatusAccepted)
	io.WriteString(w, "checking "+site+"\n")
}

// Authorized checks a bearer token or a GitHub HMAC-SHA256 body signature against secret. A
// bearer header takes precedence when both are present. The caller must reject an empty
// configured secret.
func Authorized(header http.Header, body []byte, secret string) bool {
	if token, ok := strings.CutPrefix(header.Get("Authorization"), "Bearer "); ok {
		return subtle.ConstantTimeCompare([]byte(token), []byte(secret)) == 1
	}

	if sig, ok := strings.CutPrefix(header.Get("X-Hub-Signature-256"), "sha256="); ok {
		got, err := hex.DecodeString(sig)
		if err != nil {
			return false
		}

		mac := hmac.New(sha256.New, []byte(secret))
		mac.Write(body)
		return hmac.Equal(got, mac.Sum(nil))
	}

	return false
}

// runner permits one active check per site. Different sites may run concurrently, and repeated
// requests for a busy site are combined into one additional check.
type runner struct {
	run func(site string)

	mu    sync.Mutex
	busy  map[string]bool
	again map[string]bool
	wg    sync.WaitGroup
}

// poke starts work asynchronously, or records a request to run once more when the current check
// finishes.
func (r *runner) poke(site string) {
	r.mu.Lock()
	defer r.mu.Unlock()

	if r.busy == nil {
		r.busy, r.again = map[string]bool{}, map[string]bool{}
	}

	if r.busy[site] {
		r.again[site] = true
		return
	}

	r.busy[site] = true
	r.wg.Go(func() { r.loop(site) })
}

func (r *runner) loop(site string) {
	for {
		r.run(site)

		r.mu.Lock()
		if !r.again[site] {
			delete(r.busy, site)
			r.mu.Unlock()
			return
		}

		delete(r.again, site)
		r.mu.Unlock()
	}
}

func (r *runner) wait() { r.wg.Wait() }

// poller waits for the configured interval after each completed pass. Changing the interval
// restarts the wait.
type poller struct {
	// wake interrupts the current wait so the polling interval can be read again.
	wake chan struct{}

	mu         sync.Mutex
	last, next time.Time
}

func (p *poller) run(ctx context.Context) {
	for {
		cfg, err := box.LoadConfig()
		if err != nil {
			slog.Error("loading config", "err", err)
		}

		var tick <-chan time.Time
		every := cfg.PollEvery()
		p.mu.Lock()
		p.next = time.Time{}
		if every > 0 {
			p.next = time.Now().Add(every)
			tick = time.After(every)
		}
		p.mu.Unlock()

		select {
		case <-ctx.Done():
			return
		case <-p.wake:
			continue
		case <-tick:
		}

		// Finish the current polling pass after cancellation so an in-progress deployment can
		// complete or recover.
		results, err := box.CheckAll(context.WithoutCancel(ctx), cfg)
		if err != nil {
			slog.Error("polling", "err", err)
		}

		for _, r := range results {
			if r.Err != nil || len(r.Deployed)+len(r.Waiting) > 0 {
				slog.Info("poll", "site", r.Site, "deployed", len(r.Deployed), "waiting", len(r.Waiting), "err", r.Err)
			}
		}

		p.mu.Lock()
		p.last = time.Now()
		p.mu.Unlock()
	}
}

func (p *poller) nudge() {
	select {
	case p.wake <- struct{}{}:
	default:
	}
}

func (p *poller) times() (last, next time.Time) {
	p.mu.Lock()
	defer p.mu.Unlock()

	return p.last, p.next
}
