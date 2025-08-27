// Package dlp scans outbound submissions against the control plane's DLP
// rules (敏感词过滤 + 审批). The engine asks the backend for a verdict
// before delivering; failures fail open so mail never stops on a DLP outage.
package dlp

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"mime/multipart"
	"net/http"
	"time"
)

// Decision is the control plane's verdict for one submission.
type Decision struct {
	Action string // pass | block | hold
	Reason string
	ID     uint64 // pending approval id when held
}

// Checker validates outbound message content.
type Checker interface {
	Check(ctx context.Context, user, from string, to []string, raw []byte) (Decision, error)
}

// HTTP talks to the mailez backend /stack/dlp/check endpoint.
type HTTP struct {
	url    string
	hc     *http.Client
	logger *slog.Logger
}

// NewHTTP builds the control-plane DLP client.
func NewHTTP(url string, logger *slog.Logger) *HTTP {
	if logger == nil {
		logger = slog.Default()
	}
	return &HTTP{
		url:    url,
		hc:     &http.Client{Timeout: 10 * time.Second},
		logger: logger,
	}
}

// Check posts the message to the control plane. A transport/parsing error
// returns a pass decision so mail keeps flowing (the gap is logged).
func (c *HTTP) Check(ctx context.Context, user, from string, to []string, raw []byte) (Decision, error) {
	var buf bytes.Buffer
	mw := multipart.NewWriter(&buf)
	meta, err := mw.CreateFormField("meta")
	if err != nil {
		return Decision{Action: "pass"}, nil
	}
	_ = json.NewEncoder(meta).Encode(map[string]any{
		"sender_email": user,
		"from":         from,
		"to":           to,
	})
	rawPart, err := mw.CreateFormFile("raw", "message.eml")
	if err != nil {
		return Decision{Action: "pass"}, nil
	}
	_, _ = rawPart.Write(raw)
	_ = mw.Close()

	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.url, &buf)
	if err != nil {
		c.logger.Warn("dlp: build request", "err", err)
		return Decision{Action: "pass"}, nil
	}
	req.Header.Set("Content-Type", mw.FormDataContentType())
	resp, err := c.hc.Do(req)
	if err != nil {
		c.logger.Warn("dlp: endpoint unreachable, fail open", "err", err)
		return Decision{Action: "pass"}, nil
	}
	defer resp.Body.Close()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return Decision{Action: "pass"}, nil
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		c.logger.Warn("dlp: endpoint status", "status", resp.StatusCode)
		return Decision{Action: "pass"}, nil
	}
	var out struct {
		Action string `json:"action"`
		Reason string `json:"reason"`
		ID     uint64 `json:"id"`
	}
	if err := json.Unmarshal(body, &out); err != nil {
		return Decision{Action: "pass"}, nil
	}
	switch out.Action {
	case "block", "hold":
		return Decision{Action: out.Action, Reason: out.Reason, ID: out.ID}, nil
	default:
		return Decision{Action: "pass"}, nil
	}
}

// ErrBlocked marks a submission rejected by a DLP rule.
func ErrBlocked(reason string) error {
	return fmt.Errorf("dlp: blocked: %s", reason)
}
