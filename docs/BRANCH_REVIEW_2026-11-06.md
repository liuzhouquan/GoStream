# 未合并分支、故障脚本与 SDD 审查

日期：2026-11-06。固定主线：`9bf2bff`（PR #58）。执行 `git fetch origin --prune`，核对全部本地/远程非祖先分支及 GitHub merged/open PR；开始时没有开放 PR。Standards/Spec 两条独立审查轴读取当前工作区与未跟踪源码；未将旧的本地通过记录当作本轮最后代码的通过记录。

## 分支决定

| 非祖先引用 | tip | 实际状态/处理 |
| --- | --- | --- |
| `fix/account-storage-errors` | `3dad1d6` | PR #8 已合并，记录的 head 与本地 tip 一致，无新增工作 |
| `fix/owned-lookup-storage-errors` | `f1203b4` | PR #9 已合并，head 一致 |
| `fix/web-action-counts` | `9c05505` | PR #10 已合并，head 一致 |
| `fix/web-stored-session-recovery` | `fbaed77` | PR #11 已合并，head 一致 |
| `docs/p2-03-merge-record` | `4ad9fd9` | PR #12 已合并，head 一致 |
| `fix/upload-completion-lease` | `ee7c745` | PR #13 已合并，head 一致 |
| `feat/rabbitmq-notification-delivery` | `c0b82c2` | PR #14 已合并，head 一致 |
| `test/im-process-recovery` | `c214de7` | PR #15 已合并，head 一致 |
| `fix/group-commit-order` | `2938459` | PR #16 已合并，head 一致 |
| `feat/im-device-lifecycle` | `da64f7b` | 原方案破坏新设备未读隔离，已被 v005 逐设备范围实现替代；不合入旧实现 |
| `feat/openapi-spec` | `bfde3b1` | 原 Schema 与真实响应不符，已被修订契约/真实响应校验替代；不合入旧实现 |

前九个引用是 squash 历史导致的非祖先，不能再次直接 merge；两个暂缓原案的证据见 [七分支审核](BRANCH_REVIEW.md)。其他本地/远程引用已经是当前主线祖先。保留旧引用供历史追溯，无需通过删除引用伪造“全部完成”。

真正新增的是 `test/rabbitmq-consumer-preack-process-kill` 工作区未提交的 RabbitMQ 地址切换/精确窗口/持续输入/DLQ/集群与 MinIO 续传恢复，以及本轮 SDD 和审查修复。仅将这些有效差异纳入集成 PR。

## Standards

依据 AGENTS、DEVELOPMENT、TEST_STANDARD 和资源所有权规则，初审发现 2 项 P1，无 P0：

1. **pre-ACK 故障入口失效。** `tests/integration/rabbitmq_smoke.py` 为每个 API 设置 `RABBITMQ_URLS`，但故障场景只把 `RABBITMQ_URL` 改为 ACK proxy；`cmd/gostream/main.go` 优先 URLS，使实际 API 绕过代理。修复同时设置两个变量，恢复时还原完整列表；真实 smoke 必须观察代理实际截获 basic.ack 后才强杀，不能删除 gate 等待。
2. **外部取消遗漏资源。** runner 缺少 SIGTERM 栈展开，创建后才登记所有权，API/SQL-lock Popen 交接有窗口。最初沿用 IM 的 `InterruptedError` 处理后，fixture回归通过，但真实取消在30秒内没有退出。缩小为1秒 `subprocess.run(capture_output=True)` 后确认：标准库 selector会把这个OSError子类当EINTR吞掉，收到SIGTERM仍exit0。最终改为共享的 `LauncherInterrupted(Exception)`，避免系统调用重试逻辑吞掉信号；三个launcher共用信号/交接机制，创建前登记、幂等清理，清理期忽略二次信号。须核对最终 schema/user/broker/network/API 均为零。

主审补查同样扩展过的 `minio_outage_smoke.py`：原 SIGTERM 会跳过 finally，可能使 MinIO 保持停止。真实信号回归先复现 `-15` 退出且未恢复；现在复用共享launcher取消栈展开，停止前登记恢复意图，恢复阶段忽略二次信号。它只恢复其明确操作的隔离 Compose MinIO，不清理其他项目。

未进行风格性重构。Standards 初审2项；MinIO同类缺陷另由主审补充。Mock Docker 的信号回归只证明控制流/子进程所有权，不代表真实容器行为。

## Spec

独立 Spec 审查也确认上述 RabbitMQ 2 项偏差：故障代理没有实际控制生产 ACK；取消后未满足 TEST_STANDARD 的结束清理要求。source/delivery 精确身份、DLQ原始消息/死亡元数据、follower/leader/majority 的断言与已声明边界相符。

SDD 单独复核确认：G01–G12 依赖无环，保留10/20并发、30秒预热、60秒实测、三轮及P95/P99原门槛；已有小夹具不关闭持续负载。已按建议把 G09 媒体子项明确依赖 G04 的实际验收。长期soak/多主机容量没有新增为发布前置，未验证限制保留。

两轴初审各2项，最严重均为故障场景失效和取消后资源残留；重合项不累加为独立缺陷数量。

## 本轮验证与未关闭边界

- Go 1.27.2 `go test -json -count=1 ./...`：退出0，计时27.613秒；无环境的外部用例可能skip。
- `go vet ./...`、三个可执行程序构建、`node --check web/app.js`、`git diff --check` 已执行并成功。
- 最终 `tests/diagnostics`：15个测试方法通过，4.744秒；RabbitMQ回归覆盖9种真实信号/子进程场景，Docker/SQL由fixture替代。旧行为先复现7项失败；新增真实capture_output等待场景在IM/Rabbit旧实现均exit0而非143，修正后通过；MinIO恢复回归也先失败后通过。
- ACK proxy独立网络回归：1项通过，0.226秒。
- ACK环境切换修复后的真实 RabbitMQ 完整 smoke：exit0，173.992秒，两个精确进程强杀窗口、300条持续输入（29.138秒）、末批后1.328秒排空、DLQ限容恢复和运维回放通过，schema/user/broker/network/API均零残留。该运行先于后续共享取消异常修正；最后代码由CI再跑同一完整入口。
- 真实取消首次30秒超时，已清理此次明确创建的 `fcbf97e490` 隔离资源；最小复现证实异常类型问题。共享异常修正后重跑相同真实MySQL/broker创建后SIGTERM路径：exit143，信号后2.206秒完成，schema/user/broker/network/API全为零。原始日志在忽略的 `data/diagnostics/rabbitmq-real-interrupt-fixed-20261106.log`。

已存在的三节点本地 follower/leader/majority 证据见 RABBITMQ；本轮不因为新增文档重跑本地集群，最终代码由 CI 的三个场景复验。MinIO此前17.513秒双API/MySQL真实停服续传结果是局部能力证据；本轮新增信号回归不等同再次运行整个对象栈。

SDD列出尚未完成的最终联合配置、20并发持续暖缓存、群扇出、十视频真实格式、生产搜索规模、恢复成本和契约收口。没有将本次分支合并等同于 `v1.0-demo` 完成。最终 PR 必须等待其最后提交的完整 CI 后合并。
