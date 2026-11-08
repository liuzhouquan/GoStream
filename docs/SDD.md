# GoStream 求职展示版 SDD 与开发任务清单

更新时间：2026-11-07。目标版本：`v1.0-demo`。本文件是后续 coding agent 的执行入口；产品边界见 [DELIVERY_SCOPE](DELIVERY_SCOPE.md)，验收门槛见 [TEST_STANDARD](TEST_STANDARD.md)，历史证据见 [PROGRESS](../PROGRESS.md)。SDD 在这里指以明确规格、任务和验证证据驱动开发。


> 2026-11-07 审查结论：项目尚未完成最终验收。G02–G11 已新增候选测试/脚本，不能据此勾选交付条件；G12 的“全部通过”签收已撤回。当前事实与剩余项见 [发布核查](RELEASE_ACCEPTANCE.md) 和 [本轮审查](BRANCH_REVIEW_2026-11-07.md)。G01 已由 PR #59 合并，后续从 G02 的真实环境验证继续。

## 1. 最终交付物与停止条件

交付一个可以在本机启动、由两个独立浏览器账号实际使用的 Go 短视频社交平台。正式展示配置是两个 API 实例共享 MySQL、Redis、RabbitMQ、MinIO，启用 FFmpeg。Go 模块化单体、静态 Web 客户端、规则推荐和现有搜索方案都是有效交付选择。

用户必须能注册登录、维护资料、关注、浏览关注/推荐/热榜、上传和续传、等待转码发布、互动评论、搜索、接收通知、单聊和群聊。重启、重复请求、组件短暂离线后，持久状态必须符合已声明的恢复保证。

发布必须同时满足：

- [ ] 本文件所有“发布必做”任务有对应最终提交/PR、命令、真实结果和证据路径。
- [ ] A01–A12 功能门槛及 TEST_STANDARD 的既定性能门槛逐项通过；一个测试同时证明多个条件可以复用证据。
- [ ] 最后一次合并后的 CI 全部通过，最终 Compose 与两浏览器验收来自同一发布候选代码树。
- [ ] 没有未处理的已确认 P0/P1；未确认风险明确记录复现条件、影响和后续验证，不假装为已修复。
- [ ] README 可从新环境复现；API/迁移/故障恢复文档与代码相符；本地学习笔记保留且未被 Git 跟踪。
- [ ] 发布报告列出已支持场景、数据规模、真实性能、故障边界和演示步骤。

完成这些条件即可发布。多主机容量、长期 soak、原生移动端、机器学习推荐、强制拆成微服务、换前端框架、搜索引擎迁移都不是继续延长本阶段的理由。三节点 RabbitMQ 已有本地故障证据，但不新增集群负载或混沌工程的发布门槛。出现新正确性缺陷仍须按严重性处理。

## 2. 当前基线与距离交付的判断

本轮审查开始的远程主线为 `9bf2bff`（PR #58）。待合入的工作区包括 RabbitMQ 精确强杀窗口、持续输入/DLQ、三节点故障及多地址连接支持，以及真实 MinIO 停服后的续传恢复。是否最终合入及验证结果见 [本轮审查](BRANCH_REVIEW_2026-11-06.md)。

**主要业务模块已实现；项目处在集成与交付收尾阶段。** 当前不能按代码量给“完成百分比”，也不能把 A01–A12 全部“部分通过”解释成没有业务实现。缺口主要是最终配置的联合证据、已有规定负载、异常矩阵和文档收口。

