# Changelog

All notable changes to sandboxd are recorded here. The format follows
[Keep a Changelog](https://keepachangelog.com/en/1.1.0/); versions follow
[Semantic Versioning](https://semver.org/) with the usual 0.x caveat that
anything may change.

## [0.1.0] - 2026-10-10

Initial release.

### Added

- Linux sandbox runtime: per-job PID, mount, UTS, IPC, network and user
  namespaces; `pivot_root` into a read-only rootfs with private `/proc`,
  minimal `/dev`, tmpfs `/tmp` and `/dev/shm`, writable `/workspace`.
- cgroup v2 limits (`memory.max`, swap off, OOM group, `cpu.max`,
  `pids.max`) applied via `clone3(CLONE_INTO_CGROUP)` before the workload
  starts; usage, peak memory and OOM kill counts reported per job.
- PID 1 init (`sandboxd internal-init`): zombie reaping, signal forwarding,
  capability drop, `no_new_privs`, seccomp deny-list filter (TSYNC), kill of
  all remaining processes on exit.
- Job lifecycle with a validated state machine and immutable terminal
  states; timeouts; cancellation with SIGTERM, grace period and
  `cgroup.kill`; graceful daemon shutdown.
- Bounded FIFO scheduler with configurable concurrency and queue size;
  `429 QUEUE_FULL` on overflow.
- Bounded per-job log retention (4 MiB default) with live Server-Sent Events
  streaming and drop accounting.
- HTTP API: `POST/GET /v1/jobs`, `GET /v1/jobs/{id}`, cancel, delete, logs
  (with `follow`), artifacts served through `os.Root` with symlink
  containment; `/health`, `/ready`, `/version`, `/metrics`.
- Configuration by flags with `SANDBOXD_*` environment twins; `sandboxd
  doctor` host preflight; stale cgroup and workspace cleanup at startup.
- Prometheus metrics without per-job labels; structured `slog` logging that
  never records environment values.
- `scripts/build-rootfs.sh` (Alpine minirootfs with checksum verification),
  `scripts/test-linux.sh` (privileged Docker test harness),
  `scripts/demo.sh`, `cmd/sandboxbench` load generator.
- Documentation: security model, invariants, roadmap, contributing and
  security policy.

### Known limitations

See "Known limitations" in `docs/SECURITY_MODEL.md`: root required, shared
kernel, deny-list seccomp, no `/dev/pts`, no job persistence, workspace disk
usage not metered.
