# mailezine 架构设计

> 状态：设计基线 v1 · 2026-08-24 · 配套文档：[`PLAN.md`](./PLAN.md)（里程碑与决策点）、
> [`mailez/docs/engines/mailezine-recon.md`](../mailez/docs/engines/mailezine-recon.md)

## 0. 设计立场

mailezine 的架构以"可验证的正确性、可解释的并发、清晰的边界、可操作的运维、
长期可演进"为第一原则。它站在经典邮件系统（队列与投递语义、maildir 保真、
Go 实现细节、单二进制 + 可插拔存储形态）的肩膀上，但不照搬任何
一家的实现。

三条铁律，任何代码评审、任何重构都不得违反：

1. **依赖单向且无环**。领域层不感知协议层；协议层不感知存储拓扑；组合根是唯一
   允许"什么都知道"的地方。CI 用工具强制（§11）。
2. **状态有明确属主**。每份可变状态恰好属于一个组件；跨属主只能经接口或通道
   传递，禁止全局可变状态、禁止隐式共享。
3. **每个持久化写点都有确定的顺序与故障语义**。要么整批原子生效，要么可恢复；
   崩溃窗口、恢复动作、人工兜底路径必须写进设计（§3.5、§4、§5）。

## 1. 分层与依赖规则

### 1.1 层定义

| 层 | 包 | 责任 | 允许依赖 |
|---|---|---|---|
| L0 基础设施 | `util`、`telemetry`（log/metrics/trace）、`limits` | 与业务无关的通用能力 | 仅标准库 |
| L1 领域内核 | `store`（kv/blob/index）、`directory`、`auth`、`mailbox`/`message` 领域类型 | 数据模型、不变式、存储契约、目录契约 | L0 |
| L2 服务 | `delivery`（入站管道）、`queue`（出站）、`sieve`、`spam` | 业务流程与状态机 | L1 |
| L3 协议适配 | `smtp`、`imap`、`pop3`、`managesieve`、`jmap`、`management` | 把字节流翻译成领域调用；只做协议，不做业务 | L2、L1 |
| L4 应用入口 | `cmd/mailezine`、`config`、`lifecycle` | 组合根、配置、监听器编排 | 全部 |

### 1.2 依赖规则

- L3 协议适配器**不得**直接触碰 KV/blob/maildir；一切数据访问经 L1 `store.Account`
  接口与 L2 管道。
- L1 领域内核**不得** import 任何 L2/L3 包（例如 `store` 不知道 SMTP 是什么）。
- L4 是唯一的"上帝层"：装配接口实现、启动监听器、注入配置与生命周期。
- 包内禁止 `init()` 副作用；全局变量只允许不可变配置与只读注册表。
- vendor 进 internal/ 的协议代码（`internal/imapserver`、`internal/gosieve`，
  MIT）视为 L3，保留原版权头，且同样遵守依赖规则——它只能通过我们定义的接缝
  接口回调 L1/L2，不反向依赖。

### 1.3 依赖图（示意）

```
cmd/mailezine ──▶ config/lifecycle ──▶ management ──▶ smtp/imap/pop3/managesieve/jmap
        │                                   │              │
        │                                   └──────────────┼──▶ delivery/queue/sieve/spam
        └──────────────┬───────────────────────────────────┘              │
                       └──────────▶ store/directory/auth ◀───────────────┘
                                         │
                                        util/telemetry/limits
```

## 2. 核心领域模型

### 2.1 概念

一切以业界单二进制实现同构、但独立设计的模型展开：

```
Account（= 一个邮箱账户，来自目录契约 user）
└── Collection（类型化对象集合）
    └── Document（不可变或带版本的对象）
        └── BlobRef（对原始字节的引用，内容寻址）
```

v1 落地的 collection：

