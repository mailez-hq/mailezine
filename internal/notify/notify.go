// Package notify tells the mailez control plane that a message was just
// delivered to a local mailbox, so web push, webhooks and the webmail SSE
// stream fire immediately instead of waiting for the poller's next tick.
// Delivery notifications are fire-and-forget: a slow or unreachable control
// plane never delays or fails the delivery itself.
package notify

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net/http"
	"time"

	"mailezine/internal/stackhttp"
)

// Delivered names one stored copy: mailbox (wire spelling) plus its UID.
type Delivered struct {
	Mailbox string `json:"mailbox"`
	UID     uint32 `json:"uid"`
}

// Client posts delivery receipts to the backend /stack API.
type Client struct {
	url    string
	hc     *http.Client
	logger *slog.Logger
}

// New builds the client pointed at the receipt endpoint (the config derives
// it from BackendAddress when MAILEZINE_NOTIFY_URL is unset). The optional
// secret authenticates the internal API.
func New(url string, logger *slog.Logger, secret ...string) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		url:    url,
		hc:     stackhttp.New(stackhttp.First(secret), 5*time.Second),
		logger: logger,
	}
}

// Delivered posts one receipt for the account. It is synchronous so callers
// can surface errors in tests; production paths use DeliveredAsync.
func (c *Client) Delivered(ctx context.Context, account string, refs []Delivered) error {
	if len(refs) == 0 {
		return nil
	}
	body, err := json.Marshal(map[string]any{"account": account, "deliveries": refs})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, bytes.NewReader(body))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode >= 300 {
		return fmt.Errorf("notify: backend status %d", resp.StatusCode)
	}
	return nil
}

// DeliveredAsync fires the receipt in the background with its own timeout.
// A failure logs at debug level: the pollers still cover the gap, so the
// only consequence of a lost notification is slower push, never lost mail.
func (c *Client) DeliveredAsync(account string, refs []Delivered) {
	if len(refs) == 0 {
		return
	}
	go func() {
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if err := c.Delivered(ctx, account, refs); err != nil {
			c.logger.Debug("notify: delivery receipt failed", "account", account, "err", err)
		}
	}()
}
