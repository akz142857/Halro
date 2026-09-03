# v0.5.0 评审整改进度

评审结论见 [`260901.md`](260901.md)。按方案 §11，评审阶段不改代码；整改另起提交，进度记在这里。

## 已处置

### P1 · 升级路径上的备份陷阱（阻断项）→ **已修复并实测**

- **问题**：`internal/app/backup.go` 把"陈旧但合法的 usage checkpoint payload"（v0.4.0 写的
  版本 7）与"payload 与水位不符（损坏）"合成同一条致命错误。于是用 HEAD 二进制对 v0.4.0
  数据目录执行 `backup create` 时，目录先被单向迁移到 schema 33，随后备份失败——而 v0.4.0
  二进制此时已打不开该目录，**没有任何二进制能给它做备份**。
- **修复**：让备份路径与启动路径回答同一个问题——一个本 build 读不出的 checkpoint 是
  "档案说不出的位置"，不是"拒绝归档的理由"。payload 无法恢复或与信封不符时记为零水位
  （`ErrNotFound` 早就走这条分支），存储层错误仍然致命。注释写明了这次事故与 7→8 的版本移动。
- **回归测试**：`TestBackupSucceedsWhenTheStoredUsageCheckpointPredatesThisBuild`
  （`internal/app/backup_test.go`），构造"有效信封 + 版本 7 payload"，断言备份成功且
  `checkpoint_watermark` 为零。
- **反向验证**：把修复退回去（脚本带 `assert old in s`，确认搜索串没有失效），
  `go test -count=1` 该测试以实测到的原话失败：
  `usage checkpoint payload does not match its watermark`。恢复修复后通过。
- **真二进制复测**（不是靠单元测试收工）：
  | 步骤 | 结果 |
  |---|---|
  | v0.4.0 目录（schema 32）+ 新二进制 `backup create` | **成功**，manifest `schema_version: 33`、8 个文件、`checkpoint_watermark` 全零 |
  | `backup verify` | 通过 |
  | `backup restore` 到另一个 scratch 目录 | `schema_version_before/after = 33/33`，`ledger_sequence = 120` |
  | 从恢复出的目录启动 | `WARN usage derivatives rebuilt from the ledger reason="usage checkpoint rejected: usage checkpoint version 7 is not supported"` |
  | 恢复实例的 `usage_daily_rollup` 与**直接升级**那份对比 | **逐行相等** |
- 测试范围：`internal/app`、`internal/backup`、`internal/usage`、`internal/store/bolt` 全绿。

### P2 · MiniMax Chat 的输出上限拒绝穿过能力过滤 → **已修复**

- **决定**：两条出路里选了"承认那条转换在这条路径上不成立"。理由是它描述的是上游的真实
  限制，不是 Halro 的取舍——MiniMax 只有一个输出上限且它计入推理，而 Anthropic Messages
  **必填** `max_tokens`，所以"既限定答案又思考"在这个组合上没有可发送的形状。
  改 `mapping.go:31` 的 `ptr(0)` 只能解决"根本没写 max_tokens"那一小类（而且那类在
  Anthropic 的契约下本就不合法），解决不了主干。
- **修复**：给 `ProfileMiniMaxChat` 补上 DeepSeek 的镜像字段规则（字段名要翻转——两个上游的
  单一上限互为镜像）；端点清单在两个端点上声明 `max_tokens` 并说明理由；Messages 那行不再
  宣称一条它到不了的转换。`minimaxThinkingIsOn` 抽出来，渲染器与字段规则读同一个谓词。
- **补上缺失的守卫（这是根因）**：新增
  `TestMiniMaxRendersEveryRequestItsDeclarationAdmits`（DeepSeek 早就有，MiniMax 没有）与
  `TestMiniMaxChatIsRoutedAwayFromAnEffortBearingMessagesRequest`；并给
  `coverageProbes()` 补上 `VisibleOutputTokenLimit` 这根轴——此前只驱动了 DeepSeek 的那根，
  恰好是 MiniMax 不拒绝的那根，于是规则与声明同时沉默，双向校验也满意。
- **反向验证**：移除规则后，覆盖校验**双向都红**，两个新测试也红（其中一个引用了渲染器
  自己的拒绝原话）。
- 已发布契约与控制台 fixture 各刷新 6 行，diff 只动 MiniMax Chat 两行，无其它 profile 受影响。
- **未做**：`reasoning_effort: "max"` 那条同类缺口（影响 5 个 profile），见下 P2b。

### P3 · 平台清单为绑定表指了一道假守卫 → **文档已修，守卫未加**

