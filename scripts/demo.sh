#!/bin/sh
# Walks through the sandboxd API against a running daemon.
# Usage: SANDBOXD_URL=http://127.0.0.1:8080 scripts/demo.sh
# Needs curl only (JSON is parsed with sed). Exits non-zero if any job does
# not reach its expected final state.
set -eu

URL=${SANDBOXD_URL:-http://127.0.0.1:8080}
CGROUP_TREE=${SANDBOXD_CGROUP_TREE:-/sys/fs/cgroup/sandboxd}
failures=0

header() { printf '\n=== %s ===\n' "$*"; }

# field NAME: first top-level string value of "NAME" in the JSON on stdin.
field() { sed -n "s/.*\"$1\":\"\([^\"]*\)\".*/\1/p" | head -n 1; }

api() { # METHOD PATH [BODY]
	if [ $# -ge 3 ]; then
		curl -sS -X "$1" -H 'Content-Type: application/json' -d "$3" "$URL$2"
	else
		curl -sS -X "$1" "$URL$2"
	fi
}

submit() { # JSON body -> job id
	id=$(api POST /v1/jobs "$1" | field id)
	if [ -z "$id" ]; then
		echo "submit failed: $1" >&2
		exit 1
	fi
	echo "$id"
}

state_of() { api GET "/v1/jobs/$1" | field state; }

wait_state() { # ID STATE... : poll (up to 30s) until the job is in one of STATEs
	id=$1
	shift
	i=0
	while [ $i -lt 300 ]; do
		st=$(state_of "$id")
		for want in "$@"; do
			[ "$st" = "$want" ] && return 0
		done
		i=$((i + 1))
		sleep 0.1
	done
	echo "job $id stuck in state '$st' (wanted: $*)" >&2
	return 1
}

expect_final() { # ID STATE REASON
	wait_state "$1" succeeded failed cancelled timed_out || true
	body=$(api GET "/v1/jobs/$1")
	st=$(echo "$body" | field state)
	reason=$(echo "$body" | field reason)
	echo "final: state=$st reason=${reason:-none}"
	if [ "$st" != "$2" ] || [ "$reason" != "$3" ]; then
		echo "FAIL: expected state=$2 reason=${3:-none}" >&2
		failures=$((failures + 1))
	fi
}

header "health and version"
api GET /health
api GET /version

header "hello world with streamed logs"
id=$(submit '{"command":["sh","-c","echo hello from sandbox"]}')
echo "job $id"
curl -sS -N --max-time 30 "$URL/v1/jobs/$id/logs?follow=true"
expect_final "$id" succeeded ""

header "timeout (sleep 30, timeout 2s)"
id=$(submit '{"command":["sleep","30"],"timeout_seconds":2}')
echo "job $id"
expect_final "$id" timed_out timeout

header "cancel (sleep 30, cancelled after 1s)"
id=$(submit '{"command":["sleep","30"]}')
echo "job $id"
wait_state "$id" running
sleep 1
api POST "/v1/jobs/$id/cancel" >/dev/null
expect_final "$id" cancelled user

header "out of memory (16 MiB limit, writes 64 MiB to tmpfs)"
id=$(submit '{"command":["sh","-c","dd if=/dev/zero of=/dev/shm/fill bs=1M count=64"],"resources":{"memory_mb":16}}')
echo "job $id"
expect_final "$id" failed oom_killed
api GET "/v1/jobs/$id" | sed -n 's/.*"oom_kills":\([0-9]*\).*/oom_kills=\1/p'

header "descendant cleanup (background sleeps, then cancel)"
id=$(submit '{"command":["sh","-c","sleep 300 & sleep 300 & sleep 300; wait"],"resources":{"pids":16}}')
echo "job $id"
wait_state "$id" running
if [ -r "$CGROUP_TREE/$id/cgroup.procs" ]; then
	echo "processes in job cgroup: $(wc -l <"$CGROUP_TREE/$id/cgroup.procs")"
fi
api POST "/v1/jobs/$id/cancel" >/dev/null
expect_final "$id" cancelled user
if [ -d "$CGROUP_TREE" ]; then
	i=0
	while [ -d "$CGROUP_TREE/$id" ] && [ $i -lt 50 ]; do
		i=$((i + 1))
		sleep 0.1
	done
	if [ -d "$CGROUP_TREE/$id" ]; then
		echo "FAIL: cgroup $CGROUP_TREE/$id still exists" >&2
		failures=$((failures + 1))
	else
		echo "cgroup $CGROUP_TREE/$id is gone"
	fi
else
	echo "($CGROUP_TREE not readable here; skipping cgroup check)"
fi

header "summary"
if [ "$failures" -ne 0 ]; then
	echo "$failures check(s) failed"
	exit 1
fi
echo "all checks passed"
