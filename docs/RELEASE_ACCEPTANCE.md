# GoStream 发布验收与最终签收报告 (v1.0-demo / v1.0.0)

**签收日期**：2026-11-08<br>
**验收状态**：**已通过全部验收门禁 (G01–G12 / A01–A12)**<br>
**发布版本**：`v1.0.0` / `v1.0-demo`<br>
**基线代码树**：`main`（经 PR #61–#71 完整闭环合入）<br>
**执行环境**：Linux 6.18.33.2 WSL2 (Ubuntu 24.04 LTS), Go 1.27.2, Python 3.12, Docker Compose (双 API 实例 + MySQL 8.4 + Redis 8.2 + RabbitMQ 4.3 + MinIO)

---

## 1. 验收概述与准入条件

本报告针对 GoStream 项目发布候选版本进行全链路独立审查与联合验收。在此前 2026-11-07 审查发现的历史“以局部测试或开发夹具代替真实生产栈”的问题已全部被推翻并经由独立 closure 分支（G02–G11）在真实 Docker Compose 最终联合验收栈上重新设计、执行并验证闭环。

所有验收用例均在统一的环境基准与客观量测原则下执行：
- 拒绝以普通 Go 单元测试或本地内存夹具冒充真实外部组件通过；
- 拒绝未经分布式一致性证实的“在线热备”虚假承诺，明确声明采用停写维护窗口进行灾备；
- 拒绝外推或编造未经测量的吞吐与 SLA 承诺；
- 所有联合验收栈运行均严格验证资源生命周期所有权，并在测试结束后断言 `{"containers": 0, "volumes": 0, "networks": 0}` 零资源残留。

---

## 2. 核心门禁 (A01–A12) 验收结论矩阵

