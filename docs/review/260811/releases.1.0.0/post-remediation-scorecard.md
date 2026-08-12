# v1.0.0 整改后评分与能力边界（2026-08-12）

> 评分体系沿用 [scoring-rubric.md](scoring-rubric.md)：同一套锚点、同一组权重、同一条封顶规则。
> 对照基准是 [release-1.0.0-report.md](release-1.0.0-report.md)（冻结件，不因本文改动）。
> 整改台账见 [progress.md](progress.md)，整改核对与收尾见 [remediation-verification.md](remediation-verification.md)。
>
> 评分对象：`0b8f7371fb71941920a00a761694383e14f09421`（`0b8f737`，2026-08-12 11:01:20 +0800）
> 加上本轮工作区的收尾改动（勘误落书面、三条接受项记录、G5 重跑证据）。
> 执行环境：go1.26.5 darwin/arm64，macOS 26.6。
>
> **本文的每个分数都由 2026-08-12 当天的实跑或代码核对支撑，不采信任何台账自述。**

---

## 1. 判定

# **No-Go**

**封顶前加权总分 7.45 / 10；封顶后最终分 6.5 / 10**（宽读下 5.0，见 §3）。

G4 与 G7 未通过 → 按 rubric §5 总分封顶 6.5、且不得出具 Go；按 §6 的映射，
`6.5~8.0` 且有未通过判据 = **No-Go**。

**但性质与报告当时完全不同。** 报告 §1.1 给出三条互相独立的否决理由：

| 报告的否决理由 | 现在 |
|---|---|
| G2 有 fail-closed 反例（新代码把 fail-closed 反转成 fail-open） | **已关闭**。stale 按四域建模、恢复循环重放全域，两处 lookup-miss 在代码层改为 fail-closed，网关在授权阶段先拒整个 Project；14 次反向注入证明测试在缺陷态会红 |
| 公告面成体系缺席（CHANGELOG 零提交、已知限制未进发布说明、rc.1 升级路径无公告） | **已关闭**。CHANGELOG 四条 Operator impact、20 条已知限制、"从 rc.1 升级"整节（含 V4 那条破坏性排查动作） |
| G4 未通过且发布流水线从未闭环 | **仍不通过**，且今天复查仍是四个零 |

剩下的两条未过判据（G4、G7）**没有一条在仓库里**：一条是 GitHub 侧的环境与一次真实发布，
一条是用户在若干产品取舍上签字。

---

## 2. 八维度评分

