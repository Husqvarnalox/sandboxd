//go:build linux && integration

package cgroup

import (
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"syscall"
	"testing"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Husqvarnalox/sandboxd/internal/job"
)

func openTestTree(t *testing.T) *Tree {
	t.Helper()
	if os.Geteuid() != 0 {
		t.Skip("cgroup integration tests require root")
	}
	var st unix.Statfs_t
	if err := unix.Statfs(DefaultRoot, &st); err != nil || st.Type != unix.CGROUP2_SUPER_MAGIC {
		t.Skip("cgroup integration tests require cgroup v2 at " + DefaultRoot)
	}
	name := fmt.Sprintf("sandboxd-test-%d", os.Getpid())
	tree, err := Open(DefaultRoot, name)
	if err != nil {
		t.Fatalf("Open: %v", err)
	}
	t.Cleanup(func() {
		if err := unix.Rmdir(tree.Path()); err != nil && err != unix.ENOENT {
			t.Errorf("remove test tree: %v", err)
		}
	})
	return tree
}

func newID(t *testing.T) string {
	t.Helper()
	id, err := job.NewID()
	if err != nil {
		t.Fatal(err)
	}
	return id
}

func readTrim(t *testing.T, path string) string {
	t.Helper()
	b, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return strings.TrimSpace(string(b))
}

func startIn(t *testing.T, cg *Cgroup, args ...string) *exec.Cmd {
	t.Helper()
	cmd := exec.Command(args[0], args[1:]...)
	cmd.SysProcAttr = &syscall.SysProcAttr{UseCgroupFD: true, CgroupFD: cg.FD()}
	if err := cmd.Start(); err != nil {
		t.Fatalf("start %v in cgroup: %v", args, err)
	}
	return cmd
}

func waitFor(t *testing.T, timeout time.Duration, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestCreateLimitsStatsKillRemove(t *testing.T) {
	tree := openTestTree(t)
	if _, err := tree.Create("../escape", job.Resources{}); err == nil {
		t.Fatal("Create accepted a non-id name")
	}

	id := newID(t)
	cg, err := tree.Create(id, job.Resources{MemoryBytes: 64 << 20, CPUMillis: 500, PIDs: 16})
	if err != nil {
		t.Fatalf("Create: %v", err)
	}
	t.Cleanup(func() { _ = cg.Remove() })

	for file, want := range map[string]string{
		"cpu.max":          "50000 100000",
		"memory.max":       fmt.Sprint(64 << 20),
		"memory.oom.group": "1",
		"pids.max":         "16",
	} {
		if got := readTrim(t, filepath.Join(cg.Path(), file)); got != want {
			t.Errorf("%s = %q, want %q", file, got, want)
		}
	}
	if b, err := os.ReadFile(filepath.Join(cg.Path(), "memory.swap.max")); err == nil && strings.TrimSpace(string(b)) != "0" {
		t.Errorf("memory.swap.max = %q, want 0", b)
	}

	ids, err := tree.List()
	if err != nil || !slices.Contains(ids, id) {
		t.Fatalf("List = %v, %v; want it to contain %s", ids, err, id)
	}

	cmd := startIn(t, cg, "sh", "-c", "sleep 100 & sleep 100 & wait")
	waitFor(t, 5*time.Second, "3 processes in cgroup", func() bool {
		p, err := cg.Procs()
		return err == nil && len(p) == 3
	})
	procs, _ := cg.Procs()
	if !slices.Contains(procs, cmd.Process.Pid) {
		t.Errorf("cgroup.procs %v does not contain %d", procs, cmd.Process.Pid)
	}

	st, err := cg.Stats()
	if err != nil {
		t.Fatalf("Stats: %v", err)
	}
	if st.PIDsCurrent != 3 || st.MemoryCurrent <= 0 {
		t.Errorf("Stats = %+v, want 3 pids and non-zero memory", st)
	}
	if _, err := os.Stat(filepath.Join(cg.Path(), "memory.peak")); err == nil && (!st.MemoryPeakKnown || st.MemoryPeak <= 0) {
		t.Errorf("memory.peak exists but Stats = %+v", st)
	}

	if err := cg.Kill(); err != nil {
		t.Fatalf("Kill: %v", err)
	}
	err = cmd.Wait()
	var ee *exec.ExitError
	if !errors.As(err, &ee) || ee.Sys().(syscall.WaitStatus).Signal() != syscall.SIGKILL {
		t.Fatalf("Wait after Kill = %v, want SIGKILL", err)
	}
	waitFor(t, 5*time.Second, "cgroup to empty", func() bool {
		p, err := cg.Procs()
		return err == nil && len(p) == 0
	})

	if err := cg.Remove(); err != nil {
		t.Fatalf("Remove: %v", err)
	}
	if _, err := os.Stat(cg.Path()); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("cgroup dir still exists after Remove: %v", err)
	}
	if err := cg.Remove(); err != nil {
		t.Fatalf("second Remove: %v", err)
	}
	if err := cg.Kill(); err != nil {
		t.Fatalf("Kill after Remove: %v", err)
	}
}

func TestOOMKills(t *testing.T) {
	tree := openTestTree(t)
	cg, err := tree.Create(newID(t), job.Resources{MemoryBytes: 16 << 20, CPUMillis: 1000, PIDs: 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = cg.Remove() })
	cmd := startIn(t, cg, "sh", "-c", `x=$(head -c 100000000 /dev/zero | tr "\0" a); echo ${#x}`)
	_ = cmd.Wait()
	n, err := cg.OOMKills()
	if err != nil || n == 0 {
		t.Fatalf("OOMKills = %d, %v; want > 0", n, err)
	}
}

func TestRemoveStale(t *testing.T) {
	tree := openTestTree(t)
	cg, err := tree.Create(newID(t), job.Resources{MemoryBytes: 64 << 20, CPUMillis: 1000, PIDs: 16})
	if err != nil {
		t.Fatal(err)
	}
	cmd := startIn(t, cg, "sleep", "100")
	waitDone := make(chan error, 1)
	go func() { waitDone <- cmd.Wait() }()

	empty, err := tree.Create(newID(t), job.Resources{})
	if err != nil {
		t.Fatal(err)
	}
	_ = empty.dir.Close()

	foreign := filepath.Join(tree.Path(), "not-a-job")
	if err := os.Mkdir(foreign, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = unix.Rmdir(foreign) })

	_ = cg.dir.Close()
	n, err := tree.RemoveStale(slog.New(slog.NewTextHandler(io.Discard, nil)))
	if err != nil || n != 2 {
		t.Fatalf("RemoveStale = %d, %v; want 2, nil", n, err)
	}
	select {
	case <-waitDone:
	case <-time.After(5 * time.Second):
		t.Fatal("process in stale cgroup was not killed")
	}
	for _, p := range []string{cg.Path(), empty.Path()} {
		if _, err := os.Stat(p); !errors.Is(err, os.ErrNotExist) {
			t.Errorf("%s still exists", p)
		}
	}
	if _, err := os.Stat(foreign); err != nil {
		t.Errorf("RemoveStale touched a non-job directory: %v", err)
	}
}
