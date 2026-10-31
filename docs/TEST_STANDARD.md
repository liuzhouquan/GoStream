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

当前已有 opt-in MySQL 测试位于 `internal/platform/mysql_integration_test.go`，并依赖 `GOSTREAM_TEST_MYSQL_DSN`。CI 的 MySQL 任务先在全新服务库执行固定 SQL 的 v001–v004 安装，再用第二个隔离库检查已有数据采用、缺失索引恢复、从 v001 升级及重复运行，并比较两个库的列、索引和外键元数据。`tests/integration/mysql_recovery.sh` 另建两个临时库，执行真实 MySQL 逻辑备份、异库恢复、v002 回滚和重新升级，随后执行全部 `TestMySQL*` 平台测试。SQLite 迁移测试对照当前 GORM 模型、模拟建索引失败后重试、验证 v004 已完成任务的回填，并拒绝缺列旧库。现有 `tests/integration/media_smoke.py`、`media_failure_smoke.py`、`minio_smoke.py`、`minio_outage_smoke.py`、`minio_processing_outage_smoke.py` 和 `tests/load` 提供部分测试能力。`TestMinIOMediaAcrossInstances` 使用隔离桶验证双实例文件/分片、Range、完成重试和垃圾回收；`TestMinIOMediaTransfer` 验证最终媒体与有效续传分片导入、清单备份、恢复、重复运行和损坏/冲突拒绝。`TestMinIOMediaValidationAndRecovery` 在真实 MinIO 上用两套 API 路由覆盖匿名/越权、会话超限、分片长度与索引、冲突哈希、同大小损坏对象的重试修复、缺失对象恢复、重复完成及错误整文件哈希；配置 `GOSTREAM_TEST_MINIO_MYSQL_DSN` 时使用已迁移的隔离 MySQL，CI 在联合恢复演练后执行此组合。CI 的真实 MinIO 任务还运行 SQL 与对象联合恢复演练，并在新数据库和新桶完成切换前的上传会话。RabbitMQ 已有独立真实组件脚本，完整混合负载、存储完全中断及更多视频编解码格式与权限负例仍待补齐。

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

IM 双进程故障验证运行 `python3 tests/integration/im_smoke.py --go-bin /usr/local/go/bin/go --mysql-container gostream-mysql-1 --mysql-addr 127.0.0.1:3307`。脚本建立随机 MySQL 库/用户与临时 Redis 8.2.9，以 `-race` 编译生产 API，并以 `-race` 运行 `tests/integration/im`。两个真实 API 进程各持有独立 SQL/Redis 连接，检查同键并发发送、冲突拒绝、单聊/群聊远端提示和发送者其他设备回显、重复 Redis 提示、越权和混合无效 ACK 批次、Redis 停机后的 SQL 补读/重订阅、双方真实 SIGKILL 与重连/重试、105 条单聊和 105 条群消息的 100 条重放上限、完整历史补页、逐设备 ACK，以及群成员移除后的访问拒绝。SIGKILL 检查操作系统退出信号，正常关闭检查退出码和子进程竞态报告。需要 Linux、C 编译器、Docker 和 MySQL 容器内的 root 密码环境；独立 `im-integration` CI 任务执行相同入口，再以 `--check-timeout-cleanup` 故意在 API 启动后触发 Go 十秒超时，要求外层脚本成功回收其进程组、临时目录、Redis 和 SQL 资源；内层超时是预期失败，外层清理失败必须让 CI 失败。普通 Go 测试在没有四个 `GOSTREAM_TEST_IM_*` 环境变量时跳过该矩阵；优先使用自动隔离资源的脚本。此项不代替两浏览器验收、群前端完整补页、稀疏回执查询成本或 20 客户端/1,000 消息压测，详见 [`IM.md`](IM.md)。

群提交顺序回归为 `go test -race ./internal/platform -run '^TestMySQLGroupCommitOrder$' -count=3 -v`，需要已迁移的隔离 `GOSTREAM_TEST_MYSQL_DSN`。两套独立 SQL 连接/路由在真实 MySQL 插入后暂停事务，要求同群后续提交等待、回滚释放锁、不同群能继续提交、ACK 低 ID 后高 ID 仍未读、重试保持单行。旧代码已复现“高 ID 已 ACK、低 ID 迟提交后未读为零”。此测试使用进程内故障注入，不等同于真实进程强杀；后者继续由 IM 矩阵验证。前端 P1-09 的退出标准：105 条以上积压逐页完整显示；较高实时 ID 先到或切换群/重连时先补历史再推进累计 ACK，DOM 与 SQL 游标都要断言，不得只测 API 分页。

## 功能验收矩阵

