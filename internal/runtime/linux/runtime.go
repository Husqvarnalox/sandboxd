//go:build linux

package linux

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/Husqvarnalox/sandboxd/internal/cgroup"
	"github.com/Husqvarnalox/sandboxd/internal/runtime"
)

// InitArg is the hidden subcommand under which the daemon binary runs as the
// sandbox init. main must dispatch it to RunInit before doing anything else.
const InitArg = "internal-init"

const (
	defaultHostUIDBase  = 100000
	idMapSize           = 65536
	defaultSandboxID    = 65534
	defaultStartTimeout = 10 * time.Second
	defaultHostname     = "sandbox"
)

// Config configures a Runtime. v0.1 requires the daemon to run as root.
type Config struct {
	// Rootfs is the host directory used read-only as every sandbox's root.
	// It must contain bin/.
	Rootfs string
	Tree   *cgroup.Tree
	// InitPath is the binary re-executed as the sandbox init, normally
	// os.Executable() of the daemon.
	InitPath string
	// UserNS runs each sandbox in a user namespace mapping sandbox uid/gid 0
	// to HostUIDBase on the host. Callers should enable it; it is false only
	// as the Go zero value.
	UserNS bool
	// HostUIDBase is the first host uid/gid of the 65536-id range mapped into
	// the sandbox when UserNS is set. Default 100000.
	HostUIDBase int
	// SandboxUID and SandboxGID are the ids the workload runs as when UserNS
	// is false. Zero means 65534 (nobody): root is never used.
	SandboxUID int
	SandboxGID int
	// StartTimeout bounds how long Start waits for init to exec the workload.
	// Default 10s.
	StartTimeout time.Duration
	Logger       *slog.Logger
}

// Runtime starts sandboxes on Linux.
type Runtime struct {
	cfg Config
	log *slog.Logger
}

var _ runtime.Runtime = (*Runtime)(nil)

