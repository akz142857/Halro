# HA 健康观测长跑采样

`tests/ha-health-soak/collector.py` 每隔不超过 30 秒通过操作员 mTLS 身份
只读请求独立健康服务的 `/api/health`，把每次观测的五张状态卡、成员状态、
服务端观测时间和采集失败原因写入私有 JSONL。它不发送 Gateway 写请求，
不调用 Provider，也不执行提升、播种或故障注入。它只提供连续观测材料，
**不能单独签收 72 小时 HA soak、RTO/RPO 或 G0–G7**。

运行环境应已有经批准的 CA、操作员证书和私钥文件。采样器只读取文件，
不复制证书到输出目录、不记录密钥、完整响应或请求 URL。不要为了运行本工具
从 Kubernetes Secret 导出私钥到仓库或共享目录。输出目录必须不存在；程序以
`0700` 新建目录，以 `0600` 创建并逐行 fsync `samples.jsonl`。

先做短时工具 smoke：

```sh
python3 tests/ha-health-soak/collector.py \
  --url https://ha-health.example.internal/ \
  --ca /approved/path/ca.crt \
  --cert /approved/path/operator.crt \
  --key /approved/path/operator.key \
  --environment kind-local --cluster CLUSTER_ID \
  --members halro-0,halro-1,halro-2 \
  --duration-seconds 120 --interval-seconds 15 \
  --output /private/evidence/ha-health-smoke-001
```

正式候选观测至少 259200 秒，并要求精确候选 SHA、镜像、配置和规则摘要：

```sh
python3 tests/ha-health-soak/collector.py \
  --url https://ha-health.example.internal/ \
  --ca /approved/path/ca.crt \
  --cert /approved/path/operator.crt \
  --key /approved/path/operator.key \
  --environment TARGET_ENV --cluster CLUSTER_ID \
  --members halro-0,halro-1,halro-2 \
  --duration-seconds 259200 --interval-seconds 15 \
  --candidate-sha FULL_40_HEX_SHA \
  --image-digest sha256:FULL_64_HEX_DIGEST \
  --config-sha256 FULL_64_HEX_SHA256 \
  --rules-sha256 FULL_64_HEX_SHA256 \
  --output /private/evidence/ha-health-candidate-001
```

每次查询最多读取 256 KiB；HTTP 非 200、TLS/网络失败、环境或集群错配、
成员清单不一致、无效状态卡及服务端观测时间偏差超过一分钟，均记录为
`unavailable`，不填入上一轮的健康值。程序使用单调时钟安排采样，`SIGTERM`
或手工中断会使 `summary.json` 的 `collection_complete=false`；异常终止后
若没有 summary，不能声称长跑完成。`summary.json` 另外区分
`collection_complete`（运行窗口结束）、`sampling_continuous`（未错过计划
采样且单调间隔不超过 30 秒）与 `health_endpoint_coverage_complete`（前一
条件成立且每次健康接口都有可解析响应）。后者不代表成员抓取完整或集群
一直健康；还须检查五卡分布和
原始行。候选 SHA 与摘要由操作员填写，本工具不会独立证明它们对应实际 Pod
与文件。`kind` 只区分
`smoke_only` 和 `candidate_observation_only`，`ha_acceptance` 始终为
`NOT_RUN`。即使 72 小时观测全绿，还须独立核对源数据、目标负载、故障注入、
客户端最终结果、告警送达、资源趋势、正式 RTO/RPO 和签署记录。

采样中断或目标版本变更后重新建一个目录并保留旧证据，不拼接成连续 72 小时。
核对每行时间、总样本数、`unavailable` 次数、五卡分布和证据文件 SHA-256；
把原件转交独立不可变存储后再从归档副本读回。此采样文件不是成员迁移日志
或采集器持久事件链的归档回执。
