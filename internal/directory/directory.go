// Package directory implements the engine-agnostic mailbox directory.
//
// The Service interface mirrors the mailez /stack/directory/* contract
// (mailez/backend/internal/stack/directory.go) so a mailez-backed
// implementation can be dropped in without protocol changes. A 404 in the
// mailez contract maps to ErrNotFound here.
package directory

import (
	"context"
	"errors"
)

// ErrNotFound means "valid request, nothing in the directory for it".
var ErrNotFound = errors.New("directory: not found")

// Service is the directory contract consumed by delivery, submission and
// provisioning paths.
type Service interface {
	User(ctx context.Context, email string) (User, error)
	Domain(ctx context.Context, name string) (Domain, error)
	Aliases(ctx context.Context, addr string) ([]string, error)
	Relay(ctx context.Context, email string) (Relay, error)
	Sender(ctx context.Context, email string) (Sender, error)
	SenderRate(ctx context.Context, sender string) (SenderRate, error)
	SRSForward(ctx context.Context, sender string) (string, error)
	SRSRestore(ctx context.Context, recipient string) (string, error)
	Quota(ctx context.Context, email string) (Quota, error)
	UpdateQuotaUsed(ctx context.Context, email string, used int64) error
	Sieve(ctx context.Context, email string) (SieveScript, error)
	Close() error
}

// User mirrors the /stack/directory/users/:email response.
type User struct {
	Email              string   `json:"email"`
	Enabled            bool     `json:"enabled"`
	QuotaBytes         int64    `json:"quotaBytes"`
	QuotaBytesUsed     int64    `json:"quotaBytesUsed"`
	ForwardEnabled     bool     `json:"forwardEnabled"`
	ForwardKeep        bool     `json:"forwardKeep"`
	ForwardTargets     []string `json:"forwardTargets"`
	RecipientDelimiter string   `json:"recipientDelimiter"`
}

// Domain mirrors /stack/directory/domains/:domain.
type Domain struct {
	IsLocal bool   `json:"isLocal"`
	Name    string `json:"name"`
}

// Relay mirrors /stack/directory/relays/:email.
type Relay struct {
	Domain    string `json:"domain"`
	Transport string `json:"transport"`
}

// Sender mirrors /stack/directory/senders/:email.
type Sender struct {
	Allowed   bool     `json:"allowed"`
	Addresses []string `json:"addresses"`
}

// SenderRate mirrors /stack/directory/senders/:email/rate.
type SenderRate struct {
	Allowed bool   `json:"allowed"`
	Reason  string `json:"reason,omitempty"`
}

// Quota mirrors /stack/directory/quota/:email.
type Quota struct {
	Limit int64 `json:"limit"`
	Used  int64 `json:"used"`
}

// SieveScript mirrors /stack/directory/sieve/:email.
type SieveScript struct {
	Name   string `json:"name"`
	Script string `json:"script"`
}