| Collection | 内容 | 关键字段 |
|---|---|---|
| `Mailbox` | 文件夹 | name、uidvalidity、uidnext、special-use、subscription、ACL、parent |
| `Email` | 邮件文档 | blob ref、mailbox 归属、UID、flags/keywords、内部日期、envelope、size、mime 结构缓存、thread 引用 |
| `Thread` | 会话（推导集合） | root email、参与地址、最近时间 |
| `Identity` | 发件身份 | name、email、replyTo、签名偏好 |
| `EmailSubmission` | 待发/已发提交记录 | identity、envelope、state、失败原因 |
| `SieveScript` | 过滤脚本 | name、script、active、顺序 |
| `Principal` | 主体（账户/组） | 权限与 ACL 关联 |

### 2.2 不变式（节选，完整清单见 §14）

- **INV-UID**：同一 mailbox 内 UID 严格单调且连续可分配；`uidnext` 只能前进；
  `uidvalidity` 在该 mailbox 生命周期内不变。
- **INV-BLOB**：Email 引用的每个 blob 都有一条 blob-link；无引用的 blob 必须被
  GC；blob 内容不可变（内容寻址保证）。
- **INV-QUOTA**：账户已用字节 = 其全部 blob 引用字节之和（含去重引用计数）；
  任何写路径先预检、后记账，账目守恒。
- **INV-CHANGE**：每账户 change log 单调递增、无空洞；一次事务产生的所有变更
  共享同一 changeID。
- **INV-DELIVERY**：一封消息要么已投递（有 Email 文档），要么在队列/退信路径上，
  绝不两处都存在或两处都丢失（崩溃窗口内由 §3.5 的恢复保证）。

### 2.3 变更日志（Change Log）

每账户一个 append-only 变更序列 `(accountID, collection, changeID, op, target)`，
写入与业务事务同批（同一次 KV batch）。用途：JMAP `changes`、未来 IMAP
CONDSTORE/QRESYNC、通知总线（IDLE/推送）。设计要点：

- changeID 为每账户单调计数器，分配与事务提交同原子。
- 日志只增，compaction 策略由存储后端负责；GC 保留窗口可配置。
- 订阅者（IMAP IDLE、JMAP push、quota 回写）从日志消费，绝不轮询业务表。

## 3. 存储层

### 3.1 接口契约

```go
// KV 是字节级有序键值存储。RocksDB 与 Pebble 实现；测试用内存实现。
type KV interface {
    Get(key []byte) ([]byte, error)
    Put(key, val []byte) error
    Delete(key []byte) error
    Scan(prefix []byte, fn func(k, v []byte) error) error
    Batch(ops []Op) error // 原子、有序、可含计数/合并操作
    Close() error
}

// Blob 是不可变字节对象存储。MinIO(S3)、本地 FS 实现。
type Blob interface {
    Put(ctx context.Context, id string, r io.Reader) (int64, error)
    Get(ctx context.Context, id string, w io.Writer) error
    Delete(ctx context.Context, id string) error
    Stat(ctx context.Context, id string) (int64, error)
}
```

协议适配层不直接见这两个接口；它们只被 `store.Account` 与管道使用。

### 3.2 键编码规范

二进制前缀键，按字典序可扫描。布局：

```
space(1B) | accountID(4B BE) | collection(1B) | documentID(8B BE) | field(1B) | value…
```

子空间（space）注册表：

| space | 用途 |
|---|---|
| `m` meta | 配置、schema 版本、账户注册表 |
| `a` account | 账户属性、计数器（uidnext、changeID、blob 计数） |
| `i` index | 邮箱/邮件索引（mailbox→UID→docID、keywords、日期、thread） |
| `d` document | 文档主体 |
| `l` log | change log |
| `b` blob-link | blob 引用计数 |
| `q` queue | 出站队列元数据 |
| `u` quota | 配额计数 |

编码规则由 `store/key` 包统一实现，禁止各后端各自拼键——这是可移植性与
调试工具（`mailezine debug dump`）的前提。

