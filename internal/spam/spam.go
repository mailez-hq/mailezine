// Package spam implements the inbound classification stage
// (ARCHITECTURE.md §4): rspamd /checkv2 over HTTP (decision D5). The
// protocol follows docs.rspamd.com/developers/protocol: envelope data travels
// in From/Rcpt/IP/Helo/Hostname/Queue-Id headers, and the JSON reply carries
// the action, score and headers to add (milter.add_headers).
package spam

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"sort"
	"time"
)

// Result is the classification of one message.
type Result struct {
	Action        string // "no action", "greylist", "add header", "rewrite subject", "soft reject", "reject"
	Score         float64
	RequiredScore float64
	Headers       []string // "Name: value" lines to prepend, in insertion order
}

// Client scans messages with rspamd.
type Client struct {
	URL      string // e.g. http://rspamd:11333/checkv2
	LearnURL string // controller endpoint, e.g. http://rspamd:11334; "" disables learning
	Password string // controller password (rspamc -P)
	Hostname string // our hostname, passed to rspamd
	hc       *http.Client
	logger   *slog.Logger
}

// New builds an rspamd client.
func New(url, learnURL, password, hostname string, logger *slog.Logger) *Client {
	if logger == nil {
		logger = slog.Default()
	}
	return &Client{
		URL:      url,
		LearnURL: learnURL,
		Password: password,
		Hostname: hostname,
		hc:       &http.Client{Timeout: 30 * time.Second},
		logger:   logger,
	}
}

// Learn trains the classifier with one message: /learnspam or /learnham on
// the controller. Learning is best-effort; callers log failures.
func (c *Client) Learn(ctx context.Context, isSpam bool, data []byte) error {
	if c.LearnURL == "" {
		return nil
	}
	path := "/learnspam"
	if !isSpam {
		path = "/learnham"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.LearnURL+path, bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "message/rfc822")
	if c.Password != "" {
		req.Header.Set("Password", c.Password)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("spam: rspamd learn %s status %d", path, resp.StatusCode)
	}
	return nil
}

// Fuzzy adds or removes a message in the rspamd fuzzy hash
// (/fuzzyadd|/fuzzydel). Best-effort: a missing fuzzy rule or storage error
// is logged by the caller, never fatal to learning.
func (c *Client) Fuzzy(ctx context.Context, flag int, add bool, data []byte) error {
	if c.LearnURL == "" {
		return nil
	}
	path := "/fuzzydel"
	if add {
		path = "/fuzzyadd"
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		fmt.Sprintf("%s%s?flag=%d", c.LearnURL, path, flag), bytes.NewReader(data))
	if err != nil {
		return err
	}
	req.Header.Set("Content-Type", "message/rfc822")
	if c.Password != "" {
		req.Header.Set("Password", c.Password)
	}
	resp, err := c.hc.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("spam: rspamd fuzzy %s flag=%d status %d", path, flag, resp.StatusCode)
	}
	return nil
}

// FuzzySpamFlag and FuzzyHamFlag are the fuzzy hash flags used by the
// mailez learning convention (13 spam, 11 ham).
const (
	FuzzySpamFlag = 13
	FuzzyHamFlag  = 11
)

// LearnWithFuzzy trains the classifier and updates the fuzzy hash in one
// best-effort pass.
func (c *Client) LearnWithFuzzy(ctx context.Context, isSpam bool, data []byte) error {
	if err := c.Learn(ctx, isSpam, data); err != nil {
		return err
	}
	if isSpam {
		_ = c.Fuzzy(ctx, FuzzySpamFlag, true, data)
		_ = c.Fuzzy(ctx, FuzzyHamFlag, false, data)
	} else {
		_ = c.Fuzzy(ctx, FuzzyHamFlag, false, data)
		_ = c.Fuzzy(ctx, FuzzySpamFlag, true, data)
	}
	return nil
}

// Classify scans a raw message. A non-2xx or unparsable response is an
// error; the caller decides how to degrade (fail-open with a mark).
func (c *Client) Classify(ctx context.Context, peer net.IP, from string, to []string, data []byte) (Result, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, c.URL, bytes.NewReader(data))
	if err != nil {
		return Result{}, err
	}
	req.Header.Set("Content-Type", "message/rfc822")
	req.Header.Set("From", from)
	for _, r := range to {
		req.Header.Add("Rcpt", r)
	}
	if peer != nil {
		req.Header.Set("IP", peer.String())
	}
	req.Header.Set("Hostname", c.Hostname)
	req.Header.Set("Queue-Id", fmt.Sprintf("mailezine-%d", time.Now().UnixNano()))
	req.Header.Set("Pass", "all")

	resp, err := c.hc.Do(req)
	if err != nil {
		return Result{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return Result{}, fmt.Errorf("spam: rspamd status %d", resp.StatusCode)
	}
	var rsp struct {
		Action        string  `json:"action"`
		Score         float64 `json:"score"`
		RequiredScore float64 `json:"required_score"`
		Milter        struct {
			AddHeaders map[string]struct {
				Value string `json:"value"`
				Order int    `json:"order"`
			} `json:"add_headers"`
		} `json:"milter"`
		Headers map[string]json.RawMessage `json:"headers"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&rsp); err != nil {
		return Result{}, fmt.Errorf("spam: decode rspamd reply: %w", err)
	}
	return Result{
		Action:        rsp.Action,
		Score:         rsp.Score,
		RequiredScore: rsp.RequiredScore,
		Headers:       collectHeaders(rsp.Milter.AddHeaders, rsp.Headers),
	}, nil
}

func collectHeaders(ordered map[string]struct {
	Value string `json:"value"`
	Order int    `json:"order"`
}, fallback map[string]json.RawMessage) []string {
	if len(ordered) > 0 {
		type hdr struct {
			name  string
			value string
			order int
		}
		out := make([]hdr, 0, len(ordered))
		for name, h := range ordered {
			out = append(out, hdr{name, h.Value, h.Order})
		}
		sort.Slice(out, func(i, j int) bool { return out[i].order < out[j].order })
		lines := make([]string, 0, len(out))
		for _, h := range out {
			lines = append(lines, h.name+": "+h.value)
		}
		return lines
	}
	var lines []string
	for name, raw := range fallback {
		var value string
		if err := json.Unmarshal(raw, &value); err == nil {
			lines = append(lines, name+": "+value)
		}
	}
	sort.Strings(lines)
	return lines
}
