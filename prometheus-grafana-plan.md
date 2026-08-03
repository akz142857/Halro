# Prometheus 与 Grafana 可观测性实施方案

状态：Repository implementation complete; production admission pending target-environment evidence
适用范围：Heimdall Standalone；为后续 HA/Cluster 保留兼容边界
目标：在不削弱 Metrics 安全边界、不引入高基数或敏感标签的前提下，提供可重复部署、可验证、可告警的 Prometheus 与 Grafana 集成。

文档职责：本文是项目计划和生产准入契约，不替代详细架构 ADR、安全 RFC、告警规则文件或运维 Runbook。详细契约维护在 `docs/observability/metrics-contract.md`、`security-rfc.md`、`alerting-rules.md`、`capacity-model.md` 和 `operations-runbook.md`；仓库验证映射维护在 `implementation-evidence.md`，生产证据索引维护在 `admission-checklist.md`。本文只保留范围、阶段、依赖、责任、工时、Go/No-Go 与关键决策摘要，避免同一约束在多处漂移。

## 1. 背景与结论

### 1.1 实施结果（2026-08-03）

- Phase A0/A1 的契约文档、Prometheus/Alertmanager rules、四组 Grafana Dashboard、固定 digest 的单机示例、CI 校验与 SBOM 任务已落库；
- Phase B 的版本化 credential、有限重叠轮换、独立吊销、恢复防复活、热重载和 Metrics listener mTLS 已实现并有自动测试；
- Phase C 采用 classic histogram，bucket 进入 Usage checkpoint/replay；build/runtime contract、真实 Provider/Deployment capacity 分母、scrape 并发上限和 write deadline 已实现；
- Phase D 必须依赖目标环境 PKI、Secret store、SSO/RBAC、独立通知路径、不可变审计和备份平台，不能在仓库内伪造完成。所有门禁默认 No-Go，并在 `docs/observability/admission-checklist.md` 中逐项记录阻塞与证据 ID。

因此，“仓库实现完成”不等于“生产准入完成”。只有 admission checklist 全部通过并取得 Application、Security、SRE、Platform 四方 sign-off，才能声明本方案整体完成。

Heimdall 已经提供独立 Metrics listener，并在 `/metrics` 输出 Prometheus text format。基础指标覆盖请求、Provider attempt、Token、成本、延迟总量、并发、fallback、Ledger/WAL、Usage analytics、Alert、策略拒绝以及 Provider/Deployment 状态；Metrics 默认启用 Bearer Token 鉴权，Token 从 Master Key 的独立 HKDF domain 派生。

因此，本项目不是从零引入 Metrics SDK，而是完成以下交付闭环：

1. 规范并补齐现有指标契约；
2. 提供 Prometheus 抓取和 Grafana provisioning；
3. 提供 Overview、Reliability、Accounting、Provider 四组 Dashboard；
4. 提供经过验证的告警规则和处置说明；
5. 为本地、单机生产和未来多节点部署定义清晰的网络及标签边界。

第一阶段不引入 OpenTelemetry Collector、分布式追踪或日志聚合。这些能力应在独立方案中评估，不能成为基础监控上线的前置条件。

本计划将“可本地运行的 Monitoring MVP”和“生产安全前置能力”分开管理：MVP 可以复用现有派生 Token 和 loopback listener；任何生产准入必须先完成版本化 Metrics credential 和跨节点双向身份认证。SSO/MFA、加密备份等组织级平台能力是外部依赖，本项目负责验证其已存在且适用于本部署，不负责从零建设这些平台能力。

## 2. 目标与非目标

### 2.1 目标

- Prometheus 能持续、经过认证地抓取每个 Heimdall 实例；
- Grafana 能通过仓库内版本化文件自动配置数据源、Dashboard 和告警；
- 运维人员能快速回答流量、错误、延迟、成本、容量、账务健康和 Provider 健康问题；
- 告警覆盖服务不可用、账务风险、队列积压、持续错误和上游异常；
- 所有查询、标签和告警消息不包含 Secret、请求内容、原始错误、Key、IP 或 Request ID；
- 本地验证和生产部署使用相同的指标及规则定义；
- 配置、Dashboard、规则和 Runbook 都可代码审查、版本化和自动验证。

### 2.2 非目标

- 不由 Grafana 或 Prometheus 承担 Heimdall 的账务权威状态；Ledger 仍是权威来源；
- 不把 Admin Dashboard 替换为 Grafana；两者面向不同操作场景；
- 不在第一阶段导出 Project、Gateway Key、Route、模型、请求 ID、来源 IP、提示词、响应内容或原始错误标签；
- 不在第一阶段建设日志聚合、Trace、SLO 自动预算或长期账单系统；
- 不允许为了抓取方便而将未鉴权的 Metrics endpoint 暴露到公网；
- 不在本方案中实现 HA/Cluster，但指标命名和 Dashboard 查询不得阻碍未来演进。

## 3. 当前基线

### 3.1 已具备

- `server.metrics_listen` 独立 listener，默认 `127.0.0.1:9090`；
- `metrics.enabled` 和 `metrics.require_auth` 配置；
- `GET /metrics` Prometheus text format；
- `GET /health/live` 进程存活检查；
- `heimdall metrics token --config <path>` 本地生成 Metrics Bearer Token；
- Token 不写入 YAML，运行时以 SHA-256 常量时间比较；
- 业务、Ledger、Usage analytics、Alert、策略拒绝和 Provider/Deployment 指标；
- 容器暴露 `9090` 端口。

### 3.2 当前缺口

- 仓库没有标准 Prometheus scrape 配置；
- 仓库没有 Grafana datasource、Dashboard 和 alerting provisioning；
- 没有可直接运行的本地监控栈或生产部署示例；
- 延迟指标只有 `_sum` 和 `_count`，只能可靠计算平均值，不能计算 P50/P95/P99；
- Go process/runtime 指标覆盖不足，当前主要只有 goroutine 数；
- 缺少实例构建信息和 scrape 目标维度约定；
- `docs/metrics-reference.md` 与实际代码输出的完整指标集合需要同步；
- 缺少 PromQL、告警阈值、持续时间和处置 Runbook；
- 缺少对 Dashboard、规则和 Metrics 格式的自动验证。

## 4. 总体架构

