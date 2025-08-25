# 基准测试结果：mailezine vs postdove（10,000 人企业邮箱）

> 方法与环境见 [benchmark.md](./benchmark.md)。本页为可发布结果。

## 环境

- 单机：14 vCPU / 32 GiB（Docker Desktop，Windows 宿主）
- 两栈均为 Linux 容器，存储使用 Docker named volume（Linux VM 磁盘）
- postdove：nginx gateway + legacy MTA + legacy IMAP（maildir）+ rspamd（基准中禁用 milter，见下）+ backend（sqlite）+ redis
- mailezine：单容器（Pebble KV + MinIO S3 blob，dev 目录桩）
- 10,000 用户，同一负载工具（`cmd/bench`）驱动，两栈同机分时运行
- 消息：16 KiB 纯文本（企业典型）；收件模拟外部发件人 → 本地用户，提交为认证用户互发

## 吞吐与延迟

| 指标 | postdove | mailezine | 比值 |
|---|---|---|---|
| 收件吞吐（20 并发，200 封） | 11.3 msg/s | **63.5 msg/s** | 5.6× |
| 收件 p50 延迟 | 1.63 s | **286 ms** | 5.7× 快 |
| 收件 p95 延迟 | 2.62 s | **437 ms** | 6.0× 快 |
| 提交吞吐（20 并发，100 封） | 5.0 msg/s | **49.3 msg/s** | 9.9× |
| 提交 p50 延迟 | 3.66 s | **370 ms** | 9.9× 快 |
| 8 小时日收件等价量 | 5,414 封 | **30,500 封** | 5.6× |

> 收件 = 入站 SMTP 投递（外部发件人 → 本地用户）；提交 = AUTH 认证提交（本地用户互发）。两者都走完整队列路径。

## 并发连接

| 指标 | postdove | mailezine |
|---|---|---|
| IMAP 并发会话（100 连接） | 100 建立，8.8 session/s | **100 建立，370 session/s（42×）** |
| IMAP FETCH p50（优化后） | 33 ms | **26 ms** |
| SMTP 并发（测试集） | 20 | 20 |

mailezine 的 IMAP 会话建立速率是 postdove 的 42 倍；单次 FETCH 读延迟在
读路径优化后（按需下载 + envelope/body-structure LRU + SELECT 快照复用）
从 235 ms 降至 26 ms，已反超 postdove（33 ms）。

## 内存 RSS

| 状态 | postdove（3 容器合计） | mailezine（1 容器） | 比值 |
|---|---|---|---|
| 空闲 | 176 MiB（legacy IMAP 94 + gateway 71 + legacy MTA 12） | **37 MiB** | 4.8× 省 |
| 负载（50 并发灌信） | ~290 MiB（legacy IMAP 107 + legacy MTA 110 + gateway 76） | **33 MiB** | 8.8× 省 |

mailezine 单容器承载全部协议，负载下内存几乎不增长；postdove 的 legacy MTA 队列与 legacy IMAP 索引在负载下各增加 ~100 MiB。

## 测试中发现并解决的问题

测试过程暴露了 3 个 postdove 栈（mailez）问题与 2 个工具问题，均已修复：

1. **nginx 反解析客户端 IP 4 秒超时**（gateway）：Docker 内嵌 DNS 对 PTR 查询慢；resolver 指向外部 DNS 后 greeting 从 4000ms 降至 42ms。
2. **rspamd milter 在隔离环境 DNS 查询超时**（每封 4.5-20s）：基准中禁用 milter（`/overrides/postfix.cf`），两栈对称；rspamd 作为共享组件单独验证。
3. **backend 的 `MAILEZ_SUBNET` 默认网段与测试栈不一致**导致 legacy IMAP 认证失败；显式配置后修复。
4. **bench 收件人负随机数**（`u-5346@example.com`）与 **body 换行失效**（`Len()%80` 可能永不触发导致整封一行）——工具缺陷，已修。

## 结论

- **吞吐与延迟**：mailezine 在收件、提交、IMAP 会话建立上全面占优（5.6×/9.9×/42×）。
- **资源占用**：mailezine 内存为 postdove 的 1/5（空闲）到 1/9（负载），且为单容器部署。
- **差距来源**：postdove 的多进程架构（nginx 代理 + legacy IMAP 登录代理 + legacy MTA 队列）+ maildir 小文件 IO + 每次认证/查询的跨进程往返；mailezine 单进程内完成认证、路由、存储（Pebble KV + S3 blob 流式），队列状态机在 KV 中事务化。
- **mailezine 已优化项**：IMAP FETCH 读路径——按需下载 blob（UID/FLAGS/
  SIZE 等元数据零下载）、envelope/body-structure LRU 缓存（8192 条目）、
  FETCH 复用 SELECT 快照；235 ms → 26 ms。

复现：`cmd/bench`（仓库内）+ `deploy/docker-compose.bench-postdove.yml`（postdove 隔离栈）；mailezine 单容器按 benchmark.md §4 启动。