| 编号 | 最终必须验证的结果 | 最低证据 | 当前判断 |
| --- | --- | --- | --- |
| A01 启动与持久化 | 空库一键启动后 `/health`、`/ready`、`/metrics` 可访问；重启 API 后账号、视频、互动和消息仍在；依赖故障时 `/ready` 的语义与文档一致 | Compose、真实 MySQL/Redis/RabbitMQ/MinIO、重启记录 | 部分通过：MinIO 覆盖配置可启动，RabbitMQ 独立停机/重启恢复及真实 API worker 生命周期已验；最终全组件 Compose 持久化恢复仍待验 |
| A02 身份与权限 | 注册、登录、JWT 过期/伪造、资料和头像修改、关注与取消；未登录者和其他用户不能修改私有资源；重复关注结果一致 | API 单元/集成测试 + 两账号浏览器操作 | 部分通过：主路径已有，最终部署权限矩阵待复验 |
| A03 视频与对象 | 文件和分片上传能在断点后续传；同一会话可跨两 API 实例完成；错块、错哈希、越权、超限均拒绝；FFmpeg 成功后视频/封面可从 MinIO 读取，失败后不进入公共 Feed；重试、删除、孤儿清理无误删 | 真实 MySQL/MinIO/FFmpeg + 视频探测 + 故障测试 | 部分通过：真实 MinIO 双实例续传、FFmpeg、Range、对象清理及停机后上传恢复已验；离线旧媒体导入及隔离 SQL/对象恢复已验；分片与资产负例已在真实 MySQL/MinIO 上验证；输出 PUT 成功响应丢失及 JPEG/PNG/WebP 截断文件已验；存储完全中断、更多视频格式/权限场景与较大规模恢复成本待验 |
| A04 Feed | 时间线、关注、热榜、推荐均正确分页；相同时间戳与新插入记录不造成稳定数据集中的漏项/重复；普通作者推送、高粉作者拉取可配置且结果一致；Redis 中断可回退数据库 | 单元测试 + 真实 MySQL/Redis 双实例 + 压测 | 部分通过：混合推拉、批量幂等、迟到关注及真实 MySQL 同时间戳翻页已局部验证；v004 完整数据集交替读压测只改善约 11% 与 4% 的 P95，未达 20% 门槛；真实 MySQL 双连接接管 130 粉任务且重放无重复，小数据读写混合短跑 1,907 请求无错误；实际进程故障、完整数据集 20 并发/60 秒三轮及持续积压仍未验收 |
| A05 互动与计数 | 并发重复点赞/收藏、取消及关注只有一次状态转换；计数与明细一致且非负；评论回复层级、权限和通知正确；播放计数策略明确 | 真实 MySQL 并发集成测试，结束后核对表和计数 | 部分通过：真实 MySQL 并发删除曾出现 1213 死锁并返回 500；整笔事务有限重试后，双连接 8×80 次点赞/收藏切换与隔离库混合短跑通过且计数匹配；多用户/多视频、播放量和完整通知矩阵待验 |
| A06 异步事件 | 业务事务失败不留待发事件；提交成功后 broker 停机也不丢事件；发布确认后重复投递不产生重复通知；消费者重启、失败重试和死信可恢复 | 真实 MySQL/RabbitMQ 故障注入 + outbox/消费者状态核对 | 部分通过：确认发布、不可路由退回、确认后 SQL 状态丢失、提交后 ACK 丢失、双消费者重复投递、SQL 重试/耗尽死信/回放、broker 重启及真实 API worker 已在隔离 MySQL/RabbitMQ 验证；实际强杀进程窗口、DLQ 目标中断及完整最终配置仍待验 |
| A07 IM | 两实例间单聊/群聊、成员权限、同一 `client_id` 重试、每设备 ACK、离线重放、超过 100 条的历史补页都正确；Redis 中断后重连能补齐；相同会话历史按服务端 ID 排序，重复推送可去重 | 两浏览器配置 + 两 API 实例 + 真实 MySQL/Redis 故障场景 | 部分通过：真实双 API/MySQL/Redis、双方 SIGKILL、Redis 停机/重订阅、重复提示、105 条单聊/群聊补页、逐设备 ACK 和群权限撤回已验证；同群提交行锁另有 MySQL 故障回归；前端补页/安全 ACK（含 350 条缺口/失败/过时响应）与真实 MySQL 成员撤回另有回归；最终两浏览器、默认 presence 完整过期已接入真实进程 CI；v005 活跃回执压缩及强杀恢复已验；1k/10k 稀疏查询已测量并优化；规定压测与最终部署仍待验 |
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
- RabbitMQ 新脚本的 SIGKILL 是一般重启检查，五条静态积压是小批量排空；精确窗口和持续积压仍待验。
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
