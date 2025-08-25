#!/usr/bin/env bash
# RocksDB storage backend e2e: build the engine with -tags rocksdb (cgo,
# grocksdb dist from the module cache) inside the Linux CI environment, then
# run the same send/read/restart/persist loop as the pebble e2e, all in one
# container (no port mapping needed).
#
# Usage: bash deploy/scripts/rocksdb-storage-e2e.sh
set -euo pipefail

export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL="*"
cd "$(dirname "$0")/../.."

VOL=mailezine-rocks-e2e
docker volume rm "$VOL" >/dev/null 2>&1 || true
trap 'docker volume rm "$VOL" >/dev/null 2>&1 || true' EXIT

MODCACHE=$(go env GOMODCACHE | sed 's/\\/\//g')

docker run --rm \
	-v "$(pwd):/src" \
	-v "$MODCACHE:/root/go/pkg/mod:ro" \
	-v mailezine-go-build:/root/.cache/go-build \
	-v "$VOL:/data" \
	mailezine/ci-env:local bash -c '
set -euo pipefail
cd /src

echo "== building rocksdb engine"
go build -tags rocksdb,testing -o /tmp/mailezine-rocks ./cmd/mailezine

start() {
	MAILEZINE_DIRECTORY_MODE=dev \
	MAILEZINE_DIRECTORY_FILE=/src/internal/directory/testdata/dev-directory.json \
	MAILEZINE_AUTH_MODE=dev \
	MAILEZINE_AUTH_DEV_FILE=/src/internal/auth/testdata/dev-passwords.json \
	MAILEZINE_STORAGE_BACKEND=rocksdb \
	MAILEZINE_ROCKS_PATH=/data/rocks \
	MAILEZINE_OUTBOUND_ENABLED=false \
	MAILEZINE_POP3_ADDR=:10110 \
	/tmp/mailezine-rocks >/tmp/mz.log 2>&1 &
	PID=$!
	for i in $(seq 1 30); do
		if (exec 3<>/dev/tcp/127.0.0.1/11480) 2>/dev/null; then
			return 0
		fi
		sleep 1
	done
	echo "rocksdb engine failed to start" >&2
	cat /tmp/mz.log
	exit 1
}

echo "== starting rocksdb engine"
start

echo "== send + read"
SUBJECT="rocksdb-smoke-$(date +%s)"
go run ./cmd/storage-smoke -mode send -smtp 127.0.0.1:1587 -imap 127.0.0.1:1143 -pop3 127.0.0.1:10110 -subject "$SUBJECT"

echo "== restart engine, verify persistence"
kill "$PID"
wait "$PID" 2>/dev/null || true
start
go run ./cmd/storage-smoke -mode check -imap 127.0.0.1:1143 -pop3 127.0.0.1:10110 -subject "$SUBJECT"

kill "$PID" 2>/dev/null || true
echo "== rocksdb storage e2e passed"
'
