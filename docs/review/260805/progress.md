# 评审整改进度

> [260805.md](260805.md) 是一份有日期的发现记录，不改动。本文件是它的**活的对照表**：哪些做了、哪些没做、以及做的过程中改变了对原结论的判断。
>
> 编号沿用 260805.md 第十章的修复清单。最后更新：2026-08-07 收尾（清单结项；`internal/app` 拆 adminapi 决定不做，#86/#87 已关闭；首个 tag `v1.0.0-rc.1` 已打并推送）。

## 一句话状态

清单共 23 项，**已结项**：22 项做完，1 项（P2-19 拆 adminapi）经两次量化边界后决定**不做**并已关闭 issue。P0 四项、P1 八项、P2 十一项。全部改动已合并进 main 并推送，工作区干净。

2026-08-07 这一轮把此前**刻意推迟的两项和一项拆出来的新发现**做完了：数据面全局按源限流（`b4c8235`）、Admin 只读角色的前端落地（`817f7d9`）、step-up 推广到 9 个破坏性删除端点（`ca26a3c`）、`phase2` 标识符改名（`956c06b`）；随后修掉了 RBAC 落地暴露的历史数据缺口与两处面板布局问题（`1b33357`、`782b1ef`、`af08694`）。

整改过程中原报告的四条结论被修正，另外发现**八项**报告里没有的问题（全部已修完）。

## 已完成

每项都按同一套流程验证：写测试 → **把代码改回缺陷状态确认测试真的失败** → 恢复 → 目标包 race 检测 → 全仓无缓存套件。反向验证是必需步骤，不是可选的——它是唯一能证明"测试守护的是真问题"的手段。前端项另加 `make frontend` 重建 + `npm run typecheck` + 全量 vitest。**注意是 `npm run typecheck`（即 `tsc -b`）而不是 `npx tsc --noEmit`**——后者不检查测试文件，2026-08-07 这一轮它对一个"函数作用域里没有 `readOnly` 变量"的错误一声不吭，直到 vitest 运行时才炸。

| 编号 | 内容 | 提交 |
|---|---|---|
| P0-1 | 熔断器三态化：本地拒绝不再冒充上游健康 | `e989dbb` |
| P0-2 | safelog 覆盖 slog 的全部属性形状 + deadman logger | `83ebf07` |
| P0-3 | 登录限流前置 + 审计按分钟聚合 + 去重索引有界 | `4871de8` |
| P0-4 | 拆开"是否可重试"与"是否该计费" | `d116f77` |
| P1-5 | 认证前置到解析之前（部分，见下） | `effef81` |
| P1-6 | 让被拒绝的请求说清楚哪个字段错了 + 404/405 信封 | `203f5e5` |
| P1-8 | 首启体验：去 npm 依赖、master.key 警告、CLI 可读报错 | `d03c32e` |
| P1-9 | apply 失败后拒绝继续追加 + 共享状态标记不可用 | `3c2bc31` |
| P1-10 | Modal 脏检查上提 + Dashboard 首启 checklist + `routeBlocked` 接线 | `4476d93` `f7aa889` |
| P1-11 | 删除两个从未被调用的抽象（净删约 420 行，包数 57→56） | `61f8bd0` |
| P1-12 | 字体栈补 CJK、去掉从未打包的 Inter、8/9px 收敛到 12px | `da36316` |
| P2-13 | XFF 读全部 header 行 + Workbench 可达时启动告警 | `874eef0` |
| P2-14 | `startAttempt` 后的五条本地失败路径统一走 `abort()` | `28abffb` |
| P2-15 | 删除策略时，被停用项目持有的引用同样算引用 | `4f55cea` |
| P2-18 | SSRF deny 前缀表 + 告警 fan-out 上限 + 无效 CIDR 拒绝启动 | `1edc14c` |
| P2-22 | dead-man 探测移出状态锁 + 告警退避可取消 + SSE 裸 `\r` | `7dd39c3` |
| P2-17 | sse/openaiapi/safelog 三处 fuzz 目标 + 语料入库 + CI fuzz 作业 | `8c61229` |
| P2-21 | 焦点环收敛到一个 token、Light 二三级层级对调、尺寸 ratchet（见下） | `4cf84d6` |
| P2-20 | 网关时钟可注入、预算超限首次即短路、流式中断按已投递量计费、`reserved` 崩溃后可回收 | `d6601dc` `a7c905f` `4b61b49` |
| P2-23 | 版本注入 + `make version`、首启配置带注释、CHANGELOG 收敛、英文 user-guide（4/6，见下） | `f503d9c` `e0922dd` |
| P2-16 | ledger 帧 epoch 4：HMAC + hash 链，[ADR 0016](../../adr/0016-ledger-frame-integrity.md) Accepted。fail-closed 启动门禁、v3 gate 缺口一并补齐、backup manifest 链头字段、`heimdall ledger verify` 三态报告（见下） | `5d4936e` |
| P1-7 | 审计外部锚点，[ADR 0015](../../adr/0015-audit-chain-external-anchoring.md) Accepted。默认 sink = dead-man 拉取；bbolt 实例身份、有界锚点环、metrics 监听器上独立凭证端点、deadman 侧增量拉取+持久化高水位、`heimdall audit verify-anchor` 三态报告（见下） | `5d4936e` |
| P2-23 | Parquet 降级为可选格式（[ADR 0017](../../adr/0017-usage-export-format.md) Accepted）+ Admin 多用户登录、管理员/只读两级权限、管理员账户创建/删除的 step-up（见下"P2-23 验证记录"） | `5d4936e` |

## 决定不做（清单唯一一项）

| 编号 | 内容 | 结论 |
|---|---|---|
| P2-19 | `internal/app` 拆 adminapi 子包 | **不做，issue 已关闭。** 同项下的 `store.go` 按数据域拆（5340→1025 行）、`phase2` 改名（`956c06b`）、57 行公共助手外提（`9ee0fc3`）三个子项都已完成，剩下的只有拆包本身：41 个文件、12062 行非测试代码 + 4699 行测试，全部落在 CLAUDE.md 点名要保护的 admin mutation 路径上，收益是零功能零安全。边界已量清并记在下方"P2-19 验证记录（2026-08-07 复量）"，将来若要做不必重新摸 |

