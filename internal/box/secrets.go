package box

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"
)

func secretPath(name string) string { return filepath.Join(secretsDir(), name) }

// The dot in api.key prevents a collision with a valid site name.
func apiKeyPath() string { return filepath.Join(secretsDir(), "api.key") }

// NewSecret returns 32 cryptographically random bytes encoded as 64 lowercase hexadecimal
// digits.
func NewSecret() string {
	b := make([]byte, 32)
	rand.Read(b)
	return hex.EncodeToString(b)
}

var keyPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)

// HookSecret validates name and reads the site secret. A missing file returns an empty string
// without an error.
func HookSecret(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}

	return readSecret(secretPath(name))
}

// EnsureHookSecret returns the existing site secret or creates and saves one if it is missing
// or empty.
func EnsureHookSecret(name string) (string, error) {
	s, err := HookSecret(name)
	if err != nil || s != "" {
		return s, err
	}

	return RotateHookSecret(name)
}

// RotateHookSecret generates and saves a replacement site secret. Successful replacement
// invalidates the old secret for subsequent webhook requests.
func RotateHookSecret(name string) (string, error) {
	if err := ValidName(name); err != nil {
		return "", err
	}

	s := NewSecret()
	return s, writeSecret(secretPath(name), s)
}

// APIKey reads the stored command API key. A missing file returns an empty string without an
// error.
func APIKey() (string, error) { return readSecret(apiKeyPath()) }

// SetAPIKey validates and writes a 64-character lowercase hexadecimal key with mode 0600.
// Callers must update any in-memory copy after saving it.
func SetAPIKey(key string) error {
	if !keyPattern.MatchString(key) {
		return Invalid(fmt.Errorf("an API key is 64 lowercase hex digits"))
	}

	return writeSecret(apiKeyPath(), key)
}

func readSecret(path string) (string, error) {
	b, err := os.ReadFile(path)
	if errors.Is(err, os.ErrNotExist) {
		return "", nil
	}

	return strings.TrimSpace(string(b)), err
}

func writeSecret(path, s string) error {
	if err := os.MkdirAll(secretsDir(), 0o700); err != nil {
		return err
	}

	return writeFileAtomic(path, []byte(s+"\n"), 0o600)
}
