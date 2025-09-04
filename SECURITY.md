# Security

## Reporting a vulnerability

Please do **not** open a public issue for security problems. Report privately to
the maintainers (mailto:security@mailez.com) and include:

- affected version / commit
- a minimal reproduction
- impact assessment if known

You should receive an acknowledgement within 72 hours. We follow a 90-day
disclosure window for confirmed issues unless the reporter agrees otherwise.

## Scope

This repository hosts the mail engine (SMTP / IMAP / POP3 / ManageSieve /
DKIM / queue / storage). Protocol-adjacent issues (control plane, webmail,
admin console) belong to the sibling mailez repository; report them through
the same address and mention the affected component.

## Security posture

- All protocol adapters share one connection discipline: bounded line/literal
  sizes, read/write timeouts, TLS-upgrade handling, structured log context
  (session_id); unknown or out-of-order commands get a protocol-correct
  response, never a crash.
- Storage access goes through the store API surface only; adapters never touch
  maildir/KV internals directly.
- Outbound transport defaults to opportunistic TLS with verification;
  DANE and MTA-STS are supported; DKIM signing happens once at enqueue time.
- Secrets are environment-driven with no baked-in defaults for production.

## Supported versions

Only the latest release receives security fixes. Backports are made on request
for the previous minor release.