- `adding-a-platform.md` 那一节现在说明"没有任何东西抓得住它"，附上三个反向实验的结果，
  并写出一道真守卫必须断言什么（binding 命名的 primitive 必须是该 profile 的 adapter 在
  该操作上真正执行的那个——目前没有任何代码表达这层联系）。
- 顺带修掉该文档两处漂移的文件路径与"Two"实列三条的计数。
- **守卫本身未加**：它需要把"binding 的 primitive"与"adapter 实际执行的 primitive"连起来，
  而路由断言做不到（路由是 adapter builder 的属性，不是绑定表的）。这是设计工作，不是补测试。

## 未处置（按严重度排，均不阻断发布）

| # | 内容 | 依据 | 状态 |
|---|---|---|---|
| P2b | `output_config.effort: "max"` 是 Anthropic 合法值但不在 `openaiapi.ReasoningEffortLevels` 里，实测对 5 个 profile 的字段规则全部返回 `[]`，然后死在预留之后；只有 `deepseek.chat.v1` 声明了 `reasoning_effort` | `adversarial-verdicts.md` V1 | 待处置。与 P2 同源但影响面更宽，改动跨 5 个 profile |
| P3b | 给 `profileOperationTable` 的绑定值加一道真守卫 | `260901.md` §4 | 待处置（设计工作） |
| P4 | 新增 Admin 端点族 `/admin/api/v1/usage/summary` 不在冻结路由清单、不在任何 manifest、响应外层是 `map[string]any` | `260901.md` §5.2 | 待处置 |
| P5 | 汇总端点两段读竞态（已复现，自愈；`watermark_sequence` 会宣称覆盖到没计入的 sequence） | `adversarial-verdicts.md` V3 | 待处置。修法是让两次读描述同一 WAL 前缀，不是换顺序 |
| P6 | 默认月窗口一年里有 34 天只给 11 个月，违反 PRD 写的"最近 12 个月" | `adversarial-verdicts.md` V5-B | 待处置 |
| P7 | 前端：后端 400 时整个筛选栏随面板卸载，操作员无法把范围调回来 | `260901.md` §5.3 | 待处置 |
| P8 | 前端：P95 越界按精确值画，中文文案承诺了界面上不存在的 `> 120s` | 同上 | 待处置 |
| P9 | 双向覆盖校验对三个 native Anthropic 行整体跳过（假声明可通过六个测试） | `260901.md` §5.2 | 待处置 |
| P10 | `usage rebuild-summary` 用未经认证的 WAL 重放并 durably 写两个派生物 | 同上 | **已于 260903 后续整改关闭**:离线 usage 逐帧认证全部世代并校验可信 checkpoint,失败时保留已有派生物;见 `../260903/progress.md` |
| P11 | `providers.types.*` i18n 无守卫，而 golden fixture 已带 `provider_types`，只差十行 | `260901.md` §4 | 待处置 |
| P12 | 汇总遍历不接 context、admin server 无 `WriteTimeout`；一年满负荷 = 2.01 s 读事务 | `adversarial-verdicts.md` V5-C | 待处置 |
| ~~P13~~ | 文档漂移 | `260901.md` §5.4 | **已修**：`provider_table.go` 与 `minimax.go` 的伞形注释与实测对齐；`adding-a-platform.md` 两处路径与计数；`manifest.go` / 两个测试的 "fourteen"→13、"65"→77；`--provider-type` 帮助文本改为**读表推导**（它是类型清单的第四份副本，offered 六个而表里有八个） |
| P14 | `performance-baseline.md` 对路由解析差 12 倍（两个版本同时复核，漂移早于 v0.4.0），已不能作回归判据 | `runtime-evidence.md` R4 | **已于 260903 后续整改关闭**:旧表明确退役为历史数据,以同机双版本 8 组样本和可复核原始结果替代;见 `../../verification/assessments/v0.6.0-followup.md` |
| P15 | `ledger verify` 在零帧实例上把"没有"说成"不能认证"（v0.3.0 记过，第三次观测） | `runtime-evidence.md` R3 | 待处置 |
| P16 | `ledger verify` 会静默把数据目录单向迁移（与 P1 同源，但改它是命令契约变更） | `runtime-evidence.md` R2-a | **未修**，改为在发布说明写死操作顺序 |
| P17 | `bootstrap` 用 `DefaultProviderCapabilities(type)` 而非默认 profile 的 Defaults，Bedrock 连接开箱少声明五项能力 | `260901.md` §5.4 | 待处置 |
