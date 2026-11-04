// Package metrics holds the Prometheus metrics exposed on /metrics.
package metrics

import (
	"strconv"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

const namespace = "hostinger_webhook"

// Metrics groups every collector. A nil *Metrics is valid and records nothing,
// which keeps tests and callers simple.
type Metrics struct {
	Registry *prometheus.Registry

	apiRequests      *prometheus.CounterVec
	apiDuration      *prometheus.HistogramVec
	apiRetries       *prometheus.CounterVec
	rateLimitRemain  prometheus.Gauge
	recordChanges    *prometheus.CounterVec
	managedRecords   prometheus.Gauge
	managedZones     prometheus.Gauge
	lastSyncSuccess  prometheus.Gauge
	authFailures     prometheus.Counter
	skippedEndpoints *prometheus.CounterVec
}

// New creates the collectors and registers them, plus the Go and process
// collectors, on a fresh registry.
func New(version string) *Metrics {
	m := &Metrics{
		Registry: prometheus.NewRegistry(),
		apiRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "api_requests_total",
			Help: "Requests sent to the Hostinger API, by operation and HTTP status code.",
		}, []string{"operation", "code"}),
		apiDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace, Name: "api_request_duration_seconds",
			Help:    "How long Hostinger API requests take, by operation.",
			Buckets: prometheus.DefBuckets,
		}, []string{"operation"}),
		apiRetries: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "api_retries_total",
			Help: "Hostinger API requests that were retried, by operation and reason.",
		}, []string{"operation", "reason"}),
		rateLimitRemain: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "api_ratelimit_remaining",
			Help: "Requests left in the current Hostinger rate limit window, as last reported by the API.",
		}),
		recordChanges: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "record_changes_total",
			Help: "DNS record changes applied, by action and result.",
		}, []string{"action", "result"}),
		managedRecords: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "records",
			Help: "Records returned to external-dns on the last successful read.",
		}),
		managedZones: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "zones",
			Help: "Zones that match the domain filter on the last successful read.",
		}),
		lastSyncSuccess: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace, Name: "last_success_timestamp_seconds",
			Help: "Unix time of the last successful read or write against Hostinger.",
		}),
		authFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Namespace: namespace, Name: "auth_failures_total",
			Help: "Times the Hostinger API refused the API token (HTTP 401).",
		}),
		skippedEndpoints: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace, Name: "skipped_endpoints_total",
			Help: "Desired endpoints or targets dropped because Hostinger cannot store them, by reason.",
		}, []string{"reason"}),
	}
	build := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace, Name: "build_info",
		Help: "Always 1. The version label tells which build is running.",
	}, []string{"version"})
	build.WithLabelValues(version).Set(1)

	m.Registry.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
		build, m.apiRequests, m.apiDuration, m.apiRetries, m.rateLimitRemain,
		m.recordChanges, m.managedRecords, m.managedZones, m.lastSyncSuccess,
		m.authFailures, m.skippedEndpoints,
	)
	return m
}

// ObserveRequest records one finished API call. code 0 means no response.
func (m *Metrics) ObserveRequest(operation string, code int, d time.Duration) {
	if m == nil {
		return
	}
	m.apiRequests.WithLabelValues(operation, strconv.Itoa(code)).Inc()
	m.apiDuration.WithLabelValues(operation).Observe(d.Seconds())
}

// ObserveRetry records that a call is being retried.
func (m *Metrics) ObserveRetry(operation, reason string) {
	if m == nil {
		return
	}
	m.apiRetries.WithLabelValues(operation, reason).Inc()
}

// SetRateLimitRemaining stores the last remaining-requests value of the RateLimit header.
func (m *Metrics) SetRateLimitRemaining(v float64) {
	if m == nil {
		return
	}
	m.rateLimitRemain.Set(v)
}

// ObserveChange records the result of one record change.
func (m *Metrics) ObserveChange(action string, err error) {
	if m == nil {
		return
	}
	result := "success"
	if err != nil {
		result = "error"
	}
	m.recordChanges.WithLabelValues(action, result).Inc()
}

// SetInventory stores how many zones and records were seen.
func (m *Metrics) SetInventory(zones, records int) {
	if m == nil {
		return
	}
	m.managedZones.Set(float64(zones))
	m.managedRecords.Set(float64(records))
}

// MarkSuccess stores the time of the last successful sync.
func (m *Metrics) MarkSuccess() {
	if m == nil {
		return
	}
	m.lastSyncSuccess.SetToCurrentTime()
}

// ObserveAuthFailure counts a request that Hostinger refused with HTTP 401.
func (m *Metrics) ObserveAuthFailure() {
	if m == nil {
		return
	}
	m.authFailures.Inc()
}

// ObserveSkipped counts an endpoint or target that was dropped.
func (m *Metrics) ObserveSkipped(reason string) {
	if m == nil {
		return
	}
	m.skippedEndpoints.WithLabelValues(reason).Inc()
}
