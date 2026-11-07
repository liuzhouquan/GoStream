# 独立分支与“全部完成”声明核查

日期：2026-11-07。远程基线 `a922b6b`（PR #59），待审代码树为本地 `52d1376`：本地 main 已被其他 agent 合入 release/v1.0.0，领先远程21个提交，未推送、未建立发布PR。审查以 `git diff a922b6b...52d1376` 和对应21个commit固定范围，共51文件、12,326新增行。

## 分支清点与集成决定

| 分支 | tip | 处理 |
| --- | --- | --- |
| test/final-stack-launcher | 92afef4 | G02候选实现已在本地main祖先中，修正资源所有权/后代清理/报告发布后集成 |
| test/web-full-flow-compose | c99afaa | G03新增外部双API入口保留，未见最终Compose两浏览器运行证据，不标验收完成 |
| test/g04-media-matrix-and-ten-videos | 8e6ba12 | G04候选视频矩阵保留，区分SQLite/ffprobe兼容shim与真实对象栈 |
| test/g05-interactions-notifications-security | 90e6cdc | G05交互/权限回归保留，最终环境证据待补 |
| test/g06-real-production-queries | b23d6c6 | G06入口/查询探针保留，修复端点环境合同，MySQL正式规模证据待补 |
| test/g07-feed-warm-cache | c1df146 | G07保留组件smoke，修复错误正文假成功；正式standard未实现时明确拒绝，不以短测关闭门槛 |
| test/g08-group-im-fanout | e38ad00 | G08保留群测试和候选真实驱动，修正生产重试排序及端点；最终群门槛仍待验证 |
| test/g09-component-outages-persistence | a66913b | G09增加source/delivery精确断言；完整对象栈入口未实现时明确拒绝，不以SQL通知列表代替broker恢复 |
| test/g10-migration-backup-recovery | 8e39a22 | G10保留SQLite开发恢复；MySQL/MinIO入口明确拒绝，不静默另起SQLite |
| test/g11-openapi-docs-consistency | e6e0688 | G11契约扩展保留，待最终配置复验；运维恢复承诺收窄至实际证据 |
| release/v1.0.0 | 163aa85 | 有效代码与修正统一集成；撤回全A01–A12/G01–G12通过的签收声明，不发布正式版本 |

这些“独立分支”实际上是串行依赖的历史tips，均已包含在本地main，不需要重复merge十一次。本轮以审查修正分支经PR进入远程main，并保留原本地历史。以前九个squash分支和两个已替代方案的处理不变，见 [上一轮审查](BRANCH_REVIEW_2026-11-06.md)；不合入旧设备全局ACK压缩或错误OpenAPI方案。原本地v1.0.0/v1.0.0-rc1标签未作为正式发布依据。

## Standards

依据 AGENTS、DEVELOPMENT、原SDD、TEST_STANDARD；独立标准轴确认以下重要缺陷，并由受控回归先复现再修正：

1. **P1 群重试复用回滚ID。** `groups.go:sendGroupMessage` 新增重试却复用闭包外msg，GORM回滚不清空结构体ID。回归得到重试ACK ID=1，而中间同群消息已提交ID=11，重新产生低ID迟提交/累计ACK漏未读。每attempt重建msg并重置created/conflict；逐设备ACK改用request context，取消不再继续写游标。
2. **P1 锁错误误分类。** `db_retry.go:isRetryableDBLock` 用busy字符串误重试业务错误、漏SQLite ErrLocked。改用MySQL 1213/1205和SQLite Busy/Locked驱动错误码；保留整个事务重试与取消。
3. **P1 项目资源所有权。** `acceptance_stack.py` 可对已有项目执行up后down -v。现在启动前拒绝同project标签的container/volume/network，未取得所有权时不执行破坏清理。
4. **P1 后代与清理报告。** launcher见leader退出就跳过进程组、丢弃句柄，且down前写成功/零卷报告。真实OS回归证明leader退出后sleep后代仍存活；现保留句柄并清理独立进程组，核对container/volume/network，全部清理成功后才发布成功报告。
5. **P1 取消被元数据采集吞掉。** 实例检查的宽泛except会吞LauncherInterrupted；现显式传播并补信号回归。

标准轴另确认端点合同和未实现Compose恢复（见Spec）；不为风格进行大范围重构。

## Spec

独立需求轴发现6类偏差（其中端点/恢复与标准轴有重合，不重复计独立缺陷数）：

