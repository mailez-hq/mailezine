# mailezine vs postdove 基准测试方案

> 场景：10,000 人规模企业邮箱，日常工作负载。本方案定义可复现的负载模型、
> 环境与指标采集方式；结果用于官网发布（mailezine = 企业版，postdove =
> 社区版）。

## 1. 负载模型（10k 用户企业）

假设（行业常见取值，可配置）：

| 参数 | 取值 | 依据 |
|---|---|---|
| 用户规模 | 10,000 | 场景定义 |
| 活跃用户占比 | 80%（8,000） | 企业邮箱典型活跃度 |
| 每活跃用户日均收件 | 40 封 | 含通知/群发/事务邮件 |
| 每活跃用户日均发件 | 15 封 | 业务往来 + 群发 |
| 日收件总量 | 320,000 封 | 8,000 × 40 |
| 日发件总量 | 120,000 封 | 8,000 × 15 |
| 工作时间 | 8 小时（480 分钟） | 工作日 |
| 平均消息大小 | 75 KB | 含附件权重（文本 20KB + 附件混合） |

换算为速率：

- 收件：320,000 / 480 ≈ **667 msg/min**（高峰 2× ≈ 1,300 msg/min）
- 发件：120,000 / 480 ≈ **250 msg/min**
- 稳态测试时长：15 分钟（收件 10,000 封、发件 3,750 封等价半小时工作日）

## 2. 并发模型

| 指标 | 取值 |
|---|---|
| SMTP 提交并发连接 | 300（峰值） |
| SMTP 收件并发连接（灌信） | 200 |
| IMAP 并发会话 | 1,000（在线 20% × 连接复用） |
| 出站队列并发投递 | 20 |

## 3. 指标与采集

| 指标 | 采集方式 |
|---|---|
| 每分钟消息 / 日总量 | 负载工具计数（提交成功/投递成功），换算日总量 |
| SMTP 并发连接 | 工具并发数 + `ss` 采样 |
| IMAP 并发连接 | 工具会话数 + `ss` 采样 |
| 队列性能 | mock MX 接收速率、入队→投递延迟、滞留队列深度 |
| 内存 RSS（空闲/负载） | `docker stats --no-stream` 周期采样（空闲 5 分钟、负载期采样） |
| 存储 IO | `docker stats` BlockIO 增量 / `pidstat -d`（容器内） |

## 4. 环境

### 共享控制面（两栈共用，不属任一引擎）

- mailez backend（**MySQL 8.0 生产形态** + Redis，10k 用户 seed）
  - 开发默认 SQLite；基准按生产配置使用 MySQL（`DB_DRIVER=mysql`）。
  - backend 是两栈共同的目录/认证控制面：postdove 的 nginx gateway/
    legacy MTA/IMAP stack 通过 `/stack/*` 内部 API 认证与查目录；mailezine 以
    `MAILEZINE_DIRECTORY_MODE=mailez` / `MAILEZINE_AUTH_MODE=mailez`
    消费同一套 API。
  - Redis 由 backend 用于会话/限流，同样为共享控制面依赖。
- rspamd 为共享组件，基准中两栈均禁用 milter（对称隔离，另行验证）。
- 消息存储隔离：postdove 用 maildir volume，mailezine 用 Pebble KV +
  MinIO blob（S3）。

### 两栈组件对照（对称）

| | postdove（社区版） | mailezine（企业版） |
|---|---|---|
| gateway | **nginx**：邮件代理 + TLS 终止（25/465/587/110/995/143/993/4190） | **caddy**：仅 HTTP/ACME（80/443）；邮件端口由引擎直出，不经过 gateway |
| 引擎 | legacy MTA（队列）+ legacy IMAP（maildir） | mailezine 单容器（Pebble KV + MinIO blob，内化隐式 TLS 465/993/995） |
| 目录/认证 | 共享 backend（MySQL + Redis） | 共享 backend（MySQL + Redis） |
| 存储 | maildir 小文件 + legacy IMAP 索引 | Pebble KV（元数据）+ MinIO blob（正文，S3） |
| rspamd | 禁用（共享组件） | 禁用（与 postdove 对称） |

> **邮件路径对称性**：postdove 的邮件流量必须经过 nginx 邮件代理（认证
> 代理 + TLS 终止），基准打 nginx 端口；mailezine 模式下邮件流量不经过
> caddy（caddy 只服务 HTTP/ACME），引擎直接对外，基准打的引擎端口就是
> 生产路径。内存对比中 gateway 双方都计入（nginx vs caddy）。

两栈同机（14 核 / 32GB），分时运行避免互相干扰；消息落盘路径隔离。

## 5. 测试工具

`cmd/bench`（仓库内，可复现）。flag 可写在子命令前或后（`bench seed
-conns 20` 与 `bench -conns 20 seed` 等价）：

- `seed`：IMAP/SMTP 灌信到本地域
- `smtp`：并发认证提交（per-connection 一封）
- `imap`：并发登录 + SELECT + FETCH 采样
- `queue`：并发提交外部收件人 → 内置 mock MX 记录接收，按 Message-ID
  关联入队→投递延迟，采样滞留队列深度
- `stats`：docker stats / /proc 采样（内存 RSS、块 IO）

## 6. 可复现性

- 固定镜像 tag（mailez/*:local）
- 固定随机种子（收件人/大小分布）
- 多用户轮询（`-users`）：认证提交/IMAP 会话按用户循环，避免
  legacy IMAP `mail_max_userip_connections` 与 backend 每用户发信限流
  把并发测试打成单用户伪瓶颈；`-seq` 让灌信收件人顺序轮询，保证
  IMAP FETCH 每个会话都有真实邮件可读。
- 输出原始数据 + 汇总表
