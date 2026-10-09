// Package api serves the sandboxd HTTP API.
package api

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"path"
	"strings"
	"time"

	"github.com/Husqvarnalox/sandboxd/internal/executor"
	"github.com/Husqvarnalox/sandboxd/internal/job"
	"github.com/Husqvarnalox/sandboxd/internal/runtime"
	"github.com/Husqvarnalox/sandboxd/internal/scheduler"
)

// VersionInfo is reported by GET /version.
type VersionInfo struct {
	Version, Commit, GoVersion string
}

// Config wires the API to the executor.
type Config struct {
	Executor *executor.Executor
	// Ready reports readiness; nil means always ready. Its error message is
	// shown in the /ready response body.
	Ready   func() error
	Metrics http.Handler
	Version VersionInfo
	// MaxBodyBytes bounds request bodies; default 1 MiB.
	MaxBodyBytes int64
	Logger       *slog.Logger
}

type server struct {
	cfg Config
	ex  *executor.Executor
	log *slog.Logger
	mux *http.ServeMux
}

// New returns the API handler.
func New(cfg Config) http.Handler {
	if cfg.MaxBodyBytes <= 0 {
		cfg.MaxBodyBytes = 1 << 20
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.New(slog.DiscardHandler)
	}
	s := &server{cfg: cfg, ex: cfg.Executor, log: cfg.Logger, mux: http.NewServeMux()}
	s.mux.HandleFunc("GET /health", s.health)
	s.mux.HandleFunc("GET /ready", s.ready)
	s.mux.HandleFunc("GET /version", s.version)
	if cfg.Metrics != nil {
		s.mux.Handle("GET /metrics", cfg.Metrics)
	}
	s.mux.HandleFunc("POST /v1/jobs", s.createJob)
	s.mux.HandleFunc("GET /v1/jobs", s.listJobs)
	s.mux.HandleFunc("GET /v1/jobs/{id}", s.getJob)
	s.mux.HandleFunc("POST /v1/jobs/{id}/cancel", s.cancelJob)
	s.mux.HandleFunc("DELETE /v1/jobs/{id}", s.deleteJob)
	s.mux.HandleFunc("GET /v1/jobs/{id}/logs", s.streamLogs)
	s.mux.HandleFunc("GET /v1/jobs/{id}/artifacts/{path...}", s.artifact)
	return s.middleware(http.HandlerFunc(s.route))
}

// route dispatches through the mux but replaces its plain-text 404/405
// responses with the JSON error shape.
func (s *server) route(w http.ResponseWriter, r *http.Request) {
	h, pattern := s.mux.Handler(r)
	if pattern != "" {
		s.mux.ServeHTTP(w, r)
		return
	}
	rec := &bufferedWriter{header: make(http.Header)}
	h.ServeHTTP(rec, r)
	switch rec.status {
	case http.StatusNotFound:
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "no such endpoint")
	case http.StatusMethodNotAllowed:
		w.Header().Set("Allow", rec.header.Get("Allow"))
		writeErr(w, http.StatusMethodNotAllowed, "METHOD_NOT_ALLOWED", "method not allowed")
	default: // e.g. the mux's redirect to a cleaned path
		for k, v := range rec.header {
			w.Header()[k] = v
		}
		w.WriteHeader(rec.status)
		w.Write(rec.body.Bytes())
	}
}

type bufferedWriter struct {
	header http.Header
	status int
	body   bytes.Buffer
}

func (b *bufferedWriter) Header() http.Header { return b.header }
func (b *bufferedWriter) Write(p []byte) (int, error) {
	if b.status == 0 {
		b.status = http.StatusOK
	}
	return b.body.Write(p)
}
func (b *bufferedWriter) WriteHeader(code int) {
	if b.status == 0 {
		b.status = code
	}
}

func (s *server) health(w http.ResponseWriter, _ *http.Request) {
	writeJSON(w, http.StatusOK, map[string]string{"status": "ok"})
}

func (s *server) ready(w http.ResponseWriter, _ *http.Request) {
	if s.cfg.Ready != nil {
		if err := s.cfg.Ready(); err != nil {
			writeErr(w, http.StatusServiceUnavailable, "NOT_READY", err.Error())
			return
		}
	}
	writeJSON(w, http.StatusOK, map[string]string{"status": "ready"})
}

func (s *server) version(w http.ResponseWriter, _ *http.Request) {
	v := s.cfg.Version
	writeJSON(w, http.StatusOK, map[string]string{"version": v.Version, "commit": v.Commit, "go_version": v.GoVersion})
}

func (s *server) createJob(w http.ResponseWriter, r *http.Request) {
	var req jobRequest
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, s.cfg.MaxBodyBytes))
	dec.DisallowUnknownFields()
	err := dec.Decode(&req)
	if err == nil {
		if dec.Decode(&struct{}{}) != io.EOF {
			err = errors.New("unexpected data after JSON object")
		}
	}
	if err != nil {
		if mbe := (*http.MaxBytesError)(nil); errors.As(err, &mbe) {
			writeErr(w, http.StatusRequestEntityTooLarge, "REQUEST_TOO_LARGE",
				fmt.Sprintf("request body exceeds %d bytes", s.cfg.MaxBodyBytes))
			return
		}
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", "invalid JSON body: "+jsonErrMessage(err))
		return
	}
	spec, err := req.spec()
	if err != nil {
		s.fail(w, r, err)
		return
	}
	snap, err := s.ex.Submit(spec)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	w.Header().Set("Location", "/v1/jobs/"+snap.ID)
	writeJSON(w, http.StatusCreated, toJSON(snap, time.Now()))
}

