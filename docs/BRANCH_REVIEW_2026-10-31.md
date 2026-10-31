# 三个收尾分支审核

固定主分支基线：`9f3f0d1`。三个差异各自只有一个提交，独立审核原始差异后，在 `review/three-delivery-branches` 合并并修正；原分支保留。

| 分支 | 固定候选 | 实际新增范围 |
| --- | --- | --- |
| test/broker-fault-boundaries | 6f83e23 | broker 接收/重复投递/毒消息组件测试和 API worker 夹具 |
| perf/feed-hybrid-optimization | 4e5252a | hybrid 读取去掉 EXISTS、SQLite benchmark、MinIO TCP 断连与头像权限回归 |
| test/final-delivery-acceptance | 2fd4517 | SQLite 关闭连接池后的 liveness/readiness 回归及文档段落 |

这份记录不是最终交付通过声明。所有 A01–A12 仍按 [测试标准](TEST_STANDARD.md) 的实际证据判定。本轮只有混合 Feed 查询的少量生产改动，其余为测试、脚本及文档。

## Standards

独立只读审核原始三个差异（未运行测试）报告以下问题。P1/P2 为本轮修复优先级，测试误失败/证据缺口不等于已发现生产数据丢失。

| 级别/原始位置 | 证据与影响 | 最小修正 |
| --- | --- | --- |
| P1 broker：rabbitmq_smoke.py 原207–210行 | 活跃 worker 可先领取刚插入的 source，创建 delivery；脚本再插同 key，存在重复键竞态。代码确认，未在本机真实 broker 复现 | source/可选 delivery 在同一事务准备，source 已标记发送；管理 API 作为显式测试生产者，不与 SQL 发布者争抢 |
| P2 broker：原233–251行及 Go mixed 用例 | 同一关注关系重复10次最多一个通知；DLQ 深度/前缀计数可能把重复帧当不同消息，发布 routed 响应被忽略 | 并发发布独立有效事件/毒消息；每个有效 delivery 检查完成和唯一性，检查全部精确毒消息身份/原因/正文、CLI replayed=3 |
| P2 broker：原434–448行 | 新 AMQP 会话未立即注册清理；close() 先正常 AMQP 关闭，不能称进程 SIGKILL | 立即 defer 回收；直接断底层 TCP、检查 Redelivered/相同 ID/正文，最后确认消费 ACK 后空队列；文档保留真实进程窗口待验 |
| P2 Feed：feed_bench_test.go 原98–108行 | GORM default:true 会替换 false；关注时间晚于全部视频。HTTP200不足以证明夹具和返回页，SQL 池未回收 | 显式更新/检查 false，关注早于发布；计时前后核对20个精确 ID、游标和更多页标志，关闭池 |
| P2 媒体：minio_outage_test.go 原197–238行 | hijack 失败的 HTTP502 fallback 也可通过；恢复仅检查 complete200 | 实际 TCP 关闭计数、严格503、失败分片零行、completed 会话、唯一资产与对象字节检查 |
| 证据范围：finaldelivery | 唯一新执行路径是 SQLite pool.Close，不是最终全组件配置 | 测试命名及文档限定到 SQL 探针；不扩大 A01/A09/A12 通过范围 |

未发现需要为风格进行大规模重构的依据。原缺陷已在集成候选修正；修正后的独立复核及真实组件结果在下方补充。

## Spec

独立只读需求审核初稿报告两项核心证据问题：broker 分支未实现它声称的精确进程强杀窗口；混合流量不是十个独立事件。另指出 Feed 没有完整 MySQL P95 证明、TCP 恢复断言宽松、最终交付措辞超过 SQLite/代表性契约的执行范围。原始审核未运行测试。

修正后的范围：

