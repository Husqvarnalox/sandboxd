package job

import (
	"errors"
	"strings"
	"testing"
	"time"
)

func TestValidateDefaults(t *testing.T) {
	l := DefaultLimits()
	s := Spec{Command: []string{"/bin/true"}}
	if err := s.Validate(l); err != nil {
		t.Fatal(err)
	}
	if s.WorkDir != DefaultWorkDir || s.Network != NetworkNone || s.Timeout != l.DefaultTimeout {
		t.Fatalf("defaults not applied: %+v", s)
	}
	if s.Resources != (Resources{MemoryBytes: l.DefaultMemory, CPUMillis: l.DefaultCPUMillis, PIDs: l.DefaultPIDs}) {
		t.Fatalf("resource defaults not applied: %+v", s.Resources)
	}
}

func TestValidateRejects(t *testing.T) {
	l := DefaultLimits()
	long := strings.Repeat("a", l.MaxArgLen+1)
	manyArgs := make([]string, l.MaxArgs+1)
	for i := range manyArgs {
		manyArgs[i] = "x"
	}
	manyEnv := map[string]string{}
	for i := 0; i < l.MaxEnv+1; i++ {
		manyEnv[strings.Repeat("A", i+1)] = "v"
	}

	cases := []struct {
		name  string
		spec  Spec
		field string
	}{
		{"empty command", Spec{}, "command"},
		{"empty executable", Spec{Command: []string{""}}, "command"},
		{"too many args", Spec{Command: manyArgs}, "command"},
		{"arg too long", Spec{Command: []string{long}}, "command"},
		{"arg with NUL", Spec{Command: []string{"a\x00b"}}, "command"},
		{"invalid utf8 arg", Spec{Command: []string{"\xff"}}, "command"},
		{"bad env key", Spec{Command: []string{"x"}, Env: map[string]string{"1A": "v"}}, "env"},
		{"env key with equals", Spec{Command: []string{"x"}, Env: map[string]string{"A=B": "v"}}, "env"},
		{"env value too long", Spec{Command: []string{"x"}, Env: map[string]string{"A": strings.Repeat("v", l.MaxEnvValueLen+1)}}, "env"},
		{"env value NUL", Spec{Command: []string{"x"}, Env: map[string]string{"A": "v\x00"}}, "env"},
		{"too many env", Spec{Command: []string{"x"}, Env: manyEnv}, "env"},
		{"relative workdir", Spec{Command: []string{"x"}, WorkDir: "rel"}, "working_dir"},
		{"network bridge", Spec{Command: []string{"x"}, Network: "bridge"}, "network"},
		{"timeout too small", Spec{Command: []string{"x"}, Timeout: time.Millisecond}, "timeout_seconds"},
		{"timeout too large", Spec{Command: []string{"x"}, Timeout: l.MaxTimeout + time.Second}, "timeout_seconds"},
		{"memory too small", Spec{Command: []string{"x"}, Resources: Resources{MemoryBytes: 1 << 20}}, "resources.memory_mb"},
		{"memory too large", Spec{Command: []string{"x"}, Resources: Resources{MemoryBytes: l.MaxMemory + 1}}, "resources.memory_mb"},
		{"cpu too small", Spec{Command: []string{"x"}, Resources: Resources{CPUMillis: 1}}, "resources.cpu_millis"},
		{"cpu too large", Spec{Command: []string{"x"}, Resources: Resources{CPUMillis: l.MaxCPUMillis + 1}}, "resources.cpu_millis"},
		{"pids negative", Spec{Command: []string{"x"}, Resources: Resources{PIDs: -1}}, "resources.pids"},
		{"pids too large", Spec{Command: []string{"x"}, Resources: Resources{PIDs: l.MaxPIDs + 1}}, "resources.pids"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.spec.Validate(l)
			var ve *ValidationError
			if !errors.As(err, &ve) {
				t.Fatalf("got %v, want ValidationError", err)
			}
			if ve.Field != tc.field {
				t.Fatalf("field = %q, want %q (%v)", ve.Field, tc.field, err)
			}
		})
	}
}

func TestValidateAcceptsShellStyleCommand(t *testing.T) {
	s := Spec{
		Command: []string{"/bin/sh", "-lc", "echo hello; sleep 1"},
		Env:     map[string]string{"FOO": "bar", "_X1": ""},
		Timeout: 10 * time.Second,
		Resources: Resources{
			MemoryBytes: 128 << 20,
			CPUMillis:   500,
			PIDs:        32,
		},
	}
	if err := s.Validate(DefaultLimits()); err != nil {
		t.Fatal(err)
	}
}
