// Package runtimetest provides an in-memory runtime.Runtime for tests of the
// layers above the sandbox. It is the only test-support package in the tree.
package runtimetest

import (
	"bytes"
	"context"
	"io"
	"os"
	"sync"
	"syscall"

	"github.com/Husqvarnalox/sandboxd/internal/job"
	"github.com/Husqvarnalox/sandboxd/internal/runtime"
)

// Behavior configures one fake sandbox.
type Behavior struct {
	StartErr error
	// BlockStart makes Start block until its context is done.
	BlockStart bool

	ExitCode  int
	Signal    int
	OOMKilled bool
	WaitErr   error
	CloseErr  error

	// RunUntilSignal keeps the process alive until Signal or Kill. With
	// IgnoreTerm only Kill ends it.
	RunUntilSignal bool
	IgnoreTerm     bool
	// Release, if non-nil, keeps the process alive until it is closed (then
	// it exits with ExitCode), or until Signal/Kill.
	Release chan struct{}

	Stdout, Stderr []byte
	// LargeOutput writes this many bytes of 'x' to stdout in 64 KiB writes
	// before anything else happens.
	LargeOutput int64

	Stats    job.Stats
	StatsErr error
}

// Runtime is a fake runtime.Runtime. Behave decides what each Start does; a
// nil Behave means "exit 0 immediately".
type Runtime struct {
	Behave func(runtime.Spec) Behavior

	mu       sync.Mutex
	checkErr error
	starts   []runtime.Spec
	procs    map[string]*Process
}

func New(behave func(runtime.Spec) Behavior) *Runtime {
	return &Runtime{Behave: behave, procs: make(map[string]*Process)}
}

func (r *Runtime) SetCheckErr(err error) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.checkErr = err
}

func (r *Runtime) Check() error {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.checkErr
}

// Starts returns every spec passed to Start, including failed and blocked calls.
func (r *Runtime) Starts() []runtime.Spec {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]runtime.Spec(nil), r.starts...)
}

// Proc returns the process started for job id, or nil.
func (r *Runtime) Proc(id string) *Process {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.procs[id]
}

func (r *Runtime) Start(ctx context.Context, spec runtime.Spec) (runtime.Process, error) {
	var b Behavior
	if r.Behave != nil {
		b = r.Behave(spec)
	}
	r.mu.Lock()
	r.starts = append(r.starts, spec)
	r.mu.Unlock()

	if b.BlockStart {
		<-ctx.Done()
		return nil, ctx.Err()
	}
	if b.StartErr != nil {
		return nil, b.StartErr
	}
	p := newProcess(b)
	r.mu.Lock()
	r.procs[spec.ID] = p
	r.mu.Unlock()
	go p.run()
	return p, nil
}

// Process is a fake runtime.Process whose output is produced by a goroutine
// and whose lifetime is controlled by its Behavior.
type Process struct {
	b Behavior

	stdoutR, stderrR *io.PipeReader
	stdoutW, stderrW *io.PipeWriter

	once   sync.Once
	exited chan struct{}
	result runtime.Result
	term   chan struct{} // closed by a Signal the process honours

	mu      sync.Mutex
	signals []os.Signal
	killed  bool
	closed  int
}

func newProcess(b Behavior) *Process {
	p := &Process{b: b, exited: make(chan struct{}), term: make(chan struct{})}
	p.stdoutR, p.stdoutW = io.Pipe()
	p.stderrR, p.stderrW = io.Pipe()
	return p
}

func (p *Process) run() {
	if p.b.LargeOutput > 0 {
		chunk := bytes.Repeat([]byte{'x'}, 64<<10)
		for left := p.b.LargeOutput; left > 0; {
			n := min(left, int64(len(chunk)))
			if _, err := p.stdoutW.Write(chunk[:n]); err != nil {
				return // killed
			}
			left -= n
		}
	}
	if len(p.b.Stdout) > 0 {
		if _, err := p.stdoutW.Write(p.b.Stdout); err != nil {
			return
		}
	}
	if len(p.b.Stderr) > 0 {
		if _, err := p.stderrW.Write(p.b.Stderr); err != nil {
			return
		}
	}
	if p.b.RunUntilSignal || p.b.Release != nil {
		select {
		case <-p.b.Release: // nil channel blocks forever
		case <-p.term:
			p.exit(runtime.Result{ExitCode: 128 + int(syscall.SIGTERM), Signal: int(syscall.SIGTERM)})
		case <-p.exited:
			return
		}
	}
	p.exit(runtime.Result{ExitCode: p.b.ExitCode, Signal: p.b.Signal, OOMKilled: p.b.OOMKilled})
}

// exit records the first result and closes the pipes, which makes any
// pending output write fail like a write to a pipe whose reader died.
func (p *Process) exit(r runtime.Result) {
	p.once.Do(func() {
		p.result = r
		close(p.exited)
		p.stdoutW.Close()
		p.stderrW.Close()
	})
}

func (p *Process) Stdout() io.Reader { return p.stdoutR }
func (p *Process) Stderr() io.Reader { return p.stderrR }

func (p *Process) Wait() (runtime.Result, error) {
	<-p.exited
	if p.b.WaitErr != nil {
		return runtime.Result{}, p.b.WaitErr
	}
	return p.result, nil
}

func (p *Process) Signal(sig os.Signal) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.signals = append(p.signals, sig)
	if sig == syscall.SIGTERM && !p.b.IgnoreTerm {
		select {
		case <-p.term:
		default:
			close(p.term)
		}
	}
	return nil
}

func (p *Process) Kill() error {
	p.mu.Lock()
	p.killed = true
	p.mu.Unlock()
	p.exit(runtime.Result{ExitCode: 128 + int(syscall.SIGKILL), Signal: int(syscall.SIGKILL)})
	return nil
}

func (p *Process) Stats() (job.Stats, error) {
	return p.b.Stats, p.b.StatsErr
}

func (p *Process) Close() error {
	p.mu.Lock()
	defer p.mu.Unlock()
	p.closed++
	return p.b.CloseErr
}

// Signals returns the signals delivered so far.
func (p *Process) Signals() []os.Signal {
	p.mu.Lock()
	defer p.mu.Unlock()
	return append([]os.Signal(nil), p.signals...)
}

func (p *Process) Killed() bool {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.killed
}

// Closed returns how many times Close was called.
func (p *Process) Closed() int {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.closed
}

// Exited reports whether the process has ended.
func (p *Process) Exited() bool {
	select {
	case <-p.exited:
		return true
	default:
		return false
	}
}

var _ runtime.Runtime = (*Runtime)(nil)
