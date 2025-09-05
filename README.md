<p align="center">
  <img src="branding/mailez-logo.svg" alt="mailezine" width="320">
</p>

<p align="center"><b>English</b> | <a href="README.zh-CN.md">简体中文</a></p>

# mailezine

The official mail engine of mailez (community edition): a single Go binary
mail server providing SMTP send/receive, IMAP/POP3 access, and Sieve
filtering. The protocol stack is built in-house or on MIT-licensed third-
party libraries (go-msgauth/go-imap).

## Features

### Mail protocols

- **SMTP / Submission** — inbound reception, authenticated submission,
  recipient verification, rspamd anti-spam scanning, local delivery
- **Outbound relay** — outbound queue (SRS rewriting, tunable rate limits
  and exponential backoff, automatic RFC 3464 DSN bounces on terminal
  failure); third-party smarthost relay with SASL AUTH support
- **IMAP** — full read path; extensions: CONDSTORE (RFC 7162, incremental
  sync), SORT (RFC 5256, server-side sorting), ACL (RFC 4314, folder
  sharing), ID (RFC 2971); non-ASCII folder names correctly encoded as
  mUTF-7 (RFC 3501)
- **POP3** — optional
- **ManageSieve** (default `:11490`) — LISTSCRIPTS/GETSCRIPT/PUTSCRIPT/
  SETACTIVE/DELETESCRIPT, scripts stored in KV, pairs with the webmail
  filter editor
- **Extended addresses** — `user+tag@example.com` normalized automatically
  on delivery (configurable delimiter, empty string disables)

### Sieve and anti-spam

- The Sieve executor covers every extension the mailez control-plane
  default templates rely on: spamtestplus (X-Spam-Level junk routing),
  editheader, vacation auto-replies (`:days` throttling with persistence,
  null sender, loop prevention), `:index`, regex, date, mailbox
  (`fileinto :create`)
- Delivery prefers the user's own active script and falls back to the
  control-plane default script when absent

### Storage architecture

Dual-plane storage: **KV** carries metadata/mailbox documents/quotas and
**blob** carries message bodies. Both implement the same contract (optimistic
transactions, read-your-writes, conflict replay) and share one contract test
suite (basic semantics / transactions / concurrent races).

| Plane | Backend | Notes |
|---|---|---|
| KV | `pebble` | pure-Go LSM tree, single-node default |
| blob | local FS | default; optional gzip compression |

Upgrading legacy data: `mailezine migrate` migrates existing data from the
traditional multi-process mail stack (maildir) architecture to pebble in one
shot (POSIX platforms, `--dry-run` for a rehearsal).

### Full-text search

The full-text index is enabled by default (embedded bleve, token/prefix
semantics; disable with `MAILEZINE_FTS_ENABLED=false`): delivery and IMAP
operations maintain the index automatically; `SEARCH TEXT` goes through the
index plus original-text verification; indexed content is structured MIME
text (headers + tag-stripped text/plain|html), with optional Apache Tika
extraction for PDF/Office attachments; falls back to full scans when the
index is unavailable.

### Transport security

- STARTTLS/STLS: all four protocols (SMTP/IMAP/POP3/ManageSieve); once
  enabled, plaintext AUTH is always rejected (passwords only on encrypted
  channels)
- Implicit TLS (RFC 8314): SMTPS `:465` / IMAPS `:993` / POP3S `:995` — the
  engine holds its own certificates, mail ports can face the public internet
  directly
- PROXY protocol v1: passes through real client IPs behind gateways/LBs
  (per-port or global)
- Outbound privacy scrubbing: the Received chain and client fingerprint
  headers (User-Agent, X-Mailer, X-Originating-IP, etc.) are stripped
  automatically before sending

### Control-plane integration

- **DKIM vault** — outbound signing keys are pulled from the mailez
  control plane; the engine never stores private keys on disk
- **Directory/auth integration** — recipients and authentication go through
  the mailez control-plane API (a local dev stub substitutes during
  development)

### Operations

- Health endpoint `:11480/health` + management API (`/v1/status`, `/v1/queue`
  retry/cancel, shared-secret auth); storage-backend readiness gate: data
  plane not ready means no listening service ports
- **Cascading purge** — when the control plane deletes a user, engine-side
  mailbox data is purged through the management API using
  `MAIL_ENGINE_MGMT_SECRET`
- Tiered message buffering: small messages stay in memory, large ones spill
  to temp files; outbound cleanup and relaying stream end to end
- `mailezine reindex` rebuilds the full-text index (POSIX)
- `MAILEZINE_CONFIG` points to a toml config file (flat MAILEZINE_* keys;
  environment variables take precedence)

## Quick start (standalone development)

mailezine does not depend on mailez code; during development a local stub
substitutes for the directory contract and authentication:

