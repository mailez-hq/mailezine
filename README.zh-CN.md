<p align="center">
  <img src="branding/mailez-logo.svg" alt="mailezine" width="320">
</p>

<p align="center"><a href="README.md">English</a> | <b>简体中文</b></p>

# mailezine

mailez 官方邮件引擎（社区版）：单 Go 二进制邮件服务器，提供 SMTP 收发、
IMAP/POP3 存取与 Sieve 过滤。协议栈自研或采用 MIT 许可的第三方库
（go-msgauth/go-imap）。

## 功能特性

### 邮件协议

- **SMTP / Submission** — 入站接收、认证提交、收件人校验、rspamd 反垃圾
  扫描、本地投递
- **出站中继** — 出站队列（SRS 改写、限速、指数退避可调、终态失败自动回
  RFC 3464 DSN 退信）；第三方 smarthost 中继支持 SASL AUTH
- **IMAP** — 完整读路径；扩展：CONDSTORE（RFC 7162，增量同步）、SORT
  （RFC 5256，服务端排序）、ACL（RFC 4314，文件夹共享）、ID（RFC 2971）；
  非 ASCII 文件夹名按 mUTF-7（RFC 3501）正确编解码
- **POP3** — 可选开启
- **ManageSieve**（默认 `:11490`）— LISTSCRIPTS/GETSCRIPT/PUTSCRIPT/
  SETACTIVE/DELETESCRIPT，脚本存 KV，配 webmail 筛子编辑器
- **扩展地址** — `user+tag@example.com` 自动归一投递（分隔符可配，空串禁用）

### Sieve 与反垃圾

- Sieve 执行器覆盖 mailez 控制面默认模板的全部依赖扩展：spamtestplus
  （X-Spam-Level 分流 Junk）、editheader、vacation 自动回复（`:days` 节流
  持久化、null 发件人、防循环）、`:index`、regex、date、mailbox
  （`fileinto :create`）
- 投递优先本地用户活动脚本，无则回退控制面默认脚本

### 存储架构

双面存储：**KV** 承载元数据/邮箱文档/配额，**blob** 承载消息正文，
均实现同一契约（乐观事务、read-your-writes、冲突重放）并共享一套契约
测试套件（基础语义 / 事务 / 并发竞态）。

| 面 | 后端 | 说明 |
|---|---|---|
| KV | `pebble` | 纯 Go LSM，单机默认 |
| blob | 本地 FS | 默认；gzip 压缩可选 |

存量数据升级：`mailezine migrate` 将传统多进程邮件栈（maildir）架构的
存量数据一次性迁移到 pebble（POSIX 平台，`--dry-run` 预演）。

### 全文搜索

全文索引默认启用（bleve 内嵌，词/前缀语义；`MAILEZINE_FTS_ENABLED=false`
可关闭）：投递与
IMAP 操作自动维护索引；`SEARCH TEXT` 走索引 + 原文验证；索引内容为 MIME
结构化文本（头 + text/plain|html 去标签），可选 Apache Tika 提取 PDF/Office
附件文本；索引不可用时回退全量扫描。

### 传输安全

- STARTTLS/STLS：SMTP/IMAP/POP3/ManageSieve 四协议；启用后一律拒绝明文
  AUTH（口令只走加密信道）
- 隐式 TLS（RFC 8314）：SMTPS `:465` / IMAPS `:993` / POP3S `:995`，引擎
  自持证书，邮件端口可直连公网
- PROXY protocol v1：网关/LB 前置时透传真实客户端 IP（按端口或全局开启）
- 出站隐私清理：出站前自动剥离 Received 链与客户端指纹头（User-Agent、
  X-Mailer、X-Originating-IP 等）

### 控制面集成

- **DKIM vault** — 出站签名密钥从 mailez 控制面统一拉取，引擎不落盘私钥
- **目录/认证对接** — 收件人与认证走 mailez 控制面 API（开发期可用本地
  dev 桩替代）

### 运维管理

- 健康端点 `:11480/health` + 管理 API（`/v1/status`、`/v1/queue` 重试/取消，
  共享密钥认证）；存储后端就绪门禁：数据面未就绪不对外监听服务端口
- **级联清除** — 控制面删除用户时，经管理 API 按 `MAIL_ENGINE_MGMT_SECRET`
  级联清除引擎侧邮箱数据
- 消息缓冲分级：小消息驻内存、大消息落临时文件，出站清理与中继全程流式
- `mailezine reindex` 重建全文索引（POSIX）
- `MAILEZINE_CONFIG` 指定 toml 配置文件（扁平 MAILEZINE_* 键，环境变量优先）

## 快速开始（独立开发）

mailezine 不依赖 mailez 代码，开发期用本地桩替代目录契约与认证：

```powershell
# 1. 准备数据目录与 dev 桩
$env:MAILEZINE_ROCKS_PATH     = "data\kv"
$env:MAILEZINE_DIRECTORY_FILE = "internal/directory/testdata/dev-directory.json"
$env:MAILEZINE_AUTH_DEV_FILE  = "internal/auth/testdata/dev-passwords.json"

# 2. 启动（默认监听 :11480 健康端点）
go run ./cmd/mailezine
# 默认再监听 :1025（SMTP）、:1587（Submission）、:1143（IMAP）、
# :11490（ManageSieve）、:10110（POP3，可关）

# 3. 验证
curl http://127.0.0.1:11480/health

# 4. 端到端冒烟（本地投递 + 中继入队）
go test ./cmd/mailezine -run TestEndToEnd -v
```