## P2-16 验证记录（2026-08-06，本轮完成）

`docs/adr/0016-ledger-frame-integrity.md` 状态改 Accepted，三项 Open decision 已拍板并落地（见 ADR 本身）。实现范围：

- `internal/ledger/log.go`：`frameVersionLedgerIntegrity = 4`，24 字节原头不动，v4 帧追加 `previous-hash[32]+MAC[32]`（共 88 字节头）；CRC 覆盖范围不变，MAC 覆盖 `header[0:24]||previousHash||payload`；`eventFrameVersion` collapse 成无条件返回 v4；`Log` 新增 `chainKey`/`chainHash`/`chainSequence` 状态，`ChainHead()` 导出；新增 `VerifyChain` 做三态（authenticated/checksum-only/failed）全量校验。
- 密钥：`internal/vault/ledger.go`（`DeriveLedgerHMACKey`，HKDF salt/info 独立于 audit）+ `internal/app/vault_material.go`（`loadLedgerHMACKey`/`encryptLedgerHMACKey`，逐行照抄 audit 的信封两层结构）。`init.go`（file 模式首启）、`kms_master_key.go`（key_slots 模式首启，`KeySlotInitialization` 新增 `LedgerHMACEnvelope`）、`key_rotation.go`+`kms_key_lifecycle.go`（两条轮换路径都验证轮换后旧帧仍可验证）全部接上。
- 链头 checkpoint：`boltstore.LedgerChainCheckpoint`，`reconcileLedgerChainCheckpoint` fail-closed 挂在 `runtime.go` 主启动路径 `ledgerLog.Replay` 之后，镜像 `reconcileAuditCheckpoint`。
- v3 gate 缺口：migration 17（`ledger_frame_integrity`）把 `keyLedgerFeatureEpoch`/`keyMinimumLedgerReaderVersion` 从 2 直接提到 4；`runtime.go`/`internal/backup/archive.go` 的门禁判断从精确相等改成"至少 N"+字符串按 epoch 派生，不再重复"精确相等导致漏保护"这个坑本身。
- backup manifest：新增 `LedgerChainHeadSequence/Offset/Hash/Verified` 四个字段，restore 侧链头不一致直接拒绝恢复（`internal/app/backup.go`）。
- CLI：`heimdall ledger verify --config <path>`（`internal/app/ledger_verify.go` + `cmd/heimdall/main.go`），已跑通 smoke test（init → verify，空账本报告 `ChainVerified:false` 符合预期）。
- 测试：`internal/ledger/log_test.go`/`period_identity_test.go` 全量适配 v4（`validReservation` 等 fixture 补 `PeriodTimezone`），新增 `chain_test.go` 三个对抗测试——伪造 payload+重算 CRC 被 MAC 拒绝、删除中间帧被链拒绝（序列号本身不够，验证了这点）、密钥"轮换"（信封换、明文不变）后历史帧仍可验证。**反向验证做了**：把 `verifier.verify()` 判断临时改成 `false`，确认这三个新测试和混合 epoch 测试全部会失败，再恢复。级联修复：`internal/budget`/`internal/gateway`/`internal/usage`/`internal/store/bolt`/`internal/backup`/`internal/app` 里凡是直接开 ledger 或构造裸 `Event` 走 Append 的测试都补了 chain key / `PeriodTimezone`。
- 验证：`go test ./...`、`go test -race` 覆盖到的包（ledger/app/store/bolt/backup/budget/gateway/usage）全绿，`go vet ./...` 干净。

## P1-7 验证记录（2026-08-06，本轮完成）

`docs/adr/0015-audit-chain-external-anchoring.md` 状态改 Accepted，默认 sink 定为 A（dead-man 拉取），ADR 末尾"Implementation notes"记录了实现细节和范围收窄的地方。实现范围：

- 实例身份：`internal/store/bolt/audit_anchor.go` 的 `Store.SeedInstanceID`（bbolt 首启生成 UUID，之后调用只读不改，镜像 `SeedInstanceAccountingSettings` 的"只种一次"写法）。
- 锚点存储：新桶 `bucketAuditAnchors`（migration 18），`AppendAuditAnchor`（序列号必须连续、按序列号算术裁剪到最近 1000 条而不是靠 `bucket.Stats()`——后者在同一事务内不反映未提交的增删，曾经把整个桶删空，过程中改过来了）、`AuditAnchorsSince`、`LatestAuditAnchor`。
- 发射：`internal/app/audit_anchor.go` 的 `runAuditAnchorMaintenance`，10 秒轮询（`var` 不是 `const`，测试可调快），满足"到间隔"或"过记录增量"任一条件即发射，fail-open（失败只记日志计数，绝不挡 audit append 或网关流量）。
- 端点：`GET /audit/anchors?since=<seq>` 挂在现有 metrics 监听器上，认证复用 `internal/metricsauth` 整个包（换一个凭证文件路径就是一条独立的轮换生命周期，没有另起一个包）。
- deadman 侧：`TargetConfig.AnchorURL`（仅 `heimdall` kind）+ `Config.AnchorFile`；`Engine.Checker` 签名塞不进 payload，锚点拉取是 `Tick` 里独立于 `probeTargets` 的一段（`pullAnchors`），JSON-lines 落盘（`anchorWriter`，镜像既有 `auditWriter`），高水位存进已有的 `TargetState`，重启不重放旧锚点。
- 验证命令：`heimdall audit verify-anchor --config <path> --anchors <path>`，重放本地审计链，按序列号比对锚点的 `LastHash`，报告 agree/disagree/truncated。CLI smoke test 跑通（一个声称 999 条记录的伪锚点，对着 0 条记录的新实例，正确报 `truncated`）。
- 测试：`internal/store/bolt`（锚点环增删裁剪+边界）、`internal/deadman`（拉取成功+增量+跨重启保序、端点不可达不挡探测循环+心跳、config 校验）、`internal/app`（端点鉴权隔离——metrics token 认证不了锚点端点、`since` 语义、record_delta 触发发射、启动告警、`VerifyAuditAnchors` 的 agree/disagree/truncated 三态）全部覆盖。**反向验证做了**：把 `VerifyAuditAnchors` 的哈希比较临时改成永远为真，确认"伪造锚点"用例会失败，再恢复。
- 验证：`go test ./...` 全绿，`go vet ./...` 干净；`go test -race` 覆盖 app/config/deadman/store/bolt 四包。

