// Package client stores server profiles, signs command API requests, and manages the server
// container through Docker over SSH.
package client

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	"connectrpc.com/connect"

	"github.com/alfredr/box/internal/gen/box/v1/boxv1connect"
	"github.com/alfredr/box/internal/sign"
)

// Profile contains the credentials and connection details for one box server.
type Profile struct {
	// URL is the command API base URL, normally an HTTPS address.
	URL string `json:"url"`
	// Key signs command API requests and grants control of the server Docker daemon.
	Key string `json:"key"`
	// SSH is the SSH destination used for server maintenance, such as root@example.com. It may
	// also be an alias from the local SSH configuration.
	SSH string `json:"ssh,omitempty"`
}

// Profiles stores named server connections and the default selection. Changes remain in memory
// until Save is called.
type Profiles struct {
	Default string             `json:"default,omitempty"`
	Servers map[string]Profile `json:"servers,omitempty"`
}

// ProfilesPath returns BOX_CONFIG when set, otherwise box/servers.json under the platform
// configuration directory. If the configuration directory cannot be determined, it uses
// box/servers.json under the working directory.
func ProfilesPath() string {
	if p := os.Getenv("BOX_CONFIG"); p != "" {
		return p
	}

	dir, err := os.UserConfigDir()
	if err != nil {
		dir = "."
	}

	return filepath.Join(dir, "box", "servers.json")
}

// LoadProfiles reads the file selected by ProfilesPath. A missing file returns an empty
// Profiles value without an error.
func LoadProfiles() (Profiles, error) {
	var p Profiles
	b, err := os.ReadFile(ProfilesPath())
	if errors.Is(err, os.ErrNotExist) {
		return p, nil
	}

	if err != nil {
		return p, err
	}

	if err := json.Unmarshal(b, &p); err != nil {
		return p, fmt.Errorf("%s: %w", ProfilesPath(), err)
	}

	return p, nil
}

// Save writes profiles as JSON through a temporary file and renames it to ProfilesPath. New
// directories use mode 0700 and the temporary file is created with mode 0600 because it
// contains API keys.
func (p Profiles) Save() error {
	b, err := json.MarshalIndent(p, "", "  ")
	if err != nil {
		return err
	}

	path := ProfilesPath()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}

	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, append(b, '\n'), 0o600); err != nil {
		return err
	}

	return os.Rename(tmp, path)
}

// Put adds or replaces a profile in memory. It selects that name as the default if no default
// is set.
func (p *Profiles) Put(name string, pr Profile) {
	if p.Servers == nil {
		p.Servers = map[string]Profile{}
	}

	p.Servers[name] = pr
	if p.Default == "" {
		p.Default = name
	}
}

// Names returns the saved profile names in sorted order.
func (p Profiles) Names() []string {
	var names []string
	for n := range p.Servers {
		names = append(names, n)
	}

	slices.Sort(names)
	return names
}

// Pick selects a profile by name, then BOX_SERVER, then the saved default, then the sole
// profile if there is only one. It returns an error if the selection is missing or ambiguous.
func (p Profiles) Pick(name string) (string, Profile, error) {
	if name == "" {
		name = os.Getenv("BOX_SERVER")
	}

	if name == "" {
		name = p.Default
	}

	if name == "" && len(p.Servers) == 1 {
		name = p.Names()[0]
	}

	if name == "" {
		if len(p.Servers) == 0 {
			return "", Profile{}, errors.New("no servers yet: box setup root@your-server --domain box.example.com")
		}

		return "", Profile{}, fmt.Errorf("several servers and no default: pick one with --server (%s)", strings.Join(p.Names(), ", "))
	}

	pr, ok := p.Servers[name]
	if !ok {
		return "", Profile{}, fmt.Errorf("no server %q in %s", name, ProfilesPath())
	}

	return name, pr, nil
}

// New returns a Connect client that signs requests with the profile key. It removes trailing
// slashes from the base URL and uses the default HTTP transport without a client-wide timeout.
func New(p Profile) boxv1connect.BoxServiceClient {
	hc := &http.Client{Transport: &signer{key: p.Key, next: http.DefaultTransport}}
	return boxv1connect.NewBoxServiceClient(hc, strings.TrimSuffix(p.URL, "/"))
}

type signer struct {
	key  string
	next http.RoundTripper
}

// RoundTrip reads and closes the original request body, signs a cloned request, and passes it
// to the next transport.
func (s *signer) RoundTrip(req *http.Request) (*http.Response, error) {
	var body []byte
	if req.Body != nil {
		b, err := io.ReadAll(req.Body)
		req.Body.Close()
		if err != nil {
			return nil, err
		}

		body = b
	}

	// Clone the request before changing headers to preserve the RoundTripper contract.
	signed := req.Clone(req.Context())
	signed.Body = io.NopCloser(bytes.NewReader(body))
	signed.ContentLength = int64(len(body))
	sign.Sign(signed, body, s.key, time.Now())
	return s.next.RoundTrip(signed)
}

// Message returns an error message suitable for CLI output, omitting the Connect status code
// prefix. The error must be non-nil.
func Message(err error) string {
	var ce *connect.Error
	if errors.As(err, &ce) {
		return ce.Message()
	}

	return err.Error()
}
