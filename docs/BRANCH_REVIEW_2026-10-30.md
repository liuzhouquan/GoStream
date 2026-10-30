# 2026-10-30 十分支审核与验证

基线 `1f290085d7419899843681634e063bfdbf12f2d0`；每个候选原始分支都是基线上的单提交。审核命令为 `git diff 1f29008...<SHA>` 和 `git log 1f29008..<SHA> --oneline`。标准来自 `DEVELOPMENT.md`、`ARCHITECTURE.md`、`DELIVERY_SCOPE.md`、`TEST_STANDARD.md`；需求按 A01–A12、六类故障和上次 `BRANCH_REVIEW.md` 的缺口核对。两个独立只读审核轴均先报告源码问题，主 agent 再复现、修正并执行验证。

## 分支决定

| 原始分支 / 固定提交 | 审核后的处理 | 验收边界 |
| --- | --- | --- |
| `feat/im-presence-lifecycle` / `b723f73` | 修正续租配置范围；进程测试检查真实默认租约并等待实际过期；纳入集成 | 未加入上轮否决的全局回执压缩；活跃回执压缩仍待实现 |
| `feat/openapi-contract-parity` / `91a914c` | 补成熟结构/Schema 校验器、真实 HTTP/空列表/错误/实际 WS 帧校验，修正发现的规范偏差；纳入集成 | 不宣称完整请求约束、所有错误变体和最终部署文档均已验收 |
| `test/query-plans-and-index-validation` / `d5acd5b` | MySQL 探针从空业务表改为每表 500 行，并撤回虚构的计划/恒定复杂度结论；纳入集成 | 简化探针仍不等同完整生产 SQL 或 A08 规模验收 |
| `test/rabbitmq-fault-windows` / `8383047` | 修正 GORM 错误检查、DLQ 同一消息去向和暂停清理；纳入集成 | 50 条是有限积压；精确进程强杀窗口与持续负载仍待验证 |
| `test/media-matrix-and-storage-outage` / `9da28b0` | 本地/MinIO 搜索与状态隐私检查、清理错误保留；纳入集成 | WebM/MOV 容器头不证明真实编解码，A03 仍部分通过 |
| `test/feed-and-interaction-matrix` / `e5ed775` | 修正播放路由和逐请求断言；保留 Feed 边界与并发回归、取关有限死锁重试；纳入集成 | 未关闭混合 Feed 性能收益门槛 |
| `test/security-boundaries-and-permissions` / `ffeedc2` | 改测真实 WS 群发送、PATCH 编辑和 SQL 行存在性；纳入集成 | 小数据权限测试不代表最终部署所有安全路径 |
| `test/web-full-flow-e2e` / `0a9dd61` | 修正外部环境继承、清理、真实视频、资料保存断言和 API 进程重启；加入独立 CI；纳入集成 | SQLite/FFmpeg 组件流程，仍非最终 Compose 双实例或浏览器中断续传验收 |
| `test/startup-persistence-and-ready-semantics` / `4dfdedb` | 保留 SQLite 重开、健康/准备度和 DB 状态组件回归；纳入集成 | 重建 Router 不是进程重启，不能关闭 A01/A10 全组件和恢复成本门槛 |
| `test/fault-consistency-scenarios` / `d36f7bd` | 加入实际 INSERT 后失败和原子回滚；对其他用例改用真实范围名称；纳入集成 | 不把共享 SQLite、手工 SQL 条件和本地 outbox 当作实际组件停机/双进程/强杀 |

“纳入集成”指经下述修正后的候选代码。原始提交的“通过”文字不自动成为当前完成状态；最终是否合并以集成 PR 最新提交的检查结果为准。所有原始分支引用保留。

## Standards

独立审核报告 9 项，最严重的是测试给出错误安全/一致性保证；编号按该轴保留，不和 Spec 数目相加。

