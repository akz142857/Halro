# A7 · 数据迁移与升级

> 阶段 1 独立评审。范围 `v0.3.0(abfc05c)..HEAD(8bb4847)`，靶子为迁移 32
> （`internal/store/bolt/store.go:839-898`，`structured_output_capability_split`）。
> 事实底座为 `range-map.md` 表 1、表 4；未读同目录其他角色文件。
> 分级：【肯定】【建议】【问题】【疑似BUG】。基准编号见 `review-plan.md` §3。

## 0. 实证方法（本文结论的证据来源）

静态阅读之外，本轮用**真实二进制侧的代码**造了一个真实的 schema 31 库，而不是用 HEAD 的
结构体倒着拼一个「像 31 的记录」——后者测的是自己的模型（`CLAUDE.md`「Verify, never assume」）。

1. 在 `scratchpad/v030`（v0.3.0 worktree，`abfc05c`）内写一个一次性程序，**用 v0.3.0 自己的
   `internal/store/bolt` 与 `internal/domain`** 写库：一个 provider（含 1 个 binding）、两个
   deployment（`dep_a7_on` 勾了 `json_mode`；`dep_a7_off` 的 snapshot 确立了 `json_mode` 而
   `operator_disabled=["json_mode"]`，即「运维显式关闭」）、一条 `DetectorVersion:
   capability-detector-v1` 的探测记录。产出 `schema=31`，`strings` 可见 `json_mode` 与
   `capability-detector-v1` 字节。
2. 用 **HEAD 的 store** 打开同一个文件（包内临时 test，运行后已删除），dump 迁移后的每个桶。
3. 用 HEAD 的 `openWithMigrationStepHook` 在迁移 32 的两个注入点各杀一次，检查回滚与重试。
4. 用 **v0.3.0 的 store** 回头打开已迁到 32 的库（可写与只读两条路径）。

一次性库与临时程序留在 scratchpad，未进仓库；临时 test 文件已删除
（`git status internal/store/bolt/` 干净）。

---

## A7-1 迁移 32 的 COW 纪律与中断恢复成立（实测）

- **位置**：`internal/store/bolt/store.go:839-898`（迁移体）、`:1098-1113`
  （`rewriteBucketIfPresent`）、`:1076-1096`（`patchArrayMember`）、`:1599`
  （整条迁移链包在一个 `s.db.Update` 内）、`:1615-1637`（逐版本应用循环）
- **基准**：B7（durable 状态一致）、B2（fail-closed）
- **分级**：【肯定】
- **是否阻塞发布**：否

写法与此前迁移同形：迁移 31（`store.go:784-810`）也是
`rewriteBucketIfPresent` + `patchArrayMember` + 对 `model_capability_snapshot` 先
`Unmarshal` 再 `Marshal` 回写。记录以 `map[string]json.RawMessage` 读入、只改认识的字段、
其余原样 `Marshal` 回去（`store.go:1103-1112`），所以迁移不会丢掉它不认识的字段——这正是本仓
COW 惯例的实质。整条链在**一个 bbolt 写事务**内（`store.go:1599`），bbolt 的 MVCC 保证
「要么全落要么全不落」，不存在半迁移的持久状态。

**中断实测**（在真实 31 库的副本上，两个注入点各一次）：

| 注入点 | 打开报错 | 崩溃后 schema | `json_mode` 是否仍在 | 重开结果 |
|---|---|---|---|---|
| `before_structured_output_capability_split` | `apply metadata migration 32 (...): injected kill` | 31 | 是 | schema=32，detections=0 |
| `after_structured_output_capability_split` | 同上 | 31 | 是 | schema=32，detections=0 |

即：**回滚干净、重跑成功**，不存在「半迁移库被误读」的第三种状态。`meta.schema_version`
的写入（`store.go:1647`）在迁移体之后、同一事务内，所以版本号不可能领先于数据。

---

## A7-2 逐桶幂等性：对崩溃成立，对「重放到已迁移库」不成立（不可达，但值得记下）

- **位置**：`internal/store/bolt/store.go:906-928`（`splitJSONModeCapabilities`）、
  `:933-955`（`splitJSONModeEvidence`）、`:964-992`（`dropDisabledCapability`）
- **基准**：B7
- **分级**：【建议】
- **是否阻塞发布**：否

逐桶核对三个辅助函数的重入行为：

