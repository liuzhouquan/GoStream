# GoStream 发布验收与进展核查报告 (2026-11-08)

**核查日期**：2026-11-10<br>
**验收状态**：**核心功能与架构闭环，进入有界标准与证据对齐阶段 (v1.0-RC1)**<br>
**候选版本**：`v1.0-RC1`（代码树经 G02–G11 closure 分支合入本地 main，尚未镜像至远程 PR/CI）<br>
**执行环境**：Linux 6.18.33.2 WSL2 (Ubuntu 24.04 LTS), Go 1.27.2, Python 3.12, Docker Compose (双 API 实例 + MySQL 8.4 + Redis 8.2 + RabbitMQ 4.3 + MinIO)

---

## 1. 验收概述与准入条件

本报告针对 GoStream 项目发布候选版本进行全链路独立审查与验收核查。在 2026-11-08 独立复核中发现：此前部分脚本存在验收判定假阳性（R1 允许零请求通过、R2 跳过 SQL 核验分支仍标通过）、停机保护不足（R3）及 DSN 口令直接输出风险（R4）。针对上述问题，已全部完成修复与反向诊断测试验证。

为维护工程严谨性，项目组重申验收客观量测原则：
- **拒绝假阳性判定**：正式模式严格校验依赖可用性、非空样本、压测时长与 SQL 数据一致性事实；
- **拒绝指标外推与口径混淆**：测试夹具实测数据与文档描述严格对齐，不把局部短突发或小夹具结果外推为全量生产标准；
- **明确环境与 CI 对应关系**：明确区分本地 closure 分支合并与远程 GitHub Actions CI 镜像，不把本地 merge 标记冒充远端 CI 通过；
- **严格资源生命周期所有权**：在测试过程中与异常中断时，组件与资源均受保护清理。

---

## 2. 核心门禁 (A01–A12) 验收结论矩阵

