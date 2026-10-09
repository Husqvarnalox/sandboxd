package config

import (
	"errors"
	"flag"
	"strings"
	"testing"
	"time"
)

func env(m map[string]string) func(string) (string, bool) {
	return func(k string) (string, bool) {
		v, ok := m[k]
		return v, ok
	}
}

func TestDefaults(t *testing.T) {
	c, err := Parse(nil, env(nil))
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != ":8080" || c.StateDir != "/var/lib/sandboxd" || c.MaxQueueSize != 256 || c.MaxLogBytes != 4<<20 ||
		c.ShutdownTimeout != 30*time.Second || !c.UserNS || c.HostUIDBase != 100000 || c.Hostname != "sandbox" ||
		c.MaxConcurrentJobs < 1 || c.Rootfs != "" {
		t.Fatalf("defaults: %+v", c)
	}
	if err := c.RequireRootfs(); err == nil || !strings.Contains(err.Error(), "SANDBOXD_ROOTFS") {
		t.Fatalf("RequireRootfs: %v", err)
	}
}

func TestPrecedence(t *testing.T) {
	e := env(map[string]string{
		"SANDBOXD_LISTEN":              ":9000",
		"SANDBOXD_MAX_CONCURRENT_JOBS": "3",
		"SANDBOXD_KILL_GRACE_PERIOD":   "250ms",
		"SANDBOXD_USERNS":              "false",
		"SANDBOXD_ROOTFS":              "/env/rootfs",
	})
	c, err := Parse([]string{"--listen", "127.0.0.1:1", "--rootfs=/flag/rootfs"}, e)
	if err != nil {
		t.Fatal(err)
	}
	if c.Listen != "127.0.0.1:1" || c.Rootfs != "/flag/rootfs" {
		t.Fatalf("flags did not win: %+v", c)
	}
	if c.MaxConcurrentJobs != 3 || c.KillGracePeriod != 250*time.Millisecond || c.UserNS {
		t.Fatalf("env not applied: %+v", c)
	}
	if err := c.RequireRootfs(); err != nil {
		t.Fatal(err)
	}
}

func TestErrors(t *testing.T) {
	tests := []struct {
		name string
		args []string
		env  map[string]string
		want string
	}{
		{"unknown flag", []string{"--bogus"}, nil, "bogus"},
		{"bad env duration", nil, map[string]string{"SANDBOXD_SHUTDOWN_TIMEOUT": "soon"}, "SANDBOXD_SHUTDOWN_TIMEOUT"},
		{"bad env bool", nil, map[string]string{"SANDBOXD_USERNS": "perhaps"}, "SANDBOXD_USERNS"},
		{"zero concurrency", []string{"--max-concurrent-jobs=0"}, nil, "max-concurrent-jobs"},
		{"negative queue", nil, map[string]string{"SANDBOXD_MAX_QUEUE_SIZE": "-1"}, "max-queue-size"},
		{"log level", []string{"--log-level=verbose"}, nil, "log-level"},
		{"log format", []string{"--log-format=xml"}, nil, "log-format"},
		{"empty listen", []string{"--listen="}, nil, "listen"},
		{"cgroup name", []string{"--cgroup-name=a/b"}, nil, "cgroup-name"},
		{"root sandbox uid", []string{"--sandbox-uid=0"}, nil, "sandbox-uid"},
		{"positional", []string{"extra"}, nil, "extra"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := Parse(tt.args, env(tt.env))
			if err == nil || !strings.Contains(err.Error(), tt.want) {
				t.Fatalf("err = %v, want mention of %q", err, tt.want)
			}
		})
	}
}

func TestHelp(t *testing.T) {
	if _, err := Parse([]string{"-h"}, env(nil)); !errors.Is(err, flag.ErrHelp) {
		t.Fatalf("err = %v", err)
	}
	var b strings.Builder
	PrintFlags(&b)
	if !strings.Contains(b.String(), "SANDBOXD_MAX_CONCURRENT_JOBS") {
		t.Fatal(b.String())
	}
}

func TestEnvName(t *testing.T) {
	if got := EnvName("kill-grace-period"); got != "SANDBOXD_KILL_GRACE_PERIOD" {
		t.Fatal(got)
	}
}
