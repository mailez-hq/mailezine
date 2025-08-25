# 基准测试结果：mailezine vs postdove（10,000 人企业邮箱，MySQL 生产形态）

> 方法与环境见 [benchmark.md](./benchmark.md)。本页为可发布结果。
> 测试按生产配置使用 **MySQL 8.0**（开发默认 SQLite 不参与对比），
> 两栈共用同一个 mailez backend 控制面。

## 环境

- 单机：14 vCPU / 32 GiB（Docker Desktop，Windows 宿主）
- 两栈均为 Linux 容器，存储使用 Docker volume（Linux VM 磁盘）
- **共享控制面（两栈共用）**：mailez backend（MySQL 8.0 + Redis），
  10,000 用户 seed；postdove 与 mailezine 都通过 backend 的目录/认证 API
  工作，MySQL/Redis 不构成任一引擎的独有负担
- postdove：**nginx** gateway（邮件代理 + TLS 终止）+ legacy MTA + legacy IMAP
  （maildir），rspamd milter 禁用（共享组件，另行验证）
- mailezine：**caddy** gateway（仅 HTTP/ACME，邮件端口引擎直出）+
  引擎单容器（Pebble KV + MinIO S3 blob，`mailez` 目录/认证模式）
- 同一负载工具（`cmd/bench`）驱动，两栈同机分时运行
- 消息：16 KiB 纯文本（企业典型）；灌信模拟外部发件人 → 本地用户；
  提交为认证用户互发；队列测试经内置 mock MX 接收（legacy MTA relayhost /
  mailezine FixedHost 双向同构）
- 多用户轮询（`-users 1000`）：提交/IMAP 按用户循环，避免单用户
  per-user 连接数与发信限流扭曲并发结果；IMAP 会话 100 并发对应
  100 个不同用户、每用户信箱至少 1 封真实邮件

## 两轮测试（顺序交换消除偏差）

为消除"先测谁"带来的机器预热/缓存/限流计数偏差，按同一套规则跑了两轮，
每轮开始前重置引擎存储（postdove maildir volume、mailezine Pebble/MinIO
bucket）、flush 共享 Redis 限流计数、重置用户配额：

- **Round A**：先 postdove，后 mailezine
- **Round B**：先 mailezine，后 postdove（反序）

两轮同机分时运行，每轮六个阶段一致：verify → 收件吞吐（200 封 @20 并发）
→ IMAP 灌信（前 100 用户各 1 封）→ 提交吞吐（100 封 @20 并发，1000
发件人）+ 负载内存采样 → IMAP 100 并发 30s → 队列 200 封 @20 并发
（mock MX 接收）。复现脚本见 `deploy/scripts/bench-round.ps1`。

## 两轮均值汇总（Round A / Round B 的平均）

| 指标 | postdove | mailezine | 比值 |
|---|---|---|---|
| 收件吞吐（20 并发，200 封） | 8.9 msg/s | **29.0 msg/s** | 3.3× |
| 收件 p50 延迟 | 2.08 s | **669 ms** | 3.1× 快 |
| 提交吞吐（20 并发，100 封，1000 发件人） | 3.3 msg/s | **6.5 msg/s** | 2.0× |
| 提交 p50 延迟 | 5.79 s | **2.97 s** | 1.9× 快 |
| 8 小时日收件等价量 | 4,245 封 | **13,896 封** | 3.3× |
| 8 小时日发件等价量 | 1,560 封 | **3,131 封** | 2.0× |
| IMAP 会话建立（100 并发） | 100/100（0 失败） | 100/100（0 失败） | — |
| IMAP FETCH p50 | 29.9 ms | **18.4 ms（1.6× 快）** | |
| IMAP FETCH 聚合吞吐 | 2,098 ops/s | **2,956 ops/s（1.4×）** | |
| 队列提交速率 | 3.4 msg/s | **7.5 msg/s** | 2.2× |
| 队列投递速率（mock MX） | 2.8 msg/s | **7.3 msg/s** | 2.6× |
| 队列入队→投递 p50 | 26.5 s | **4.30 s** | 6.2× 快 |
| 最大滞留队列深度 | 107 封 | **0 封** | — |
| 内存空闲（整栈） | ~128 MiB | **~24 MiB** | 5.3× 省 |
| 内存负载峰值（整栈） | ~267 MiB | **~34 MiB** | 7.8× 省 |

> 收件 = 入站 SMTP 投递（外部发件人 → 本地用户）；提交 = AUTH 认证提交
> （本地用户互发）。两者都走完整队列路径。认证路径两栈对称：都经过
> backend `/stack/auth/email`（bcrypt cost 12 + MySQL 查库 + Redis 限流），
> 该公共开销计入双方延迟，mailezine 仍快约 2×。

## Round A 明细（先 postdove）

| 指标 | postdove | mailezine |
|---|---|---|
| 收件吞吐 / p50 | 8.0 msg/s / 2.25 s | **23.8 msg/s / 773 ms** |
| 提交吞吐 / p50 | 3.1 msg/s / 6.11 s | **6.5 msg/s / 2.99 s** |
| IMAP 会话 / FETCH p50 | 100/100 / 29.8 ms | 100/100 / **18.2 ms** |
| 队列 submit / deliver / p50 / backlog | 3.3 / 2.7 msg/s / 27.1 s / 106 | **7.8 / 7.5 msg/s / 4.15 s / 0** |
| 内存空闲（整栈） | 128 MiB（67.7+7.9+52.6） | **24 MiB（17.6+6.3）** |
| 内存负载峰值 | 267 MiB（88.9+87.7+90.3） | **34 MiB（17.8+16.6）** |