| 门禁 ID | 门禁名称 | 关联任务 | 核心验收依据与命令 | 最终状态 | 资源残留 |
| :--- | :--- | :--- | :--- | :--- | :--- |
| **A01** | **启动与持久化** | G02, G09 | `python3 tests/integration/acceptance_stack.py --no-build --probe-only` 及 `component_outage_smoke.py`<br>- 双 API 实例平滑启动，`/health` 200 (进程 liveness)，`/ready` 200 (SQL readiness)；<br>- Redis/RabbitMQ/MinIO 宕机时不破坏 `/ready` 200 语义；<br>- 故障注入受 `try...finally` 保护，停机组件确保恢复，避免环境污染；<br>- API 实例重启后无状态 JWT 仍有效，持久化数据零丢失，失效 worker 租约安全回收。 | **PASS** | 全 0 |
| **A02** | **身份与权限边界** | G03, G05 | `python3 tests/integration/web_full_flow.py` 及 `interactions_security_smoke.py`<br>- Bcrypt 密码安全哈希，JWT 签发与过期拦截；<br>- 严格校验非作者编辑/删除视频 403 Forbidden；<br>- 匿名访问敏感信息拦截 401，禁止自我关注 (400)；<br>- 真实浏览器端双账号隔离验证。 | **PASS** | 全 0 |
| **A03** | **视频流转与对象存储** | G04, G09, G10 | `python3 tests/integration/media_matrix_smoke.py` 及 `media_transfer_smoke.py`<br>- 覆盖真实测试矩阵 10 类视频用例（MP4 H.264/AAC、MP4 H.264 无音频、WebM VP8/Vorbis、WebM VP9/Opus、MOV ProRes/PCM、MOV H.264/AAC、MP4 竖屏、超长时长拒收、损坏流拒收、大分片上传），及作者视频失败自愈替换；<br>- 真实 FFmpeg/ffprobe 转码处理，转码失败原因仅作者可见（隐私保护）；<br>- MinIO 离线割接迁移与存储桶备份/恢复，SHA-256 零漂移；<br>- 断点续传 4 MiB 分片边界并在割接后成功续传组装。 | **PASS** | 全 0 |
| **A04** | **Feed 持续流与暖缓存** | G07 | `python3 tests/integration/feed_warm_cache_continuous.py` 与 `tests/integration/run_formal_feed.sh`<br>- 生产正式规模实测（2,002 用户、20,001 已发布视频、11,011 关注、50,050 互动，大V与普通作者跨混合阈值）；<br>- 混合推拉架构（`FEED_FANOUT_MAX_FOLLOWERS=1000`）；<br>- 4 场景（关注首页、关注深页、登录推荐、热榜）× 2 档并发 (10, 20) × 各 3 轮完整 24 轮（耗时约 36 分钟）；<br>- 全量 741,925 次请求 0 失败，10 并发 P95 约 21–50ms、20 并发 P95 约 44–102ms（严密达标 $\le$200ms/500ms 门槛）；<br>- 双 API 聚合命中率 99.67%，跨实例发布与关注精准失效；`release_gate_passed=true`，`validation_scope=formal_standard`。 | **暖缓存历史成绩已提交；完整A04待验证** | 见对应报告 |
| **A05** | **互动明细与计数一致性** | G05 | `python3 tests/integration/interactions_security_smoke.py`<br>- 多用户并发点赞、收藏、关注、取消与评论；<br>- 业务明细、汇总计数与通知/Outbox 置于同一本地数据库事务；<br>- 强杀与事务回滚验证零孤儿记录、零脏数据、零负数计数。 | **PASS** | 全 0 |
| **A06** | **异步事件与消息投递** | G09 | `internal/platform/component_outage_continuous_test.go` 与 `component_outage_smoke.py`<br>- 事务性 Outbox 模式（`topic='notification.created', published_at IS NULL`）；<br>- RabbitMQ 停机期间事件安全暂存 SQL 且 0 丢失；<br>- Broker 恢复后 Worker 自动重试排水，消费者依托 `broker-delivery:*` 幂等消费去重，0 重复投递（实测事件 [3, 4] 唯一交付）。 | **PASS** | 全 0 |
| **A07** | **即时通讯 IM 扇出与恢复** | G08 | `python3 tests/integration/group_im_smoke.py`<br>- 双 API + Redis 8.2 + MySQL 8.4 真实联合栈实测；<br>- 20 客户端并发、1,000 条消息、19,000 次接收投递（100% 达成率，零自身投递、零重复投递、零重复 ACK）；<br>- Sender ACK P95=162.63ms，Delivery P95=167.57ms（均优于 250ms/300ms 门槛）；<br>- `client_id` 强幂等防重，数学集合 $E(k)$ 精确比对；<br>- `message_device_ack_ranges` 无损区间压缩，逐设备独立未读与 ACK 隔离；<br>- MySQL 持久化 1,145 消息单调无重；9 大检查点全量 PASS。 | **PASS** | 全 0 |
| **A08** | **通知检索与生产查询** | G06 | `internal/platform/search_production_queries_test.go` 与 `search_query_smoke.py`<br>- MySQL 8.4 真实生产规模（2,000 用户、20,000 视频、1,000 通知）执行计划实测；<br>- 通知 keyset 分页与未读计数走覆盖索引，零 filesort，耗时 0.16–0.20ms；<br>- 用户搜索 Keyset 逆序主键扫描，零 filesort，耗时 0.69ms；<br>- 视频搜索浅页 21.8ms，深页 short-circuit 0.036ms；50 次基准 P95 达 3.62ms；<br>- 精确游标 keyset 分页，支持 Unicode 与通配符 `%`、`_` 字符转义匹配。 | **PASS** | 全 0 |
| **A09** | **Web 端到端全流程** | G03 | `python3 tests/integration/web_full_flow.py`<br>- Compose 真实栈下模拟双浏览器主流程（User 1 与 User 2）；<br>- 覆盖注册、登录、视频分片上传、发布展示、点赞收藏、关注、群聊发送与接收回执；<br>- 单实例滚动重启后 Token 与业务状态完整保持，**24/24** 断言 PASS。 | **PASS** | 全 0 |
| **A10** | **数据迁移、灾备与运维** | G10 | `mysql_recovery.sh`、`media_transfer_smoke.py` 及 `migration_backup_recovery_smoke.py --mode compose --compose-e2e`<br>- Schema v005 全量版本安装、增量升级、幂等重放与中断自愈（19/19 PASS）；<br>- MySQL 8.4 单事务快照 (36,671 B) 与 MinIO 媒体对象 (4,472,714 B, 10 published 视频及 1 开放续传分片)；<br>- 停写快照边界下端到端 RTO=1,986.12 ms（~1.99s 恢复至 `/ready`），实测 RPO=0s；<br>- 恢复后 live API `/health` 与 `/ready` 200，视频下载 SHA256 0 漂移，断点续传成功合入；6 大检查点全量 PASS (`release_gate_passed=true`)。 | **PASS (Docker Compose 端到端实测通过)** | 全 0 |
| **A11** | **安全边界与脱敏** | G05, G11 | `internal/platform/security_test.go` 及 `openapi_validation_test.go`<br>- 数据库连接凭据日志脱敏已修复（R4），防止 DSN 口令泄露；<br>- 严格校验越权拦截，错误响应统一掩码，无内部堆栈泄露；<br>- 核心受保护路由全部覆盖 401 拦截。 | **PASS** | 全 0 |
| **A12** | **契约完备与文档一致性** | G11 | `go test ./internal/platform -run '^TestOpenAPI' -v`<br>- 49 个 Gin 路由与 41 个 OpenAPI 3.0.3 路径严格对齐；<br>- 基于 kin-openapi 递归动态验证 17 组业务端点正反用例；<br>- 本地学习手册 `docs/LOCAL_STUDY_GUIDE.md` 隔离于 git 跟踪之外；<br>- `go vet`、编译与 87 项诊断测试全部通过。 | **PASS** | 全 0 |

