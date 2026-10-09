# Roadmap

No dates. Items move when there is a concrete design and someone to build it.

## v0.1 (this release)

Standalone Linux daemon: namespaces, cgroup v2 limits, read-only rootfs with
`pivot_root`, seccomp deny-list, PID 1 init, bounded FIFO scheduler, streamed
logs, cancellation and timeouts, Prometheus metrics, `doctor`.

## v0.2

- **Rootless operation.** Run the daemon as an unprivileged user with
  subuid/subgid ranges and a delegated cgroup subtree (systemd
  `Delegate=yes`). Most of the init already runs inside a user namespace;
  the missing pieces are cgroup delegation and the ownership of the rootfs.
- **Persistent job metadata.** Durable records for finished jobs so history
  survives a restart. Running jobs still die with the daemon; the record
  would say so.
- **Input files.** Upload a bounded set of files into the workspace at
  submission time.
- **Allow-list seccomp profile** derived from the deny-list plus a recorded
  baseline of common workloads, with a per-job profile selector.
- **Network modes.** `restricted` (loopback plus a veth to a daemon-managed
  bridge with egress filtering) alongside the current `none`.
- **OCI rootfs.** Accept an OCI image layout or a `docker export` tarball as
  the rootfs source instead of a prepared directory.

## v0.3

- Pluggable backends behind the `runtime.Runtime` interface: a `runc`/OCI
  backend for hosts that already have one.
- Warm sandbox pool to cut start latency for short jobs.
- Sandbox templates (rootfs + limits + env presets) selectable by name.
- Stronger filesystem mediation: per-job overlayfs upper layer instead of a
  shared read-only rootfs plus a workspace bind.

## Later

- microVM backend (Firecracker / Cloud Hypervisor) for workloads that need a
  hardware-virtualisation boundary.
- Multi-node scheduling.
