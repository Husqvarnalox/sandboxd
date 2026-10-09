//go:build linux

package cgroup

import (
	"bytes"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/Husqvarnalox/sandboxd/internal/job"
)

// DefaultRoot is where the unified cgroup v2 hierarchy is mounted.
const DefaultRoot = "/sys/fs/cgroup"

// controllers are the controllers every job cgroup needs.
var controllers = []string{"cpu", "memory", "pids"}

// Tree is the daemon's own cgroup subtree. Job cgroups are created directly
// below it and named by job id.
type Tree struct {
	path string
}

// Open prepares <root>/<name> as the daemon's subtree: root must be a cgroup2
// mount and the cpu, memory and pids controllers must be delegated to it.
func Open(root, name string) (*Tree, error) {
	var st unix.Statfs_t
	if err := unix.Statfs(root, &st); err != nil {
		return nil, fmt.Errorf("statfs %s: %w", root, err)
	}
	if st.Type != unix.CGROUP2_SUPER_MAGIC {
		return nil, fmt.Errorf("%s is not a cgroup v2 mount (cgroup v1 and hybrid hierarchies are unsupported)", root)
	}
	if name == "" || name != filepath.Base(name) || name == "." || name == ".." {
		return nil, fmt.Errorf("invalid cgroup tree name %q", name)
	}

	available, err := readFields(filepath.Join(root, "cgroup.controllers"))
	if err != nil {
		return nil, err
	}
	for _, c := range controllers {
		if !available[c] {
			return nil, fmt.Errorf("cgroup controller %q is not available in %s; it must be delegated to this cgroup (systemd: Delegate=yes)", c, root)
		}
	}
	enabled, err := readFields(filepath.Join(root, "cgroup.subtree_control"))
	if err != nil {
		return nil, err
	}
	for _, c := range controllers {
		if enabled[c] {
			continue
		}
		if err := writeFile(filepath.Join(root, "cgroup.subtree_control"), "+"+c); err != nil {
			return nil, fmt.Errorf("enable cgroup controller %q in %s (check cgroup delegation; the cgroup must not contain processes itself): %w", c, root, err)
		}
	}

	path := filepath.Join(root, name)
	if err := os.Mkdir(path, 0o755); err != nil && !errors.Is(err, os.ErrExist) {
		return nil, fmt.Errorf("create cgroup tree %s: %w", path, err)
	}
	for _, c := range controllers {
		if err := writeFile(filepath.Join(path, "cgroup.subtree_control"), "+"+c); err != nil {
			return nil, fmt.Errorf("enable cgroup controller %q in %s: %w", c, path, err)
		}
	}
	return &Tree{path: path}, nil
}

// Path returns the absolute path of the subtree.
func (t *Tree) Path() string { return t.path }

// Create makes the cgroup for job id with every limit in r installed. No
// process is ever placed in a cgroup whose limits are not fully written.
func (t *Tree) Create(id string, r job.Resources) (*Cgroup, error) {
	if !job.ValidID(id) {
		return nil, fmt.Errorf("create cgroup: invalid job id %q", id)
	}
	path := filepath.Join(t.path, id)
	if err := os.Mkdir(path, 0o755); err != nil {
		return nil, fmt.Errorf("create cgroup %s: %w", path, err)
	}
	cg, err := configure(path, r)
	if err != nil {
		if rerr := unix.Rmdir(path); rerr != nil {
			err = errors.Join(err, fmt.Errorf("remove cgroup %s: %w", path, rerr))
		}
		return nil, err
	}
	return cg, nil
}

