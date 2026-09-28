# HA 健康系统本地验收盘点（2026-09-28）

状态：**本地 kind 已运行三个真实 HA 成员、独立 Pod 的逐成员 Prometheus 与健康服务，以及本地 Alertmanager/接收器；客户端 Service、机器状态、事件采集、成员告警及独立客户端入口探针送达/恢复已通过有限验证。故障矩阵和独立不可变归档仍未完成。**
本记录保留 2026-09-28 约 02:46 UTC 的初始盘点、隔离健康服务 smoke、随后
约 02:59 UTC 的本地底座准备、约 03:06–03:17 UTC 的主机进程预演及约
03:20 UTC 之后的 kind 部署。各阶段结论按当时状态陈述，不是候选版本或生产放行记录。
仓库基线为 `2425ae8ebb957cecfd79508f8fa9b227701c4349`，HA 健康系统改动仍在
未提交工作树，因此没有可冻结的候选 SHA、镜像 digest 或发布归档。

| 截至本次记录的验收范围 | 结论 | 证据边界 |
| --- | --- | --- |
| 三成员采集、受控健康入口、客户端 Service 根探针 | 本地有限通过 | kind 中三个成员逐一抓取；入口只读认证、单成员故障及恢复已演练，未冻结发布候选 |
| HA 告警与规则健康 | 本地有限通过 | 包含目标失联、角色/身份缺报或错标、固定信号缺报、重复采集来源的实际触发与恢复；其他组合仍按矩阵执行 |
| Ledger 与 metadata 同窗口需确认写 | 本地有限通过 | kind 的非计费 HTTPS 替身收到真实 Gateway 请求，同一稳定任期窗口内 Ledger 和 metadata 必需 ACK 均有正向样本，确认卡曾为健康；重启后旧证据不会维持绿灯。仅覆盖隔离合成写，未签署正式写负载与故障注入 |
| 客户端最终逻辑操作结果 | 未接入 | 真实 SDK 或负责完整重试及响应消费的入口责任方尚未确定 |
| 持久迁移链外部不可变归档 | 未验收 | 已有本地认证快照、采集与离线核验；没有独立存储回执与归档读回 |
| G0–G7、相邻版本、72 小时 soak 与正式 RTO/RPO | `NOT_RUN` | 当前演练不替代正式故障与时间线矩阵 |

## 只读长跑采样工具预演

新增[HA 健康观测采样器](ha-health-soak.md)，每轮只读调用独立 `/api/health`，
把五卡与成员状态按单调时间计划写入私有 JSONL，失败单列且不沿用旧健康值。
三项仓库单测验证身份错配拒绝、72 小时候选元数据门禁、逐行持久化、
来源 503 与错过采样时不宣称完整覆盖，以及
`ha_acceptance=NOT_RUN`。另用**临时合成 CA、服务端证书和客户端证书**建立
本机双向 TLS 替身，真实 HTTPS 采集四轮成功；该替身没有读取 kind Secret，
结果仍为 `smoke_only`／`NOT_RUN`。当前未冻结候选提交与镜像摘要，也未取得
经批准的真实操作员凭据文件供采样器读取，因此 kind 真实健康入口的持续采样、
目标负载、72 小时运行及正式 RTO/RPO 均未执行。此工具不替代下面已有的
短时 kind 故障证据，也不证明独立不可变归档。

## 初始盘点（约 02:46 UTC）

| 初始检查 | 约 02:46 UTC 的本地证据 | 当时结论 |
| --- | --- | --- |
| 初始 Halro 成员 | 初始进程清单只有一个本机 `go run ./cmd/halro start --config config.yaml`；监听 `127.0.0.1:8080/8081/9090`。当前配置没有启用 `replication` 的顶层段；Gateway 根路径返回 200，但没有 `role` 或 `cluster_id` | 这是既有 Standalone；不停止、不改用其数据目录做故障注入 |
| 本地容器 | OrbStack Docker `ps` 只看到与本次验收无关的 Langfuse 容器；本机虽然缓存旧版 Halro 与 Prometheus/Alertmanager 镜像，当前没有 HA 成员或监控容器 | 缓存镜像及可用 Docker 不能代替已启动的三节点候选环境 |
| Kubernetes | 本机没有 `kind` 可执行文件；当前 `kubectl` 上下文指向远程 Rackspace 控制面，读取 Pod 被要求提供凭据 | 该上下文不是“当前本地环境”，不对它部署或注入故障 |
| 独立健康服务本机 smoke | 用当前工作树构建的二进制和一次性证书，在 `127.0.0.1:19105` 启动；Prometheus 指向未运行的本机 `127.0.0.1:19991`。无客户端证书请求在 TLS 握手被拒；带证书请求 `GET /api/health` 返回 HTTP 503 `monitoring data unavailable`；服务随后已停止 | 只验证 mTLS 入口与缺失数据时失败关闭；未验证真实 scrape、规则、Service 路由、独立故障域或恢复 |

## 后续本地底座准备（约 02:59 UTC）

安装了 kind v0.33.0，使用仓库的
[`halro-ha-health-kind.example.yaml`](../../deploy/kubernetes/halro-ha-health-kind.example.yaml)
同等三节点拓扑创建独立的 `halro-ha-health-local` 集群；`kubectl --context
kind-halro-ha-health-local get nodes` 显示 control-plane、worker、worker2 均为
Ready，Kubernetes 版本均为 v1.37.0。所有后续 Kubernetes 命令必须显式指定
`--context kind-halro-ha-health-local`；此前未认证的远程上下文不参与验收。

从当前未提交工作树构建了 `halro-ha-health-local:worktree-20260928`（Linux
arm64，镜像 ID
`sha256:982afe143a52bc660c2a38ae2a5a613b57cbc1e177a8c6e0bb60f221cb0a5bf7`），
并加载至当时的三个 kind 节点。该镜像只表示**构建当时**的工作树，不是不可变发布
候选；后续代码变化后须重新构建、记录新 ID 并运行相应门禁。
已创建专用 `halro` namespace，并在该 context 对 HA StatefulSet 清单中的
两个 Service、ServiceAccount、StatefulSet、PDB、基础 NetworkPolicy，以及
可选观测 NetworkPolicy 完成 API 服务端 dry-run；这些对象没有因此被部署。
仍没有成员配置 Secret、证书、独立 PVC、seed、Halro Pod、Prometheus、
Alertmanager 或健康服务 Pod。因此三个 Kubernetes
节点 Ready 不证明任何 HA 成员就绪或监控链可用。

