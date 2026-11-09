# GoStream 验收复核（2026-11-08）

## 结论与范围

**当前不能按交付文档全量验收。** 本轮发现可实测复现的验收假阳性，以及正式报告与底层证据不一致的问题。这不等于所有业务功能不可用，也不是说现有 Feed 延迟超过目标；它意味着“全部验收通过”的结论尚不成立。

被检查版本：`0c897afa6841307b2822ba0f897036f5120b8fda`。复核时工作区干净。本轮只读检查源码、既有报告和 Git/GitHub 状态，运行有界验证，仅新增本复核文档与脱敏汇总；未修改业务代码、合并分支或发布版本。

检查范围包括 README、AGENTS、SDD、DELIVERY_SCOPE、TEST_STANDARD、RELEASE_ACCEPTANCE、进度记录，最近新增的 Feed/群聊/故障/恢复验收脚本、种子工具及 benchmarks 报告。按代码规范和需求符合性两条轴分别复核。既有 Compose、浏览器及媒体报告代表已有进展，但本轮没有重新运行完整外部依赖矩阵。

## 本轮实际验证

| 检查 | 实际结果 | 边界 |
| --- | --- | --- |
| `/usr/local/go/bin/go version` | Go 1.27.2 linux/amd64 | 使用项目要求的工具链 |
| `go test -json -count=1 ./...` | 退出 0；40.600 秒 | 54 个测试事件为 skip；包括需 MySQL、MinIO、故障环境或显式启用的测试，不能计作已通过外部依赖验收 |
| `python3 -m unittest discover -s tests/diagnostics -p 'test_*.py' -v` | 81 项通过；包装器 35.268 秒 | 诊断工具测试不代替业务端到端验收 |
| `go vet ./...` | 退出 0 | 静态检查 |
| `go build ./cmd/gostream ./cmd/gostream-media ./cmd/gostream-events` | 退出 0 | 三个入口可构建 |
| `node --check web/app.js` | 退出 0 | JavaScript 语法检查，非浏览器功能验证 |
| `git diff --check` | 退出 0 | 报告落盘后再次运行 |
| 未修改的 Feed 验收器零负载反例 | 退出 0；4.864 秒；仍报告正式通过 | 证明验收判定存在假阳性，详见 R1 |

长命令通过 `tools/task_timing.py` 执行并等待退出，原始日志在忽略目录 `data/diagnostics/`；可提交的脱敏结果见 [JSON 汇总](benchmarks/acceptance-recheck-2026-11-08.json)。本轮未重跑完整 race、长时间 A04、真实组件故障、浏览器全流程；已存在阻塞反例，继续消耗长时间不能使当前版本满足验收。

## 重要问题（规范与可靠性轴）

### R1 · P1 · Feed 正式验收允许零测量请求通过【已运行复现】

位置：`tests/integration/feed_warm_cache_continuous.py:247`、`:411`、`:421`、`:446`、`:654`。

复现命令（使用独立 SQLite/MiniRedis/API 测试夹具）：

```sh
PATH=/usr/local/go/bin:$PATH python3 tests/integration/feed_warm_cache_continuous.py \
  --mode sqlite --warmup-seconds 0 --duration-seconds 0 --rounds 0 \
  --report data/diagnostics/acceptance-zero-load-report-20261108.json
```

实际输出中 `status=PASS`、`validation_scope=formal_standard`、`release_gate_passed=true`；测量请求数为 0、轮次列表为空，P95/P99 为 0。夹具初始化和语义检查仍有 API 请求，这里的“零请求”特指压测统计窗口。空样本被转换为零延迟，代码只校验失败数与延迟上界；三轮 checkpoint 和正式标志未依据实际覆盖生成。

影响：A04/G07 可以在未完成性能标准时被签收。最小修复：区分 smoke 与 formal；正式模式校验依赖、数据规模、预热/测量时间、轮数、非空样本及完整场景，缺失时拒绝签收。增加零/负时间、零轮、SQLite、空样本和失败中断的回归测试，确认这些情况不能输出正式通过。

### R2 · P1 · 群聊 SQL 核验可未执行却标为通过【源码确认；反例未运行】

位置：`tests/integration/group_im_smoke.py:678`、`:687`。

SQLite 路径和 MySQL 回调均未提供时，SQL 检查分支可被跳过，但 `sql_facts_consistent_zero_duplicates` 仍写入 true。正常 Compose 路径可能传入了回调；本条不表示已提交报告必然跳过 SQL，而是验收函数未保证该前置条件。

影响：消息持久化、去重与顺序的验收可信度。最小修复：根据真实执行结果设置 checkpoint；正式模式必须提供核验器，缺失时报错。补充缺少核验器、核验抛错、重复行及数量不一致的测试。

### R3 · P1 · 故障脚本断言失败后可能留下停机组件【源码确认；未破坏环境复现】

位置：`tests/integration/component_outage_smoke.py:575`、`:584`、`:589`、`:599`、`:745`。

MinIO 停止与恢复之间有断言，异常会跳过启动；Redis/RabbitMQ 路径也需检查同类恢复问题。独立入口可指向既有 Compose project，不能假定外层夹具总会负责清理。

影响：失败验收污染后续测试或目标开发栈，造成难以解释的后续故障与耗时。最小修复：记录资源归属及初始状态，以 try/finally 恢复本轮停止的组件，明确中断处理；用可控失败验证恢复动作，不应在共享运行栈上冒险试错。