func configure(path string, r job.Resources) (*Cgroup, error) {
	memMax, pidsMax := "max", "max"
	if r.MemoryBytes > 0 {
		memMax = strconv.FormatInt(r.MemoryBytes, 10)
	}
	if r.PIDs > 0 {
		pidsMax = strconv.Itoa(r.PIDs)
	}
	settings := []struct {
		file, value string
		optional    bool
	}{
		{"cpu.max", CPUMax(r.CPUMillis), false},
		{"memory.max", memMax, false},
		// Without swap accounting (CONFIG_MEMCG_SWAP=n) the file does not
		// exist and there is no swap to disable.
		{"memory.swap.max", "0", true},
		// Kill the whole job atomically on OOM rather than a single victim.
		{"memory.oom.group", "1", false},
		{"pids.max", pidsMax, false},
	}
	for _, s := range settings {
		err := writeFile(filepath.Join(path, s.file), s.value)
		if err != nil && !(s.optional && errors.Is(err, os.ErrNotExist)) {
			return nil, fmt.Errorf("set %s=%s: %w", s.file, s.value, err)
		}
	}
	dir, err := os.OpenFile(path, os.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC, 0)
	if err != nil {
		return nil, fmt.Errorf("open cgroup dir %s: %w", path, err)
	}
	return &Cgroup{path: path, dir: dir}, nil
}

// List returns the names of child cgroups that are canonical job ids. Other
// directories are not ours and are never reported.
func (t *Tree) List() ([]string, error) {
	entries, err := os.ReadDir(t.path)
	if err != nil {
		return nil, fmt.Errorf("list cgroup tree %s: %w", t.path, err)
	}
	var ids []string
	for _, e := range entries {
		if e.IsDir() && job.ValidID(e.Name()) {
			ids = append(ids, e.Name())
		}
	}
	return ids, nil
}

// RemoveStale kills and removes job cgroups left behind by a previous daemon
// instance. It only touches job-id-named directories below the tree.
func (t *Tree) RemoveStale(log *slog.Logger) (int, error) {
	ids, err := t.List()
	if err != nil {
		return 0, err
	}
	removed := 0
	var errs []error
	for _, id := range ids {
		cg := &Cgroup{path: filepath.Join(t.path, id)}
		procs, err := cg.Procs()
		if err != nil {
			errs = append(errs, err)
			continue
		}
		if len(procs) > 0 {
			log.Warn("killing processes in stale job cgroup", "cgroup", cg.path, "procs", len(procs))
			if err := cg.Kill(); err != nil {
				errs = append(errs, err)
				continue
			}
			if err := cg.waitEmpty(5 * time.Second); err != nil {
				errs = append(errs, err)
				continue
			}
		}
		if err := cg.Remove(); err != nil {
			errs = append(errs, err)
			continue
		}
		log.Info("removed stale job cgroup", "cgroup", cg.path)
		removed++
	}
	return removed, errors.Join(errs...)
}

// Cgroup is one job's cgroup.
type Cgroup struct {
	path string
	dir  *os.File
}

// Path returns the absolute path of the cgroup directory.
func (c *Cgroup) Path() string { return c.path }

// FD returns a directory descriptor suitable for CLONE_INTO_CGROUP
// (syscall.SysProcAttr.CgroupFD). It is valid until Remove.
func (c *Cgroup) FD() int { return int(c.dir.Fd()) }

// Stats reads current usage. memory.peak only exists since Linux 5.19; when it
// is missing MemoryPeakKnown is false.
func (c *Cgroup) Stats() (job.Stats, error) {
	var s job.Stats
	cur, err := c.readInt("memory.current")
	if err != nil {
		return s, err
	}
	s.MemoryCurrent = cur
	switch peak, err := c.readInt("memory.peak"); {
	case err == nil:
		s.MemoryPeak, s.MemoryPeakKnown = peak, true
	case !errors.Is(err, os.ErrNotExist):
		return s, err
	}
	cpu, err := c.readKeyValues("cpu.stat")
	if err != nil {
		return s, err
	}
	s.CPUUsage = time.Duration(cpu["usage_usec"]) * time.Microsecond
	pids, err := c.readInt("pids.current")
	if err != nil {
		return s, err
	}
	s.PIDsCurrent = int(pids)
	if s.OOMKills, err = c.OOMKills(); err != nil {
		return s, err
	}
	return s, nil
}