**范围收窄**（ADR 已记录）：sink B（syslog）、C（S3 Object Lock）只做了配置占位（校验直接拒绝"未实现"），没做实现——本轮只交付 A。deadman 侧锚点凭证与探测端点共用同一份 `bearer_token_file`，操作员需要自己把锚点凭证的活跃 token 同步进那份文件，没有做自动同步。

## P2-19 验证记录（2026-08-06，一项完成两项未完成）

> 后续：`phase2` 改名已于 2026-08-07 完成（`956c06b`）；拆 adminapi 的边界已在同日重新量过，结论见上方"P2-19 验证记录（2026-08-07 复量）"。以下保留 08-06 当时的判断原文。

三个子项风险不对等，分开处理：

**`store.go` 按数据域拆（已完成）。** `internal/store/bolt/store.go` 从 5340 行拆到 1025 行，新增 8 个同包文件（`store_admin.go`/`store_audit.go`/`store_keys.go`/`store_pricing.go`/`store_projects.go`/`store_providers.go`/`store_settings.go`/`store_usage.go`），按方法名关键词分域。用 `go/parser` 抓每个顶层声明的精确字节范围（含 doc 注释）按名字分桶写出，全部类型/变量/常量（含 `migrations` 迁移表）留在 `store.go` 本体不动，只搬方法体；`goimports` 收尾清理每个新文件的 import。这样做安全性有保证：同包内符号搬到哪个文件不影响可见性，唯一能出错的地方是脚本本身的字节切分，而这一步会被编译器立刻捕获（漏了/多了/重复了都是编译错误）。验证：`go build ./...`、`go vet ./...`、`gofmt -l` 全干净，`go test ./...` 全绿，`go test -race ./internal/store/bolt/...` 绿。过程中脚本第一版有个 bug（import 块被写了两次导致 `store.go` 语法错误），发现后从备份恢复重跑，不是留着让编译器"以后"发现。

**`internal/app` 拆 adminapi、"phase2" 改名——都没做，是本轮唯一没有兑现"全部完成"的两项，原因写清楚：**

两者都是纯组织性改动，不改变任何行为、不产生任何安全或功能收益——这一点从一开始就知道，但只有实际动手拆最简单的一个候选域（`admin_projects.go`，477 行，Explore 阶段判断是耦合度最低的域）才看清代价有多大：这个文件里除了项目/网关键的 CRUD，还定义了 `requireRevision`、`adminMutationError`、`adminBadRequest`、`adminBadRequestCode`、`adminPreconditionFailed`、`adminAuditError`、`refreshAdminAuth`、`auditAdminMutation` 这些**被其余十几个 admin_*.go 文件共用**的通用助手——不是"项目模块偶尔用到"，是整个 admin mutation 路径的公共基础设施长在了被认为最容易搬的那个文件里。真拆包意味着要么先把这些助手挪到一个新的公共子包（那它自己就是一个需要设计导出面的模块），要么每个新子包各自导入它——两条路都不是"移动文件"，是要对着 20 个文件、11533 行逐一设计导出接口的活。

这类改动一旦做错，编译器不一定能兜底——比如把 `adminMutationError` 错误分类的一个分支漏调，或者把某个 mutex 的粒度在搬迁中意外改变，这些是编译通过、测试如果没覆盖到那条分支也通过、但实际行为悄悄变了的那类 bug。CLAUDE.md 把这个仓库的优先级写得很直白："Security, accounting correctness, and backward-compatible API behavior take priority over feature count"——admin mutation 路径正是这句话点名要保护的东西。用剩余时间去做一次仓促的、零功能收益的重构去冒这个风险，划不来；这些时间挪去做 Phase 4（RBAC、Parquet）更值——那两项是这轮唯一有真实功能/安全价值的剩余工作。