- AMQP TCP 中断发生在生产持久接收方法提交后、未 ACK 前，检查真实 Redelivered 和同一身份；这仍是组件连接故障，不是生产 API 精确窗口强杀。
- Python 的预提交 delivery 夹具只证明去重。原暂停 broker 后观察 SQL claim 的 SIGKILL 用例仍只证明一般重启/租约恢复，未观测进入 confirm wait 的具体时刻。
- Go 混合用例验证 durable accept；Python 混合用例额外逐 ID 验证真实 API worker 的 delivery 完成。毒消息全 ID 集合和拒绝原因、运维重投数量必须一致。
- SQLite 单连接并发调用不是 MySQL 并发事务证据；正常/race 的本地通过不代替真实 MySQL/RabbitMQ/MinIO。
- 删除预查询只减少活跃读者一次往返；零关注者由便宜 EXISTS 改为 UNION，成本收益须实测。500 视频 SQLite 模式比较不证明 20% P95 改善，默认仍为 SQL-only。
- `TestLivenessSurvivesClosedSQLPool` 没有 broker/Redis/MinIO；已有 OpenAPI 路由映射及代表性响应校验不表示所有49路由的所有参数/响应变体都经过验证；已有浏览器工作流是 SQLite/FFmpeg，最终联合 Compose 待验。

精确进程窗口、满 DLQ、持续流量及最终配置继续开放，没有将未实现工作改成通过。

## 实际验证

本机 Go 1.27.2，Docker WSL 集成不可用。原分支合并的全量普通测试通过（platform17.152s）；它不能证明外部依赖分支已执行。修正后验证记录及最终候选 CI 待补。

已实际复现的 benchmark 夹具问题：加入真实存储标志检查后，原夹具失败，输出 `completed-push fixture has 500 pull rows`。显式写 false、修正关注时间并检查精确页面后，`go test ./internal/platform -run '^$' -bench '^BenchmarkFollowingFeed$' -benchtime=100x -count=3` 通过：SQL-only 440067/428639/406127 ns/op；Hybrid 866695/825537/789622 ns/op。环境为本地 Ryzen 7 5800H / Go1.27.2 / SQLite 内存库 / 500 视频。不是优化前后对照，没有 MySQL、网络或 P95 数据，不能作为性能收益。

broker 管理 API 的批量检查使用 ack_requeue_true 保留毒消息供 CLI：RabbitMQ4.3.7 的 [管理实现](https://github.com/rabbitmq/rabbitmq-server/blob/v4.3.7/deps/rabbitmq_management/src/rabbit_mgmt_wm_queue_get.erl) 先取整批再重新入队；测试仍需实际检查完整 ID，不能只相信该机制。

## 是否已经到尾声

主干业务和大部分工程机制已有实现，阶段上属于验收收尾；仍有需要编码、排错和实测的工作，不是只整理文档即可完成。上述三个分支也未提交最终联合部署证据。

本版剩余工作按三个出口集中推进：

1. **Feed 与一致性负载**：按标准数据量和运行时长完成真实 MySQL/Redis 三轮对照与混合流量；达到20%关注 Feed P95收益并核对计数/积压，补 fanout 实际进程故障。没有收益时继续定位和修改实现，不能把“少一次查询”当作达标。
2. **故障与媒体完整链路**：确定性的发布/消费进程窗口、满 DLQ/持续积压；10个真实短视频、实际可解码格式/转码封面和明确终态，记录资源与恢复结果。头部字节样本不代替媒体处理。
3. **最终交付冻结**：最终 MySQL/Redis/RabbitMQ/MinIO 双实例 Compose，两浏览器走注册、断点续传、Feed/互动/搜索、通知和聊天，重启后核对持久状态；补足权限/接口变体及运维证据，README从空环境复验、文档与本地学习指南对齐。

群聊扇出/长稳态和较大搜索数据集按原 A07/A08 继续补证。多节点 RabbitMQ 集群、互联网规模容量、机器学习推荐、移动端和强行微服务拆分不是本次交付新增前提。没有基于分支名、测试数量或代码行数计算完成百分比。