### R4 · P2 · 种子工具错误日志输出完整 DSN【源码确认】

位置：`tools/seed_feed_benchmark.go:52`、`tools/seed_group_im_benchmark.go:62`。

数据库连接失败时日志格式包含 `dbDSN`，可能把口令写入原始输出。未使用真实凭据复现，也未发现实际泄漏证据。最小修复：移除原始 DSN 或进行可靠脱敏，使用虚构带密码 DSN 验证输出。

## 交付需求与证据轴

### R5 · P1 · Feed 数据规模与压测场景未达到文档标准【源码及报告确认】

位置：`docs/TEST_STANDARD.md:111`、`tools/seed_feed_benchmark.go:77`、`:117`、`tests/integration/feed_warm_cache_continuous.py:421`。

标准要求 2,000 用户、20,000 视频、10,000 关注、50,000 互动，以及首页/深分页/关注/登录推荐/热榜等场景。新夹具仅 10 作者、120 视频等小规模数据；默认三轮轮流执行 following、timeline、hot，每个场景只有一轮。登录推荐与深分页的语义请求不能替代对应持续压测。

既有 `feed-warm-cache-continuous-2026-11-08.json` 中三个场景 P95 约 42–43 ms，本身未超过 200 ms 门槛；问题是规模、覆盖与重复轮数不够。最小修复：接入达到门槛的正式夹具，为每个要求的场景执行规定并发和重复次数，记录真实行数、请求量、分页与用户身份。

### R6 · P1 · 最终签收、版本和底层报告不一致【已检查；部分主张待确认】

- SDD 与 RELEASE_ACCEPTANCE 声称全部完成，候选 main 却比远端 main 多 37 个本地提交。复核时远端仍为 `c2e3376e3115784bd13654c048597e7323d2c320`，最新实际 GitHub PR 为 #60；查询 #71 返回 404。本地 merge message 的 PR 编号不能证明真实 PR、审查或 CI。
- G02 的 `g02-acceptance-stack-probe-2026-11-08.json` 记录的是 `c2e3376`，不能独自证明最终候选树已完成发布验证。旧证据并非全部失效，但须明确适用变更与补测范围。
- RELEASE_ACCEPTANCE 的 Feed P95 11.2–15.8 ms/P99 22.4–31.2 ms，与上述 JSON 的约 42–43 ms/52 ms 不一致。
- `search-query-smoke-2026-11-08.json` 明确为 SQLite、`component_query_smoke`、`release_gate_passed=false`，不能作为 MySQL 正式搜索签收证据。任务报告另有 MySQL 摘要，真实性和完整覆盖应以相应日志、数据规模与执行计划确认，当前标为待确认。
- `component-outage-compose-2026-11-08.json` 主要是启动器元数据，没有业务故障 checkpoint 或 SQL 源事件/投递事实；它不能独自证明 G09。不能由此断言其他故障测试从未执行，应补交对应业务结果及关联版本。
- 恢复报告声称 RPO=0、端到端 RTO<30 秒，但 `migration_backup_recovery_smoke.py:440` 仍明确 RPO/RTO 未测量，`:463` 的 MySQL 模式未实现。项目另有 `mysql_recovery.sh` 和 `media_transfer_smoke.py`，它们可能提供部分真实验证；需要将实际测量与完整数据库/对象恢复结果对应，不能用文件复制阶段耗时替代端到端指标。

影响：无法确认最终交付版本满足 G01–G12/A01–A12。最小修复：撤回证据不足的完成标志或标为待验证；每项证据记录 source SHA、工作区状态、环境、命令、门槛、实际覆盖及结果。建立候选版本与最终 CI 的明确对应，不使用手写的成功摘要替代原始结果。

### R7 · P2 · 交付文档的路径与覆盖描述有误【文档/源码对照确认】

RELEASE_ACCEPTANCE 引用不存在的 `tests/integration/web_full_flow_smoke.py`，实际为 `web_full_flow.py`；浏览器汇总写 20/20，JSON 为 24 项。媒体描述包含 H.265/AV1/FLV、零字节/纯音频等，与已提交矩阵中的 H.264、VP8/VP9、MOV 等用例不能一一对应。

最小修复：从可执行用例和结构化结果生成覆盖表，缺失格式标待验证，不把规划写成结果。

## 接下来如何达到可验收

1. **先修验收判定（小型 PR）**：R1/R2，补充反向测试，保证 smoke、空样本、缺少 SQL 核验不能冒充正式通过；运行相关诊断、Go 回归与正式模式预检。
2. **修故障工具资源恢复与日志（独立 PR）**：R3/R4，用失败注入证明资源恢复和凭据脱敏，不削弱业务断言。
3. **纠正文档证据（独立 PR）**：R6/R7，保留真实已通过记录，将尚不具备证据的门槛恢复为待验收；标清版本与 CI 关系。
4. **再做有界正式验证**：R5 正式规模及完整场景；MySQL 查询计划；真实 MQ/Redis/MinIO 故障一致性；数据库与对象恢复；浏览器与媒体矩阵。每条长任务设超时、心跳与清理，失败后先定位，不盲目重跑整套。
5. **最终候选签收**：对确定的候选 SHA 运行规定检查和最终 CI，关联每个交付门槛的真实证据，再决定合并/发布。

本轮未确认 P0；这不是安全问题不存在的保证。上述待确认项未伪装为运行失败，未重跑的外部依赖测试也未计入通过项。当前开发重点应先使验收可靠，再补齐验收证据，而不是继续增加业务模块。