- `splitJSONModeCapabilities`（:918-921）：`delete(json_mode)` 后**无条件**把两个后继写成
  `false`。对一条已经迁过、且运维事后手工勾上了 `json_object` 的记录再跑一次，会把这一勾
  **静默清零**。
- `splitJSONModeEvidence`（:945-948）：同形，无条件写 `unsupported`。
- `dropDisabledCapability`（:979-984）：真幂等——名字已不在则 `len(kept)==len(names)` 直接返回。
- 探测三桶（:887-896）：`DeleteBucket` + `CreateBucketIfNotExists`，对空桶幂等。

结论：**前两个不是幂等的，是「无条件覆写」**。这在当前不可达——迁移只在
`currentVersion < schemaVersion` 时执行（`store.go:1615`），版本号与数据在同一事务里推进
（A7-1），所以不存在「已迁移的库再跑一次迁移 32」的路径。但它意味着一条纪律：任何以后手工
回绕 `schema_version` 的操作（测试里做过：`migration32_test.go:204-206`）会丢掉运维在升级后
补的勾。**建议**在 `splitJSONModeCapabilities` 的注释里写明这是覆写而非幂等补齐——它与同文件
`backfillCapabilityEvidence`（:1044-1072，「已存在则跳过」）语义相反，两者相邻却不同规。

---

## A7-3 清理面完整：没有第六处存能力词的地方被漏掉

- **位置**：`internal/domain/models.go:322-323`（ProviderInstance）、`:349-350`
  （ProviderProfileBinding）、`:874-875`（Deployment）、`:729`/`:735`
  （ModelCapabilitySnapshot 的 `capabilities`/`evidence`）
- **基准**：B6（durable 格式改动使陈旧状态被拒而非误读）
- **分级**：【肯定】
- **是否阻塞发布**：否

全仓 `grep json_mode --include="*.go"`（排除测试）命中 15 处，逐条判定：
`internal/store/bolt/store.go` 的迁移自身 6 处（:813/836/861/901/918/945/957），其余全是
**注释**（`capability_detection.go:22,67-68`、`modelcatalog/snapshot.go:23`、
`domain/models.go:480,488`、`domain/provider_table.go:306`、`gateway/service.go:2525`）。
**无任何存量读点残留**。

持久化结构里携带 `ProviderCapabilities` / `CapabilityEvidenceSet` 的一共就是上列四处
（provider、binding、deployment、snapshot），迁移 32 全部覆盖（`store.go:849-854`、
`:857-858`、`:872-877`）；`ProviderResource`（`models.go:32-57`）不含能力字段，
`runtime_settings` / `instance_ui_settings` 等 meta 键同样不含。第五处是
`operator_disabled`（A7-5），第六处是探测三桶（A7-4）——range-map 表 1 的清单**完整**，
本轮未找出遗漏。

实测佐证：迁移后对 `providers`、`deployments` 两桶逐记录扫原始字节，`json_mode` 零命中；
provider / binding / 两个 deployment / 两个 snapshot 的 `Validate()` 全部通过。

---

## A7-4 探测桶清空不留悬空引用；审计侧是历史而非活引用

- **位置**：`internal/store/bolt/store.go:887-896`（三桶清空）、
  `internal/app/admin_deployments.go:666-668`（按 ID 取探测）、`:692-696`（指纹重算）
- **基准**：B2（fail-closed）
- **分级**：【肯定】
- **是否阻塞发布**：否

逐类关联对象核对：

1. **进行中的探测 run**：探测记录是 durable 的，但迁移只在进程启动打开库时跑
   （`internal/app/runtime.go:198`），而本仓是单写者单数据目录——升级时不存在活着的探测
   goroutine。升级前 `queued`/`running` 的记录连同整桶被删，没有 orphan 状态机残留。
2. **建部署时引用探测 ID**：`admin_deployments.go:666` 取不到即返回
   `errCapabilityDetectionTargetMismatch`（:668），是**干净拒绝**，不是误读。控制台上的表现
   是「探测已失效，请重跑」而非静默放行。
3. **审计引用**：审计是独立的 append-only 日志（未在本范围改动），其中提到探测 ID 的历史事件
   仍在。这不是悬空引用——审计记的是「当时发生过什么」，不是指向活对象的外键。**未发现**
   任何代码把审计里的探测 ID 再解引用回 bbolt。
