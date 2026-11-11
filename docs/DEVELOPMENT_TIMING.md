# 开发任务耗时诊断与监控

2026-11-12 POST-02：`feed_warm_cache_continuous.py` 统一总耗时为 `time.monotonic()`，记录 `elapsed_clock=monotonic`，同时保留独立的 `wall_elapsed_seconds`。墙钟跳变回归证明2秒不会记成25,900秒；这不证明旧报告差异由挂起引起。Compose报告另记录测量端口对应容器的镜像ID及运行中二进制SHA256；不可采集时显式未验证，checkout与镜像不等价。真实双API采集通过、资源清零，见 [POST-RISKS](tasks/POST-RISKS.md)。本轮不重复长A04矩阵。

2026-11-10 补充：PR #63 首轮 CI `42813554279` 的 video-recovery 安装 FFmpeg 步骤从 12:11:14Z 持续至 12:21:08Z 被 10 分钟任务上限取消，业务测试尚未开始。日志为 Azure Ubuntu HTTP 源下载，多个小包耗时 15–30 秒，部分包重试；不能把这段时间归因于 Go 测试。两个 FFmpeg 安装步骤统一改用仅限 GitHub runner 的 `tools/install_ci_ffmpeg.sh`：保留 Ubuntu 签名及 suite，切换该镜像为 Ubuntu 官方 HTTPS 源，metadata/install 分别设置 90s/240s 上限并验证真实 ffmpeg/ffprobe；安装失败依然失败，不降测试门槛。最终有效性以本 PR 最新 CI 为准。

2026-11-06补充：真实取消演练发现 `InterruptedError` 信号异常会被 `subprocess.run(capture_output=True)` 底层selector当EINTR吞掉，launcher继续运行而不执行finally；原仅模拟Docker返回的信号测试没有覆盖这个等待窗口。现用 `tests/integration/launcher_lifecycle.py` 的 `LauncherInterrupted(Exception)`，IM/RabbitMQ/MinIO共用。最小真实子进程等待回归先失败后通过；真实MySQL/broker创建后SIGTERM从超过30秒不退出修复为2.206秒清理完成，exit143、资源零残留。此发现说明要同时观察信号实际退出码和清理，不能把“已发送SIGTERM”当作命令已停止。

本次调查针对开发和验证流程的耗时，不是平台 API 的容量测试。原始会话记录只在本机读取；仓库内仅保留脱敏后的数值、测试名、Actions 步骤名和来源链接。

## 已确认的原因

1. **反复执行全尺寸 A04。** 上一整轮任务的时间边界是 2026-11-04 01:49:28–10:21:56 UTC，共约8小时32分钟。当天最近9次 A04 从开始到最后更新累计约2小时40分钟，其中8次进入约18–21分钟的完整流程，1次在 helper 阶段提前失败。这是多次运行的累计量，不是一条命令运行了数小时。
2. **race 测试主要消耗 CPU。** 本次只跑一次全仓库 race：总 wall time 238.47秒，platform 包238.339秒；进程树 user/system CPU 为252.99/21.82秒。199次成功注册的 handler wall time 累计181.932秒。最慢测试 `TestPublicRouteBudgetsUsePeerAddress` 为39.29秒，包含11次注册和31次密码比较。定向 profile 中，racecall 与 runtime._ExternalCode 占 flat CPU 约71%，Blowfish encryptBlock 另占15.67%。这支持密码哈希路径及其 race 插桩造成较大 CPU 开销，不能把全部注册耗时视为纯 bcrypt CPU。
3. **短命令存在执行工具往返开销。** 连续3次 `true` 的工具报告等待时间均低于0.1毫秒，外层往返分别约4.49、4.01、3.59秒。工具的等待字段不包含完整启动链路，不能直接当作 shell wall time；因此又在同一个 Python 进程内实测 zsh：非 login 启动4.45–8.98毫秒，login 启动3.93–4.13毫秒。该批探针的外层往返却为7.59秒。现有证据不能继续区分权限检查、工具调度和消息传输的各自占比，未发现 zsh 启动造成数秒延迟的证据。
4. **此前存在命令跟踪问题。** 整轮执行记录中有641个 `exec_command` 包装只输出 stdout，未呈现退出码和会话 ID。这些调用并非全部发生了问题，但长命令一旦返回 `session_id` 且 stdout 为空，这种包装就会丢掉“仍在执行”的信息。最近一段83分钟记录中，有空输出的 artifact 下载、push、PR创建和 merge 后出现额外检查或重试。不能仅凭空输出认定网络命令没有执行。
5. **轮询和串行等待叠加。** 整轮记录有189次显式 sleep，累计约2小时14分钟；153次 Actions 状态查询约19分钟，54次 PR check 查询约6分钟。这些量用于说明开销，不能与后台 CI/测试时间直接相加，因为它们重叠。无法通过现有记录把剩余所有时间精确分配给模型推理、工具调度或上下文重建。

这说明测试和压测确实较重，但不足以单独解释整轮8小时；重复的完整验证、较多轮询及丢失进程状态也需要修正。完整数值见 [诊断结果 JSON](benchmarks/development-timing-2026-11-04.json)。

已用实际 Actions 数据复核这几个关键运行：

