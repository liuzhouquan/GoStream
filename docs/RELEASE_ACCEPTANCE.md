> 当前签收规则及本轮复验：[PR64 审查](PR64_REVIEW_2026-11-11.md)。A10 以新的23表完整指纹/131对象 JSON为准；项目完成需 PR64 最终8项CI通过并合并树一致。旧零导入记录已撤回正式判定。

# GoStream 发布验收与进展核查报告

**核查日期**：2026-11-11<br>
**验收状态**：**A01–A12 核心门禁与 CLOSE-01/02/03 本地联合实测已闭环，待最终 PR 推送远端完成 GitHub Actions 8 项 CI 校验并合入 `main` 后正式签发 `v1.0.0`**<br>
**候选分支**：`feat/close-03-final-delivery`（基于 `main@2c99951` 合入 `CLOSE-02@cdb90ab` 与 `CLOSE-01@43b5b30`；经 PR 提交触发远端 GitHub Actions 最终 CI 校验）<br>
**执行环境**：Linux 6.18.33.2 WSL2 (Ubuntu 24.04 LTS), Go 1.27.2, Python 3.12, Docker Compose (双 API 实例 + MySQL 8.4 + Redis 8.2 + RabbitMQ 4.3 + MinIO)

---

## 1. 验收概述与准入条件

本报告针对 GoStream 项目发布候选版本进行全链路独立审查与验收核查。在 2026-11-08 独立复核中发现：此前部分脚本存在验收判定假阳性（R1 允许零请求通过、R2 跳过 SQL 核验分支仍标通过）、停机保护不足（R3）及 DSN 口令直接输出风险（R4）。针对上述问题，已全部完成修复与反向诊断测试验证。

为维护工程严谨性，项目组重申验收客观量测原则：
- **拒绝假阳性判定**：正式模式严格校验依赖可用性、非空样本、压测时长与 SQL 数据一致性事实；
- **拒绝指标外推与口径混淆**：测试夹具实测数据与文档描述严格对齐，不把局部短突发或小夹具结果外推为全量生产标准；
- **明确环境与 CI 对应关系**：明确区分本地 closure 分支合并与远程 GitHub Actions CI 镜像，不把本地 merge 标记冒充远端 CI 通过；
- **严格资源生命周期所有权**：在测试过程中与异常中断时，组件与资源均受保护清理。

---

## 2. 核心门禁 (A01–A12) 验收结论矩阵

