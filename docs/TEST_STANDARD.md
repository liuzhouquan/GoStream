# GoStream 测试与验收标准

更新日期：2026-10-31。此文档规定 [`DELIVERY_SCOPE.md`](DELIVERY_SCOPE.md) 所述求职展示版如何判定完成。带有“目标”的条件尚未通过；已运行过的命令和实测数字以 [`PERFORMANCE.md`](PERFORMANCE.md) 与 [`PROGRESS.md`](../PROGRESS.md) 为准。

## 判定规则

一个验收项只有同时满足以下条件才能标记为 **通过**：实现存在；自动化检查覆盖核心成功路径和至少一个失败/重试路径；要求真实组件的场景在隔离的 MySQL/Redis/RabbitMQ/MinIO 上跑通；结果及运行环境写入进度或性能文档。只在 SQLite、进程内 Redis 替身或浏览器单次手工操作中通过时，状态为 **部分通过**。尚无实现或没有执行证据时为 **未验证**。

每次合并功能前记录受影响的验收编号、执行命令、结果和限制。最终发布前所有编号必须通过；失败项不得用“已有代码”代替验证。测试数据只进入隔离数据库和对象桶；不得在正在演示的数据上运行会清空表或覆盖文件的脚本。

## 基础检查与环境

当前项目声明 Go `1.27.2`。遇到版本或工具链问题，先定位原因并向项目所有者确认处理方式，不自行降级。提交前的基础检查是：

```sh
go version
go test ./...
go test -race ./...
go vet ./...
go build ./cmd/gostream ./cmd/gostream-media ./cmd/gostream-events
node --check web/app.js
docker compose config --quiet
```

Compose 现在要求提供唯一的 `JWT_SECRET`（可放在已被 Git 忽略的 `.env` 中）；CI 的配置检查使用仅供测试的环境变量。直接运行本地 SQLite 配置默认只监听 `127.0.0.1:8080`。公网监听或通过反向代理对外开放时必须显式设置独立密钥；检查默认密钥被拒绝及伪造 JWT 返回 401。

账户存储错误由 `TestAccountStorageErrorResponses` 覆盖 SQLite 实际路由：真实用户名重复为 409，用户名不存在或密码错误为 401，损坏的密码哈希、数据库关闭后的注册/登录/资料查询为 500，响应不包含底层 SQL 错误。`TestMySQLAccountStorageErrorResponses` 在已迁移的隔离 MySQL 库中复验重复键 409、正常登录以及事务失效后的 500；需要 `GOSTREAM_TEST_MYSQL_DSN`，默认本地测试会跳过，CI 的 MySQL 任务执行。

群成员与上传会话的所有权查询由 `TestOwnedLookupStorageErrors`、`TestGroupMembershipLookupStorageError` 覆盖：本人可读、其他账户看到 404、数据库关闭或群成员表查询失败返回不泄漏 SQL 内容的 500。`TestMySQLOwnedLookupStorageErrors` 在已迁移的隔离 MySQL 库中验证相同的 200/404/500 分类；它需要 `GOSTREAM_TEST_MYSQL_DSN`，默认本地测试跳过，CI MySQL 任务执行。

Web 互动计数运行 `python3 tests/integration/web_action_counts.py`（需已安装 `agent-browser` CLI 和 Chromium）。脚本在回环地址启动临时静态服务器和 mock Feed/API，创建显示计数为 999、1000 和 10000 的卡片；触发点赞、取消，核对按钮的原始 `data-count` 和缩写文字，并对收藏执行同样的 1000 边界检查。请求失败时，原始计数、选中状态不变且按钮重新可用。该场景验证客户端展示逻辑，不作为真实后端计数一致性或完整 A09 浏览器流程的证据；后者仍需最终 Compose 环境复验。

浏览器认证缓存恢复运行 `python3 tests/integration/web_stored_session.py`（同样需要 `agent-browser` CLI 和 Chromium）。脚本使用隔离的回环静态服务与模拟 Feed：损坏 JSON、错误用户结构、缺失或空 token、缺失用户时清理本地认证缓存并显示游客 Feed；有效用户与 token 保留。脚本不连接正在运行的 GoStream API，也不验证 JWT 的服务端有效期；最终 A09 仍需在真实 Compose 环境用登录和刷新流程验收。

HTTP 服务对接收请求头和请求体设置十分钟总读取期限。`TestAPIServerBodyReadDeadline` 使用缩短的测试期限确认慢 body 会释放读取中的 handler，后续完整请求仍可处理；`TestHTTPReadTimeoutAllowsChunkAndWebSocket` 通过真实 HTTP 连接检查完整分片、超时的未完成分片不落库及重试，并在超过缩短的 HTTP 期限后检查 WebSocket 心跳。十分钟是配置上限，不代表已验证真实 250 MiB 慢速上传的吞吐；发布前仍需在目标网络运行大文件续传和多客户端慢连接验收。

公开高成本路由按连接 IP 设置固定一分钟预算，超限返回 429 和 `Retry-After`。`TestPublicRouteBudgetsUsePeerAddress` 验证注册、登录、播放计数、另一 IP 正常请求及伪造转发头无法绕过；`TestRateLimiterRedisAtomicAndLocalFallback` 验证双实例并发计数、Redis 断开时本地回退、恢复和指标；`TestRateLimiterLocalCapacityAndWindowRecovery`、`TestRateLimiterWindowRollover` 验证本机键数量上限、过期回收与分钟切换。`TestRateLimiterRealRedisIntegration` 需设置 `GOSTREAM_TEST_REDIS_ADDR` 指向隔离 Redis，检查真实 Lua 原子计数并清理测试键；CI 的 `redis-integration` 任务也运行该测试。固定窗口边界、代理后共享 IP、分布式来源和 Redis 故障期间各实例独立预算仍是容量限制，不代表已完成大规模防刷压测。

群邀请的 200 人上限由 `TestMySQLGroupMemberCapIntegration` 在已迁移的隔离 MySQL 库中验证：预置 199 人，两套独立数据库连接同时处理 16 个邀请，恰好一个成功、15 个返回 409，最终人数为 200；已在群中的用户于满员时重复邀请返回 200，且通知、未读游标和 outbox 各只新增一份。该测试需要 `GOSTREAM_TEST_MYSQL_DSN`，默认无外部依赖的 Go 测试会跳过；CI 的 MySQL 任务提供该环境。

Outbox 租约交接由 `TestOutboxLeaseHandoff`、`TestOutboxExpiredLeaseWithoutTakeover` 及 opt-in 的 `TestMySQLOutboxLeaseHandoffIntegration` 验证。测试在第一 worker 已领取、尚未结束发送时把租约时刻推进到过去，再让第二 worker 成功或死信：旧 worker 的失败不能改写已发布事件，旧 worker 的成功不能给死信补上 `published_at`；没有接管者时过期 worker 也不得确认。真实 MySQL 测试使用两套独立连接和已迁移的隔离库。`attempts` 只保证最终状态更新的租约所有权，不能防止租约交接前已经发生的实时重复投递。

视频处理器配置切换由 `TestVideoSourceReplacementRequiresProcessorForUnpublishedVideos` 和 `TestMySQLVideoSourceRecoveryWithoutProcessor` 覆盖：检查 `failed`/`processing` 的同源和替换源、事务回滚、所有权、无处理器的已发布视频、重新启用后的任务以及旧任务退出。后者需要已迁移的隔离 MySQL 库和 `GOSTREAM_TEST_MYSQL_DSN`。`tests/integration/video_recovery_restart.py` 使用临时 SQLite/媒体目录与真实 FFmpeg，启动三个 API 进程，验证超长视频失败、停用处理器时返回 409、重新启用后真实转码和关注 Feed；它不覆盖 MinIO 或多机配置切换。手工运行先构建 API，然后执行 `python3 tests/integration/video_recovery_restart.py --binary /path/to/gostream --ffmpeg /path/to/ffmpeg --ffprobe /path/to/ffprobe`。CI 的 `video-recovery-integration` 任务运行该脚本。

运行环境需要在记录中注明：Git commit、Go 版本、操作系统/CPU/内存、容器镜像版本、MySQL/Redis/RabbitMQ/MinIO 配置、数据库规模、是否启用 FFmpeg，以及具体命令。CI 最终至少执行 Go 测试、vet、构建和前端语法检查；race、真实组件集成测试和浏览器验收可以在单独任务中执行，但发布前必须有对应结果。

