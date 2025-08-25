// Sieve script storage, part of the mailbox surface: scripts are per-account
// documents managed through ManageSieve and consumed by the delivery
// pipeline. The mailez control plane renders a default script for new
// accounts; once a user saves a script locally it becomes the active filter
// (per-account local storage with a directory-provided default).
package mailstore

import (
	"context"
	"errors"

	"mailezine/internal/store"
)

// SieveScriptMeta is one stored script's metadata.
type SieveScriptMeta struct {
	Name   string
	Active bool
}

// SieveStore is the per-account script surface. Implementations: KV
// (CollectionSieveScript documents) and maildir (sidecar).
type SieveStore interface {
	ListSieveScripts(ctx context.Context, account string) ([]SieveScriptMeta, error)
	GetSieveScript(ctx context.Context, account, name string) (string, error)
	PutSieveScript(ctx context.Context, account, name, content string, activate bool) error
	SetSieveActive(ctx context.Context, account, name string) error
	DeleteSieveScript(ctx context.Context, account, name string) error
}

// Sieve script document fields (CollectionSieveScript).
const (
	scriptFieldName    byte = 1
	scriptFieldActive  byte = 2
	scriptFieldContent byte = 3
)

// KV implementation.

// ListSieveScripts returns stored scripts with their active flag.
func (k *KV) ListSieveScripts(ctx context.Context, account string) ([]SieveScriptMeta, error) {
	acctID, err := k.s.AccountByEmail(ctx, account)
	if errors.Is(err, store.ErrNotFound) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	ids, err := k.s.ListDocumentIDs(ctx, acctID, store.CollectionSieveScript)
	if err != nil {
		return nil, err
	}
	var out []SieveScriptMeta
	for _, id := range ids {
		fields, err := k.s.GetDocumentFields(ctx, acctID, store.CollectionSieveScript, id)
		if err != nil {
			return nil, err
		}
		out = append(out, SieveScriptMeta{
			Name:   string(fields[scriptFieldName]),
			Active: len(fields[scriptFieldActive]) == 1 && fields[scriptFieldActive][0] == 1,
		})
	}
	return out, nil
}

// GetSieveScript returns one script's content.
func (k *KV) GetSieveScript(ctx context.Context, account, name string) (string, error) {
	acctID, err := k.s.AccountByEmail(ctx, account)
	if err != nil {
		return "", err
	}
	docID, err := k.sieveScriptDocID(ctx, acctID, name)
	if err != nil {
		return "", err
	}
	fields, err := k.s.GetDocumentFields(ctx, acctID, store.CollectionSieveScript, docID)
	if err != nil {
		return "", err
	}
	return string(fields[scriptFieldContent]), nil
}

// PutSieveScript stores a script, optionally activating it.
func (k *KV) PutSieveScript(ctx context.Context, account, name, content string, activate bool) error {
	acctID, err := k.ensureAccount(ctx, account)
	if err != nil {
		return err
	}
	docID, err := k.sieveScriptDocID(ctx, acctID, name)
	if errors.Is(err, store.ErrNotFound) {
		docID, err = k.s.CreateDocument(ctx, acctID, store.CollectionSieveScript)
	}
	if err != nil {
		return err
	}
	active := byte(0)
	if activate {
		active = 1
	}
	if activate {
		if err := k.deactivateSieveScripts(ctx, acctID); err != nil {
			return err
		}
	}
	return k.s.PutDocumentFields(ctx, acctID, store.CollectionSieveScript, docID, map[byte][]byte{
		scriptFieldName:    []byte(name),
		scriptFieldActive:  []byte{active},
		scriptFieldContent: []byte(content),
	})
}

// SetSieveActive activates a stored script ("" deactivates).
func (k *KV) SetSieveActive(ctx context.Context, account, name string) error {
	acctID, err := k.s.AccountByEmail(ctx, account)
	if err != nil {
		return err
	}
	if err := k.deactivateSieveScripts(ctx, acctID); err != nil {
		return err
	}
	if name == "" {
		return nil
	}
	docID, err := k.sieveScriptDocID(ctx, acctID, name)
	if err != nil {
		return err
	}
	return k.s.UpdateDocumentAtomically(ctx, acctID, store.CollectionSieveScript, docID,
		map[byte][]byte{scriptFieldActive: []byte{1}})
}

// DeleteSieveScript removes a script (active pointer cleared by the caller
// activating "" first).
func (k *KV) DeleteSieveScript(ctx context.Context, account, name string) error {
	acctID, err := k.s.AccountByEmail(ctx, account)
	if err != nil {
		return err
	}
	docID, err := k.sieveScriptDocID(ctx, acctID, name)
	if err != nil {
		return err
	}
	return k.s.DeleteDocument(ctx, acctID, store.CollectionSieveScript, docID)
}

func (k *KV) deactivateSieveScripts(ctx context.Context, acctID store.AccountID) error {
	ids, err := k.s.ListDocumentIDs(ctx, acctID, store.CollectionSieveScript)
	if err != nil {
		return err
	}
	for _, id := range ids {
		fields, err := k.s.GetDocumentFields(ctx, acctID, store.CollectionSieveScript, id)
		if err != nil {
			return err
		}
		if len(fields[scriptFieldActive]) == 1 && fields[scriptFieldActive][0] == 1 {
			if err := k.s.UpdateDocumentAtomically(ctx, acctID, store.CollectionSieveScript, id,
				map[byte][]byte{scriptFieldActive: []byte{0}}); err != nil {
				return err
			}
		}
	}
	return nil
}

func (k *KV) sieveScriptDocID(ctx context.Context, acctID store.AccountID, name string) (uint64, error) {
	ids, err := k.s.ListDocumentIDs(ctx, acctID, store.CollectionSieveScript)
	if err != nil {
		return 0, err
	}
	for _, id := range ids {
		fields, err := k.s.GetDocumentFields(ctx, acctID, store.CollectionSieveScript, id)
		if err != nil {
			return 0, err
		}
		if string(fields[scriptFieldName]) == name {
			return id, nil
		}
	}
	return 0, store.ErrNotFound
}

var _ SieveStore = (*KV)(nil)