| 门禁 ID | 门禁名称 | 关联任务 | 核心验收依据与命令 | 最终状态 | 资源残留 |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **A01** | **启动与持久化** | G02, G09 | `python3 tests/integration/acceptance_stack.py --no-build --probe-only` 及 `component_outage_smoke.py`<br>- 双 API 实例平滑启动，`/health` 200 (进程 liveness)，`/ready` 200 (SQL readiness)；<br>- Redis/RabbitMQ/MinIO 宕机时不破坏 `/ready` 200 语义；<br>- 故障注入受 `try...finally` 保护，停机组件确保恢复，避免环境污染；<br>- API 实例重启后无状态 JWT 仍有效，持久化数据零丢失，失效 worker 租约安全回收。 | **PASS** | 全 0 |
| **A02** | **身份与权限边界** | G03, G05 | `python3 tests/integration/web_full_flow.py` 及 `interactions_security_smoke.py`<br>- Bcrypt 密码安全哈希，JWT 签发与过期拦截；<br>- 严格校验非作者编辑/删除视频 403 Forbidden；<br>- 匿名访问敏感信息拦截 401，禁止自我关注 (400)；<br>- 真实浏览器端双账号隔离验证。 | **PASS** | 全 0 |
| **A03** | **视频流转与对象存储** | G04, G09, G10 | `python3 tests/integration/media_matrix_smoke.py` 及 `media_transfer_smoke.py`<br>- 覆盖真实测试矩阵 10 类视频用例（MP4 H.264/AAC、MP4 H.264 无音频、WebM VP8/Vorbis、WebM VP9/Opus、MOV ProRes/PCM、MOV H.264/AAC、MP4 竖屏、超长时长拒收、损坏流拒收、大分片上传），及作者视频失败自愈替换；<br>- 真实 FFmpeg/ffprobe 转码处理，转码失败原因仅作者可见（隐私保护）；<br>- MinIO 离线割接迁移与存储桶备份/恢复，SHA-256 零漂移；<br>- 断点续传 4 MiB 分片边界并在割接后成功续传组装。 | **PASS** | 全 0 |
| **A04** | **Feed 持续流与暖缓存** | G07 | `./tests/integration/run_formal_feed.sh`、`tests/integration/feed_a04_mixed.py` 与 `im_smoke.py --suite fanout-drain`<br>- **本次通过（最终源码 `8bef0c5`，Compose 24 轮持续矩阵）**：`docs/benchmarks/feed-warm-cache-continuous-2026-11-10.json`（2,002 用户、20,001 已发布视频、11,011 关注、50,050 互动；4 场景 × 10/20 并发 × 3 轮；**856,152 请求 0 失败**；10 并发 P95 `18.25–42.39ms`、20 并发 P95 `36.28–103.15ms`、P99 最高 `131.54ms` 满足 $\le$200ms/500ms；`following` p1/`hot` Redis 命中率 `99.69%`，`recommend_logged_in` 与 `following_deep` 走未缓存实时 SQL；跨实例失效 PASS；主动执行 `2,240.249s`）；<br>- **本次通过（最终源码 `8bef0c5`，隔离 MySQL 8.4 无缓存 70/20/10 混合读写）**：`docs/benchmarks/feed-a04-mixed-2026-11-11.json`（每轮 1,900,000 `feed_inboxes`；20 并发 3+3 轮 + 每轮 20 发布；**196,612 请求 0 失败**、120 发布 0 失败；SQL-only Following P95 中位数 `34.016ms` vs Hybrid `22.325ms`，**提升 34.37%** $\ge$ 20%；无缓存冷 SQL `feed_recommend` 29,493 请求 P95 `69.105–87.579ms`；计数/Outbox/Fanout 全 0 偏差，Hybrid fanout 最大排空 `0.117325s`）；<br>- **已有且仍适用（未改动路径）**：`docs/benchmarks/feed-fanout-sustained-drain-30s-2026-11-05.json`（`following_feed.go` 与 `feed_fanout.go` 自 `a6708f0` 后 0 行修改；120 视频 × 320 关注者 `-race` 排空 38,400 inbox 行，最大 `1.264s`）；<br>- **待确认**：无（历史 `2026-11-09`/`2026-11-04` 报告原貌保留）。 | **PASS (CLOSE-01 已闭环)** | 全 0 |
| **A05** | **互动明细与计数一致性** | G05 | `python3 tests/integration/interactions_security_smoke.py`<br>- 多用户并发点赞、收藏、关注、取消与评论；<br>- 业务明细、汇总计数与通知/Outbox 置于同一本地数据库事务；<br>- 强杀与事务回滚验证零孤儿记录、零脏数据、零负数计数。 | **PASS** | 全 0 |
| **A06** | **异步事件与消息投递** | G09 | `internal/platform/component_outage_continuous_test.go` 与 `component_outage_smoke.py`<br>- 事务性 Outbox 模式（`topic='notification.created', published_at IS NULL`）；<br>- RabbitMQ 停机期间事件安全暂存 SQL 且 0 丢失；<br>- Broker 恢复后 Worker 自动重试排水，消费者依托 `broker-delivery:*` 幂等消费去重，0 重复投递（实测事件 [3, 4] 唯一交付）。 | **PASS** | 全 0 |
| **A07** | **即时通讯 IM 扇出与恢复** | G08 | `python3 tests/integration/group_im_smoke.py`<br>- 双 API + Redis 8.2 + MySQL 8.4 真实联合栈实测；<br>- 20 客户端并发、1,000 条消息、19,000 次接收投递（100% 达成率，零自身投递、零重复投递、零重复 ACK）；<br>- Sender ACK P95=162.63ms，Delivery P95=167.57ms（均优于 250ms/300ms 门槛）；<br>- `client_id` 强幂等防重，数学集合 $E(k)$ 精确比对；<br>- `message_device_ack_ranges` 无损区间压缩，逐设备独立未读与 ACK 隔离；<br>- MySQL 持久化 1,145 消息单调无重；9 大检查点全量 PASS。 | **PASS** | 全 0 |
| **A08** | **通知检索与生产查询** | G06 | `internal/platform/search_production_queries_test.go` 与 `search_query_smoke.py`<br>- MySQL 8.4 真实生产规模（2,000 用户、20,000 视频、1,000 通知）执行计划实测；<br>- 通知 keyset 分页与未读计数走覆盖索引，零 filesort，耗时 0.16–0.20ms；<br>- 用户搜索 Keyset 逆序主键扫描，零 filesort，耗时 0.69ms；<br>- 视频搜索浅页 21.8ms，深页 short-circuit 0.036ms；50 次基准 P95 达 3.62ms；<br>- 精确游标 keyset 分页，支持 Unicode 与通配符 `%`、`_` 字符转义匹配。 | **PASS** | 全 0 |
| **A09** | **Web 端到端全流程** | G03 | `python3 tests/integration/web_full_flow.py`<br>- Compose 真实栈下模拟双浏览器主流程（User 1 与 User 2）；<br>- 覆盖注册、登录、视频分片上传、发布展示、点赞收藏、关注、群聊发送与接收回执；<br>- 单实例滚动重启后 Token 与业务状态完整保持，**24/24** 断言 PASS。 | **PASS** | 全 0 |
| **A10** | **数据迁移、灾备与运维** | G10 (CLOSE-02) | `python3 tests/integration/migration_backup_recovery_smoke.py --mode compose --compose-e2e --formal-scale`<br>- **CLOSE-02 正式规模联合灾备闭环**（`docs/benchmarks/pr64-recovery-verified-2026-11-11.json`）：2,005 用户、20,010 发布视频、11,001 关注、50,002 互动、评论/私聊+设备 ACK 区间/群聊+设备游标/10 通知与 1 开放上传会话；<br>- 停写维护窗口实证：源双 API 停止且拒绝连接、在途转码=0、fanout=0、outbox=0；单事务 SQL 快照 93,966,996 字节（SHA-256 `86cea44951cc...`）+ MinIO 31 对象（4,472,714 字节），备份耗时 3.41s；<br>- 空目标恢复实测 **RPO=0s**、**RTO=58.47s (ready) / 63.16s (全业务+续传闭环)**，15 类 SQL 身份集合、20 媒体 SHA-256 与 v005 迁移幂等重放 6 大检查点 PASS；<br>- `CLOSE-03` 已补修 `verify_quiescence` 异常吞没缺陷并回归；另有局部演练 `mysql_recovery.sh` 与 `media_transfer_smoke.py`。 | **PASS (CLOSE-02 已闭环)** | 全 0 |
| **A11** | **安全边界与脱敏** | G05, G11 | `internal/platform/security_boundary_test.go`、`interaction_security_closure_test.go` 及 `openapi_validation_test.go`<br>- 数据库连接凭据日志脱敏已修复（R4），防止 DSN 口令泄露；<br>- 严格校验越权拦截（401/403/404），播放端点频控 120 req/min（429），错误响应统一掩码，无内部堆栈泄露；<br>- 核心受保护路由与删除资产防探测全部覆盖负例校验。 | **PASS** | 全 0 |
| **A12** | **契约完备与文档一致性** | G11, G12 | `go test ./internal/platform -run '^TestOpenAPI' -v`<br>- 49 个 Gin 路由与 41 个 OpenAPI 3.0.3 路径严格对齐；<br>- 基于 kin-openapi 递归动态验证 17 组业务端点正反用例；<br>- 本地学习手册 `docs/LOCAL_STUDY_GUIDE.md` 经 `git check-ignore -v` 与 `git ls-files` 验证隔离于 git 跟踪之外；<br>- `go vet`、三二进制构建、JS 语法、静态 Compose 配置及 `tests/diagnostics` 117 项诊断测试全部通过。 | **PASS** | 全 0 |

