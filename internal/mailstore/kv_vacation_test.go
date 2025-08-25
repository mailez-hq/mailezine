package mailstore

import (
	"testing"

	"mailezine/internal/store"
)

func TestVacationSuiteKV(t *testing.T) {
	vacationSuite(t, func(t *testing.T) MailboxStore {
		t.Helper()
		return NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))
	})
}
