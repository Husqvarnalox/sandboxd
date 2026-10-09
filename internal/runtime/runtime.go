// Package runtime defines the boundary between the job executor and the
// platform-specific sandbox implementation.
//
// The executor owns the job lifecycle (states, timeouts, cancellation, log
// capture); a Runtime owns everything below it: namespaces, cgroups, mounts,
// privilege reduction and the host process. The contract is deliberately
// narrow so the executor can be tested against a fake and the Linux runtime
// can be tested without the scheduler or HTTP layers.
package runtime

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/Husqvarnalox/sandboxd/internal/job"
)

// ErrUnavailable is returned by Check when the runtime cannot run jobs on
// this host (wrong OS, no cgroup v2, missing privileges, ...).
var ErrUnavailable = errors.New("runtime unavailable")

// Spec is a fully validated, fully resolved description of one sandbox.
type Spec struct {
	// ID is the canonical job id; the runtime may use it for cgroup and
	// directory names.
	ID      string
	Command []string
	// Env is the complete KEY=VALUE environment the workload receives. The
	// runtime adds nothing from the daemon's own environment.
	Env     []string
	WorkDir string
	// Workspace is the host path of the job's writable directory. It exists
	// and is empty when Start is called; the runtime fixes ownership and
	// mounts it at job.DefaultWorkDir inside the sandbox.
	Workspace string
	Resources job.Resources
	Hostname  string
}

// Runtime starts sandboxed processes.
type Runtime interface {
	// Start creates the sandbox and returns once the workload has exec'd, or
	// fails having released every resource it acquired. ctx bounds only the
	// start phase; it does not control the running workload.
	//
	// Isolation and resource limits are installed before the user command is
	// exec'd; a Process is never returned for a workload running outside them.
	Start(ctx context.Context, spec Spec) (Process, error)

	// Check reports whether the runtime is currently able to start jobs. It
	// must be cheap; it backs the readiness endpoint.
	Check() error
}

// Process is a started sandbox. Stdout/Stderr must be drained concurrently
// with Wait: the pipes reach EOF only after every process in the sandbox has
// exited or closed them.
type Process interface {
	Stdout() io.Reader
	Stderr() io.Reader

	// Wait blocks until the sandbox's init process has exited and been reaped.
	// It is called exactly once.
	Wait() (Result, error)

	// Signal delivers sig to the workload for graceful termination.
	Signal(sig os.Signal) error

	// Kill forcefully terminates every process in the sandbox. It is safe to
	// call concurrently with Wait and after the process has already exited.
	Kill() error

	// Stats reads current resource usage. It may be called while running and
	// after Wait, until Close.
	Stats() (job.Stats, error)

	// Close releases host resources (cgroup, descriptors). It must be called
	// after Wait returns and is idempotent. A non-nil error means a resource
	// may have leaked and should be logged by the caller.
	Close() error
}

// Result describes how the workload ended.
type Result struct {
	// ExitCode is the workload's exit status, or 128+signal when it was
	// killed by a signal.
	ExitCode int
	// Signal is the terminating signal number, 0 if it exited normally.
	Signal int
	// OOMKilled is true when the cgroup reported an OOM kill during the job.
	OOMKilled bool
}