当前已有 opt-in MySQL 测试位于 `internal/platform/mysql_integration_test.go`，并依赖 `GOSTREAM_TEST_MYSQL_DSN`。CI 的 MySQL 任务先在全新服务库执行固定 SQL 的 v001–v004 安装，再用第二个隔离库检查已有数据采用、缺失索引恢复、从 v001 升级及重复运行，并比较两个库的列、索引和外键元数据。`tests/integration/mysql_recovery.sh` 另建两个临时库，执行真实 MySQL 逻辑备份、异库恢复、v002 回滚和重新升级，随后执行全部 `TestMySQL*` 平台测试。SQLite 迁移测试对照当前 GORM 模型、模拟建索引失败后重试、验证 v004 已完成任务的回填，并拒绝缺列旧库。现有 `tests/integration/media_smoke.py`、`media_failure_smoke.py`、`minio_smoke.py`、`minio_outage_smoke.py`、`minio_processing_outage_smoke.py` 和 `tests/load` 提供部分测试能力。`TestMinIOMediaAcrossInstances` 使用隔离桶验证双实例文件/分片、Range、完成重试和垃圾回收；`TestMinIOMediaTransfer` 验证最终媒体与有效续传分片导入、清单备份、恢复、重复运行和损坏/冲突拒绝。`TestMinIOMediaValidationAndRecovery` 在真实 MinIO 上用两套 API 路由覆盖匿名/越权、会话超限、分片长度与索引、冲突哈希、同大小损坏对象的重试修复、缺失对象恢复、重复完成及错误整文件哈希；配置 `GOSTREAM_TEST_MINIO_MYSQL_DSN` 时使用已迁移的隔离 MySQL，CI 在联合恢复演练后执行此组合。CI 的真实 MinIO 任务还运行 SQL 与对象联合恢复演练，并在新数据库和新桶完成切换前的上传会话。RabbitMQ 已有独立真实组件脚本；本地 `minio_outage_smoke.py` 使用实际双 API、MySQL、MinIO 容器，验证存储进程停止时的直接上传/分片失败、失败分片不落库、会话保留及恢复后的跨实例完成和字节读取；CI 另有 HTTP/TCP 故障注入覆盖。更广的读路径故障矩阵、视频编解码格式与权限负例仍待补齐。

MinIO 测试使用独立 Compose 项目和端口，不应对正在演示的 `gostream` 项目运行 `down -v`：

```sh
docker compose -p gostream-object -f docker-compose.yml -f docker-compose.object.yml -f tests/integration/object-test.compose.yml up -d --build --scale api=2
python3 tests/integration/minio_smoke.py --video-file /path/to/valid-over-4MiB.mp4
python3 tests/integration/minio_outage_smoke.py
python3 tests/integration/minio_processing_outage_smoke.py --video-file /path/to/valid-over-4MiB.mp4
docker compose -p gostream-object -f docker-compose.yml -f docker-compose.object.yml -f tests/integration/object-test.compose.yml down -v
```

媒体切换与联合恢复的步骤在 [`MEDIA_OPERATIONS.md`](MEDIA_OPERATIONS.md)。`media_transfer_smoke.py` 拒绝非空 MySQL 源库，并在停写窗口内导入旧媒体、备份 SQL 和对象、恢复到新库/新桶后读取旧 URL。

`minio_smoke.py` 要求真实 MP4 大于 4 MiB，用两个 API 端口分别发送分片并核对 MySQL 发布态、视频/封面内容、HTTP Range 和 Feed。两个停机脚本均在 `finally` 中启动隔离 MinIO，但仍应在结束后检查容器健康。处理任务停机脚本核对失败尝试期间视频不进入公共 Feed，恢复后完成第二次尝试并发布。Opt-in Go 测试可设置 `GOSTREAM_TEST_MINIO_ENDPOINT`、`GOSTREAM_TEST_MINIO_ACCESS_KEY`、`GOSTREAM_TEST_MINIO_SECRET_KEY` 后执行 `go test ./internal/platform -run '^TestMinIOMediaAcrossInstances$' -count=1`；它创建并删除随机桶。新增的故障矩阵测试同样创建独立桶；MySQL 模式会在隔离数据库留下测试用户和会话，测试结束时应销毁该项目的专用卷。

`TestImageUploadsRejectTruncatedPayloads` 在本地模式检查 JPEG、PNG、WebP 的完整与截断文件、直传与分片完成，以及 WebP 头像所有权。`TestImagePixelBudgetRejectsLargeHeader` 用有效 PNG 头声明 2000 万像素，确认在解码前拒绝且不登记资产。设置上述三个 MinIO 环境变量后运行 `go test ./internal/platform -run '^TestMinIOImagePayloadValidation$' -count=1` 可重复同一矩阵并使用随机对象桶。`TestMinIOProcessedOutputLostResponse` 用代理让真实 MinIO 成功写入 MP4/封面对象，却向 worker 返回 503；两次失败后核对对象清理、未发布状态和未登记资产，第三次任务尝试核对发布与对象字节。设置 `GOSTREAM_TEST_MINIO_MYSQL_DSN` 可改用已经迁移的隔离 MySQL。该测试直接提供已生成的输出字节来定位对象上传阶段；真实 FFmpeg 转码另由 `minio_smoke.py` 验证。

上传完成与清理由 `TestUploadCompletionKeepsLeaseDuringAssembly`、`TestUploadCleanupRechecksCompletionLease`、`TestUploadCompletionLeaseLossKeepsCommittedMedia`、`TestUploadCleanupRecoversAfterDatabaseFailure` 和 `TestUploadCompletionHeartbeatStopsOnFailure` 覆盖。它们检查暂停后的续租、清理候选状态变更、旧 worker 不删除新 worker 已登记的本地文件、清理事务失败后的租约接管，以及取消/数据库故障时心跳退出。`TestMinIOUploadCompletionCleanupLease` 需要真实 MinIO 和专用、已迁移且没有上传会话的隔离 MySQL 库（`GOSTREAM_TEST_UPLOAD_LEASE_MYSQL_DSN`）；它用两套独立 SQL 连接与代理暂停真实对象 GET，观察续租后推进清理时刻，检查最终对象哈希、唯一资产登记、跨实例完成重试及废弃会话清理。运行命令为 `go test -race ./internal/platform -run '^TestMinIOUploadCompletionCleanupLease$' -count=1 -v`，MinIO 配置沿用上述三个环境变量；CI 会另建专用库。该用例模拟清理时间边界，不代表已测量真实长时间传输、进程强杀或存储完全中断。

RabbitMQ 验证运行 `python3 tests/integration/rabbitmq_smoke.py --go-bin /usr/local/go/bin/go --mysql-container gostream-mysql-1 --mysql-addr 127.0.0.1:3307`。脚本只在随机 MySQL 库/用户和临时 RabbitMQ 4.3.7 上写入，运行真实 broker 矩阵的 race 测试、API 后台 worker 自动投递、SIGTERM 退出及运维 CLI，结束后删除资源。它需要 Docker、可拉取的固定镜像以及 MySQL 容器内部的 root 密码环境；CI 新增 `rabbitmq-integration` 任务执行同一入口。直接运行 `TestRabbitMQNotificationIntegration` 需要 `GOSTREAM_TEST_RABBITMQ_URL`、专用且已迁移的空 outbox 库 `GOSTREAM_TEST_RABBITMQ_MYSQL_DSN`；只有显式提供允许停止的临时 `GOSTREAM_TEST_RABBITMQ_CONTAINER` 才执行 broker 重启分项。用例覆盖发布确认/不可路由、确认后 SQL 完成丢失、消费提交后 ACK 丢失、两消费者同时处理 16 份重复投递、瞬时/持续 SQL 故障、死信回放、重启恢复和实际 WebSocket 内容。无环境时普通 Go 测试跳过此矩阵。详细保证、命令和未验证窗口见 [`RABBITMQ.md`](RABBITMQ.md)。

