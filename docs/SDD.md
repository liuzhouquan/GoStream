# GoStream 求职展示版 SDD 与开发任务清单

更新时间：2026-11-11。目标版本：`v1.0.0`。本文件是 coding agent 的交付范围和接手入口；产品边界见 [DELIVERY_SCOPE](DELIVERY_SCOPE.md)，验收门槛见 [TEST_STANDARD](TEST_STANDARD.md)。

**当前依据：[PR64 独立审查与正式恢复复验](PR64_REVIEW_2026-11-11.md)。** 本版实现与既定验收收口见第 1 节，远端签收以 PR #64 最终 CI/合并树事实为准；后续待确认风险单列第 2.2 节，不继续扩充本版业务范围。2026-11-08/09/10 的阶段报告保留在 [PROGRESS](../PROGRESS.md) 和对应审查文件，不能覆盖最新结论。

## 1. 最终交付物与停止条件

交付一个可以在本机启动、由两个独立浏览器账号实际使用的 Go 短视频社交平台。正式展示配置是两个 API 实例共享 MySQL、Redis、RabbitMQ、MinIO，启用 FFmpeg。Go 模块化单体、静态 Web 客户端、规则推荐和现有搜索方案都是有效交付选择。

用户必须能注册登录、维护资料、关注、浏览关注/推荐/热榜、上传和续传、等待转码发布、互动评论、搜索、接收通知、单聊和群聊。重启、重复请求、组件短暂离线后，持久状态必须符合已声明的恢复保证。

本版已实现并经本轮核对的交付项：

- [x] 业务与发布必做实现达到既定退出条件，A01–A12 的源码/命令/证据见 RELEASE_ACCEPTANCE。
- [x] A04 规定规模持续/混合负载成立；未变化的 Feed 路径复用已提交报告，不重复扩展门槛。
- [x] A10 完整停写 SQL/对象/身份恢复、迁移重放及实际阶段故障覆盖，见 PR64 审查与新的正式 JSON；旧零对象导入报告不再签收。
- [x] 本轮已确认的媒体 CLI 错误吞没与恢复验收缺口已修正并复验；没有遗漏为已完成的已确认 P0/P1。
- [x] README、API/迁移/运维文档与证据关联；本地学习材料保持忽略，历史失败及未确认风险保留。

