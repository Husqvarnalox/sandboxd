// Package linux implements runtime.Runtime with Linux namespaces, cgroup v2,
// pivot_root onto a read-only rootfs, capability and privilege reduction and a
// seccomp filter. Everything except this comment is built only on Linux.
//
// The daemon re-executes its own binary as the sandbox init (RunInit), which
// becomes PID 1 of the job's pid namespace, builds the filesystem view, drops
// privileges and only then executes the workload, which it supervises.
package linux