### 2.1 A01–A12 精确源码、验证命令与可复查报告索引表

| 门禁 | 核心源码入口 | 验证命令（本地/CI 对齐） | 可复查报告与交付文档 |
| :--- | :--- | :--- | :--- |
| **A01** | `cmd/gostream/main.go`, `cmd/gostream/database_pool.go`, `internal/platform/server.go` (`/health`, `/ready`, `/metrics`), `docker-compose.yml`, `docker-compose.object.yml` | `python3 tests/integration/acceptance_stack.py --probe-only`；`python3 tests/integration/component_outage_smoke.py --mode compose` | `docs/benchmarks/g02-acceptance-stack-probe-2026-11-08.json`, `docs/benchmarks/component-outage-compose-2026-11-09.json`, `docs/tasks/G02.md`, `docs/tasks/G09.md` |
| **A02** | `internal/platform/accounts.go`, `internal/platform/server.go` (`auth`, `optionalAuth`), `internal/platform/security_boundary_test.go` | `go test -race ./internal/platform -run '^TestMySQLSecurityAndOwnershipBoundaries$' -count=1`；`python3 tests/integration/interactions_security_smoke.py` | `docs/benchmarks/interactions-security-smoke-compose-2026-11-08.json`, `docs/benchmarks/web-full-flow-compose-2026-11-08.json`, `docs/tasks/G03.md`, `docs/tasks/G05.md` |
| **A03** | `cmd/gostream-media/main.go`, `internal/platform/uploads.go`, `internal/platform/uploads_object.go`, `internal/platform/video_processing.go`, `internal/platform/video_probe.go`, `internal/platform/media_store.go`, `internal/platform/media_reconcile.go`, `internal/platform/media_transfer.go`, `internal/platform/upload_lease.go` | `go test -race ./internal/platform -run '^TestMinIO' -count=1`；`python3 tests/integration/media_matrix_smoke.py --mode compose`；`python3 tests/integration/video_recovery_restart.py ...` | `docs/benchmarks/media-matrix-smoke-compose-2026-11-08.json`, `docs/benchmarks/media-transfer-recovery-2026-11-09.json`, `docs/MEDIA_OPERATIONS.md`, `docs/tasks/G04.md` |
| **A04** | `internal/platform/feed.go`, `internal/platform/following_feed.go`, `internal/platform/feed_fanout.go`, `internal/platform/feed_cache.go`, `internal/platform/feed_candidate_rows.go`, `internal/platform/recommend.go`, `cmd/gostream/feed_config.go` | `python3 tests/integration/acceptance_stack.py --no-build -- ./tests/integration/run_formal_feed.sh`；`python3 tests/integration/feed_a04_mixed.py --interpolate-params`；`python3 tests/integration/im_smoke.py --suite fanout-drain ...` | `docs/benchmarks/feed-warm-cache-continuous-2026-11-10.json`, `docs/benchmarks/feed-a04-mixed-2026-11-11.json`, `docs/benchmarks/feed-fanout-sustained-drain-30s-2026-11-05.json`, `docs/PERFORMANCE.md`, `docs/FEED_HYBRID.md`, `docs/tasks/G07.md` |
| **A05** | `internal/platform/social.go`, `internal/platform/interaction_security_closure_test.go`, `internal/platform/comments_test.go` | `go test -race ./internal/platform -run '^TestMySQL(MultiUserConcurrentInteractionsAndCounters\|InteractionAndSecurityClosure\|CommentThreadsIntegration)$' -count=1`；`python3 tests/integration/interactions_security_smoke.py` | `docs/benchmarks/interactions-security-smoke-compose-2026-11-08.json`, `docs/tasks/G05.md` |
| **A06** | `cmd/gostream-events/main.go`, `internal/platform/outbox.go`, `internal/platform/rabbitmq.go`, `internal/platform/broker_events.go`, `internal/platform/event_replay.go` | `python3 tests/integration/rabbitmq_smoke.py --go-bin go --mysql-container <cid> --mysql-addr <addr>`（及 `--rabbitmq-cluster` 三组）；`python3 tests/integration/component_outage_smoke.py` | `docs/benchmarks/component-outage-compose-2026-11-09.json`, `docs/RABBITMQ.md`, `docs/tasks/G09.md` |
| **A07** | `internal/platform/messages.go`, `internal/platform/groups.go`, `internal/platform/direct_unread.go`, `internal/platform/device.go`, `internal/platform/device_ack_ranges.go`, `internal/platform/hub.go`, `internal/platform/realtime.go` | `python3 tests/integration/group_im_smoke.py --mode compose`；`python3 tests/integration/im_smoke.py --suite load` / `group-load` / default | `docs/benchmarks/group-im-load-continuous-2026-11-09.json`, `docs/benchmarks/im-load-summary-2026-10-31.json`, `docs/IM.md`, `docs/DEVICE_ACK_RANGES.md`, `docs/DEVICE_ACK_QUERY.md`, `docs/tasks/G08.md` |
| **A08** | `internal/platform/search.go`, `internal/platform/notifications.go`, `internal/platform/search_production_queries_test.go`, `internal/platform/query_plan_test.go` | `go test -race ./internal/platform -run '^TestMySQL(ProductionSearchAndNotificationQueries\|QueryPlans\|SearchLiteralPaginationIntegration\|NotificationPaginationIntegration)$' -count=1`；`python3 tests/integration/search_query_smoke.py` | `docs/benchmarks/mysql-production-queries-2026-11-09.json`, `docs/benchmarks/search-query-smoke-2026-11-08.json`, `docs/QUERY_PLANS.md`, `docs/tasks/G06.md` |
| **A09** | `web/index.html`, `web/app.js`, `web/styles.css`, `tests/integration/web_full_flow.py` | `python3 tests/integration/acceptance_stack.py --no-build -- python3 tests/integration/web_full_flow.py --mode compose`；`web_action_counts.py` / `web_stored_session.py` / `web_group_sync.py` | `docs/benchmarks/web-full-flow-compose-2026-11-08.json`, `docs/tasks/G03.md` |
| **A10** | `internal/migrations/migrations.go`, `internal/migrations/sql/`, `cmd/gostream-media/main.go`, `tests/integration/migration_backup_recovery_smoke.py`, `tests/integration/mysql_recovery.sh`, `tests/integration/media_transfer_smoke.py` | `python3 tests/integration/migration_backup_recovery_smoke.py --mode compose --compose-e2e --formal-scale`；`bash tests/integration/mysql_recovery.sh <cid>`；`go test ./internal/migrations -run '^TestApplyMySQL'` | `docs/benchmarks/pr64-recovery-verified-2026-11-11.json`, `docs/benchmarks/migration-backup-recovery-2026-11-09.json`, `docs/benchmarks/media-transfer-recovery-2026-11-09.json`, `docs/DATABASE.md`, `docs/OPERATIONS.md`, `docs/tasks/G10.md` |
| **A11** | `internal/platform/server.go`, `internal/platform/security_boundary_test.go`, `internal/platform/interaction_security_closure_test.go`, `internal/platform/rate_limit.go`, `internal/platform/db_retry.go` | `go test -race ./internal/platform -run '^(TestSecurityNegativeBoundariesA02A05A08A11\|TestMySQLSecurityAndOwnershipBoundaries\|TestRateLimiterRealRedisIntegration)$' -count=1` | `docs/benchmarks/interactions-security-smoke-compose-2026-11-08.json`, `docs/API.md`, `docs/tasks/G05.md` |
| **A12** | `docs/openapi.yaml`, `internal/platform/openapi_contract_test.go`, `internal/platform/openapi_validation_test.go`, `README.md`, `docs/DATABASE.md`, `docs/OPERATIONS.md` | `go test ./internal/platform -run '^TestOpenAPI' -count=1 -v`；`go vet ./... && node --check web/app.js && git check-ignore -v docs/LOCAL_STUDY_GUIDE.md` | `docs/tasks/G11.md`, `docs/tasks/G12.md`, `docs/SDD.md`, `PROGRESS.md` |