本地验收下一步应在**新 kind 集群的独立 PVC**准备三个成员、成员证书与
Master Key，按 HA 运维手册播种两个 Replica；用同一候选构建部署逐成员 Metrics、
独立 Prometheus/Alertmanager、健康服务和受控操作员入口。先冻结候选 SHA、
配置及规则摘要、成员清单和证据位置，再按
[HA 测试指南](ha-test-guide.zh-CN.md#4-ha-健康系统目标环境验收)逐场景执行。
现有 Standalone 进程及其目录不参与这些破坏性演练。

客户端最终结果的真实 SDK/入口代理责任方尚未确定；该生产来源保持未接入。
本地也没有已指定的**独立不可变归档**存储及回执，冻结快照在同一台机器上的
复制和读回只能作为恢复预演，不能签署外部留存或删段门禁。kind 工作负载故障、
告警投递/恢复、G0–G7、相邻版本、Linux 磁盘故障及 72 小时 soak 均为 `NOT_RUN`；
下述主机进程单 Replica 停止/恢复只是有限预演。

## 隔离主机三成员与观测链预演（约 03:06–03:17 UTC）

在 `/tmp/halro-ha-health-host-20260928` 的三个**独立私有目录**使用当前工作树
构建的本机 `halro` 二进制和一次性 CA/成员证书：先按 Standalone 配置初始化
`halro-0`、引导管理员、启动并正常关闭，再建立 term 1 的 Primary；停机时分别
批准 `halro-1`、`halro-2` 的 index 0 seed，复制除本地成员状态等排除项之外的
权威快照，并逐个执行 `seed-install`。三个成员均在停止状态下用精确水位从 v2
迁移到 v3，随后以本机不同 loopback 端口运行。没有使用既有 Standalone 的数据、
主密钥或监听端口。这个路径验证了实际 CLI 引导顺序，但不是 kind 的三个 PVC。

主机 Prometheus 3.15.0 在 `127.0.0.1:19100` 使用成员各自的 mTLS 证书和版本化
Metrics bearer，以 5 秒间隔抓取三个固定目标；配置和 14 条记录规则、63 条告警
规则通过 `promtool check config`。原始 `up{job="halro",environment="host-local",
cluster="halro-health-local"}` 与三条 `halro_cluster_member_info` 均完整且为 1。
独立 `halro-ha-health` 进程在 `127.0.0.1:19105` 使用操作员 mTLS、固定机器状态
清单、持久事件文件和持久迁移采集文件，逐成员 `/ha/status` 与 `/metrics` 均返回
200。三个成员同属 `halro-health-local` / `inc_local_20260928`，恰有一个 Primary；
三个节点的 durable、confirmed、applied 均追至 6，且返回相同 index 6 的
ordering head SHA-256。健康页安全和追平卡为 `healthy`，总览与客户端入口卡为
`unknown: client Service probe not configured`，确认卡为
`unknown: no recent required confirmation success observed`。`/api/client-final` 返回
`not_configured`，没有将内部确认或服务端 HTTP 结果冒充客户端最终完成。

约 03:13:41 UTC 正常停止 `halro-2` 后，下一轮 Prometheus `up` 为 0，健康页安全、
追平卡转为 `unknown`，固定三成员清单未缩小。`HalroTargetDown` 于
03:13:56 UTC 进入 pending，按原规则 `for: 2m` 后进入 firing；健康服务的
`/api/alerts` 同样显示 `halro-2` firing。约 03:16:42 UTC 用原目录重启该
Replica，下一轮 `up` 为 1，三个成员水位仍为 6/6/6，安全与追平卡恢复
`healthy`；稍后 Prometheus 活动告警和健康服务 `/api/alerts` 均清空。
本机私有证据保存在 `/tmp/halro-ha-health-host-20260928/evidence/`：
`replica2-firing-prometheus-alerts.json` 的 SHA-256 为
`ae3a46fac047e0e95c98840e1008f90d7bb37b0df404f155242cc4e91c8f784c`，
`replica2-firing-health.json` 为
`1497eb8014f0f5fff1739bd2c7b4e33bd0451544f320edbfe18acd2b113391e1`，
`replica2-recovered-health.json` 为
`afd6cb586f7b947c1625a3421d281ce5f41343e8c9be34ee27e50b5e6f072b9d`，
`replica2-recovered-alerts.json` 为
`37517e5f3dc66819f61f5a7bb8ace1921282415f10551d2defa5c3eb0985b570`。
该 `/tmp` 目录是本机临时证据，未做独立不可变归档。

此预演没有 Kubernetes Service endpoints、PVC、NetworkPolicy、独立故障域、
Alertmanager/Contact Point、实际客户端重试或确认写流量，不能签署 G0–G7、
告警**投递**、RTO/RPO 或 72 小时 soak。进程日志还显示空闲复制会话约每
30 秒因读取超时重新连接，机器状态的某些 peer connected 采样短暂为 false；
须核查这一连接生命周期对拓扑展示和告警的影响，不能把单次连通样本当作
连续健康。隔离主机服务当前保持运行以便继续验收，均只绑定 loopback；证书
约两天过期，后续须轮换或停止这些临时进程。

## kind 三成员和独立监控部署（约 03:20–03:49 UTC）

首次三节点 kind 拓扑只有两个可调度 worker，不能满足 StatefulSet 的三 Pod
硬性节点反亲和。此时 `halro` namespace 只有空对象，没有业务 Pod、PVC 或
Secret；删除该专用集群后，按更新的仓库
[`kind` 样例](../../deploy/kubernetes/halro-ha-health-kind.example.yaml)重建为一个
control-plane 加三个 worker。四个节点均为 Kubernetes v1.37.0 Ready。
随后将上述 Halro 镜像加载到四节点；它仍是未提交工作树的本地构建，且后续
源码变化需要重建。

在三个独立的 50 Gi `ReadWriteOncePod` PVC 上离线准备成员：对独立数据目录
执行 Standalone 初始化和管理员引导、建立 term 1 Primary、批准并安装两个
index 0 Replica seed；停机状态按各自准确水位迁移到 v3 持久迁移日志，再分别
轮换 Metrics 与机器状态凭据。私有源包和三个 PVC 中的每份 12 文件清单以
SHA-256 逐项比对一致。一次性本地 CA、成员证书、监控客户端证书和 Secret
仅用于该 kind 集群，和既有 Standalone 的数据、密钥及端口隔离。

StatefulSet 的 `halro-0/1/2` 分别在三个 worker 上 `1/1 Running`，无重启；
每个 Pod 的 `cluster status` 均报告集群 `halro-kind-health-local`、同一
`inc_kind_local_20260928`、恰一 Primary 两 Replica、三成员持久/确认/应用
水位均为 3/3/3，状态版本为 v3。客户端 Service 的 EndpointSlice 有三个
Ready endpoint。从只有 CA、没有成员凭据和 ServiceAccount token 的隔离客户端
Pod，向 `https://halro.halro.svc.cluster.local:8080/` 发起 30 次独立连接；
得到 13 次 Primary 与 17 次 Replica 根响应，均包含正确 `cluster_id`。
此探测证明 Service 确实轮到 Replica，不证明客户端完整重试或最终写入结果。

这一轮初版监控在一个三容器 Pod 共置 Prometheus v3.13.0、健康服务和
机器状态凭据同步器。Prometheus 只绑定该 Pod 的 `127.0.0.1:9090`，使用
逐成员 bearer 和 mTLS 抓取三个稳定 DNS 目标；规则配置经 `promtool check
config` 确认为 14 条 recording 和 63 条 alert 规则，三个 `up` 均为 1。
健康服务镜像 `halro-ha-health-view-local:worktree-20260928` 的本地 ID 为
`sha256:b367c82fe3c8de433ce98a0a5711ead7af032213e973c5f7a4b61338a4e6b713`，
使用独立 1 Gi RWOP PVC 保存事件与迁移采集证据，Prometheus 使用单独 5 Gi
RWOP PVC。监控 Pod 位于 control-plane，和三个成员分处不同节点；健康服务
仅经操作员 mTLS 的 9105 Service 对外提供 API，没有 Prometheus API Service。
kind 默认网络组件的 NetworkPolicy 强制效果尚未证明，不能仅凭策略对象存在
声称网络隔离通过。

初次监控 Pod 的状态采集返回 `credential_unavailable`：Kubernetes 投射
Secret 为组只读 `0440`，严格的状态采集器要求无组/全局访问位。已在该 Pod
内由同一非 root 服务用户将投射凭据原子复制到只该用户可访问的内存卷
`0600` 文件，并以同步容器跟随 Secret 更新；采集器本身的拒绝逻辑没有放宽。
重新部署后，三个 `/ha/status` 均无错误，返回正确角色及 3/3/3 水位；
`/api/event-archive` 从 `partial` 恢复 `ok`。客户端卡为 `healthy`，安全与
追平卡为 `healthy`；确认卡及总览仍为 `unknown`，原因是还没有需确认操作的
近期成功证据。`/api/client-final` 仍为 `not_configured`；客户端 SDK/入口代理
责任方未确定。

机器凭据的实际轮换/撤销、TLS 失效、Prometheus 或健康服务 Pod 丢失、PVC
故障、NetworkPolicy 强制、不可变外部归档及 G0–G7 均待
单独演练。上述监控 PVC 仅是本地持久化，不是独立不可变归档。

## kind Replica 停止与恢复（约 03:50–03:52 UTC）

在仅供本次验收使用的 kind context 中，把 `halro` StatefulSet 从 3 暂调为 2，
撤下 `halro-2`，PVC 按清单保留。下一轮健康 API 将该成员标记为
`transport`，安全与追平卡变 `unknown`，持久事件归档变 `partial` 并列出
`halro-2` 缺口；客户端卡因仍能通过 Service 到达 Primary 保持 `healthy`。
`HalroTargetDown{instance="halro-2"}` 先为 pending，达到规则两分钟等待期后
为 firing，健康总览变 `critical`。将 StatefulSet 恢复到 3 后，`halro-2`
以原 PVC 再次 Ready；三个成员状态恢复为一 Primary 两 Replica、3/3/3，
安全与追平卡为 `healthy`，活动告警清空，事件归档重新为 `ok`。总览仍因
缺少近期需确认成功而为 `unknown`。这次演练只涉及一个 Replica 的短时缺失，
并未测试 Primary 切换、复制有负载时的追赶或实际通知送达。

私有证据在 `/tmp/halro-ha-health-kind-20260928/evidence/`，JSON 未包含 token
或私钥。停止时 `replica2-down-health.json`、`replica2-down-alerts.json`、
`replica2-down-archive.json` 的 SHA-256 依次为
`c79d55cfd3f2f4a14f5a0cb5d5662a199d17c9713be84cf18293e9ddcd25e1b7`、
`e30ad20a658d3af185f3ba2f9db60ef89615f9b13204e037403782b56af66187`、
`4032a730080c827c2e82b1c9b52daf7bd6614494d4a84f95f0184a9f3841756b`；
恢复时三个同名前缀 `replica2-recovered-*.json` 的 SHA-256 依次为
`14ab8eea9d9ead94395688159d603812a81f7aa7e3ebba825894dadf1cbc28a9`、
`37517e5f3dc66819f61f5a7bb8ace1921282415f10551d2defa5c3eb0985b570`、
`0685dcb85210bf92f7d64b6855e27be298a1f9f703b35f6c79e79e3997051f34`。
这是本机临时证据，还没有签名或独立不可变归档。

## kind 采集端凭据失效与恢复

仅替换监控 namespace 的 `ha-health-status-tokens` Secret 中 `halro-2`
采集端 token 为明确无效的测试字符串，成员自己的凭据未变。kubelet 将更新
投射到 Pod，非 root 同步器写入私有 `0600` 文件后，健康 API 将该成员标记为
`authentication`，总览为 `unknown`，事件归档为 `partial`；客户端入口仍
`healthy`。把原 token 写回同一 Secret 后，等待投射与同步，三个成员机器状态
重新无错误，事件归档恢复 `ok`。该实验验证失效凭据不会被当成成功及
Secret 更新路径会重新读取；它不是新版本 token 的正式轮换，也没有执行
旧版本永久撤销。投射有可观察的传播延迟，操作员必须等待应用层认证结果，
不能把 `kubectl apply` 成功视为轮换完成。

初版监控 Pod 的结构随后拆分；当前双 Deployment 样例保存为
[`halro-ha-health-monitoring.kind.example.yaml`](../../deploy/kubernetes/halro-ha-health-monitoring.kind.example.yaml)，
并通过当前 kind API 的 server dry-run；私有 ConfigMap/Secret 内容仍只在
`/tmp` 和目标集群内。

## kind 告警实际送达与恢复（约 04:03–04:07 UTC）

在监控 namespace 另部署 Alertmanager v0.28.1 与独立的本地 webhook
接收器，分别使用 1 Gi RWOP PVC。Alertmanager 从仓库配置派生，但本地
`group_wait`/`group_interval`/`repeat_interval` 为演练缩短至 5 秒/10 秒/
1 分钟；接收器在收到 JSON 后 `fsync` 到私有 JSONL，再返回 HTTP 200。
该接收器只供 kind 验收，没有生产级鉴权、跨机不可变留存或外部通知渠道。
Prometheus 的 Alertmanager 目标加入后重启监控 Pod；重建后的三个成员
状态均可读取，事件归档为 `ok`，且 `promtool check config` 确认 14 条
recording 和 63 条 alert 规则。独立接收器先收到 `Watchdog` check-in
和本地两天测试证书的到期预警，证明通知路径已通；证书到期预警是本地
短有效期材料的预期结果，不能据此评估生产证书阈值。

再次撤下 `halro-2` 后，接收器于 04:06:07.941 UTC 记录
`HalroTargetDown{instance="halro-2"}` 的 firing，告警 `startsAt` 为
04:06:02.917 UTC。恢复该 Replica 后，于 04:06:47.945 UTC 记录同一
告警的 resolved，`endsAt` 为 04:06:47.917 UTC。接收器 Pod 后续重启，
原 PVC 上两条目标回执仍在。原始 JSONL 私有副本位于
`/tmp/halro-ha-health-kind-20260928/evidence/alertmanager-webhook-notifications.jsonl`，
捕获时包含 10 条本地通知，SHA-256 为
`22162e8082ad1a22342eb7e1aa43bb9e39fb89e0ecdfa32d114c4ef0edf32c05`。
此证据证明该本地 webhook 的 firing/resolved 送达与接收端重启留存；没有
证明真实 PagerDuty/邮件等操作员渠道、外部故障域、长期交付保证或不可变归档。

## 需确认元数据与凭据轮换补验（约 04:13–04:24 UTC）

经 Primary 的受 TLS 保护 Admin API，以本地管理员 session、同源 Origin 和
CSRF 创建了一个**没有 Provider、路由或 Gateway key** 的验收项目。
创建成功后，三成员复制水位从 3 推进，`metadata/confirmed` 计数仍为 0；
代码契约表明新增项目属于异步、增加权限的元数据。随后仅更新该项目名称，
HTTP 200、revision 从 1 到 2，Primary 的
`halro_replication_required_confirmation_wait_total{store="metadata",outcome="confirmed"}`
从 0 增为 1。这验证了真实需确认 metadata 路径，不调用计费 Provider。
`ledger/confirmed` 保持 0，确认能力卡仍为 `unknown`，不能把单类屏障
成功写成完整确认能力。随后为修复凭据审计文件进行了三成员停机重启，进程
计数器归零；健康服务没有沿用重启前的成功把卡片显示为绿色。

第一次尝试对运行中的 `halro-2` 执行 `ha-status rotate` 时，CLI 以
`audit ledger is missing` 安全拒绝。查明最初离线 PVC 搬运只包含每成员的
Metrics/HA 状态凭据 JSON，遗漏两个原始 `.audit` 文件；`.revocations`
在该初始版本尚不存在。三个成员停机、RWOP PVC 卸载后，使用与 Halro
相同的 UID 65532 的临时 Pod，先核对六份 JSON 与原始私有源包的 SHA-256，
再逐个补回**原始** `.audit` 字节。六份侧文件 SHA-256 全部与各自离线源包
一致，属主为 65532:65532、权限 `0600`；没有重建或绕过审计链。移除
临时 Pod 后三个成员使用原 PVC 重新 Ready；实际成员 CLI 对三个 Metrics
和三个 HA 状态凭据执行 `verify-audit` 均通过，初始审计序号为 1。

随后在运行中的 `halro-2` 使用十分钟重叠窗口轮换 HA 状态 token，得到
v2 和审计序号 2。原 v1 与新 v2 在重叠期直接请求成员 `/ha/status`
均为 HTTP 200。更新监控 namespace 的采集端 Secret，确认 kubelet 投射
及私有内存卷同步后的文件摘要等于 v2，才对成员执行 `revoke --version 1`。
撤销后审计序号为 3，使用同一受信任客户端证书直连测试得到旧 v1 为
HTTP 401、新 v2 为 HTTP 200；健康服务的三个机器状态均无错误，事件归档
为 `ok`，三成员水位均为 17/17/17。一次性 token 仅留在权限为 `0600`
的本地私有文件和 Kubernetes Secret 中，本记录不收录 token 或其哈希。
本次只验证 `halro-2` 的 HA 状态凭据；其余成员、Metrics token 和证书的
轮换/撤销尚未在目标环境演练。
不含 token 的复验报告位于私有
`/tmp/halro-ha-health-kind-20260928/evidence/status-credential-rotation-v2.json`，
SHA-256 为
`806b9eea8160c15059283efec3454b8bd4f6c48b40db07cbbd5f8e516d5efa78`；
它同时记录撤销后的 HTTP 401/200、审计序号 3、采集器三成员无错误及三条
`up=1` 的原始样本时间。此文件仍只是本机私有证据，未完成不可变归档。

## kind Primary 不可用与独立入口（约 04:27–04:32 UTC）

对 `halro-0` 所在 kind worker 临时 `cordon` 后删除该 Primary Pod，令其
替换 Pod 留在 Pending；另外两个 Replica 持续 Ready。客户端 Service 的
EndpointSlice 只含 `halro-1/2`，Prometheus 三个固定目标原始 `up` 为
0/1/1。独立健康 API 在 Primary 不可用期间持续可访问：客户端卡明确为
`unknown: client Service returned only Replica routes within probe budget`，
安全、追平和确认卡也为未知，机器状态将 `halro-0` 标为 `transport`。
两分钟后 `HalroTargetDown{instance="halro-0"}` firing，总览转危险；
独立 webhook 接收器于 04:30:07.939 UTC 收到 firing。`HalroNoPrimary`
未在这次**成员缺报**场景触发，符合该规则要求完整成员覆盖再判断角色缺失
的前提；不能把它误记为已验证无 Primary 告警。

解除 `cordon` 后原 Primary 用原 PVC 重建，`cluster status` 为同一
incarnation、term 1、角色 Primary，三个成员 durable/confirmed/applied
均恢复到 20/20/20；客户端 Service 的三个 endpoint 均 Ready。健康页
客户端、安全、追平卡重新健康，`HalroTargetDown` 活动告警清空；接收器于
04:31:37.952 UTC 收到 resolved。确认卡与总览仍因没有重启后的近期两类
需确认成功而未知。本次**未执行人工提升**，所以不证明切换 RTO、旧 Primary
隔离后的提升安全或提升后的写入恢复；它证明故障期独立入口可达、旧绿灯
消失、缺报与路由语义及本地通知链一致。

私有复合证据 `primary-down-health.json` 和 `primary-recovered-health.json`
位于 `/tmp/halro-ha-health-kind-20260928/evidence/`，SHA-256 依次为
`4375cbdf80602eed044685a98d0f68ce3f5ad444809a6cb375e14844e3699f99`、
`927f79d8d542838e7ad45e1dc9884247c69858dd8ae9a2e5d9de7964088aab02`；
包含 API 状态、固定目标原始样本及客户端 EndpointSlice。它们仍不是外部
不可变归档。

## 停服证据卷快照与只读核验

确认 `ha-health` 为唯一使用 1 Gi `ha-health-data` RWOP PVC 的采集器后，
将其 Deployment 暂缩为 0 并等待 Pod 完全停止。独立、非 root 的临时 Pod
只读挂载该 PVC，将私有目录中的 `durable-transitions.json`、相应 `.lock`、
事件文件及其锁一起打包到本机 `0600` 的冻结 tar，再解包到 `0700`
的私有目录。临时 Pod 不具备 ServiceAccount token、写挂载或其他成员
PVC 访问。没有从运行中的 manifest 做逐文件拷贝。

用当前工作树构建的 `halro-ha-health -verify-durable-snapshot` 对解包后的
**绝对 manifest 路径**执行只读核验，环境 `kind-local`、集群
`halro-kind-health-local` 与完整三成员清单均匹配；结果为
`local_files_verified`，存储字节数 3094，关闭分段数 0，确定性文件清单根
`fe41408ad8687794dd522e4efe82a67fb878525f7051a95d670535f87f9ca094`。
报告 `/tmp/halro-ha-health-kind-20260928/evidence/durable-snapshot-verification.json`
的 SHA-256 为
`ed42e0dd419bc14eae7f9e718a9a0312ca91c5099d5cbb5e87c4843ab770a3d2`。
三条成员链都只有已建立的基线，stored/committed 序号为 0；本次**没有**
验证非空迁移事件、关闭分段回放、成员私有 MAC 或采集追到实时头部。

卸载临时 Pod 后，原监控 Deployment 恢复为 1，Prometheus、凭据同步器和
健康服务均 Ready；`/api/health` 重新成功读取三成员，事件归档为 `ok`，
持久迁移接口为 `caught_up`。冻结 tar 与报告都位于本机 `/tmp`，不具备
独立故障域、不可变保留和归档回执；不能据此批准删段或生产保留策略。

## kind 客户端入口独立探针演练（约 04:44–04:50 UTC）

从当前工作树构建 `halro-deadman`，在本机独立进程中启用
`ha_client_root`、Prometheus 原始 `up` 新鲜度检查和 Alertmanager
readiness。三种目标均通过 HTTPS 访问；Prometheus、Alertmanager 的
本地 HTTP 入口由本机临时 TLS 转接，通知发送到同机临时 HTTPS 接收器。
接收器按事件 ID 去重，先向本机 JSONL 追加并 `fsync`，再返回 HTTP 204。
这是受限本地验收桩，不具备独立存储或通知故障域。

首次使用 `kubectl port-forward service/halro` 时，转发在建立时固定到
一个 Replica；探针因此连续报 `ha_client_primary_not_observed`。这只是
端口转发的固定后端造成的**无效路由样本**，未归入下面的正式演练。
随后建立仅允许监控 namespace 特定桥 Pod 访问 Halro 8080 的临时
NetworkPolicy，经该 Pod 为每次 TCP 连接重新进入实际 ClusterIP
Service；另取全新 `halro-kind-local-service-probe` 身份和状态文件。
独立抽样 24 次连接实际命中同集群 Primary 10 次、Replica 14 次，
三个 deadman 目标均稳定 `up`。

在 Primary 所在 worker 临时 `cordon` 后删除 `halro-0`；替换 Pod
Pending 时，两个 Replica 仍 Ready。客户端目标连续失败达到阈值 2，
接收器收到序号 9 的 `down`，原因
`ha_client_primary_not_observed`，观测时间 04:48:53.313 UTC。
解除 `cordon` 后原 Primary 从原 PVC 恢复，目标连续成功达到阈值 2，
接收器收到序号 15 的 `up`，观测时间 04:49:43.313 UTC。
Prometheus 与 Alertmanager 两目标在该窗口保持 `up`；最终三个目标
均为 `up`、outbox 为 0，三成员 Pod 均 Ready。独立接收器记录的
两个转移事件各有唯一 event ID，心跳与转移序号单调递增。

私有复合报告
`/tmp/halro-ha-health-kind-20260928/evidence/deadman-client-service-local.json`
的 SHA-256 为
`b6146e804d2a4acb526b058fdf0962f09b1800ef7ebb55edfddacf736399f866`；
报告还记录状态、探针审计和本机接收器事件文件的摘要。演练结束后停止
临时探针并清理桥 Pod/策略。尚未验证接收器 heartbeat TTL 报警、重放
拒绝、外部不可变审计、独立联系点、错误集群/缺身份注入或真实客户端
最终响应；本次原 Primary 重启恢复也不是人工提升或切换 RTO 证据。

## kind 成员监控入口 NetworkPolicy 矩阵

保留 `halro-ha-ingress` 和 `halro-ha-observability-ingress` 两条正式本地策略，
从无 ServiceAccount token、无应用凭据的临时非 root Pod 对 Ready 的
`halro-0.halro-members.halro.svc.cluster.local` 进行 TCP 连接测试，
每个端口等待最多 2 秒：

| 来源 namespace / Pod 标签 | 9090 Metrics | 8080 Gateway |
| --- | --- | --- |
| `halro-monitoring` / 无受控标签 | 超时 | 超时 |
| `halro-monitoring` / `app.kubernetes.io/name=prometheus` | 连通 | 超时 |
| `halro-monitoring` / `app.kubernetes.io/name=halro-ha-health` | 连通 | 连通 |
| `default` / `app.kubernetes.io/name=halro-ha-health` | 超时 | 超时 |

这证明当前 kind CNI 对这一个 Ready 成员的 namespace、Pod 标签和端口
选择器按预期执行；连通不等于通过 Metrics/HA-status 的 mTLS 与 bearer
认证，也不证明其他成员或 NotReady 时的 DNS 行为。拥有在已批准
namespace 创建或重标 Pod 权限的人仍可使用这些标签取得相同的网络身份，
生产准入需要审查该权限。矩阵测试时 Prometheus 与健康服务仍在同一
Pod，NetworkPolicy 在 Pod 边界生效，不能限制其中的 Prometheus 容器
使用健康服务获准的 8080 路径。此后已拆为两个 Pod，见下节。

四个测试 Pod 已删除。私有矩阵报告
`/tmp/halro-ha-health-kind-20260928/evidence/network-policy-local-matrix.json`
的 SHA-256 为
`5266ab97b0f3dd2055c6f864f9103a933d3ac03a977f750f772985607a4d476c`。

## kind 监控 Pod 拆分与 Prometheus 双向 TLS（约 05:15 UTC）

将初版共置 Pod 缩至 0 后，在 `halro-monitoring` 分别部署单容器
`ha-prometheus` 和含状态凭据同步器、view 的 `ha-health`；沿用原有
Prometheus 5 Gi 与健康证据 1 Gi RWOP PVC。两个 Pod 均为 Ready，
位于本地 kind control-plane；这是 Pod 与网络权限隔离，**不是**节点
故障域隔离。当前样例和本地 API server dry-run 使用
[`halro-ha-health-monitoring.kind.example.yaml`](../../deploy/kubernetes/halro-ha-health-monitoring.kind.example.yaml)。

Prometheus 3.13.0 通过内部 Service 提供 HTTPS，专用本地 CA 验证服务端
与健康客户端证书，服务端再按客户端 SAN 限制调用方。客户端私钥由 init
容器从 Secret 复制到内存卷的 `0600` 文件，健康服务只接受完整的
`-prometheus-ca`、`-prometheus-client-cert`、`-prometheus-client-key`
组合访问远端 HTTPS 原点。无客户端证书、另一 CA 的操作员证书、同 CA
但错误 SAN 的客户端证书均在 TLS 层拒绝；专用健康客户端返回 HTTP 200。
`ha-prometheus-health-ingress` 只准健康 Pod 到 Prometheus 9090；无该标签
的 Alertmanager 接收器 Pod 连接内部 Service 超时。此测试不证明有权创建
受控标签 Pod 的用户已被限制，生产还需检查 RBAC。

拆分后 Prometheus 固定三个成员原始 `up=1`，加载 14 条记录规则及 63 条
告警规则，并保留 Alertmanager 目标；管理、生命周期、remote-write 与
OTLP 接收 API 对应运行标志均为 false。健康服务的 `/api/health` 为 HTTP
200：客户端、安全、追平健康，确认与总览因缺少近期完整需确认依据仍为
未知；三成员水位为 23/23/23。`/api/event-archive` 为 `ok`，
`/api/durable-transitions` 为 `caught_up` 且关闭段数 0，活动告警为空。

原始部署、策略、Prometheus 响应及健康响应存于私有报告
`/tmp/halro-ha-health-kind-20260928/evidence/split-prometheus-local.json`；
报告 SHA-256 为
`6b10ecb38204537db8bbb110e86a60aba5eea67a5afc2d14805d327950df3b35`。
报告逐项列出原始文件摘要。证书仅为本地两天演练材料，尚未验证独立 PKI
及轮换；Prometheus 原生 mTLS 保护完整内部只读 API，若要按路径收紧，
还需受审代理。客户端最终结果来源和外部不可变归档仍未接入。

在拆分部署上将 `ha-prometheus` Deployment 从 1 缩至 0，确认 Pod 删除后，
操作员 mTLS 访问健康服务 `/api/health` 从故障前 HTTP 200 变为 HTTP 503
`monitoring data unavailable`。将同一 Deployment 恢复为 1 并等待 rollout
后，API 再次返回 HTTP 200：客户端、安全、追平为健康，确认和总览仍未知。
三个原始响应的摘要写在私有报告
`/tmp/halro-ha-health-kind-20260928/evidence/prometheus-outage-local.json`，
其 SHA-256 为
`09d08ffd2e8b66cc72fcde17bbe79c2d16d527910d5601431579e7415cd541b3`。
此轮只模拟 Prometheus Pod 停服，未丢失 PVC 或节点，也不形成 RTO 数值。

## kind 独立健康服务重启与本地证据读回（约 05:31 UTC）

在 Prometheus 和三个 HA 成员运行时，先经操作员 mTLS 保存健康、事件归档、
持久迁移采集三个 API 原始响应，再对 `ha-health` Deployment 执行 rollout
restart。Pod UID 从 `a36d0b76-098b-405f-88c3-6e567e41c2be` 换为
`8c71cdc1-1c7c-4601-86df-70607119ae5e`；新 Pod 使用原
`ha-health-data` PVC，三个 API 均恢复 HTTP 200。事件归档前后均为 `ok`，
47 条记录的规范化内容 SHA-256 均为
`3a2dfded3622b473b98f2e8fd0e9018693f536f127bbc0152c7bd5ab95983291`。
持久迁移采集前后均为 `caught_up`，三个成员的 journal ID、incarnation、
基线、存储序号和观测头一致；当前每条链仍为序号 0、无基线后事件，
关闭段数为 0。健康客户端、安全、追平卡仍健康，确认和总览仍未知；
结束时 Prometheus 与健康服务 Deployment 均 1/1、HA StatefulSet 3/3。

私有报告 `/tmp/halro-ha-health-kind-20260928/evidence/health-restart-local.json`
包含六份原始响应的逐项 SHA-256，报告自身 SHA-256 为
`813825303d689fca5ddb23f43cd622a18ca0a610a6d029453387718adb002577`。
这只验证当前 PVC 中基线链和已采事件在健康 Pod 更换后可读；没有验证非空
持久迁移链、段轮换、PVC 丢失、节点故障或外部不可变留存。

## kind Prometheus 客户端证书重叠轮换与旧 SAN 撤销（约 05:37–05:39 UTC）

在现有专用本地 CA 下签发 SAN 为 `ha-health-prometheus-client-v2` 的新客户端
证书，原证书 SAN 为 `ha-health-prometheus-client`。先让 Prometheus Web 配置
同时允许两个 SAN，使用部署锁定的 Prometheus 3.13.0 `promtool check
web-config` 核对重叠与最终两份配置均有效，再 rollout Prometheus；旧、新
证书经受控诊断端口分别得到 HTTP 200。随后更新健康服务客户端 Secret 并
rollout 健康服务，新服务 `/api/health` 返回 HTTP 200。最后只保留新 SAN 并
再次 rollout Prometheus：旧证书遭 TLS `bad certificate` 拒绝（curl 退出
56、HTTP 000），新证书仍返回 HTTP 200，健康服务 API 继续 HTTP 200。
结束时两个监控 Deployment 均 1/1、三个 HA 成员 3/3；健康客户端、安全、
追平卡健康，确认和总览未知。

旧证书 SHA-256 指纹为
`E671F0B82942B5F2EE4A90FECFC32DD841FCAF800277BCB75F707E8AEEACCCC7`，
新证书指纹为
`2032B7BBE414F44988A32E7D947CCE8FFB5C77EC6523D057BDDEC5F4EC38061D`。
私有报告 `/tmp/halro-ha-health-kind-20260928/evidence/prom-client-rotation-local.json`
列出两版配置、公开证书及健康响应摘要，自身 SHA-256 为
`63a45813984b13f47b214d8959608c1b24913f1c77181543f5935e67568a4148`。
本轮轮换保留原 CA 和 Prometheus 服务端证书；它验证了本地客户端身份切换
与旧 SAN 拒绝，不等于 CA/服务端证书轮换、生产 PKI、RBAC、审计或长期证书
生命周期验收。私钥只保留在私有临时目录和 Kubernetes Secret，未写入仓库。

## kind 固定 HA 指标单轮缺报与告警根因修复（约 05:45–06:02 UTC）

只在 Prometheus 的 `halro-member-2` 抓取中用 `metric_relabel_configs` 过滤
`halro_cluster_maintenance`，不改成员进程或其他指标。Prometheus 3.13.0
检查了故障配置语法。恢复后读取同一 TSDB 的原始范围样本：该指标最后一次
故障前采样为 05:44:52.165 UTC，下次恢复采样为 05:59:00.894 UTC，
缺口 848.729 秒；期间 `halro-2` 有 165 次 `up=1` 原始样本。健康页的安全
与追平卡转未知，客户端卡保持健康，没有把旧指标补成绿色。

旧 `HalroMemberHASignalMissing` 只检查即时向量的数量，Prometheus 默认
回看会继续返回旧样本；本地旧规则直到 05:50:02.917 UTC 才进入 pending，
05:51:02.917 UTC firing，独立 webhook 于 05:51:07.938 UTC 收到通知。
这里必须使用原始范围向量或 `timestamp()` 读取样本时刻：即时查询响应里
的时间是**求值时刻**，不能拿它充当最近原始采样时间。

修订规则要求维护、启动裁决、复制阻断、三种不兼容原因及 Peer 连接的原始
采样时刻与同成员 `up` 完全一致，然后再计算覆盖数量。新的锁定 Prometheus
规则夹具包含“已有旧值、下一轮缺报、持续一分钟 firing、恢复”回归，并通过
`promtool test rules`；当前 63 条告警规则通过 `promtool check rules`。
在缺报仍持续时加载修订规则，`halro-2` 于 05:57:02.917 UTC 进入 pending，
约一分钟后 firing。恢复原抓取配置后，`up` 与维护指标再次同轮出现，健康
安全/追平回到健康，Prometheus 活动缺报告警清空；本地接收器最终于
06:02:08.099 UTC 收到 resolved（`endsAt` 06:02:02.917 UTC）。

私有报告 `/tmp/halro-ha-health-kind-20260928/evidence/metric-omission-local.json`
逐项列出配置、原始范围样本、健康响应、规则状态和 webhook 回执的 SHA-256；
报告 SHA-256 为
`29cc892a1048af3ff7e4ed488b8608f424552303287d0e42f60fa200e4f66097`。
结束时过滤规则已撤销，修订告警规则保留，Prometheus、健康服务、
Alertmanager、接收器均 1/1，HA 成员 3/3。修订规则是在缺报已发生后加载；
从首次缺报到一分钟触发由规则夹具证明，本地仍须逐项完成其他固定信号
及更多故障矩阵。webhook 是本地演练接收器，不是生产联系点。

## kind 角色缺报与角色类告警同轮判定（约 06:18–06:24 UTC）

审查角色、成员身份、incarnation 与 Primary 数量规则时发现同类 Prometheus
lookback 风险：旧样本可能在下一轮 `up=1` 后继续被即时向量返回，使角色缺报
延迟、旧 Primary 与新 Replica 并存造成误报，或旧 incarnation 被当成实时冲突。
六条规则现将每条角色、身份、incarnation 原始样本时间与**同一个成员**的
`up` 样本时间对齐；带 `role`、`cluster_id/node_id`、`incarnation` 标签的
序列分别独立校验，不让同实例另一条新序列替旧序列证明新鲜。
锁定镜像的 `promtool check rules`、`promtool test rules` 和
`go test -count=1 ./deploy/observability` 通过。新夹具覆盖角色/身份缺报、
旧 Primary 变 Replica、角色在成员间移交和 incarnation 更换；现有真冲突
夹具也带同轮 `up` 证据。规则已加载到当前 kind Prometheus，六条规则健康
状态为 `ok`，三个成员 `up=1`。

只对 `halro-member-2` 的抓取过滤 `halro_cluster_role`，不改成员进程。
故障配置通过 Prometheus 3.13.0 `promtool check config --syntax-only`。
故障期间 `halro-2` 的 `up` 原始样本继续每约 5 秒为 1，角色仅剩故障前旧样本；
`HalroMemberRoleMissing{instance="halro-2"}` 于 06:18:47.917 UTC 进入
pending、06:19:47.917 UTC firing，独立本地 webhook 于 06:19:52.931 UTC
收到 firing。`HalroNoPrimary`、`HalroMultiplePrimaries` 和
`HalroMemberRoleAmbiguous` 未误触发。健康 API 显示该成员角色缺失，总览、
确认、安全、追平转未知，客户端 Service 卡仍健康。

恢复原抓取配置并 rollout 后，`halro-2` 的角色与 `up` 再次同轮采样，
06:21:23 UTC 检查 Prometheus 活动角色告警已清空；健康安全、追平卡回健康，
总览、确认仍因没有最近需确认写成功而未知。本次通过 Prometheus Pod 重启
恢复配置时，接收器未立即收到 resolved；Alertmanager 保留最后一次 firing 到
`endsAt` 06:23:47.917 UTC，本地 webhook 在 06:23:52.994 UTC 收到 resolved。
因此操作时要将 Prometheus 当前规则状态与 Alertmanager 通知生命周期分别核对。
本轮只注入一个角色缺报，真实角色切换、双 Primary、incarnation 冲突和
完整 G0–G7 故障矩阵仍未签署。

私有报告 `/tmp/halro-ha-health-kind-20260928/evidence/role-omission-local.json`
逐项保存故障前后配置、Prometheus 规则/告警与健康 API 原始文件的 SHA-256，
以及接收器 firing/resolved 时间线；报告 SHA-256 为
`6ce62104724efef7fa92ec7720cda4008c0129d1b3c98814bb1f8936d07fbe67`。

## kind 安全类告警旧样本门禁与身份纠错预演（约 06:29–06:45 UTC）

继续审查 HA 安全与进度告警，发现身份不符、复制水位顺序、承诺任期、
Primary 启动裁决/复制阻断/Peer 会话、确认停滞、Replica 应用停滞及兼容性
告警仍可能在成功的新抓取中读取旧指标或旧角色。九条规则现要求参与当前
判断的每个指标原始样本与该成员 `up` 同轮；五分钟 `delta` 仍按其定义读取
历史，但当前滞后量与索引必须同轮。无 Peer 会话规则还要求所有配置 Peer
的当轮 gauge 均存在，少一条时由信号缺报规则报告，不能断言“全断开”。

成员身份纠错夹具暴露了比误报更严重的问题：原表达式对新旧两条身份序列
分别 `label_replace` 时生成相同标签集，Prometheus 在修正后的回看窗口内
报 `vector cannot contain metrics with the same labelset`。规则现先按抓取
目标聚合为唯一来源，再改写预期标签，并只让当前同轮的身份样本参加不符
判定。夹具覆盖 `node_id` 与 `cluster_id` 纠错、旧水位/任期操作数、角色
切换、单 Peer gauge 缺报、旧不兼容及确认/应用滞后量缺报。锁定
Prometheus 3.13.0 的 63 条规则检查、`promtool test rules`、
`go test -count=1 ./deploy/observability` 与部署配置验证均通过。第一次
`validate.sh` 在受限 shell 中因 Docker socket 权限拒绝，授权本地 Docker
后同一脚本通过；这不是规则失败。

新规则加载至本地 kind 后，九条修订规则健康状态均为 `ok`，三个成员
`up=1`；安全/追平卡健康，确认/总览因无最近需确认写成功仍未知。
只在 `halro-member-2` 的 Prometheus `metric_relabel_configs` 中把
`halro_cluster_member_info` 的 `node_id` 临时改成 `halro-wrong`，成员实际
状态不变。错误身份原始指标出现后，`HalroMemberIdentityMismatch` 于
06:39:47.917 UTC firing，本地 webhook 06:39:52.942 UTC 收到通知，
健康安全/总览为 critical，而规则健康状态仍为 `ok`。

恢复原抓取配置后，06:41:29 UTC 的即时向量**同时**保留旧
`node_id=halro-wrong` 和新 `node_id=halro-2`，规则仍为 `ok`、身份不符活动
告警已清空。健康服务的 45 秒原始样本窗口短暂显示身份覆盖未知；旧错误
样本退出窗口后，06:41:43 UTC 安全/追平恢复健康。Alertmanager 于
06:43:47.917 UTC 结束最后一次 firing，本地 webhook 06:43:52.988 UTC
收到 resolved。结束时故障 relabel 已撤销，修订规则保留；本轮证明的是
**监控标签错误与纠错**，没有修改真实成员身份或执行身份轮换。

私有报告 `/tmp/halro-ha-health-kind-20260928/evidence/identity-mismatch-local.json`
保存故障配置、原始范围和即时样本、规则与健康响应摘要及通知时间线的文件
SHA-256；报告 SHA-256 为
`4cbf4fe0da4cdbffcccd2a1d5b53531d7dbed6d7fc64e2e007bf2a9f40445d03`。
真实状态损坏、错误集群、双 Primary、完整 G0–G7 与生产通知仍未验收。

随后补齐 `HalroReplicationIndexRegressed` 的当轮索引、任期和唯一
incarnation 门槛；五分钟 `delta` 仍保留真实历史比较，但本轮缺报时不再把旧
回退值解释为当前事故。新增“回退后索引缺报”夹具在最后有效采样告警，下一
轮成功抓取但缺报时停止告警。锁定版本规则夹具、63 条规则检查、定向 Go
测试和部署配置验证再通过；更新至 kind 后，20 条 HA 告警规则健康状态
全部 `ok`，三个成员 `up=1`，没有活动 HA 告警，客户端/安全/追平卡健康，
确认/总览仍因缺需确认写成功为未知。最终原始规则、告警、`up` 与健康响应
保存在私有文件
`/tmp/halro-ha-health-kind-20260928/evidence/regression-current-scrape-live.json`，
SHA-256 为
`e8bb1ca58d7f02d099fca2bed49d048bab61e1636b5455d6ff1249ba7be4b5bb`。

## 稳定任期窗口完整性与 term 缺报告警（约 07:08–07:27 UTC）

`halro:ha_epoch_stable:bool` 原先只要求任期五分钟内未变化、唯一 incarnation
且当前抓取有原始样本。只对 `halro-2` 的 Prometheus 抓取过滤 term 后，记录规则
在缺报时停止产生新样本；但 term 恢复后，即使五分钟窗口内仍有一段缺口，也
立即重新产生 `1`。已将 term 与 incarnation 在五分钟窗口的原始样本数分别
同该成员 `up` 对齐，同时保留当前成功抓取门槛。锁定 Prometheus 夹具验证两
种选择性缺报：缺报期间和恢复后窗口内记录缺席，缺口退出窗口后才恢复 `1`。
本地真实抓取的 term 最大间隔为 135.594 秒；恢复初期 term/up 窗口样本数为
33/57，稳定任期记录的 45 秒原始范围为空；缺口退出后变为 60/60，记录恢复
为 `1`。原始十分钟 TSDB 历史保存在私有文件
`/tmp/halro-ha-health-kind-20260928/evidence/epoch-window-coverage-raw-history.json`
（SHA-256 `9c6c500e33748a566a026766de80bdbaebd6a0c6b95a6031c301b7033f36d04d`），
演练报告为 `epoch-window-coverage-local.json`（SHA-256
`dfd296b477bfbf4d41b97837c14f0a0f0badfefd8ad1086a50f2dbf52999c1bd`）。
记录规则来源 SHA-256 为
`370dfb968850880fcde6a3c1a919027ab8f87af258c100a0ace591a789977fdc`。
整台 Prometheus 未抓取的窗口不能由相同样本数识别，仍需独立监控可用性证据。

同一次核查发现 `HalroMemberHASignalMissing` 尚未覆盖 term、promised term
与三类复制索引。现要求这五种标量信号与本轮 `up` 同时采到，三类索引各恰好
一条；新增夹具在其余信号完整时只让 term 缺报，另以重复 durable 索引和
缺失 applied 索引验证不能仅靠总数掩盖种类缺口；一分钟后 firing，恢复
后清除。规则夹具、63 条规则配置检查、`go test -count=1
./deploy/observability` 和观测配置验证通过。修订告警规则已加载本地 kind；
只过滤 `halro-2` term、保持 `up=1` 的故障中，故障时点 45 秒原始范围有
9 条 `up`、0 条 term；term 的即时向量仍保留旧值。该告警先 pending 再 firing。
恢复原抓取后规则 API 为 `inactive`、活动告警 API 为空，三成员均 `up=1`、
规则健康为 `ok`；`ALERTS` 即时向量短暂保留旧 firing 样本，不能替代活动
告警 API。故障 relabel 已撤销，私有报告
`/tmp/halro-ha-health-kind-20260928/evidence/core-term-missing-alert-local.json`
的 SHA-256 为
`db685644cfceb331cfae9c596975660834b580ea58d0ff448e3ffab47d02b5fb`，
告警规则来源 SHA-256 为
`e97a6b0555fc5abddd034f8c1c041b53db28a72f9cbfe31f74b69e4a43643398`。
本地仅演练 term 缺报；其他新增标量信号仍待逐项故障注入。

## 两成员需确认写仓库集成补验

`TestPrimaryAndReplicaRuntimesReplicateAndApplyAnAuditFrame` 原来直接追加
普通 `EventRequestAccepted`；该事件无需等待 Replica ACK，不能为 Ledger
必需确认计数提供正向证据。现在该测试用真实 accounting
`BeginRequestDetailed`/`ReserveAttemptDetailed` 写预留记录，本地 State
应用后经 Ledger receipt 等待 Replica 确认，再断言 `ledger/confirmed=1`。
同一测试在撤销 Gateway key 后断言 `metadata/confirmed=1`。定向普通及
`-race -count=1` 均通过，未调用任何 Provider。该验证仅在测试进程中的
两成员复制链完成；kind 三成员 Prometheus 的同五分钟窗口、健康页确认卡、
真实请求和客户端最终结果仍未据此签署。

## 重复采集来源、规则求值与恢复（约 07:51–08:18 UTC）

在本地 kind 的 Prometheus 清单中只给 `halro-2` 临时增加第二个抓取目标，
保留相同逻辑 `instance`，另加不同来源标签；成员进程和持久状态未修改。
首次演练时，健康页把成员标为来源冲突，安全与追平不绿，独立
`HalroMemberDuplicateScrapeSource` 告警 firing；但旧 HA 规则中 18/20
条因双份原始序列出现多对多匹配错误，恢复配置后还受 Prometheus 即时
回看期影响。首次故障报告
`/tmp/halro-ha-health-kind-20260928/evidence/duplicate-member-source-local.json`
的 SHA-256 为 `67abadedf453b4780ac4a13863a7bd98fabb30943943dd5432c694711d13cd0d`。
这是规则健康缺陷，不能将页面和单条告警的正确表现当作通过。

规则现按逻辑成员聚合 `up` 的原始抓取时间，比较类告警要求唯一采集来源；
双份 Primary 角色也不能误报双 Primary 或角色冲突。稳定任期记录额外要求
五分钟内 term、incarnation、`up` 各只有一个来源。锁定版本的规则夹具、
64 条告警与 14 条记录规则检查、`go test -count=1 ./deploy/observability`
以及观测配置验证均通过。最终规则重新加载到 kind 后再次注入相同故障：
45 秒窗口中 `halro-2` 的 `up` 与 term 各有两条来源、每条 9 个样本；
健康页标 `conflicted=true`，安全/追平未知，总览因专门告警为 critical。
故障时 21 条 HA 告警规则与稳定任期记录规则均为 `ok`；逐条以故障时刻
重查 21 条告警表达式均无错误，仅重复来源告警命中，`halro-2` 的稳定任期
记录缺席，其他两成员仍为 `1`。这证明本次重复来源注入时的规则求值，
不代表其他故障组合均已覆盖。

撤销第二目标后，健康页 45 秒原始窗口先恢复成员唯一性与安全/追平健康；
专门告警继续保留到旧序列退出 Prometheus 五分钟即时回看期。最终
三个成员各只有一条 `up=1` 来源，稳定任期记录均为 `1`，21 条 HA 告警
规则健康，活动 HA 告警为空。本地 webhook 在 08:07:07.934 UTC 收到
firing，08:13:08.015 UTC 收到 resolved，故障抓取配置已撤销。
私有报告 `/tmp/halro-ha-health-kind-20260928/evidence/duplicate-source-normalized-rules-local.json`
包含故障时点原始来源、21 条表达式求值、恢复状态与通知时间线，
SHA-256 为 `0db9891f7f71718ee0267fbe3a5df6058728c419ade4333d0efef13d4cdac708`。
告警及记录规则源文件 SHA-256 分别为
`84e77f270934dc45509b46a1688a5c79bbdf879beb849aff185e455223e8b990`
和 `68193da13e06109d0d89cb617d3b630ac8386e6b4083774d97b2bed4fa448c22`。

## 非计费需确认写与请求影响正向验收（约 08:36–09:02 UTC）

为补齐同窗口 Ledger 与 metadata 证据，在隔离 kind 内部署只响应固定
OpenAI 形状模型列表与非流式 Chat 的 HTTPS 替身；证书由该环境的一次性 CA
签发，Service 仅为 ClusterIP。先私有备份三成员配置，临时启用
`allow_private_provider_endpoints` 并通过 `SSL_CERT_FILE` 信任本地 CA，
逐成员重启并核对 Ready。Admin 从替身真实 `GET /v1/models` 发现
`halro-ha-test`，创建启用的测试 Provider、探针为 `healthy` 的 Deployment、
明确 `free` 的价格、测试 Route 和仅允许该别名的 Project/Key。没有调用
外部 Provider 或运行计费 smoke。

首次经 Primary Gateway 发起一笔非流式请求，完整收到 HTTP 200 和固定响应；
同窗口更新测试 Project 名称，触发需确认 metadata 写。Prometheus 的
Primary 原始计数由 `ledger/confirmed=0`、`metadata/confirmed=0` 变为
`2` 与 `1`，两类五分钟 `increase` 均为正；三成员 `up=1`、稳定任期记录
均为 `1`，三个成员的 durable/confirmed/applied 同为 `73/73/73`。
健康页五张卡均为 `healthy`，其确认依据保留当前计数与稳定任期原始样本时间。
这证明本地内部必需 ACK 链和手工测试客户端收到这一次完整响应，不构成
真实 SDK 的重试、流终止或客户端最终逻辑操作结果生产来源。

同一轮发现 `/api/impact` 返回 `observed=false`，而 Prometheus 已有三成员
各八类当前 HTTP 结果序列及全部五分钟增量。根因是代码在查询前记录本机
`evaluated_at`，却让 Prometheus 在稍后自行选择求值时间，再把该时间判为
“未来样本”。已将原始覆盖查询和增量查询固定在同一个毫秒精度求值时刻，
回归测试要求两次请求带相同 `time` 参数并以该真实求值时间构造样本。
`go test -count=1 ./cmd/halro-ha-health`、定向测试、`go vet`、格式与空白
检查通过。重新构建并部署健康服务镜像
`sha256:aed16d4a187ec938e2adb2d15ade4171dff0fad442975da2261b8d21e99d9b51`
后，`/api/impact` 在无近期写时先返回 `observed=true`、各类估算增量为 0。
再次发送本地固定请求并更新 Project 后，该接口返回 `observed=true`，
2xx 五分钟 Prometheus 估算增量约 `1.017`；随后的同次快照五张卡均健康。
中间一次快照因 Primary 空闲 Peer 会话短暂断开使确认卡降级，下一轮认证
会话恢复后重新健康；不能将单轮连接状态当作持续可用性证明。

验收后立即停用测试 Key，原 Key 调用得到 HTTP 401；禁用测试 Project 并清除
允许模型，删除 Route、Deployment、Provider、Credential 和替身工作负载。
原三成员 Secret 字节已恢复，`SSL_CERT_FILE` 已从 StatefulSet 移除，
三成员逐个重启后全部 Ready。Admin 读回 Provider、Deployment、Route、
Credential 列表均为空，测试 Project 保留为禁用且无允许模型，Key 为禁用，
以保留本地审计历史。结束时三个 `up=1`、21 条 HA 告警规则健康、活动 HA
告警为空；确认卡因重启后没有近期需确认成功恢复为 `unknown`，安全与追平
仍健康，证明没有沿用旧绿灯。

私有总报告
`/tmp/halro-ha-health-kind-20260928/evidence/nonbillable-ledger-metadata-and-impact-local.json`
包含各阶段原始文件 SHA-256、Gateway 响应、替身访问日志、健康/影响响应、
恢复后监控摘要与源码 SHA-256，报告 SHA-256 为
`4d3bb622a6500e2fda18d427f78bddccb121193a83e0fcd12792b5f90a239c2c`。
本地短有效期 CA、同一测试机、未冻结发布候选及未完成 G0–G7/72 小时
长跑限制了结论；生产客户端最终来源和外部不可变归档仍未接入。

## 双 Replica ACK 停滞、失败写对账与确认卡修复（约 09:14–09:36 UTC）

在同一隔离 kind 中确认 StatefulSet 从 3 缩到 1 时三个 PVC 均按 `Retain`
保留。故障前 `up=1/1/1`、三成员水位均为 `110/110/110`，安全和追平卡
健康；近期没有必需确认成功，确认卡及总览为未知，`/api/impact` 完整观测。
临时移除两个 Replica 后，保持唯一 Primary，向此前已禁用且无允许模型的
专用测试 Project 发起一次无计费 `PUT` 元数据更新。第一次镜像的请求立即
返回 HTTP 400，错误实际是复制不可用；Primary 随后导出
`halro_replication_unavailable=1`、`metadata/unavailable=1`，水位为
`115/110/110`。两个 Replica 的 `up=0`，严格要求全成员结果序列的
`/api/impact` 为 `observed=false`。`HalroReplicationUnavailable` 和
`HalroTargetDown` 都达到 firing；持久本地 webhook 于
09:17:22.940 UTC 收到前者 firing，于 09:18:32.958 UTC 收到 resolved。
故障期间健康总览因告警为 critical，但确认卡在 Primary 已明确阻断时仍显示
unknown，暴露出局部成员缺报覆盖了已知负面证据的判定缺口。

恢复两个 Replica 后，失败请求的测试 Project 实际从修订 4 变为 5，名称
也变为请求值；HTTP 失败并不证明本地持久变更永不确认。回读并核对后，以
正常三成员需确认写恢复原名称，修订变为 6。源码现把被包装的复制不可用
错误映射为 Admin `503`、`code=replication_unavailable`，正文提示先回读资源
再决定重试，并为中英文 Console 提供相同的可操作提示，不泄露 Peer 内部错误。
确认卡仅在新鲜、身份一致且无矛盾的唯一 Primary 明确报告阻断时，即使
Replica 覆盖缺失也显示 degraded；安全和追平卡继续 unknown，任何失鲜、
错标或第二 Primary 仍不能凭这一信号得出可信确认结论。

修复镜像现场复测同一个禁用 Project：两个 Replica 缺席时，Admin 在约
0.026 秒返回上述 HTTP 503；Primary `unavailable=1` 且
`metadata/unavailable=1`，水位为 `126/123/123`；确认卡和总览为 degraded，
安全/追平 unknown，`/api/impact observed=false`。本次较短故障未等待
`for` 后再次触发投递，不借第一次的 webhook 回执代签第二次通知。恢复后
再次读到失败请求已生效，修订 7；以正常三成员写恢复原名称，修订 8。
最终测试 Project 仍禁用、允许模型为空；三成员 Ready、三个 PVC 留存，
水位均为 `133/133/133`，三个 `up=1`、阻断指标为 0、21 条 HA 告警规则
均 `ok`、活动告警为空。安全和追平卡健康；进程重启后稳定任期窗口尚无
近期必需确认成功，确认卡及总览保持未知，没有沿用故障前绿灯。

现场健康服务镜像 ID 为
`sha256:bb6b4ab5fb92243e518ae32b6fb81e8f5c89e3b698e4f382b1ae5e4598581eca`；
含最终 Console 本地化与嵌入 bundle 的三成员镜像 ID 为
`sha256:d8fda71526651c37819f2a2fcab87511e291449159e006c45b9248952ed0b4d9`。
`internal/hahealth` 整包、受影响 Go 定向测试、受影响包 `go vet` 和格式检查
通过。`internal/app` 整包运行发现新错误码未本地化；补齐后对应 Go 定向
测试通过，按仓库策略未为仅本地化修订重跑约 263 秒的整包。前端 Node 22
下 48 个测试文件、700 项测试通过；typecheck、生产构建、bundle 与浏览器
产物检查及重建前后嵌入 bundle 对比通过。发布前仍须按最终候选运行全仓
门禁，不能把这组本地镜像当作不可变发布候选。

私有报告
`/tmp/halro-ha-health-kind-20260928/evidence/ack-stall-local-report.json`
包含八阶段健康/影响/原始指标、两次 Admin 响应、Project 修订对账、
告警投递回执、102 份原始证据文件摘要、源码/镜像摘要及恢复后的工作负载
读回；报告 SHA-256 为
`8437b1602bf3949e666a61449e31000c467ee9e68cc5077bb8610722e545afe5`。
此次仅覆盖无计费 metadata 必需确认故障，尚未验证 Ledger 写在 ACK 停滞下
的客户端最终结果、流式终止与重试去重、人工提升、跨故障域和独立不可变归档，
不能替代完整 G0–G7 与 72 小时验收。

## 计划交接与非空持久迁移链对账（约 09:49–10:03 UTC）

在专用 `kind-halro-ha-health-local` 中，先保存交接前健康与 Prometheus
快照。三个成员为 term 1、`134/134/134`，恰有一个 Primary；安全和追平
卡健康，确认卡因近期没有需确认成功保持未知。将 StatefulSet 从 3 缩为 2，
确认 `halro-2` 完全退出、PVC 仍保留后，用不带 HA 成员 Service 标签的
短时助手独占挂载该 PVC。助手仅临时获得到成员复制端口 9910 的 NetworkPolicy
准入；其管理员密码经本机私有文件和 stdin 传给 CLI，没有写入 Kubernetes
Secret 或命令参数。交接前目标 PVC 另存本机 `0600` 快照。

目标 `halro-2`、旧 Primary `halro-0` 和保留 Replica `halro-1` 的认证
状态均为 term 1、`134/134/134`。在**停服的目标 Replica** 上执行
`cluster stepdown --from halro-0 --to halro-2 --expect-term 1 --expect-index
134`，管理员再认证通过，收到 `halro-0` 和 `halro-1` 对 term 2 的持久
承诺。目标本地先持久记录 term 2 Primary、`137/134/134`；旧 Primary
退出并作为 Replica 重启。卸载助手、将 StatefulSet 恢复为 3 后，三成员
均 Ready，term 2 恰有一个 Primary `halro-2`、两个 Replica，三个持久/
确认/应用水位先共同到达 `139/139/139`，后续读回新 Primary 已到
`142/142/142`。临时复制端口准入已删除。健康 API 返回 HTTP 200，
三个 `up=1`，安全与追平健康，活动相关告警为空；近期确认能力仍为未知，
因此总览没有误标全绿。这是一次隔离本地计划交接，不是正式 RTO/RPO、
无计划失联提升或客户端最终操作验收。

独立采集器的 `/api/durable-transitions` 在交接后为 `caught_up`：三个成员
各有两条事件。`halro-0` 为 `promise → adopt_higher_term`，`halro-1`
同样为 `promise → adopt_higher_term`，`halro-2` 为 `promise → promote`；
各自 stored sequence 与 observed head 均为 2。将唯一使用证据 PVC 的健康
Deployment 停至 0，等待 Pod 删除；只读挂载该卷后导出私有冻结 tar，离线
`-verify-durable-snapshot` 返回 `local_files_verified`，三条链的 stored/
committed sequence 都是 2、覆盖为 `caught_up_to_observed_head`，清单根
`1bb9727342855f08e9fbf5b575113fd65f4438de73470c52e86be33e6efd9722`。

随后短停全部三个 HA Pod，分别在所属节点只读挂载独立 PVC，从停机数据
复制稳定快照，再以匹配的 Master Key 和成员配置执行
`cluster verify-transition-snapshot`。三个结果均为 `member_mac_verified`，
序号 2；与冻结采集器报告比较得到 `snapshot_heads_match`，逐成员 incarnation、
journal ID、基线、提交摘要与事件序列哈希均相同。每个成员原始 PVC 的
`state.json` 和 `transitions.journal` 字节 SHA-256 也与其认证报告中的文件
清单一致。全部冻结助手已删除，三成员与 Prometheus、Alertmanager、
接收器、健康服务重新 Ready；健康服务重启后再次追到三条序号 2 的链头。

私有本机证据位于 `/tmp/halro-ha-health-kind-20260928/`，其中：

| 文件 | SHA-256 |
| --- | --- |
| `evidence/handoff-collector-frozen.tar` | `6570973d1d4dfbb77c8386cf800353babe1cd05f0cf7313355bc555243abad9c` |
| `evidence/handoff-durable-snapshot-verification.json` | `39dfc2868a4c9cf627575d563d8719fd447875de06c1ac101e611e333b230919` |
| `evidence/handoff-transition-head-comparison.json` | `5c8ffd0cf8a65ab372b0b06261e19ed46964b72fb17cc805b744135bb7dbbf95` |
| `handoff-member-0-frozen.tar` | `dd17524c608e7c6212417b0ae882da54c4ffea7fc718760d236871a438f1b315` |
| `handoff-member-1-frozen.tar` | `e1364651be13a4016cb087c01152e0395b9da9ef8e49e917dace24afd494d121` |
| `handoff-member-2-frozen.tar` | `fec418dabccf9fed99fca18ec9d9a164493465e8399a009dc02c7bbbcb4f8aee` |

这份冻结与对账在**同一台本地机器**完成，未取得独立不可变存储的归档
回执和归档副本读回；Legacy 基线之前的历史不可重建，也未触发关闭段
轮换。报告比较在本机验证了三份成员报告及其冻结文件的对应关系，不等于
外部故障域的长期留存证明，不能授权删除旧段。客户端最终结果来源仍未指定，
完整 G0–G7、72 小时长跑及正式放行仍为 `NOT_RUN`。

## 旧 Primary 隔离、人工提升、回归拒绝与重播种（约 10:07–10:44 UTC）

在同一专用 kind 中，交接后基线为 term 2、三成员认证水位
`142/142/142`，`halro-2` 是唯一 Primary；三成员采集 `up=1`、安全与追平
健康。记录旧 Pod UID `85096758-fe4a-4271-bb5e-3d499fd55239` 后，把
StatefulSet 缩至 2 并确认 `halro-2` Pod 已删除、PVC `data-halro-2`
保留、控制器期望副本数为 2。此为本地外部隔离事实；不是生产环境的节点
电源、存储或网络 fencing。旧 Primary 缺席时，健康入口仍返回 HTTP 200，
客户端 Service 只路由到 Replica，客户端卡转未知，安全/追平未知，
`/api/impact observed=false`。旧 Primary 正常退出使余下两个 Replica
认证到 term 2、`143/143/143`。

继续缩至 1，停服并独占挂载候选 `halro-1` 的 PVC；候选与仍运行的
`halro-0` 均为 `143/143/143`。在候选上执行 `cluster promote`，准确
指定 `--expect-term 2 --expect-index 143 --old-primary halro-2
--old-primary-fenced-by pod-deleted-pvc-retained`，管理员口令仅由本机私有
文件经 stdin 进入命令。命令收到 `halro-0` 对 term 3 的持久承诺，然后
持久发布 `halro-1` 为 term 3 Primary；未使用 `--no-peer-promise`。
启动新 Primary 后，`halro-0/1` 均为 term 3、`147/147/147`，旧
`halro-2` 继续隔离。客户端 Service 再次到达 Primary；固定三成员清单
因第三成员缺席仍保持安全/追平未知，`HalroTargetDown` firing 使总览
critical。这些卡没有把一个可工作的双成员 quorum 误写成完整三成员覆盖。

旧 Primary 的只读 PVC 快照认证为 term 2、`143/143/143` 后，受控将其
原 PVC 加回。它始终未通过 readiness，出现 CrashLoop，持久
`reseed-required` 标记写明：旧成员尝试回归后的 `145/143/143` 与更高
任期 Peer 的 `147/147` 不兼容。重新缩至 2 隔离该 Pod，保存原目录
完整字节和拒绝日志；没有删除标记或修改 `state.json`。从停机快照运行
`cluster verify-transition-snapshot` 返回 `member_mac_verified`，旧日志
最终 committed sequence 为 3；独立采集器在该成员消失前只保存到 sequence
2，因此存在**一条已提交但未采到的旧链迁移事件**。这是本次演练发现的
实际证据缺口，不是可用健康页颜色代替的推测。

新增的离线尾页读取命令在该冻结旧成员目录上，用独立采集器的原始旧游标
`(fcb2f54e5caa4ae8852a70e62d0c7ad5, sequence 2)` 精确读取，
返回且仅返回 sequence 3 的已提交 `adopt_higher_term` 事件；其
`previous_digest` 等于旧游标摘要，返回的终点摘要等于旧成员 MAC 报告的
committed digest，文件清单摘要仍为
`4a499c08b5d26ab704f5e5334863a139cd95235c27bd28fbdffe1f6e0cacb600`。
只读结果保存在私有本机
`/tmp/halro-ha-health-kind-20260928/evidence/failover-old-member-authenticated-tail.json`，
SHA-256 为 `56e211dcfe964dde7ae188753f83bb19e80d93cd5e77115fe78ee1c25fdcd8c0`。
这是旧事件的可核验补采，**尚未**写入采集器，也未证明新日志代际获批交接。

在原采集器的**隔离副本**上演练停机导入：从
`handoff-collector-frozen` 复制私有 journal，在不触碰运行中 PVC 的条件下，
对同一冻结旧成员目录运行 `-reconcile-retired-member-snapshot`。命令独占
采集器锁、重验成员快照，并从旧 sequence 2 恰好导入一条事件至 sequence
3，返回 `retired_member_tail_imported_new_generation_unapproved`。导入后的
采集器副本再次通过 `-verify-durable-snapshot`，其本地文件清单摘要为
`e1696a75b195e440c2ba079563bfb346a257916aa93f339403b05e04090c728d`，
`halro-2` 事件 SHA-256 为
`347c7c2274d24b67aecd7a94109081a97c6969a799c9a1dac0964c09f78de389`，
与冻结成员的 MAC 报告相同。该隔离演练完成时，**在线**采集器仍保留旧
sequence 2 和 `partial`；后续停机导入见本节末尾。

为恢复第三成员，先停止已追平的新 Primary `halro-1`；其认证状态 term 3、
`148/148/148`。管理员再认证执行 `seed-approve --target halro-2`，
清单签 MAC 并列出六个权威文件；逐项 SHA-256 与停机源快照匹配。
在目标 PVC 内把旧 `data` 重命名为 `retired-data-term2-index143` 保留，
把且仅把获批六文件放入同文件系统私有 staging；`seed-install` 验证
清单 MAC、文件和排序前缀后原子发布 term 3/index 148 的 v2 Replica。
停机状态以精确角色、任期及三个 index 执行 `migrate-transitions`，得到
v3 新日志 ID `7fdc1036750b701a0cf528795ffd847b`。恢复 StatefulSet
后，三成员 Ready，恰一个 `halro-1` Primary、两个 Replica，最终三个
水位均为 `150/150/150`。监控四个 Deployment Ready，三目标 `up=1`，
活动 HA 告警清空；近期无必需确认成功，确认卡和总览仍为未知。

重播种还揭示采集器的代际缺口：集群 incarnation 未变，但 `halro-2`
的新成员日志 ID 不同。旧健康镜像保留旧 sequence 2 并报 `cursor_conflict`；
新增的只读诊断仅在第二次认证读取确认日志 ID 已变时报告
`journal_changed_requires_reconciliation`，**不**自动重置、拼接或覆盖
旧游标。新健康镜像
`sha256:53fd0abbf9346201658f47cbc24c44cce57869bb49b77200d88555548d0115a8`
已在 kind 实测：`/api/durable-transitions` 为 `partial`，`halro-0/1`
各追到 sequence 4，`halro-2` 的旧链停在 sequence 2 并明确待对账。
采集端包测试 `-count=1`、`go vet` 和 `git diff --check` 通过；新诊断并非
旧事件补采或代际归档交接。正式的同 incarnation 重播种证据协议须按
[持久迁移契约](../contracts/ha-transition-durability.md#same-incarnation-member-reseed-is-a-separate-evidence-handoff)
继续实现，未完成前不能把持久迁移全链列为通过。

随后把包含新诊断和中文操作提示的健康镜像
`sha256:63d9c0731b4995fb71e9866bce03291091aa5b7c0cf759bb23125a729bfed82c`
加载到同一 kind 并完成 `ha-health` Deployment rollout。带操作员 mTLS
实际读取 `/api/durable-transitions` 为 HTTP 200，仍是 `partial`：
`halro-0/1` sequence 4，`halro-2` 旧 sequence 2 且明确报
`journal_changed_requires_reconciliation`；页面 `/app.js` 返回 HTTP 200，
SHA-256 为 `f84f60961d05f132961d71b1c92eaeb9259400e1f12a6cb8b0416011867fb3a1`，
包含保留采集器文件和旧成员数据的提示。定向页面测试 18/18 通过。

随后停下本地 `ha-health` Deployment 并确认 Pod 退出；临时非 root
维护 Pod 独占挂载 `ha-health-data`。把 PVC 的完整私有证据目录复制到
权限受限的本机目录，原 `durable-transitions.json` SHA-256
`2f6796866faa10b3e9b679e2f9568f0582c05b53d5008f274ac7261f42eeaa03`
与 PVC 逐项一致；`event-journal.json` 未改。在本机副本上再次执行停机
旧链导入，只新增 `halro-2` 的 authenticated sequence 3，`halro-0/1`
各保留 sequence 4。导入后的 `-verify-durable-snapshot` 返回
`local_files_verified`，清单摘要
`6098ae090cda1e83ea60fc43d5aa7a11882c0c0f58138bed979d672132c44505`；
差异核对显示仅 `halro-2` 增加该事件。原文件在 PVC 及本机均留有
备份，已核验暂存文件 SHA-256
`f823680c3fb0626ffa764eb411a635d2180d275d4f69d8bcc0363877ec5fa9ad`，
再于同一 PVC 原子替换并同步。删除维护 Pod、恢复 Deployment 后，
操作员 mTLS 读取 `/api/durable-transitions` HTTP 200：
`halro-0/1` sequence 4，`halro-2` **旧链 sequence 3**，但仍是
`partial` / `journal_changed_requires_reconciliation`，没有自动接受新日志。
同轮 `/api/health` HTTP 200：客户端入口、安全、追平三卡为 healthy；
近五分钟无必需确认成功，确认卡与总览为 unknown。`/api/alerts` HTTP 200
且活动告警数为 0。这些是恢复后的本地观测，不能替代最终客户端结果。
本机导入及验证报告
`evidence/failover-live-old-tail-reconcile-report.json` SHA-256 为
`d3780143c37d6d0a64baa5bc519f9bdbb8a3c877b06b04ff7fdad6d22adf6e94`；
恢复后 API 响应
`evidence/failover-live-after-old-tail-durable-transitions.json` SHA-256 为
`bd4ba5a9854e6d6509477860c5706c2b13a8dfd63209963b4eeaa0eff6ec984b`。
两文件位于 `/tmp/halro-ha-health-kind-20260928/` 下，仍无独立不可变回执。

## 同 incarnation 重播种的离线来源核验与交接预检（约 10:19–11:46 UTC）

在专用 `kind-halro-ha-health-local`，从此前签署的批准包提取精确六个
权威文件，并只读核验批准清单、文件字节和排序日志 MAC 前缀。停机源
Primary `halro-1` 的冻结副本同时核验其 term 3/index 148 的 Primary
状态、完整迁移日志、排序头及权威文件；这些文件与批准包逐字节一致。
新成员 `halro-2` 曾短暂停止以复制当前 PVC 的只读冻结副本，随后
StatefulSet 恢复 3 副本，`halro-2` 再次 Ready。新副本核验了 seed
安装应产生的**精确 v2 Replica 状态**与新 v3 迁移日志基线 MAC、批准的
排序前缀及当前排序头。没有修改运行中的成员数据。

完整离线预检使用冻结采集器、退休旧成员、源 Primary、批准包和新成员
副本。它认证旧 `halro-2` journal
`fcb2f54e5caa4ae8852a70e62d0c7ad5` 的提交序号 3、末摘要
`248f9a265292b8a206c9fdbb11ddb24c674506fe1986f43d8a91b761bc7b2b87`
与采集器完整一致；新 journal
`7fdc1036750b701a0cf528795ffd847b` 的基线摘要为
`87f1bb299eb19129270dcd16c10b40bb5172c57733abef706125b322b2944e34`。
获批 seed 清单摘要为
`dae37afe0aa43590fce546deb041fedc6b9fa55b3290ce7352fb412201f9e665`；
采集器清单摘要仍为
`6098ae090cda1e83ea60fc43d5aa7a11882c0c0f58138bed979d672132c44505`。

| 本机私有报告（`/tmp/halro-ha-health-kind-20260928/evidence/`） | SHA-256 |
| --- | --- |
| `failover-seed-approval-offline-verification.json` | `9ca3b7404a23b95bf2f0e5adff67f6e3bf166f214344194e97906c5ed3f91560` |
| `failover-source-primary-seed-origin-verification.json` | `eb49f7a5ad379998f44302ccf162f52104e3e9cff5dac76dbd552590be0330a2` |
| `failover-new-member-seed-origin-verification.json` | `8c36cb13693812ba2afd5295c7dc078d157ed57a7d097f2561d194ae7cd914f2` |
| `failover-reseed-handoff-preflight.json` | `f797c97bac26b3346d5f30961f055949c027dbc59b941983a5ce610c2f1aa5bc` |

预检状态为 `authenticated_inputs_match_handoff_uncommitted`：它**没有**提交
采集器代际记录，健康服务仍显示 `partial` / `journal_changed_requires_reconciliation`。
冻结时间与隔离证据摘要来自操作员输入，本预检只保留这些关联，不独立
证明隔离动作。报告及原始快照仅在本机私有 `/tmp` 目录，没有独立不可变
存储回执或读回；因此不能据此签署完整链、删段或生产留存验收。

本机私有报告
`/tmp/halro-ha-health-kind-20260928/evidence/failover-local-report.json`
汇总隔离前后健康/原始指标、Pod UID 与拒绝状态、旧成员 MAC 报告、
获批 seed 清单、六文件快照、旧 PVC 与候选快照及最终诊断共 67 份文件的
SHA-256；报告本身的 SHA-256 为
`5f52e3fe36f528d1dded4900fce1db795ee8b83ffefee5d075a98e3e7e8510da`。
所有 tar 与配置留在权限受限的本机 `/tmp`，没有外部不可变回执；本次
没有计费 Provider 调用、真实客户端重试/流消费、正式 RTO/RPO、完整
G0–G7、跨主机故障域或 72 小时验收。

## 同 incarnation 代际记录在本地采集器提交与重启（约 12:09–12:22 UTC）

仓库新增 v3 采集器格式和离线 `-commit-reseed-handoff`：要求已认证旧链
追到退休成员最终提交头，重新执行源 seed 与新基线预检，并比对停止的
采集器与冻结副本的**确切文件字节**；同一次原子清单写入保留旧链、代际
记录和新基线。回放和离线快照核验按 journal ID 保留两代，拒绝缺少
代际记录或旧事件序列摘要不符。单元测试覆盖重启续采与篡改拒绝。

在专用 kind 中先把 `ha-health` 缩至 0 并确认旧 Pod 删除，非 root 维护
Pod 独占挂载其 RWOP PVC。备份完整私有目录后发现此前预检的冻结清单
与当前 PVC 字节不同：另外两成员的 `observed_at` 因正常持续轮询前进，
但各 journal ID、提交序号与摘要未变。命令没有放宽字节比对；改用
新备份重新预检，得到采集器清单摘要
`060f7a0edb7f0373b923545df99f520d6bd2d6aa745afd36cf96b3f4737d6321`。
PVC 原清单 SHA-256 为
`26651ede6d4701cc828171c84e3448cbf93dc3f9cc4c71e4d4bd1282462ea2a2`。

在该备份的另一工作副本执行交接，候选 v3 清单 SHA-256 为
`18e981a9119559b34c6c7596f555d1bfd002a34645d1dce7dd30df25bbe76d53`。
离线快照重放为 `local_files_verified`，清单根摘要
`8faedc79c08b4254cfb780cffdcfb72795e77e48528267d3f49892881becf6a8`：
四条链中 `halro-2` 的旧链 sequence 3 和新链 sequence 0 均存在，
交接记录恰有一条。候选复制到 PVC 临时文件并核对 SHA-256 后，先保留
`durable-transitions.json.before-reseed-handoff` 再原子替换、同步。维护
Pod 删除后，Deployment 切到镜像
`halro-ha-health-view-local:reseed-handoff-v2-20260928`
（本机镜像 ID
`sha256:923b17d02ab72bf4224c92eab9aa9a706ac1302a62c3da7005727e2d912beaff`）
并恢复 1 副本，2/2 Ready。受控 mTLS API 的
`/api/durable-transitions` 为 `caught_up`：`halro-0/1` 仍是各自 sequence 4，
`halro-2` 使用新 journal
`7fdc1036750b701a0cf528795ffd847b`、sequence 0；再执行一次
Deployment 重启后仍为同样结果。健康总览保持 `unknown`，原因是近期
没有必需确认写成功；客户端入口、安全和追平卡为 healthy。

| 本机私有报告（`/tmp/halro-ha-health-kind-20260928/evidence/`） | SHA-256 |
| --- | --- |
| `live-pre-reseed-handoff-preflight.json` | `a2235cd5bc62b9b5a6baa1b6af2c02b56a87a62af690664774c7e22e3097d925` |
| `live-reseed-handoff-commit.json` | `346fc69ff2973a7db2b9046fa74f70cee43a2b7a982f4a8ae0e2299033c21957` |
| `live-reseed-handoff-candidate-verification.json` | `b7644cc530c0db066aacd6c3945e43d6f882e6d0457774d710acfe7a3f97b056` |
| `live-reseed-handoff-after-api.json` | `f1e86bcdb38e184dc051577c151472cfc02bfda912b40e04a82655dbd80308bf` |
| `live-reseed-handoff-after-restart-api.json` | `8bbb5507943d2655af4f1f3db452b9badb788d3c233acdc4c451698882ea9286` |
| `live-reseed-handoff-health.json` | `b142a282c6ff69ca134ed7fede13342c1a298eaf23b82ecc60603386e4411987` |

这些证据说明本地目标采集器可保留旧链、获批接受新代际，并从 PVC
重启恢复。随后把专用 kind 的三成员 StatefulSet 短暂停至 0，保留全部
PVC，由三个只读、非 root 辅助 Pod 复制当前成员原件；删除辅助 Pod 后
按原 PVC 恢复，三成员均重新 Ready。使用版本 3 私有成员清单逐条指定
`(node_id, incarnation, journal_id)`、匹配配置和冻结目录：当前
`halro-0/1`、退休 `halro-2` 与新 `halro-2` 共四份副本，逐条重新
解锁 Master Key、认证 v3 状态和完整成员 MAC 日志，并对比采集器的
事件序列、提交头以及交接记录中的旧/新成员清单摘要。结果为
`member_authenticated_all_collected_chains_match` /
`all_collected_member_generations`，四条链均匹配采集器清单根
`8faedc79c08b4254cfb780cffdcfb72795e77e48528267d3f49892881becf6a8`。
三成员恢复后在线持久迁移 API 仍为 `caught_up`，活动告警为 0。
最终回放校验另要求新基线的 Replica 任期与批准 seed 任期相等，且交接
提交时间不早于新成员冻结时间。包含该校验的镜像
`halro-ha-health-view-local:reseed-handoff-v3-20260928`
（本机镜像 ID
`sha256:b1b97194f60d068ec5d43646de9208f348389c9f6e796aa10a2c65fbf57d7406`）
已完成 Deployment rollout；其 mTLS API 仍为 `caught_up`，三成员
游标仍为 4/4/0。

| 后续本机私有证据 | SHA-256 |
| --- | --- |
| `allgen-verification/members.json` | `c43dde18838d63febc3f002a97d58bf3279b5ee7971839e6836cebecbf9cdd3a` |
| `evidence/live-reseed-handoff-all-generations.json` | `048e4d821dade57eb5877756e9587c90c235e5362b975e5a1dd2ea0e84f02e6b` |
| `evidence/live-reseed-handoff-after-member-restart-api.json` | `71139c8ce24cfbf21b10e63f99d3ca3c493562aece17e309a6341f6c1a892aa2` |
| `evidence/live-reseed-handoff-after-member-restart-alerts.json` | `37517e5f3dc66819f61f5a7bb8ace1921282415f10551d2defa5c3eb0985b570` |
| `evidence/live-reseed-handoff-final-image-api.json` | `f8247a666aa894e937d8c5ebc7263eb6fa7f0f897b945ff2b49a68e60f64413b` |

所有本机备份和报告仍在临时磁盘，没有独立不可变存储回执或读回。
冻结时间与隔离摘要仍是操作员提供的来源引用，不能据此宣布外部
fencing、生产 RTO/RPO、72 小时稳定性或删段门禁通过。

## 缺失来源身份的端点级回放（约 12:54 UTC）

本机 kind 集群仍有三个 Ready 的 HA 成员、独立 Prometheus、健康服务、
Alertmanager、接收器和客户端探针。新增 `TestHealthKeepsConfirmationUnknownWithoutProgress`
的端点级回放：在两成员完整、新鲜且模拟了必需确认成功的指标向量中，分别
额外注入无 `instance` 标签的 `up` 和 `halro_cluster_role` 序列。`/api/health`
每次均列出 `identity_missing`、原始样本时间，使安全卡和总览从健康转为未知；
移除异常序列后恢复健康，原有成员不被误判为清单外节点。定向测试与
`GOCACHE=/tmp/halro-go-cache go test -count=1 ./cmd/halro-ha-health` 均通过。
整包测试需本机临时 loopback 监听，因此在允许绑定端口的环境重跑。

这是仓库 E2 指标回放，尚未在 kind Prometheus 注入来源标签故障、核对
页面截图和五分钟规则恢复；§4.2「缺失目标身份标签」仍未完成目标环境逐行签收。
此处是当时的验证边界；后续 kind 注入与恢复见本记录末尾。

## kind 事件文件写入失败与恢复（约 13:03–13:08 UTC）

在专用 kind 中先通过现行 CA 与客户端 mTLS 证书读取 `/api/event-archive`：
状态 `ok`，已有 166 条记录。确认健康服务 Deployment 使用 `Recreate`
策略、事件文件位于独立 PVC 的私有目录后，保存原部署 JSON；只把
`prepare-data` 的私有目录权限从 `0700` 临时改为 `0500`，使新 Pod
可读既有文件但无法创建原子替换临时文件。故障 Pod 仍为 2/2 Ready，
而 `/api/event-archive` 返回 `journal_write_failed`，既有 166 条记录
仍可读取。故障期间 `/api/health` 总览为 `unknown`；当时同时没有近期
必需确认成功，故此轮运行观测**不能单独证明**事件文件失败将原本健康
的总览降级。对应端点级测试在必需确认成功的模拟条件下覆盖了该门禁。

立即将权限初始化命令恢复为原值，Deployment 重新 rollout 后，
`/api/event-archive` 为 `ok`，`polled_at` 前进到
`2026-09-28T13:07:38.190614543Z`，仍可读取 166 条记录。
三个 HA 成员均 1/1 Ready，健康服务 2/2 Ready，Prometheus、
Alertmanager 和接收器均 Ready；Deployment 中的命令已核对为 `chmod 700`。
端口转发使用空闲的 `127.0.0.1:19106`；19105 上存在另一服务，
未把它的 TLS 响应当作 kind 证据。

| 本机私有证据（`/tmp/halro-ha-health-kind-20260928/evidence/`） | SHA-256 |
| --- | --- |
| `event-archive-before-write-fault.json` | `bd018768de90722abe4044678a77ab8305158ee002568296c40258853f2fc233` |
| `event-archive-during-write-fault.json` | `54a2ae0c4272a73fa6a138f23078f12e81886802a4c5734096ac0d51ec8aae30` |
| `health-during-event-write-fault.json` | `2fd390940ee62d11d3abe18ee8f2179437ce183f349e61bc51704de8e6581574` |
| `event-archive-after-write-fault.json` | `d433c1f23219cb62b5772169b7fb462e464170272b263a931c267a8abc63ca72` |

这完成了本地「文件不可写」子项；成员事件环溢出、1000 条保留淘汰和
独立不可变留存仍未在目标环境演练，§4.2「事件文件连续性」整行仍未签收。

## 事件文件保留边界与 deadman 接收器核对（约 13:13 UTC）

新增 `TestEventArchiveRetentionSurvivesRestart`：同一成员以 32 条一批
提交 1001 条有序状态迁移，归档另记 1 条首次观测缺口；达到 1000 条上限后，
最早两条被淘汰，`retention_dropped=2`。关闭归档并从私有文件重新打开后，
仍保留第 2–1001 条迁移和淘汰计数。定向测试与
`GOCACHE=/tmp/halro-go-cache go test -count=1 ./cmd/halro-ha-health` 通过。
这是持久化 E2 回放，尚未在 kind 对真实成员事件流达到上限。

同时核对当前 `halro-client-probe` Pod：它只运行 `sleep`，用作网络探测
容器；先前独立 `halro-deadman` 进程已在演练后停止。本地接收器脚本
仅按 event ID 保存并 `fsync`，未实现[接收器契约](../../deploy/observability/external-probe/RECEIVER-CONTRACT.md)
中的心跳截止、过期 `firing`、新心跳 `resolved` 和重放拒绝。因此停这个 Pod
不会产生可验收的 heartbeat TTL 报警；该项仍需真正承担这些职责的独立
接收器与运行中探针，在本地和最终目标故障域分别演练。

## 本机 deadman 心跳 TTL、恢复与重放回放（约 13:22–13:25 UTC）

仓库新增[本地验收接收器夹具](../../tests/deadman-receiver/README.md)，以测试
mTLS 与 bearer 认证接收事件，用 SQLite WAL/full sync 保存事件、最大序号、
心跳截止和通知决策；其单元测试覆盖截止触发、恢复、重复/旧序号和重启读回。
从当前工作树构建真实 `halro-deadman`，使用独立测试状态文件，每 10 秒
探测夹具提供的合成 HA 根路径、Prometheus freshness 和 Alertmanager
readiness，`heartbeat_ttl=30s`。三种目标均为合成响应，**没有**在此轮
经客户端 Service 路由实际 HA 成员；实际 Service 路由演练见上节。

接收器先持久接收连续心跳，停掉发送端后，截止时间
`2026-09-28T13:22:59.481493Z` 到达；约 0.22 秒后只记录一次
`heartbeat_ttl_expired/firing`。报警保持期间，原 event ID 的重复心跳
返回 HTTP 204，旧序号配新 event ID 返回 409；最大序号、截止时间和
`firing` 不变。用同一 deadman 状态文件重启发送端后，新心跳使接收器
记录一次 `heartbeat_restored/resolved`，没有重复通知。停止两进程后，
SQLite `integrity_check=ok`；重开数据库仍有 8 个事件，序号 8，通知
恰为一条 `firing` 和一条 `resolved`。夹具自身的单元测试通过。
另用独立临时数据库补验入口：无客户端证书在 TLS 握手被拒，有证书但
bearer 错误返回 HTTP 401，双重身份正确读取测试状态返回 200；测试服务已停止。

| 私有证据（`/tmp/halro-ha-health-kind-20260928/evidence/`） | SHA-256 |
| --- | --- |
| `deadman-ttl-after-stop.json` | `a5bd8e9fb8f0c97639fe7c5c8014b7f69fd9ee385321a51eb7927a3ad2016cba` |
| `deadman-ttl-replay.json` | `13b0f0f5783de1fd15c34919f3a500286c9024577ffb04e40a61d7d029dc3fab` |
| `deadman-ttl-after-resume.json` | `82839729f81c7804dbc740f47c820133ae2cda2d5fd0bfffea01be87a80160a1` |
| `deadman-ttl-receiver.sqlite` | `2e96884c16791c7d4bde90a6db2c9da33c2a2aab3e52088adc148c368552a423` |
| `deadman-ttl-auth.json` | `bc7f667cd7e9e7947dc0c5a7b96bef2f4afedb9b765db7792d8b43da816b1c9e` |

夹具只在本机 SQLite 留下通知决策，没有实际外部 Contact Point、独立
不可变审计、分离的故障域或正式接收器部署；这项 E3 回放不能把
§4.2「独立客户端入口 deadman」整行改为通过。

## kind 客户端 Service 与心跳 TTL 联合演练（约 13:36–13:41 UTC）

重新创建先前验证过的临时 TCP 桥 Pod 和限于该 Pod 的 Halro 8080
NetworkPolicy，经新 TCP 连接进入真实客户端 ClusterIP Service，避免
`kubectl port-forward service/halro` 固定到单个后端。12 条独立 mTLS
根请求中，Primary 4 次、Replica 8 次，12 次 `cluster_id` 均为
`halro-kind-health-local`。本轮 `halro-deadman` 的 `ha_client_root`
目标指向该桥，其余 Prometheus freshness 和 Alertmanager readiness
目标由本机夹具返回合成正常值；接收端仍为同主机 SQLite 夹具。

运行中三个 deadman 目标均稳定 `up`，接收器已持久接收心跳且未报警。
停止发送端后，心跳截止于 `2026-09-28T13:38:03.737335Z`，约 0.21 秒
后收到唯一 `heartbeat_ttl_expired/firing`。用相同状态文件重启发送端，
通过真实 Service 的新成功探测送来心跳，于
`2026-09-28T13:38:59.516219Z` 收到唯一
`heartbeat_restored/resolved`。停止本轮进程后，接收器 SQLite
`integrity_check=ok`，9 个事件，最高序号 10，通知恰为一次触发和一次
恢复。临时桥 Pod、策略及端口转发均已删除；三个 HA 成员、健康服务、
Prometheus、Alertmanager 和本地告警 webhook 接收器均 Ready。

| 私有证据（`/tmp/halro-ha-health-kind-20260928/evidence/`） | SHA-256 |
| --- | --- |
| `deadman-realservice-root-samples.json` | `0cf63b9b9881347151125d79685f1072c5b6aaae53c62befa213d8ad6e9f52ab` |
| `deadman-realservice-ttl-before-stop.json` | `484046a36fb493e5f56abd7f7db14511a8c7be6fa924f23f63bbf6cd5b9487dc` |
| `deadman-realservice-ttl-after-stop.json` | `9621bf6e191389ea55d11c9edf4d3a8e4f5987db1a5922b30a4b2ca6c2e7b16f` |
| `deadman-realservice-ttl-after-resume.json` | `ad194cdc174b9a83b8574282f263c0d21aef3e103a558ac24ec2a24b5dd82d98` |
| `deadman-realservice-ttl-receiver.sqlite` | `28f931ea1bfe614cf6a4dc494b5f25e6f3625ab2e725f75c54e3be318b1e0b83` |

本轮把真实客户端 Service 路由和 TTL 生命周期放在同一条发送链上，
仍未验证独立生产接收器、外部联系点、不可变审计、跨主机故障域、错误
cluster/缺身份注入及客户端最终逻辑操作结果。`ha_client_root`
只能证明找到同集群 Primary，不能推导写请求已完整返回给调用方。

## 精确提交健康服务镜像启动（约 15:06–15:08 UTC）

仓库源码检查点为 `02f2da143907dcdf1e64c6e58dc651317d11e355`。
此前注入该版本字段却来自后续文档提交的两个本地镜像已排除；准确镜像的
`go version -m` 显示相同 `vcs.revision` 和 `vcs.modified=false`，详情见
[仓库检查点门禁](ha-health-repository-gate-2026-09-28.md)。本轮仅把
`halro-monitoring/ha-health` Deployment 的 `view` 容器更新到
`halro-ha-health-view-local:02f2da14-exact`，未更新成员 StatefulSet。
Recreate rollout 成功，实跑 imageID 为
`sha256:f4162d7a39d79a0c0d90c02f5b5c4319398a10aec145a0fa5b2f79fed3bf40fd`；
Deployment 1/1，`view` 与 `status-token-sync` 均 Ready、零次重启，
日志显示服务在 `0.0.0.0:9105` 监听。三个成员仍运行
`halro-ha-health-local:ack-stall-localized-20260928`，StatefulSet 3/3 Ready。

当时尚未定位到匹配的本地操作员客户端证书路径；尝试从 localhost 临时转发的
Prometheus HTTPS 入口只读查询时，无客户端证书的 TLS 连接不能取得查询
结果，端口转发随后已停止。**此项只证明精确镜像启动和工作负载就绪**，
未核对 `/api/health`、机器状态、事件链追赶、历史图或告警恢复，
不能将 §4.2 任一完整场景改成 `PASS`。成员镜像绑定同一源码 SHA、
正式制品 provenance、外部不可变归档及 G0–G7 仍待验收。

## 精确镜像的认证健康 API 与证据导出（约 15:14–15:18 UTC）

随后找到此前本地演练生成、仍在有效期内的 kind 操作员测试证书和 CA，
仅使用文件路径发起双向 TLS 请求，未输出私钥或 Kubernetes Secret 值。
第一次 `127.0.0.1:19105` 查询误命中了已有主机服务，响应的
`environment=host-local`、`cluster=halro-health-local` 与 Deployment 参数不符；
该文件单独保留为误路由诊断，**不计入 kind 验收**。改用独占的 IPv4
`127.0.0.1:29105` 端口转发后，握手中的公开服务端证书为
`CN=halro-health-view-kind-local`，SAN 含 `127.0.0.1`，由本地 kind CA
签发。以匹配的操作员证书和受信 CA 查询以下三个只读 API 均返回 HTTP 200；
仅提供 CA、不提供客户端证书的 `/api/health` 请求在 TLS 阶段失败。

| 私有证据（`/tmp/halro-ha-health-kind-20260928/evidence/`，文件权限 0600） | SHA-256 |
| --- | --- |
| `health-exact-02f2-kind.json` | `9545913b35267bec72efa5e9e4a2d53f273f34275dd684221e006e54cc3d2b4d` |
| `archive-exact-02f2-kind.json` | `a09fc2ba8663fb308bcd2163854a3f3ef0267c3c505ed9d46439a8d90ea4a7a3` |
| `durable-exact-02f2-kind.json` | `41eca8d52088fab12f141906a772354cc5fdbb8ac20ec6656cce9d9cf05b99ce` |
| `evidence-exact-02f2-kind-15m.json` | `22283cfe364c55f9b4d57c0d5e0b73c4ada5064141ec9d91dcc93e5abf0c37a5` |

`/api/health` 的 `environment=kind-local`、`cluster=halro-kind-health-local`、
`expected_members=3` 与部署参数一致。三名成员均通过机器状态采集，
同属 `inc_kind_local_20260928`、term 3，角色为一个 Primary、两个 Replica；
各自 `durable=confirmed=applied=153`，live/ready 为真。客户端 Service
入口、安全、追平卡为健康；近五分钟无真实必需确认成功样本，确认能力与
总状态正确保持 `unknown`。这些相等的 index 不能证明成员前缀相同或可提升。

`/api/event-archive` 为 `ok`，本地文件当前留存 166 条已采记录；
`/api/durable-transitions` 为 `caught_up`，三名成员当前代际的
`stored_sequence/observed_head` 分别是 4/4、4/4、0/0。
15 分钟 `/api/evidence` 导出包含三名机器状态、166 条已采事件、
三名持久迁移摘要与 51 个指标快照序列；`client_final=not_configured`，
未把服务端计数冒充客户端最终结果。简单敏感词扫描未发现私钥、Bearer
或密码片段；该扫描不等同于完整隐私审计。端口转发已停止。

这补强了精确健康服务镜像重启后的读取与本地追赶证据；三名成员仍运行
先前本地镜像，未完成同一 SHA 的整集群验证。旧代际与当前链的独立
不可变归档、跨故障域恢复、完整矩阵和 G0–G7 继续为 `NOT_RUN`。

## 真实集群短时采样与纳秒时间修复（约 15:21–15:26 UTC）

用同一 kind 操作员证书执行仓库的只读长跑采样器时，首次 25 秒预演的
五次 HTTPS 请求均到达 `/api/health`，但被记为
`observation_time_invalid`。根因是服务端 Go `RFC3339Nano` 输出 9 位小数，
本机 Python 3.9 的 `datetime.fromisoformat` 只接受至多 6 位；既有合成
测试只用了 Python 自身的 6 位时间。采样器现先严格检查 RFC3339 的
1–9 位小数和时区，再仅为 Python 解析截到微秒，保留原始纳秒时间串
写入证据。新增回归测试验证真实 Go 格式可接收、超过 9 位或缺时区
会拒绝；四项定向单测与 Python 3.9 语法检查通过。

修复后的第二轮 25 秒、5 秒间隔预演取得 5 次有效观测、0 个漏采时隙，
最大单调采样间隔 5.004 秒；`sampling_continuous=true`、
`health_endpoint_coverage_complete=true`。五次总状态均为 `unknown`，
与健康 API 无近期必需确认写的事实一致。记录中包含部署健康镜像 ID、
源码提交、成员清单，以及当时健康 ConfigMap 和 Prometheus 配置的
SHA-256；私有目录为 0700、文件为 0600。正式 72 小时采样仍未运行，
两轮结果均保持 `kind=smoke_only`、`ha_acceptance=NOT_RUN`。

| 私有证据（`/tmp/halro-ha-health-kind-20260928/evidence/soak-preview-exact-02f2-v2/`） | SHA-256 |
| --- | --- |
| `samples.jsonl` | `33eb22455de26f16ad84ed97ad8afdd774cd67d246ed4cd58fd1047b33dfd7f8` |
| `summary.json` | `c225df32fce8fddb8ee16c8ca0e487f49b69490dbb5bb96cbabb091bf221639b` |

## 三成员精确提交镜像滚动与故障期观测（约 15:33–15:37 UTC）

先由认证 `/api/health` 取得基线：term 3、同一 incarnation、一个 Primary
和两个 Replica，各成员 `durable=confirmed=applied=153`；总览因没有近期
必需确认成功保持 `unknown`。本地 StatefulSet 为 `OnDelete`，PVC 为
三个独立的 50 Gi `Bound` 卷；将模板设为
`halro-ha-health-local:02f2da14-exact` 后，按 `halro-2`、`halro-0`、
`halro-1` 顺序逐个删除并等待新 Pod Ready。两个 Replica 各自恢复后，
认证健康 API 均返回 200，三成员身份、角色、term 和 153/153/153
水位一致。三个新 Pod 的运行中 `/usr/local/bin/halro version` 都返回
完整提交 `02f2da143907dcdf1e64c6e58dc651317d11e355`；Pod 均为
Ready、零次重启。kind 导入后的容器运行时 imageID 是本地转换摘要，
本记录以新 Pod 指定的精确 tag、二进制实际版本及前述本地镜像来源共同
核对身份，不把转换摘要直接等同于 Docker 原始镜像 ID。

Primary 重启前，独立健康服务每两秒只读采集 70 秒，共 35 次有效观测、
零漏采时隙、最大单调间隔 2.005 秒。入口卡在 15:34:11 UTC 从健康
转未知，安全卡在 15:34:13 UTC 转未知；15:34:15 UTC 总览和追平卡
短暂降级。15:34:17 UTC 安全和追平恢复，15:34:19 UTC 客户端入口恢复
健康。35 次总览有 34 次 `unknown`、一次 `degraded`，故障期未错误变绿。
这证明独立入口持续应答并显示故障期变化；它不证明真实客户端写请求
恢复时间或具备人工切换所需的旧 Primary fencing。

Primary 新 Pod Ready 后，健康 API 返回一个 Primary、两个 Replica，
三者仍同属 term 3 和 `inc_kind_local_20260928`，各自水位为
156/156/156，live/ready 为真；客户端入口、安全和追平卡健康，
确认能力与总览仍因缺近期必需确认成功为 `unknown`。迁移接口返回
`caught_up`，三名成员的 `stored_sequence/observed_head` 仍为
4/4、4/4、0/0。此滚动重启没有产生可作为客户端最终结果的证据。

| 私有证据（`/tmp/halro-ha-health-kind-20260928/evidence/`） | SHA-256 |
| --- | --- |
| `exact-members-before-health.json` | `e702d3d03968ec44d92131b6b7b34e9ade9de0b2576364e78f79ed4248ef9f79` |
| `exact-members-after-replica2-health.json` | `abf246f7a220b2d0f2914ba7c556cbbaecf82ceeeeb2610a7ef81e519fcbdff0` |
| `exact-members-after-replica0-health.json` | `1e0af77d565162c4e2f00fbd5469ea1f5af4bffe6110e4812cf4866ce361083c` |
| `exact-members-after-primary-health.json` | `6c22659e9ea582f4d78d82868df8caf9920391e2118f682b45881875946da265` |
| `exact-members-after-all-durable.json` | `7f3769edef7e69d2f6ab3575bc6cd88e8ad62a06b8a50bc4e618b44240aa2768` |
| `primary-image-rollout-samples/samples.jsonl` | `d0058e27b8bf3ac7247bf7bdf4db6b86dab126d1cff501f2fe60cdc1d1ae6c5d` |
| `primary-image-rollout-samples/summary.json` | `077df31a561fe2f058e732afdd56e39c7f3698072985b058207b6b827028abb9` |

采样器仍标 `kind=smoke_only`、`ha_acceptance=NOT_RUN`。三成员相同
源码提交的本地运行只补强镜像绑定和受控重启证据；正式 CI/制品来源、
故障域、独立不可变归档、G0–G7、完整 20 行矩阵、相邻版本和 72 小时
长跑均未由此签收。

## 真实数据页面视觉复核与中文显示（约 15:50–15:58 UTC）

在运行中的 kind 监控入口，以本地操作员 mTLS 证书经仅绑定
`127.0.0.1` 的临时只读代理打开页面。页面实际读取当前三成员的健康数据，
显示 `kind-local`、`halro-kind-health-local`、采集覆盖 3/3、一个 Primary
和两个 Replica；没有近期需确认写样本时，总览与确认能力保持未知，
“客户端最终逻辑操作结果”仍为未接入。切换历史面板后，真实时间序列折线、
告警与事件区域均能加载；首屏没有可见文字溢出。

视觉检查发现卡片原因仍显示原始英文码、时间使用浏览器默认地区格式。
源码提交 `4cb69cf469f8d255f82fff31e0a0b759e88ea971` 将 45 个固定原因
及动态原因前缀映射为中文，并显式以 `zh-CN` 格式显示时间；未知的新原因
仍回退显示原码，API 与原始证据字段未改。脚本语法、45 个固定原因覆盖、
`cmd/halro-ha-health` 整包 `-count=1` 和 `go vet` 均通过；包测试首次在
受限沙箱因本机 loopback 绑定权限失败，在允许 loopback 的环境原命令重跑通过。

从该干净提交构建的本地 Linux arm64 二进制 SHA-256 为
`7f42d5eb74ba0495cfa7b7da180cc0567cb1850d29c608c6ae2b365920280a59`，
`go version -m` 报告该提交且 `vcs.modified=false`。离线本地镜像
`halro-ha-health-view-local:4cb69cf4-exact` 的 Docker image ID 为
`sha256:c83209e53ed1dffd5997073eaf000998c1f648a2581753789c913dc61592edfa`；
部署更新后 `ha-health` 为 1/1 Ready，运行中 `view` 容器使用该 tag，
容器内 `-version` 返回完整提交。通过运行中服务获取的 `/app.js` 与提交源码
SHA-256 同为 `db36a5d31f1cfcb4d4d8e608c190cb4ebc043de44f737e6ee15e72f824f0b83e`；
移除代理的临时脚本覆盖后重新加载浏览器，确认实际镜像已显示中文原因和
中文日期格式。三名 HA 成员仍为 `02f2da14` 精确提交镜像，本次只替换
独立健康页。此检查补齐本地真实数据视觉证据，不代替正式操作员代理、
完整故障矩阵、独立归档、客户端最终结果或 G0–G7 验收。

## kind Peer 固定指标缺报与恢复（约 16:11–16:17 UTC）

在现行三成员 kind 的 `halro-2` Prometheus 抓取任务中，临时用
`metric_relabel_configs` 只过滤 `halro_replication_peer_connected{peer="halro-1"}`；
其他成员、该成员 `up` 和其余指标保持正常。故障配置先用部署版本
Prometheus v3.13.0 的 `promtool check config --syntax-only` 验证，再等待
ConfigMap 投射并对 Prometheus 发送 HUP；日志确认热重载完成。故障前
安全卡健康，活动告警为空；过滤生效后原始
`up{instance="halro-2"}=1`，该成员当前只剩 `peer="halro-0"` 一条 Peer
序列，`peer="halro-1"` 最后一条原始样本停在 16:11:13.245 UTC，
健康 API 的同成员 `up_sampled_at` 已前进至 16:11:48.246 UTC。
安全卡与追平卡均转未知，没有用旧 Peer 值补全当轮数据。

`HalroMemberHASignalMissing{instance="halro-2"}` 达到一分钟 `for` 后
firing；故障期间 Prometheus API 所列 21 条 HA 告警规则的健康状态均为
`ok`。本地独立 webhook 于 16:12:37.944 UTC 收到 firing，告警
`startsAt=16:12:32.917 UTC`。将 ConfigMap 精确恢复、核对 Pod 中配置与
冻结基线逐字节一致并再次热重载后，两条 Peer 当前序列均重新出现；
16:17:36 UTC 健康 API 的安全和追平卡均恢复健康，活动告警为空。
webhook 于 16:17:07.979 UTC 收到 resolved，`endsAt=16:17:02.917 UTC`。
总览与确认能力在整个演练期间仍因缺近期必需确认写而为未知。
演练结束时 Prometheus 与健康服务 Deployment 均为 1/1 Ready，HA
StatefulSet 为 3/3 Ready。

本机私有证据报告
`/tmp/halro-ha-health-kind-20260928/evidence/peer-omission-local-report.json`
逐项列出基线、故障、恢复配置及健康/告警/原始 Prometheus/接收器文件的
SHA-256；报告自身 SHA-256 为
`13cec591a4d64dd81b22df09977f4f998a14a4a23901af7d421c37a5f3203666`。
这是“成员固定指标首次缺报”中**一个 Peer 指标**的本地实际触发与恢复；
其余固定指标仍须逐项注入，本地 webhook 不是正式联系点，临时磁盘不是
独立不可变归档，此矩阵行仍未完整签署。

## kind v3 日志容量三指标同轮门禁（约 16:33–16:42 UTC）

规则复核发现原 `HalroTransitionJournalCapacityUnreadable` 只要求同轮
`capacity_readable=1`，没有要求同轮的 `segments` 和 `bytes`，因此指标被
选择性过滤时可误认为容量证据齐全。现要求 v3 成员成功抓取时可读性、
段数和字节数三项容量序列各恰好一份且时间与本轮 `up` 相同；v2 仍不要求容量序列。Prometheus
规则夹具新增 `readable=1` 但段数或字节数缺报的 pending、firing、恢复
场景，并覆盖段数与可读性重复来源；锁定部署版本的规则/配置检查、全部规则夹具及
`go test -count=1 ./deploy/observability` 通过。

本地 kind 加载新规则后，`halro-2` 的 v3 状态版本、`up=1`、
`capacity_readable=1`、段数 1、字节数 516 在 16:33:45.809 UTC 同轮。
随后只从该成员的 Prometheus 抓取中过滤
`halro_replication_transition_journal_bytes`；16:37:18.171 UTC 其他四条
原始序列仍同轮刷新，字节数最后原始样本停在 16:36:53.242 UTC。
新告警先 pending 后 firing，告警 `startsAt=16:38:02.917 UTC`；
独立健康服务显示该 firing 并将总览降级，本地 webhook 于
16:38:07.934 UTC 收到 firing。故障期间 21 条 HA 告警规则健康状态均为
`ok`。

恢复精确原始抓取配置后，16:42:35.569 UTC 的五条原始序列再次同轮，
健康服务活动告警为空，总览回到缺近期需确认写证据的 `unknown`；
webhook 于 16:42:17.982 UTC 收到 resolved，告警
`endsAt=16:42:17.917 UTC`。Pod 中抓取配置与冻结基线逐字节一致，
运行规则与仓库新规则逐字节一致。本机私有报告
`/tmp/halro-ha-health-kind-20260928/evidence/capacity-bytes-local-report.json`
包含原始样本、规则、健康状态、告警及接收器文件的逐项 SHA-256；报告
SHA-256 为 `e41979f5255937177fcd8ca6877819acb25f8c52093a6432030c00677b772751`。
结束时两个监控 Deployment 均为 1/1 Ready，三成员 StatefulSet 为
3/3 Ready。
本地实际注入的是**字节数缺报**；段数缺报和两种重复来源由锁定规则夹具覆盖。此演练
不测真实磁盘容量核算失败、最大留存启动资源或外部不可变归档，
“长留存恢复资源”整行仍未正式签署。

上述实际字节缺报注入后，最终复核又补上 `capacity_readable` 自身的单份
同轮门禁；新增重复来源夹具通过。最终规则文件 SHA-256 为
`fb8d2c8d53724c1dbb989d62170a43c3e188601289df1db3f079cd67dff5aa8a`，
与 Prometheus Pod 投射文件逐字节相同；16:55:24.956 UTC 热重载成功。
最终健康 API 仍有三名成员、活动告警为零，安全与追平健康，总览与确认能力
因缺近期需确认写保持未知。最终加载报告
`/tmp/halro-ha-health-kind-20260928/evidence/capacity-final-rule-rollout.json`
的 SHA-256 为
`35c2739924584ef21113f41e2bc8ea85bf8b8d7a6789389bdfa2f2add3ab763a`。
字节缺报没有在补充可读性唯一性之后重复注入；前述 live 证据对应其未变的
字节覆盖分支，最终三项唯一性由规则夹具与加载状态验证。

## kind 缺失 `instance` 身份来源与窗口恢复（约 17:01–17:13 UTC）

本地三成员与两个监控 Deployment 均 Ready；注入前 Prometheus 有三条
`halro_cluster_role`，分别属于 `halro-0`、`halro-1`、`halro-2`，
健康服务安全卡健康且活动告警为空。只在 kind Prometheus 临时加入一条
录制规则，复制 `halro-2` 的角色指标并移除其 `instance` 标签；三条成员
原始序列及三个 `up` 目标均保持不变。临时规则经 `promtool check rules`
验证后热重载。Prometheus 随后返回四条角色序列，新增一条确实不含
`instance`，并非已知的第四名成员。

独立 `/api/health` 把异常来源列为
`identity_missing=true`、`observed=false`，保留原始样本时间
`2026-09-28T17:08:17.672Z`；安全卡由健康转未知，页面首屏显示
“采集 3/3 · 异常来源 1（未证实 1）”及“缺少 instance 标签”。本机
浏览器已视觉核对当前页，截图未保存到私有证据目录。`/api/evidence`
的 `metric_snapshot` 同时保留三条正常角色序列和一条缺标签的原始序列；
故障期间 79 条 Prometheus 规则健康状态均为 `ok`。总览在故障前后均因
缺近期必需确认写而为未知，因此本轮只证明安全卡的因果降级，不能单凭
总览未知归因于这条异常来源。

撤除临时规则并核对 Pod 中录制规则与冻结基线逐字节一致后，最后一条异常
原始样本仍在 45 秒当前窗口内时安全卡保持未知；退出该窗口后，
`unexpected_members` 消失、安全卡恢复健康，Prometheus 只余三条正常
角色序列。最终 78 条原有规则均为 `ok`、活动告警为空，浏览器刷新后
不再显示异常来源。本机私有报告
`/tmp/halro-ha-health-kind-20260928/evidence/missing-instance-local-report.json`
列出前中后健康响应、原始序列、规则、证据导出和配置的逐项 SHA-256；
报告 SHA-256 为 `04e776c1e159da2d9f3eb2a9e17c0d32905b186e6036dcdc9aa70f89100bc777`。
这是本地真实 Prometheus 的**角色指标缺标签**子场景；没有持久截图、
同一候选 SHA 的整栈冻结或独立不可变归档，§4.2 该行仍未正式签署。

## 冻结采集快照的本机读回预演（约 17:25 UTC）

以此前停服冻结且核验通过的三成员采集快照为源，将其私有清单复制到另一
`0700` 本机目录，读回文件权限为 `0600`，再以新增的
`-verify-archive-readback` 和源目录的 `-verify-durable-snapshot` 一次执行
双侧只读核验。命令返回 `readback_bytes_match_local_only`；两个报告各有
三条成员链、一份清单、零个关闭分段，文件清单根均为
`1bb9727342855f08e9fbf5b575113fd65f4438de73470c52e86be33e6efd9722`。
私有报告位于
`/tmp/halro-ha-archive-readback.w4lXgP/readback-report.json`，SHA-256
为 `31ddb7539677948e2f6364bf5e178cd975f1d791a6193ba28ca7cd7207d616ef`。
定向测试另覆盖关闭段读回、有效 JSON 的字节变化、漏段及硬链接冒充独立
副本；`cmd/halro-ha-health` 全包测试在允许回环监听的环境通过。

这次读回仍在同一台本机，且源快照没有关闭分段；没有独立不可变存储的写入
回执、保留版本、外部读回或成员 MAC 链的**归档副本**认证。因此只能作为
归档交接工具的本地预检，冻结采集快照与外部留存矩阵行继续 `NOT_RUN`，
也不允许删除任何旧段。

## 冻结成员迁移快照的本机读回预演（约 17:35 UTC）

从此前三成员故障演练留存的私有冻结数据目录，按四条日志代际分别复制 v3
`cluster/state.json` 与完整 `cluster/transitions.journal` 到其他 `0700`
本机目录，文件均为 `0600`。以各成员匹配的配置和 Master Key 执行新增
`halro cluster verify-transition-snapshot --archive-readback-dir`，源与副本
各自完成全链 MAC 认证，均返回
`member_readback_mac_and_bytes_match_local_only`，逐文件哈希与根摘要一致：

| 冻结成员链 | 已提交序号 | 文件清单根 SHA-256 | 私有报告 SHA-256 |
| --- | ---: | --- | --- |
| `halro-0` 当前链 | 4 | `07d6e03ec5e4f11ab555b856a94c4a3c2df90b6d7a46329b5eddcba3640bd21a` | `fb59175bce3c7a14bad0cbde62eeaa75f5a0a9a5cf52cb89e60d60ab3d716e21` |
| `halro-1` 当前链 | 4 | `c6753b72d2cad2aa1898821a138317e8d2e824b554711d2c108133ee868fd0b2` | `b97d9eb26dbf0244378852f17fc6b2a36634493d0ce1e4262a36e5f6514f49a7` |
| `halro-2` 新链 | 0 | `9c8da8a387107aa1405de27dafbab34ad8b5f1fec6baa4e2e69022dde8f6afd1` | `efc6b0c864fcdc75501c78fc624ae700819820a297fb811e9b80be99bd83f8c6` |
| `halro-2` 退役旧链 | 3 | `4a499c08b5d26ab704f5e5334863a139cd95235c27bd28fbdffe1f6e0cacb600` | `63cfc702baa1cfdbe9c11c746e06024aa1b0028cef4f9911751961dbb9daeb1c` |

四份私有报告位于 `/tmp/halro-member-archive-readback.5cy4Bf/`，按上表
依次为 `readback-report.json`、`node1-report.json`、`node2-report.json`、
`retired2-report.json`；每份均列出源与副本各两份已认证文件、成员身份、
journal ID、提交头与事件摘要。
定向测试覆盖副本缺日志、字节变化、原目录和硬链接冒充，并保持源目录字节
未变；受影响 `internal/app` 定向测试、`cmd/halro` CLI 测试、`go vet`
和本地 CLI 构建通过。

这仍是同主机四条已留存链的读回预演。尚无独立不可变存储回执、保留版本、
外部故障域读回及与采集端归档副本的一体化交接证明；完整 §4.2 成员
归档行继续 `NOT_RUN`，不能授权删段。

## 只读 24 小时连续观测启动（约 17:45 UTC，进行中）

本地 kind 当前三成员 `halro` StatefulSet 为 3/3 Ready，`ha-health`、
Prometheus、Alertmanager 和本地接收器 Deployment 均为 1/1 Ready。
所用操作员证书与服务端证书到 2026-09-30 03:36 UTC 到期，信任 CA 到
03:27 UTC 到期；从本轮启动时起不足 72 小时，因此不能用这套凭据启动
正式 72 小时候选观测。三份证书分别运行 `openssl x509 -checkend 266400`
均返回过期预警。健康服务和成员也仍是不同源码版本的本地镜像。

经 `127.0.0.1:19115` 的本地 Service 端口转发及操作员 mTLS 运行 30 秒
预检：6 次 `/api/health` 均有效，总览均为缺近期确认写证据的 `unknown`，
`sampling_continuous=true`、`health_endpoint_coverage_complete=true`、
漏采时隙 0，最大单调间隔 5.010 秒。私有预检目录
`/tmp/halro-ha-health-kind-20260928/evidence/soak-24h-preflight-1745/`
中 `summary.json` SHA-256 为
`4f09dd462f673c0fd1bc762374d349aef4c6a6b4e076f823fe0aa20b7d2308bb`，
`samples.jsonl` 为
`1ad180f17ab4eb5251854f6b8a9678770db72bd9c806beb71aec5d6405b9e1bf`。

随后启动 86400 秒、15 秒间隔的非计费只读采样，证据目录为
`/tmp/halro-ha-health-kind-20260928/evidence/soak-24h-readonly-20260928/`。
启动后首条样本在 `2026-09-28T17:45:49.334836Z` 成功，采样器和
端口转发进程在记录时均仍运行。完成前没有 `summary.json`，不能宣称
24 小时连续覆盖；即使最后完成，本轮也只属于 `smoke_only`，不会签署
72 小时、RTO/RPO 或完整 G0–G7。

## 72 小时证书窗口只读预检（约 17:58 UTC）

按本地 kind 五个实际证书 Secret 的公开 `.crt` 键建立精确清单，并把主机
操作员公开证书作为独立文件纳入，以
`tests/ha-health-soak/cert_preflight.py` 检查至少 266400 秒（72 小时加
2 小时余量）的有效期。`kubectl` 进程读取 Secret 后，模板只把键名和公开
证书传给预检进程，不把私钥写入输出或报告；清单漏/多证书键、无效 PEM、
尚未生效及证书链中任一证书提前到期均阻止通过。10 项仓库单测通过，
其中包含链内第二张证书到期、主机文件到期和不读取私钥字段的检查。

本地结果为 `certificate_window_blocked`：5 个 Secret 的 13 张证书加
1 张主机操作员证书共 14 张，全部早于所需截止时间
`2026-10-01T20:06:34.132461Z` 到期；最早的是
`2026-09-30T03:27:23Z`。清单 SHA-256 为
`b2e6439e387d511626b637299aab4698e1f62eb75ca276018be8ac2a5f4bfce5`。
私有报告
`/tmp/halro-ha-health-kind-20260928/evidence/cert-window-final-v4-20260928/certificate-window.json`
的 SHA-256 为
`18589636e9daa15a7af4b3e064a13f6f85a2d049e0448e76caa64f19ba636b15`，
目录权限 `0700`，报告 `0600`。此报告只检查所列证书的有效期；正式候选
仍须核对部署引用、轮换及跨故障域可用性，然后重新从零启动 72 小时窗口。

## 本地证书轮换材料预备（约 18:15 UTC）

只读取当前五个 Secret 的公开证书字段，确认成员/健康入口与 Prometheus 查询
分别使用两套将在候选窗口内到期的 CA；核对三个成员、健康服务、采集器、
Prometheus 抓取、查询服务端和查询客户端的 SAN、用途与当前查询端
`client_allowed_sans`。在私有目录
`/tmp/halro-ha-health-kind-20260928/cert-rotation-stage-20260928/`
用 `stage_kind_certificates.py` 离线生成两套 14 天本地测试 CA 与九张叶子
证书，并逐张验证证书链、用途、SAN、私钥匹配及 74 小时有效期。脚本未
访问或更改 Kubernetes。目录和两个 CA 私钥权限分别为 `0700`、`0600`；
只含公开指纹与本地 Secret 文件路径映射的 `stage-report.json` SHA-256 为
`ff85e3d8805f1014e6f24e5fa926ec5e4ca10ce18407b625d08257fd70e5cce9`。
五个待替换 Secret 的文件键集合分别与当前 kind 实物精确一致（7/5/3/3/3），
仅比较键名，未读取或输出集群中的私钥值。
证书**仅已准备，尚未安装**；当前 24 小时采样仍使用原信任链。完整采样
结束后才可在维护窗口轮换、验证旧证书拒绝，并重跑 Secret 有效期预检。
