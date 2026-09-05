# 独立证伪与裁决汇总（2026-09-05）

基线：`381743f6613607dc256828f4776b52af8bdd232c`。结论来自已授权的原角色报告及独立证伪文档，本次仅汇总，不将阅读报告算作一次新的独立实验。发现详情、前提、防御、最小修复与回归见 [findings.md](findings.md)。所有修复均未实施。

**18 个去重根因中，11 个已有独立 CONFIRMED 裁决，含全部 3 个 P1；另 7 个由原角色确认，尚未独立证伪。** 独立裁决中的范围收窄不抹去已确认根因；被推翻的是扩大的解释，不是新增一个“已关闭缺陷”。P0 0、P1 3、P2 14、P3 1。没有整项 finding 被 REFUTED，也没有据本轮证据关闭任何缺陷。

## 独立裁决登记

| 原 ID / 最终级别 | 原发现者 → 独立证伪者 | 尝试推翻的解释 / 有效防御 | 最终裁决与关键证据 |
| --- | --- | --- | --- |
| SEC-01 / P1，高 | Security → 独立 rotation reviewer | 是否元数据迁移、bridge 或旧 key slot 已覆盖对象；是否对象本来损坏；是否 KEK rewrap 也必然失败 | **CONFIRMED** 更换 Master Key/DEK 时外部留存密文漏迁移。原 file reproduction 独立重跑；新 fake-KMS DEK 测试重新解开发布的 primary，旧钥可解、新钥失败、对象字节未变。**REFUTED 扩大解释：** same-DEK KEK rewrap 控制通过。详见 [rotation adjudication](roles/security-rotation-adjudication.md)。 |
| SEC-02 / P1，高 | Security → 独立 reliability/session reviewer | 是否只是已授权 in-flight 请求完成；cookie 删除、bbolt 原子性、generation、absolute expiry 是否阻止后续访问 | **CONFIRMED** 真实 logout 200、row absent 后延迟 upsert 重建；新 Manager 与新的 protected GET 接受旧 cookie。generation 变化和绝对过期对照拒绝。race 通过不消除事务时序缺陷。详见 [session adjudication](roles/security-session-adjudication.md)。 |
| PROV-01 / P1，高 | Provider → 独立 Provider adjudicator | 原 fixture 是否因缺少生产价格 pin/immutable snapshot 才零结算；是否固定费会拒绝零；Retryable=false 或 admission 是否阻止重复 | **CONFIRMED** 四种 HTTP 200 不可用 body/envelope 均二调用、零结算；每次观察到 50 micro-USD 正预留及 committed metered pin（含固定费）。unservedSettlement 绕过已知价格公式，误分类仍成立；ambiguity-only 控制一调用、50 micro-USD，HTTP 400 对照一调用零费。详见 [Provider adjudication](roles/provider-adjudication.md)。 |
| REL-01 / P2，高 | Reliability → 独立 dead-man adjudicator | 原 fixture 直接 drainOne 是否构造了 Run 中不可达窗口；是否 delivery 自己会保存 | **CONFIRMED，收窄窗口。** real Engine.Run worker 在 durable 0/41 时发送 1/42；重载旧 disk 后同 ID 新 observation。任一路径成功 save 即关闭窗口，不能把整个慢 probe 时段都说成未持久化。详见 [dead-man adjudication](roles/deadman-adjudication.md)。 |
| SUM-01 / P2，高 | 主持 summary 候选 → 独立 summary adjudicator | CatchUp、aggregate mutex、bbolt transaction 是否令暂停边界不可达；是否真正账务丢失 | **CONFIRMED** TakeCheckpoint drain 后 HTTP summary requests 1→0→1、cost 90→0→90，ReturnCheckpoint 恢复。源码确认 CatchUp 不重建已 drain 增量、store/pending 无共同读事务。是短暂报表漏计，非永久账务丢失。详见 [summary adjudication](roles/deadman-adjudication.md)。 |
| CFG-01 / P2，高 | 主持配置候选 → 独立 reliability/config reviewer | 默认模板是否按承诺恢复每个删掉字段；所有 normalized 差异是否都影响行为；metrics 是否变公网匿名 | **CONFIRMED** 单键删除 census 110 项：20 校验失败、77 相同、13不同；逐消费者检查排除 inert/accessor-default 差异。loopback 无 credential 的 require_auth 删除可改变本地授权；公网约束仍阻止该组合。详见 [reliability 配置复核](roles/reliability.md)。 |
| FE-01 / P2，高 | Frontend → 主持真实浏览器 | 是否只是假 DOM/URL 断言造成误报；当前内嵌 bundle 是否真正切换 view | **CONFIRMED（failure-link/tab 路径）** 隔离 full runtime loopback，Summary 216 完成/1 最终失败；点击链接后 URL 含 tab=failures/start/end，但 AX 显示汇总仍选中。直接点最终失败 tab 后显示 HTTP 500/mock_failure/1 次尝试及详情。2026-09-05 主持本轮直接补充；[原角色复现](roles/frontend.md)。grouped filters 未据此扩大独立确认。 |
| FE-03 / P2，高 | Frontend → 独立 frontend boundary adjudicator | 相同 token 的 server collision、每次 step-up、普通 CreateKey 稳定 ref 是否阻止该 retry | **CONFIRMED** 原定向组件 fixture 独立重跑，后两次 token 不同；server 每次 step-up 后按 actor/project/token 派生 ID，不同 token 避开正确 collision 防御。没有实际 HTTP commit-disconnect 或写两条 key；后果由源码确认。[边界裁决](roles/frontend-boundary-adjudication.md)。 |
| FE-05 / P2，高 | Frontend → 独立 frontend boundary adjudicator | 500 是否非法、ETag/dirty guard 是否拒绝、0 是否等于无限制 | **CONFIRMED** 原组件 fixture 重跑，未改任何字段即提交500→0；API允许精确bytes与0，更新替换后刷新snapshot。实例hard limit有效，不是越权或无限制。持久化/gateway下一请求仅源码核对。[边界裁决](roles/frontend-boundary-adjudication.md)。 |
| SEC-03 / P2，高 | Security → 独立 security secondary adjudicator | 是否隐藏因子失败记账、fixture未接预算，或登录MFA/required策略已挡住全部路径 | **CONFIRMED** 四条管理路径真实router各7次错误因子均401且零failure audit；各自错误密码控制5×401、第6次429。因子校验本身仍拒绝，不是匿名MFA绕过。required disable保护有效；其他变体/并发尚未覆盖。[独立裁决](roles/security-secondary-adjudication.md)。 |
| SEC-04 / P2，高 | Security → 独立 security secondary adjudicator | 是否只有store级假象、syncUsage触发Purge，或审计/权限在HTTP层阻止超期正文 | **CONFIRMED** 一小时TTL在恰好一小时与两小时认证HTTP均200，Purge后404；匿名401、错误scope解密失败、audit失败503且不返回正文。定点注入store时钟，没有测实际maintenance延迟。[独立裁决](roles/security-secondary-adjudication.md)。 |

