# Halro v1.0.0 放行评审报告（2026-08-11）

> 本文是 [review-plan.md](review-plan.md) §8 规定的报告，评审后不再改动。
> 整改活对照表见 [progress.md](progress.md)，继承台账见 [carry-forward.md](carry-forward.md)。
> 评分体系见 [scoring-rubric.md](scoring-rubric.md)，角色提示词见 [role-prompts.md](role-prompts.md)。
>
> 评审 HEAD：`2cd24a76a569fe53f878c1ab1be31441f4c008e0`（`2cd24a7`，2026-08-11 14:46:21 +0800）
> 基线：`33bc13b`。区间：46 提交（非 merge 36 + merge 10），193 文件、+22,648 / −4,496（V4 复核数字，与方案 §2.1 一致）
> 汇总执行环境：go1.26.5 darwin/arm64，macOS 26.6

---

## 1. 放行判定

# **No-Go**

**封顶前加权总分 6.2 / 10；封顶后最终分 6.2 / 10**（另见 §2.3：G1 的宽读法下最终分为 5.0）。
按 [scoring-rubric.md](scoring-rubric.md) §6 的映射，`< 6.5` 在任何 G 状态下都判 **No-Go**。

### 1.1 为什么是 No-Go

三条互相独立、任一单独成立即足以否定：

1. **G4 未通过，且从未被执行过一次。** 发布流水线的门禁部分被证明有效（rc.1 没有误发一个 Release），但交付部分从未闭环：`gh release list` 为空、release workflow 幸存 run 数为 0、`v1-release` Environment 与 `M11_RELEASE_EVIDENCE_JSON` secret 今天都 `total_count: 0`。**今天打一个 tag，publish 必然仍然失败**（V5 §4.3 独立复核）。且即便有产物，三处面向使用者的文档都不给 `--certificate-identity`，cosign keyless 模式强制要求它——外部使用者验不了签（B3-04，V5 §1.2 复现）。按 rubric §5，硬性判据未过即**不得出具 Go**。
2. **G2 有一条反例。** 五条最高优先级不变量中，"fail-closed" 一条不是"缺证据"，而是**有被新代码破坏的正面证据**：V1 以独立探针实证，拓扑提交协议自己的恢复循环会在 ≤5.5 秒内把 stale 拒流清掉，而它并不修复 redaction / Token Guard 快照；查不到策略时 redaction 原文放行、Token Guard 无条件 Allowed——不是"旧策略继续跑"，是**整条策略旁路**。这正是该机制声称要关死的方向。
3. **公告面成体系地缺席。** 本区间 22.5k 行改动，CHANGELOG 零提交（V4 CONFIRMED）；已知限制清单未进发布说明（G8）；rc.1 → 1.0.0 事实上无升级路径且三处断裂无一公告，其中两处失败方式把版本不兼容呈现为密钥/篡改问题，且**排查动作本身具破坏性**（用 1.0.0 试启动一次会把 bbolt 单向迁到 v27，退回 rc.1 二进制也打不开了，V4 §3）。1.0.0 的 tag 一打，写漏的合同就是永久合同。

### 1.2 为什么不是"有条件 Go"

有条件 Go 要求总分 ≥ 6.5 或 G1~G9 全过。两条都不满足。更实质的理由是：**上面三条里没有一条能用运维手段绕过**——运维不能自己发一个 Release，不能自己写发布说明的已知限制，也不能在不改代码的前提下阻止恢复循环把 fail-closed 反转成 fail-open。

### 1.3 最短关闭路径（按"阻塞项 × 关闭成本"排序）

先做这三件，状态可从 No-Go 翻为 Go 或有条件 Go：

| 顺序 | 做什么 | 关掉哪些判据 | 成本 |
|---|---|---|---|
| **一** | **修 stale 跨域清除**（R-03）：stale 按域分维度、`markCurrent(domain)` 只清本域、恢复循环重放全部四个域；补 3 条负面测试（A4 的 T-1/T-2/T-3）。同时给 `halro_activation_stale` / `_stale_seconds` 两个 gauge + 一条 critical 告警 + 一条短 runbook + 控制台 `activation` 字段（R-04） | **G2**（fail-closed 反例消失）、**G1 宽读**（唯一 CONFIRMED 的代码级 fail-open 关闭）、D1/D4/D7/D6 四维同时提分 | 代码半天~一天；观测面半天 |
| **二** | **打通发布流水线**（R-01、R-02）：建 `v1-release` Environment + required reviewers、把 secret 设为 environment secret（不是 repo secret）；**先修可复现构建**（B3-03：`Date` 改用 tag 的 committer date、`tar` 加 `--sort/--mtime` + `gzip -n`），否则 evidence bundle 与单次 run 锁死、只能 `gh run rerun --failed` 且受 artifact 90 天保留期限制；打 rc.2 走通一次 publish 产出 Release；在发布说明与 README 补完整的 `cosign verify-blob` 命令块 | **G4** | 1~2 天（含一次真实四方审批） |
| **三** | **补齐公告与文档三件套**：① 发布说明写入 §8 的已知限制清单（**G8**）+ "从 rc.1 升级：无路径，必须重建实例"一节；② CHANGELOG 补迁移 24~27、`Idempotency-Key` 必填、`allow_private_provider_endpoints` 生效三条 Operator impact；③ `operator-guide.md:497` 一行 + 容器小节补 `init` 步骤、3 条 m11 runbook 顶部加 release-blocked 标注、修 `catalog/unsigned-snapshot-v1.example.json:11` 的 `profile_id`、`gateway-key-compromise.md:103` 命令补 `--config` | **G6**、**G8**，并解除 B4-F1/F2、B2-01、B1-01 的阻塞标注 | **小时级，纯文档** |

做完这三件之后仍未关闭的只剩 **G7**——17 条"仍开"条目的"接受 / 修复 / 写入已知限制"三选一书面裁决。本报告 §9 已给出逐条**建议裁决**，但其中 6 条是产品取舍，必须由用户拍板，评审组不能代做。G3 / G5 / G9 已通过。

**注意第三件事的成本结构**：它是三件里唯一"小时级"的，却单独卡住两条硬性判据。如果只做一件事，做它。

---

## 2. 八维度发布就绪度评分

### 2.1 维度分（[scoring-rubric.md](scoring-rubric.md) §7 规定格式）

| 维度 | 权重 | 得分 | 封顶后 | 主要扣分项（带 file:line） | 要到 8 分还差什么 |
|---|---:|---:|---:|---|---|
| D1 安全性 | 0.20 | 6.5 | 6.5 | V1 CONFIRMED：`internal/app/activation_state.go:52-58` 无条件清空 stale，导致 `internal/redaction/engine.go:428-431` 与 `internal/tokenguard/manager.go:308-312` 两处 lookup-miss fail-open 被打开（探针实证 `SECRET-12345` 明文穿透、Token Guard `allowed=true`）；`internal/app/admin_invocation_targets.go:196` 的 GET 侧信道无 CSRF/同源/角色门即可花费 Provider 凭据；`docs/verification/security-review-v1.md:62` 自称 blocked 仍成立且覆盖面落后 469 提交，`internal/kms/awskms/adapter.go:31` 的 `LoadDefaultConfig` 出现在该文档点名"需要新威胁评审"的路径上而评审未发生；`internal/adminauth/password.go:16` 每并发登录 64.2 MiB 无信号量（实测 64 并发 4.13 GiB） | ① 关闭 R-03（stale 按域分维度）并把两处 lookup-miss 的 fail-open 方向一并裁决；② `resolve.describe` 纳入 `requireAdministratorRole` + 同源校验（R-24）；③ 对本区间新增的 6 个子系统（kms / masterkey / hostsecurity / bearercred / modelcatalog / bedrockmantle）补一次增量安全评审，或在 `security-review-v1.md` 显式声明覆盖边界；④ argon2 加全局信号量（R-13） |
| D4 逻辑正确性 | 0.20 | 6.5 | 6.5 | V1 CONFIRMED 的跨域 stale 清除与恢复循环"治好症状不治病因"（`internal/app/activation_state.go:107-131` 只重放 topology + auth）；`runActivationRecovery` 全仓零测试覆盖；四个真实 `markStale` 调用点无一被测试触达（`internal/app/commit_protocol_test.go:64,87` 是全仓仅有的两处，均为直接注入）；V6 §5.2：`internal/safetransport/transport.go:139,151,155` 三个"本方保证零字节发出"的拒绝点返回裸 `fmt.Errorf`，`provider.Unsent`（`internal/provider/provider.go:77-87`）认不出，按 ambiguous 满额结算并抑制 failover；`internal/store/bolt/model_capability_detection.go:169-197` 在 `ForEach` 内 `Put`，违反 bbolt 明文游标契约且与全仓其余同类代码写法相悖 | ① R-03 + A4 的 T-1/T-2/T-3 三条从调用点出发的负面测试；② T-6（跨 `Open()` 的 intent drain 与失败拒启）、T-5（三个 create 端点的必填/重放断言）；③ V6 §5.2 的类型化错误 + 负面测试（fake-IP → denied 地址 → 结算必须为 0）；④ `ForEach` 内 mutation 改为"先收集后写入"并补含数百条在途探测的恢复用例 |
| D2 系统设计 | 0.12 | 7.0 | 7.0 | `activationTracker`（`internal/app/activation_state.go:31-36`）用**一个**标志表达**四个**独立激活域的状态——这是设计层面的错误，不只是 bug；`internal/app/runtime.go:47-128` Runtime 68 个字段、跨 admin/非 admin 边界符号从约 11 个升到约 27 个（≈2.5×）、`adminTopologyMu` 已越出 admin 边界（`activation_state.go:118`）；审计意图机制四代构造并存（`domain.AdminAuditIntent` / `AdminMFAAuditIntent` / `bucketPricingAuditIntents` / `keyKeySlotAuditIntent`）且 `internal/app/admin_users.go:82-93` 的身份类 mutation 仍在 fire-and-forget 路径 | ① stale 按域建模（R-03 的设计部分）；② 1.0.0 后第一个间隔做 internal/app 的书面二选一，并停止引用 260807 已过期的边界表；③ 把 `admin_user.create/delete` 迁到 `AdminAuditIntent`；④ 四处内联幂等校验改调 `adminCreateIdempotencyKey` 助手 |
| D7 可运维性 | 0.12 | 6.5 | 6.5 | `configuration_stale` 是一个会让整台实例对全部 Project 返回 503 的状态，却**同时**没有 runbook（`docs/runbooks/` 零覆盖）、没有指标（`grep halro_.*stale internal/ docs/contracts/` 零命中）、没有告警（`deploy/observability/prometheus/alert-rules.yml` 24 条无一涉及）、控制台不可见（`web/src/types.ts:821-832` 无 `activation` 字段）；本区间 10 个新指标对应 0 条告警 / 0 条 rule test / 0 条 runbook 条目；5 条平台告警无 `runbook_url`（其中 3 条 critical）；`docs/observability/operations-runbook.md` 全 125 行无任何按 alertname 的小节；`docs/runbooks/` 无 file 模式 Master Key 轮换 runbook | ① R-04（指标 + 告警 + runbook + 控制台四件套一起关）；② capability / signed-catalog / detection 至少 3 条告警并进 `rule-tests.yml`；③ `operations-runbook.md` 增"按告警"一节，`runbook_url` 改带锚点并加 CI 断言；④ 三条 m11 runbook 顶部加 release-blocked 标注 |
| D8 可交付性 | 0.12 | **4.0** | **4.0** | 从未产出过任何 Release（`gh release list` 空）；release workflow 幸存 run 数 0；`v1-release` Environment 与 repo secrets 今天 `total_count: 0`；外部验签参数只存在于 `release.yml:297-298` 的内部自验里；构建不可复现（`release.yml:153` 的 `date -u`、`:165` 的 `tar -czf` 缺 `gzip -n`，实测 A==B、A≠C）；`docs/verification/dependency-license-review.md:10` 称 5 个直接 Go 依赖，实际 12 个，整个 AWS KMS 凭据面从未被 license review 覆盖；`deploy/kubernetes/halro-aws-kms.yaml:35` 是占位镜像且无任何 registry 推送；CHANGELOG 区间零提交；`docs/verification/crash-recovery-matrix.md:21` 为一个已被 `d0bb2b8` 删除的测试断言 Pass | ① R-01 + R-02（走通一次端到端并把外部验签命令写进发布说明）；② 修可复现构建，解开 evidence bundle 与单次 run 的锁死；③ 重新生成 license review 并加 CI 门禁（依赖文件变更而文档未变即失败）；④ registry 推送或把 k8s 清单从交付面撤下并在已知限制写明；⑤ CHANGELOG + 发布说明补齐（R-05/R-06/R-07） |
| D6 可用性 | 0.10 | 6.0 | 6.0 | `docs/guides/operator-guide.md:497` 与 `README.md:81-83` / `backup-restore.md:169-197` 直接矛盾，照容器小节做 100% 起不来（V2 逐字复现，含同一 digest）；`-p` 发布的端口不可达而 `docker ps` 报 healthy（`Dockerfile:33-35` 的 HEALTHCHECK 走容器内回环）；`halro doctor` 把父目录不可写报成"数据目录正被占用"并把人送去找不存在的持有者；`internal/app/admin_onboarding.go:202-208,254-256,282-284` 只认探针证据，`halro bootstrap` 建成的可用链被报成 0/4 且文案说"模型未发布"；五个创建入口撞 409 幂等重放全部退化成"数据被别人改了，刷新重试"，且服务端英文原句直接出现在 zh-CN 界面（`web/src/i18n/errors.ts:53-56`）；stale 在控制台完全不可见 | ① R-08/R-18（operator-guide 一行 + init 步骤）、R-09（TLS 或 `-allow-insecure-public-listen` 二选一写清）、R-17（`data_lock` 区分 EACCES 与 EWOULDBLOCK）；② R-10（把"该 Route 上出现过成功请求"作为 publish/access 的充分证据）；③ R-11（五个 `*_idempotency_replay` code 各一条 i18n 条目 + "去看那条记录"操作，Gateway Key 单独一句更强提示）；④ R-04 的控制台部分 |
| D5 性能与容量 | 0.08 | 6.5 | 6.5 | `internal/adminauth/password.go:16` 每并发登录 64.2 MiB、完全线性、无上界，唯一控制点是 per-source-per-minute 的 `login_rpm`（`internal/config/default.go:46`），实测 16 并发即 1.06 GiB 越过 k8s 的 1Gi 兜底，裸机/Docker 默认无 limit，且高水位 4 分钟不归还；测试环境是 macOS/APFS（`F_FULLFSYNC`），绝对值是地板不是天花板；`docs/verification/performance-baseline.md:105-107` 的 `release_24h` 工件仍未归档 | ① R-13（argon2 全局信号量或进程级 in-flight-hash 上界）；② 在 Linux/NVMe + 生产构建标志下复跑一次并归档；③ 跑满 24h soak 并在 GA commit 上归档 `release_24h`；④ 把 30s 优雅关闭预算做成配置项或写进已知限制（R-30） |
| D3 工程规范 | 0.06 | 6.5 | 6.5 | `docs/contracts/idempotency-contract.md:3-4` 称 Idempotency-Key 是 "optional…Clients that omit it retain existing behavior"，与 Admin 面必填直接矛盾——这份文档现在是**主动误导**；`docs/contracts/metrics-reference.md` 缺 3 个已导出的 `halro_audit_anchor_*` 族，且 `internal/app/metrics_contract_test.go:82-89` 的门禁对条件导出**结构上是盲的**（只检查本次渲染出现过的族）；`internal/app/metrics.go:163-167` 与 `metrics-reference.md:169-172` 各点名要求一条告警，两条在 `alert-rules.yml` 中都不存在；chat/embeddings 两份 manifest 漏报 unknown-field 拒收（实测 `frequency_penalty` 等 7 个官方参数一律 400）；提交协议语义只在 `docs/architecture/provider-to-project-api-call-chain.zh-CN.md:117-127`（仅中文），自称 normative 的 `gateway-correctness.md` 零提及 | ① 修 `idempotency-contract.md`（把 optional 限定到数据面，另立 Admin 面一节）；② 补 3 个 anchor 族并把门禁改成"静态清单 ⊆ 文档"；③ C2-04 的两条告警"要么补规则、要么把措辞降级"，不要并存；④ 两份 manifest 加 unknown-field deviation 并 `HALRO_UPDATE_GOLDEN=1` 重生成；⑤ stale/503 语义进 `gateway-correctness.md` |
| **加权总分** | **1.00** | **6.21** | **6.21** | 触发的封顶规则：**G2/G3(见 §3)/G4/G6/G7/G8 未通过 → 总分封顶 6.5**（rubric §5 第 2 行）。加权分 6.21 已低于 6.5，**封顶不咬合**，最终分 = 加权分 | — |

