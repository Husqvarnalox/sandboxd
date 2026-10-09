#!/bin/sh
# Runs the Linux unit and integration tests (cgroup, runtime, e2e) inside a
# privileged Docker container with a private cgroup namespace, then a daemon
# smoke test (sandboxd doctor, sandboxd serve + scripts/demo.sh).
# Usage: scripts/test-linux.sh [go test args...]   e.g. -v -run TestKill
# Set SANDBOXD_SKIP_SMOKE=1 to skip the daemon smoke test.
set -eu

cd "$(dirname "$0")/.."
image=${SANDBOXD_TEST_IMAGE:-golang:1.26-alpine}

exec docker run --rm --privileged --cgroupns=private \
	-v "$PWD":/src -w /src \
	-v sandboxd-gomod:/go/pkg/mod -v sandboxd-gocache:/root/.cache/go-build \
	-e GOFLAGS=-buildvcs=false -e CGO_ENABLED=0 \
	-e SANDBOXD_SKIP_SMOKE="${SANDBOXD_SKIP_SMOKE:-}" \
	"$image" sh -c '
set -e
# The container sees its own cgroup as the root, so the "no internal
# processes" rule applies: move every process into a leaf before enabling
# controllers for children.
mkdir -p /sys/fs/cgroup/init
for p in $(cat /sys/fs/cgroup/cgroup.procs); do
	echo "$p" > /sys/fs/cgroup/init/cgroup.procs 2>/dev/null || true
done
echo "+cpu +memory +pids" > /sys/fs/cgroup/cgroup.subtree_control

# Minimal sandbox rootfs built from the image itself (busybox + musl +
# util-linux for unshare), without the Go toolchain in /usr/local.
apk add --no-cache util-linux curl >/dev/null
mkdir -p /rootfs/proc /rootfs/dev /rootfs/tmp /rootfs/workspace /rootfs/usr
cp -a /bin /sbin /lib /etc /rootfs/
cp -a /usr/bin /usr/sbin /usr/lib /rootfs/usr/

export SANDBOXD_TEST_ROOTFS=/rootfs SANDBOXD_TEST_STATE=/var/lib/sandboxd-test
mkdir -p "$SANDBOXD_TEST_STATE"
go test -tags integration -count=1 ./internal/cgroup/... ./internal/runtime/... ./internal/e2e/... "$@"

if [ -n "$SANDBOXD_SKIP_SMOKE" ]; then
	echo "skipping daemon smoke test (SANDBOXD_SKIP_SMOKE set)"
	exit 0
fi
echo "--- daemon smoke test"
go build -o /usr/local/bin/sandboxd ./cmd/sandboxd
sandboxd doctor --rootfs /rootfs --state-dir "$SANDBOXD_TEST_STATE"
sandboxd serve --rootfs /rootfs --state-dir "$SANDBOXD_TEST_STATE" --listen 127.0.0.1:8080 &
daemon=$!
i=0
until curl -fsS http://127.0.0.1:8080/ready >/dev/null 2>&1; do
	i=$((i + 1))
	if [ $i -ge 100 ] || ! kill -0 $daemon 2>/dev/null; then
		echo "sandboxd did not become ready" >&2
		kill -KILL $daemon 2>/dev/null || true
		exit 1
	fi
	sleep 0.1
done
demo=0
SANDBOXD_URL=http://127.0.0.1:8080 sh scripts/demo.sh || demo=$?
kill -TERM $daemon
code=0
wait $daemon || code=$?
echo "sandboxd exited with status $code"
[ "$demo" -eq 0 ] && [ "$code" -eq 0 ]
' sh "$@"