三个 P1 的严重度依据分别是授权维护后重要留存流程不可用、注销后的服务器会话撤销边界失效、正常推理失败路径的执行不确定性与保守账务失效。无需假定恶意上游、匿名攻击或已发生真实收费来成立；也不据此升级为 P0。

## 独立实验索引及边界

完整可执行命令、临时 overlay 路径和日志在上表链接中。本次文档汇总没有再次运行测试。

| 对象 | 关键选择器 / 记录结果 | 证明范围与尚缺证据 |
| --- | --- | --- |
| SEC-01 file | `TestSecurityReviewRotationRetainedCiphertext`，独立重跑 exit 0，`original.log` | 实际 file rotation、capture store、synthetic resource ciphertext。未做注册资源 HTTP 全旅程。 |
| SEC-01 KMS / rewrap | `TestAdjudicationKMSRetainedCiphertext` 与 `TestKMSRewrapPreservesMasterKeyCiphertextAndKeyVersion`，最终 exit 0，`kms.log` | 实际 Halro DEK lifecycle + fake KMS；不等于真实 AWS/IAM/恢复验收。初次 exit 1 是临时 fixture 编译错误，非产品失败。 |
| SEC-02 | `TestIndependentSessionRevocationAdjudication`，`-race -count=1`，exit 0，5.070s package，`session-adjudication.log` | 实际 router、CSRF/origin、bbolt、随后新请求；确定性扩大窗口，不证明自然命中率、token 窃取或 elevation 恢复。 |
| PROV-01 | `TestAdjudicationPROV01`，`-count=1`，最终 exit 0，2.127s package，`run2.log` | 四缺陷与两对照，真实 adapter/bridge/metered accounting；fake pin store、内存 transport，无真实出站/计费。初次 fixture bool/error 编译误用已纠正。 |
| REL-01 | `TestAdjudicationRunSendBeforePersist`，`-race -count=1`，exit 0，1.815s package，`run.log` | real Run + fake 204 transport，干净 join；loadState/enqueue 模拟重启。非 kill/power-loss/receiver durable acknowledgement 验收，非同一 Run 的后续 tick 实验。 |
| SUM-01 | `TestReviewSummaryOmitsInFlightCheckpoint`，`-count=1`，独立 exit 0，1.210s package，`summary.log` | 实际已认证 summary handler；手动暂停真实交接位置，非维护线程内真实并发 commit。counts 被断言，cost 由日志独立核对。 |
| CFG-01 | `TestReviewOmittedTemplateKeys`，`-count=1`，exit 0，0.668s package，`config-omissions.log` | production Decode/Normalize/Validate 单键删除；未覆盖任意自定义组合/整节删除，不启动服务。 |

