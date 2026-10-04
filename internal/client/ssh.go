package client

import (
	"bytes"
	"context"
	_ "embed"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/alfredr/box/internal/box"
	"github.com/alfredr/box/internal/engine"
)

// Host identifies a remote Docker host through the system SSH client. SSH configuration,
// agents, and jump hosts are handled by that client.
type Host struct {
	Target string
}

const remoteSocket = "/var/run/docker.sock"

// Run executes command in the remote login shell with a pseudo-terminal and local stdin,
// stdout, and stderr attached. The command is passed without additional shell quoting.
func (h Host) Run(ctx context.Context, command string) error {
	cmd := exec.CommandContext(ctx, "ssh", "-t", h.Target, command)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	if err := cmd.Run(); err != nil {
		return fmt.Errorf("%s on %s: %w", command, h.Target, err)
	}

	return nil
}

// Output executes command in the remote login shell and returns trimmed stdout. On failure, the
// error includes stderr when available.
func (h Host) Output(ctx context.Context, command string) (string, error) {
	var out, errb bytes.Buffer
	cmd := exec.CommandContext(ctx, "ssh", h.Target, command)
	cmd.Stdout, cmd.Stderr = &out, &errb
	if err := cmd.Run(); err != nil {
		msg := strings.TrimSpace(errb.String())
		if msg == "" {
			msg = err.Error()
		}

		return "", fmt.Errorf("%s on %s: %s", command, h.Target, msg)
	}

	return strings.TrimSpace(out.String()), nil
}

// sudo checks the remote effective user ID and prefixes command with sudo for non-root
// accounts.
func (h Host) sudo(ctx context.Context, command string) (string, error) {
	uid, err := h.Output(ctx, "id -u")
	if err != nil {
		return "", err
	}

	if uid == "0" {
		return command, nil
	}

	return "sudo " + command, nil
}

// ErrNoDocker indicates that the forwarded Docker socket did not answer a ping. Docker may be
// absent, stopped, or inaccessible to the SSH account.
var ErrNoDocker = errors.New("no Docker to talk to")

// Docker forwards the remote Docker socket to a temporary local Unix socket and verifies that
// the daemon responds. The returned cleanup function must be called exactly once to stop SSH
// and remove the local socket. Cancelling ctx after this function returns does not close the
// forward.
func (h Host) Docker(ctx context.Context) (*engine.Client, func(), error) {
	dir, err := os.MkdirTemp("", "box-ssh-")
	if err != nil {
		return nil, nil, err
	}

	local := filepath.Join(dir, "docker.sock")
	var errb bytes.Buffer
	cmd := exec.Command("ssh", "-N",
		"-o", "ExitOnForwardFailure=yes",
		"-o", "StreamLocalBindUnlink=yes",
		"-L", local+":"+remoteSocket,
		h.Target)
	cmd.Stderr = &errb
	if err := cmd.Start(); err != nil {
		os.RemoveAll(dir)
		return nil, nil, err
	}

	exited := make(chan error, 1)
	go func() { exited <- cmd.Wait() }()

	closeFn := func() {
		cmd.Process.Kill()
		<-exited
		os.RemoveAll(dir)
	}

	// Wait for SSH to create the local socket before attempting an API request.
	deadline := time.Now().Add(30 * time.Second)
	for {
		if _, err := os.Stat(local); err == nil {
			break
		}

		select {
		case err := <-exited:
			exited <- err
			closeFn()
			return nil, nil, fmt.Errorf("ssh %s: %s", h.Target, strings.TrimSpace(errb.String()))
		case <-ctx.Done():
			closeFn()
			return nil, nil, ctx.Err()
		case <-time.After(100 * time.Millisecond):
		}

		if time.Now().After(deadline) {
			closeFn()
			return nil, nil, fmt.Errorf("ssh %s: the Docker socket forward didn't come up", h.Target)
		}
	}

	// A local forwarding socket can exist even when the remote socket is unavailable. Confirm
	// the daemon answers before returning the client.
	eng := engine.Unix(local)
	pingCtx, cancel := context.WithTimeout(ctx, 15*time.Second)
	defer cancel()

	if err := eng.Ping(pingCtx); err != nil {
		closeFn()
		detail := strings.TrimSpace(errb.String())
		if detail == "" {
			detail = err.Error()
		}

		return nil, nil, fmt.Errorf("%w on %s: %s", ErrNoDocker, h.Target, detail)
	}

	return eng, closeFn, nil
}

//go:embed scripts/install-docker.sh
var installDocker []byte

// InstallDocker copies the bundled installer to a temporary remote path and runs it with sh as
// root, using sudo when needed. The script removes itself on exit. This method does not add the
// SSH account to the docker group.
func (h Host) InstallDocker(ctx context.Context) error {
	script := "/tmp/box-install-docker-" + box.NewSecret()[:12] + ".sh"
	cmd := exec.CommandContext(ctx, "ssh", h.Target, "cat > "+script)
	cmd.Stdin = bytes.NewReader(installDocker)
	if out, err := cmd.CombinedOutput(); err != nil {
		return fmt.Errorf("copying the install script to %s: %v %s", h.Target, err, out)
	}

	command, err := h.sudo(ctx, "sh "+script)
	if err != nil {
		return err
	}

	return h.Run(ctx, command)
}

// RestartDocker runs systemctl restart docker on the host, using sudo for a non-root account.
func (h Host) RestartDocker(ctx context.Context) error {
	command, err := h.sudo(ctx, "systemctl restart docker")
	if err != nil {
		return err
	}

	return h.Run(ctx, command)
}
