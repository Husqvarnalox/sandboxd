//go:build linux && (amd64 || arm64)

package linux

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"testing"

	"golang.org/x/sys/unix"
)

const seccompHelperEnv = "SANDBOXD_TEST_SECCOMP_HELPER"

func TestSeccompFilterAssembles(t *testing.T) {
	prog, err := seccompFilter()
	if err != nil {
		t.Fatal(err)
	}
	if prog.Len == 0 || prog.Len > maxBPFInstructions {
		t.Fatalf("filter has %d instructions", prog.Len)
	}
	seen := map[uintptr]bool{}
	for _, nr := range append(append([]uintptr{}, commonDenied...), archDenied...) {
		if seen[nr] {
			t.Errorf("syscall %d denied twice", nr)
		}
		seen[nr] = true
	}
}

// seccompHelper runs in a re-executed test binary: it installs the filter as
// init does and reports what the filtered process can and cannot do. Each
// probe is chosen so that it succeeds or fails differently without the filter
// and needs no privileges.
func seccompHelper() {
	if err := setNoNewPrivs(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	if err := installSeccomp(); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(2)
	}
	report := func(name string, err error) { fmt.Printf("%s=%v\n", name, errno(err)) }
	report("unshare0", unix.Unshare(0)) // a no-op that always succeeds unfiltered
	report("unshare_uts", unix.Unshare(unix.CLONE_NEWUTS))
	report("setns", unix.Setns(-1, 0)) // EBADF unfiltered
	_, err := unix.PtraceGetEventMsg(os.Getppid())
	report("ptrace", err)
	cmd := exec.Command("/bin/true")
	cmd.SysProcAttr = &syscall.SysProcAttr{Cloneflags: syscall.CLONE_NEWUSER}
	report("clone_newuser", cmd.Run())
	report("plain_exec", exec.Command("/bin/true").Run())
	os.Exit(0)
}

func errno(err error) string {
	if err == nil {
		return "ok"
	}
	var e syscall.Errno
	if errors.As(err, &e) {
		return unix.ErrnoName(e)
	}
	return err.Error()
}

func TestSeccompBlocksInChild(t *testing.T) {
	// The helper applies no_new_privs and the filter with AllThreadsSyscall,
	// which returns ENOTSUP once runtime/cgo is linked in (always the case
	// under -race). sandboxd itself is built with CGO_ENABLED=0.
	if _, _, e := syscall.AllThreadsSyscall(unix.SYS_PRCTL, unix.PR_GET_NO_NEW_PRIVS, 0, 0); e == unix.ENOTSUP {
		t.Skip("AllThreadsSyscall unavailable in a cgo build (e.g. -race)")
	}
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	cmd := exec.Command(exe, "-test.run=^$")
	cmd.Env = append(os.Environ(), seccompHelperEnv+"=1")
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("helper failed: %v\n%s", err, out)
	}
	// The helper's report reaching us proves write() still works.
	got := map[string]string{}
	for _, line := range strings.Split(strings.TrimSpace(string(out)), "\n") {
		k, v, _ := strings.Cut(line, "=")
		got[k] = v
	}
	want := map[string]string{
		"unshare0":      "EPERM",
		"unshare_uts":   "EPERM",
		"setns":         "EPERM",
		"ptrace":        "EPERM",
		"clone_newuser": "EPERM",
		"plain_exec":    "ok",
	}
	for k, v := range want {
		if got[k] != v {
			t.Errorf("%s = %q, want %q (helper output:\n%s)", k, got[k], v, out)
		}
	}
}