**gofmt 与 vet 说明**：A6 实跑 `gofmt -l .` 与 `go vet ./internal/app/ ./internal/modelcatalog/` 均干净；本轮汇总的 `make check` 包含 `fmt-check` 与 `go vet ./...`，结果见 §3 的 G3。

### 2.2 两个数字的差距是信息

加权分 6.21 与封顶值 6.5 只差 0.29，**说明封顶规则几乎没起作用——问题是弥散的，不是卡在少数几点上**。八个维度里没有一个到 8，最高的 D2 是 7.0，最低的 D8 是 4.0，六个维度落在 6.0~6.5 的窄带里。这与 260805 的 6.5~7.5 区间对照，符合 rubric §6 那句提醒的反面：**分数没有升，而中间隔着 22.5k 行新代码和三轮整改——新增面把整改的收益吃掉了，并且吃掉的方式是可指认的**：本区间新建的三大子系统（拓扑提交协议、能力探测、签名模型目录）在代码正确性上做得相当克制（A5 全仓找不到 P0/P1），但它们的**测试、监控、运维、交互、契约文档五个配套面几乎整体缺席**——十个新指标零告警、stale 零测试零指标零 runbook 零控制台、新语义零英文契约、新错误码零 i18n。

### 2.3 逐角色评分（与维度分并存，不互相推导）

| 角色 | 模型 | 自评 | 汇总校正 | 校正理由 |
|---|---|---:|---:|---|
| 第 0 步 继承核对 | Fable 5 | 7.0 | 7.0 | — |
| A1 核心逻辑与记账不变量 | Fable 5 | 6.5 | 6.5 | V1 CONFIRMED 其头号发现，评分成立 |
| A2 安全 · 信任边界与凭据链 | Fable 5 | 7.5 | 7.0 | B3-08(iii-附) 转交的 `LoadDefaultConfig` 未经威胁评审一条，A2 未覆盖 |
| A3 安全 · 授权与状态完整性 | Fable 5 | 7.0 | 7.0 | 头号发现被 V3 判 REFUTED（降 P3），但其三条下限的方法论正确、正面结论（两阀无法组合越权）经 V3 独立确认为真，分数不动 |
| A4 测试盲区 | Opus 5 | 6.5 | 6.5 | 探针实证质量高，V1 全盘确认并补全了 A4 自述"推理"的那一半 |
| A5 BUG 排查 | Fable 5 | 7.5 | 7.0 | 全仓未发现 P0/P1 是可接受的诚实结论，但 A5 明确把 stale 归口给 A1 而未独立复核，恰好错过本轮最重的一条 |
| A6 架构设计与工程规范 | Fable 5 | 7.5 | 7.0 | `activationTracker` 单标志是 D2 范畴的设计错误，A6 的分层扫描未覆盖状态建模层 |
| B1 可用性与开箱 | Opus 5 | 4.0 | 5.5 | V2 把其唯一 P0 降为 P1 并证伪"四份工件同源"（3/4 被推翻）；但实跑质量与卡壳记录扎实，且它是唯一真正把产品从零跑到 200 的角色 |
| B2 API 兼容性契约 | Fable 5 | 7.0 | 7.0 | — |
| B3 供应链与发布工程 | Opus 5 | 4.0 | 4.0 | V5 判 PARTIAL：结论方向站得住、取证论证不站得住（两条论证必须重写），分档不改 |
| B4 数据迁移与升级 | Fable 5 | 5.5 | 5.5 | V4 两条全 CONFIRMED 且原判漏了加重情节，档位准确 |
| C1 运维就绪与 Runbook 演练 | Opus 5 | 7.5 | 7.5 | G5 证据是本轮质量最高的一份实跑 |
| C2 可观测性与告警 | Opus 5 | 6.0 | 6.0 | — |
| C3 性能与容量 | Fable 5 | 7.0 | 6.5 | argon2 P1 无书面绕过，按锚点应落 5~6 上沿而非 7 |
| D1 交互设计 | Sonnet | 6.0 | 6.0 | — |
| D2 视觉设计与整改验收 | Sonnet | 8.0 | 8.0 | 本轮唯一到 8 的角色，两类独立复核（grep + vitest）实跑 |

**模型分布**：Fable 5 承担 9 个（继承核对、A1、A2、A3、A5、A6、B2、B4、C3）+ 对抗验证 3 个（V1、V4、V6）；Opus 5 承担 5 个（A4、B1、B3、C1、C2）+ 对抗验证 3 个（V2、V3、V5）；Sonnet 承担 2 个（D1、D2）。
**拒答情况：全轮零拒答**——21 份产出（15 角色 + 继承核对 + 6 裁决，其中 A2/A3 为两份独立报告）每份都有 `file:line`（最少 18 处，最多 114 处）、模型标注、附录三件套（读过的文件 / 运行过的命令 / 无法验证项），无一份需要作废重跑。§9 的空结论真伪核对已全量执行并通过。

---

## 3. G1~G9 逐条判定

| # | 判据 | 判定 | 依据 |
|---|---|---|---|
| **G1** | 经对抗验证 CONFIRMED 的 P0 数量为 0 | **通过（窄读）／不通过（宽读）** | 见 §3.1 的裁断 |
| **G2** | 五条最高优先级不变量各有至少一条本轮证据支持其未被新代码破坏 | **不通过** | 四条有支持证据，**fail-closed 一条有反例**。见 §3.2 |
| **G3** | `make check` 全绿 + `-race` 覆盖本轮改动包 + `internal/webui/dist` 干净 + CI 红灯被查过 | **通过** | 见 §3.3（本轮汇总实跑） |
| **G4** | 发布流水线端到端产出一次可被外部使用者独立验签的 artifact，且 rc.1 publish 根因已关闭 | **不通过** | 两个合取项均为假且互相独立。见 §3.4 |
| **G5** | 备份→恢复→启动→账目一致，实跑一次并留证据 | **通过** | C1 §5.3：ledger head / chain hash / WAL sha256 / Usage 记录数四项三态比对逐字节吻合，四条归档异常路径全部 fail-closed |
| **G6** | 每条 runbook 实操走通，或明确标注"当前不可用"并从发布说明中撤下 | **不通过（采严格口径）** | 见 §3.5 的裁断 |
| **G7** | 260807 的 8 条 + 继承台账全部"仍开"条目，每条有"接受/修复/写入已知限制"三选一书面裁决 | **未通过（本报告提供建议裁决草案）** | 17 条"仍开"当前无一条有书面裁决。见 §9 |
| **G8** | 已知限制清单已写入 `docs/milestones/release-notes-v1.0.0.md` | **未完成（清单已备好）** | 按边界要求本轮不修改发布说明；清单见 §8，共 18 条，可直接采用 |
| **G9** | 容量基线取得实测数字（Linux/NVMe，或书面说明替代环境及其偏差），含长跑内存观测 | **通过** | 见 §3.6 的裁断 |

**软性判据**：S1（真实 Provider 证据进 `provider-real-matrix.md`）未做，按纪律不跑计费 smoke，需书面接受；S2（浏览器验收）已于 2026-08-10 完成 fixture 本地 RC（carry-forward §4 #5），真实 Provider 部分仍是精确 RC commit 的外部门禁；S3（260810 视觉 P1 三项）**已全部整改完毕并有零容忍测试守护**（D2 实跑复核，116 处 → 0 处）；S4（P1 全部修复）未达成，本报告 §6 逐条给出绕过方案或标注"无绕过"。

### 3.1 G1 的裁断：P0 是否包含发布门禁类

**事实基础**：本轮共 5 条被标为 P0 的发现。

| 发现 | 性质 | 对抗验证 | 现状 |
|---|---|---|---|
| B1-01 容器/K8s/systemd 部署布局全部无法启动 | 代码/工件缺陷 | **V2 → PARTIAL，降为 P1** | 缺陷真实、100% 复现，但爆炸半径高估 4 倍：只有 `operator-guide.md:497` 一句话错，Dockerfile/systemd/k8s 三项被证伪 |
| B3-01 rc.1 publish 未产出 Release 的根因 | 发布门禁 | **V5 → PARTIAL** | 结论方向成立（两个前置条件今天可验证地缺失），但"根因已查明"高估约一档；V5 明言"P0 存续" |
| B3-02 rc.1 的 release run 已被删除，证据链断裂 | 证据链 | 未单独对抗（V5 复核为真） | 需裁决 |
| B3-03 构建不可复现，与 M11 证据门禁互相锁死 | 发布工程 | V5 §1.3 复核事实层为真，但判定它**不属于 G4** | P0 标签未被降级 |
| B1-09 连接失败满额结算（B1 标 P2，此处不计） | — | V6 → PARTIAL，降 P3 | — |

**裁断：G1 的"P0"指代码缺陷类 P0，不含发布门禁类 P0。** 三条理由：

1. **避免同一件事被两道判据重复否决并重复封顶。** G4 就是为发布流水线设的判据，且 rubric §5 的封顶规则对 "CONFIRMED P0" 和 "硬性判据未过" 各有一条独立的封顶。若 G1 也吃发布门禁类 P0，同一组事实会同时触发 D8 封顶 4 + 总分封顶 5.0 和 总分封顶 6.5——这不是更严格，是把一个结论算了两遍。
2. **G1 的判定方式栏写的是"对抗验证章节"**（review-plan §7.1）。对抗验证的对象定义在 §6：对**最严重发现**在代码里复现完整路径或找出已有拦截防御。发布门禁的"缺陷"不在代码里，它在 GitHub 仓库设置里，不适用这套方法论——V5 自己也是这么做的（它验的是 API 事实与论证有效性，不是代码路径）。
3. **G1 的功能是防"带着一个已确认的代码 P0 上线"**。而发布门禁的 P0 恰恰**阻止**上线——它不是"带着 X 发出去"的风险，它是"发不出去"。两者在放行评审里的语义相反。

**两种读法下的结论都写出来**：

- **窄读（本报告采用）**：CONFIRMED 的代码缺陷 P0 数量 = **0**（唯一候选 B1-01 已被 V2 降为 P1）。**G1 通过。** 封顶不触发 5.0，最终分 **6.21**。
- **宽读**：B3-01（V5 明言 P0 存续）与 B3-03（事实层复核为真、P0 标签未降）计入，CONFIRMED P0 数量 = **2**。**G1 不通过**，且 D8 存在 CONFIRMED P0 → D8 封顶 4（其得分本就是 4.0，不变）、**总分封顶 5.0**，最终分 **5.0**。

**这个裁断不影响放行结论**——两种读法下判定都是 No-Go（G4 独立否决，且 6.21 与 5.0 都 < 6.5）。它只影响记录在案的分数。若用户倾向宽读，把最终分改记为 5.0 即可，报告其余部分不变。

### 3.2 G2 的逐条判定

| 不变量 | 判定 | 本轮证据 |
|---|---|---|
| 单写者、单数据目录 | **未被破坏** | C1 实跑：运行时在跑时 `halro key disable` / `doctor` 均被 `data directory is already locked by another process` 挡住（`internal/app/keys.go:55-59`）；V2 复核发布锁机制 `internal/store/lock/lock_unix.go:62-65` 无旁路、KMS 路径同走该锁（`internal/app/kms_master_key.go:414`） |
| Ledger 为记账权威 | **未被破坏** | A1 §5.2 按代码时序独立走查 `internal/budget/manager.go` 并 `go test -race ./internal/budget/` 通过（含 64 worker 与注入 durability 故障的负面测试）；C1 §5.3 恢复后 ledger head / chain hash / WAL sha256 逐字节回到备份点；V6 复核 `EventAttemptSettled` 同事件释放预约并提交成本，且 `halro_cost_usd_total` 从 Ledger 重放 |
| **fail-closed** | **✗ 有反例** | **V1 CONFIRMED**：`internal/app/activation_state.go:107-131` 的恢复循环在 ≤5.5s 内清除它并不修复的 stale（探针实证 `{Stale:false Generation:1}`、`/health/ready` 200），而 `internal/redaction/engine.go:428-431` 与 `internal/tokenguard/manager.go:308-312` 在查不到策略时 fail-open（探针实证 `out="here is SECRET-12345 in a prompt"`、`allowed=true`）。这是**本区间新代码引入的机制自己**把 fail-closed 反转成 fail-open |
| 无秘密泄漏 | **未被破坏（有一处需与上一行合看）** | A2：审计只记 reason class、`model_sha256`、`publicCapabilityDetection` 逐字段剥离；C1：轮换只印指纹与计数；D2：生产 bundle 无字面量密钥。**但** V1 的探针显示，stale 被误清后绑定该策略的项目其 PII/secret 规则一条都不跑——这不是"秘密进日志"，是"秘密该被脱敏而没被脱敏"，归 fail-closed 那一行 |
| 重放确定性 | **未被破坏** | A1 §5.1 四个崩溃窗口逐一走查，审计重投递按 EventID 去重且负载不一致即报错（`internal/audit/log.go:217-222`）；创建 ID 由幂等键派生（`internal/app/admin_create_idempotency.go:47-56`）；B4 实证迁移链在单个 bbolt 写事务内跑完（`internal/store/bolt/store.go:1131-1209`）；A1 探测调用崩溃收口为 UNKNOWN、绝不重放 |

**G2 不通过。** 判据要求的是"各有至少一条证据支持其**未被**破坏"，fail-closed 一条不是缺证据而是有反证。关闭路径 = R-03（本报告最短关闭路径第一件事）。

### 3.3 G3 的实跑证据

<!-- MAKE_CHECK_PLACEHOLDER -->

### 3.4 G4 的判定

**不通过。** 两个合取项互相独立地为假（V5 §4 独立复核，未采信 B3 转述）：

- **合取项 A（产出过一次可被外部独立验签的 artifact）：否，且是被观测到的否。** `gh release list` 空；`gh api .../actions/workflows/324475184/runs --jq .total_count` = 0；rc.1 的产物随 run 删除。叠加：即便有产物，`docs/milestones/release-notes-v1.0.0.md:129`、`docs/guides/operator-guide.md:427`、`docs/guides/releasing.md:44` 三处都只说"要验"不给命令，而 cosign keyless 强制要求 `--certificate-identity`（V5 独立复现报错）。两半都不成立。
- **合取项 B（rc.1 根因已关闭）：否，且不依赖"7 秒断在哪一步"这个争议。** `gh api .../environments` 与 `.../actions/secrets` 今天都 `total_count: 0`，而 `release.yml:279-280` 是 `set -euo pipefail` 后紧跟 `test -n "${M11_RELEASE_EVIDENCE_JSON}"`——**今天打一个新 tag，publish 必然仍然失败**。"根因已关闭"要求关闭，而现状是连根因的具体断点都还没查明（run 已删）。

