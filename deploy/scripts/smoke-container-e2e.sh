#!/usr/bin/env bash
# Container smoke: build the mailezine image (or reuse mailez/mailezine:local),
# start it with the dev directory/auth stubs and Pebble storage, then verify
# the health endpoint and the protocol port contract (IMAP login + LIST,
# SMTP EHLO capabilities, POP3 CAPA) through the published ports.
#
# Usage: bash deploy/scripts/smoke-container-e2e.sh [--build]
set -euo pipefail

# Git Bash on Windows rewrites /devdata-style container paths; disable it.
export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL="*"

cd "$(dirname "$0")/../.."

IMAGE=mailez/mailezine:local
NAME=mailezine-smoke

if [[ "${1:-}" == "--build" ]]; then
	docker build -t "$IMAGE" .
fi

devdata=$(mktemp -d)
devdata_win=$(cygpath -w "$devdata" 2>/dev/null || echo "$devdata")
trap 'rm -rf "$devdata"; docker rm -f "$NAME" >/dev/null 2>&1 || true' EXIT

cp internal/directory/testdata/dev-directory.json "$devdata/"
cp internal/auth/testdata/dev-passwords.json "$devdata/"

docker rm -f "$NAME" >/dev/null 2>&1 || true
docker run -d --name "$NAME" \
	-p 11480:11480 \
	-p 11025:1025 \
	-p 11587:1587 \
	-p 11143:1143 \
	-p 11190:11490 \
	-p 11010:10110 \
	-e MAILEZINE_DIRECTORY_MODE=dev \
	-e MAILEZINE_DIRECTORY_FILE=/devdata/dev-directory.json \
	-e MAILEZINE_AUTH_MODE=dev \
	-e MAILEZINE_AUTH_DEV_FILE=/devdata/dev-passwords.json \
	-e MAILEZINE_STORAGE_BACKEND=pebble \
	-e MAILEZINE_ROCKS_PATH=/data/rocks \
	-v "$devdata_win:/devdata:ro" \
	"$IMAGE" >/dev/null

echo "== waiting for health"
for i in $(seq 1 30); do
	if curl -fsS http://127.0.0.1:11480/health >/dev/null 2>&1; then
		break
	fi
	sleep 1
	if [[ $i -eq 30 ]]; then
		echo "health check failed" >&2
		docker logs "$NAME"
		exit 1
	fi
done
echo "== health OK"

# IMAP: LOGIN + LIST, assert the greeting advertises ACL.
exec 3<>/dev/tcp/127.0.0.1/11143
read -r -t 10 -u 3 line
echo "IMAP: $line"
[[ "$line" == *"ACL"* && "$line" == *"ID"* ]] || { echo "IMAP greeting missing ACL/ID capability: $line" >&2; exit 1; }
printf 'A1 LOGIN alice@example.com s3cret\r\n' >&3
read -r -t 10 -u 3 line
echo "IMAP: $line"
[[ "$line" == A1\ OK* ]] || { echo "IMAP login failed: $line" >&2; exit 1; }
printf 'A2 LIST "" *\r\n' >&3
read -r -t 10 -u 3 line
echo "IMAP: $line"
[[ "$line" == *LIST*INBOX* ]] || { echo "IMAP LIST missing INBOX: $line" >&2; exit 1; }
read -r -t 10 -u 3 line
echo "IMAP: $line"
printf 'A3 ID NIL\r\n' >&3
read -r -t 10 -u 3 line
echo "IMAP: $line"
[[ "$line" == *ID*mailezine* ]] || { echo "IMAP ID response malformed: $line" >&2; exit 1; }
read -r -t 10 -u 3 line
echo "IMAP: $line"
printf 'A4 LOGOUT\r\n' >&3
exec 3>&- 3<&-

# SMTP: EHLO, assert the advertised capability set.
exec 3<>/dev/tcp/127.0.0.1/11025
read -r -t 10 -u 3 line
echo "SMTP: $line"
printf 'EHLO smoke\r\n' >&3
found=0
while IFS= read -r -t 10 -u 3 line; do
	echo "SMTP: $line"
	[[ "$line" != 250-* ]] && break
	[[ "$line" == *SMTPUTF8* || "$line" == *DSN* ]] && found=1
done
[[ $found -eq 1 ]] || { echo "SMTP EHLO missing SMTPUTF8/DSN" >&2; exit 1; }
printf 'QUIT\r\n' >&3
exec 3>&- 3<&-

# POP3: CAPA.
exec 3<>/dev/tcp/127.0.0.1/11010
read -r -t 10 -u 3 line
echo "POP3: $line"
printf 'CAPA\r\n' >&3
ok=0
while IFS= read -r -t 10 -u 3 line; do
	echo "POP3: $line"
	[[ "$line" == *"UIDL"* ]] && ok=1
	[[ "$line" == "."* ]] && break
done
[[ $ok -eq 1 ]] || { echo "POP3 CAPA missing UIDL" >&2; exit 1; }
printf 'QUIT\r\n' >&3
exec 3>&- 3<&-

echo "== container smoke passed"