| 工作面 | 已有能力/证据 | 发布前的明确缺口 |
| --- | --- | --- |
| 用户/社交/互动 | JWT、资料头像、关注、点赞收藏评论，存储错误与幂等回归；真实 MySQL 并发动作测试 | 最终配置的两账号权限与状态一致性、完整互动/通知对应关系 |
| 视频 | 本地/MinIO、续传、跨实例、FFmpeg、Range、清理、租约与输出响应丢失恢复 | 真实编解码输入矩阵、10 个视频终态与资源/耗时、最终配置故障复验 |
| Feed | 混合推拉、游标、Redis 故障回退/跨实例失效；完整无缓存 A04 收益门槛已通过 | 20 并发持续暖缓存三轮的既定延迟门槛；无需因写文档重跑已通过 A04 |
| 异步 | SQL outbox、确认发布、消费幂等、重试/死信/回放，真实强杀和集群局部测试 | 本轮缺陷修复、最后提交 CI、最终配置停机后逐事件核对 |
| IM | 单聊/群聊/逐设备 ACK、离线补页、真实进程/Redis 恢复、三轮单聊负载 | 群聊扇出负载、两浏览器在最终配置的重连/未读/历史 |
| 搜索/通知 | 分页、转义、私有内容过滤、索引探针 | 生产查询在规定规模的数据集上的计划、扫描成本和结果正确性 |
| 运维/契约 | v001–v005 迁移、联合恢复工具、OpenAPI 结构及代表响应校验、监控 | 最终安装/重启/备份恢复、契约遗漏、统一发布验收记录 |

以下为11月6日排期基线，已有候选代码后的剩余工作需逐项实测再估算，不能将它作为当前工期承诺。工作量估算用于排期，不能代替验收：环境稳定且不再发现重大缺陷时，下面剩余工作约 **8–13 个有效工作日**，可并行任务约 **2–3 个**；最终联合验收必须顺序执行。真实组件、浏览器和 CI 的等待会影响日历时间。完成每个任务后用实际耗时更新估算，禁止用测试数量推算百分比。

## 3. 规格与代码边界

| 模块 | 入口/核心位置 | 必须保持的不变量 |
| --- | --- | --- |
| API/配置 | `cmd/gostream/main.go`, `internal/platform/server.go` | 使用 go.mod 指定版本；JWT 由服务端校验；私有错误不泄漏凭据；health/ready 语义有文档 |
| 用户与权限 | `internal/platform/accounts.go`, `security_boundary_test.go` | 身份来自 JWT；所有权和当前成员身份在服务端检查，不能信任页面状态 |
| 视频/上传 | `uploads.go`, `uploads_object.go`, `video_processing.go` | 失败分片不登记；完成幂等；租约只由当前持有者完成；只有 published 进入公共结果；清理不误删引用对象 |
| Feed | `following_feed.go`, `feed_fanout.go` 及 Feed 缓存实现 | 混合结果无漏无重；取关后过滤旧 inbox；持久 fanout 可恢复；Redis 不是唯一事实来源 |
| 互动/outbox | `social.go`, `outbox.go`, `rabbitmq.go` | 明细、计数、通知/outbox 同事务；重复请求不增加业务效果；broker 至少一次投递由 SQL 幂等处理 |
| IM | `messages.go`, `groups.go`, `direct_unread.go`, `realtime.go` | 会话内按服务端 ID 排序；client_id 去重；每设备未读隔离；累计 ACK 不跳过未同步消息；实时提示丢失可从 SQL 补齐 |
| 数据/契约 | `internal/migrations`, `models.go`, `docs/openapi.yaml` | 已发布迁移不改写；新 Schema 用增量迁移；契约校验覆盖请求/响应和真实帧 |
| Web | `web/app.js`, `web/index.html` | 两账号独立会话；刷新可恢复；响应错误可见；页面计数、聊天去重/补页按服务端状态 |

上表相对路径默认以 `internal/platform` 为根（明确给出的 `cmd/`、`docs/`、`web/` 除外）。文件名有变动时先定位真实调用链，不能按此表虚构不存在的模块。

## 4. 多 agent 协作协议