**判"不通过"而不是"无法验证"**（V5 §5 的表态，本报告采纳）：G4 是一个"正向成就"判据，问的是"是否已经产出过"，答案是可观测的**否**——零 Release、零幸存 run、零 artifact。这是被观测到的否定事实，不是证据空洞。把"东西不存在"记成"没查清"，等于把系统的缺陷记到评审头上。**确实"无法验证"的是一个更窄的问题**：rc.1 publish 究竟断在哪一个 step（run `31131173718` 已 404，日志/artifact/check run 全部不可得）。G4 不依赖它。

**同时记录 B3 的两条论证必须重写**（V5 §2.2、§2.3）：删掉"无 `waiting` 状态 ⇒ 没配审批人"（`waiting` 不是 deployment status 的合法取值，该论证从一个不可能出现的状态的缺席推结论，无效）与"7 秒 = 失败在 `test -n` 第一行"（`test -n` 是第四个 step 里的第一行，前面有 checkout + 下载 80~100MB artifact + 装 cosign；且 B3 的前提"三个 step 跑不完 7 秒"推翻的正是它支撑的结论）；替换为 `created_at 23:35:08Z → in_progress 23:35:12Z` **间隔 4 秒**（四方 reviewer 下载 release-assets、审 SBOM/checksum/Sigstore、设 secret、批准环境的整套动作不可能在 4 秒内完成）与"两个前置条件今天可验证地缺失"。另把 B3-03（不可复现构建）从 G4 判定表移出——G4 说的是"验签"，不要求第三方逐字节重建产物，那是供应链纵深的另一个属性。剥掉之后 G4 判定不变。

### 3.5 G6 的裁断：严格口径还是宽口径

C1 把这条口径判断明确交给汇总方。**本报告采严格口径，G6 判不通过。** 理由三条：

1. **判据第二分支的措辞是"明确标注'当前不可用'**并**从发布说明中撤下"——两个动作，不是二选一。** 发布说明的 `release-notes-v1.0.0.md:98-102` 确实写了 release-blocked（撤下这一半成立），但三条 `m11-*.md` 文件本身没有任何标注。**这条判据要防的失败模式是"运维照着一份走不通的 runbook 操作"**，而运维的入口是 `docs/runbooks/`，不是发布说明。标注不在事故中会被读到的位置，等于没标注。
2. **非 KMS 的两条里有一条实操没走通到底。** `model-catalog-publishing.md` 的 prepare → 签名 → assemble 全过，`verify` 失败：仓库唯一的示例文件 `catalog/unsigned-snapshot-v1.example.json:11` 写的 `profile_id: "openai_chat_embeddings"` 不是注册值（注册值是 `openai.chat-embeddings.v1`，`internal/domain/provider_profile.go:27`）。拦下来本身是对的（fail-closed），但"照着仓库给的模板做一遍"这件事走不通。
3. **走通的那一条也在实跑中打偏了。** `gateway-key-compromise.md:103` 的 `halro key disable --key-id <keyID>` 缺 `--config`，C1 在仓库根目录按原文执行，命令加载了**另一份** `config.yaml` 并返回 `load gateway key: record not found`——事故中这句话会被读成"这把 key 已经处置过了"。这与该 runbook 自己 §1:21 警告的误判是同一类。

**宽口径下的结论也写出来**：若接受"发布说明已声明 release-blocked 即视为撤下"，则 G6 判**有条件通过**，关闭条件为 C1 §5.5 列的三条（均为小时级文档改动）。

**这个裁断不影响放行结论**——G4 已独立否决。它影响的是最短关闭路径第三件事的必做项清单：严格口径下那三条文档改动是**必做**，宽口径下是建议。鉴于成本是小时级，本报告按必做处理。

### 3.6 G9 的裁断：darwin 上的实测数字算不算"取得实测数字"

**算，G9 通过。** 判据原文是"容量基线取得实测数字（**Linux/NVMe，或书面说明替代环境及其偏差**）"——括号里的第二分支就是为这种情况写的，C3 §2 是一份合格的书面说明：

- 环境逐项列出（Apple M4 Pro / darwin-arm64 / 14 核 / 64 GiB / APFS / `F_FULLFSYNC`）；
- 偏差方向与量级明确（`F_FULLFSYNC` 比 Linux NVMe 的 `fdatasync` 贵 1~2 个数量级，**所有吞吐绝对值是地板不是天花板**）；
- 明确区分了"可迁移的是形状，不可迁移的是数字"（扁平 vs 随并发扩展、线性内存放大、协议前后相对差跨主机成立）；
- 与仓库既有文档 `docs/verification/standalone-capacity-baseline.md:41-43` 的口径一致，不是临时找的借口。

三条下限全部取得数字，非推断：账目 1223 lifecycles/s @ 64 并发且随并发而非项目数扩展（验证 ADR 0018 的形状）；长跑 2588 次真实 HTTP 变更后 RSS +1.4 MiB 并在 ~900 次后持平、`go_goroutines` 稳定 18（**无内存泄漏、无 goroutine 泄漏**）；拓扑提交协议使管理写路径吞吐降 25.3%（41.69→31.13 mut/s，p=0.008），且是**两侧都构建、同 harness 同输入**的反向验证而非从 diff 推断——这一条正是 CLAUDE.md "要归因就把两侧都建出来跑"的正确做法。argon2 内存放大也是实测复现（每并发精确 64.2 MiB，16/32/64 并发分别 1.06/2.08/4.13 GiB）。

**同时明确 G9 不覆盖的**：Linux/NVMe 的发布级绝对数字、24h soak 的 `release_24h` 工件归档——这两项属于 B3-08 的 soak 门与 S 级软判据，不在 G9 内。它们仍开。

---

## 4. 各角色分章

> 每章给"这个视角看到的系统怎么样"的一句话结论 + 该角色的下限项应答摘要。完整证据见 `findings/` 下对应文件。冲突处已按裁决改写并标注"原判 X → 裁决后 Y"。

### 4.0 第 0 步 · 继承核对（Fable 5 · 7.0）

35 条历史结论重核：**已关闭 18 / 仍开 17 / 无法验证 0**。260811 API 链路的 7 条 finding 与 3 项子项**全部既有修复提交、又有具名回归测试**，实跑全绿——"已修但无守护"表为空，另列 3 处守护覆盖不完整的边角供 A4。260810 视觉 8 条全部关闭，12px 下限声明数 116 → 0 且有零容忍测试守护。仍开的 17 条全部来自 260807 遗留（8 条原样未动）与 260809 方案 §5（9 项，因该轮未执行而无一有结论记录）。

**汇总修正**：§7 表中 F-03 "mutation/激活/审计三阶段无统一提交点 → 已关闭（有守护）" 需**部分撤销**——见 §9.2 第 1 条。

### 4.1 A1 · 核心逻辑与记账不变量（Fable 5 · 6.5）

**记账链成立，拓扑链有一个真实缺口。** ADR 0018 的额度不变量经独立走查与 `-race` 实跑证实：额度在任一瞬间被记在 `pendingAdmitted` 或 `state.Balance` 之一、绝不两处皆无，失败路径与终态均 fail-closed，且有真负面测试（`TestFailedAppendReleasesAdmittedSpend` 注入 durability 故障）。拓扑提交协议在四个崩溃窗口下都成立，审计意图同事务落盘、重投递幂等。**缺口是 A1-01（P1）**：四个激活域共享一个 stale 标志——经 V1 CONFIRMED 且后果比原判更重。

下限应答：① 崩溃点成立、并发成立**但有 A1-01 的例外**；stale 自伤路径面窄、可自愈、外部不可触发，评估为可接受的设计代价。② ADR 0018 额度不变量成立（独立走查，非照 ADR 复核）。③ 能力探测计费调用不进 Ledger **不阻塞 1.0.0**，条件是写入已知限制；理由是 Ledger 的权威范围是"项目的数据面代账"，探测是管理面自发流量、没有 project/key/价格快照归属，记进某个项目是错账而非补账；缺口三重有界（≤12 次/检测、≤2048B 入 16 token 出、RPM 限制 + single-flight）且逐调用有 durable 台账与 Prometheus 计数。**附两个条件**：必须公告；不得以此为先例让未来任何**数据面**调用绕过 Ledger。

### 4.2 A2 · 安全 · 信任边界与凭据链（Fable 5 · 7.5 → 校正 7.0）

**三条新增面总体延续了既有信任边界纪律，未发现 P0/P1。** 新写端点走 `requireAdminMutation`；签名目录全路径 fail-closed（无签名/无可信根/过期/回滚一律拒绝并回落 bundled）；私网开关生效后出网面只多了 RFC1918 + CGN，云元数据（169.254.169.254、100.100.100.200）、环回、保留段（含 6to4/Teredo/NAT64）仍被无条件拦截，且 `pinnedDialContext` 在 dial 时对每个解析地址复跑校验，杜绝 DNS-rebind。两处分层缺口（均 P2、不阻塞）：invocation-target 的两条 GET 侧信道能在只有 SameSite=Strict 单层兜底下花费 Provider 凭据（`resolve.describe` 连角色门都没有，read_only 管理员即可触发上游调用）；能力探测发起花费凭据但无 step-up（与既有 `test` 端点一致，属设计取舍）。

**汇总追加（B3-08 iii-附 转交，A2 未覆盖）**：`docs/verification/security-review-v1.md:58` 写着"未来的 ambient AWS 凭据支持需要一次新的 IMDSv2/默认链威胁评审"，而 `internal/kms/awskms/adapter.go:31` 的 `LoadDefaultConfig` 已经存在于 Master Key 托管路径上（`512d517`，2026-08-03，晚于该评审）。**那次评审没有发生。** 该项列入 §6 的 P1 清单（R-15 的一部分）。

### 4.3 A3 · 安全 · 授权与状态完整性（Fable 5 · 7.0）

**能力体系的核心闸门健壮且 fail-closed。** profile ceiling 在 `reviewCapabilities` 里被无条件、最先检查；源类型（`operator_declared` / `verified_probe`）的豁免只作用于"目录 vs 声明"这一层，绝不作用于 ceiling；目录侧任何来源都被 `Clamp` 到天花板；`setOffered` 只"提供"不"启用"，采纳需一次推进 revision 的管理员 PUT 重过 subset 检查。**下限 #1（两个安全阀能否组合越权）与 #2（快照经备份/热加载/重启被降级）均判否**，且 V3 独立确认这两个正面结论为真。

**A3-01 原判 P2「需裁决」→ 裁决后 P3「建议」，不阻塞。** V3 判 **PARTIAL，安全结论 REFUTED**：管理面事实成立（`isStrictOperationProfile` 确实把 Gemini/Bedrock Converse/Mantle 排除在天花板校验外，超默认能力可以落盘），但**数据面用的天花板根本不是 `binding.Capabilities`**——`gemini.Options` 与 `bedrock.Options` **结构里没有 Capabilities 字段**，天花板不可注入是**类型层面**保证；`deploymentCapabilities` 是逐位 AND 交集（声明只能缩不能扩），且 adapter 硬编码的能力集与 `domain/models.go:500-505` 钉死的 profile 默认**逐位相同**。数据面四道独立闸门（能力交集 / profile 键控兼容性过滤 / 编译期算子表 / adapter translate 期无条件拒绝）拦下越界请求，结果 400 `unsupported_feature` 且**未预留额度、未建立任何上游连接**。A3.md:93-96 那句"下游 provider adapter 以同一 `binding.Capabilities` 构造，无二次兜底"是**事实错误**——`providers.go:580` 的 `capabilities` 变量根本没有被 Gemini/Bedrock Converse 两个分支引用。残余问题性质变为"管理面接受一个永不生效的声明"（可诊断性/误导，P3）。A3 §5 下限 #2 结尾"旧备份里的放宽 binding 会带回软 ceiling"同样 REFUTED。

A3-02（能力声明类审计记录非事务、崩溃可丢，P3）与 A3-03（stale 进出无可信/持久留痕，P3）成立，后者与 C2-01 交叉。

### 4.4 A4 · 测试盲区（Opus 5 · 6.5）

**新门禁在单元与注册表层守护得相当好，盲区高度集中在一处：stale 状态。** 漂移扣留、越界能力拒绝（三层各自独立断言）、签名目录验签（对抗性负面用例）、迁移拒绝（含"拒绝时未改动文件"）、ADR 0018 并发额度都有以缺陷行为命名的负面用例，且 CI 对全树无条件跑 `-race`（`.github/workflows/ci.yml:55`）。但本区间唯一能让整个数据面拒流的状态只有一条**直接注入 `markStale`** 的测试，四个真实调用点无一被触达——**正因为没人从调用点走过一遍，两处 fail-open 至今不可见**，A4 用只读探针（`go build -overlay`，仓库零改动）实证复现，V1 全盘 CONFIRMED 并补全了 A4 自述为"推理"的那一半。

其余：`Halro-Activation: stale` 分支零断言；5 个 create 端点中 3 个的必填/重放**零负面断言**（本轮唯一对外收紧的语义，一次重构就能改回旧语义而 CI 全绿）；`drainAdminAuditIntents` 无 app 级测试（没有任何用例跨过一次 `Open()`）；两条 `reset_` 破坏性迁移只断言了迁移名、从未在非空数据上跑过；`MaxProviderCalls` 耗尽的两条分支从未执行——而按 A1 的裁决，这个上限是探测类支出的**唯一**上界。§7 给出 12 条可执行补测试清单，前三条是 8 分的硬前提。

### 4.5 A5 · BUG 排查（Fable 5 · 7.5 → 校正 7.0）

**全仓未发现可复现的 P0/P1。** 新增三大子系统在错误路径与并发路径上写得克制：可能计费的 provider 调用用 reserved→running→unknown 三态 durable 记账、崩溃后不重放；`publicCapabilityDetection` 逐字段剥离；provider 变更在提交前就用同一 policy 校验 credential audience，**堵死了"改坏 BaseURL 让整张注册表致命 refuse → 全数据面 stale"这条自伤路径**（这条正面结论重要，值得保护）。唯一有代码证据的具体缺陷是 `internal/store/bolt/model_capability_detection.go:169-197` 在 `ForEach` 迭代中对同一 bucket `Put`——违反 bbolt 明文游标契约，且与全仓其余**所有**同类代码的"先收集后写入"写法相悖；A5 用 2000 条记录做的复现**未触发丢数据**，据此诚实降为 P3。

**汇总校正理由**：A5 明确把"stale 拒全数据面流量"归口给 A1 而未独立复核，恰好错过本轮最重的一条。这不是失职（角色分工如此），但它说明"独立视角"在归口约定面前会失效。

### 4.6 A6 · 架构设计与工程规范（Fable 5 · 7.5 → 校正 7.0）

**新增子系统延续了既有分层，没有绕过。** modelcatalog 独立成包且只依赖 domain + safetransport，远端抓取强制 HTTPS + host 白名单 + 1 MiB 上界；能力探测的 Provider I/O 全部经 `provider.Adapter` 接口，本区间非测试 Go 代码未新增任何 `http.Get/http.Client/net.Dial` 直连；全仓层次无违规。**pre-1.0.0 纪律未发现严格违规**：旧模型列表路由被替代后是 404 且有具名测试钉住、无人读取的 scheduling 字段被直接删除、错误的探测数据用 reset 迁移丢弃重建而非加第二读径。

**下限一的重新量化（不是重复上一轮的判断）**：app 非测试行数 17,629 → 21,079（+19.6%），admin_*.go +28%，Runtime 68 个字段，**跨 admin/非 admin 边界符号从约 11 个升到约 27 个（≈2.5×）**，五把 admin 锁中 `adminTopologyMu` 已越界。结论：**"1.0.0 前不拆"仍然正确，但 260807 关闭 P2-19 时的两个支撑事实已不再成立**——"以后拆也不贵"这个隐含前提已经失效，且**08-07 的边界表已过期，不得再被引用**。

