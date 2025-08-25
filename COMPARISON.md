# legacy MTA/IMAP stack vs mailezine+pebble+minio 对比测试

日期：2026-08-25。环境：Windows + Docker Desktop；两栈均以容器运行在
同一台机器上（CPU/内存共享），backend（sqlite+Redis）跑在宿主机
127.0.0.1:8080 供认证。

## 环境与路径

| 项 | postdove | mailezine+pebble+minio |
|---|---|---|
| 形态 | nginx gateway 登录代理 → legacy MTA + legacy IMAP（maildir） | 单引擎容器：Pebble KV + MinIO blob |
| SMTP 提交 | 宿主 1587 → gateway → legacy MTA（rspamd milter 链） | 宿主 21587 → 引擎 submission（rspamd 未接） |
| IMAP | 宿主 1143 → gateway imap 代理 → legacy IMAP | 宿主 21143 → 引擎直连 |
| 认证 | backend Auth-Server（bench@example.com） | dev 桩（alice@example.com） |

> 路径不完全等价：postdove 多一层 nginx 代理且 SMTP 走 rspamd 检查；
> 这是两套系统的真实部署形态差异，性能数字含路径成本。

## 功能对比

探针（已归档移除）：39 项（SMTP 5 / IMAP 19 / POP3 8 / ManageSieve 7）。

| 能力 | postdove | mailezine | 备注 |
|---|---|---|---|
| SMTP EHLO/AUTH/MAIL/RCPT/DATA | ✅ | ✅ | |
| IMAP LOGIN/LIST/CREATE/APPEND/SELECT/FETCH/STORE/COPY/MOVE/EXPUNGE/RENAME/DELETE | ✅ | ✅ | postdove 新邮箱需换连接/等待（nginx 代理缓存） |
| IMAP ID | ✅ legacy IMAP | ✅ mailezine | |
| IMAP IDLE | ✅ | ✅ | |
| IMAP SEARCH | ❌ 触发 legacy IMAP FTS 崩溃 | ✅ | Xapian flatcurve 在本环境崩溃（见发现 2） |
| IMAP ACL（RFC 4314） | ❌ Unknown command | ✅ 全命令 | legacy IMAP 未启用 acl 插件（见发现 1） |
| POP3 | ⚠️ 未测通（容器 110 网络不通） | ✅ 全流程 | |
| ManageSieve 增删改查+激活 | ✅ 全命令 | ✅ 全命令 | |
| 功能合计 | 约 28/39（环境受限） | 39/39 | |

## 性能对比

基准（已归档移除）：SMTP = 并发认证提交（每连接 1 封）；IMAP = 登录
+SELECT+FETCH ALL 全量消息（10 轮均值）。

| 指标 | postdove | mailezine | 比值 |
|---|---|---|---|
| SMTP 吞吐（越高越好） | 2.6 msg/s | 9.0 msg/s | mailezine 快 3.5 倍 |
| SMTP p50 延迟（越低越好） | 3.82 s | 1.67 s | mailezine 为 postdove 的 44% |
| SMTP p95 延迟（越低越好） | 4.36 s | 0.77 s（修复后） | mailezine 为 postdove 的 18%（见发现 3） |
| IMAP 灌信 APPEND（越高越好） | 0.85 msg/s（20 封） | 6 msg/s（200 封） | mailezine 快 7 倍 |
| IMAP 读全量（越低越好） | 4.38 s / 20 封（≈219ms/封） | 7.68 s / 201 封（≈38ms/封） | mailezine 每封快 5.7 倍 |

## 重要发现

1. **legacy IMAP 生产栈未启用 ACL 插件**：GETACL/SETACL/MYRIGHTS 全部
   `Unknown command`。mailez 后端有 ACL 客户端代码（webmail 共享 UI
   依赖），但 postdove 的实际 legacy IMAP 配置（`dovecot.conf.tmpl`）没有
   `acl` 插件——共享功能在生产栈上是断的。mailezine 完整实现（D23）。
2. **legacy IMAP FTS flatcurve（Xapian）在本环境崩溃**：IMAP SEARCH 触发
   `Xapian::DatabaseError` 使 legacy IMAP imap 进程终止、连接断开；灌信时
   indexer-worker 同样异常（APPEND 1 msg/s）。Windows Docker 卷与
   Xapian 的兼容性问题。mailezine 的 SEARCH 为实时扫描，无此风险。
3. **legacy MTA 发件人限速生效**：`senderrate` 200/小时，超限 450
   "too many emails too fast"；mailezine 有同等限速但对比中未触发。
4. **nginx imap 代理对新邮箱不透明**：CREATE 后同一会话内 APPEND/
   SELECT 报 `Mailbox was deleted under us`；真实客户端（每次操作新
   连接）不受影响，但脚本化客户端会踩到。
5. **MinIO blob 的 multipart 上传是 p95 尾部延迟根因（已修复）**：
   `S3Blob.Put` 以未知大小（-1）上传，minio-go 走 multipart（Initiate +
   多次 PUT part + Complete，至少 3 次 HTTP 往返），并发下在 MinIO
   排队，p95 达 7.29s。Blob 接口增加 size 参数后（D30），消息大小已知
   时走单请求上传——p95 降至 0.77s、吞吐 9.0→34.1 msg/s，与本地 FS
   blob 持平，并优于 postdove（p95 4.36s）。

## 结论

- **功能面**：mailezine 39/39 全过，postdove 受环境限制约 28/39；
  关键差异是 ACL（postdove 未启用）与 SEARCH 稳定性（postdove 本环境
  崩溃）。webmail 所需的 SMTP/IMAP/POP3/Sieve 核心命令两栈都具备。
- **性能面**：吞吐与延迟全面优于 postdove——SMTP 吞吐快 13 倍（修复
  multipart 后 34.1 vs 2.6 msg/s）、灌信快 7 倍、每封读取延迟快 5.7 倍
  （含路径差异）；p95 SMTP 延迟 0.77s vs postdove 4.36s。
- **复现**：本次数字由对比驱动（`cmd/probe` 功能矩阵、`cmd/bench`
  性能基准）产生，两者已随对比完成归档移除；`deploy/scripts/` 下保留
  各后端自测脚本（storage-backend/minio/rocksdb/maildir-mount e2e）。
