# IM 两实例负载工作包

基线：`b9957ea`。本工作包验证 A07 单聊负载与恢复，不关闭整个 A07，也不替代同一性能表中的十个真实视频处理及最终全组件 Compose/浏览器验收。

## 固定范围与退出条件

- 真实生产 API 两进程、独立 MySQL/Redis 连接；随机空 schema/专用用户和临时 Redis，拒绝复用演示数据。
- 20 个不同账户、一账户一条 WebSocket，两个进程各 10 条；每账户串行发送 50 条消息，20 个发送者并发，收件人位于另一个进程。每轮 1,000 条、256 字节正文、最多 20 条等待发送 ACK 的消息，运行三轮，不将此闭环负载视为最大吞吐。
- 逐帧校验 sender/recipient/body/client_id/server ID，完整统计 1,000 个发送 ACK 和 1,000 个接收消息；任意错误、未知 ID、错收件人、同键不同 ID、连接丢失或超时都失败。实时提示可重复，但重复帧单独统计，SQL 的消息身份必须唯一。
- 单调时钟记录写入前到发送 ACK、写入前到接收事件的客户端延迟，保留每条原始样本、P50/P95/P99/max、测量时长与数量；不设置未经验证的绝对毫秒阈值。
- 采样两个 API 的 Prometheus 进程 CPU、RSS、goroutine 和 WebSocket 连接数。CPU 是进程累计值差；RSS 为采样峰值，不是精确内核峰值。记录采样开销、构建是否开启 race、宿主机 CPU/内存及组件版本。MySQL/Redis CPU 不据此声称已测量。
- 每轮通过另一个 API 查询精确未读集合并逐设备 ACK，随后重复至少一次发送，要求返回原 ID；SQL 最终核对每条身份和正文，不只检查总数。
- 关闭一条接收连接，再提交 125 条消息；重连同设备只重放 100 条并发同步标记，完整未读/历史以 37 条分页核对、ACK 后为零，新设备保留独立积压。最终 SQL 为 3,125 条唯一消息。
- 负载只使用生产公开 HTTP/WebSocket 接口；SQL 只做隔离前置检查与最终状态核对。报告不包含 JWT/密码/DSN。
- 外层启动器管理子进程组，正常/失败/超时后都先停进程，再移除 Redis、SQL、临时目录，核对零残留。CI 对最终提交运行现有普通/race/真实组件矩阵与本负载。

## 执行入口

```sh
python3 tests/integration/im_smoke.py \
  --go-bin /usr/local/go/bin/go \
  --mysql-container gostream-mysql-1 \
  --mysql-addr 127.0.0.1:3307 \
  --suite load --report-file /tmp/gostream-im-load.json
```

默认 recovery 模式维持原来的 API/test race 构建；load 默认常规构建。`--race-load` 仅用于单独的竞态回归，结果标注 race，不与常规延迟混合。`--check-timeout-cleanup` 只允许 recovery。直接 Go 入口必须显式设置负载开关与隔离环境，普通本地测试跳过。

实际结果、独立审核及最终候选 CI 待实现后补充。单机回环 CI 的数据不能推算生产容量；没有群聊扇出、媒体、长稳态运行或跨主机网络测量。
