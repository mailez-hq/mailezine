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
}
