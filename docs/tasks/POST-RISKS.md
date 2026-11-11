# POST-RISKS：发布后待确认风险完善

Owner：集成 agent。基线 `ce0416ff0d964a5d0264ce79e0b6c0a6404d9fd6`（PR #64 合并）。分支 `fix/post-release-diagnostics`。状态：实现、本地验证完成；远端验收以本 PR 最终提交的 CI 为准。

本轮不重新打开 v1.0 的业务清单。证据摘要：[JSON](../benchmarks/post-release-risk-checks-2026-11-12.json)，原始日志留在忽略的 `data/diagnostics/`；本地学习指南继续忽略。

## POST-01：SQLite 500

先用原版池配置运行真实恢复组件，14轮成功、第15轮添加群成员500。新保留的日志明确指出 `internal/platform/groups.go:142 database is locked`。保留原失败 `data/diagnostics/post01-original-failure.failure.json`，不上传SQL正文。

紧缩到 `TestSQLitePoolSerializesReadThenWriteTransactions`：事务读取快照；同池另一连接提交；第一事务再写。修复前第二写成功、快照升级立即报锁错误，busy_timeout不能等待一个已经过期的快照。修复为 API SQLite 池 `MaxOpen=1, MaxIdle=1`，第二写先按context超时等待，第一事务提交后第二写成功、最终计数精确为2。MySQL配置沿用原50/25及自定义设置。

修复后完整组件15/15成功（79.814s），最终日志/取消保护改动后的组件演练另通过（5.669s）。该证据证明新复现的竞态已修；历史视频发布500的原日志已丢失，不能声称其确切原因得到证明。SQLite支持单API本地配置；多实例交付配置使用MySQL。

恢复runner的 source/restored/replay 进程均写入本轮临时日志；异常退出、临时目录销毁前导出同名 `.failure.json`（0600、每进程末64KiB、去凭据与SQL行正文）。保留原异常；日志磁盘不可写不遮蔽错误。SIGTERM真实子进程测试验证启动赋值前后所有权，不让helper提前抛取消导致孤儿。成功只删除同一路径的过期失败证据。

## POST-02：时间与部署证据

原报告总时间使用 `time.time()`，可确定性复现墙钟跃进让2秒记成25,900秒。总耗时所有报告路径改为monotonic；墙钟差单列，指标仍是原测量窗口。旧报告不重写，历史差异是否由挂起或时钟调整引起仍未知。

`deployment_provenance.inspect_api_deployment` 通过项目标签选API、按测量端口精确关联容器，记录 immutable image ID 和 `/proc/1/exe` SHA256。只选择非敏感inspect字段，避免输出Config环境变量。没有Docker、端口不匹配、摘要非法或无法关联时显式未验证；两个端点须对应不同容器，不用checkout SHA冒充部署身份。

真实既有镜像 `--no-build` 双API探测通过（25.541s），报告保留具体容器、镜像、二进制摘要；只证明本次部署身份可采集，不证明旧镜像等同当前checkout。容器/卷/网络残留各0。不重复未修改生产Feed的长A04矩阵，不改历史性能数据。

## POST-03：IM注册EOF

历史CI的失败进程及日志已不存在；没有证据允许直接修网络实现。新增 `httptrace` 记录连接是否取得/复用/空闲、请求是否写出、首字节、连接建立失败；错误继续wrap原错误，`errors.Is(err, io.EOF)`成立，错误路径不重放POST。失败测试在子进程停止后输出有界、脱敏API日志，包含race提示及数据库/HTTP错误。

真实HTTP服务完整读取POST后直接关连接，回归证明EOF、一次请求、trace包含`got_conn=true first_byte=false`且没有密码。修复前这个测试因没有trace而失败。另用429、Retry-After=1和10ms服务端IdleTimeout验证正常恢复；单凭空闲超时不能推断历史EOF原因。

隔离MySQL8.4.11/Redis8.2.9双API的完整负载普通/race各通过：92.278s/125.645s，总218.800s；分别因生产注册预算等待38.032s/52.048s。各包含三轮实时负载、离线补齐、3,125条精确持久消息和60次幂等检查。schema/user/Redis/API子进程全部清零，专用MySQL已删除。

**结论：诊断与复验完成；历史EOF根因仍未知，不声称故障消除。** 下次若复现，先读failure trace及同一次API日志，区分连接建立、写后断连、请求deadline、进程退出/资源异常；保持生产限流、bcrypt和一次POST失败语义。按证据另开最小修复PR。

## 验证与交接

- `/usr/local/go/bin/go test -count=1 ./...` PASS，39.527s。
- SQLite池/数据库配置/注册错误与429/日志脱敏的定向race PASS：cmd1.136s、IM2.192s。
- Python诊断131项OK、0跳过，wrapper38.076s。首轮失败来自默认本地路径的旧恢复报告缺少`sql_snapshot`，旧文件已另存；重新执行当前SQLite组件生成证据后通过，未删除测试或削弱断言。
- `go vet ./...`、`go build ./cmd/...`、`node --check web/app.js`、`git diff --check` exit0。
- 时钟跃变、日志脱敏、不可写诊断磁盘、SIGTERM启动所有权、容器身份不匹配均有回归。单元服务和Docker真实组件的证据明确区分。
- 本PR最后提交仍须运行远端CI；合并后核对主线与被测树一致。历史EOF及历史视频500的精确归因保持以上边界，不新增无依据的功能或“修复”。

## 本轮 CI 发现的 apt 镜像列表遗漏

首轮 run [43056633542](https://github.com/liuzhouquan/GoStream/actions/runs/43056633542) 的浏览器/视频两任务在业务测试前的FFmpeg安装步骤退出124。日志显示 `Get file:/etc/apt/apt-mirrors.txt` 后仍四次访问Azure HTTP源，各约20秒，90秒metadata预算耗尽。原脚本只替换 `.list/.sources` 中的直接URL，未替换间接镜像列表。

新增纯文件改写helper覆盖 `apt-mirrors*.txt`，仍仅替换Azure Ubuntu origin，保留套件、签名和其他厂商源，不放宽90s/240s预算。镜像列表夹具在原直接改写逻辑下失败，修正后两项文件回归及幂等验证通过。开发机保护实测仍在apt操作前退出。最终全量诊断133项OK、0跳过（37.217s），替代前述131项计数。该CI失败不归因于业务Go测试；最终完整CI须针对新增修正后的head重新验证。


第二轮 run43058017410 的日志证实镜像列表已改成archive HTTPS，但该域名仍持续超时。因此不能把“已重写文件”当作“网络已恢复”。安装器增加有界下载探测：只允许三个Ubuntu官方HTTPS源，实际下载当前suite的InRelease，每个最多8秒；选择首个可达源，apt使用相同IPv4路径且继续原签名校验。全部不可达则在修改源或安装前明确失败，不跳过真实FFmpeg测试。原90s/240s apt预算不变，探测额外最多24秒。默认路径/IPv6是否是历史超时根因尚未证实，不作归因结论。

真实shell配合临时curl/sudo/ffmpeg夹具验证首源失败后选择次源、全部失败时不修改系统；未修改本机apt。最终全量135项OK、0跳过，取代前述133计数；最终CI仍须验证实际远端网络和业务步骤。
