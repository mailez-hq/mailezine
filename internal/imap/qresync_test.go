// QRESYNC (RFC 7162) end-to-end: capability advertisement, ENABLE,
// tombstoned expunges surfacing as VANISHED, UID FETCH CHANGEDSINCE, and
// SELECT (QRESYNC ...) resynchronization. The go-imap beta.8 client cannot
// express SELECT (QRESYNC ...) or parse VANISHED, so those parts drive the
// server over the raw-wire harness (wire_test.go).
package imap

import (
	"fmt"
	"strconv"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
)

// TestIMAPQResyncCapability: QRESYNC is advertised. (The beta.8 client
// cannot ENABLE it — ENABLE itself is covered by the raw-wire test.)
func TestIMAPQResyncCapability(t *testing.T) {
	c, _ := startTestServer(t)
	if !c.Caps().Has(imap.CapCondStore) || !c.Caps().Has(imap.CapQResync) {
		t.Fatal("CONDSTORE/QRESYNC capabilities not advertised")
	}
}

// TestIMAPQResyncFetchChangedSince: UID FETCH ... (CHANGEDSINCE n) returns
// only messages with modseq strictly after n, with MODSEQ forced on.
// (CHANGEDSINCE is a CONDSTORE feature and needs no ENABLE.)
func TestIMAPQResyncFetchChangedSince(t *testing.T) {
	c, _ := startTestServer(t)
	u1 := appendMessage(t, c, "INBOX", "From: a@x.test\r\nSubject: one\r\n\r\n1\r\n", nil)
	u2 := appendMessage(t, c, "INBOX", "From: b@x.test\r\nSubject: two\r\n\r\n2\r\n", nil)
	u3 := appendMessage(t, c, "INBOX", "From: c@x.test\r\nSubject: three\r\n\r\n3\r\n", nil)

	c.Select("INBOX", nil).Wait()
	got, err := c.Fetch(imap.UIDSetNum(u1, u2, u3), &imap.FetchOptions{UID: true, ChangedSince: 2}).Collect()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[imap.UID]bool{}
	for _, m := range got {
		seen[m.UID] = true
		if m.ModSeq == 0 {
			t.Fatalf("UID %d: CHANGEDSINCE response missing MODSEQ", m.UID)
		}
	}
	if seen[u1] || seen[u2] {
		t.Fatalf("CHANGEDSINCE=2 returned stale messages: %v", seen)
	}
	if !seen[u3] {
		t.Fatalf("CHANGEDSINCE=2 did not return uid %d: %v", u3, seen)
	}
}