Smoke runner 还在隔离 broker 上预先为生产 quorum DLQ 安装单消息 operator limit，填入 filler 后经真实 API consumer 投递两个 poison。验收检查目标受限期间源队列保留待转移消息；释放已占目标容量后，所有原始 poison ID 必须最终出现，源队列清空且 DLQ 可稳定排空。由于 quorum at-least-once dead lettering 可能有在途接受和重复副本，测试不把瞬时 queue depth 当作严格硬上限，也不要求 DLQ 副本恰好一份。Go 1.27.2、MySQL 8.4.11、RabbitMQ 4.3.7 本地运行 exit 0、176.173 秒，无超时；12 个 Go race 子测试通过，runner 报告 `dlq_capacity_backpressure_recovery=true`、300 条 sustained source event、schema/user 清理为0及 API 退出。原始计时报告位于 ignored `data/diagnostics/rabbitmq-a06-dlq-capacity-20261105f.json`；详见 [`RABBITMQ.md`](RABBITMQ.md)。

三节点 quorum follower 故障场景运行 `python3 tests/integration/rabbitmq_smoke.py --rabbitmq-cluster --cluster-only --go-bin /usr/local/go/bin/go --mysql-container gostream-mysql-1 --mysql-addr 127.0.0.1:3307`。它创建临时三节点 RabbitMQ 4.3.7 集群，停止一个非 leader 且非 API 主节点的 follower，使用真实 API 完成一次 follow 通知 source publish 和 delivery consumer 持久化，再启动 follower 并等待三个队列副本全部在线。Go 1.27.2、MySQL 8.4.11、RabbitMQ 4.3.7 本地执行 exit 0、61.773 秒，无超时，报告 `cluster_follower_failure_recovery=true`；schema/user 已清理，API 子进程退出。报告在 ignored `data/diagnostics/rabbitmq-three-node-follower-targeted-v2-20261105.json`。加入 `RABBITMQ_URLS` 后同场景回归也通过，65.751 秒，报告 `data/diagnostics/rabbitmq-three-node-follower-v3-20261106.json`；本轮共享 runner 清理逻辑回归再次通过，60.984 秒，报告 `data/diagnostics/rabbitmq-three-node-follower-v4-20261106.json`。完整 `--rabbitmq-cluster` 本地复跑还执行了12个 race 子测试、两个真实 API 强杀窗口、300条持续 outbox、DLQ 容量恢复和运维 CLI，Go 1.27.2/MySQL 8.4.11/RabbitMQ 4.3.7 下 249.608 秒 exit 0、无超时，所有资源清理完成。集群 DLQ 恢复最多等待60秒；20秒首轮未通过，扩大窗口后全量通过。完整报告在 ignored `data/diagnostics/rabbitmq-three-node-full-acceptance-v2-20261105.json`。另外，`--rabbitmq-cluster --cluster-leader-only` 停掉当前 quorum leader，等待选主，通过 `RABBITMQ_URLS` 让生产 API 切换到存活节点并完成通知投递；Go 1.27.2/MySQL 8.4.11/RabbitMQ 4.3.7 本地 exit 0、86.373 秒，leader 从 `rabbit-test-1` 切到 `rabbit-test-3`，恢复后原成员全在线，schema/user 清理且 API 退出。报告在 ignored `data/diagnostics/rabbitmq-three-node-leader-v2-20261106.json`。多数派故障未覆盖；多数派恢复用例 `--rabbitmq-cluster --cluster-majority-only` 另外停两个节点，检查 API 已提交但 source 未确认且没有 delivery 行；恢复第二节点后，在第三节点仍离线时要求 outbox source 与唯一 delivery 均完成，再恢复最后节点。Go 1.27.2/MySQL 8.4.11/RabbitMQ 4.3.7 本地 exit 0、109.304 秒，报告 `source_retained_during_majority_loss=true`、`delivery_resumed_after_majority_restore=true`，清理 schema/user/API。报告在 ignored `data/diagnostics/rabbitmq-three-node-majority-loss-20261106.json`。CI follower、leader 和 majority recovery 步骤的远程结果待确认。

真实进程的 consumer commit/pre-ACK 场景由 smoke runner 启动生产 API 并经测试 TCP 代理暂扣 AMQP `basic.ack`，确认 MySQL 中唯一 delivery outbox 行已提交后 SIGKILL API；重启后要求消费者将重投事件判为 duplicate，通知与 delivery 行仍各一条且队列排空。producer post-confirm/pre-SQL 场景在 RabbitMQ 暂停时先锁住已领取的 source outbox 行，恢复 broker 后要求 `published` confirm 指标增长、消费者收到事件且应用连接确实在 `performance_schema.data_lock_waits` 等待该源行的 `published_at` 更新，再 SIGKILL API；释放锁并重启后要求同 ID 重发、状态最终完成且通知/投递记录均唯一。代理帧处理另由 `python3 -m unittest discover -s tests/integration -p 'test_rabbitmq_ack_proxy.py' -v` 验证，CI quality job 会运行该测试。runner 还以每秒一批、每批十条的节奏持续 30 秒写入 300 组 SQL outbox 夹具；停写后检查全部 source confirm、delivery 完成、通知行数、SQL 死信和 RabbitMQ 队列排空。此场景验证持续输入期间真实 worker 的正确性，不是 API 写入压测。

IM 双进程故障验证运行 `python3 tests/integration/im_smoke.py --go-bin /usr/local/go/bin/go --mysql-container gostream-mysql-1 --mysql-addr 127.0.0.1:3307`。脚本建立随机 MySQL 库/用户与临时 Redis 8.2.9，以 `-race` 编译生产 API，并以 `-race` 运行 `tests/integration/im`。两个真实 API 进程各持有独立 SQL/Redis 连接，检查同键并发发送、冲突拒绝、单聊/群聊远端提示和发送者其他设备回显、重复 Redis 提示、越权和混合无效 ACK 批次、Redis 停机后的 SQL 补读/重订阅、双方真实 SIGKILL 与重连/重试、105 条单聊和 105 条群消息的 100 条重放上限、完整历史补页、逐设备 ACK，以及群成员移除后的访问拒绝。SIGKILL 检查操作系统退出信号，正常关闭检查退出码和子进程竞态报告。需要 Linux、C 编译器、Docker 和 MySQL 容器内的 root 密码环境；独立 `im-integration` CI 任务执行相同入口，再以 `--check-timeout-cleanup` 故意在 API 启动后触发 Go 十秒超时，要求外层脚本成功回收其进程组、临时目录、Redis 和 SQL 资源；内层超时是预期失败，外层清理失败必须让 CI 失败。普通 Go 测试在没有四个 `GOSTREAM_TEST_IM_*` 环境变量时跳过该矩阵；优先使用自动隔离资源的脚本。此项不代替两浏览器验收、群前端完整补页、稀疏回执查询成本或 20 客户端/1,000 消息压测，详见 [`IM.md`](IM.md)。

群提交顺序回归为 `go test -race ./internal/platform -run '^TestMySQLGroupCommitOrder$' -count=3 -v`，需要已迁移的隔离 `GOSTREAM_TEST_MYSQL_DSN`。两套独立 SQL 连接/路由在真实 MySQL 插入后暂停事务，要求同群后续提交等待、回滚释放锁、不同群能继续提交、ACK 低 ID 后高 ID 仍未读、重试保持单行。旧代码已复现“高 ID 已 ACK、低 ID 迟提交后未读为零”。此测试使用进程内故障注入，不等同于真实进程强杀；后者继续由 IM 矩阵验证。前端 P1-09 的退出标准：105 条以上积压逐页完整显示；较高实时 ID 先到或切换群/重连时先补历史再推进累计 ACK，DOM 与 SQL 游标都要断言，不得只测 API 分页。

## 功能验收矩阵