**"phase2" 改名**同理但风险构成不同：命中 `internal/domain`、`internal/compatibility`、`internal/provider`（+`openai`）、`internal/gateway`、`internal/gatewayapi` 共 5+ 个包，24 个文件（含测试）。相比 adminapi 拆分，这个改动确实可以完全被编译器兜底（改名不对会直接报 undefined），风险主要是纯体力活的规模，以及一个必须手工守住、编译器管不到的硬约束——`internal/store/bolt/store_admin.go`（原 store.go）里 `{version: 6, name: "phase2_capability_evidence", ...}` 这个已经在真实部署上跑过的迁移历史字符串字面量绝对不能碰。本轮评估后判断和 adminapi 拆分一样优先级最低，一并推迟，留给下一轮单独执行——这项风险低，适合作为一个独立、专注的小改动去做，不该和其它工作混在一起仓促收尾。两项一并跟踪于 [#86](https://github.com/akz142857/Heimdall/issues/86)。

## P2-19 验证记录（2026-08-07 复量）

08-06 的判断是"拆 adminapi 代价太大、编译器兜不住"。2026-08-07 重新量了一次边界，结论**比当时乐观**，理由如下——记在这里是为了下一轮不必重新量：

| 量到的事实 | 含义 |
|---|---|
| 5 个 admin 互斥锁（`adminTopologyMu`/`adminProjectMu`/`adminSettingsMu`/`adminIdentityMu`/`adminAlertMu`）在 `admin_*.go` 之外**零引用**（只有 struct 里的声明） | 状态可以整体搬走，不存在"锁粒度在搬迁中被意外改变"这一类编译器兜不住的风险 |
| `auditBatch*`、`providerModels`、`setupMu`/`setupToken`、`adminLogin`/`adminSetupRate`、`adminStepUp` 同样零外部引用 | 同上 |
| `admin_*.go` 触及的 Runtime 字段约 25 个（`r.store` 占 200 次） | 依赖面可枚举，不是无限发散 |
| 跨界符号约 11 个：`appendAdminAudit`（被 `alerts.go` 用）、`reloadProviderRegistry`/`adapterForDeployment`/`matchingBindingID`/`normalizedProviderCapabilities`（`providers.go`）、`loadAlertEndpoints`/`webhookPolicy`/`webhookAudienceSubject`/`webhookCredentialType`（`alerts.go`）、`checkpointAudit`、`writeTimeContext`、`writeJSON` | **这一格是修正过的**：最初只抽查了几个符号就写成"只有两个"，全量扫描后是约 11 个。仍然可枚举、仍然不是"十几个文件互相纠缠"，但比第一次记的要多，据此定计划的人应当按 11 个算 |
| 08-06 说"通用助手长在最容易搬的文件里" | 属实，但那 5 个助手（`admin_projects.go:436-480`）**只有 57 行、全是无状态自由函数**，搬进新包做导出函数是机械操作 |
| 已有 `TestFrozenV1AdminRoutesAreRegistered` + 48 路由 `chi.Walk` 只读扫描 + 本轮新增的破坏性删除 step-up 扫描 | 路由接线、角色校验、step-by-step 覆盖三张回归网，正好覆盖搬迁最怕出错的地方 |

搬迁本身有一个让编译器全程兜底的写法：让 `adminapi.Server` 的字段**沿用 Runtime 里同名的字段名**，这样 `func (r *Runtime)` → `func (s *Server)` 加 `r.` → `s.` 就是纯文本替换，任何漏搬的字段都是编译错误而不是行为漂移。

建议的做法（四个各自可编译、可全绿的提交）：① 57 行助手外提——**已完成**（`9ee0fc3`，先在同包内独立成 `internal/app/admin_errors.go`，跨包那一步留给 ②）；② 定义 `adminapi.Deps` + `New()`，把审计批处理整体搬进去并导出 `Server.AppendAudit`，`alerts.go` 改为通过它调用——这一步就解决了两个跨界符号里的一个；③ 按域搬（alerts → usage/resources → prices/adjustments → providers/credentials/deployments/routes → projects/keys → session/mfa/setup/users），每域一次提交、搬完立刻跑三张回归网，不攒到最后；④ 20 个 `admin_*_test.go`（4699 行）跟迁。

**硬性纪律**：全程不得夹带任何行为修改。任何"顺手改改"都要另开提交，否则 review 无法把"移动"和"改动"分开看——而这正是 08-06 判断风险高的根源。

这一项仍然是零功能收益，**不做也是一个可以辩护的结论**；上面这些是为了让"做"的那次不必从头摸边界。

**2026-08-07 的决定：不做，[#86](https://github.com/akz142857/Heimdall/issues/86) 已关闭。** 边界量清楚之后重新权衡的结果——改动面是 41 个文件、12062 行非测试代码加 4699 行测试，全部落在 CLAUDE.md 点名要保护的 admin mutation 路径上，而收益是零功能、零安全。阶段一（57 行助手外提）已经单独完成（`9ee0fc3`），它本身就有价值，不依赖后续是否拆包。关闭而不是挂着：一个永远排不到的 issue 只会让人误以为这里还欠着东西——真要做，上面那张边界表和四步做法就是完整的起点，重开一个新 issue 即可。

## P2-23 验证记录（2026-08-06，Parquet 完成；RBAC 完成，step-up 覆盖面部分推迟）

两个子项风险构成完全不同，分开记：

**Parquet 依赖降级（已完成）。** [ADR 0017](../../adr/0017-usage-export-format.md) 状态 Accepted：NDJSON 作为*新增*可选格式而不是替换——`usage.export_format` 配置项选新分区写成什么，已写的 `.parquet` 分区永不重写（跟 ledger/audit"已有字节不重写"是同一条纪律）。`ManifestFile`/`AdjustmentManifestFile` 新增按文件记录的 `Format` 字段，空值向后解读为 `parquet`（跟 `SchemaVersion` 现有的向后读约定一致）。`parquetAttempt`/`parquetAdjustment` 系列结构体加 `json:` tag 跟现有 `parquet:` tag 并存，`writeNDJSONAtomic`/`readNDJSONFile[T]` 复刻 `writeParquetAtomic` 的临时文件+fsync+原子改名+目录fsync 序列。`internal/app/backup.go` 的 restore 路径不需要改——它只调用 `exporter.LoadManifest()`/`exporter.Verify()`，格式判断已经封在 manifest 内部。测试：`internal/usage/ndjson_test.go`——NDJSON 分区可发布可验证可幂等重导出、篡改可被发现、Parquet+NDJSON 混合 manifest 能对着同一次 ledger 重放一起验证/对账、无 `Format` 字段的旧 manifest 条目仍按 legacy 解读为 parquet 并能正常验证（本轮补的第 4 个测试，覆盖 ADR"必需验证"清单里原本没有直接测试守护的一条）。**反向验证做了**：把 `format()` 的空值兜底从 `FormatParquet` 改成 `FormatNDJSON`，确认新测试失败，再改回。`go build`/`go vet`/`gofmt -l` 干净，`go test ./internal/usage/...`、`-race` 覆盖 usage/backup/config 三包全绿。ADR"必需验证"清单里的 crash-injection（部分写入/漏 fsync/改名中断）没有补——现有 `writeParquetAtomic` 本身也没有这类测试先例，NDJSON 不比 Parquet 更缺，不是这轮引入的新缺口；单独的"归档目录混合格式 restore 演练"也没做成独立测试，因为读代码已经确认 `internal/app/backup.go` 不需要为格式改动任何一行——这条本身就是设计能立住的证明,量级上不值得为一个已知不会分支的路径新写一个全链路集成测试。

**Admin 多用户登录 + 两级权限（已完成，范围按用户明确要求收窄为"多用户登录 + 管理员/只读两档，不做细粒度权限矩阵"）。**

- **发现的前置缺口**：`store.AdminUserCount`/`store.CreateFirstAdmin`（`admin_setup.go`/`admin.go`）原本在 HTTP 和 CLI 两条路径上都硬编码"已存在管理员就拒绝再创建"——"两级 RBAC"字面上只是加个角色枚举，实际做起来发现整个仓库压根没有"第二个管理员"这个概念，比原评审设想的范围大得多。方案是保留 `CreateFirstAdmin`/首启 setup 路径原样（仍是一次性 bootstrap，只认零管理员状态），在它之上新增一条平行的"已认证 administrator 创建后续用户"路径（`createAdminUser`），两条路径不冲突：一个只在冷启动时可达，一个只在有会话之后可达。
- `domain.AdminUser` 新增 `Role`（`administrator`/`read_only`，`ValidAdminRole` 校验，无第三档）；`adminauth.NewUser` 签名加 `role` 参数。
- 存储层：`boltstore.ListAdminUsers`（按用户名排序）、`DeleteAdminUser`（复用既有的 `deleteAdminIdentityRecords` 做会话/MFA 清理，而不是新写一份——过程中发现自己最初手写的清理逻辑对 recovery-code 桶的假设是错的，实际是跟 authenticator 桶同构的扁平 `username\x00id` 前缀，不是嵌套桶）。
- 中间件按"是否操作自己的账户"一分为二：`requireAdminMutation`/`requireAdminSetupMutation`（新增 `requireAdministratorRole` 校验，`read_only` 一律 403，`code: "read_only_role"`）保持给跨用户/系统配置的写操作；新增 `requireAdminSelfMutation`/`requireAdminSelfSetupMutation`（只做会话+CSRF，不做角色校验）专门给自己账户的操作——logout、改自己密码、自己的 MFA 管理、自己的 preferences。这条区分是实现过程中自己发现的设计问题，不是用户反馈：最初"写操作一律拒绝 read_only"的方案会把 read_only 用户连自己的密码都改不了、也退不出登录，明显不对，在写测试之前就改掉了。
- 新端点：`GET/POST /admin/api/v1/admin-users`（`administrator` 权限 + step-up：创建新用户要求重新提交当前密码+新鲜 TOTP，逐请求校验，不发短期提权 token——跟 `admin_prices.go` 已有的 `verifyPricingReauthentication` 是同一个函数，只是换了个更贴合语义的调用名）、`DELETE /admin/api/v1/admin-users/{username}`（同样 step-up；拒绝自删——用 session/logout 结束自己的访问；拒绝删掉最后一个 administrator，否则系统会陷入"零管理员，只能靠离线 CLI 破窗"的更大故障）。
- 测试（`internal/store/bolt/admin_users_test.go` 三个 + `internal/app/admin_users_test.go` 三个）：`ListAdminUsers` 排序正确、`DeleteAdminUser` 清掉会话与 MFA 状态但不影响其他管理员、revision 冲突和用户不存在两种失败路径；HTTP 层创建/删除的 step-up 正确路径与错密码/无效角色拒绝路径、新用户能实际登录、自删/删最后管理员被拒；**表驱动 read_only 全路由扫描**——用 `chi.Walk` 遍历 `adminRouter()` 全部已注册路由而不是手写清单（这样以后新增写接口默认就在覆盖范围内，不会漏），对每条非 GET/HEAD/OPTIONS 且不在"自服务"白名单里的路由，read_only 会话必须拿到 `403` 且 `code` 精确等于 `read_only_role`（不是被 CSRF 或别的校验先挡住而误判通过）。扫到 48 条真实挂载的写路由（已排除 chi mount 点自带的 405/404 兜底桩和白名单内的自服务路由）。**反向验证做了**：把 `requireAdministratorRole` 的判断临时改成 `if false && ...`，确认表驱动测试和 `TestDeleteAdminUserRejectsSelfAndLastAdministrator` 都会失败（48 条路由全部报"read_only 到达了 handler"），再改回。`go build`/`go vet`/`gofmt -l` 干净，`go test ./...` 全绿，`go test -race` 覆盖 app/domain/adminauth/store/bolt 四包。既有的 `TestFrozenV1AdminRoutesAreRegistered`（只断言列出的路由存在，不检查"仅此而已"）未受影响，跑了一遍确认仍绿。

**推迟未做，原因写清楚（跟踪于 [#87](https://github.com/akz142857/Heimdall/issues/87)）——以下两条都已在次日（2026-08-07）做完，`ca26a3c` + `817f7d9`，#87 已关闭；保留原文是因为"当时为什么先不做"本身是这份记录要留住的东西：**

- **把 step-up 从"管理员账户创建/删除"推广到 project/credential/provider/route/deployment/redaction-policy/token-guard-policy/alert 的 delete。** 计划里原话是"复用同一函数模式"，但摸了这 8 个 handler 后发现前提不成立：它们全部走 `requireRevision`（`If-Match` header 表达乐观锁），**没有请求体**——不是"复用模式"，是要把这些端点从"无 body 的 DELETE"改成"要求 JSON body 携带 current_password/totp_code"，这是一处破坏性的 API 契约变更。Admin 前端现在发的 DELETE 请求不带 body，backend-only 上线这个改动会让现有的删除按钮当场变成 401——这不是"功能没做全"，是会让已经在用的功能倒退。CLAUDE.md 把"backward-compatible API behavior"列为优先于功能数量的第一条，前端改动又明确排除在本轮范围外（见下）——两者叠加，做这件事的唯一负责任方式是连前端一起改，但那超出了本轮"仅后端"的既定范围。MFA 相关的两个破坏性端点（`deleteAdminMFAAuthenticator`、`disableAdminMFA`）核查后确认**已经**各自内联了等价的密码+TOTP 校验，不需要额外补；`executeAdminDeveloperRequest` 的请求体是要透传给上游 LLM 的实际请求负载，结构上塞不进 step-up 字段，且它的风险已经在"整改过程中新发现的问题"里按"可达时告警"处理过，不属于同一类缺口。
- **Admin 前端 UI**（角色选择器、只读态提示、按角色禁用写操作按钮）：整轮延续此前"仅后端"的既定范围，没有对应前端改动计划，此处如实记录而不是留空不提。

## 2026-08-07 这一轮（四项，全部已提交）

同样按"写测试 → 把代码改回缺陷状态确认测试真的失败 → 恢复 → race → 全仓无缓存套件"验证，前端项另加 `npm run typecheck`、全量 vitest、`make frontend` 重建并提交 `internal/webui/dist`。

**数据面全局按源限流（`b4c8235`）。** 这是 P1-5 拆出来的最后一项，也是三项未完成里唯一没有 issue 跟踪的。新增 `internal/sourcelimit` + `gateway.source_rate_limit` 配置（默认 600/min/源），中间件 `LimitOpenAI`/`LimitAnthropic` 挂在 guard **之前**——顺序本身是不变量，用组装好的 router 断言（伪造 key 的第三次请求必须是 429 而不是 guard 的 401），单元测试看不到这个顺序。设计要点三条：① 计数 map 有界，超过 `max_tracked_sources` 的地址共用一个溢出预算并单独计量，否则限流器自己就是它要防的放大器；② 解析不出来的来源记到零地址而不是放行，否则畸形 XFF 就是绕过限流最省事的办法；③ 健康检查**刻意**不限流——编排器从单一地址定频探测，按源限流只会把健康实例标成 unready。`Decode` 不与 `Default()` 合并，所以缺失的配置段不能拒绝启动：校验放行、`Normalize` 补追踪上限、`doctor` 对"限流器被关掉"报 warn。

**Admin 只读角色的前端落地（`817f7d9`，#87 的一半）。** 实现前发现一个比 issue 描述更靠前的缺口：**五条签发会话的路径没有一条返回 `role`**，前端根本无从得知自己是不是只读。补齐五条并用一个测试同时覆盖（登录、MFA 完成、会话回读、改密、首启 setup），而不是只测登录。写操作按流程**入口**收口而不是逐个控件——弹不出的表单不需要禁用提交按钮；`ConfirmButton` 承载全部破坏性操作，这一处就是"下一个破坏性按钮默认不会漏网"的保证。自服务保持可用（改自己的密码、自己的 MFA、自己的外观与语言），与服务端 `requireAdminSelfMutation` 对齐；实例级设置（运行时、记账时区、默认语言）照常禁用。Settings 新增"管理员账户"面板，新账户默认 `read_only`。

**step-up 推广到 9 个破坏性删除端点（`ca26a3c`，#87 的另一半）。** 08-06 推迟的理由是"这是破坏性 API 变更、前端会当场 401"。理由本身没错，但两个事实改变了它的分量：本仓库**已有** DELETE 带 body 的先例（MFA 的两个端点，前端在用），且**至今没有任何 git tag**——首个 tag 之前这只是改默认，之后才是要写迁移说明的破坏性变更。所以正确的时间窗口就是现在，且必须在 `v1.0.0-rc.1` 之前。前后端同批改：`ConfirmButton` 在"说明后果"的同一个对话框里收凭据。

推广过程中暴露了原语自身的缺口（不在原报告内）：`verifyPricingReauthentication` **既不限流也不审计失败**。在一个已认证会话背后，这是个离线速度的口令 oracle——cookie 是有效的，请求路径上没有任何东西会拖慢一次猜测，Argon2id 只是让每次猜测对服务端更贵。改为每账户每分钟 5 次失败、按窗口审计一次（不是按次，否则审计追加本身成了放大器），**只计失败不计成功**——预算是用来限制猜测的，而证明了自己身份的操作员没有在猜；按次计会让清理六个资源的操作员做对事情却被锁在半路。原语改名 `verifyReauthenticationMaterial` 且不再是任何人的入口，pricing/adjustment 四个调用点一并挪到有界路径上（按端点各给一份预算，等于让攻击者在每个端点重新猜一遍）。覆盖面从 router 扫出来而不是手写清单，以后新增的删除端点注册当天就在范围内。

**收尾三个提交（`1b33357`、`782b1ef`、`af08694`）。** 都是上面四项落地后自己暴露出来的，不是新工作：老账户 role 回填（见"新发现的问题"表最后一行），以及新增的"管理员账户"面板两处布局——状态提示与 step-up 字段各自成行、账户列表与面板标题对齐。前端项按同样的流程验证并重建了 `internal/webui/dist`。

**`phase2` 标识符改名（`956c06b`，#86 的一半）。** 55 个标识符、25 个文件。新名字不是起的，是代码自己早就公布了的：北向 profile 是 `heimdall.inference-resources.v1`，所以门面与机器件叫 `InferenceResources`；OpenAI 服务商 profile 自己的值是 `openai.media-resources.v1`，所以那个常量叫 `ProfileOpenAIMediaResources`——按 wire 值 grep 现在能找到符号。**不跨线也不落盘**：profile ID 从来没有以 `phase2` 的形式持久化或发布过，`docs/compatibility/endpoint-manifests.json` 里这个字符串命中数为 0。三处字面量刻意保留旧名——bbolt 迁移 `phase2_capability_evidence` 及其两个 step 名，那是每个已经跑过它的实例里的历史记录，改了会让升级实例与自己的迁移日志对不上；现在有测试钉住，下一次"顺手清理最后几处"会失败而不是悄悄成功。

## 时区升级影响核查（2026-08-06）

`feat/timezone-governance`（PR #82）+ 后续 UI 修复（PR #85）落地后，核查是否影响上面四项未完成任务：

- **与 P2-16 冲突，已处理。** 时区改动为记账周期自描述（zone/version/UTC 区间）新增了 ledger WAL frame version 3（`internal/ledger/log.go:23` `frameVersionPeriod`），跟 ADR 0016 原提案要用的"frame epoch 3"撞号。ADR 0016 已改为 epoch 4，v3 并入 v1/v2 永久 checksum-only 档。帧头布局不冲突（仍是 24 字节头，period 字段走 payload JSON，MAC/chain-hash 走头部扩容）——冲突只在版本号，已修好。
- **与 P1-7 不冲突。** 时区改动自带审计记录（双 zone、双 version、生效时刻），不碰审计锚点机制本身。
- **与 P2-23 的 Parquet 降级不冲突。** 只改了 `internal/usage/query.go` 的按周期查询逻辑，没碰 ADR 0014 定的两个 Parquet manifest 版本/水位。
- **与 P2-19 弱相关，不算冲突。** `internal/app` 又新增 `admin_accounting_settings.go`、`time_context.go` 等文件，待拆分的体积又涨了一点，纯记账，不改变"没做"的结论。

这是本文件存在的主要理由——报告是 8 月 5 日的判断，实现过程中有三处发生了变化。

**P1-5 的 `WriteTimeout` 建议是错的，没有照做。** `http.Server.WriteTimeout` 从读到请求头开始计时，挂上去会切断每一条 SSE 流。代码里没有它不是疏漏——流式路径用 `SetWriteDeadline` 逐写设限，那才是流式服务器的正确机制。但评审指出的底层问题为真：**非流式响应确实没有任何写超时**。这一条已拆为独立项并**已修完**（`df05301`）：在网关路由上包一层 writer，在响应真正开始时才 arm 截止时间——在请求进来时 arm 同样是错的，上游有权花掉 `routeTimeout`，那比写预算长得多。

**P1-5 的全局限流未做。** 需要新增配置项（速率、突发、校验、文档），应独立成一个改动而不是塞进认证前置里。

**Redaction/TokenGuard 悬空引用 fail-open 已被证伪**（对抗验证第十一章 #2），从 P0 降为 P2 加固项。攻击链在"重新启用项目"时被 409 拦截，另有两层 fail-closed 纵深。

**Developer Workbench 的源 IP 伪造在默认配置下被证伪**，从 P0 降为 P2。绕过网络隔离一条成立，但需要已认证管理员身份。

**`startAttempt` 资源泄漏的后果被证伪**，从 P0 降为 P2。缺 defer 清理属实，但三处是不可达死代码，两处被现有 adapter 绕过。修的时候这一点被再次确认：想通过 `ChatStream` 走到那条分支是走不通的——严格模式的流式拒绝发生在 attempt 创建**之前**（`AllowsStreaming` 早于 `startAttempt`），所以测试直接测共用的 `abort()` helper 本身。

**Developer Workbench 没有改默认关闭，改成了"可达时告警"。** 评审给的是"默认关闭**或**文档明示"，文档已经写了。对抗验证把它收窄为"已认证管理员的横向能力"，而绕过只在 Admin 监听器可被别处访问时才成立；loopback-only 既是 quickstart，也是首启 checklist 送人去的地方（P1-10 的动线终点就是工作台）。所以默认保持 enabled，改为在"工作台开着 + Admin 监听非 loopback"时启动告警——把取舍摆到该看见的人面前，而不是默认掰断一条正在用的路径。

**P1-12 的范围按"只动 8px/9px"收窄。** 报告写的是"最小字号提到 12px"，但 `styles.css` 里小于 12px 的字号声明共 139 处（8px×9、9px×35、10px×48、11px×47）。按字面执行会把 12px 以下的四级层级压平成一级，且没有任何测试守护字号、只能靠肉眼验证。经确认后只改 8/9px 共 44 处，`--font-size-xs` 从 11px 提到 12px，10px/11px 保持不动——**实际地板是 10px 而不是 12px**，这是有意的取舍，不是遗漏。

## 整改过程中新发现的问题（不在原报告内）

| 问题 | 位置 | 状态 |
|---|---|---|
| deadman 测试偶发挂死 10 分钟 | `internal/deadman/deadman_test.go` | **已修** `c1b814d`。根因不是"排队心跳未到期"这么简单：`NextAttempt` 与比较它的 `e.now()` 都取 `time.Now().UTC()`，丢掉了单调钟，墙钟回拨就会让 `drainOne` 早返回、接收端 handler 永不进入。修法两条：把队头 `NextAttempt` 显式打到过去让投递路径确定发生，再给 `<-entered` 加 10 秒兜底——兜底里必须先放开接收端再 `t.Fatal`，否则 `httptest.Close` 等待在途请求会二次死锁 |
| 非流式响应无写超时 | `internal/gatewayapi/handler.go` | **已修** `df05301`。见上方 P1-5 修正 |
| `admin_developer.go` 未通过 gofmt | `internal/app/admin_developer.go` | **已修** `655678d`。CI 跑 `go vet` 但不跑 gofmt，所以没人拦住它 |
| 数据面无全局按源限流 | `internal/app/runtime.go` `gatewayRouter` | **已修** `b4c8235`。从 P1-5 拆出，新增 `internal/sourcelimit` 与 `gateway.source_rate_limit` |
| step-up 原语对失败既不限流也不审计 | `internal/app/admin_prices.go` `verifyPricingReauthentication` | **已修** `ca26a3c`。已认证会话背后的离线速度口令 oracle；推广到 13 个端点前必须先补上。只计失败不计成功 |
| 会话响应不含 `role`，前端无从得知自己只读 | `internal/app/admin_session.go` 等五条签发路径 | **已修** `817f7d9`。做"两级 RBAC 的前端"时才发现的前置缺口 |
| `npx tsc --noEmit` 不检查测试文件 | `web/` 验证流程 | **已修**（流程）。改用 `npm run typecheck`，见上方说明 |
| 角色枚举落地前创建的 Admin 账户 role 为空，被自己的实例拒绝 | `internal/domain` 校验 + bbolt 记录 | **已修** `1b33357`。两级 RBAC 的收尾缺口：老账户存的 role 是空串，而校验是严格的——存偏好设置报校验失败、所有 administrator 写操作被当成 read_only 拒绝。迁移只回填空值为 `administrator`（它们本来就有的权限），其它无法识别的值继续大声失败，不做"归一化到最高权限"这种事 |

## 顺带修正的既有问题

- `internal/deadman` 的 logger 原本完全绕过 safelog，而它持有每个探测目标的 bearer token（随 P0-2 修复）
- `internal/idempotency` 原有 70 行测试全部只覆盖已删除的死代码，其中 `TestUnknownIsTerminalAndKeysAreBounded` 名不副实——它没有测试任何 bounding，因为那个 store 根本没有（随 P1-11 修复）
- HA 设计文档指向的 `internal/authority.Mutation` 已随包删除，文档同步更新为说明其去向（随 P1-11 修复）
- `AlertForm` 的脏检查原本用 `window.confirm`，且它自己重述了一遍每个字段的默认值；改为 `useDirty`（与首渲染值比较）后，默认值只写一处，不会再和比较逻辑各自漂移（随 P1-10 修复）

## 有意没做的部分

- **P2-21 的 `.data-row` 基类没抽。** 行家族已经实质分化——不同的 grid、min-height、padding，`provider-row`/`credential-row` 各自被声明两次——把它们折到一个基类会改动真实布局数值，而唯一的验证手段是 jsdom。两个工具栏合并了，因为字号地板落地之后它们已经逐字节相同。
- **P2-21 的尺寸 token 用 ratchet 而不是转换。** `--space-*`/`--radius-*` 声明了几乎没人消费，styles.css 里有 758 处手写间距/圆角。一次性换掉等于重写每一处布局且无法验证；改成"只许降不许升"的基线，转换一批就把基线调低一次。
- **`parquet-go` 依赖本身没删，是 ADR 决定的，不是漏做。** [ADR 0017](../../adr/0017-usage-export-format.md) 把 NDJSON 定为**并列的可选格式而不是替换**：`usage.export_format` 只决定*新*分区写成什么，默认仍是 `parquet`，已写的 `.parquet` 分区永不重写——所以依赖必须留着，否则老实例的历史分区就读不了了。想真正甩掉这个依赖，前提是某个部署从第一天起就只写 NDJSON，那是部署侧的选择，不是代码里能一刀切的事。（这条此前与上方"Parquet 依赖降级（已完成）"并存，读起来像自相矛盾，现改写为两者实际描述的同一件事。）
- **`jsonschema-go` 的 SBOM 范围没动。** 它只被 `deploy/observability/schema_test.go` 用到。想把它移出主模块要建嵌套模块，但发版 SBOM 用 `anchore/sbom-action` 扫 `path: .`，嵌套 go.mod 一样会被收录——机制达不到目的，先不做。
- **流式中断计费的 ambiguous 分支没动。** 那条分支一个字节都没投递过，没有可用来封顶的量；ambiguous 的语义是"上游可能已经完整服务过"，预留正是为此存在的。

## 已知的未验证面

P1-10、P1-12 都是纯视觉/交互改动，**没有在真实浏览器里逐页核对过**，只有 jsdom 断言和类型检查守护：

- 8/9px → 12px 涉及侧栏、徽章、工具栏等固定宽度容器，可能有被撑破的地方；
- 首启 checklist 只在"用量水位从未推进过"的实例上出现，jsdom 测了显示/隐藏与链接目标，没测过真实布局；
- Modal 脏检查用 `.modal.discarding > :not(header):not(.discard-prompt) { display: none }` 遮住表单，靠的是 CSS 而不是卸载子树——这是"取消后每个字段原样还在"的前提，但也意味着任何绕开这条选择器的子元素会漏出来。

## 推送与发版状态

截至 2026-08-07 收尾，`main` 与 `origin/main` **同步**，工作区干净，没有悬空分支。此前记录里"某某提交还在本地"的说法一并作废——`b4c8235` 起的全部提交（含本次文档收尾）都已推送。

**首个 tag `v1.0.0-rc.1` 已打并推送**（签名 annotated tag），走的是 `docs/milestones/implementation-status.md` 的 RC 序列第一步。此前"打 tag 只差一个发版决定"的记录到此为止。三件事要在读这份记录时同时知道：

- **窗口关了。** 首个 tag 之前是改默认，之后就是需要迁移说明的破坏性变更。本轮的 step-up 推广（`ca26a3c`）正是踩着窗口关闭前做的，CHANGELOG 里那条 `Breaking (Admin API, pre-1.0)` 也写明了这一点。往后 Admin API 的同类改动都要按破坏性变更处理。
- **RC 前置的 24 小时 soak 没跑。** milestone 文档的临界路径第 1 步是"在确切的 RC commit 上跑并归档 24 小时 soak"，打 tag 时它没有完成——tag 是先打的，soak 属于 RC 门禁里仍欠的一项。
- **release workflow 跑完了，publish 停在 M11 evidence 门禁上——这是它该有的行为。** quality / sdk-compatibility / stress / web / 四个平台的 binaries / container / provenance 全部成功，产物、SPDX SBOM、checksums、Sigstore bundle 都已生成并签名；publish 在第一行 `test -n "${M11_RELEASE_EVIDENCE_JSON}"` 上失败，因为 `v1-release` environment 还没有那份 secret。tag 的签名本身已被 GitHub 验证（`verified: true`），三道门禁里只剩 evidence bundle 与 environment 审批。
  这份 bundle 不是配置项，是 `tools/m11/release-evidence/` 定义的人工证据：14 个真实 AWS KMS 场景、EKS 与 VM/systemd 两套部署（含三种 CrashLoop）、由非实现作者的操作员做的主/恢复还原演练与随后的权限回收、七处 Secret Canary、以及四位不同评审人对同一个完整 commit SHA 的签署。仓库里的 `template.json` **故意是不完整的**，且有测试钉住"verifier 必须拒绝它"——所以它没法被"顺手填一下"，只能由真正做过这些事的人在受控系统里填。没有它就没有 GitHub Release，这正是这道门禁存在的意义。

Developer Workbench 的默认已按上文决定保持 enabled 并改为可达时告警；metrics 端口与 KMS 推荐仍未动。