主持 FE-01 浏览器观察直接提供于本轮：使用真实浏览器和当前内嵌 bundle，Provider 为隔离模拟 fixture。无 shell exit code，原始 trace/截图/日志索引由主持保管；本汇总者未另行运行。尚需 grouped row filters、精确 API interval 及 Back/Forward 回归。主持同时观察 mock `/models` 返回 review-chat、unknown 需声明、声明 Chat/Streaming 后停用保存成功、启用前要求测试且测试后仍无价格；这是有限正向防御证据，不代表真实 Provider 能力或成功启用/调用。

FE-03/FE-05 独立执行：原临时 Vitest config 加 `-t 'reproduces debug key retry|reproduces name-only project edit'`，exit 0，2 passed / 3 skipped，总耗时1.18s；日志 `boundary-adjudication.log`，完整命令/哈希见 [边界裁决](roles/frontend-boundary-adjudication.md)。服务端权限、幂等collision、project校验和实例请求体限制为本轮独立源码复核，未运行实际HTTP写入/断线及gateway体积实验。

SEC-03/SEC-04/C2 独立追加证据：原两项 selector 重跑 exit 0（3.730s package）；`TestSecondaryManagementFactorBudget` 与 `TestSecondaryCaptureHTTPRoleTTL` 新overlay exit 0（6.440s package），`independent.log`。前者四路径/错误密码控制，后者实际Open runtime/Vault/store/Admin router的角色、TTL、匿名/scope与audit失败控制；无本轮sandbox失败。完整命令/哈希见 [security-secondary-adjudication.md](roles/security-secondary-adjudication.md)。C2是授权解释裁决，不增加18项根因数。

以上 exit 0 多数表示成功复现坏行为，不能作为修复后不变量已满足的证据。原角色/父任务中的 cache、socket、Docker 或 DNS sandbox failure 与临时测试编译/selector 错误分开记录；不得加入产品缺陷数。全量门禁、浏览器、SDK、soak 和生产验收继续由主持证据负责，本文不据协调消息宣称已独立验收。

## 尚未独立证伪的确认项

以下是明确的待办状态，不是将这些 finding 改判为无证据猜测。其原角色已给出相应强度的证据；新证伪者仍需检查反例和已有防御。

| ID / 当前级别 | 原证据 | 优先证伪方向 / 保留边界 |
| --- | --- | --- |
| SEC-05 / P2 | [Security](roles/security.md)：O(history) 内存与同 append mutex 源码 | 大 fixture 的内存/append 延迟；操作严重度置信度中，不宣称已复现 OOM/DoS。 |
| PROV-02 / P2 | [Provider](roles/provider.md)：open pipe，CR-only 完整事件等 EOF | 持续流、split CRLF、line/event 限长；未识别真实 Provider 使用该 framing。 |
| FE-02 / P2 | [Frontend](roles/frontend.md)：API client pagination + consumer trace | 各目标 picker 第 51+ 项可达性；不把正确管理列表的分页当作所有消费者防御。 |
| FE-04 / P2 | [Frontend](roles/frontend.md)：pending promise 下 Escape/× 关闭 | 消费者成功/失败结果是否保留，明确 pending dismiss 契约；不能泛化所有结果丢失。 |
| REL-02 / P2 | [Reliability](roles/reliability.md)：delta/report 函数输出 5/0 | 增长、不变、下降 gauge 与 counters 对照；原始指标无损坏。 |
| REL-03 / P2 | [Reliability](roles/reliability.md)：文档与 workflow graph 对照 | 清晰区分 v0.x owner decision 和 1.0.0 target；不是新加治理要求或实际发布绕过。 |
| REL-04 / P3 | [Reliability](roles/reliability.md)：单侧 pointer Validate=nil | 验证外部入口/正常构造前提；无少计费、伪造 Ledger 或可利用输入链路证据。 |