```text
Heimdall Metrics listener
  /metrics + Bearer authentication
            |
            | private network scrape
            v
Prometheus --------------------> Grafana
  recording/alert rules           datasource
            |                     dashboards
            v                     alerting UI
Alert evaluation
            |
            v
Configured contact point
```

生产环境的 Metrics 传输和身份边界以第 6.2 节为唯一规范；架构图中的 `private network scrape` 不表示私网可以替代加密或双方身份认证。

Grafana 只访问 Prometheus，不直接访问 Heimdall Metrics endpoint。Metrics Token 只进入 Prometheus 的 Secret 管理路径，不进入 Grafana、Dashboard JSON、Git、日志、环境变量或命令行参数。

## 5. 指标契约

### 5.1 命名与单位

- 所有 Heimdall 业务指标继续使用 `heimdall_` 前缀；标准 Go/process collector 保留生态兼容的 `go_*` 和 `process_*` 命名；
- Counter 使用 `_total`；
- 时间统一使用 seconds；
- 字节数使用 `_bytes`；
- 比率不直接由应用导出，优先由 PromQL recording rule 计算；
- 指标 HELP、TYPE 和 label set 属于兼容性契约；删除或改名需要 release note 和迁移窗口。

### 5.2 标签策略

允许的标签必须来自有限、受控枚举或受管理资源 ID：

- 允许：`status`、`direction`、`reason`、`provider_type`；
- 有条件允许：`provider_id`、`deployment_id`，必须设置实例内资源数量上限并监控 series 数量；
- 由 Prometheus target labels 或 `relabel_configs` 注入：`job`、`instance`、`environment`、`region`、`cluster`；这些标签必须使用受控、非机密、不可嵌入客户名称的枚举；
- 禁止：Project、Gateway Key、Route、原始模型名、Request ID、用户 ID、来源 IP、URL、Prompt、Response、原始错误文本和动态状态描述。

未来 Cluster 模式下，不把 node ID 直接写入应用指标；由 scrape target 的 `instance` 或静态 relabeling 提供节点维度。标签契约保留由服务发现注入的 `shard`、`role` 或 `authority`：process/runtime 指标按 node 观察，Ledger/Usage 派生指标只从 authoritative owner 汇总，Dashboard 不得对 leader/follower 或不同 authority 无条件求和。

标签契约由 CI 强制执行：维护 label-name allowlist、每个指标允许的 label set、受控枚举和禁止模式；测试必须拒绝新增 Project/Key/Route/model/IP/Request ID 等标签，以及把域名、客户编码或 Credential 片段写入 `provider_id`/`deployment_id`。

当前不存在已冻结的 authority-scoped Metrics 契约，因此本文不提供 `authority="writer"` 的示例 PromQL。特别是 `heimdall_requests_total` 属于 Gateway 请求执行指标，与 Ledger writer authority 无关；未来多个节点都能处理请求时按 writer 过滤会漏计流量。只有 Cluster ADR 逐指标冻结 ownership、replica emission 和聚合语义后，才能为相应指标增加 `shard/role/authority` 过滤。CI 必须拒绝 Dashboard 或 recording rule 对未声明 authority 语义的指标使用这些标签。process/runtime 指标始终按 `instance` 保留节点维度。

### 5.3 Series 基数预算

基数预算必须计算而不是只做原则评审：

```text
total_series_per_instance =
    fixed_application_series
  + provider_count * series_per_provider
  + deployment_count * series_per_deployment
  + histogram_bucket_count * histogram_label_combinations
  + go_process_series

total_series_per_environment =
    total_series_per_instance * scraped_instance_count
  + recording_rule_series
  + platform_monitoring_series
```

`histogram_bucket_count` 必须包含 `_bucket`、`_sum` 和 `_count` 的实际序列成本。`histogram_label_combinations` 是所有允许 label value 数量的笛卡尔积，不得用平均值估算。`capacity-model.md` 必须记录当前值、Provider/Deployment 上限、实例数、12 个月增长假设和 Prometheus 保留期；运行时以 `scrape_samples_post_metric_relabeling`、TSDB head series 和磁盘增长交叉验证。达到预算 80% 触发 warning，达到 100% 阻止新增指标/资源进入生产，除非重新评审容量。

存储基线使用实际 soak 测得的 `bytes_per_series_day`：

```text
required_tsdb_bytes =
    bytes_per_series_day
  * admitted_series
  * retention_days
  * 1.30
```

其中 30% 是 compaction/WAL/增长余量，不替代文件系统保留空间。Phase A1 的本地 MVP 默认 retention 为 7 天并设置明确磁盘上限；Phase D 必须用目标环境数据重新计算，70% 水位 warning、85% critical，且预计达到 critical 的时间必须早于值班响应和扩容周期。

### 5.4 第一阶段保留指标

- 请求：`heimdall_requests_total`、`heimdall_attempts_total`；
- 用量：`heimdall_tokens_total`、`heimdall_cost_usd_total`；
- 延迟：`heimdall_request_duration_seconds_*`、`heimdall_attempt_duration_seconds_*`；
- 并发：`heimdall_active_requests`、Provider/Deployment active requests；
- 路由与策略：fallback、policy rejection；
- 账务与队列：WAL append errors、durable queue、analytics queue/drop/lag；
- Alert：delivery outcome、queue depth、Token Guard dropped events；
- 健康：Provider adapter、Deployment active-probe health；
- Runtime：goroutine。

### 5.5 建议新增指标

以下变更需要单独实现和测试，不是部署配置的前置条件：

- `heimdall_build_info{version,commit}`，固定值 `1`，复用现有 `internal/buildinfo`；完整 commit 只保留在受控 Metrics 边界内，不进入外部通知；
- 请求和 Provider attempt latency histogram；
- process CPU、RSS、open file descriptors、GC pause/collection 等标准 runtime 指标；
- Metrics render/scrape 自身错误计数（如实现存在可失败路径）；
- 配置 reload/restart 版本信息，如未来支持热加载再引入。

Histogram 不是单纯的 exporter 增量。Phase A0 必须通过 ADR 冻结权威事件源：如果在 Gateway 热路径记录，需要解释它与 Ledger-derived Usage 指标的语义差异；如果由 Usage aggregate 生成，bucket counts 必须进入 checkpoint/replay，并以 Event ID、authoritative watermark 和 checkpoint schema 保证通知丢弃、Ledger catch-up、重启和重复回放下不会重复累计。

