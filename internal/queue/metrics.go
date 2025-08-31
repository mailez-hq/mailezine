// Prometheus instrumentation for the outbound queue: queue depth (recounted
// from the spooled metadata on every scrape) and delivery attempt duration.
package queue

import (
	"context"
	"encoding/json"
	"time"

	"github.com/prometheus/client_golang/prometheus"

	"mailezine/internal/metrics"
	"mailezine/internal/store"
)

// depthDesc is shared by every depthCollector instance; the HA term switch
// relies on the shared fingerprint to unregister the previous term's
// collector (see SetMetrics).
var depthDesc = prometheus.NewDesc(
	"mailezine_queue_depth",
	"Outbound queue water level, by message state.",
	[]string{"state"}, nil,
)

// SetMetrics wires Prometheus instrumentation. Delivery attempts observe
// mailezine_delivery_duration_seconds; mailezine_queue_depth{state} is a
// scrape-time recount of the spooled metadata, so it stays accurate across
// restarts and HA term switches without state-transition bookkeeping. ctx
// scopes the depth collector: once cancelled it reports nothing (the KV
// store may be closing underneath a finished term). Call before Run; a nil
// mtr disables instrumentation.
func (m *Manager) SetMetrics(mtr *metrics.Metrics, ctx context.Context) {
	if mtr == nil {
		return
	}
	m.mtr = mtr
	c := &depthCollector{mgr: m, ctx: ctx}
	// HA re-wires the queue per term into the process-wide registry: drop
	// the previous term's collector first (same metric fingerprint).
	mtr.Registry.Unregister(c)
	mtr.Registry.MustRegister(c)
}

// observeDelivery records one deliverer round-trip. m.mtr is nil-safe.
func (m *Manager) observeDelivery(d time.Duration, ok bool) {
	m.mtr.ObserveDelivery(d, ok)
}

// observeClaim records one claim outcome (claimed/stolen/lost) — the
// multi-active health signal. m.mtr is nil-safe.
func (m *Manager) observeClaim(outcome string) {
	m.mtr.QueueClaimEvent(outcome)
}

// depthCollector recounts the queue states at scrape time.
type depthCollector struct {
	mgr *Manager
	ctx context.Context
}

func (c *depthCollector) Describe(ch chan<- *prometheus.Desc) {
	ch <- depthDesc
}

func (c *depthCollector) Collect(ch chan<- prometheus.Metric) {
	if c.ctx.Err() != nil {
		return
	}
	counts := map[State]int{}
	err := c.mgr.kv.Scan(store.QueueMetaPrefix(queueName), func(_, v []byte) error {
		var msg Message
		if err := json.Unmarshal(v, &msg); err != nil {
			return nil // skip corrupt rows rather than fail the scrape
		}
		counts[msg.State]++
		return nil
	})
	if err != nil {
		return
	}
	for state, n := range counts {
		ch <- prometheus.MustNewConstMetric(depthDesc, prometheus.GaugeValue, float64(n), string(state))
	}
}
