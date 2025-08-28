// Package metrics owns the Prometheus registry of mailezine.
package metrics

import (
	"time"

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

	SMTPConnections       prometheus.Counter
	SMTPConnectionsActive prometheus.Gauge
	SMTPMessagesIn        *prometheus.CounterVec
	SMTPAuthFailures      prometheus.Counter
	IMAPSessionsActive    prometheus.Gauge
	POP3Sessions          prometheus.Counter
	QueueDepth            *prometheus.GaugeVec
	DeliveryDuration      *prometheus.HistogramVec
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
		SMTPConnections: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mailezine_smtp_connections_total",
			Help: "SMTP connections accepted (inbound and submission).",
		}),
		SMTPConnectionsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mailezine_smtp_connections_active",
			Help: "SMTP sessions currently open.",
		}),
		SMTPMessagesIn: prometheus.NewCounterVec(prometheus.CounterOpts{
			Name: "mailezine_smtp_messages_in_total",
			Help: "Inbound message outcomes, by outcome (accepted/rejected/deferred).",
		}, []string{"outcome"}),
		SMTPAuthFailures: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mailezine_smtp_auth_failures_total",
			Help: "Failed SMTP AUTH attempts.",
		}),
		IMAPSessionsActive: prometheus.NewGauge(prometheus.GaugeOpts{
			Name: "mailezine_imap_sessions_active",
			Help: "Authenticated IMAP sessions currently open.",
		}),
		POP3Sessions: prometheus.NewCounter(prometheus.CounterOpts{
			Name: "mailezine_pop3_sessions_total",
			Help: "POP3 sessions served.",
		}),
		QueueDepth: prometheus.NewGaugeVec(prometheus.GaugeOpts{
			Name: "mailezine_queue_depth",
			Help: "Outbound queue water level, by message state.",
		}, []string{"state"}),
		DeliveryDuration: prometheus.NewHistogramVec(prometheus.HistogramOpts{
			Name:    "mailezine_delivery_duration_seconds",
			Help:    "Outbound delivery attempt duration, by transport result (ok/error).",
			Buckets: prometheus.DefBuckets,
		}, []string{"result"}),
	}
	reg.MustRegister(m.HealthChecks, m.DirectoryRequests, m.QueueMessages,
		m.SMTPConnections, m.SMTPConnectionsActive, m.SMTPMessagesIn,
		m.SMTPAuthFailures, m.IMAPSessionsActive, m.POP3Sessions,
		m.QueueDepth, m.DeliveryDuration)
	reg.MustRegister(
		collectors.NewGoCollector(),
		collectors.NewProcessCollector(collectors.ProcessCollectorOpts{}),
	)
	return m
}

// The helpers below are nil-safe: protocol servers keep an optional *Metrics
// and instrumentation must never change behavior (a nil receiver no-ops).

// SMTPSessionOpened records a new SMTP connection and opens the active gauge.
func (m *Metrics) SMTPSessionOpened() {
	if m == nil {
		return
	}
	m.SMTPConnections.Inc()
	m.SMTPConnectionsActive.Inc()
}

// SMTPSessionClosed closes the active-connections gauge at session end.
func (m *Metrics) SMTPSessionClosed() {
	if m == nil {
		return
	}
	m.SMTPConnectionsActive.Dec()
}

// SMTPMessageIn records one inbound message outcome (accepted/rejected/deferred).
func (m *Metrics) SMTPMessageIn(outcome string) {
	if m == nil {
		return
	}
	m.SMTPMessagesIn.WithLabelValues(outcome).Inc()
}

// SMTPAuthFailed records a rejected SMTP authentication attempt.
func (m *Metrics) SMTPAuthFailed() {
	if m == nil {
		return
	}
	m.SMTPAuthFailures.Inc()
}

// IMAPSessionOpened opens the active-sessions gauge once a session
// authenticated.
func (m *Metrics) IMAPSessionOpened() {
	if m == nil {
		return
	}
	m.IMAPSessionsActive.Inc()
}

// IMAPSessionClosed closes the active-sessions gauge at session end.
func (m *Metrics) IMAPSessionClosed() {
	if m == nil {
		return
	}
	m.IMAPSessionsActive.Dec()
}

// POP3SessionServed records one POP3 session.
func (m *Metrics) POP3SessionServed() {
	if m == nil {
		return
	}
	m.POP3Sessions.Inc()
}

// ObserveDelivery records one outbound delivery attempt's duration, labeled
// by transport result.
func (m *Metrics) ObserveDelivery(d time.Duration, ok bool) {
	if m == nil {
		return
	}
	result := "ok"
	if !ok {
		result = "error"
	}
	m.DeliveryDuration.WithLabelValues(result).Observe(d.Seconds())
}
