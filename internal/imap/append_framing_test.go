// Framing regressions for rejected literals: pipelined payload after a
// refused literal must never be parsed as commands.
package imap

import (
	"fmt"
	"strings"
	"testing"
)

// Pre-auth APPEND gets tagged BAD/NO instead of a continuation.
func TestAppendPreAuthRefusedBeforeLiteral(t *testing.T) {
	r := startRawTestServer(t)

	r.tag++
	id := fmt.Sprintf("t%d", r.tag)
	if _, err := fmt.Fprintf(r.c, "%s APPEND INBOX {10}\r\n", id); err != nil {
		t.Fatal(err)
	}
	line := r.readLine()
	if strings.HasPrefix(line, "+") {
		t.Fatalf("pre-auth APPEND got a literal continuation: %q", line)
	}
	if !strings.HasPrefix(line, id+" BAD") && !strings.HasPrefix(line, id+" NO") {
		t.Fatalf("pre-auth APPEND: want tagged BAD/NO, got %q", line)
	}

	// No payload was sent, so the session must still be in sync.
	if resp := r.cmd("NOOP"); !strings.Contains(resp, " OK") {
		t.Fatalf("NOOP after refused APPEND: %q", resp)
	}
}

// An APPEND literal over the append limit: tagged NO, then teardown — the
// client may have pipelined payload bytes already.
func TestAppendOversizedLiteralTearsDown(t *testing.T) {
	r := dialRawTestServer(t) // MaxMessageBytes 1MiB is the append limit

	r.tag++
	id := fmt.Sprintf("t%d", r.tag)
	if _, err := fmt.Fprintf(r.c, "%s APPEND INBOX {%d}\r\n", id, 2<<20); err != nil {
		t.Fatal(err)
	}
	line := r.readLine()
	if strings.HasPrefix(line, "+") {
		t.Fatalf("oversized literal got a continuation: %q", line)
	}
	if !strings.HasPrefix(line, id+" NO") {
		t.Fatalf("oversized literal: want tagged NO, got %q", line)
	}
	if _, err := r.readLineErr(); err == nil {
		t.Fatal("connection stayed open after oversized-literal rejection")
	}
}

// A rejected LOGIN literal followed by a pipelined payload encoding a full
// second command: the payload goes away with the connection.
func TestRejectedLiteralPipelinedPayloadCannotSmuggle(t *testing.T) {
	r := startRawTestServer(t)

	smuggled := "t99 LOGIN alice@example.com s3cret\r\n"
	payload := strings.Repeat("A", 9999-len(smuggled)) + smuggled
	if len(payload) != 9999 {
		t.Fatalf("payload length %d", len(payload))
	}
	if _, err := fmt.Fprintf(r.c, "t1 LOGIN {9999+}\r\n%s", payload); err != nil {
		t.Fatal(err)
	}

	line := r.readLine()
	if strings.HasPrefix(line, "+") {
		t.Fatalf("rejected literal got a continuation: %q", line)
	}
	if !strings.HasPrefix(line, "t1 BAD") && !strings.HasPrefix(line, "t1 NO") {
		t.Fatalf("rejected literal: want tagged BAD/NO, got %q", line)
	}
	if _, err := r.readLineErr(); err == nil {
		t.Fatal("connection stayed open; pipelined payload could be parsed as commands")
	}
}