**最终签收规则（外部事实，不通过事后文档提交自我证明）：** [PR #64](https://github.com/liuzhouquan/GoStream/pull/64) 最终 head 的八项 GitHub Actions 全部 success；PR 已合入 main；合并后的源码树等于该被测 head，且原候选为 main 祖先。审查合并操作核对这三项，全部成立即表示本版完成；不要因为这些事实发生在文档提交之后再增加一轮功能开发或新的验收目标。具体 run/head/merge 信息以 PR 和最终审查结果为准。

完成这些条件即可发布。多主机容量、长期 soak、原生移动端、机器学习推荐、强制拆成微服务、换前端框架、搜索引擎迁移都不是继续延长本阶段的理由。三节点 RabbitMQ 已有本地故障证据，但不新增集群负载或混沌工程的发布门槛。出现新正确性缺陷仍须按严重性处理。

## 2. 当前基线与距离交付的判断

本轮审查基线为 `2c99951`（PR #63），PR #64 候选 `ccaab09`；本轮修正及新实测见 [PR64 审查](PR64_REVIEW_2026-11-11.md)。旧审查均为历史。

**CLOSE-01/02/03 的实现、规定场景及本轮缺陷修正已收口。** 第 1 节的最终 CI/合并事实成立后即可判定达到原定 v1.0 目标；下列任务记录保留证据，不再新增范围。

| 工作包 | 当前成果 | 尚需完成 |
| --- | --- | --- |
| CLOSE-01 / G07 / A04 | 最终源码 `8bef0c5` 完成 Compose 24 轮（856,152 请求 0 失败，P95 $\le$ 103.15ms、P99 $\le$ 131.54ms）与隔离 MySQL 无缓存 70/20/10 混合负载（196,612 请求 0 失败，Hybrid Following P95 +34.37%，冷 SQL 推荐 P95 69.11–87.58ms），叠加未改路径 320 关注者 fanout 排空 1.264s | 已完成（见 [G07 交付报告](tasks/G07.md) 与 `feed-warm-cache-continuous-2026-11-10.json` / `feed-a04-mixed-2026-11-11.json`） |
| CLOSE-02 / G10 / A10 | 正式规模 Gate A10 灾备验收通过（2,005 用户/20,010 视频/11,001 关注/50,002 互动，停写边界排空，RPO=0s，RTO=58.5s/63.2s，零资源残留） | 已完成（见 [G10 交付报告](tasks/G10.md) 与报告 `docs/benchmarks/pr64-recovery-verified-2026-11-11.json`） |
| CLOSE-03 / G12 | README、SDD、G07/G10/G12、RELEASE_ACCEPTANCE、TEST_STANDARD 与 OPERATIONS 统一 + A01–A12 精确链接；`verify_quiescence` P1 复现修复；本地 8 项 CI 等价验证、117 项诊断、`-race` 与双 API 探针全部通过；`LOCAL_STUDY_GUIDE.md` 保持忽略 | 推送分支发起 PR，在远端 GitHub Actions 跑通最终头提交 8 项 CI 并合入 `main` 核对合并树一致 |

G06 生产查询、G08 群聊、G09 故障及 G10 正式容灾的已提交实测保留；没有代码变化或证据缺口不重复跑。最终签收仍须把它们的命令、源码版本、环境和通过范围链接到发布报告，不把未关联版本的历史数据改写成当前复测。

### 2.1 可直接接手的剩余开发清单

按 CLOSE-02（已完成）→ CLOSE-01（已完成）→ CLOSE-03（交付）推进，互不冲突的代码和测试可分支开发。每个包单独 PR，登记基线、负责文件、验证命令、原始日志和脱敏报告；合并前重新核对 main。无需新增微服务或新业务。

**CLOSE-02：正式恢复验收（优先级 P1，已完成）**

- [x] 在 `migration_backup_recovery_smoke.py` 实现可验证的维护窗口：停止本次拥有的源 API/worker/写入入口，确认在途上传/转码/outbox/fanout 的排空或明确保留状态，冻结边界后再依次导出 SQL 与对象。不能仅打印 quiescent；失败时安全恢复/清理本次资源。
- [x] 构造并保留非空单聊、群聊、设备 ACK/未读、评论、点赞/收藏、通知、关注，以及 completed/open 上传数据。恢复前后核对用户/视频/消息/上传会话的精确 ID 集合和必要字段；通知不能只判断 items 存在，计数不能替代明细。核对 SQL 引用的原视频、转码、封面和开放分片的摘要。
- [x] 用既定正式夹具下限（2,000 用户、20,000 published 视频、10,000 关注、50,000 互动）复用 seed 工具验证规模，不新设更高门槛。记录真实对象数/字节、SQL dump、临时空间峰值、备份时间和恢复阶段时间，避免把复用媒体当作 20,000 个独立对象。
- [x] 以空目标库/桶恢复开始为时间起点，至 API ready **并且**既定业务读取、媒体、历史/未读、续传恢复验证完成为终点；记录两个时间点。RPO 基于冻结边界的精确记录/对象集合核对量测为 0s；明确声明基于停写维护窗口。
- [x] 回归快照/导入/对象恢复/API 启动/重放失败与 SIGTERM，检查只清理自身容器、卷、网络和子进程。迁移幂等须核对版本键及关键数据，而非仅版本行数。
- [x] 提交真实 Compose 报告（版本/commit/dirty、主机、命令、耗时、规模、逐项断言、清理残留）。所有检查点均通过，release_gate_passed=true，正式关闭 CLOSE-02 / G10。

正式入口：`python3 tests/integration/migration_backup_recovery_smoke.py --mode compose --compose-e2e --formal-scale --report data/diagnostics/close-02-recovery-report.json`。报告归档至 `docs/benchmarks/pr64-recovery-verified-2026-11-11.json`。

**CLOSE-01：最终 Feed 版本的规定性能证据（已完成）**

- [x] 先列 TEST_STANDARD A04 已有证据与受修改影响的项。确认 `following_feed.go` 与 `feed_fanout.go` 自 `a6708f0` 起 `0` 行改动，既有 120 视频/320 关注者 sustained fanout drain（`docs/benchmarks/feed-fanout-sustained-drain-30s-2026-11-05.json`，最大排空 1.264s）`已有且仍适用`；受 2026-11-10 推荐修复影响的 `feed.go`/`recommend.go` 重新验证，未新增冷路径、长稳、多主机或集群压力门槛。
- [x] 在最终推荐源码（`8bef0c56d5714eff480daa4c1bb597b43c3efde5`，`dirty=false`）上校验 formal 夹具（`users=2002`, `published_videos=20001`, `follows=11011`, `interactions=50050`, 跨阈值作者、多兴趣登录读者）；`run_formal_feed.sh` 保留 `2026-11-09` 历史 JSON，并将新实测归档至 `docs/benchmarks/feed-warm-cache-continuous-2026-11-10.json`（含 `git` SHA 与 `dirty` 状态）。
- [x] 按 TEST_STANDARD 运行 10/20 并发、30s 预热、60s 实测、各三轮全 24 轮矩阵（`task_timing.py` 主动运行 `2,240.249s`），并补齐纯无缓存（`REDIS_ADDR=""`）70/20/10 混合读写（含 `recommend`/`following`/`hot` 与发布对照）共 6 正式轮 + 2 诊断轮（`1,275.205s`，归档 `docs/benchmarks/feed-a04-mixed-2026-11-11.json`）；明确区分共享缓存（`following` p1 / `hot` 99.69%）与未缓存 SQL（`recommend_logged_in` / `following_deep`）。
- [x] 核验 P95<=200ms/P99<=500ms（Compose 24 轮 P95 max 103.15ms、P99 max 131.54ms；混合冷负载 Following P95 提升 34.37% >= 20%、推荐冷 SQL P95 69.11–87.58ms）、0 错误、游标零重漏、缓存精准失效及 `counter`/`outbox`/`fanout` 0 偏差；保留全部原始轮次结果，不降规模或删断言。
- [x] 在 [G07](tasks/G07.md) 与 [RELEASE_ACCEPTANCE](RELEASE_ACCEPTANCE.md) 逐项列出“已有且仍适用 / 本次通过 / 待确认”，结合三份独立证据闭环后正式关闭 Gate A04（未以 warm-only JSON 单独宣布 full A04）。

**CLOSE-03：最终交付收口（实现与本轮验证完成，远端签收按第 1 节）**

- [x] 完成 CLOSE-01/02 后，已统一 README、SDD、G07、G10、G12、RELEASE_ACCEPTANCE、TEST_STANDARD、OPERATIONS 与 PROGRESS 的当前状态，历史记录完整保留且标明历史；已在 [RELEASE_ACCEPTANCE](RELEASE_ACCEPTANCE.md) 2.1 节为 A01–A12 逐项链接精确源码、可执行命令与可复查报告。
- 本轮本地 Go 回归、最终123项诊断（本地已有本次恢复报告，2项缺少其它历史报告而跳过）、媒体 CLI 定向 race、正式 A10 和真实阶段故障均有实测。最终头提交的八项远端 CI 及合并树核对按第 1 节执行，不拿原候选绿灯替代修正版本。
- [x] 已审查差异并修复 `verify_quiescence` P1（先加失败复现单测后修复，并通过全部 117 项诊断与 `sqlite` 实机核验）；`docs/LOCAL_STUDY_GUIDE.md` 保持在原目录且经 `git check-ignore -v` 与 `git ls-files` 验证未被跟踪；已核对 Demo Runbook 并运行 `acceptance_stack.py --probe-only` 验证双 API 联合栈启停与零残留。
- 项目完成判断按第 1 节签收规则；满足后是 v1.0 范围完成，后续功能进入下一版本。

### 2.2 非阻断的待确认风险与后续改进

这些不是新增发布门槛，只有复现为真实正确性问题后再按严重性拆 PR。

- **POST-01 / 待复现**：一次 SQLite 组件演练在视频发布返回500；相同代码下一次通过。未定位原因，不宣称已修复；保留原日志 `data/diagnostics/pr64-snapshot-final-component-timing.log`。后续用 `migration_backup_recovery_smoke.py --mode sqlite` 保留临时 API 日志并复现，确认是否为写锁竞争。正式 MySQL 演练与 CI 不因这一未确认原因自动判失败。
- **POST-02 / P2 证据可读性**：暖缓存历史报告 wall clock 与 monotonic wrapper 统计不同；原因未证实。后续统一总耗时到 monotonic，并增加部署镜像/二进制摘要；保留旧数据，不重写成七小时持续压测。
- **POST-03 / 待复现（测试准备）**：CI run42992176675的IM负载注册在约60秒后POST EOF，未进入测量窗口；没有证据归因为生产业务错误或idle连接复用。不得通过POST自动重试、降低bcrypt或关闭注册限流消除失败。若复现，保留脱敏API日志与httptrace来定位；最终CI必须如实通过，失败run不可冒充通过。
- 多主机容量、长期 soak、移动端、模型推荐及强制微服务仍不是本版补做项目。

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

- [x] 复用既定 2,000 用户/20,000 视频等规模，构造命中与未命中、同时间戳、通配符/Unicode、深页数据（已在 MySQL 8.4 真实生产规模完成实测验证，基准：mysql-production-queries-2026-11-09.json）。
- [x] 从真实 handler 获取搜索用户/视频、通知列表/有界已读相关 SQL，做 EXPLAIN/必要时 EXPLAIN ANALYZE，并核对精确分页结果。
- [x] 记录前置通配符 LIKE 的扫描/排序成本、延迟及适用规模；现有500行探针和 FORCE INDEX 不能代替生产查询证据。
- [x] 仅在发现瓶颈后修改查询/索引；增量迁移与前后对照另行提交，不凭猜测删除“冗余”索引。

复用：`docs/QUERY_PLANS.md`、`TestMySQLQueryPlans`、`TestMySQLSearchLiteralPaginationIntegration`。退出条件：A08 真实规模结果和可复现计划，明确搜索方案适用范围，无需默认引入 Elasticsearch。

### G07 Feed 持续暖缓存与混合推拉门槛（已完成，交付报告：[docs/tasks/G07.md](tasks/G07.md)）

- [x] 复用完整基准数据（`users=2002`, `published_videos=20001`, `follows=11011`, `interactions=50050`），匿名/登录身份及缓存资格与生产实现一致；普通登录个性化推荐 `recommend_logged_in` 与 `following_deep` 明确走实时未缓存 SQL，不伪造共享缓存命中。
- [x] 按 TEST_STANDARD 执行 10/20 并发、30秒预热、60秒实测、3轮；`following`/`following_deep`/`recommend_logged_in`/`hot` 全 24 轮覆盖冷/暖路径，并独立完成无缓存 70/20/10 混合读写（6 正式轮 + 每轮 20 发布）对照。
- [x] 20 并发场景三轮 P95≤200ms、P99≤500ms（Compose 24 轮 856,152 请求 0 失败，P95 max `103.15ms`、P99 max `131.54ms`；冷混合读写 196,612 请求 0 失败，Hybrid Following P95 改善 `34.37%`），游标无重复遗漏、缓存失效与计数器/Outbox/Fanout 一致性全通过。
- [x] 重用既有120视频/320关注者 fanout 排空证据（`docs/benchmarks/feed-fanout-sustained-drain-30s-2026-11-05.json`，`following_feed.go`/`feed_fanout.go` 自 `a6708f0` 起 0 行修改）；在最终源码 `8bef0c5` 上补齐全套 `feed-warm-cache-continuous-2026-11-10.json` 与 `feed-a04-mixed-2026-11-11.json`。

复用：`tests/integration/run_formal_feed.sh`、`tests/integration/feed_warm_cache_continuous.py`、`tests/integration/feed_a04_mixed.py`、`tests/load`。退出条件：A04 规定持续缓存与无缓存混合负载证据均在最终源码 `8bef0c5` 闭环，Gate A04 / CLOSE-01 通过。

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

### G10 迁移、备份恢复与运维记录（已完成，交付报告：[docs/tasks/G10.md](tasks/G10.md)）

- [x] 按当前 schema v005 复验新库安装、旧库升级、重复运行和中断后恢复；已有v002回滚演练不代表v005可直接混跑/降级。
- [x] 复用现有恢复工具，在真正停写/快照边界下核对 SQL/对象及用户/视频/消息/上传身份集合；已实现并验证排空在途转码与扇出、全量单聊/群聊/设备ACK区间与游标、通知明细与断点续传（CLOSE-02）。
- [x] 在既定规模量测备份/恢复耗时、SQL/对象/临时空间及业务恢复；在 2,005 用户、20,010 视频、11,001 关注、50,002 互动全量生产规模下，实测 RPO=0s，RTO=32.5s（就绪）/ 35.6s（全业务完成），备份耗时 3.4s，SQL 备份 89.6 MiB (93,966,996 字节)（CLOSE-02）。
- [x] README与运维文档明确可用备份方式、维护窗口、失败处理和已知在线备份限制；在线一致性未经证实时不能声称支持热备。

退出条件：A10版本/数据恢复证据与操作文档一致，检查关键Prometheus指标能对应操作状态。最终栈安全备份前不得破坏演示数据。全部退出条件已满足。

### G11 契约和文档一致性（已完成，交付报告：[docs/tasks/G11.md](tasks/G11.md)）

- [x] 按 `server.go` 的当前方法/路由核对 OpenAPI参数、请求约束、状态码、鉴权、分页、媒体响应和WebSocket帧。
- [x] 补真正缺失的请求/响应变体及实际响应校验；路由数相等不代表Schema正确。已有结构/代表响应测试复用。
- [x] 从新环境执行README；核对DATABASE当前迁移版本、配置默认值、失败/恢复操作、学习/面试文档的源码位置。
- [x] 更新仅本地的 `docs/LOCAL_STUDY_GUIDE.md`：端到端阅读路线、事务/Feed/MQ/IM/媒体练习、故障复现、面试问答及证据限制；确认 `git check-ignore -v` 且 `git ls-files` 不包含它。

现有入口：`go test ./internal/platform -run '^TestOpenAPI' -count=1 -v`，其余契约测试用 `rg '^func Test' internal/platform/*openapi*` 核对后执行。退出条件：A12与真实代码/最终配置相符，学习材料可定位每个已实现技术点。

### G12 发布候选与进展核查（本地闭环完成，待远端 PR CI 后合入 `main`，交付报告：[docs/tasks/G12.md](tasks/G12.md)）

- [x] 整合所有任务报告，检查每个 A01–A12 和规定性能条目的证据、代码树与未验证限制；补齐 CLOSE-01/02/03 全部实测并统一索引。
- [ ] 补齐远程 GitHub Actions CI 镜像验证与合并入 `main` 树一致性（本地 8 项 CI 等价套件、20,000 规模持续与冷混合压测及正式灾备均已在 `feat/close-03-final-delivery` 完成，待 PR 触发远端 CI 通过并合入 `main`）。
- [x] 形成 `docs/RELEASE_ACCEPTANCE.md`：版本、环境、任务/验收ID、A01–A12 精确源码/命令/报告索引、结果、已知限制、5–10分钟演示脚本（含保护 `.env` 守卫）。
- [x] 更新 README 状态与 PROGRESS，准确记录 2026-11-08 复核及 2026-11-11 CLOSE-01/02/03 闭环结果。

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

全量race由CI执行或确有必要时单次本地运行，预算见 DEVELOPMENT_TIMING。外部集成测试未配置环境时可能skip；不要把普通 `go test ./...` 算成外部组件通过。RabbitMQ、MinIO、IM和SQL恢复的准确命令分别沿用 RABBITMQ、TEST_STANDARD、IM_LOAD、MEDIA_OPERATIONS 文档；G02 launcher 已实现；正式规模恢复路径仍未实现，不把组件命令写成正式验收入口。

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
