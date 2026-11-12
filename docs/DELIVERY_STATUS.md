# v1.0 交付签收与维护入口

核对日期：2026-11-12。业务基线 `main@1dec28e`。原定学习/求职展示版的 G01–G12 和 A01–A12 已完成验收，目前没有尚未实现的 v1.0 必做业务工作包。新文档提交不改变下列已验收业务源码。

## 已核对的远端事实

| 阶段 | 被测head | 最终CI | 合并提交 |
| --- | --- | --- | --- |
| [PR64 正式交付及恢复验收修正](https://github.com/liuzhouquan/GoStream/pull/64) | `f9c9bf4652e3f9dbc2224bed78220ab85c2004ab` | [run42994024180](https://github.com/liuzhouquan/GoStream/actions/runs/42994024180)，八项success | `ce0416ff0d964a5d0264ce79e0b6c0a6404d9fd6` |
| [PR65 SQLite可靠性与诊断维护](https://github.com/liuzhouquan/GoStream/pull/65) | `c2283adc56ca492982065816a9cc0645654691c1` | [run43060308009 attempt2](https://github.com/liuzhouquan/GoStream/actions/runs/43060308009/attempts/2)，八项success | `1dec28ec7dc51748efad2f60896b3c7ec09de14f` |

两次合并提交的源码树均与对应被测head一致。PR65重跑的是受Ubuntu DNS阻塞的浏览器/视频两个作业，另外六项同一head的成功结果保留；失败attempt仍可查看，没有删除失败记录。核对可执行：

```sh
git diff --exit-code f9c9bf4652e3f9dbc2224bed78220ab85c2004ab ce0416ff0d964a5d0264ce79e0b6c0a6404d9fd6
git diff --exit-code c2283adc56ca492982065816a9cc0645654691c1 1dec28ec7dc51748efad2f60896b3c7ec09de14f
```

## 代码、验收与学习入口

| 目的 | 入口 |
| --- | --- |
| 启动、配置、演示 | [README](../README.md)、[演示步骤](RELEASE_ACCEPTANCE.md)、[运维](OPERATIONS.md) |
| 已完成范围及停止条件 | [交付边界](DELIVERY_SCOPE.md)、[SDD](SDD.md) |
| 每项业务的源码/命令/证据 | [A01–A12索引](RELEASE_ACCEPTANCE.md)、[测试标准](TEST_STANDARD.md) |
| Feed结果与适用环境 | [G07](tasks/G07.md)、[正式暖缓存](benchmarks/feed-warm-cache-continuous-2026-11-10.json)、[无缓存混合负载](benchmarks/feed-a04-mixed-2026-11-11.json) |
| SQL/对象完整恢复与失败窗口 | [PR64审查](PR64_REVIEW_2026-11-11.md)、[正式恢复JSON](benchmarks/pr64-recovery-verified-2026-11-11.json)、[失败窗口JSON](benchmarks/pr64-recovery-faults-2026-11-11.json) |
| 发布后可靠性与诊断 | [POST-RISKS](tasks/POST-RISKS.md)、[复验摘要](benchmarks/post-release-risk-checks-2026-11-12.json) |

本地学习手册为 `docs/LOCAL_STUDY_GUIDE.md`，保持git忽略且不上传。公共学习材料见 [LEARNING_GUIDE](LEARNING_GUIDE.md)。

正式A10的准确口径：23表、2,003,037行、131对象；SQL94,368,396字节、对象5,959,014字节；备份5.01s，恢复至ready58.47s，至业务读取/续传验证63.16s，迁移重放另外通过。131对象中的100个为种子视频共享对象。RPO=0仅指已验证的停写维护边界；临时空间是阶段采样，不是连续峰值或Docker卷容量。

## 仍保留的证据边界

| 事项 | 已做工作 | 下一步的触发条件 |
| --- | --- | --- |
| 历史IM注册EOF | transport trace、脱敏API日志、真实断连一次POST回归及普通/race负载通过 | 同类故障再现时，保留同一次请求trace、API退出/日志和DB状态后定位；不凭历史猜测增加POST自动重试 |
| 历史视频发布500 | 已修复实际复现的SQLite写竞争，15轮完整恢复通过 | 另一发布500再现时取证；原视频500缺少日志，精确归因仍未知 |
| 旧墙钟/单调时钟差异 | 新报告使用monotonic并保留墙钟差，部署摘要可采集 | 有新的时钟/挂起事件记录时再归因；旧数据和失败历史原样保留 |

这些观测事项不等于尚未实现的业务模块。SQLite仅为单API开发模式，正式双API配置使用MySQL。多主机容量、长期soak、原生客户端、训练模型及强制微服务属于另行定义的版本范围。新缺陷按 [SDD协作规则](SDD.md) 提供复现、最小修复及相关回归；不因文档整理重复长压测。
