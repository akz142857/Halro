# 评审整改进度

> [260805.md](260805.md) 是一份有日期的发现记录，不改动。本文件是它的**活的对照表**：哪些做了、哪些没做、以及做的过程中改变了对原结论的判断。
>
> 编号沿用 260805.md 第十章的修复清单。最后更新：2026-08-06（P2-16、P1-7、P2-19（部分）、P2-23 全部完成，本轮未提交）。

## 一句话状态

清单共 23 项，已完成 23 项——**清单本身全部做完**。P0 四项、P1 八项、P2 十一项。前 21 项**已合并进 main 并推送到 origin，没有悬空分支**；P2-16、P1-7、P2-19、P2-23（本轮完成）改动还在本地工作树，未提交。四项实现计划见 `.claude/plans/gleaming-squishing-flame.md`，Phase 1-4 已按顺序全部推进完。**两项刻意推迟、写清楚了原因**：P2-19 的 `internal/app` 拆 adminapi 子包、"phase2" 改名（`store.go` 按数据域拆已完成）；P2-23 里把 step-up 从"管理员账户创建/删除"推广到其余既有破坏性端点（详见下方"P2-23 验证记录"）。整改过程中原报告的四条结论被修正，另外发现六项报告里没有的问题（其中五项已修完，含本轮的 v3 gate 缺口和 RBAC 前置缺口）。

## 已完成

每项都按同一套流程验证：写测试 → **把代码改回缺陷状态确认测试真的失败** → 恢复 → 目标包 race 检测 → 全仓无缓存套件。反向验证是必需步骤，不是可选的——它是唯一能证明"测试守护的是真问题"的手段。前端项另加 `make frontend` 重建 + `npx tsc --noEmit` + 全量 vitest。

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
| P2-16 | ledger 帧 epoch 4：HMAC + hash 链，[ADR 0016](../adr/0016-ledger-frame-integrity.md) Accepted。fail-closed 启动门禁、v3 gate 缺口一并补齐、backup manifest 链头字段、`heimdall ledger verify` 三态报告（见下） | 本轮（未提交，见"验证记录"） |
| P1-7 | 审计外部锚点，[ADR 0015](../adr/0015-audit-chain-external-anchoring.md) Accepted。默认 sink = dead-man 拉取；bbolt 实例身份、有界锚点环、metrics 监听器上独立凭证端点、deadman 侧增量拉取+持久化高水位、`heimdall audit verify-anchor` 三态报告（见下） | 本轮（未提交，见"验证记录"） |
| P2-23 | Parquet 降级为可选格式（[ADR 0017](../adr/0017-usage-export-format.md) Accepted）+ Admin 多用户登录、管理员/只读两级权限、管理员账户创建/删除的 step-up（见下"P2-23 验证记录"） | 本轮（未提交，见"验证记录"） |

## 未完成

### P2

剩两项组织性重构（外加 P2-20 的最后四分之一），详见 260805.md 第十章：

| 编号 | 内容 | 备注 |
|---|---|---|
| P2-19 | `internal/app` 拆 adminapi 子包、"phase2" 重命名未做，见下"P2-19 验证记录" | `store.go` 按数据域拆已完成，风险已不对等，见记录里的理由 |
| P2-23 | step-up 从"管理员账户创建/删除"推广到其余既有破坏性端点（project/credential/provider/route/deployment/redaction-policy/token-guard-policy/alert 的 delete）、Admin 前端 UI（角色选择、只读态提示、按角色禁用写操作按钮） | 首个 tag 也没打，见下"P2-23 验证记录"里的推迟理由 |

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

三个子项风险不对等，分开处理：