1. 每次开始先 `git fetch origin --prune`，读 AGENTS、本文件、TEST_STANDARD 与近期 PROGRESS；核对实际主线和工作区，保留别人的未提交内容。
2. 每个 agent 领取一个任务 ID；在独立分支/worktree 工作。在任务报告 `docs/tasks/<ID>.md` 记录 owner、基线 SHA、分支、状态、计划测试、实际证据、资源清理和交接点。目录/报告在领取时创建。
3. 状态只有 `候选代码已实现，待验收 → 进行中 → 待审查 → 已合并 → 已验收`；阻塞写清依赖、失败命令和待确认事实。只有证据满足退出条件才能勾选。
4. 同一时间只允许一个集成 agent 更新本文件的状态和合并顺序。任务 agent 提交自己的报告，避免多分支同时改 PROGRESS/矩阵产生覆盖。
5. 不共享运行中的测试库、Redis DB、RabbitMQ 队列、MinIO 桶或 Compose 项目。容器/端口/随机 schema 归启动者所有；不要对演示项目 `down -v`。
6. 创建资源前登记清理意图；SIGTERM/SIGINT 必须走 finally，子进程启动交接受保护；记录清理后进程、schema、user、容器/网络数量。SIGKILL 的清理局限要明说。
7. 准确区分“代码存在”“本地验证”“CI 通过”“最终部署验收”。Mock helper、SQLite、同进程 router 和真实外部组件的证据不可互换。
8. 缺陷先给可失败的回归；不删断言、不降低 bcrypt/权限/一致性/性能门槛。只为有证据的瓶颈改设计。
9. 长命令用 `tools/task_timing.py`，保留 session_id 并跟踪到 exit_code。重复全套测试需要新修改/失败/未解决疑点；没有新依据时不重跑完整 A04。
10. PR 写明任务 ID、最终行为、真实测试和局限；检查结果对应最后提交。集成 agent 审查后合并，随后把 SHA/CI/报告写回任务清单。

推荐并行：先完成 G01；G03/G04/G05/G06/G07 可按文件与环境隔离并行，其中最终栈部分等待 G02；G08/G09/G10 按依赖接续。禁止三个 agent 同时占用同一个最终验收栈。

## 5. 详细 TODO（全部为发布必做）

当前 owner 均为“候选代码已实现，待验收”；G01 由本轮集成 agent 处理。下列估算含实现/局部验证，不含等待别的任务。勾选表示该工作包退出条件全部满足。

### G01 审查与主线收敛（PR #59 已合并，本轮增量另见审查记录）

- [x] 核对全部本地/远程非祖先引用；用 PR head SHA 和 squash 记录排除已合并旧分支，列明已替代且不合入的方案。
- [x] 修复本轮 RabbitMQ pre-ACK 代理被 URLS 绕过、外部取消遗漏清理和标准库EINTR重试吞掉信号的问题；保留可复现回归、真实取消和独立审查结果。
- [x] 运行单节点真实 broker 完整 smoke、相关 Python/Go 测试和最后提交 CI；三节点场景由 CI 执行，不因文档重复运行本地集群。
- [x] 合并有效新增改动，记录 PR/合并 SHA，确认主线干净且无真正遗漏的新分支。

范围：`rabbitmq.go`、两个 CLI、`rabbitmq_smoke.py`、ACK proxy、`minio_outage_smoke.py`、CI。证据：`BRANCH_REVIEW_2026-11-06.md`。旧分支留档不等于待重开发。

### G02 最终联合验收栈（已完成，交付报告：[docs/tasks/G02.md](tasks/G02.md)）

- [x] 在现有 object-test Compose 上增加可复用 launcher/配置，两个 API 对外地址明确，API 构建来自当前提交。
- [x] 自动创建独立配置/数据，等待 health/ready，记录版本、CPU/内存、commit、部署参数及 API 实例身份。
- [x] 支持浏览器测试使用现有 base URLs，禁止最终验收脚本另启 SQLite API 后冒充 Compose 验收。
- [x] 正常结束、断言失败和 SIGTERM 都清理本轮资源；失败保存脱敏诊断，报告不得沿用旧成功文件。

复用：`tests/integration/object-test.compose.yml`、`im_smoke.py` 生命周期模式。退出条件：同一 launcher 可从空测试卷启动、完成探测、清理；新环境与外部取消各有实测。A01/A09/A10 的后续脚本统一复用它。

### G03 两浏览器全业务与重启（已完成，交付报告：[docs/tasks/G03.md](tasks/G03.md)）

