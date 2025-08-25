//go:build unix

package maildir

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strconv"
)

// sidecarData is the rebuildable per-account metadata. Losing it loses only
// derived state (keywords, persisted uidnext); messages and UIDs always come
// from the maildir + dovecot-uidlist (ARCHITECTURE.md §5.3).
type sidecarData struct {
	UIDNext       map[string]uint32              `json:"uidnext"`
	Keywords      map[string]map[string][]string `json:"keywords"` // mailbox → uid → keywords
	Subscriptions map[string]bool                `json:"subscriptions"`
	ACL           map[string]map[string]string   `json:"acl"`       // mailbox → identifier → rights (RFC 4314)
	Vacation      map[string]int64               `json:"vacation"`  // sender → last auto-reply unixnano
	ModSeq        map[string]uint64              `json:"modseq"`    // mailbox → highest modification sequence
	MsgModSeq     map[string]map[string]uint64   `json:"msgmodseq"` // mailbox → uid → message modseq
	Sieve         map[string]string              `json:"sieve"`     // script name → content
	ActiveSieve   string                         `json:"activeSieve"`
}

func newSidecar() *sidecarData {
	return &sidecarData{
		UIDNext:       map[string]uint32{},
		Keywords:      map[string]map[string][]string{},
		Subscriptions: map[string]bool{},
		Vacation:      map[string]int64{},
		ModSeq:        map[string]uint64{},
		MsgModSeq:     map[string]map[string]uint64{},
		Sieve:         map[string]string{},
	}
}

// withSidecar atomically applies fn to the account sidecar (account lock
// held). The caller must already hold the mailbox mutex; lock order is
// mailbox → account, so there is no cycle.
func (a *Account) withSidecar(fn func(*sidecarData) error) error {
	a.mu.Lock()
	defer a.mu.Unlock()

	path := filepath.Join(a.root, ".mailezine", "keywords.json")
	s := newSidecar()
	if data, err := os.ReadFile(path); err == nil {
		_ = json.Unmarshal(data, s) // corrupt sidecar → empty, rebuilt on next write
	} else if !os.IsNotExist(err) {
		return err
	}
	if s.UIDNext == nil {
		s.UIDNext = map[string]uint32{}
	}
	if s.Keywords == nil {
		s.Keywords = map[string]map[string][]string{}
	}
	if s.Subscriptions == nil {
		s.Subscriptions = map[string]bool{}
	}
	if s.Vacation == nil {
		s.Vacation = map[string]int64{}
	}
	if s.ModSeq == nil {
		s.ModSeq = map[string]uint64{}
	}
	if s.MsgModSeq == nil {
		s.MsgModSeq = map[string]map[string]uint64{}
	}
	if s.ACL == nil {
		s.ACL = map[string]map[string]string{}
	}
	if s.Sieve == nil {
		s.Sieve = map[string]string{}
	}
	if err := fn(s); err != nil {
		return err
	}

	dir := filepath.Join(a.root, ".mailezine")
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(s, "", "  ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".sidecar-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		return err
	}
	if err := tmp.Sync(); err != nil {
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (m *Mailbox) sidecarUIDNext() (uint32, error) {
	var next uint32
	err := m.acct.withSidecar(func(s *sidecarData) error {
		next = s.UIDNext[m.name]
		return nil
	})
	return next, err
}

func (m *Mailbox) setSidecarUIDNext(next uint32) error {
	return m.acct.withSidecar(func(s *sidecarData) error {
		s.UIDNext[m.name] = next
		return nil
	})
}

func (m *Mailbox) keywords(uid uint32) ([]string, error) {
	var out []string
	err := m.acct.withSidecar(func(s *sidecarData) error {
		out = append(out, s.Keywords[m.name][strconv.FormatUint(uint64(uid), 10)]...)
		return nil
	})
	return out, err
}

func (m *Mailbox) setKeywords(uid uint32, keywords []string) error {
	return m.acct.withSidecar(func(s *sidecarData) error {
		if len(keywords) == 0 {
			delete(s.Keywords[m.name], strconv.FormatUint(uint64(uid), 10))
			return nil
		}
		if s.Keywords[m.name] == nil {
			s.Keywords[m.name] = map[string][]string{}
		}
		s.Keywords[m.name][strconv.FormatUint(uint64(uid), 10)] = append([]string(nil), keywords...)
		return nil
	})
}

func (m *Mailbox) dropKeywords(uid uint32) error {
	return m.setKeywords(uid, nil)
}
