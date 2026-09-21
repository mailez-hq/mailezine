// Package management serves the internal management API (port 8090):
// status and queue operations. It is reachable only inside
// the compose network and is protected by a shared secret
// (ARCHITECTURE.md §8.3).
package management

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"mailezine/internal/mailstore"
	"mailezine/internal/queue"
	"mailezine/internal/store"
)

// Info is the read-only state the management API reports. Extra carries
// build-specific entries merged verbatim into the /v1/status payload.
type Info struct {
	Version       string
	Storage       string
	DirectoryMode string
	AuthMode      string
	Extra         map[string]any
	StartedAt     time.Time
}

// QueueManager is the outbound queue surface exposed by the management API.
// queue.Manager satisfies it.
type QueueManager interface {
	List(ctx context.Context) ([]queue.Message, error)
	Retry(ctx context.Context, id uint64) error
	Cancel(ctx context.Context, id uint64) error
	Pause()
	Resume()
}

// AccountLister enumerates accounts for the management overview.
type AccountLister interface {
	ListAccounts(ctx context.Context) ([]string, error)
}

// SessionKicker drops an account's live protocol sessions (imap.Server).
type SessionKicker interface {
	Disconnect(account string) int
}

// NewHandler builds the management API mux.
func NewHandler(info Info, qm QueueManager, mstore mailstore.MailboxStore, accounts AccountLister, sessions SessionKicker, logger *slog.Logger) http.Handler {
	if logger == nil {
		logger = slog.Default()
	}
	mux := http.NewServeMux()
	mux.HandleFunc("/v1/status", func(w http.ResponseWriter, _ *http.Request) {
		status := map[string]any{
			"version":       info.Version,
			"storage":       info.Storage,
			"directory":     info.DirectoryMode,
			"auth":          info.AuthMode,
			"uptimeSeconds": int(time.Since(info.StartedAt).Seconds()),
		}
		for k, v := range info.Extra {
			status[k] = v
		}
		if qm != nil {
			if msgs, err := qm.List(context.Background()); err == nil {
				counts := map[string]int{}
				for _, m := range msgs {
					counts[string(m.State)]++
				}
				status["queue"] = counts
			}
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(status)
	})
	mux.HandleFunc("/v1/queue", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if qm == nil {
			http.Error(w, "outbound queue disabled", http.StatusServiceUnavailable)
			return
		}
		msgs, err := qm.List(r.Context())
		if err != nil {
			logger.Error("management: queue list", "err", err)
			http.Error(w, "queue unavailable", http.StatusInternalServerError)
			return
		}
		w.Header().Set("Content-Type", "application/json")
		type view struct {
			ID          uint64    `json:"id"`
			From        string    `json:"from"`
			Recipients  []string  `json:"recipients"`
			Subject     string    `json:"subject,omitempty"`
			State       string    `json:"state"`
			Attempts    int       `json:"attempts"`
			MaxAttempts int       `json:"maxAttempts"`
			NextAttempt time.Time `json:"nextAttempt"`
			LastError   string    `json:"lastError,omitempty"`
			CreatedAt   time.Time `json:"createdAt"`
		}
		out := make([]view, 0, len(msgs))
		for _, m := range msgs {
			rcpts := make([]string, 0, len(m.Recipients))
			for _, r := range m.Recipients {
				rcpts = append(rcpts, r.Address)
			}
			out = append(out, view{
				ID: m.ID, From: m.From, Recipients: rcpts, Subject: m.Subject,
				State: string(m.State), Attempts: m.Attempts, MaxAttempts: m.MaxAttempts,
				NextAttempt: m.NextAttempt, LastError: m.LastError, CreatedAt: m.CreatedAt,
			})
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"count": len(out), "messages": out})
	})
	mux.HandleFunc("/v1/queue/pause", func(w http.ResponseWriter, r *http.Request) {
		if qm == nil {
			http.Error(w, "outbound queue disabled", http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		qm.Pause()
		logger.Info("audit: management queue pause", "client", r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "action": "pause"})
	})
	mux.HandleFunc("/v1/queue/resume", func(w http.ResponseWriter, r *http.Request) {
		if qm == nil {
			http.Error(w, "outbound queue disabled", http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		qm.Resume()
		logger.Info("audit: management queue resume", "client", r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "action": "resume"})
	})
	mux.HandleFunc("/v1/accounts", func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if accounts == nil || mstore == nil {
			http.Error(w, "account overview unavailable", http.StatusServiceUnavailable)
			return
		}
		emails, err := accounts.ListAccounts(r.Context())
		if err != nil {
			logger.Error("management: list accounts", "err", err)
			http.Error(w, "account listing failed", http.StatusInternalServerError)
			return
		}
		type accountView struct {
			Account   string `json:"account"`
			Mailboxes int    `json:"mailboxes"`
			Messages  int    `json:"messages"`
		}
		out := make([]accountView, 0, len(emails))
		for _, email := range emails {
			boxes, err := mstore.ListMailboxes(r.Context(), email)
			if err != nil {
				continue
			}
			var messages int
			for _, mb := range boxes {
				messages += int(mb.NumMessages)
			}
			out = append(out, accountView{Account: email, Mailboxes: len(boxes), Messages: messages})
		}
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(out)
	})
	// DELETE /v1/accounts/{email} purges an account with all of its engine
	// data. The control plane calls this on user deletion: without it the
	// engine keeps orphaned mailboxes, and a re-created same-address account
	// would silently inherit the previous owner's mail.
	//
	// POST /v1/accounts/{email}/disconnect drops the account's live IMAP
	// sessions, so a control-plane policy change reaches them.
	mux.HandleFunc("/v1/accounts/", func(w http.ResponseWriter, r *http.Request) {
		email, action, _ := strings.Cut(strings.TrimPrefix(r.URL.Path, "/v1/accounts/"), "/")
		if email == "" {
			http.Error(w, "account email required", http.StatusBadRequest)
			return
		}
		if action != "" && action != "disconnect" {
			http.Error(w, "unsupported action", http.StatusNotFound)
			return
		}
		if action == "disconnect" {
			if r.Method != http.MethodPost {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			if sessions == nil {
				http.Error(w, "session disconnect unsupported", http.StatusNotImplemented)
				return
			}
			dropped := sessions.Disconnect(email)
			logger.Info("audit: management disconnect", "account", email, "sessions", dropped, "client", r.RemoteAddr)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "account": email, "sessions": dropped})
			return
		}
		if r.Method != http.MethodDelete {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		purger, ok := mstore.(mailstore.AccountPurger)
		if !ok {
			http.Error(w, "account purge unsupported by storage backend", http.StatusNotImplemented)
			return
		}
		if err := purger.DeleteAccount(r.Context(), email); err != nil {
			if errors.Is(err, store.ErrNotFound) {
				http.Error(w, "account not found", http.StatusNotFound)
				return
			}
			logger.Error("management: purge account", "account", email, "err", err)
			http.Error(w, "account purge failed", http.StatusInternalServerError)
			return
		}
		logger.Info("audit: management account purge", "account", email, "client", r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "account": email})
	})
	mux.HandleFunc("/v1/queue/", func(w http.ResponseWriter, r *http.Request) {
		if qm == nil {
			http.Error(w, "outbound queue disabled", http.StatusServiceUnavailable)
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/v1/queue/"), "/")
		if len(parts) != 2 {
			http.Error(w, "unsupported action", http.StatusNotFound)
			return
		}
		id, err := strconv.ParseUint(parts[0], 10, 64)
		if err != nil || id == 0 {
			http.Error(w, "invalid message id", http.StatusBadRequest)
			return
		}
		var opErr error
		switch parts[1] {
		case "retry":
			opErr = qm.Retry(r.Context(), id)
		case "cancel":
			opErr = qm.Cancel(r.Context(), id)
		default:
			http.Error(w, "unsupported action", http.StatusNotFound)
			return
		}
		if errors.Is(opErr, mailstore.ErrNotFound) {
			http.Error(w, "message not found", http.StatusNotFound)
			return
		}
		if opErr != nil {
			logger.Error("management: queue action", "action", parts[1], "id", id, "err", opErr)
			http.Error(w, "operation failed", http.StatusInternalServerError)
			return
		}
		logger.Info("audit: management queue action", "action", parts[1], "id", id, "client", r.RemoteAddr)
		w.Header().Set("Content-Type", "application/json")
		_ = json.NewEncoder(w).Encode(map[string]any{"ok": true, "id": id, "action": parts[1]})
	})
	return mux
}

// WithSecret requires "Authorization: Bearer <secret>".
func WithSecret(next http.Handler, secret string) http.Handler {
	want := "Bearer " + secret
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), []byte(want)) != 1 {
			w.Header().Set("WWW-Authenticate", `Bearer realm="mailezine-management"`)
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}