// jsonErrMessage keeps decoder messages but strips the "json: " prefix.
func jsonErrMessage(err error) string {
	if errors.Is(err, io.EOF) {
		return "empty body"
	}
	if errors.Is(err, io.ErrUnexpectedEOF) {
		return "truncated body"
	}
	return strings.TrimPrefix(err.Error(), "json: ")
}

func (s *server) listJobs(w http.ResponseWriter, _ *http.Request) {
	now := time.Now()
	snaps := s.ex.List()
	jobs := make([]jobJSON, len(snaps))
	for i, snap := range snaps {
		jobs[i] = toJSON(snap, now)
	}
	writeJSON(w, http.StatusOK, map[string][]jobJSON{"jobs": jobs})
}

func (s *server) getJob(w http.ResponseWriter, r *http.Request) {
	snap, err := s.ex.Get(r.PathValue("id"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, toJSON(snap, time.Now()))
}

func (s *server) cancelJob(w http.ResponseWriter, r *http.Request) {
	id := r.PathValue("id")
	if err := s.ex.Cancel(id); err != nil {
		s.fail(w, r, err)
		return
	}
	snap, err := s.ex.Get(id)
	if err != nil {
		s.fail(w, r, err)
		return
	}
	writeJSON(w, http.StatusAccepted, toJSON(snap, time.Now()))
}

func (s *server) deleteJob(w http.ResponseWriter, r *http.Request) {
	if err := s.ex.Delete(r.PathValue("id")); err != nil {
		s.fail(w, r, err)
		return
	}
	w.WriteHeader(http.StatusNoContent)
}

func (s *server) artifact(w http.ResponseWriter, r *http.Request) {
	f, err := s.ex.OpenArtifact(r.PathValue("id"), r.PathValue("path"))
	if err != nil {
		s.fail(w, r, err)
		return
	}
	defer f.Close()
	fi, err := f.Stat()
	if err != nil {
		s.fail(w, r, fmt.Errorf("stat artifact: %w", err))
		return
	}
	// Artifacts are workload-controlled bytes: never let a browser sniff
	// them into something executable on the API origin.
	w.Header().Set("Content-Type", artifactType(fi.Name()))
	w.Header().Set("X-Content-Type-Options", "nosniff")
	http.ServeContent(w, r, "", fi.ModTime(), f)
}

func artifactType(name string) string {
	switch strings.ToLower(path.Ext(name)) {
	case ".txt", ".log", ".csv", ".md":
		return "text/plain; charset=utf-8"
	case ".json":
		return "application/json"
	}
	return "application/octet-stream"
}

// fail maps an error to a client-safe response. Unknown errors are logged in
// full and reported generically so host paths and syscall details stay
// server-side.
func (s *server) fail(w http.ResponseWriter, r *http.Request, err error) {
	var ve *job.ValidationError
	switch {
	case errors.As(err, &ve):
		writeErr(w, http.StatusBadRequest, "INVALID_REQUEST", ve.Error())
	case errors.Is(err, executor.ErrNotFound), errors.Is(err, fs.ErrNotExist):
		writeErr(w, http.StatusNotFound, "NOT_FOUND", "not found")
	case errors.Is(err, executor.ErrFinished):
		writeErr(w, http.StatusConflict, "JOB_FINISHED", "job already finished")
	case errors.Is(err, executor.ErrNotFinished):
		writeErr(w, http.StatusConflict, "JOB_NOT_FINISHED", "job has not finished")
	case errors.Is(err, scheduler.ErrQueueFull):
		w.Header().Set("Retry-After", "1")
		writeErr(w, http.StatusTooManyRequests, "QUEUE_FULL", "job queue is full, retry later")
	case errors.Is(err, executor.ErrStopped):
		writeErr(w, http.StatusServiceUnavailable, "SHUTTING_DOWN", "server is shutting down")
	case errors.Is(err, runtime.ErrUnavailable):
		s.log.Error("runtime unavailable", "method", r.Method, "path", r.URL.Path, "err", err)
		writeErr(w, http.StatusServiceUnavailable, "RUNTIME_UNAVAILABLE", "sandbox runtime unavailable")
	default:
		s.log.Error("request failed", "method", r.Method, "path", r.URL.Path, "err", err)
		writeErr(w, http.StatusInternalServerError, "INTERNAL", "internal error")
	}
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func writeErr(w http.ResponseWriter, status int, code, msg string) {
	writeJSON(w, status, errorBody{Error: errorDetail{Code: code, Message: msg}})
}

type statusWriter struct {
	http.ResponseWriter
	status int
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status == 0 {
		w.status = code
	}
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

// Unwrap lets http.ResponseController reach Flush and SetWriteDeadline.
func (w *statusWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (s *server) middleware(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		start := time.Now()
		sw := &statusWriter{ResponseWriter: w}
		defer func() {
			if v := recover(); v != nil {
				if v == http.ErrAbortHandler {
					panic(v)
				}
				s.log.Error("panic in handler", "method", r.Method, "path", r.URL.Path, "panic", v)
				if sw.status == 0 {
					writeErr(sw, http.StatusInternalServerError, "INTERNAL", "internal error")
				}
			}
			level := slog.LevelDebug
			if sw.status >= 500 {
				level = slog.LevelWarn
			}
			s.log.Log(r.Context(), level, "request", "method", r.Method, "path", r.URL.Path,
				"status", sw.status, "duration_ms", time.Since(start).Milliseconds())
		}()
		next.ServeHTTP(sw, r)
	})
}
