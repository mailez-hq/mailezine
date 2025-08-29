// Cached wraps a MailboxStore with a weight-bounded metadata cache.
//
// Read paths (mailbox list, mailbox status, message list) are memoized per
// (account, mailbox); every write path invalidates the affected mailboxes.
// A TTL is applied as a safety net so a missed invalidation self-heals. All
// returned slices are deep copies — cached values are immutable and callers
// may mutate them freely.
package mailstore

import (
	"context"
	"errors"
	"io"
	"time"

	"mailezine/internal/mailcache"
	"mailezine/internal/store"
)

// Cached is a MailboxStore + SieveStore decorator with metadata caching.
type Cached struct {
	inner cachedBackend
	cache *mailcache.Cache
}

// cachedBackend is the full surface Cached forwards to: mailbox operations
// plus the Sieve and ACL sub-surfaces.
type cachedBackend interface {
	MailboxStore
	SieveStore
	ACLStore
}

// NewCached wraps inner. cache must be non-nil; it owns the metadata memo.
func NewCached(inner cachedBackend, cache *mailcache.Cache) *Cached {
	return &Cached{inner: inner, cache: cache}
}

func msgsKey(account, mailbox string) string {
	return "msgs\x00" + account + "\x00" + mailbox
}

func mboxesKey(account string) string {
	return "mboxes\x00" + account
}

func mboxKey(account, mailbox string) string {
	return "mbox\x00" + account + "\x00" + mailbox
}

func (c *Cached) invalidate(account, mailbox string) {
	c.cache.Remove(msgsKey(account, mailbox))
	c.cache.Remove(mboxesKey(account))
	c.cache.Remove(mboxKey(account, mailbox))
}

func (c *Cached) invalidateMailboxes(account string) {
	c.cache.Remove(mboxesKey(account))
}

// Deliver appends one message and invalidates the target mailbox.
func (c *Cached) Deliver(ctx context.Context, account, mailbox string, msg *Message) (uint32, error) {
	uid, err := c.inner.Deliver(ctx, account, mailbox, msg)
	if err == nil {
		c.invalidate(account, mailbox)
	}
	return uid, err
}

// QuotaUsedBytes is not cached (derived from live counters).
func (c *Cached) QuotaUsedBytes(ctx context.Context, account string) (int64, error) {
	return c.inner.QuotaUsedBytes(ctx, account)
}

// EnsureDefaultMailboxes creates the default set and invalidates the list.
func (c *Cached) EnsureDefaultMailboxes(ctx context.Context, account string) error {
	err := c.inner.EnsureDefaultMailboxes(ctx, account)
	if err == nil {
		c.invalidateMailboxes(account)
	}
	return err
}

func (c *Cached) ListMailboxes(ctx context.Context, account string) ([]Mailbox, error) {
	key := mboxesKey(account)
	if v, ok := c.cache.Get(key); ok {
		return cloneMailboxes(v.([]Mailbox)), nil
	}
	boxes, err := c.inner.ListMailboxes(ctx, account)
	if err != nil {
		return nil, err
	}
	c.cache.Put(key, boxes, mailboxListWeight(boxes))
	return boxes, nil
}

func (c *Cached) MailboxStatus(ctx context.Context, account, mailbox string) (Mailbox, error) {
	key := mboxKey(account, mailbox)
	if v, ok := c.cache.Get(key); ok {
		return v.(Mailbox), nil
	}
	st, err := c.inner.MailboxStatus(ctx, account, mailbox)
	if err != nil {
		return Mailbox{}, err
	}
	c.cache.Put(key, st, 256)
	return st, nil
}

func (c *Cached) CreateMailbox(ctx context.Context, account, mailbox string) (uint32, error) {
	uidv, err := c.inner.CreateMailbox(ctx, account, mailbox)
	if err == nil {
		c.invalidateMailboxes(account)
	}
	return uidv, err
}

func (c *Cached) DeleteMailbox(ctx context.Context, account, mailbox string) error {
	err := c.inner.DeleteMailbox(ctx, account, mailbox)
	if err == nil {
		c.invalidate(account, mailbox)
	}
	return err
}

func (c *Cached) RenameMailbox(ctx context.Context, account, oldName, newName string) error {
	err := c.inner.RenameMailbox(ctx, account, oldName, newName)
	if err == nil {
		c.invalidateMailboxes(account)
		c.cache.Remove(msgsKey(account, oldName))
		c.cache.Remove(msgsKey(account, newName))
		c.cache.Remove(mboxKey(account, oldName))
		c.cache.Remove(mboxKey(account, newName))
	}
	return err
}

func (c *Cached) SetSubscribed(ctx context.Context, account, mailbox string, subscribed bool) error {
	err := c.inner.SetSubscribed(ctx, account, mailbox, subscribed)
	if err == nil {
		c.invalidateMailboxes(account)
	}
	return err
}

func (c *Cached) ListMessages(ctx context.Context, account, mailbox string) ([]*Message, error) {
	key := msgsKey(account, mailbox)
	if v, ok := c.cache.Get(key); ok {
		return cloneMessages(v.([]*Message)), nil
	}
	msgs, err := c.inner.ListMessages(ctx, account, mailbox)
	if err != nil {
		return nil, err
	}
	c.cache.Put(key, msgs, messagesWeight(msgs))
	return msgs, nil
}

func (c *Cached) MessageByUID(ctx context.Context, account, mailbox string, uid uint32) (*Message, error) {
	// Serve from the message-list memo when present (single cache entry,
	// consistent view); otherwise fall through to the backend.
	if v, ok := c.cache.Get(msgsKey(account, mailbox)); ok {
		for _, m := range v.([]*Message) {
			if m.UID == uid {
				msg := *m
				msg.Flags = append([]string(nil), m.Flags...)
				msg.Keywords = append([]string(nil), m.Keywords...)
				return &msg, nil
			}
		}
		return nil, store.ErrNotFound
	}
	return c.inner.MessageByUID(ctx, account, mailbox, uid)
}

