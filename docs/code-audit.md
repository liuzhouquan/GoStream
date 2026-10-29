# GoStream 代码质量审计（第一阶段）

审计日期：2026-10-27；基线提交：`7fe452b`；审计方式：只读源码检查、现有检查与测试、隔离环境复现。本轮仅新增这份审计记录，没有修改业务代码、测试或断言。**优先级是修复顺序，不是对所有部署环境的风险断言。**“已复现”指本轮实际执行过；“代码确认”指由当前执行路径直接推出，但尚未做专门的运行时复现；“待确认”指仍需故障注入、并发测试或产品语义确认。

## 当前阅读入口（2026-10-29，基线 `83b8383`）

原始审计及逐次复现保留在下文；历史段落中的“待记录”不是当前合并状态。修复已合并到 PR #1–#13；RabbitMQ 与真实 IM 进程矩阵分别由 PR #14/#15 增补。当前交付判断查看 [`TEST_STANDARD.md`](TEST_STANDARD.md)，下一工作包查看 [`DELIVERY_SCOPE.md`](DELIVERY_SCOPE.md)。

| 当前状态 | 项目 | 继续工作的标准 |
| --- | --- | --- |
| 已有修复及回归 | P0 默认密钥、P1-01–05、P2-01–03、P1-06/07 上传租约与文件保留 | 下文记录原复现、修复及局限；后续改动仍跑相关回归 |
| 已复现，修复见 [PR #16](https://github.com/liuzhouquan/GoStream/pull/16) | P1-08 群消息低 ID 迟提交与累计 ACK | 真实 MySQL 受控暂停复现未读遗漏；同群行锁及提交/回滚/跨群回归通过，见文末 |
| 已复现，修复见分支 `fix/web-group-sync-ack` | P1-09 群前端补页和累计 ACK | tests/integration/web_group_sync.py 浏览器自动化复现未读跳跃；群会话补页、安全 ACK、缺口同步与切群防竞争回归通过，见文末 |
| 优先修复/确认 | P1-10 成员撤回/发送/ACK 交错（待复现） | 真实 MySQL 双连接权限竞态验证 |
| 待补验证 | A03/A04/A06/A07 的剩余故障、性能及最终配置；群前史可见性、已知媒体 URL 的产品边界 | 不将风险当作已证实缺陷，也不以普通测试通过替代外部依赖矩阵 |
| 文档维护 | 旧开发顺序把已实现模块列成待实现；原审计混有历史与当前状态 | 以工作包退出条件替换旧清单，并保留原始证据，不覆写失败历史 |

本次质量复核覆盖启动/架构、群发送/成员/未读/ACK/重放、浏览器群 ACK 调用、真实 IM 测试资源释放及 CI 入口。基线 Go 1.27.2 的全量普通测试、`go vet ./...`、三个命令构建和 `node --check web/app.js` 均通过（platform 11.632s）；可选外部依赖测试无配置时跳过。此轮聚焦可靠性边界，不声称再次逐行审阅了全仓库。

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

**P2-02 网页在互动计数达到缩写阈值后显示错误｜已复现并通过 PR #10 修复。**

- 位置：[`web/app.js`](../web/app.js) 第 30、38、310 行：显示 `1.0k`/`1.0万` 后，点赞回调从按钮文字用 `/\d+/` 取值再加一。
- 证据/复现：本轮按相同表达式计算，1000 次点赞显示 `1.0k`，再点一次后显示 `2`；10000 显示 `1.0万`，再点一次也显示 `2`。后端计数没有因此改变。
- 影响：高互动视频的前端显示错误，影响演示可信度。最小修复：保留原始数值于 DOM 状态或使用服务端最新计数，再格式化显示；覆盖 999/1000/10000 阈值及取消点赞。

**P2-03 浏览器本地认证缓存损坏会在脚本入口直接中断｜浏览器已复现并通过 PR #11 修复。**

- 位置：[`web/app.js`](../web/app.js) 第 1 行在顶层直接 `JSON.parse(localStorage.getItem('gostream.user') || 'null')`，无异常处理。
- 证据/复现方法：在浏览器开发者工具将 `gostream.user` 设成无效 JSON 后刷新；初始化抛异常，后续事件处理和页面渲染无法注册。最小修复：小范围容错解析并清除损坏缓存；增加浏览器刷新恢复检查。

## 尚未验证的风险与关键测试缺口

| 优先级与状态 | 风险/代码证据 | 后续确认标准 |
| --- | --- | --- |
| P1-06，已复现，修复与验证见文末 | `uploads.go:completeUploadSession` 的固定五分钟租约不续期，`CleanupExpiredUploads` 删除前也不原子领取。暂停完成请求并推进过期边界后，清理器删除分片，完成返回 409。另发现 P1-07：旧 worker 登记失败时删除已被新 worker 成功登记的本地最终文件。详见文末。 | 保留复现与对照；验证续租、清理接管、旧 worker 回滚、数据库故障恢复；真实 MySQL/MinIO 双连接和对象 GET 交错测试，并运行相关回归。 |
| P1，产品语义待确认 | [`internal/platform/groups.go`](../internal/platform/groups.go) 第 110–122 行邀请时将未读游标推进到当时最新消息，但第 178–195 行历史查询只看**当前**成员身份。新加入或被移除后重新加入的人可查询加入前、离群期间的消息。`DELIVERY_SCOPE.md` 只规定“群历史仅成员可见”，没有规定加入前历史是否可见。 | 先明确产品规则；若历史应按成员有效期隔离，补邀请、移除、重邀的权限测试和数据库过滤。不要在规则未明确前把此项写成已证实漏洞。 |
| P2，语义已确认并验证 | [`internal/platform/server.go`](../internal/platform/server.go) 第 55–59 行的 `/media` 采用 128 位高熵随机 token。未发布/处理中视频不会泄露至公开 Feed、个人主页或搜索，但在已知完整 URL 时支持直接内容寻址；已在本地及 MinIO 模式完成自动化回归验证。 | 产品明确采用标准 CDN/对象存储直接访问模型；`TestMediaEndpointAccessAndPrivacySemantics` 验证了路径穿越拦截、无效名 404 及未发布状态隐私。 |
| P2，已实现并验证 | `docs/DELIVERY_SCOPE.md` 与 `docs/TEST_STANDARD.md` 已声明：更多视频编解码格式、权限负例及对象存储完全中断恢复边界。 | `TestMediaFormatValidationAndNegativeMatrix`、`TestMediaPermissionAndOwnershipMatrix` 与 `TestMinIOCompleteStorageOutage` 已通过普通与 race 测试；较大规模生产恢复成本仍待最终阶段压测。 |

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

2026-10-27：文档 PR #6 的首轮 MySQL CI 暴露 `TestMySQLHybridFollowingIntegration` 在首批写入后四次立即轮询仍未拿到完成标记。MySQL `available_at` 的毫秒精度与读取时刻交错是待确认原因；旧测试在本机隔离库连续 30 次未复现。改为最多两秒、短间隔重查任务，并保留 `done_at` 和 Feed ID 断言；新隔离库 100 次目标测试、五次 race 和所有 `TestMySQL*` 通过。更新后的 CI 结果待记录。

2026-10-28：PR #6 已合并，更新后的 quality、Redis、MySQL、MinIO CI 均通过（run 40531605249）。P1-05 在真实 FFmpeg 和三次 API 进程启动的隔离 SQLite 演练中复现：超长视频失败后关闭处理器，原代码对更换源返回 200，视频却仍为 `failed`。修复在锁定当前视频的事务中拒绝对 `failed`/`processing` 视频提交 `media_url`，返回 409 并回滚其他字段；重新启用 FFmpeg 后，真实视频转码、封面与关注 Feed 验证通过。SQLite/MySQL 回归覆盖两种状态、所有权、已发布视频及旧任务；MinIO 和混合配置多实例尚未在此场景中验证。CI 和合并状态待记录。

P1-05 本地验证还包括 Go 1.27.2 的全量测试、全量 race、vet、API/媒体工具构建、前端/Python 语法与三套 Compose 配置；隔离 MySQL 8.4 视频恢复测试在 race 下通过，临时库与授权已删除。

2026-10-28：P1-05 已通过 PR #7 合并到 `main`（`54853a7`），quality、Redis、MySQL、MinIO 与新增的真实 FFmpeg 重启 CI 均通过（run 40612367587）。P2-01 的账户路径在 `fix/account-storage-errors` 分支分步处理：SQLite 实际 HTTP 测试先复现数据库关闭后注册误报 409、资料误报 404，修复后未知用户/错误密码仍为 401、重复用户名仍为 409，存储故障及损坏哈希返回 500。隔离 MySQL 8.4 的重复键、正常登录和无效事务测试在 race 下通过，临时库与授权已删除。Go 1.27.2 全量测试、全量 race、vet、构建、Web 语法和三套 Compose 配置检查通过。群成员及上传会话查询的同类映射尚未处理；本分支的 CI 与合并状态待记录。

2026-10-28：P2-01 账户路径已通过 PR #8 合并到 `main`（`fb3c3bd`），quality、Redis、MySQL、MinIO 和真实 FFmpeg CI 均通过（run 40613897342）。群成员和上传会话查询的同类问题在 `fix/owned-lookup-storage-errors` 分支继续处理：SQLite HTTP 测试先复现数据库关闭或成员表查询失败被误报 404；修复后这些故障为 500，非成员或非上传者仍为 404。隔离 MySQL 8.4 测试在 race 下通过，临时库与授权已删除。Go 1.27.2 全量测试、全量 race、vet、构建、Web 语法和三套 Compose 配置检查通过；CI 与合并状态待记录。

2026-10-28：P2-01 群成员/上传会话路径已通过 PR #9 合并到 `main`（`df9f2a4`），quality、Redis、MySQL、MinIO 和真实 FFmpeg CI 均通过（run 40614837522）。P2-02 的 `tests/integration/web_action_counts.py` 用隔离静态服务、模拟 API 和 Chromium 复现：对基线 `df9f2a4:web/app.js` 中 1000 次点赞的卡片点击后，测试失败并观察到 `♥ 2`。`fix/web-action-counts` 将原始计数存于按钮 `data-count`，成功响应后更新，期间禁用按钮；同一脚本在修复后通过 999、1000、10000 的点赞/取消、1000 收藏/取消及请求失败状态不变检查。Go 1.27.2 全量测试、全量 race、vet、构建、Web/Python 语法和三套 Compose 配置检查通过。服务端接口为模拟响应，真实 MySQL 互动端到端尚未在本场景复验；CI 与合并状态待记录。

2026-10-28：P2-02 已通过 PR #10 合并到 `main`（`35dedfc`），quality、Redis、MySQL、MinIO 和真实 FFmpeg CI 均通过（run 40616889554）。P2-03 的 `tests/integration/web_stored_session.py` 在隔离静态服务和 Chromium 中复现：原始 `df9f2a4:web/app.js` 遇到损坏的 `gostream.user` JSON 后脚本不再初始化，Feed 卡片数为 0 且损坏缓存保留。`fix/web-stored-session-recovery` 从缓存读取时验证用户 ID、昵称与 token；无效或不完整缓存清理后以游客身份打开 Feed。浏览器测试已覆盖无效 JSON、错误用户结构、缺失/空 token、缺失用户及有效缓存保留。Go 1.27.2 全量测试、race、vet、API/媒体工具构建、Web/Python 语法、原互动计数浏览器回归与三套 Compose 配置检查通过。该测试使用模拟 Feed，不能证明服务端 JWT 有效；CI 与合并状态待记录。

2026-10-28：P2-03 已通过 PR #11 合并到 `main`（`9bd34af`）；quality、Redis、MySQL、MinIO 和真实 FFmpeg CI 均通过（run 40650210463）。原审计问题留作修复前证据。剩余上传完成与清理交错、群历史语义、媒体源文件访问规则仍须按上表确认，不能由本次浏览器回归代替。

### 2026-10-29：P1-06 上传完成与清理竞态；P1-07 过期 worker 删除已提交文件

- **P1-06｜已复现。** 位置：`internal/platform/uploads.go:completeUploadSession`、`CleanupExpiredUploads`。在基线 `ccac936` 上仅加入私有租约时长测试参数，将五分钟缩短为 300 毫秒；`TestUploadCompletionKeepsLeaseDuringAssembly` 暂停真实 HTTP 完成请求，经过三个租约周期后调用清理。连续两轮观察到 `cleaned=1`，恢复请求返回 `409 upload chunk is missing`；相同暂停但不清理的对照为 200。`TestUploadCleanupRechecksCompletionLease` 还复现了候选查询后续租、清理器仍删除分片的行为。影响是接近续传到期时，仍在读取/合并的完成请求丢失分片，无法重试原会话。
- **P1-07｜已复现。** 位置：`internal/platform/uploads.go:completeUploadSession` 的本地 `registerCompletedUpload` 失败分支。`TestUploadCompletionLeaseLossKeepsCommittedMedia` 暂停旧 worker 的资产登记，令租约过期并由另一请求完成；新请求 200、旧请求 500 后，`UploadSession` 为 `completed`，已登记的本地最终文件却不存在。影响是成功上传的源媒体在租约交接后被旧请求误删。
- **修复范围。** `upload_lease.go` 每 100 秒条件续租五分钟，停止并等待心跳后用最新未过期租约提交资产与完成态。清理器先条件领取为 `cleaning`，再删除外部字节；SQL 清理事务失败或进程中断后，该五分钟清理租约可由后续清理接管。本地最终文件与 MinIO 对象统一保留给重试及现有孤儿回收，旧 worker 失败不再立即删除共享最终路径。现有数据库列可容纳这些状态，无 schema 变更。
- **验证与限制。** 三轮定向 race 覆盖续租、过期清理、旧 worker 回滚、清理数据库事务失败后的恢复、请求取消/数据库关闭/租约丢失时的心跳退出。最初 300 毫秒时长测试在续租实现后有一次调度相关失败；最终回归改为观察真实心跳，再将清理时刻推进到旧租约之后，保留状态与媒体字节断言。`TestMinIOUploadCompletionCleanupLease` 在 MySQL 8.4 的两套独立连接和临时 MinIO 桶中暂停真实 chunk GET，三轮 race 通过，核对最终对象 SHA-256、单个资产、跨实例完成重试及废弃会话清理；全部 opt-in `TestMySQL*` 平台测试通过。临时数据库、用户和 MinIO 容器已删除。Go 1.27.2 全量普通/race 测试、vet、API/媒体构建、Web 语法、CI YAML 与三套 Compose 配置检查通过；CI 与合并记录见 [PR #13](https://github.com/liuzhouquan/GoStream/pull/13)。测试没有等待真实五分钟传输，也未强杀进程或模拟整个存储服务中断。


## 2026-10-29：A06 功能跟进（不改写原审计基线）

原审计中“尚未接入 RabbitMQ”描述的是 `7fe452b` 的历史状态，未被列为缺陷。本轮新增真实 broker 通知链路、发布确认、消费提交后 ACK、唯一投递 outbox、延迟重试/死信、运维回放和独立 CI。隔离 MySQL 8.4/RabbitMQ 4.3.7 的七个 race 子用例、真实 API worker 生命周期与运维命令已通过，包括两消费者同时处理 16 份重复事件只新增一个持久任务。测试环境全部清理。完整命令、最初测试夹具失败和局限见 [`PROGRESS.md`](../PROGRESS.md)；A06 仍需强杀进程、DLQ 目标中断与最终全组件验收。此记录不把新增功能等同于原审计所有待确认风险均已关闭。


## 2026-10-29：A07 IM 真实进程恢复验收

本轮补充 `tests/integration/im_smoke.py` 和 `tests/integration/im`，用随机 MySQL 库、临时真实 Redis、两个以 Go race 编译的生产 API 执行公开 HTTP/WebSocket 协议；操作系统退出状态确认双方实际 SIGKILL。五个场景覆盖跨实例消息幂等/权限、重复实时提示、Redis 中断后 SQL 补偿与重订阅、进程重启/发送重试、105 条单聊和群聊的重放上限/完整补页、逐设备 ACK 和群权限撤回。已运行结果与环境见 [`PROGRESS.md`](../PROGRESS.md)，保证边界与复现入口见 [`IM.md`](IM.md)。这些场景没有复现新的业务缺陷，不能据此关闭所有历史审计风险。

**仍待确认**：迟提交低 ID 群消息与高 ID ACK 游标的交互未做事务故障注入；完整崩溃节点 presence 过期未等待观察；持续活跃设备回执压缩、两浏览器最终部署及规定 IM 负载仍未验收。重复 Pub/Sub 提示已证实不会新增持久消息行，但可以重复推送同一 ID，客户端仍需去重和未读/历史同步。

测试基础设施跟进：强制 Go 五秒超时复现两个残留 API；超时跳过 `t.Cleanup`，旧外层脚本未拥有子进程生命周期。现已用独立进程组、依赖删除前的 TERM/KILL 和受控临时目录修复，新增 `--check-timeout-cleanup` CI 入口，预期内层十秒超时且外层清理必须成功；清理前可观察到存活 API，之后活动进程/schema/user 均应为零。此问题属于本轮新增测试脚本，未将其描述为生产 IM 服务的进程泄漏。


## 2026-10-29 质量复核与后续小型修复

### P1-08：同群事务迟提交被累计 ACK 跳过（已复现；修复及 CI 记录见 [PR #16](https://github.com/liuzhouquan/GoStream/pull/16)）

- **位置与影响**：[`groups.go`](../internal/platform/groups.go) 的 `sendGroupMessage` 分配自增 ID，`ackGroupMessage` 更新累计游标，`groupUnread` 与 [`hub.go`](../internal/platform/hub.go) 的 `replayPendingGroups` 过滤 `id > last_ack_id`。MySQL 多连接并发发送可以让较低 ID 稍后提交，已读设备漏掉其未读和重放；SQL 历史记录仍存在。
- **复现**：[`TestMySQLGroupCommitOrder`](../internal/platform/group_order_test.go) 在真实 SQL 插入后暂停 ID 1，第二实例提交 ID 2 并由接收者 ACK，然后释放 ID 1。原代码输出 `late row count=1; unread response 200 {"last_ack_id":2,"unread_count":0}`。不是以 race 无报错替代逻辑并发验证。
- **最小修复**：发送事务在插入前锁定 `chat_groups` 对应行，直到提交/回滚；与邀请的群锁一致，无新表、迁移或全局互斥。测试验证提交、回滚释放、跨群独立与幂等。已推进的历史游标不会自动回退；旧数据可从历史读取。热点群写入串行成本待负载测量。

### P1-09：群前端只展示最近 100 条却推进累计 ACK（已复现；修复见分支 `fix/web-group-sync-ack`）

- **位置/证据**：[`web/app.js`](../web/app.js) 的 `openGroup` 原仅请求一次 `messages?limit=100`，未消费 `has_more`/`next_cursor`，且立即 ACK 该页最大 ID；WebSocket `group_message` 收到即直接调用 ACK。
- **复现**：[`tests/integration/web_group_sync.py`](../tests/integration/web_group_sync.py) 启动隔离静态服务、模拟 API 与 Chromium 会话。在原代码上打开积压 105 条消息的群聊时，立即触发断言失败：`AssertionError: Defect reproduced: premature ACK for message 105 before paging unread backlog! acks=[(1, 105)]`。确认未读积压超 100 条时较早消息无法加载且累计游标跨过未读。
- **最小修复**：
  1. 在 `web/app.js` 增加群会话分页与同步状态（`groupCursor`、`groupHasMore`、`groupLoadID`、`groupLastAckID`、`groupAckedID` 等）。
  2. 渲染“加载更早的群消息”按钮（`#group-more`），点击调用 `loadOlderGroupMessages` 向上翻页并维持列表滚动条高度；到达历史起点时隐藏。
  3. 实现安全累计确认 `syncGroupAck`：检查当前已渲染消息起点与服务端 `groupLastAckID`；若历史仍有更早消息且未读范围未覆盖到起点，严禁推进累计 ACK。仅在向后翻页闭合未读缺口或历史已翻到最前时才推进 ACK。
  4. 实时乱序与跳跃缺口同步：收到实时消息若发现 ID 跳跃（`m.id > lastID + 1`），暂缓 ACK 并调用 `syncGroupMessages` 补全当前群消息，补全排版后再推进 ACK。
  5. 代数防竞争：`openGroup` 和补页使用自增 `groupLoadID`，切群或退出群聊时快速使过时异步响应失效，丢弃串包渲染并阻止误发送 ACK。
- **验证与限制**：`AGENT_BROWSER_ARGS="--no-sandbox" python3 tests/integration/web_group_sync.py` 通过全部场景（105 条积压补页及延迟确认、实时消息跳跃同步、快速切群丢弃过时响应）；原有 `web_action_counts.py` 与 `web_stored_session.py` 均保持通过；Go 1.27.2 全量普通与 race 测试、vet、构建与 JS 语法检查通过。测试基于 Chromium 与模拟 API，真实双实例 IM 多客户端压测仍属 A07 后续范围。

### P1-10：群成员撤回与发送/ACK 竞态（已修复并验证）

- **位置与影响**：[`groups.go`](../internal/platform/groups.go) 的 `removeGroupMember` 之前未持有群行锁；`sendGroupMessage` 在事务外先查成员，再在事务中锁定群行并插入消息；`ackGroupMessage` 先读取群成员或共享游标，在后续事务中更新或插入设备读游标。在并发场景下，若成员撤回在检查之后、提交之前执行，撤回的成员仍可能成功发送消息，或者 ACK 事务仍会插入/更新已被踢出用户的 `GroupDeviceRead` 记录。
- **最小修复**：
  1. `removeGroupMember` 在删除成员记录的事务中，首先执行 `SELECT id FROM chat_groups WHERE id = ? FOR UPDATE` 锁定群行，与群邀请和消息发送串行化。
  2. `sendGroupMessage` 在群行锁保护的事务内，再次核查发送者是否依然为该群成员（`GroupMember`），若成员已被移除则回滚并返回 `group not found`（保持与非成员访问群相同的错误语义）。
  3. `ackGroupMessage` 在设备读游标的写入事务（或无设备 ID 时的共享游标更新）中，显式复核当前用户是否仍为群成员；若不存在则回滚并返回 404（`group membership not found`），防止已移除成员通过滞后 ACK 产生孤立游标记录。
- **验证**：
  - [`internal/platform/group_member_race_test.go`](../internal/platform/group_member_race_test.go) 添加了 `TestGroupMemberRevocationPermissions`，测试了群主移除成员、非群主/外部用户移除鉴权、群主不能退出自身群、以及被移除成员后续获取历史/未读/发送/ACK 的行为（均被拒绝且不生成游标）。
  - 添加了 `TestMySQLGroupMemberRevocationRace`（要求 `GOSTREAM_TEST_MYSQL_DSN`），使用真实独立连接模拟踢出与发送/ACK 并发时的群行锁串行化保障。


修复顺序：本次 P1-08 → P1-09 前端同步 → P1-10 复现/修复 → 设备回执生命周期及剩余 A07 验收。P2 文档偏差已修正开发顺序和当前审计入口；不因代码风格拆分整个平台或降低验收标准。
