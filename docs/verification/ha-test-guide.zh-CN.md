# HA 如何测试：从仓库门禁到三节点验收

本指南用于验证已实现的 Primary/Replica 与人工切换。完整操作顺序以
[HA 运维手册](../runbooks/ha-operations.md)为准，仓库测试的具体 oracle 见
[HA repository gates](ha-repository-gates.md)。这些测试的证据等级不同：本机测试
只能证明仓库实现；生产启用还需满足 [§1.3 进入条件](../todo/halro-ha-architecture.zh-CN.md#13-进入条件)
和 [G0–G7](production-validation-plan.zh-CN.md#6-分阶段执行门禁)。

## 1. 先跑不计费的仓库测试

在仓库根目录运行：

```sh
go test -count=1 ./internal/replication/
go test -count=1 ./internal/app/ -run 'TestPrimaryAndReplicaRuntimesReplicateAndApplyAnAuditFrame|TestPlannedStepdownDrainsBeforeFreezingTheConfirmedPrefix|TestPlannedStepdownDrainWithdrawsReadinessAndWaitsForHTTPServers|TestReplicaBackupRecoversNativeTailAndCarriesOneAppliedPrefix'
```

第二条覆盖真实 TCP/mTLS 双 Runtime、已确认吊销在提升后保留、计划交接先 drain
后冻结，以及 Replica 备份前缀。它不访问真实 Provider，也不模拟 Kubernetes 的
endpoint/PVC/网络行为。要把测试结果用于候选发布，按仓库政策在最终 push 前对精确
候选 SHA 跑一次完整门禁；上述定向通过不能替代它。

## 2. 建立隔离的三节点测试环境

准备三个 Kubernetes 节点、三个独立 PVC、成员专属的配置和证书、同一 Master Key、
私有的 Admin/Metrics 入口、可保存不可变证据的存储。仓库的
[`halro-ha-statefulset.yaml`](../../deploy/kubernetes/halro-ha-statefulset.yaml)
是带镜像 digest 和 Secret 占位符的拓扑样例，不是开箱即用的部署命令。
先按运维手册建立 Primary、批准并安装两个 Replica 的 seed，再启动 StatefulSet。
不能把同一个数据目录挂给多个成员，也不能跳过 seed 直接让 Replica 初始化自己。

每次演练冻结候选 SHA、镜像 digest、配置摘要、测试环境 ID、目标 RPO/RTO 与证据位置。
在每个节点运行只读命令，并保留其 JSON：

```sh
halro cluster status --config /etc/halro/config.yaml
```

在 Primary 的 Admin Console 打开「集群状态」。页面显示本节点的角色、term、
durable/confirmed/applied index 与已认证 peer 会话；它不读取对端的实际水位。
对端数据必须到对端取 `cluster status`，或分别抓取其 `/metrics`。
正常稳定态应满足：同一 cluster ID 和 incarnation，恰有一个 Primary；每节点
`durable >= confirmed >= applied`；Replica 最终追平 Primary 的已确认前缀。
已连接不等于已追平，也不构成可以提升的证明。

## 3. 按故障边界逐项演练

每一项都记录操作前后各节点的角色、term、三个 index、`/health/ready`、
`halro_replication_*` 指标、审计和 Ledger 校验结果。使用合成数据和模拟 Provider；
不得把一次真实计费调用当作故障注入的先决条件。

| 场景 | 操作与通过条件 |
| --- | --- |
| 正常复制 | 在 Primary 做一次可审计的测试变更；等待确认并核对两个 Replica 的 applied 前缀。已确认变更必须在候选 Replica 上可见。 |
| 单个 Replica 中断/恢复 | 只停止一个 Replica，保持其 PVC；Primary 的可用性按剩余 quorum 判定。重启后它必须从认证前缀追平，不能接受相同 index 的不同字节。 |
| 计划交接 | 在有长请求/SSE 的条件下运行运维手册中的 `cluster stepdown`；旧 Primary 先撤 readiness 并 drain，随后才冻结前缀和承诺新 term。旧 index 失效时命令应拒绝并可按新 index 重试。交接后重新以 Replica 打开旧 Primary，核对其元数据投影与已确认前缀，再等待它追平。 |
| 正常停机与播种 | Primary 与至少一个 Replica 追平后正常停止 Primary；同时安排延迟响应收尾或告警投递，让后台审计可能在关停时写入。后台写入必须先于 `system.shutdown`，停止后的 durable/confirmed/applied 必须相等，`system.shutdown` 必须落在已确认前缀。用该快照执行 `seed-approve`/`seed-install`；未确认尾帧必须被拒绝，不能手改 state 或排序日志。 |
| 非计划提升 | 先在 Halro 外部**证明旧 Primary 已被隔离**，再按手册用精确 term/index 提升已追平 Replica；旧 Primary 回归不得重新确认写入。无外部 fencing 证据就停止此场景。 |
| 非对称分区 | 分别隔断不同方向的复制连接；任何时刻至多一个成员能确认权威写。无 quorum 的请求必须拒绝或留下可归因的保守状态，不能静默成功。 |
| 单 PVC 丢失 | 保留其它成员，从已认证 seed 重建该 Replica；旧本地状态不能被当作当前前缀。 |
| 同 incarnation 重播种后的成员日志换代 | 冻结并认证旧成员完整迁移日志、获批 seed 与新成员基线；逐成员记录旧/新 journal ID、旧链最后提交游标及采集端最后游标。旧链若有漏采，必须补入或显式标缺口，不能清空采集器文件后把新基线说成连续。现有离线工具可从认证旧快照补采并停机导入旧链，且能核验源 Primary 的 seed 原件、新成员精确基线及完整预检；未获批换代时采集器安全拒绝新日志；本地 kind 已在旧链补采和完整预检后，停机提交 v3 代际记录并两次重启恢复 `caught_up`。本地四条链的冻结成员原件 MAC 读回已通过；独立不可变留存与完整故障矩阵仍须另验。 |
| 全集群恢复 | 从已验证的 Replica 备份恢复为**新 incarnation**，重新播种成员；旧 incarnation 的节点必须被拒绝。核对 Ledger、Audit、Gateway Key 吊销和幂等记录。 |
| 相邻版本 | 用两个相邻的真实发布二进制按 §15 的同 schema 顺序逐个升级/回退；schema-changing 版本只能按离线新 incarnation 恢复流程验。 |
| Linux 故障与容量 | 在 Linux 参考机注入 ENOSPC、只读和慢盘，比较 Standalone 与三节点吞吐/延迟；错误必须可见且失败关闭。 |

最后在目标负载下做 72 小时 HA soak；[只读健康观测采样器](ha-health-soak.md)
可辅助收集持续状态，但不能代替负载、故障或资源证据。从实际故障开始测量人工响应、追平、提升与
客户端恢复的完整 RTO。所有测试都应有可访问的证据 ID、目标 SHA/digest 和明确 PASS/FAIL；
一次失败先修根因，再重跑受影响场景。自动故障切换不在当前交付范围。

## 4. HA 健康系统目标环境验收

本节验收独立的 `halro-ha-health` 观测链，须在上述隔离三节点环境执行，并与
[健康服务部署契约](../observability/ha-health-service.md)和
[生产 G0–G7 门禁](production-validation-plan.zh-CN.md#6-分阶段执行门禁)一起留证。
这些步骤不替代 §3 的数据安全、人工提升和 72 小时 soak。每个场景在故障注入前后
保存状态；故障恢复后核对页面、原始来源和告警均回到预期状态，不能只截图绿色卡片。

### 4.1 冻结采集清单与基线

在开始前把以下字段写入一次演练记录。清单一旦改变，重新核对所有成员及告警规则，
不得让自动发现缩小分母。

| 字段 | 必须记录和核对的值 |
| --- | --- |
| 验证单元 | 候选 SHA、运行版本和镜像 digest、环境/区域/cluster ID、配置及 `halro-alerts` 规则摘要、开始时间（UTC）和证据 ID。 |
| 独立故障域 | 健康服务、Prometheus、Alertmanager、操作员入口代理与事件文件持久卷的位置；证明它们不依赖 Primary Pod、PVC 或单节点入口。记录 `-members` 的**精确** node ID 集合。 |
| 逐成员来源 | 每个成员 `/metrics` 的 mTLS/版本化 token 抓取目标、`/ha/status` 的独立凭据和状态清单、可选的直连 `/health/live` 与 `/health/ready`；记录 Secret 版本和轮换时间，不记录密钥内容。 |
| Kubernetes 监控路径 | 如采用仓库的 HA StatefulSet 和可选监控入口策略，记录每个 Pod 的 headless DNS、9090 端口、证书 SAN、NotReady 时 DNS 与抓取结果；核对监控 namespace 与两个 Pod 的标签、标签变更 RBAC、其他叠加策略、监控 Pod egress、Metrics/HA-status 独立 Secret 投射和轮换。验证 Prometheus 与健康服务均能访问成员 9090，只有健康服务通过该策略访问 Gateway 8080 的客户端 Service 根路径及可选逐成员 live/ready，且未把整个监控 namespace 标成客户端入口来源。策略样例只放通流量，不证明身份或抓取成功；逐节点停止 Metrics 并验证对应 `up=0` 和健康页缺报。未部署到目标环境时记 `NOT_RUN`。 |
| 客户端入口 | `-client-url` 指向 HTTPS 客户端 **Service 根路径**，并记录负载均衡/endpoints 拓扑；核对根响应的 `cluster_id` 与监控配置一致，误路由到其他集群必须显示危险，缺失标识不得显示健康。直连成员的 ready 不替代此来源。 |
| 时间与采样 | 成员、Prometheus、健康服务和证据存储的时钟同步结果；实际 scrape 间隔、查询时间戳与 30 秒新鲜度判定。建议 15 秒抓取，记录任何偏离。 |
| 告警链 | Prometheus 已加载的 `halro-alerts` 表达式、`for`、最近求值时间、Alertmanager 路由与已获授权的测试 Contact Point；区分 `pending`、`firing`、通知送达、`resolved` 四类证据。 |

先在 Prometheus 的受控 API 核对
`up{job="halro",expected_target="true",environment="<env>",cluster="<cluster>"}`
的 `instance` 集合与 `-members` 完全一致，且所有值为 1。再核对每个成员的
`halro_cluster_member_info{cluster_id,node_id}` 与目标标签相符、角色只有一份，
机器状态的 `peers[]` 恰为其余成员。记录 Prometheus 原始向量及**样本时间**、
`/api/health` 的服务端 `observed_at`，以及每成员的 `sampled_at`、`up_sampled_at`、`newest_sampled_at`、
状态采集结果和页面的采集覆盖数；三种时间须处于同轮抓取的 1 秒对齐窗口。
还要核对独立事件文件所在持久卷可读、权限为
私有、重启后能读回；未配置 `-event-journal` 时不得把“未配置”写成持久事件链通过。
健康页当前成员指标从最近 45 秒原始抓取样本取最新值，并以其实际样本时间执行
30 秒新鲜度门限；验收时停止更新某个指标但保持其它 scrape 正常，确认下一次
成功抓取的 `up` 已前进而该指标未前进时，安全卡立即转未知、节点表标“本轮指标缺报”。
同时让整节点停止抓取，确认其 30 秒前的样本不能以 Prometheus 即时查询求值时间
伪装为新鲜。两种故障分别留原始抓取时间，不能互相代替。
同样暂停 `halro:ha_epoch_stable:bool` 记录规则求值而保持成员抓取正常，确认
关键写确认卡转为未知；`confirmation_evidence` 须保留独立的 `epoch_sampled_at`
与 Ledger/metadata 最旧计数 `sampled_at`，任一原始时间在响应前超过 30 秒均
不能保留健康结论。再用受控重启验证计数器重置窗口不能沿用旧成功增量。
另让单个成员的 term 或 incarnation 指标在一轮成功抓取中缺报、`up` 继续
前进，核对共享稳定任期记录停止生成新样本；恢复该指标后，在原始五分钟
样本数仍少于 `up` 时不得立即恢复 `1`，缺口退出窗口才可恢复。记录原始
`count_over_time`、记录规则样本时间与健康响应。整台 Prometheus 停服属于
另一类监控可用性故障，不能仅以三个序列数量相等代替独立 deadman 证据。

经目标环境已批准的操作员身份入口读取以下只读端点，各响应连同 UTC 获取时间存入
访问受控的证据库；`/api/evidence` 含运行状态和成员信息，勿提交到仓库。命令中的
主机名与证书路径须换成目标环境值；若通过审计代理访问，使用该代理实际认证方式。

```sh
curl --fail --show-error --cacert /path/to/ca.pem \
  --cert /path/to/operator.crt --key /path/to/operator.key \
  https://ha-health.example.internal/api/health
curl --fail --show-error --cacert /path/to/ca.pem \
  --cert /path/to/operator.crt --key /path/to/operator.key \
  'https://ha-health.example.internal/api/evidence?minutes=60'
```

同时读取 `/api/alerts`、`/api/impact`、`/api/latency` 和 `/api/event-archive`，
核对页面与 JSON 一致。五分钟内无必需确认写时，确认卡应为未知；只有稳定任期内
真实的内部必需确认成功证据才可改变该卡。`/api/impact` 仅统计服务端写路由结果：
每个成员都须有八类当前序列及八类五分钟增量，否则 `observed=false`；即使
`observed=true`，它也不是客户端完整响应成功率。

### 4.2 故障与恢复矩阵

只在允许故障注入的隔离环境执行；外部 fencing、提升、播种仍严格按运维手册，
健康页不能发起它们。对于双 Primary、错误 incarnation、错标成员和矛盾 Prometheus
样本，可用隔离的指标回放或替身源验证页面与规则，**不**制造真实双权威写。
每次注入前导出 60 分钟证据，注入期间保留原始样本与采样时间，恢复后再导出并核对
规则恢复。页面健康状态一般应在下一次成功采集或 30 秒过期界限内变化；规则触发
仍按各自加载的 `for` 条件判定，不能要求与页面同时变色。

| 场景 | 必须观察到的事实与通过条件 |
| --- | --- |
| 单 Replica 停止、恢复并追平 | 固定清单仍有三个成员；缺失成员首次抓取失败后总览不绿，节点表保留其身份和最后样本时间；目标 `up=0` 满足规则 `for` 后才有 `HalroTargetDown` firing。恢复后记录水位、认证 Peer 会话和实际前缀核验；连通或 index 数值相等本身不算已安全追平。 |
| Primary 停止与人工切换 | Primary Pod 不可用时，独立操作员入口和证据导出仍可访问；客户端 Service 探针、确认卡、成员与安全卡按实际来源降级或未知，无旧绿灯常驻。保存停止、外部隔离、精确前缀核验、提升、Service 路由恢复的时间线，按 §3 核对旧 Primary 不再确认写。RTO 从真实故障到客户端恢复另计。 |
| 独立客户端入口 deadman | 如启用 `halro-deadman` 的 `ha_client_root` 目标，确认它从独立故障域访问实际客户端 Service 根路径，按所配 cluster ID 核查 Primary；根路径返回带 `role: replica` 的 200 时继续尝试，单次 Replica 路由不产生错误告警。每次新连接须经过真实 Service 负载均衡；`kubectl port-forward service/halro` 会固定到一个后端，不能用它作为重选路验收入口。分别注入只到 Replica、路由到别的 cluster、缺 `cluster_id`、入口网络失联及恢复；核对持久 `down/up` 转移、独立接收者的 firing/resolved 回执、探针停止后的 heartbeat TTL 告警及恢复。该检查不得调用 Provider、不得携带写凭据，也不能代替最终客户端响应验收。未部署独立探针与接收者时记 `NOT_RUN`。 |
| 清单或身份矛盾 | 回放遗漏一个预期 `up` 目标、额外目标、缺失/重复角色、`cluster_id`/`node_id` 错标、不同 incarnation、同位不同 ordering digest，以及机器状态与新鲜 Metrics 的角色/term 不一致。逐项核对总览不绿并显示异常来源；来源间非原子快照差异只标待核查，不自行推断双 Primary。实际双 Primary 信号应按安全事故升级。 |
| 状态卡局部证据缺口 | 用隔离指标回放让两个成员同时自报 Primary，但保留其中一个的内部确认成功样本：安全卡和总览须危险，确认能力卡须未知。另保持一个 Replica 追平、使另一个 Replica 的角色或某个进度序列缺失：追平卡须未知，不能由前者的相等 index 掩盖清单部分缺报。恢复完整且唯一的角色/进度序列后再核对卡片恢复。 |
| 重复采集来源 | 在隔离 Prometheus 中给同一预期 `instance` 配置两个目标或附加不同来源标签，使 `up`、term、同一 `kind` 的 index 等逻辑序列各出现两份且值相同。成员必须显示来源冲突，安全卡和总览不得为绿色；独立 `HalroMemberDuplicateScrapeSource` 须立即 firing，不能把一台 Primary 的双份指标误报为两个 Primary 或冲突角色。故障期间全部 HA 告警和稳定任期记录规则均须保持 `ok`，重复成员不得获得稳定任期记录。清除多余目标后分别核对页面 45 秒原始窗口恢复、旧重复序列离开五分钟回看期后告警退出和稳定任期恢复。不同 `kind`、`peer`、`reason` 的合法序列仍分别保留。 |
| 缺失目标身份标签 | 在隔离指标回放中保留正确的预期成员序列，再额外注入同一环境/集群/job 但没有 `instance` 标签的 `up` 或 HA 成员指标。页面须列出“缺少 instance 标签”、原始样本时间并使安全卡与总览未知；不能把它计为已确认的清单外成员，也不能忽略。修复 relabeling 后等异常序列退出当前样本窗口再核对恢复。 |
| 采集陈旧和规则隔离 | 暂停单个 scrape、使 Prometheus 查询过期或令机器状态凭据失效，保留其他成员正常响应。失败/超时不可用 0 或其他 job 的同名节点补足；超过 30 秒无成功当前查询时，浏览器卡片变未知并保留最近成功时间。检查 `HalroTargetDown`、角色缺失或角色重复规则与当前加载表达式对应，恢复后规则退出 firing。 |
| 成员固定指标首次缺报 | 在一个成员上分别停止维护、term、promised term、durable/confirmed/applied 任一索引、启动裁决、复制阻断、不兼容原因或某个 Peer 指标的输出，保持该成员 `up=1` 与其他指标正常刷新。下一轮 `up` 前进后，安全卡须为未知，节点表显示“本轮指标缺报”，详情列出具体缺项及三种原始样本时间；固定指标与 `up` 的真实采样时间不一致须使规则开始 pending，持续 1 分钟后 `HalroMemberHASignalMissing` firing，不得等即时查询的旧样本离开默认回看窗口。恢复全部指标后核对页面与告警恢复。三类索引各须恰好一条，单纯的 Peer 序列数量相等还要逐名比对清单。 |
| 操作员浏览器时钟偏差与慢响应 | 在隔离浏览器将系统墙上时间调快、调慢各 1 小时，保持监控服务与成员时钟正常。节点新鲜度和采集覆盖应与服务端 `/api/health` 一致，超过 30 秒没有成功健康响应时卡片仍按浏览器单调计时过期；再延迟一份健康响应超过 30 秒，确认其旧绿状态不能复活。历史视图的一分钟自动轮询也须按单调时间执行，墙上时钟跳变不得提前触发或长期推迟。页面本地更新时间只作显示标签。留服务端 `observed_at`、原始样本时间、请求耗时和浏览器截图。 |
| 身份与网络边界 | 用无证书、错误证书、无权限身份及已撤销的 `/ha/status` 机器 token 验证拒绝；核对代理的操作员认证与访问审计、应用到达后的状态读取审计及握手前拒绝日志来源。轮换时证明新 Secret 生效、旧凭据失效，且 Metrics 凭据不被状态凭据撤销连带破坏。屏蔽 Primary 入口后操作员页面仍可用。 |
| 事件文件连续性 | 在独立持久卷上重启健康服务，核对已采记录、序号、来源实例开始时间与文件权限；再演练成员失联/重启、成员事件环溢出、文件不可写及最多 1000 条保留淘汰。`/api/event-archive` 显示可检测的缺口、采集错误、`journal_write_failed` 或淘汰数；配置留存后这些异常不能被绿色总览掩盖。两次轮询之间发生并消失的事件可能无法检测，不声称完整 Audit。 |
| 持久迁移全链（仓库部分实现，目标环境待验收） | 按[持久迁移证据契约](../contracts/ha-transition-durability.md)对成员日志写入、状态 rename/目录 fsync、状态已持久但进程未回包、离线 v2→v3 迁移命令、跨段缺失/篡改、未提交新段恢复和监控断线分页追赶逐点注入故障；核对状态游标与日志 MAC/序号一致，已提交迁移恰好一次、未提交意图不冒充事件。仓库已有成员写前日志、64 MiB 成员追加分段、认证分页、独立追赶、采集端分段与离线迁移命令，但旧段无归档确认后删除协议，也无外部不可变留存；目标环境此行仍须记 `NOT_RUN`，不能用上一行的监控事件文件连续性代签。 |
| 冻结采集快照与外部留存（仓库核验命令已实现，目标环境待验收） | 停止健康服务或取得原子文件系统快照后，复制清单与所有 `.segment.*` 文件；以相同环境、cluster 和完整成员清单执行 `halro-ha-health -verify-durable-snapshot`。把确切文件和输出的 `inventory_sha256` 存到独立不可变存储，在归档副本再次核验并比较根摘要、成员游标、缺基线清单。演练漏段、篡改、旧文件版本、活动采集锁与归档读取失败。记录冻结时间、存储回执、归档读回、容量和恢复用时；本地 `local_files_verified` 不代替外部交接，也不授权删除旧段。当前目标环境记 `NOT_RUN`。 |
| 冻结成员迁移快照与外部留存（仓库核验命令已实现，目标环境待验收） | 停止成员或取得原子文件系统快照，复制 v3 `state.json` 和全部 `transitions.journal` 段；使用匹配的成员配置及 Master Key 执行 `halro cluster verify-transition-snapshot --snapshot-dir <绝对路径>`，保存逐文件 SHA-256、`inventory_sha256`、基线和提交游标。对漏段、篡改、未提交意图、身份错配、同大小文件变化及归档读回失败进行注入；在独立不可变存储中留存确切字节和根摘要，再对归档副本复验并对比。并列记录采集端最近游标及冻结时间，不把成员完整链误当作采集已追上；v2 仍需先按离线迁移流程处理，旧基线前历史未知。`member_mac_verified` 不证明外部归档完成，也不授权删除旧段。当前目标环境记 `NOT_RUN`。 |
| 成员与采集快照链对账（仓库离线模式已实现，目标环境待验收） | 冻结并归档采集端完整文件和每个成员的 v3 状态、全日志段。在安全验证主机提供匹配成员配置、Master Key 与私有清单，执行 `halro-ha-health -verify-durable-snapshot ... -verify-member-snapshot-manifest <清单>`。版本 1 清单要求每个当前成员一份冻结副本，核对当前 incarnation 的 journal、基线、提交序号/摘要、采集存储游标及基线后的逐条事件投影摘要，期望 `member_authenticated_current_chain_match`；版本 2 清单要求每条已采集 `(node_id, incarnation)` 链一份冻结副本；若同一 incarnation 有多代日志，须用版本 3 清单为每条 `(node_id, incarnation, journal_id)` 链提供冻结副本，且每个预期成员有链，期望 `member_authenticated_all_collected_chains_match`。若只用预先生成的成员报告，则执行 `-compare-member-snapshot-reports <清单>`，期望 `snapshot_heads_match`，并从确切归档字节独立重跑每份报告；报告比较本身不能排除伪造报告。演练缺失/重复历史链、配置身份错配、成员日志或采集事件字段篡改、成员快照领先、采集未追上、同序号摘要冲突和归档读回失败。保存两侧文件摘要、事件序列摘要、冻结与最近观测时间及独立不可变存储回执。所有结果都不证明跨主机同时冻结、旧基线之前历史完整或可删段。当前目标环境记 `NOT_RUN`。 |
| 同 incarnation 重播种证据交接（协议缺口，目标环境待验收） | 人工提升后让旧 Primary 带未确认尾帧回归，验证它不 Ready 且持久要求重播种；保留旧目录，再由新 Primary 的停机已确认前缀批准 seed，安装并迁移新 Replica。核对旧成员认证日志头、采集器旧游标、新成员基线、源 seed 清单与原始文件；若旧成员的提交序号领先于采集器，须在受控离线流程补入漏采事件或明确记录不可恢复缺口。未经独立核验的 journal ID 更换必须保持旧链和 `partial`，不能用重置采集器或新基线掩盖。本地 kind 已验证旧链 MAC 补采、源和新成员的获批 seed 原件及离线交接预检；停机提交 v3 代际记录后，新镜像与再次重启均采集新链至观测头。按 journal ID 的四条冻结成员原件读回已在本地通过；独立不可变归档与完整故障矩阵尚未执行，本行完整验收仍为 `NOT_RUN`。 |
| 长留存恢复资源（仓库基准与容量指标已建立，目标环境待验收） | 用预期最大成员证据段数、迁移次数与磁盘配置测量启动全链认证的用时及峰值堆内存，核对启动等待裁决时限；再测跨段首、中、尾分页 64 条的时延和缺段时的错误传播。核对 v3 成员的 `halro_replication_transition_journal_segments`、`halro_replication_transition_journal_bytes` 与卷剩余字节/inode，对 v2 缺席和容量核算失败分别处理，并在失败持续一分钟后核对 `HalroTransitionJournalCapacityUnreadable` 的 firing/恢复。成员已改为逐条认证加稀疏分页索引；仓库 256／2,048 次迁移启动基准及 2,048 次、125 小段的首/中/尾分页基准只给出本机时间和累计分配，新增指标只显示本地占用，不能代替峰值资源与目标容量门禁；当前目标环境记 `NOT_RUN`。 |
| 同步停滞与写请求影响 | 用合成、不计费的需确认写和受控 ACK 停滞验证 Primary `replication_unavailable`、确认卡、三个阶段时延与 Replica 接收/应用边界；无写流量时不得把平坦曲线判为零延迟。可信唯一 Primary 明确报告阻断时，即使两个 Replica 缺报，确认卡也应显示已知降级，安全与追平仍未知。Admin mutation 返回 `503 replication_unavailable` 或超时后须待确认链恢复再回读资源与修订号：失败响应不能证明本地持久变更不会随后确认。让一个成员缺少一个 HTTP outcome 的当前或窗口序列，`/api/impact` 应为未观测，不由其他成员补齐。服务端 2xx 和内部屏障成功不得标为客户端已完整收到。 |
| 告警证据与访问 | 对一次实际 firing 保存规则表达式与 `for`、活动告警的求值向量、最近成员原始样本与各自时间、Alertmanager 投递/恢复证据；抽屉不得把当前规则或最近样本伪称为触发瞬间原样本。检查运维手册链接经过目标受控入口可访问，并确认页面/API 无提升或播种操作。直接个人证书访问须以 Subject 与证书 DER SHA-256 指纹回指凭据；经共享代理证书访问时，应用日志只识别代理，须在代理保留逐人身份与同请求关联证据。TLS 握手前拒绝另取服务端日志，不能伪称应用访问审计。 |

两类冻结快照的归档读回可分别使用 `halro-ha-health -verify-archive-readback`
和 `halro cluster verify-transition-snapshot --archive-readback-dir`。二者会重验
源与取回副本、比较 SHA-256 清单并拒绝硬链接冒充；返回的 `local_only`
状态仅是字节与成员 MAC 校验。矩阵中的“外部留存”仍须独立不可变存储回执、
保留版本、真实取回记录和故障域证据，不能由同一主机的复制预演签署。

### 4.3 记录格式与结论

每个矩阵行单独登记：`SCENARIO_ID`、执行人、候选 SHA/digest、目标环境和 cluster、
开始/故障/首次非绿/规则 firing/恢复/规则 resolved 的 UTC 时间、注入动作及回滚动作、
预期与实际结果、`PASS/FAIL/NOT_RUN`、证据 ID 与 SHA-256。证据包至少包含前中后
三份 `/api/health`、一份 `/api/evidence`、对应的 `/api/alerts`/`/api/event-archive`
（按场景）、启用客户端最终结果后的一份 `/api/client-final`、原始 Prometheus 选择器及样本时间、已加载规则、客户端 Service 探针
结果、节点命令输出和相关访问审计。通知送达及恢复须附独立 Alertmanager/Contact
Point 证据；活动告警 API 或页面时间线不能证明通知已送达。

判定时分别记录：**仓库门禁**、**目标环境观测链**、**HA 数据安全/人工切换**、
**客户端最终逻辑操作结果**和 **72 小时 soak/RPO/RTO**。按[客户端最终结果契约](../contracts/ha-client-final-result.md)
核对客户端或具备调用方应用层完成回执的入口层的观测者清单、结果互斥性、重试去重、流终止与故障时持续可观测；
验收前保持 `-client-final-manifest` 未配置；验收记录、固定类别和完整观测者清单齐备后再配置，并对 `/api/client-final` 注入观测者失联、抓取断点、单一结果缺报、未对账操作和计数重置，均须显示 `unobserved` 且不含结果计数。
启用观测者后，还须验证 `HalroClientObserverDown` 与 `HalroClientObserverCoverageIncomplete` 的实际规则加载、投递和恢复；未配置目标时无告警不等于清单完整，另以部署清单核对 Prometheus targets。
启动 72 小时候选观测前，应按[长跑采样指南](ha-health-soak.md)核对全部实际
证书 Secret 和不在 Secret 中的操作员证书；`certificate_window.json` 必须
显示覆盖整个窗口及恢复余量。证书清单遗漏、证书不足期、运行中 Pod 未加载
核验字节或轮换未经演练时，不得把长跑采样进程存活当作 72 小时通过。
缺少来源或任一覆盖证明时，客户端最终成功率维持 `NOT_RUN`。缺少跨成员重启的
完整持久事件来源时，事件全链维持 `NOT_RUN`。前述任一项缺证据都不能用健康页绿色、
仓库测试通过或一次故障截图代签。正式生产准入仍按 G0–G7 和四方签署执行。