// TestIMAPQResyncWire exercises the wire parts the typed client cannot
// express: VANISHED instead of EXPUNGE on QRESYNC connections, the
// SELECT (QRESYNC ...) resynchronization payload, and the BAD when the
// parameter is used without ENABLE QRESYNC.
func TestIMAPQResyncWire(t *testing.T) {
	r := dialRawTestServer(t)
	if resp := r.cmd("ENABLE QRESYNC"); !okStatus(resp) {
		t.Fatalf("ENABLE QRESYNC failed: %s", resp)
	}

	// Seed three messages (modseqs 1..3), then select with CONDSTORE to
	// learn UIDVALIDITY and the current HIGHESTMODSEQ (3).
	r.appendRaw("From: a@x.test\r\nSubject: one\r\n\r\n1\r\n")
	r.appendRaw("From: b@x.test\r\nSubject: two\r\n\r\n2\r\n")
	r.appendRaw("From: c@x.test\r\nSubject: three\r\n\r\n3\r\n")
	resp, untagged := r.cmdCollect("SELECT INBOX (CONDSTORE)")
	if !okStatus(resp) {
		t.Fatalf("select failed: %s", resp)
	}
	uidvalidity := extractCode(t, untagged, "UIDVALIDITY")
	highest := extractCode(t, untagged, "HIGHESTMODSEQ")

	// Append a fourth message and expunge it: a QRESYNC connection gets a
	// VANISHED line, never a per-message EXPUNGE. The tombstone lands at a
	// modseq strictly above `highest`.
	appendResp := r.appendRaw("From: d@x.test\r\nSubject: four\r\n\r\n4\r\n")
	uid4 := extractUIDFromAppend(t, appendResp)
	if resp := r.cmd("UID STORE %d +FLAGS.SILENT (\\Deleted)", uid4); !okStatus(resp) {
		t.Fatalf("store failed: %s", resp)
	}
	resp, untagged = r.cmdCollect("EXPUNGE")
	if !okStatus(resp) {
		t.Fatalf("expunge failed: %s", resp)
	}
	wantVanished := "* VANISHED " + strconv.FormatUint(uid4, 10)
	if !hasLinePrefix(untagged, wantVanished) {
		t.Fatalf("no %q in expunge responses: %v", wantVanished, untagged)
	}
	for _, l := range untagged {
		if strings.HasSuffix(l, "EXPUNGE") {
			t.Fatalf("QRESYNC connection got a per-message EXPUNGE: %q", l)
		}
	}

	// Resynchronize from the pre-expunge modseq: VANISHED (EARLIER) for the
	// tombstoned UID and no flag updates (nothing changed after `highest`).
	resp, untagged = r.cmdCollect("SELECT INBOX (QRESYNC (%d %d))", uidvalidity, highest)
	if !okStatus(resp) {
		t.Fatalf("QRESYNC select failed: %s", resp)
	}
	if !hasLinePrefix(untagged, "* VANISHED (EARLIER) "+strconv.FormatUint(uid4, 10)) {
		t.Fatalf("no VANISHED (EARLIER) in resync: %v", untagged)
	}
	if hasFlagFetch(untagged) {
		t.Fatalf("unexpected flag updates in resync: %v", untagged)
	}

	// Resynchronize from modseq 1: the two surviving messages with
	// modseqs 2..3 come back as UID+MODSEQ+FLAGS fetches.
	resp, untagged = r.cmdCollect("SELECT INBOX (QRESYNC (%d 1))", uidvalidity)
	if !okStatus(resp) {
		t.Fatalf("QRESYNC select failed: %s", resp)
	}
	if !hasFlagFetch(untagged) {
		t.Fatalf("no flag-update FETCH lines in resync from 1: %v", untagged)
	}

	// QRESYNC parameter without ENABLE QRESYNC must be rejected.
	r2 := dialRawTestServer(t)
	resp = r2.cmd("SELECT INBOX (QRESYNC (%d %d))", uidvalidity, highest)
	if !strings.Contains(resp, " BAD") {
		t.Fatalf("QRESYNC without ENABLE not rejected: %s", resp)
	}
}

func hasLinePrefix(lines []string, prefix string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, prefix) {
			return true
		}
	}
	return false
}

func hasFlagFetch(lines []string) bool {
	for _, l := range lines {
		if strings.HasPrefix(l, "* ") && strings.Contains(l, "FETCH") &&
			strings.Contains(l, "MODSEQ") && strings.Contains(l, "UID") {
			return true
		}
	}
	return false
}

func extractCode(t *testing.T, lines []string, code string) uint64 {
	t.Helper()
	needle := "[" + code + " "
	for _, l := range lines {
		if i := strings.Index(l, needle); i >= 0 {
			rest := l[i+len(needle):]
			var v uint64
			if _, err := fmt.Sscanf(rest, "%d", &v); err != nil {
				t.Fatalf("extract %s from %q: %v", code, l, err)
			}
			return v
		}
	}
	t.Fatalf("no [%s] code in responses: %v", code, lines)
	return 0
}

// extractUIDFromAppend parses the UID out of "tN OK [APPENDUID <uv> <uid>]".
func extractUIDFromAppend(t *testing.T, line string) uint64 {
	t.Helper()
	const needle = "[APPENDUID "
	i := strings.Index(line, needle)
	if i < 0 {
		t.Fatalf("no APPENDUID in %q", line)
	}
	fields := strings.Fields(line[i+len(needle):])
	if len(fields) < 2 {
		t.Fatalf("bad APPENDUID in %q", line)
	}
	var v uint64
	if _, err := fmt.Sscanf(fields[1], "%d", &v); err != nil {
		t.Fatalf("bad APPENDUID uid in %q: %v", line, err)
	}
	return v
}