| 编号 | 最终必须验证的结果 | 最低证据 | 当前判断 |
| --- | --- | --- | --- |
| A01 启动与持久化 | 空库一键启动后 `/health`、`/ready`、`/metrics` 可访问；重启 API 后账号、视频、互动和消息仍在；依赖故障时 `/ready` 的语义与文档一致 | Compose、真实 MySQL/Redis/RabbitMQ/MinIO、重启记录 | 部分通过：MinIO 覆盖配置可启动，RabbitMQ 独立停机/重启恢复及真实 API worker 生命周期已验；最终全组件 Compose 持久化恢复仍待验 |
| A02 身份与权限 | 注册、登录、JWT 过期/伪造、资料和头像修改、关注与取消；未登录者和其他用户不能修改私有资源；重复关注结果一致 | API 单元/集成测试 + 两账号浏览器操作 | 部分通过：主路径已有，最终部署权限矩阵待复验 |
| A03 视频与对象 | 文件和分片上传能在断点后续传；同一会话可跨两 API 实例完成；错块、错哈希、越权、超限均拒绝；FFmpeg 成功后视频/封面可从 MinIO 读取，失败后不进入公共 Feed；重试、删除、孤儿清理无误删 | 真实 MySQL/MinIO/FFmpeg + 视频探测 + 故障测试 | 部分通过：真实 MinIO 双实例续传、FFmpeg、Range、对象清理及停机后上传恢复已验；离线旧媒体导入及隔离 SQL/对象恢复已验；分片与资产负例已在真实 MySQL/MinIO 上验证；输出 PUT 成功响应丢失及 JPEG/PNG/WebP 截断文件已验；存储完全中断、更多视频格式/权限场景与较大规模恢复成本待验 |
| A04 Feed | 时间线、关注、热榜、推荐游标分页；推拉切换后关注结果一致；缓存故障回退与跨实例版本失效正确 | Go 单测、真实 MySQL/Redis、两实例及混合负载 | 部分通过：完整无缓存混合读写 run #41803621425 通过 +20% Following P95 门槛（Hybrid 26.250ms、SQL-only 46.051ms，改善43%），48,300请求、120次发布及一致性检查全通过。跨实例版本失效、独立进程 Redis 停机/SQL回退/恢复已有真实组件测试；暖缓存12并发、每场景240请求、100%命中、零候选SQL的有界实测已补完整24视频及正文计时断言。320关注者、120视频、约30秒写入和38,400条inbox的双进程 fanout 排空及3页完整性已验。证据见 [审查记录](PENDING_BRANCH_REVIEW.md)、[实测 JSON](benchmarks/pending-branch-review-2026-11-05.json) 和 [PERFORMANCE](PERFORMANCE.md)；20并发持续暖缓存性能门槛、不同关注规模和多主机仍待验。 |
| A05 互动与计数 | 并发重复点赞/收藏、取消及关注只有一次状态转换；计数与明细一致且非负；评论回复层级、权限和通知正确；播放计数策略明确 | 真实 MySQL 并发集成测试，结束后核对表和计数 | 部分通过：真实 MySQL 曾因 1213 死锁返回 500；动作事务现按整个事务最多重试 10 次并加入随机退避；双连接、20 worker、每轮 80 次热点 like/favorite 切换在本地 MySQL 8.4.11 重复 30 轮均通过且计数匹配，MySQL 8.4.13 并发集成 CI 已通过；多用户/多视频、播放量和完整通知矩阵待验 |
| A06 异步事件 | 业务事务失败不留待发事件；提交成功后 broker 停机也不丢事件；发布确认后重复投递不产生重复通知；消费者重启、失败重试和死信可恢复 | 真实 MySQL/RabbitMQ 故障注入 + outbox/消费者状态核对 | 部分通过：确认/不可路由、确认后 SQL 状态保留重试、SQL 提交后断连重投、并发去重、SQL 重试/死信/回放、DLQ 目标解绑恢复和真实 API worker 已有组件矩阵；本地 Go 1.27.2/MySQL 8.4.11/RabbitMQ 4.3.7 已验证 consumer commit/pre-ACK 和 producer post-confirm/pre-SQL 两个真实 API SIGKILL、重投、幂等与队列排空；30秒/300条持续 outbox 输入全部确认并在末批后1.213秒排空；单消息限容 DLQ 回压/恢复验收要求源端保留、原始消息身份完整及队列稳定排空；长时间运行与最终联合配置仍待验；三节点 follower/leader 故障、临时失去多数派后的 outbox 恢复及 `RABBITMQ_URLS` 客户端切换已有本地真实集群结果 |
| A07 IM | 两实例间单聊/群聊、成员权限、同一 `client_id` 重试、每设备 ACK、离线重放、超过 100 条的历史补页都正确；Redis 中断后重连能补齐；相同会话历史按服务端 ID 排序，重复推送可去重 | 两浏览器配置 + 两 API 实例 + 真实 MySQL/Redis 故障场景 | 部分通过：真实双 API/MySQL/Redis、双方 SIGKILL、Redis 停机/重订阅、重复提示、105 条单聊/群聊补页、逐设备 ACK 和群权限撤回已验证；同群提交行锁另有 MySQL 故障回归；前端补页/安全 ACK（含 350 条缺口/失败/过时响应）与真实 MySQL 成员撤回另有回归；最终两浏览器、默认 presence 完整过期已接入真实进程 CI；v005 活跃回执压缩及强杀恢复已验；1k/10k 稀疏查询已测量并优化；20 连接单聊三轮负载已验；群聊扇出与最终部署仍待验 |
| A08 通知与搜索 | 关注/点赞/评论/私信通知按权限显示，分页和有界已读不吞掉新通知；搜索用户与已发布视频支持独立分页和转义字符，隐藏视频不出现 | 真实 MySQL + 浏览器分页测试 + 查询计划记录 | 部分通过：功能已验证，较大数据量查询计划待补 |
| A09 Web 全流程 | 两个独立浏览器账号完成注册、续传发布、Feed、互动、主页、搜索、通知、单聊和群聊；刷新/重启后状态一致；手机宽度下主要操作可用 | 最终 Compose 版本的浏览器记录，附账号/数据清理说明 | 部分通过：现有功能分批验证，完整最终配置待验 |
| A10 数据升级与运维 | 新库安装、从当前 schema 升级、重复运行迁移、失败恢复均有结果；CI 通过；Prometheus 指标可解释；备份与恢复操作有记录 | 版本化迁移测试 + CI 运行记录 + 恢复演练 | 部分通过：v004 本地新库安装、旧库回填和隔离 MySQL 备份/恢复重放已通过；CI 运行 40441635650 的质量、MySQL、MinIO 任务均通过；在线备份一致性与较大规模恢复成本待验 |
| A11 安全边界 | 上传文件名和路径不能越界；会话、私信、群、未发布视频和管理操作都有身份/所有权检查；密码不明文存储，公开响应不泄露内部错误或 JWT | 自动化负例 + 最终浏览器/API 复验 | 部分通过：现有负例覆盖一部分路径 |
| A12 文档契约 | README 能从空环境启动最终配置；API 参数、错误、鉴权、WebSocket 帧及数据库迁移说明与代码一致；机器可读 API 契约经过校验；学习和面试指南只列已实现内容 | 文档链接/示例核对 + OpenAPI 校验 + 最终代码审阅 | 部分通过：已提供 OpenAPI 3.0.3，结构/引用和代表性真实响应、空列表、错误及实际 WebSocket 帧接受校验；完整请求约束/所有响应变体及最终部署文档复验仍待完成 |

## 故障和一致性用例

验收时必须实际执行并记录以下动作及数据库/对象存储结果：

1. **事务中断**：在点赞或发布事务失败后，业务行、计数、通知与 outbox 不出现半提交。
2. **重复请求**：相同点赞、上传完成、消息 `client_id`、broker 事件 ID 各重试至少两次，确认最终状态与单次执行相同；冲突内容返回明确错误。
3. **单组件停机**：Redis 停机时 Feed 走数据库且历史/未读可用；RabbitMQ 停机时已提交事件留在 outbox，恢复后最终消费；MinIO 停机时上传/处理进入可重试的失败状态，不发布缺失对象。
4. **进程重启**：API、outbox 发布者、消费者和 FFmpeg worker 在持有租约或处理事件时重启，过期租约能回收，已提交事件与视频只形成一次最终效果。
5. **双实例**：同一上传会话跨实例继续；一个实例上的发送者和另一个实例上的接收者互发单聊/群聊；Redis Pub/Sub 中断期间的消息在重连后从 MySQL 补齐。
6. **权限切换**：取消关注、退出/移除群成员、删除视频后，缓存、搜索、历史和实时推送都按当前权限返回；不得依赖旧浏览器状态放行。

每项检查都要给出最终数据库或对象状态，不能只凭 HTTP 200 判断成功。故障脚本运行在隔离环境，结束后验证测试数据和临时文件已清理。

## 性能验收方案与目标

下表是**预设目标，不是现有成绩**。先在相同环境测当前 SQL 拉取版基线，再测最终实现；保留原始结果和命令。参考宿主机为现有 WSL2 Linux、AMD Ryzen 7 5800H；Docker Desktop 分配的 CPU/内存及 MySQL 参数须记录，若换主机，对照组和最终组必须在同一台机器上测。最低数据集为 2,000 用户、20,000 已发布视频、10,000 关注关系、50,000 互动记录，其中至少一位作者的粉丝数超过混合 Feed 阈值，另一位低于阈值。测试账号覆盖匿名、已关注、多兴趣互动三类读者。所有压测使用隔离数据库；视频内容可以使用合成元数据，但媒体流程另用真实文件测试。