Histogram bucket 必须由生产基线决定，不直接照搬通用默认值。初始候选可覆盖 10ms 至 120s；实现前必须选择 classic 或 native histogram，并用“bucket 数 × label 组合 × 实例数”计算 series 预算。第一版 histogram 不携带 Provider/Deployment 等业务 ID 标签。

标准 Go/process collector 的版本、启用集合和 label contract 必须与目标 Prometheus client 版本一起锁定，并用 golden/contract test 保护 Dashboard 依赖；Dashboard 不得假定所有平台都存在 FD、RSS 等 OS-specific 指标，对缺失序列显示 `N/A`。Phase C 必须形成 classic/native histogram 决策记录，包含 Prometheus/Grafana 版本兼容、remote write、长期存储、查询函数、迁移和回滚，不允许把选型继续留到实现中临时决定。

## 6. Prometheus 设计

### 6.1 抓取配置

仓库应提供模板化 scrape 示例，要求：

- 固定 `metrics_path: /metrics`；
- 通过只读文件或平台 Secret mount 注入 Bearer Token，文件 owner/mode 必须保证仅 Prometheus 的非 root 运行身份可读，目标权限为 `0400` 或 `0440`；
- 禁止把 Token 直接提交到配置；
- 禁止通过环境变量、Compose interpolation、进程参数或临时渲染文件注入 Token；
- 为 `environment`、`region`、`cluster` 提供 target labels 或 `relabel_configs` 示例；`external_labels` 只标识 Prometheus 自身并用于远端通信/告警，不假定它自动成为本地 scraped series 标签；
- scrape interval 初始为 15s，evaluation interval 初始为 15s；
- scrape timeout 必须小于 scrape interval；
- 生产保留期和存储容量由部署环境配置，不在应用中写死。

### 6.2 传输与 Metrics 凭据生命周期

生产前置能力必须交付版本化 Metrics credential，而不只是形成决策。契约至少包括：

- 凭据是实例级、环境级还是 Prometheus 身份级；
- 泄露后可独立立即吊销，不旋转 Master Key；
- 支持带版本/epoch 的新旧 Token 短暂重叠窗口；
- Prometheus 如何执行 Secret 文件原子替换、reload、`up` 验证、撤销旧 Token 和失败回滚；
- Master Key 轮换时如何保持抓取连续，并保证它不成为 Metrics Token 日常轮换或吊销机制；
- Token 轮换、吊销和失败是否产生不含 Secret 的审计证据。

当前实现由 Master Key 确定性派生单一 Token，不具备独立吊销和双 Token 重叠能力。它只允许用于本地开发、测试和 Monitoring MVP，不满足生产准入；维护窗口或风险审批不能替代真实吊销能力。生产版本必须使 credential version/epoch 进入受保护的持久状态，允许至少一个 active 和一个 retiring Token，在重叠窗口结束后拒绝旧 Token，并保证备份恢复不会重新激活已吊销版本。

跨 network namespace 或跨主机生产抓取必须满足：

- TLS 1.2 或更高版本，并验证服务端证书和主机身份；
- 必须具备客户端与服务端双向身份认证：mTLS、提供等价 workload identity 的 service mesh，或受认证本地代理；仅服务端 TLS 加 Bearer Token 不满足跨主机生产准入；
- CA、服务端证书和客户端证书有轮换、失效和过期告警；
- TLS/Token 更新失败时保留上一份可用配置，并按 fail-closed 原则停止错误发布；
- 传输、凭据轮换和证书过期场景纳入集成验收。

### 6.3 网络拓扑

支持三种明确拓扑：

1. **同一 network namespace**：保留 `127.0.0.1:9090`，Prometheus 进程、host-network sidecar 或受控本地代理通过 loopback 抓取；这是 Phase A1 唯一承诺不修改 Heimdall listener/TLS 代码的拓扑。Loopback 只隔离远程网络，不能隔离同宿主机进程；裸 loopback 仅允许单租户或所有本地进程都受同一信任域控制的主机；
2. **普通容器 bridge 网络**：当前 Heimdall 禁止 Metrics 在无 TLS 时绑定非 loopback，且 TLS 是 Gateway/Admin/Metrics 的全局开关；因此不能把 `0.0.0.0:9090 + 私有网络 + Bearer` 描述为零代码接入。实施前必须选择全局 TLS、受认证 loopback sidecar/proxy，或单独设计 per-listener TLS/metrics-only 监听能力；
3. **Kubernetes**：ClusterIP/PodMonitor/ServiceMonitor 只提供发现与可达性；跨 Pod 抓取仍必须通过 TLS/mTLS 或 service mesh 身份，Token/证书通过只读 Secret mount 注入。

共享宿主机、多租户 CI runner、托管开发机或存在不可信本地进程时，不允许使用裸 host-network/loopback 作为安全边界。必须使用独立 network namespace、专用主机、Unix socket 加受认证代理，或提供等价进程身份隔离的机制。Token 文件权限只能保护 Token 内容，不能阻止其他本地进程向 `127.0.0.1:9090` 发起请求。

任何非 loopback Metrics listener 必须满足第 6.2 节以及私有网络 ACL、安全组/NetworkPolicy。仓库已实现专用 Metrics mTLS，但示例 Compose 仍只覆盖单机 loopback；普通 bridge/Kubernetes 只有完成目标环境 PKI、Secret 和网络策略集成验收后才能声明受支持。

### 6.4 Recording rules

建议先提供稳定的派生序列，Dashboard 和告警尽量依赖 recording rules：

```promql
sum by (environment, region, cluster) (
  rate(heimdall_requests_total[5m])
)
sum by (environment, region, cluster) (
  rate(heimdall_requests_total{status="error"}[5m])
)
/
clamp_min(
  sum by (environment, region, cluster) (
    rate(heimdall_requests_total[5m])
  ),
  0.001
)
sum by (environment, region, cluster) (
  rate(heimdall_request_duration_seconds_sum[5m])
)
/
clamp_min(
  sum by (environment, region, cluster) (
    rate(heimdall_request_duration_seconds_count[5m])
  ),
  0.001
)
sum by (environment, region, cluster, instance) (
  rate(heimdall_request_duration_seconds_sum[5m])
)
/
clamp_min(
  sum by (environment, region, cluster, instance) (
    rate(heimdall_request_duration_seconds_count[5m])
  ),
  0.001
)
(heimdall_usage_queue_depth / heimdall_usage_queue_capacity)
and on (environment, region, cluster, instance)
(heimdall_usage_queue_capacity > 0)
```

