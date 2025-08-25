// imapsieve-equivalent learning integration: APPEND/COPY/MOVE across the
// Junk boundary must hand the message bytes to the learner with the right
// polarity (into Junk = spam, out of Junk = ham).
package imap

import (
	"context"
	"io"
	"log/slog"
	"net"
	"strings"
	"testing"

	"github.com/emersion/go-imap/v2"
	"github.com/emersion/go-imap/v2/imapclient"

	"mailezine/internal/auth"
	"mailezine/internal/directory"
	"mailezine/internal/mailstore"
	"mailezine/internal/store"
)

func startLearnServer(t *testing.T) (*imapclient.Client, *[]learnCall) {
	t.Helper()
	dir := directory.NewDev(directory.DevData{
		Users: map[string]directory.User{
			"alice@example.com": {Email: "alice@example.com", Enabled: true, QuotaBytes: 1 << 20},
		},
		Domains: []string{"example.com"},
	})
	var calls []learnCall
	srv := New(&Server{
		Store:           mailstore.NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob())),
		Auth:            auth.NewDev(map[string]string{"alice@example.com": "s3cret"}),
		Directory:       dir,
		MaxMessageBytes: 1 << 20,
		Logger:          slog.New(slog.NewTextHandler(io.Discard, nil)),
		Learn: func(_ context.Context, _ string, isSpam bool, data []byte) {
			calls = append(calls, learnCall{IsSpam: isSpam, Body: string(data)})
		},
	})
	ln, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = ln.Close() })
	go func() { _ = srv.Serve(ln) }()

	client, err := imapclient.DialInsecure(ln.Addr().String(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = client.Logout().Wait() })
	if err := client.Login("alice@example.com", "s3cret").Wait(); err != nil {
		t.Fatal(err)
	}
	return client, &calls
}

type learnCall struct {
	IsSpam bool
	Body   string
}

// TestIMAPLearnMoveToJunk: MOVE INBOX -> Junk trains spam.
func TestIMAPLearnMoveToJunk(t *testing.T) {
	c, calls := startLearnServer(t)
	body := "Subject: spam\r\n\r\njunk me\r\n"
	uid := appendMessage(t, c, "INBOX", body, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Move(imap.UIDSetNum(uid), "Junk").Wait(); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || !(*calls)[0].IsSpam || !strings.Contains((*calls)[0].Body, "junk me") {
		t.Fatalf("learn calls = %+v, want one spam learn with body", *calls)
	}
}

// TestIMAPLearnMoveOutOfJunk: MOVE Junk -> INBOX trains ham.
func TestIMAPLearnMoveOutOfJunk(t *testing.T) {
	c, calls := startLearnServer(t)
	body := "Subject: ham\r\n\r\nnot junk\r\n"
	uid := appendMessage(t, c, "Junk", body, nil)
	if _, err := c.Select("Junk", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Move(imap.UIDSetNum(uid), "INBOX").Wait(); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 2 || !(*calls)[0].IsSpam || (*calls)[1].IsSpam {
		t.Fatalf("learn calls = %+v, want append-spam then move-ham", *calls)
	}
}

// TestIMAPLearnCopyToJunk: COPY INBOX -> Junk trains spam.
func TestIMAPLearnCopyToJunk(t *testing.T) {
	c, calls := startLearnServer(t)
	body := "Subject: copy junk\r\n\r\nx\r\n"
	uid := appendMessage(t, c, "INBOX", body, nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Copy(imap.UIDSetNum(uid), "Junk").Wait(); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 1 || !(*calls)[0].IsSpam {
		t.Fatalf("learn calls = %+v, want one spam learn", *calls)
	}
}

// TestIMAPLearnNoop: moves that do not cross the Junk boundary learn
// nothing (beyond the append into Junk itself).
func TestIMAPLearnNoop(t *testing.T) {
	c, calls := startLearnServer(t)
	uid := appendMessage(t, c, "INBOX", "Subject: a\r\n\r\nb\r\n", nil)
	if _, err := c.Select("INBOX", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if err := c.Create("Archive", nil).Wait(); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Move(imap.UIDSetNum(uid), "Archive").Wait(); err != nil {
		t.Fatal(err)
	}
	if len(*calls) != 0 {
		t.Fatalf("learn calls = %+v, want none", *calls)
	}
}
