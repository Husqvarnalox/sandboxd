//go:build linux

package linux

import "golang.org/x/sys/unix"

const (
	auditArch = unix.AUDIT_ARCH_X86_64
	nrLimit   = 0x40000000 // __X32_SYSCALL_BIT
)

var archDenied = []uintptr{
	unix.SYS_MKNOD, unix.SYS_IOPL, unix.SYS_IOPERM, unix.SYS_USELIB,
	unix.SYS_USTAT, unix.SYS_SYSFS, unix.SYS__SYSCTL,
	unix.SYS_GET_KERNEL_SYMS, unix.SYS_CREATE_MODULE, unix.SYS_QUERY_MODULE,
	unix.SYS_AFS_SYSCALL, unix.SYS_SECURITY, unix.SYS_TUXCALL, unix.SYS_VSERVER,
}
