# Security model

This document describes what sandboxd v0.1 actually does to isolate a job,
what it deliberately does not do, and what it assumes about the host. It is
written for someone deciding whether the boundary is adequate for their
workload. Read the "Non-goals" and "Known limitations" sections before
anything else.

**Summary:** sandboxd provides process/container-style isolation built from
Linux namespaces, cgroup v2, a read-only `pivot_root` filesystem, an empty
capability set, `no_new_privs` and a seccomp deny-list. The kernel is shared
with the host. It is not a virtual-machine boundary. Do not expose it as
multi-tenant public compute for hostile code without an additional layer
(microVM, gVisor, dedicated hosts).

## Threat model

sandboxd aims to protect the host, the daemon and other jobs from a job
that is buggy, greedy or moderately hostile at the user level. Concretely, a
job must not be able to:

- read or modify the daemon's memory, files, environment or credentials;
- see or signal processes outside its own sandbox;
- reach the host network or any network;
- read or write host filesystem paths other than its own workspace;
- exhaust host memory, CPU or process table beyond its declared limits;
- survive its own cancellation, timeout or the daemon's shutdown;
- make the daemon read or serve files outside the job's workspace through
  the artifacts API, including via symlinks the job created.

The attacker is assumed to run arbitrary user-space code inside the sandbox
with the ability to fork, create files, create symlinks, allocate memory,
spin CPUs and invoke any syscall the filter allows.

## Boundaries

Everything below is installed by the sandbox init **before** the user
command is exec'd. There is no window in which untrusted code runs with
fewer restrictions than listed here (`docs/INVARIANTS.md`, invariant 9).

### Namespaces

Each job gets fresh **PID, mount, UTS, IPC and network** namespaces, and by
default a fresh **user** namespace.

- *PID*: the sandbox init is PID 1 of the job. The job cannot see or signal
  host processes or other jobs. When init exits, the kernel kills every
  remaining process in the namespace.
- *Mount*: all mounts happen inside the private namespace; the host mount
  table is never touched and there is nothing to clean up on the host.
- *Network*: the namespace contains only `lo` (brought up so `127.0.0.1`
  works). There is no veth, no route, no DNS. See limitations for kernel
  auto-created tunnel devices.
- *UTS/IPC*: hostname is `sandbox`; SysV IPC and POSIX message queues are
  private.
- *User*: sandbox uid/gid 0 maps to host uid/gid `100000` (configurable
  base, range 65536). "root" inside the sandbox is an unprivileged user on
  the host. When the user namespace is disabled (`--userns=false`), init
  drops to uid/gid 65534 before exec instead.

### cgroup v2

Each job has its own cgroup under `/sys/fs/cgroup/sandboxd/<job-id>` with:

- `memory.max` = requested memory, `memory.swap.max = 0`,
  `memory.oom.group = 1` (an OOM kills the whole job atomically, and the
  job is reported as `oom_killed`);
- `cpu.max` = requested millicores over a 100 ms period;
- `pids.max` = requested PID limit (fork bombs fail at the limit).

The init is cloned directly into the cgroup (`clone3` with
`CLONE_INTO_CGROUP`), so no process of the job ever runs outside it.
Cancellation and cleanup use `cgroup.kill`, which SIGKILLs every process in
the cgroup regardless of PID namespace or process group, including
processes that daemonised or re-parented.

### Filesystem

- The configured rootfs is bind-mounted and remounted **read-only,
  nosuid, nodev**; the job then `pivot_root`s into it and the old root is
  detached. The host filesystem is not reachable by path.
