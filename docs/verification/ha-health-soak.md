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

启动前核对操作员证书、信任 CA 和服务端证书在整个观测窗口及恢复余量内
均有效。例如 72 小时窗口可用
`openssl x509 -checkend 266400 -noout -in <证书路径>` 逐份检查；服务端
证书须从实际目标部署核对。采样器在
启动时加载客户端证书和信任 CA，运行期间不会自动重载；即使脚本持续运行，
证书中途到期也会产生 `unavailable`，不能把采样进程存活视为覆盖完整。

对于 Kubernetes 部署，先复核实际使用的全部证书 Secret，把每个 Secret
及其 `.crt` 键写入版本 1 清单；本地 kind 示例见
[`halro-ha-health-certificates.kind.example.json`](../../deploy/kubernetes/halro-ha-health-certificates.kind.example.json)。
在具备这些 Secret 只读权限的验证主机执行：

```sh
python3 tests/ha-health-soak/cert_preflight.py \
  --manifest deploy/kubernetes/halro-ha-health-certificates.kind.example.json \
  --certificate-file operator=/approved/path/operator.crt \
  --min-valid-seconds 266400 \
  --output /private/evidence/ha-cert-window-001
```

状态须为 `ready_for_window`，才具备这项有效期前提。该工具核对清单中每个
Secret 的 `.crt` 键集合，逐张检查 PEM 链中的证书。Kubernetes API 仍会
把完整 Secret 交给 `kubectl` 进程；模板只将键名和公开证书值传给预检
进程，报告不输出私钥。应在受控验证主机运行；`certificate-window.json`
在新建的 `0700` 目录中以 `0600` 保存。尚未生效的证书也会阻止通过。
不在 Secret 中的操作员或其他客户端证书必须逐一通过可重复的
`--certificate-file 名称=/绝对路径/公开证书.crt` 纳入同一报告；该选项只读
公开证书文件。工具不会发现清单遗漏的整个 Secret 或文件，
也不证明证书信任关系、身份权限、运行中 Pod 已加载的字节或后续轮换安全。
清单必须与目标部署
配置和 Secret 实际引用核对，报告的清单 SHA-256 与证据一同留存。

### 本地 kind 证书轮换准备

当前本地 kind 样例同时使用成员/健康入口 CA 和独立 Prometheus 查询 CA。
正式 72 小时窗口前，两套 CA、所有服务端及客户端证书都必须覆盖完整窗口；
只续签叶子证书不能补救已临近到期的 CA。只在本地隔离验收环境中，
可用以下命令**离线准备**两套 14 天测试 PKI：

```sh
python3 tests/ha-health-soak/stage_kind_certificates.py \
  --output /private/ha-health-kind-certificates-new
```

脚本新建 `0700` 目录、生成 `0600` 私钥，为三个成员、健康服务、采集器、
Prometheus 抓取、操作员和 Prometheus 查询双方生成九张叶子证书；校验链、
用途、SAN、证书与私钥匹配及至少 74 小时有效期。`stage-report.json` 只含
证书指纹和五个本地 Secret 的**文件路径映射**，不含私钥字节。它不访问
Kubernetes，也不安装或轮换任何证书。输出目录应在受控私有路径，绝不可
把其内容提交仓库。脚本的名称和 SAN 专用于
`halro-ha-health-local` 样例；目标环境须用受控 PKI 自行签发和审查。

**已有 incarnation 的成员不能直接使用上述暂存包中的三把新成员私钥。**
复制握手还会核对成员持久配置和认证 `state.json` 中的 SPKI pin；仅扩展 CA
信任不能授权公钥变化。实际本地演练中，只替换第一台 Replica 的新私钥便使
Primary 记录 `spki` 不兼容，页面安全卡降级。`kind_tls_replace.py` 现拒绝
没有固定 SPKI 派生报告的成员 `leaves` 阶段。例行续签且确认旧私钥未泄露时，
先冻结双 CA 阶段的成员 Secret 私有备份、核对三份运行配置中的 peer pin，
再离线生成保留成员公钥的新证书暂存包：

```sh
python3 tests/ha-health-soak/reissue_kind_pinned_members.py \
  --source-stage /private/ha-health-kind-certificates-new \
  --source-report-sha256 SOURCE_STAGE_SHA256 \
  --member-backup /private/secret-backups/leaves-halro-halro-ha-cluster-tls-rvNNN.json \
  --member-backup-sha256 MEMBER_BACKUP_SHA256 \
  --pin halro-0=sha256:CURRENT_HALRO_0_PIN \
  --pin halro-1=sha256:CURRENT_HALRO_1_PIN \
  --pin halro-2=sha256:CURRENT_HALRO_2_PIN \
  --output /private/ha-health-kind-certificates-pinned
```

