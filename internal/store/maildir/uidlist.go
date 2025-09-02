//go:build unix

package maildir

import (
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"
)

// uidListVersion is the dovecot-uidlist format version we read and write.
const uidListVersion = 3

// uidList is the in-memory form of dovecot-uidlist: uidvalidity plus the
// uid → relative-filename mapping ("cur/..." or "new/...").
type uidList struct {
	validity uint32
	guid     string // mailbox GUID field of the format (kept verbatim when present)
	next     uint32 // next-uid hint (N field)
	entries  map[uint32]string
}

// loadUIDList reads dovecot-uidlist. A missing file yields an empty list
// with a fresh validity; a corrupt file returns an error so the caller can
// rebuild from the directory (scan path).
func loadUIDList(dir string) (*uidList, error) {
	data, err := os.ReadFile(filepath.Join(dir, "dovecot-uidlist"))
	if os.IsNotExist(err) {
		// Millisecond resolution so a mailbox recreated in the same second
		// still gets a fresh UIDVALIDITY (RFC 3501: must change when UIDs
		// can no longer be guaranteed stable).
		return &uidList{
			validity: uint32(time.Now().UnixNano() / 1e6),
			entries:  map[uint32]string{},
		}, nil
	}
	if err != nil {
		return nil, err
	}
	ul, err := parseUIDList(data)
	if err != nil {
		return nil, fmt.Errorf("maildir: parse uidlist: %w", err)
	}
	return ul, nil
}

// saveUIDList atomically writes dovecot-uidlist.
func saveUIDList(dir string, ul *uidList) error {
	path := filepath.Join(dir, "dovecot-uidlist")
	tmp, err := os.CreateTemp(dir, ".uidlist-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(serializeUIDList(ul)); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	syncDir(dir)
	return nil
}

// syncDir flushes a directory entry to disk after rename so a power cut
// cannot silently undo the rename (crash durability). Best-effort:
// platforms that cannot fsync directories (Windows) ignore failures.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}

// parseUIDList parses the legacy IMAP version-3 format. The first line carries
// the version and metadata together:
//
//	3 V<uidvalidity> N<next-uid> G<guid>
//	<uid> <relative filename>
//
// The two-line legacy shape (version, then a bare uidvalidity) is also
// accepted. Relative filenames follow the format's convention: "cur/<name>" or
// "<name>", and "new/<name>" or ":<name>".
func parseUIDList(data []byte) (*uidList, error) {
	lines := strings.Split(string(data), "\n")
	for len(lines) > 0 && strings.TrimSpace(lines[len(lines)-1]) == "" {
		lines = lines[:len(lines)-1]
	}
	if len(lines) < 2 {
		return nil, fmt.Errorf("too few lines (%d)", len(lines))
	}
	ver, meta, err := parseVersionLine(strings.TrimSpace(lines[0]))
	if err != nil || ver != uidListVersion {
		return nil, fmt.Errorf("unsupported version %q", lines[0])
	}
	ul := &uidList{entries: map[uint32]string{}}
	start := 1
	if meta == "" {
		// Legacy shape: the metadata lives on its own second line.
		if err := parseMetadataLine(strings.TrimSpace(lines[1]), ul); err != nil {
			return nil, err
		}
		start = 2
	} else {
		if err := parseMetadataLine(meta, ul); err != nil {
			return nil, err
		}
	}
	for _, line := range lines[start:] {
		line = strings.TrimSpace(line)
		if line == "" {
			continue
		}
		fields := strings.Fields(line)
		if len(fields) != 2 {
			return nil, fmt.Errorf("bad uidlist line %q", line)
		}
		uid, err := strconv.ParseUint(fields[0], 10, 32)
		if err != nil {
			return nil, fmt.Errorf("bad uid %q", fields[0])
		}
		ul.entries[uint32(uid)] = normalizeRelPath(fields[1])
	}
	return ul, nil
}

// parseVersionLine splits "3 V... N... G..." into the version and the
// remaining metadata ("" for a bare version line).
func parseVersionLine(line string) (int, string, error) {
	ver, rest, ok := strings.Cut(line, " ")
	if !ok {
		v, err := strconv.Atoi(line)
		return v, "", err
	}
	v, err := strconv.Atoi(strings.TrimSpace(ver))
	return v, strings.TrimSpace(rest), err
}

func parseMetadataLine(line string, ul *uidList) error {
	if strings.HasPrefix(line, "V") {
		var guid string
		var validity, next uint64
		rest := line
		for _, part := range strings.Fields(rest) {
			switch {
			case strings.HasPrefix(part, "V"):
				v, err := strconv.ParseUint(part[1:], 10, 32)
				if err != nil {
					return fmt.Errorf("bad uidvalidity %q", line)
				}
				validity = v
			case strings.HasPrefix(part, "N"):
				n, err := strconv.ParseUint(part[1:], 10, 32)
				if err != nil {
					return fmt.Errorf("bad next-uid %q", line)
				}
				next = n
			case strings.HasPrefix(part, "G"):
				guid = part[1:]
			}
		}
		ul.validity = uint32(validity)
		ul.next = uint32(next)
		ul.guid = guid
		return nil
	}
	validity, err := strconv.ParseUint(line, 10, 32)
	if err != nil {
		return fmt.Errorf("bad uidvalidity %q", line)
	}
	ul.validity = uint32(validity)
	return nil
}

// normalizeRelPath maps the uidlist format's path forms to "<dir>/<name>".
func normalizeRelPath(rel string) string {
	if strings.HasPrefix(rel, "cur/") || strings.HasPrefix(rel, "new/") {
		return rel
	}
	if strings.HasPrefix(rel, ":") {
		return "new/" + rel[1:]
	}
	return "cur/" + rel
}

// denormalizeRelPath reverses normalizeRelPath for serialization.
func denormalizeRelPath(rel string) string {
	if strings.HasPrefix(rel, "new/") {
		return ":" + rel[len("new/"):]
	}
	return strings.TrimPrefix(rel, "cur/")
}

func serializeUIDList(ul *uidList) []byte {
	var b strings.Builder
	guid := ul.guid
	if guid == "" {
		guid = newGUID()
	}
	next := ul.next
	if maxUID(ul)+1 > next {
		next = maxUID(ul) + 1
	}
	fmt.Fprintf(&b, "%d V%d N%d G%s\n", uidListVersion, ul.validity, next, guid)
	uids := make([]uint32, 0, len(ul.entries))
	for uid := range ul.entries {
		uids = append(uids, uid)
	}
	sort.Slice(uids, func(i, j int) bool { return uids[i] < uids[j] })
	for _, uid := range uids {
		fmt.Fprintf(&b, "%d %s\n", uid, denormalizeRelPath(ul.entries[uid]))
	}
	return []byte(b.String())
}

func newGUID() string {
	var b [16]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "00000000000000000000000000000000"
	}
	return hex.EncodeToString(b[:])
}

func maxUID(ul *uidList) uint32 {
	var max uint32
	for uid := range ul.entries {
		if uid > max {
			max = uid
		}
	}
	return max
}

func hasEntry(ul *uidList, rel string) bool {
	for _, r := range ul.entries {
		if r == rel {
			return true
		}
	}
	return false
}