### 3.3 RocksDB 后端

- 单实例打开一个 RocksDB，按 space 前缀分区（不用多 column family，保持简单；
  若性能需要再评估 CF）。
- 写路径：业务操作组装一个 `Batch`（文档 + 索引 + change log + 配额 + blob-link），
  一次 `Write` 提交；崩溃后要么全有要么全无。
- 读路径：统一走迭代器包装（`Scan`），外层禁止泄漏迭代器；长扫描限时、可取消。
- 打开时校验 `schema_version`（meta space），不匹配则拒绝启动并给出迁移指引。

### 3.4 maildir 后端（存量格式保真）

目标不是"像 maildir"，而是**就是 maildir**：

- 目录布局：`/mail/<account>/{cur,new,tmp}` + 子文件夹，兼容 `maildir:/mail/%u`。
- `dovecot-uidlist`：按该既有格式读写（含 `dovecot-uidlist.lock`），首次挂载
  旧目录时解析接管；UID 分配与消息文件落盘在同一临界区。
- `dovecot-keywords`：关键词持久化；系统旗标（S/R/F/T/D/P）落 maildir `:2,` 信息段。
- 派生索引（搜索、thread、snooze 视图）放**可重建 sidecar**（默认 SQLite），
  任何 sidecar 损坏都只触发重建，绝不阻塞读信。
- 并发：每账户单写者（进程内 `sync.RWMutex` + 文件锁双重保护）；多实例写入
  明确不支持——这是文档化的契约，不是实现缺陷。
- 投递原子性：消息写入 `tmp/` → fsync → 改名进 `new/` → 提交 UID 映射。

### 3.5 一致性协议（跨后端统一）

写顺序固定为：

1. 校验（配额、去重、权限、输入限制）；
2. 写 blob（内容寻址，幂等）；
3. 提交 KV/maildir 元数据批（含 blob-link、change log、配额、索引）；
4. 发布变更通知。

崩溃窗口与恢复：

- 窗口 A（blob 已写、元数据未提交）：孤儿 blob 由 GC 回收（blob-link 扫描）。
- 窗口 B（元数据已提交、通知未发）：订阅者以 change log 为准收敛，通知是优化
  不是正确性依赖。
- 窗口 C（maildir 半程）：`tmp/` 残留由启动清理；`cur/`/`new/` 内文件与
  uidlist 不一致时以 uidlist 为准并重建 sidecar，冲突文件进 `quarantine/` 供人工裁决。

## 4. 入站管道（Inbound Pipeline）

SMTP 只是入口之一；入站管道是独立于协议的服务，任何入口（SMTP、将来 LMTP/JMAP
import）都走同一管道：

```
accept → session(限流/语法) → verify(SPF/DKIM/DMARC) → classify(rspamd[+junk])
       → fingerprint(去重) → sieve → deliver(本地投递) → notify + quota 回写
```

> 当前落地（M3）：`verify → classify → deliver` 三段已接线并有端到端测试；
> `fingerprint`（去重）与 `sieve`（脚本路由）随 M4/M6 加入，契约已固定。

### 4.1 阶段契约

| 阶段 | 输入 | 输出/副作用 | 失败语义 |
|---|---|---|---|
| verify | 信封 + 原始消息 | auth-results 元数据 | 校验失败不阻断，标记后进分类 |
| classify | 消息 + auth-results | action/score/headers | rspamd 超时 → 按配置降级（放行 + 标记或拒绝） |
| fingerprint | 规范化消息 | 指纹（sha256 of 规范化头+正文） | 重复投递：最近窗口内同指纹 → 静默丢弃/计数 |
| sieve | 消息 + 分类结果 | 目标 mailbox、动作序列 | 脚本错误 → 保留默认投递 + 记录 |
| deliver | 目标 + 消息 | Email 文档 + 通知 | 配额超限 → 退信；存储失败 → 452 临时失败 |