| 场景 | 方法 | 发布门槛 |
| --- | --- | --- |
| Feed 读流量 | 关注首页/深页、已登录推荐、热榜；冷缓存和预热缓存分开；10 与 20 并发用户，每组预热 30 秒、正式运行 60 秒，重复 3 次 | 无意外 5xx、无分页漏项/重复；20 并发、预热缓存下关注和推荐的三次运行 P95 均不超过 200 ms、P99 均不超过 500 ms |
| 混合推拉收益 | 相同数据与并发下比较原 SQL 拉取和最终实现的无缓存关注首页；同时记录发布请求延迟与 fanout 积压 | 普通作者场景三次运行的 P95 中位数至少降低 20%；新视频最终进入目标粉丝 Feed，队列在停止写入后 60 秒内清空；若没有收益，重新设计并重测 |
| 读写混合 | 70% Feed、20% 点赞/取消及收藏、10% 关注/评论，20 并发运行 60 秒并重复 3 次 | 无意外 5xx；结束后互动明细和计数一致，outbox/消费者最终清空或有明确死信原因；记录 QPS、P50/P95/P99、CPU、内存、数据库连接和队列深度 |
| IM 与媒体 | 至少两实例、20 个活跃 WebSocket 客户端发送 1,000 条消息；另用 10 个真实短视频走上传、转码、封面和发布 | 消息 ID 无重复持久化、离线补页无缺失；10 个视频均到达明确 `published` 或有解释的 `failed` 终态；记录消息延迟、转码排队时间、处理时间和 CPU，不臆造吞吐结论 |
A04 digest 诊断另需在30秒只读Following warmup后、60秒混合实测后各采集 Performance Schema 快照；正式API P95和CPU/RSS窗口是后60秒。digest窗口用于解释，不替代+20% client P95 gate。

上述绝对延迟目标只适用于记录的本地 Compose 环境与该数据集，不构成生产容量承诺。压测报告必须列出每次结果、错误率、缓存命中率、MySQL 查询计划、资源占用及对照方案。若本机资源不足以运行该数据集，应先记录瓶颈并优化测试环境；不能缩小样本后沿用同一通过结论。

## 发布记录模板

每个验收编号在 `PROGRESS.md` 或 `PERFORMANCE.md` 留下：

```text
编号/提交：
环境与组件版本：
数据集与初始状态：
命令或浏览器步骤：
预期不变量：
实际结果（含失败、修复、复验）：
日志/指标/数据库核对：
遗留限制：
结论：通过 / 部分通过 / 未验证
```

发布前由此矩阵逐项复核，任何“部分通过”都需要补证据或明确缩小公开声明的范围。


## 七分支复核后的新增验证入口

- `python3 tests/integration/web_group_sync.py`：隔离静态服务/模拟 API 和 Chromium，验证 105 条初始分页、增长至 350 条的多页补读、历史 503 时不推进 ACK、恢复重试、切群丢弃过时历史及 ACK 响应。需要 `agent-browser`；不代替真实 SQL/浏览器端到端。
- `go test -race ./internal/platform -run '^TestMySQLGroupMemberRevocationRace$' -count=3`：已迁移的隔离 `GOSTREAM_TEST_MYSQL_DSN`，验证 send/ACK/成员移除事务顺序和移除后无残留游标。
- `go test -race ./internal/platform -run '^TestMinIO(CompleteStorageOutage|MediaFormatAndPermissionMatrix)$' -count=1 -v`：隔离 MinIO 三个环境变量，随机桶。503 由反向代理注入，SQL 为 SQLite；测试检查对象桶清理错误，不是 TCP 断连或 MySQL/MinIO 联合故障。CI 已接入该入口。
- 媒体矩阵的 WebM/MOV 样本是文件头，验证签名/扩展名和所有权，不证明可解码或真实 FFmpeg 转码/缩略图；真实编解码格式矩阵继续待验。媒体 URL 可公开读取的现状仅由本地测试覆盖，产品隐私边界不据此自动关闭。
- RabbitMQ 的历史一般重启/五条积压检查已由后续 A06 验收扩展：producer post-confirm/pre-SQL 和 consumer commit/pre-ACK 真实 SIGKILL、300 条持续 outbox、DLQ 容量恢复，以及三节点 follower/leader 故障均有本地实际组件报告；细节和仍未覆盖的多数派丢失/长时运行见 [`RABBITMQ.md`](RABBITMQ.md)。
- A12 保持部分通过。未合入的 OpenAPI 路由测试不校验参数/响应 Schema，也不能关闭 WebSocket 帧契约验收。


## 2026-10-30 增补的验证边界

- `TestOpenAPI*` 使用固定版本 kin-openapi 校验结构，并将代表性真实响应及 WebSocket 捕获帧交给规范中实际引用的 Schema。旧的字段名检查保留为补充；它们自身不能证明契约正确。
- IM 进程矩阵验证默认 75 秒崩溃节点租约，较上一版多等待约 75 秒；不得改成短租约后沿用默认过期通过的声明。
- `web_full_flow.py` 要求真实 FFmpeg/ffprobe，使用隔离 SQLite、媒体目录和两个浏览器会话，验证可解码视频、资料保存、互动/通知/聊天与真实 API 重启；清空共享服务配置，所有子进程在删目录前回收。CI 的 `browser-workflow` 执行它以及三个既有浏览器回归。它不代表最终 MySQL/Redis/RabbitMQ/MinIO Compose 双实例验收，也未覆盖浏览器中途断上传。
- `TestFaultScenario1_TransactionInterruptionNoHalfCommits` 在通知 INSERT 之后注入真实错误，检查点赞/评论行、计数、通知和 outbox 回滚。其他新增 SQL 回退、手工租约条件、共享 SQLite 路由测试是组件回归；第六节要求的真实停机、强杀和双实例场景继续由专用脚本/后续工作覆盖。
- 查询计划的新 MySQL 探针使用每张业务表 500 行和回滚事务，输出实际 EXPLAIN；查询仍是索引探针，不能等同于生产完整双向私信/混合 Feed SQL 的规模验收。见 `QUERY_PLANS.md`。
- RabbitMQ 增补暂停 broker 后强杀已领取源事件、50 条有限积压、确认发布后 SQL 完成遗漏重试及 DLQ 原消息恢复。领取状态不证明已收到确认，省略 SQL 更新不等于真实断连；消费者提交后 ACK 前的实际进程强杀、持续负载和 DLQ 容量边界仍待验证。
- WebM/MOV 新测试仍是容器头识别。新增搜索/未发布视频隐私在本地和真实 MinIO 复用同一矩阵；真实 WebM/MOV 转码及规模恢复仍待完成。


## 2026-10-31 逐设备回执压缩工作包

`TestDeviceAckRange*` 覆盖精确缺口、1,000 条连续 ACK、重试、新设备/旧客户端、WebSocket 重放、区间写入后的回滚与过期删除；`TestConcurrentDeviceAckRangesSQLite` 验证八个并发请求的新增总量。`TestMySQLDeviceAckRanges` 用独立连接池、事务屏障、重叠 ACK 与其他用户并行，以及低 ID 迟提交验证相同语义；MySQL CI 普通运行并额外重复三次 race。迁移 CI 在隔离 legacy 库验证 v004→v005，现有恢复脚本检查五个版本。IM 进程矩阵增加压缩后 SIGKILL/重启、设备间未读隔离与严格的重放结束屏障。真实组件证据以本工作包 PR 最终提交的 CI 为准，本地跳过不是通过。

该方案不承诺稀疏回执常数空间或生产吞吐收益。更大规模区间查询、规定 IM 负载及最终全组件浏览器仍需单独验收，A07 保持部分通过。升级前停止旧 API 并备份；回退不能只删除区间表。详见 [设计及升级约束](DEVICE_ACK_RANGES.md)。


## 2026-10-31 稀疏 ACK 查询后续工作包

