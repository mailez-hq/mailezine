// Package imap implements the IMAP4 server of mailezine on the go-imap/v2
// imapserver base. The session is backed by the persistent mailstore
// (MailboxStore): KV+blob or maildir. Bounded subset:
// LIST/SELECT/FETCH/SEARCH/STORE/APPEND/COPY/MOVE/EXPUNGE/IDLE.
package imap

import (
	"context"
	"crypto/tls"
	"fmt"
	"log/slog"
	"sync"

	"github.com/emersion/go-imap/v2"

	"mailezine/internal/auth"
	"mailezine/internal/directory"
	"mailezine/internal/fts"
	"mailezine/internal/imapserver"
	"mailezine/internal/mailcache"
	"mailezine/internal/mailstore"
	"mailezine/internal/metrics"
)

// mailboxDelim is the hierarchy separator exposed to clients (matches the
// mailez deployment template "/" separator).
const mailboxDelim rune = '/'

// Server is the mailezine IMAP server.
type Server struct {
	Store           mailstore.MailboxStore
	Auth            auth.Service
	Directory       directory.Service
	Port            string // listening port, passed to auth so the control plane recognizes webmail ports
	MaxMessageBytes int64
	TLSConfig       *tls.Config // optional; enables STARTTLS for direct deploys
	Logger          *slog.Logger
	// Learn trains the spam classifier from Junk-boundary mailbox
	// operations. Called with the message bytes when a message enters
	// (isSpam=true) or leaves (false) the Junk mailbox. Optional;
	// failures are logged, never fatal to the IMAP operation.
	Learn func(ctx context.Context, account string, isSpam bool, data []byte)
	// FTS is the optional full-text index; SEARCH TEXT consults it for
	// candidates and verifies against raw bytes.
	FTS *fts.Indexer
	// CacheSizeBytes bounds the envelope/body-structure memo (0 = default).
	CacheSizeBytes int64

	// Metrics instruments the server; nil disables.
	Metrics *metrics.Metrics

	// cache memoizes message-derived data (envelope, body structure) across
	// sessions. nil disables caching.
	cache *mailcache.Cache

	// live maps account -> authenticated connections, for Disconnect.
	mu   sync.Mutex
	live map[string]map[*imapserver.Conn]struct{}
	// raw memoizes whole message buffers, so the second read of a message
	// within (or soon after) one page load does not hit the blob store again:
	// the list path reads each row for its body structure and then reads the
	// same rows again for their preview fragments. Separate from cache on
	// purpose — bodies are orders of magnitude larger than the metadata
	// entries, and sharing one budget would let a burst of mail evict them.
	raw *mailcache.Cache
}

// New builds the go-imap server. TLS is terminated by the mailez gateway,
// so STARTTLS is left disabled here; InsecureAuth is on because the
// gateway's login proxy terminates TLS and forwards LOGIN on the internal
// network (the trusted-subnet model, ADR-002).
func New(s *Server) *imapserver.Server {
	if s.Logger == nil {
		s.Logger = slog.Default()
	}
	if s.MaxMessageBytes <= 0 {
		s.MaxMessageBytes = 50 << 20
	}
	if s.cache == nil {
		s.cache = mailcache.NewCache(s.CacheSizeBytes)
	}
	if s.raw == nil {
		s.raw = mailcache.NewCache(s.CacheSizeBytes)
	}
	caps := imap.CapSet{
		imap.CapIMAP4rev1: {},
		imap.CapID:        {},
		imap.CapMove:      {},
		imap.CapUIDPlus:   {},
		imap.CapNamespace: {},
		imap.CapIdle:      {},
		imap.CapCondStore: {},
		imap.CapSort:      {},
	}
	if s.hasACL() {
		caps[imap.CapACL] = struct{}{}
		caps[imap.CapQuota] = struct{}{}
	}
	return imapserver.New(&imapserver.Options{
		NewSession: func(c *imapserver.Conn) (imapserver.Session, *imapserver.GreetingData, error) {
			return &session{srv: s, conn: c}, nil, nil
		},
		Caps:      caps,
		TLSConfig: s.TLSConfig,
		// Credentials only over TLS when the engine terminates it; the
		// gateway model (no TLSConfig) keeps plaintext auth on the internal
		// link.
		InsecureAuth: s.TLSConfig == nil,
		Logger:       slogAdapter{s.Logger},
	})
}

func (s *Server) track(c *imapserver.Conn, account string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.live == nil {
		s.live = make(map[string]map[*imapserver.Conn]struct{})
	}
	conns := s.live[account]
	if conns == nil {
		conns = make(map[*imapserver.Conn]struct{})
		s.live[account] = conns
	}
	conns[c] = struct{}{}
}

func (s *Server) untrack(c *imapserver.Conn, account string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if conns := s.live[account]; conns != nil {
		delete(conns, c)
		if len(conns) == 0 {
			delete(s.live, account)
		}
	}
}

// Disconnect closes every authenticated connection of account and reports
// how many were dropped.
func (s *Server) Disconnect(account string) int {
	s.mu.Lock()
	conns := s.live[account]
	delete(s.live, account)
	s.mu.Unlock()
	for c := range conns {
		_ = c.NetConn().Close()
	}
	return len(conns)
}

// slogAdapter adapts slog to go-imap's Logger interface.
type slogAdapter struct{ l *slog.Logger }

func (a slogAdapter) Printf(format string, args ...any) {
	if len(args) == 0 {
		a.l.Error("imap: " + format)
		return
	}
	a.l.Error("imap: " + fmt.Sprintf(format, args...))
}

var _ imapserver.Logger = slogAdapter{}
