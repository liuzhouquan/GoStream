# 开发入口与工作包验收

## 先读哪些文档

| 要解决的问题 | 事实来源 |
| --- | --- |
| 最后要交付什么，哪些事情不做 | [交付范围](DELIVERY_SCOPE.md) |
| 怎样才算通过，依赖哪些真实组件 | [测试标准](TEST_STANDARD.md) 的 A01–A12 及性能门槛 |
| 现在有哪些缺陷，优先修哪个 | [代码审计](code-audit.md) 的当前状态和最新问题 |
| 已经运行了什么，结果是什么 | [进度记录](../PROGRESS.md)、[性能记录](PERFORMANCE.md) |
| 请求/事务/后台任务如何连接 | [架构](ARCHITECTURE.md)、[API](API.md)、[数据库](DATABASE.md)及模块设计 |
| 按什么顺序读源码 | [学习指南](LEARNING_GUIDE.md) |

进度里的勾选表示局部工作完成。只有测试标准对应项的证据齐全，才能将该项从“部分通过”改成“通过”。历史记录保留原始基线和失败，最新状态放在阅读入口，不将旧结果当作当前提交的验证。

当前十分支审核见 [2026-10-30 审核记录](BRANCH_REVIEW_2026-10-30.md)；此前七分支的历史决定保留在 [BRANCH_REVIEW.md](BRANCH_REVIEW.md)。新的 presence 分支未引入被否决的全局回执压缩；OpenAPI 修订已加入真正的结构及代表性响应/帧校验，完整交付仍以测试矩阵为准。

当前 A07 工作包见 [逐设备回执压缩](DEVICE_ACK_RANGES.md)：v005 增量迁移、精确 ACK 语义、事务与重启验证，以及仍未关闭的规模/交付条件。历史分支复审结论不代替当前实现状态。

当前后续工作包：[稀疏回执查询测量](DEVICE_ACK_QUERY.md)。明确区分 SQL 样本、HTTP 压测及 A07 两实例负载，不以其中一种替代其他验收。

## 一次工作包怎么推进

1. 从审计中选择一个可复现的问题，或从验收矩阵选一个未通过的条件；写清受影响路径、不变量与退出条件。
2. 读入口、事务、异步回调和调用方。跨实例一致性必须查真实 SQL/Redis/broker 行为；单进程 race 不能证明数据库事务顺序。
3. 缺陷先建立能失败的回归。故障注入只放在测试中；记录原代码失败和修复后结果，不删除断言来通过测试。
4. 小范围改动后跑相关测试和下列基本检查；涉及真实依赖时执行对应脚本/CI 任务。资源隔离和清理也属于通过条件。
5. 更新实际改变的 API/设计契约、验收状态、审计及进度；PR 写明故障触发、修复机制、实际验证及未覆盖范围。合并依据必须对应最后提交的 CI。

## 基本检查与真实组件入口

使用 `go.mod` 指定版本；本机 Go 在 `/usr/local/go/bin/go`。版本不匹配时先解决环境，不自动降级或改模块版本。

```sh
export PATH=/usr/local/go/bin:$PATH
go version
go test -count=1 ./...
go test -race -count=1 ./...
go vet ./...
go build ./cmd/gostream ./cmd/gostream-media ./cmd/gostream-events
node --check web/app.js
git diff --check
```

普通测试可能跳过未配置的外部依赖分支。真实 MySQL 测试需要 `GOSTREAM_TEST_MYSQL_DSN` 指向专用隔离库，先运行 `go test ./internal/migrations -run '^TestApplyMySQLIntegration$' -count=1`，再运行目标 `TestMySQL*`；CI 的 `mysql-integration` 提供完整安装/升级/恢复环境。不要将演示数据库的 DSN 用于这些用例。

| 改动范围 | 额外验证入口 |
| --- | --- |
| 群消息提交顺序 | `go test -race ./internal/platform -run '^TestMySQLGroupCommitOrder$' -count=3 -v`，隔离且已迁移的 MySQL |
| IM 进程/Redis 恢复 | [IM](IM.md) 中 `im_smoke.py`；脚本自动隔离 SQL/Redis/子进程 |
| RabbitMQ/outbox | [RabbitMQ](RABBITMQ.md) 中 `rabbitmq_smoke.py` |
| 分片/MinIO/FFmpeg | [测试标准](TEST_STANDARD.md) 和 [媒体运维](MEDIA_OPERATIONS.md) 中对应矩阵 |
| Web 状态与交互 | 目标浏览器回归；涉及持久状态再用真实 API 验证，模拟响应不能替代端到端结果 |

已有八项 CI：质量、MySQL、Redis、MinIO、RabbitMQ、视频恢复、IM 恢复及浏览器工作流。相关代码改变时按实际依赖检查，不把仅解析 Compose YAML 当作已部署验收。

本地学习笔记 `docs/LOCAL_STUDY_GUIDE.md` 保持在 Git 排除列表中，不加入 PR。提交前检查 `git check-ignore -v docs/LOCAL_STUDY_GUIDE.md` 及暂存文件列表。
