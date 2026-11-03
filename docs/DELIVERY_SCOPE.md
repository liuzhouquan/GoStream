# GoStream 交付边界与完成标准

更新日期：2026-11-03。本文件定义求职展示版的验收范围；[`TEST_STANDARD.md`](TEST_STANDARD.md) 定义通过条件，实际完成情况以代码、自动化检查和 [`PROGRESS.md`](../PROGRESS.md) 的验证记录为准。下文的目标不等于当前已实现功能。

## 目标形态

一个可以在本机用 Docker Compose 启动并通过浏览器操作的短视频社交平台。后端以 Go 模块化单体为主，提供 MySQL 持久化、Redis 缓存和跨实例协作、可靠的异步任务、视频处理、Feed、社交互动、通知和 IM。前端覆盖主要用户流程。项目文档能让读者复现启动、测试、故障恢复和性能结果，并能定位关键设计在代码中的位置。

模块化单体是当前选定的部署边界。仅在测得独立扩容或故障隔离确有收益时拆服务。推荐系统使用可解释规则；原生移动客户端、机器学习训练平台、互联网规模的容量承诺不属于本次交付。

## 固定的产品边界

| 使用者 | 必须完成的行为 | 数据及权限规则 |
| --- | --- | --- |
| 游客 | 浏览已发布视频、公开评论、公开主页、用户和视频搜索 | 看不到未发布或处理失败的视频、私信、通知和管理操作 |
| 登录用户 | 修改自己的资料与头像，关注/取关，点赞/取消、收藏/取消、评论与回复，查看 Feed、热榜和通知 | 每个操作由 JWT 确定身份；重复提交不会重复增加计数或通知 |
| 视频作者 | 上传或续传、发布、编辑、删除自己的视频，查看处理状态并在失败后重试或替换源文件 | 视频和封面须由作者拥有；只有 `published` 状态进入公共 Feed；失败详情只对作者可见 |
| 聊天参与者 | 单聊、群聊、历史、离线未读、逐设备 ACK；群主维护成员 | 私信仅双方可见；当前群成员可读完整保留历史（含入群前及离群期间），重邀只重置未读起点；客户端以服务端消息 ID 去重 |

Web 页面至少能完成表中全部流程。界面使用当前静态客户端即可；没有将 React/Vue 或移动客户端列为交付条件。搜索在本项目规模先支持用户、视频和文本内容的子串查询，是否升级索引由 [`TEST_STANDARD.md`](TEST_STANDARD.md) 中的查询计划和负载结果决定。

## 固定的技术边界

- **MySQL 是交付配置的数据源**：账户、关系、视频、交互状态、消息、通知、上传会话和异步任务均持久化。SQLite 是便于学习和快速测试的开发配置。Schema 变更最终由版本化迁移执行。
- **Redis 承担可丢失的加速与协作数据**：Feed 缓存、在线状态和实时事件提示；Redis 故障不能使已提交的数据库记录消失。缓存失效的最大容忍时间必须在测试中验证。
- **RabbitMQ 承担跨实例异步事件投递**：业务事务写入 outbox，再由发布者确认送达；消费者按事件 ID 幂等处理。视频处理可以继续采用已存在的数据库租约队列，是否改走 broker 取决于测量结果，不能形成两套互相矛盾的任务状态。
- **MinIO 承担最终视频、封面等对象存储**：分片暂存和完成、FFmpeg 产物发布、删除及垃圾回收须有明确的对象所有权。两台 API 实例必须能继续同一个上传会话；单机本地文件配置保留为开发模式。
- **IM 的持久保证由 MySQL 提供**：同一发送者的 `client_id` 重试只形成一条消息；服务器消息 ID 是会话历史的排序依据。实时推送允许重复或短暂丢失，重连必须通过未读/历史补齐。不存在跨不同会话的全局到达顺序保证。
- **部署仍以模块化单体为主**：API、后台 worker 可以运行在同一个 Go 程序，必要时可通过配置拆开启动。服务拆分需要可复现的容量或故障隔离证据。