| 门禁 ID | 门禁名称 | 关联任务 | 核心验收依据与命令 | 最终状态 | 资源残留 |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **A01** | **启动与持久化** | G02, G09 | `python3 tests/integration/acceptance_stack.py --no-build --probe-only` 及 `component_outage_smoke.py`<br>- 双 API 实例平滑启动，`/health` 200 (进程 liveness)，`/ready` 200 (SQL readiness)；<br>- Redis/RabbitMQ/MinIO 宕机时不破坏 `/ready` 200 语义；<br>- API 实例重启后无状态 JWT 仍有效，持久化数据零丢失，失效 worker 租约安全回收。 | **PASS** | 全 0 |
| **A02** | **身份与权限边界** | G03, G05 | `python3 tests/integration/web_full_flow_smoke.py` 及 `interactions_security_smoke.py`<br>- Bcrypt 密码安全哈希，JWT 签发与过期拦截；<br>- 严格校验非作者编辑/删除视频 403 Forbidden；<br>- 匿名访问敏感信息拦截 401，禁止自我关注 (400)；<br>- 真实浏览器端双账号隔离验证。 | **PASS** | 全 0 |
| **A03** | **视频流转与对象存储** | G04, G09, G10 | `python3 tests/integration/media_matrix_smoke.py` 及 `media_transfer_smoke.py`<br>- 覆盖 10 类视频格式矩阵（H.264/AAC, H.265, VP9, AV1, 损坏文件头, 超大文件, 零字节, 纯音频, WebM, FLV）；<br>- 真实 FFmpeg/ffprobe 转码处理，转码失败原因仅作者可见（隐私保护）；<br>- MinIO 离线割接迁移与存储桶备份/恢复，SHA-256 零漂移；<br>- 断点续传 4 MiB 分片边界并在割接后成功续传组装。 | **PASS** | 全 0 |
| **A04** | **Feed 持续流与暖缓存** | G07 | `python3 tests/integration/feed_warm_cache_continuous.py` 与 GitHub Actions run #41803621425<br>- 混合推拉架构（`FEED_FANOUT_MAX_FOLLOWERS=1000`）；<br>- Compose 真实栈上 10 与 20 并发、30s 预热、60s 实测（连续 3 轮）；<br>- 20 并发预热场景三轮实测 P95 达标（11.2ms–15.8ms，远优于门槛 200ms），P99 达标（22.4ms–31.2ms，远优于门槛 500ms），缓存命中率 > 98%，0 错误；<br>- Redis 宕机自动触发 5s 熔断冷却并安全回源 SQL。 | **PASS** | 全 0 |
| **A05** | **互动明细与计数一致性** | G05 | `python3 tests/integration/interactions_security_smoke.py`<br>- 多用户并发点赞、收藏、关注、取消与评论；<br>- 业务明细、汇总计数与通知/Outbox 置于同一本地数据库事务；<br>- 强杀与事务回滚验证零孤儿记录、零脏数据、零负数计数。 | **PASS** | 全 0 |
| **A06** | **异步事件与消息投递** | G09 | `internal/platform/component_outage_continuous_test.go` 与 `component_outage_smoke.py`<br>- 事务性 Outbox 模式（`topic='notification.created', published_at IS NULL`）；<br>- RabbitMQ 停机期间事件安全暂存 SQL 且 0 丢失；<br>- Broker 恢复后 Worker 自动重试排水，消费者依托 `broker-delivery:*` 幂等消费去重，0 重复投递。 | **PASS** | 全 0 |
| **A07** | **即时通讯 IM 扇出与恢复** | G08 | `python3 tests/integration/group_im_smoke.py`<br>- 双 API + Redis 真实栈下 20 客户端并发、1,000 条消息、19,000 次接收投递；<br>- `client_id` 强幂等防重；<br>- `message_device_ack_ranges` 无损区间压缩，90 天闲置淘汰；<br>- 离线设备重连基于严格消息 ID 游标流式回放。 | **PASS** | 全 0 |
| **A08** | **通知检索与生产查询** | G06 | `python3 tests/integration/search_query_smoke.py`<br>- 2,000 用户与 20,000 视频真实生产数据规模；<br>- EXPLAIN 查询计划深入分析，验证前置通配符 LIKE 扫描开销并明确适用规模；<br>- 精确游标 keyset 分页，支持 Unicode 与通配符 `%`、`_` 字符转义匹配。 | **PASS** | 全 0 |
| **A09** | **Web 端到端全流程** | G03 | `python3 tests/integration/web_full_flow_smoke.py`<br>- Compose 真实栈下模拟双浏览器主流程（User 1 与 User 2）；<br>- 覆盖注册、登录、视频分片上传、发布展示、点赞收藏、关注、群聊发送与接收回执；<br>- 单实例滚动重启后 Token 与业务状态完整保持，20/20 断言 PASS。 | **PASS** | 全 0 |
| **A10** | **数据迁移、灾备与运维** | G10 | `mysql_recovery.sh`、`media_transfer_smoke.py` 及 `migration_backup_recovery_smoke.py`<br>- Schema v005 全量版本安装、增量升级、幂等重放与中断自愈（19/19 PASS）；<br>- MySQL 8.4 单事务快照转储、恢复、v002 回滚演练与重放自愈完全闭环；<br>- 停写维护窗口下 SQL 记录与 MinIO 对象 SHA-256 摘要 100% 对齐；<br>- 灾备后断点续传成功续接并发布。 | **PASS** | 全 0 |
| **A11** | **安全边界与脱敏** | G05, G11 | `internal/platform/security_test.go` 及 `openapi_validation_test.go`<br>- 严格校验越权拦截，错误响应统一掩码，无内部堆栈泄露；<br>- 核心受保护路由全部覆盖 401 拦截。 | **PASS** | 全 0 |
| **A12** | **契约完备与文档一致性** | G11 | `go test ./internal/platform -run '^TestOpenAPI' -v`<br>- 49 个 Gin 路由与 41 个 OpenAPI 3.0.3 路径严格对齐；<br>- 基于 kin-openapi 递归动态验证 17 组业务端点正反用例；<br>- 本地学习手册 `docs/LOCAL_STUDY_GUIDE.md` 隔离于 git 跟踪之外；<br>- `go vet`、编译与 81 项诊断测试全部通过。 | **PASS** | 全 0 |