4. **幂等桶与指纹索引**：与主桶同批清空（:887），不会出现「主记录没了、幂等键还在」导致
   重放被误判为重复。

实测：迁移后三桶记录数均为 0（`model_capability_detections` / `..._idempotency` /
`..._fingerprint_index`），`ListModelCapabilityDetections` 返回 0 条。

---

## A7-5 `operator_disabled` 丢的是「运维说过不」这个意图，下游会重新推荐

- **位置**：`internal/store/bolt/store.go:861`（只删不加后继）、`:964-992`
  （`dropDisabledCapability`）、`internal/app/capability_drift.go:207-215`（`setOffered`）
- **基准**：B2（可行动性一面）、B6
- **分级**：【问题】
- **是否阻塞发布**：否

**实测**（`dep_a7_off`，升级前 `operator_disabled=["json_mode"]`）：升级后
`operator_disabled=[]`——因为 `kept` 为空时该字段被整体 `delete`（`store.go:982-984`）。
迁移**刻意不把任何后继名写进去**（`store.go:861` 只做删除，注释 :830-832 的理由是
「词表不认识的名字会让下次读校验失败」）。

下游后果，逐跳：`capability_drift.go:207-215` 的 `setOffered` 用
`slices.Contains(deployment.OperatorDisabled, name)` 区分「运维拒绝过」与「从未确立」。两半
既不在该列表里，一旦目录确立了它们，就落入 `AvailableForReview`（:214）而非
`OperatorDisabled`（:212）。控制台据此把它们渲染为「可以开启」而不是「已由管理员关闭」。

于是**升级前明确说过「不要 JSON 模式」的部署，和升级前刚勾了 JSON 模式的部署，升级后在
控制台上完全一样**——都是「有两项新能力可以开启」。运维的一次决定被抹平成从未做过。

判定：这是**信息丢失**，不是记账/安全/数据损坏类，且再推荐**只是提议、绝不自动启用**
（`capability_drift.go:37-40` 的契约与 `setOffered` 的实现一致），所以不阻塞发布。但它是
一条真实的语义漂移，值得写进发布说明：**升级后所有关于 `json_mode` 的「已关闭」记录都会
被遗忘，运维需要对不想开的部署重新表态**。

一个可选的更忠实做法（供整改参考，非本轮要求）：把 `json_mode` 换成两个后继名写回
`operator_disabled`，而不是删除——两半此时确实都是关的，`Deployment.Validate`
（`models.go:981-990` 的「不得既关闭又在用」）成立，词表也认识这两个名字。当前实现选择删除
的理由（注释 :830-832）只论证了「不能留旧名」，没有论证「不能写新名」。

---

## A7-6 迁移注释写错了它自己 bump 到的探测契约版本

- **位置**：`internal/store/bolt/store.go:836-838` vs
  `internal/provider/capability_detection.go:70`
- **基准**：B6（durable 格式的版本描述）
- **分级**：【问题】
- **是否阻塞发布**：否

迁移 32 的注释原文：「their fingerprints carry the detector contract version, **which moved
to v4 with this split**: a **v3 record's** json_mode result answers a question no longer
asked」。实际常量是 `capability-detector-v5`（`capability_detection.go:70`），且
`capability_detection.go:67-69` 自己写的是「**v5** splits json_mode ... so a **v4** result
carries an answer to a question no longer asked」。同一件事在两处被记成两个不同的版本号。

这与 range-map 文末「出入 1」（方案误记 v4）是**同一个错误的两个源头**——方案很可能就是照着
这条迁移注释抄的。契约版本号在本仓是「每个号只花一次」的资源（`capability_detection.go:49-50`
明说），注释把它记错会让下一个改探测的人以为 v5 还没被用掉。改两行注释即可，零风险。

---

## A7-7 降级路径两道门都 fail-closed，但错误信息没告诉运维下一步

- **位置**：HEAD `internal/store/bolt/store.go:1609-1613`（可写打开）、`:1302-1304`（只读）；
  v0.3.0 同文件 `:1432`（可写）、`:1125`（只读）
- **基准**：B2、B6
- **分级**：【建议】
- **是否阻塞发布**：否

**实测**（v0.3.0 的真实 store 代码打开已迁到 32 的库）：

| 路径 | 实际输出 | v0.3.0 行号 |
|---|---|---|
| 可写打开 `Open` | `metadata schema version 32 is newer than this build supports (31)` | store.go:1432 |
| 只读打开 `OpenReadOnly`（doctor 走这条） | `metadata schema version 32 does not match required version 31` | store.go:1125 |

