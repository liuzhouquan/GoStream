# G07 Feed 验收场景驱动修复

日期：2026-11-08。基线：`3db51b43d856c68a524b429f209ed009cb4caf1a`。
分支：`fix/feed-acceptance-scenario-matrix`。状态：驱动修复及本地短测完成，正式规模性能验收仍待执行。

## 问题与修复

旧驱动把三轮分给 following/timeline/hot，正式判定却要求 following/following_deep/recommend_logged_in/hot 各三轮，判定要求与执行路径不一致。

`tests/integration/feed_warm_cache_continuous.py` 现在执行四场景 × 10/20 并发 × 每场景三轮，共 24 组。关注深页使用首页返回的真实游标，推荐使用登录 token；不把“following_deep”等报告标签当成不存在的 API scene。

正式判定检查两档并发、各场景至少独立轮次 1/2/3、成功样本、错误数、实际预热/测量时长及 P95/P99。保留 `concurrency_10_stats` 作为首条记录以兼容旧读取器，新增完整 `concurrency_10_rounds`；`rounds` 为全体记录，`concurrency_20_rounds` 为 20 并发子集。

真实短测进一步发现：登录推荐的首页响应游标含每次变化的时间锚点 `a`。旧压测器逐字比较游标，导致正常请求全部算失败。现在解析推荐游标，检查 scene、末条 ID、有效 anchor 并比较其余字段及完整页面身份，允许 anchor 变化。坏游标、错页、重复视频和场景串用仍计为失败。

## 验证边界

新增 `tests/diagnostics/test_feed_continuous_scenarios.py`，实际调用负载函数，HTTP 边界用合成响应；证明两实例/四场景/两档并发/各三轮执行、游标与身份正确、正式判定拒绝缺轮/重复轮/空样本/短测以及坏推荐响应。合成响应的毫秒值不是性能成绩。新增行为测试先失败，再实施修复并通过。

真实本地 smoke 使用独立 SQLite、MiniRedis 和两个 API，执行全部 24 组、检查真实游标与缓存失效，退出 0；报告为 `component_smoke`、`release_gate_passed=false`。首次运行在推荐游标比较处失败，修复后重跑通过，未忽略断言。精简证据见 `docs/benchmarks/feed-scenario-matrix-fix-2026-11-08.json`，原始日志在忽略目录 `data/diagnostics/`。

鲁棒性加固：
1. 在 Compose 模式下，若指定 `--formal-dataset`，先行执行数据规模核验；若未达标直接拒绝，避免破坏性或无意义地调用种子装填。
2. 入参校验对齐底层矩阵要求，凡 `--rounds < 3` 均前置拒绝并产生标准失败报告，避免未捕获异常退出。
3. 循环热路径优化推荐场景 URL 查询解析，并为 `concurrency_10_stats` 提供安全回退。

本轮最终验证：100 项诊断测试全部通过，Go 常规全包测试通过（54 个依赖/显式启用测试事件 skip，未计入外部组件签收）；vet、build、前端语法检查通过。真实双 API 短测约 8.7 秒退出 0。实际重放非法入参与小数据正式模式均退出 1、正式标志为 false。

## 如何运行

使用项目 Go 1.27.2，确保 `/usr/local/go/bin` 在 PATH 中。

```sh
PATH=/usr/local/go/bin:$PATH python3 tools/task_timing.py run \
  --label feed-scenario-smoke \
  --report data/diagnostics/feed-scenario-smoke-timing.json \
  --heartbeat-seconds 15 --timeout-seconds 120 --quiet -- \
  python3 tests/integration/feed_warm_cache_continuous.py \
  --mode sqlite --warmup-seconds 0 --duration-seconds 0.2 --rounds 3 \
  --report data/diagnostics/feed-scenario-smoke.json
```

短测可以验证驱动和协议，不可用来签收生产性能。

正式配置必须由隔离 acceptance_stack 提供两个 API、真实 MySQL/Redis 及相应环境变量，并预先准备满足 TEST_STANDARD 的正式数据（现有 120 视频种子本身不够）。再以 `--mode compose --formal-dataset --warmup-seconds 30 --duration-seconds 60 --rounds 3` 执行，报告输出到新的路径。小数据会在负载执行前拒绝正式模式。

24 × (30 秒预热 + 60 秒测量) = **至少 2,160 秒，即约 36 分钟**，另加构建、夹具、基线请求与媒体失效检查。预先安排总超时（例如 2,700 秒，加上实际构建预算），保留心跳，禁止把旧版三场景的耗时作为当前上限。本轮未运行这项正式长测，G07/A04 不因此勾选完成。
