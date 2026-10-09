package job

import (
	"fmt"
	"regexp"
	"time"
	"unicode/utf8"
)

// NetworkNone is the only network mode in v0.1: a fresh network namespace with
// no interfaces besides loopback.
const NetworkNone = "none"

// DefaultWorkDir is the sandbox-internal path of the writable workspace.
const DefaultWorkDir = "/workspace"

// Resources are the cgroup v2 limits applied to a job.
type Resources struct {
	MemoryBytes int64
	// CPUMillis is CPU bandwidth in thousandths of a core: 1000 == one core.
	CPUMillis int
	PIDs      int
}

// Spec describes what a job runs. It is validated and normalized by Validate
// before it reaches the scheduler.
type Spec struct {
	Command   []string
	Env       map[string]string
	WorkDir   string
	Resources Resources
	Timeout   time.Duration
	Network   string
}

// Limits bound what the API accepts. Zero values are filled from
// DefaultLimits by Normalize.
type Limits struct {
	MaxArgs       int
	MaxArgLen     int
	MaxCommandLen int

	MaxEnv         int
	MaxEnvKeyLen   int
	MaxEnvValueLen int

	MinTimeout     time.Duration
	MaxTimeout     time.Duration
	DefaultTimeout time.Duration

	MinMemory     int64
	MaxMemory     int64
	DefaultMemory int64

	MinCPUMillis     int
	MaxCPUMillis     int
	DefaultCPUMillis int

	MaxPIDs     int
	DefaultPIDs int
}

// DefaultLimits returns conservative defaults suitable for a single host.
func DefaultLimits() Limits {
	return Limits{
		MaxArgs:       256,
		MaxArgLen:     32 << 10,
		MaxCommandLen: 128 << 10,

		MaxEnv:         64,
		MaxEnvKeyLen:   256,
		MaxEnvValueLen: 4 << 10,

		MinTimeout:     time.Second,
		MaxTimeout:     time.Hour,
		DefaultTimeout: 5 * time.Minute,

		// The sandbox init process is a small Go binary and is charged to the
		// job's cgroup, so limits below ~16 MiB would OOM before the workload
		// starts.
		MinMemory:     16 << 20,
		MaxMemory:     4 << 30,
		DefaultMemory: 256 << 20,

		MinCPUMillis:     10,
		MaxCPUMillis:     16000,
		DefaultCPUMillis: 1000,

		MaxPIDs:     4096,
		DefaultPIDs: 64,
	}
}

// ValidationError describes a rejected field. It is safe to show to clients.
type ValidationError struct {
	Field   string
	Message string
}

func (e *ValidationError) Error() string {
	return fmt.Sprintf("%s: %s", e.Field, e.Message)
}

func invalid(field, format string, args ...any) error {
	return &ValidationError{Field: field, Message: fmt.Sprintf(format, args...)}
}

var envKeyRe = regexp.MustCompile(`^[A-Za-z_][A-Za-z0-9_]*$`)

// Validate fills defaults for omitted fields and checks the spec against l.
// It mutates s in place so the stored spec always reflects what will run.
func (s *Spec) Validate(l Limits) error {
	if len(s.Command) == 0 {
		return invalid("command", "must not be empty")
	}
	if len(s.Command) > l.MaxArgs {
		return invalid("command", "too many arguments (max %d)", l.MaxArgs)
	}
	total := 0
	for i, arg := range s.Command {
		if len(arg) > l.MaxArgLen {
			return invalid("command", "argument %d exceeds %d bytes", i, l.MaxArgLen)
		}
		if !utf8.ValidString(arg) {
			return invalid("command", "argument %d is not valid UTF-8", i)
		}
		for _, r := range arg {
			if r == 0 {
				return invalid("command", "argument %d contains NUL", i)
			}
		}
		total += len(arg)
	}
	if total > l.MaxCommandLen {
		return invalid("command", "command exceeds %d bytes", l.MaxCommandLen)
	}
	if s.Command[0] == "" {
		return invalid("command", "executable must not be empty")
	}

	if len(s.Env) > l.MaxEnv {
		return invalid("env", "too many variables (max %d)", l.MaxEnv)
	}
	for k, v := range s.Env {
		if len(k) > l.MaxEnvKeyLen || !envKeyRe.MatchString(k) {
			return invalid("env", "invalid variable name %q", truncate(k, 32))
		}
		if len(v) > l.MaxEnvValueLen {
			return invalid("env", "value of %s exceeds %d bytes", k, l.MaxEnvValueLen)
		}
		if !utf8.ValidString(v) {
			return invalid("env", "value of %s is not valid UTF-8", k)
		}
		for _, r := range v {
			if r == 0 {
				return invalid("env", "value of %s contains NUL", k)
			}
		}
	}

	if s.WorkDir == "" {
		s.WorkDir = DefaultWorkDir
	}
	if s.WorkDir[0] != '/' || len(s.WorkDir) > 4096 || !utf8.ValidString(s.WorkDir) {
		return invalid("working_dir", "must be an absolute path")
	}
	for _, r := range s.WorkDir {
		if r == 0 {
			return invalid("working_dir", "contains NUL")
		}
	}

	if s.Network == "" {
		s.Network = NetworkNone
	}
	if s.Network != NetworkNone {
		return invalid("network", "unsupported mode %q (only %q is available)", truncate(s.Network, 32), NetworkNone)
	}

	if s.Timeout == 0 {
		s.Timeout = l.DefaultTimeout
	}
	if s.Timeout < l.MinTimeout || s.Timeout > l.MaxTimeout {
		return invalid("timeout_seconds", "must be between %d and %d", int(l.MinTimeout.Seconds()), int(l.MaxTimeout.Seconds()))
	}

	r := &s.Resources
	if r.MemoryBytes == 0 {
		r.MemoryBytes = l.DefaultMemory
	}
	if r.MemoryBytes < l.MinMemory || r.MemoryBytes > l.MaxMemory {
		return invalid("resources.memory_mb", "must be between %d and %d", l.MinMemory>>20, l.MaxMemory>>20)
	}
	if r.CPUMillis == 0 {
		r.CPUMillis = l.DefaultCPUMillis
	}
	if r.CPUMillis < l.MinCPUMillis || r.CPUMillis > l.MaxCPUMillis {
		return invalid("resources.cpu_millis", "must be between %d and %d", l.MinCPUMillis, l.MaxCPUMillis)
	}
	if r.PIDs == 0 {
		r.PIDs = l.DefaultPIDs
	}
	if r.PIDs < 1 || r.PIDs > l.MaxPIDs {
		return invalid("resources.pids", "must be between 1 and %d", l.MaxPIDs)
	}
	return nil
}

func truncate(s string, n int) string {
	if len(s) <= n {
		return s
	}
	return s[:n] + "..."
}
