// Package limits centralises resource ceilings shared by every protocol and
// pipeline (ARCHITECTURE.md §4.2). It depends only on the standard library.
package limits

import "fmt"

// Config holds every resource limit mailezine enforces.
type Config struct {
	MaxMessageSize int64 // bytes, applied to the whole message including headers
	MaxRecipients  int   // per SMTP transaction / JMAP submission
	MaxConnections int   // per protocol listener
	MaxLineLength  int   // bytes per protocol line
}

// Defaults returns the production defaults.
func Defaults() Config {
	return Config{
		MaxMessageSize: 50 << 20, // 50 MiB
		MaxRecipients:  100,
		MaxConnections: 256,
		MaxLineLength:  1000,
	}
}

// Validate rejects non-positive or otherwise nonsensical limits.
func (c Config) Validate() error {
	if c.MaxMessageSize <= 0 {
		return fmt.Errorf("limits: MaxMessageSize must be positive, got %d", c.MaxMessageSize)
	}
	if c.MaxRecipients <= 0 {
		return fmt.Errorf("limits: MaxRecipients must be positive, got %d", c.MaxRecipients)
	}
	if c.MaxConnections <= 0 {
		return fmt.Errorf("limits: MaxConnections must be positive, got %d", c.MaxConnections)
	}
	if c.MaxLineLength < 256 || c.MaxLineLength > 65536 {
		return fmt.Errorf("limits: MaxLineLength out of range [256, 65536], got %d", c.MaxLineLength)
	}
	return nil
}