## 被推翻的扩大解释与未裁定契约

| 命题 | 裁决 | 原因与来源 |
| --- | --- | --- |
| SEC-01 表示任何 KMS 轮换都会损坏对象 | REFUTED | same-DEK rewrap 对照保持 ciphertext/key version 并正常 unlock；只有改变对象派生密钥的轮换机制被确认。[裁决](roles/security-rotation-adjudication.md) |
| SEC-02 仅是已进行请求结束；或证明 generation/绝对期限失效 | REFUTED | 后续新请求 200；generation/expiry 对照 401。两个相反扩大解释均不成立。[裁决](roles/security-session-adjudication.md) |
| PROV-01 只是缺失 production price pin；或证明所有 adapter/stream 都重复收费 | 前者 REFUTED；后者 UNVERIFIED | metered pin 路径重复结果；真实收费未知，Responses/embeddings sibling 仅源码支持，Mantle stream 未独立复现。[裁决](roles/provider-adjudication.md) |
| REL-01 delivery 完成后必须等慢 probe 完成才安全 | REFUTED | drainOne 的成功 save 也关闭窗口；真正条件是两条 save 都尚未成功。[裁决](roles/deadman-adjudication.md) |
| SUM-01 已证实耐久数据/结算丢失 | REFUTED 就本证据的解释 | 证明的是报表读视图缺口，ReturnCheckpoint/commit 恢复可见；Ledger 损坏未被建立。[裁决](roles/deadman-adjudication.md) |
| CFG-01 的 13 个结构变化都是运行变化，且导致公网匿名 metrics | REFUTED | accessor 默认、disabled anchor 消除部分差异；非 loopback TLS/credential/require_auth 校验仍有效。[复核](roles/reliability.md) |
| deferred worker cancel 后，finishDeferred 传递取消 ctx 必然写终态失败并永久 in_progress | REFUTED（仅该因果解释；主持完整链反证） | 主持追踪 `internal/gateway/deferred_response.go:616,766-810` 的 finishDeferred/saveDeferred 至真实 `internal/store/bolt/store_providers.go:397-455` 的 PutProviderResource：该 bbolt 实现不检查 `ctx.Err()`，直接 `db.Update`，所以不能仅凭 ctx 已取消推断终态写失败。主持报告 shutdown/queued cancel 现有测试及全量通过；本汇总未重复执行。此反证不证明磁盘写失败有重试，也不关闭其他持久化故障路径；不新增 finding。 |
| 历史 C2：read_only 内容 GET 必然是绕过 | 授权读取行为 CONFIRMED；越权解释 REFUTED（当前明确契约） | 独立真实router证实read_only可读正文且生成read审计；audit失败503拒绝正文。domain/wrapper明确实例级GET权限和自账户窄例外，产品更细内容权限决策仍开放。不是未验证行为，也不与SEC-04合并或计为新缺陷。[独立安全裁决](roles/security-secondary-adjudication.md)。 |
| Developer 项目选择本身决定计费、自己的偏好/MFA 修改属于实例越权 | REFUTED 这些推断 | Gateway Key 决定项目，UI 有说明；自账户操作是明确例外，server 授权控制。[Frontend](roles/frontend.md) |
| 任意 URL adapter 即是可达 SSRF；contentscan 应等同杀毒 | REFUTED 这些推断 | 实际 Admin/runtime URL 校验、DNS pin、禁 proxy/redirect；scanner 明确为格式准入。[Security](roles/security.md) |
| 22–26h guard 永远不会拒绝真实 IANA 日 | REFUTED 绝对断言；生产影响 UNVERIFIED | 历史 Casey 27h 已复现，2026 sampling 无异常；当前外部历史重算入口未建立。保留 P3 文档/支持边界，不新增缺陷计数。[Reliability](roles/reliability.md) |

上述反命题状态不代表完整系统性质已被证明。例如“未证明永久丢失”不能读作“任何崩溃都绝不丢失”；独立裁决只覆盖列明的路径和控制。临时 CLI empty 未纳入，由主持最终决定。其他测试缺口与架构提案不使用 REFUTED 冒充验证完成。

## 关闭要求

3 个 P1 均须先处置并独立复验；回归要从“断言坏行为”改成保护目标不变量，同时保留有效防御对照。其余按 [findings.md](findings.md) 的领域分配具体负责人/迭代，源码级性能发现需补容量证据，文档项按现行实现核对。只有修复后对应复现不再成立、相关集成验证通过，才可转为关闭；风险接受必须有责任人、理由、缓解、失效日期和重评条件，不能写成已修复。