// OOMKills returns the number of processes the kernel OOM-killed in this
// cgroup (memory.events oom_kill, which includes descendants).
func (c *Cgroup) OOMKills() (int, error) {
	ev, err := c.readKeyValues("memory.events")
	if err != nil {
		return 0, err
	}
	return int(ev["oom_kill"]), nil
}

// Kill SIGKILLs every process in the cgroup, whatever pid namespace it lives
// in. cgroup.kill (Linux 5.14+) does this atomically, including processes
// forked concurrently; older kernels fall back to killing listed pids until
// the cgroup is empty.
func (c *Cgroup) Kill() error {
	err := writeFile(filepath.Join(c.path, "cgroup.kill"), "1")
	if err == nil {
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("kill cgroup %s: %w", c.path, err)
	}
	if _, serr := os.Stat(c.path); errors.Is(serr, os.ErrNotExist) {
		return nil
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		procs, err := c.Procs()
		if err != nil {
			return err
		}
		if len(procs) == 0 {
			return nil
		}
		for _, pid := range procs {
			if err := unix.Kill(pid, unix.SIGKILL); err != nil && err != unix.ESRCH {
				return fmt.Errorf("kill pid %d in %s: %w", pid, c.path, err)
			}
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("kill cgroup %s: %d processes remain", c.path, len(procs))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// Procs lists the pids (in the caller's pid namespace) in the cgroup.
func (c *Cgroup) Procs() ([]int, error) {
	data, err := os.ReadFile(filepath.Join(c.path, "cgroup.procs"))
	if err != nil {
		return nil, fmt.Errorf("read cgroup.procs of %s: %w", c.path, err)
	}
	var pids []int
	for _, f := range strings.Fields(string(data)) {
		pid, err := strconv.Atoi(f)
		if err != nil {
			return nil, fmt.Errorf("parse cgroup.procs of %s: %w", c.path, err)
		}
		pids = append(pids, pid)
	}
	return pids, nil
}

// Remove closes the directory descriptor and removes the cgroup. Killed
// processes may take a moment to leave the cgroup, so EBUSY is retried for
// about two seconds. Removing an already removed cgroup is not an error.
func (c *Cgroup) Remove() error {
	var errs []error
	if c.dir != nil {
		if err := c.dir.Close(); err != nil {
			errs = append(errs, fmt.Errorf("close cgroup dir %s: %w", c.path, err))
		}
		c.dir = nil
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := unix.Rmdir(c.path)
		if err == nil || err == unix.ENOENT {
			break
		}
		if err != unix.EBUSY || time.Now().After(deadline) {
			errs = append(errs, fmt.Errorf("remove cgroup %s: %w", c.path, err))
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	return errors.Join(errs...)
}

func (c *Cgroup) waitEmpty(timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		procs, err := c.Procs()
		if err != nil {
			return err
		}
		if len(procs) == 0 {
			return nil
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("cgroup %s still has %d processes after %s", c.path, len(procs), timeout)
		}
		time.Sleep(20 * time.Millisecond)
	}
}

func (c *Cgroup) readInt(file string) (int64, error) {
	data, err := os.ReadFile(filepath.Join(c.path, file))
	if err != nil {
		return 0, fmt.Errorf("read %s: %w", file, err)
	}
	return ParseInt(data)
}

func (c *Cgroup) readKeyValues(file string) (map[string]int64, error) {
	data, err := os.ReadFile(filepath.Join(c.path, file))
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", file, err)
	}
	m, err := ParseKeyValues(bytes.NewReader(data))
	if err != nil {
		return nil, fmt.Errorf("parse %s: %w", file, err)
	}
	return m, nil
}

func readFields(path string) (map[string]bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	m := make(map[string]bool)
	for _, f := range strings.Fields(string(data)) {
		m[f] = true
	}
	return m, nil
}

// writeFile writes value to an existing cgroup control file. cgroupfs reports
// errors from write(2), so the file is never created or truncated.
func writeFile(path, value string) error {
	f, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	_, werr := f.WriteString(value)
	cerr := f.Close()
	if werr != nil {
		return werr
	}
	return cerr
}