三个 `--pin` 必须来自实际成员配置，且与冻结证书和私钥逐一匹配；输出为
另一份 `0700` 私有暂存目录，`stage-report.json` 给出新的摘要。双 CA
`trust` 阶段仍使用原始暂存包；此后 `leaves` 和 `final` 阶段都使用
**派生目录及其摘要**，不得用原始新私钥包更新成员。
此流程只更新证书有效期，不轮换成员密钥。若私钥泄露或必须换成员公钥，
须走能认证修改成员 pin 的独立协议或新 incarnation 恢复流程，不能修改
Secret 和 YAML 后把断开的复制会话视为可用。

准备不等于轮换。采样器启动时会加载 CA 和操作员证书，运行中不会自动
重载；正式候选采样应在证书轮换和有效期预检完成后启动。若已有采样正在
运行，应先保存该轮 `summary.json`，再依据 `stage-report.json` 核对实际
Secret 键、服务端名称与 Prometheus `client_allowed_sans`，于维护窗口
更新两套信任域和全部引用它们的工作负载。轮换期间出现的采样缺口不能
拼接为连续 72 小时，须另起候选目录重新计时。根 CA 跨代时须分三阶段：

1. **扩展信任**：分别把当前 CA 与新 CA 拼接为两套 PEM bundle，只更换五个
   Secret 中对应的 `ca.crt` 或 `client-ca.crt`，保留旧叶子证书与私钥。重启
   所有引用这些 CA 的成员、健康服务及 Prometheus，验证旧链仍能工作；
   在未证明全部进程加载双 CA 前，不更换任何叶子证书。
2. **更换身份**：保留双 CA bundle，以固定 SPKI 的派生暂存包逐一更换三个成员
   的证书，再更换健康服务、采集器、
   Prometheus 抓取、Prometheus 查询服务端与客户端证书，并切换操作员证书。
   成员按实际角色先 Replica 后 Primary 滚动；每一步核对角色、term、成员
   身份、真实 Peer 会话、复制水位和客户端 Service。Prometheus 的
   `client_allowed_sans` 须与新客户端 SAN 一致。
3. **移除旧信任**：确认所有身份都已使用新证书后，把五个 Secret 的 CA 字段
   缩减为新 CA，重启引用方，分别验证旧操作员、抓取和查询客户端证书被
   拒绝，新链各路径仍可用。最后以实际 Secret 重跑 74 小时预检。

成员证书使用 `subPath` 挂载，Secret 更新不会让运行中成员自动读取新字节；
健康服务与 Prometheus 也须核对实际进程加载的新证书。更新 Secret 时避免
把私钥写入命令参数、仓库文件或 `kubectl apply` 的 last-applied 注解；由
受控 Secret 管理器执行，或通过标准输入传入经过资源版本核对的替换对象。
每阶段记录 Pod UID、服务端与客户端证书指纹、采集缺口、恢复时间、
Secret 资源版本和回滚点。公开证书的离线链校验只证明 PEM 可互信，
不能证明运行中进程已加载 bundle；这些证据齐备前不启动正式窗口。

本地 kind 的逐 Secret 操作工具是
`tests/ha-health-soak/kind_tls_replace.py`。它仅接受上面离线暂存报告中的
五个 Secret，默认对指定的 `trust`、`leaves` 或 `final` 阶段执行 Kubernetes
**服务端 dry-run**。每次都核对两个报告 SHA-256、Secret 精确键集合、现行
CA 与证书阶段、资源版本、元数据安全性；它不会把私钥放进命令参数或
last-applied 注解。下面的示例只预检一个 Secret：

```sh
python3 tests/ha-health-soak/kind_tls_replace.py \
  --secret halro/halro-ha-cluster-tls --phase trust \
  --stage-dir /private/ha-health-kind-certificates-new \
  --stage-report-sha256 STAGE_REPORT_SHA256 \
  --overlap-dir /private/ha-health-kind-ca-overlap \
  --overlap-report-sha256 OVERLAP_REPORT_SHA256
```

实际更新必须另外传 `--apply --expect-resource-version <刚核对的值>
--backup-dir <已存在的 0700 私有目录>`；工具会先做服务端 dry-run，随后
以资源版本保护执行一次 Secret replace、重新从 API 读回并比较所有键。
每次应用前把原 Secret 完整保存为该私有目录中的 `0600`、fsync 备份，
输出仅包含操作状态、版本、变更键名和备份摘要。备份含真实私钥，须按
目标环境密钥管理规则保管与清理，绝不可提交仓库。每次只处理一个 Secret；
工具**不**滚动 Pod、不替换操作员本机证书、不验证运行中 mTLS，也不代替
上述阶段间的业务和复制检查。任何阶段出现部分成功或身份不符，先停止
后续更新并按私有备份与现场资源版本人工恢复。

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
