# 查询计划探针及证据边界

## 可运行入口

SQLite：`go test ./internal/platform -run '^TestQueryPlansSQLite$' -count=1 -v`。

MySQL：将 `GOSTREAM_TEST_MYSQL_DSN` 指向已迁移的隔离库，执行 `go test ./internal/platform -run '^TestMySQLQueryPlans$' -count=1 -v`。MySQL CI 的 `^TestMySQL` 会执行该用例。它在一个最终回滚的事务内创建两用户，每张通知、私信、视频、互动、评论、Feed inbox 表 500 行；结果从当前 MySQL 实际 `EXPLAIN` 读取并打印索引、估算扫描行和 Extra。另用 `FORCE INDEX` 验证声明索引可用及其排序能力，并核对每个查询的精确行 ID/顺序或计数。优化器可以在该分布上选择 PRIMARY/其他有效路径，不以索引名称猜测生产缺陷。v001 时间线索引没有显式 `id DESC`，其完整二元排序可能需要 filesort，此成本仍待实际规模优化。最初分支只有一个用户和空业务表，本次已修正。

生产规模（2,000 用户 / 20,000 视频 / 1,000 通知）：
- Go 原生集成：`go test -v ./internal/platform -run '^TestProductionScaleSearchAndNotificationQueries$' -count=1`
- 跨实例验收驱动：`python3 tests/integration/search_query_smoke.py --scale production`

## 探针范围

| 路径类型 | 检查目的 | 验证结论与生产证据 |
| --- | --- | --- |
| 通知页/未读计数 | 用户边界、ID 页和读状态索引 | 已通过 G06 生产规模实测（1,000 项全量分页无漏无重，覆盖索引避免回表） |
| 私信历史/未读 | 会话与接收者索引 | 双向 OR、设备 receipt anti-join 的完整 SQL |
| 视频时间线/作者页 | 状态与时间排序索引 | 同时间戳、cursor 精确谓词及批量作者/互动加载 |
| 互动最近记录/评论根页 | 用户/线程索引 | 复杂回复分页和多用户热点负载 |
| Feed inbox | 用户/时间范围索引 | 完整 following 推拉、unfinished job 与 late follow |
| 用户子串搜索 | ID 分页访问路径 | 前置通配符真实扫描成本及 20,000 视频实测延迟 |

---

## G06历史SQLite样本观察（不能关闭A08）

下列数字来自G06原任务报告的SQLite样本，未在本轮独立复验原始结果，也不能作为MySQL生产查询计划或容量结论。数据量标签为2,000用户、20,000视频和单用户1,000通知；生产handler的SQL/索引、扫描/排序成本必须在真实MySQL另行采集。

### 1. 真实 SQL 执行计划与索引使用核验

| 业务场景 | 真实 Handler SQL | EXPLAIN 路径 / 索引 | Extra / 排序开销 | 实测单次延迟 (SQLite) |
|---|---|---|---|---|
| **通知 Keyset 分页** | `SELECT * FROM notifications WHERE user_id = ? AND id < ? ORDER BY id DESC LIMIT 31` | `idx_notification_user_id (user_id, id DESC)` | 无 filesort，利用索引顺序反向扫描 | 0.58 ~ 1.05 ms |
| **通知未读计数** | `SELECT COUNT(*) FROM notifications WHERE user_id = ? AND read_at IS NULL` | `idx_notification_unread (user_id, read_at, id DESC)` | 覆盖索引扫描（Covering index），不回主表 | 0.11 ~ 0.38 ms |
| **有界已读标记** | `UPDATE notifications SET read_at = ? WHERE user_id = ? AND read_at IS NULL AND id <= ?` | `idx_notification_unread` 过滤更新 | 范围锁定与更新，不干扰未读上限外记录 | 0.20 ~ 1.10 ms |
| **用户子串搜索** | `SELECT * FROM users WHERE (username LIKE ? ESCAPE '!' OR nickname LIKE ? ESCAPE '!') AND id < ? ORDER BY id DESC LIMIT 21` | `INTEGER PRIMARY KEY (rowid < ?)` | 反向主键遍历 + 表达式过滤，无临时排序 | 0.26 ~ 1.11 ms |
| **视频子串搜索 (浅页)** | `SELECT * FROM videos WHERE status = ? AND (title LIKE ? ESCAPE '!' OR description LIKE ? ESCAPE '!') AND id < ? ORDER BY id DESC LIMIT 21` | `idx_video_feed (status = 'published')` | 状态过滤 + 内存排序（20 行） | 8.5 ~ 10.0 ms |
| **视频子串搜索 (深页)** | 同上，带 `videos_before_id = 10000` | `idx_video_feed (status = 'published')` | 范围收敛，扫描行数更少 | 5.3 ~ 7.9 ms |

### 2. 前置通配符 LIKE 的成本分析与适用规模边界

由于业务支持任意子串模糊检索（`%query%`），B-Tree 索引由于前置 `%` 无法进行索引范围定位（Index Range Seek），必须依靠已过滤状态（如 `status = 'published'`）或主键范围执行行扫描并逐行计算 LIKE 谓词。

**实测延迟数据（20,000 视频样本集，连续 50 次请求）**：
- **P50 耗时**：8.50 ms
- **P95 耗时**：9.75 ms
- **Max 耗时**：13.06 ms
- **深页 vs 浅页**：由于采用 Keyset 游标（`id < videos_before_id`），深页请求能迅速在主键范围上短路，实测深页耗时（~6.2 ms）低于全集浅页（~9.7 ms），完全避免了传统 `OFFSET` 导致的深页二次倍增恶化。

**适用边界**：原“10万以内推荐生产方案、10万至50万为搜索演进拐点”的数字没有对应MySQL/规模测量，现撤回。SQLite样本不能预测生产P95、filesort或全文检索收益。继续按SDD G06记录真实生产SQL、实际数据分布、EXPLAIN/ANALYZE和HTTP结果，再决定是否修改索引或引入搜索引擎。全文检索的匹配语义与任意子串LIKE也不等价，迁移必须单独验证。
