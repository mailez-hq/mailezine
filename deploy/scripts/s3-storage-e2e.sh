#!/usr/bin/env bash
# S3-compatible blob closure: run mailezine with Pebble KV + an S3 blob
# store (the production shape) against RustFS or MinIO, submit/read a
# message over SMTP/IMAP/POP3, verify the message body actually landed in
# the object store, restart the engine and re-read it (persistence across
# restart via S3).
#
# Usage: bash deploy/scripts/s3-storage-e2e.sh [rustfs|minio]   # default rustfs
set -euo pipefail

export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL="*"
cd "$(dirname "$0")/../.."

STORE="${1:-rustfs}"
case "$STORE" in
  rustfs)
    STORE_IMAGE=rustfs/rustfs:latest
    ;;
  minio)
    STORE_IMAGE=minio/minio:latest
    ;;
  *)
    echo "unknown store: $STORE (rustfs|minio)" >&2
    exit 2
    ;;
esac

IMAGE=mailez/mailezine:local
ENGINE=mailezine-s3-engine
STOREC=mailezine-s3-store
STORE_VOL=mailezine-s3-data
KV_VOL=mailezine-s3-kv
ROOT_USER=mailezine
ROOT_PASS=mailezine-e2e-pass
# Host ports are overridable so concurrent stacks (customer demos, the ee
# compose) do not collide with this script.
STORE_PORT="${S3E2E_STORE_PORT:-19000}"
SMTP_PORT="${S3E2E_SMTP_PORT:-11025}"
SUBMIT_PORT="${S3E2E_SUBMIT_PORT:-11587}"
IMAP_PORT="${S3E2E_IMAP_PORT:-11143}"
MTA_PORT="${S3E2E_MTA_PORT:-11190}"
POP3_PORT="${S3E2E_POP3_PORT:-10110}"
MGMT_PORT="${S3E2E_MGMT_PORT:-11480}"

devdata=$(mktemp -d)
devdata_win=$(cygpath -w "$devdata" 2>/dev/null || echo "$devdata")
trap 'rm -rf "$devdata"; docker rm -f "$ENGINE" "$STOREC" >/dev/null 2>&1 || true; docker volume rm "$STORE_VOL" "$KV_VOL" >/dev/null 2>&1 || true' EXIT

cp internal/directory/testdata/dev-directory.json "$devdata/"
cp internal/auth/testdata/dev-passwords.json "$devdata/"

echo "== building image (ee tag: S3 blob lives in internal/ee/storeee)"
docker build -f Dockerfile.ee -t "$IMAGE" . >/dev/null

docker rm -f "$ENGINE" "$STOREC" >/dev/null 2>&1 || true
docker volume rm "$STORE_VOL" "$KV_VOL" >/dev/null 2>&1 || true

# Credential env differs per store; the S3 protocol side is identical.
if [ "$STORE" = "rustfs" ]; then
  CRED_ENV=(-e RUSTFS_ACCESS_KEY="$ROOT_USER" -e RUSTFS_SECRET_KEY="$ROOT_PASS")
  HEALTH_PATH=/health
else
  CRED_ENV=(-e MINIO_ROOT_USER="$ROOT_USER" -e MINIO_ROOT_PASSWORD="$ROOT_PASS")
  HEALTH_PATH=/minio/health/ready
fi

echo "== starting $STORE ($STORE_IMAGE)"
docker run -d --name "$STOREC" \
  -p ${STORE_PORT}:9000 -p $((STORE_PORT+1)):9001 \
  "${CRED_ENV[@]}" \
  -v "$STORE_VOL:/data" \
  "$STORE_IMAGE" server /data --console-address ":9001" >/dev/null
for i in $(seq 1 30); do
  curl -fsS "http://127.0.0.1:${STORE_PORT}$HEALTH_PATH" >/dev/null 2>&1 && break
  sleep 1
  [[ $i -eq 30 ]] && { echo "$STORE failed to start" >&2; docker logs "$STOREC"; exit 1; }
done
echo "== $STORE ready"

echo "== engine probe (s3-probe: CRUD/multipart/list/conditional writes)"
go run ./cmd/s3-probe -endpoint 127.0.0.1:${STORE_PORT} -access "$ROOT_USER" -secret "$ROOT_PASS" -bucket mailezine-s3-probe

echo "== starting engine (pebble KV + $STORE blob)"
docker run -d --name "$ENGINE" \
  -p ${SMTP_PORT}:1025 -p ${SUBMIT_PORT}:1587 -p ${IMAP_PORT}:1143 -p ${MTA_PORT}:11490 -p ${POP3_PORT}:10110 -p ${MGMT_PORT}:11480 \
  -e MAILEZINE_DIRECTORY_MODE=dev \
  -e MAILEZINE_DIRECTORY_FILE=/devdata/dev-directory.json \
  -e MAILEZINE_AUTH_MODE=dev \
  -e MAILEZINE_AUTH_DEV_FILE=/devdata/dev-passwords.json \
  -e MAILEZINE_STORAGE_BACKEND=pebble \
  -e MAILEZINE_KV_PATH=/data/rocks \
  -e MAILEZINE_OUTBOUND_ENABLED=false \
  -e MAILEZINE_POP3_ADDR=:10110 \
  -e MAILEZINE_S3_ENDPOINT=host.docker.internal:${STORE_PORT} \
  -e MAILEZINE_S3_ACCESS_KEY="$ROOT_USER" \
  -e MAILEZINE_S3_SECRET_KEY="$ROOT_PASS" \
  -e MAILEZINE_S3_BUCKET=mailezine \
  -e MAILEZINE_S3_USE_SSL=false \
  --add-host host.docker.internal:host-gateway \
  -v "$devdata_win:/devdata:ro" \
  -v "$KV_VOL:/data" \
  "$IMAGE" >/dev/null
for i in $(seq 1 30); do
  curl -fsS http://127.0.0.1:${MGMT_PORT}/health >/dev/null 2>&1 && break
  sleep 1
  [[ $i -eq 30 ]] && { echo "engine failed to start" >&2; docker logs "$ENGINE"; exit 1; }
done
echo "== engine healthy"

echo "== send + read + verify blob in $STORE"
SUBJECT="s3-smoke-$(date +%s)"
go run ./cmd/storage-smoke -mode send \
  -smtp 127.0.0.1:${SUBMIT_PORT} -imap 127.0.0.1:${IMAP_PORT} -pop3 127.0.0.1:${POP3_PORT} \
  -subject "$SUBJECT" \
  -s3-endpoint 127.0.0.1:${STORE_PORT} -s3-access "$ROOT_USER" -s3-secret "$ROOT_PASS" -s3-bucket mailezine

echo "== restart engine, verify persistence through $STORE"
docker restart "$ENGINE" >/dev/null
for i in $(seq 1 30); do
  curl -fsS http://127.0.0.1:${MGMT_PORT}/health >/dev/null 2>&1 && break
  sleep 1
  [[ $i -eq 30 ]] && { echo "engine failed to restart" >&2; exit 1; }
done
go run ./cmd/storage-smoke -mode check \
  -imap 127.0.0.1:${IMAP_PORT} -pop3 127.0.0.1:${POP3_PORT} -subject "$SUBJECT"

echo "== $STORE blob storage e2e passed"
