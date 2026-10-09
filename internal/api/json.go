package api

import (
	"math"
	"time"

	"github.com/Husqvarnalox/sandboxd/internal/job"
)

type resourcesJSON struct {
	MemoryMB  int64 `json:"memory_mb"`
	CPUMillis int   `json:"cpu_millis"`
	PIDs      int   `json:"pids"`
}

type jobRequest struct {
	Command        []string          `json:"command"`
	Env            map[string]string `json:"env"`
	WorkingDir     string            `json:"working_dir"`
	Network        string            `json:"network"`
	Resources      resourcesJSON     `json:"resources"`
	TimeoutSeconds int64             `json:"timeout_seconds"`
}

// spec converts the request, rejecting values that would overflow when scaled
// to bytes or nanoseconds (an overflow could wrap into an accepted range).
func (r *jobRequest) spec() (job.Spec, error) {
	if r.Resources.MemoryMB > math.MaxInt64>>20 || r.Resources.MemoryMB < 0 {
		return job.Spec{}, &job.ValidationError{Field: "resources.memory_mb", Message: "out of range"}
	}
	if r.TimeoutSeconds > int64(math.MaxInt64/time.Second) || r.TimeoutSeconds < 0 {
		return job.Spec{}, &job.ValidationError{Field: "timeout_seconds", Message: "out of range"}
	}
	return job.Spec{
		Command: r.Command,
		Env:     r.Env,
		WorkDir: r.WorkingDir,
		Network: r.Network,
		Resources: job.Resources{
			MemoryBytes: r.Resources.MemoryMB << 20,
			CPUMillis:   r.Resources.CPUMillis,
			PIDs:        r.Resources.PIDs,
		},
		Timeout: time.Duration(r.TimeoutSeconds) * time.Second,
	}, nil
}

type statsJSON struct {
	CPUSeconds         float64 `json:"cpu_seconds"`
	MemoryCurrentBytes int64   `json:"memory_current_bytes"`
	MemoryPeakBytes    *int64  `json:"memory_peak_bytes,omitempty"`
	PIDsCurrent        int     `json:"pids_current"`
	OOMKills           int     `json:"oom_kills"`
}

type jobJSON struct {
	ID              string            `json:"id"`
	State           job.State         `json:"state"`
	Command         []string          `json:"command"`
	Env             map[string]string `json:"env,omitempty"`
	WorkingDir      string            `json:"working_dir"`
	Network         string            `json:"network"`
	Resources       resourcesJSON     `json:"resources"`
	TimeoutSeconds  int64             `json:"timeout_seconds"`
	ExitCode        *int              `json:"exit_code"`
	Reason          string            `json:"reason,omitempty"`
	CreatedAt       string            `json:"created_at"`
	StartedAt       *string           `json:"started_at"`
	FinishedAt      *string           `json:"finished_at"`
	WallTimeSeconds *float64          `json:"wall_time_seconds,omitempty"`
	Stats           statsJSON         `json:"stats"`
}

const timeFormat = "2006-01-02T15:04:05.000Z07:00"

func formatTime(t time.Time) string { return t.UTC().Format(timeFormat) }

func optTime(t time.Time) *string {
	if t.IsZero() {
		return nil
	}
	s := formatTime(t)
	return &s
}

func toJSON(s job.Snapshot, now time.Time) jobJSON {
	out := jobJSON{
		ID:         s.ID,
		State:      s.State,
		Command:    s.Spec.Command,
		Env:        s.Spec.Env,
		WorkingDir: s.Spec.WorkDir,
		Network:    s.Spec.Network,
		Resources: resourcesJSON{
			MemoryMB:  s.Spec.Resources.MemoryBytes >> 20,
			CPUMillis: s.Spec.Resources.CPUMillis,
			PIDs:      s.Spec.Resources.PIDs,
		},
		TimeoutSeconds: int64(s.Spec.Timeout / time.Second),
		Reason:         s.Reason,
		CreatedAt:      formatTime(s.CreatedAt),
		StartedAt:      optTime(s.StartedAt),
		FinishedAt:     optTime(s.FinishedAt),
		Stats: statsJSON{
			CPUSeconds:         s.Stats.CPUUsage.Seconds(),
			MemoryCurrentBytes: s.Stats.MemoryCurrent,
			PIDsCurrent:        s.Stats.PIDsCurrent,
			OOMKills:           s.Stats.OOMKills,
		},
	}
	if s.Exited {
		code := s.ExitCode
		out.ExitCode = &code
	}
	if s.Stats.MemoryPeakKnown {
		peak := s.Stats.MemoryPeak
		out.Stats.MemoryPeakBytes = &peak
	}
	if !s.StartedAt.IsZero() {
		end := now
		if !s.FinishedAt.IsZero() {
			end = s.FinishedAt
		}
		wall := end.Sub(s.StartedAt).Seconds()
		out.WallTimeSeconds = &wall
	}
	return out
}

type stateEvent struct {
	State    job.State `json:"state"`
	ExitCode *int      `json:"exit_code"`
	Reason   string    `json:"reason,omitempty"`
}

type chunkEvent struct {
	Data         *string `json:"data,omitempty"`
	DataBase64   []byte  `json:"data_base64,omitempty"`
	DroppedBytes int64   `json:"dropped_bytes,omitempty"`
}

type errorBody struct {
	Error errorDetail `json:"error"`
}

type errorDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}
