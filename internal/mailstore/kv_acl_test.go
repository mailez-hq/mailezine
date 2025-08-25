package mailstore

import (
	"testing"

	"mailezine/internal/store"
)

func TestACLSuiteKV(t *testing.T) {
	aclSuite(t, func(t *testing.T) MailboxStore {
		t.Helper()
		return NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))
	})
}