`TestMySQLDeviceAckQueryProfile` 仅在 `GOSTREAM_TEST_ACK_QUERY_PROFILE=1` 且指定已迁移隔离 MySQL DSN 时执行，CI 单独启用。每次检查 1k/10k 区间、首/中/尾/空页的精确 ID，保留三个交替样本与实际计划。`TestDeviceAckRangeUnreadCursorBoundaries` 检查 HTTP 端点/间隙/设备及全局 ACK；`TestDeviceUnreadAcrossGloballyAckedRanges` 检查超过区间批次容量的隐藏历史与读取失败/恢复。MySQL 原有回归加入候选读取后另一连接真实提交 ACK 的快照屏障。实测环境、入口、退出条件及最终 CI 见 [DEVICE_ACK_QUERY.md](DEVICE_ACK_QUERY.md)。该证据不替代规定 WebSocket 负载或最终 Compose/浏览器验收，A07 仍部分通过。


## 2026-10-31 两实例 IM 单聊负载

[IM_LOAD.md](IM_LOAD.md) 固定 20 个正式账户/连接、两个 API 实例、每轮 1,000 条跨实例单聊、三轮、256 字节正文和至多 20 条等待发送 ACK 的命令。CI 通过 `im_smoke.py --suite load` 运行常规构建，并通过 `--race-load` 单独复验竞态；普通 Go 测试只运行驱动自身的错误/统计/准备逻辑回归，未配置隔离环境时跳过真实负载。报告先写临时目录，只有 Go 测试和所有资源清理成功才发布，CI artifact 与仓库无损 gzip 原件保留逐消息/资源样本。

实际证据包括三轮精确 ACK/投递集合、发送重试、125 条离线消息、100 条重放/同步标记、完整未读与 425 条双向历史、275 条新设备独立未读和 3,125 条 SQL 唯一身份。CPU/采样 RSS、抓取耗时及普通/race 延迟明确分开。它验证该 IM 子场景；性能表“IM 与媒体”仍有十个真实视频、最终环境和其他数据要求待补，A07 继续部分通过。


## 2026-10-31 三分支审核后的验证边界

- `following_feed.go` 去掉 hybrid 读取前的 EXISTS，保留空关注、旧 inbox 和取关过滤；它改变查询数量，不证明 HTTP P95。`BenchmarkFollowingFeed` 明确写入并检查 feed_pull=false、关注早于发布、精确 ID/游标和连接回收；SQLite 500 视频模式比较不是优化前后对照，更不是完整 MySQL 门槛。
- MinIO TCP 故障必须计数实际 hijack/关闭，严格返回 503，失败分片不落行；恢复后核对 completed 会话、唯一资产和实际对象字节。头像权限仍为既有接口的新增回归，不宣称新增实现。
- `TestLivenessSurvivesClosedSQLPool` 只验证 SQLite 连接池关闭后的 health=200/ready=503，未配置 broker/Redis/MinIO，不能称 broker 故障或最终部署验证。
- 49 个 Gin 路由的方法/路径映射与代表性响应/帧 Schema 校验是不同证据。现有浏览器流水线使用 SQLite/FFmpeg；最终 MySQL/Redis/RabbitMQ/MinIO 双实例、两个浏览器、断点续传与恢复验收仍待完成。
- broker 新增的确定性 TCP 断连不等同于真实进程强杀。独立事件混合流量必须逐 ID 验证持久接收/派发，DLQ 必须核对完整 ID 集合及原因，不能只凭深度或 HTTP 200。

原问题、修正、实际结果及发布缺口见 [三分支审核](BRANCH_REVIEW_2026-10-31.md)。

## 2026-11-02 Feed 查询变更验证边界

