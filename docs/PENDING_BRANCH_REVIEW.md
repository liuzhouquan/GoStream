# 未合并分支核对与审查记录

日期：2026-11-05。固定主线基线：`87b1f598b23e8d54a498fda017640300ffb0b441`。

## 分支核对

执行 `git fetch origin --prune`，并核对 GitHub 全部 PR 的合并状态。开始核对时没有开放 PR。此前单独使用 `git branch --no-merged` 得到的清单包含已 squash 合并的旧引用，不能直接作为待合并队列。

| 遗留本地分支 | 证据与决定 |
| --- | --- |
| `fix/account-storage-errors` | PR #8 已合并，patch 等价，无新增改动 |
| `fix/owned-lookup-storage-errors` | PR #9 已合并，patch 等价 |
| `fix/web-action-counts` | PR #10 已合并，patch 等价 |
| `fix/web-stored-session-recovery` | PR #11 已合并，patch 等价 |
| `docs/p2-03-merge-record` | PR #12 已合并，patch 等价 |
| `fix/upload-completion-lease` | PR #13 的末提交与遗留 tip `ee7c745` 一致，已 squash 合并 |
| `feat/rabbitmq-notification-delivery` | PR #14 的末提交与 tip `c0b82c2` 一致，已 squash 合并 |
| `test/im-process-recovery` | PR #15 的末提交与 tip `c214de7` 一致，已 squash 合并 |
| `fix/group-commit-order` | PR #16 的末提交与 tip `2938459` 一致，已 squash 合并 |
| `feat/im-device-lifecycle` | 原审查已暂缓；其全局 `acked_at` 提升会损害新设备未读隔离，后续 v005 已实现逐设备范围压缩；保持暂缓，不合入旧实现 |
| `feat/openapi-spec` | 原审查已暂缓；后续契约修正、结构校验及真实响应/帧测试已覆盖其目标，不合入旧 Schema |

两个暂缓分支的原始理由见 [七分支审查](BRANCH_REVIEW.md)，后续实现见 [IM 设计](IM.md) 与 `internal/platform/openapi_validation_test.go`。

真正的新工作是从当前主线分出的 `chore/development-timing`（`0d1f2f0`）及工作区里的 Feed 缓存、真实进程恢复、暖缓存和 fanout 测试。将全部工作复制到独立 `review/pending-branches-20261105` worktree 审查；原工作区在审查过程中保持原状。

## Standards

依据 `AGENTS.md`、`DEVELOPMENT.md`、`TEST_STANDARD.md`。没有 P0；以下缺陷均已修正：

1. **P1，外部取消遗漏清理。** `tools/task_timing.py` 只发送同一进程组信号，而 `tests/integration/im_smoke.py` 会另起 Go session。原 runner 收到 SIGTERM 后跳过 finally。另有依赖创建后、标志赋值前，以及 Popen 已生成子进程、句柄尚未交给所有者时的取消窗口。修正为信号触发栈展开、创建前记录随机资源清理意图、幂等 SQL/容器删除、Popen 交接期间延后中断、清理阶段忽略二次中断。默认清理宽限期为30秒。
2. **P2，排空预算提前耗尽。** `feed_fanout_load_test.go` 的90秒 context 已消耗启动和约30秒写入时间，却继续用于承诺60秒的排空阶段。改为独立60秒排空 context 和10秒核对 context。
3. **P2，延迟计时止于响应头。** `feed_cache_load_test.go` 原先在 `client.Do` 返回后就记录耗时，遗漏正文传输。现计至正文全部读完；历史 JSON 标注原口径，不改写旧数值。
4. **P2，缓存响应断言不完整。** 原 helper 接受24个视频中的任意非空子集，且只记录候选 SQL 增量。现要求全部24条、无重复/外部记录，并显式断言零候选 SQL。真实 Redis 测试还增加 DB15 空库前置检查，避免清理已有数据。

Standards 轴初审4项，最严重为取消时遗漏独立会话/依赖清理；复审未发现剩余具体正确性问题。没有执行风格性重构。

## Spec

依据交付范围、A04 的缓存/排空不变量及故障用例清理要求。独立只读审查也确认上述4类偏差：取消后资源残留、60秒预算不足、响应正文耗时未统计、完整响应与零 SQL 声明未由断言保证。对应修正保持原有鉴权、哈希成本和验收门槛。Spec 轴4项，最严重为取消时清理失效；两个旧功能分支没有需要重新合并的独有正确实现。

两轴发现有所重合，不把计数相加当作独立缺陷数量。

## 验证

- `python3 -m unittest discover -s tests/diagnostics -p 'test_*.py' -v`：10项通过。独立会话、schema/user/Redis 创建取消及 Popen 交接取消均先在旧实现失败，再通过修正验证；这些回归的 OS 子进程/信号是真实的，Docker/Go 命令由 fixture 替代。
- 缓存响应 helper：旧行为下丢视频和漏算正文耗时两项均失败；恢复修正后通过。
- Go 1.27.2 `go test -json -count=1 ./...`：退出0，总41.733秒；需要显式环境的组件测试不由这条命令证明。
- `go vet ./...`、三项可执行程序构建、Web JS语法通过；59项 A04 Python helper 通过。
- 真实 Redis8.2.9 上两项跨实例缓存测试启用 race 通过；测试包5.501秒，含构建的监控命令14.293秒。
- 真实 MySQL8.4.11/Redis8.2.9 双 API 缓存故障恢复通过，runner30.449秒。
- 完整正文计时的暖缓存：两个场景各240请求、12并发、100%命中、零错误、零候选 SQL，全部24视频完整；Following P95 17.018ms，匿名推荐11.545ms，runner15.212秒。
- 持续 fanout：120条视频、320关注者、38,400条 inbox；写入29.784秒，采样峰值12个待处理 job，停写后1.772秒排空，120个 job 完成、无错误，3页包含全部120条且无重复；runner45.540秒。
- 最终 launcher 真实中断：生产 API 就绪后发送 SIGTERM，launcher退出143，API进程、临时 schema、用户、Redis容器均为0；验证命令9.454秒。

精确数据和环境见 [脱敏 JSON](benchmarks/pending-branch-review-2026-11-05.json)。原始日志仅在忽略的 `data/diagnostics/`。以上是单机有界场景，不能关闭20并发持续暖缓存性能、多主机容量和最终联合部署验收。

## 集成规则

只集成本轮修正后的真实新增工作；历史已合并分支和已被替代的暂缓分支不重复合入。PR 必须通过完整 CI（含全量 race、真实组件矩阵）后再合并。SIGKILL 或超出清理宽限期仍可能留下外部资源，监控器不会把强杀等同于清理成功。