两条都**拒绝而非误读**，range-map 表 4 的描述成立（可写是范围门、只读是精确相等门）。降级
不会损坏数据：v0.3.0 在打开阶段就退出，一个字节都不写。

可行动性上的不足：两条消息都只陈述版本不匹配，没有说**怎么办**。对照本仓已有的更好范例——
迁移 21 拒绝 legacy 证据时消息里带 `make reset`，并且有测试断言这一点
（`store_test.go:290-292`：「refusal does not tell the operator what to do」）。降级场景的
正确动作是「回到新版本，或从降级前的备份恢复」，值得在 `store.go:1610-1613` 的可写门上补一句。
注意这句要加在 **HEAD** 上没有意义（是旧二进制在报错），所以它是**下一个版本的**改进项，
本轮只需在发布说明里写明「v0.4.0 的数据目录不能用 v0.3.0 打开，回退需要备份」。

---

## A7-8 backup/restore 跨版本的拒绝点在数据目录发布之前

- **位置**：`internal/app/backup.go:309-314`（`validateRestoreStage` 里 `boltstore.Open`
  暂存库）、`:225-230`（暂存 store 与 `SchemaVersion()`）、`:134-135` 与 `:295`
  （manifest 记 `schema_version_before/after`）
- **基准**：B2、B7
- **分级**：【肯定】
- **是否阻塞发布**：否

把 schema 32 的 `.hmbk` 恢复到 v0.3.0 二进制上：restore 先把归档解到
`.halro-restore-stage-*` 暂存目录，再 `boltstore.Open` 暂存库（`backup.go:310`）。用 v0.3.0
的 store 打开一个 32 的库，命中 A7-7 的可写门，`validateRestoreStage` 返回
`open staged metadata: metadata schema version 32 is newer than this build supports (31)`。

关键在于**这发生在同函数后段的目录换名发布之前**——现役 `data/` 还没被 rename 走，
`defer os.RemoveAll(stagingRoot)` 清掉暂存，运维的现役数据目录**毫发无损**。这是正确的顺序，
与 B2 一致。反方向（31 的备份恢复到 HEAD）不需要拒绝：`backup.go:310` 的 `Open` 会就地把
暂存库迁到 32，`SchemaVersionAfter`（:230/295）如实记 32，manifest 里 before=31/after=32 的
差值本身就是升级发生过的记录——**这是目前唯一一处会把「迁移发生过」写下来给人看的地方**，
但它只在 restore 时产生，正常升级路径没有对应物（见 A7-9）。

---

## A7-9 「需要重新初始化数据目录吗」：不需要。但有一份必须写进发布说明的损失清单

- **位置**：`internal/store/bolt/store.go:839-898`、`internal/modelcatalog/snapshot.go:232-233`、
  `internal/provider/capability_detection.go:70`
- **基准**：B2、B6
- **分级**：【肯定】（结论）+ 发布说明原材料
- **是否阻塞发布**：否（但发布说明缺这份清单则**是**阻塞项，见结论段）

#231 提交信息声称「不需要重新初始化」。**实测复核：成立。** 一个由 v0.3.0 自己写出的、
带 `json_mode` 存量记录与 v1 探测记录的真实 31 库，被 HEAD 打开后干净迁到 32，
provider / binding / 两个 deployment / 两个 snapshot 全部 `Validate()` 通过，
`migration_history[32] = {Version:32 Name:structured_output_capability_split}`。
**无需 `make reset`，无需重建数据目录。**

「不需要重新初始化」不等于「无损失」。三件事叠加后，运维实际要重建的状态清单：

| 丢失/失效的状态 | 成因（行号） | 运维要做的事 | 代价 |
|---|---|---|---|
| **所有能力探测记录**（含幂等键与指纹索引） | `store.go:887-896` 整桶清空 | 对每个需要 verified 证据的部署**重跑探测** | 计费的上游调用，9 个探针/次 |
| **`json_object` / `structured_outputs` 两半皆关**，证据 `unsupported` | `store.go:918-921`、`:945-948` | 对每个真要 JSON 输出的部署**逐个重新勾选**（并按需重探以拿到 verified） | 人工逐部署复核 |
| **`operator_disabled` 里的 `json_mode` 意图** | `store.go:861`（A7-5） | 对**不想开**的部署重新表态，否则会被反复推荐 | 人工，且不做也不会出错 |
| **旧词表签发的远端签名目录** | `snapshot.go:232-233` 精确相等门 | 需要以词表 v2 重新签发的目录；否则回落内置目录 | 仅影响使用远端目录的部署 |
| **升级期间对存量部署的 JSON 输出流量** | `gateway/service.go:958` + 配对表 `:2537-2538` | 无（在重新勾选前会以 400 `unsupported_feature` 拒绝） | **可见的服务中断窗口** |

