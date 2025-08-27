// Script resolution for the delivery pipeline: an account's locally stored
// active script (ManageSieve) wins; without one the control plane's default
// script (directory contract) applies.
package sieve

import (
	"context"
	"errors"

	"mailezine/internal/directory"
	"mailezine/internal/mailstore"
)

// ScriptSource resolves the active script of an account.
type ScriptSource interface {
	ActiveSieveScript(ctx context.Context, account string) (content string, ok bool, err error)
}

// StaticSource serves one fixed script; used by tests.
type StaticSource string

// ActiveSieveScript returns the fixed script.
func (s StaticSource) ActiveSieveScript(context.Context, string) (string, bool, error) {
	return string(s), s != "", nil
}

var _ ScriptSource = StaticSource("")

// DefaultScriptSource combines the local script store with the directory
// default.
type DefaultScriptSource struct {
	Store     mailstore.SieveStore
	Directory directory.Service
}

// ActiveSieveScript returns the local active script, or the directory
// default when no local script is active.
func (s DefaultScriptSource) ActiveSieveScript(ctx context.Context, account string) (string, bool, error) {
	if s.Store != nil {
		scripts, err := s.Store.ListSieveScripts(ctx, account)
		if err != nil && !errors.Is(err, mailstore.ErrNotFound) {
			return "", false, err
		}
		for _, meta := range scripts {
			if !meta.Active {
				continue
			}
			content, err := s.Store.GetSieveScript(ctx, account, meta.Name)
			if err != nil {
				return "", false, err
			}
			return content, content != "", nil
		}
	}
	script, err := s.Directory.Sieve(ctx, account)
	if err != nil {
		return "", false, err
	}
	return script.Script, script.Script != "", nil
}

var _ ScriptSource = DefaultScriptSource{}
