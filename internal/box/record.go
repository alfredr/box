package box

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"syscall"
	"time"
)

// Record stores deployment history and update state in state/<name>.json under Root. Image maps
// are keyed by Compose service name. A zero value represents a site with no recorded
// operations.
type Record struct {
	// DeployedAt is updated after a successful deploy, edit, or rollback.
	DeployedAt time.Time `json:"deployed_at,omitzero"`
	// CheckedAt records when the most recent image pull check began.
	CheckedAt time.Time `json:"checked_at,omitzero"`
	// Result describes the recorded deployment or rollback outcome.
	Result string `json:"result,omitempty"`
	// CheckError holds the most recent pull failure. A successful pull check clears it.
	CheckError string `json:"check_error,omitempty"`
	// Images maps services to the image IDs recorded after successful operations.
	Images map[string]string `json:"images,omitempty"`
	// Previous holds the prior image for each service that changed. Unchanged services retain
	// their existing history.
	Previous map[string]string `json:"previous,omitempty"`
	// Pending holds image IDs pulled for manual services and awaiting deployment.
	Pending map[string]string `json:"pending,omitempty"`
	// Rejected holds image IDs excluded from checks after a failed deployment or rollback.
	// Explicit deployment can retry them.
	Rejected map[string]string `json:"rejected,omitempty"`
}

func recordPath(name string) string { return filepath.Join(stateDir(), name+".json") }

// LoadRecord reads the saved state for name. A missing file returns a zero Record without an
// error. The caller must validate name.
func LoadRecord(name string) (Record, error) {
	var r Record
	b, err := os.ReadFile(recordPath(name))
	if errors.Is(err, os.ErrNotExist) {
		return r, nil
	}

	if err != nil {
		return r, err
	}

	if err := json.Unmarshal(b, &r); err != nil {
		return r, fmt.Errorf("%s: %w", recordPath(name), err)
	}

	return r, nil
}

func (r Record) save(name string) error {
	b, err := json.MarshalIndent(r, "", "  ")
	if err != nil {
		return err
	}

	return writeFileAtomic(recordPath(name), append(b, '\n'), 0o644)
}

func put(m *map[string]string, k, v string) {
	if *m == nil {
		*m = map[string]string{}
	}

	(*m)[k] = v
}

// lockSite acquires an exclusive file lock for mutating operations on one site. The caller must
// validate name and call the returned function to release the lock. Waiting for the lock is not
// cancellable.
func lockSite(name string) (func(), error) {
	if err := os.MkdirAll(stateDir(), 0o755); err != nil {
		return nil, err
	}

	f, err := os.OpenFile(filepath.Join(stateDir(), name+".lock"), os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, err
	}

	if err := syscall.Flock(int(f.Fd()), syscall.LOCK_EX); err != nil {
		f.Close()
		return nil, err
	}

	return func() { f.Close() }, nil
}