最后一行是清单里唯一有对外影响的：**升级后到运维重新勾选之前，所有走
`response_format: json_object` / `json_schema` 的存量流量会被拒**。这条必须进 CHANGELOG 与
发布说明——`review-plan.md` §9 第一条已经指出 `[Unreleased]` 是空的，本清单就是要填进去的
内容。

「探测契约中间版本在野吗」（任务第 7 点）：**v2/v3/v4 从未随任何 tag 发布**。v0.3.0 的常量是
`capability-detector-v1`，HEAD 是 `v5`，而范围内只有三个提交（`ee96d29`/`18a8cb3`/`8bb4847`），
均非 tag。所以在野记录只可能是 v1（或更早、更早的已被迁移 25/26 清过）。而**这个问题在升级后
是空问题**：迁移 32 无条件清空三桶，升级后的库里**任何版本的探测记录都不存在**，
`admin_deployments.go:692-696` 的指纹重算路径面对的永远是「记录不存在」
（→ `errCapabilityDetectionTargetMismatch`，A7-4），而不是「v1 记录指纹对不上」
（→ `errCapabilityDetectionStale`）。两条都是干净拒绝，唯一需要对的那条已实测为对的。

---

## A7-10 · H1 复核：迁移 32 对运维完全不可见【问题】

- **位置**：`internal/store/bolt/store.go:1598-1677`（`initialize` 无任何日志出口）、
  `internal/app/runtime.go:198`（`boltstore.Open` 不接 logger）、
  `internal/app/doctor.go:119`（只报二进制常量）、`:570-577`（只报计数）、
  `store.go:1653-1667`（`migration_history` 写入，无读者）
- **基准**：B2（fail-closed 的**可行动性**一面）
- **分级**：【问题】
- **是否阻塞发布**：**否**（但需以发布说明 + CHANGELOG 补偿，见 A7-9）

**独立复核 range-map 表 1 的结论：成立，且比表里写的更彻底。** 逐信号：

1. **启动日志：无。** `initialize`（`store.go:1598-1677`）整段没有任何 logger；调用点
   `runtime.go:198` 的 `boltstore.Open(cfg.MetadataPath())` 是单参数签名，**根本没有把
   logger 传进去的通道**。所以这不是「忘了打日志」，是当前 API 形状不允许。
2. **`migration_history`：有记录，无读者。** `store.go:1653-1667` 写入 32 号记录（实测
   `{Version:32 Name:structured_output_capability_split}` 确在库里）。全仓 grep
   `migration_history` / `MigrationHistory` 在 `internal/app` 与 `cmd/halro` **零命中**——
   这条记录只有 bbolt 命令行工具或本仓的测试能看到，运维看不到。
3. **doctor：不报迁移，只报常量。** `doctor.go:119` 打印的是 `boltstore.CurrentSchemaVersion()`
   ——**二进制里的常量**，永远是 32，与「这个库刚迁过」无关；升级前后输出一字不差。
   `capability_drift`（:570-577）是计数型 warn，且只在目录 revision 移动时才触发，
   与迁移无因果。
4. **控制台：一般性提示，且语义被 A7-5 污染。** 两半落入 `AvailableForReview`，渲染成
   「有能力可以开启」——与「操作者从来没勾过」**完全同形**。
5. **调用方：只在失败时。** 400 `unsupported_feature`（`gateway/service.go:961-966`）。

即：**唯一会告诉运维「有事发生」的信号，是生产流量开始报 400。** 这正是 H1 的假设，本轮
独立证实。

**为什么判【问题】而非【建议】**：B2 的字面（拒绝而非降级）已经满足——两半关掉是拒绝，是
fail-closed 的正确一侧。但方案 §6 明确把 B2 的另一面写成「运维**看得出它被关了、也看得出
为什么**」。这里两问都答不出：看不出被关了（与从未勾选同形），更看不出为什么（无任何一处
提到迁移）。一个只能通过生产 400 被发现的状态变更，是可行动性上的缺陷，不是风格问题。

