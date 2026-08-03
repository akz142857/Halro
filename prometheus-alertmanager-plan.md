# Prometheus、Alertmanager 与独立 dead-man 实施方案

状态：仓库实现完成；本地运行验证完成；目标环境生产准入待执行
适用范围：Heimdall Standalone；为后续 HA/Cluster 保留兼容边界

## 1. 结论

Heimdall 的可观测性链路固定为：

```text
Heimdall Metrics listener
  /metrics + authentication
            |
            v
Prometheus
  recording rules + alert rules
            |
            v
Alertmanager -----------------> Operations Contact Point

独立 heimdall-deadman --------> Independent Receiver/Contact Point
  checks Heimdall, Prometheus and Alertmanager
```

Prometheus rule files是唯一告警评估权威。Ledger 仍是账务权威；Metrics
只是派生观测。Core 由 Prometheus、Alertmanager 和部署在独立故障域的
dead-man 构成。

仓库实现完成不等于生产准入完成。只有 Phase D 的目标环境证据和
Application、Security、SRE、Platform 四方签字齐全，才能声明
`Prometheus/Alertmanager Core Production Admitted`。

## 2. 已完成的仓库交付

- 独立 Metrics listener、Bearer/版本化 credential 和 mTLS；
- Prometheus scrape、8 条 recording rules 和 16 条 alert rules；
- Alertmanager operations 与 Watchdog 路由；
- Prometheus/Alertmanager Linux Compose 和 macOS 本地 Secret override；
- 7 天 retention、精确 5 GiB size limit、70% warning 和 85% critical；
- `promtool`/`amtool` 配置与语义 fixtures；
- 可配置回环端口的隔离 runtime smoke；
- 正式 `heimdall-deadman` 二进制、Dockerfile、systemd unit、配置和事件
  schema、receiver contract、持久化状态/outbox、审计与重试；
- digest 固定、SBOM、镜像扫描和 Release 产物；
- Runbook、安全 RFC、容量模型、实现证据和生产准入清单；
- Go、race、vet、前端构建以及多角色安全/SRE/验收评审。

## 3. 指标与标签契约

- 应用指标使用 `heimdall_` 前缀；标准 Go/process 指标保留生态名称；
- duration 使用秒，byte 使用 `_bytes`，counter 使用 `_total`；
- `status`、`direction`、`reason`、`provider_type` 是有限枚举；
- 禁止提示词、响应、原始错误、Request ID、Key、Project、来源 IP 和
  credential 片段进入指标或标签；
- `environment`、`region`、`cluster` 和 `instance` 由 Prometheus target
  labels 提供；
- 所有跨环境查询和告警必须显式保留环境与集群边界；
- histogram 使用 classic buckets，checkpoint/replay 保证幂等。

详细契约维护在 `docs/observability/metrics-contract.md` 和
`docs/metrics-reference.md`。

## 4. 网络与身份

单机 MVP 使用 host network 保留 loopback listener：

- Heimdall Metrics：`127.0.0.1:9090`；
- Prometheus：`127.0.0.1:9091`；
- Alertmanager：`127.0.0.1:9093`。

loopback 只允许用于单租户主机或独立 network namespace。跨 namespace
或跨主机生产流量必须使用 TLS 1.2+ 和双向 workload identity。私网或
NetworkPolicy 只能作为纵深防御，不能替代身份认证。

Prometheus 与 Alertmanager 管理面不得直接暴露公网。需要访问 UI/API、
reload、admin、silence 或 config 端点时，必须经过认证代理或受限运维
网络，并实施最小权限和不可变审计。

## 5. Secret 与 credential

- Secret 不进入 Git、YAML、环境变量、进程参数、日志、截图或 CI artifact；
- Linux 从 `/run/heimdall-observability` 挂载；
- macOS 本地开发使用 `/private/tmp/heimdall-observability` override；
- 生产使用目标 Secret Store 和版本化 Metrics credential；
- rotation 支持 active/retiring 有限重叠、热重载、立即吊销和恢复不复活；
- credential 审计链必须锚定到独立不可变存储。

## 6. 告警与 dead-man

告警覆盖：

- Heimdall target 缺失或不可用；
- 请求错误率、Provider/Deployment 健康、fallback 与 capacity；
- Ledger/WAL、Usage analytics 和应用告警投递；
- Prometheus rule/config、TSDB/WAL 磁盘；
- Alertmanager target 与通知失败；
- 持续 `Watchdog` check-in。