**汇总校正理由**：A6 的分层扫描覆盖了包依赖与符号边界，但没有覆盖状态建模层——`activationTracker` 用一个标志表达四个独立域的状态是 D2 范畴的设计错误，V1 已 CONFIRMED 其后果。

### 4.7 B1 · 可用性与开箱（Opus 5 · 4.0 → 校正 5.5）

**裸机主干路径很好，部署工件面有一处真错。** 干净机器 `make start` 4 秒出二进制并自初始化，`halro bootstrap` 一条命令建完整链，README 的 curl 原样可用——从零到第一个请求 200 用了 **11 分 24 秒**，其中**产品本身耗时不到 30 秒**，其余是卡壳与评审环境搭建。9 次卡壳中 8 次可归因于产品。

**B1-01 原判 P0「四份部署工件全部无法启动」→ 裁决后 P1，且工件清单从 4 份缩到 1 份。** V2 判 PARTIAL：机制（发布锁建在 `filepath.Dir(dataDir)`）与"照 operator-guide 原文跑 100% 起不来"**逐字复现，连 digest `fdff7c09c3790d29` 都相同**；但"四份工件都错"被证伪——Dockerfile 的 `WORKDIR /var/lib/halro` + 仓库自带 `configs/config.example.yaml:66` 的相对路径 `./data` 恰好解析成**正确布局**，在 k8s 只读根文件系统形状下实跑 `Halro initialized`（实证 F）；k8s manifest 在正确 `data_dir` 下完整跑通并 healthy（实证 E）；systemd unit 不规定 `data_dir`，而 `operator-guide.md:497` 自我限定为 "**Container** configuration"，不覆盖裸机 systemd。**B1 提的"四处统一改成 `/var/lib/halro-volume`"是过度修复，会破坏实证 F 里已经工作的默认路径。** 真正需要改的只有 `operator-guide.md:497` 一行。V2 同时指出**唯一的系统性问题是守护缺失**：CI 的 `container` job 从不挂卷启动容器，另立一条独立整改项（R-21）。

**B1-09 原判 P2「连接失败满额结算」→ 裁决后 P3，核心主张被证伪。** 见 §4.16。

其余成立且重要：容器发布端口不可达而 `docker ps` 报 healthy（在 k8s 上意味着 readiness 通过、Service 导流、全部超时）；`halro doctor` 把父目录不可写报成"数据目录正被占用"并**主动误导**（Troubleshooting 第一条会让人去找一个不存在的持有者）；**首启 checklist 在一个能正常出 200 的实例上报 0/4、"未发布模型、未授予访问"**——根因是三个就绪判定只认探针证据，而本仓推荐的最快路径 `halro bootstrap` 不跑任何探针。

### 4.8 B2 · API 兼容性契约（Fable 5 · 7.0）

**数据面兼容承诺基本能兑现：机制对、门禁在，缺的是"合同写全"这一步。** 本区间对 `internal/gatewayapi` / `openaiapi` / `anthropicapi` / `internal/compatibility` / `tests/compatibility` 的 diff **为零**（实证），四个 compatible 端点有 golden 清单门禁 + 六个官方 SDK 黑盒矩阵；B2 起了真 facade 对四个端点逐项实跑探测，三者与 manifest 声明逐字吻合。

**下限 1（manifest 与实际行为一致性）：基本一致，两处自述缺口。** 19 条 manifest 的 method+path 与 chi 挂载逐条比对全部存在、无多无少——但**这一步没有自动门禁**，靠人眼。chat/embeddings 的 deviations 漏报 unknown-field 拒收（实测 `frequency_penalty`/`presence_penalty`/`logit_bias`/`logprobs`/`store`/`metadata`/`service_tier` 全部 400），而对照的 responses/anthropic 两份写了——外部使用者拿 manifest 做 diff 会得出错误结论。

**下限 2（`Idempotency-Key` 必填的影响）：影响面安全，文档化负面。** 官方 SDK 零影响（只触达 `/v1/*`，数据面 diff 为零）；唯一已知调用方控制台在同一提交内已改且每表单 `useRef` 稳定跨重试；pre-1.0.0 无野生脚本——**"必填而非可选"这个激进选择能成立的前提只在 1.0.0 之前成立，现在不写文档，1.0.0 之后就没有无痛窗口了**。而 `docs/contracts/idempotency-contract.md:3-4` 现在是**主动误导**。B2-01 判"阻塞发布，补写即解除"。

另一条值得记的边界澄清（B2-04）：SDK 黑盒矩阵覆盖的是"协议层线型"，backend 是桩——语义翻译、路由、能力过滤、redaction、budget、真实鉴权、以及真实二进制多挂的 `refuseWhileSnapshotsStale`/限流中间件**一个都没穿过**。这个 gate **不应在放行论证里被引用成端到端证据**。

### 4.9 B3 · 供应链与发布工程（Opus 5 · 4.0）

**门禁部分被证明有效，交付部分从未闭环过一次。** rc.1 没有误发一个正式 Release，这是控制点起作用而不是运气——`release.yml:277-313` 的顺序（evidence 非空 → 绑定 expected commit/tag → `sha256sum --check` → 逐 artifact `cosign verify-blob` → 校验 annotated tag → 最后才 `gh release create`）设计正确且 fail-closed，**这是本轮唯一一处"门禁真的挡住了一次发布"的实证**。M11 evidence verifier 拒绝仓库自带模板且这条断言有 CI 门禁（B3 自己纠正了最初的误判）。

**B3-01 原判「根因已查明为缺 secret」→ 裁决后「两个前置条件缺失（CONFIRMED）+ 具体失败步骤不可验证」。** 见 §3.4 的完整改写要求。**B3-03 从 G4 判定表移出**（范畴错误），其作为供应链发现是否成立本轮未单独裁决，事实层经 V5 复核为真。

其余成立且需处理：外部使用者无法独立完成 cosign 验签（三处文档都只说"要验"）；文档描述的"四方审批环境门禁"在真实仓库中不存在，且 GitHub 会在 job 首次引用不存在的 environment 时自动创建且不带保护规则——**谁掌握 secret 写权限，谁就能同时提供"四方签署"和"审批"**；`dependency-license-review.md` 严重过期（称 5 个直接 Go 依赖，实际 12 个；4 行版本漂移；**整个 AWS KMS 凭据面从未被覆盖**；前端断言的 CC-BY 已归零而多出 12 条 dev-only MPL-2.0 未提及），且"本区间依赖零变更"是**被误用的证据**——漂移发生在区间之前，以文档自己的基准算有 14 次提交动过依赖文件；`NOTICE` 缺 AWS SDK 与 smithy-go 的 Apache-2.0 §4(d) NOTICE 传递（唯一有实际合规后果的一条）；`security-review-v1.md` 自称 blocked **仍然成立且理由不是文档写错了而是它写对了**，但同时**不可执行**（"M10" 在 HEAD 已无定义，支撑它的 `crash-recovery-matrix.md:21` 还为一个已被删除的测试断言 Pass），覆盖面落后 469 提交；`MODEL_CATALOG_TRUST_ROOTS` 未设置使签名目录子系统在真实产物里永远 inert（fail-closed，不是安全洞，是交付面问题）；packaging 门：**无任何 registry 推送，而 k8s 清单是占位镜像**；`check-production-assets.sh` 用"字符串是否出现"当发布链门禁，结构上看不见环境审批这一环。

### 4.10 B4 · 数据迁移与升级（Fable 5 · 5.5）

**机制可发，公告不可发。** 迁移机制是这个仓库里做得最扎实的部分之一：全链单事务（任何中途崩溃则 schema 版本、迁移历史、数据变更一起回滚）、降级双向 fail-closed 且不静默、schema 20 的拒绝信息是本仓库可行动错误的范本、restore 原子切换并保留回滚目录、迁移名被测试冻结。B4 用**三方二进制对照实跑**（基线 / rc.1 / HEAD）取得四个方向的跨版本 backup/restore 失败方式。

**两条 P1 经 V4 全部 CONFIRMED，且原判还漏了一处加重情节。** F-1（区间 22.5k 行改动 CHANGELOG 零提交，迁移 24~27 完全未公告，运行它的实例 9 行启动日志里什么都看不到）：CONFIRMED，阻塞。F-2（rc.1 → 1.0.0 事实上无升级路径，三处断裂无一公告、两处失败方式误导）：CONFIRMED，需裁决——根因是 `0814cac`（Heimdall→Halro 改名，基线前）把备份加密域串与 vault HKDF 串一起换掉，同一把密钥在两个版本派生出不同 AEAD 密钥，于是**正确的密钥被报成 `backup authentication failed`、正确的 master key 被报成 `master key does not authenticate the metadata store`**。V4 补充的加重情节：HEAD 启动在密钥校验**之前**已把 bbolt 从 v19 迁到 v27 并提交，key-check 失败不回滚——**用 1.0.0 试启动一次之后，退回 rc.1 二进制也打不开了**，"留在旧构建"这条退路在 rename 边界上试过一次就消失。"排查动作本身具破坏性"，这是维持 P1 而非下调的独立理由。

**"rc.1 用户是否存在"的裁决（V4 §5）**：二进制渠道**不存在**（从无 Release）；源码渠道**无法证伪**——仓库 public，rc.1 标签已推到公网，旧名在 GitHub 重定向到现仓库，module path 至今可解析可构建；14 天 5,956 次 clone / 498 唯一来源（大量必是模块代理与爬虫，0 star / 0 fork）。概率"低而非零"。**裁决路径**：发布说明写明"rc.1 从未发布、与 1.0.0 不互通、必须重建实例"即可带着这个事实发；不写不能发。修复成本是一节文档。

**一处评审前提修正**：`role-prompts.md` §5 B4 段说"其中三条是 `reset_` 前缀"与事实不符——实为**两条**（25、26），`review-plan.md` §2.1 的枚举是对的。且其破坏面仅限未发布的中间构建：探测桶创建于迁移 24，rc.1 是 schema 19、基线是 23，任何已发布或基线实例**根本没有可丢的探测记录**。公告按"两条、影响面限于中间构建"如实措辞即可，不必夸大。

### 4.11 C1 · 运维就绪与 Runbook 演练（Opus 5 · 7.5，承担 G5/G6）

**四项必做全部实跑完成，G5 证据完整，扣分集中在"事故中找不到 / 找到了不指路"。** 所有异常路径（WAL 提交字节被改、撕裂尾帧、审计篡改、归档截断/篡改/错密钥、错确认串、Master Key 不匹配）都是实跑打出来的 fail-closed，不是推断；`gateway-key-compromise.md` §1「语义边界」的每一条实测都成立，是 C1 见过最贴合代码行为的一份 runbook。崩溃恢复矩阵抽样 6 条 6 条成立，**包括"预留后崩溃"被保守结算（`recovered_settled=3`）而非静默退款**——这是对 CLAUDE.md 记账不变量的一次真实二进制级验证。

**F-C1-06（`configuration_stale` 是一个无 runbook、无指标、无告警的整机停服状态，P1）与 C2-01、D1-02 三方交叉**，见 §9.1。其余：`docs/runbooks/` 缺 file 模式 Master Key 轮换 runbook（过程本身走得通，只是不在事故中会被检索到的位置）；break-glass 命令缺 `--config` 会打到别的实例（实跑打偏）；恢复会复活已撤销的 Gateway Key 且工具侧零提示（**文档已警告且实测为真**，缺的是工具侧护栏）；轮换后旧备份被拒时错误信息把矛头指向"staged Vault"而不是"你的 config 指着新 key"（自救路径存在且实跑成功，只是没人告诉你）。

### 4.12 C2 · 可观测性与告警（Opus 5 · 6.0）

**既有基座质量高，本区间的新增面整体没有接进告警链。** 46 个提交里 `deploy/observability/` 只动了 `smoke.sh` 一个文件；`alert-rules.yml`、`recording-rules.yml`、`rule-tests.yml`、`alertmanager.yml`、`operations-runbook.md` **一行未改**。10 个新指标进了 metrics-reference，对应 **0 条告警、0 条 rule test、0 条 runbook 处置**。

**最严重的一条是本区间最关键的新机制**：stale 拒全数据面流量在 Prometheus 侧没有任何指标、没有任何告警，表现形式是"**流量归零 + 错误率 0 + target up**"——恰好是所有现有告警都看不见的形状（`/metrics` 由独立 router 提供、不挂 stale 中间件，所以 `HalroTargetDown` 保持沉默；`halro_requests_total` 完全不动所以错误率显示 0.00 且门槛条件为假；规则集中没有任何"流量归零"告警）。**这与"今天没人调用"在监控上完全同形。** 唯一的旁路信号（deadman 探 `/health/ready`）**不在 Core 出厂形态里**。

**下限 3 的应答质量值得单独记**：PR #146 的修复被确认为真实、推导正确、且把推导写进了注释——**这是本轮见到的最扎实的一处修复**。C2 逐条核对了其余 11 处等待与超时，找到两处同类错误：优雅关闭预算硬编码 30s 而同配置允许的最长请求是 2m（**预算比最坏值小 4 倍**，是最像 PR #146 的一处）、deadman 单 target 的 5s timeout 被 readiness 与 freshness 两次请求共享。**但 PR #146 的修复本身没有守护**：`validate.sh` 钉的是 `repeat_interval` 而预算真正依赖的是 `group_interval`——**钉错了旋钮**，改后者不会让任何门禁变红，该门禁按构造又会约一半概率变红。

`metrics_contract_test.go` 的门禁**对条件导出结构上是盲的**（只检查本次渲染出现过的族），3 个 `halro_audit_anchor_*` 已经因此漏出去——C2 用 `-count=1` 实跑证实门禁为绿的同时三个族缺文档，这是盲区本身的证明。

**deadman 独立故障域的假设：部分成立**——在文档与代码层面成立且被诚实标注，在**默认部署形态下不存在**（出厂 compose 只含 prometheus + alertmanager），且覆盖面比"独立故障域"这个词让人以为的要窄（不覆盖 ops 通知投递路径）。

### 4.13 C3 · 性能与容量（Fable 5 · 7.0 → 校正 6.5，承担 G9）

见 §3.6。三条下限全部取得实测数字。**一条未修的 P1**：argon2 登录验证无并发信号量，每并发精确 64.2 MiB、完全线性、无上界，实测 16 并发即 1.06 GiB 越过 k8s 的 1Gi 兜底，且 4.13 GiB 的高水位 4 分钟不归还。唯一控制点 `login_rpm` 是 per-source-per-minute，不是全局并发上限。**默认 Admin 监听为回环**，把利用面收窄到"可达 Admin 监听"的部署。这是 260807 的 P1-6(b) 遗留，本区间未新增控制点。

**汇总校正理由**：按 rubric 锚点，"P1 有运维绕过手段但绕过手段没写进文档"是 5~6 档的定义句，argon2 正是如此（k8s memory limit 是绕过，但没有任何文档说它是必需的），取上沿 6.5。

### 4.14 D1 · 交互设计（Sonnet · 6.0）

**能力探测是本区间设计得最讲究的一块，两条下限给出明确负面结论。** 能力探测把"探测不到"和"模型拒绝"分开说、分状态给出明确下一步、失败时逐个候选接口列出"问了什么、答了什么、这个接口本来能验证什么"并提供"改用人工声明"的退路，且有测试守护；保存被拦截时命名每一个具体原因而不是一个灰掉的按钮；技术细节默认折叠。

**D1-01（P1，阻塞）**：五个创建入口撞 409 幂等重放全部退化成通用"数据已被其他操作修改，请刷新后重试"——这句话对重放场景是**误导性的**（什么都没被别人改，且刷新重试不会解决问题，同一个 idempotency key 还在），且服务端英文原句会未经翻译直接出现在 zh-CN 界面。团队清楚这个反模式并为其他几个 code 主动屏蔽了它，**唯独没有把新增的这五个加进去**。Gateway Key 场景尤其需要单独一句更强的提示——明文已不可能再取回，必须撤销重开，而这句话只存在于 `admin_projects.go:257` 的 Go 源码注释里。

