# vendored go-sieve (fork of foxcpp/go-sieve)

Upstream anchor: `github.com/foxcpp/go-sieve`
`v0.0.0-20260523221512-9ae51b269e52` (commit `9ae51b269e52`, 2026-05-23) —
the same pseudo-version pinned in the root go.mod, which still supplies the
unmodified `lexer`/`parser` packages. Regenerate/diff against it with
`scripts/vendor-diff.sh gosieve`.

The mailez control plane's default Sieve template requires extensions the
upstream interpreter does not implement (vacation, spamtestplus, editheader,
index, regex, date, mailbox). This directory vendors `foxcpp/go-sieve` (MIT):

- `sieve.go` is the upstream root package (package `sieve`).
- `interp/` is the upstream `interp` package (package `interp`), extended:
  - `mailez.go`: vacation (RFC 5230 subset), spamtestplus (legacy IMAP
    `X-Spam-Level` semantics), editheader (RFC 5293), date/currentdate
    (RFC 5260 subset), mailboxexists/metadataexists (RFC 5490), and the
    regex match type (RFC 5182).
  - `load.go`: extension registry entries for the above.
  - `load_tests.go`/`test.go`: `:index` support on the header test.
  - `load_action.go`/`action.go`/`applied_action.go`: `fileinto :create`
    (RFC 5490).

`lexer`/`parser` remain module dependencies (public packages). Only the
import paths inside `interp/` were rewritten.

Delivery semantics live in `mailezine/internal/delivery`: editheader actions
are applied to the stored copy; vacation replies go out with a null envelope
and an in-memory per-recipient `:days` throttle.

Do not pull upstream changes in blindly: the extension registry is part of
this fork.