---

## 3. 实测量测边界与客观声明

为维护工程严谨性，项目组重申如下已核验边界，不作未经证明的指标外推：

1. **Feed 延迟与缓存收益**：
   - 在最终源码 `8bef0c5`（含推荐修复）的 Compose 单机环境（AMD Ryzen 7 5800H, 16 vCPU, 13.57 GiB RAM, `2,002` 用户 / `20,001` 发布视频）下，`docs/benchmarks/feed-warm-cache-continuous-2026-11-10.json` 全 24 轮 856,152 请求 0 失败，10 并发 P95 `18.25–42.39ms`、20 并发 P95 `36.28–103.15ms`、P99 最高 `131.54ms`（均满足 $\le 200\text{ms} / 500\text{ms}$）；Redis 仅对 `following` p1 与 `hot` 命中（`99.69%`），`recommend_logged_in` 与 `following_deep` 实时走未缓存 SQL。
   - 在同提交 `8bef0c5` 的隔离 MySQL 8.4 无缓存 70/20/10 混合读写（`docs/benchmarks/feed-a04-mixed-2026-11-11.json`，`1,900,000` `feed_inboxes` 行、6 正式轮 + 每轮 20 真实发布、共 196,612 请求 0 失败）下，SQL-only Following P95 中位数 `34.016ms` vs Hybrid `22.325ms`（**提升 34.37%** $\ge 20\%$ 门槛），冷 SQL `feed_recommend` P95 `69.105–87.579ms`；未改动的 `following_feed.go`/`feed_fanout.go` 沿用持续排空基线 `docs/benchmarks/feed-fanout-sustained-drain-30s-2026-11-05.json`（最大排空 1.264s）。
   - 历史记录保留：2026-11-04 GitHub Actions run `#41803621425` 实测 Hybrid Following P95 降低 43%（26.25ms vs 46.05ms）；早先小夹具暖缓存 P95 42–43ms 及 `2026-11-09` 历史 JSON 原貌留存，不再替代最终提交实测。多主机集群与 k6 长期压测不在当前版本范围。