**D1-02（P1，阻塞）**：stale 在控制台**完全不可见**——不是"说得不够清楚"，是 `web/src/types.ts:821-832` 的 `SystemStatus` 类型定义里都没有这个字段。管理员没有任何控制台内的方式知道网关正在拒绝真实客户端流量、拒绝了多久、为什么、是否需要人工干预。

### 4.15 D2 · 视觉设计与整改验收（Sonnet · 8.0）

**260810 的 P1×3 已全部关闭，且不是作者自述级别。** D2 本人跑了 grep 与 vitest 两类独立复核：低于 12px 的字号声明 **116 → 0**，字重 650 及一切字面量 `font-weight` 为 0，二者都由零容忍测试在 `npm test` / `make check` / CI 中强制守护（13/13 通过）。字阶从 10 档收敛为 6 个 token；H1 从 54px 降到 `clamp(32px, 4vw, 42px)`；本区间新增界面全部复用语义 token，未新起一套，无十六进制色值、无内联样式；间距/圆角 ratchet 基线单调下降 758→748→743 且实测当前值 703（**有 40 的余量，不是压线过关，也不是顶高基线掩盖新增违规**）。

残留 P2/P3：Light 主操作色仍是深橄榄绿（本轮做的是"三套绿统一成一套绿"，不是"这套绿变亮"）；9 处字号声明游离在 6 级字阶之外（均 ≥12px，不违反下限，但字阶收敛这条治理只锁了下限、不锁离散度）。**本轮唯一到 8 分的角色。**

### 4.16 对抗验证六条（V1~V6）

见 §7 的裁决表。**裁决覆盖原判，以裁决为准。**

---

## 5. 统一 BUG 清单

> 已按裁决改写级别。"阻塞"列已合并对抗验证的表态。
> 完整证据在 `findings/` 对应文件；此处只保留定位与一句话结论。

| ID | 级别 | 来源 | 一句话 | 阻塞 |
|---|---|---|---|---|
| A1-01 / A4-01 / A4-02 | **P1** | A1、A4，V1 CONFIRMED | 四个激活域共享一个 stale 标志，任一域成功清除其他域的失败；恢复循环 ≤5.5s 清掉它并不修复的 redaction/Token Guard 域，导致整条策略旁路 | **是** |
| A1-02 | P2 | A1 | 审计意图投递失败后无进程内重试，只有下次重启的 drain；注释宣称的"after a delivery failure"重试不存在 | 否 |
| A1-03 | P3 | A1 | `TestAdmissionIsConservativeWhileASettlementIsInFlight` 名不副实（实为串行序列），且保守性主张在结算成本 > 预约额时有未声明的例外 | 否 |
| A1-04 | P3 | A1 | 记账观察者在 `applyMu` 持有期间被调用，observer panic 会永久挂死全部记账（挂起而非拒绝） | 否 |
| A1-05 | P3 | A1 | Credential 创建不在幂等键纪律之内，重试产生重复凭据记录 | 否 |
| A1-06 | P3 | A1 | 能力探测计费调用不进 Ledger —— 评估为可接受边界，**须公告** | 否（条件：进已知限制） |
| A2-01 | P2 | A2 | invocation-target 的两条 GET 侧信道在无 CSRF/同源/step-up 下花费 Provider 凭据；`resolve.describe` 连角色门都没有 | 否 |
| A2-02 | P2 | A2 | 能力探测发起花费凭据但不要求 step-up（与既有 `test` 端点一致，属设计取舍） | 否 |
| A2-03 | P3 | A2 | 签名目录来源约束依赖编译期常量，release 构建若启用 catalog 但未注入 trust roots 会静默无法更新 | 否 |
| A3-01 | ~~P2~~ → **P3** | A3，**V3 PARTIAL / 安全结论 REFUTED** | 非 strict Beta profile 的能力天花板管理面可写，但数据面四道闸门拦截且天花板不可注入是类型层面保证；性质变为"管理面接受永不生效的声明" | 否 |
| A3-02 | P3 | A3 | 能力声明类审计记录非事务、崩溃可丢，与实际 mutation 不一致 | 否 |
| A3-03 | P3 | A3 | stale 进出无可信/持久留痕，整段拒流事故只在进程日志里 | 否 |
| A4-03~A4-13 | P2/P3 | A4 | 11 条测试与可观测性盲区（stale 无调用点测试、`Halro-Activation: stale` 零断言、3 个 create 端点零负面断言、`drainAdminAuditIntents` 无 app 级测试、两条 `reset_` 迁移未在非空数据上跑过、`MaxProviderCalls` 两条分支未执行、stale 中间件两个 route group 只覆盖一个、`adminTopologyMu` 测试断言错了层、single-flight 测试是全顺序的、stale 无 Prometheus 指标、前端对两个新语义零处理） | 否 |
| A5-01 | P3 | A5 | `InterruptModelCapabilityDetections` 在 `ForEach` 内 `Put`，违反 bbolt 游标契约且与全仓写法相悖（2000 条复现未触发丢数据） | 否 |
| A5-02 | P3 | A5 | 取消探测的乐观并发在高频探测写入下大概率首发 409 | 否 |
| A5-03 | P3 | A5 | Gemini `DescribeInvocationTarget` 解码结构体缺 JSON tag，靠大小写不敏感匹配 | 否 |
| A6-01 | P2 | A6 | internal/app 拆分前提在衰减（耦合 ≈2.5×，`adminTopologyMu` 已越界）；1.0.0 前不拆仍对，但 260807 边界表已过期 | 否 |
| A6-02 | P2 | A6 | 审计意图机制四代构造并存，`createAdminUser`/`deleteAdminUser` 仍在 fire-and-forget 路径 | 否 |
| A6-03 / B2-01 | **P1** | A6、B2 | `idempotency-contract.md:3-4` 称该头 optional，与 Admin 面必填直接矛盾；英文契约面与发布说明对本轮唯一的对外语义收紧完全沉默 | **是**（补写即解除） |
| A6-04 | P3 | A6 | 幂等校验助手与四份内联拷贝并存，其中一份出自引入助手的同一批提交 | 否 |
| A6-05 / C2-03 | P2 | A6、C2 | `metrics-reference.md` 缺 3 个 `halro_audit_anchor_*` 族，且门禁对条件导出结构上是盲的 | 否 |
| A6-06 | P3 | A6 | `Halro-Operation-Id` / `Halro-Activation` 响应头与 `*_idempotency_replay` 在 `web/src` 零命中 | 否 |
| A6-07 / B2-02 | P2 | A6、B2 | 提交协议与 503 `configuration_stale` 语义只在 zh-CN 架构文档，自称 normative 的 `gateway-correctness.md` 零提及；Anthropic 路由上 503 用错信封、无 `Retry-After` | 否 |
| B1-01 | ~~P0~~ → **P1** | B1，**V2 PARTIAL** | 照 `operator-guide.md:489-497` 跑容器 100% 起不来（逐字复现）；但 Dockerfile / systemd / k8s 三项被证伪，只需改一行 | **是** |
| B1-02 | P1 | B1 | 容器路径无 init 步骤，`load vault key check: record not found` 与 `master key already exists` 两种失败都不可行动 | **是** |
| B1-03 | P1 | B1 | `-p` 发布的端口不可达而 `docker ps` 报 healthy；HEALTHCHECK 走容器内回环 | 需裁决 → **建议是** |
| B1-04 | P1 | B1 | `halro doctor` 把父目录不可写报成"数据目录正被占用"，并把人送去找不存在的持有者 | 否 |
| B1-05 | P2 | B1 | `init` 因为它不会绑定的监听地址而拒绝执行，且无对应放行开关 | 否 |
| B1-06 | P3 | B1 | `--help` / `-h` / `help` 全部不识别且不回落到 usage | 否 |
| B1-07 | P3 | B1 | 端口占用的报错不说改哪个键 | 否 |
| B1-08 | P1 | B1 | 首启 checklist 在能正常出 200 的实例上报 0/4、"未发布模型、未授予访问" | 需裁决 → **建议是** |
| B1-09 | ~~P2~~ → **P3** | B1，**V6 PARTIAL / 核心主张 REFUTED** | 真正没连上的失败结算为 0、预约全额释放（`d116f77` 已修，三条具名回归测试守护）；真缺陷更窄：safetransport dial 前自拒返回裸 `fmt.Errorf`，`Unsent` 认不出，按 ambiguous 满额结算并抑制 failover | 否 |
| B2-03 | P2 | B2 | chat/embeddings 的 manifest 漏报 unknown-field 拒收（实测 7 个 OpenAI 官方参数一律 400） | 否 |
| B2-04 | P2 | B2 | SDK 黑盒矩阵覆盖的是协议线型不是网关行为，**不应被引用成端到端证据** | 否 |
| B2-05~07 | P3 | B2 | 409 replay 回归测试只覆盖 2/4 资源；同名头两个面两套约束未互相声明；embeddings 响应多带 `completion_tokens:0` | 否 |
| B3-01 | **P0（发布门禁）** | B3，**V5 PARTIAL** | 两个前置条件（Environment、secret）今天可验证地缺失，今天打 tag 必然仍失败；"根因已查明"高估一档，证据段须重写 | **是** |
| B3-02 | P0（证据链） | B3 | rc.1 的 release run 已删除，日志/artifact/check run 全部消失，仓库自述不可独立复核 | 需裁决 |
| B3-03 | P0（发布工程） | B3，V5 复核事实为真 | 构建不可复现（`date -u` + `tar` 缺 `gzip -n`），与 M11 证据门禁互相锁死，把发布压成一条无文档、受 90 天保留期限制的窄路。**不属于 G4** | **是**（先修可大幅降低 R-01 成本） |
| B3-04 | **P1** | B3 | 外部使用者无法独立完成 cosign 验签：`--certificate-identity` 只存在于 workflow 内部自验 | **是**（G4 的直接内容） |
| B3-05 | **P1** | B3 | 文档描述的"四方审批环境门禁"在真实仓库中不存在；repo 级 secret 可让 publish 零人工审批直接建 Release | **是** |
| B3-06 | **P1** | B3 | `dependency-license-review.md` 严重过期，AWS KMS 凭据面从未被 license review 覆盖 | 需裁决 → **建议是** |
| B3-07 | P2 | B3 | `NOTICE` 缺 AWS SDK / smithy-go 的 Apache-2.0 §4(d) 传递（唯一有实际合规后果的一条）；`THIRD_PARTY_NOTICES.md` 漏 4+2 项 | 否 |
| B3-08 | P2 | B3 | `security-review-v1.md` 自称 blocked **仍成立**，四道门全未关；且它不可执行（M10 已无定义、`crash-recovery-matrix.md:21` 为已删除测试断言 Pass）、覆盖面落后 469 提交、`LoadDefaultConfig` 未经点名要求的威胁评审 | 需裁决 → **建议是** |
| B3-09 | P2 | B3 | `MODEL_CATALOG_TRUST_ROOTS` 未设置，签名目录子系统在真实产物里永远 inert | 需裁决（二选一：设置 or 写入已知限制） |
| B3-10 | P2 | B3 | Action 引用 19 处可变 tag / 8 处 SHA-pin，未 pin 的恰好是产出被签名产物的构建 job | 否 |
| B3-11~13 | P2/P3 | B3 | release 的 govulncheck 未 pin 工具链；`check-production-assets.sh` 用 grep 当发布链门禁；SBOM 是源树 SBOM 而非产物 SBOM，`provenance` job 无任何 SLSA attestation（名字比内容承诺得多） | 否 |
| B3-14 | **P1** | B3 | 容器从不推送到任何 registry，而 k8s 清单是占位镜像 `ghcr.io/OWNER/halro@sha256:REPLACE_...`；`check-production-assets.sh` 验证了它的安全属性却看不见镜像引用不可解析 | 需裁决 → **建议是** |
| B4-F1 | **P1** | B4，**V4 CONFIRMED** | 本区间全部改动无一进入 CHANGELOG，迁移 24~27 与两条 reset 迁移完全未公告，实例侧零提示 | **是** |
| B4-F2 | **P1** | B4，**V4 CONFIRMED（原判还漏了加重情节）** | rc.1 → 1.0.0 无升级路径，三处断裂无一公告、两处失败方式误导；且排查动作本身具破坏性（试启动一次后退回 rc.1 也打不开） | 需裁决 → **写明即可发** |
| B4-F3 | P2 | B4 | 老备份在新二进制上恢复成功，但迁移发生在 restore 内部且全程无提示 | 否 |
| B4-F4 | P2 | B4 | 新备份在旧二进制上的拒绝信息把版本不兼容与八种损坏折叠成 `backup manifest is invalid` | 否 |
| B4-F5/F6 | P3 | B4 | reset 迁移是两条不是三条（评审前提修正）；迁移 24~27 无专属回归测试；发布说明的"39 bbolt migration boundaries"易被误读 | 否 |
| F-C1-01 | P2 | C1 | `docs/runbooks/` 缺 file 模式 Master Key 轮换 runbook | 否 |
| F-C1-02 | P2 | C1 | break-glass 命令缺 `--config`，事故中会打到另一个实例（实跑打偏） | 否 |
| F-C1-03 | P3 | C1 | runbook 承诺的 412 在第一次尝试时拿不到（step-up 检查排在修订号检查之前） | 否 |
| F-C1-04 | P2 | C1 | 恢复会复活已撤销的 Gateway Key，工具侧零提示（文档已警告且实测为真） | 否（应进发布说明） |
| F-C1-05 | P2 | C1 | 轮换后旧备份被拒，错误信息指向"staged Vault"而非真正的原因 | 否 |
| F-C1-06 / C2-01 / D1-02 | **P1** | C1、C2、D1 三方交叉 | `configuration_stale` 是整机停服状态，却同时无 runbook、无指标、无告警、控制台不可见 | 需裁决 → **建议是** |
| F-C1-07 | P2 | C1 | 签名目录 runbook 的唯一示例文件无法通过它自己的 verify 步（`profile_id` 未注册） | 否（G6 关闭条件） |
| F-C1-08 | P2 | C1 | 三条 m11 runbook 文件本身无 release-blocked 标注 | 否（G6 关闭条件） |
| C2-02 | **P1** | C2 | 本区间 10 个新指标对应 0 条告警、0 条 rule test、0 条 runbook 条目 | 否（直接压 D7） |
| C2-04~C2-10 | P2/P3 | C2 | 契约点名要求的两条告警根本不存在；5 条平台告警无 `runbook_url`（3 条 critical）且 `operations-runbook.md` 无按告警名的小节；PR #146 的修复无守护（`validate.sh` 钉错了旋钮）；优雅关闭预算 30s vs 请求上限 2m；deadman 两次请求共享一个 5s；ops webhook 故障时报告它的告警走的正是坏掉的那条路；`runbook_url` 是仓库相对路径 | 否 |
| C3-01 | **P1** | C3 | argon2 登录验证无并发信号量，每并发 64.2 MiB、线性无上界，实测 64 并发 4.13 GiB 且高水位不归还 | 需裁决 → **建议是** |
| C3-02/03 | P3 | C3 | 拓扑提交协议使管理写路径吞吐降 25.3%（darwin 实测，不阻塞）；管理元数据写走 `db.Update` 不合并，单机地板 ~100 tx/s | 否 |
| D1-01 | **P1** | D1 | 五个创建入口撞 409 幂等重放全部退化成"数据被别人改了，刷新重试"，且服务端英文原文直接出现在 zh-CN 界面 | **是** |
| D1-03 | P2 | D1 | 能力探测的成本提示没说明这些调用不进 Ledger/预算 | 否 |
| D2-F1/F2 | P2/P3 | D2 | Light 主操作色的色相未处理（只统一了一致性）；9 处字号游离在 6 级字阶之外（均 ≥12px，字阶收敛无门禁） | 否 |

