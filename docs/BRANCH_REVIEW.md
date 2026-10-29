# 七分支代码审核与集成记录

审核基线：`876b4b695af4468bad02f46df5ac3551140af767`。七分支均为此基线上的单提交。以 `git diff 876b4b6...<提交>` 和 `git log 876b4b6..<提交>` 固定审核范围。标准来自 `DEVELOPMENT.md`、`ARCHITECTURE.md`、`DELIVERY_SCOPE.md`、`TEST_STANDARD.md`；需求来自 A03/A06/A07/A12 和审计 P1-09/P1-10。Standards 与 Spec 两个独立审核轴均为只读，主 agent 负责复现、修正与集成验证。

## 分支决定

| 分支 | 固定提交 | 决定与理由 |
| --- | --- | --- |
| `fix/web-group-sync-ack` | `a8951e0` | 原样不通过；修正多页缺口、失败时 ACK、切群旧 ACK 和同步代次后纳入集成 |
| `fix/group-member-revocation-race` | `fac7417` | 原样不通过；ACK 缺少群锁且测试允许残留游标；修正并补事务双顺序回归后纳入集成 |
| `feat/im-device-lifecycle` | `da64f7b` | **暂缓**；回执压缩会让新设备丢失未读；并发注册窗口没有用户行锁。原代码和测试不进入主分支 |
| `test/rabbitmq-fault-boundary` | `2d1a068` | 一般进程重启/小批积压测试可以保留；撤回“精确窗口/持续积压已验收”声明，检查实际 SIGKILL 退出码后纳入集成 |
| `test/minio-outage-and-media-matrix` | `378e21a` | 文件头/权限/503 恢复用例可以保留；修正清理错误处理并限制验收声明后纳入集成 |
| `feat/openapi-spec` | `bfde3b1` | **暂缓**；多个真实响应字段错误，测试只覆盖路由名称/方法，不能宣称参数/Schema/帧契约完整；A12 仍部分通过 |
| `feat/im-group-history-semantics` | `57308d7` | 顺序生命周期测试及现有历史可见性说明纳入集成；移除不必要的第三方产品类比；并发权限由独立 MySQL 测试验证 |

以下两个轴保持各自结论；同一缺陷可以同时违反标准和规格，不把跨轴计数相加当作独立缺陷数量。

## Standards

共 7 项；最严重的是设备生命周期的未读隔离破坏。原候选源码证据如下，运行复现列于后文。

1. **P1，Web**，`a8951e0:web/app.js:333`：同步只读最新 100 条，失败后 `finally` 仍 ACK；违反错误路径不变量要求。应补页到已确认边界，并在失败时禁止 ACK。
2. **P1，Web**，`a8951e0:web/app.js:375`：ACK 回调不检查群 ID/加载代次；切群旧响应能改写新群游标。应在成功、失败和同步结束时检查上下文。
3. **P1，撤回**，`fac7417:internal/platform/groups.go:286`：ACK 内普通成员查询不能与删除串行化；测试 `group_member_race_test.go:382` 只在 404 时检查残留，允许 200 加孤儿游标。应共享群锁并无条件检查删除后状态。
4. **P1，生命周期**，`da64f7b:internal/platform/device.go:253`：事务外设备计数与注册未共用用户锁；全局 `acked_at` 改变新设备补读。应保留逐设备语义并补注册/回收交错测试。
5. **P2，RabbitMQ**，`2d1a068:tests/integration/rabbitmq_smoke.py:127`：杀进程不定位 confirm/commit/ACK 状态，五条离线事件不等于持续积压；文档违反实证要求。
6. **P2，MinIO**，`378e21a:internal/platform/media_matrix_test.go:24`：短文件头不能证明真实编解码；隐私用例未执行 MinIO/search；清理错误被忽略。应收窄声明、检查清理错误。
7. **P2，OpenAPI**，`bfde3b1:docs/openapi.yaml:231`：公开用户响应遗漏外层结构，通知 `is_read` 与实际 `read_at` 不符；路由名称测试无法发现。需真实响应校验。

未发现必须为了代码风格执行的大规模重构。

## Spec

共 4 项；最严重的是 OpenAPI 的错误契约及 A12 完成误报。