1. **P1，互动矩阵**：`e5ed775:feed_matrix_test.go:379–394` 丢弃互动状态；`:387` 请求不存在的 `/view`，播放数零也能通过。必须用 `/views`、检查每次状态和非零成功数量。
2. **P1，安全矩阵**：`ffeedc2:security_boundary_test.go:367–370` 用不存在的 HTTP 群发送路由证明权限；`:281/:291` 未检查 SQL 重查错误。需真实 WS、成员对照和最终行断言。主审核另发现视频编辑用了 PUT，实际路由是 PATCH，已同步修正本地/MySQL 两处。
3. **P1，Web 资源隔离**：`0a9dd61:web_full_flow.py:107` 继承 Redis/MQ/MinIO 等环境，构建和启动在清理保护外，kill 后不 wait。需显式隔离配置、从创建目录起保护清理及回收进程组。
4. **P1，故障误报**：`d36f7bd:fault_consistency_test.go:44–78` 原本只有写前 404；`:290–342` 没配置 MQ/Redis；`:419–423` 自写 SQL；`:451–455` 共用数据库对象。不能声称六类故障已完整覆盖。
5. **P1，Rabbit DLQ**：`8383047:rabbitmq_integration_test.go:433–455` 在 poison1 还未观察处理时恢复绑定，只检查 poison2；既可能丢消息而通过，也可能波动失败。应观察拒绝并验证原消息恢复。
6. **P2，OpenAPI**：`91a914c:openapi_contract_test.go:254–465` 不读取规范，`:469–540` 仅自造 JSON 往返，无法发现字段类型、required 和引用漂移。需把真实样本交给规范校验器。
7. **P2，查询计划**：`d5acd5b:query_plan_test.go:253–264` MySQL 只有一个用户且业务表为空，SQL 也简化了生产路径，不能据此关闭规模门槛。
8. **P2，presence**：`b723f73:main.go:119–127` 接受 renew≥lease，健康用户可能周期离线；2 秒不能代替默认 75 秒观察。
9. **P2，提前验收**：`4dfdedb:startup_persistence_test.go:168–177/:370–386` 同进程建 Router 不是重启；`9da28b0` 未增加真实 WebM/MOV 转码，不能关闭 A01/A03/A09/A10 或降低最低证据。

未发现为了代码风格必须执行的大规模重构。

## Spec

独立审核报告 7 项，最严重的是 broker 故障窗口和文档契约验收误报。

1. **P1，broker 窗口**：规格要求确认发布、消费提交与 ACK 窗口的真实强杀。`8383047:rabbitmq_smoke.py:130` 领取状态不证明确认等待；集成测试 `:408` 成功 publish 后不写 SQL 也不等于断连。消费者提交后 ACK 前的进程强杀没有实现。
2. **P1，DLQ**：同文件 `:433–459` 未确认 poison1 在故障期被处理、也未验证其恢复。应核对同一消息及第二消息的去向。
3. **P1，OpenAPI**：A12 要求契约与代码一致，但真实响应测试不加载规范，帧测试不使用 schema。空关注 `items:null` 不符合原数组声明，未读默认 limit 错为 50，实际通知帧遗漏。需真正校验，A12 保持部分通过。
4. **P1，Web**：A09 要求最终 Compose、续传、刷新/重启。原脚本使用单 SQLite API 和文件头加零，资料保存只读原输入框；必须保留组件测试和最终验收区别。
5. **P2，计划**：A08 要求规模数据的实际查询，原 MySQL 只有用户，单向私信与生产双向 OR 不同，文档却列出未测计划。需数据和实际 EXPLAIN，并保持规模门槛。
6. **P2，启动/故障矩阵**：同进程对象重建、手调租约和共享 SQLite 不能满足最终组件启动、进程故障与双实例最低证据。
7. **P2，媒体**：WebM/MOV 仍是头片段，没有完整编解码流程，不能把 A03 改为通过。

Presence 修订已取消不安全压缩；其余价值以实际测试和本轮修正为准。