具体 recording rule 名称在实现 PR 中冻结，固定窗口应进入名称，例如 `...:rate5m`。Overview 使用 environment/region/cluster 聚合延迟；Reliability 使用保留 `instance` 的独立规则。除法必须避免零分母；queue capacity 为 0、missing 或 disabled 时，公共 recording rule 返回无序列，Dashboard 显示 `N/A`，告警使用同一规则而不是另一套 clamp 口径。低流量错误率告警必须同时设置最小请求量，避免单次失败造成噪声。所有 Dashboard 和告警查询必须显式选择 `environment` 和 `cluster`，不得让一个环境的流量稀释另一个环境的故障。Dashboard 临时查询使用 Grafana `$__rate_interval`；recording rules 使用固定窗口。

## 7. Grafana 设计

### 7.1 Provisioning

仓库内版本化以下文件：

- Prometheus datasource provisioning；
- Dashboard provider provisioning；
- Dashboard JSON；
- 单一告警规则来源：默认由 Prometheus rule files 评估，Grafana 只展示状态；如果目标环境明确选择 Grafana-managed alerting，则不得同时加载等价 Prometheus rules，CI 必须检查重复规则；
- contact point 仅提供无 Secret 示例，实际地址和签名 Secret 由部署平台注入。

实现时固定 datasource、folder 和 dashboard UID，并给 provisioning 文件设置明确版本。生产 provisioned Dashboard 默认不可编辑；如允许临时 UI 编辑，必须定义导出、评审、覆盖和漂移检测流程。Datasource URL、组织和环境通过受审查的部署模板配置，不假定 Grafana 会对任意 Dashboard JSON 字段执行环境变量替换。

### 7.2 Dashboard 清单

#### Heimdall Overview

- 当前 QPS、成功率、错误率；
- 平均请求延迟；新增 histogram 后显示 P50/P95/P99；
- Provider attempt rate 与 request rate 对比；
- input/output token rate；
- 成本速率与 scrape 观察窗口内的 `increase`；进程重启会重置 counter，不能把该 Panel 称为权威账期累计，权威成本仍来自 Ledger；
- active requests、fallback rate、policy rejection rate；
- Deployment unhealthy 数量；
- 当前版本和实例数。

#### Heimdall Reliability

- 请求错误和 Provider attempt 错误趋势；
- fallback 与各类 policy rejection；
- active requests 和容量趋势；
- goroutine、CPU、RSS、FD、GC；
- scrape health、进程重启和实例缺失；
- Prometheus scrape duration/sample count、rule evaluation/config reload/notification failure、TSDB/WAL/磁盘水位；
- 按 `instance` 展示，不使用业务敏感标签。

#### Heimdall Accounting & Queues

- durable Ledger queue depth/capacity/ratio；
- WAL append error 增量；
- Usage analytics queue depth、dropped 增量和 lagging；
- Alert queue depth、delivery failed/dropped；
- Token Guard event dropped；
- 明确标注 Grafana 数据是派生观测，不是 Ledger 权威账务记录。

#### Heimdall Providers

- Provider adapter availability；
- Deployment active-probe status；
- Provider/Deployment active requests；
- request/attempt/fallback 的整体关联；
- 仅允许使用受管理的 Provider/Deployment ID，不展示 Credential、endpoint 或原始模型名。

### 7.3 Dashboard 约束

- 默认刷新间隔不低于 scrape interval；
- 每个 Panel 包含单位、说明和无数据语义；
- `No data`、`0`、`absent` 必须区分，尤其是尚未 probe 的 Deployment；
- 对 counter 使用 `rate`/`increase`，不直接展示原始 counter 作为瞬时值；
- Dashboard 查询必须适用于单实例和多实例；
- Dashboard JSON 禁止包含 datasource UID 之外的环境专属标识和任何 Secret。
- `provider_id`、`deployment_id`、成本、流量和拓扑属于内部敏感遥测；ID 不得由名称、域名、客户编码或 Credential 片段构成，生产和非生产 datasource 默认隔离授权。

## 8. 告警设计

第一批告警以低噪声和明确处置为原则：

| 告警 | 初始表达式契约 | `for` | 严重度 | 核心处置 |
|---|---|---:|---|---|
| HeimdallTargetDown | 对预期 target：`up{job="heimdall"} == 0`；另用独立 expected-target/config invariant 发现整个 target 从服务发现中消失 | 2m | critical | 检查进程、网络、TLS、Token 和 listener |
| HeimdallWALAppendErrors | 按 environment/cluster/instance：`increase(heimdall_wal_append_errors_total[5m]) > 0` | 0m | critical | 停止风险扩散，检查磁盘/EIO/权限和 accounting readiness |
| HeimdallUsageAnalyticsLagging | `heimdall_usage_analytics_lagging == 1` | 10m | warning | 检查 derivative queue 与 watermark catch-up |
| HeimdallLedgerQueueHigh | `heimdall_usage_queue_capacity > 0` 且 queue depth/capacity 超过 75%；missing 和 capacity<=0 单独处理 | 5m | warning | 检查磁盘延迟、Provider 流量和 Ledger 写入吞吐 |
| HeimdallAlertDeliveryFailing | `increase(heimdall_alert_delivery_total{status=~"failed|dropped"}[10m]) > 0` | 10m | warning | 检查 webhook、网络和接收端；防止监控盲区 |
| HeimdallDeploymentUnhealthy | `heimdall_deployment_up == 0`；absent 表示尚未 probe 或序列缺失，不能直接视为 unhealthy | 5m | warning | 检查上游状态、Credential、网络和限流 |
| HeimdallMultipleDeploymentsUnhealthy | 同一 environment/cluster 中已 probe Deployment 的 unhealthy 数量或比例超过 baseline 阈值 | 3m | critical | 判断 Provider/区域级联故障；抑制同根因的单 Deployment warning |
| HeimdallFallbackSaturation | `rate(heimdall_fallbacks_total[5m])` 相对 request/attempt rate 持续超过 baseline 阈值，且绝对流量超过最小值 | 5m | warning/critical | 检查首选 Provider 退化、剩余健康容量和 fallback 链耗尽风险 |
| HeimdallProviderCapacityPressure | Provider/Deployment active requests 相对受控容量持续超过阈值；容量指标未冻结前不得伪造比例 | 5m | warning | 检查排队、并发上限、吞吐下降和错误率联动 |
| HeimdallHighErrorRate | 10m 错误率超过经 baseline 冻结的阈值，同时 `increase(heimdall_requests_total[10m]) >= N` | 10m | warning | 比较 Provider attempt、fallback、rejection 和上游状态 |
| HeimdallNoTraffic | 预期有流量且 target 正常的环境中，请求增量为 0 | 15m | info/warning | 仅由环境开关和 expected-traffic 配置启用 |

