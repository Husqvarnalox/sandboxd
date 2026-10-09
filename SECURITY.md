# Security policy

sandboxd is an experimental process-isolation runtime. It is not a
virtual-machine boundary and is not hardened for hostile multi-tenant use.
Read [docs/SECURITY_MODEL.md](docs/SECURITY_MODEL.md) for what it does and
does not protect against before deploying it anywhere.

## Reporting a vulnerability

If you find a way for a sandboxed job to do something the security model says
it cannot (escape the mount namespace, reach the host network, exceed its
cgroup limits, read daemon secrets, leave processes behind, traverse out of
the workspace through the artifacts API, ...), please report it privately
rather than opening a public issue.

Use GitHub's private vulnerability reporting on this repository. Include the
kernel version, how the daemon was started, and a reproducer.

Issues that are explicitly out of scope in the security model (kernel
exploits, side channels, denial of service against the host through means the
cgroup does not cover) are still interesting, but are tracked as regular
issues.
