# 基准测试结果：mailezine vs postdove（10,000 人企业邮箱，MySQL 生产形态）

> 方法与环境见 [benchmark.md](./benchmark.md)。本页为可发布结果。
> 本轮测试按生产配置使用 **MySQL 8.0**（开发默认 SQLite 不参与对比），
> 两栈共用同一个 mailez backend 控制面。

## 环境

- 单机：14 vCPU / 32 GiB（Docker Desktop，Windows 宿主）
- 两栈均为 Linux 容器，存储使用 Docker volume（Linux VM 磁盘）
- **共享控制面（两栈共用）**：mailez backend（MySQL 8.0 + Redis），
  10,000 用户 seed；postdove 与 mailezine 都通过 backend 的目录/认证 API
  工作，MySQL/Redis 不构成任一引擎的独有负担
- postdove：nginx gateway + legacy MTA + legacy IMAP（maildir），rspamd milter
  禁用（共享组件，另行验证）
- mailezine：单容器（Pebble KV + MinIO S3 blob，`mailez` 目录/认证模式）
- 同一负载工具（`cmd/bench`）驱动，两栈同机分时运行
- 消息：16 KiB 纯文本（企业典型）；灌信模拟外部发件人 → 本地用户；
  提交为认证用户互发；队列测试经内置 mock MX 接收（legacy MTA relayhost /
  mailezine FixedHost 双向同构）
- 多用户轮询（`-users 1000`）：提交/IMAP 按用户循环，避免单用户
  per-user 连接数与发信限流扭曲并发结果；IMAP 会话 100 并发对应
  100 个不同用户、每用户信箱至少 1 封真实邮件

## 吞吐与延迟（MySQL backend）

| 指标 | postdove | mailezine | 比值 |
|---|---|---|---|
| 收件吞吐（20 并发，200 封） | 9.1 msg/s | **20.3 msg/s** | 2.2× |
| 收件 p50 延迟 | 2.04 s | **997 ms** | 2.0× 快 |
| 收件 p95 延迟 | 3.60 s | **1.44 s** | 2.5× 快 |
| 提交吞吐（20 并发，100 封，1000 发件人） | 3.1 msg/s | **5.9 msg/s** | 1.9× |
| 提交 p50 延迟 | 6.27 s | **3.21 s** | 2.0× 快 |
| 提交 p95 延迟 | 8.18 s | **4.25 s** | 1.9× 快 |
| 8 小时日收件等价量 | 4,391 封 | **9,739 封** | 2.2× |
| 8 小时日发件等价量 | 1,475 封 | **2,847 封** | 1.9× |

> 收件 = 入站 SMTP 投递（外部发件人 → 本地用户）；提交 = AUTH 认证提交
> （本地用户互发）。两者都走完整队列路径。认证路径两栈对称：都经过
> backend `/stack/auth/email`（bcrypt cost 12 + MySQL 查库 + Redis 限流），
> 该公共开销计入双方延迟，mailezine 仍快约 2×。

## IMAP 并发会话与 FETCH（100 并发，30 秒）

| 指标 | postdove | mailezine |
|---|---|---|
| 会话建立 | 100/100（0 失败） | 100/100（0 失败） |
| FETCH p50 | 43.0 ms | **19.4 ms（2.2× 快）** |
| FETCH p95 | 99.7 ms | **38.0 ms** |
| FETCH 聚合吞吐 | 798 ops/s | **4,580 ops/s（5.7×）** |

> 首轮单用户 100 并发在 postdove 上出现 55/100 登录失败，根因是 legacy IMAP
> `mail_max_userip_connections=20` 的单用户连接上限，不是引擎故障；改用
> 100 个不同用户后两栈均 100/100。这正是多用户轮询的意义。

## 队列性能（200 封外部投递，mock MX 接收）

| 指标 | postdove | mailezine | 比值 |
|---|---|---|---|
| 提交速率 | 3.5 msg/s | **6.0 msg/s** | 1.7× |
| 投递速率（mock MX） | 2.7 msg/s | **5.8 msg/s** | 2.1× |
| 入队→投递 p50 | 29.4 s | **5.09 s** | 5.8× 快 |
| 入队→投递 p95 | 51.1 s | **6.36 s** | 8.0× 快 |
| 最大滞留队列深度 | 127 封 | **0 封** | — |
| 投递成功率 | 100%（200/200） | 100%（200/200） | — |

> postdove 的入队→投递延迟主要由 legacy MTA 队列扫描/投递调度与 legacy IMAP
> submission 中继链贡献；mailezine 的 KV 队列（poll 1s）几乎实时外发，
> 队列不积压。

## 内存 RSS

