//go:build linux

package doctor

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/Husqvarnalox/sandboxd/internal/config"
)

// Run performs every diagnostic and returns the results in a stable order.
func Run(cfg *config.Config) []Check {
	kernel, major, minor := kernelVersion()
	checks := []Check{
		{"kernel", OK, kernel},
		rootCheck(),
	}
	checks = append(checks, cgroupChecks(cfg, major, minor)...)
	checks = append(checks, userNSChecks(cfg)...)
	checks = append(checks, seccompCheck())
	checks = append(checks, rootfsChecks(cfg)...)
	checks = append(checks, stateDirCheck(cfg), executableCheck())
	return checks
}

func kernelVersion() (string, int, int) {
	var u unix.Utsname
	if err := unix.Uname(&u); err != nil {
		return "unknown", 0, 0
	}
	sys := unix.ByteSliceToString(u.Sysname[:])
	rel := unix.ByteSliceToString(u.Release[:])
	var major, minor int
	parts := strings.SplitN(rel, ".", 3)
	if len(parts) >= 2 {
		major, _ = strconv.Atoi(parts[0])
		minor, _ = strconv.Atoi(strings.TrimRightFunc(parts[1], func(r rune) bool { return r < '0' || r > '9' }))
	}
	return sys + " " + rel, major, minor
}

func atLeast(major, minor, wantMajor, wantMinor int) bool {
	return major > wantMajor || (major == wantMajor && minor >= wantMinor)
}

func rootCheck() Check {
	if uid := os.Geteuid(); uid != 0 {
		return Check{"root", Fail, fmt.Sprintf("running as uid %d; sandboxd v0.1 must run as root", uid)}
	}
	return Check{"root", OK, "running as uid 0"}
}

func fields(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	set := make(map[string]bool)
	for _, f := range strings.Fields(string(data)) {
		set[f] = true
	}
	return set, nil
}

func missing(set map[string]bool, want ...string) []string {
	var out []string
	for _, w := range want {
		if !set[w] {
			out = append(out, w)
		}
	}
	return out
}

func cgroupChecks(cfg *config.Config, major, minor int) []Check {
	root := cfg.CgroupRoot
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil {
		return []Check{{"cgroup v2", Fail, fmt.Sprintf("statfs %s: %v", root, err)}}
	}
	if st.Type != unix.CGROUP2_SUPER_MAGIC {
		return []Check{{"cgroup v2", Fail, root + " is not a cgroup2 mount (cgroup v1 or hybrid hosts are unsupported)"}}
	}
	checks := []Check{{"cgroup v2", OK, root}}

	ctrl, err := fields(filepath.Join(root, "cgroup.controllers"))
	switch {
	case err != nil:
		checks = append(checks, Check{"cgroup controllers", Fail, err.Error()})
	case len(missing(ctrl, "cpu", "memory", "pids")) > 0:
		checks = append(checks, Check{"cgroup controllers", Fail, fmt.Sprintf("missing %v in cgroup.controllers", missing(ctrl, "cpu", "memory", "pids"))})
	default:
		checks = append(checks, Check{"cgroup controllers", OK, "cpu memory pids"})
	}

	sub, err := fields(filepath.Join(root, "cgroup.subtree_control"))
	if m := missing(sub, "cpu", "memory", "pids"); err != nil || len(m) > 0 {
		checks = append(checks, Check{"cgroup delegation", Warn, fmt.Sprintf("%v not enabled in subtree_control; will be enabled at startup", m)})
	} else {
		checks = append(checks, Check{"cgroup delegation", OK, "cpu memory pids enabled"})
	}

	tree := filepath.Join(root, cfg.CgroupName)
	if _, err := os.Stat(tree); err == nil {
		if _, err := os.Stat(filepath.Join(tree, "cgroup.kill")); err == nil {
			checks = append(checks, Check{"cgroup.kill", OK, "supported"})
		} else {
			checks = append(checks, Check{"cgroup.kill", Warn, "not supported; falling back to killing pids one by one"})
		}
	} else if atLeast(major, minor, 5, 14) {
		checks = append(checks, Check{"cgroup.kill", OK, "kernel >= 5.14"})
	} else {
		checks = append(checks, Check{"cgroup.kill", Warn, "kernel < 5.14; falling back to killing pids one by one"})
	}
	if atLeast(major, minor, 5, 19) {
		checks = append(checks, Check{"memory.peak", OK, "kernel >= 5.19"})
	} else {
		checks = append(checks, Check{"memory.peak", Warn, "kernel < 5.19; peak memory will not be reported"})
	}
	return checks
}

func readInt(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, err
	}
	return strconv.Atoi(strings.TrimSpace(string(data)))
}

func userNSChecks(cfg *config.Config) []Check {
	if !cfg.UserNS {
		return []Check{{"user namespaces", Skip, fmt.Sprintf("disabled; workloads run as uid %d", cfg.SandboxUID)}}
	}
	var checks []Check
	if n, err := readInt("/proc/sys/user/max_user_namespaces"); err != nil || n <= 0 {
		checks = append(checks, Check{"user namespaces", Fail, "user namespaces unavailable (/proc/sys/user/max_user_namespaces is 0 or missing)"})
	} else {
		checks = append(checks, Check{"user namespaces", OK, fmt.Sprintf("max_user_namespaces=%d", n)})
	}
	if os.Geteuid() != 0 {
		if n, err := readInt("/proc/sys/kernel/unprivileged_userns_clone"); err == nil && n == 0 {
			checks = append(checks, Check{"unprivileged userns", Warn, "kernel.unprivileged_userns_clone=0"})
		}
	}
	base, end := cfg.HostUIDBase, cfg.HostUIDBase+65536
	if uid := os.Geteuid(); uid >= base && uid < end {
		checks = append(checks, Check{"uid range", Warn, fmt.Sprintf("daemon uid %d lies inside the sandbox range %d-%d", uid, base, end-1)})
	} else {
		checks = append(checks, Check{"uid range", OK, fmt.Sprintf("%d-%d", base, end-1)})
	}
	return checks
}

func seccompCheck() Check {
	actions, err := fields("/proc/sys/kernel/seccomp/actions_avail")
	if err != nil {
		return Check{"seccomp", Fail, "kernel lacks seccomp filter support"}
	}
	if !actions["errno"] {
		return Check{"seccomp", Fail, "seccomp errno action unavailable"}
	}
	return Check{"seccomp", OK, "filter mode available"}
}

func stateDirCheck(cfg *config.Config) Check {
	fi, err := os.Stat(cfg.StateDir)
	if err != nil || !fi.IsDir() {
		return Check{"state dir", Warn, cfg.StateDir + " does not exist; serve will create it"}
	}
	if err := unix.Access(cfg.StateDir, unix.W_OK); err != nil {
		return Check{"state dir", Fail, cfg.StateDir + " is not writable"}
	}
	return Check{"state dir", OK, cfg.StateDir}
}