## 配置

全部可环境变量覆盖（默认值见 `internal/config`）：

| 变量 | 默认 | 说明 |
|---|---|---|
| `MAILEZINE_STORAGE_BACKEND` | `pebble` | 存储后端（社区版：`pebble`） |
| `MAILEZINE_ROCKS_PATH` | — | pebble 数据路径（必填） |
| `MAILEZINE_DIRECTORY_MODE` / `MAILEZINE_AUTH_MODE` | `dev` | 生产用 `mailez`（走控制面 API） |
| `MAILEZINE_BACKEND_ADDRESS` | `127.0.0.1:8080` | mailez 控制面地址（目录/认证/DKIM vault） |
| `MAILEZINE_DIRECTORY_FILE` / `MAILEZINE_AUTH_DEV_FILE` | — | dev 模式目录/口令桩 |
| `MAILEZINE_HOSTNAME` | 本机名 | EHLO / Authentication-Results 主机名 |
| `MAILEZINE_RECIPIENT_DELIMITER` | `+` | 扩展地址分隔符（空 = 禁用 plus 地址） |
| `MAILEZINE_TRUSTED_NETS` | `127.0.0.1/8,::1/128` | 网关子网（跳过认证 + 允许中继） |
| `MAILEZINE_RSPAMD_URL` | 空 | 如 `http://mail-filter:11333/checkv2`；空 = 不扫描 |
| `MAILEZINE_DKIM_VAULT_URL` | `http://<控制面>/stack/rspamd/vault` | 出站 DKIM 密钥源 |
| `MAILEZINE_OUTBOUND_ENABLED` / `MAILEZINE_OUTBOUND_PORT` | `true` / `25` | 中继队列开关与投递端口 |
| `MAILEZINE_QUEUE_MAX_ATTEMPTS` / `MAILEZINE_QUEUE_BASE_RETRY_SECONDS` / `MAILEZINE_QUEUE_MAX_RETRY_SECONDS` / `MAILEZINE_QUEUE_POLL_INTERVAL_SECONDS` | `10` / `60` / `86400` / `5` | 出站队列重试与轮询 |
| `MAILEZINE_SMTPS_ADDR` / `MAILEZINE_IMAPS_ADDR` / `MAILEZINE_POP3S_ADDR` | 空 | 隐式 TLS 端口（465/993/995），需设置证书 |
| `MAILEZINE_TLS_CERT_FILE` / `MAILEZINE_TLS_KEY_FILE` | — | STARTTLS 证书（同时设置生效） |
| `MAILEZINE_PROXY_PROTOCOL` | 空 | 解析 PROXY 头的端口列表或 `all` |
| `MAILEZINE_PROXY_TRUSTED` | 回环+私网 CIDR | 允许携带 PROXY 头的对端 CIDR 列表；LB/网关在公网时必须显式加其 IP，防客户端 IP 伪造 |
| `MAILEZINE_FTS_ENABLED` / `MAILEZINE_FTS_TIKA_URL` | `true` / 空 | 全文索引开关与附件文本提取 |
| `MAILEZINE_BLOB_COMPRESSION` | `false` | blob gzip 压缩（兼容旧未压缩数据） |
| `MAILEZINE_JMAP_ENABLED` | `false` | JMAP 接口开关（预留，功能尚未实现，v2 规划） |
| `MAILEZINE_MANAGEMENT_ADDR` / `MAILEZINE_MANAGEMENT_SECRET` | 空 | 管理 API 监听与共享密钥 |
| `MAILEZINE_CACHE_SIZE` | `8388608` | IMAP envelope/body-structure 缓存预算（字节） |
| `MAILEZINE_META_CACHE_SIZE` | `33554432` | 邮箱元数据 + 目录查询缓存预算（字节） |
| `MAILEZINE_AUTH_CACHE_SIZE` | `1048576` | 认证结果缓存预算（字节，短 TTL） |

## 端到端验证工具

| 工具 | 覆盖 |
|---|---|
| `deploy/scripts/storage-backend-e2e.sh` | Pebble 收发 + 重启持久化回归 |
| `deploy/scripts/smoke-container-e2e.sh` | 容器镜像构建 + 端口协议面契约 |
| `deploy/scripts/mailez-backend-e2e.sh` | mailez 控制面全链路（建域建户 → 提交 → 投递） |
| `cmd/storage-smoke` | 存储冒烟工具（send/check） |
| `cmd/e2e-mailez` | 控制面驱动 e2e 客户端 |

## 质量门

```powershell
make verify   # gofmt / tidy / vet / go test -race / build
```

CI：GitHub Actions 三作业——ubuntu 全量（`make verify`，含 -race 与社区
纯净检查）、windows（gofmt/vet/test/build）、真实 rspamd 容器 e2e
（`-tags docker`，SMTP→分类→存储全链路）。
`deploy/scripts/ci-env` 提供本地 Linux 容器全量验证环境。

## 文档

- [docs/ARCHITECTURE.md](./docs/ARCHITECTURE.md) — 架构设计（分层、领域模型、不变式、一致性、并发、安全）
