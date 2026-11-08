# 第二轮验收复核（2026-11-08）

**结论：有实质改进，尚未改进完成，不能全量签收。**

候选 `2ef80fd1a4a61b946b1cda8f0cdef214c9c332fc`，对照上轮 `0c897af` 和 [上轮发现](ACCEPTANCE_RECHECK_2026-11-08.md)。本轮检查六个修复/文档提交，工作区起始干净；未修改业务代码、合并或发布，仅新增本报告及 [脱敏验证记录](benchmarks/acceptance-recheck-second-2026-11-08.json)。

## 实际验证

| 命令/检查 | 本轮结果 |
| --- | --- |
| Go 1.27.2 `go test -json -count=1 ./...` | 退出 0，41.720 秒；外部依赖/显式启用测试存在 skip，数量见 JSON，不计为正式验收通过 |
| `python3 -m unittest discover -s tests/diagnostics -p 'test_*.py' -v` | 87 项通过，36.510 秒（包装器耗时） |
| `go vet ./...`、三个命令入口 `go build`、`node --check web/app.js` | 均退出 0 |
| 重放上轮零时间/零轮 Feed 命令 | 退出 1，0.151 秒；FAIL / rejected_invalid_parameters / release_gate_passed=false，原反例已阻断 |
| SQLite Feed 短测，预热 0 秒、测量 0.2 秒、三轮 | 退出 0，6.219 秒；PASS / component_smoke / release_gate_passed=false，正常短测不冒充正式签收 |
| 两个种子工具的特殊字符虚构口令测试 | 两个工具均退出 1，均仍输出完整虚构口令，见 Standards S1 |
| `git fetch origin --prune` 与版本对照 | origin/main 比候选少 43 个提交；未将本地结果视为最终远程 CI |

长命令均通过 `tools/task_timing.py` 设置超时并跟踪到退出；原始日志在忽略的 `data/diagnostics/`。未重跑完整 Compose 故障矩阵、正式规模压测、浏览器/媒体矩阵或全量 race；本轮不能据常规测试推断这些门槛通过。

## Standards（规范与可靠性轴）

### S1 · P1 · R4 脱敏仍有可复现的凭据泄漏

位置：`tools/seed_feed_benchmark.go:168`、`tools/seed_group_im_benchmark.go:163`。

`sanitizeDSN` 将任意位置的 `://` 当作协议前缀。口令包含它时，后续分隔解析失败，返回原始 DSN。

本轮分别实际执行（全为虚构数据，无真实数据库/凭据）：

```sh
/usr/local/go/bin/go run ./tools/seed_feed_benchmark.go -driver mysql \
  -dsn 'fake_user:sentinel_secret://tail@tcp(127.0.0.1:99999)/gostream'
/usr/local/go/bin/go run ./tools/seed_group_im_benchmark.go -driver mysql \
  -dsn 'fake_user:sentinel_secret://tail@tcp(127.0.0.1:99999)/gostream'
```

两者输出均含完整口令 `sentinel_secret://tail`。已有简单口令测试通过，无法覆盖此边界。违反 SDD 的“私有错误不泄漏凭据”要求。最小修复：不记录 DSN，或用驱动解析后只输出安全字段，同时审查错误字符串；加入特殊字符密码回归。

### S2 · P2 · 新测试未实际覆盖所声称的关键路径

- `tests/diagnostics/test_group_im.py:112` 复制 SQL 去重循环，并未调用被修复的验收函数。缺失核验器再次被误标成功时，该测试仍可通过。
- `test_feed_payload_validation.py:49` 在基线请求返回 500 时抛错，没有验证“测量窗口无样本”时返回的延迟或最终 gate。
- `test_component_outage.py:83` 断言源码字符串包含 finally，并未注入失败验证恢复动作。

最小修复：调用真实判定函数，通过可控依赖模拟覆盖缺核验器、异常核验回调、空样本、短测范围和故障恢复调用。不要复制实现来充当回归测试。

### S3 · P2 · R3 普通断言恢复改善，初始状态和中断窗口仍未闭环

