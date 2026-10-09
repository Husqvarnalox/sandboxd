//go:build linux

package linux

import (
	"bufio"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"syscall"

	"github.com/Husqvarnalox/sandboxd/internal/cgroup"
	"github.com/Husqvarnalox/sandboxd/internal/job"
	"github.com/Husqvarnalox/sandboxd/internal/runtime"
)

type process struct {
	cmd    *exec.Cmd
	cg     *cgroup.Cgroup
	stdout *os.File
	stderr *os.File
	status *bufio.Reader
	statR  *os.File
	log    *slog.Logger

	closeOnce sync.Once
	closeErr  error
}

func (p *process) Stdout() io.Reader { return p.stdout }
func (p *process) Stderr() io.Reader { return p.stderr }

// Wait reaps the init. Init exits with the workload's status (128+signal when
// the workload was signalled) and reports the signal on the status pipe, so
// the result reflects the workload, not init. If init itself was killed (OOM
// group kill, Kill) the init's own wait status is used.
func (p *process) Wait() (runtime.Result, error) {
	err := p.cmd.Wait()
	var res runtime.Result
	ws, ok := p.cmd.ProcessState.Sys().(syscall.WaitStatus)
	switch {
	case !ok:
		return res, fmt.Errorf("wait sandbox init: %w", err)
	case ws.Signaled():
		res.Signal = int(ws.Signal())
		res.ExitCode = 128 + res.Signal
	default:
		res.ExitCode = ws.ExitStatus()
		line, _ := p.status.ReadString('\n')
		if s, ok := strings.CutPrefix(strings.TrimSpace(line), "signal "); ok {
			if n, err := strconv.Atoi(s); err == nil {
				res.Signal = n
			}
		}
	}
	n, oerr := p.cg.OOMKills()
	if oerr != nil {
		return res, fmt.Errorf("wait sandbox: %w", oerr)
	}
	res.OOMKilled = n > 0
	return res, nil
}

func (p *process) Signal(sig os.Signal) error {
	err := p.cmd.Process.Signal(sig)
	if errors.Is(err, os.ErrProcessDone) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("signal sandbox init: %w", err)
	}
	return nil
}

// Kill uses the cgroup rather than the init pid so that every process of the
// job dies, including any the init failed to track, even if init is stuck.
func (p *process) Kill() error {
	err := p.cg.Kill()
	if perr := p.cmd.Process.Kill(); perr != nil && !errors.Is(perr, os.ErrProcessDone) {
		err = errors.Join(err, fmt.Errorf("kill sandbox init: %w", perr))
	}
	return err
}

func (p *process) Stats() (job.Stats, error) { return p.cg.Stats() }

func (p *process) Close() error {
	p.closeOnce.Do(func() {
		var errs []error
		for _, f := range []*os.File{p.stdout, p.stderr, p.statR} {
			if err := f.Close(); err != nil && !errors.Is(err, os.ErrClosed) {
				errs = append(errs, err)
			}
		}
		if err := p.cg.Remove(); err != nil {
			errs = append(errs, err)
		}
		p.closeErr = errors.Join(errs...)
	})
	return p.closeErr
}
