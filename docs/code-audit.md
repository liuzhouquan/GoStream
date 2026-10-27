# GoStream 代码质量审计（第一阶段）

审计日期：2026-10-27；基线提交：`7fe452b`；审计方式：只读源码检查、现有检查与测试、隔离环境复现。本轮仅新增这份审计记录，没有修改业务代码、测试或断言。**优先级是修复顺序，不是对所有部署环境的风险断言。**“已复现”指本轮实际执行过；“代码确认”指由当前执行路径直接推出，但尚未做专门的运行时复现；“待确认”指仍需故障注入、并发测试或产品语义确认。

## 范围与实际执行路径

- 阅读 `README.md`、`docs/ARCHITECTURE.md`、`docs/DELIVERY_SCOPE.md`、`docs/TEST_STANDARD.md`、`docs/TECHNICAL_DESIGN.md`、`docs/API.md`、`PROGRESS.md`、CI 和 Compose 配置。仓库及上级目录未找到磁盘上的 `AGENTS.md`；本会话提供的 AGENTS.md 指令已作为项目约束阅读。
- 从 `cmd/gostream/main.go` 追踪配置、迁移、HTTP 服务和后台 worker；从 `internal/platform/server.go` 追踪路由、鉴权、媒体、Feed、搜索、互动、通知和 WebSocket。重点阅读账户、视频上传/续传、对象存储、转码、媒体清理、Feed 推拉与缓存、outbox、IM 群组/逐设备 ACK/跨实例事件和相关测试。查看 `web/app.js` 的认证状态、列表及互动路径。
- 当前架构是一个 Go API 进程内的模块化单体；MySQL 是交付数据源，SQLite 是本地开发配置。Redis 用于可失效缓存、在线状态和尽力而为的跨实例实时提示；通知和视频处理使用数据库 outbox/租约。RabbitMQ 是交付目标，当前尚无 broker 集成。不能将数据库 outbox 描述成已接入 MQ。
- `MigrateSchema` 与版本化迁移、媒体导入工具的所有分支、CSS、完整浏览器交互及生产部署网络策略未逐行审计。静态与普通测试通过不等于这些场景已验证。

没有把直接使用 GORM、模块化单体、短期 Redis 缓存或未接入 RabbitMQ 本身列为代码缺陷：这些是文档明确的现状/阶段目标。未发现需要仅为消除重复或风格差异而大规模重构的依据。

## 本轮验证结果

环境：WSL2 Linux 6.18.33.2，x86_64，AMD Ryzen 7 5800H（16 逻辑 CPU），13 GiB 内存；`/usr/local/go/bin/go` 为 **Go 1.27.2**。以下命令在提交 `7fe452b` 的代码上执行；“通过”只表示命令退出码为 0。

| 检查 | 实际结果 | 范围/限制 |
| --- | --- | --- |
| `go test -count=1 ./...` | 通过；`internal/platform` 7.636s，`internal/migrations` 4.782s，其余有测试的包通过 | 未设置可选 MySQL/MinIO 环境变量的测试会跳过真实组件分支 |
| `go test -race -count=1 ./...` | 通过；`internal/platform` 66.788s，`internal/migrations` 6.058s | Go 数据竞态检测不能证明跨进程 SQL/对象存储操作安全 |
| `go vet ./...`；`go build -o /tmp/gostream-audit-build ./cmd/gostream`；`go build -o /tmp/gostream-audit-media-build ./cmd/gostream-media` | 均通过 | 没有启动所有部署组合 |
| `node --check web/app.js`；`bash -n tests/integration/*.sh`；`docker compose config --quiet` 及对象存储、双实例测试覆盖文件组合的 `config --quiet` | 均通过 | 只检查语法/配置，没有执行浏览器和 MinIO 故障演练 |
| 独立 MySQL 临时库：`go test ./internal/migrations -run '^TestApplyMySQLIntegration$' -count=1`，随后 `go test ./internal/platform -run '^TestMySQL' -count=1` | 通过，分别 1.730s、2.719s；临时库已删除 | 使用现有 MySQL 8.4 容器，仅针对隔离临时库；没有运行另需第二个库的 legacy/恢复演练 |
| 默认密钥复现：启动隔离 SQLite API，注册临时用户，用代码中的默认 `JWT_SECRET` 自行签发 HS256 JWT，请求 `/api/users/me` | **HTTP 200，返回所指定账户**；临时进程与文件已清除 | 没有对正在运行的演示 API 或真实用户执行伪造请求 |
| `web/app.js` 互动计数表达式的 Node 复现 | 实际计数 1000 显示 `1.0k` 后，点一次赞的前端表达式显示 `2`；10000 显示 `1.0万` 后也显示 `2` | 仅验证计算表达式，尚未运行浏览器 UI 自动化 |