- [x] 扩展 `web_full_flow.py` 的外部栈入口；保留原 SQLite/FFmpeg 小测试，不混淆两者证据。
- [x] 两个独立浏览器完成注册登录、资料/头像、关注、续传发布、推荐/关注/热榜、点赞收藏评论/回复、搜索、通知及有界已读。
- [x] 完成跨实例单聊、群创建/邀请/移除、超过100条离线补页、刷新/重连后未读与 ACK；核对 DOM 与 SQL 身份集合。
- [x] 重启一个 API 后登录态、视频/互动/消息持久存在；390px 宽度的主要操作可用；匿名与另一账号越权失败。

退出条件：真实 MySQL/Redis/RabbitMQ/MinIO/FFmpeg 两 API，两浏览器全流程成功；报告包含使用的端点、实例映射、截图/关键断言、SQL核对和清理。对应 A01/A02/A07/A09/A11；不能只截图首页。

### G04 媒体格式、十视频与恢复矩阵（已完成，交付报告：[docs/tasks/G04.md](tasks/G04.md)）

- [x] 生成可复现真实 MP4/MOV/WebM fixture，至少包含音轨/无音轨、不同输入编解码；用 ffprobe 记录真实格式，魔数样本仅测试类型判断。
- [x] 10 个真实短视频完成上传→处理→封面→发布或有解释的 failed；采集排队/处理时间、终态、CPU、输出/封面字节及探测元数据。
- [x] 复用已验证的错块/哈希/大小/所有权测试；仅补未覆盖的格式/权限交叉项，失败视频在 Feed/搜索不可见，作者可恢复。
- [x] 复用 `minio_smoke.py`、`minio_outage_smoke.py`、`minio_processing_outage_smoke.py`，确认最终栈的上传/转码重试及唯一资产，不把已有测试重新实现一套。

退出条件：A03 与 TEST_STANDARD“IM与媒体”中的十视频部分有逐文件结果；错误、重试、对象引用/清理可核对。扩展 `MEDIA_OPERATIONS.md`/任务报告，不编造转码吞吐。

### G05 互动、通知及权限收口（已完成，交付报告：[docs/tasks/G05.md](tasks/G05.md)）

- [x] 先读 `TestMySQLMultiUserConcurrentInteractionsAndCounters`、`TestMySQLSecurityAndOwnershipBoundaries`、通知分页/评论测试，列出已有证据，避免重复造矩阵。
- [x] 补并核对多用户/多视频重复点赞、收藏、取消、关注、评论/回复的明细、计数、通知/outbox 对应关系，禁止负数和重复业务效果。
- [x] 明确播放计数与请求预算；覆盖重复播放/限流边界、评论层级、陌生账号/匿名访问、取消权限后的缓存与搜索。
- [x] 最终栈运行代表性权限负例，并将 A02/A05/A08/A11 的缺项与证据一一对应。

现有入口（需要隔离已迁移 MySQL）：`go test -race ./internal/platform -run '^TestMySQL(MultiUserConcurrentInteractionsAndCounters|SecurityAndOwnershipBoundaries|NotificationPaginationIntegration|CommentThreadsIntegration)$' -count=1 -v`。退出条件是完整状态核对，不能仅检查 HTTP 200；发现生产缺陷拆小修复 PR。

### G06 搜索与通知的真实生产查询（已完成，交付报告：[docs/tasks/G06.md](tasks/G06.md)）

- [x] 复用既定 2,000 用户/20,000 视频等规模，构造命中与未命中、同时间戳、通配符/Unicode、深页数据。
- [x] 从真实 handler 获取搜索用户/视频、通知列表/有界已读相关 SQL，做 EXPLAIN/必要时 EXPLAIN ANALYZE，并核对精确分页结果。
- [x] 记录前置通配符 LIKE 的扫描/排序成本、延迟及适用规模；现有500行探针和 FORCE INDEX 不能代替生产查询证据。
- [x] 仅在发现瓶颈后修改查询/索引；增量迁移与前后对照另行提交，不凭猜测删除“冗余”索引。