实现 PR 必须把每条规则扩展为完整 PromQL、比较值、聚合维度、`for`、owner、service、category、severity、Runbook URL、summary 和 description，并提供 `promtool test rules` 的 firing、pending、recovery、counter reset、低流量、target absent、尚未 probe 和多 Deployment 级联测试向量。规则按 `environment/cluster/alertname` 分组；聚合 critical 抑制由同一根因产生的单 Deployment warning；部署维护通过有审批和到期时间的 silence 流程完成。

阈值在上线前通过 baseline 或 soak evidence 校准。告警 payload 使用 allowlist 模板，只引用受控枚举、稳定标识和需要认证的 Runbook 链接，不拼接动态错误、内部 URL、查询参数、客户标识、请求字段或 Secret。告警触发、恢复和通知送达都必须做一次端到端演练。

监控平台必须自监控 scrape duration/sample count、Prometheus TSDB/WAL/磁盘、rule evaluation、configuration reload、notification delivery，以及 Grafana datasource/provisioning 健康。至少由监控栈之外的探针观察 Heimdall 和 Prometheus，并提供 dead-man 或 synthetic notification，避免 Prometheus/Grafana 自身故障时静默失联。

## 9. 安全与隐私

- 生产始终保持 `metrics.require_auth: true`；
- Metrics Token 视为 Secret，与 Admin Session、Gateway Key、Provider Credential 分离；
- Token 仅通过 `0400/0440` 的只读 Secret file 或平台 Secret mount 进入 Prometheus；备份、临时目录、模板渲染和诊断采集必须排除 Secret；
- Token 不出现在 Git、Dashboard、Grafana datasource JSON、环境变量、进程参数、状态页面、日志、截图、CLI 参数或 CI artifact；
- Metrics 传输遵守第 6.2 节；Prometheus 和 Grafana 不直接暴露公网；如必须远程访问，置于 TLS、SSO/MFA 和网络访问控制之后；
- 禁止 Grafana 匿名访问和默认管理员凭据；Viewer/Editor/Admin、datasource、Explore、alert/contact-point 和 service account 权限分别验证。普通 Viewer 默认没有 Explore、任意 datasource query、原始数据导出或 datasource proxy 权限，因为 Dashboard 隐藏字段不能构成数据访问控制；需要原始遥测的 SRE 角色单独授权并审计。生产 Dashboard 修改通过 Git 流程回灌；
- Prometheus UI/API、reload、admin API 和 lifecycle endpoint 只允许运维网络或受认证自动化身份访问；未使用的写入/admin 功能默认关闭；
- Grafana datasource 使用最小权限的只读服务身份，service account 凭据具有轮换、吊销和审计要求；
- Prometheus 查询和 Grafana Panel 不导出提示词、响应、Request ID、用户标识或原始错误；
- 对 Provider/Deployment ID 进行受控基数审计；
- 通知模板、Grafana link、Runbook URL、external labels、silence/comment 和 webhook payload 使用 allowlist，并通过敏感 canary 快照测试；
- Grafana 登录、查询、导出、告警静默/修改、contact point/service account 变更，以及 Prometheus reload 都应进入脱敏审计；审计流发送到与被审计监控栈分离的 append-only 或具备不可变保留策略的存储，并通过签名/哈希链或平台原生完整性控制检测篡改。监控栈管理员不能同时删除审计证据；
- SSRF/任意出网边界覆盖 Grafana datasource、contact point/webhook、panel link、插件，Prometheus service discovery、remote write、Alertmanager webhook 和配置 reload；所有目标使用 allowlist、受控 DNS/代理和最小网络出口，拒绝 loopback、link-local、云 metadata、Unix socket、私有管理网段及重定向绕过，除非目标被逐项批准；
- Prometheus TSDB/Grafana DB 备份必须静态加密、密钥隔离、访问审计，并定义保留、销毁和恢复权限；恢复后不得重新启用已吊销 Token 或恢复明文 Secret；
- Metrics 属于内部敏感遥测，保留与备份策略需考虑流量、成本和资源拓扑泄露风险。

### 9.1 供应链与运行时加固

- Prometheus、Grafana及辅助镜像固定到经过评审的版本，生产建议固定 digest；校验工具必须与目标 Prometheus/Grafana 版本一致；
- CI 执行镜像漏洞、配置和 Secret 扫描，保存 SBOM，并定义紧急安全升级及回滚窗口；
- 禁止自动安装未批准的 Grafana 插件，使用显式 plugin allowlist；
- 容器使用非 root、只读根文件系统、最小 writable volume、drop capabilities、资源限制和受限网络出口；
- Compose 示例不得通过插值、日志或 `docker inspect` 可见字段传入 Secret；
- 生产前验证镜像签名或可信来源、文件权限、运行 UID、挂载和网络策略。

## 10. 仓库交付结构

建议新增以下结构，最终名称可在实现 PR 中微调：

```text
deploy/observability/
  README.md
  prometheus/
    prometheus.example.yml
    recording-rules.yml
    alert-rules.yml
  grafana/
    provisioning/
      datasources/
      dashboards/
      alerting/
    dashboards/
      heimdall-overview.json
      heimdall-reliability.json
      heimdall-accounting.json
      heimdall-providers.json
  compose.example.yaml
docs/
  observability-runbook.md
```

示例部署不得包含真实 Secret，不自动发布端口到公网，也不得暗示示例 Compose 等同于生产编排。第一版 Compose 必须采用 host network、共享 network namespace 或 loopback proxy；在 listener/TLS 架构决策完成前，不提供会诱导用户配置 `0.0.0.0:9090` 明文抓取的普通 bridge 示例。