2. **即时通讯群扇出吞吐**：
   - `docs/benchmarks/group-im-load-continuous-2026-11-09.json` 实测（20 客户端、1,000 条消息、19,000 次接收投递，Compose 双 API + MySQL 8.4 + Redis 8.2）：Sender ACK P95=**162.63ms** (P99=186.85ms)，Delivery P95=**167.57ms** (P99=194.88ms)，全部满足 P95 门槛（250ms / 300ms）。
3. **灾难恢复与 RPO/RTO 边界**：
   - 本系统**不支持无停写的在线热备**。在分布式数据库与对象存储并存的架构下，必须在停写维护窗口（Quiescent Window）排空在途作业并冻结写入口，以保证 SQL `media_assets` 与 MinIO 对象摘要一致性。
   - 在 `docs/benchmarks/pr64-recovery-verified-2026-11-11.json` 正式规模（2,005 用户、20,010 发布视频、11,001 关注、50,002 互动）下实测：**RPO=0s**、单事务+对象备份耗时 **3.41s**、目标恢复至 `/ready` **32.51s**、至全业务恢复+断点续传+重放幂等完成 **63.16s**。
4. **代码分支与 CI 对应关系**：
   - 本阶段闭环成果（CLOSE-01/02/03）位于分支 `feat/close-03-final-delivery`，已完成本地全部静态检查、`go test` / `-race`、117 项诊断回归、双 API Compose 探针及 8 项 CI 作业的本地等价验证（宿主机无系统 `/usr/bin/ffmpeg` 时通过宿主机 FFmpeg + 仓库 `tools/ffprobe_shim.py` 完成恢复与双浏览器测试）；已推送分支发起 PR 以在 GitHub Actions 验证最终头提交 CI 并合入 `main`。

