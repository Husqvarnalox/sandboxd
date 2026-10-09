//go:build linux

package linux

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

// initConfig is sent from the daemon to the sandbox init over fd 3.
type initConfig struct {
	Command   []string
	Env       []string
	WorkDir   string
	Rootfs    string
	Workspace string
	Hostname  string
	UserNS    bool
	UID       int
	GID       int
	Seccomp   bool
}

const (
	configFD      = 3
	statusFD      = 4
	maxConfigSize = 1 << 20
	reapGrace     = time.Second
	defaultPath   = "/usr/local/sbin:/usr/local/bin:/usr/sbin:/usr/bin:/sbin:/bin"
)

// forwarded are the signals init relays to the workload's process group.
var forwarded = []os.Signal{
	unix.SIGTERM, unix.SIGINT, unix.SIGHUP, unix.SIGQUIT, unix.SIGUSR1, unix.SIGUSR2,
}

// RunInit is the sandbox init: it runs as PID 1 of the job's namespaces,
// builds the sandbox, execs the workload and supervises it. It never returns.
//
// Init must never write to stdout or stderr: they belong to the workload.
// Its only output is the status protocol on fd 4: "ok <pid>\n" once the
// workload runs or "error: <msg>\n", and, when the workload was killed by a
// signal, a final "signal <n>\n" just before exiting.
func RunInit() {
	status := os.NewFile(statusFD, "status")
	fail := func(code int, format string, args ...any) {
		msg := strings.ReplaceAll(fmt.Sprintf(format, args...), "\n", " ")
		_, _ = status.WriteString("error: " + msg + "\n")
		os.Exit(code)
	}
	// The workload must not inherit the status pipe.
	unix.CloseOnExec(statusFD)

	cfg, err := readInitConfig()
	if err != nil {
		fail(1, "%v", err)
	}
	if err := setupSandbox(cfg); err != nil {
		fail(1, "%v", err)
	}

	// Subscribe before the child exists so no SIGCHLD or forwarded signal is
	// lost between fork and the supervision loop.
	sigs := make(chan os.Signal, 32)
	signal.Notify(sigs, append([]os.Signal{unix.SIGCHLD}, forwarded...)...)

	path, err := lookPath(cfg.Command[0], cfg.Env)
	if err != nil {
		fail(127, "exec %s: %v", cfg.Command[0], err)
	}
	// Dir is deliberately unset (init already chdir'd): exec.Cmd would add
	// PWD to the environment, and the workload must get exactly cfg.Env.
	cmd := &exec.Cmd{
		Path:        path,
		Args:        cfg.Command,
		Env:         cfg.Env,
		Stdout:      os.Stdout,
		Stderr:      os.Stderr,
		SysProcAttr: &syscall.SysProcAttr{Setpgid: true},
	}
	if err := cmd.Start(); err != nil {
		fail(127, "exec %s: %v", cfg.Command[0], err)
	}
	pid := cmd.Process.Pid
	_, _ = fmt.Fprintf(status, "ok %d\n", pid)

	code, sig := supervise(pid, sigs)
	if sig != 0 {
		_, _ = fmt.Fprintf(status, "signal %d\n", sig)
	}
	os.Exit(code)
}

func readInitConfig() (*initConfig, error) {
	f := os.NewFile(configFD, "config")
	data, err := io.ReadAll(io.LimitReader(f, maxConfigSize+1))
	_ = f.Close()
	if err != nil {
		return nil, fmt.Errorf("read init config: %w", err)
	}
	if len(data) > maxConfigSize {
		return nil, errors.New("read init config: too large")
	}
	var cfg initConfig
	if err := json.Unmarshal(data, &cfg); err != nil {
		return nil, fmt.Errorf("decode init config: %w", err)
	}
	if len(cfg.Command) == 0 || cfg.Rootfs == "" || cfg.Workspace == "" || !filepath.IsAbs(cfg.WorkDir) {
		return nil, errors.New("invalid init config")
	}
	return &cfg, nil
}