本轮未运行真实 MinIO/FFmpeg、RabbitMQ、浏览器端到端和负载测试。MinIO 可选测试需要 `GOSTREAM_TEST_MINIO_ENDPOINT`、`GOSTREAM_TEST_MINIO_ACCESS_KEY`、`GOSTREAM_TEST_MINIO_SECRET_KEY`，部分组合还需要已迁移的隔离 `GOSTREAM_TEST_MINIO_MYSQL_DSN` 和随机测试桶；故障脚本需要独立 Compose 项目及有效 MP4。RabbitMQ 当前没有可运行的集成测试，因为 broker 链路尚未实现。仓库文档记录了以前的演练，本表仅记录**本轮**结果。

## 问题清单

### P0：严重安全或正确性问题

**P0-01 默认公开 Compose 配置使用可预测 JWT 签名密钥｜已复现。**

- 位置：[`cmd/gostream/main.go`](../cmd/gostream/main.go) 第 25–29、156–174 行附近；[`docker-compose.yml`](../docker-compose.yml) 第 30–47 行；[`internal/platform/server.go`](../internal/platform/server.go) 第 156–192 行。Compose 在未设置 `JWT_SECRET` 时使用仓库公开的固定值，并将 API 映射到宿主机所有地址；本地二进制也有固定默认值。
- 证据/复现：本轮在隔离 SQLite 进程中注册用户，用该默认密钥签发 `sub=<用户 ID>`、有效 `exp` 的 HS256 token，`GET /api/users/me` 返回 200 和目标用户。鉴权只校验签名与声明，没有第二道会阻断这种 token 的条件。
- 影响：**当默认配置可由不可信网络访问时**，知道仓库配置的人能伪装任意账户并调用受保护接口，包括私信、资料、视频管理等。若部署已覆盖为独立强密钥，此风险不成立；本轮未检查运行中演示实例的实际密钥。
- 最小修复：Compose 中强制显式提供 `JWT_SECRET`；非仅限本机监听时拒绝内置开发密钥，并更新启动文档。补测试验证默认配置启动约束、伪造 token 拒绝、合法 token 可用、过期 token 拒绝。修复后用隔离实例重复本轮复现。

### P1：重要可靠性、权限与可用性问题

**P1-01 HTTP 请求体读取没有时间上限｜代码确认，慢连接复现待做。**

- 位置：[`cmd/gostream/main.go`](../cmd/gostream/main.go) 第 123 行仅设置 `ReadHeaderTimeout`/`IdleTimeout`；[`internal/platform/videos.go`](../internal/platform/videos.go) 第 114 行及[`internal/platform/uploads.go`](../internal/platform/uploads.go) 第 129–130 行读取上传体。
- 证据/复现方法：向 `POST /api/uploads` 或分片 `PUT` 发送完整请求头后，以极低速持续传 body。现有字节上限限制总量，但头部读完后没有全局或端点级 body 截止时间。尚未量化能占用多少连接/内存。
- 影响：公开上传入口可长期占用连接与处理协程；在并发慢客户端下削弱服务可用性。最小修复是在普通 HTTP 请求读取上设置明确 deadline/最小传输速率，并单独回归 WebSocket 升级、正常大文件续传与慢但合法的客户端。

**P1-02 公开高成本入口缺少请求频率约束｜代码确认，容量影响待压测。**

- 位置：[`internal/platform/server.go`](../internal/platform/server.go) 第 75–77、105 行直接开放注册、登录、播放计数；[`internal/platform/accounts.go`](../internal/platform/accounts.go) 第 39、67 行执行 bcrypt；[`internal/platform/social.go`](../internal/platform/social.go) 第 318–328 行每次匿名播放请求写数据库。Compose 没有前置限流代理配置。
- 证据/复现方法：持续发送格式合法的注册/登录请求和同一视频的播放请求；当前路由与处理函数中没有频率限制或去重窗口。未运行攻击流量测试，也不据此声称具体吞吐损失。
- 影响：登录/注册可被用于消耗 CPU；播放接口可放大数据库写入并刷高播放量。最小修复：给这些入口添加有界、可观测的速率预算；播放事件增加同一主体/时间窗的去重规则或明确“请求次数”语义。分别测试正常用户、超限响应、并发与 Redis 不可用时的行为。

