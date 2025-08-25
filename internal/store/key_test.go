package store

import (
	"bytes"
	"testing"
)

func TestKeyOrdering(t *testing.T) {
	// Document keys must sort by account, then collection, then document ID.
	keys := [][]byte{
		DocumentKey(1, CollectionMailbox, 2),
		DocumentKey(1, CollectionEmail, 1),
		DocumentKey(2, CollectionMailbox, 1),
		DocumentKey(1, CollectionMailbox, 1),
	}
	// Stable sort by bytes.
	sorted := append([][]byte(nil), keys...)
	for i := 1; i < len(sorted); i++ {
		for j := i; j > 0 && bytes.Compare(sorted[j-1], sorted[j]) > 0; j-- {
			sorted[j-1], sorted[j] = sorted[j], sorted[j-1]
		}
	}
	want := [][]byte{
		DocumentKey(1, CollectionMailbox, 1),
		DocumentKey(1, CollectionMailbox, 2),
		DocumentKey(1, CollectionEmail, 1),
		DocumentKey(2, CollectionMailbox, 1),
	}
	for i := range want {
		if !bytes.Equal(sorted[i], want[i]) {
			t.Fatalf("order mismatch at %d: got %x want %x", i, sorted[i], want[i])
		}
	}
}

func TestCollectionKeyIsPrefixOfDocumentKey(t *testing.T) {
	prefix := CollectionKey(42, CollectionEmail)
	doc := DocumentKey(42, CollectionEmail, 7)
	if !bytes.HasPrefix(doc, prefix) {
		t.Fatalf("document key %x does not start with collection key %x", doc, prefix)
	}
}

func TestAccountKeyDistinctFromOtherSpaces(t *testing.T) {
	if bytes.Equal(AccountKey(1), []byte{SpaceAccount, 0, 0, 0, 1}) == false {
		t.Fatalf("account key encoding changed unexpectedly: %x", AccountKey(1))
	}
}