### 4.2 资源纪律

- 连接数、并发 DATA、单消息大小、RCPT 数、会话时长全部有上限（`limits` 包统一
  定义与校验）。
- 慢客户端：读超时 + 写缓冲上限；不做无限缓冲。
- 限速：连接级令牌桶 + 目录契约的每发件人限速（`/stack/directory/senders/:email/rate`）。

## 5. 出站管道（Outbound Pipeline）

### 5.1 队列状态机

```
             submit
               │
               ▼
           SUBMITTED ──spool──▶ QUEUED ──scheduler──▶ ACTIVE
                                      ▲                  │
                                      │ retry/backoff     │ deliver
                                      │                  ▼
                                      │           DELIVERED（终态）
                                      │
                                   DEFERRED ◀── 临时失败
                                      │
                                      ▼
                                  BOUNCED（DSN 已发，终态）
                                      │
                                   FAILED（人工可见，终态）
```

- 状态迁移只发生在队列管理器内，凭据（消息 ID + 收件人）保证同一消息不会双投递。
- 元数据在 KV `q` space；消息体是 blob。调度器按到期时间轮询，事件驱动唤醒
  （避免空转）。
- 重试：指数退避 + 抖动，上限可配；每域并发与节流独立。
- 投递：自研 `internal/mailsmtp` + `internal/maildns`（D43/D44，已无上游邮件服务器依赖）；出站策略依次 DANE → MTA-STS → 普通 TLS → 明文（可配，机会式升级
  失败自动明文重连）；DKIM 签名在入队时完成一次（go-msgauth）。
- DSN：自研 `internal/maildsn`（RFC 3464）生成；bounce 走回入站管道。
- SRS：转发路径在信封阶段经 `/stack/directory/srs/*` 改写（沿用 mailez 边界策略）。

### 5.1.1 多活认领（cluster.mode=multi）

多副本共享同一 KV+blob 时，`QUEUED/DEFERRED → ACTIVE` 的迁移是一次 KV 事务
内的**认领**：记录 `owner`（节点 ID）与 `leaseUntil`（租期），并把到期索引
条目挪到租期时刻——于是任一副本的调度扫描天然跳过“他副本持有有效租约”的
消息，而持有者崩溃后，消息恰好在租期到期时重新可见、被任意副本接管
（接管计一次尝试，crash-loop 有界）。投递结果的写回以 ownership fencing
为前置条件：认领已丢失的迟到 worker 其写回整批丢弃，绝不覆盖接管者的
状态。消息 ID 分配（counter 自增）同样在事务内，多副本不重号。租期默认
10 分钟（`MAILEZINE_QUEUE_CLAIM_LEASE_SECONDS`），须大于最慢单次投递；
租约窗口内接管造成的重复投递符合 at-least-once 语义（与单机崩溃恢复一致）。

配套的全局单例 worker（snooze sweeper 等）用 `internal/kvlease` 的命名租约
（owner + TTL，tick 即续约）跨节点选主，同一时刻全集群恰有一个执行者。

多活下的全文检索由 `internal/ftssync` 收敛：bleve 索引是节点本地的，每个
节点按账户水位 tail 变更日志（§2.3，INV-CHANGE），create/update 重索引、
delete 精确删除（变更日志的删除条目扩展携带被删副本的 mailbox 与 UID，
10 字节旧编码向前兼容）。投递路径在多活下不内联索引——每副本经追踪器恰好
索引一次。索引是纯派生数据：水位损坏或副本重建时从 0 重放即全量重建。

### 5.2 队列管理

管理 API（`management`，仅内网 + 共享密钥）暴露：队列深度、按域视图、重试/冻结/
丢弃单条、导出失败样本。管理动作全部进审计日志。

## 6. 会话状态机（协议适配层）

### 6.1 统一连接纪律