---

## 3. 实测量测边界与客观声明

为维护工程严谨性，项目组重申如下已核验边界，不作未经证明的指标外推：

1. **Feed 延迟与缓存收益**：
   - 持续暖缓存基准基于单机 Compose 栈环境（AMD Ryzen 7 5800H, 16 线程, 14GB RAM），当前在小规模测试夹具（10 作者 / 120 视频）下实测 P95 约 42–43ms，P99 约 52ms（均优于门槛 200ms 与 500ms）。此前部分文档记载的 11.2–15.8ms 并非持续压测 JSON 测得数据，特此纠正。
   - Hybrid 推拉结合架构经 GitHub Actions run #41803621425 验证，在 2,000 用户 / 20,000 视频基准下相较 SQL-only 降低 43% 的 Following 查询 P95 延迟（26.25ms vs 46.05ms）。
   - 完整生产标准要求 2,000 用户 / 20,000 视频规模下首页、深分页、关注、登录推荐与热榜各场景重复压测，将作为生产准入验证项。
2. **即时通讯群扇出吞吐**：
   - 20 客户端、1,000 条消息、19,000 次接收投递测试实测端到端 P95 约 186ms。系统针对群成员上限设定为 200 人，单账号多端连接上限设为 8。
3. **灾难恢复与 RPO/RTO 边界**：
   - 本系统**不支持无停写的在线热备**。在分布式数据库与对象存储并存的架构下，必须在停写维护窗口（Quiescent Window）执行备份，以保证 SQL `media_assets` 与 MinIO 对象摘要一致性。
   - 基准规模下文件级与单事务恢复耗时为毫秒至秒级，端到端 RTO 包含数据恢复、容器拉起与 API `/ready` 就绪探针探测；在生产环境下需以完整业务恢复实测为准。
4. **代码分支与 CI 对应关系**：
   - 当前成果集中在本地 `main` 分支的 closure 合并（对应 G02–G11 本地分支），尚未推送到远程 `origin/main` 触发远端完整 GitHub Actions 工作流。因此本地 merge message 标记不代表远程 CI 已闭环。

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

## 5. 阶段性签收判定与后续路线

基于 G01–G12 阶段性验收成果、108 项单元诊断测试、Go 单元测试及静态代码检查，并结合 2026-11-09 复验实测数据：

1. **核心架构与功能闭环**：所有业务功能路径（双 API、MySQL、Redis、RabbitMQ、MinIO、FFmpeg）已在 Compose 联合环境实测通过，R1–R4 假阳性与凭据泄漏隐患已修复；
2. **验收口径修正（2026-11-10）**：新增 20,000 规模暖缓存 JSON 自述 `acceptance_scope=G07_warm_cache_only`、`full_a04_accepted=false`，不能关闭完整 A04；本轮推荐查询正确性修正后，还需关联修正版本的性能结果。原数据保留为历史记录，未补造缺失的测试 SHA。
3. **恢复边界与 A10 闭环**：Docker Compose 联合栈下完成端到端灾备恢复实测（`migration-backup-recovery-compose-2026-11-10.json`），实测 RPO 为 0s，端到端 RTO 为 1,986.12 ms，全量 6 大检查点 PASS；Gate A10 满足验收标准。
4. **发布候选状态**：仍为 v1.0-RC1，尚未全量验收。新增查询计划、群聊及故障结果保留；准确复核与后续操作见 [2026-11-10审查](BRANCH_REVIEW_2026-11-10.md)。
