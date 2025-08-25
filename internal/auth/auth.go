// Package auth validates client credentials.
//
// The mailez contract makes /stack/auth/email the single authentication
// authority; engines do not store passwords or decide 2FA/PGP policies
// (ARCHITECTURE.md §6.4). This package defines the seam: Dev implements it
// locally for standalone development; a mailez-backed implementation lands
// in M1.
package auth

import "context"

// Options carries protocol context for a single authentication attempt.
type Options struct {
	Protocol string // smtp|imap|pop3|sieve
	Port     string
	ClientIP string
}

// Service validates credentials and reports whether they are accepted.
type Service interface {
	Authenticate(ctx context.Context, email, password string, opts Options) (bool, error)
	Close() error
}