每个协议适配器共享一套 `conn` 基础：行读取限制、字面量上限、读写超时、TLS 升级、
优雅关闭信号、结构化日志上下文（session_id）。协议差异只体现在命令表上。

### 6.2 SMTP 会话

go-smtp 基座（D9，`BackendFunc` 暴露对端地址）+ 上游会话语义参考。状态表：

```
GREETING → EHLO/HELO → AUTH? → MAIL → RCPT* → DATA → 下一封
              │         │       │       │      │
              └── RSET/NOOP/VRFY/QUIT 随时可回退
```

- 非法序列/未知命令：按 RFC 5321 返回正确响应码，不中断连接（防探测指纹）。
- AUTH 只接受 PLAIN/LOGIN；受信对等方（网关子网）跳过认证，直连回源
  `/stack/auth/email`。

### 6.3 IMAP 会话

状态机：`NOT_AUTH → AUTH → SELECTED`（+ `AUTHENTICATED` 无选中邮箱）。命令分派
按状态表白名单；未知/越权命令返回 `BAD`，不 panic、不断连。

数据访问只经 `store.Account` 的方法集（`ListMailboxes`/`GetMessage`/`Search`/
`UpdateFlags`/`Move`/`Append`/…），适配器不感知 maildir 或 RocksDB 的差异。

### 6.4 认证边界

```
外部客户端 ──▶ gateway ──/stack/auth/email──▶ backend（唯一认证真源）
                    │
                    └─受信子网──▶ mailezine（跳过二次认证）
直连（裸部署）──▶ mailezine ──回调 /stack/auth/email──▶ backend
```

引擎内不存密码、不做 2FA/PGP 决策；temp token 只在 webmail 端口（1143/1587/
11490）被信任，且信任前提是 peer 在网关子网内。

## 7. 并发模型

### 7.1 goroutine 模型

- 每连接一个 goroutine（协议层），由 `limits` 控制的连接上限收口。
- 投递 worker 是固定大小池（semaphore），从队列事件消费。
- 跨 goroutine 只经 channel / 事件总线；不共享可变状态。

### 7.2 账户属主与锁

- 账户注册表（`store/registry`）：accountID ↔ 账户目录/配置，加载一次、缓存。
- 每账户一把锁：RocksDB 后端是进程内写锁（批量合并），maildir 后端是文件锁 +
  进程内互斥。锁顺序固定（账户锁 → 子资源），杜绝死锁。
- 锁内禁止网络 IO（目录回源、rspamd 调用在锁外完成，结果入参）。

### 7.3 通知与背压

- 变更通知走有界 channel 的事件总线；订阅者慢时丢弃并让订阅者从 change log 收敛。
- 管道级背压：队列深度、投递 inflight、rspamd 并发均有上限；达到上限时 SMTP
  返回 4xx 而不是无限积压内存。

### 7.4 优雅关闭

两阶段：① 停止接受新连接、拒绝新投递任务；② 等 in-flight 完成（带超时）、
flush 队列元数据、关闭存储（RocksDB flush + close；maildir 释放文件锁）。SIGTERM
全程可重复执行、可被监控判定。

## 8. 安全模型

### 8.1 信任边界

```
公网 ──TLS──▶ gateway（nginx，TLS 终止 + 认证前置）
                │ 内网/受信子网
                ▼
           mailezine（数据面，最小权限）
                │
                ▼
        rspamd / macro-scanner / unbound / MinIO（按需最小暴露）
```

### 8.2 输入与解析安全

- MIME 深度与头数量上限（防解析炸弹）；附件类型不信任，由 macro-scanner 扫描。
- 路径安全：maildir 账户名/文件夹名经严格白名单 + `path.Clean` 校验，防穿越。
- 字面量/大小限制在协议层强制执行，存储层只信任协议层传入的已校验对象。
- 常量时间比较（token/密钥）；bcrypt 校验永不发生在引擎（回源 backend）。

