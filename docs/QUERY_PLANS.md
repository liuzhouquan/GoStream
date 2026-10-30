# 查询计划探针及证据边界

## 可运行入口

SQLite：`go test ./internal/platform -run '^TestQueryPlansSQLite$' -count=1 -v`。

MySQL：将 `GOSTREAM_TEST_MYSQL_DSN` 指向已迁移的隔离库，执行 `go test ./internal/platform -run '^TestMySQLQueryPlans$' -count=1 -v`。MySQL CI 的 `^TestMySQL` 会执行该用例。它在一个最终回滚的事务内创建两用户，每张通知、私信、视频、互动、评论、Feed inbox 表 500 行；结果从当前 MySQL 实际 `EXPLAIN` 读取并打印索引、估算扫描行和 Extra。另用 `FORCE INDEX` 验证声明索引可用及其排序能力，并核对每个查询的精确行 ID/顺序或计数。优化器可以在该分布上选择 PRIMARY/其他有效路径，不以索引名称猜测生产缺陷。v001 时间线索引没有显式 `id DESC`，其完整二元排序可能需要 filesort，此成本仍待实际规模优化。最初分支只有一个用户和空业务表，本次已修正。

这些查询是有界索引探针。私信探针只查单向关系，未覆盖生产双向 OR；Feed 探针也不等于完整推拉 UNION、同时间戳谓词和互动状态加载。它们不能证明生产查询无 filesort、吞吐恒定或较大规模验收通过。

## 探针范围

| 路径类型 | 检查目的 | 仍缺少的生产验证 |
| --- | --- | --- |
| 通知页/未读计数 | 用户边界、ID 页和读状态索引 | 多用户规模、bounded mark-read 与真实分页查询采集 |
| 私信历史/未读 | 会话与接收者索引 | 双向 OR、设备 receipt anti-join 的完整 SQL |
| 视频时间线/作者页 | 状态与时间排序索引 | 同时间戳、cursor 精确谓词及批量作者/互动加载 |
| 互动最近记录/评论根页 | 用户/线程索引 | 复杂回复分页和多用户热点负载 |
| Feed inbox | 用户/时间范围索引 | 完整 following 推拉、unfinished job 与 late follow |
| 用户子串搜索 | ID 分页访问路径 | 前置通配符的真实扫描成本、视频搜索及更大数据集 |

没有将预期计划表当作已经运行的 MySQL 结果。准确输出见对应提交的 CI 日志；索引检查失败时应保留真实输出，检查数据分布和优化器选择，再决定改查询、索引还是测试条件。

A08 仍部分通过；现有 v001 索引没有因这些探针自动删除、增加或宣称“冗余”。所有索引迁移都需要真实请求 SQL 和可复现的改进数据。