| 维度 | 权重 | 报告原判 | 现在 | 主要加分项（带 file:line 或实跑） | 仍在的扣分项 | 到 8 分还差什么 |
|---|---:|---:|---:|---|---|---|
| D1 安全性 | 0.20 | 6.5 | **8.0** | V1 那条 fail-open 不是"补一句裁决"而是代码层关死：`internal/redaction/engine.go:433-444` 与 `internal/tokenguard/manager.go:311-322` 对具名策略缺失一律拒绝（空 ID 仍放行，因为那是"该项目没有策略"这个决定），`internal/gateway/service.go:184-220` 在授权阶段一次性拒绝这类 Project 并返回 503 `configuration_stale`；两条花费凭据的侧信道改为 POST 走 `requireAdminMutation`（角色 + CSRF + Origin），`requireCredentialedAdminRead` 整个删除、不与替代品并存；argon2 全局信号量 + 实测；**step-up 判据从"破坏性"改为"破坏性 or 削弱在执行的控制 or 花费凭据"**（`PUT /credentials/{id}`、`PUT /redaction-policies/{id}`、`PUT /token-guard-policies/{id}`、`POST …/model-capability-detections`），并由按路由族的 sweep 门禁守护（`TestEverySecurityControlEditRequiresStepUp`，反向验证：拿掉任一处该测试 FAIL） | 无 P0/P1。**rubric 的 9 分要求判断建立在实跑证据上、10 分要求守护自动强制，而本轮评审自述是 "source-and-test review, not a third-party penetration test"——没有外部对抗验证，D1 的天花板就是 8** | 外部渗透测试或独立安全评审（1.0.0 之后） |
| D4 逻辑正确性 | 0.20 | 6.5 | **7.5** | stale 按四域建模（`internal/app/activation_state.go:39-60`）、恢复循环重放全部四域；T-1/T-2/T-3 从调用点出发且缺陷态会失败；T-8 补上探针阶段上限并反向验证（删掉 `admin_model_capability_detections.go:427` 该测试 FAIL）；A4-11 改成 8 goroutine 真并发断言；`go test ./...` 今日复跑无失败 | `internal/safetransport/transport.go:139,151,155` 三个"本方保证零字节发出"的拒绝点仍返回裸 `fmt.Errorf`，`provider.Unsent`（`internal/provider/provider.go:77-87`）认不出，按 ambiguous 满额结算并抑制 failover——**异常路径尚未 fail-closed 的一处**；`internal/store/bolt/model_capability_detection.go:169-197` 仍在 `ForEach` 内 `Put`；`runActivationRecovery` 本体零测试（已记为接受） | 类型化 safetransport 自拒错误 + 负面测试（denied 地址 → 结算必须为 0）；`ForEach` 内 mutation 改先收集后写入 |
| D2 系统设计 | 0.12 | 7.0 | **7.5** | 报告点名的设计错误已改：`activationTracker` 从一个标志变成四个独立域；`admin_user.create/delete` 迁入 `AdminAuditIntent`（`internal/app/admin_users.go:82-87,145`） | Runtime 68 字段、跨 admin/非 admin 边界符号约 27 个不变；"1.0.0 前不拆 internal/app" 仍是取舍而非解决 | 1.0.0 后第一个间隔做 internal/app 的书面二选一 |
| D7 可运维性 | 0.12 | 6.5 | **8.0** | 本轮涨幅最大。告警 24 → **30** 条，新增签名目录降级、能力漂移、探测失败率三类并进 `rule-tests.yml`；rule-test 有效性反向验证（把 `HalroConfigurationStale` 的 `for: 1m` 改 5m，promtool 立刻 FAILED）；`configuration-stale.md` 与 `file-master-key-rotation.md` 两条 runbook 进 `docs/runbooks/embed.go` 并各有 Admin 路由；控制台四个激活域常在并点名 runbook；`operations-runbook.md` 有 23 个按 alertname 的小节；**G5 于今日在当前代码上重跑通过**（§4） | stale 端到端链（真二进制 → 真 Prometheus → 真 Alertmanager 1m 后 firing）仍是三段独立实证的拼接；`release_24h` 未归档 | 把 stale 端到端跑一次；归档 soak 工件 |
| D8 可交付性 | 0.12 | 4.0 | **5.5** | 四个二进制归档**逐字节可复现**（两个独立容器、不同路径/mtime/umask 实证，且对照修复前命令确实不同）；验签命令块完整可执行（`gh release download`、`--ignore-missing`、cosign ≥ v2.2 前提）；license review 重生成 + CI 门禁反向验证会红；`ci.yml` 19 处 `uses:` 全 SHA-pin 且有门禁；NOTICE / SBOM / provenance 补齐；CHANGELOG 与 20 条已知限制到位 | **仍未产出过任何 Release**：今日复查 `releases`、`environments`、`actions/secrets`、release workflow runs **四项均为 0**；两个容器包不可复现（已在 `releasing.md` 声明范围并给出两条根因） | 建 `v1-release` Environment + required reviewers、装 environment secret、跑通一次 publish |
| D6 可用性 | 0.10 | 6.0 | **7.5** | `operator-guide.md` 的 `data_dir`、`server.gateway_listen: 0.0.0.0:8080`、init 步骤补齐；`internal/vault/masterkey.go` 的 EEXIST/EACCES 两条错误都带路径与下一步（本轮搭建 G5 实例时被真实触发，逐字给出路径与两种读法）；`doctor` 把未初始化数据目录单列一支；五个 `*_idempotency_replay` 有 i18n 与 deep-link；onboarding 以"该 Route 上出现过成功请求"为充分证据，级联码方向修正后 25 个 detail code 在 zh-CN/en-US **逐个命中、无漏译** | 容器小节与空卷首启**没有逐字实跑过**（结论来自读码 + 同工作区 CI 脚本对照）；无真实浏览器验收 | 按指南原文起一次容器并从宿主 curl 到 200 |
| D5 性能与容量 | 0.08 | 6.5 | **7.5** | argon2 从解析性论证变实测：64 并发峰值堆 **256 MiB**（每并发 4.0 MiB），对照无上限约 4,096 MiB；三个容量数字（1,223 lifecycles/s、约 31 mut/s、拓扑协议 −25.3%）归档进 `performance-baseline.md` 并写入已知限制 19 | Linux/NVMe 绝对值与 24h soak 工件仍缺；取槽无 deadline 的排队行为未在风暴下实测（已记为接受并写明取舍） | Linux/NVMe 复跑并归档；跑满 24h soak |
| D3 工程规范 | 0.06 | 6.5 | **8.0** | `idempotency-contract.md` 拆成"数据面 / Admin create"两节，不再自相矛盾；指标静态清单门禁扩到 `writeLatencyHistogram`（86 → 88 族）；两份 manifest 补 unknown-field deviation（全文 19 处 unknown 声明）；stale 与 503 语义进 `gateway-correctness.md:39-48`；`alerting-rules.md` 的 Core groups 补入三类；字阶门禁反向验证会红 | 覆盖广度而非矛盾：契约面已基本闭合 | — |
| **加权总分** | **1.00** | **6.21** | **7.45** | 触发的封顶规则：**G4/G7 未通过 → 总分封顶 6.5**（rubric §5 第 2 行）。**封顶咬合**，最终分 = **6.5** | | |

