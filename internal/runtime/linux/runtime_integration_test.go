//go:build linux && integration

package linux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Husqvarnalox/sandboxd/internal/cgroup"
	"github.com/Husqvarnalox/sandboxd/internal/job"
	"github.com/Husqvarnalox/sandboxd/internal/runtime"
)

var (
	initOnce sync.Once
	initPath string
	initErr  error
)

// initBinary copies the test binary (which dispatches to RunInit in TestMain)
// to a world-executable location: go test builds in a 0700 directory, and with
// user namespaces init is exec'd as the unprivileged mapped uid.
func initBinary(t *testing.T, state string) string {
	t.Helper()
	initOnce.Do(func() {
		exe, err := os.Executable()
		if err != nil {
			initErr = err
			return
		}
		data, err := os.ReadFile(exe)
		if err != nil {
			initErr = err
			return
		}
		dir := filepath.Join(state, fmt.Sprintf("bin-%d", os.Getpid()))
		if err := os.MkdirAll(dir, 0o755); err != nil {
			initErr = err
			return
		}
		if err := os.Chmod(state, 0o755); err != nil {
			initErr = err
			return
		}
		initPath = filepath.Join(dir, filepath.Base(exe))
		initErr = os.WriteFile(initPath, data, 0o755)
	})
	if initErr != nil {
		t.Fatalf("prepare init binary: %v", initErr)
	}
	return initPath
}

type env struct {
	rt    *Runtime
	tree  *cgroup.Tree
	state string
}

func setup(t *testing.T, userNS bool) *env {
	t.Helper()
	rootfs := os.Getenv("SANDBOXD_TEST_ROOTFS")
	if rootfs == "" || os.Geteuid() != 0 {
		t.Skip("runtime integration tests need root and SANDBOXD_TEST_ROOTFS (see scripts/test-linux.sh)")
	}
	state := os.Getenv("SANDBOXD_TEST_STATE")
	if state == "" {
		state = t.TempDir()
	}
	tree, err := cgroup.Open(cgroup.DefaultRoot, fmt.Sprintf("sandboxd-rt-test-%d", os.Getpid()))
	if err != nil {
		t.Fatalf("open cgroup tree: %v", err)
	}
	t.Cleanup(func() {
		if err := unix.Rmdir(tree.Path()); err != nil && err != unix.ENOENT {
			t.Errorf("remove test cgroup tree: %v", err)
		}
	})
	exe := initBinary(t, state)
	rt, err := New(Config{
		Rootfs:       rootfs,
		Tree:         tree,
		InitPath:     exe,
		UserNS:       userNS,
		StartTimeout: 10 * time.Second,
		Logger:       slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return &env{rt: rt, tree: tree, state: state}
}

func (e *env) spec(t *testing.T, cmd ...string) runtime.Spec {
	t.Helper()
	id, err := job.NewID()
	if err != nil {
		t.Fatal(err)
	}
	ws := filepath.Join(e.state, id, "workspace")
	if err := os.MkdirAll(ws, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(filepath.Join(e.state, id)) })
	return runtime.Spec{
		ID:        id,
		Command:   cmd,
		Env:       []string{"PATH=/usr/sbin:/usr/bin:/sbin:/bin"},
		WorkDir:   job.DefaultWorkDir,
		Workspace: ws,
		Resources: job.Resources{MemoryBytes: 64 << 20, CPUMillis: 1000, PIDs: 32},
	}
}

func (e *env) cgroupPath(spec runtime.Spec) string { return filepath.Join(e.tree.Path(), spec.ID) }

// syncBuffer collects a stream while the test inspects it.
type syncBuffer struct {
	mu  sync.Mutex
	buf bytes.Buffer
}

func (b *syncBuffer) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.Write(p)
}

func (b *syncBuffer) String() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buf.String()
}

type running struct {
	t              *testing.T
	e              *env
	spec           runtime.Spec
	proc           runtime.Process
	stdout, stderr *syncBuffer
	drained        sync.WaitGroup
}

func (e *env) start(t *testing.T, spec runtime.Spec) *running {
	t.Helper()
	proc, err := e.rt.Start(context.Background(), spec)
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	r := &running{t: t, e: e, spec: spec, proc: proc, stdout: &syncBuffer{}, stderr: &syncBuffer{}}
	r.drained.Add(2)
	go func() { defer r.drained.Done(); _, _ = io.Copy(r.stdout, proc.Stdout()) }()
	go func() { defer r.drained.Done(); _, _ = io.Copy(r.stderr, proc.Stderr()) }()
	return r
}

