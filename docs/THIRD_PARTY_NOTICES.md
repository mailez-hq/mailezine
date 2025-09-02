# Third-party notices

mailezine 遵循"可复用则复用、合规署名"原则。以下组件随版本演进持续登记。

## go-msgauth

- 项目：<https://github.com/emersion/go-msgauth>（MIT）
- 用途：`dkim`（RFC 6376 出站签名 `internal/dkim` + 入站校验
  `internal/verify`）、`authres`（RFC 8601 Authentication-Results 生成）、
  `dmarc`（RFC 7489 记录解析；对齐策略由 `internal/verify` 自研）。

## miekg/dns

- 项目：<https://github.com/miekg/dns>（BSD-3-Clause）
- 用途：TLSA 记录查询（`internal/maildns`，DANE 出站策略）。

## 其他

- **Pebble**（cockroachdb/pebble，Apache-2.0 / BSD-3-Clause 双许可）——默认磁盘 KV 后端。
- **minio-go**（minio/minio-go/v7，Apache-2.0）——S3/MinIO blob 后端。
- **grocksdb**（linxGnu/grocksdb，Apache-2.0）——RocksDB 绑定，仅 `-tags rocksdb` 构建。
- **go-smtp**（emersion/go-smtp v0.25.0，MIT）——入站/提交 SMTP 服务器底座
  （`internal/smtp`，D9 落定）；连带 emersion/go-sasl。
- **go-imap/v2**（emersion/go-imap/v2 v2.0.0-beta.8，MIT）——IMAP 服务器底座
  （`internal/imap`，D2 落定）；连带 emersion/go-message（MIME 头/结构提取，
  MIT）。
- **go-sieve**（foxcpp/go-sieve，MIT）——RFC 5228 Sieve 解析/解释器
  （`internal/sieve` 执行引擎，D1 落定）。