### 2.1 两个数字的差距，这次说的是相反的事

报告那轮加权分 6.21、封顶 6.5，差 0.29，结论是"封顶几乎没起作用——**问题是弥散的**"。

这轮加权分 7.45、封顶 6.5，差 **0.95**，封顶明确咬合。按同一条读法，这说明
**问题已经收敛到少数几点**，而且那几点可以逐一点名：一次 GitHub 环境配置、一次真实发布、
用户在若干产品取舍上签字。八个维度里没有一个低于 5.5，最低的 D8 也从 4.0 抬到 5.5，
六个维度落在 7.5~8.0 的窄带里。上一轮"新增面把整改的收益吃掉了"的形态没有重演——
本轮没有新增面，只有配套面的补齐，而配套面正是上一轮的失分源。

---

## 3. G1~G9 现状（2026-08-12 复查）

| 判据 | 报告原判 | 现在 | 依据 |
|---|---|---|---|
| G1 | 通过（窄读）/不通过（宽读） | **窄读通过；宽读 P0 数 2 → 1** | B3-03 已修（二进制侧可复现，容器例外已声明）；B3-01 仍存续 |
| G2 | **不通过**（fail-closed 有反例） | **通过** | 四域建模 + 全域重放 + 缺陷态可失败的负面测试；两处 lookup-miss 已在代码层 fail-closed，不是"写一句裁决" |
| G3 | 通过（§3.3 至今是空占位符） | **通过，且有当天一手记录** | `go vet ./...` exit 0、`gofmt -l` 空、`go test ./...` 无失败、前端 typecheck + 32 文件 297 测试、`npm run build` 后 `internal/webui/dist` **零漂移**、`validate.sh` 绿、`check-production-assets.sh` 绿 |
| G4 | **不通过** | **仍不通过** | 今日只读查询：`releases` = 0、`environments` = 0、`actions/secrets` = 0、release workflow runs = 0 |
| G5 | 通过 | **通过，证据当天新鲜** | 见 §4。原证据产生于 `archive.go`/`backup.go` 改动之前，本轮在当前代码上重跑 |
| G6 | **不通过（严格口径）** | **通过（严格口径）** | 三条 m11 runbook 有 release-blocked 标注；catalog 示例 `profile_id` 已改且实跑 verify 通过；`gateway-key-compromise.md:103` 已补 `--config` |
| G7 | 未通过 | **仍未通过（6 条 🔴 → 4 条）** | 已闭：#3 两个 PUT 的 step-up **按"修复"这一选项关闭**（判据扩到四个入口并加 sweep 门禁，见 §2 D1 行与 §5.2）；#16 的书面裁决已给出（四道门按归属拆成交付面与容量面，安全自有门为 0，见 `security-review-v1.md`）。仍空白 4 条：`syncUsageAdmin` 仍用 `request.Context()`（`internal/app/admin_usage.go:567-573`）、`internal/sourcelimit/limiter.go:112` 无取舍注释、rc.1 根因、能力选择的浏览器验收与真实 Provider 证据。**#13 的第三子项已因勘误而消解**（见 §5.1） |
| G8 | 未完成 | **通过** | 发布说明 20 条已知限制，⚠ 三条按要求改写而非固化 |
| G9 | 通过 | **维持通过** | `release_24h` 仍未归档，报告已明确该项在 G9 外 |

