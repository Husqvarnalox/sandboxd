package main

import (
	"context"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	goruntime "runtime"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"

	"github.com/Husqvarnalox/sandboxd/internal/api"
	"github.com/Husqvarnalox/sandboxd/internal/config"
	"github.com/Husqvarnalox/sandboxd/internal/executor"
	"github.com/Husqvarnalox/sandboxd/internal/job"
	"github.com/Husqvarnalox/sandboxd/internal/metrics"
	"github.com/Husqvarnalox/sandboxd/internal/scheduler"
)

func newLogger(cfg *config.Config) *slog.Logger {
	level, _ := cfg.SlogLevel() // validated by config.Parse
	opts := &slog.HandlerOptions{Level: level}
	if cfg.LogFormat == "json" {
		return slog.New(slog.NewJSONHandler(os.Stderr, opts))
	}
	return slog.New(slog.NewTextHandler(os.Stderr, opts))
}

func serve(cfg *config.Config) int {
	log := newLogger(cfg)

	rt, err := newRuntime(cfg, log)
	if err != nil {
		log.Error("cannot initialise sandbox runtime", "err", err)
		return 1
	}
	if err := os.MkdirAll(cfg.StateDir, 0o700); err != nil {
		log.Error("create state dir", "err", err)
		return 1
	}

	reg := prometheus.NewRegistry()
	reg.MustRegister(collectors.NewGoCollector(), collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}))
	m := metrics.New(reg)
	sched := scheduler.New(scheduler.Config{MaxConcurrent: cfg.MaxConcurrentJobs, MaxQueue: cfg.MaxQueueSize})
	m.RegisterScheduler(reg, sched.Stats)

	ex := executor.New(executor.Config{
		Limits:          job.DefaultLimits(),
		MaxLogBytes:     cfg.MaxLogBytes,
		KillGracePeriod: cfg.KillGracePeriod,
		StateDir:        cfg.StateDir,
		Hostname:        cfg.Hostname,
	}, rt, sched, m, log)
	if err := ex.CleanupStale(); err != nil {
		log.Error("remove stale job directories", "err", err)
		return 1
	}
	sched.Start()

	handler := api.New(api.Config{
		Executor: ex,
		Ready: func() error {
			if !ex.Accepting() {
				return errors.New("shutting down")
			}
			// Check's error names host paths; only the fact is client-facing.
			if err := rt.Check(); err != nil {
				log.Warn("readiness check failed", "err", err)
				return errors.New("sandbox runtime unavailable")
			}
			return nil
		},
		Metrics:      metrics.Handler(reg),
		Version:      api.VersionInfo{Version: version, Commit: commit, GoVersion: goruntime.Version()},
		MaxBodyBytes: cfg.MaxRequestBytes,
		Logger:       log,
	})

	ln, err := net.Listen("tcp", cfg.Listen)
	if err != nil {
		log.Error("listen", "err", err)
		shutdownExecutor(ex, cfg.ShutdownTimeout, log)
		return 1
	}
	// No WriteTimeout: log streams are long-lived. The SSE handler sets a
	// deadline per event instead.
	srv := &http.Server{
		Handler:           handler,
		ReadHeaderTimeout: 10 * time.Second,
		IdleTimeout:       120 * time.Second,
		ErrorLog:          slog.NewLogLogger(log.Handler(), slog.LevelWarn),
	}
	serveErr := make(chan error, 1)
	go func() { serveErr <- srv.Serve(ln) }()
	log.Info("listening", "addr", ln.Addr().String(), "version", version,
		"max_concurrent_jobs", cfg.MaxConcurrentJobs, "max_queue_size", cfg.MaxQueueSize)

	sigs := make(chan os.Signal, 2)
	signal.Notify(sigs, syscall.SIGINT, syscall.SIGTERM)
	code := 0
	select {
	case sig := <-sigs:
		log.Info("shutting down", "signal", sig.String(), "timeout", cfg.ShutdownTimeout)
	case err := <-serveErr:
		log.Error("http server failed", "err", err)
		code = 1
	}
	go func() {
		sig := <-sigs
		log.Error("second signal, exiting immediately", "signal", sig.String())
		os.Exit(1)
	}()

	// The API keeps serving while jobs wind down: /ready reports 503, new
	// submissions get SHUTTING_DOWN, and log followers see the final states.
	if !shutdownExecutor(ex, cfg.ShutdownTimeout, log) {
		code = 1
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := srv.Shutdown(ctx); err != nil {
		log.Warn("http shutdown incomplete, closing connections", "err", err)
		if err := srv.Close(); err != nil {
			log.Error("close http server", "err", err)
		}
	}
	log.Info("stopped", "exit_code", code)
	return code
}

func shutdownExecutor(ex *executor.Executor, timeout time.Duration, log *slog.Logger) bool {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	if err := ex.Shutdown(ctx); err != nil {
		log.Error("jobs still running at shutdown deadline", "err", err)
		return false
	}
	return true
}
