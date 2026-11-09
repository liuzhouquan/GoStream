# 2026-11-09 分支审查、修复与交付缺口

## 审查基线与分支结论

本轮开始的本地 main：`aa976dbfadf5b7ec656063b5905d4cad33be5374`；远端 main：`c2e3376e3115784bd13654c048597e7323d2c320`，本地领先 55 个提交。指定的 `bd3894f731f69150b63bbe2705a747af45565e52` 已通过 `aa976db` 合入本地主线；因此不是待重复合并的分支。

已执行 `git fetch origin --prune`，逐个核对本地/远端引用、真实 GitHub PR head SHA、PR merge SHA 是否为 main 的祖先。完整记录见 [分支清单](benchmarks/branch-inventory-2026-11-09.json)：93 个引用已为 main 祖先，18 个引用对应 9 个早已 squash 合并的分支，2 个引用为已替代旧方案。引用数量包括本地与 origin 重复，不能当作独立任务数。

| Git 显示非祖先的分支 | 实际处理 |
| --- | --- |
| fix/account-storage-errors | PR #8，tip 与真实 PR head 一致，merge 已在 main，不重复合并 |
| fix/owned-lookup-storage-errors | PR #9，同上 |
| fix/web-action-counts | PR #10，同上 |
| fix/web-stored-session-recovery | PR #11，同上 |
| docs/p2-03-merge-record | PR #12，同上 |
| fix/upload-completion-lease | PR #13，同上 |
| feat/rabbitmq-notification-delivery | PR #14，同上 |
| test/im-process-recovery | PR #15，同上 |
| fix/group-commit-order | PR #16，同上 |
| feat/im-device-lifecycle (`da64f7b`) | 已被逐设备 v005 方案替代，旧方案会破坏设备隔离；不合并 |
| feat/openapi-spec (`bfde3b1`) | 已被修订契约和真实响应校验替代；不合并 |

旧分支保留，未删除分支或改动它们的工作区。除此之外，刷新引用后未发现遗漏的独立候选；本轮修正放在 `review/pending-branches-20261109`，走集成 PR 将本地候选和修复同步到远端。本文的基线与清单记录的是审查开始时状态；PR 的最后提交 CI 和最终合并状态以 GitHub 为准，未在运行前预写成功。

## Standards

### P1 · 正式数据预检后仍重置 benchmark 夹具【已修复】

审查 `3db51b4..bd3894f` 时发现：正式模式先缓存达标规模，再调用 `seed_mysql_feed_db`；该工具会删除匹配 benchmark 前缀的作者/视频及测试账号的关注、inbox，然后重建 10 作者/120 视频。它不是清空整个数据库，但足以破坏使用这些记录的正式夹具。代码随后复用旧规模，可能错误签收。

用实际 `main()` 配合合成数据库/种子/负载边界复现了 `大库预检 → 小种子重置 → 小库测量 → 旧规模正式通过`。这属于编排反例，不是数据库性能结果。

修复：正式 Compose 模式不再调用 smoke seed；使用预先准备的正式账号和数据。负载结束重新查规模，保留 `dataset_scale_before_load` 与当前 `dataset_scale`；下降则退出 1、`rejected_scale_changed`。新增“达标后仍不得 seed”和“负载后规模下降必须拒绝”的实际 main 回归；原行为先失败再通过。

### P1 · 本地验收 HTTP 请求继承代理导致重复连接超时【已定位并修复】

两次真实 Feed 短测在不同场景发生 TCP connect timeout，分别耗时 23.239 秒、21.573 秒；没有把失败计为通过。一次专用探针重放在 22.736 秒失败，捕获到请求已被 urllib 改写到本机代理 `127.0.0.1:7897`，而实际 API 监听另两个测试端口；代理 bypass 对 loopback 为 false。

排除了“只因并行测试”的解释（隔离重放仍失败）；内核监听丢弃计数不支持 API backlog 溢出。保持相同服务器、负载、超时和门槛，仅让回环 API 请求走局部无代理 opener 后，24 组全部通过，耗时 8.321 秒。真实本地 HTTP 服务 + 不可用代理的回归测试在旧代码报连接拒绝，修复后通过。

作用范围仅本脚本的 loopback HTTP、ready 和 metrics 读取；外部地址仍使用原有 urllib 代理策略，未修改 `.zshrc`、环境变量或系统代理。没有靠重试、延长超时或削弱断言掩盖失败。

## Spec

两轴审查共同确认上述正式夹具 P1；一次缺陷不按两次计数。四场景 × 两档并发 × 各三轮、深页真实 cursor、推荐真实身份、动态 anchor、非空样本与实际时长校验均保留。

### P2 · 暖缓存子项不能关闭完整 A04【已明确口径，验证仍待完成】

