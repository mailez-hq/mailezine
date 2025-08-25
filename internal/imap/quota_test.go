// RFC 2087 QUOTA integration: GETQUOTAROOT/GETQUOTA expose the account
// quota through the client library.
package imap

import (
	"testing"

	"github.com/emersion/go-imap/v2"
)

func TestIMAPQuota(t *testing.T) {
	c, _ := startTestServer(t)
	if !c.Caps().Has(imap.CapQuota) {
		t.Fatal("QUOTA capability not advertised")
	}
	qr, err := c.GetQuotaRoot("INBOX").Wait()
	if err != nil {
		t.Fatal(err)
	}
	if len(qr) != 1 {
		t.Fatalf("quota responses = %d", len(qr))
	}
	quota := qr[0]
	st := quota.Resources[imap.QuotaResourceStorage]
	if st.Limit != 1<<20 {
		t.Fatalf("quota limit = %d, want %d", st.Limit, 1<<20)
	}
	if st.Usage != 0 {
		t.Fatalf("quota used = %d, want 0", st.Usage)
	}
}