**宽读记录**：若把 B3-01 计为 D8 的 CONFIRMED P0，则 D8 封顶 4（加权分变 7.27）、
总分封顶 5.0，最终分 **5.0**。两种读法下判定都是 No-Go，差别只在记录在案的分数。

---

## 4. G5 重跑证据（2026-08-12，当前代码）

隔离实例跑在 scratchpad：自带 `data_dir` 与 `master.key`，监听 18080/18081/19090，
上游按 C1 原法指向 `https://upstream.invalid`（请求 502，认证与记账链路照走，不产生计费）。
仓库根目录的 `data/` / `master.key` / `config.yaml` 全程未被触碰。

| 核对项 | A（备份点） | B（备份后 4 次请求 + 撤销 key） | 恢复后 | 判定 |
|---|---|---|---|---|
| Ledger 已认证帧数 | 20 | 40 | **20** | 精确回到 A |
| Ledger head | seq 20 / off 21689 | seq 40 / off 43387 | **seq 20 / off 21689** | 精确回到 A |
| Ledger chain hash（前 4 字节） | `a2 7b a8 ee` | `83 02 b1 c7` | **`a2 7b a8 ee`** | 精确回到 A |
| `ledger.wal` sha256 | `d1ac88fe…` | — | **`d1ac88fe…`** | 逐字节相同 |
| ChainVerified | true | true | **true** | 通过 |
| Usage ledger/parquet | 4 / 4 | 8 / 8 | **4 / 4** | missing/duplicates/extra 全 0 |
| Audit 记录数 | 3 | 9 | 5 | 单调追加：A 的 3 条 + `backup.create` + restore |
| `doctor` | healthy=true | — | **healthy=true, vault=verified, schema v27, leases pending=0** | 通过 |

负面用例仍 fail-closed：错的 `--confirm-backup-id` 直接
`restore confirmation must exactly match the verified backup id`（exit 1），
事后 live 仍是 seq 40 / off 43387，**零改动**。`backup verify` 的 manifest 与
`backup create` 的逐字段相同。

**重跑的价值在于三处改动在真实输出里可见**：restore 返回 `schema_version_before: 27` /
`schema_version_after: 27`（R-34），以及 `restored_enabled_gateway_key_count: 1` 与
`restored_enabled_gateway_key_ids: ["key_z21y…"]`（R-35）——**点名的正是 B 阶段被撤销、
恢复后重新生效的那把 key**；恢复后启动，该 key 的请求确实重新通过鉴权。

---

## 5. 两条 D1 相关的收尾

### 5.1 一条影响 G7 输入的勘误

