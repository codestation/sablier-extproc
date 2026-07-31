package observability

import (
	"net/http"
	"time"

	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promhttp"
)

const namespace = "sablier_extproc"

const (
	maxConcurrentScrapes = 5
	metricsTimeout       = 5 * time.Second
)

// Metrics owns the service's Prometheus collectors.
type Metrics struct {
	registry       *prometheus.Registry
	decisions      *prometheus.CounterVec
	sablierCalls   *prometheus.CounterVec
	sablierLatency *prometheus.HistogramVec
	activeStreams  prometheus.Gauge
}

// NewMetrics creates and registers the service metrics.
func NewMetrics(version, revision string) *Metrics {
	metrics := &Metrics{
		registry: prometheus.NewRegistry(),
		decisions: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "decisions_total",
			Help:      "External processing decisions by configured group and result.",
		}, []string{"group", "result"}),
		sablierCalls: prometheus.NewCounterVec(prometheus.CounterOpts{
			Namespace: namespace,
			Name:      "sablier_requests_total",
			Help:      "Calls to Sablier by configured group and result.",
		}, []string{"group", "result"}),
		sablierLatency: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Namespace: namespace,
			Name:      "sablier_request_duration_seconds",
			Help:      "Latency of calls to Sablier.",
			Buckets:   prometheus.DefBuckets,
		}, []string{"group", "result"}),
		activeStreams: prometheus.NewGauge(prometheus.GaugeOpts{
			Namespace: namespace,
			Name:      "grpc_active_streams",
			Help:      "Number of active ext_proc gRPC streams.",
		}),
	}
	buildInfo := prometheus.NewGaugeVec(prometheus.GaugeOpts{
		Namespace: namespace,
		Name:      "build_info",
		Help:      "Build information for sablier-extproc.",
	}, []string{"version", "revision"})
	buildInfo.WithLabelValues(version, revision).Set(1)

	metrics.registry.MustRegister(
		metrics.decisions,
		metrics.sablierCalls,
		metrics.sablierLatency,
		metrics.activeStreams,
		buildInfo,
	)
	return metrics
}

// Handler returns an HTTP handler that exposes the metrics.
func (m *Metrics) Handler() http.Handler {
	return promhttp.HandlerFor(m.registry, promhttp.HandlerOpts{
		MaxRequestsInFlight: maxConcurrentScrapes,
		Timeout:             metricsTimeout,
	})
}

// RecordDecision records a proxy decision.
func (m *Metrics) RecordDecision(group, result string) {
	m.decisions.WithLabelValues(group, result).Inc()
}

// RecordSablierCall records a Sablier call and its latency.
func (m *Metrics) RecordSablierCall(group, result string, duration time.Duration) {
	m.sablierCalls.WithLabelValues(group, result).Inc()
	m.sablierLatency.WithLabelValues(group, result).Observe(duration.Seconds())
}

// StreamStarted increments the active gRPC stream count.
func (m *Metrics) StreamStarted() {
	m.activeStreams.Inc()
}

// StreamFinished decrements the active gRPC stream count.
func (m *Metrics) StreamFinished() {
	m.activeStreams.Dec()
}

// Gatherer returns the Prometheus registry used by the service.
func (m *Metrics) Gatherer() prometheus.Gatherer {
	return m.registry
}