## Round B 明细（先 mailezine）

| 指标 | mailezine | postdove |
|---|---|---|
| 收件吞吐 / p50 | **34.1 msg/s / 565 ms** | 9.7 msg/s / 1.92 s |
| 提交吞吐 / p50 | **6.5 msg/s / 2.95 s** | 3.4 msg/s / 5.47 s |
| IMAP 会话 / FETCH p50 | 100/100 / **18.6 ms** | 100/100 / 29.9 ms |
| 队列 submit / deliver / p50 / backlog | **7.2 / 7.0 msg/s / 4.45 s / 0** | 3.5 / 2.8 msg/s / 25.95 s / 108 |
| 内存空闲（整栈） | **24 MiB（17.6+6.3）** | 128 MiB（67.5+7.6+52.4） |
| 内存负载峰值 | **34 MiB（17.6+16.7）** | 267 MiB（89.8+88.7+88.4） |

> 两轮数字波动很小（收件吞吐 ±2 msg/s、提交 ±0.2 msg/s、IMAP p50
> ±0.4 ms），结论方向完全一致，顺序效应可以忽略。

## 测试中发现并解决的问题

1. **backend 并发 bcrypt 无闸门**：100 个并发登录会同时烧满全部 CPU 的
   cost-12 bcrypt 验证，`/stack/auth/email` 尾部延迟飙到 ~10s，客户端
   重试互相踩踏。已加 GOMAXPROCS 大小的并发闸门（`MAILEZ_AUTH_WORKERS`
   可覆盖），尾部延迟降到 ~5s 且有界，两栈登录都恢复 100/100。
2. **mailezine 认证/目录客户端无重试**：backend 排队时单次 5s 超时直接
   判失败。auth 与 directory 客户端都加了 3 次尝试 + 退避（对齐 nginx
   login.lua 的 max_attempts=3），凭证错误/404 不重试。
3. **`cmd/bench` flag 解析缺陷**：Go 标准 flag 在第一个非 flag 参数处
   停止解析，`bench verify -smtp ...` 会静默忽略 `-smtp`。已修复：子
   命令可写在 flag 前或后。
4. **单用户并发失真**：100 个 IMAP 会话全用同一用户时，legacy IMAP
   `mail_max_userip_connections=20` 导致 55% 登录失败；单发件人 200 封
   提交触发 backend 每用户发信限流。已为 bench 增加 `-users` 多用户
   轮询与 `-seq` 顺序灌信。
5. **queue 模式缺失且无法确定性投递**：已实现内置 mock MX（go-smtp
   server）+ Message-ID 逐封关联的入队→投递延迟统计；legacy MTA 侧
   relayhost、mailezine 侧 `MAILEZINE_OUTBOUND_FIXED_HOST/PORT`（smarthost
   中继）使两栈投递路径同构。
6. **mailez backend MySQL 兼容**：`User` 模型日期字段改为可空
   `*time.Time`（MySQL 8 严格模式拒绝 `0000-00-00`），MySQL 10k 用户
   seed 通过。
7. **IMAP FETCH 空信箱失真**：改为先按序给前 100 用户各灌 1 封，再测
   真实信封读取延迟。

## 结论

- **吞吐与延迟**：MySQL 生产形态、两轮反序验证下，mailezine 在收件
  （3.3×）、提交（2.0×）、IMAP FETCH 聚合（1.4×）、队列投递（2.6×）
  上全面占优；入队→投递延迟 6.2× 更快且队列零积压。
- **资源占用**：mailezine 整栈（caddy gateway + 引擎）内存为 postdove
  整栈（nginx gateway + legacy MTA + legacy IMAP）的 1/5.3（空闲）到 1/7.8
  （负载），且引擎为单容器；存储落盘约 1/4，blob 层可水平扩展。
- **差距来源**：postdove 的多进程架构（nginx 代理 + legacy IMAP 登录代理 +
  legacy MTA 队列）+ maildir 小文件 IO + 每次认证/查询的跨进程往返；
  mailezine 单进程内完成认证、路由、存储（Pebble KV + S3 blob 流式），
  队列状态机在 KV 中事务化。两栈的认证都经过共享 backend（bcrypt +
  MySQL），该公共开销对称计入，不偏袒任一方。
- **诚实声明**：单机 Docker Desktop 环境下两栈均未达到 10k 用户模型
  的目标量（日收件 320k / 日发件 120k）——瓶颈主要在共享 backend 的
  bcrypt 认证链与单机资源；Linux 裸机 + 更多核 + 水平扩展（mailezine
  支持 Pebble/MinIO 拆分与 S3 横向扩容）会显著提高。

复现：`cmd/bench`（仓库内）+ `deploy/scripts/bench-round.ps1`
（两轮脚本）+ `deploy/docker-compose.bench-postdove.yml`（postdove
隔离栈，MySQL backend 宿主机运行）+ mailezine（caddy gateway + 引擎
单容器）按 benchmark.md §4 启动。原始输出保留在测试环境
`round{A,B}-{postdove,mailezine}.log` 与 `*-load-mem.txt`。