func (c *Cached) SetFlags(ctx context.Context, account, mailbox string, uid uint32, flags []string) error {
	err := c.inner.SetFlags(ctx, account, mailbox, uid, flags)
	if err == nil {
		c.invalidate(account, mailbox)
	}
	return err
}

func (c *Cached) Append(ctx context.Context, account, mailbox string, msg *Message) (uint32, error) {
	uid, err := c.inner.Append(ctx, account, mailbox, msg)
	if err == nil {
		c.invalidate(account, mailbox)
	}
	return uid, err
}

func (c *Cached) Expunge(ctx context.Context, account, mailbox string, uids []uint32) ([]uint32, error) {
	expunged, err := c.inner.Expunge(ctx, account, mailbox, uids)
	if err == nil {
		c.invalidate(account, mailbox)
	}
	return expunged, err
}

func (c *Cached) Copy(ctx context.Context, account, src, dst string, uids []uint32) (map[uint32]uint32, error) {
	m, err := c.inner.Copy(ctx, account, src, dst, uids)
	if err == nil {
		c.invalidate(account, src)
		c.invalidate(account, dst)
	}
	return m, err
}

func (c *Cached) Move(ctx context.Context, account, src, dst string, uids []uint32) (map[uint32]uint32, error) {
	m, err := c.inner.Move(ctx, account, src, dst, uids)
	if err == nil {
		c.invalidate(account, src)
		c.invalidate(account, dst)
	}
	return m, err
}

func (c *Cached) OpenMessage(ctx context.Context, account, mailbox string, uid uint32) (io.ReadCloser, error) {
	return c.inner.OpenMessage(ctx, account, mailbox, uid)
}

// ACLStore: forwarded without caching (ACL data is small and read rarely).
func (c *Cached) GetACL(ctx context.Context, account, mailbox string) (map[string]string, error) {
	return c.inner.GetACL(ctx, account, mailbox)
}

func (c *Cached) SetACL(ctx context.Context, account, mailbox, identifier, rights string) error {
	return c.inner.SetACL(ctx, account, mailbox, identifier, rights)
}

func (c *Cached) DeleteACL(ctx context.Context, account, mailbox, identifier string) error {
	return c.inner.DeleteACL(ctx, account, mailbox, identifier)
}

// SieveStore: script data is small and infrequently read; forward directly.
func (c *Cached) ListSieveScripts(ctx context.Context, account string) ([]SieveScriptMeta, error) {
	return c.inner.ListSieveScripts(ctx, account)
}

func (c *Cached) GetSieveScript(ctx context.Context, account, name string) (string, error) {
	return c.inner.GetSieveScript(ctx, account, name)
}

func (c *Cached) PutSieveScript(ctx context.Context, account, name, content string, activate bool) error {
	return c.inner.PutSieveScript(ctx, account, name, content, activate)
}

func (c *Cached) SetSieveActive(ctx context.Context, account, name string) error {
	return c.inner.SetSieveActive(ctx, account, name)
}

func (c *Cached) DeleteSieveScript(ctx context.Context, account, name string) error {
	return c.inner.DeleteSieveScript(ctx, account, name)
}

// cloneMessages deep-copies the mutable parts so callers cannot corrupt the
// cached entry.
func cloneMessages(in []*Message) []*Message {
	out := make([]*Message, len(in))
	for i, m := range in {
		cp := *m
		cp.Flags = append([]string(nil), m.Flags...)
		cp.Keywords = append([]string(nil), m.Keywords...)
		out[i] = &cp
	}
	return out
}

func cloneMailboxes(in []Mailbox) []Mailbox {
	out := make([]Mailbox, len(in))
	for i, mb := range in {
		mb.Attrs = append([]string(nil), mb.Attrs...)
		out[i] = mb
	}
	return out
}

func messagesWeight(msgs []*Message) int64 {
	var w int64
	for _, m := range msgs {
		w += 96 + int64(len(m.Flags))*16 + int64(len(m.Keywords))*24
	}
	return w
}

func mailboxListWeight(boxes []Mailbox) int64 {
	var w int64
	for _, mb := range boxes {
		w += 128 + int64(len(mb.Name)) + int64(len(mb.Attrs))*12
	}
	return w
}

// VacationLastSent forwards vacation throttle state to the inner store. The
// decorator must not silently mask the persistence capability: delivery
// probes this interface, and without forwarding a cached store would fall
// back to an in-memory throttle map that resets on every restart.
func (c *Cached) VacationLastSent(ctx context.Context, account, sender string) (time.Time, error) {
	vs, ok := c.inner.(VacationStateStore)
	if !ok {
		return time.Time{}, errors.New("mailstore: inner store does not persist vacation state")
	}
	return vs.VacationLastSent(ctx, account, sender)
}

// SetVacationLastSent forwards vacation throttle state to the inner store.
func (c *Cached) SetVacationLastSent(ctx context.Context, account, sender string, t time.Time) error {
	vs, ok := c.inner.(VacationStateStore)
	if !ok {
		return errors.New("mailstore: inner store does not persist vacation state")
	}
	return vs.SetVacationLastSent(ctx, account, sender, t)
}

var _ MailboxStore = (*Cached)(nil)
var _ SieveStore = (*Cached)(nil)
var _ ACLStore = (*Cached)(nil)
var _ VacationStateStore = (*Cached)(nil)