**P1-03 群 200 人上限的检查与写入不在同一串行化范围｜代码确认，并发复现待做。**

- 位置：[`internal/platform/groups.go`](../internal/platform/groups.go) 第 101–123 行：先在事务外 `Count`，再开启事务插入成员，没有锁住群或在事务内复核人数。
- 证据/复现方法：预置 199 人，在真实 MySQL 中同时邀请两个不同用户；两请求均可能看到 199 而各自插入，最终达到 201。现有 `TestGroupChatMembershipDeliveryUnreadAndAck` 覆盖权限和正常邀请，未覆盖此边界竞争。
- 影响：违反明确返回的 200 人业务上限，增加群实时扇出成本。最小修复：邀请事务内锁定群行，事务内计数并插入；增加并发邀请和重复邀请测试，核对最终人数与返回状态。

**P1-04 outbox 过期租约后的完成/失败更新没有校验租约所有权｜代码确认，多 worker 复现待做。**

- 位置：[`internal/platform/outbox.go`](../internal/platform/outbox.go) 第 61–89 行：领取只把 `available_at` 推后 30 秒，分发后的 `published_at`、`dead_at`/重试更新仅按 `id`；第 123–127 行本地推送再发布 Redis 事件。慢客户端推送可能使分发超过租约。
- 证据/复现方法：让 worker A 的分发在 30 秒后仍未返回，worker B 领取同一事件；再分别让 A/B 成功或失败，观察同一行状态与推送次数。普通测试只有单 worker 分发/死信路径。实时重复投递本身符合当前“尽力而为、客户端去重”的约定；这里的问题是过期 worker 还能覆盖新 worker 的状态，甚至形成同时 `published_at` 与 `dead_at` 的矛盾记录。
- 影响：任务状态、重试/死信指标不可信，重复推送增加；SQL 通知本体仍是持久的。最小修复：用独立租约 token/代数进行领取和最终更新 CAS，超时后旧 worker 不再更改行状态；用受控慢分发和两个实例验证状态转移。

**P1-05 关闭 FFmpeg 后，失败视频更换源文件无法恢复发布｜代码确认，重启端到端复现待做。**

- 位置：[`internal/platform/videos.go`](../internal/platform/videos.go) 第 384–417 行：`needProcessing` 要求 `s.ffmpegBin != ""`；源文件变更时只重置尺寸元数据，只有 `needProcessing` 才修改 `status`。[`cmd/gostream/main.go`](../cmd/gostream/main.go) 第 90–100 行允许不启用 FFmpeg。
- 证据/复现方法：先在启用 FFmpeg 的环境使视频进入 `failed`，然后以同一数据库关闭 `FFMPEG_BIN`，作者上传新文件并 `PATCH media_url`；代码会返回成功，但状态仍是 `failed`，公共 Feed 不会显示。`processing` 状态在相同配置切换下也可能悬停。
- 影响：配置切换后的作者恢复路径给出成功响应却没有恢复内容可见性。最小修复：明确“关闭处理器时替换源文件”的状态策略（直接发布或拒绝并提示），保证事务和 Feed 更新一致；补两阶段启动/重启测试。

### P2：维护性、错误语义与界面正确性

**P2-01 数据库故障被错误映射成用户输入错误｜代码确认。**

- 位置：[`internal/platform/accounts.go`](../internal/platform/accounts.go) 第 45–47 行把任何注册 `Create` 错误回成 409；第 67–69 行把任何登录查询错误回成 401。其他如 `memberGroup`、`ownedUpload` 对数据库故障也容易返回 404，但本轮没有逐一复现。
- 证据/复现方法：在测试中让注册插入或登录查询返回非唯一键/非 `ErrRecordNotFound` 的存储错误，检查 HTTP 状态。当前测试未注入这些错误。
- 影响：客户端收到“用户名已存在”或“密码错误”的误导信息，故障监控难以区分依赖故障。最小修复：只对已知唯一键冲突和未找到映射 409/401，其他存储错误返回 5xx 并记录内部原因；增加错误注入测试。

**P2-02 网页在互动计数达到缩写阈值后显示错误｜已复现表达式。**

- 位置：[`web/app.js`](../web/app.js) 第 30、38、310 行：显示 `1.0k`/`1.0万` 后，点赞回调从按钮文字用 `/\d+/` 取值再加一。
- 证据/复现：本轮按相同表达式计算，1000 次点赞显示 `1.0k`，再点一次后显示 `2`；10000 显示 `1.0万`，再点一次也显示 `2`。后端计数没有因此改变。
- 影响：高互动视频的前端显示错误，影响演示可信度。最小修复：保留原始数值于 DOM 状态或使用服务端最新计数，再格式化显示；覆盖 999/1000/10000 阈值及取消点赞。