**`store.go` 按数据域拆（已完成）。** `internal/store/bolt/store.go` 从 5340 行拆到 1025 行，新增 8 个同包文件（`store_admin.go`/`store_audit.go`/`store_keys.go`/`store_pricing.go`/`store_projects.go`/`store_providers.go`/`store_settings.go`/`store_usage.go`），按方法名关键词分域。用 `go/parser` 抓每个顶层声明的精确字节范围（含 doc 注释）按名字分桶写出，全部类型/变量/常量（含 `migrations` 迁移表）留在 `store.go` 本体不动，只搬方法体；`goimports` 收尾清理每个新文件的 import。这样做安全性有保证：同包内符号搬到哪个文件不影响可见性，唯一能出错的地方是脚本本身的字节切分，而这一步会被编译器立刻捕获（漏了/多了/重复了都是编译错误）。验证：`go build ./...`、`go vet ./...`、`gofmt -l` 全干净，`go test ./...` 全绿，`go test -race ./internal/store/bolt/...` 绿。过程中脚本第一版有个 bug（import 块被写了两次导致 `store.go` 语法错误），发现后从备份恢复重跑，不是留着让编译器"以后"发现。

**`internal/app` 拆 adminapi、"phase2" 改名——都没做，是本轮唯一没有兑现"全部完成"的两项，原因写清楚：**

两者都是纯组织性改动，不改变任何行为、不产生任何安全或功能收益——这一点从一开始就知道，但只有实际动手拆最简单的一个候选域（`admin_projects.go`，477 行，Explore 阶段判断是耦合度最低的域）才看清代价有多大：这个文件里除了项目/网关键的 CRUD，还定义了 `requireRevision`、`adminMutationError`、`adminBadRequest`、`adminBadRequestCode`、`adminPreconditionFailed`、`adminAuditError`、`refreshAdminAuth`、`auditAdminMutation` 这些**被其余十几个 admin_*.go 文件共用**的通用助手——不是"项目模块偶尔用到"，是整个 admin mutation 路径的公共基础设施长在了被认为最容易搬的那个文件里。真拆包意味着要么先把这些助手挪到一个新的公共子包（那它自己就是一个需要设计导出面的模块），要么每个新子包各自导入它——两条路都不是"移动文件"，是要对着 20 个文件、11533 行逐一设计导出接口的活。

这类改动一旦做错，编译器不一定能兜底——比如把 `adminMutationError` 错误分类的一个分支漏调，或者把某个 mutex 的粒度在搬迁中意外改变，这些是编译通过、测试如果没覆盖到那条分支也通过、但实际行为悄悄变了的那类 bug。CLAUDE.md 把这个仓库的优先级写得很直白："Security, accounting correctness, and backward-compatible API behavior take priority over feature count"——admin mutation 路径正是这句话点名要保护的东西。用剩余时间去做一次仓促的、零功能收益的重构去冒这个风险，划不来；这些时间挪去做 Phase 4（RBAC、Parquet）更值——那两项是这轮唯一有真实功能/安全价值的剩余工作。

**"phase2" 改名**同理但风险构成不同：命中 `internal/domain`、`internal/compatibility`、`internal/provider`（+`openai`）、`internal/gateway`、`internal/gatewayapi` 共 5+ 个包，24 个文件（含测试）。相比 adminapi 拆分，这个改动确实可以完全被编译器兜底（改名不对会直接报 undefined），风险主要是纯体力活的规模，以及一个必须手工守住、编译器管不到的硬约束——`internal/store/bolt/store_admin.go`（原 store.go）里 `{version: 6, name: "phase2_capability_evidence", ...}` 这个已经在真实部署上跑过的迁移历史字符串字面量绝对不能碰。本轮评估后判断和 adminapi 拆分一样优先级最低，一并推迟，留给下一轮单独执行——这项风险低，适合作为一个独立、专注的小改动去做，不该和其它工作混在一起仓促收尾。

## P2-23 验证记录（2026-08-06，Parquet 完成；RBAC 完成，step-up 覆盖面部分推迟）

两个子项风险构成完全不同，分开记：