1. **P1 无证据的全部签收。** G02/G03只记录诊断/普通Go/构建，不是最终联合Compose与两浏览器。SDD全勾、README发布就绪及G12全PASS已撤回；原门槛保留。
2. **P1 性能/RTO范围扩张。** G07 SQLite/miniredis短测不能支持Feed P95 0.73–4.21ms；G08报告不能支持200人/80+设备/P95 8.2ms；G10五用户/十视频/4MiB文件恢复4.57ms不能称生产MySQL/MinIO RTO。撤回这些发布数字，保留原报告并加审查覆盖说明。
3. **P1 正式端点被静默替换。** G02注入GOSTREAM_API_A/B，G06/G07/G08只读API_A/BASE；auto另起SQLite，显式compose可能打8080演示端点。统一acceptance_config合同；外部模式缺两个不同明确端点、显式SQLite与外部栈冲突时拒绝，先删除旧成功报告。
4. **P1 G07假门槛。** 120视频、5/10秒，Following/Timeline/Hot各跑一次不等于原规定规模/30/60秒/逐场景三轮；HTTP200空正文也计成功。加入完整20条唯一ID/顺序/游标签名检查；完整standard未实现时明确非零拒绝；组件报告release_gate_passed=false，不降低原门槛。
5. **P1 G09 broker恢复假阳性。** 原只检查互动计数及SQL通知（事务中已存在），broker不恢复也可能PASS。现在停机核对具体通知ID source待发布、无delivery；恢复要求source发布和唯一delivery已派发。SQLite不把broker项标真，缺owned栈正式入口明确拒绝，完整媒体/Compose恢复仍未实现。
6. **P1 G10 Compose名实不符。** 原解析compose后仍无条件启动SQLite，恢复分支pass。正式MySQL/MinIO恢复明确NotImplementedError，禁止伪造完成；既有真实联合恢复工具继续可用。

复查还撤回了QUERY_PLANS与search runner把20k SQLite样本外推成10万/50万生产容量边界的结论；G10报告字段也不再把文件恢复阶段填为RPO/RTO。

两轴最严重问题分别为群消息重试破坏提交顺序、发布完成与性能证据误报；未将历史单位测试通过视作最终发布通过。

## 本轮验证记录

- 初始候选树普通Go测试exit0，38.852秒；Python诊断exit0，14.223秒。它们没有暴露所有验收假阳性，也没有证明外部矩阵通过。
- 生产新回归在原树确认旧ID、请求取消仍写、锁分类三类错误；修复后含既有撤回/设备隔离定向race通过，监控总计17.781秒。
- G02原实现下已有项目、后代残留、提前报告、卷遗漏、实例检查吞中断回归均先失败，修复后15个诊断方法通过。
- 正式端点配置回归旧实现15项失败，修复后通过；G10明确拒绝正式未实现路径。
- Feed错误正文回归原实现5类HTTP200均误计成功，修正后通过；standard模式拒绝未实现门槛。
- G09原source缺失仍进入下一checkpoint的假阳性已复现；修正后缺delivery/未派发/重复delivery均拒绝。
- 修正后普通Go测试exit0，41.059秒；vet、三个程序构建、Web语法和diff检查通过。全量race及真实MySQL/broker/Redis/MinIO由最后提交CI复验，不重复无缓存A04。
- 本轮真实G02从构建到双API探测/清理exit0、111.110秒；报告核对两个API容器身份、Go1.27.2及container/volume/network全0。元数据明确记录52d1376+dirty工作树，不冒充最终发布代码树。
- 同修正启动器的真实SIGTERM：已就绪栈与独立子进程启动后取消，exit143，信号后3.791秒清理完成（整轮24.626秒）；子进程/container/volume/network均0，未发布成功报告。
- 最终Python诊断78方法通过（监控12.959秒，测试体12.693秒）。公共Markdown链接和CI YAML检查通过。
- 新群驱动首次在MySQL启动未就绪时0.869秒失败，尚未进入业务；确认ready后重跑真实MySQL8.4.11/Redis8.2.9双API：20客户端、1000消息、19000接收投递、离线/成员/设备/SQL检查通过，测试体24.242秒、runner31.483秒；接收P95为186.796ms，单机有界夹具而非容量承诺；进程/schema/user/Redis全0。
- 最后提交CI的最终状态见集成PR；未运行的最终业务/发布门槛仍不标完成。脱敏汇总见 [JSON](benchmarks/release-review-2026-11-07.json)。

原始命令日志在忽略的data/diagnostics，文档仅保留脱敏汇总。CI增加bounded群负载入口验证候选真实驱动，但不把单次驱动通过当全部G08/G12完成。

## 当前完成判断

**核心业务已实现，最终验收没有完成。** 已有新的候选脚本/测试减少后续编码工作，但G02最终取消/失败证据、G03两浏览器、G04对象媒体、G05权限、G06规模SQL、G07持续缓存、G08群负载、G09联合故障、G10真实恢复和G11契约收口仍按SDD逐项核对。G12未签收。审查修复合入主线不等于发布正式版本。
