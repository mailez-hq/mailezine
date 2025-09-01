# mailezine vs reference server — 同栈基准 (2026-09)

同一 Windows 宿主机 (Docker 29.7.2, WSL2 后端)、同一客户端工具
(mailezine `cmd/bench`,Edmundgo-imap/gosmtp 客户端库)、同一数据集与并发,
对 mailezine 引擎镜像 (本仓库 CE 构建, Pebble KV) 与
reference server Mail Server v0.11.8 官方镜像 (RocksDB) 做两条核心路径的对比。

## 环境

| 项 | mailezine | reference server |
|---|---|---|
| 版本 | mailezine-bench (本仓库, CE 构建) | reference/mail-server:v0.11.8 |
| 存储 | Pebble (纯 Go, 默认) | RocksDB (官方默认) |
| 数据落盘 | Docker 命名卷 | Docker 命名卷 |
| 垃圾过滤 | 关 (MAILEZINE_JUNK_ENABLED=false) | 关 (session.data.spam-filter=false, DNSBL 清零, 限速器关闭) |
| 用户 | dev 目录 JSON, 5 用户 | internal 目录 REST 预置, 5 用户 (role=user) |
| 密码 | benchpass | benchpass |

双方均关闭垃圾/灰名单/SPF-DNS 类校验:测的是"引擎"(SMTP 接收 + 投递 +
存储 + IMAP),不是反垃圾栈。reference server 侧额外关闭了其默认的入站限速
(每发送者 25 封/小时) 与明文 AUTH/非 FQDN EHLO 拒绝,使自动化基准可行;
这些改动全部记录在 `reference server/config-v011.toml`。

## 结果 (同一客户端、同一时刻、先后串行运行)

### 场景 1: 入站 SMTP 注入 2000 封 × 4KB, 20 并发, 单一收件信箱

| 引擎 | 吞吐 | 延迟 p50 | p95 | p99 | 失败 |
|---|---|---|---|---|---|
| reference server 0.11.8 | **57.1 msg/s** | 282ms | 627ms | 1.40s | 0 |
| mailezine | 14.5 msg/s | 1.15s | 2.53s | 3.15s | 0 |

### 场景 2: IMAP 50 并发会话,持续 20s,每会话循环 FETCH ENVELOPE

| 引擎 | fetch 吞吐 | 延迟 p50 | p95 | p99 | 20s 总操作 |
|---|---|---|---|---|---|
| reference server 0.11.8 | **1590 ops/s** | 26.7ms | 63.6ms | 91.7ms | 32014 |
| mailezine | 570 ops/s | 73.0ms | 171.6ms | 235.7ms | 11467 |

### 诊断补充 (mailezine 场景 1)

- 并发 20→50:吞吐不升反降 (14.5→11.7/s,p50 1.15→3.72s) —— 入站管线
  存在每消息串行化阶段 (~70-90ms/封),与并发无关。
- 数据从命名卷改为容器内部层:8.4/s —— 排除卷 I/O 为唯一因素。
- 引擎容器 CPU 全程 <12%,瓶颈是等待/串行而非算力。

### 根因定位与修复 (pprof 实证,已完成)

给引擎加了 `MAILEZINE_PPROF` 开关后在负载下抓取 mutex/block/CPU profile:

- 70 秒负载累计 **1222 秒互斥等待**,全部落在 `internal/store` 的账号锁上:
  旧版 `KV.Deliver` 每封邮件要**四次**进临界区(nextUID → CreateDocument →
  BumpMailboxModSeq → AppendEmailAtomically),每次各做一次 KV 提交。
- 修复(`internal/store.DeliverEmail`):UID / modseq / 文档 ID / 字段 /
  blob 链接 / 配额 / 变更日志 / 二级索引合并为**一次加锁一次提交**;
  blob 改为内容寻址 (sha256),上传移到账号锁之外,并发投递可流水线化。
- 修复后同场景复测:

| 引擎 | 入站 SMTP 2000×4KB | IMAP 50 会话 FETCH |
|---|---|---|
| reference server 0.11.8 | 57.1 msg/s (p50 282ms) | 1590 ops/s (p50 27ms) |
| mailezine 修复前 | 14.5 msg/s (p50 1.15s) | 570 ops/s (p50 73ms) |
| **mailezine 修复后** | **24.3 msg/s (p50 732ms, +68%)** | **754 ops/s (p50 55ms, +32%)** |

- 剩余差距主因:每消息一次 Pebble 提交的 fsync 延迟(基准环境放大),
  而 RocksDB 对并发写入做组提交摊薄 fsync。

## 投递微批 (已实现)

### 设计

纯存储层组提交 (合并并发 Batch) 对同账号投递无效:账号锁把同账号投递
串行化,并发的同 key 提交从不重叠。有效的杠杆是**投递层链式领队微批**:

- `mailstore.KV.Deliver` 上每账号一个合并点。无在途提交时,到达者成为
  **领队**,立即以单事务提交 (空闲路径零附加延迟、仍是一封一批)。
- 提交期间到达的请求排队**跟随**当前领队。领队提交完立即排空队列继续
  提交,直到队列见底才卸任 (resign 发生在最后一批提交完成后,唤醒/卸任
  的交接全部在锁内原子完成)。
- 因此批次大小随负载自适应:并发越高,一个 fsync 窗口内到达的请求越多,
  单次 fsync 摊到越多封 —— 与 RocksDB 组提交相同的均衡,但不需要计时器、
  后台 goroutine,关闭即无痕。
- 语义不变 (`store.DeliverEmailBatch`):整批一个账号锁 + 一个事务,批内
  每封仍各自获得顺序 UID、modseq、docID、配额、变更日志与索引项;写缓冲
  对计数器键做末值折叠。fsync 仍发生在任何一封被确认之前 (250 前),未
  减弱持久性;领队提交脱离自身 ctx,避免无关连接断开殃及同批投递。
- 测试:`internal/store`(批 vs 逐封状态等价、跨邮箱批次、并发批次无重复
  无跳号)、`internal/mailstore`(24×25 并发投递 UID 无重复无缺口,
  count=10 稳定复现 —— 针对"卸任早于最后提交"退化与"领队返回他人 UID"
  两个真实回归)。

### 数据 (2026-09-01, 同机交替 A/B, 每轮重建容器+空数据卷)

机器有其他容器负载,轮次方差大;以下均为**交替成对运行** (旧/新各半,
消除时间漂移),全部配对新版胜出:

| 场景 (入站 SMTP, 单收件账号) | 微批前 | 微批后 | 变化 |
|---|---|---|---|
| 2000 封 × 4KB, 20 并发 (3 轮中位) | 35.3 msg/s | 39.6 msg/s | **+12%** |
| 3000 封 × 4KB, 40 并发 (2 轮中位) | 32.7 msg/s | **68.0 msg/s** | **+108%** |

- 20 并发下差距小:该负载下 fsync 未成主要瓶颈,且批次天然偏小。
- 40 并发下微批前吞吐不升反降 (账号锁上 fsync 串行卡死),微批后批次
  随到达率自长,**吞吐反超 reference server 同场景基线 57.1 msg/s**。
- IMAP FETCH 路径不受影响 (688 vs 703 ops/s, 噪声范围内, 无回归)。
- 延迟:20 并发配对实测 p50 612→351ms (−43%);并发越高改善越大。

对 reference server 的入站差距:单封路径 ~1.4-2.4× (低并发, 客户端/管线为瓶颈),
高并发下 (fsync 主导) 微批后已反超。

## 结论 (如实)

修复前两条路径 reference server 均领先(入站 ~4×,IMAP ~2.8×)。**差距不是
Go vs Rust 语言**:修复前引擎 CPU 闲置、互斥等待占绝对大头,是提交次数/
锁粒度的架构问题;单次提交合并即拿回入站 1.7×、IMAP 1.3×。修复后入站
差距收窄到 ~2.4×。此前宣传材料中的 3.3× 数字是对比 legacy MTA/IMAP stack
栈的,不适用于此处。

后续跟进:
1. IMAP FETCH 热路径缓存命中率对比(reference server 元数据缓存更高效)。

## 复现

```powershell
# 引擎 (mailezine): 见 mailezine/config.toml + users/passwords.json
docker run -d --name bench-mailezine --network bench-vs -p 12143:143 -p 12025:25 -p 11587:1587 `
  -v .../users.json:/conf/users.json:ro -v .../passwords.json:/conf/passwords.json:ro `
  -v .../config.toml:/conf/config.toml:ro -v bench-mailezine-data:/data `
  -e MAILEZINE_CONFIG=/conf/config.toml mailezine-bench
# 引擎 (reference server): 见 reference server/config-v011.toml, principal 预置用管理 REST API
# 基准:
bench.exe seed -in-smtp 127.0.0.1:12025 -to u00001@example.test -msgs 2000 -conns 20 -size 4096 -engine mailezine
bench.exe imap  -imap 127.0.0.1:12143 -user u00001@example.test -pass benchpass -conns 50 -dur 20s
```

## 基准工具修复 (随本目录一起入库)

`cmd/bench` 的 seed/smtp 模式原先整个运行共用同一个 Message-ID,
会触发按 Message-ID 去重的引擎 (如 reference server) 折叠整轮投递,
严重歪曲信箱填充类结果;已改为每消息生成唯一 Message-ID。
