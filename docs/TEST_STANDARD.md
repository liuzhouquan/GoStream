# GoStream 测试与验收标准

更新日期：2026-10-26。此文档规定 [`DELIVERY_SCOPE.md`](DELIVERY_SCOPE.md) 所述求职展示版如何判定完成。带有“目标”的条件尚未通过；已运行过的命令和实测数字以 [`PERFORMANCE.md`](PERFORMANCE.md) 与 [`PROGRESS.md`](../PROGRESS.md) 为准。

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
go build ./cmd/gostream
node --check web/app.js
docker compose config --quiet
```

运行环境需要在记录中注明：Git commit、Go 版本、操作系统/CPU/内存、容器镜像版本、MySQL/Redis/RabbitMQ/MinIO 配置、数据库规模、是否启用 FFmpeg，以及具体命令。CI 最终至少执行 Go 测试、vet、构建和前端语法检查；race、真实组件集成测试和浏览器验收可以在单独任务中执行，但发布前必须有对应结果。

当前已有 opt-in MySQL 测试位于 `internal/platform/mysql_integration_test.go`，并依赖 `GOSTREAM_TEST_MYSQL_DSN`。CI 的 MySQL 任务先在全新服务库执行固定 SQL 的 v001–v004 安装，再用第二个隔离库检查已有数据采用、缺失索引恢复、从 v001 升级及重复运行，并比较两个库的列、索引和外键元数据。`tests/integration/mysql_recovery.sh` 另建两个临时库，执行真实 MySQL 逻辑备份、异库恢复、v002 回滚和重新升级，随后执行全部 `TestMySQL*` 平台测试。SQLite 迁移测试对照当前 GORM 模型、模拟建索引失败后重试、验证 v004 已完成任务的回填，并拒绝缺列旧库。现有 `tests/integration/media_smoke.py`、`media_failure_smoke.py`、`minio_smoke.py`、`minio_outage_smoke.py`、`minio_processing_outage_smoke.py` 和 `tests/load` 提供部分测试能力。`TestMinIOMediaAcrossInstances` 使用隔离桶验证双实例文件/分片、Range、完成重试和垃圾回收；`TestMinIOMediaTransfer` 验证最终媒体与有效续传分片导入、清单备份、恢复、重复运行和损坏/冲突拒绝。`TestMinIOMediaValidationAndRecovery` 在真实 MinIO 上用两套 API 路由覆盖匿名/越权、会话超限、分片长度与索引、冲突哈希、同大小损坏对象的重试修复、缺失对象恢复、重复完成及错误整文件哈希；配置 `GOSTREAM_TEST_MINIO_MYSQL_DSN` 时使用已迁移的隔离 MySQL，CI 在联合恢复演练后执行此组合。CI 的真实 MinIO 任务还运行 SQL 与对象联合恢复演练，并在新数据库和新桶完成切换前的上传会话。RabbitMQ、完整混合负载、存储完全中断及更多视频编解码格式与权限负例仍待开发。

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

## 功能验收矩阵

| 编号 | 最终必须验证的结果 | 最低证据 | 当前判断 |
| --- | --- | --- | --- |
| A01 启动与持久化 | 空库一键启动后 `/health`、`/ready`、`/metrics` 可访问；重启 API 后账号、视频、互动和消息仍在；依赖故障时 `/ready` 的语义与文档一致 | Compose、真实 MySQL/Redis/RabbitMQ/MinIO、重启记录 | 部分通过：MinIO 覆盖配置可启动，RabbitMQ 与全组件持久化恢复仍待验 |
| A02 身份与权限 | 注册、登录、JWT 过期/伪造、资料和头像修改、关注与取消；未登录者和其他用户不能修改私有资源；重复关注结果一致 | API 单元/集成测试 + 两账号浏览器操作 | 部分通过：主路径已有，最终部署权限矩阵待复验 |
| A03 视频与对象 | 文件和分片上传能在断点后续传；同一会话可跨两 API 实例完成；错块、错哈希、越权、超限均拒绝；FFmpeg 成功后视频/封面可从 MinIO 读取，失败后不进入公共 Feed；重试、删除、孤儿清理无误删 | 真实 MySQL/MinIO/FFmpeg + 视频探测 + 故障测试 | 部分通过：真实 MinIO 双实例续传、FFmpeg、Range、对象清理及停机后上传恢复已验；离线旧媒体导入及隔离 SQL/对象恢复已验；分片与资产负例已在真实 MySQL/MinIO 上验证；输出 PUT 成功响应丢失及 JPEG/PNG/WebP 截断文件已验；存储完全中断、更多视频格式/权限场景与较大规模恢复成本待验 |
| A04 Feed | 时间线、关注、热榜、推荐均正确分页；相同时间戳与新插入记录不造成稳定数据集中的漏项/重复；普通作者推送、高粉作者拉取可配置且结果一致；Redis 中断可回退数据库 | 单元测试 + 真实 MySQL/Redis 双实例 + 压测 | 部分通过：混合推拉、批量幂等、迟到关注及真实 MySQL 同时间戳翻页已局部验证；新 v004 拉取索引和回填在隔离 MySQL 通过，2,000 用户/20,000 视频/10,999 关注/50,000 互动的交替压测只改善约 11% 与 4% 的 P95，未达 20% 门槛；单个 38 粉任务在 2.201 秒完成，双实例故障、混合读写、持续积压仍未验收 |
| A05 互动与计数 | 并发重复点赞/收藏、取消及关注只有一次状态转换；计数与明细一致且非负；评论回复层级、权限和通知正确；播放计数策略明确 | 真实 MySQL 并发集成测试，结束后核对表和计数 | 部分通过：事务与局部测试已有，最终并发矩阵待验 |
| A06 异步事件 | 业务事务失败不留待发事件；提交成功后 broker 停机也不丢事件；发布确认后重复投递不产生重复通知；消费者重启、失败重试和死信可恢复 | 真实 MySQL/RabbitMQ 故障注入 + outbox/消费者状态核对 | 未验证：当前只有数据库 outbox worker |
| A07 IM | 两实例间单聊/群聊、成员权限、同一 `client_id` 重试、每设备 ACK、离线重放、超过 100 条的历史补页都正确；Redis 中断后重连能补齐；相同会话历史按服务端 ID 排序，重复推送可去重 | 两浏览器配置 + 两 API 实例 + 真实 MySQL/Redis 故障场景 | 部分通过：局部多实例与浏览器测试已有，故障和长期回执清理待验 |
| A08 通知与搜索 | 关注/点赞/评论/私信通知按权限显示，分页和有界已读不吞掉新通知；搜索用户与已发布视频支持独立分页和转义字符，隐藏视频不出现 | 真实 MySQL + 浏览器分页测试 + 查询计划记录 | 部分通过：功能已验证，较大数据量查询计划待补 |
| A09 Web 全流程 | 两个独立浏览器账号完成注册、续传发布、Feed、互动、主页、搜索、通知、单聊和群聊；刷新/重启后状态一致；手机宽度下主要操作可用 | 最终 Compose 版本的浏览器记录，附账号/数据清理说明 | 部分通过：现有功能分批验证，完整最终配置待验 |
| A10 数据升级与运维 | 新库安装、从当前 schema 升级、重复运行迁移、失败恢复均有结果；CI 通过；Prometheus 指标可解释；备份与恢复操作有记录 | 版本化迁移测试 + CI 运行记录 + 恢复演练 | 部分通过：v004 本地新库安装、旧库回填和隔离 MySQL 备份/恢复重放已通过；本次提交的 CI 待运行，隔离 MySQL 与 MinIO 联合恢复已有早期验证；在线备份一致性与较大规模恢复成本待验 |
| A11 安全边界 | 上传文件名和路径不能越界；会话、私信、群、未发布视频和管理操作都有身份/所有权检查；密码不明文存储，公开响应不泄露内部错误或 JWT | 自动化负例 + 最终浏览器/API 复验 | 部分通过：现有负例覆盖一部分路径 |
| A12 文档契约 | README 能从空环境启动最终配置；API 参数、错误、鉴权、WebSocket 帧及数据库迁移说明与代码一致；机器可读 API 契约经过校验；学习和面试指南只列已实现内容 | 文档链接/示例核对 + OpenAPI 校验 + 最终代码审阅 | 部分通过：当前有人工维护的 API/架构/学习文档，机器可读契约未提供 |

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
