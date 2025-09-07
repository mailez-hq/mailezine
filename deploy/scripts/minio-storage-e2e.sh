#!/usr/bin/env bash
# MinIO blob closure: run mailezine with Pebble KV + MinIO/S3 blob (the
# production shape), submit/read a message over SMTP/IMAP/POP3, verify the
# message body actually landed in the object store, restart the engine and
# re-read it (persistence across restart via MinIO).
#
# Usage: bash deploy/scripts/minio-storage-e2e.sh
set -euo pipefail

export MSYS_NO_PATHCONV=1 MSYS2_ARG_CONV_EXCL="*"
cd "$(dirname "$0")/../.."

IMAGE=mailez/mailezine:local
ENGINE=mailezine-minio-engine
MINIO=mailezine-minio
MINIO_VOL=mailezine-minio-data
KV_VOL=mailezine-minio-kv
ROOT_USER=mailezine
ROOT_PASS=mailezine-e2e-pass

devdata=$(mktemp -d)
devdata_win=$(cygpath -w "$devdata" 2>/dev/null || echo "$devdata")
trap 'rm -rf "$devdata"; docker rm -f "$ENGINE" "$MINIO" >/dev/null 2>&1 || true; docker volume rm "$MINIO_VOL" "$KV_VOL" >/dev/null 2>&1 || true' EXIT

cp internal/directory/testdata/dev-directory.json "$devdata/"
cp internal/auth/testdata/dev-passwords.json "$devdata/"

echo "== building image"
docker build -t "$IMAGE" . >/dev/null

docker rm -f "$ENGINE" "$MINIO" >/dev/null 2>&1 || true
docker volume rm "$MINIO_VOL" "$KV_VOL" >/dev/null 2>&1 || true

echo "== starting MinIO"
docker run -d --name "$MINIO" \
	-p 19000:9000 -p 19001:9001 \
	-e MINIO_ROOT_USER="$ROOT_USER" \
	-e MINIO_ROOT_PASSWORD="$ROOT_PASS" \
	-v "$MINIO_VOL:/data" \
	minio/minio:latest server /data >/dev/null
for i in $(seq 1 30); do
	curl -fsS http://127.0.0.1:19000/minio/health/ready >/dev/null 2>&1 && break
	sleep 1
	[[ $i -eq 30 ]] && { echo "minio failed to start" >&2; exit 1; }
done
echo "== MinIO ready"

echo "== starting engine (pebble KV + MinIO blob)"
docker run -d --name "$ENGINE" \
	-p 11025:1025 -p 11587:1587 -p 11143:1143 -p 11190:11490 -p 10110:10110 -p 11480:11480 \
	-e MAILEZINE_DIRECTORY_MODE=dev \
	-e MAILEZINE_DIRECTORY_FILE=/devdata/dev-directory.json \
	-e MAILEZINE_AUTH_MODE=dev \
	-e MAILEZINE_AUTH_DEV_FILE=/devdata/dev-passwords.json \
	-e MAILEZINE_STORAGE_BACKEND=pebble \
	-e MAILEZINE_KV_PATH=/data/rocks \
	-e MAILEZINE_OUTBOUND_ENABLED=false \
	-e MAILEZINE_POP3_ADDR=:10110 \
	-e MAILEZINE_S3_ENDPOINT=host.docker.internal:19000 \
	-e MAILEZINE_S3_ACCESS_KEY="$ROOT_USER" \
	-e MAILEZINE_S3_SECRET_KEY="$ROOT_PASS" \
	-e MAILEZINE_S3_BUCKET=mailezine \
	-e MAILEZINE_S3_USE_SSL=false \
	--add-host host.docker.internal:host-gateway \
	-v "$devdata_win:/devdata:ro" \
	-v "$KV_VOL:/data" \
	"$IMAGE" >/dev/null
for i in $(seq 1 30); do
	curl -fsS http://127.0.0.1:11480/health >/dev/null 2>&1 && break
	sleep 1
	[[ $i -eq 30 ]] && { echo "engine failed to start" >&2; docker logs "$ENGINE"; exit 1; }
done
echo "== engine healthy"

echo "== send + read + verify blob in MinIO"
SUBJECT="minio-smoke-$(date +%s)"
go run ./cmd/storage-smoke -mode send \
	-smtp 127.0.0.1:11587 -imap 127.0.0.1:11143 -pop3 127.0.0.1:10110 \
	-subject "$SUBJECT" \
	-s3-endpoint 127.0.0.1:19000 -s3-access "$ROOT_USER" -s3-secret "$ROOT_PASS" -s3-bucket mailezine

echo "== restart engine, verify persistence through MinIO"
docker restart "$ENGINE" >/dev/null
for i in $(seq 1 30); do
	curl -fsS http://127.0.0.1:11480/health >/dev/null 2>&1 && break
	sleep 1
	[[ $i -eq 30 ]] && { echo "engine failed to restart" >&2; exit 1; }
done
go run ./cmd/storage-smoke -mode check \
	-imap 127.0.0.1:11143 -pop3 127.0.0.1:10110 -subject "$SUBJECT"

echo "== MinIO blob storage e2e passed"
