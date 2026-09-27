# Halro HA 使用手册

本手册面向部署和运维人员，说明三成员 Primary/Replica 集群的建立、查看、人工切换、恢复与验证。涉及精确的离线文件操作时，以 [HA 运维 runbook](../runbooks/ha-operations.md) 为准；故障演练的通过条件见 [HA 测试指南](../verification/ha-test-guide.zh-CN.md)。[English version](ha-usage.en.md)。

> **当前状态**：HA 的命令、复制运行时和只读控制台页面已有仓库实现，但目标环境的生产进入条件、G0–G7、Linux/kind 故障注入、72 小时浸泡和 RTO/RPO 验收尚未完成。不要把本机三进程演练当成生产放行。README 中的 `v0.8.5` Docker 镜像不包含 `halro cluster` 命令；必须先确认所用二进制来自包含 HA 实现的源码或之后经过验收的发布制品。自动故障切换不在当前设计内。

## 1. 先理解运行方式

- 推荐三个成员，每个成员一个进程、独立的数据目录或 PVC，且同一时刻最多一个 Primary。两个成员可以复制和备份，但失去一个成员后不能继续确认需要副本的写，因此不能提供切换后的完整可写可用性。
- 三个成员使用相同的 `replication.cluster_id`、同一次 `incarnation` 和相同的 Master Key 材料。每个成员有唯一 `node_id`、自己的 mTLS 证书、自己的监听地址，以及对端证书的 SPKI pin。复制端口只允许成员访问。
- Primary 承担 Gateway 与正常 Admin Console 流量。Replica 不作为写入入口；客户端如果打到 Replica，会得到 `503 not_primary`。客户端路由和重试要按部署拓扑设计。
- 有些账务和吊销写必须等待副本确认；副本数不足时这些操作失败关闭。`durable_index`、`confirmed_index`、`applied_index` 是不同阶段，不能把“节点可连接”当作“已追平”。
- 进程重启、成员故障或网络分区不会触发自动选主。计划交接用 `stepdown`；非计划提升必须先从 Halro 外部隔离旧 Primary，再由人工执行 `promote`。

## 2. 准备二进制、配置和数据

在仓库根目录为**隔离的测试环境**构建当前源码，并确认 CLI 确实包含 HA 命令：

```sh
go build -trimpath -o ./bin/halro ./cmd/halro
./bin/halro version
./bin/halro help cluster
```

如果看到 `unknown command "cluster"`，先核对正在执行的二进制路径与构建来源；当前目录里的旧 `bin/halro` 不会因为 Git 更新而自动替换。生产使用制品还须完成独立的发布与验收门禁。
下文命令以仓库根目录中的 `./bin/halro` 为例；在各主机上应替换为实际安装的二进制路径。

先按 [普通部署与初始化流程](operator-guide.md)准备一个已有管理员、可正常启动的 Standalone 数据目录，随后停止进程。以完整的 [`configs/config.example.yaml`](../../configs/config.example.yaml) 为基础，为每个成员准备完整配置；不能只保存下面的片段。每个成员只在自己的配置中加入 `replication` 块。`halro-0` 的示意片段如下，其余成员调换 `node_id` 和 `peers`：

```yaml
replication:
  cluster_id: production-a
  node_id: halro-0
  listen: 0.0.0.0:9910
  peers:
    - name: halro-1
      address: halro-1.halro-internal:9910
      spki_sha256: "sha256:<halro-1 证书 SPKI 的 64 位小写十六进制摘要>"
    - name: halro-2
      address: halro-2.halro-internal:9910
      spki_sha256: "sha256:<halro-2 证书 SPKI 的 64 位小写十六进制摘要>"
  tls:
    ca_file: /run/secrets/halro-cluster/ca.crt
    cert_file: /run/secrets/halro-cluster/tls.crt
    key_file: /run/secrets/halro-cluster/tls.key
```

