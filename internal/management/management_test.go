package management

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"mailezine/internal/mailstore"
	"mailezine/internal/queue"
	"mailezine/internal/store"
)

func testHandler() http.Handler {
	info := Info{
		Version:       "dev",
		Storage:       "maildir",
		DirectoryMode: "dev",
		AuthMode:      "dev",
		StartedAt:     time.Now().Add(-30 * time.Second),
	}
	return WithSecret(NewHandler(info, nil, nil, nil, nil, nil), "sekret")
}

func TestStatusRequiresSecret(t *testing.T) {
	h := testHandler()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	resp, err := http.Get(srv.URL + "/v1/status")
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("expected 401 without secret, got %d", resp.StatusCode)
	}
}

func TestStatusWithSecret(t *testing.T) {
	h := testHandler()
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/status", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("expected 200, got %d", resp.StatusCode)
	}
	body, _ := io.ReadAll(resp.Body)
	var status map[string]any
	if err := json.Unmarshal(body, &status); err != nil {
		t.Fatal(err)
	}
	if status["version"] != "dev" || status["storage"] != "maildir" {
		t.Fatalf("unexpected status: %s", body)
	}
	if uptime := status["uptimeSeconds"].(float64); uptime < 20 || uptime > 40 {
		t.Fatalf("uptime out of range: %v", uptime)
	}
}

func TestQueueEndpoints(t *testing.T) {
	h := WithSecret(NewHandler(Info{}, &fakeQueue{
		messages: []queue.Message{
			{ID: 1, From: "a@x.test", State: queue.StateDeferred, Attempts: 2, MaxAttempts: 5},
		},
	}, nil, nil, nil, nil), "sekret")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	get := func(path string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodGet, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer sekret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := get("/v1/queue")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("queue list: %d", resp.StatusCode)
	}
	var out struct {
		Count    int `json:"count"`
		Messages []struct {
			ID    uint64 `json:"id"`
			From  string `json:"from"`
			State string `json:"state"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if out.Count != 1 || out.Messages[0].ID != 1 || out.Messages[0].State != "deferred" {
		t.Fatalf("queue list body: %+v", out)
	}
}

func TestQueueActions(t *testing.T) {
	fq := &fakeQueue{}
	h := WithSecret(NewHandler(Info{}, fq, nil, nil, nil, nil), "sekret")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	post := func(path string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer sekret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}
	resp := post("/v1/queue/7/retry")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("retry: %d", resp.StatusCode)
	}
	resp.Body.Close()
	resp = post("/v1/queue/7/cancel")
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("cancel: %d", resp.StatusCode)
	}
	resp.Body.Close()
	if fq.retried != 7 || fq.cancelled != 7 {
		t.Fatalf("actions: retry=%d cancel=%d", fq.retried, fq.cancelled)
	}
}

type fakeQueue struct {
	messages  []queue.Message
	retried   uint64
	cancelled uint64
}

func (f *fakeQueue) List(context.Context) ([]queue.Message, error) { return f.messages, nil }
func (f *fakeQueue) Retry(_ context.Context, id uint64) error {
	f.retried = id
	return nil
}
func (f *fakeQueue) Cancel(_ context.Context, id uint64) error {
	f.cancelled = id
	return nil
}
func (f *fakeQueue) Pause()  {}
func (f *fakeQueue) Resume() {}

func TestAccountsEndpoint(t *testing.T) {
	s := store.New(store.NewMemoryKV(), store.NewMemoryBlob())
	ms := mailstore.NewKV(s)
	ctx := context.Background()
	if _, err := ms.Deliver(ctx, "alice@example.com", "INBOX", &mailstore.Message{Data: []byte("m1\r\n")}); err != nil {
		t.Fatal(err)
	}
	h := WithSecret(NewHandler(Info{}, nil, ms, s, nil, nil), "sekret")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/accounts", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("accounts: %d", resp.StatusCode)
	}
	var out []struct {
		Account   string `json:"account"`
		Mailboxes int    `json:"mailboxes"`
		Messages  int    `json:"messages"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if len(out) != 1 || out[0].Account != "alice@example.com" || out[0].Messages < 1 {
		t.Fatalf("accounts body: %+v", out)
	}
}

// fakeKicker records the accounts the control plane asked to disconnect.
type fakeKicker struct{ kicked []string }

func (f *fakeKicker) Disconnect(account string) int {
	f.kicked = append(f.kicked, account)
	return 2
}

func TestAccountsDisconnect(t *testing.T) {
	k := &fakeKicker{}
	h := WithSecret(NewHandler(Info{}, nil, nil, nil, k, nil), "sekret")
	srv := httptest.NewServer(h)
	t.Cleanup(srv.Close)

	post := func(path string) *http.Response {
		t.Helper()
		req, _ := http.NewRequest(http.MethodPost, srv.URL+path, nil)
		req.Header.Set("Authorization", "Bearer sekret")
		resp, err := http.DefaultClient.Do(req)
		if err != nil {
			t.Fatal(err)
		}
		return resp
	}

	resp := post("/v1/accounts/alice%40example.com/disconnect")
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("disconnect: %d", resp.StatusCode)
	}
	var out struct {
		OK       bool   `json:"ok"`
		Account  string `json:"account"`
		Sessions int    `json:"sessions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&out); err != nil {
		t.Fatal(err)
	}
	if !out.OK || out.Account != "alice@example.com" || out.Sessions != 2 {
		t.Fatalf("disconnect body: %+v", out)
	}
	if len(k.kicked) != 1 || k.kicked[0] != "alice@example.com" {
		t.Fatalf("kicker: %+v", k.kicked)
	}

	// Unknown actions and the wrong method stay rejected.
	if resp := post("/v1/accounts/alice%40example.com/purge"); resp.StatusCode != http.StatusNotFound {
		t.Fatalf("unknown action: %d, want 404", resp.StatusCode)
	}
	req, _ := http.NewRequest(http.MethodGet, srv.URL+"/v1/accounts/alice%40example.com/disconnect", nil)
	req.Header.Set("Authorization", "Bearer sekret")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("GET disconnect: %d, want 405", resp.StatusCode)
	}
}
