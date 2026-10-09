//go:build linux

package linux

import (
	"fmt"
	"os"
	"strconv"
	"strings"
	"syscall"
	"unsafe"

	"golang.org/x/sys/unix"
)

// The privilege-reduction syscalls below act per thread, and the Go init is
// multithreaded, so each is applied to every thread with AllThreadsSyscall.
// AllThreadsSyscall is unavailable when cgo is enabled: build with
// CGO_ENABLED=0.

// dropBoundingSet removes every capability from the bounding set and clears
// the ambient set, so nothing exec'd later can gain a capability, not even
// root inside the user namespace or a setuid binary.
func dropBoundingSet() error {
	last := 40
	if b, err := os.ReadFile("/proc/sys/kernel/cap_last_cap"); err == nil {
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err == nil {
			last = n
		}
	}
	for c := 0; c <= last; c++ {
		_, _, e := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_CAPBSET_DROP, uintptr(c), 0, 0, 0, 0)
		if e != 0 && e != unix.EINVAL { // EINVAL: capability unknown to this kernel
			return fmt.Errorf("drop capability %d from bounding set: %w", c, e)
		}
	}
	_, _, e := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_CAP_AMBIENT, unix.PR_CAP_AMBIENT_CLEAR_ALL, 0, 0, 0, 0)
	if e != 0 {
		return fmt.Errorf("clear ambient capabilities: %w", e)
	}
	return nil
}

// clearCapabilities empties init's effective, permitted and inheritable
// sets. With an empty bounding and inheritable set, the workload starts with
// no capabilities even as uid 0 in the user namespace.
func clearCapabilities() error {
	hdr := unix.CapUserHeader{Version: unix.LINUX_CAPABILITY_VERSION_3}
	var data [2]unix.CapUserData
	_, _, e := syscall.AllThreadsSyscall(unix.SYS_CAPSET, uintptr(unsafe.Pointer(&hdr)), uintptr(unsafe.Pointer(&data[0])), 0)
	if e != 0 {
		return fmt.Errorf("clear capabilities: %w", e)
	}
	return nil
}

func setNoNewPrivs() error {
	_, _, e := syscall.AllThreadsSyscall6(unix.SYS_PRCTL, unix.PR_SET_NO_NEW_PRIVS, 1, 0, 0, 0, 0)
	if e != 0 {
		return fmt.Errorf("set no_new_privs: %w", e)
	}
	return nil
}
