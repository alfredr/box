// Package sign authenticates HTTP requests with HMAC-SHA256 and tracks recent nonces to reject
// replay. Signatures cover the method, request URI, timestamp, nonce, and SHA-256 body hash.
// Keys are used as string bytes without hexadecimal decoding.
package sign

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// TimeHeader carries the request timestamp as decimal Unix seconds.
	TimeHeader = "X-Box-Time"
	// NonceHeader carries the random value used to detect replay.
	NonceHeader = "X-Box-Nonce"
	// SignatureHeader carries the hexadecimal HMAC-SHA256 signature.
	SignatureHeader = "X-Box-Signature"
	// Window is the maximum accepted clock difference in either direction, including its
	// endpoints.
	Window = 5 * time.Minute
)

var (
	// ErrNoKey indicates that the verifier has no configured signing key.
	ErrNoKey = errors.New("the server has no API key yet (box setup installs one)")
	// ErrUnsigned indicates a missing authentication header or an invalid timestamp encoding.
	ErrUnsigned = errors.New("the request isn't signed")
	// ErrStale indicates a timestamp outside the accepted clock window.
	ErrStale = errors.New("the request's time is more than 5 minutes from the server's clock")
	// ErrReplay indicates that a valid request reused a nonce still held by the verifier.
	ErrReplay = errors.New("the request was already used")
	// ErrBadSignature indicates an invalid hexadecimal signature or an HMAC mismatch.
	ErrBadSignature = errors.New("bad signature (wrong key?)")
)

func digest(key, method, uri, ts, nonce string, body []byte) []byte {
	sum := sha256.Sum256(body)
	mac := hmac.New(sha256.New, []byte(key))
	fmt.Fprintf(mac, "%s\n%s\n%s\n%s\n%x", method, uri, ts, nonce, sum)
	return mac.Sum(nil)
}

// Sign adds authentication headers to req using now and a fresh nonce. The body argument must
// contain the exact bytes that will be sent. Sign does not read or replace req.Body, and
// req.Header must be initialized.
func Sign(req *http.Request, body []byte, key string, now time.Time) {
	ts := strconv.FormatInt(now.Unix(), 10)
	nonce := rand.Text()
	req.Header.Set(TimeHeader, ts)
	req.Header.Set(NonceHeader, nonce)
	req.Header.Set(SignatureHeader, hex.EncodeToString(digest(key, req.Method, req.URL.RequestURI(), ts, nonce, body)))
}

// Verifier checks request signatures and keeps accepted nonces in memory until their timestamps
// expire. Verify calls may run concurrently, provided the Key and Now callbacks are safe for
// concurrent use. The verifier must not be copied after use.
type Verifier struct {
	// Key returns the current signing key. It must be non-nil. An empty result rejects all
	// requests.
	Key func() string
	// Now supplies the verification clock. A nil callback uses time.Now.
	Now func() time.Time

	mu   sync.Mutex
	seen map[string]time.Time
}

// Verify checks the authentication headers against body and records the nonce on success. The
// body argument must contain the bytes received from the client. Verify does not consume r.Body.
// It rejects reuse of an accepted nonce until the original timestamp expires.
func (v *Verifier) Verify(r *http.Request, body []byte) error {
	key := v.Key()
	if key == "" {
		return ErrNoKey
	}

	ts, nonce, sig := r.Header.Get(TimeHeader), r.Header.Get(NonceHeader), r.Header.Get(SignatureHeader)
	if ts == "" || nonce == "" || sig == "" {
		return ErrUnsigned
	}

	now := time.Now()
	if v.Now != nil {
		now = v.Now()
	}

	sec, err := strconv.ParseInt(ts, 10, 64)
	if err != nil {
		return ErrUnsigned
	}

	t := time.Unix(sec, 0)
	if t.Before(now.Add(-Window)) || t.After(now.Add(Window)) {
		return ErrStale
	}

	got, err := hex.DecodeString(sig)
	if err != nil || !hmac.Equal(got, digest(key, r.Method, r.URL.RequestURI(), ts, nonce, body)) {
		return ErrBadSignature
	}

	// Only authenticated requests may add nonces to the replay cache.
	v.mu.Lock()
	defer v.mu.Unlock()

	if v.seen == nil {
		v.seen = map[string]time.Time{}
	}

	for n, until := range v.seen {
		if now.After(until) {
			delete(v.seen, n)
		}
	}

	if _, ok := v.seen[nonce]; ok {
		return ErrReplay
	}

	// Retain the nonce until the request timestamp is too old to pass verification, including
	// requests dated ahead of the server clock.
	v.seen[nonce] = t.Add(Window)
	return nil
}
