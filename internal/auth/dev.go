// Dev is a development-only credential validator (PLAN.md §4.4). Passwords
// are plaintext test values loaded from a JSON file; never use in production.
package auth

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"os"
)

// Dev implements Service with a static email→password map.
type Dev struct {
	passwords map[string]string
}

// NewDev returns a dev auth service from a password map.
func NewDev(passwords map[string]string) *Dev {
	return &Dev{passwords: passwords}
}

// LoadDevFile loads a JSON object mapping email to password.
func LoadDevFile(path string) (*Dev, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var passwords map[string]string
	if err := json.Unmarshal(raw, &passwords); err != nil {
		return nil, err
	}
	return NewDev(passwords), nil
}

func (d *Dev) Authenticate(_ context.Context, email, password string, _ Options) (bool, error) {
	want, ok := d.passwords[email]
	if !ok {
		return false, nil
	}
	// Constant-time compare even for dev mode: the seam must not teach
	// callers timing habits that would leak into the mailez-backed mode.
	return subtle.ConstantTimeCompare([]byte(want), []byte(password)) == 1, nil
}

func (d *Dev) Close() error { return nil }

var _ Service = (*Dev)(nil)
