# mailezine capabilities

mailezine is a single-binary mail engine: SMTP (inbound + submission),
IMAP, POP3 and ManageSieve on top of an embedded KV+blob storage pair.

| Capability | Notes |
|---|---|
| SMTP inbound/submission, IMAP, POP3, ManageSieve | protocol listeners per config |
| Sieve filtering engine | full RFC 5228 core + extensions used by the UI |
| Full-text search incl. CJK bigram analyzer | embedded bleve index; optional Apache Tika attachment extraction |
| Pebble KV + local FS blob storage | single-node default pair |
| Pluggable storage backends | opener registry (`store.RegisterKVOpener` / `SetS3BlobOpener`); add-on backend packages register themselves at init |
| SPF/DKIM/DMARC verification | Authentication-Results headers on inbound mail |
| Built-in baseline spam classifier | authentication-results + DNSBL + sender lists; lenient posture |
| rspamd inbound scanning | `/checkv2` verdicts, Junk headers/reject when configured |
| Outbound queue | retry with backoff, DSNs, MTA-STS |
| Delivery receipts, snooze wake-up sweeper | control-plane push/webhook/SSE |
| Quotas / limits, backup export+import, migrate/reindex | `mailezine` sub-commands |
| Health / metrics / management API | `/health`, Prometheus, `/v1/status` + queue/account operations |
| Multi-active cluster | per-message queue claims + fenced outcomes, cross-node singleton leases, per-node FTS convergence, advisory per-account write gate — requires a transactional KV backend and a shared blob store |

Degrade, never wedge: features whose add-on backend is not linked in this
build log a "not available in this build" warning and the engine keeps
running on its default path.