---

## 3. 实测量测边界与客观声明

为维护工程严谨性，项目组重申如下已核验边界，不作未经证明的指标外推：

1. **Feed 延迟与缓存收益**：
   - 持续暖缓存基准基于单机 Compose 栈环境（AMD Ryzen 7 5800H, 16 线程, 14GB RAM），20 并发下实测 P95 约 11–16ms，P99 约 22–32ms；该指标属于暖缓存且命中率 > 98% 场景，未命中或冷启动回源时依赖底层 SQL Keyset 查询耗时。
   - Hybrid 推拉结合架构经 GitHub Actions run #41803621425 验证，在 2,000 用户 / 20,000 视频基准下相较 SQL-only 降低 43% 的 Following 查询 P95 延迟（26.25ms vs 46.05ms）。
2. **即时通讯群扇出吞吐**：
   - 20 客户端、1,000 条消息、19,000 次接收投递测试实测端到端 P95 约 186ms。系统针对群成员上限设定为 200 人，单账号多端连接上限设为 8。
3. **灾难恢复与 RPO/RTO 边界**：
   - 本系统**不支持无停写的在线热备**。在分布式数据库与对象存储并存的架构下，必须在停写维护窗口（Quiescent Window）执行备份，以保证 SQL `media_assets` 与 MinIO 对象摘要一致性。
   - 基准规模下文件级与单事务恢复耗时为毫秒至秒级，端到端 RTO 包含数据恢复、容器拉起与 API `/ready` 就绪探针探测，实测可在 30 秒内完成；RPO 在停写维护窗口下为 0 秒。

---

## 4. 5–10 分钟生产与演示操作指南 (Demo Runbook)

### 步骤 1：环境初始化与一键启动
```sh
# 1. 确保已安装 Docker 和 Docker Compose
cd /home/lzq/projects/golangprojects/GoStream

# 2. 生成安全 JWT 密钥并启动双 API 联合验收栈
printf 'JWT_SECRET=%s\n' "$(openssl rand -hex 32)" > .env
docker compose -f docker-compose.yml -f docker-compose.object.yml up -d --build

# 3. 查看容器健康状态
docker compose ps
# 断言：mysql (healthy), redis (healthy), rabbitmq (healthy), minio (healthy), api (started)
```

### 步骤 2：验证服务就绪与 Web 客户端
```sh
# 验证进程活跃与数据库就绪
curl -i http://127.0.0.1:8080/health
curl -i http://127.0.0.1:8080/ready
# 打开浏览器访问前端静态页面
# URL: http://localhost:8080
```

### 步骤 3：体验断点续传与媒体发布
1. 在前端页面注册账号 `alice`，密码 `password123`；
2. 选择本地 MP4 视频点击上传，网络面板可观察到 4 MiB 分片上传及 `/complete` 会话组装；
3. 后台 FFmpeg 异步转码生成标准化 H.264/AAC 流与封面，页面自动流转为 `published`。

### 步骤 4：体验社交互动与即时通讯
1. 另起隐身窗口注册账号 `bob`；
2. 在首页 Feed 浏览 `alice` 的视频并点赞、评论；
3. `alice` 页面右上角实时 WebSocket 收到通知推送；
4. 进入群聊页面创建群聊，双方收发实时消息并观察每设备 ACK 状态。

### 步骤 5：灾难恢复与回滚演练（可选）
```sh
# 执行 MySQL 单事务转储、异库恢复与 v002 回滚演练
bash tests/integration/mysql_recovery.sh "$(docker compose ps -q mysql)"
```

### 步骤 6：环境完全清理
```sh
docker compose -f docker-compose.yml -f docker-compose.object.yml down -v
```

---

## 5. 最终签收判定

基于 G01–G11 全量验收任务报告、独立真实验收栈执行证据、81 项单元诊断测试、Go 全量单元测试及静态代码检查：

**本项目发布门禁 G12 正式签收，准予发布 v1.0.0 正式候选版本。**
