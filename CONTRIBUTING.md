# Contributing

Issues and pull requests are welcome. Keep the scope of a change small and
explain the behaviour it changes.

## Development

Pure-Go packages build and test on any platform:

```bash
make test        # go test ./...
make test-race   # go test -race ./...
make lint        # gofmt, go vet
```

The Linux runtime (`internal/runtime/linux`, `internal/cgroup`) only compiles
on Linux and is exercised by integration tests behind the `integration` build
tag. They need root, cgroup v2 and a rootfs. On a Linux host:

```bash
sudo SANDBOXD_TEST_ROOTFS=/path/to/rootfs go test -tags integration ./internal/...
```

From macOS or any host with Docker, `make test-integration` runs them inside a
privileged Linux container (see `scripts/test-linux.sh`).

## Ground rules

- `gofmt`, `go vet` and `go test -race ./...` must pass.
- Every goroutine, process, cgroup, mount and file descriptor has an owner
  that releases it; a change that acquires a resource shows where it is
  released.
- Changes to the sandbox boundary (namespaces, mounts, seccomp, capabilities,
  cgroups) must update `docs/SECURITY_MODEL.md`.
- Changes to lifecycle behaviour must keep `docs/INVARIANTS.md` true, and
  ideally add a test that would fail if they did not.
- Comments explain non-obvious kernel or concurrency behaviour, not what the
  next line does.
- No new dependencies without a reason stated in the pull request.
