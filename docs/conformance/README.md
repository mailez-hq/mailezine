# 客户端一致性回归（IMAP / SMTP）

GA 差距 ① 的交付物：**自动化真实客户端一致性套件**（本目录所述命令）+ **人工矩阵清单**（下表）。
自动化部分在协议层用真实客户端库（go-imap/v2 `imapclient`、go-smtp）打真实端口；
GUI 层（客户端安装、账户向导、界面渲染）无法自动化，由人工矩阵覆盖。

## 一、自动化部分：`cmd/conformance`

前置：一个正在运行的 mailezine 实例（docker compose 暴露 11143/IMAP、11587/submission）
和一个已开通的测试账号（可用 `cmd/e2e-mailez` 或管理台创建）。

```sh
# 全量（IMAP + SMTP，含端到端投递核验）
MAILEZINE_CONFORMANCE_PASS='secret' \
  go run ./cmd/conformance -imap 127.0.0.1:11143 -smtp 127.0.0.1:11587 \
    -user conformance@example.com

# 只跑某一侧
go run ./cmd/conformance -skip-smtp -user u@example.com -imap 127.0.0.1:11143
```

任一检查失败退出码非 0，可直接接入 CI 的部署后冒烟阶段。

### IMAP 检查清单（真实 imapclient）

| 检查 | 验证内容 | 依赖 |
| --- | --- | --- |
| `imap/preauth-capability` | 未认证态声明 IMAP4rev1 与 AUTH=PLAIN | 协议基线 |
| `imap/login` | LOGIN 成功、会话建立 | 认证 |
| `imap/list-inbox` | LIST 返回 INBOX | 基础 |
| `imap/status-inbox` | STATUS 返回 MESSAGES/UIDNEXT/UIDVALIDITY/UNSEEN | 基础 |
| `imap/select-highestmodseq` | SELECT 返回非 0 HIGHESTMODSEQ | CONDSTORE |
| `imap/enable-condstore-qresync` | ENABLE CONDSTORE QRESYNC 双双确认 | RFC 7162 |
| `imap/mailbox-crud` | CREATE/RENAME/RENAME 回/SUBSCRIBE | 文件夹管理 |
| `imap/append-uidplus` | APPEND（带标志+时间）返回 UID | UIDPLUS |
| `imap/fetch-modseq-body` | UID FETCH 返回 MODSEQ 与报文头 | CONDSTORE |
| `imap/store-unchangedsince` | STORE UNCHANGEDSINCE 推进 MODSEQ | CONDSTORE |
| `imap/search` | SEARCH SUBJECT 命中刚追加的邮件 | 搜索 |
| `imap/copy-move` | COPY/MOVE 均返回 COPYUID | UIDPLUS/MOVE |
| `imap/idle-push` | A 连接 IDLE 期间 B 连接 APPEND，A 在 20s 内收到 EXISTS | IDLE 推送 |
| `imap/expunge-unselect` | 标记删除 → EXPUNGE → UNSELECT | 生命周期 |
| `imap/cleanup-and-logout` | 删除临时邮箱、正常登出 | 收尾 |

### SMTP 检查清单（真实 go-smtp）

| 检查 | 验证内容 |
| --- | --- |
| `smtp/ehlo-extensions` | EHLO 声明 PIPELINING、SIZE、8BITMIME、ENHANCEDSTATUSCODES |
| `smtp/auth-bad-credentials` | 错误密码必须被拒绝（不能 250） |
| `smtp/submit-8bit` | UTF-8 主题/正文经 submission 提交成功 |
| `smtp/recipient-unknown-rejected` | 不存在的本地收件人必须被 RCPT TO 拒绝 |
| `smtp/delivered-to-inbox` | 提交的邮件 30s 内落到本账号 INBOX（端到端） |

## 二、人工矩阵清单

逐项人工验证，记录版本与结果。**每季度回归一次**；发版前对“发布门禁”列全绿。

图例：✅ 通过｜⚠️ 可用但有限制（备注说明）｜❌ 失败（附抓包/日志）

| 客户端（版本） | 平台 | 账户手动配置 | IMAP 全量同步 | IDLE 实时推送 | 文件夹层级+中文/UTF-7 名 | 已读/星标双向同步 | SMTP 587 STARTTLS | SMTP 465 隐式 TLS | 大附件（≥25MB） | 断线重连增量同步（QRESYNC） | 备注 |
| --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- | --- |
| 桌面客户端 ESR | Windows | | | | | | | | | | 发布门禁 |
| 商业客户端（新版） | Windows | | | | | | | | | | IMAP 限速为客户端固有 |
| system mail app | macOS | | | | | | | | | | 发布门禁 |
| system mail app | iOS | | | | | | | | | | 推送依赖 IDLE |
| mainstream clients | Windows | | | | | | | | | | |
| 网页邮箱 App（IMAP 账户） | Android | | | | | | | | | | |
| 传统系统客户端 | Windows | | | | | | | | | | 尽力兼容 |

### 人工检查项操作口径

1. **账户手动配置**：只填服务器地址/端口/凭据即可成功建户，无需自动发现。
2. **IMAP 全量同步**：首连后文件夹树与邮件数和 Webmail 一致（抽 3 封核对内容）。
3. **IDLE 实时推送**：客户端在线，Webmail 发一封，≤10s 客户端弹新邮件。
4. **文件夹层级**：客户端创建 `工作/2026` 二级文件夹（UTF-7 编码路径），Webmail 端可见同名。
5. **已读/星标双向**：客户端读一封 → Webmail 显示已读；Webmail 加星 → 客户端可见。
6. **SMTP 587/465**：两端口的对外发送均成功且进入对方收件箱（或自身 INBOX 的 Sent 视图）。
7. **大附件**：发送 ≥25MB 附件不中断；接收同尺寸附件能完整下载。
8. **断线重连增量同步**：客户端断网期间服务端投递 3 封、删除 1 封，恢复后增量一致（QRESYNC/CONDSTORE 路径）。

## 三、记录

| 日期 | 执行人 | 自动化结果 | 人工矩阵结论 | 备注 |
| --- | --- | --- | --- | --- |
| | | `go run ./cmd/conformance ...`（贴输出摘要） | 上面矩阵更新 | |
