// Package metrics owns the Prometheus registry of mailezine.
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/collectors"
)

// Metrics is the process-wide registry. Protocol packages register their own
// collectors through it at construction time.
type Metrics struct {
	Registry *prometheus.Registry

	HealthChecks      *prometheus.CounterVec
	DirectoryRequests *prometheus.CounterVec
	QueueMessages     *prometheus.CounterVec
}

// New returns a registry pre-populated with the core collectors.
func New() *Metrics {
	reg := prometheus.NewRegistry()
	m := &Metrics{
		Registry: reg,
		HealthChecks: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mailezine_health_checks_total",
			Help: "Health endpoint probes, by endpoint.",
		}, []string{"endpoint"}),
		DirectoryRequests: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mailezine_directory_requests_total",
			Help: "Directory contract lookups, by operation and HTTP status.",
		}, []string{"op", "status"}),
		QueueMessages: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mailezine_queue_messages_total",
			Help: "Outbound queue message events (submitted/delivered/bounced/deferred/failed).",
		}, []string{"event"}),
	}
	reg.MustRegister(m.HealthChecks, m.DirectoryRequests, m.QueueMessages)
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}