**P2-03 浏览器本地认证缓存损坏会在脚本入口直接中断｜代码确认，浏览器复现待做。**

- 位置：[`web/app.js`](../web/app.js) 第 1 行在顶层直接 `JSON.parse(localStorage.getItem('gostream.user') || 'null')`，无异常处理。
- 证据/复现方法：在浏览器开发者工具将 `gostream.user` 设成无效 JSON 后刷新；初始化抛异常，后续事件处理和页面渲染无法注册。最小修复：小范围容错解析并清除损坏缓存；增加浏览器刷新恢复检查。

## 尚未验证的风险与关键测试缺口

| 优先级与状态 | 风险/代码证据 | 后续确认标准 |
| --- | --- | --- |
| P1，待确认 | [`internal/platform/uploads.go`](../internal/platform/uploads.go) 第 379–406 行先选过期会话，再删分片，删除前没有复核当前租约。若完成操作在 24 小时到期前开始、处理超过五分钟租约，清理器可能在其仍运行时删掉分片。现有测试只覆盖清理时租约仍有效及租约已过的顺序路径。 | 用可暂停的对象读取或受控时钟，在 MySQL/MinIO 双 worker 下把完成与清理交错；核对最终资产、会话与对象一致性。确认后再决定续租还是完成中保护。 |
| P1，产品语义待确认 | [`internal/platform/groups.go`](../internal/platform/groups.go) 第 110–122 行邀请时将未读游标推进到当时最新消息，但第 178–195 行历史查询只看**当前**成员身份。新加入或被移除后重新加入的人可查询加入前、离群期间的消息。`DELIVERY_SCOPE.md` 只规定“群历史仅成员可见”，没有规定加入前历史是否可见。 | 先明确产品规则；若历史应按成员有效期隔离，补邀请、移除、重邀的权限测试和数据库过滤。不要在规则未明确前把此项写成已证实漏洞。 |
| P2，待确认 | [`internal/platform/server.go`](../internal/platform/server.go) 第 55–59 行的 `/media` 是公开文件/对象路径；发布前的上传源文件已有 URL。公开 Feed 不列出处理/失败视频，但获知源 URL 的人可能直接读取。 | 明确上传源文件是否应为私有对象；在本地/MinIO 两模式测试游客使用已知 URL 的访问结果，并与产品隐私要求对齐。 |
| P2，已知未验收 | `docs/DELIVERY_SCOPE.md` 与 `docs/TEST_STANDARD.md` 已声明：RabbitMQ broker、完整混合 Feed 负载、真实双实例 IM 故障矩阵、更多编解码与对象存储完全中断仍未通过最终验收。 | 在对应功能实现/环境具备后执行隔离组件测试，记录命令、数据规模、失败/恢复结果；不得用本轮普通测试通过代替这些验收。 |

现有测试覆盖正常注册/发布/互动、上传完整性、过期会话的顺序清理、Feed 推拉、MySQL 并发点赞、IM 离线/ACK、媒体清理和 outbox 重试等重要路径。缺口集中在：默认部署密钥防护、慢 body/高频请求、数据库故障状态码、群人数并发边界、outbox 租约过期交接、上传清理与完成交错、处理器配置切换、浏览器持久状态损坏与高计数交互。这些不能由 `go test -race` 或单 worker 的通过结果替代。

## 后续小型修复 PR 顺序（本轮不实施）

1. **PR 1 / P0：密钥启动约束。** 只改配置校验、Compose 和相关文档/认证测试；用隔离实例重复伪造 JWT 复现，要求默认外部监听拒绝启动、独立密钥下伪造 token 为 401；跑 `go test ./...`、`go test -race ./...`、`go vet ./...`、构建与 Compose 配置检查。
2. **PR 2 / P1：请求资源预算。** 限制 HTTP 上传体读取时间并对公开高成本端点设频率预算；用慢连接和突发请求验证超限/释放，同时回归大文件续传与 WebSocket；跑相关 Go 测试、race、真实 HTTP smoke。
3. **PR 3 / P1：群人数边界。** 邀请事务内串行化计数；真实 MySQL 上从 199 人并发邀请，断言不超过 200，并回归群主权限、重复邀请和 IM 推送。
4. **PR 4 / P1：outbox 租约所有权。** 增加最终状态 CAS 和双 worker 受控超时测试；核对不出现矛盾 `published_at`/`dead_at`，保留重复投递容忍语义；跑普通/race 测试及真实 MySQL 集成测试。
5. **PR 5 / P1：视频状态恢复。** 明确并实现关闭 FFmpeg 后的替换源策略；用隔离数据库两阶段重启测试 `failed`、`processing`、`published` 路径，回归 Feed 与媒体所有权。
6. **后续 P2 PR：错误映射、计数显示、本地缓存容错。** 每个 PR 单独处理一类行为，增加对应 API 或浏览器回归测试。上传清理竞态、群历史策略和源媒体权限先完成表中的确认实验，再决定是否插入更高优先级修复。

