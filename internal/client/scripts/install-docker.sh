#!/bin/sh
# Install Docker with the distribution package manager. Run as root on a systemd host.
# The exit trap deletes this script, so callers must run a temporary copy.
set -eu
trap 'rm -f "$0"' EXIT

# Read the target host distribution metadata at runtime.
# shellcheck source=/dev/null
. /etc/os-release

packages="docker-ce docker-ce-cli containerd.io"

# apt_install uses $1 as the Docker repository distribution and $2 as its codename.
apt_install() {
	export DEBIAN_FRONTEND=noninteractive
	apt-get update -qq
	apt-get install -y -qq ca-certificates curl
	install -m 0755 -d /etc/apt/keyrings
	curl -fsSL "https://download.docker.com/linux/$1/gpg" -o /etc/apt/keyrings/docker.asc
	chmod a+r /etc/apt/keyrings/docker.asc
	cat >/etc/apt/sources.list.d/docker.sources <<EOF
Types: deb
URIs: https://download.docker.com/linux/$1
Suites: $2
Components: stable
Signed-By: /etc/apt/keyrings/docker.asc
EOF
	apt-get update -qq
	# Split the package list into separate arguments.
	# shellcheck disable=SC2086
	apt-get install -y -qq $packages
}

# dnf_install selects the Docker repository for the distribution named by $1.
dnf_install() {
	curl -fsSL "https://download.docker.com/linux/$1/docker-ce.repo" -o /etc/yum.repos.d/docker-ce.repo
	# Split the package list into separate arguments.
	# shellcheck disable=SC2086
	dnf install -y -q $packages
}

like=" ${ID} ${ID_LIKE:-} "
case "$like" in
*" ubuntu "*)
	echo "box: installing Docker from Docker's apt repository for Ubuntu"
	apt_install ubuntu "${UBUNTU_CODENAME:-$VERSION_CODENAME}"
	;;
*" debian "*)
	echo "box: installing Docker from Docker's apt repository for Debian"
	apt_install debian "$VERSION_CODENAME"
	;;
*" fedora "*)
	if [ "$ID" = fedora ]; then
		echo "box: installing Docker from Docker's dnf repository for Fedora"
		dnf_install fedora
	elif [ "$ID" = rhel ]; then
		echo "box: installing Docker from Docker's dnf repository for RHEL"
		dnf_install rhel
	else
		echo "box: installing Docker from Docker's dnf repository for CentOS"
		dnf_install centos
	fi
	;;
*" arch "*)
	echo "box: installing Arch's docker package"
	pacman -Sy --noconfirm --needed docker
	;;
*)
	echo "box: no package recipe for ${PRETTY_NAME:-$ID}. Install Docker Engine 25 or later, then run box setup again." >&2
	exit 1
	;;
esac

systemctl enable --now docker
