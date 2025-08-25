//go:build unix

package maildir

import "testing"

// FuzzParseUIDList ensures arbitrary uidlist bytes never panic and that
// round-tripping a parsed list stays stable.
func FuzzParseUIDList(f *testing.F) {
	f.Add([]byte("3 V1 N2 Gabc\n1 cur/a:2,S\n"))
	f.Add([]byte("3\n1700000000\n1 cur/a:2,S\n"))
	f.Add([]byte("garbage"))
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		ul, err := parseUIDList(data)
		if err != nil {
			return
		}
		// Serializing and re-parsing must not panic and must keep entries.
		again, err := parseUIDList(serializeUIDList(ul))
		if err != nil {
			t.Fatalf("reparse after serialize: %v\ninput=%q", err, data)
		}
		if len(again.entries) != len(ul.entries) {
			t.Fatalf("entry count changed: %d → %d", len(ul.entries), len(again.entries))
		}
		for uid, rel := range ul.entries {
			if again.entries[uid] != rel {
				t.Fatalf("entry %d changed: %q → %q", uid, rel, again.entries[uid])
			}
		}
	})
}