每次修复都保留原有测试及断言，记录实际命令和结果；需要 MySQL/MinIO 的 PR 使用独立库/桶，不操作演示数据。性能修复必须在同一数据集和环境下与基线重复比较，不凭静态代码推断性能收益。

## 审计后修复跟踪

2026-10-27：P0-01 的修复已在 `fix/jwt-secret-default` 分支实现并本地验证。默认本地监听改为回环地址；公开监听拒绝两种旧开发密钥；Compose 要求显式 `JWT_SECRET`。启动回归测试先失败后通过，隔离 HTTP 验证旧密钥 token 为 401、独立密钥 token 为 200；Go 全量测试、race、vet、构建与 Compose 配置检查通过。运行中的本机演示 API 已轮换到 `.env` 中的随机密钥并恢复 `/ready` 200。上面的审计发现和原始复现保留为基线证据；其他 P1/P2 项仍未修复。

2026-10-27：P0-01 已通过 PR #1 合并到 `main`，quality、MySQL 和 MinIO CI 均通过。P1 请求资源预算拆成两个小 PR：先给 HTTP 请求体总读取时间设上限，再单独处理公开高成本路由的频率预算；原审计问题和修复顺序不变。`fix/upload-body-timeout` 已加入慢 body、后续完整请求、分片持久化与 WebSocket 心跳回归；P1-02 频率约束仍未实现。

2026-10-27：P1-01 已通过 PR #2 合并到 `main`，三项 CI 均通过。P1-02 已在 `fix/public-route-rate-limits` 分支加入注册、登录和播放计数的每 IP 一分钟预算；Redis 原子计数与有界本机回退、429/`Retry-After`、Prometheus 结果计数和对应测试均已实现。原始审计问题仍保留；此项的最终 CI 和合并状态以 PR 记录为准。

2026-10-27：P1-02 已通过 PR #3 合并到 `main`；quality、Redis、MySQL、MinIO CI 均通过。本机演示 API 更新后，真实 MySQL/Redis 的 30 次无效登录返回 401，第 31 次返回 429 和 `Retry-After`，`/ready` 仍为 200。P1-03 的真实 MySQL 回归测试在原代码上复现了 199 人同时邀请 16 人后达到 208 人；`fix/group-member-cap` 把群行锁、人数复核及插入放入同一事务。隔离 MySQL 8.4 双实例测试连续十次通过，满员重复邀请和通知/游标/outbox 单次写入也已检查；所有 `TestMySQL*` 测试及群测试三次 race 运行通过。Go 全量测试、race、vet、构建及配置检查通过；CI 合并状态待记录。

2026-10-27：P1-03 已通过 PR #4 合并到 `main`，quality、Redis、MySQL、MinIO CI 均通过。演示 API 更新后容器健康、`/ready` 与 `/health` 返回 200；紧跟重建的第一次 ready 探测曾返回 502，确认是启动窗口。P1-04 的受控双 worker 测试在旧代码上确认了两种状态覆盖：新 worker 发布后旧 worker 写入失败信息，或新 worker 死信后旧 worker 又标记发布。`fix/outbox-lease-fencing` 用 `attempts` 代数和未过期条件约束最终写入；SQLite 五轮 race、真实 MySQL 8.4 双连接十轮及三轮 race、全部 MySQL 平台测试、Go 全量测试/race/vet/构建及配置检查通过。原始审计问题保留；CI 与合并状态待记录。

2026-10-27：P1-04 已通过 PR #5 合并到 `main`；quality、Redis、MySQL、MinIO CI 均通过。本机演示 API 更新后 `/ready`、`/health` 均为 200，容器二进制 SHA-256 与 Go 1.27.2 构建一致。此修复防止旧 worker 覆盖新租约的数据库状态；实时重复通知仍可能发生，当前 Web toast 尚未按通知 ID 去重。下一项 P1-05 需用启用/关闭 FFmpeg 的两阶段重启验证失败视频更换源文件后的状态恢复。