## 修正与可失败的证据

- OpenAPI 新校验在原规范上实际报错：空关注数组不允许 null、群列表应为对象却声明数组、401 无响应 schema。修正后使用固定 `kin-openapi v0.152.0` 校验整份结构，并校验原 17 类真实 HTTP 响应、空列表/错误，以及捕获到的 ready/pong/error/direct/group ACK、消息、通知和两类 sync marker。非法帧类型明确拒绝。通知帧和默认未读 limit 也已修正；新依赖只用于测试，MIT 许可保留在 `third_party_licenses/kin-openapi-LICENSE`。
- 互动矩阵要求 200 次操作全部成功、32 次实际 view 成功及表/计数一致；错误路径不再被忽略。安全测试在同一真实 WS 路由检查 outsider error/零 SQL 行、合法成员 ACK/单行，视频修改使用实际 PATCH。
- 故障用例现在在通知 INSERT 之后注入错误，确认故障点真的执行两次，并检查点赞/评论、计数、通知和 outbox 全部回滚。其他组件测试保留原断言，但名称/文档不再伪称外部故障。
- 原 50 条 broker 轮询误将 `Count()` 返回的 `*gorm.DB` 当 `error`，条件恒真导致永远等待；已改 `.Error`。DLQ 新流程在未绑定时观察生产消费者拒绝计数，再恢复目标并核对原始 poison1，随后核对 poison2。SQL claim 仍不冒充 confirm 屏障，50 条仍是有限积压。
- Web 脚本使用 `ExitStack` 管理临时目录、日志、API 进程组及两浏览器；构建失败/提前退出也清理。API 只获得显式本地配置，不继承共享服务凭据。真实 FFmpeg 生成可播放片段，经 UI 上传和后台处理，浏览器检查解码/播放；资料保存从服务端查询确认，另执行实际 API 重启及历史读取。移动宽度和原导航布局条件保留并增加横向溢出检查。
- Presence 配置要求 `0 < renew < lease`，默认仍 75/30 秒。IM matrix 不再缩短租约，核对新 Redis score 约 75 秒，SIGKILL 后真实等待到离线；存活发送者正常心跳。CI 才能完成该真实 MySQL/Redis 观察。
- MySQL 索引探针布置每表 500 行，在事务结束回滚；索引存在/指定索引能力和排序断言保留，并增加精确结果 ID/顺序/计数断言；非提示计划按实记录。v001 时间线索引不承诺完整二元降序无需 filesort，不为该探针新增生产索引。没有虚构原始 MySQL EXPLAIN 或收益，也未增加/删除生产索引。

## 验证和剩余范围

本机 Go 1.27.2。当前环境 Docker socket 缺失，FFmpeg/ffprobe 不在 PATH，故外部组件和真实视频浏览器流程在 CI 中执行；普通测试跳过外部分支不是通过证据。浏览器脚本在本机正确拒绝缺少真实工具，未启动 API 或写演示数据。初次候选全量普通测试通过；修正后的普通测试通过（platform 16.369s），Schema/错误路径等定向测试通过。全量 race 通过（platform 151.426s），vet、三个程序构建、Web/Python 语法、八任务 YAML、文档链接、gofmt/diff 及原分支引用/祖先校验通过。实际八项 CI 结果记录在集成 PR 中，以最后提交为准。

CI 新增 `browser-workflow`：固定 agent-browser 0.37.1、Node 24 和真实 FFmpeg，执行四项浏览器回归；原七任务继续覆盖质量/MySQL/Redis/RabbitMQ/MinIO/视频恢复/IM。CI 资源按隔离 schema、容器和子进程回收。本轮没有重建本地演示容器。

A01–A12 保持证据驱动；未完成的最终全组件配置、完整浏览器断点续传、实际精确 broker 强杀窗口、持续流量、Feed 性能收益、真实 WebM/MOV 转码、安全活跃回执压缩和完整契约变体仍在原门槛中。


