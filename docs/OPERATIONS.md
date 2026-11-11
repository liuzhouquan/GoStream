# GoStream 运维与灾备恢复手册 (Operations & Disaster Recovery)

更新时间：2026-11-07。适用版本：`v1.0-demo`。

---

## 1. 灾备策略与一致性边界

GoStream 采用**模块化单体与多组件共享架构**。核心业务事实存储在 MySQL/SQLite 关系数据库中，媒体资产文件存储在 MinIO 对象存储或本地文件系统，异步事件由 RabbitMQ 削峰投递，实时连接与缓存由 Redis 托管。

### 1.1 停写快照边界与恢复保证边界

> [!IMPORTANT]
> **在线热备限制说明**：由于关系型数据库（MySQL/SQLite）与对象存储（MinIO/S3）属于独立的分布式存储系统，缺乏跨存储的分布式两阶段提交（2PC）或全局原子快照硬件支持，在系统持续写入状态下分别进行备份**无法保证 SQL 元数据与底层物理对象在微秒级别的时序对齐**。
> 因此，**GoStream 明确声明：严禁在未停写的生产高频写入状态下进行无协同的“在线热备”**。合规备份必须在明确的**停写维护窗口（Quiescent Window）**下执行。

- **停写维护窗口操作**：
  1. 通过反向代理或负载均衡将写流量切流至只读/维护提示页面（HTTP 503 / Maintenance Mode）。
  2. 观察并等待当前执行中的转码作业（`video_process_jobs`）与 Outbox 事件排空。
  3. 停止所有 API 实例和独立后台 worker，确认没有转码、上传完成或垃圾回收继续写 SQL/对象；仅屏蔽浏览器写请求不足以形成停写边界。
  4. 执行快照备份：
     - **SQL 快照**：`mysqldump --single-transaction --routines --triggers --no-tablespaces --set-gtid-purged=OFF`（或 SQLite `PRAGMA wal_checkpoint(TRUNCATE)` 后复制文件）。
     - **对象存储备份**：`gostream-media backup --dir /backup/media`（或全量归档对象存储卷）。
  5. 备份完成并校验后，启动服务并恢复对外写流量。

- **恢复目标与测量边界**：在 `CLOSE-02`（`docs/benchmarks/pr64-recovery-verified-2026-11-11.json`）的正式 Compose 规模（2,005 用户 / 20,010 发布视频 / 11,001 关注 / 50,002 互动）且严格停写排空维护窗口下，已实测单事务+对象备份耗时 **5.01s**、目标恢复至 `/ready` **58.47s**、至全业务核对与断点续传闭环 **63.16s**、**RPO=0s**（均针对停写快照边界，未证在线热备一致性，多主机扩容另计）。历史记录中的 4.57ms 仅为 5 用户/10 视频 SQLite 文件复制耗时，不可作为全量 RTO 口径；`CLOSE-03` 亦已修复 `migration_backup_recovery_smoke.py` 中 `verify_quiescence` 异常吞没问题，确保源端仍在响应 HTTP 时强制阻断快照。

---

## 2. 灾备恢复标准操作流程 (Runbook)

### 2.1 灾难发生时的恢复步骤

1. **环境准备**：
   - 启动全新或已重置的目标环境（MySQL 8.4、MinIO、Redis 8.2、RabbitMQ 4.3）。
   - 确保各中间件已通过健康检查。

2. **恢复数据库快照**：
   ```bash
   # MySQL 模式
   mysql -h $MYSQL_HOST -u $MYSQL_USER -p$MYSQL_PASSWORD $DATABASE_NAME < /backup/snapshot/database.sql

   # SQLite 模式
   cp /backup/snapshot/database.sqlite /data/gostream.db
   ```

3. **恢复媒体对象存储**：
   ```bash
   # 使用官方 gostream-media 恢复工具
   gostream-media restore --dir /backup/media
   ```

4. **版本迁移核验与幂等校验**：
   - 启动服务实例，服务将在启动阶段自动调用 `migrations.Apply`。
   - 校验 `schema_migrations` 表中的版本标识，必须按序包含 `schema_v001` 至 `schema_v005`。
   - 重复执行迁移不会产生额外记录或 DDL 冲突（严格幂等）。

5. **服务就绪探测**：
   - 访问 `GET /health`（验证进程存活状态，HTTP 200）。
   - 访问 `GET /ready`（验证 SQL Ping 数据库可用性，HTTP 200）。

---

## 3. 故障处理与降级响应矩阵

| 故障场景 | 系统表现与降级行为 | 自动化恢复行为 | 运维介入指引 |
| :--- | :--- | :--- | :--- |
| **Redis 宕机** | `/ready` 保持 200；Feed 缓存熔断器触发，所有 Feed 请求自动回源 SQL（200 OK）；IM 跨实例实时广播降级，消息在 SQL 事务中正常持久化，发送端获得 ACK。 | Redis 重启后，缓存熔断器冷却重置，Feed 自动重新预热命中；Realtime 协程自动重连订阅通道。 | 检查 Redis 内存与持久化 AOF 日志，确认无端口冲突后重启服务。 |
| **RabbitMQ 宕机** | `/ready` 保持 200；点赞、评论等互动请求在 SQL 事务中正常提交，`OutboxEvent` 以待发状态（`published_at IS NULL`）安全滞留在数据库中；Worker 记录 `last_error` 并自适应指数退避。 | Broker 重启后，Outbox Worker 自动恢复投递并确认；消费端利用 `broker-delivery:*` 键进行幂等去重，0 重复通知。 | 检查 RabbitMQ 节点集群状态与磁盘警报，排查 Erlang 内存配额。 |
| **MinIO 宕机** | `/ready` 保持 200；分片上传请求快速失败返回 503 (`media storage unavailable`)；底层数据库严格保持零脏分片、零孤儿资产，会话维持 `"open"`。 | MinIO 重启就绪后，客户端直接重试失败分片（200 OK），后续 `complete` 接口正常组装发布。 | 检查存储驱动挂载状态与 MinIO API 端口响应。 |
| **MySQL / SQL 宕机** | `/health` 保持 200（进程存活）；`/ready` 立即返回 503 (`database unavailable`)。 | 数据库恢复连接后，`/ready` 自动恢复 200，API 恢复对外业务处理。 | 排查数据库主从同步、死锁锁争用及连接池水位。 |
| **API 实例崩溃重启** | 客户端持有的现有 JWT 凭据免刷鉴权（Stateless JWT）；持久化会话与未读计数完好；崩溃前持有的租约过期后，其他或重启后的 Worker 安全重新接管。 | 租约单调世代防护，旧 Worker 滞后提交被行级版本锁彻底拦截。 | 分析 coredump 或 OOM-killer 记录，检查内存上限。 |

---

## 4. 自动化验证入口

全套迁移、备份恢复与运维规格已固化在自动化测试套件中：

```bash
# 1. 运行 Go 原生迁移与 v005 回归套件
go test -v ./internal/migrations -run '^TestContinuousV005_'

# 2. 运行端到端灾备备份恢复钻井与 Gate A10 正式验收
python3 tests/integration/migration_backup_recovery_smoke.py --mode sqlite
python3 tests/integration/migration_backup_recovery_smoke.py --mode compose --compose-e2e --formal-scale

# 3. 运行运维与灾备诊断单元测试
python3 -m unittest tests/diagnostics/test_migration_backup.py -v
```