### 8.3 部署加固

- 非 root 运行、容器只读根文件系统、仅挂载 `/mail`、`/data`、`/run`。
- TLS：默认要求 STARTTLS；证书由 gateway ACME 管线共享，引擎内网证书独立管理。
- 管理 API 仅内网 + 共享密钥；指标端点只暴露计数不暴露内容。

## 9. 可观测性

### 9.1 结构化日志

统一 slog，字段约定：`session_id`、`message_id`、`account`、`stage`、`queue`、
`outcome`。每封消息在管道里产生一条可串联的日志链（入站/出站各一条摘要 +
关键阶段明细）。日志永不包含正文内容与密钥。

### 9.2 指标

| 指标 | 用途 |
|---|---|
| `mailezine_smtp_connections_total`、`mailezine_smtp_connections_active` | SMTP 连接总数与活跃连接（入站 + 提交两个监听） |
| `mailezine_smtp_messages_in_total{outcome}` | 入站结果（accepted/rejected/deferred） |
| `mailezine_smtp_auth_failures_total` | SMTP 认证失败 |
| `mailezine_delivery_duration_seconds{result}` | 出站投递延迟（ok/error） |
| `mailezine_queue_depth{state}`、`mailezine_queue_messages_total{event}` | 队列水位（按状态抓取时盘点）与状态迁移 |
| `mailezine_imap_sessions_active`、`mailezine_pop3_sessions_total` | 容量 |
| `mailezine_storage_ops_total{backend,op}`、`storage_io_bytes` | 存储健康（规划中，未实现） |
| `mailezine_quota_errors_total` | 配额事件（规划中，未实现） |

### 9.3 追踪与健康

- OpenTelemetry span 贯穿 verify→classify→sieve→deliver（入站）与
  spool→deliver（出站）；采样率可配。
- 健康分两级：liveness（进程活着）与 readiness（存储可达、队列可写、目录缓存
  未过期）；容器 healthcheck 用 readiness。

### 9.4 审计

管理 API 与供给类操作（账户变更、队列干预、配置重载）写入审计日志
（`audit` 子空间），包含操作者、动作、结果、时间戳。

## 10. 配置与生命周期

### 10.1 配置模型

- 类型化 struct + 校验（必填、范围、枚举），来源：环境变量（compose 注入）+
  可选 TOML 覆写；敏感项（密钥、MinIO 凭据）只从环境/secret 文件读取，绝不落配置。
- 启动即校验 + 展示"将监听哪些端口/启用哪些功能"的启动摘要；配置错误拒绝启动
  并给出可操作错误信息。

### 10.2 组合根

`cmd/mailezine/main.go` 只做四件事：解析配置 → 组装依赖（存储、目录、管道、
协议）→ 启动监听器 → 阻塞等待信号并优雅退出。所有接口在组合根注入；单元测试
在组合根之下任意替换实现。

### 10.3 功能开关

POP3、junk、JMAP、全文搜索、远程 blob 等为显式开关，默认值按 PLAN.md 决策
（POP3 on、junk off、JMAP off）。开关只影响"是否启动该组件"，不影响核心路径语义。

## 11. 测试与质量门

### 11.1 测试金字塔

| 层 | 覆盖 | 工具/方式 |
|---|---|---|
| 单元 | 键编码、配额、sieve 动作、状态机转移、解析器 | `go test` + 表驱动 |
| 契约 | 目录契约对照、认证边界、auth-results 格式 | 与真实 backend 的 contract tests |
| 协议一致性 | IMAP（go-imap v1 客户端矩阵）、SMTP（互操作矩阵）、JMAP 官方套件 | 内存存储 + 真实套件 |
| 存储 | 双后端同一套测试（参数化）、崩溃恢复、故障注入 | kill -9 / fs 故障注入 |
| E2E | mailez 现有 e2e + mailezine 双后端矩阵 | docker compose + `go run ./cmd/e2e` |