---

## 6. P0 / P1 / P2 修复清单

> 编号与 [progress.md](progress.md) 一一对应。"绕过"列回答 S4 的要求：P1 未修则必须给出运维绕过方案。

### 6.1 P0（发布门禁类；按 §3.1 的裁断不计入 G1）

| ID | 事项 | 来源 | 关闭判据 | 绕过 |
|---|---|---|---|---|
| R-01 | 建 `v1-release` Environment + required reviewers，secret 设为 **environment secret**；走通一次 publish 产出 Release | B3-01、B3-05，V5 | `gh api .../environments` 返回 `total_count: 1` 且 `protection_rules` 含 `required_reviewers`；`gh release list` 非空 | **无**——运维无法自己发一个 Release |
| R-02 | 在发布说明与 README 给出可直接复制的完整 `cosign verify-blob` 命令块（含 `--certificate-identity` / `--certificate-oidc-issuer` 与"先验 checksums.txt 再用它验其余"的正确顺序） | B3-04 | 一个没读过仓库的人照着能跑通 | 无 |
| R-03p | 修可复现构建：`Date` 改用 tag 的 committer date 或 `SOURCE_DATE_EPOCH`（四条 matrix 腿与 container 共用同一值）；`tar` 加 `--sort=name --owner=0 --group=0 --numeric-owner --mtime` 并 `gzip -n` | B3-03 | 同一 tag 在两台机器上重建，`sha256sum` 与 `checksums.txt` 逐行相等 | 有：`gh run rerun --failed` 复用同一 run 的 artifact（**但无文档、且受 artifact 90 天保留期限制**） |
| R-04p | 不要删除 rc.2 的 run；在 `docs/verification/` 下用 run id + artifact 摘要留痕 | B3-02 | 该 run 与其 artifact 可被第三方按 id 查到 | 不适用（已发生的部分不可挽回） |

### 6.2 P1（阻塞发布）

| ID | 事项 | 来源 | 关闭判据 | 绕过 |
|---|---|---|---|---|
| **R-03** | **stale 按域分维度**：`markCurrent(domain)` 只清本域、任一域非空即拒流；恢复循环重放全部四个激活域。补 A4 的 T-1/T-2/T-3 三条从调用点出发的负面测试。**同时裁决两处 lookup-miss 的 fail-open 方向**（`redaction/engine.go:428-431`、`tokenguard/manager.go:308-312`） | A1-01、A4-01/02，**V1 CONFIRMED** | T-1/T-2/T-3 在缺陷态失败、修复后通过（`-count=1`） | **无**——运维无法阻止恢复循环；唯一的人工动作是"再保存一次同类策略"或重启，而没有信号告诉运维需要这么做 |
| **R-04** | stale 四件套：`halro_activation_stale` + `halro_activation_stale_seconds` 两个 gauge（标签基数 0）→ `docs/contracts/metrics-reference.md` + `metrics_contract_test.go`；一条 `== 1 for: 1m` 的 critical 告警 + `rule-tests.yml` 断言；一条短 runbook（是什么/影响什么/看哪里/怎么恢复/何时重启安全）；控制台 `SystemStatus.activation` 字段 + `DiagnosticsPane` 条目 | C1 F-C1-06、C2-01、D1-02、A4-12、A3-03 | 注入一次 stale，指标出现、告警在 1m 后 firing、控制台显示、runbook 链接可点 | 有但很弱：人工轮询 `/admin/api/v1/system/status` 或 `/health/ready` 的 body |
| **R-05** | 发布说明加"从 rc.1 升级"一节：rc.1 从未发布、数据目录与备份与 1.0.0 不互通、必须重建实例；**并写明用 1.0.0 试启动一次会把 bbolt 单向迁到 v27，退回 rc.1 二进制也打不开**。可选：给两条错误信息补一句"若归档/目录产自 2026-08-08 rename 之前的构建，属版本不兼容" | B4-F2，**V4 CONFIRMED** | 发布说明含该节；`grep -i "rc.1" docs/milestones/release-notes-v1.0.0.md` 命中 | 无 |
| **R-06** | CHANGELOG 补 Operator impact：迁移 24/27（就地新增桶）、25/26（重置探测缓存，**两条不是三条**，影响面限于未发布的中间构建）、`Idempotency-Key` 必填、`allow_private_provider_endpoints` 生效 | B4-F1，**V4 CONFIRMED** | `git diff -- CHANGELOG.md` 含上述四条 | 无 |
| **R-07** | 已知限制清单写入 `docs/milestones/release-notes-v1.0.0.md`（§8 的 18 条可直接采用） | G8 | `grep -niE "limitation|单写者|single.writer|高可用" release-notes-v1.0.0.md` 非零命中 | 无 |
| **R-08** | 改 `docs/guides/operator-guide.md:497`：`data_dir` 用挂载点的**子目录**（或直接删掉这句让仓库自带的相对默认值 `./data` 生效）。**不要动 Dockerfile / systemd unit / k8s manifest** | B1-01，**V2 PARTIAL** | `docker run` 起来后 `curl /health/ready` 返回 200 | 有：读 `README.md:81-83` 或 `backup-restore.md:169-197`（它们是对的） |
| **R-09** | 容器小节明确二选一：容器内启 TLS 并给证书挂载方式，或写明 `-allow-insecure-public-listen` + `0.0.0.0` 的适用边界；HEALTHCHECK 与外部可达性的关系写清 | B1-03 | 按文档跑起来后从宿主 curl 得到 200，且 `docker ps` 的 healthy 与外部可达一致 | 有：`--network container:` 或改用 host 网络（无文档） |
| **R-10** | `evaluateOnboardingReadiness` 把"该 Route 上出现过成功请求"作为 publish/access 两个目标的充分证据；无证据时 detail_code 区分"对象不存在"与"对象存在但未验证"，不要用 "blocking" 描述一个已经存在的对象 | B1-08 | `halro bootstrap` 之后 checklist 不得报 `provider_blocking_model` | 有：跑一次 Provider 连接测试（无文档说明这是必需的） |
| **R-11** | 五个 `*_idempotency_replay` code 各一条 `errors.ts` 条目与 zh/en 文案（"这不是冲突，是你已经成功创建过了"）；利用响应里的 `id` 提供"去看那条记录"；Gateway Key 单独一句更强提示（明文不可再取回，必须撤销重开） | D1-01 | `grep -rn "idempotency_replay" web/src` 非零命中，且 zh-CN 界面不再出现英文原句 | 有：管理员自行去列表页确认对象已存在 |
| **R-12** | 修 `docs/contracts/idempotency-contract.md`：把 "optional" 限定到数据面，另立 Admin 面一节（必填 + 409 replay 语义 + 派生 ID + ≤256 与数据面 1–128 的差异）；发布说明加一段 | A6-03、B2-01 | 契约文档与 `admin_create_idempotency.go:32-38` 不再矛盾 | 有：读 `provider-to-project-api-call-chain.zh-CN.md:135`（唯一记载，仅中文） |
| **R-13** | argon2 验证前置一个小容量全局信号量（排队而非并行分配 64 MiB 块），或补进程级 in-flight-hash 上界 | C3-01（260807 P1-6(b)） | 64 并发登录时 RSS 增量不再线性 | 有：保持 Admin 监听在回环 + 设 cgroup/k8s memory limit ≥ 2Gi（**当前无任何文档说这是必需的**） |
| **R-14** | 重新生成 `dependency-license-review.md`（12 个直接 Go 依赖、AWS KMS 凭据面、前端 11 个 runtime 依赖、去掉 CC-BY、补 dev-only MPL-2.0）；加 CI 门禁：依赖文件变更而该文档未变即失败 | B3-06 | 文档中直接依赖数与 `go.mod` 一致；构造一次依赖 bump，门禁变红 | 无（合规义务） |
| **R-15** | `security-review-v1.md`：先关四道门再改那句话（**那句话是对的**）；同时补一次覆盖本区间新增子系统（kms / masterkey / hostsecurity / bearercred / modelcatalog / bedrockmantle / 能力探测 / 拓扑提交协议）的增量安全评审，或显式声明覆盖边界；**单独裁决 `internal/kms/awskms/adapter.go:31` 的 `LoadDefaultConfig`**——文档自己说这类变更需要一次新的威胁评审，而那次评审没有发生；并修正 `crash-recovery-matrix.md:21` 为已删除测试断言 Pass | B3-08、A2（转交） | 四道门有归档证据；矩阵不再引用不存在的测试 | 无 |
| **R-16** | packaging：三选一——加 registry 推送并把镜像纳入签名与 checksums；或保持 tar.gz 分发但把 k8s 清单的 `image:` 改成带注释的显式占位并在已知限制写明**没有官方 registry**；或把 k8s 清单从 1.0.0 交付面撤下 | B3-14 | 使用者按清单能部署，或清单明确说明需要替换 | 有：`docker load` + 自行 push 到私有 registry + 自行改清单（**无文档**） |
| **R-17** | `data_lock` 检查区分 `EACCES`（父目录不可写 → detail 指向父目录路径与所需权限）与 `EWOULDBLOCK`（真的被占用） | B1-04 | 在只读父目录下 `halro doctor`，detail 必须指向父目录 | 有：读 `lock_unix.go:64`（要求运维读源码） |
| **R-18** | `operator-guide.md` 容器小节补 `init` 步骤（含 master.key 该放在可写位置还是先在宿主生成）；`master key already exists` 补一句状态判断（"数据目录为空但 Master Key 已存在"与"两者都在"是不同情况） | B1-02 | 空卷首启按文档能走通 | 有：先在宿主 `init` 再挂进去 |
| **R-19** | capability / signed-catalog / detection 至少 3 条告警（`halro_signed_model_catalog_degraded == 1 for: 15m`；`increase(halro_capability_drift_total[1h]) > 0` 按 reason 分开；detection 失败率），每条进 `rule-tests.yml` 并在 runbook 给处置动作 | C2-02 | `rule-tests.yml` 有对应 firing/resolved 断言 | 有：人工看面板（**没有面板**） |
| **R-20** | G6 三件套：三条 `m11-*.md` 顶部加 release-blocked 标注并链接发布说明；修 `catalog/unsigned-snapshot-v1.example.json:11` 的 `profile_id`（并在 runbook 的 "Prepare and sign a candidate" 一节指向它）；`gateway-key-compromise.md:103` 命令补 `--config` | F-C1-08/07/02 | 三条 runbook 顶部有标注；示例文件能过自己的 verify 步；命令带 `--config` | 有：C1 已实证自救路径，但没人告诉你 |

### 6.3 P2（不阻塞，建议随 1.0.0 或紧随其后）

| ID | 事项 | 来源 |
|---|---|---|
| R-21 | **CI `container` job 增加"挂卷启动 + init + 就绪探测"步骤**（V2 认为这条守护缺失比缺陷本身更值得记）。验证方式：故意把 `data_dir` 改回挂载点，CI 必须红 | V2、B1-01 |
| R-22 | 审计意图投递改用 runtime context；恢复循环或独立 ticker 在积压 > 0 时 drain；注释与实现对齐 | A1-02 |
| R-23 | `metrics-reference.md` 补 3 个 `halro_audit_anchor_*` 族；门禁改成"静态导出清单 ⊆ 文档"而不是"本次渲染 ⊆ 文档" | A6-05、C2-03 |
| R-24 | `resolve.describe` 纳入 `requireAdministratorRole`；两条花费凭据的 GET 侧信道改 POST 走 `requireAdminMutation`，或在 GET 上补 `adminSameOrigin` | A2-01 |
| R-25 | C2-04 的两条告警：要么补规则（`time() - halro_audit_anchor_last_emit_timestamp_seconds > 3×Interval`；`absent(halro_deployment_capability_status)` + `{state="conflicting"} > 0`），要么把契约措辞从"必须"降级。**不要并存** | C2-04 |
| R-26 | 5 条平台告警补 `runbook_url`；`operations-runbook.md` 增"按告警"一节逐 alertname 给三行；`runbook_url` 改带锚点并加 CI 断言 | C2-05 |
| R-27 | `smoke.sh` 从 `alertmanager.yml` 解析 `group_interval` 并断言 `budget >= 2×group_interval + 30`（把预算变成会自动跟随配置的不变量，而不是注释里的常数） | C2-06 |
| R-28 | 两份 manifest 的 deviations 加 "unknown fields are rejected"，`HALRO_UPDATE_GOLDEN=1` 重生成；在 workflow step 或 manifest README 写明 SDK gate 的证明边界 | B2-03、B2-04 |
| R-29 | 提交协议与 503 `configuration_stale` 语义进 `docs/contracts/gateway-correctness.md`；stale 拒绝按路由协议选信封；考虑加 `Retry-After: 5` | A6-07、B2-02 |
| R-30 | 优雅关闭预算做成配置项且默认 ≥ `route_total_timeout`，或写进已知限制；加 `halro_shutdown_truncated_attempts_total` | C2-07 |
| R-31 | `NOTICE` 并入 AWS SDK 与 smithy-go 的 NOTICE 文本；`THIRD_PARTY_NOTICES.md` 补 4 个 Go 模块 + 2 个 i18n 包 | B3-07 |
| R-32 | `release.yml` 全部 `uses:` 改 SHA-pin，或打开仓库的 `sha_pinning_required` | B3-10 |
| R-33 | 裁决 `MODEL_CATALOG_TRUST_ROOTS`：设置它并跑通 `model-catalog-publish.yml`，或在已知限制写明该功能在 1.0.0 未启用、`trust_root_count: 0` 是预期值 | B3-09 |
| R-34 | `restore` 结果 JSON 加 `schema_version_before/after` 并在迁移发生时输出一行；`backup manifest is invalid` 拆出 schema 越界分支报双版本号 | B4-F3、B4-F4 |
| R-35 | `backup restore` 结果带上恢复进来的启用中 Gateway Key 数量与 ID；或 `doctor` 在检测到 `previous_data_dir` 比 live 新时提示"恢复后请重新执行密钥撤销清单" | F-C1-04 |
| R-36 | 轮换后旧备份被拒的错误打印双方指纹前缀并给出下一步 | F-C1-05 |
| R-37 | `docs/runbooks/` 增 file 模式 Master Key 轮换 runbook 或索引指针 | F-C1-01 |
| R-38 | `admin_user.create/delete` 迁到 `AdminAuditIntent` 路径 | A6-02 |
| R-39 | 能力探测成本提示加一句"这些调用不计入项目预算与用量统计" | D1-03 |
| R-40 | `init` 只校验它真正要用的东西，监听地址校验留给 `start`/`serve` | B1-05 |
| R-41 | A4 §7 的 T-4~T-12 九条补测试 | A4 |
| R-42 | `release.yml:36` 同步 `GOTOOLCHAIN=go1.26.5` | B3-11 |
| R-43 | 额外生成一份以二进制为输入的 SBOM，或把"权威清单"的说法改为源树声明清单；`provenance` job 加 `actions/attest-build-provenance` 或改名为 `sign` | B3-13 |
| R-44 | Light 主操作色给出关闭或明确推迟的书面结论；字阶收敛加门禁（业务 CSS 字号只能是 token 值或显式 allowlist） | D2-F1/F2 |

### 6.4 P3（记录，不排期）

A1-03/04/05、A3-02/03、A5-01/02/03、A6-04/06、B1-06/07、B1-09（V6 降级后的窄缺陷：safetransport 自拒返回类型化错误 + `error_class` 语义拆分）、B2-05/06/07、B3-12、B4-F5/F6、C2-08/09/10、C3-02/03、F-C1-03、A3-01（V3 降级后：管理面拒绝永不生效的能力声明 + 一条负面测试）。

