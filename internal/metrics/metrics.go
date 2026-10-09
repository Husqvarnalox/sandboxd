// Package metrics defines the daemon's Prometheus instruments. Metrics are
// observational only and carry no per-job labels.
package metrics

import (
	"net/http"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/Husqvarnalox/sandboxd/internal/scheduler"
)

// Metrics holds every instrument the executor and API update.
type Metrics struct {
	JobsTotal       *prometheus.CounterVec
	JobDuration     prometheus.Histogram
	QueueWait       prometheus.Histogram
	StartLatency    prometheus.Histogram
	JobCPU          prometheus.Histogram
	JobMemoryPeak   prometheus.Histogram
	OOMTotal        prometheus.Counter
	TimeoutTotal    prometheus.Counter
	CancelTotal     *prometheus.CounterVec
	StartFailures   prometheus.Counter
	CleanupFailures prometheus.Counter
	QueueRejected   prometheus.Counter
}

// New creates the instruments and registers them on reg.
func New(reg prometheus.Registerer) *Metrics {
	durationBuckets := []float64{.01, .05, .1, .25, .5, 1, 2.5, 5, 10, 30, 60, 300, 900, 3600}
	m := &Metrics{
		JobsTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sandboxd_jobs_total",
			Help: "Jobs that reached a terminal state, by final status.",
		}, []string{"status"}),
		JobDuration: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "sandboxd_job_duration_seconds",
			Help:    "Wall time from job start to finish.",
			Buckets: durationBuckets,
		}),
		QueueWait: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "sandboxd_job_queue_wait_seconds",
			Help:    "Time a job spent queued before a worker picked it up.",
			Buckets: durationBuckets,
		}),
		StartLatency: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "sandboxd_job_start_latency_seconds",
			Help:    "Duration of the runtime Start call (sandbox setup until exec).",
			Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
		}),
		JobCPU: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "sandboxd_job_cpu_seconds",
			Help:    "CPU time consumed by finished jobs.",
			Buckets: durationBuckets,
		}),
		JobMemoryPeak: prometheus.NewHistogram(prometheus.HistogramOpts{
			Name:    "sandboxd_job_memory_peak_bytes",
			Help:    "Peak memory of finished jobs, when the kernel reports it.",
			Buckets: prometheus.ExponentialBuckets(1<<20, 2, 13), // 1 MiB .. 4 GiB
		}),
		OOMTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sandboxd_job_oom_total",
			Help: "Jobs in which the kernel OOM-killed a process.",
		}),
		TimeoutTotal: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sandboxd_job_timeout_total",
			Help: "Jobs terminated because they exceeded their timeout.",
		}),
		CancelTotal: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "sandboxd_job_cancel_total",
			Help: "Cancelled jobs, by cancellation reason.",
		}, []string{"reason"}),
		StartFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sandboxd_job_start_failures_total",
			Help: "Jobs whose sandbox failed to start.",
		}),
		CleanupFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sandboxd_cleanup_failures_total",
			Help: "Failures releasing sandbox resources after a job.",
		}),
		QueueRejected: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "sandboxd_queue_rejected_total",
			Help: "Submissions rejected because the queue was full.",
		}),
	}
	reg.MustRegister(m.JobsTotal, m.JobDuration, m.QueueWait, m.StartLatency, m.JobCPU,
		m.JobMemoryPeak, m.OOMTotal, m.TimeoutTotal, m.CancelTotal, m.StartFailures,
		m.CleanupFailures, m.QueueRejected)
	return m
}

// RegisterScheduler exports the scheduler's occupancy as gauges read on scrape.
func (m *Metrics) RegisterScheduler(reg prometheus.Registerer, stats func() scheduler.Stats) {
	gauge := func(name, help string, f func(scheduler.Stats) int) prometheus.Collector {
		return prometheus.NewGaugeFunc(prometheus.GaugeOpts{Name: name, Help: help},
			func() float64 { return float64(f(stats())) })
	}
	reg.MustRegister(
		gauge("sandboxd_jobs_active", "Jobs currently running.", func(s scheduler.Stats) int { return s.Running }),
		gauge("sandboxd_jobs_queued", "Jobs waiting for a worker.", func(s scheduler.Stats) int { return s.Queued }),
		gauge("sandboxd_scheduler_capacity", "Maximum concurrently running jobs.", func(s scheduler.Stats) int { return s.MaxConcurrent }),
		gauge("sandboxd_queue_capacity", "Maximum queued jobs.", func(s scheduler.Stats) int { return s.MaxQueue }),
	)
}

// Handler serves the metrics gathered from g in the Prometheus text format.
func Handler(g prometheus.Gatherer) http.Handler {
	return promhttp.HandlerFor(g, promhttp.HandlerOpts{})
}
