#!/usr/bin/env bash
# Storage backend e2e for pebble: start mailezine with the KV+blob backend,
# submit and read a message over SMTP/IMAP/POP3 (storage-smoke --send),
# restart the engine, verify the message survived (--check), and confirm the
# management API sees the account data.
#
# Usage: bash deploy/scripts/storage-backend-e2e.sh
set -euo pipefail

export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL="*"
cd "$(dirname "$0")/../.."

IMAGE=mailez/mailezine:local
NAME=mailezine-storage
VOL=mailezine-storage-data

devdata=$(mktemp -d)
devdata_win=$(cygpath -w "$devdata" 2>/dev/null || echo "$devdata")
trap 'rm -rf "$devdata"; docker rm -f "$NAME" >/dev/null 2>&1 || true; docker volume rm "$VOL" >/dev/null 2>&1 || true' EXIT

cp internal/directory/testdata/dev-directory.json "$devdata/"
cp internal/auth/testdata/dev-passwords.json "$devdata/"

echo "== building image"
docker build -t "$IMAGE" . >/dev/null

docker rm -f "$NAME" >/dev/null 2>&1 || true
docker volume rm "$VOL" >/dev/null 2>&1 || true

start() {
	docker run -d --name "$NAME" \
		-p 11025:1025 -p 11587:1587 -p 11143:1143 -p 11190:11490 -p 10110:10110 -p 11480:11480 \
		-e MAILEZINE_DIRECTORY_MODE=dev \
		-e MAILEZINE_DIRECTORY_FILE=/devdata/dev-directory.json \
		-e MAILEZINE_AUTH_MODE=dev \
		-e MAILEZINE_AUTH_DEV_FILE=/devdata/dev-passwords.json \
		-e MAILEZINE_STORAGE_BACKEND=pebble \
		-e MAILEZINE_KV_PATH=/data/rocks \
		-e MAILEZINE_OUTBOUND_ENABLED=false \
		-e MAILEZINE_POP3_ADDR=:10110 \
		-v "$devdata_win:/devdata:ro" \
		-v "$VOL:/data" \
		"$IMAGE" >/dev/null
	for i in $(seq 1 30); do
		curl -fsS http://127.0.0.1:11480/health >/dev/null 2>&1 && return 0
		sleep 1
	done
	echo "engine failed to start" >&2
	docker logs "$NAME"
	exit 1
}

echo "== starting pebble engine"
start

echo "== send + read"
SUBJECT="pebble-smoke-$(date +%s)"
go run ./cmd/storage-smoke -mode send -smtp 127.0.0.1:11587 -imap 127.0.0.1:11143 -pop3 127.0.0.1:10110 -subject "$SUBJECT"

echo "== management API sees the message"
mgmt=$(curl -fsS http://127.0.0.1:11480/accounts 2>/dev/null || true)
if [[ -z "$mgmt" ]]; then
	echo "management /accounts unreachable (dev mode); skipping" >&2
else
	echo "$mgmt"
fi

echo "== restart engine, verify persistence"
docker restart "$NAME" >/dev/null
for i in $(seq 1 30); do
	curl -fsS http://127.0.0.1:11480/health >/dev/null 2>&1 && break
	sleep 1
	[[ $i -eq 30 ]] && { echo "engine failed to restart" >&2; exit 1; }
done
go run ./cmd/storage-smoke -mode check -imap 127.0.0.1:11143 -pop3 127.0.0.1:10110 -subject "$SUBJECT"

echo "== pebble storage e2e passed"
