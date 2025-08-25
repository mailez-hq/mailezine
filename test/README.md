# mailezine 测试策略

开发期不需要 webmail：webmail 只是「backend REST → IMAP/SMTP」链路的消费者。
接前端前，mailezine 按四层测试（ARCHITECTURE.md §11）。

## 层一：单元 + 契约（`go test`，无外部依赖）

- 存储键编码、配额、sieve 动作、状态机、目录契约、认证边界
- 协议适配器用内存 KV/blob 桩离线测

## 层二：协议冒烟（直连 mailezine 端口）

| 协议 | 工具 |
|---|---|
| SMTP | swaks、openssl s_client / telnet |
| IMAP | openssl s_client、emersion/go-imap 集成测试 |
| POP3 | telnet |
| ManageSieve | 后端 sieve 客户端（mailez/backend/internal/mail/sieve.go）|
| 一致性 | legacy IMAP imaptest（临时容器） |

## 层三：backend REST 全链路（webmail 替身）

dev compose（mailezine + backend + redis + rspamd + unbound [+ minio]），
`MAILEZ_MAIL_ENGINE=mailezine`，然后：

1. `go run ./cmd/seed` 建管理员
2. `go run ./cmd/e2e`（登录 → 建用户 → SMTP 发信 → IMAP 收信 → 验 DKIM 头）
3. curl / 小 Go 驱动打 `backend/internal/mailbox` 与 `compose` 的全部路由
   （folders/messages/raw/thread/search/flag/snooze/move/delete/acl/labels/send/draft）

全绿 = 前端接上即绿。

## 层四：真实客户端 + 故障

- mainstream clients / mutt / 手机客户端直连网关人工冒烟
- 双后端对拍（maildir 与 RocksDB 同一操作序列，结果哈希一致）
- 崩溃矩阵（各阶段 kill -9 后重启验不变式）
- SMTP/IMAP/MIME/Sieve 解析器 fuzz

## 落地状态

- M0：`go test -race` 覆盖层一（config/limits/store/directory/auth）
- M1：层二冒烟脚本 + 层三 dev compose
- M2+：双后端对拍、崩溃矩阵、fuzz
- M3：`go test ./cmd/mailezine -run TestEndToEnd -v` 两层端到端冒烟——
  `TestEndToEndLocalDelivery`（真实 SMTP → 目录解析 → 校验/分类 → KV 存储，
  停机后回读验证）与 `TestEndToEndRelayQueue`（AUTH 提交 → 中继入队 →
  队列元数据 + spooled blob 持久化验证）。rspamd 端到端（真 rspamd 容器）
  待 dev compose 落地后并入层三。
- M4：`internal/imap` 集成测试（真实 go-imap 客户端走线：APPEND→SELECT→
  FETCH/SEARCH/STORE→COPY/MOVE→EXPUNGE→STATUS）+
  `TestEndToEndIMAP`（SMTP 投递 → IMAP 登录收信全栈）；
  `mailstore` 双后端共享 mailbox 套件（KV 本机、maildir CI）。
- M6：`internal/sieve` 执行测试（fileinto/keep/discard/envelope/坏脚本降级）
  与 ManageSieve 行协议测试（读流程 + 鉴权失败）；投递级测试验证
  Subject 含 spam → Junk、discard 不入库；`TestEndToEndIMAP` 追加
  ManageSieve 全栈读流程（AUTHENTICATE PLAIN + LISTSCRIPTS）。
- M3 收尾 / M5：`internal/smtp` 增加 SenderRate 限速与 SRS RCPT 还原测试；
  `internal/queue` RelayDeliverer 按 smarthost 分组测试；
  `TestEndToEndRelayQueue` 增加远端发件人中继 → SRS 信封改写断言；
  `internal/pop3` 协议集成测试 + `TestEndToEndPOP3`（SMTP→POP3 收信/
  删除/QUIT 提交 → 存储验证）。
- M7 / 工程化：`TestMigrateMaildirToPebble`（unix/CI：maildir 双账户双邮箱
  迁移 → KV 校验顺序/旗标/关键词/正文）；管理 API 队列端点测试 +
  relay e2e 经真实 HTTP 验证 `/v1/queue`；CI 三 job（ubuntu verify /
  windows / rocksdb `-tags rocksdb`，rocks 契约套件 `TestStoreRocks`）。
