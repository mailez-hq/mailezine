#!/usr/bin/env bash
# Full control-plane e2e: build and start the mailez backend (sqlite +
# local Redis), seed the admin, start mailezine in mailez directory/auth
# mode against it, then run cmd/e2e-mailez (backend API -> SMTP submission
# -> IMAP delivery). This is the MVP switch verification: the same contract
# the gateway proxies must work engine-side.
#
# Usage: bash deploy/scripts/mailez-backend-e2e.sh [path-to-mailez-backend]
# Requires: Redis on localhost:6379 (e.g. the mailez dev compose redis).
set -euo pipefail

export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL="*"
cd "$(dirname "$0")/../.."

BACKEND_DIR="${1:-D:/code/mailez-hq/mailez/backend}"
ROOT="$(pwd)"
DATA="$ROOT/.e2e-data"
DATA_WIN=$(cygpath -w "$DATA" 2>/dev/null || echo "$DATA")
mkdir -p "$DATA"

export MAILEZ_PORT=18081
export REDIS_ADDR=localhost:6379
export DB_DRIVER=sqlite
export DB_DSN="$DATA_WIN/mailez.db"
export MAILEZ_SECRET_KEY=e2e-secret-change-me
export MAILEZ_METRICS_ADDR=:19090

echo "== building backend"
(cd "$BACKEND_DIR" && go build -o "$DATA_WIN/backend-server.exe" ./cmd/server && go build -o "$DATA_WIN/backend-seed.exe" ./cmd/seed)

echo "== seeding admin"
"$DATA_WIN/backend-seed.exe"

echo "== starting backend"
"$DATA_WIN/backend-server.exe" >"$DATA/backend.log" 2>&1 &
BACKEND_PID=$!
trap 'kill $BACKEND_PID 2>/dev/null || true; docker rm -f mailezine-e2e >/dev/null 2>&1 || true' EXIT
for i in $(seq 1 30); do
	curl -fsS "http://127.0.0.1:$MAILEZ_PORT/api/v1/health" >/dev/null 2>&1 && break
	sleep 1
	[[ $i -eq 30 ]] && { echo "backend failed to start" >&2; exit 1; }
done
echo "== backend healthy"

echo "== building mailezine image"
docker build -t mailez/mailezine:local . >/dev/null

echo "== starting mailezine"
docker rm -f mailezine-e2e >/dev/null 2>&1 || true
docker run -d --name mailezine-e2e \
	-p 11480:11480 -p 11025:1025 -p 11587:1587 -p 11143:1143 -p 11190:11490 -p 11010:10110 \
	-e MAILEZINE_DIRECTORY_MODE=mailez \
	-e MAILEZINE_AUTH_MODE=mailez \
	-e MAILEZINE_BACKEND_ADDRESS=host.docker.internal:18081 \
	-e MAILEZINE_STORAGE_BACKEND=pebble \
	-e MAILEZINE_KV_PATH=/data/rocks \
	-e MAILEZINE_OUTBOUND_ENABLED=false \
	-e MAILEZINE_RSPAMD_URL=http://host.docker.internal:11333/checkv2 \
	-e MAILEZINE_HEALTH_ADDR=:11480 \
	-e MAILEZINE_SMTP_ADDR=:1025 \
	-e MAILEZINE_SUBMISSION_ADDR=:1587 \
	-e MAILEZINE_IMAP_ADDR=:1143 \
	-e MAILEZINE_POP3_ADDR=:10110 \
	-e MAILEZINE_MANAGESIEVE_ADDR=:11490 \
	--add-host host.docker.internal:host-gateway \
	mailez/mailezine:local >/dev/null
for i in $(seq 1 30); do
	curl -fsS http://127.0.0.1:11480/health >/dev/null 2>&1 && break
	sleep 1
	[[ $i -eq 30 ]] && { echo "mailezine failed to start" >&2; docker logs mailezine-e2e; exit 1; }
done
echo "== mailezine healthy"

echo "== running e2e-mailez"
go run ./cmd/e2e-mailez \
	-api "http://127.0.0.1:$MAILEZ_PORT" \
	-smtp 127.0.0.1:11587 \
	-imap 127.0.0.1:11143