RabbitMQ 通知链路已接入确认发布、消费者持久化幂等、延迟重试、死信和显式回放；真实组件与进程验证见 [`RABBITMQ.md`](RABBITMQ.md)，A06 的完整进程故障窗口与最终配置验收仍待补齐。混合 Feed 已实现可配置推拉、持久化投递及 v004 拉取索引；完整数据集无缓存读取子门槛曾测得 36.16% median P95 改善。A04 混合读写首次六轮用20 worker共用10个reader，结果未达门槛且有账号写入干扰；runner随后改为20个独立reader。修正后的 [run #41566643755](https://github.com/liuzhouquan/GoStream/actions/runs/41566643755) 和新增服务端直方图后的 [run #41573060866](https://github.com/liuzhouquan/GoStream/actions/runs/41573060866) 请求、120次发布和一致性检查均通过；新增阶段直方图后的 [run #41579403460](https://github.com/liuzhouquan/GoStream/actions/runs/41579403460) 中，Hybrid median following client P95 为74.257 ms，SQL-only为54.445 ms（慢36.39%）；一致性全通过而+20%性能目标未通过。Hybrid候选阶段的P95桶较高，但均值较低；互动阶段P95桶相同。MySQL全局Questions和API CPU与模式相关，尚无查询级根因证据。当前实现增加了 60 秒测量窗口的数据库连接池等待计数/时长，以及覆盖 30 秒预热加 60 秒混合负载的 MySQL schema 级语句 digest 诊断；本地 MySQL 8.4 样例查询及后续全尺寸 runs #41585998119、#41590181656 已采集账户级 prepared-event 汇总；它们仍不能关联到单条 SQL，Hybrid P95 门槛继续失败。新全尺寸 run #41595760362 已在 50/25 pool defaults 下采集 idle-close：六轮 wait 和 MaxIdleClosed 均为0，MySQL Threads_connected 为23；`statement/sql/set_option` 从旧运行的数千次降至17次，支持减少连接初始化的判断，但 Hybrid P95 仍慢45.77%。Go CPU profile 与 goroutine 快照采集已加入后续 A04 工作，完整 profile run 待执行。默认仍为 SQL-only。Redis 与双实例故障验收也未完成。MinIO 对象模式已接入上传、分片续传、转码产物、HTTP Range 读取和定期清理；真实双实例与停机恢复演练已通过，离线旧媒体导入与隔离 MySQL/MinIO 备份恢复演练也已实现；完整 A03 失败矩阵、在线一致性和生产规模恢复时间仍待完成。当前已有 CI、固定 SQL 的 `schema_v001`、outbox 索引的 v002 升级、Feed 表的 v003 升级、v004 索引/回填及隔离 MySQL 库的备份恢复和 v002 回滚演练。不得在 README 或简历中把未实现的部分描述成已实现。

## 当前状态

| 范围 | 已有实现与证据 | 达到交付标准前仍需完成 |
| --- | --- | --- |
| 用户、社交、互动、通知和搜索 | 注册登录、JWT、资料与头像、关注、点赞、收藏、播放、一级评论回复、通知、分页搜索；API、SQLite 测试及部分 MySQL/浏览器验证见 [`API.md`](API.md)、[`PERFORMANCE.md`](PERFORMANCE.md) | 对用户列表和搜索等查询做真实数据量的索引/查询计划检查；补齐关键接口的异常与权限验收 |
| 视频 | 本地和 MinIO 双模式上传、4 MiB 分片续传与校验、发布编辑删除、FFmpeg 异步转码和封面、失败重试与清理；MinIO 双实例跨进程续传、转码和停机恢复见 [`PROGRESS.md`](../PROGRESS.md) | 继续补存储完全中断、更多权限负例及视频编解码格式矩阵；测量较大数据量的恢复时间与空间 |
| Feed | 时间线、关注、热榜、规则推荐、游标；Redis 短期首页缓存与失效；普通作者推送、高粉作者拉取已实现；完整夹具 MySQL 无缓存读对比三轮中位 P95 改善 36.16%，零失败且前 40 个 ID 相同，见 [`PERFORMANCE.md`](PERFORMANCE.md) | 此结果只通过无缓存读收益子门槛；20 独立 reader 的完整六轮混合读写对照已复测两次，Hybrid following P95 均未达改善门槛；新增服务端 scene/outcome histogram 后又完成一次六轮，服务端 P95 桶显示轮次差异但不能定位 SQL 根因；缓存预热下的关注/推荐目标、写入时 fanout 排空的持续性、Redis 故障回退与双实例进程故障验收仍待完成 |
| 异步与一致性 | 事务 outbox、租约、RabbitMQ 确认发布、持久化消费者幂等、重试/死信、操作命令和真实组件恢复验证 | 补实际生产者/消费者强杀窗口、DLQ 目标中断、持续积压及最终 Compose 联合故障验收；实时推送仍允许重复 |
| IM | 单聊、群聊、WebSocket、多设备、离线重放、历史、未读、ACK、Redis Pub/Sub 跨实例通知；真实双进程故障/重连、重复提示、105 条单聊/群聊补页及设备 ACK 已验，见 [`IM.md`](IM.md) | 群前端积压/缺口同步及成员撤回已补回归；默认 presence 完整过期已有真实 CI 证据；仍需最终全组件两浏览器、区间压缩的规模查询成本及规定压测 |
| 工程与运维 | Compose、健康检查、Prometheus/Grafana、pprof、Go 单元与集成测试、部分真实 MySQL/浏览器检查；CI 质量与 MySQL 集成任务已通过；`schema_v001` 固定 SQL、v002 升级及隔离库的备份恢复/回滚演练已实现 | 隔离 SQL 与媒体恢复已验证；仍需较大数据量的恢复时间、空间成本与多场景压测记录 |
| 前端与文档 | 静态 Web 客户端覆盖主要流程；README、架构、数据库、API、技术设计、性能、学习与面试文档均存在 | 用最终部署方式重走浏览器主流程；补齐统一验收记录、配置/部署故障排查和剩余 API 契约变体，修正文档与实现的偏差 |

上表的“已有实现”表示当前代码中存在并有相应局部验证，不表示所有并发、故障或部署场景已通过。现有跨实例 Pub/Sub 是尽力而为的实时推送；消息历史与未读状态保存在数据库。现有数据库 outbox 已接入可选 RabbitMQ 通知投递，消费者用独立的持久化投递 outbox 记录保证幂等；IM 的直接消息/群消息实时提示仍经 Redis。现有搜索以带前置通配符的 `LIKE` 查询为主，规模上限未经压测。

## 求职展示版验收门槛

以下项目全部满足后，才能在 README 和简历中称为本阶段“完成”；每项须在进度或性能文档留下命令、环境和结果。具体测试编号、样例数据和通过规则见 [`TEST_STANDARD.md`](TEST_STANDARD.md)。

1. **可复现启动**：按 README 用当前 `go.mod` 指定的 Go 版本或 Compose 启动。MySQL、Redis、消息代理、MinIO、FFmpeg 和 Web 客户端配置可复现；`/ready` 与监控指标正常。
2. **完整用户路径**：浏览器完成注册登录、上传/续传、异步发布、浏览/推荐/关注 Feed、点赞评论收藏、搜索、单聊群聊、通知；刷新或重启后数据仍可读取。
3. **Feed 深度**：混合推拉策略有实际代码、可配置阈值、游标和去重处理；用包含多作者、高粉作者、读写混合的真实 MySQL/Redis 数据集，与原 SQL 拉取方案比较吞吐、P95/P99、错误率和资源占用。
4. **可靠异步链路**：业务提交与待发事件原子落库；消息代理发布确认、消费者幂等、失败重试/死信和恢复经过自动化及至少一次真实组件故障演练。视频任务和通知对重复执行有明确结果。
5. **IM 语义**：两 API 实例间单聊/群聊、断线重连、离线历史、多设备 ACK、顺序及重复投递经过真实 Redis/MySQL 验证；说明实时推送与持久历史各自提供的保证，并控制回执表增长。
6. **数据与发布安全**：关键模块有清晰的业务与存储边界；版本化迁移能从空库安装、从上一版本升级，并有回滚或恢复说明。CI 至少执行 Go 测试、vet、前端语法检查与构建；关键事务有真实 MySQL 集成测试。
7. **可审查证据**：文档与当前代码一致；压测记录环境、数据规模、命令、至少三次结果及局限；不得把 SQLite 内存基准或单机 warm-cache 读压测称为生产容量。

目前七项均有不同程度的基础，但尚未全部通过最终配置的验收。当前可验证的功能与缺口见上表；`PROGRESS.md` 的勾选是局部功能完成记录，不代表这里的整项门槛通过。

## 变更和完成记录规则

每个后续工作包都应同步更新代码、API/数据设计、`PROGRESS.md`、对应的测试证据和本地学习笔记。记录格式至少包含：目标和取舍、修改的文件、执行过的命令、真实结果、尚存限制。未运行的测试标为“未运行”，失败后修复的测试保留原因和复验结果。任何新的技术组件都要说明它解决的具体问题、失效时的行为和可观测指标。

范围变更应先修改本文件及测试标准，再实现代码；避免只因功能已写出就倒推“完成标准”。

## 接下来的开发顺序

每个工作包先核对审计风险，再补可失败的回归用例、最小修复和真实依赖验证。按下面的退出条件推进，不用已写出的功能数量计算完成率。范围和判定以本文件及 `TEST_STANDARD.md` 为准；`PROGRESS.md` 只保存执行证据。

| 顺序 | 工作包 | 退出条件与证据 |
| --- | --- | --- |
| 1 | A07 群可靠性 | 服务端提交顺序、前端补页/安全 ACK 与成员撤回均已有定向回归；最终部署浏览器验证仍待完成 |
| 2 | A07 设备生命周期与 A12 契约修订 | presence 和新设备不变量已补进程/数据库回归，OpenAPI 已加入结构和代表性 Schema 校验；逐设备连续 ID 回执压缩已实现，继续验证稀疏查询成本、完整请求约束及最终部署。见 BRANCH_REVIEW_2026-10-30.md |
| 3 | A06 broker 故障边界 | 对确认发布、消费提交与 ACK 窗口做真实进程强杀；验证 DLQ 目标故障和持续积压的恢复，核对 SQL 与 broker 状态 |
| 4 | A03/A04/A05 剩余验收 | 补对象存储完全中断与格式/权限矩阵；20 独立 reader 的两次完整六轮 A04 混合读写均未达 +20% 门槛，最新 Hybrid median following P95 慢 28.74%。新增 Feed 候选查询、互动状态与推荐上下文的 API 阶段 histogram，能进一步区分 handler 数据加载阶段，但不等同 MySQL 执行时间；该指标尚无新全尺寸样本。仍需解释 redo checkpointer lag、稳定环境重复观测和可复现 profile/查询等待证据，再决定优化；Redis 与双实例故障仍待完成 |
| 5 | A01/A02/A08–A12 最终交付 | 最终全组件双实例部署完成两浏览器流程、查询计划、机器可读 API 契约、文档复核，以及规定的 IM/媒体负载 |

这些是工作包，不是按日期承诺。已实现的版本化迁移、CI、对象存储和混合 Feed 不再列为“从零实现”；它们剩余的故障、容量及最终配置验收仍按矩阵保留。

2026-10-30 十分支复核：新增的组件测试、真实默认 presence 过期、OpenAPI 和浏览器 CI 是验收证据的补充，不能将 A01–A12 全部或任意整项自动改成通过。当前决定及实测记录见 [BRANCH_REVIEW_2026-10-30.md](BRANCH_REVIEW_2026-10-30.md)。此前否决的全局回执压缩仍未纳入。

2026-10-31 A07 回执生命周期：新增每设备连续 ID 区间，在保留未确认缺口和新设备未读语义的前提下渐进压缩；v005 增量迁移不修改已有回执或消息。实现、升级限制与本轮证据见 [DEVICE_ACK_RANGES.md](DEVICE_ACK_RANGES.md)。规定负载和最终全组件验收仍未关闭。

2026-10-31 三分支审核：broker 增量区分真实 TCP 断连组件验证、预提交去重夹具与尚未完成的精确进程强杀；混合流量核对独立事件及完整 DLQ 身份。Feed 只减少一次预查询，不宣称达到 20% P95 收益；SQLite 基准及真实 MySQL 门槛分别记录。媒体补头像权限与严格 TCP 恢复断言。最终交付分支新增 SQLite `/health`/`/ready` 解耦回归，既有 49 路由映射/代表性 Schema 和 SQLite 浏览器 CI 不等同于最终全组件验收。结论和运行证据见 [本轮审核](BRANCH_REVIEW_2026-10-31.md)。A01–A12 继续按原测试矩阵判断。

2026-11-02 Feed 查询微调：三路 hybrid 来源通过 `feed_pull` 与 inbox 存在性条件互斥，改用 `UNION ALL`；SQLite 小型 handler 基准 Hybrid 中位数下降约 4.8%。该数字本身不是 MySQL、HTTP P95 或正式负载结果；同日后续 workflow 的完整夹具无缓存读对比改善 36.16%，详见 [`PERFORMANCE.md`](PERFORMANCE.md) 与 [`TEST_STANDARD.md`](TEST_STANDARD.md)。读写混合、积压及双实例故障门槛仍未通过。

2026-11-02 A04 负载驱动与无缓存读取结果：HTTP driver 支持规定的 30s/60s 窗口和原始成功延迟报告；workflow run [#41452090999](https://github.com/liuzhouquan/GoStream/actions/runs/41452090999) 随后在完整 MySQL 夹具上完成 SQL-only/Hybrid 各三轮，P95 median 改善 36.16%、请求零失败、前 40 个 ID 一致。它关闭无缓存读收益子门槛；A04 读写混合、缓存、fanout 积压与双实例故障仍开放。

2026-11-03 A04 混合读写验收：原 GitHub Actions [run #41478569849](https://github.com/liuzhouquan/GoStream/actions/runs/41478569849) 使用20 worker/10 reader，所有一致性通过但 Hybrid median following P95 慢36.82%。runner 修正为20个独立 reader 后，[run #41566643755](https://github.com/liuzhouquan/GoStream/actions/runs/41566643755) 完成各模式三轮；45,951个混合请求、120次 API 发布均成功，fanout 和计数/可见性/outbox检查通过，但 Hybrid median following P95 68.163 ms、SQL-only 53.055 ms（慢28.48%），20%目标仍未达到。随后 [run #41579403460](https://github.com/liuzhouquan/GoStream/actions/runs/41579403460) 采集了 API route 和 Feed stage 直方图：请求及一致性检查通过，Hybrid median client following P95 为74.257 ms、SQL-only为54.445 ms（慢36.39%），目标仍未通过。再后来的数据库诊断 run [#41585998119](https://github.com/liuzhouquan/GoStream/actions/runs/41585998119) Hybrid median following P95 慢39.33%，正确性通过且六轮 pool wait 均为0，但每轮仅见4个 statement digest；本地 GORM/MySQL 8.4 复现显示带参语句通过 `statement/com/Execute` 事件记录。A04 runner 已补 account 级命令事件计数与总耗时；它不能区分单条 SQL，根因仍未确认，需完整复测和 profile。逐轮数据及边界见 [`PERFORMANCE.md`](PERFORMANCE.md)、[`feed-a04-mixed-2026-11-03.json`](benchmarks/feed-a04-mixed-2026-11-03.json) 和 [`feed-a04-stage-histograms-2026-11-03.json`](benchmarks/feed-a04-stage-histograms-2026-11-03.json)。默认保持 SQL-only。

2026-11-03 A04 prepared-event follow-up [run #41590181656](https://github.com/liuzhouquan/GoStream/actions/runs/41590181656)：46,630 个请求与 120 次发布成功，一致性通过；Hybrid median following P95 为75.102 ms，SQL-only为53.334 ms（慢40.81%），门槛仍失败。六轮 pool wait 都是0；Hybrid 的账户级 prepared execute 数约为 SQL-only 的1.53倍，`statement/sql/set_option` 事件约为3.18倍，但事件聚合并不能证明具体 SQL 或建连次数是根因。API 现显式配置 MySQL pool 上限并导出 idle-cap 关闭计数；连接复用改善仍待同条件全尺寸复测。保持 SQL-only 默认，未对 Hybrid 查询作未经证据支持的更改。见 [`PERFORMANCE.md`](PERFORMANCE.md) 与 [`feed-a04-prepared-and-pool-diagnostics-2026-11-03.json`](benchmarks/feed-a04-prepared-and-pool-diagnostics-2026-11-03.json)。

2026-11-03 A04 pool reuse follow-up [run #41595760362](https://github.com/liuzhouquan/GoStream/actions/runs/41595760362)：连接池 defaults 50/25 下，6轮 pool wait 与 MaxIdleClosed 全为0，prepared-event `statement/sql/set_option` 从约3.7k/11.8k降至17，MySQL Threads_connected 从3提高并稳定在23。连接初始化减少得到支持，但 Hybrid median following P95 慢45.77%，没有达到门槛。

2026-11-03 A04 profile run [#41601458252](https://github.com/liuzhouquan/GoStream/actions/runs/41601458252)：62,549 个请求无失败、120 次发布成功且一致性全部通过；六轮 SQL-only median following P95 为45.027 ms，Hybrid为59.553 ms（慢32.26%），性能 gate 未通过。额外两轮 profile fixture 未计入 gate，均采集到 CPU profile 与 goroutine dump。Go CPU profile 未发现单一明显计算热点；快照里每模式约20个请求 goroutine 在读取 MySQL 响应，没有明显 mutex/semaphore 堵塞证据。A04 workflow 现有默认关闭的 `interpolateParams` 实验输入；下一次手动运行需显式开启，以观察 per-SQL digest 和同条件 P95。不得将实验结果当作生产连接配置或已确认的查询根因。逐轮测量、pprof 文件和边界见 [`PERFORMANCE.md`](PERFORMANCE.md) 及 [`feed-a04-pprof-2026-11-03.json`](benchmarks/feed-a04-pprof-2026-11-03.json)。