位置：`component_outage_smoke.py:428`、`:510`、`:579`、`:764`。

三个依赖的正常断言失败后已进入 finally 恢复；但 stop/sleep 仍在 try 外，独立入口最终无条件启动三组件，没有记录原始运行状态。它可能改变既有目标栈的状态；未运行破坏性停机复现。本轮仅源码确认。

最小修复：记录资源归属和初始状态，将停机动作置于保护范围；仅恢复本轮改变的状态，并用行为测试覆盖 stop 返回异常和中断。

## Spec（需求与证据轴）

### Q1 · P1 · R1/R5 仅部分关闭：正式 Feed 判定仍不满足规格

位置：`feed_warm_cache_continuous.py:683`；种子工具 `seed_feed_benchmark.go:78`、`:119`。

`TEST_STANDARD.md:111` 要求最低 2,000 用户、20,000 视频、10,000 关注、50,000 互动；`:115` 要求关注首页/深页、登录推荐、热榜，各组重复三次。

正式判定仍只检查 Compose、预热/测量时间、总轮数及 checkpoint，没有数据规模和场景覆盖校验。夹具仍为 10 作者/120 视频，默认三轮仍分别分给三个场景。**原零负载反例已修复，但不完整的 Compose 小夹具仍可能获得正式标志。** 本条依据源码，不声称本轮已重跑正式长测。

最小修复：未满足完整规格前始终标 smoke；正式模式验证真实行数、完整场景及逐场景重复次数，然后再运行正式压测。

### Q2 · P1 · R6 撤回签收不完整，任务入口仍互相矛盾

SDD 顶部与 README 已正确改为 RC1/待补证，Feed 延迟也已对齐；但：

- `docs/SDD.md:125`、`:134`–`:136` 仍勾选生产规模和完整场景已完成。
- `docs/tasks/G12.md:6`、`:11`–`:12`、`:75`–`:80` 仍声明“已验收”、PR #71、v1.0.0、全部同版证据和正式签收。

这违反 `SDD.md:65` 的“只有证据满足退出条件才能勾选”。其他 agent 继续按这些明细会再次过早签收。最小修复：同步 SDD 明细和任务报告，待补项恢复未完成；旧声明明确标为已撤回，不能只修改顶部摘要。

### Q3 · P2 · R7 媒体覆盖仍有超出真实矩阵的描述

`RELEASE_ACCEPTANCE.md:28` 仍写“纯音频提取”，实际 `media_matrix_smoke.py` 的十个夹具不包含纯音频输入。浏览器路径与 24 项检查计数已纠正，但媒体覆盖仍需按真实夹具逐项对应。

## 上轮问题闭环情况与下一步

| 上轮问题 | 当前判断 |
| --- | --- |
| R1 零负载误报正式通过 | 原反例已实测阻断；正式完整性仍待修 Q1 |
| R2 SQL 核验跳过却成功 | 源码已按实际核验设置 checkpoint；Compose 自动 MySQL 核验器已接通；缺关键行为回归 S2 |
| R3 异常后组件未恢复 | 普通断言路径已改善；初始状态/中断与行为验证待补 S3 |
| R4 DSN 泄漏 | 简单口令已修，特殊字符反例仍实测失败 S1 |
| R5 数据规模与场景 | 未补齐 Q1；不是仅写文档即可关闭 |
| R6 最终证据与签收 | 摘要改善，任务/明细仍矛盾 Q2，最终 CI 与生产规模证据待补 |
| R7 文档描述错误 | 部分纠正，仍有 Q3 |

建议先提交小型修复：S1 凭据日志 → Q1 正式 gate 与 S2 行为回归 → S3 资源恢复 → Q2/Q3 全入口文档一致性。修复后再运行规定规模 Feed、查询计划、群聊、真实组件故障和恢复量测，将最终候选 SHA 与 CI 关联。不要以“87 项诊断通过”代替发布验收。

规范轴 3 项（最高 P1）；需求轴 3 项（最高 P1）。本轮未新增业务功能，未声称这些未验证的正式门槛已经通过。