1. **P1，OpenAPI 响应错误**：A12 要求“API 参数、错误、鉴权、WebSocket 帧……与代码一致”。`openapi.yaml:642/665/690/713` 写 `liked/favorited`，实际 `social.go` 返回 `active`；`:242` 写扁平用户结构，实际 `accounts.go` 返回 `{user,video_count,follower_count,following_count}`；`:1437` 写 `last_seen`，实际为 `last_seen_at`。生成客户端会误读结果。
2. **P1，OpenAPI 验收不足**：`openapi_contract_test.go:23–73` 只解析 YAML 并检查版本/路由/方法；没有校验参数、Schema、引用、响应或 WebSocket 帧。`/api/ws` 只描述握手。不能将 A12 写成“通过”；应修正契约、执行结构校验和代表性响应测试。
3. **P1，RabbitMQ 需求尚未覆盖**：规格要求“确认发布、消费提交与 ACK 窗口做真实进程强杀”。HTTP follow 后直接 SIGKILL 可能在投递完成后才发生；五条静态积压不足以证明持续积压恢复。保留一般重启覆盖，但维持精确窗口和负载待验。
4. **P2，媒体证据范围**：WebM/MOV 样本仅容器头，未执行真实解码/FFmpeg/封面。隐私测试只覆盖本地 timeline/profile，不能声称 MinIO/search 已验证。原分支还用媒体“已验”替换了其他 Feed/broker/IM 待验风险，应保留这些范围。

## 实际复现与最小修正

### 群 ACK 撤回竞态

将原候选测试的“仅 404 才检查”改成“删除后无条件检查”，在真实 MySQL 两连接上得到：删除 HTTP 200，ACK HTTP 200，`resurrected GroupDeviceRead after completed removal: count=1`。设备 ACK 和 legacy ACK 现均在事务内先取得 `chat_groups` 行锁，再复核成员/写游标。最终三轮 race 验证 ACK 先取得锁及移除先提交两种顺序，并检查无残留游标。发送用例也增加有效 ACK ID 和 SQL 消息断言，修正空断言及错误路径空响应解引用。

### Web 多页缺口与请求失败

增强后的 Chromium 脚本在原 `a8951e0` 上失败：`multi-page live sync skipped rows; rendered=208`，场景为已有 108 条，错过后续消息，只收到 350 的提示。候选修正分页查询到此前验证边界，成功后才更新同步水位；失败保留未完成状态；ACK 回调检查群 ID 和加载代次。修复后验证 105 条初始分页、完整 1–350 显示、503 不推进 ACK、恢复重试、切群过时历史及 ACK 响应隔离。模拟 API 严格限制单页 100 条；这不是最终真实数据库浏览器验收。

### 设备回执压缩（暂缓）

顺序用例已复现：设备 A 注册并 ACK 一条消息，执行原分支压缩，再首次注册设备 B；B 的 `/api/messages/unread` 返回 `items:[]`。不需要依赖并发就能观察新设备语义被改变。压缩前原实现保留精确回执，B 应获得该消息。另有事务外设备计数与注册交错的代码风险，尚未做 MySQL 屏障复现。

复现源保存在 `tests/review/compaction_review_test.go.txt`，故意不属于主分支 Go 测试包。在独立 `da64f7b` worktree 复制为 `internal/platform/compaction_review_test.go` 后运行 `go test ./internal/platform -run '^TestReviewCompactionPreservesNewDeviceUnread$' -count=1`，预期失败。该用例使用临时 SQLite，不写演示数据。原分支完整回执压缩不可仅加锁后直接合并，必须先明确并保留新设备补读规则。

## 集成验证

环境：Go 1.27.2、WSL2 Linux x86_64、MySQL 8.4.11、Redis 8.2.9、RabbitMQ 4.3.7；MinIO 复用本机固定源码镜像 `gostream-minio:source-7aac2a2c5b7c` 启动独立临时容器。演示组件只提供 MySQL 管理连接创建随机测试库/用户，没有使用演示业务表。

- 增强群撤回测试三轮 race 通过；隔离 MySQL 全部 `TestMySQL*` 及真实 MinIO `TestMinIO*` race 矩阵通过（platform 30.853s），随机数据库/用户和 MinIO 容器删除。
- 增强 Chromium 群同步脚本通过全部场景；原 Web 分支经同脚本验证确实失败。
- RabbitMQ 七子场景 race 通过（50.44s 测试体），实际 API 一般 SIGKILL/重启、五条排空及运维 CLI 通过，临时库/用户/broker 删除。退出信号断言补强后另以 CI 结果为准。
- 真实 IM 双 API 故障矩阵通过（15.81s 测试体），包含 Redis 停机、SIGKILL、105 条历史/回放、逐设备 ACK；剩余测试进程/数据库/用户均为零。
- 最终全量普通测试（platform 13.989s）、全量 race（platform 127.689s）、vet、三个程序构建、Web 语法、既有计数/本地会话 Chromium 回归、文档链接和配置解析均通过；最新 CI/合并结果以集成 PR 最后提交为准。

这些时长是正确性测试耗时，不能当作延迟或吞吐数据。A03/A06/A07/A12 和最终部署验收仍部分通过。后续优先修订两个暂缓分支，再推进精确 broker 故障窗口、默认 presence 完整过期、真实编解码及最终两浏览器负载。