独立 dead-man 提供第二故障域：

1. 外部 receiver 在 `Watchdog` 停止后报警并在新 check-in 后恢复；
2. `heimdall-deadman` 通过 authenticated HTTPS 直接探测 Heimdall、
   Prometheus 和 Alertmanager；
3. Prometheus freshness 查询只接受有限的成功 `up == 1` 原始样本年龄；
4. 探针发送 down/up transition 和带 TTL 的自身 heartbeat；
5. outbox 严格 FIFO，通知与探测解耦，状态和审计 fsync 持久化；
6. probe、receiver、身份、存储、网络和最终通知渠道必须与 Core 独立。

仓库测试只能证明行为，不能证明故障域独立。

## 7. 容量与保留

- 本地基线 retention 为 7 天、size limit 为 5 GiB；
- 3.5 GiB/70% 触发 warning，4.25 GiB/85% 触发 critical；
- series 在 80% admission budget 预警，未经评审不得超过 100%；
- 生产必须通过 24 小时规模化 soak 冻结 series、bytes/series/day、
  TSDB/WAL 增长、PromQL/rule evaluation 成本和扩容提前量。

## 8. 仓库结构

```text
deploy/observability/
  compose.example.yaml
  compose.macos.example.yaml
  external-probe.example.yaml
  smoke.sh
  validate.sh
  prometheus/
    prometheus.yml
    recording-rules.yml
    alert-rules.yml
    rule-tests.yml
  alertmanager/
    alertmanager.yml
  external-probe/
    Dockerfile
    RECEIVER-CONTRACT.md
    config.example.yaml
    config.schema.json
    event.schema.json
    heimdall-deadman.service
    probe.sh
cmd/heimdall-deadman/
internal/deadman/
docs/observability/
```

## 9. 实施阶段

### Phase A：契约与 Monitoring MVP（仓库完成）

- 指标、标签、series、规则和 histogram 契约；
- Prometheus/Alertmanager Compose、Secret 注入和自监控；
- recording/alert rules、Runbook、fixtures、runtime smoke；
- 固定镜像、UID、SBOM 和供应链验证。

### Phase B：生产安全能力（代码完成，目标集成待 Phase D）

- 版本化 Metrics credential、轮换、重叠、吊销和审计；
- Metrics listener mTLS 和真实握手测试；
- 恢复防复活、原子替换和热重载。

### Phase C：指标质量（仓库完成）

- classic request/attempt histogram 与 checkpoint/replay；
- build/runtime 指标和 Provider/Deployment capacity 分母；
- scrape 并发限制、write deadline、慢客户端和 series 回归测试。

### Phase D：生产准入（目标环境待执行）

- 接入目标 PKI、Secret Store、身份代理、网络和持久化；
- 验证真实 Contact Point firing/resolved；
- 在独立故障域部署 dead-man 与 receiver；
- 执行 Prometheus/Alertmanager/probe 停止恢复、TSDB 满盘/只读演练；
- 验证 SSRF/egress、不可变审计、备份恢复、RPO/RTO；
- 执行生产规模 24 小时 soak、升级和回滚；
- 归档 evidence ID 并取得四方签字。

## 10. 验证命令

```sh
go test ./...
go test -race ./...
go vet ./...
npm test --prefix web
npm run build --prefix web
./deploy/observability/validate.sh
./deploy/observability/smoke.sh
docker compose -f deploy/observability/compose.example.yaml config --quiet
go run ./cmd/heimdall-deadman \
  -config deploy/observability/external-probe/config.example.yaml \
  -check-config
```

## 11. 完成定义

仓库实现完成要求：

- 配置、规则、fixtures、dead-man、CI、SBOM、文档和 smoke 全部存在；
- 自动测试、race、vet、Prometheus/Alertmanager 校验和 runtime smoke 通过；
- 本地部署中 Heimdall、Prometheus、Alertmanager targets 全部为 `up`；
- Watchdog 到达 Alertmanager；
- 安全、SRE 和验收评审无仓库级阻塞项。

生产准入完成要求：

- `docs/observability/admission-checklist.md` 所有门禁具有真实 evidence ID；
- Application、Security、SRE、Platform 四方签署 Go；
- 状态才能变更为 `Prometheus/Alertmanager Core Production Admitted`。