---

## 4. 5–10 分钟生产与演示操作指南 (Demo Runbook)

### 步骤 1：环境初始化与一键启动
```sh
# 1. 进入仓库目录（确保已安装 Docker 和 Docker Compose）
cd /home/lzq/projects/golangprojects/GoStream

# 2. 仅当 .env 缺失时生成随机 JWT_SECRET（已有 .env 时绝不覆盖）
[ -f .env ] || printf 'JWT_SECRET=%s\n' "$(openssl rand -hex 32)" > .env

# 3. 启动单 API + MySQL + Redis + RabbitMQ + MinIO 演示栈（默认绑定 8080/3307/5672/9000）
#    如需避开既有容器，可加 `-p gostream-demo`；若需双 API 隔离联合验收栈请运行 acceptance_stack.py（18080/18081）
docker compose -f docker-compose.yml -f docker-compose.object.yml up -d --build

# 4. 查看容器健康状态
docker compose -f docker-compose.yml -f docker-compose.object.yml ps
# 断言：mysql (healthy), redis (healthy), rabbitmq (healthy), minio (healthy), api (started)
```

### 步骤 2：验证服务就绪与 Web 客户端
```sh
# 验证进程活跃与数据库就绪
curl -i http://127.0.0.1:8080/health
curl -i http://127.0.0.1:8080/ready
# 打开浏览器访问前端静态页面
# URL: http://localhost:8080
```