---

## 7. 对抗验证裁决表

**裁决覆盖原判，以裁决为准。** 六条全部由与原角色不同的独立执行体完成，且证伪型角色均给出读过的文件与走过的代码路径（review-plan §6 的要求），无一份需作废重跑。

| # | 对象 | 原判 | 裁决 | 裁决后 | 关键论据 |
|---|---|---|---|---|---|
| **V1** | A1-01 / A4-01 / A4-02 · stale 跨域共享标志 | P1，阻塞 | **CONFIRMED** | **P1，阻塞（后果比原判更重）** | 找过八个假想控制点，全部不存在（按域 tracker、条件式 `markCurrent`、恢复循环重放全域、其他重装路径、周期 reload、lookup-miss fail-closed、运行期悬空守卫、按域 readiness）。探针实证 5.4s 自动清除后 `/health/ready` 200，且**补全了 A4 自述为"推理"的那一半**：store 有策略 rev=1 而 engine `HasPolicy=false`。后果不是"旧策略继续跑"而是**整条策略旁路**——`ProcessText` 输出 `here is SECRET-12345 in a prompt`、`tokenGuard.Admit` 返回 `allowed=true status=normal`。**未升 P0**：需要一次真实激活失败作前置，且 mandatory 内建规则仍在 |
| **V2** | B1-01 · 容器/K8s/systemd 部署布局 | **P0**，阻塞 | **PARTIAL** | **P1，发布前必修** | 缺陷成立且**逐字复现（含同一 digest `fdff7c09c3790d29`）**；但"四份工件都错"被证伪——只有 `operator-guide.md:497` 一处真的设了 `data_dir`。**实证 F 是最强的一条证伪**：不改仓库自带的 `./data` 相对默认值，在 k8s 只读形状下 `Halro initialized`——**镜像的布局设计本来就是对的**。B1 提的四处统一改是**过度修复，会破坏已工作的默认路径**。失效方式是理想的 fail-closed（第一次启动、写任何数据之前、带完整路径退出）。另建议单独立项：**CI container job 从不挂卷启动容器，这条守护缺失比缺陷本身更值得记** |
| **V3** | A3-01 · 非 strict Beta profile 天花板管理员可写 | P2，需裁决 | **PARTIAL（安全结论 REFUTED）** | **P3，建议，不阻塞** | M1（管理面可写）CONFIRMED；M2（越过钉死上限）**REFUTED**。数据面天花板不是 `binding.Capabilities`——`gemini.Options` / `bedrock.Options` **结构里根本没有 Capabilities 字段**，天花板不可注入是**类型层面**保证；adapter 硬编码集与 `domain/models.go:500-505` **逐位相同**。四道独立闸门（能力 AND 交集 / profile 键控过滤 / 编译期算子表 / translate 期无条件拒绝）分布在三个包，结果 400 `unsupported_feature` 且**未预留额度、未建连**。Mantle 三 profile 逐位检查，每一位都被另外两道闸堵死。**严重度高估约两档。** A3.md:93-96 那句是事实错误 |
| **V4** | B4-F2 + B4-F1 · rc.1 无升级路径 / CHANGELOG 零提交 | P1 两条 | **两条均 CONFIRMED** | **P1，档位准确** | 五条证伪方向逐条清算全部失败。**原判还漏了一处加重情节**：HEAD 启动在 vault key-check **之前**已把 bbolt 从 v19 迁到 v27 并提交，key-check 失败不回滚——**退回 rc.1 二进制也打不开了**，"留在旧构建"这条退路在 rename 边界上试过一次就消失，排查动作本身具破坏性。"rc.1 用户是否存在"：二进制渠道**不存在**（从无 Release），源码渠道**无法证伪**（public 仓库、标签已推公网、旧名重定向、14 天 5,956 clone / 498 uniques，但 0 star / 0 fork），概率"低而非零"——这正好锚定 P1 而非 P0 或 P3 |
| **V5** | B3-01 根因 + B3 §6 的 G4 判定 | P0 + G4 不通过 | **PARTIAL** | **结论方向成立，证据段必须重写；G4 判不通过（成立）** | (a) "job 被触发过"CONFIRMED，推不翻。(b) "无 `waiting` 状态 ⇒ 没配审批人"**论证无效**——`waiting` 不是 deployment status 的合法取值，从一个不可能出现的状态的缺席推结论。(c) "7 秒 = 断在 `test -n` 第一行"**REFUTED**——`test -n` 是第四个 step 的第一行，前面有 checkout + 下载 80~100MB artifact + 装 cosign；且 B3 的前提"三个 step 跑不完 7 秒"**推翻的正是它支撑的结论**。**结论被 B3 未用的证据救回**：`created_at 23:35:08Z → in_progress 23:35:12Z` **间隔 4 秒**，四方审批流程不可能在 4 秒内完成。**具体断在哪一步无法验证（run 已 404）**。另指出把"不可复现构建"算进 G4 是**范畴错误**（G4 说验签，不要求第三方逐字节重建），应移出；剥掉后 G4 判定不变。**G4 判"不通过"而非"无法验证"**——产物从来不存在是被观测到的否定事实，且今天 environments/secrets 均 `total_count:0`，今天打 tag 仍必然失败 |
| **V6** | B1-09 · 连接失败满额结算 | P2，需裁决 | **PARTIAL（核心主张 REFUTED）** | **P3，不阻塞** | 真正没连上的两类失败（refused dial、DNS 失败）**结算为 0、预约全额释放**——`d116f77` 专门修的，三条具名回归测试守护，实测 `halro_cost_usd_total 0.000000`；上游 HTTP 5xx 同样结算 0。**但 130 micros 确是 Ledger 已结算成本**（来自 `EventAttemptSettled` 的 `CommittedMicrosUSD`，非预约中间态），B1 没看错。真缺陷更窄：`internal/safetransport/transport.go:139,151,155` 三个**本方保证零字节发出**的拒绝点返回裸 `fmt.Errorf`，`provider.Unsent` 认不出自家的拒绝，按 ambiguous 满额结算**并抑制 failover**；V6 在 fake-IP 劫持 DNS 环境下逐位复现 B1 的账面现象（2×130 → 第三笔 `budget_exceeded`），机制完全不同。另：`error_class: connect` 同时覆盖"从未连上"与"连上后失败"，**正是这个标签把 B1 引向了错误前提** |

**六条裁决的分布**：1 条 CONFIRMED（加重）、1 条 CONFIRMED（两项，加重）、4 条 PARTIAL（其中 3 条含降级、2 条含证伪核心主张）。历史对照：260805 六条最严重发现无一原样成立；260807 五条中四条 CONFIRMED、一条 PARTIAL。**本轮的形态介于两者之间，且首次出现"裁决把后果判得比原判更重"（V1、V4）**——这说明对抗验证不是单向的降级机器。

---

## 8. 已知限制清单（G8 的输入）

> **本清单只写在本报告里。** 按边界要求，本轮**未修改** `docs/milestones/release-notes-v1.0.0.md`——那是用户的发布说明，由用户决定何时写入。G8 相应判为"未完成（清单已备好）"。
> 下列 18 条可直接采用为发布说明的"已知限制"章节。带 ⚠ 的三条在 R-03 / R-04 关闭之后应当**改写或删除**，不要固化成永久承诺。

**架构与一致性边界**

1. **单写者、单数据目录。** v1 的一致性边界是一个进程独占一个数据目录（加锁）。Docker / Kubernetes 必须运行**恰好一个副本**（`Recreate` 策略，不是滚动更新）。不存在共享目录或多写者的支持形态。
2. **无高可用。** HA 提案已定 1.1.0（`docs/todo/halro-ha-architecture.zh-CN.md`）。单实例故障即服务中断，恢复责任全部在运维手上。
3. **数据目录布局**：Halro 在 `data_dir` 的**父目录**里创建发布锁，因此父目录必须可写。容器/K8s 请挂载持久化的**父目录**，`storage.data_dir` 取它的**子目录**（见 `README.md` 与 `docs/guides/backup-restore.md`）。

**升级与兼容**

4. **`v1.0.0-rc.1` 从未发布，且与 1.0.0 不互通。** 2026-08-08 的 Heimdall→Halro 改名换掉了备份加密域串与 vault HKDF 串，因此 rc.1 的**数据目录、备份归档、配置文件三者全部不可迁移**，必须重建实例。失败方式会呈现为 `backup authentication failed` 与 `master key does not authenticate the metadata store`——**那不是密钥错误，是版本不兼容**。⚠ **更重要的是：用 1.0.0 试启动一次 rc.1 数据目录，会在密钥校验之前把 bbolt 单向迁移到 v27 并提交，此后退回 rc.1 二进制也打不开该目录。请先备份再尝试。**
5. **Admin 创建端点必填 `Idempotency-Key`**：`POST /admin/api/v1/{providers,deployments,routes,projects}` 缺少该头返回 400 `idempotency_key_required`，重放返回 409 `<resource>_idempotency_replay` 并点名既有 ID（不回放旧记录）。Admin 面的键长上限是 256，与数据面契约的 1–128 不同。
6. **迁移 25/26 会丢弃已有的能力探测缓存**（`reset_capability_detections_*`）。对任何已发布或 schema ≤23 的实例这是空操作（探测桶创建于迁移 24）；只有跑过"24 之后、25/26 之前"未发布中间构建的开发实例会丢数据，后果是需要重新探测。

**能力与 Provider**

7. **Gemini / AWS Bedrock 为 Beta，能力上限被契约钉死。** 管理面目前**接受**超出 profile 默认的能力声明并落盘，但这些声明**在数据面永远不生效**——天花板由编译期常量执行，越界请求在任何 Provider I/O 之前被拒（400 `unsupported_feature`，不预留额度、不建连）。控制台上显示的能力若超出 profile 默认，以数据面行为为准。
8. **签名模型目录在 1.0.0 的发布构建中未启用**：`MODEL_CATALOG_TRUST_ROOTS` 未设置，`ProductionTrustRoots()` 为空，任何远程签名目录都验不过并回落到内置目录（fail-closed）。Admin 里看到 `trust_root_count: 0` 是预期值。`model_catalog.enabled` 默认 false。
9. **chat / embeddings 端点拒收一切未在 manifest 列出的请求字段**，包括 OpenAI 官方常用参数（`frequency_penalty`、`presence_penalty`、`logit_bias`、`logprobs`、`store`、`metadata`、`service_tier`），一律返回 400。从 OpenAI 直连迁移时需先核对字段集。

**记账与计费**

10. **能力探测、Provider 连接测试、健康探测产生的上游费用不计入 Halro 账本与预算。** 这些是管理面调用，只记 `ProviderCalls` 计数与 Prometheus 指标，不进 Ledger、不计入项目余额或用量统计。上限有界（每次检测 ≤12 次调用，每次 ≤2048 字节入 / 16 token 出），但运营者的上游真实账单会略高于 Halro 账本合计。
11. **含糊的 Provider 结果按保守方向结算**（`docs/contracts/gateway-correctness.md`）。若上游连接被中间设备接受后立即断开，Halro 无法证明请求未到达上游，会按预留的最大输出长度结算——这是有意设计。真正未连上的失败（连接被拒、DNS 失败、上游 5xx）结算为 0 并全额释放预留。
12. **优雅关闭预算硬编码 30 秒**，而默认 `route_total_timeout` 是 2 分钟。升级或重启时，在途的长请求（流式生成、Bedrock async、大 batch）可能被截断并按含糊结果保守结算。

**运行状态与可观测性**

13. ⚠ **`configuration_stale`：任一管理面变更提交成功但激活失败时，网关对全部 Project 的全部数据面请求返回 503 `configuration_stale`**，`/health/ready` 同步返回 503。每 5 秒自动重试恢复。**当前该状态没有 Prometheus 指标、没有告警规则、没有 runbook、控制台不显示**——监控上的表现是"流量归零 + 错误率 0 + target up"，与"今天没人调用"同形。运维需人工轮询 `/admin/api/v1/system/status` 的 `activation` 字段或 `/health/ready` 的响应体。
14. ⚠ **本版本的能力探测、签名目录、拓扑提交协议三个子系统没有出厂告警规则。** 10 个新指标已进 `metrics-reference.md` 但无对应 alert / rule test / runbook 条目。
15. **deadman 看门狗需要独立部署。** Core 的出厂 `compose.example.yaml` 只含 Prometheus + Alertmanager；不部署 deadman 时**不存在独立故障域的见证**。且 deadman 覆盖的是"Core 活着吗"，不覆盖"告警送得出去吗"。

**运维与恢复**

16. **从撤销之前的备份恢复，会让已撤销的 Gateway Key 重新生效，且恢复过程没有任何提示。** 泄露处置之后如需恢复更早的归档，必须在恢复后重新执行一遍密钥撤销清单。
17. **Master Key 轮换之后，轮换前创建的归档无法直接恢复**（`staged Vault belongs to a different Master Key than the backup manifest`）。自救路径是把 `storage.master_key.file` 暂时指回退役的旧 key 后重试。请记录每份历史归档对应的 Master Key 代次。
18. **KMS（`key_slots`）模式与三条 m11 runbook 为 release-blocked**，未经真实 AWS 账号矩阵、独立恢复演练与四方签署验证。默认的 file 模式已完整验证（备份/恢复/轮换/崩溃恢复实跑齐备）。

**容量参考（非承诺）**

19. 单机容量实测（macOS/APFS/`F_FULLFSYNC`，**这是地板不是天花板**，Linux/NVMe 会显著更高）：账目生命周期 1223/s @ 64 并发且随并发而非项目数扩展；管理写路径约 31 变更/s（受 bbolt 每事务全量 fsync 限制，且拓扑提交协议使其再降 25.3%）；长跑 2588 次管理变更 RSS 仅 +1.4 MiB，无内存与 goroutine 泄漏。**Admin 登录验证（argon2）每并发占用约 64 MiB 且无并发上限**——在没有 cgroup 内存限制的裸机/Docker 上，并发登录风暴可能触发 OOM；请保持 Admin 监听在回环并设置内存限制。24 小时 soak 的 `release_24h` 工件尚未归档。

**分发**

20. **没有官方容器 registry。** 容器镜像以 `halro-container.tar.gz` 作为 Release 附件分发，使用方式是 `gzip -dc halro-container.tar.gz | docker load` 后自行推送到私有 registry。`deploy/kubernetes/*.yaml` 中的 `image:` 是**占位符**（`ghcr.io/OWNER/halro@sha256:REPLACE_WITH_REVIEWED_DIGEST`），必须替换为你自己 registry 的 digest。

---

## 9. G7 的建议裁决、交叉证实与历史结论修正

### 9.1 跨角色交叉证实（review-plan §8 要求主动标注）

**① stale 机制 —— 本轮最强的交叉证实。** 五个互不知情的角色从五个方向指向同一个机制，且没有任何一个方向能独立发现另一个方向的问题：

| 角色 | 从哪个方向看到它 | 看到了什么 |
|---|---|---|
| **A1** | 记账与提交协议不变量 | 四个激活域共享一个标志，`markCurrent` 无条件清空；恢复循环的修复范围与 stale 的标记来源不对齐（A1-01，P1） |
| **A4** | 测试盲区 | 全仓仅有两处 stale 测试调用且都是直接注入；`runActivationRecovery` 零覆盖。**用探针从调用点走了一遍，撞出两处 fail-open**（A4-01/02，P1） |
| **C1** | 事故中能否自救 | 一个会让整台实例停止服务的状态，`docs/runbooks/` 零覆盖（F-C1-06，P1） |
| **C2** | 监控能否看见 | 无指标、无告警，且拒流形态是"流量归零 + 错误率 0 + target up"——**所有现有告警都看不见的形状**（C2-01，P1） |
| **D1** | 管理员能否理解 | 前端 `SystemStatus` **类型定义里都没有这个字段**（D1-02，P1） |

