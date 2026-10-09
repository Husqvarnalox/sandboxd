//go:build linux && (amd64 || arm64)

package linux

import (
	"fmt"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The seccomp filter is a conservative deny-list, not an allow-list. Its job
// is defence in depth behind namespaces, an empty capability set and
// no_new_privs: it removes kernel attack surface that sandboxed code never
// legitimately needs (mounting, namespaces, module loading, keyrings, BPF,
// ptrace, io_uring, clock and host administration). An allow-list would be
// tighter but would break arbitrary user binaries in ways that are hard to
// diagnose; the deny-list keeps "any static or dynamic Linux binary works".
//
// Besides the listed syscalls, clone with any CLONE_NEW* flag fails with
// EPERM (no new namespaces, user namespaces in particular), and clone3 fails
// with ENOSYS because its flags live in memory a classic BPF filter cannot
// inspect; libc and Go fall back to clone on ENOSYS.

// commonDenied are denied syscalls defined on every supported architecture.
var commonDenied = []uintptr{
	unix.SYS_MOUNT, unix.SYS_UMOUNT2, unix.SYS_PIVOT_ROOT, unix.SYS_CHROOT,
	unix.SYS_MOUNT_SETATTR, unix.SYS_OPEN_TREE, unix.SYS_MOVE_MOUNT,
	unix.SYS_FSOPEN, unix.SYS_FSCONFIG, unix.SYS_FSMOUNT, unix.SYS_FSPICK,
	unix.SYS_SWAPON, unix.SYS_SWAPOFF, unix.SYS_REBOOT,
	unix.SYS_KEXEC_LOAD, unix.SYS_KEXEC_FILE_LOAD,
	unix.SYS_INIT_MODULE, unix.SYS_FINIT_MODULE, unix.SYS_DELETE_MODULE,
	unix.SYS_KEYCTL, unix.SYS_ADD_KEY, unix.SYS_REQUEST_KEY,
	unix.SYS_PTRACE, unix.SYS_PROCESS_VM_READV, unix.SYS_PROCESS_VM_WRITEV,
	unix.SYS_SETNS, unix.SYS_UNSHARE, unix.SYS_BPF, unix.SYS_PERF_EVENT_OPEN,
	unix.SYS_USERFAULTFD, unix.SYS_OPEN_BY_HANDLE_AT, unix.SYS_NAME_TO_HANDLE_AT,
	unix.SYS_ACCT, unix.SYS_SETTIMEOFDAY, unix.SYS_CLOCK_SETTIME,
	unix.SYS_CLOCK_ADJTIME, unix.SYS_ADJTIMEX,
	unix.SYS_SETHOSTNAME, unix.SYS_SETDOMAINNAME, unix.SYS_MKNODAT,
	unix.SYS_LOOKUP_DCOOKIE, unix.SYS_NFSSERVCTL, unix.SYS_PERSONALITY,
	unix.SYS_QUOTACTL, unix.SYS_SYSLOG, unix.SYS_VHANGUP,
	unix.SYS_FANOTIFY_INIT, unix.SYS_FANOTIFY_MARK,
	unix.SYS_IO_URING_SETUP, unix.SYS_IO_URING_ENTER, unix.SYS_IO_URING_REGISTER,
	unix.SYS_PIDFD_GETFD, unix.SYS_KCMP,
}

const (
	seccompDataNr   = 0
	seccompDataArch = 4
	seccompDataArg0 = 16 // low 32 bits on little-endian

	cloneNewMask = unix.CLONE_NEWNS | unix.CLONE_NEWCGROUP | unix.CLONE_NEWUTS | unix.CLONE_NEWIPC |
		unix.CLONE_NEWUSER | unix.CLONE_NEWPID | unix.CLONE_NEWNET | unix.CLONE_NEWTIME

	maxBPFInstructions = 4096
)

func stmt(code uint16, k uint32) unix.SockFilter {
	return unix.SockFilter{Code: code, K: k}
}

func jump(code uint16, k uint32, jt, jf uint8) unix.SockFilter {
	return unix.SockFilter{Code: code, Jt: jt, Jf: jf, K: k}
}

func retErrno(errno unix.Errno) unix.SockFilter {
	return stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ERRNO|uint32(errno)&unix.SECCOMP_RET_DATA)
}

// seccompFilter assembles the filter for the native architecture.
func seccompFilter() (*unix.SockFprog, error) {
	denied := append(append([]uintptr{}, commonDenied...), archDenied...)
	prog := []unix.SockFilter{
		// Syscall numbers are only meaningful for the native ABI; reject any
		// other (e.g. i386 compat on amd64) outright.
		stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataArch),
		jump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, auditArch, 1, 0),
		retErrno(unix.ENOSYS),
		stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataNr),
	}
	if nrLimit != 0 {
		// amd64 x32 ABI shares AUDIT_ARCH_X86_64 but sets bit 30 of the
		// number; without this check every denied syscall is reachable
		// under its x32 alias.
		prog = append(prog,
			jump(unix.BPF_JMP|unix.BPF_JGE|unix.BPF_K, nrLimit, 0, 1),
			retErrno(unix.ENOSYS))
	}
	for _, nr := range denied {
		prog = append(prog,
			jump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, uint32(nr), 0, 1),
			retErrno(unix.EPERM))
	}
	prog = append(prog,
		jump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, unix.SYS_CLONE3, 0, 1),
		retErrno(unix.ENOSYS),
		jump(unix.BPF_JMP|unix.BPF_JEQ|unix.BPF_K, unix.SYS_CLONE, 0, 3),
		stmt(unix.BPF_LD|unix.BPF_W|unix.BPF_ABS, seccompDataArg0),
		jump(unix.BPF_JMP|unix.BPF_JSET|unix.BPF_K, cloneNewMask, 0, 1),
		retErrno(unix.EPERM),
		stmt(unix.BPF_RET|unix.BPF_K, unix.SECCOMP_RET_ALLOW),
	)
	if len(prog) > maxBPFInstructions {
		return nil, fmt.Errorf("seccomp filter has %d instructions (max %d)", len(prog), maxBPFInstructions)
	}
	return &unix.SockFprog{Len: uint16(len(prog)), Filter: &prog[0]}, nil
}

// installSeccomp loads the filter into every thread of the calling process.
// SECCOMP_FILTER_FLAG_TSYNC is essential: prctl(PR_SET_SECCOMP) would only
// cover the calling thread of the multithreaded Go runtime. no_new_privs must
// already be set on all threads.
func installSeccomp() error {
	prog, err := seccompFilter()
	if err != nil {
		return err
	}
	r, _, e := unix.Syscall(unix.SYS_SECCOMP, unix.SECCOMP_SET_MODE_FILTER, unix.SECCOMP_FILTER_FLAG_TSYNC, uintptr(unsafe.Pointer(prog)))
	if e != 0 {
		return fmt.Errorf("install seccomp filter: %w", e)
	}
	if r != 0 {
		return fmt.Errorf("install seccomp filter: thread %d could not be synchronized", r)
	}
	return nil
}