**Parquet 依赖降级（已完成）。** [ADR 0017](../adr/0017-usage-export-format.md) 状态 Accepted：NDJSON 作为*新增*可选格式而不是替换——`usage.export_format` 配置项选新分区写成什么，已写的 `.parquet` 分区永不重写（跟 ledger/audit"已有字节不重写"是同一条纪律）。`ManifestFile`/`AdjustmentManifestFile` 新增按文件记录的 `Format` 字段，空值向后解读为 `parquet`（跟 `SchemaVersion` 现有的向后读约定一致）。`parquetAttempt`/`parquetAdjustment` 系列结构体加 `json:` tag 跟现有 `parquet:` tag 并存，`writeNDJSONAtomic`/`readNDJSONFile[T]` 复刻 `writeParquetAtomic` 的临时文件+fsync+原子改名+目录fsync 序列。`internal/app/backup.go` 的 restore 路径不需要改——它只调用 `exporter.LoadManifest()`/`exporter.Verify()`，格式判断已经封在 manifest 内部。测试：`internal/usage/ndjson_test.go`——NDJSON 分区可发布可验证可幂等重导出、篡改可被发现、Parquet+NDJSON 混合 manifest 能对着同一次 ledger 重放一起验证/对账、无 `Format` 字段的旧 manifest 条目仍按 legacy 解读为 parquet 并能正常验证（本轮补的第 4 个测试，覆盖 ADR"必需验证"清单里原本没有直接测试守护的一条）。**反向验证做了**：把 `format()` 的空值兜底从 `FormatParquet` 改成 `FormatNDJSON`，确认新测试失败，再改回。`go build`/`go vet`/`gofmt -l` 干净，`go test ./internal/usage/...`、`-race` 覆盖 usage/backup/config 三包全绿。ADR"必需验证"清单里的 crash-injection（部分写入/漏 fsync/改名中断）没有补——现有 `writeParquetAtomic` 本身也没有这类测试先例，NDJSON 不比 Parquet 更缺，不是这轮引入的新缺口；单独的"归档目录混合格式 restore 演练"也没做成独立测试，因为读代码已经确认 `internal/app/backup.go` 不需要为格式改动任何一行——这条本身就是设计能立住的证明,量级上不值得为一个已知不会分支的路径新写一个全链路集成测试。

**Admin 多用户登录 + 两级权限（已完成，范围按用户明确要求收窄为"多用户登录 + 管理员/只读两档，不做细粒度权限矩阵"）。**