| 运行 | job 总跨度 | 主要耗时步骤 |
| --- | ---: | --- |
| [A04 #41803621425](https://github.com/liuzhouquan/GoStream/actions/runs/41803621425) | 20分12秒 | 六轮验收及两个诊断 fixture，19分19秒 |
| [PR56 CI #41809615602](https://github.com/liuzhouquan/GoStream/actions/runs/41809615602) | 6分33秒 | quality 的 race 4分48秒；IM 决定整个 run 的结束时间 |
| [PR57 CI #41812517048](https://github.com/liuzhouquan/GoStream/actions/runs/41812517048) | 6分39秒 | quality 的 race 3分51秒；MinIO 编译2分20秒；IM 是最后完成的 job |

这些任务并行执行，因此完整CI耗时约6–7分钟，不能把八个 job 的时间逐个相加。

另一个实际环境问题是默认沙箱初始化对 `~/.aws` 符号链接的只读路径校验失败，随后需要使用获准的执行路径。现有记录无法量化该环节在数秒往返中的占比。收尾时机器有16个逻辑CPU、约10 GiB可用内存，load average约0.39，未发现遗留的 go/platform.test/gh/git 进程；这个快照不能代表之前的资源状况。

## 本地命令监控

工具只依赖 Python 标准库和 Linux/POSIX 进程接口。它执行 argv，不通过 shell 展开；聚合 JSON 不记录命令参数或环境变量。原始 stdout/stderr 合并保存于同名 `.log`，仍可能包含敏感信息，因此默认放在已被忽略的 `data/diagnostics/`，审查脱敏后才可分享。

```sh
python3 tools/task_timing.py run \
  --label platform-unit \
  --report data/diagnostics/platform-unit.json \
  --heartbeat-seconds 15 --timeout-seconds 180 --quiet -- \
  /usr/local/go/bin/go test -json -count=1 ./internal/platform
```

每个心跳显示 PID、elapsed、距最后输出的时间、当前 Go 测试、存活进程树 CPU 与采样 RSS。结束时保存退出码、user/system CPU、最慢20个测试、未完成测试及包结果；非零退出码原样传播。超时返回124，并对同一进程组发送终止与强制退出信号，避免测试派生进程留在后台。无输出时间长只是观察值，不会自动判定挂死。 默认在超时后给命令30秒清理宽限期（可用 `--grace-seconds` 调整），所以总耗时可能超过主超时。独立会话需要由 launcher 自己回收；`im_smoke.py` 将 SIGTERM/SIGINT 转入清理、在创建前记录资源所有权，并在 Popen 句柄交接期间延后中断。SIGKILL 或宽限期耗尽仍无法保证外部 Docker/MySQL 资源清理。

已有验证：真实 Redis recovery 用例的心跳显示测试名持续不变，CPU 基本不增长、输出静默约4秒，随后正常完成，符合测试中五秒熔断等待的行为。这里的存活树 CPU 可能因编译子进程退出而下降；最终 child CPU 是独立汇总。RSS 是采样峰值，不能当作精确的最大内存。

全仓库 race 可以使用相同入口，设置足够的预算：

```sh
python3 tools/task_timing.py run \
  --label full-race --report data/diagnostics/full-race.json \
  --timeout-seconds 480 --heartbeat-seconds 15 --quiet -- \
  /usr/local/go/bin/go test -race -json -count=1 -timeout=6m ./...
```

## GitHub Actions 步骤耗时

```sh
python3 tools/task_timing.py ci 41812517048 \
  --repo liuzhouquan/GoStream \
  --report data/diagnostics/ci-41812517048.json
```

该入口读取现有 run 和 job 元数据，不启动新工作流。输出 job 总跨度与最慢步骤，支持多页 jobs 和当前安装的 `gh 2.45`。一次采集即可分析完整历史；监控进行中的 run 时，每次采集都是一个时间点的快照。

## 后续执行规则

- 修改后先跑相关测试；只有代码变化或未解决的问题需要时才扩大验证范围。
- 完整 race 与 A04 分别设预算并记录运行 ID，不因空 stdout 再启动一次。
- 工具返回过程会话 ID 时保留并继续跟踪；在退出码出现前，不执行依赖它的 push、PR或合并动作。
- 已用两秒命令复现：首个结果同时出现空 stdout、无退出码和有效 `session_id`，继续读取该会话后才得到成功退出。命令只启动一次；这个场景不能用空输出作为重试依据。
- 不因诊断文档、报告格式或工具小改重复跑完整20分钟 A04；需要真实 MySQL/负载证据时才执行，并先说明预计时长。
- 当前未修改密码哈希参数、race覆盖、故障断言或性能验收门槛。

诊断工具回归验证：

```sh
python3 -m unittest discover -s tests/diagnostics -p 'test_*.py' -v
```

用例覆盖心跳/当前测试、非零退出、启动失败、超时后清理不响应 SIGTERM 的子进程、CI并行任务跨度与分页兼容性。

## 执行工具外层的计时

Python监控脚本从自身启动后开始计时，不能测到执行工具之前的权限/调度时间。调用端需另外记录往返时间，并保留过程会话ID和退出码，例如：

```javascript
const started = Date.now();
const result = await tools.exec_command({cmd: "true", yield_time_ms: 1000});
text({
  tool_round_trip_ms: Date.now() - started,
  session_id: result.session_id ?? null,
  exit_code: result.exit_code ?? null,
  output: result.output,
});
```

有 `session_id` 且无 `exit_code` 时继续读取该会话，直到拿到最终结果；这正是此前只输出 `result.output` 会丢失的部分。

2026-11-05 分支审查新增真实信号回归：独立测试会话运行中取消、schema/user/Redis 创建后的取消、Popen 已生成子进程但尚未返回时取消；均先在旧实现上失败，再验证修正。真实 API 就绪后的 SIGTERM 清理结果见 [本轮审查记录](PENDING_BRANCH_REVIEW.md)。