复用：`docs/QUERY_PLANS.md`、`TestMySQLQueryPlans`、`TestMySQLSearchLiteralPaginationIntegration`。退出条件：A08 真实规模结果和可复现计划，明确搜索方案适用范围，无需默认引入 Elasticsearch。

### G07 Feed 持续暖缓存门槛（已完成，交付报告：[docs/tasks/G07.md](tasks/G07.md)）

- [x] 复用完整基准数据，匿名/登录身份及缓存资格必须与生产实现一致；普通登录个性化推荐若不缓存应明确走 SQL，不能伪造命中。
- [x] 按 TEST_STANDARD 执行 10/20 并发、30秒预热、60秒实测、3轮；关注首页/深页、登录推荐、热榜覆盖冷/暖路径。
- [x] 20 并发预热场景三轮 P95≤200ms、P99≤500ms，无意外5xx与页面重复遗漏；记录完整正文时延、命中、SQL、CPU/RSS。
- [x] 重用既有120视频/320关注者 fanout 和已通过无缓存A04收益证据；只在相关查询发生改变时重跑完整收益对照。

复用：`tests/integration/im/feed_cache_load_test.go`、`feed_a04_mixed.py`、`tests/load`。新增驱动/入口在任务报告写出后再引用，当前12并发240请求短突发不能直接勾选本项。退出条件：A04 规定持续缓存负载证据完整，未达门槛先诊断，不降规模/阈值。

### G08 群聊扇出与设备恢复（已完成，交付报告：[docs/tasks/G08.md](tasks/G08.md)）

- [x] 复用 `tests/integration/im` 的20客户端/1000消息驱动，在两实例上补群聊扇出；先在任务规格列出每个客户端期望接收的ID集合。
- [x] 覆盖重复 client_id、慢/离线成员、移除/重邀、重连补页、逐设备未读/ACK，区分发送ACK和接收ACK。
- [x] 核对 SQL唯一消息、每个合法接收者的完整去重集合、历史顺序、未读最终状态；实时丢提示后可以由历史恢复。
- [x] 记录群大小/分布、正文大小、正常与race口径、延迟/CPU/RSS；不新增未经约定的百万连接目标。

复用：`IM_LOAD.md`、`im_smoke.py --suite load`、群顺序/撤回回归。退出条件：A07单聊已有负载证据与本次群聊结果共同满足门槛，最终浏览器证据由G03提供。

### G09 最终栈单组件故障与持久化（已完成，交付报告：[docs/tasks/G09.md](tasks/G09.md)）

- [x] 在同一最终版本分别停止 Redis、RabbitMQ、MinIO；核对 SQL事实、HTTP错误、Feed回源、消息历史/未读及恢复后收敛。
- [x] RabbitMQ停机期间事务已提交事件保留，恢复后按具体 event_id 验证 source/delivery唯一结果；已有精确强杀窗口复用G01证据。
- [x] 检查 health为进程活性、ready为SQL可用；SQL不可用时ready失败，其他依赖停机不随意改变既定语义。
- [x] API/worker重启后租约可重领、上传/视频/通知/IM状态可恢复；记录最终残留与清理。

退出条件：A01/A03/A04/A06/A07/A10中最终配置故障条件有同版证据，不能以“停了容器后HTTP 200”替代状态验证。不再新增集群压力测试作为前置。

### G10 迁移、备份恢复与运维记录（候选代码已实现，待验收，0.5–1 日；依赖 G02/G04）

- [ ] 按当前 schema v005 复验新库安装、旧库升级、重复运行和中断后恢复；已有v002回滚演练不代表v005可直接混跑/降级。
- [ ] 复用 `mysql_recovery.sh`、`media_transfer_smoke.py`，在明确停写/快照边界下保证SQL与对象清单对应；记录用户/视频/消息/上传会话恢复后的身份集合与对象摘要。
- [ ] 在既定规模数据上记录备份/恢复耗时、SQL/对象/临时空间及恢复后读取；没有量测的 RPO/RTO 不写成承诺。
- [ ] README与运维文档明确可用备份方式、维护窗口、失败处理和已知在线备份限制；在线一致性未经证实时不能声称支持热备。