// setupSandbox runs every isolation step, in order, before the workload exists.
func setupSandbox(cfg *initConfig) error {
	if err := setupRootfs(cfg); err != nil {
		return err
	}
	if err := unix.Sethostname([]byte(cfg.Hostname)); err != nil {
		return fmt.Errorf("set hostname: %w", err)
	}
	if err := loopbackUp(); err != nil {
		return err
	}
	unix.Umask(0o022)
	if err := os.Chdir(cfg.WorkDir); err != nil {
		return fmt.Errorf("working directory %s: %w", cfg.WorkDir, err)
	}
	if err := dropBoundingSet(); err != nil {
		return err
	}
	if !cfg.UserNS {
		if err := syscall.Setgroups(nil); err != nil {
			return fmt.Errorf("setgroups: %w", err)
		}
		if err := syscall.Setgid(cfg.GID); err != nil {
			return fmt.Errorf("setgid %d: %w", cfg.GID, err)
		}
		if err := syscall.Setuid(cfg.UID); err != nil {
			return fmt.Errorf("setuid %d: %w", cfg.UID, err)
		}
	}
	if err := clearCapabilities(); err != nil {
		return err
	}
	if err := setNoNewPrivs(); err != nil {
		return err
	}
	if cfg.Seccomp {
		if err := installSeccomp(); err != nil {
			return err
		}
	}
	return nil
}

// supervise reaps every process in the namespace and returns the workload's
// exit code (128+signal if signalled) and terminating signal.
//
// Only raw wait4(-1) is used: mixing exec.Cmd.Wait with a wait4(-1) reaper
// races, because the reaper can collect the main child first.
func supervise(pid int, sigs <-chan os.Signal) (code, sig int) {
	exited := false
	reap := func() (done bool) {
		for {
			var ws unix.WaitStatus
			r, err := unix.Wait4(-1, &ws, unix.WNOHANG, nil)
			if err == unix.EINTR {
				continue
			}
			if err != nil { // ECHILD: nothing left in the namespace
				return true
			}
			if r == 0 {
				return false
			}
			if r == pid {
				exited = true
				switch {
				case ws.Signaled():
					sig = int(ws.Signal())
					code = 128 + sig
				default:
					code = ws.ExitStatus()
				}
			}
		}
	}

	for !exited {
		s := <-sigs
		if s == unix.SIGCHLD {
			reap()
			continue
		}
		// The workload may already be gone; ESRCH is expected then.
		_ = unix.Kill(-pid, s.(syscall.Signal))
	}

	// The main workload process is gone. Ask the rest of the namespace to
	// stop, then force it. kill(-1) from PID 1 reaches every other process
	// in the namespace, including ones that left the process group.
	if reap() {
		return code, sig
	}
	_ = unix.Kill(-1, unix.SIGTERM)
	grace := time.NewTimer(reapGrace)
	defer grace.Stop()
	for {
		select {
		case s := <-sigs:
			if s == unix.SIGCHLD && reap() {
				return code, sig
			}
		case <-grace.C:
			_ = unix.Kill(-1, unix.SIGKILL)
			for {
				var ws unix.WaitStatus
				_, err := unix.Wait4(-1, &ws, 0, nil)
				if err != nil && err != unix.EINTR {
					// Exiting PID 1 makes the kernel kill anything left.
					return code, sig
				}
			}
		}
	}
}

// lookPath resolves name against PATH from the workload's environment, not
// init's (which is empty).
func lookPath(name string, env []string) (string, error) {
	if strings.Contains(name, "/") {
		return name, checkExecutable(name)
	}
	path := defaultPath
	for _, kv := range env {
		if v, ok := strings.CutPrefix(kv, "PATH="); ok {
			path = v
		}
	}
	for _, dir := range filepath.SplitList(path) {
		if dir == "" {
			dir = "."
		}
		p := filepath.Join(dir, name)
		if checkExecutable(p) == nil {
			return p, nil
		}
	}
	return "", fmt.Errorf("executable file not found in PATH")
}

func checkExecutable(p string) error {
	st, err := os.Stat(p)
	if err != nil {
		return err
	}
	if st.IsDir() || st.Mode()&0o111 == 0 {
		return fmt.Errorf("%s is not an executable file", p)
	}
	return nil
}