```powershell
# 1. Prepare the data directory and dev stubs
$env:MAILEZINE_ROCKS_PATH     = "data\kv"
$env:MAILEZINE_DIRECTORY_FILE = "internal/directory/testdata/dev-directory.json"
$env:MAILEZINE_AUTH_DEV_FILE  = "internal/auth/testdata/dev-passwords.json"

# 2. Start (listens on :11480 for health by default)
go run ./cmd/mailezine
# Also listens by default on :1025 (SMTP), :1587 (Submission), :1143 (IMAP),
# :11490 (ManageSieve), :10110 (POP3, optional)

# 3. Verify
curl http://127.0.0.1:11480/health

# 4. End-to-end smoke (local delivery + relay enqueue)
go test ./cmd/mailezine -run TestEndToEnd -v
```

## Configuration

Everything can be overridden via environment variables (defaults in
`internal/config`):

| Variable | Default | Description |
|---|---|---|
| `MAILEZINE_STORAGE_BACKEND` | `pebble` | storage backend (community edition: `pebble`) |
| `MAILEZINE_ROCKS_PATH` | — | pebble data path (required) |
| `MAILEZINE_DIRECTORY_MODE` / `MAILEZINE_AUTH_MODE` | `dev` | use `mailez` in production (control-plane API) |
| `MAILEZINE_BACKEND_ADDRESS` | `127.0.0.1:8080` | mailez control-plane address (directory/auth/DKIM vault) |
| `MAILEZINE_DIRECTORY_FILE` / `MAILEZINE_AUTH_DEV_FILE` | — | dev-mode directory/password stubs |
| `MAILEZINE_HOSTNAME` | local hostname | hostname for EHLO / Authentication-Results |
| `MAILEZINE_RECIPIENT_DELIMITER` | `+` | extended-address delimiter (empty = disable plus addressing) |
| `MAILEZINE_TRUSTED_NETS` | `127.0.0.1/8,::1/128` | gateway subnets (skip auth + allow relay) |
| `MAILEZINE_RSPAMD_URL` | empty | e.g. `http://mail-filter:11333/checkv2`; empty = no scanning |
| `MAILEZINE_DKIM_VAULT_URL` | `http://<control plane>/stack/rspamd/vault` | outbound DKIM key source |
| `MAILEZINE_OUTBOUND_ENABLED` / `MAILEZINE_OUTBOUND_PORT` | `true` / `25` | relay queue switch and delivery port |
| `MAILEZINE_QUEUE_MAX_ATTEMPTS` / `MAILEZINE_QUEUE_BASE_RETRY_SECONDS` / `MAILEZINE_QUEUE_MAX_RETRY_SECONDS` / `MAILEZINE_QUEUE_POLL_INTERVAL_SECONDS` | `10` / `60` / `86400` / `5` | outbound queue retries and polling |
| `MAILEZINE_SMTPS_ADDR` / `MAILEZINE_IMAPS_ADDR` / `MAILEZINE_POP3S_ADDR` | empty | implicit TLS ports (465/993/995), certificates required |
| `MAILEZINE_TLS_CERT_FILE` / `MAILEZINE_TLS_KEY_FILE` | — | STARTTLS certificate (both must be set) |
| `MAILEZINE_PROXY_PROTOCOL` | empty | port list or `all` for parsing PROXY headers |
| `MAILEZINE_PROXY_TRUSTED` | loopback + private CIDRs | peer CIDRs allowed to send PROXY headers; when the LB/gateway faces the public internet, add its IP explicitly to prevent client-IP spoofing |
| `MAILEZINE_FTS_ENABLED` / `MAILEZINE_FTS_TIKA_URL` | `true` / empty | full-text index switch and attachment text extraction |
| `MAILEZINE_BLOB_COMPRESSION` | `false` | blob gzip compression (compatible with legacy uncompressed data) |
| `MAILEZINE_JMAP_ENABLED` | `false` | JMAP interface switch (reserved; not yet implemented, planned for v2) |
| `MAILEZINE_MANAGEMENT_ADDR` / `MAILEZINE_MANAGEMENT_SECRET` | empty | management API listener and shared secret |
| `MAILEZINE_CACHE_SIZE` | `8388608` | IMAP envelope/body-structure cache budget (bytes) |
| `MAILEZINE_META_CACHE_SIZE` | `33554432` | mailbox metadata + directory query cache budget (bytes) |
| `MAILEZINE_AUTH_CACHE_SIZE` | `1048576` | auth-result cache budget (bytes, short TTL) |

## End-to-end verification tools

| Tool | Coverage |
|---|---|
| `deploy/scripts/storage-backend-e2e.sh` | Pebble send/receive + restart persistence regression |
| `deploy/scripts/smoke-container-e2e.sh` | container image build + port protocol-surface contract |
| `deploy/scripts/mailez-backend-e2e.sh` | full mailez control-plane chain (create domain/user → submit → deliver) |
| `cmd/storage-smoke` | storage smoke tool (send/check) |
| `cmd/e2e-mailez` | control-plane-driven e2e client |

## Quality gates

```powershell
make verify   # gofmt / tidy / vet / go test -race / build
```

CI: three GitHub Actions jobs — ubuntu full (`make verify`, including -race
and the community-purity check), windows (gofmt/vet/test/build), and a real
rspamd container e2e (`-tags docker`, SMTP→classification→storage chain).
`deploy/scripts/ci-env` provides a local Linux container environment for the
full verification suite.

## Documentation

- [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md) — architecture design
  (layering, domain model, invariants, consistency, concurrency, security)