### 11.2 强制质量门（CI + `make verify`）

- `go vet`、`gofmt`、`staticcheck`
- **依赖方向检查**（go-arch-lint 或等价）：违反 §1 依赖规则即失败
- `go test -race ./...`
- 覆盖率门（核心包不低于阈值，初版 ≥ 70%）
- fuzz smoke：SMTP/IMAP/MIME/Sieve 解析器各跑短时 fuzz
- 许可检查：vendor 代码版权头、NOTICE、THIRD_PARTY_NOTICES.md 存在

### 11.3 稳定性工具

- 双后端一致性对拍：同一操作序列在 maildir 与 RocksDB 后端分别执行，结果哈希一致。
- 崩溃测试矩阵：在每阶段注入 `os.Exit`/SIGKILL，重启后校验 INV-* 不变式。

## 12. 演进与兼容

- 存储格式版本化（`meta.schema_version`）；升级脚本与迁移工具随版本发布。
- 配置字段一旦发布即受兼容约束：只加不改删，弃用至少保留一个版本。
- 与传统栈的行为差距以兼容矩阵跟踪（webmail 功能、IMAP 扩展、Sieve 扩展、
  POP3、配额、转发/SRS），MVP 切换只允许"已枚举、可接受"的差距。
- 回退路径：切换前保留传统栈副本；maildir 后端本身零拷贝，回退成本最低。

## 13. 决策记录（ADR）

| ADR | 主题 | 决定 | 后果 |
|---|---|---|---|
| ADR-001 | 存储抽象 | KV + Blob 双接口，逻辑模型 account/collection/document | 协议层不感知后端；双后端成本显式化 |
| ADR-002 | 认证信任边界 | gateway 前置认证，引擎信任受信子网 | 与传统栈现状一致；裸部署需显式直连认证 |
| ADR-003 | 出站队列 | KV spool + 事件驱动调度 + 状态机 | 单实例内可靠；多实例协调留 v2 |
| ADR-004 | maildir 保真 | 一等后端，uidlist/keywords 既有格式兼容 | 存量零迁移；单实例单写者约束 |
| ADR-005 | IMAP 基座 | go-imap/v2 或自研，上游语义/测试参考 | 面收敛到 §6.2 表格；互操作套件兜底 |
| ADR-006 | Sieve 子集 | v1 交付 mailez 模板子集，v2 全量 | 筛选编辑器可用；正确性风险有界 |
| ADR-007 | 上游复用边界 | **已废弃（D43/D44）**：上游邮件服务器全量移除；协议组件自研或 go-msgauth/go-imap/miekg-dns | 无上游依赖，许可证面更干净 |

## 14. 不变式总表（Invariants Wall）

评审与测试引用编号，不重复论证：

| 编号 | 不变式 | 强制点 |
|---|---|---|
| INV-UID | UID 单调、uidnext 只前进、uidvalidity 稳定 | store 分配器 + 契约测试 |
| INV-BLOB | blob 不可变、引用守恒、孤儿可回收 | 写批 + GC 测试 |
| INV-QUOTA | 用量守恒、写前预检 | 配额层 + 故障注入 |
| INV-CHANGE | change log 单调无空洞 | 事务封装 + 崩溃测试 |
| INV-DELIVERY | 消息恰好一个归宿（投递/队列/退信） | 管道状态机 + 双写测试 |
| INV-AUTH | 认证唯一真源是 backend；引擎无密码 | 认证边界测试 |
| INV-CONN | 资源上限恒成立（连接/大小/速率） | limits 包 + 压测 |
| INV-DIR | 目录缓存与 backend 最终一致 | 缓存失效测试 |
| INV-FS | 路径永不越界（账户/文件夹名） | 白名单 + 路径测试 |
| INV-OBS | 每消息有日志链与指标；日志无正文/密钥 | 审计测试 |
