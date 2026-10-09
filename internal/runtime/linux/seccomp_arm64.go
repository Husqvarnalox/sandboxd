//go:build linux

package linux

import "golang.org/x/sys/unix"

const (
	auditArch = unix.AUDIT_ARCH_AARCH64
	nrLimit   = 0
)

// arm64 uses the generic syscall table, which never had the legacy calls
// denied on amd64 (mknod, iopl, uselib, ustat, sysfs, _sysctl, ...).
var archDenied = []uintptr{}