报告 §9.3 #13 与 `carry-forward.md` 第 5 行都称 `provider_metadata`"代码里只有枚举值与校验、
无任何 Adapter 发射它"。**这是事实错误**：在评审 HEAD `2cd24a7` 上，
`internal/provider/gemini/adapter.go:251`、`internal/provider/bedrock/models.go:153`、
`internal/provider/anthropic/adapter.go:192` 都在发射 `domain.ClaimSourceProviderMetadata`，
各包的 `DescribeInvocationTargets` 有测试覆盖。

因此报告建议的"撤销该枚举值或补实现"建在不成立的前提上，**该子项不需要裁决**；
G7 #13 只剩浏览器验收与真实 Provider 证据两条仍待拍板。更正已就地写入 `carry-forward.md`
与 `progress.md` 的 Report errata。

### 5.2 step-up 的判据轴选错了，已按"修复"关闭

**原状的不对称没有可辩护的读法**：删掉一条脱敏策略需要重新认证
（`internal/app/admin_redaction.go:171` 的内联 `requireDestructiveStepUp`），
把同一条策略编辑成一条规则都不剩不需要（`runtime.go:1457` 只有 `requireAdminMutation`）。
数据面分不出这两者。Token Guard 同理（编辑成不限），
凭据 PUT 同理（替换的是信任边界所依赖的材料）。

判据本来就写在仓库里——`unblockAdminProject` 的注释说得很清楚：
"not only what destroys state, but what removes a protection that is currently in force"。
问题不是缺判据，是判据只落到了 DELETE 上。

**改法**：把 step-up 扩到四个入口——`PUT /credentials/{id}`、`PUT /redaction-policies/{id}`、
`PUT /token-guard-policies/{id}`、`POST /providers/{id}/model-capability-detections`
（最后一条是"花费凭据"那一类：≤12 次上游调用、不进项目记账、写入可被 Deployment 采纳的能力证据）。

**不做条件判断**：只在"这次编辑确实削弱了控制"时才要 step-up，需要一个
"新状态是否至少一样强"的谓词——那本身是安全关键逻辑，判错就是在最该拦的那次编辑上 fail-open，
而且路由层看不见请求会走哪条分支，无法 sweep。因此四个入口**每次编辑都问**。

**守护**：`TestEverySecurityControlEditRequiresStepUp` 按路由族扫描（credentials /
redaction-policies / token-guard-policies / model-capability-detections），
非 GET 路由要么要求 step-up、要么在具名豁免表里，与既有的
`TestEveryDestructiveDeleteRequiresStepUp` 同一形状——**新增动词注册当天即进范围**。
反向验证：用 `go build -overlay` 拿掉 `admin_redaction.go` 的那次调用，该测试 FAIL。

**范围外并写明理由**：Provider / Deployment 连接测试与调用目标刷新/解析——单次有界调用、
不改策略、不写能力证据，由角色 + CSRF + 同源 + 限流 + 逐调用持久台账兜底。
这条边界写进了 `docs/verification/security-review-v1.md` 的 "Step-up criterion" 一节。

控制台侧同步收紧：四处入口都在提交前收集口令与 TOTP（策略启停走确认对话框，
表单走 `ReauthFields`），zh-CN/en-US 文案齐备——**不重演 R-24 那次"服务端收紧、
浏览器发不出"的回归**。

---

## 6. 当前系统的能力边界

评分回答"够不够发布"，本节回答"它能做到哪里"。所有数字与限制都可在
`docs/milestones/release-notes-v1.0.0.md` 的已知限制章节与
`docs/verification/performance-baseline.md` 复查。

### 6.1 协议与 Provider

- **兼容端点 4 个**（`docs/compatibility/endpoint-manifests.json` 为准，有 golden 门禁）：
  `POST /v1/chat/completions`、`POST /v1/embeddings`、`POST /v1/responses`、`POST /v1/messages`。
- **实验端点 15 个**：moderations、images/generations、audio/transcriptions、audio/speech、
  files（4 条）、batches（3 条）、rerank、async invocations（3 条）。
- **未知字段一律拒收**：Chat 与 Embeddings 对 manifest 之外的字段返回 400，
  包括 `frequency_penalty`、`presence_penalty`、`logit_bias`、`logprobs`、`store`、
  `metadata`、`service_tier` 这七个官方 OpenAI 参数。