示例中的 SPKI 值与地址必须替换。还要为三个成员分别设置不冲突的 Gateway、Admin、Metrics、复制监听地址和独立的 `storage.data_dir`；同机演练尤其如此。不要让两个进程共享数据目录或写同一个 PVC。检查各配置：

```sh
./bin/halro config check --config /etc/halro/halro-0.yaml
./bin/halro config check --config /etc/halro/halro-1.yaml
./bin/halro config check --config /etc/halro/halro-2.yaml
```

Kubernetes 的 [`halro-ha-statefulset.yaml`](../../deploy/kubernetes/halro-ha-statefulset.yaml) 只是拓扑样例：镜像 digest、Secret、网络策略、存储与客户端入口都需按目标环境替换和评审。不要直接把 Standalone Deployment 的 `replicas` 改成 3。

## 3. 建立 Primary，再播种两个 Replica

**所有离线命令均要求 `--config` 指向的本地成员进程已经停止，数据目录锁已释放。** `cluster status` 是只读检查，可用于分别查看成员。不要删除 `.halro.lock` 来绕过仍在运行的进程。

1. 停止已有的 `halro-0`，确认普通数据目录、管理员和 Master Key 已准备好。在本节点建立初始 Primary，并记录所选的 `incarnation`：

   ```sh
   ./bin/halro cluster establish --config /etc/halro/halro-0.yaml \
     --role primary --incarnation inc_example_001 --term 1
   ```

2. 在播种每个 Replica 前，先让 Primary 的已确认前缀追平并**停止 Primary**。在 Primary 上以管理员身份批准目标成员；启用 MFA 时再提供只含当前验证码的 `--totp-file`：

   ```sh
   ./bin/halro cluster seed-approve --config /etc/halro/halro-0.yaml \
     --target halro-1 --output /secure/halro-1.seed.json \
     --username admin --password-file /secure/admin-password
   ```