- `/proc` is a fresh procfs for the job's PID namespace (`nosuid, nodev,
  noexec`).
- `/dev` is a small tmpfs with bind-mounted `null, zero, full, random,
  urandom, tty` and the usual `fd`/`stdin`/`stdout`/`stderr` symlinks. No
  other device nodes; `mknod` is denied by seccomp and impossible in a user
  namespace anyway. `/dev/shm` is a 64 MiB tmpfs.
- `/tmp` is a private 256 MiB tmpfs.
- `/workspace` is the job's own host directory
  (`<state-dir>/jobs/<id>/workspace`), bind-mounted **nosuid, nodev**,
  writable, owned by the sandbox's host uid. Its parent directory is owned
  by the daemon and not writable by the job.
- tmpfs contents are charged to the job's memory cgroup.

`/sys` is not mounted. Nothing from the daemon's filesystem view other than
the rootfs and the workspace is visible.

### Privileges

Inside init, before exec, in this order: capability bounding set emptied,
ambient set cleared, effective/permitted/inheritable sets cleared on every
thread, `PR_SET_NO_NEW_PRIVS` set, seccomp installed. The user command
therefore starts with **zero capabilities** even though it is uid 0 in the
user namespace, cannot regain any through setuid/file-capability binaries
(`no_new_privs` + `nosuid` mounts), and cannot create nested namespaces.

### seccomp

A classic-BPF **deny-list** filter installed with `SECCOMP_FILTER_FLAG_TSYNC`
so it covers every thread. Denied (EPERM): mount family (`mount`,
`umount2`, `pivot_root`, `chroot`, `open_tree`, `move_mount`, `fs*`,
`mount_setattr`), namespace entry (`setns`, `unshare`, `clone` with any
`CLONE_NEW*` flag; `clone3` returns ENOSYS so libc falls back to `clone`),
module loading, `kexec`, `reboot`, swap, keyrings, `ptrace`,
`process_vm_*`, `bpf`, `perf_event_open`, `userfaultfd`, `io_uring_*`,
`open_by_handle_at`, clock/time setting, `sethostname`, `mknod*`,
`fanotify_*`, `pidfd_getfd`, `kcmp`, `quotactl`, `syslog`, `acct` and
assorted legacy calls. Foreign-ABI syscalls (i386 compat, x32) get ENOSYS.

It is a deny-list because an allow-list that still runs arbitrary binaries
is a long project of its own. The filter is defence in depth behind the
namespaces and the empty capability set, not the primary boundary.

### Environment

The workload's environment is exactly `PATH`, `HOME` plus the variables in
the request, validated for name syntax and size. The daemon's environment
(`AWS_*`, tokens, `SSH_AUTH_SOCK`, ...) is never inherited: init itself is
started with an empty environment.

### API surface

- Commands are exec'd as argv. There is no implicit shell.
- Job IDs are random UUIDv4 and are validated against the canonical form
  before being used as a path component or cgroup name.
- Artifacts are served through `os.Root` (openat-based resolution rooted at
  the workspace). Paths containing `..` are rejected, symlinks that resolve
  outside the workspace fail, and only regular files are served. Artifacts
  are only readable once the job is terminal; by then its PID namespace has
  been torn down (or its cgroup killed), so the job cannot swap files while
  they are read.
- Request bodies, argument counts and sizes, environment counts and sizes,
  timeouts and resource ranges are bounded (`internal/job/spec.go`).
- Error responses never include host paths or syscall details.

## Non-goals

sandboxd does **not** protect against:

- **Kernel vulnerabilities.** The kernel is shared. A local privilege
  escalation bug reachable through an allowed syscall defeats every
  boundary above. This is the fundamental difference from a VM.
- **Side channels** (cache timing, Spectre-class attacks, `/proc` timing
  information).
- **Resource exhaustion the cgroup does not meter**: disk space in the
  workspace, inode counts, kernel memory not attributed to the cgroup,
  loopback network buffers, host-wide limits such as file-max.
- **Denial of service against the daemon via the API** (no authentication,
  no rate limiting). Put it behind something that does.
- **Malicious rootfs.** The rootfs is trusted configuration provided by the
  operator.
- **Hostile multi-tenant workloads without further hardening.**

## Privileges of the daemon

v0.1 requires the daemon to run as **root** (uid 0). It needs:

- `CAP_SYS_ADMIN` to create PID/mount/network/UTS/IPC namespaces and to
  write `uid_map`/`gid_map` with a range;
- write access to the cgroup v2 tree to create `/sys/fs/cgroup/sandboxd`
  and enable controllers;
- `CAP_CHOWN` to hand the workspace to the sandbox uid;
- `CAP_KILL`/`CAP_SYS_ADMIN` to kill processes in a job cgroup.

The daemon itself keeps all capabilities; only the sandboxed side drops
them. Rootless operation (subuid ranges + delegated cgroup subtree) is on
the roadmap; most of the init already runs inside a user namespace, which is
the main prerequisite. The daemon binary and every directory above it must
be world-executable because it is re-executed as the sandbox init by the
mapped sandbox uid; `sandboxd doctor` checks this.

The `sandboxd` HTTP API has no authentication. Bind it to localhost or a
private interface and front it with your own access control.

## Known limitations

- **Shared kernel.** See non-goals. Treat the boundary as "container-grade".
- **Tunnel devices.** Kernels with `net.core.fb_tunnels_only_for_init_net=0`
  (the default) create dormant `tunl0`/`gre0`/`sit0`-style devices in every
  new network namespace. They are down and have no route, but they exist.
  Set the sysctl to `2` on the host to suppress them.
- **No `/dev/pts`.** Programs that need a pseudo-terminal will fail.
- **Deny-list seccomp.** New or exotic syscalls are allowed by default.
- **Workspace disk usage is unbounded** by the cgroup. Use a filesystem
  quota or a dedicated volume for the state directory.
- **Timing of OOM detection.** `oom_killed` is derived from
  `memory.events` after exit; a job that handles its own allocation failure
  and exits 1 is reported as `non_zero_exit`, which is correct.
- **Signals to processes that called `setsid`.** Graceful cancellation sends
  SIGTERM to the workload's process group; a process that left the group
  only sees the SIGKILL that follows the grace period.
- **No persistence.** Job records live in memory; a daemon restart forgets
  them and kills any running job (via `Pdeathsig` and cgroup cleanup at the
  next start).
- **Docker as a test host.** The integration suite runs in a `--privileged`
  container because it needs real namespaces and cgroup delegation. That is
  a test convenience; it says nothing about running sandboxd inside Docker
  in production, which is not a supported deployment.
