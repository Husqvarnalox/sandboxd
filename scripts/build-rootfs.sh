#!/bin/sh
# Build a minimal Alpine rootfs for sandboxd.
#
#   scripts/build-rootfs.sh [DEST] [--docker] [--alpine VERSION]
#
# Default: download the official Alpine minirootfs tarball for this machine's
# architecture, verify its published SHA-256, and unpack it into DEST
# (default ./rootfs). With --docker the rootfs is exported from the
# alpine:<VERSION> image instead (useful offline or where the CDN is blocked).
#
# The result is a plain directory: no device nodes, no setuid binaries. The
# daemon bind-mounts it read-only inside every sandbox and supplies /proc,
# /dev, /tmp and /workspace itself, so the directory only needs those mount
# points to exist. Nothing here needs root; the sandbox init does the
# ownership mapping.
set -eu

DEST=./rootfs
USE_DOCKER=0
ALPINE=3.20

while [ $# -gt 0 ]; do
	case "$1" in
	--docker) USE_DOCKER=1 ;;
	--alpine) shift; ALPINE="$1" ;;
	-h|--help) sed -n '2,16p' "$0"; exit 0 ;;
	-*) echo "unknown option: $1" >&2; exit 2 ;;
	*) DEST="$1" ;;
	esac
	shift
done

if [ -e "$DEST" ] && [ -n "$(ls -A "$DEST" 2>/dev/null)" ]; then
	echo "$DEST already exists and is not empty; remove it first" >&2
	exit 1
fi
mkdir -p "$DEST"

case "$(uname -m)" in
	x86_64|amd64) ARCH=x86_64 ;;
	aarch64|arm64) ARCH=aarch64 ;;
	*) echo "unsupported architecture: $(uname -m)" >&2; exit 1 ;;
esac

if [ "$USE_DOCKER" = 1 ]; then
	cid=$(docker create "alpine:$ALPINE" /bin/true)
	trap 'docker rm -f "$cid" >/dev/null' EXIT
	docker export "$cid" | tar -x -C "$DEST"
else
	base="https://dl-cdn.alpinelinux.org/alpine/v$ALPINE/releases/$ARCH"
	tmp=$(mktemp -d)
	trap 'rm -rf "$tmp"' EXIT
	curl -fsSL "$base/latest-releases.yaml" -o "$tmp/releases.yaml"
	# The yaml lists several flavours; take the file and sha256 of the
	# alpine-minirootfs entry.
	file=$(awk '/flavor: alpine-minirootfs/{f=1} f&&/file:/{print $2; exit}' "$tmp/releases.yaml")
	sha=$(awk '/flavor: alpine-minirootfs/{f=1} f&&/sha256:/{print $2; exit}' "$tmp/releases.yaml")
	if [ -z "$file" ] || [ -z "$sha" ]; then
		echo "could not find alpine-minirootfs in $base/latest-releases.yaml" >&2
		exit 1
	fi
	echo "downloading $file"
	curl -fsSL "$base/$file" -o "$tmp/$file"
	if command -v sha256sum >/dev/null 2>&1; then
		echo "$sha  $tmp/$file" | sha256sum -c - >/dev/null
	else
		echo "$sha  $tmp/$file" | shasum -a 256 -c - >/dev/null
	fi
	tar -xzf "$tmp/$file" -C "$DEST"
fi

# Mount points the sandbox init expects. The rootfs is mounted read-only
# before the workload starts, so they must exist up front.
mkdir -p "$DEST/proc" "$DEST/dev" "$DEST/tmp" "$DEST/workspace" "$DEST/dev/shm"
# resolv.conf is irrelevant without network, but tools expect the file.
: > "$DEST/etc/resolv.conf" 2>/dev/null || true

echo "rootfs ready at $DEST ($(du -sh "$DEST" | cut -f1))"