// wait waits for the sandbox, closes it and checks that the cgroup is gone.
func (r *running) wait(timeout time.Duration) runtime.Result {
	r.t.Helper()
	type waited struct {
		res runtime.Result
		err error
	}
	ch := make(chan waited, 1)
	go func() {
		res, err := r.proc.Wait()
		ch <- waited{res, err}
	}()
	var w waited
	select {
	case w = <-ch:
	case <-time.After(timeout):
		_ = r.proc.Kill()
		r.t.Fatalf("sandbox did not exit within %s; stdout=%q stderr=%q", timeout, r.stdout, r.stderr)
	}
	if w.err != nil {
		r.t.Fatalf("Wait: %v", w.err)
	}
	r.drained.Wait()
	if err := r.proc.Close(); err != nil {
		r.t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(r.e.cgroupPath(r.spec)); !errors.Is(err, os.ErrNotExist) {
		r.t.Fatalf("cgroup %s still exists after Close (stat err %v)", r.e.cgroupPath(r.spec), err)
	}
	if err := r.proc.Close(); err != nil {
		r.t.Fatalf("second Close: %v", err)
	}
	return w.res
}

func waitUntil(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out after %s waiting for %s", timeout, what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (e *env) run(t *testing.T, spec runtime.Spec) (runtime.Result, string, string) {
	t.Helper()
	r := e.start(t, spec)
	res := r.wait(30 * time.Second)
	return res, r.stdout.String(), r.stderr.String()
}

func TestHelloWorld(t *testing.T) {
	e := setup(t, true)
	res, out, errOut := e.run(t, e.spec(t, "sh", "-c", "echo hello; echo oops >&2; exit 3"))
	if out != "hello\n" || errOut != "oops\n" {
		t.Errorf("stdout=%q stderr=%q", out, errOut)
	}
	if res.ExitCode != 3 || res.Signal != 0 || res.OOMKilled {
		t.Errorf("result = %+v, want exit 3", res)
	}
}

func TestWorkloadKilledBySignal(t *testing.T) {
	e := setup(t, true)
	res, _, _ := e.run(t, e.spec(t, "sh", "-c", "kill -9 $$"))
	if res.ExitCode != 137 || res.Signal != 9 {
		t.Errorf("result = %+v, want exit 137 signal 9", res)
	}
}

func TestPIDNamespace(t *testing.T) {
	e := setup(t, true)
	res, out, errOut := e.run(t, e.spec(t, "sh", "-c", "echo $$; cat /proc/1/comm; ls /proc | grep -c '^[0-9]'"))
	if res.ExitCode != 0 {
		t.Fatalf("result %+v stderr %q", res, errOut)
	}
	lines := strings.Split(strings.TrimSpace(out), "\n")
	if len(lines) != 3 {
		t.Fatalf("unexpected output %q", out)
	}
	// Not exactly 2: the Go init's threads also take ids in the namespace.
	if pid, _ := strconv.Atoi(lines[0]); pid < 2 || pid > 64 {
		t.Errorf("workload pid = %s, want a small pid in a fresh namespace", lines[0])
	}
	comm := filepath.Base(initPath)
	if len(comm) > 15 {
		comm = comm[:15]
	}
	if lines[1] != comm {
		t.Errorf("/proc/1/comm = %q, want init %q", lines[1], comm)
	}
	if n, _ := strconv.Atoi(lines[2]); n < 2 || n > 5 {
		t.Errorf("sandbox sees %s processes in /proc, want a handful", lines[2])
	}
}

// fallbackTunnels are the devices the kernel creates in every new network
// namespace when tunnel modules are loaded and
// net.core.fb_tunnels_only_for_init_net is 0. They are down, have no
// addresses and provide no connectivity.
var fallbackTunnels = []string{"tunl0", "gre0", "gretap0", "erspan0", "ip_vti0", "ip6_vti0", "sit0", "ip6tnl0", "ip6gre0"}

func TestNoNetwork(t *testing.T) {
	e := setup(t, true)
	res, out, errOut := e.run(t, e.spec(t, "sh", "-c", `
		cat /proc/net/dev; echo ---
		ip -o link; echo ---
		tail -n +2 /proc/net/route; echo ---
		wget -q -T 2 -O /dev/null http://1.1.1.1/ 2>&1; echo "wget rc=$?"`))
	if res.ExitCode != 0 {
		t.Fatalf("result %+v stderr %q", res, errOut)
	}
	parts := strings.Split(out, "---\n")
	if len(parts) != 4 {
		t.Fatalf("unexpected output %q", out)
	}
	// The sysctl is only visible from the initial network namespace (not from
	// inside a container); when it cannot be read, fallback tunnels may exist.
	// They are still required to be down, and the checks below prove that
	// there is no connectivity.
	fbAllowed := true
	if b, err := os.ReadFile("/proc/sys/net/core/fb_tunnels_only_for_init_net"); err == nil && strings.TrimSpace(string(b)) != "0" {
		fbAllowed = false
	}
	for _, line := range strings.Split(parts[0], "\n") {
		name, _, ok := strings.Cut(line, ":")
		if !ok {
			continue
		}
		name = strings.TrimSpace(name)
		if name == "lo" || (fbAllowed && slices.Contains(fallbackTunnels, name)) {
			continue
		}
		t.Errorf("unexpected interface %q in sandbox:\n%s", name, parts[0])
	}
	sawLo := false
	for _, line := range strings.Split(strings.TrimSpace(parts[1]), "\n") {
		// "1: lo: <LOOPBACK,UP,LOWER_UP> ..."
		f := strings.Fields(line)
		if len(f) < 3 {
			continue
		}
		name := strings.TrimSuffix(strings.Split(f[1], "@")[0], ":")
		up := strings.Contains(f[2], ",UP") || strings.Contains(f[2], "<UP")
		if name == "lo" {
			sawLo = true
			if !up {
				t.Errorf("loopback is down: %s", line)
			}
		}
		if name != "lo" && up {
			t.Errorf("non-loopback interface is up: %s", line)
		}
	}
	if !sawLo {
		t.Errorf("ip link did not list lo: %q", parts[1])
	}
	if strings.TrimSpace(parts[2]) != "" {
		t.Errorf("sandbox has IPv4 routes: %q", parts[2])
	}
	if !strings.Contains(parts[3], "Network unreachable") && !strings.Contains(parts[3], "Network is unreachable") {
		t.Errorf("outbound connection did not fail with ENETUNREACH: %q", parts[3])
	}
}

func TestFilesystem(t *testing.T) {
	e := setup(t, true)
	spec := e.spec(t, "sh", "-c", `
		touch /bin/x 2>/dev/null && echo rootfs-writable
		echo data > /workspace/out.txt && echo workspace-ok
		touch /tmp/t && echo tmp-ok
		for p in /src /usr/local/go "$STATE" /proc/1/root/src; do test -e "$p" && echo "visible $p"; done
		echo "uid=$(id -u) pwd=$(pwd) host=$(hostname)"
	`)
	spec.Env = append(spec.Env, "STATE="+e.state)
	res, out, errOut := e.run(t, spec)
	if res.ExitCode != 0 {
		t.Fatalf("result %+v stdout %q stderr %q", res, out, errOut)
	}
	want := "workspace-ok\ntmp-ok\nuid=0 pwd=/workspace host=sandbox\n"
	if out != want {
		t.Errorf("stdout = %q, want %q", out, want)
	}
	host := filepath.Join(spec.Workspace, "out.txt")
	data, err := os.ReadFile(host)
	if err != nil || string(data) != "data\n" {
		t.Fatalf("host workspace file: %q, %v", data, err)
	}
	st, _ := os.Stat(host)
	if uid := st.Sys().(*syscall.Stat_t).Uid; uid != defaultHostUIDBase {
		t.Errorf("workspace file owned by host uid %d, want %d", uid, defaultHostUIDBase)
	}
}

func TestPrivilegesDropped(t *testing.T) {
	e := setup(t, true)
	res, out, errOut := e.run(t, e.spec(t, "grep", "-E", "^(CapInh|CapPrm|CapEff|CapBnd|CapAmb|NoNewPrivs|Seccomp):", "/proc/self/status"))
	if res.ExitCode != 0 {
		t.Fatalf("result %+v stderr %q", res, errOut)
	}
	for _, line := range strings.Split(strings.TrimSpace(out), "\n") {
		k, v, _ := strings.Cut(line, ":")
		v = strings.TrimSpace(v)
		want := "0000000000000000"
		switch k {
		case "NoNewPrivs":
			want = "1"
		case "Seccomp":
			want = "2"
		}
		if v != want {
			t.Errorf("%s = %s, want %s", k, v, want)
		}
	}
}

func TestSeccompDeniesNamespaces(t *testing.T) {
	e := setup(t, true)
	res, out, errOut := e.run(t, e.spec(t, "sh", "-c", "unshare -U true; echo rc=$?; unshare -n true; echo rc=$?"))
	if res.ExitCode != 0 {
		t.Fatalf("result %+v stderr %q", res, errOut)
	}
	if out != "rc=1\nrc=1\n" || strings.Count(errOut, "Operation not permitted") != 2 {
		t.Errorf("stdout=%q stderr=%q; want both unshare calls to fail with EPERM", out, errOut)
	}
}

func TestEnvironmentIsExact(t *testing.T) {
	t.Setenv("SANDBOXD_CANARY", "leaked")
	e := setup(t, true)
	spec := e.spec(t, "env")
	spec.Env = []string{"PATH=/usr/bin:/bin", "FOO=bar", "EMPTY="}
	res, out, errOut := e.run(t, spec)
	if res.ExitCode != 0 {
		t.Fatalf("result %+v stderr %q", res, errOut)
	}
	got := strings.Split(strings.TrimSpace(out), "\n")
	slices.Sort(got)
	want := []string{"EMPTY=", "FOO=bar", "PATH=/usr/bin:/bin"}
	if !slices.Equal(got, want) {
		t.Errorf("env = %q, want exactly %q", got, want)
	}
}

func TestMemoryLimitOOM(t *testing.T) {
	e := setup(t, true)
	spec := e.spec(t, "sh", "-c", `x=$(head -c 300000000 /dev/zero | tr "\0" a); echo survived ${#x}`)
	spec.Resources.MemoryBytes = 32 << 20
	res, out, _ := e.run(t, spec)
	if !res.OOMKilled {
		t.Errorf("result = %+v, want OOMKilled", res)
	}
	if strings.Contains(out, "survived") {
		t.Errorf("workload survived the memory limit: %q", out)
	}
}

func TestPIDsLimit(t *testing.T) {
	e := setup(t, true)
	spec := e.spec(t, "sh", "-c", "for i in $(seq 1 100); do sleep 301 & done; wait")
	spec.Resources.PIDs = 8
	r := e.start(t, spec)
	waitUntil(t, 10*time.Second, "fork failure on stderr", func() bool {
		return strings.Contains(r.stderr.String(), "fork")
	})
	// pids.events counts forks refused by pids.max.
	b, err := os.ReadFile(filepath.Join(e.cgroupPath(spec), "pids.events"))
	if err != nil {
		t.Fatal(err)
	}
	if ev, err := cgroup.ParseKeyValues(bytes.NewReader(b)); err != nil || ev["max"] == 0 {
		t.Errorf("pids.events = %q, want refused forks", b)
	}
	if st, err := r.proc.Stats(); err != nil || st.PIDsCurrent > 8 {
		t.Errorf("Stats = %+v, %v; want at most 8 pids", st, err)
	}
	// busybox sh treats a failed fork as fatal, so the job may already be
	// ending; Kill must work either way and nothing may survive.
	if err := r.proc.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	res := r.wait(5 * time.Second)
	if res.ExitCode == 0 {
		t.Errorf("result = %+v, want failure", res)
	}
	if pids := findProcs("sleep301"); len(pids) > 0 {
		t.Errorf("sleep 301 still running: %v", pids)
	}
}

func TestSignalForwarded(t *testing.T) {
	e := setup(t, true)
	r := e.start(t, e.spec(t, "sh", "-c", `trap "exit 42" TERM; sleep 30 & echo ready; wait`))
	waitUntil(t, 10*time.Second, "workload ready", func() bool { return r.stdout.String() == "ready\n" })
	if err := r.proc.Signal(syscall.SIGTERM); err != nil {
		t.Fatalf("Signal: %v", err)
	}
	res := r.wait(10 * time.Second)
	if res.ExitCode != 42 || res.Signal != 0 {
		t.Errorf("result = %+v, want exit 42", res)
	}
}

func TestKillKillsEverything(t *testing.T) {
	e := setup(t, true)
	spec := e.spec(t, "sh", "-c", "sleep 1001 & sleep 1001 & wait")
	r := e.start(t, spec)
	procsFile := filepath.Join(e.cgroupPath(spec), "cgroup.procs")
	waitUntil(t, 10*time.Second, "init, sh and two sleeps in the cgroup", func() bool {
		b, err := os.ReadFile(procsFile)
		return err == nil && len(strings.Fields(string(b))) == 4
	})

	if err := r.proc.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	type waited struct {
		res runtime.Result
		err error
	}
	ch := make(chan waited, 1)
	start := time.Now()
	go func() { res, err := r.proc.Wait(); ch <- waited{res, err} }()
	var w waited
	select {
	case w = <-ch:
	case <-time.After(5 * time.Second):
		t.Fatal("Wait did not return after Kill")
	}
	if w.err != nil || w.res.Signal != int(syscall.SIGKILL) {
		t.Errorf("Wait = %+v, %v; want SIGKILL", w.res, w.err)
	}
	t.Logf("Wait returned %s after Kill", time.Since(start))
	waitUntil(t, 2*time.Second, "empty cgroup.procs", func() bool {
		b, err := os.ReadFile(procsFile)
		return err == nil && len(bytes.TrimSpace(b)) == 0
	})
	if pids := findProcs("sleep1001"); len(pids) > 0 {
		t.Errorf("sleep 1001 still running: %v", pids)
	}
	if err := r.proc.Kill(); err != nil {
		t.Errorf("Kill after exit: %v", err)
	}
	r.drained.Wait()
	if err := r.proc.Close(); err != nil {
		t.Fatalf("Close: %v", err)
	}
	if _, err := os.Stat(e.cgroupPath(spec)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cgroup still exists after Close")
	}
	if err := r.proc.Kill(); err != nil {
		t.Errorf("Kill after Close: %v", err)
	}
}

// findProcs returns pids whose NUL-joined argv equals cmdline with NULs removed.
// It scans the whole host, and go test runs packages in parallel (the cgroup
// and e2e suites start sleeps too), so callers use durations no other test
// uses: a match must be a survivor of this test, not someone else's job.
func findProcs(cmdline string) []int {
	entries, _ := os.ReadDir("/proc")
	var pids []int
	for _, ent := range entries {
		pid, err := strconv.Atoi(ent.Name())
		if err != nil {
			continue
		}
		b, err := os.ReadFile(filepath.Join("/proc", ent.Name(), "cmdline"))
		if err == nil && string(bytes.ReplaceAll(b, []byte{0}, nil)) == cmdline {
			pids = append(pids, pid)
		}
	}
	return pids
}

func TestStartFailureCleansUp(t *testing.T) {
	e := setup(t, true)
	for _, cmd := range [][]string{{"/no/such/binary"}, {"no-such-command"}} {
		spec := e.spec(t, cmd...)
		_, err := e.rt.Start(context.Background(), spec)
		if err == nil || !strings.Contains(err.Error(), "exec "+cmd[0]+":") {
			t.Errorf("Start(%v) error = %v, want exec failure", cmd, err)
		}
		if _, serr := os.Stat(e.cgroupPath(spec)); !errors.Is(serr, os.ErrNotExist) {
			t.Errorf("cgroup left behind after failed start of %v", cmd)
		}
	}
	spec := e.spec(t, "true")
	spec.WorkDir = "/does/not/exist"
	if _, err := e.rt.Start(context.Background(), spec); err == nil || !strings.Contains(err.Error(), "working directory") {
		t.Errorf("Start with missing workdir error = %v", err)
	}
	if ids, err := e.tree.List(); err != nil || len(ids) != 0 {
		t.Errorf("tree has leftover cgroups %v (err %v)", ids, err)
	}
	if zombies := countZombies(); zombies != 0 {
		t.Errorf("%d zombie processes left", zombies)
	}
}

func TestStartCancelled(t *testing.T) {
	e := setup(t, true)
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	spec := e.spec(t, "true")
	if _, err := e.rt.Start(ctx, spec); !errors.Is(err, context.Canceled) {
		t.Errorf("Start with cancelled ctx = %v, want context.Canceled", err)
	}
	if _, err := os.Stat(e.cgroupPath(spec)); !errors.Is(err, os.ErrNotExist) {
		t.Errorf("cgroup left behind after cancelled start")
	}
}

func countZombies() int {
	n := 0
	entries, _ := os.ReadDir("/proc")
	for _, ent := range entries {
		b, err := os.ReadFile(filepath.Join("/proc", ent.Name(), "stat"))
		if err != nil {
			continue
		}
		if i := bytes.LastIndexByte(b, ')'); i > 0 && len(b) > i+2 && b[i+2] == 'Z' {
			n++
		}
	}
	return n
}

func TestWithoutUserNS(t *testing.T) {
	e := setup(t, false)
	spec := e.spec(t, "sh", "-c", "id -u; id -g; echo x > /workspace/f; grep CapEff /proc/self/status")
	res, out, errOut := e.run(t, spec)
	if res.ExitCode != 0 {
		t.Fatalf("result %+v stdout %q stderr %q", res, out, errOut)
	}
	if !strings.HasPrefix(out, "65534\n65534\n") || !strings.Contains(out, "CapEff:\t0000000000000000") {
		t.Errorf("stdout = %q", out)
	}
	st, err := os.Stat(filepath.Join(spec.Workspace, "f"))
	if err != nil || st.Sys().(*syscall.Stat_t).Uid != defaultSandboxID {
		t.Errorf("workspace file: %v, %v", st, err)
	}
}
