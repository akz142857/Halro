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
| 正常停机与播种 | Primary 与至少一个 Replica 追平后正常停止 Primary；停止后的 durable/confirmed/applied 必须相等，`system.shutdown` 审计必须落在已确认前缀。用该快照执行 `seed-approve`/`seed-install`；未确认尾帧必须被拒绝，不能手改 state 或排序日志。 |
| 非计划提升 | 先在 Halro 外部**证明旧 Primary 已被隔离**，再按手册用精确 term/index 提升已追平 Replica；旧 Primary 回归不得重新确认写入。无外部 fencing 证据就停止此场景。 |
| 非对称分区 | 分别隔断不同方向的复制连接；任何时刻至多一个成员能确认权威写。无 quorum 的请求必须拒绝或留下可归因的保守状态，不能静默成功。 |
| 单 PVC 丢失 | 保留其它成员，从已认证 seed 重建该 Replica；旧本地状态不能被当作当前前缀。 |
| 全集群恢复 | 从已验证的 Replica 备份恢复为**新 incarnation**，重新播种成员；旧 incarnation 的节点必须被拒绝。核对 Ledger、Audit、Gateway Key 吊销和幂等记录。 |
| 相邻版本 | 用两个相邻的真实发布二进制按 §15 的同 schema 顺序逐个升级/回退；schema-changing 版本只能按离线新 incarnation 恢复流程验。 |
| Linux 故障与容量 | 在 Linux 参考机注入 ENOSPC、只读和慢盘，比较 Standalone 与三节点吞吐/延迟；错误必须可见且失败关闭。 |

最后在目标负载下做 72 小时 HA soak，并从实际故障开始测量人工响应、追平、提升与
客户端恢复的完整 RTO。所有测试都应有可访问的证据 ID、目标 SHA/digest 和明确 PASS/FAIL；
一次失败先修根因，再重跑受影响场景。自动故障切换不在当前交付范围。