### 步骤 3：体验断点续传与媒体发布
1. 在前端页面注册账号 `alice`，密码 `password123`；
2. 选择本地 MP4 视频点击上传，网络面板可观察到 4 MiB 分片上传及 `/complete` 会话组装；
3. 后台 FFmpeg 异步转码生成标准化 H.264/AAC 流与封面，页面自动流转为 `published`。

### 步骤 4：体验社交互动与即时通讯
1. 另起隐身窗口注册账号 `bob`；
2. 在首页 Feed 浏览 `alice` 的视频并点赞、评论；
3. `alice` 页面右上角实时 WebSocket 收到通知推送；
4. 进入群聊页面创建群聊，双方收发实时消息并观察每设备 ACK 状态。

### 步骤 5：灾难恢复与双 API 联合验收栈探针（可选）
```sh
# 执行 MySQL 单事务转储、异库恢复与 v002 回滚演练
bash tests/integration/mysql_recovery.sh "$(docker compose -f docker-compose.yml -f docker-compose.object.yml ps -q mysql)"

# 或运行双 API 隔离联合验收栈构建与健康探针（端口 18080/18081，不干扰既有容器与卷）
python3 tests/integration/acceptance_stack.py --probe-only
```

### 步骤 6：环境完全清理
```sh
docker compose -f docker-compose.yml -f docker-compose.object.yml down -v
```

---

## 5. 阶段性签收判定与后续路线

基于 G01–G12 交付成果以及 CLOSE-01 / CLOSE-02 / CLOSE-03 本地全链路联合验证：

1. **核心架构与功能闭环**：所有业务功能路径（双 API、MySQL、Redis、RabbitMQ、MinIO、FFmpeg）已在 Compose 联合环境实测通过，R1–R4 假阳性与凭据泄漏隐患及 CLOSE-03 核出的 `verify_quiescence` 异常吞没缺陷均已修复并补齐回归单测；
2. **Gate A04 闭环（CLOSE-01 PASS）**：2026-11-09 暖缓存 JSON 保留作历史快照；在最终源码 `8bef0c5`（含推荐修复）上已补齐 Compose 24 轮持续矩阵（`docs/benchmarks/feed-warm-cache-continuous-2026-11-10.json`，856,152 请求 0 失败，P95 $\le$ 103.15ms）与隔离 MySQL 8.4 无缓存 70/20/10 混合读写对照（`docs/benchmarks/feed-a04-mixed-2026-11-11.json`，196,612 请求 0 失败，Hybrid Following P95 +34.37%，冷 SQL `feed_recommend` P95 `69.11–87.58ms`），结合未改路径 fanout 排空证据（`feed-fanout-sustained-drain-30s-2026-11-05.json`），**Gate A04 逐项闭环 PASS**（逐项处置见 [G07](tasks/G07.md) 3.3.1 节）。
3. **Gate A10 闭环（CLOSE-02 PASS）**：在真实 Compose 联合栈上以正式规模（2,005 用户、20,010 视频、11,001 关注、50,002 互动）并严格验证停写维护窗口，完成 SQL 与 MinIO 全量恢复，实测 **RPO=0s、RTO=58.47s (/ready) / 63.16s (全业务+断点续传)**，6 大检查点全量通过且 0 资源残留（见 [G10](tasks/G10.md) 与 `docs/benchmarks/pr64-recovery-verified-2026-11-11.json`）；早先 `2026-11-09`/`a04-a10-review-2026-11-10.json` 组件钻井保留作历史参考。
4. **最终交付边界与发布前置**：A01–A12、本地 8 项 CI 等价测试与 Demo 联合探针均已在分支 `feat/close-03-final-delivery` 验证通过（详见 [G12](tasks/G12.md) 与 [PROGRESS](../PROGRESS.md)）；**最终签发 `v1.0.0` 仍待完成远端 GitHub Actions 8 项工作流验证并合入 `main` 且核对合并树一致性**。