- M6 写面：`mailstore` SieveStore 双后端共享套件（KV 本机 / maildir CI：
  put/list/get/activate/deactivate/delete）；ManageSieve 行协议写流程
  （PUTSCRIPT→SETACTIVE→GETSCRIPT→LISTSCRIPTS→DELETESCRIPT）；delivery
  优先级测试（本地活动脚本覆盖目录默认）；`TestEndToEndIMAP` 全栈
  ManageSieve 写 + 过滤落 Junk 验证。
- M4 推送 / 真容器：`TestIMAPIdlePush`（IDLE 中外部投递/删除 → EXISTS/
  EXPUNGE 非请求更新）；`internal/spam` 与 `cmd/mailezine` 的
  `-tags docker` 真 rspamd 测试（本地：
  `docker run -d -p 11333:11333 mailez/rspamd:local /usr/bin/rspamd -f --insecure`
  后 `go test -tags docker ...`；CI 用官方镜像跑同一套）。
- M7 对拍 / TLS：`internal/imap` 与 `internal/pop3` 的线级生命周期测试
  双后端共享（KV 本机 + maildir CI）；`TestSMTPStartTLS` /
  `TestIMAPStartTLS` 用自签证书验证 STARTTLS 广告与升级后收发。
- M7 挂载演练：`TestMountDovecotMaildir`（legacy IMAP 真实 uidlist v3 单行
  V/N/G、`:new` 前缀、`,S=,W=` 文件名 → mailezine 读 UID1 + 追加 UID2 +
  uidlist 保真）+ 真容器 `deploy/scripts/maildir-mount-e2e.sh`（legacy IMAP
  播种 → mailezine 挂载 → IMAP/SMTP → legacy IMAP legacy-imap-ctl 回读）。
- 质量批次：崩溃矩阵（`TestCrashConsistency` Pebble 本机 /
  `TestCrashConsistencyRocks` CI；`TestQueueCrashConsistency`）、fuzz
  （maildir/sieve/imap）、POP3 `TestPOP3StartTLS`、ManageSieve
  `TestManageSieveStartTLS`、`TestLoadTOMLOverlay`、队列事件与指标测试、
  `TestMigratePebbleToMaildir`（反向迁移 + keywords 保真）、Sieve redirect
  （单测 + delivery 钩子 + 无队列降级）、`TestSMTPServerCapabilities`
  （8BITMIME/SIZE/DSN/SMTPUTF8 广告）、`TestOutboundTLSPolicy`
  （MTA-STS/DANE 选择：无策略/无记录 → opportunistic、TLSA → required、
  IP 主机跳过 DANE）、Sieve reject（550 映射）、Message-ID 注入、
  `TestAuthSenderIdentityEnforced`（防伪造）、`TestPauseResume`、
  `TestAccountsEndpoint`、PROXY protocol（`internal/server` 头解析矩阵 +
  SMTP 端到端 peer IP 透传）、TLS 强制认证（四协议明文 AUTH 拒绝）、
  IMAP `HEADER.FIELDS` 深度测试（含无空格形式）、UIDVALIDITY 重建变化
  （双后端 mailbox 套件）、`TestMigrateMaildirToPebbleS3Blob`（maildir ⇄
  KV(S3 blob) 全量往返，blob 落真实 S3 mock）、队列调度参数 main 覆盖测试。
- 退信 DSN：`TestComposeBounceDSN`（RFC 3464 multipart/report 结构）、
  `TestBounceHandlerInvoked`（终态触发一次）、`TestEndToEndBounceDSN`
  （SMTP → 不可达域 → 队列 bounced → DSN null 发件人送达 alice INBOX
  全链路）。
