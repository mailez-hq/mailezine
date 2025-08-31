// Package ftssync converges each node's local full-text index to the
// shared KV by tailing the per-account change logs.
//
// In multi-active deployments every engine delivers its own share of the
// mail, but bleve indexes are node-local — without this worker a node's
// index would only know its own deliveries and IMAP SEARCH would silently
// miss the rest of the cluster. The change log (INV-CHANGE: append-only,
// gapless, ordered per account) is the convergence feed: create/update
// entries re-index the message copy, extended delete entries (which carry
// the copy's mailbox and UID) remove it exactly.
//
// Semantics: eventual consistency. A message becomes searchable on a node
// at most one sync interval after it lands anywhere in the cluster. A node
// that loses its index (or joins fresh) replays from watermark 0 and
// rebuilds in full. Delivery-time indexing (single-node path) is replaced
// by this worker under cluster.mode=multi, so each copy is indexed exactly
// once per node regardless of which node delivered it.
package ftssync

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"time"

	"mailezine/internal/fts"
	"mailezine/internal/store"
)

// Worker tails the change logs of every account and applies the email
// collection's changes to one local Indexer. One Worker per node.
type Worker struct {
	Store *store.Store
	Index *fts.Indexer
	// StatePath persists the per-account watermarks; empty derives
	// "<index dir>.sync.json" — callers pass the same path the Indexer
	// opened.
	StatePath string
	// Interval between sync passes; defaults to 2s.
	Interval time.Duration
	Logger   *slog.Logger
}

// state is the durable tailer position: the last applied change ID per
// account email. Account emails (not IDs) survive DeleteAccount/re-create.
type state struct {
	Accounts map[string]uint64 `json:"accounts"`
}

// Run syncs every Interval until ctx is cancelled. Errors are per-pass and
// never stop the worker — the next pass retries from the last committed
// watermark.
func (w *Worker) Run(ctx context.Context) {
	w.defaults()
	t := time.NewTicker(w.Interval)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
			if _, err := w.SyncOnce(ctx); err != nil {
				w.Logger.Warn("ftssync: pass", "err", err)
			}
		}
	}
}

// SyncOnce applies every unseen change to the local index and returns the
// number of applied entries. The watermark advances per account only after
// its whole batch applied, so a mid-pass crash replays the batch (index
// writes are idempotent upserts/deletes).
func (w *Worker) SyncOnce(ctx context.Context) (int, error) {
	w.defaults()
	st, err := w.load()
	if err != nil {
		return 0, err
	}
	accounts, err := w.Store.ListAccounts(ctx)
	if err != nil {
		return 0, err
	}
	applied := 0
	live := make(map[string]uint64, len(accounts))
	for _, account := range accounts {
		mark, n, err := w.syncAccount(ctx, st, account)
		if err != nil {
			return applied, err
		}
		applied += n
		live[account] = mark
	}
	// Accounts that vanished since the last pass (DeleteAccount) drop out
	// of the watermarks; their index entries age out on the next reindex.
	st.Accounts = live
	if err := w.save(st); err != nil {
		return applied, err
	}
	return applied, nil
}

func (w *Worker) syncAccount(ctx context.Context, st *state, account string) (mark uint64, applied int, err error) {
	acctID, err := w.Store.AccountByEmail(ctx, account)
	if err != nil {
		return 0, 0, err
	}
	mark = st.Accounts[account]
	changes, err := w.Store.ChangesSince(ctx, acctID, store.CollectionEmail, mark)
	if err != nil || len(changes) == 0 {
		return mark, 0, err
	}
	for _, ch := range changes {
		if err := w.apply(ctx, acctID, account, ch); err != nil {
			// Per-change failures (blob read hiccup, corrupt row) never
			// stall the tail: log and keep going. A lost update self-heals
			// on the next flag change or reindex.
			w.Logger.Warn("ftssync: apply", "account", account, "change", ch.ChangeID, "err", err)
		}
		applied++
		mark = ch.ChangeID
	}
	return mark, applied, nil
}

// apply projects one change onto the local index.
func (w *Worker) apply(ctx context.Context, acctID store.AccountID, account string, ch store.Change) error {
	switch ch.Op {
	case store.OpCreate, store.OpUpdate:
		fields, err := w.Store.GetDocumentFields(ctx, acctID, store.CollectionEmail, ch.DocID)
		if err != nil {
			if errors.Is(err, store.ErrNotFound) {
				// Superseded by a later delete inside the same batch (or
				// a purged account): nothing to index.
				return nil
			}
			return err
		}
		mailbox := string(fields[store.EmailFieldMailbox])
		uid := uidOf(fields[store.EmailFieldUID])
		blobID := string(fields[store.EmailFieldBlob])
		if mailbox == "" || uid == 0 || blobID == "" {
			return nil
		}
		var buf bytes.Buffer
		if err := w.Store.GetBlob(ctx, blobID, &buf); err != nil {
			return err
		}
		return w.Index.IndexMessage(ctx, account, mailbox, uid, buf.Bytes())
	case store.OpDelete:
		if ch.Mailbox == "" || ch.UID == 0 {
			// Legacy entry written before the extended encoding: the exact
			// copy is unrecoverable, leave the stale hit in place (it is
			// filtered out by the raw-byte verification after every FTS
			// candidate match). Full reindex clears it.
			return nil
		}
		return w.Index.DeleteMessage(ctx, account, ch.Mailbox, ch.UID)
	}
	return nil
}

func (w *Worker) stateFile() string {
	if w.StatePath != "" {
		return w.StatePath
	}
	return "ftssync-state.json"
}

func (w *Worker) load() (*state, error) {
	st := &state{Accounts: map[string]uint64{}}
	data, err := os.ReadFile(w.stateFile())
	if errors.Is(err, os.ErrNotExist) {
		return st, nil
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(data, st); err != nil {
		// A corrupt watermark file replays from zero: worst case is a full
		// (idempotent) rebuild, never a permanently stuck tail.
		w.Logger.Warn("ftssync: state unreadable; replaying from zero", "err", err)
		return &state{Accounts: map[string]uint64{}}, nil
	}
	if st.Accounts == nil {
		st.Accounts = map[string]uint64{}
	}
	return st, nil
}

func (w *Worker) save(st *state) error {
	data, err := json.Marshal(st)
	if err != nil {
		return err
	}
	path := w.stateFile()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return err
	}
	tmp := path + ".tmp"
	if err := os.WriteFile(tmp, data, 0o644); err != nil {
		return err
	}
	return os.Rename(tmp, path)
}

func (w *Worker) defaults() {
	if w.Interval <= 0 {
		w.Interval = 2 * time.Second
	}
	if w.Logger == nil {
		w.Logger = slog.Default()
	}
}

// uidOf decodes the EmailFieldUID convention (8-byte BE).
func uidOf(b []byte) uint32 {
	if len(b) != 8 {
		return 0
	}
	return uint32(binary.BigEndian.Uint64(b))
}
