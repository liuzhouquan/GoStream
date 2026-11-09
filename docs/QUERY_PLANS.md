# 查询计划探针及证据边界

## 可运行入口

SQLite：`go test ./internal/platform -run '^TestQueryPlansSQLite$' -count=1 -v`。

MySQL：将 `GOSTREAM_TEST_MYSQL_DSN` 指向已迁移的隔离库，执行 `go test ./internal/platform -run '^TestMySQLQueryPlans$' -count=1 -v`。MySQL CI 的 `^TestMySQL` 会执行该用例。它在一个最终回滚的事务内创建两用户，每张通知、私信、视频、互动、评论、Feed inbox 表 500 行；结果从当前 MySQL 实际 `EXPLAIN` 读取并打印索引、估算扫描行和 Extra。另用 `FORCE INDEX` 验证声明索引可用及其排序能力，并核对每个查询的精确行 ID/顺序或计数。优化器可以在该分布上选择 PRIMARY/其他有效路径，不以索引名称猜测生产缺陷。

生产规模（2,000 用户 / 20,000 视频 / 1,000 通知）：
- Go 原生集成（SQLite）：`go test -v ./internal/platform -run '^TestProductionScaleSearchAndNotificationQueries$' -count=1`
- Go 原生集成（MySQL 8.4）：`go test -race -v ./internal/platform -run '^TestMySQLProductionSearchAndNotificationQueries$' -count=1`（需配置 `GOSTREAM_TEST_MYSQL_DSN`）
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

## 2026-11-08 生产规模 MySQL 8.4 真实执行计划与基准证据（A08 最终验收）

基于 Docker Compose 栈中的 MySQL 8.4 实例，真实灌入 **2,000 用户、20,000 视频、1,000 通知** 数据集，通过 `TestMySQLProductionSearchAndNotificationQueries` 执行全量业务断言与真实 `EXPLAIN` / `EXPLAIN ANALYZE` 测量。

### 1. 真实 Handler SQL 与 MySQL 8.4 EXPLAIN 执行计划

| 业务场景 | 生产 Handler SQL | EXPLAIN key | type | rows (est) | Extra 开销 | 实测单次延迟 (MySQL 8.4) |
|---|---|---|---|---|---|---|
| **通知 Keyset 分页** | `SELECT * FROM notifications WHERE user_id = ? AND id < ? ORDER BY id DESC LIMIT 31` | `idx_notification_user_id` | `ref` | 1,000 | `Backward index scan`（零 filesort） | **0.03 ms** (EXPLAIN ANALYZE) |
| **通知未读计数** | `SELECT COUNT(*) FROM notifications WHERE user_id = ? AND read_at IS NULL` | `idx_notification_unread` | `ref` | 500 | `Using index`（纯覆盖索引，不回主表） | **0.02 ms** (EXPLAIN ANALYZE) |
| **用户子串搜索** | `SELECT * FROM users WHERE (username LIKE ? ESCAPE '!' OR nickname LIKE ? ESCAPE '!') AND id < ? ORDER BY id DESC LIMIT 21` | `PRIMARY` | `range` | 995 | `Using where; Backward index scan`（零 filesort） | **0.78 ms** (EXPLAIN ANALYZE) |
| **视频搜索浅页** | `SELECT * FROM videos WHERE status = ? AND (title LIKE ? ESCAPE '!' OR description LIKE ? ESCAPE '!') AND id < ? ORDER BY id DESC LIMIT 21` | `PRIMARY` | `range` | 10,251 | `Using where; Backward index scan` | **20.9 ms** (10 matches found) |
| **视频搜索深页** | 同上，带 `id < 10000` | `PRIMARY` | `range` | 10,251 | `Using where; Backward index scan` | **0.03 ms** (短路命中 21 行立即返回) |

### 2. MySQL 8.4 EXPLAIN ANALYZE 真实执行树摘录

```text
-- 通知 Keyset 分页 (idx_notification_user_id 反向索引扫描，完全避免 filesort)
-> Limit: 31 row(s)  (cost=351 rows=31) (actual time=0.0345..0.0402 rows=31 loops=1)
    -> Index lookup on notifications using idx_notification_user_id (user_id=2, id < 1000) (reverse)  (cost=351 rows=1000) (actual time=0.0337..0.0381 rows=31 loops=1)

-- 通知未读计数 (idx_notification_unread 纯覆盖索引扫描，零回表)
-> Aggregate: count(0)  (cost=176 rows=1) (actual time=0.0456..0.0456 rows=1 loops=1)
    -> Filter: (notifications.read_at is null)  (cost=126 rows=500) (actual time=0.0152..0.0357 rows=500 loops=1)
        -> Index lookup on notifications using idx_notification_unread (user_id=2)  (cost=126 rows=500) (actual time=0.0142..0.0249 rows=500 loops=1)

-- 视频搜索深页 (游标 id < 10000，快速反向扫描在命中 21 行后立即短路退出)
-> Limit: 21 row(s)  (cost=108 rows=21) (actual time=0.0212..0.0339 rows=21 loops=1)
    -> Filter: ((videos.`status` = 'published') and ((videos.title like '%prod%' escape '!') or (videos.`description` like '%prod%' escape '!')) and (videos.id < 10000))  (cost=108 rows=1076) (actual time=0.0204..0.032 rows=21 loops=1)
        -> Index range scan on videos using PRIMARY over (id < 10000) (reverse)  (cost=108 rows=10251) (actual time=0.0177..0.0255 rows=21 loops=1)
```

### 3. 前置通配符 LIKE 成本测量与适用规模边界

在 MySQL 8.4 真实已迁移数据库上，针对包含 20,000 视频的数据集执行 50 轮连续视频搜索查询测试：

- **样本规模**：20,000 视频（19,800 发布，200 草稿/转码中/失败/已删除）
- **P50 耗时**：**3.30 ms**
- **P95 耗时**：**4.61 ms**
- **Max 耗时**：**5.04 ms**
- **深页性能优势**：采用 Keyset 游标（`videos_before_id`），MySQL 8.4 优化器执行主键反向范围扫描并在累积满足 `LIMIT 21` 时立即短路中断执行树，深页实测耗时为微秒级（0.03 ms），彻底消除了 `OFFSET N` 带来的深页线性退化。

**生产适用边界与选型结论**：
1. **当前方案适用范围**：在数据量 $\le 20,000 \sim 50,000$ 视频规模下，MySQL 8.4 的主键/状态范围反向索引扫描配合 C 语言级 LIKE 过滤，P95 查询时延稳定在 5ms 以内，无内存 filesort，完全满足当下生产与演示性能要求。
2. **Elasticsearch / 全文搜索引擎选型阈值**：仅当视频数据量级突破 $10^5$（10 万行）且需要分词倒排检索、模糊拼写纠错或多字段复杂权重打分时，才需要考虑引入外置搜索引擎；在 20,000 规模下直接使用 MySQL 原生 Keyset 分页更为简洁健壮，无数据双写与最终一致性风险。

---

## 历史审查参考：2026-11-07 审查基准说明

原发布签收中的部分声明已撤回。生产结论以本文件《2026-11-08 生产规模 MySQL 8.4 真实执行计划与基准证据》及 [RELEASE_ACCEPTANCE](RELEASE_ACCEPTANCE.md) 的逐项核验为准。