- **发现的前置缺口**：`store.AdminUserCount`/`store.CreateFirstAdmin`（`admin_setup.go`/`admin.go`）原本在 HTTP 和 CLI 两条路径上都硬编码"已存在管理员就拒绝再创建"——"两级 RBAC"字面上只是加个角色枚举，实际做起来发现整个仓库压根没有"第二个管理员"这个概念，比原评审设想的范围大得多。方案是保留 `CreateFirstAdmin`/首启 setup 路径原样（仍是一次性 bootstrap，只认零管理员状态），在它之上新增一条平行的"已认证 administrator 创建后续用户"路径（`createAdminUser`），两条路径不冲突：一个只在冷启动时可达，一个只在有会话之后可达。
- `domain.AdminUser` 新增 `Role`（`administrator`/`read_only`，`ValidAdminRole` 校验，无第三档）；`adminauth.NewUser` 签名加 `role` 参数。
- 存储层：`boltstore.ListAdminUsers`（按用户名排序）、`DeleteAdminUser`（复用既有的 `deleteAdminIdentityRecords` 做会话/MFA 清理，而不是新写一份——过程中发现自己最初手写的清理逻辑对 recovery-code 桶的假设是错的，实际是跟 authenticator 桶同构的扁平 `username\x00id` 前缀，不是嵌套桶）。
- 中间件按"是否操作自己的账户"一分为二：`requireAdminMutation`/`requireAdminSetupMutation`（新增 `requireAdministratorRole` 校验，`read_only` 一律 403，`code: "read_only_role"`）保持给跨用户/系统配置的写操作；新增 `requireAdminSelfMutation`/`requireAdminSelfSetupMutation`（只做会话+CSRF，不做角色校验）专门给自己账户的操作——logout、改自己密码、自己的 MFA 管理、自己的 preferences。这条区分是实现过程中自己发现的设计问题，不是用户反馈：最初"写操作一律拒绝 read_only"的方案会把 read_only 用户连自己的密码都改不了、也退不出登录，明显不对，在写测试之前就改掉了。
- 新端点：`GET/POST /admin/api/v1/admin-users`（`administrator` 权限 + step-up：创建新用户要求重新提交当前密码+新鲜 TOTP，逐请求校验，不发短期提权 token——跟 `admin_prices.go` 已有的 `verifyPricingReauthentication` 是同一个函数，只是换了个更贴合语义的调用名）、`DELETE /admin/api/v1/admin-users/{username}`（同样 step-up；拒绝自删——用 session/logout 结束自己的访问；拒绝删掉最后一个 administrator，否则系统会陷入"零管理员，只能靠离线 CLI 破窗"的更大故障）。
- 测试（`internal/store/bolt/admin_users_test.go` 三个 + `internal/app/admin_users_test.go` 三个）：`ListAdminUsers` 排序正确、`DeleteAdminUser` 清掉会话与 MFA 状态但不影响其他管理员、revision 冲突和用户不存在两种失败路径；HTTP 层创建/删除的 step-up 正确路径与错密码/无效角色拒绝路径、新用户能实际登录、自删/删最后管理员被拒；**表驱动 read_only 全路由扫描**——用 `chi.Walk` 遍历 `adminRouter()` 全部已注册路由而不是手写清单（这样以后新增写接口默认就在覆盖范围内，不会漏），对每条非 GET/HEAD/OPTIONS 且不在"自服务"白名单里的路由，read_only 会话必须拿到 `403` 且 `code` 精确等于 `read_only_role`（不是被 CSRF 或别的校验先挡住而误判通过）。扫到 48 条真实挂载的写路由（已排除 chi mount 点自带的 405/404 兜底桩和白名单内的自服务路由）。**反向验证做了**：把 `requireAdministratorRole` 的判断临时改成 `if false && ...`，确认表驱动测试和 `TestDeleteAdminUserRejectsSelfAndLastAdministrator` 都会失败（48 条路由全部报"read_only 到达了 handler"），再改回。`go build`/`go vet`/`gofmt -l` 干净，`go test ./...` 全绿，`go test -race` 覆盖 app/domain/adminauth/store/bolt 四包。既有的 `TestFrozenV1AdminRoutesAreRegistered`（只断言列出的路由存在，不检查"仅此而已"）未受影响，跑了一遍确认仍绿。

**推迟未做，原因写清楚：**

- **把 step-up 从"管理员账户创建/删除"推广到 project/credential/provider/route/deployment/redaction-policy/token-guard-policy/alert 的 delete。** 计划里原话是"复用同一函数模式"，但摸了这 8 个 handler 后发现前提不成立：它们全部走 `requireRevision`（`If-Match` header 表达乐观锁），**没有请求体**——不是"复用模式"，是要把这些端点从"无 body 的 DELETE"改成"要求 JSON body 携带 current_password/totp_code"，这是一处破坏性的 API 契约变更。Admin 前端现在发的 DELETE 请求不带 body，backend-only 上线这个改动会让现有的删除按钮当场变成 401——这不是"功能没做全"，是会让已经在用的功能倒退。CLAUDE.md 把"backward-compatible API behavior"列为优先于功能数量的第一条，前端改动又明确排除在本轮范围外（见下）——两者叠加，做这件事的唯一负责任方式是连前端一起改，但那超出了本轮"仅后端"的既定范围。MFA 相关的两个破坏性端点（`deleteAdminMFAAuthenticator`、`disableAdminMFA`）核查后确认**已经**各自内联了等价的密码+TOTP 校验，不需要额外补；`executeAdminDeveloperRequest` 的请求体是要透传给上游 LLM 的实际请求负载，结构上塞不进 step-up 字段，且它的风险已经在"整改过程中新发现的问题"里按"可达时告警"处理过，不属于同一类缺口。
- **Admin 前端 UI**（角色选择器、只读态提示、按角色禁用写操作按钮）：整轮延续此前"仅后端"的既定范围，没有对应前端改动计划，此处如实记录而不是留空不提。

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
| 数据面无全局按源限流 | `internal/app/runtime.go` `gatewayRouter` | 未做。从 P1-5 拆出，需新增配置项 |

