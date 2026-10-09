package cgroup

import (
	"strings"
	"testing"
)

func TestCPUMax(t *testing.T) {
	tests := []struct {
		millis int
		want   string
	}{
		{500, "50000 100000"},
		{1000, "100000 100000"},
		{10, "1000 100000"},
		{2500, "250000 100000"},
		{0, "max 100000"},
		{-1, "max 100000"},
	}
	for _, tt := range tests {
		if got := CPUMax(tt.millis); got != tt.want {
			t.Errorf("CPUMax(%d) = %q, want %q", tt.millis, got, tt.want)
		}
	}
}

func TestParseKeyValues(t *testing.T) {
	tests := []struct {
		name    string
		in      string
		want    map[string]int64
		wantErr bool
	}{
		{
			name: "cpu.stat",
			in:   "usage_usec 1234\nuser_usec 1000\nsystem_usec 234\n",
			want: map[string]int64{"usage_usec": 1234, "user_usec": 1000, "system_usec": 234},
		},
		{
			name: "memory.events with blank line",
			in:   "low 0\nhigh 0\nmax 3\noom 1\noom_kill 1\n\n",
			want: map[string]int64{"low": 0, "high": 0, "max": 3, "oom": 1, "oom_kill": 1},
		},
		{name: "empty", in: "", want: map[string]int64{}},
		{name: "missing value", in: "usage_usec\n", wantErr: true},
		{name: "non-numeric", in: "usage_usec abc\n", wantErr: true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ParseKeyValues(strings.NewReader(tt.in))
			if tt.wantErr {
				if err == nil {
					t.Fatalf("expected error, got %v", got)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != len(tt.want) {
				t.Fatalf("got %v, want %v", got, tt.want)
			}
			for k, v := range tt.want {
				if got[k] != v {
					t.Errorf("%s = %d, want %d", k, got[k], v)
				}
			}
		})
	}
}

func TestParseInt(t *testing.T) {
	tests := []struct {
		in      string
		want    int64
		wantErr bool
	}{
		{"123\n", 123, false},
		{"max\n", -1, false},
		{"0", 0, false},
		{"", 0, true},
		{"12x", 0, true},
	}
	for _, tt := range tests {
		got, err := ParseInt([]byte(tt.in))
		if (err != nil) != tt.wantErr || got != tt.want {
			t.Errorf("ParseInt(%q) = %d, %v; want %d, err=%v", tt.in, got, err, tt.want, tt.wantErr)
		}
	}
}
