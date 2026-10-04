package box

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
)

var daemonPath = "/etc/docker/daemon.json"

// DaemonSettings describes the daemon.json fields managed by box. Zero numeric values, an empty
// LogSize, and a nil LiveRestore leave their settings unchanged.
type DaemonSettings struct {
	// LogSize sets max-size and must be a positive integer followed by k, m, or g.
	LogSize string
	// LogFiles sets max-file. Negative values are rejected.
	LogFiles int
	// LiveRestore sets whether containers keep running while the daemon restarts. A non-nil
	// pointer can explicitly enable or disable it.
	LiveRestore *bool
}

var logSizePattern = regexp.MustCompile(`^[1-9][0-9]*[kmg]$`)

// MergeDaemon validates and merges settings into a daemon.json document while preserving
// unrelated fields. It returns formatted JSON and whether the decoded settings changed. It does
// not write a file or restart Docker.
func MergeDaemon(current []byte, s DaemonSettings) ([]byte, bool, error) {
	m := map[string]any{}
	if len(bytes.TrimSpace(current)) > 0 {
		if err := json.Unmarshal(current, &m); err != nil {
			return nil, false, fmt.Errorf("daemon.json: %w", err)
		}
	}

	// Compare normalized JSON so whitespace and object key order do not count as setting
	// changes.
	before, err := json.Marshal(m)
	if err != nil {
		return nil, false, err
	}

	opts, _ := m["log-opts"].(map[string]any)
	if opts == nil {
		opts = map[string]any{}
	}

	if s.LogSize != "" {
		if !logSizePattern.MatchString(s.LogSize) {
			return nil, false, fmt.Errorf("log size %q: want a number and k, m or g, like 10m", s.LogSize)
		}

		opts["max-size"] = s.LogSize
	}

	if s.LogFiles < 0 {
		return nil, false, fmt.Errorf("log files %d: want at least 1", s.LogFiles)
	}

	if s.LogFiles > 0 {
		opts["max-file"] = strconv.Itoa(s.LogFiles)
	}

	if len(opts) > 0 {
		m["log-opts"] = opts
	}

	if s.LiveRestore != nil {
		m["live-restore"] = *s.LiveRestore
	}

	after, err := json.Marshal(m)
	if err != nil {
		return nil, false, err
	}

	out, err := json.MarshalIndent(m, "", "  ")
	if err != nil {
		return nil, false, err
	}

	return append(out, '\n'), !bytes.Equal(before, after), nil
}

// DaemonSummary formats logging and live-restore settings for the CLI. Invalid JSON produces an
// explanatory line in the result.
func DaemonSummary(data []byte) []string {
	m := map[string]any{}
	if len(bytes.TrimSpace(data)) > 0 {
		if err := json.Unmarshal(data, &m); err != nil {
			return []string{"daemon.json doesn't parse: " + err.Error()}
		}
	}

	driver, _ := m["log-driver"].(string)
	if driver == "" {
		driver = "json-file"
	}

	opts, _ := m["log-opts"].(map[string]any)
	logs := fmt.Sprintf("logs          %s", driver)
	if size, ok := opts["max-size"].(string); ok {
		logs += ", max-size " + size
	} else {
		logs += ", no rotation (box docker config --log-size 10m --log-files 3)"
	}

	if files, ok := opts["max-file"].(string); ok {
		logs += ", max-file " + files
	}

	live := "off"
	if on, _ := m["live-restore"].(bool); on {
		live = "on"
	}

	return []string{logs, "live-restore  " + live}
}

// ReadDaemon reads the Docker daemon configuration. A missing file returns nil data without an
// error.
func ReadDaemon() ([]byte, error) {
	b, err := os.ReadFile(daemonPath)
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}

	return b, err
}

// Registries returns sorted registry names from Docker auths and credHelpers entries. It uses
// DOCKER_CONFIG, falling back to ~/.docker. Missing or invalid configuration returns an empty
// list.
func Registries() []string {
	dir := os.Getenv("DOCKER_CONFIG")
	if dir == "" {
		home, _ := os.UserHomeDir()
		dir = filepath.Join(home, ".docker")
	}

	b, err := os.ReadFile(filepath.Join(dir, "config.json"))
	if err != nil {
		return nil
	}

	var c struct {
		Auths       map[string]json.RawMessage `json:"auths"`
		CredHelpers map[string]string          `json:"credHelpers"`
	}
	if json.Unmarshal(b, &c) != nil {
		return nil
	}

	var out []string
	for r := range c.Auths {
		out = append(out, r)
	}

	for r := range c.CredHelpers {
		if !slices.Contains(out, r) {
			out = append(out, r)
		}
	}

	slices.Sort(out)
	return out
}
