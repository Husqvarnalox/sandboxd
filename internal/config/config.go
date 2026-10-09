// Package config parses daemon settings from flags and SANDBOXD_* environment
// variables. Precedence is flag > environment > default.
package config

import (
	"errors"
	"flag"
	"fmt"
	"io"
	"log/slog"
	goruntime "runtime"
	"strconv"
	"strings"
	"time"
)

// Config holds every daemon setting.
type Config struct {
	Listen            string
	StateDir          string
	Rootfs            string
	CgroupRoot        string
	CgroupName        string
	MaxConcurrentJobs int
	MaxQueueSize      int
	MaxLogBytes       int
	MaxRequestBytes   int64
	ShutdownTimeout   time.Duration
	KillGracePeriod   time.Duration
	LogLevel          string
	LogFormat         string
	UserNS            bool
	HostUIDBase       int
	SandboxUID        int
	Hostname          string
}

// Parse reads flags from args (without the program and subcommand names),
// with defaults taken from the environment via getenv. It validates
// everything except the presence of Rootfs, which only `serve` needs (see
// RequireRootfs), so that `doctor` can report a missing rootfs instead of
// refusing to run. -h/--help returns flag.ErrHelp.
func Parse(args []string, getenv func(string) (string, bool)) (*Config, error) {
	c, fs := newFlagSet(io.Discard)
	// Seed each flag from its SANDBOXD_* twin before parsing the command
	// line, so explicit flags still win.
	var envErrs []error
	fs.VisitAll(func(f *flag.Flag) {
		name := EnvName(f.Name)
		if v, ok := getenv(name); ok {
			if err := f.Value.Set(v); err != nil {
				envErrs = append(envErrs, fmt.Errorf("invalid %s=%q: %w", name, v, err))
			}
		}
	})
	if err := errors.Join(envErrs...); err != nil {
		return nil, err
	}
	if err := fs.Parse(args); err != nil {
		return nil, err
	}
	if fs.NArg() > 0 {
		return nil, fmt.Errorf("unexpected argument %q", fs.Arg(0))
	}
	if err := c.validate(); err != nil {
		return nil, err
	}
	return c, nil
}

// PrintFlags writes the flag reference with defaults and environment names.
func PrintFlags(w io.Writer) {
	_, fs := newFlagSet(w)
	fs.VisitAll(func(f *flag.Flag) {
		fmt.Fprintf(w, "  --%s (env %s, default %q)\n      %s\n", f.Name, EnvName(f.Name), f.DefValue, f.Usage)
	})
}

func newFlagSet(out io.Writer) (*Config, *flag.FlagSet) {
	c := &Config{
		Listen:            ":8080",
		StateDir:          "/var/lib/sandboxd",
		CgroupRoot:        "/sys/fs/cgroup",
		CgroupName:        "sandboxd",
		MaxConcurrentJobs: goruntime.NumCPU(),
		MaxQueueSize:      256,
		MaxLogBytes:       4 << 20,
		MaxRequestBytes:   1 << 20,
		ShutdownTimeout:   30 * time.Second,
		KillGracePeriod:   5 * time.Second,
		LogLevel:          "info",
		LogFormat:         "text",
		UserNS:            true,
		HostUIDBase:       100000,
		SandboxUID:        65534,
		Hostname:          "sandbox",
	}
	fs := flag.NewFlagSet("sandboxd", flag.ContinueOnError)
	fs.SetOutput(out)
	fs.StringVar(&c.Listen, "listen", c.Listen, "HTTP listen address")
	fs.StringVar(&c.StateDir, "state-dir", c.StateDir, "directory for job workspaces")
	fs.StringVar(&c.Rootfs, "rootfs", c.Rootfs, "read-only root filesystem for sandboxes (required by serve)")
	fs.StringVar(&c.CgroupRoot, "cgroup-root", c.CgroupRoot, "cgroup v2 mount point")
	fs.StringVar(&c.CgroupName, "cgroup-name", c.CgroupName, "name of the daemon's cgroup subtree")
	fs.IntVar(&c.MaxConcurrentJobs, "max-concurrent-jobs", c.MaxConcurrentJobs, "jobs running at once")
	fs.IntVar(&c.MaxQueueSize, "max-queue-size", c.MaxQueueSize, "jobs waiting for a slot before submissions get 429")
	fs.IntVar(&c.MaxLogBytes, "max-log-bytes", c.MaxLogBytes, "log bytes retained per job (stdout+stderr)")
	fs.Int64Var(&c.MaxRequestBytes, "max-request-bytes", c.MaxRequestBytes, "maximum request body size")
	fs.DurationVar(&c.ShutdownTimeout, "shutdown-timeout", c.ShutdownTimeout, "time allowed for graceful shutdown")
	fs.DurationVar(&c.KillGracePeriod, "kill-grace-period", c.KillGracePeriod, "SIGTERM to SIGKILL delay when stopping a job")
	fs.StringVar(&c.LogLevel, "log-level", c.LogLevel, "debug, info, warn or error")
	fs.StringVar(&c.LogFormat, "log-format", c.LogFormat, "text or json")
	fs.BoolVar(&c.UserNS, "userns", c.UserNS, "run sandboxes in a user namespace")
	fs.IntVar(&c.HostUIDBase, "host-uid-base", c.HostUIDBase, "first host uid/gid of the 65536-id range mapped into sandboxes")
	fs.IntVar(&c.SandboxUID, "sandbox-uid", c.SandboxUID, "uid/gid for workloads when --userns=false")
	fs.StringVar(&c.Hostname, "hostname", c.Hostname, "hostname inside sandboxes")
	return c, fs
}

