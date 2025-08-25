package mailstore

import (
	"testing"

	"mailezine/internal/store"
)

func TestSieveStoreSuiteKV(t *testing.T) {
	sieveStoreSuite(t, func(t *testing.T) SieveStore {
		t.Helper()
		return NewKV(store.New(store.NewMemoryKV(), store.NewMemoryBlob()))
	})
}