- Hybrid following 的三个候选分支使用互斥条件后改为 `UNION ALL`；公开 HTTP 回归覆盖未完成 fanout 的部分 inbox 和迟到关注与 inbox 重叠。Go 1.27.2 SQLite 500 视频 handler benchmark 中位耗时下降约 4.8%，不等于 MySQL 延迟或完整数据集收益。
- A04 的完整数据集读对比已在 2026-11-02 实际运行：20 并发、30 秒预热、60 秒测量、SQL-only/Hybrid 各三轮，前 40 个 ID 一致、零请求失败，median P95 改善 36.16%，见 [run #41452090999](https://github.com/liuzhouquan/GoStream/actions/runs/41452090999) 和 [`PERFORMANCE.md`](PERFORMANCE.md)。该流程关闭无缓存读取收益子门槛；仍不覆盖混合写入、写入中的 fanout 排空、Redis 缓存/故障回退或双实例故障。
- A04 完整混合读写 workflow [run #41478569849](https://github.com/liuzhouquan/GoStream/actions/runs/41478569849) 已完成：六轮请求、发布、计数、Feed 可见性、outbox/fanout 均通过，但 Hybrid median following P95 比 SQL-only 慢 36.82%，所以性能门槛未通过。详见 [`PERFORMANCE.md`](PERFORMANCE.md)；保持 SQL-only 默认，只有有证据的优化才可改变该结论。
- 对原混合读写夹具的复核发现 20 个 worker 轮流复用 10 个 reader 身份和目标。修正后的 run [#41566643755](https://github.com/liuzhouquan/GoStream/actions/runs/41566643755) 使用20个 reader，零请求失败、120次发布成功且一致性全部通过，但 Hybrid median following P95 仍比 SQL-only 慢28.48%，性能门槛未过。该 run 仍不能将端到端差异归因到数据库查询；摘要与逐轮数据见 [`PERFORMANCE.md`](PERFORMANCE.md) 和 [`benchmarks/feed-a04-mixed-2026-11-03.json`](benchmarks/feed-a04-mixed-2026-11-03.json)。
- [A04 run #41590181656](https://github.com/liuzhouquan/GoStream/actions/runs/41590181656) 完成六轮混合负载及全部一致性检查，但 Hybrid following P95 中位数 75.102 ms、SQL-only 53.334 ms（慢40.81%），20%门槛仍未通过。数据库 pool wait 均为0；prepared execute / `set_option` 是账户级事件聚合，不足以定位 SQL 或连接创建根因。API 新增可配置 MySQL pool cap 和 `MaxIdleClosed` 指标，须等同条件复测后再判断效果。逐轮数据见 [`PERFORMANCE.md`](PERFORMANCE.md) 与 [`benchmarks/feed-a04-prepared-and-pool-diagnostics-2026-11-03.json`](benchmarks/feed-a04-prepared-and-pool-diagnostics-2026-11-03.json)。
- [A04 run #41595760362](https://github.com/liuzhouquan/GoStream/actions/runs/41595760362) 在连接池默认 50/25 后完成所有一致性检查：45,110 个混合请求、120 次发布均成功，pool wait 与 idle-cap 关闭均为0。`statement/sql/set_option` 从此前每轮约3.7k/11.8k降到17，MySQL `Threads_connected` 稳定在23；Hybrid following P95 仍慢45.77%（77.990 ms 对53.502 ms），连接复用改善未解决 Hybrid 尾延迟。
- [A04 run #41601458252](https://github.com/liuzhouquan/GoStream/actions/runs/41601458252) 完成 62,549 个混合请求、120 次发布和全部一致性检查；SQL-only median following P95 45.027 ms，Hybrid 59.553 ms（慢32.26%），+20% gate 未通过。新增的 SQL-only/Hybrid 全尺寸诊断轮均成功生成 60 秒 CPU profile 和 goroutine 快照，未参与六轮 gate。profile 未显示单一 Go CPU 热点；快照各有约20条 MySQL结果读取栈，未发现 mutex/semaphore 等待。MySQL account级 prepared EXECUTE 仍没有单 SQL 归因；独立 MySQL 8.4 probe 已验证 `interpolateParams=true` 可将事件从无 SQL_TEXT 的 `statement/com/Execute` 变为带 SQL_TEXT/digest 的 SELECT。A04 已新增默认关闭的插值实验开关，下一轮需验证 P95 和具体 query digest；详见 [`PERFORMANCE.md`](PERFORMANCE.md)、[`feed-a04-pprof-2026-11-03.json`](benchmarks/feed-a04-pprof-2026-11-03.json) 和 [`feed-a04-pprof-2026-11-03/`](benchmarks/feed-a04-pprof-2026-11-03/)。
- [A04 run #41607464424](https://github.com/liuzhouquan/GoStream/actions/runs/41607464424) 在 synthetic workload 显式开启 `interpolateParams` 后完成 64,015 个混合请求、120 次发布和全部一致性检查；SQL-only median following P95 38.483 ms，Hybrid 55.515 ms（慢44.26%），+20% gate 未通过。Hybrid candidate-query API P95 桶连续三轮高于 SQL-only，interaction-state 桶相同；这不是 MySQL 执行时间的单独测量。逐 SQL digest 只在六轮中的三轮可用，Hybrid 一条语句的直方图计数还多出五个样本，P95 被留空；原运行未保留失败阶段。runner 已补经过脱敏的失败阶段记录测试，下一次运行再确认实际失败位置。详情见 [`PERFORMANCE.md`](PERFORMANCE.md) 和 [`feed-a04-interpolate-2026-11-03.json`](benchmarks/feed-a04-interpolate-2026-11-03.json)。
- [A04 rerun #41614057041](https://github.com/liuzhouquan/GoStream/actions/runs/41614057041) 完成 64,134 个混合请求、120 次发布及所有一致性检查，SQL-only median following P95 为 37.587 ms、Hybrid 为 59.134 ms（慢 57.33%），性能门槛未通过。五轮 digest 可用；Hybrid 第三轮明确报告 digest_snapshot_parse 的 duplicate bucket。当前修复在摘要和直方图查询中统一过滤 NULL DIGEST，并由 A04 单测检查；本次实测早于该修改，必须在合并后的完整 A04 中验证真实 Performance Schema 行为。Hybrid following digest 直方图计数仍比语句计数多 2–7，故 P95 留空。两个独立 CPU profile 可解析，Linux syscall 为最大 flat symbol；不能据此归因尾延迟。完整证据见 [`PERFORMANCE.md`](PERFORMANCE.md) 与 [`feed-a04-interpolate-rerun-2026-11-03.json`](benchmarks/feed-a04-interpolate-rerun-2026-11-03.json)。
- 合并后的 A04 [run #41619221597](https://github.com/liuzhouquan/GoStream/actions/runs/41619221597) 在 interpolation=true 下完成 49,186 个请求、120 次发布与所有一致性检查；六个 digest snapshot 全部解析，duplicate-bucket 错误未复现。following histogram 仍有 1–3 条计数差，所以仅 SQL-only round 2 有 per-digest P95 桶，其余留空。SQL-only median following P95 为44.804 ms，Hybrid为63.722 ms（慢42.22%），+20% gate仍失败。候选阶段 P95 桶仍高于 Hybrid，profile 未显示单一计算热点。结果支持诊断修复有效，但没有解决 Hybrid 尾延迟。见 [`PERFORMANCE.md`](PERFORMANCE.md) 与 [`feed-a04-null-digest-fix-verification-2026-11-03.json`](benchmarks/feed-a04-null-digest-fix-verification-2026-11-03.json)。
- 后续 A04 全尺寸轮次按三组独立同规模 fixture 成对执行 SQL-only/Hybrid，每组随机决定先后次序，并在 summary.json 记录 seed 和实际顺序。它缓解固定时间顺序影响，不构成同一数据库状态上的 paired run；轮次顺序测试必须确认每组两种模式各出现一次，三组起始模式最多相差一组。
- Seeded schedule 首个全尺寸验收 [run #41625136743](https://github.com/liuzhouquan/GoStream/actions/runs/41625136743)：seed 41625136743，顺序 SQL/Hybrid、SQL/Hybrid、Hybrid/SQL；46,892 个混合请求、120 次发布、零失败且一致性通过。SQL-only median following P95 49.174 ms，Hybrid 64.791 ms（慢31.76%），+20% gate 未通过。SQL-only r1 与 Hybrid r3 再次出现 `digest_snapshot_parse` duplicate bucket，故仅4/6 digest snapshots 可用。后续 parser 分支增加 `SCHEMA_NAME` + `DIGEST` 完整分组和重复键上下文；全部42个 A04 helper 测试和 Go 1.27.2 `go test ./...` 通过，下一轮 full-size 实测待完成。结果见 [`PERFORMANCE.md`](PERFORMANCE.md) 与 [`feed-a04-seeded-order-verification-2026-11-03.json`](benchmarks/feed-a04-seeded-order-verification-2026-11-03.json)。
- 后续 A04 [run #41639479241](https://github.com/liuzhouquan/GoStream/actions/runs/41639479241) 完成 62,054 个混合请求、120 次发布及全部一致性检查；通过按 schema/digest/bucket/bounds 分组并取 `MAX(COUNT_BUCKET)` 后，六轮 digest snapshots 均可解析。部分 digest 的语句计数和 histogram 计数仍不相等，因此不能计算它们的 per-SQL P95。SQL-only median following P95 为41.125 ms，Hybrid为63.362 ms（慢54.07%），+20%性能门槛未通过；workflow 的非零退出由该性能门槛触发。独立证据见 [`PERFORMANCE.md`](PERFORMANCE.md) 与 [`feed-a04-histogram-aggregation-verification-2026-11-03.json`](benchmarks/feed-a04-histogram-aggregation-verification-2026-11-03.json)。

## 2026-11-04 A04 candidate row timing boundary

For each successful following Feed measurement, the A04 report retains `candidate_sql`, `candidate_query_start`, `candidate_row_next`, and `candidate_row_scan`. The new stages split the query call, `sql.Rows.Next()` iteration, and the remainder of GORM's scan loop. They are API-side elapsed measurements: `candidate_query_start` includes pool checkout and the initial response; `candidate_row_next` includes driver decoding and result delivery; `candidate_row_scan` is not pure CPU. They are diagnostic only. The client-side following P95 remains the acceptance gate, and a post-instrumentation full-size MySQL run must compare total `candidate_sql` and client P95 with the previous baseline before any performance conclusion.

## Feed Redis cache outage and recovery

`TestFeedRedisCacheFallbackAndRecovery` uses miniredis to stop and restart the cache server on the same TCP address. Timeline, following, hot, and recommendation requests must continue through the database while Redis is unavailable and while the five-second cache circuit is open. After cooldown, the existing Redis client must reconnect, repopulate the feed entry, and serve a subsequent cache hit. This is an in-process recovery test; the separate API-process case is described below, while production Redis load remains open.

### 跨实例共享 Feed 缓存版本

`TestFollowingFeedCacheInvalidationAcrossInstances` 使用两个独立 `Server`/router 和 Redis client，共享 SQL 与 miniredis。先由实例 A 缓存关注页，再由实例 B 关注新作者；A 的下一次读取必须包含新作者的视频。之后 B 编辑视频，A 必须读取更新后的标题。真实组件版本为 `TestFollowingFeedCacheCrossInstanceRealRedis`；本地 Redis 8.2.9、DB 15 实测通过，新增的 `redis-integration` CI 步骤会执行相同测试，远程 CI 结果待运行确认：

```sh
GOSTREAM_TEST_REDIS_ADDR=127.0.0.1:6379 /usr/local/go/bin/go test ./internal/platform -run '^TestFollowingFeedCacheCrossInstanceRealRedis$' -count=1 -v
```

该变量必须指向本次测试专用的独立 Redis；测试只在 DB 15 清理 `gostream:feed:*` 键。此用例证明 Redis 共享版本键在两个 Server 对象间传播，不是独立进程的故障恢复演练；后者由下方真实双进程测试覆盖。暖缓存突发验收见下节。


### 独立 API 进程 Redis 故障恢复

`TestFeedCacheProcessRedisRecovery` 运行两个独立生产 API 可执行文件，各自连接同一隔离 MySQL schema 和 Redis 容器。实例 A 缓存 Following 页面后，实例 B 关注新作者，A 必须观察到共享版本失效；随后停 Redis，由 B 取关，要求 SQL 状态提交且 A 在 Redis 故障时回源读到新关系；Redis 恢复后，A 必须重新缓存并实际命中更新页面。运行入口会创建/删除随机 MySQL schema、用户、API 进程和 Redis 容器：

```sh
python3 tests/integration/im_smoke.py --go-bin /usr/local/go/bin/go \
  --mysql-container <isolated-mysql-container> --mysql-addr 127.0.0.1:3306 \
  --suite feed-cache
```

本地 Go 1.27.2、MySQL 8.4.11、Redis 8.2.9 实测通过：Go test 13.12 秒，包含编译、隔离资源创建和清理的 runner 29.33 秒；API 子进程、schema、MySQL 用户和 Redis 容器清理后均为 0。该场景覆盖单机上的两个真实 API 进程，不是多主机容量测试；CI 已配置该步骤，远程结果待运行确认。

### Following 与公开推荐暖缓存突发负载

`TestFeedCacheWarmBurst` 用两个生产 API 进程共享隔离 MySQL/Redis，分别预热关注首屏和未登录公开推荐首屏，再以 12 个并发 worker、每场景 240 个请求测量缓存突发流量。每个响应须为有效 JSON 且只包含夹具视频；每个场景 Redis 命中率须至少 95%，同时记录命中/未命中、P50/P95、吞吐和 `candidate_sql` 计数变化。该场景是短突发功能与观测门槛，不是多主机容量或长稳压结论；机器相关延迟仅作环境基线，不设跨机器延迟门槛。

```sh
python3 tests/integration/im_smoke.py --go-bin /usr/local/go/bin/go \
  --mysql-container <isolated-mysql-container> --mysql-addr 127.0.0.1:3306 \
  --suite feed-cache-load
```

关注首屏按 viewer 维度缓存；公开推荐首屏使用匿名缓存键。登录后的个性化推荐和游标分页不满足当前缓存资格，因此本项不把它们列为 Redis 暖缓存场景。CI 使用真实 MySQL/Redis 并启用 `-race`，远程运行结果待确认。Go 1.27.2 本地 race-enabled 实测连续两次通过：两个场景各 240 请求、命中率 100%、无错误、burst 中 `candidate_sql` 增量为 0；runner 耗时 18.809/19.180 秒且隔离资源清理为零。历史 P50/P95 仅计到响应头，原响应检查也未要求24条齐全；数值保留并标注在 [`feed-cache-warm-burst-2026-11-05.json`](benchmarks/feed-cache-warm-burst-2026-11-05.json)。审查后测试要求24条完整、零候选SQL，并计入完整正文传输；新实测见 [本轮记录](PENDING_BRANCH_REVIEW.md)。

### 持续发布与 fanout 排空

`TestFeedFanoutSustainedWriteDrain` 以两个配置了 `FEED_FANOUT_MAX_FOLLOWERS=320` 的生产 API 进程共享同一 MySQL/Redis；夹具创建 320 个关注者和120个媒体资产，随后在30波中每波并发发布4条视频，波次间隔1秒，最后停止写入并等待队列归零。必须在60秒内完成，并核对每条视频有且仅有320个 inbox follower、job 完成、`feed_pull=false`、无处理错误，且 API Following 页能够分页读取发布视频。

```sh
python3 tests/integration/im_smoke.py --go-bin /usr/local/go/bin/go \
  --mysql-container <isolated-mysql-container> --mysql-addr 127.0.0.1:3306 \
  --suite fanout-drain
```

本地 Go 1.27.2 race-enabled 实测：30波发布共120/120个 job 完成、38,400 条 inbox 行、采样峰值待处理10个 job；29.808秒发布窗口结束后1.264秒排空，runner 总耗时48.500秒。测试还遍历了3个 Following 游标页，120条夹具视频各返回一次，无重复或遗漏；临时 API 进程、schema、MySQL 用户和 Redis 容器均清零。先前的64视频、7.274秒结果保留为短突发基线。当前证据覆盖单机上的约30秒写入窗口和320关注者夹具；更长运行、不同关注规模与多主机运行待验。CI 步骤已配置，远程结果待运行确认。完整环境和数值见 [`feed-fanout-sustained-drain-30s-2026-11-05.json`](benchmarks/feed-fanout-sustained-drain-30s-2026-11-05.json) 及 [短突发基线](benchmarks/feed-fanout-sustained-drain-2026-11-05.json)。

## 2026-11-04 A04 inbox-first acceptance result

The full-size mixed read/write run [#41803621425](https://github.com/liuzhouquan/GoStream/actions/runs/41803621425) passed the unchanged +20% client Following P95 gate: SQL-only median P95 was 46.051 ms and Hybrid was 26.250 ms (43.00% faster). It completed 48,300 requests and 120 publishes with zero failures, all consistency checks passing and zero database-pool waits. The post-acceptance production `EXPLAIN ANALYZE` fixture confirmed the Hybrid push branch reads the viewer's ordered inbox index and avoids the previous 804 point lookups. This closes the full-size cache-free mixed-load gate for this fixture; warm-Redis performance, cross-instance Feed failure recovery, and multi-host load remain separate criteria. See [`PERFORMANCE.md`](PERFORMANCE.md) and the [machine-readable result](benchmarks/feed-a04-inbox-first-2026-11-04.json).

## 2026-11-11 CLOSE-01 / CLOSE-02 / CLOSE-03 闭环同步

以上历史章节保留原貌；至 `2026-11-11`（分支 `feat/close-03-final-delivery`），前期阶段性未闭环门槛已完成最终源码实测对齐，详细源码入口、命令与门禁结论以 [发布验收核查报告](RELEASE_ACCEPTANCE.md) 及 [G12 报告](tasks/G12.md) 为准：

- **Gate A04（CLOSE-01 / G07 已闭环）**：在包含推荐修复的最终源码 `8bef0c5` 上，24 轮正式规模 Compose 连续矩阵（[`feed-warm-cache-continuous-2026-11-10.json`](benchmarks/feed-warm-cache-continuous-2026-11-10.json)，2,002 用户 / 20,001 发布视频，856,152 请求 0 失败，P95 max `103.15 ms`、P99 max `131.54 ms`）与隔离 MySQL 8.4 无缓存 70/20/10 混合读写对照（[`feed-a04-mixed-2026-11-11.json`](benchmarks/feed-a04-mixed-2026-11-11.json)，196,612 请求 0 失败，Hybrid Following P95 改善 **34.37%** $\ge 20\%$，冷 SQL `feed_recommend` P95 `69.105–87.579 ms`）共同闭环，配合未改动路径持续 fanout 排空报告（[`feed-fanout-sustained-drain-30s-2026-11-05.json`](benchmarks/feed-fanout-sustained-drain-30s-2026-11-05.json)）。
- **Gate A07 / A03 / A08（阶段收口完成）**：群聊扇出与 19,000 次投递、20 客户端连续 IM 负载（单聊+群聊）、十视频 FFmpeg 矩阵及 MySQL 8.4 20,000 规模生产查询计划分别归档于 [`group-im-load-continuous-2026-11-09.json`](benchmarks/group-im-load-continuous-2026-11-09.json)、[`media-matrix-smoke-compose-2026-11-08.json`](benchmarks/media-matrix-smoke-compose-2026-11-08.json) 与 [`mysql-production-queries-2026-11-09.json`](benchmarks/mysql-production-queries-2026-11-09.json)。
- **Gate A10（CLOSE-02 / G10 已闭环）**：真实 Compose 正式规模（2,005 用户 / 20,010 发布视频）在可验证停写维护窗口下实测 **RPO=0s、RTO=32.51s (/ready) / 35.61s (全业务+续传)**，归档于 [`close-02-recovery-report-2026-11-10.json`](benchmarks/close-02-recovery-report-2026-11-10.json)；`CLOSE-03` 已补修 [`verify_quiescence`](../tests/integration/migration_backup_recovery_smoke.py#L278) 异常吞没问题并完成全部 8 项 `.github/workflows/ci.yml` 作业的本地等价验证（宿主机无系统 `/usr/bin/ffmpeg` 时借宿主机 FFmpeg + 仓库 `tools/ffprobe_shim.py` 执行，待推送远端 PR 触发 GitHub Actions 并合入 `main`）。

## 2026-11-11 PR64 恢复验收补充（既定门槛的实际实现）

正式 A10 不以新增夹具子集或总行数签收：停写后捕获全部表/列/行的确定性指纹，在目标 API 启动前精确核对，并对冻结备份重放迁移后再次比较。全部 SQL 媒体引用与完整源/目标对象清单、摘要必须一致；公共 import-local 非零且与100个有意共享的种子对象吻合。真实复验见 [JSON](benchmarks/pr64-recovery-verified-2026-11-11.json) 与 [审查](PR64_REVIEW_2026-11-11.md)。

失败窗口入口：`python3 tests/integration/recovery_fault_windows.py --go-bin /usr/local/go/bin/go --report data/diagnostics/recovery-faults.json`。先用正式恢复入口或正常 Compose 构建当前生产 API 镜像；该脚本复用镜像，但每个窗口各自创建独占 Compose 项目。它运行实际 fixture 和恢复流程，在转储/SQL导入/对象恢复/API启动/迁移重放的调用接缝注入失败，另实际发送SIGTERM；每项检查无残留容器/卷/网络/host API、无陈旧PASS文件。它不以整体mock run_all代替阶段执行，也不声称模拟所有网络中断精确时序。