// New validates cfg, fills defaults and checks host prerequisites.
func New(cfg Config) (*Runtime, error) {
	if cfg.Tree == nil {
		return nil, errors.New("runtime config: cgroup tree is required")
	}
	if !filepath.IsAbs(cfg.Rootfs) {
		return nil, fmt.Errorf("runtime config: rootfs %q must be an absolute path", cfg.Rootfs)
	}
	if st, err := os.Stat(filepath.Join(cfg.Rootfs, "bin")); err != nil || !st.IsDir() {
		return nil, fmt.Errorf("runtime config: rootfs %s has no bin/ directory", cfg.Rootfs)
	}
	if cfg.InitPath == "" {
		return nil, errors.New("runtime config: init path is required")
	}
	if st, err := os.Stat(cfg.InitPath); err != nil || st.Mode()&0o111 == 0 {
		return nil, fmt.Errorf("runtime config: init binary %s is not executable", cfg.InitPath)
	}
	if cfg.UserNS {
		if err := checkWorldExecutable(cfg.InitPath); err != nil {
			return nil, fmt.Errorf("runtime config: %w", err)
		}
	}
	if cfg.HostUIDBase == 0 {
		cfg.HostUIDBase = defaultHostUIDBase
	}
	if cfg.HostUIDBase < 0 || cfg.HostUIDBase+idMapSize > 1<<32-1 {
		return nil, fmt.Errorf("runtime config: invalid host uid base %d", cfg.HostUIDBase)
	}
	if cfg.SandboxUID == 0 {
		cfg.SandboxUID = defaultSandboxID
	}
	if cfg.SandboxGID == 0 {
		cfg.SandboxGID = defaultSandboxID
	}
	if cfg.StartTimeout <= 0 {
		cfg.StartTimeout = defaultStartTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	r := &Runtime{cfg: cfg, log: cfg.Logger}
	if err := r.Check(); err != nil {
		return nil, err
	}
	return r, nil
}

// Check reports whether sandboxes can currently be started.
func (r *Runtime) Check() error {
	if os.Geteuid() != 0 {
		return fmt.Errorf("%w: sandboxd must run as root", runtime.ErrUnavailable)
	}
	if _, err := os.Stat(filepath.Join(r.cfg.Tree.Path(), "cgroup.procs")); err != nil {
		return fmt.Errorf("%w: cgroup tree: %v", runtime.ErrUnavailable, err)
	}
	if _, err := os.Stat(filepath.Join(r.cfg.Rootfs, "bin")); err != nil {
		return fmt.Errorf("%w: rootfs: %v", runtime.ErrUnavailable, err)
	}
	if r.cfg.UserNS {
		b, err := os.ReadFile("/proc/sys/user/max_user_namespaces")
		if err != nil {
			return fmt.Errorf("%w: user namespaces: %v", runtime.ErrUnavailable, err)
		}
		if n, err := strconv.Atoi(strings.TrimSpace(string(b))); err != nil || n <= 0 {
			return fmt.Errorf("%w: user namespaces are disabled (user.max_user_namespaces=%s)", runtime.ErrUnavailable, strings.TrimSpace(string(b)))
		}
	}
	return nil
}

// checkWorldExecutable verifies that an unprivileged uid can execute path.
// With user namespaces, init is exec'd as the mapped sandbox root (host uid
// HostUIDBase), which owns nothing on the host, so only "other" permission
// bits apply to the binary and every directory above it.
func checkWorldExecutable(path string) error {
	abs, err := filepath.Abs(path)
	if err != nil {
		return err
	}
	abs, err = filepath.EvalSymlinks(abs)
	if err != nil {
		return fmt.Errorf("init binary: %w", err)
	}
	for p := abs; ; p = filepath.Dir(p) {
		st, err := os.Stat(p)
		if err != nil {
			return fmt.Errorf("init binary: %w", err)
		}
		if st.Mode().Perm()&0o001 == 0 {
			return fmt.Errorf("init binary %s is not executable by the sandbox user: %s lacks o+x", abs, p)
		}
		if p == "/" {
			return nil
		}
	}
}

// hostIDs returns the host uid/gid the workload's files are owned by.
func (r *Runtime) hostIDs() (int, int) {
	if r.cfg.UserNS {
		return r.cfg.HostUIDBase, r.cfg.HostUIDBase
	}
	return r.cfg.SandboxUID, r.cfg.SandboxGID
}

// Start creates the sandbox and returns once the workload has been exec'd
// inside it. On error every acquired resource has been released.
func (r *Runtime) Start(ctx context.Context, spec runtime.Spec) (_ runtime.Process, retErr error) {
	if err := ctx.Err(); err != nil {
		return nil, fmt.Errorf("start sandbox: %w", err)
	}
	if len(spec.Command) == 0 {
		return nil, errors.New("start sandbox: empty command")
	}
	if !filepath.IsAbs(spec.Workspace) {
		return nil, fmt.Errorf("start sandbox: workspace %q must be an absolute path", spec.Workspace)
	}
	workDir := spec.WorkDir
	if workDir == "" {
		workDir = "/workspace"
	}
	hostname := spec.Hostname
	if hostname == "" {
		hostname = defaultHostname
	}
	env := spec.Env
	if env == nil {
		env = []string{}
	}

	// Limits first: the cgroup is fully configured before any process of
	// this job exists, and init is cloned directly into it.
	cg, err := r.cfg.Tree.Create(spec.ID, spec.Resources)
	if err != nil {
		return nil, fmt.Errorf("start sandbox: %w", err)
	}
	var closers []*os.File
	defer func() {
		if retErr == nil {
			return
		}
		for _, f := range closers {
			_ = f.Close()
		}
		if err := cg.Remove(); err != nil {
			r.log.Error("remove cgroup after failed start", "cgroup", cg.Path(), "err", err)
			retErr = errors.Join(retErr, err)
		}
	}()

	uid, gid := r.hostIDs()
	if err := os.Lchown(spec.Workspace, uid, gid); err != nil {
		return nil, fmt.Errorf("start sandbox: chown workspace: %w", err)
	}

	cfg := initConfig{
		Command:   spec.Command,
		Env:       env,
		WorkDir:   workDir,
		Rootfs:    r.cfg.Rootfs,
		Workspace: spec.Workspace,
		Hostname:  hostname,
		UserNS:    r.cfg.UserNS,
		UID:       r.cfg.SandboxUID,
		GID:       r.cfg.SandboxGID,
		Seccomp:   true,
	}
	payload, err := json.Marshal(cfg)
	if err != nil {
		return nil, fmt.Errorf("start sandbox: encode init config: %w", err)
	}

	pipe := func() (*os.File, *os.File, error) {
		rd, wr, err := os.Pipe()
		if err != nil {
			return nil, nil, fmt.Errorf("start sandbox: pipe: %w", err)
		}
		closers = append(closers, rd, wr)
		return rd, wr, nil
	}
	cfgR, cfgW, err := pipe()
	if err != nil {
		return nil, err
	}
	statR, statW, err := pipe()
	if err != nil {
		return nil, err
	}
	outR, outW, err := pipe()
	if err != nil {
		return nil, err
	}
	errR, errW, err := pipe()
	if err != nil {
		return nil, err
	}

	cmd := exec.Command(r.cfg.InitPath, InitArg)
	cmd.Env = []string{} // non-nil: nothing from the daemon's environment
	cmd.Stdout = outW
	cmd.Stderr = errW
	cmd.ExtraFiles = []*os.File{cfgR, statW} // fd 3, fd 4
	cmd.SysProcAttr = &syscall.SysProcAttr{
		Cloneflags: syscall.CLONE_NEWPID | syscall.CLONE_NEWNS | syscall.CLONE_NEWUTS |
			syscall.CLONE_NEWIPC | syscall.CLONE_NEWNET,
		// clone3 with CLONE_INTO_CGROUP: init is born inside the limited
		// cgroup, there is no window in which it runs unconstrained.
		UseCgroupFD: true,
		CgroupFD:    cg.FD(),
		Setpgid:     true,
		// Pdeathsig is tied to the forking OS thread, not the process. The
		// daemon never forks from a goroutine that locks its thread, so the
		// signal fires only when the daemon itself dies. Go applies it after
		// the credential change below, so the setuid does not reset it.
		Pdeathsig: syscall.SIGKILL,
	}
	if r.cfg.UserNS {
		cmd.SysProcAttr.Cloneflags |= syscall.CLONE_NEWUSER
		cmd.SysProcAttr.UidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: r.cfg.HostUIDBase, Size: idMapSize}}
		cmd.SysProcAttr.GidMappings = []syscall.SysProcIDMap{{ContainerID: 0, HostID: r.cfg.HostUIDBase, Size: idMapSize}}
		cmd.SysProcAttr.GidMappingsEnableSetgroups = false
		// The child keeps the daemon's host uid 0, which is unmapped in the
		// new namespace; exec as an unmapped uid would drop every capability.
		// Switching to namespace uid 0 (host HostUIDBase) after Go has written
		// the maps keeps init privileged inside, and only inside, the sandbox.
		cmd.SysProcAttr.Credential = &syscall.Credential{Uid: 0, Gid: 0, NoSetGroups: true}
	}

	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("start sandbox init: %w", err)
	}
	// From here on the init exists: every failure must kill and reap it.
	for _, f := range []*os.File{cfgR, statW, outW, errW} {
		_ = f.Close()
	}
	closers = []*os.File{cfgW, statR, outR, errR}
	abort := func(cause error) error {
		if err := cg.Kill(); err != nil {
			cause = errors.Join(cause, err)
		}
		_ = cmd.Process.Kill()
		_ = cmd.Wait()
		return cause
	}

	if _, err := cfgW.Write(payload); err != nil {
		return nil, abort(fmt.Errorf("start sandbox: send init config: %w", err))
	}
	if err := cfgW.Close(); err != nil {
		return nil, abort(fmt.Errorf("start sandbox: send init config: %w", err))
	}

	status := bufio.NewReader(statR)
	lineCh := make(chan string, 1)
	go func() {
		line, _ := status.ReadString('\n')
		lineCh <- line
	}()
	timer := time.NewTimer(r.cfg.StartTimeout)
	defer timer.Stop()
	var line string
	select {
	case line = <-lineCh:
	case <-timer.C:
		err := abort(fmt.Errorf("start sandbox: init did not report within %s", r.cfg.StartTimeout))
		<-lineCh // the reader sees EOF once init is gone
		return nil, err
	case <-ctx.Done():
		err := abort(fmt.Errorf("start sandbox: %w", ctx.Err()))
		<-lineCh
		return nil, err
	}

	line = strings.TrimSuffix(line, "\n")
	switch {
	case strings.HasPrefix(line, "ok "):
		if _, err := strconv.Atoi(strings.TrimPrefix(line, "ok ")); err != nil {
			return nil, abort(fmt.Errorf("start sandbox: malformed init status %q", line))
		}
	case strings.HasPrefix(line, "error: "):
		return nil, abort(fmt.Errorf("start sandbox: %s", strings.TrimPrefix(line, "error: ")))
	default:
		err := abort(errors.New("start sandbox: init exited without reporting status"))
		if cmd.ProcessState != nil {
			err = fmt.Errorf("%w (%s)", err, cmd.ProcessState)
		}
		return nil, err
	}

	return &process{
		cmd:    cmd,
		cg:     cg,
		stdout: outR,
		stderr: errR,
		status: status,
		statR:  statR,
		log:    r.log,
	}, nil
}
