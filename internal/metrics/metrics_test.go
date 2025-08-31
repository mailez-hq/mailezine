package metrics

import (
	"strings"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
)

func TestQueueMetricsRegistered(t *testing.T) {
	m := New()
	m.QueueMessages.WithLabelValues("submitted").Inc()
	m.QueueMessages.WithLabelValues("delivered").Inc()
	if got := testutil.CollectAndCount(m.QueueMessages); got != 2 {
		t.Fatalf("queue metric families = %d, want 2", got)
	}
	out := testutil.CollectAndCount(m.QueueMessages)
	_ = out
	// The registry serves the metric under the expected name.
	got, err := m.Registry.Gather()
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, mf := range got {
		if strings.Contains(mf.GetName(), "mailezine_queue_messages_total") {
			found = true
		}
	}
	if !found {
		t.Fatal("queue metric not in registry")
	}
	// Runtime/process collectors are registered too.
	runtimeFound := false
	for _, mf := range got {
		if strings.Contains(mf.GetName(), "go_goroutines") ||
			strings.Contains(mf.GetName(), "process_cpu_seconds_total") {
			runtimeFound = true
		}
	}
	if !runtimeFound {
		t.Fatal("runtime/process metrics not in registry")
	}
}

// TestNilReceiverSafety guards the instrumentation contract: call sites rely
// on nil-safe helpers so an unwired *Metrics never changes behavior.
func TestNilReceiverSafety(t *testing.T) {
	var m *Metrics
	m.SMTPSessionOpened()
	m.SMTPSessionClosed()
	m.SMTPMessageIn("accepted")
	m.SMTPAuthFailed()
	m.IMAPSessionOpened()
	m.IMAPSessionClosed()
	m.POP3SessionServed()
	m.ObserveDelivery(time.Second, true)
	m.QueueClaimEvent("claimed")
}

func TestQueueClaimMetrics(t *testing.T) {
	m := New()
	m.QueueClaimEvent("claimed")
	m.QueueClaimEvent("stolen")
	m.QueueClaimEvent("stolen")
	m.QueueClaimEvent("lost")
	if got := testutil.ToFloat64(m.QueueClaims.WithLabelValues("claimed")); got != 1 {
		t.Fatalf("claimed = %v, want 1", got)
	}
	if got := testutil.ToFloat64(m.QueueClaims.WithLabelValues("stolen")); got != 2 {
		t.Fatalf("stolen = %v, want 2", got)
	}
	if got := testutil.ToFloat64(m.QueueClaims.WithLabelValues("lost")); got != 1 {
		t.Fatalf("lost = %v, want 1", got)
	}
}