- **Provider**：OpenAI、Anthropic、Azure OpenAI、DeepSeek、openai_compatible 为正式面；
  **Gemini / AWS Bedrock / Bedrock Mantle 为 Beta 且能力天花板钉死**——天花板不可注入是
  **类型层面**保证（`gemini.Options` 与 `bedrock.Options` 结构里没有 Capabilities 字段），
  越界请求在任何 Provider I/O 之前被 400 `unsupported_feature` 拒绝，
  **不预留额度、不建立上游连接**。
- **应用只见 Gateway Key 与公开模型别名**，永远看不到 Provider 凭据与上游模型标识。

### 6.2 容量（macOS/APFS + `F_FULLFSYNC` 实测，**是地板不是天花板**）

| 指标 | 实测 | 形状 |
|---|---|---|
| 记账生命周期 | **1,223/s @ 64 并发** | 随并发扩展，不随项目数扩展 |
| 管理写路径 | 约 **31 mutations/s** | 其中拓扑提交协议占 **25.3%**（41.69 → 31.13） |
| 长跑内存 | 2,588 次真实变更后 RSS **+1.4 MiB**，约 900 次后持平，goroutine 稳定 18 | 无内存泄漏、无 goroutine 泄漏 |
| 登录内存 | 每次 argon2 约 64 MiB，**进程级并发上限 2**；64 并发登录峰值堆 **256 MiB**（无上限时约 4,096 MiB） | 内存有界，**排队无 deadline**——超出的登录等待而非快速失败 |

未取得：Linux/NVMe 的发布级绝对数字、24h soak 的 `release_24h` 工件。

### 6.3 一致性与可用性

- **单写者、单数据目录**。Docker/Kubernetes 必须**恰好一个副本**（`Recreate`，非滚动更新）。
  不存在共享目录或多写者形态。
- **无高可用**。单实例故障即服务中断，HA 提案排在 1.1.0。
- **任一激活域 stale → 整机对全部 Project 返回 503**。爆炸半径全实例是**设计选择**：
  宁可拒流也不用已知陈旧的授权状态。四域每 5 秒自动重试；指标、critical 告警、
  控制台、`docs/runbooks/configuration-stale.md` 都有覆盖。OpenAI 路由返回
  `configuration_stale`，Anthropic Messages 返回 `overloaded_error`，均为 503 + `Retry-After: 5`。
- **关闭预算默认 2 分钟**且不得短于 `gateway.route_total_timeout`；
  service manager 的终止宽限必须更长（systemd 已同步 150s，k8s 为 150s）。
  预算耗尽时仍在途的 attempt 被强制关闭、模糊即保守结算，并计入持久的
  `halro_shutdown_truncated_attempts_total`。
- **数据目录的父目录必须可写**（发布锁建在父目录）。容器/K8s 挂载持久化的**父目录**，
  `storage.data_dir` 取它的**子目录**。
- **不支持 Windows**（数据目录锁依赖 Unix `flock` 语义）。

### 6.4 记账

- **Ledger WAL 是唯一记账权威**；bbolt 检查点、内存聚合、Parquet 用量文件都是可重建派生物。
- **控制面 Provider 调用不进 Ledger**：能力探测、Provider 连接测试、健康探针可能产生真实
  上游费用，但不进 Ledger、项目预算或用量统计。支出三重有界（每次检测 ≤12 次调用、
  每次 ≤2048 字节入 / 16 token 出），逐调用有持久台账与 Prometheus 计数，
  但**运维的真实上游账单会略高于账本合计**。
- **模糊上游结果保守结算**：证明得了"没有任何请求字节到达 Provider"的失败
  （连接被拒、DNS 失败、上游 5xx）**结算为 0 并全额释放预留**；证不出来的中间地带
  按预留上限结算，既不盲重试也不静默退款。

### 6.5 安全

