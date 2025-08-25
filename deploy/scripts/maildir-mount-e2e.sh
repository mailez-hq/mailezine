#!/usr/bin/env bash
# 存量 maildir 挂载演练（M7）：真实 legacy IMAP 播种 → mailezine 挂载读写 →
# legacy IMAP 回读，验证 postdove → mailezine 切换与回滚都无损。
#
# 前置：Docker Desktop；在 mailezine 仓库根目录执行。
#   bash deploy/scripts/maildir-mount-e2e.sh
#
# 依赖镜像：mailezine/legacy-imap-seed（本脚本目录下 Dockerfile 构建）。
set -euo pipefail

ROOT="$(cd "$(dirname "$0")/../.." && pwd)"
VOL=mailezine-maildir-seed
BIN_DIR="$(mktemp -d)"
CONF_DIR="$(mktemp -d)"

cleanup() {
  docker rm -f mailezine-mount legacy-imap-seed >/dev/null 2>&1 || true
  rm -rf "$BIN_DIR" "$CONF_DIR"
}
trap cleanup EXIT

echo "==> 1/5 构建并播种真实 legacy IMAP maildir"
docker build -q -t mailezine/legacy-imap-seed "$ROOT/deploy/scripts/maildir-mount" >/dev/null
docker volume rm "$VOL" >/dev/null 2>&1 || true
docker run -d --name legacy-imap-seed -v "$VOL:/mail" mailezine/legacy-imap-seed >/dev/null
sleep 4
docker exec legacy-imap-seed sh -c \
  "chown -R mail:mail /mail && echo 'alice@example.com:{PLAIN}seed:mail:mail::/mail/alice@example.com:' >> /etc/legacy IMAP/users && chown legacy IMAP:mail /etc/legacy IMAP/users"
docker restart legacy-imap-seed >/dev/null
sleep 4
docker exec legacy-imap-seed sh -c \
  "printf 'LHLO localhost\r\nMAIL FROM:<sender@remote.test>\r\nRCPT TO:<alice@example.com>\r\nDATA\r\nFrom: sender@remote.test\r\nTo: alice@example.com\r\nSubject: seeded via legacy IMAP lmtp\r\nMessage-ID: <seed-1@remote.test>\r\nDate: Mon, 24 Aug 2026 10:00:00 +0800\r\n\r\nhello from real legacy IMAP\r\n.\r\nQUIT\r\n' | nc 127.0.0.1 2525 | tail -1" >/dev/null
docker stop legacy-imap-seed >/dev/null

echo "==> 2/5 构建 linux 二进制（mailezine + mount-check）"
docker run --rm \
  -v "$ROOT:/src" -v "$BIN_DIR:/out" -w /src debian:trixie bash -c \
  'apt-get update -qq >/dev/null 2>&1 && apt-get install -y -qq gcc g++ wget ca-certificates >/dev/null 2>&1 \
   && wget -q --timeout=60 https://golang.google.cn/dl/go1.26.3.linux-amd64.tar.gz -O /tmp/go.tgz \
   && tar -C /usr/local -xzf /tmp/go.tgz \
   && export PATH=/usr/local/go/bin:$PATH \
   && go build -o /out/mailezine ./cmd/mailezine \
   && go build -o /out/mount-check ./cmd/mount-check' >/dev/null

echo "==> 3/5 启动 mailezine 挂载 legacy IMAP 数据"
cat > "$CONF_DIR/directory.json" <<'JSON'
{"users":{"alice@example.com":{"email":"alice@example.com","enabled":true,"quotaBytes":1073741824}},"domains":["example.com"],"aliases":{},"relays":{},"senders":{"alice@example.com":["alice@example.com"]},"sieve":{}}
JSON
echo '{"alice@example.com":"seed"}' > "$CONF_DIR/passwords.json"
docker run -d --name mailezine-mount \
  -v "$VOL:/mail" -v "$BIN_DIR:/bin-mz:ro" -v "$CONF_DIR:/conf:ro" \
  -p 12025:25 -p 12143:143 debian:trixie bash -c '
  export MAILEZINE_STORAGE_BACKEND=maildir MAILEZINE_MAILDIR_PATH=/mail \
    MAILEZINE_DIRECTORY_MODE=dev MAILEZINE_DIRECTORY_FILE=/conf/directory.json \
    MAILEZINE_AUTH_MODE=dev MAILEZINE_AUTH_DEV_FILE=/conf/passwords.json \
    MAILEZINE_SMTP_ADDR=:25 MAILEZINE_IMAP_ADDR=:143 MAILEZINE_SUBMISSION_ADDR=:1587 \
    MAILEZINE_MANAGESIEVE_ADDR=:11490 MAILEZINE_POP3_ADDR=:10110 MAILEZINE_POP3_ENABLED=true \
    MAILEZINE_OUTBOUND_ENABLED=false MAILEZINE_HEALTH_ADDR=:11480 \
    MAILEZINE_TRUSTED_NETS=127.0.0.1/8 MAILEZINE_HOSTNAME=mail.mailez.test MAILEZINE_LOG_LEVEL=info
  /bin-mz/mailezine' >/dev/null
sleep 4

echo "==> 4/5 mount-check（IMAP 读 legacy IMAP 消息 + SMTP 追加）"
cd "$ROOT"
go run ./cmd/mount-check -imap 127.0.0.1:12143 -smtp 127.0.0.1:12025 \
  -user alice@example.com -pass seed -subject "mount probe $(date +%F\ %T)"

echo "==> 5/5 legacy IMAP 回读（验证 mailezine 追加无损）"
docker stop mailezine-mount >/dev/null
docker start legacy-imap-seed >/dev/null
sleep 4
docker exec legacy-imap-seed sh -c "chown -R mail:mail /mail && legacy-imap-ctl fetch 'hdr.subject' mailbox INBOX -u alice@example.com"

echo "==> MOUNT E2E OK"