退出条件：A10版本/数据恢复证据与操作文档一致，检查关键Prometheus指标能对应操作状态。最终栈安全备份前不得破坏演示数据。

### G11 契约和文档一致性（候选代码已实现，待验收，0.5–1 日；依赖 G04/G05/G08稳定API）

- [ ] 按 `server.go` 的当前方法/路由核对 OpenAPI参数、请求约束、状态码、鉴权、分页、媒体响应和WebSocket帧。
- [ ] 补真正缺失的请求/响应变体及实际响应校验；路由数相等不代表Schema正确。已有结构/代表响应测试复用。
- [ ] 从新环境执行README；核对DATABASE当前迁移版本、配置默认值、失败/恢复操作、学习/面试文档的源码位置。
- [ ] 更新仅本地的 `docs/LOCAL_STUDY_GUIDE.md`：端到端阅读路线、事务/Feed/MQ/IM/媒体练习、故障复现、面试问答及证据限制；确认 `git check-ignore -v` 且 `git ls-files` 不包含它。

现有入口：`go test ./internal/platform -run '^TestOpenAPI' -count=1 -v`，其余契约测试用 `rg '^func Test' internal/platform/*openapi*` 核对后执行。退出条件：A12与真实代码/最终配置相符，学习材料可定位每个已实现技术点。

### G12 发布候选与最终签收（未签收，0.5 日；依赖 G01–G11）

- [ ] 整合所有任务报告，检查每个 A01–A12 和规定性能条目的证据、代码树与未验证限制；没有证据的项继续部分通过。
- [ ] 最后提交完整CI通过；运行最终联合烟测与两浏览器主流程（相关变更才重复重测试件），确认资源/数据清理。
- [ ] 形成 `docs/RELEASE_ACCEPTANCE.md`：版本、环境、任务/验收ID、命令、结果、报告、已知限制、5–10分钟演示脚本。
- [ ] 更新 README 状态与 PROGRESS，创建用户可查看的release/标签；最终复核主线无待处理有效分支和未提交交付改动。

退出条件：第1节所有条件满足。后续功能新增进入下一版本，不继续占用本阶段任务清单。

## 6. 验证入口与证据格式

基础检查从仓库根目录执行，Go版本必须与go.mod一致：

```sh
export PATH=/usr/local/go/bin:$PATH
go version
python3 tools/task_timing.py run --label go-unit --report data/diagnostics/go-unit.json --timeout-seconds 180 --heartbeat-seconds 15 --quiet -- go test -json -count=1 ./...
go vet ./...
go build ./cmd/gostream ./cmd/gostream-media ./cmd/gostream-events
node --check web/app.js
python3 -m unittest discover -s tests/diagnostics -p 'test_*.py' -v
git diff --check
```

全量race由CI执行或确有必要时单次本地运行，预算见 DEVELOPMENT_TIMING。外部集成测试未配置环境时可能skip；不要把普通 `go test ./...` 算成外部组件通过。RabbitMQ、MinIO、IM和SQL恢复的准确命令分别沿用 RABBITMQ、TEST_STANDARD、IM_LOAD、MEDIA_OPERATIONS 文档；G02新launcher尚未实现前不编造可执行命令。

每个任务报告至少写：

```text
任务ID / 状态 / owner / 分支 / 基线SHA / 最终headSHA / PR与合并SHA
实现范围 / 不变量 / 依赖任务 / 未改变的验收门槛
环境（版本、CPU、内存、服务配置、数据规模、普通或race）
执行命令 / 开始结束 / exit_code / 断言与结果 / 证据路径或CI run
缺陷的失败复现与修复后结果；未验证风险及原因
资源所有权和清理后数量；剩余任务和可直接执行的下一步
```

原始日志放忽略的 `data/diagnostics/`，脱敏汇总可提交 `docs/benchmarks/` 或任务报告。性能结果保留每轮而非只给平均值；失败历史保留，修复后新增证据。任何 agent 只有在核对最终代码与测试输出后才能声明完成。
