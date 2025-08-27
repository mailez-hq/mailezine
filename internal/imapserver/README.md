# vendored imapserver (fork of go-imap/v2)

This package is `github.com/emersion/go-imap/v2/imapserver` v2.0.0-beta.8
(MIT, see [LICENSE](LICENSE)), vendored and extended for mailezine:

Upstream anchor: `v2.0.0-beta.8` — the same tag pinned in the root go.mod.
Diff local changes with `scripts/vendor-diff.sh imapserver`.

- `SessionExtension` + `ExtensionWriter` (`extension.go`): a minimal hook
  that dispatches commands not covered by the upstream session interface
  (e.g. RFC 4314 ACL). `Conn` hands unknown authenticated-state commands to
  the session extension before rejecting them, and advertises `ACL` when the
  session supports it.
- `internal/imapwire`, `internal/imapnum`, `internal/utf7`, `internal/gimap`
  are the upstream `internal/` packages vendored alongside it (Go's internal
  package rule forbids importing them from another module). Only the import
  paths changed.
- `message.go`: upstream struct literals rewritten to keyed form so
  `go vet` passes under this module's vet settings.

Do not pull in upstream changes blindly: review against the extension hook
when upgrading.
