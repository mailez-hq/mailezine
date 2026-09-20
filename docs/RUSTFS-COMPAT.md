# RustFS 兼容性支持矩阵

RustFS 1.0.0（image `rustfs/rustfs:latest`，build 2026-09-16）与 MinIO
（`minio/minio:latest`，本地 ee 栈）对照实测记录。验证工具：
`go run ./cmd/s3-probe -endpoint <host:port> -access <ak> -secret <sk>`
（20 项检查，两端均 20/20 通过，2026-09-20）。

## 结论

**RustFS 与 MinIO 在 mailez/mailezine 依赖的全部 S3 语义上行为一致，
包括 HA 租约所依赖的条件写。** 可作为默认对象存储后端。

## 能力矩阵

| 能力 | 消费方 | RustFS 1.0.0 | 备注 |
|---|---|---|---|
| MakeBucket / BucketExists（首启建桶） | `mailez drive`、`mailezine EnsureBucket` | ✅ | 重复建桶幂等，AlreadyExists 语义正确 |
| PutObject（已知大小）/ GetObject / RemoveObject | 网盘、备份 | ✅ | 内容逐字节一致 |
| StatObject 缺失键错误码 | `s3blob.mapS3Error`、`ha.s3NotFound` | ✅ | 返回标准 `NoSuchKey` |
| RemoveObject 幂等 | `S3Store.Delete` | ✅ | 缺失键仍成功 |
| PutObject size=-1（minio-go 自动 multipart） | `s3blob.Put` 大正文 | ✅ | 22MB 随机数据分片上传读回逐字节一致 |
| ListObjects（Recursive + Prefix） | `backup.go` 归档列表 | ✅ | ListObjectsV2 |
| **条件写 If-None-Match:\*（create-only）** | `ha` 租约首次认领 | ✅ | 已存在时返回 `PreconditionFailed`；8 路并发首认领恰好 1 胜 7 负，读回确认唯一赢家 |
| **条件写 If-Match:\<etag\>（CAS）** | `ha` 租约续期/接管 | ✅ | 陈旧 etag 返回 `PreconditionFailed` 且**不落地**（读回验证非静默忽略） |
| 写后读回一致性 | `ha.confirm` | ✅ | 强一致，未观察到陈旧读 |
| region 固定 `us-east-1` | `s3blob`/`ha` 客户端 | ✅ | RustFS 默认 region 即 us-east-1，跳过 bucket-location 探测的优化仍有效 |

## 未覆盖项（当前代码路径不使用）

- presigned URL、bucket policy / 匿名读、生命周期规则、桶通知、
  CopyObject、版本控制。若未来引入需重新跑 `s3-probe` 扩展用例。

## 运维差异（与 MinIO 的不同点）

| 项 | MinIO | RustFS |
|---|---|---|
| 凭据 env | `MINIO_ROOT_USER/PASSWORD` | `RUSTFS_ACCESS_KEY/SECRET_KEY` |
| 就绪探针 | `GET /minio/health/ready` | `GET /health`，同时兼容 `GET /minio/health/ready`（实测均 200） |
| 控制台端口 | 9001（`--console-address`） | 9001（同参数） |
| 磁盘格式 | `xl.meta` 私有布局 | 私有布局（互不兼容）；**迁移必须走对象层复制**，不能直接挂旧卷 |

## 迁移指引

MinIO → RustFS：bucket 名保持不变，用 rclone（或 `mc mirror`）做对象层
镜像，切换 `MAILEZINE_S3_ENDPOINT` 指向 rustfs 服务后重启引擎；用
`go run ./cmd/storage-smoke -mode check` 抽验读路径。回滚 = 端点指回
MinIO（切换前旧桶保持只读原样即可）。

## 残留风险与观察项

1. RustFS 1.0.0 GA 发布仅数天，生产长期稳定性（内存曲线、坏盘处理）待观察；
   ee compose 保留 `minio` profile 作为逃生门。
2. 条件写错误体细节（`ConditionRequestedUnmet` 等 AWS 新码）未测——代码只
   判 `PreconditionFailed`，若未来版本改错误码，`ha` 的失败分类会退化为
   generic error（保守失败，不会双主）。