- M4 ACL（RFC 4314）：`TestACLSuiteKV/TestACLSuiteMaildir`（双后端持久化
  套件：默认空、set/get/delete、邮箱隔离、重命名随行）、
  `TestIMAPACLCommands`（真实 go-imap v2 客户端 MYRIGHTS/GETACL/SETACL
  + `+rights` 修改 + ACL 能力宣告）、`TestIMAPACLWire`（原始线级
  LISTRIGHTS/DELETEACL/NIL 语义 + 未知命令仍 BAD）、
  `TestIMAPACLBackendCompat`（用 mailez 后端实际解析逻辑——go-imap v1
  `ParseNamedResp`——消费 GETACL/MYRIGHTS/LISTRIGHTS 响应）。
- M4 客户端矩阵：`TestMoxClientLifecycle`（peer engine `imapclient` 独立实现驱动
  Create/Append/Select/Status/Search/Store/Copy/Move/Expunge/Rename/
  Delete 全流程）与 `TestMoxClientFetch`（Proto 低级命令 FETCH 的 FLAGS/
  RFC822.SIZE/HEADER.FIELDS 响应语义）；`deploy/scripts/smoke-container-e2e.sh`
  容器冒烟（镜像构建、健康端点、IMAP/SMTP/POP3 端口契约 + ACL 能力）。
- M6 补强（Sieve 扩展）：`TestMailezTemplateCompiles`（mailez 控制面默认
  模板脚本编译 + editheader/vacation 收集 + `${N}` 变量）、
  `TestMailezTemplateSpamJunk/SpamClean`（spamtest :percent 85% 阈值分流
  Junk + `\seen` + stop 语义）、`TestMailezTemplateNoReplyNoVacation`、
  `TestSpamTestRelational`（80%/85% 边界）；delivery 集成：
  `TestDeliverSieveEditHeader`（存储副本头改写）、`TestDeliverVacation`
  （null 发件人 + 节流）、`TestDeliverNoAutoReplyLoop`（Auto-Submitted
  防循环）、`TestDeliverSpamLevelFallback`（X-Spam-Level 兜底注入）。
- 垃圾学习（D25）：`TestLearn`/`TestLearnDisabled`（spam.Client 的
  `/learnspam|/learnham` + Password 头 + 消息体，未配置时 no-op）；
  `TestIMAPLearnMoveToJunk`（MOVE INBOX→Junk 学 spam）、
  `TestIMAPLearnMoveOutOfJunk`（APPEND Junk 学 spam + MOVE Junk→INBOX 学
  ham 双触发）、`TestIMAPLearnCopyToJunk`、`TestIMAPLearnNoop`（不跨 Junk
  边界不学习）；真 rspamd 容器验证 controller 鉴权与消息格式。
- 出站 SASL（D26）：`TestSMTPDeliverSmarthostAuth`（smarthost 投递发
  `AUTH PLAIN`，base64 载荷 `\0user\0pass` 校验）、
  `TestSMTPDeliverNoAuthToMX`（直接 MX 投递零认证，凭证不泄露）。
- IMAP ID（D27）：`TestIMAPIDClient`（go-imap client 的 ID 原生命令，
  NIL 与字段列表两种请求，响应含 `name=mailezine`/`vendor=mailez`）、
  `TestIMAPIDBeforeLogin`（未认证状态 ID 合法 + greeting 广告 ID 能力）。
- backend 全链路（D28）：`cmd/e2e-mailez`（backend health、admin SSO、
  分页域/用户供给、SMTP AUTH 提交、IMAP 搜索验证投递，6 项断言）；
  `deploy/scripts/mailez-backend-e2e.sh` 一键编排并在真环境通过。
- 存储后端自测：`cmd/storage-smoke`（dev 桩 SMTP 提交 + IMAP/POP3 读信，
  send/check 两模式）；`storage-backend-e2e.sh`（Pebble：发收 + 重启持久化
  实测通过）与 `rocksdb-storage-e2e.sh`（RocksDB cgo：同一流程 + WAL 恢复
  实测通过）。
- MinIO blob（D29）：`storage-smoke --s3-*` 用 minio-go ListObjects 断言
  `email-*` 消息对象真实落桶；`minio-storage-e2e.sh`（独立 MinIO + Pebble
  KV）实测：SMTP 提交 → 落桶 → IMAP/POP3 读信 → 重启 → 经 MinIO 恢复。