## 首轮 CI 失败及修正记录

Run `41037890462` 的 IM、MinIO、Redis、视频恢复通过；IM 真实默认租约观察约 75 秒并清理全部进程/库/用户。质量、MySQL、浏览器和 broker 失败，均保留了原输出：

1. 既有群历史测试把 ready 当作重放结束，偶尔把重复重放当成移除后的响应。测试现用正常 ping/pong 屏障等待初始化重放结束，原错误帧与权限断言保持；定向 30 次通过。
2. 浏览器 CI 的 Go 安装路径不同，本机绝对路径不存在。任务显式用 setup-go 的 `go`，脚本仍检查精确版本，没有回退到旧版本。
3. MySQL 实际选择 PRIMARY、其他索引及 filesort，揭示原分支“必选某索引/必不排序”的空表假设。现保留真实计划，分别验证声明索引及指定索引的能力，并增加查询结果 ID、顺序和计数断言；完整生产性能门槛保持未通过。v001 时间线索引缺显式 `id DESC` 是待优化成本，不通过修改测试宣称已优化。
4. DLQ 45 秒超时：核查 RabbitMQ 4.3.7 源码后确认默认内部重试间隔为 180,000 ms。隔离测试 broker 显式配置并验证 1,000 ms，目标保持未绑定跨多次重试，再要求原消息恢复；生产 Compose 默认未变，不把缩短的测试间隔当生产恢复时间。直接 Go 运行默认等待 210 秒；launcher 提供已验证间隔下的有界等待。

重试间隔来源：[RabbitMQ 4.3.7 Makefile](https://github.com/rabbitmq/rabbitmq-server/blob/v4.3.7/deps/rabbit/Makefile) 与 [内部 DLQ worker](https://github.com/rabbitmq/rabbitmq-server/blob/v4.3.7/deps/rabbit/src/rabbit_fifo_dlx_worker.erl)。这次调查确认原队列参数支持 at-least-once，没有在缺少证据时改动生产队列拓扑。

第二轮 run `41040441660` 已通过真实 MySQL、Redis、IM、RabbitMQ 和视频恢复。浏览器已完成真实上传、转码、播放、资料保存与互动，在新增的未读归零断言处失败：测试误从通知列表读取 `unread_count`，该字段实际由 `/api/notifications/unread` 提供。修正为读取真实计数接口，保留归零条件，不能把此次部分流程当作全部通过。

集成与最终检查统一记录在 [PR #18](https://github.com/liuzhouquan/GoStream/pull/18)；只有该 PR 最后提交的八项检查全部通过才可合并。以上失败运行属于保留的调查证据。

第三轮 run `41041150110` 仍是七项通过、浏览器失败，但已推进到双向私信、群聊及刷新之后的重新登录。隔离本地真实 API 复现发现一个既有布局问题（P1，`web/styles.css:2`）：1280×577 时侧栏内容高于视口，底部 `#auth-open` 位于可点击范围外。原自动化点击落到空白 HTML，用户仍为已登录状态。最小修复给桌面侧栏启用纵向滚动。`web_stored_session.py` 新增同尺寸退出和 modal 断言，修复前实际失败、修复后通过；真实 API 注册/建群/退出/重新登录的独立探针也通过并清理进程。完整流程固定原来的小视口边界并显式等待退出、modal 状态，没有以放大窗口避开问题。

第四轮 run `41042162037` 的七个后端任务仍全部通过；浏览器在短视口 Feed 点赞时没有发出请求。本地将计数回归改为真实鼠标点击后复现相同失败；将滚动和点击分开执行后，同一套点赞/取消/收藏/503 断言全部通过。完整流程采用显式 `scrollintoview` 后的真实点击，计数回归也从 JS `.click()` 升级为该鼠标路径并固定 1280×577；没有直接调用业务 API 代替 UI 操作。