**为什么不阻塞发布**：它不属于 `release-assessment.md` §5 的阻塞类（无记账错误、无 fail-open、
无密钥泄漏、无数据丢失或静默误读、无无界重试、能力不是**静默**丢的——它是**有意**关的且拒绝
是显式的、升级本身有 fail-closed 拒绝）。可以用发布说明补偿，代价是运维必须先读发布说明。

**最小补法与成本估计**（三选一即可闭合，建议 a+b）：

| 方案 | 做法 | 成本 | 效果 |
|---|---|---|---|
| **(a) 启动日志一行** | 现成机制已在：`openWithMigrationHook`（`store.go:1309-1311`）有 `afterUp func(uint64)` 钩子，只是 `Open`（:1285-1287）传 nil。让 `Store` 记下本次应用的版本号列表并暴露一个 getter，`runtime.go:198` 之后 `logger.Info("applied metadata migrations", "versions", ...)` | **约 20 行 Go + 1 个测试**；不改签名（getter 而非多传参），零 durable 影响 | 升级那一次在 stdout 留痕，是最直接的一条 |
| **(b) doctor 一项** | 新增 `add("metadata_migrations", ...)`，读 `migration_history` 桶报最近一条（版本 + 名字）。桶已存在且已有数据，只缺一个 store 侧的 `LastMigration()` 与一行 doctor 输出 | **约 15 行 Go + 1 个测试** | 事后任何时刻可查，覆盖「升级时没看日志」的情况 |
| **(c) migration 记录暴露到 Admin API** | 新增只读端点 + 前端展示 | **60+ 行后端 + 前端改动 + i18n 两份 + bundle 重建** | 覆盖面最大，但本轮范围内性价比最低 |

另有**零代码**的补偿项，且无论是否做 a/b 都必须做：CHANGELOG 与发布说明写明 A7-9 的损失
清单。`review-plan.md` §9 已经把 `[Unreleased]` 为空列为硬卡点（`release.yml` 的 `prepare`
job 会拒绝启动），所以这一条本来就在必做清单上——本轮只是给它补上必须写进去的**内容**。

---

## 汇总

| 编号 | 标题 | 分级 | 阻塞发布 |
|---|---|---|---|
| A7-1 | COW 纪律与中断恢复成立（实测两个注入点回滚 + 重试） | 肯定 | 否 |
| A7-2 | 两个 split 辅助函数是覆写而非幂等（当前不可达） | 建议 | 否 |
| A7-3 | 清理面完整，无第六处能力词存量读点 | 肯定 | 否 |
| A7-4 | 探测桶清空无悬空引用，引用者干净拒绝 | 肯定 | 否 |
| A7-5 | `operator_disabled` 的「运维已关闭」意图丢失，下游会重新推荐 | 问题 | 否 |
| A7-6 | 迁移注释把探测契约版本记成 v4（实为 v5） | 问题 | 否 |
| A7-7 | 降级两道门 fail-closed（实测），但消息不含下一步 | 建议 | 否 |
| A7-8 | backup/restore 跨版本拒绝点在目录发布之前 | 肯定 | 否 |
| A7-9 | 无需重新初始化（实测）+ 五项损失清单 | 肯定 | 否\* |
| A7-10 | H1 复核：迁移完全不可见 | 问题 | 否 |

\* A7-9 的结论本身不阻塞；但**发布说明/CHANGELOG 缺这份清单则阻塞**，因为运维会以「无损升级」
的预期升级，然后在生产 400 里发现 JSON 流量停了。

**留给阶段 3 的实测项**（本轮无法在无 Admin 交互的前提下完成）：

- R2/R3：目录 entry revision 是否对**每个**存量模型都因词表拆分而变化——这决定 doctor 的
  `capability_drift` warn 与控制台 review 提示是否**必然**出现。本轮造的库用的是
  `operator_declared` 且模型不在内置目录内，走不到那条分支。
- R4：`halro doctor` 在真实 populated 目录上升级后的完整输出（本轮只从代码推导出
  `metadata: pass, bbolt schema v32`，未跑真二进制的 doctor）。
- 一次真实的 `backup create → restore` 跨 31/32 演练（本轮只推导到拒绝点的行号与顺序）。