// EnvName returns the environment variable backing a flag.
func EnvName(flagName string) string {
	return "SANDBOXD_" + strings.ToUpper(strings.ReplaceAll(flagName, "-", "_"))
}

func (c *Config) validate() error {
	var errs []error
	check := func(ok bool, format string, args ...any) {
		if !ok {
			errs = append(errs, fmt.Errorf(format, args...))
		}
	}
	check(c.Listen != "", "--listen must not be empty")
	check(c.StateDir != "", "--state-dir must not be empty")
	check(c.CgroupRoot != "", "--cgroup-root must not be empty")
	check(c.CgroupName != "" && !strings.ContainsAny(c.CgroupName, "/\x00") && c.CgroupName != "." && c.CgroupName != "..",
		"--cgroup-name must be a single path component")
	check(c.MaxConcurrentJobs > 0, "--max-concurrent-jobs must be positive")
	check(c.MaxQueueSize > 0, "--max-queue-size must be positive")
	check(c.MaxLogBytes > 0, "--max-log-bytes must be positive")
	check(c.MaxRequestBytes > 0, "--max-request-bytes must be positive")
	check(c.ShutdownTimeout > 0, "--shutdown-timeout must be positive")
	check(c.KillGracePeriod > 0, "--kill-grace-period must be positive")
	check(c.HostUIDBase > 0 && c.HostUIDBase <= 1<<32-1-65536, "--host-uid-base out of range")
	check(c.SandboxUID > 0, "--sandbox-uid must be positive (workloads never run as root)")
	check(c.Hostname != "" && len(c.Hostname) <= 64, "--hostname must be 1-64 bytes")
	_, err := c.SlogLevel()
	check(err == nil, "--log-level must be debug, info, warn or error")
	check(c.LogFormat == "text" || c.LogFormat == "json", "--log-format must be text or json")
	return errors.Join(errs...)
}

// RequireRootfs reports the missing --rootfs that `serve` cannot run without.
func (c *Config) RequireRootfs() error {
	if c.Rootfs == "" {
		return fmt.Errorf("--rootfs (or %s) is required: point it at a root filesystem containing bin/sh", EnvName("rootfs"))
	}
	return nil
}

// SlogLevel converts LogLevel.
func (c *Config) SlogLevel() (slog.Level, error) {
	switch strings.ToLower(c.LogLevel) {
	case "debug":
		return slog.LevelDebug, nil
	case "info":
		return slog.LevelInfo, nil
	case "warn":
		return slog.LevelWarn, nil
	case "error":
		return slog.LevelError, nil
	}
	return 0, fmt.Errorf("unknown log level %s", strconv.Quote(c.LogLevel))
}
