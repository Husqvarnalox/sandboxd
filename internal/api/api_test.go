package api

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"github.com/Husqvarnalox/sandboxd/internal/executor"
	"github.com/Husqvarnalox/sandboxd/internal/job"
	"github.com/Husqvarnalox/sandboxd/internal/metrics"
	"github.com/Husqvarnalox/sandboxd/internal/runtime"
	"github.com/Husqvarnalox/sandboxd/internal/runtime/runtimetest"
	"github.com/Husqvarnalox/sandboxd/internal/scheduler"
)

const testDeadline = 20 * time.Second

type env struct {
	srv      *httptest.Server
	ex       *executor.Executor
	rt       *runtimetest.Runtime
	stateDir string
}

type setup struct {
	sched      scheduler.Config
	behaviours map[string]runtimetest.Behavior
	ready      func() error
}

func newEnv(t *testing.T, s setup) *env {
	t.Helper()
	if s.sched.MaxConcurrent == 0 {
		s.sched = scheduler.Config{MaxConcurrent: 4, MaxQueue: 16}
	}
	rt := runtimetest.New(func(sp runtime.Spec) runtimetest.Behavior { return s.behaviours[sp.Command[0]] })
	reg := prometheus.NewRegistry()
	m := metrics.New(reg)
	sched := scheduler.New(s.sched)
	m.RegisterScheduler(reg, sched.Stats)
	stateDir := t.TempDir()
	ex := executor.New(executor.Config{StateDir: stateDir, KillGracePeriod: 50 * time.Millisecond}, rt, sched, m, nil)
	sched.Start()
	srv := httptest.NewServer(New(Config{
		Executor:     ex,
		Ready:        s.ready,
		Metrics:      metrics.Handler(reg),
		Version:      VersionInfo{Version: "1.2.3", Commit: "abc", GoVersion: "go1.26"},
		MaxBodyBytes: 4096,
	}))
	t.Cleanup(func() {
		srv.Close()
		ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
		defer cancel()
		if err := ex.Shutdown(ctx); err != nil {
			t.Errorf("shutdown: %v", err)
		}
	})
	return &env{srv: srv, ex: ex, rt: rt, stateDir: stateDir}
}