## 11. 实施阶段

### Phase A0：Monitoring MVP 契约与基线

- 以实际 `/metrics` 输出为准盘点指标、TYPE、HELP 和 label set；
- 修正 `docs/metrics-reference.md` 与代码不一致之处；
- 记录代表性 idle、正常流量、Provider 故障和队列积压样本；
- 确认 Provider/Deployment 最大数量和 series 预算；
- 冻结 Metrics contract golden test：使用 parser 验证完整 HELP、TYPE、label set、转义和禁止标签；
- 决定告警规则的唯一权威来源：默认 Prometheus rules，或由目标环境显式选择 Grafana-managed rules；
- 冻结 target label schema、recording rule 聚合维度和未来 `shard/role/authority` 语义；
- 冻结普通容器/Kubernetes 所需的双向身份认证接口，不把普通 bridge HTTP 视为现有能力；具体核心安全开发进入 Phase B；
- 冻结版本化 Metrics credential 的外部契约和数据迁移边界；具体实现进入 Phase B；
- 通过 ADR 决定 histogram 的权威事件源、Event ID 去重、watermark、checkpoint schema、Ledger replay、通知 drop/catch-up 和重启语义；这里只冻结数据模型和评估方法，不冻结最终 buckets；
- 创建 `docs/observability/` 下五个子文档，至少包含 owner、状态、范围、决策链接、未决项和完成标准；A0 之后的详细内容只在对应子文档维护。

退出条件：指标清单、contract test、标签政策、series 预算方法、规则归属和 histogram 数据模型通过评审；Phase B 的双向身份及 credential 外部接口、验收契约已经冻结，但不要求其实现或最终安全验证在 A0 完成；五个子文档骨架存在并有明确 owner。

### Phase A1：Monitoring MVP 接入

- 新增同 network namespace/host-network loopback 拓扑的本地 Prometheus/Grafana 示例部署；普通 bridge/Kubernetes 示例只有在 TLS/代理决策完成后才能纳入；
- 新增安全的 scrape 配置和 Secret 注入说明；
- 新增 datasource 与四组 Dashboard provisioning；
- 新增仅依赖现有指标的 recording/alert rules；
- 新增 Prometheus/Grafana 自监控、外部探针和 dead-man/synthetic notification；
- 新增 Runbook、`promtool test rules` fixtures 和端到端 smoke 脚本；
- 固定镜像/校验工具版本、UID 和 UI drift 策略，并加入供应链及 provisioning 校验。

退出条件：全新环境能按文档启动、抓取、展示并触发/恢复代表性告警。

### Phase B：生产安全前置能力

- 实现版本化 Metrics credential、独立轮换/吊销和 active/retiring 双 Token 窗口；
- 持久化 credential epoch/status，提供迁移、审计、备份恢复和旧 Token 拒绝测试；
- 实现选定的双向 workload identity：per-listener mTLS、等价 service mesh 身份或受认证 loopback proxy；
- 验证证书/身份轮换、Token 原子切换、Prometheus reload、失败回滚和泄露响应；
- 普通 bridge/Kubernetes 部署只有通过本阶段后才能成为支持的生产拓扑。

退出条件：独立吊销、双 Token 重叠、旧 Token 拒绝、恢复不复活和跨节点双向身份认证均有自动化及端到端证据。维护窗口或风险接受不能替代本阶段。

### Phase C：指标质量提升

- 依据 Phase A0 数据模型和 Phase A1 baseline/soak 选择最终 histogram 类型及 buckets，再引入 latency histogram；
- 增加 build info 和 process/runtime 指标；
- 冻结并实现 Provider/Deployment saturation 分母：并发上限、可用 slot 或等价受控容量指标；在分母可验证前只展示 active requests 绝对值，不生成虚假 saturation percentage；
- 增加 histogram checkpoint restore、Ledger replay、通知 drop/catch-up、重启和重复回放等价性测试；
- 调整 Dashboard 支持 P50/P95/P99；
- 评估并控制新指标 series 增长；
- 为 Metrics listener 增加响应 write deadline、并发上限，并执行大量 Provider/Deployment 加 slow reader 压测；Metrics 响应失败主要依赖 Prometheus `up`、scrape duration/error 外部观测，不依赖同一失败响应自报告。

退出条件：classic/native 选型、Histogram 幂等、runtime contract、Provider saturation 分母、性能回归、race、series 预算和 Dashboard 查询全部通过。

### Phase D：生产准入

- 接入目标环境的 Secret、持久化、备份、SSO 和网络策略；
- 验证双向身份认证、证书轮换/过期告警、Metrics Token 轮换和独立吊销；
- 验证 Prometheus UI/API/reload/admin 边界和 Grafana SSO/MFA、RBAC、Explore、datasource、alert/contact-point、service account 权限；
- 根据真实 baseline/soak 调整阈值；
- 完成 target down、WAL error、analytics lag、Deployment unhealthy 和通知失败演练；
- 验证 Dashboard/规则升级与回滚；
- 冻结 retention、磁盘容量/水位、备份加密、RPO/RTO 和单机监控盲区接受记录；
- 归档截图、查询结果、权限拒绝、审计、凭据/证书轮换、恢复、告警 firing/recovery 和通知送达证据。

退出条件：所有生产门禁有实际证据，且未通过项明确标为阻塞，不以“配置已加载”代替运行验证。

### 11.1 工时、责任与 Sign-off

以下为单个 Standalone 目标环境的工程量级估算，不含采购、企业 SSO/Secret manager 建设、跨区域 HA 或外部合规认证：

| 阶段 | 主要交付 | 估算 |
|---|---|---:|
| A0 | contract、capacity model、rule/label schema、Histogram ADR | 2–3 engineer-days |
| A1 | Prometheus/Grafana provisioning、Dashboard、规则、MVP smoke | 4–6 engineer-days |
| B | 版本化 credential、吊销/轮换、双向身份认证及迁移测试 | 17–35 engineer-days |
| C | Histogram/runtime 指标、checkpoint/replay、slow-scrape 性能验证 | 5–10 engineer-days |
| D | 目标环境集成、演练、Runbook、证据与 Go/No-Go | 3–5 engineer-days |

