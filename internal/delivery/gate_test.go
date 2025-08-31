package delivery

import (
	"context"
	"errors"
	"testing"
)

// recordingGate records the accounts the pipeline runs under and forwards
// to a canned result.
type recordingGate struct {
	accounts []string
	err      error
	inner    func(account string)
}

func (g *recordingGate) WithAccount(_ context.Context, account string, fn func() error) error {
	g.accounts = append(g.accounts, account)
	if g.inner != nil {
		g.inner(account)
	}
	if g.err != nil {
		return g.err
	}
	return fn()
}

// The pipeline runs each target's storage section under the account gate,
// once per target, and a gate error aborts the delivery.
func TestPipelineRunsUnderAccountGate(t *testing.T) {
	p, ms, _ := newTestPipeline(t, 1<<20)

	gate := &recordingGate{}
	p.Gate = gate
	if err := p.Deliver(context.Background(), nil, "sender@remote.test",
		[]string{"alice@example.com"}, []byte("From: sender@remote.test\r\nSubject: gated\r\n\r\ngated body\r\n")); err != nil {
		t.Fatal(err)
	}
	if len(gate.accounts) != 1 || gate.accounts[0] != "alice@example.com" {
		t.Fatalf("gate accounts = %v, want [alice@example.com]", gate.accounts)
	}
	if _, err := ms.EmailByUID(context.Background(), "alice@example.com", "INBOX", 1); err != nil {
		t.Fatalf("delivery did not land: %v", err)
	}

	// A gate failure surfaces as the delivery result (mail is not silently
	// stored outside the gate).
	boom := &recordingGate{err: errors.New("gate unavailable")}
	p.Gate = boom
	if err := p.Deliver(context.Background(), nil, "sender@remote.test",
		[]string{"alice@example.com"}, []byte("From: sender@remote.test\r\nSubject: two\r\n\r\nbody\r\n")); err == nil {
		t.Fatal("gate error must fail the delivery")
	}
	if _, err := ms.EmailByUID(context.Background(), "alice@example.com", "INBOX", 2); err == nil {
		t.Fatal("message stored despite gate failure")
	}
}

// Alias expansion: every local target is gated under its own account.
func TestPipelineGatesEveryTarget(t *testing.T) {
	p, _, _ := newTestPipeline(t, 1<<20)
	// team@example.com expands to alice + bob (newTestPipeline fixture).
	gate := &recordingGate{}
	p.Gate = gate
	if err := p.Deliver(context.Background(), nil, "sender@remote.test",
		[]string{"team@example.com"}, []byte("From: sender@remote.test\r\nSubject: alias\r\n\r\nbody\r\n")); err != nil {
		t.Fatal(err)
	}
	if len(gate.accounts) != 2 {
		t.Fatalf("gate accounts = %v, want both alias targets", gate.accounts)
	}
}