func (e *env) do(t *testing.T, method, path, body string) (*http.Response, []byte) {
	t.Helper()
	req, err := http.NewRequest(method, e.srv.URL+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	data, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp, data
}

func (e *env) create(t *testing.T, cmd string) jobJSON {
	t.Helper()
	resp, body := e.do(t, http.MethodPost, "/v1/jobs", `{"command":["`+cmd+`"]}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("create: %d %s", resp.StatusCode, body)
	}
	var j jobJSON
	mustUnmarshal(t, body, &j)
	return j
}

func (e *env) waitState(t *testing.T, id string, done func(job.State) bool) job.Snapshot {
	t.Helper()
	deadline := time.Now().Add(testDeadline)
	for {
		snap, err := e.ex.Get(id)
		if err != nil {
			t.Fatal(err)
		}
		if done(snap.State) {
			return snap
		}
		if time.Now().After(deadline) {
			t.Fatalf("job %s stuck in %s", id, snap.State)
		}
		time.Sleep(time.Millisecond)
	}
}

func mustUnmarshal(t *testing.T, data []byte, v any) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("unmarshal %s: %v", data, err)
	}
}

func errCode(t *testing.T, body []byte) string {
	t.Helper()
	var e errorBody
	mustUnmarshal(t, body, &e)
	return e.Error.Code
}

func TestCreateValidation(t *testing.T) {
	e := newEnv(t, setup{})
	tests := []struct {
		name, body string
		status     int
		code       string
	}{
		{"empty command", `{"command":[]}`, 400, "INVALID_REQUEST"},
		{"missing command", `{}`, 400, "INVALID_REQUEST"},
		{"unknown field", `{"command":["/bin/true"],"privileged":true}`, 400, "INVALID_REQUEST"},
		{"bad env key", `{"command":["/bin/true"],"env":{"1BAD":"x"}}`, 400, "INVALID_REQUEST"},
		{"timeout too big", `{"command":["/bin/true"],"timeout_seconds":999999}`, 400, "INVALID_REQUEST"},
		{"timeout overflow", `{"command":["/bin/true"],"timeout_seconds":9223372036854775807}`, 400, "INVALID_REQUEST"},
		{"memory too small", `{"command":["/bin/true"],"resources":{"memory_mb":1}}`, 400, "INVALID_REQUEST"},
		{"memory overflow", `{"command":["/bin/true"],"resources":{"memory_mb":9007199254740993}}`, 400, "INVALID_REQUEST"},
		{"bad network", `{"command":["/bin/true"],"network":"host"}`, 400, "INVALID_REQUEST"},
		{"not json", `command=/bin/true`, 400, "INVALID_REQUEST"},
		{"empty body", ``, 400, "INVALID_REQUEST"},
		{"trailing data", `{"command":["/bin/true"]} {}`, 400, "INVALID_REQUEST"},
		{"oversized", `{"command":["/bin/true"],"env":{"A":"` + strings.Repeat("x", 5000) + `"}}`, 413, "REQUEST_TOO_LARGE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			resp, body := e.do(t, http.MethodPost, "/v1/jobs", tt.body)
			if resp.StatusCode != tt.status || errCode(t, body) != tt.code {
				t.Fatalf("got %d %s", resp.StatusCode, body)
			}
		})
	}
	if n := len(e.ex.List()); n != 0 {
		t.Fatalf("%d jobs created by invalid requests", n)
	}
}

func TestCreateShapeAndGet(t *testing.T) {
	e := newEnv(t, setup{behaviours: map[string]runtimetest.Behavior{
		"/bin/x": {ExitCode: 0, Stats: job.Stats{CPUUsage: 250 * time.Millisecond, MemoryPeak: 1 << 20, MemoryPeakKnown: true}},
	}})
	resp, body := e.do(t, http.MethodPost, "/v1/jobs",
		`{"command":["/bin/x","-c","echo hi"],"env":{"K":"V"},"resources":{"memory_mb":128,"cpu_millis":500,"pids":32},"timeout_seconds":10}`)
	if resp.StatusCode != http.StatusCreated {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	var raw map[string]any
	mustUnmarshal(t, body, &raw)
	for _, k := range []string{"id", "state", "command", "env", "working_dir", "network", "resources", "timeout_seconds", "exit_code", "created_at", "started_at", "finished_at", "stats"} {
		if _, ok := raw[k]; !ok {
			t.Errorf("missing %q in %s", k, body)
		}
	}
	var j jobJSON
	mustUnmarshal(t, body, &j)
	if !job.ValidID(j.ID) || j.WorkingDir != "/workspace" || j.Network != "none" || j.TimeoutSeconds != 10 ||
		j.Resources != (resourcesJSON{128, 500, 32}) || j.Env["K"] != "V" {
		t.Fatalf("unexpected job %+v", j)
	}
	if resp.Header.Get("Location") != "/v1/jobs/"+j.ID {
		t.Fatalf("location = %q", resp.Header.Get("Location"))
	}
	if _, err := time.Parse(time.RFC3339, j.CreatedAt); err != nil || !strings.HasSuffix(j.CreatedAt, "Z") {
		t.Fatalf("created_at %q: %v", j.CreatedAt, err)
	}

	e.waitState(t, j.ID, job.State.Terminal)
	resp, body = e.do(t, http.MethodGet, "/v1/jobs/"+j.ID, "")
	if resp.StatusCode != 200 {
		t.Fatalf("%d %s", resp.StatusCode, body)
	}
	mustUnmarshal(t, body, &j)
	if j.State != job.StateSucceeded || j.ExitCode == nil || *j.ExitCode != 0 || j.StartedAt == nil ||
		j.FinishedAt == nil || j.WallTimeSeconds == nil || j.Stats.CPUSeconds != 0.25 ||
		j.Stats.MemoryPeakBytes == nil || *j.Stats.MemoryPeakBytes != 1<<20 {
		t.Fatalf("finished job %s", body)
	}

	resp, body = e.do(t, http.MethodGet, "/v1/jobs", "")
	var list struct{ Jobs []jobJSON }
	mustUnmarshal(t, body, &list)
	if resp.StatusCode != 200 || len(list.Jobs) != 1 || list.Jobs[0].ID != j.ID {
		t.Fatalf("list: %d %s", resp.StatusCode, body)
	}
}

func TestNotFound(t *testing.T) {
	e := newEnv(t, setup{})
	id, _ := job.NewID()
	for _, tc := range []struct{ method, path string }{
		{"GET", "/v1/jobs/" + id},
		{"GET", "/v1/jobs/not-a-uuid"},
		{"POST", "/v1/jobs/" + id + "/cancel"},
		{"DELETE", "/v1/jobs/" + id},
		{"GET", "/v1/jobs/" + id + "/logs"},
		{"GET", "/v1/jobs/" + id + "/artifacts/x"},
		{"GET", "/nope"},
	} {
		resp, body := e.do(t, tc.method, tc.path, "")
		if resp.StatusCode != 404 || errCode(t, body) != "NOT_FOUND" {
			t.Fatalf("%s %s: %d %s", tc.method, tc.path, resp.StatusCode, body)
		}
	}
	resp, body := e.do(t, http.MethodPut, "/v1/jobs", "")
	if resp.StatusCode != 405 || errCode(t, body) != "METHOD_NOT_ALLOWED" || resp.Header.Get("Allow") == "" {
		t.Fatalf("405: %d %s %v", resp.StatusCode, body, resp.Header)
	}
}

func TestCancelAndConflicts(t *testing.T) {
	e := newEnv(t, setup{behaviours: map[string]runtimetest.Behavior{"/bin/run": {RunUntilSignal: true}}})
	running := e.create(t, "/bin/run")
	e.waitState(t, running.ID, func(s job.State) bool { return s == job.StateRunning })

	resp, body := e.do(t, http.MethodDelete, "/v1/jobs/"+running.ID, "")
	if resp.StatusCode != 409 || errCode(t, body) != "JOB_NOT_FINISHED" {
		t.Fatalf("delete running: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(t, http.MethodGet, "/v1/jobs/"+running.ID+"/artifacts/out.txt", "")
	if resp.StatusCode != 409 {
		t.Fatalf("artifact while running: %d %s", resp.StatusCode, body)
	}

	resp, body = e.do(t, http.MethodPost, "/v1/jobs/"+running.ID+"/cancel", "")
	if resp.StatusCode != 202 {
		t.Fatalf("cancel: %d %s", resp.StatusCode, body)
	}
	snap := e.waitState(t, running.ID, job.State.Terminal)
	if snap.State != job.StateCancelled || snap.Reason != job.ReasonUser {
		t.Fatalf("got %s/%s", snap.State, snap.Reason)
	}

	resp, body = e.do(t, http.MethodPost, "/v1/jobs/"+running.ID+"/cancel", "")
	if resp.StatusCode != 409 || errCode(t, body) != "JOB_FINISHED" {
		t.Fatalf("cancel finished: %d %s", resp.StatusCode, body)
	}
	resp, _ = e.do(t, http.MethodDelete, "/v1/jobs/"+running.ID, "")
	if resp.StatusCode != 204 {
		t.Fatalf("delete: %d", resp.StatusCode)
	}
	resp, _ = e.do(t, http.MethodGet, "/v1/jobs/"+running.ID, "")
	if resp.StatusCode != 404 {
		t.Fatalf("get deleted: %d", resp.StatusCode)
	}
}

func TestQueueFull(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	e := newEnv(t, setup{
		sched:      scheduler.Config{MaxConcurrent: 1, MaxQueue: 1},
		behaviours: map[string]runtimetest.Behavior{"/bin/block": {Release: release}},
	})
	first := e.create(t, "/bin/block")
	e.waitState(t, first.ID, func(s job.State) bool { return s == job.StateRunning })
	e.create(t, "/bin/block")
	resp, body := e.do(t, http.MethodPost, "/v1/jobs", `{"command":["/bin/block"]}`)
	if resp.StatusCode != 429 || errCode(t, body) != "QUEUE_FULL" || resp.Header.Get("Retry-After") != "1" {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
}

func TestShuttingDown(t *testing.T) {
	e := newEnv(t, setup{})
	ctx, cancel := context.WithTimeout(context.Background(), testDeadline)
	defer cancel()
	if err := e.ex.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
	resp, body := e.do(t, http.MethodPost, "/v1/jobs", `{"command":["/bin/true"]}`)
	if resp.StatusCode != 503 || errCode(t, body) != "SHUTTING_DOWN" {
		t.Fatalf("got %d %s", resp.StatusCode, body)
	}
	if resp, _ := e.do(t, http.MethodGet, "/health", ""); resp.StatusCode != 200 {
		t.Fatalf("health during shutdown: %d", resp.StatusCode)
	}
}

func TestInternalErrorsAreGeneric(t *testing.T) {
	e := newEnv(t, setup{behaviours: map[string]runtimetest.Behavior{
		"/bin/x": {StartErr: errors.New("mkdir /var/lib/sandboxd/secret: permission denied")},
	}})
	j := e.create(t, "/bin/x")
	e.waitState(t, j.ID, job.State.Terminal)
	_, body := e.do(t, http.MethodGet, "/v1/jobs/"+j.ID, "")
	if strings.Contains(string(body), "/var/lib") || strings.Contains(string(body), "permission") {
		t.Fatalf("leaked details: %s", body)
	}
	var got jobJSON
	mustUnmarshal(t, body, &got)
	if got.State != job.StateFailed || got.Reason != job.ReasonStartFailed || got.ExitCode != nil {
		t.Fatalf("got %s", body)
	}
}

type sseEvent struct {
	name string
	data map[string]any
}

func readEvents(t *testing.T, r io.Reader) []sseEvent {
	t.Helper()
	var events []sseEvent
	var cur sseEvent
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		switch {
		case strings.HasPrefix(line, "event: "):
			cur.name = strings.TrimPrefix(line, "event: ")
		case strings.HasPrefix(line, "data: "):
			mustUnmarshal(t, []byte(strings.TrimPrefix(line, "data: ")), &cur.data)
		case line == "":
			events = append(events, cur)
			cur = sseEvent{}
		}
	}
	if err := sc.Err(); err != nil {
		t.Fatal(err)
	}
	return events
}

func TestLogsFollow(t *testing.T) {
	release := make(chan struct{})
	e := newEnv(t, setup{behaviours: map[string]runtimetest.Behavior{
		"/bin/x": {Release: release, Stdout: []byte("hello\n"), Stderr: []byte{0xff, 0xfe}},
	}})
	j := e.create(t, "/bin/x")
	e.waitState(t, j.ID, func(s job.State) bool { return s == job.StateRunning })

	resp, err := http.Get(e.srv.URL + "/v1/jobs/" + j.ID + "/logs?follow=true")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if ct := resp.Header.Get("Content-Type"); ct != "text/event-stream" {
		t.Fatalf("content-type %q", ct)
	}
	done := make(chan []sseEvent)
	go func() { done <- readEvents(t, resp.Body) }()
	close(release)

	var events []sseEvent
	select {
	case events = <-done: // the stream ended on its own
	case <-time.After(testDeadline):
		t.Fatal("stream did not end")
	}
	var stdout, stderr bool
	for i, ev := range events {
		switch ev.name {
		case "stdout":
			stdout = ev.data["data"] == "hello\n"
		case "stderr":
			stderr = ev.data["data_base64"] == "//4="
		case "state":
			if i != len(events)-1 && ev.data["state"] == "succeeded" {
				t.Fatalf("terminal state not last: %+v", events)
			}
		}
	}
	last := events[len(events)-1]
	if !stdout || !stderr || last.name != "state" || last.data["state"] != "succeeded" || last.data["exit_code"] != 0.0 {
		t.Fatalf("events: %+v", events)
	}
	for i := 1; i < len(events); i++ {
		if events[i].name == "state" && events[i-1].name == "state" && events[i].data["state"] == events[i-1].data["state"] {
			t.Fatalf("duplicate state event: %+v", events)
		}
	}
}

func TestLogsHistoryAndDisconnect(t *testing.T) {
	e := newEnv(t, setup{behaviours: map[string]runtimetest.Behavior{
		"/bin/done": {Stdout: []byte("out")},
		"/bin/run":  {RunUntilSignal: true, Stdout: []byte("live")},
	}})
	finished := e.create(t, "/bin/done")
	e.waitState(t, finished.ID, job.State.Terminal)
	resp, body := e.do(t, http.MethodGet, "/v1/jobs/"+finished.ID+"/logs", "")
	events := readEvents(t, strings.NewReader(string(body)))
	if resp.StatusCode != 200 || len(events) != 2 || events[0].data["data"] != "out" || events[1].data["state"] != "succeeded" {
		t.Fatalf("history: %d %+v", resp.StatusCode, events)
	}

	// A client that disconnects from a follow stream must not affect the job.
	running := e.create(t, "/bin/run")
	e.waitState(t, running.ID, func(s job.State) bool { return s == job.StateRunning })
	ctx, cancel := context.WithCancel(context.Background())
	req, _ := http.NewRequestWithContext(ctx, http.MethodGet, e.srv.URL+"/v1/jobs/"+running.ID+"/logs?follow=1", nil)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	br := bufio.NewReader(resp.Body)
	if _, err := br.ReadString('\n'); err != nil {
		t.Fatal(err)
	}
	cancel()
	resp.Body.Close()
	if snap, _ := e.ex.Get(running.ID); snap.State != job.StateRunning {
		t.Fatalf("disconnect changed job state to %s", snap.State)
	}
	if resp, _ := e.do(t, http.MethodGet, "/v1/jobs/"+running.ID+"/logs?follow=maybe", ""); resp.StatusCode != 400 {
		t.Fatalf("bad follow: %d", resp.StatusCode)
	}
}

func TestArtifacts(t *testing.T) {
	e := newEnv(t, setup{})
	j := e.create(t, "/bin/x")
	e.waitState(t, j.ID, job.State.Terminal)
	ws := filepath.Join(e.stateDir, "jobs", j.ID, "workspace")
	art := filepath.Join(ws, "artifacts")
	if err := os.MkdirAll(filepath.Join(art, "sub"), 0o755); err != nil {
		t.Fatal(err)
	}
	os.WriteFile(filepath.Join(art, "sub", "result.txt"), []byte("42"), 0o644)
	os.WriteFile(filepath.Join(ws, "secret"), []byte("no"), 0o644)
	if err := os.Symlink("/etc/passwd", filepath.Join(art, "evil")); err != nil {
		t.Fatal(err)
	}

	resp, body := e.do(t, http.MethodGet, "/v1/jobs/"+j.ID+"/artifacts/sub/result.txt", "")
	if resp.StatusCode != 200 || string(body) != "42" || resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatalf("artifact: %d %q", resp.StatusCode, body)
	}
	for _, p := range []string{"evil", "sub", "missing", "sub/%2e%2e/%2e%2e/secret", "..%2fsecret", "%2e%2e/secret"} {
		resp, body := e.do(t, http.MethodGet, "/v1/jobs/"+j.ID+"/artifacts/"+p, "")
		if resp.StatusCode != 404 || strings.Contains(string(body), "root") || strings.Contains(string(body), e.stateDir) {
			t.Fatalf("%s: %d %s", p, resp.StatusCode, body)
		}
	}
}

func TestOperationalEndpoints(t *testing.T) {
	var readyErr error
	e := newEnv(t, setup{ready: func() error { return readyErr }})

	resp, body := e.do(t, http.MethodGet, "/health", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), `"ok"`) {
		t.Fatalf("health: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(t, http.MethodGet, "/ready", "")
	if resp.StatusCode != 200 {
		t.Fatalf("ready: %d %s", resp.StatusCode, body)
	}
	readyErr = errors.New("cgroup tree unavailable")
	resp, body = e.do(t, http.MethodGet, "/ready", "")
	if resp.StatusCode != 503 || errCode(t, body) != "NOT_READY" {
		t.Fatalf("not ready: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(t, http.MethodGet, "/metrics", "")
	if resp.StatusCode != 200 || !strings.Contains(string(body), "sandboxd_queue_capacity 16") {
		t.Fatalf("metrics: %d %s", resp.StatusCode, body)
	}
	resp, body = e.do(t, http.MethodGet, "/version", "")
	var v map[string]string
	mustUnmarshal(t, body, &v)
	if resp.StatusCode != 200 || v["version"] != "1.2.3" || v["commit"] != "abc" || v["go_version"] != "go1.26" {
		t.Fatalf("version: %d %s", resp.StatusCode, body)
	}
}

func TestPanicRecovery(t *testing.T) {
	s := &server{log: slog.New(slog.DiscardHandler)}
	h := s.middleware(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { panic("boom") }))
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if rec.Code != 500 || errCode(t, rec.Body.Bytes()) != "INTERNAL" {
		t.Fatalf("got %d %s", rec.Code, rec.Body)
	}
}