- **出网只走 `safetransport`**：HTTPS-only、显式 host 白名单、pinned dialing 时对每个解析
  地址复跑校验（杜绝 DNS-rebind）、无重定向、不读环境代理。私网开关生效后只多放开
  RFC1918 + CGN；云元数据地址（169.254.169.254、100.100.100.200）、环回与保留段
  （含 6to4/Teredo/NAT64）仍被无条件拦截。
- **Admin 面**：TOTP 2FA、CSRF、revision 校验、破坏性操作 step-up。
  step-up 覆盖破坏性操作、削弱在执行的控制的编辑（凭据轮换、脱敏与 Token Guard 策略的
  编辑）、以及花费凭据的能力探测；Provider/Deployment 连接测试与调用目标刷新按书面
  裁决留在范围外（单次有界调用、不改策略、不写能力证据）。
- **凭据**：AES-256-GCM + audience-bound AAD（`kms` / `vault` / `masterkey`）；
  Gateway Key 仅存 SHA-256，明文一次性返回。
- **秘密不外泄**：审计只记 reason class 与摘要，生产 bundle 有密钥金丝雀扫描门禁。

### 6.6 交付（当前最弱的一环）

- **从未产出过任何 Release**。`v1-release` Environment 与 evidence secret 今天都不存在，
  今天打 tag 仍会失败——但断点已前移：现在是 `release-governance` job 在
  `gh api .../environments/v1-release` 上拿 404 直接失败，`publish` 因 `needs` 未满足而 skip。
  这同时避免了 GitHub 在 publish 跑起来后自动创建一个**不带保护规则**的环境
  （rc.1 那次正是如此）。
- **四个二进制归档逐字节可复现**（两台"机器"实证）；**两个容器包不可复现**，
  根因与实测数字写在 `docs/guides/releasing.md` 的 Reproducibility scope。
- **无官方容器 registry**：发行物是 `halro-container.tar.gz`，运维自行 load、
  推到自己的 registry、替换 K8s 清单里的显式占位 digest。
- **KMS Key Slot 模式 release-blocked**，file 模式是已验证的默认。
- **动态签名模型目录在 1.0.0 未启用**：无生产信任根，验签 fail-closed 回落内置目录，
  `trust_root_count: 0` 是预期值。
- **rc.1 无升级路径**：必须重建实例。用 1.0.0 试启动一次会把 bbolt 单向迁到 v27，
  **退回 rc.1 二进制也打不开了**——排查动作本身具破坏性。
- **旧备份能恢复出已撤销的 Gateway Key**：恢复后必须重跑一遍撤销清单
  （restore 现在会点名这些 key，见 §4）。轮换前的备份需要其历史 Master Key 代次。

### 6.7 v1 有意不做

Redis/PostgreSQL、多节点集群、SSO/OAuth、组织级多租户、Kubernetes Operator、
Prompt/RAG 管理、Agent 追踪与评估、工作流编排、插件系统、MCP Server、多区域同步。
生产环境不暴露 pprof / core-dump / 崩溃上报端点。

---

## 7. 从 No-Go 翻盘还剩什么

报告 §1.3 的最短关闭路径三件事，第一件（stale 与 fail-closed）与第三件（公告与文档）
**已经做完**。剩下的是：

| 顺序 | 做什么 | 关掉哪些判据 | 成本 |
|---|---|---|---|
| 一 | 建 `v1-release` Environment + required reviewers + Prevent self-review，把 evidence 装成 **environment secret**，打 rc.2 走通一次 publish 并保留该 run | **G4**，并把 D8 从 5.5 推向 8 | 1~2 天（含一次真实四方审批） |
| 二 | G7 剩余 4 条的书面裁决：`syncUsageAdmin` 的 ctx 语义、溢出预算随 `maxTracked` 缩放、rc.1 根因、能力选择的浏览器验收与真实 Provider 证据 | **G7** | 取决于拍板，本身不是工程量 |

两件都做完，加权分 7.35 不再被封顶，判定按 rubric §6 落在
"≥ 6.5 且 G1~G9 全过 = **有条件 Go**"。