TEST_STANDARD 要求冷/暖路径分开，完整 A04 还包括混合推拉收益与读写混合。此脚本只负责暖缓存持续负载。本轮报告增加 `acceptance_scope=G07_warm_cache_only`、`full_a04_accepted=false`。即便未来 `release_gate_passed=true`，也仅表示该暖缓存子项通过；完整 A04 必须关联其他真实结果。

## 本轮验证

结构化真实结果见 [本轮证据](benchmarks/branch-review-2026-11-09.json)。原始日志与代理定位探针在忽略目录 `data/diagnostics/`，不上传原始认证/夹具输出。

- Go 1.27.2 `go test -json -count=1 ./...` 已运行，退出 0；无外部依赖环境的 skip 不算正式验收通过。
- 最新 diagnostics 全量回归、vet、build、JavaScript 语法和 diff 检查以结构化结果为准。
- 真实 SQLite/MiniRedis/双 API 全 24 组短测通过，`component_smoke`、`release_gate_passed=false`；不是 20,000 视频性能成绩。
- 正式夹具保护、负载后规模下降、loopback 错误代理三个新增回归均覆盖真实函数边界。合成响应的数字不作为吞吐/时延成绩。
- 完整 race 和真实组件矩阵交由集成 PR 的 CI；通过 CI 不等于补齐下面的发布验收项。本轮未运行约 36 分钟的正式 Feed 矩阵，也未宣称已全量验收。

## 剩余工作：下一位 agent 可直接执行的清单

下表区分实际缺陷和证据缺口。当前未确认新的业务 P0；本轮两个 P1 已修复。尚未量测的内容不是已失败，也不是已通过。

| 优先级/任务 | 需要完成的工作 | 退出标准 |
| --- | --- | --- |
| 发布必做 · G07 / A04 | 准备正式隔离夹具：≥2,000 用户、20,000 已发布视频、10,000 关注、50,000 互动，混合阈值两侧作者，以及真实关注/多兴趣读者；保留 bench_viewer_prod 测量账号。执行四场景 × 10/20 并发 × 各三轮、30s预热/60s测量；单列冷路径和既有无缓存收益证据 | 每轮真实样本、完整正文延迟、P95/P99、错误、分页身份、缓存/SQL/CPU/RSS与源码 SHA 可复现；不得用本次短测代替。仅窗口至少36分钟，预设总超时/心跳/清理 |
| 发布必做 · G06 / A08 | 最终 MySQL 上按规定规模复验真实 handler 搜索/通知 SQL、通配符/Unicode/同时间戳/深页，记录实际 EXPLAIN/扫描与排序成本 | 提供真实行数、计划、正确页面结果和命令；旧 SQLite component_query_smoke 不作为 MySQL 签收 |
| 发布必做 · G08 / A07 | 将群聊20客户端/1000消息、期望接收集合、去重、离线/重邀、逐设备ACK与 SQL 核验关联到最终候选树 | 实际 MySQL/Redis双 API 结果、精确 SQL/投递事实和清理结果；不能只写业务脚本入口或 PASS 摘要 |
| 发布必做 · G09 / A06 | 对最终联合栈复验 Redis/RabbitMQ/MinIO 短暂离线和恢复，保留逐 source/delivery 事件与业务终态 | 与最终版本关联的业务 checkpoint、SQL事实与恢复后收敛；launcher 元数据不能代替故障结果 |
| 发布必做 · G10 / A10 | 在声明的停写窗口完成 MySQL + 对象一致快照/恢复，核对关键身份集合与对象摘要，量测空间和完整恢复时间 | RPO/RTO有明确起止点与原始量测；文件复制阶段耗时不能替代端到端RTO，不承诺未验证热备 |
| 发布必做 · G03/G04/G12 | 核对现有两浏览器、10视频媒体矩阵证据适用的版本和变化，必要时补测；最终README/迁移/API/学习材料与代码一致 | 不机械重跑全部历史测试；用适用性说明和必要回归补齐同版证据。所有必做项通过后才签发 v1.0.0 |
| P2 · 工具可靠性后续 | 将 API 传输异常/断言失败也写成结构化失败报告，包含失败阶段和清理结果；统一其他验收脚本对 loopback 代理的策略 | 模拟超时、连接拒绝、中断均非零退出且有可定位失败记录；不得把异常当成成功或无样本 |

从这里开始，不再新增无依据的业务功能或延长产品边界。项目已处于集成与交付收尾，但“功能大体具备”与“按标准验收完成”仍需区分。

规范轴初审：2 项 P1（正式夹具错误、实测发现的 loopback 代理路由）；需求轴初审：1 项重合 P1、1 项 P2 暖缓存证据边界。两轴分别保留，重合不重复计数。