顺序总量约 31–59 engineer-days；Application、Security、SRE 与 Platform 可并行部分工作，但 Phase B 是生产路径的硬依赖。Phase B 的估算由 credential schema/迁移/双 Token/审计 8–15 天，mTLS/service-mesh/proxy 集成 5–12 天，安全测试/恢复/升级/回滚 4–8 天组成；这不等于日历周期。计入四方设计评审、环境排期和 Sign-off 后，Phase B 初始日历预算为 4–8 周，并在 A0 后依据所选身份方案重新基线化。

| 领域 | Responsible | Accountable/Sign-off | Consulted |
|---|---|---|---|
| Metrics exporter、credential、Histogram/checkpoint | Application | Application Architecture | Security、SRE |
| PromQL、Dashboard、告警、容量模型、Runbook | SRE | SRE Lead | Application、Platform |
| 双向身份、Secret/RBAC、威胁模型 | Security + Application | Security | Platform、SRE |
| Prometheus/Grafana 运行、存储、备份和外部探针 | Platform | Platform Owner | SRE、Security |
| 生产 Go/No-Go | 各领域提供证据 | Application + Security + SRE + Platform 联合签署 | Product/Ops |

强制 Sign-off 节点：A0 契约冻结、B 安全设计批准、B 安全能力验证、D 演练完成和最终 Production Go/No-Go。任何角色的阻塞项必须记录 owner、到期时间和解除证据，不能折算为总体完成百分比。

## 12. 测试与验收

### 12.1 自动测试

- Metrics endpoint 未授权返回 401，正确 Token 返回 200；
- 输出能通过 Prometheus 格式解析；
- HELP/TYPE 唯一且与样本类型一致；
- label name/value 正确转义；
- Counter 单调且重启语义明确；
- 测试固定禁止标签和敏感 canary 不出现在输出中；
- Dashboard JSON 可解析，datasource 引用有效；
- CI 扫描 Dashboard/datasource/contact-point 中的 headers、`secureJsonData`、链接、注解、变量默认值、panel description 和查询，确认没有 Secret 或敏感 canary；
- Prometheus 配置及 rule files 通过与目标镜像同版本的 `promtool check`；
- 告警使用 `promtool test rules` fixtures 验证，不只检查语法；
- recording rules 的零流量、低流量、多实例场景有测试；
- recording rules 额外覆盖多 environment/cluster 隔离、counter reset 和 target label 缺失；
- 告警表达式覆盖 firing、pending、recovery、counter reset、expected-target absent 和 Deployment 尚未 probe；
- provisioning 测试确认固定 UID、唯一 rule source、生产不可编辑/漂移策略和幂等 reload；
- 镜像、SBOM、插件 allowlist、容器权限和 Secret mount 通过供应链/配置扫描；
- SSRF 测试覆盖 loopback、link-local、cloud metadata、DNS rebinding/变化、重定向、私有管理网段和未批准插件/datasource/webhook/service-discovery 目标；
- 审计完整性测试覆盖删除、改写、截断、乱序和管理员越权，验证独立存储仍能检测并保留证据；
- 多 Deployment unhealthy、fallback saturation 和 Provider capacity pressure 有级联、抑制及恢复测试。

### 12.2 集成验收

1. 启动全新 Heimdall、Prometheus、Grafana；
2. 验证错误 Token 抓取失败、正确 Token 抓取成功；
3. 产生成功、失败、fallback、policy rejection 和成本/Token 流量；
4. 验证 Dashboard 数值与原始 Metrics/PromQL 一致；
5. 模拟 Deployment unhealthy；
6. 模拟 analytics lag 或使用测试夹具验证规则；
7. 停止 Heimdall，验证 target-down 告警 firing 和通知；
8. 恢复 Heimdall，验证告警恢复通知；
9. 检查 Prometheus、Grafana 和通知内容没有 Secret 或请求数据；
10. 验证错误证书、过期证书、错误 Token、Token/证书轮换和 reload 失败回滚；
11. 验证匿名、越权 Viewer/Editor、Explore、datasource、alert/contact-point 和 Prometheus admin/reload 请求被拒绝；
12. 验证 notification payload、链接、annotation、external labels、silence/comment 不泄露敏感 canary；
13. 重启整个监控栈，验证 provisioning、持久化和审计结果一致；
14. 从加密备份恢复，验证 RPO/RTO，且已吊销 Token、service account 和明文 Secret 不会复活；
15. 分别停止 Prometheus 和所选告警/通知评估组件，验证独立外部探针仍能发出告警；
16. 模拟 Prometheus TSDB 磁盘写满或只读，验证外部 dead-man/synthetic monitor 报警并在恢复后发送 recovery；
17. 证明外部探针不与主监控栈共享进程、存储、唯一网络路径、通知凭据或唯一 webhook/contact point，避免共同故障静默失联；
18. 在隔离测试主机上以非授权本地进程访问 loopback Metrics endpoint，验证无 Token/错误 Token 被拒绝、认证失败不泄密且不会造成日志或 CPU 放大；多租户 profile 验证裸 host network 被策略拒绝；
19. 验证 Grafana/Prometheus/Alertmanager 所有可配置出网面不能访问未批准的 loopback、link-local、metadata 和管理网目标；
20. 篡改或删除监控栈本地审计记录，验证独立不可变存储仍保留原始证据并产生完整性告警。

### 12.3 性能验收

- Metrics scrape 不阻塞 Gateway 热路径；
- scrape 并发和慢客户端有明确上限或超时保护；
- histogram 引入后的 CPU、内存、allocation 和 series 增幅在预算内；
- 24 小时 soak 中 scrape 持续成功，不改变既有 release gate 结论；
- Prometheus 查询在预期保留期和实例数量下保持可接受响应时间；
- 记录 Prometheus TSDB series、磁盘增长、rule evaluation 和 Grafana 查询成本，验证容量预算与水位告警。

## 13. 发布与回滚