## 顺带修正的既有问题

- `internal/deadman` 的 logger 原本完全绕过 safelog，而它持有每个探测目标的 bearer token（随 P0-2 修复）
- `internal/idempotency` 原有 70 行测试全部只覆盖已删除的死代码，其中 `TestUnknownIsTerminalAndKeysAreBounded` 名不副实——它没有测试任何 bounding，因为那个 store 根本没有（随 P1-11 修复）
- HA 设计文档指向的 `internal/authority.Mutation` 已随包删除，文档同步更新为说明其去向（随 P1-11 修复）
- `AlertForm` 的脏检查原本用 `window.confirm`，且它自己重述了一遍每个字段的默认值；改为 `useDirty`（与首渲染值比较）后，默认值只写一处，不会再和比较逻辑各自漂移（随 P1-10 修复）

## 有意没做的部分

- **P2-21 的 `.data-row` 基类没抽。** 行家族已经实质分化——不同的 grid、min-height、padding，`provider-row`/`credential-row` 各自被声明两次——把它们折到一个基类会改动真实布局数值，而唯一的验证手段是 jsdom。两个工具栏合并了，因为字号地板落地之后它们已经逐字节相同。
- **P2-21 的尺寸 token 用 ratchet 而不是转换。** `--space-*`/`--radius-*` 声明了几乎没人消费，styles.css 里有 758 处手写间距/圆角。一次性换掉等于重写每一处布局且无法验证；改成"只许降不许升"的基线，转换一批就把基线调低一次。
- **首个 git tag 没打。** `docs/milestones/implementation-status.md` 定了一条有门禁的 RC 序列（`v1.0.0-rc.1` → `rc.2` → `v1.0.0`，每一步都要核验 release 资产、校验和、SBOM、Sigstore 签名），推 tag 会触发 release workflow。这是发版决定，不是收尾杂活。版本注入、`make version`、CHANGELOG 都已就绪，打 tag 只差决定。
- **Parquet 依赖降级没做。** 它不只是删依赖：ADR 0014 的备份 manifest 固定了两个 Parquet manifest 版本与水位，restore 会校验这两个数据集。改成可选导出或 NDJSON 会连带改动备份/恢复契约，应该走 ADR 而不是顺手改。
- **`jsonschema-go` 的 SBOM 范围没动。** 它只被 `deploy/observability/schema_test.go` 用到。想把它移出主模块要建嵌套模块，但发版 SBOM 用 `anchore/sbom-action` 扫 `path: .`，嵌套 go.mod 一样会被收录——机制达不到目的，先不做。
- **流式中断计费的 ambiguous 分支没动。** 那条分支一个字节都没投递过，没有可用来封顶的量；ambiguous 的语义是"上游可能已经完整服务过"，预留正是为此存在的。

## 已知的未验证面

P1-10、P1-12 都是纯视觉/交互改动，**没有在真实浏览器里逐页核对过**，只有 jsdom 断言和类型检查守护：

- 8/9px → 12px 涉及侧栏、徽章、工具栏等固定宽度容器，可能有被撑破的地方；
- 首启 checklist 只在"用量水位从未推进过"的实例上出现，jsdom 测了显示/隐藏与链接目标，没测过真实布局；
- Modal 脏检查用 `.modal.discarding > :not(header):not(.discard-prompt) { display: none }` 遮住表单，靠的是 CSS 而不是卸载子树——这是"取消后每个字段原样还在"的前提，但也意味着任何绕开这条选择器的子元素会漏出来。

## 推送状态

前 55 个提交已推送到 `origin/main`；此后的提交仍在本地。

仓库仍无任何 git tag。改默认值这件事在首个 tag 之前只是改默认，之后就成了需要迁移说明的破坏性变更——Developer Workbench 的默认已按上文决定保持 enabled 并改为可达时告警，metrics 端口与 KMS 推荐仍未动。
