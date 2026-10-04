//go:build e2e

package e2e

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// TestInstallScript installs Docker packages in distribution containers when BOX_E2E_INSTALL is
// set. Only systemctl is stubbed, so the test requires network access to package repositories.
func TestInstallScript(t *testing.T) {
	if os.Getenv("BOX_E2E_INSTALL") == "" {
		t.Skip("set BOX_E2E_INSTALL=1 to install Docker in each distribution's container")
	}

	if err := exec.Command("docker", "version").Run(); err != nil {
		t.Skip("needs a running Docker")
	}

	script, err := filepath.Abs("../internal/client/scripts/install-docker.sh")
	if err != nil {
		t.Fatal(err)
	}

	cases := []struct {
		image string
		want  string
	}{
		{"debian:12", "Docker's apt repository for Debian"},
		{"debian:13", "Docker's apt repository for Debian"},
		{"ubuntu:24.04", "Docker's apt repository for Ubuntu"},
		{"fedora:latest", "Docker's dnf repository for Fedora"},
		{"rockylinux:9", "Docker's dnf repository for CentOS"},
	}

	// Copy the installer before running it because its exit trap removes the script. The
	// systemctl shim avoids requiring systemd inside the test containers.
	const harness = `set -e
mkdir -p /shim
printf '#!/bin/sh\necho "SHIM systemctl $*"\n' > /shim/systemctl
chmod +x /shim/systemctl
cp /script/install-docker.sh /tmp/install-docker.sh
PATH=/shim:$PATH sh /tmp/install-docker.sh
test ! -e /tmp/install-docker.sh && echo "SCRIPT REMOVED ITSELF"
dockerd --version
`

	for _, c := range cases {
		t.Run(c.image, func(t *testing.T) {
			t.Parallel()
			out, err := exec.Command("docker", "run", "--rm",
				"--volume", filepath.Dir(script)+":/script:ro",
				"--entrypoint", "sh", c.image, "-c", harness).CombinedOutput()
			if err != nil {
				t.Fatalf("%v\n%s", err, lastLines(string(out), 20))
			}

			for _, want := range []string{"box: installing Docker from " + c.want, "SHIM systemctl enable --now docker",
				"SCRIPT REMOVED ITSELF", "Docker version"} {
				if !strings.Contains(string(out), want) {
					t.Errorf("output lacks %q:\n%s", want, lastLines(string(out), 20))
				}
			}

			t.Logf("%s:\n%s", c.image, lastLines(string(out), 3))
		})
	}
}

func lastLines(s string, n int) string {
	lines := strings.Split(strings.TrimSpace(s), "\n")
	return strings.Join(lines[max(0, len(lines)-n):], "\n")
}