再加四条外围：A3-03（stale 进出无持久留痕）、A4-12（无 Prometheus 指标因而指标契约测试也无从守护它）、A6-07（语义只在 zh-CN 架构文档）、B2-02（503 无英文契约、Anthropic 路由用错信封）。**V1 裁决 CONFIRMED 且后果比原判更重。**

这条交叉证实的价值不在于"五个人都说它有问题"，而在于**它证明了一个机制可以在代码正确性、测试、监控、运维、交互五个平面上同时缺口，而每个平面的负责人都只能看见自己那一片**。1.0.0 之后，这类问题的修复成本会多一个兼容性维度。

**② `Idempotency-Key` 必填 —— 四个角色从四个面命中同一处改动。** A4-05（5 个 create 端点中 3 个零负面断言，一次重构就能改回旧语义而 CI 全绿）、A6-03（契约文档直接矛盾）、B2-01（英文契约面完全沉默，`idempotency-contract.md` 现为主动误导）、D1-01（前端撞 409 时提示误导且英文原句泄漏进 zh-CN）。**本轮唯一一处收紧对外语义的改动，其测试、契约、文档、交互四个配套面同时缺席。**

**③ 容器与分发 —— 交付面的同一处空洞。** B1-01（照文档跑不起来）+ B3-14（无 registry 推送、k8s 占位镜像）+ B4-F1（无公告）+ V2（CI 从不挂卷启动容器）。四个角色分别摸到这个空洞的四个边。

**④ 契约文档与代码的方向性不一致 —— 两个角色拼出完整因果。** A6-05 发现 `metrics-reference.md` 缺 3 个已导出的族；C2-03 发现**为什么这个缺失能长期存在**——门禁 `for family := range families` 只检查本次渲染出现过的族，对条件导出结构上是盲的。单看任一条都是"补三行文档"，合起来才是"门禁的判定方向反了"。

**⑤ 能力探测计费调用不进 Ledger —— 同一边界的三个面。** A1-06（裁决不阻塞、须公告）、A4-08（`MaxProviderCalls` 是这类支出的唯一上界，而那个上界的判定分支从未被测试执行过）、D1-03（文案说"may incur cost"但没说不进账，管理员最自然的理解是"会出现在我的用量里"）。

**⑥ 证据文档自称通过而没人对着真东西跑过 —— 一类系统性缺口。** `security-review-v1.md`（落后 469 提交、点名要求的威胁评审没发生）、`dependency-license-review.md`（AWS KMS 凭据面从未覆盖）、`crash-recovery-matrix.md:21`（为已删除的测试断言 Pass）、`check-production-assets.sh`（用 grep 当发布链门禁，结构上看不见环境审批）、`docs/review/progress.md`（rc.1 产物"全部成功"已不可复核）。这正是 review-plan §1.2 预判的失败模式："每份文档都说自己通过了，但没人对着真东西跑过一遍。"**本轮的实跑角色（B1/B3/B4/C1/C3 + 六条裁决）是这个预判的直接回报。**

### 9.2 与历史结论矛盾、或表明某项"已完成"需要撤销的发现

| # | 需要修正的历史结论 | 依据 | 处置 |
|---|---|---|---|
| 1 | **260811 API 链路 F-03「mutation/激活/审计三阶段无统一提交点」宣告关闭 —— 需部分撤销** | carry-forward §7 核实它有具名回归测试且实跑全绿；但 **V1 CONFIRMED**：提交协议自称的"激活失败让运行时进入 stale 并拒流直到快照追上"这半句在跨域场景不成立，且恢复循环会主动打开它 | 状态从"已关闭（有守护）"改为**"已修但语义未闭合"**，随 R-03 一并关闭。守护测试本身有效，缺的是覆盖跨域的那条断言 |
| 2 | **260807 P2-19「internal/app 不拆」的两个支撑事实已失效** | A6 §5：当时的"五把锁零外部引用、状态可整体搬走"现为 4/5（`adminTopologyMu` 被 `activation_state.go:118` 与 `providers.go` 持有）；"跨界符号约 11 个"现为约 27 个（≈2.5×），且新增的都不是工具函数而是提交协议与能力体系的核心动词 | **结论（1.0.0 前不拆）仍然正确**，但 **08-07 的边界表已过期，不得再被引用**。1.0.0 后第一个间隔必须做一次书面二选一 |
| 3 | **`docs/verification/crash-recovery-matrix.md:21` 为一个已被 `d0bb2b8` 删除的测试断言 Pass** | B3-08(ii)：`TestDeploymentMigrationSurvivesEveryInjectedKillPoint` 全仓 grep 只在该文档里有，Go 代码里没有；替代测试自己写明"the v3 route-to-deployment synthesis this test used to cover is gone" | **该行必须撤销或替换**。它是 M10 recovery 门的支撑证据之一，而这份矩阵为一段系统已不再具备的行为断言 Pass（同表另两行未指名任何测试，按原样不可核验） |
| 4 | **`docs/review/progress.md`（`7390b55`）关于 rc.1 产物"全部成功并已签名"的自述不可独立复核** | B3-02：run `31131173718` 已 404，日志/artifact/check run 全部消失；仅有的侧证是 deployment status 证明 publish 到达并失败（间接支持前置 job 曾成功，因为 publish `needs: provenance`） | 该自述**在机制上可信但已不可独立复核**。按 review-plan §1.2"证据链本身是被审对象"，这本身是 D8 的扣分项。处置：rc.2 的 run 不得删除，并在 `docs/verification/` 用 run id + artifact 摘要留痕而非散文描述 |
| 5 | **本轮方案自身的一处事实错误**：`role-prompts.md` §5 B4 段称"迁移 24~27 中的三条 `reset_`" | B4-F5 + V4 复核：HEAD 的 migrations 表中 `reset_` 前缀**仅 25、26 两条**，区间全量 diff 证实从未存在第三条。`review-plan.md` §2.1 的枚举是对的 | 记录在案，公告按"两条"如实措辞。**这条值得单独记：评审方案本身也是需要核对的前提** |
| 6 | **`_status.md` 记录的"B1 实跑中修改仓库"—— 待确认发现现已裁决** | V2：缺陷成立、**机制判断错误**（冲突不在挂载点本身而在 `filepath.Dir(dataDir)` 的发布锁）、**修复面被高估 4 倍**（Dockerfile / systemd / k8s 三项被证伪）。若当时那个补丁被采纳，会破坏实证 F 里已经工作的默认路径 | 补丁**不应采纳**。正确修复是 `operator-guide.md:497` 一行（R-08）。同时确认：`--platform=$BUILDPLATFORM` / `GOOS/GOARCH` 那部分**未复现出对应问题**（本仓 CI 与 release 均无 buildx / 多架构构建），不计为发现。**派发提示词"不要顺手改"的要求是对的**——那次改动如果留下，会把一条一行的文档缺陷变成一次破坏性的工件重构 |
| 7 | **B3-01 的取证论证需重写；B3-03 从 G4 判定表移出** | V5 §2.2、§2.3、§4.4 | 见 §3.4。B3 报告本身不再改动（评审后不改的纪律），修正记录在本报告与 `progress.md` |
| 8 | **A3-01 的安全结论应撤销** | V3：M2 REFUTED，严重度高估约两档 | P2 需裁决 → P3 建议。残余问题性质变为"管理面接受永不生效的声明"。**同时新增一条正面结论必须被保护**：`gemini.Options` / `bedrock.Options` 里没有 Capabilities 字段是**类型层面**的天花板保证，**不要为了"统一"给它们加上这个字段** |
| 9 | **B1-09 的核心主张应撤销** | V6：真正没连上的失败结算为 0（`d116f77` 已修，三条具名回归测试守护） | P2 需裁决 → P3 不阻塞。真缺陷更窄且方向保守（只多记不少记） |
| 10 | **A5 的"全仓未发现 P0/P1"需加一条边界说明** | A5 明确把 stale 归口给 A1 而未独立复核，恰好错过本轮最重的一条 | 结论本身不改（角色分工如此），但**记录一条方法论教训**：互不知情的独立角色遇到"归口约定"时，独立性会失效。下一轮的归口约定应写成"你仍需独立复核，只是不重复报" |

### 9.3 G7 的逐条建议裁决（17 条"仍开"）

> **本报告只能给建议。** 标 🔴 的必须由用户拍板——它们是产品取舍，不是技术判断。标 🟢 的建议裁决有明确的技术依据，用户确认即可。
> 三态：**接受**（明知而不做，理由记录在案）/ **修复**（进 §6 的整改清单）/ **写入已知限制**（发布说明公告）。

**来自 260807 遗留的 8 条**

| # | 条目 | 建议裁决 | 理由 |
|---|---|---|---|
| 1 | P1-6(b) argon2 并发信号量 | 🟢 **修复**（R-13）+ **写入已知限制**（§8 第 19 条） | C3 实测复现：16 并发即 1.06 GiB 越过 k8s 的 1Gi 兜底，裸机/Docker 默认无 limit。修复面小（一个小容量信号量），且在此之前必须公告"设内存限制 + Admin 保持回环" |
| 2 | P0-5 `syncUsageAdmin` 的 `WithoutCancel` + 独立超时；`applyMu` 不感知 ctx | 🔴 **需拍板：接受 or 修复** | 本轮无角色报告它造成实际故障；但 A1-04 独立发现了同一处 `applyMu` 的另一个问题（observer panic 会永久挂死全部记账）。**建议合并处理**：若修 A1-04 就顺手把 ctx 感知一起做 |
| 3 | P1-7 `PUT /credentials/{id}`、`PUT /redaction-policies/{id}` 的 step-up | 🔴 **需拍板：接受 or 修复** | A2 独立核实 step-up 在本仓被保留给**删除/销毁类**，这是一致的设计取舍而非遗漏。**建议接受并记录理由**；若要收紧，应与 A2-02 的"花费凭据类操作"一并纳入统一的 step-up 层，而不是逐个端点决定 |
| 4 | P1-10 锚点文件自身的 HMAC / 前向 hash 链 | 🟢 **写入已知限制** | 与 C2-04 交叉：`metrics.go:163-167` 的注释点名要求一条"锚点停摆"告警而该告警不存在（R-25）。**建议：告警先补（成本 4 行），HMAC 链留到 1.1.0 并在已知限制写明"锚点文件本身未做完整性链保护"** |
| 5 | P1-12 溢出预算随 `maxTracked` 缩放 | 🔴 **需拍板：接受 or 修复** | 本轮无角色触及，无新证据。建议**接受**并在 `internal/sourcelimit/limiter.go:112` 加一行注释记录取舍 |
| 6 | P2-21 `halro audit anchor rotate` 子命令、deadman 侧独立 token 文件 | 🟢 **接受** | 260807 已有取舍论证；本轮 C1/C2 的实跑未因它卡住任何 runbook |
| 7 | P2-28 fuzz 失败语料自动回灌成回归种子 | 🟢 **接受** | CI 已有 `::notice` 提示，手工提交可行；A4 未报告 fuzz 覆盖缺口 |
| 8 | P2-29 尺寸 ratchet 扩展到 TSX 内联样式 | 🟢 **接受** | D2 实证：`grep -n 'style={{' web/src/pages/*.tsx` 零命中，本区间无内联样式绕过；ratchet 有 40 的余量、单调下降。**风险已被行为证据压低** |

**来自 260809 方案 §5 的 9 条**

| # | 条目 | 建议裁决 | 理由 |
|---|---|---|---|
| 9 | ADR 0018 额度不变量在异常路径下是否成立 | 🟢 **已关闭** | A1 §5.2 独立走查 + `-race` 实跑，不再是作者自述。**本轮已给出结论，从 G7 队列出列** |
| 10 | 能力两个"安全阀"能否组合越权 | 🟢 **已关闭** | A3 §5 下限 #1 判否，V3 独立确认。**出列** |
| 11 | Bedrock 控制面 host 派生是否仍受 safetransport 约束 | 🟢 **已关闭** | A2 下限 2 逐条核实六个路径均经统一 policy，`pinnedDialContext` 在 dial 时复跑校验。**出列** |
| 12 | rc.1 publish 未运行的根因 | 🔴 **仍开 → 修复**（R-01） | V5：两个前置条件今天可验证地缺失；具体断点不可验证（run 已删）。**这是 G4 的内容，不是可接受项** |
| 13 | 能力选择 §15 剩余三条门禁的处置（浏览器验收 / 真实 Provider 证据 / `provider_metadata`） | 🔴 **需拍板** | 浏览器验收已完成 fixture 本地 RC；真实 Provider 证据是 billable，需单独授权（S1）；`provider_metadata` 代码里只有枚举值与校验、无任何 Adapter 发射它——**建议：前两条书面接受为 S 级软判据，第三条按 pre-1.0.0 纪律撤销该枚举值或补实现，不要留"已定义但无人发射"的占位** |
| 14 | 260807 遗留 8 条逐条裁决 | 见上表 1~8 | — |
| 15 | 能力探测计费调用不进 Ledger 是否阻塞 1.0.0 | 🟢 **接受 + 写入已知限制**（§8 第 10 条） | A1 §5.3 给出完整论证：管理面、有界、有台账三条同时成立。**附条件：不得以此为先例让未来任何数据面调用绕过 Ledger** |
| 16 | `security-review-v1.md` 自称 blocked 是否仍成立 | 🔴 **仍开 → 修复**（R-15） | B3-08：四道门全部未关，**且那句话是对的**——正确处置不是改措辞而是先关门。**同时它已不可执行**（M10 无定义），需要先给 M10 一个定义或改写门禁集合 |
| 17 | 1.0.0 已知限制清单进发布说明 | 🟢 **修复**（R-07） | 清单已备好（§8），成本是复制粘贴 |

**统计**：建议出列 3 条（#9/#10/#11，本轮已给出结论）、建议接受 4 条、建议修复 5 条、建议写入已知限制 2 条、**必须由用户拍板 6 条**（#2、#3、#5、#12、#13、#16）。G7 在这 6 条得到书面裁决之前判**未通过**。

---

## 10. 附录

### 10.1 `make check` 完整记录

见 §3.3。原始日志保存在会话 scratchpad（`make-check.log`），未写入仓库。

### 10.2 仓库洁净性核对

本轮汇总**未修改仓库里的任何其它文件**——没有动代码、发布说明、CHANGELOG、`docs/milestones/`、`docs/verification/`、`deploy/`，没有触碰仓库根目录的 `data/`、`master.key`、`config.yaml`，没有运行 `make reset`，没有跑任何计费 smoke test，没有打标签或触发 workflow。

```
$ git status --porcelain
?? docs/review/260811/releases.1.0.0/
```

<!-- GIT_STATUS_PLACEHOLDER -->

唯一一行是本轮评审目录本身（未跟踪），本报告与 `progress.md` 写入其中。已跟踪文件零改动。

### 10.3 本报告的执行信息

- **模型**：Opus 5（`claude-opus-5[1m]`），汇总角色。
- **执行日期**：2026-08-11。
- **拒答 / 空响应**：无。
- **本报告的一手实跑**：`make check`（§3.3）、`git diff --exit-code -- internal/webui/dist`、`git status --porcelain`、`git rev-parse HEAD`。其余全部结论来自 15 份角色报告与 6 份裁决，冲突处以裁决为准并已逐条标注"原判 X → 裁决后 Y"。
- **本报告未独立复核的部分**：各角色报告内部的 `file:line` 未逐条回查（§9 的空结论真伪核对已由主会话全量执行并通过，15 份产出每份都有 file:line、模型标注、附录三件套，全轮零拒答）。六条对抗验证的裁决已阅读全文并按其结论改写了原判。