- Phase A1 配置交付、Phase B 安全前置能力和 Phase C 指标演进分开提交，便于独立回滚；
- Dashboard 和 rule files 使用版本控制，不依赖仅存在于 Grafana 数据库中的手工修改；
- Metrics 新增保持向后兼容；已有指标删除或改名必须经历废弃周期；
- Dashboard 发布失败时可回滚 provisioning 文件，不影响 Heimdall Gateway；
- Prometheus/Grafana 故障不得影响 Heimdall 请求处理和 Ledger；
- Metrics endpoint 异常不应使 Gateway 退出，但必须可通过外部 scrape health 发现；
- 单机 Prometheus 是明确的观测单点；部署配置必须记录其告警盲区、RPO/RTO、retention、存储耐久性和外部探针补偿措施；
- 未来 Prometheus HA 使用独立 replica identity，由 Alertmanager 对重复告警去重；Dashboard 不依赖 replica label，远端长期查询层按约定 replica label 去重；
- 回滚后验证抓取、Dashboard、规则、权限、凭据和通知渠道，而不只检查容器状态。

## 14. 风险与决策点

### 14.1 分阶段决策与外部依赖

为避免 Phase A0 成为跨职能决策漏斗，决策按最晚需要时间分配：

| 决策 | 最晚阶段 | Owner/Sign-off |
|---|---|---|
| Metrics contract、label schema、rule ownership、series 预算方法 | A0 | Application + SRE |
| MVP loopback 拓扑和本地通知渠道 | A0 | SRE |
| Histogram 权威事件源、Event ID/watermark/checkpoint/replay 模型 | A0 | Application Architecture |
| 最终 histogram 类型与 buckets | C | Application + SRE，依据 baseline/soak |
| 版本化 credential、独立吊销、重叠窗口、Master Key 迁移 | B | Security + Application |
| mTLS/service-mesh/proxy 双向身份方案 | B | Security + Platform |
| 生产运行形态、retention、磁盘、远端存储、RPO/RTO | D | Platform + SRE |
| P95/P99 是否成为正式 SLO | 独立产品治理，不阻塞 MVP | Product + SRE |

SSO/MFA、组织级审计平台、加密备份基础设施和企业 Secret manager 是外部平台依赖；本项目在 Phase D 验证其可用性、授权和证据，不承担从零建设。

### 14.2 主要风险

- 将 Metrics listener 绑定到非 loopback 后错误暴露公网；
- 把派生 Metrics 误当作 Ledger 权威数据；
- Provider/Deployment 标签随资源增长导致 series 膨胀；
- 只加载告警规则但不验证表达式、通知和恢复路径；
- 只展示平均延迟，掩盖长尾退化；
- 低流量环境中的错误率告警产生噪声；
- Grafana UI 手工变更与仓库 provisioning 漂移；
- Token 通过配置、日志或截图泄漏；
- 把 loopback 错当作同宿主机进程隔离边界，在共享主机泄露内部遥测或遭受本地拒绝服务；
- Grafana/Prometheus/Alertmanager 可配置出网面形成 SSRF，访问 metadata 或内部管理服务；
- 监控栈管理员可删除同一系统内的审计日志，导致越权和静默操作无法追溯；
- 普通 bridge 网络因现有 listener/TLS 校验无法启动，或为绕过校验而削弱安全边界；
- 全局聚合 PromQL 混合 environment/cluster，导致故障被其他环境流量稀释；
- Prometheus/Grafana 自身故障形成无告警的监控盲区；
- 多 Deployment 同时退化或 fallback 长期饱和但缺少聚合级早期信号；
- histogram 与 Ledger/Usage replay 语义不一致，导致重启或 catch-up 后分位数错误；
- UI 手工规则与仓库规则重复加载，产生重复告警和状态分裂；
- 未固定镜像、插件或校验工具版本，造成供应链和规则兼容风险。

## 15. 完成定义

本方案完成不等于生产监控已经上线。只有同时满足以下条件，才能声明 Prometheus/Grafana 集成完成：

- 指标契约和文档一致；
- 五个 `docs/observability/` 子文档存在、owner 明确，主计划与详细契约无相互矛盾的重复定义；
- Prometheus 能使用 Secret 注入的 Token 稳定抓取；
- loopback 和跨 namespace 拓扑符合已冻结的 TLS/代理契约，普通 bridge 不被误报为零代码能力；
- Metrics Token 泄露、独立轮换、立即吊销、双 Token 重叠和 Master Key 轮换有经验证的流程；旧 Token 在窗口结束后确定被拒绝；
- Dashboard 从空环境自动 provisioning，核心数值经交叉验证；
- 告警规则经过 firing、通知、恢复的端到端演练；
- 多 Deployment unhealthy、fallback saturation 和 Provider capacity pressure 能形成早期聚合信号，并正确抑制同根因低级告警；
- recording rules 保留 environment/region/cluster 语义，跨环境隔离和 counter reset 测试通过；
- Prometheus/Grafana 自监控、外部探针和 dead-man/synthetic notification 可用；
- Prometheus、告警评估组件停止及 TSDB 写满演练证明外部探针使用独立故障域和通知路径；
- series 预算公式已有真实输入值，CI label schema 通过，运行 series/磁盘低于 admission 上限；
- 安全检查确认无跨 namespace 明文 Metrics、无仓库/环境变量/状态页面 Secret、无敏感或未预算高基数标签；
- 共享主机/多租户 profile 禁止裸 loopback/host-network，非授权本地进程、SSRF 和审计篡改演练通过；
- 目标环境既有的 SSO/MFA、审计、Secret manager 和加密备份平台已完成适用性、授权和证据验证；本项目不以建设这些组织级平台为完成条件；
- 本项目范围内的 RBAC、service account、API/reload、Explore、contact-point、镜像、SBOM、插件、容器权限和恢复集成检查通过；
- 自动测试、race、format parser、`promtool test rules` 和相关 soak gate 通过；
- classic/native histogram、Go/process collector contract 和 Provider saturation 分母已经冻结并通过兼容性测试；
- Runbook、升级和回滚步骤由非作者按文档成功执行；
- 单机监控 RPO/RTO、retention、磁盘水位和告警盲区有批准记录；
- 生产未完成项和外部依赖被显式记录，不以本地演示替代生产准入。

## 16. 推荐实施顺序

Phase A0/A1、Phase B 和 Phase C 的仓库实现已完成。部署时仍按 loopback MVP、版本化 credential/mTLS、目标环境阈值校准的顺序启用。Phase D 只做目标环境集成与生产 Go/No-Go，不从零建设组织级平台能力；其证据未齐全前默认 No-Go。