| 状态 | postdove（gateway+legacy MTA/IMAP stack 合计） | mailezine（1 容器） | 比值 |
|---|---|---|---|
| 空闲 | ~254 MiB（gateway 81 + legacy MTA 62 + legacy IMAP 111） | **19.4 MiB** | 13× 省 |
| 负载（20 并发灌信 + 100 IMAP） | ~309 MiB（gateway 87 + legacy MTA 111 + legacy IMAP 111） | **23.0 MiB** | 13.4× 省 |

> 共享控制面（backend/MySQL/Redis）与 rspamd 不计入任何一方，两栈对称。
> mailezine 单进程承载全部协议，负载下内存增长 < 4 MiB；postdove 的
> legacy MTA 队列与 legacy IMAP 索引进程在负载下各增长数十 MiB。

## 存储 IO 与落盘

| 指标 | postdove | mailezine |
|---|---|---|
| 消息落盘形态 | maildir 小文件 + legacy IMAP 索引 | Pebble KV（元数据）+ MinIO blob（正文） |
| 落盘体积（同规模邮件集） | ~73 MB（maildir volume） | Pebble ~0.8 MB + blob ~17.7 MB |
| 负载期写 IO（docker stats BlockIO） | legacy IMAP 写 ~102 MB | MinIO 写 ~29.6 MB（blob） |

> 同一批消息，mailezine 全链路（KV 元数据 + S3 正文）落盘约 postdove
> maildir+legacy IMAP 索引的 1/4，且 blob 天然可水平扩展（S3/MinIO），不绑定
> 单机磁盘。

## 测试中发现并解决的问题

1. **`cmd/bench` flag 解析缺陷**：Go 标准 flag 在第一个非 flag 参数处停止
   解析，`bench verify -smtp ...` 会把 `-smtp` 当普通参数，静默打到默认
   端口（dev 栈 1587/143）而不是 bench 栈，导致假失败。已修复：子命令
   可写在 flag 前或后（`bench seed -conns 20` 与 `bench -conns 20 seed`
   等价）。
2. **单用户并发失真**：100 个 IMAP 会话全用同一用户时，legacy IMAP
   `mail_max_userip_connections=20` 导致 55% 登录失败；单发件人 200 封
   提交会触发 backend 每用户发信限流（`450 too many emails too fast`）。
   已为 bench 增加 `-users` 多用户轮询与 `-seq` 顺序灌信。
3. **queue 模式缺失且无法确定性投递**：原 bench 文档列了 queue 但没有
   实现。已实现内置 mock MX（go-smtp server）+ Message-ID 逐封关联的
   入队→投递延迟统计；legacy MTA 侧 relayhost、mailezine 侧新增
   `MAILEZINE_OUTBOUND_FIXED_HOST/PORT`（smarthost 中继）使两栈都投递到
   同一个 mock MX，路径同构。
4. **mailez backend MySQL 兼容**：`User` 模型日期字段为非空 `time.Time`，
   Go 零值渲染为 `0000-00-00`，MySQL 8 严格模式拒绝写入。改为
   `*time.Time`（可空）并同步 `parseUserDate`/`ReplyActive`/S/MIME 使用点，
   MySQL 10k 用户 seed 通过（sqlite 开发路径回归测试通过）。
5. **IMAP FETCH 空信箱失真**：FETCH seq 1 对空信箱是近乎零开销的空操作；
   改为先按序给前 100 用户各灌 1 封，再测真实信封读取延迟。

## 结论

- **吞吐与延迟**：MySQL 生产形态下 mailezine 在收件（2.2×）、提交
  （1.9×）、IMAP FETCH 聚合（5.7×）、队列投递（2.1×）上全面占优；
  入队→投递延迟 5.8× 更快且队列零积压。
- **资源占用**：mailezine 内存为 postdove 引擎容器的 1/13（空闲/负载），
  且为单容器部署；存储落盘约 1/4，blob 层可水平扩展。
- **差距来源**：postdove 的多进程架构（nginx 代理 + legacy IMAP 登录代理 +
  legacy MTA 队列）+ maildir 小文件 IO + 每次认证/查询的跨进程往返；
  mailezine 单进程内完成认证、路由、存储（Pebble KV + S3 blob 流式），
  队列状态机在 KV 中事务化。两栈的认证都经过共享 backend（bcrypt +
  MySQL），该公共开销对称计入，不偏袒任一方。
- **诚实声明**：单机 Docker Desktop 环境下两栈均未达到 10k 用户模型
  的目标量（日收件 320k / 日发件 120k）——瓶颈主要在认证链（bcrypt
  cost 12 + HTTP 往返）与单机资源；Linux 裸机 + 更多核 + 水平扩展
  （mailezine 支持 Pebble/MinIO 拆分与 S3 横向扩容）会显著提高。

复现：`cmd/bench`（仓库内）+ `deploy/docker-compose.bench-postdove.yml`
（postdove 隔离栈，MySQL backend 宿主机运行）+ mailezine 单容器按
benchmark.md §4 启动。