3. 把停止状态的 Primary 权威快照经加密、认证的通道复制到目标 `storage.data_dir` 的**私有同级暂存目录**。目标正式数据目录必须不存在。复制必须包含 `cluster/ordering.journal` 和 `cluster/provider-object-sources/`，排除来源节点的 `cluster/state.json`、`cluster/maintenance`、`cluster/totp-watermark`、`.halro.lock`。清单负责完整性和批准校验，不负责保密。以下是两台主机上的目录结构示例；在目标主机先创建私有暂存目录，且不能使用已有的旧快照：

   ```sh
   # 在 halro-1 上：暂存目录与目标 /var/lib/halro/data 同级。
   mkdir -m 0700 /var/lib/halro/.seed-staging
   # 在 halro-0 上：Primary 已停止，SSH 身份和目标目录权限已配置。
   rsync -a \
     --exclude='/.halro.lock' \
     --exclude='/cluster/state.json' \
     --exclude='/cluster/maintenance' \
     --exclude='/cluster/totp-watermark' \
     /var/lib/halro/data/ halro-1:/var/lib/halro/.seed-staging/
   ```

   把批准清单也通过受控通道送到目标主机的 `/secure/halro-1.seed.json`。现场路径、传输身份与权限需替换；安装器会校验文件和排序链。完整失败处理见 [播种 runbook](../runbooks/ha-operations.md#seed-a-replica)。

4. 为目标安装同一 Master Key 与它自己的证书后，在**停止的目标**上安装 seed：

   ```sh
   ./bin/halro cluster seed-install --config /etc/halro/halro-1.yaml \
     --staging /var/lib/halro/.seed-staging \
     --manifest /secure/halro-1.seed.json
   ```

5. 对 `halro-2` 重复批准、复制和安装，并使用它自己的配置与清单。完成后启动三个成员，确认一个 Primary、两个 Replica；每成员的 `cluster_id` 与 `incarnation` 一致。`cluster establish --role replica` 会被拒绝，Replica 只能走批准的 seed 路径。

示例的 `incarnation`、路径、主机名均需换成现场值；管理员密码文件应留在受控私有路径，不要把密码放在命令参数、日志或文档里。

## 4. 查看状态和判断能否切换

登录当前 Primary 的 Admin Console，打开 **「集群状态」**（`/admin/cluster`）。页面每 10 秒刷新，也可手动刷新。它显示**当前登录节点**的角色、集群/节点 ID、incarnation、term、已承诺 term、启动就绪状态、三个复制索引、元数据投影，以及与配置中各 peer 是否有已认证数据会话。该页面是只读的，不能在页面上发起切换。

也可以使用已认证的 `GET /admin/api/v1/cluster/status`；Replica 的 Admin API 可单独返回本节点状态，但正常 Console 从 Primary 提供。UI 和 API 不提供对端真实水位。计划交接或提升前，要**在每个成员分别**读取认证状态：

```sh
./bin/halro cluster status --config /etc/halro/halro-0.yaml
./bin/halro cluster status --config /etc/halro/halro-1.yaml
./bin/halro cluster status --config /etc/halro/halro-2.yaml
```

记录每个成员的 `role`、`term`、`promised_term`、`durable_index`、`confirmed_index`、`applied_index`。正常稳定态应只有一个 Primary，各成员满足 `durable >= confirmed >= applied`，Replica 最终追平 Primary 的已确认索引。作为候选的 Replica 必须确认自己的三个索引**相等**，且其认证前缀符合交接或提升判据。状态读不到就是未知；不要将其当成索引 0 或健康。

## 5. 计划内人工切换：`stepdown`

先把客户端路由、长请求及回退方案纳入变更窗口。以下数值只是**命令格式示例**；执行前必须从目标节点刚读取的状态替换 term 和 index。

1. 保持旧 Primary `halro-0` 在复制通道上可达，让目标 Replica `halro-1` 追平。分别记录三个成员状态，确认目标 `durable == confirmed == applied`。
2. **停止目标 `halro-1` 的 Halro serve 进程**，等待其数据目录锁释放。旧 Primary 此时仍须运行。然后在目标节点使用其自己的配置执行：

   ```sh
   ./bin/halro cluster stepdown --config /etc/halro/halro-1.yaml \
     --from halro-0 --to halro-1 \
     --expect-term 7 --expect-index 10241 \
     --username admin --password-file /secure/admin-password
   ```

3. 旧 Primary 先撤销 readiness、等待已接纳的 HTTP 处理结束，才冻结复制前缀、持久承诺更高 term 并退出。若 drain 期间索引前进，命令会因 `--expect-index` 过期而拒绝；重新启动目标 Replica 让它追平，再停止目标、读取新状态并用新值重试。**不能把拒绝当作已切换**。
4. 命令成功后启动新 Primary `halro-1`，等 `/health/ready` 返回 200；再把旧 `halro-0` 作为 Replica 启动。分别核对所有成员的角色、term、三个索引与客户端访问，最终应只有一个可服务的 Primary、Replica 追平。如果旧 Primary 的元数据投影检查拒绝启动，先调查或按批准的流程重新播种，不能手改 `cluster/state.json`。

`promotion requires the local Halro process to be stopped: data directory is already locked by another process` 表示目标进程仍在持有数据目录。先停止 `--config` 指向的目标成员并等待退出；不要杀错旧 Primary，也不要删锁文件。

## 6. 非计划提升与故障恢复

旧 Primary 故障时，先在 Halro 外部证明它已被隔离，避免它继续向 Provider 发出请求。有效的 fencing 断言仅有 `pod-deleted-pvc-retained`（Pod 已删除，PVC 不会被自动重新挂载）或 `node-isolated`（节点断网或断电）；“进程已被 kill”不是足够的 fencing。无证据时停止提升操作。

选择认证前缀最新、完全追平且已停止的 Replica，在它自己的配置下执行。下面也是**格式示例**，term/index 必须从目标节点现场读取：

```sh
./bin/halro cluster promote --config /etc/halro/halro-1.yaml \
  --expect-term 7 --expect-index 10241 \
  --old-primary halro-0 --old-primary-fenced-by node-isolated \
  --username admin --password-file /secure/admin-password
```

三成员提升还需要 peer 的持久 promise；目标落后或拿不到必要 promise 时应拒绝，不能跳过判据。`--no-peer-promise` 仅供两成员集群的特殊人工流程使用，而且在另一节点回来前无法确认需要副本的写。一个停止的旧 Primary 若因启动裁决停在等待状态，才按 runbook 评估 `promote --self`。新 Primary ready 后，先确认只有一个 Primary，再恢复客户端流量。旧成员回归时核对 term、incarnation 和投影；丢失 PVC 的 Replica 必须重新走批准的 seed。不得编辑或复制 `cluster/state.json` 来“修复”角色。

## 7. 备份、恢复与升级

- **Replica 备份**：先在该 Replica 上执行 `cluster maintenance on`，确认 readiness 失败而 liveness 仍在；再用 `backup create --replica` 创建加密备份并 `backup verify`。用 `maintenance off` 恢复并等它追平。随后停止 Primary，使用 `cluster report-backup` 记录已验证备份。完整命令及文件权限见 [runbook](../runbooks/ha-operations.md#replica-backup-and-reporting)。
- **全集群恢复**：隔离所有旧成员和客户端，用已验证的 HA 备份恢复为**新的 incarnation**，再播种所有 Replica。旧 PVC 保留旧 incarnation，不能直接加入新集群；见 [恢复流程](../runbooks/ha-operations.md#whole-cluster-restore)。
- **升级**：一次只动一个成员。先确认不是当前 Primary，必要时先做 `stepdown`；相邻版本及 schema 兼容性必须按 [HA 测试指南](../verification/ha-test-guide.zh-CN.md) 验证。示例 StatefulSet 使用 `OnDelete`，不会替运维完成升级排序。

## 8. 验证清单与常见误判

本机三进程适合验证配置、mTLS、复制、Admin 状态页和计划切换，但不能替代独立节点、PVC、网络分区及 Linux 故障测试。先使用合成请求和模拟 Provider，记录每步各节点 `cluster status`、readiness、复制指标和审计/账本检查结果。按 [HA 测试指南](../verification/ha-test-guide.zh-CN.md) 依次覆盖正常复制、单 Replica 中断、带长请求的计划交接、旧索引拒绝、旧 Primary fencing 后提升、非对称分区、PVC 丢失、全集群恢复、相邻版本、ENOSPC/慢盘与 72 小时浸泡。

| 现象 | 应如何理解 |
| --- | --- |
| `unknown command "cluster"` | 当前执行的二进制太旧或路径指向旧制品；核对 `version`、`help cluster` 和实际可执行文件。 |
| 数据目录被锁 | 离线命令指向的**本地**成员仍在运行；停机并等锁释放。 |
| peer 显示「已连接」 | 只证明已认证会话存在，不证明对端已追平；到对端分别读取三个索引。 |
| `--expect-term` / `--expect-index` 不匹配 | 状态在读取后变化；重新读取和判断，不盲目递增或覆盖。 |
| `503 not_primary` | 请求落到了 Replica；按当前 Primary 调整客户端路由或安全重试。 |
| `503 replication_unavailable` | 必须等待副本确认的写暂不可用；检查 quorum、网络和持久索引，不能静默降级为单写。 |
| 没有任何 ready Primary | 按启动裁决、fencing 和 runbook 处理；不会自动选主。 |

当前生产放行状态与证据要求以 [HA 架构设计 §1.3、§18](../todo/halro-ha-architecture.zh-CN.md) 为准。一次成功的本机切换只证明该次演练，不证明自动故障切换、生产 RPO/RTO 或目标环境可用性。
