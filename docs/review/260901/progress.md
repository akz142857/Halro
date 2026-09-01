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

## 未处置（按严重度排，均不阻断发布）

| # | 内容 | 依据 | 状态 |
|---|---|---|---|
| P2 | `/v1/messages` + 任意 `output_config.effort` 落到 `minimax.chat.v1` 时 100% 在预留后 400，而端点清单把该转换声明为受支持；同类缺口影响 5 个 profile（`reasoning_effort: "max"`） | `260901.md` §3、`adversarial-verdicts.md` V1 | 待处置。**不是补一行**：只加镜像字段规则会与 `manifest.go:353` 的声明对立，需先决定是承认那条转换不成立，还是修 `mapping.go:31` 的 `ptr(0)` |
| P3 | `profileOperationTable` 的绑定值全树无守卫；`adding-a-platform.md:208-212` 为它指的守卫经四次反向实验证伪 | `260901.md` §4 | 待处置。至少先改文档（它现在给的是虚假的安全感），守卫本身另议 |
| P4 | 新增 Admin 端点族 `/admin/api/v1/usage/summary` 不在冻结路由清单、不在任何 manifest、响应外层是 `map[string]any` | `260901.md` §5.2 | 待处置 |
| P5 | 汇总端点两段读竞态（已复现，自愈；`watermark_sequence` 会宣称覆盖到没计入的 sequence） | `adversarial-verdicts.md` V3 | 待处置。修法是让两次读描述同一 WAL 前缀，不是换顺序 |
| P6 | 默认月窗口一年里有 34 天只给 11 个月，违反 PRD 写的"最近 12 个月" | `adversarial-verdicts.md` V5-B | 待处置 |
| P7 | 前端：后端 400 时整个筛选栏随面板卸载，操作员无法把范围调回来 | `260901.md` §5.3 | 待处置 |
| P8 | 前端：P95 越界按精确值画，中文文案承诺了界面上不存在的 `> 120s` | 同上 | 待处置 |
| P9 | 双向覆盖校验对三个 native Anthropic 行整体跳过（假声明可通过六个测试） | `260901.md` §5.2 | 待处置 |
| P10 | `usage rebuild-summary` 用未经认证的 WAL 重放并 durably 写两个派生物 | 同上 | 待处置 |
| P11 | `providers.types.*` i18n 无守卫，而 golden fixture 已带 `provider_types`，只差十行 | `260901.md` §4 | 待处置 |
| P12 | 汇总遍历不接 context、admin server 无 `WriteTimeout`；一年满负荷 = 2.01 s 读事务 | `adversarial-verdicts.md` V5-C | 待处置 |
| P13 | 文档漂移：`provider_table.go:378` 与 `:409` 自相矛盾；`adding-a-platform.md` 两处路径过时 + 计数写"Two"实列三条；`manifest.go` 的 "fourteen"/"65" 计数；`cmd/halro/main.go:315` 的 `--provider-type` 帮助文本缺 minimax/anthropic | `260901.md` §5.4 | 待处置 |
| P14 | `performance-baseline.md` 对路由解析差 12 倍（两个版本同时复核，漂移早于 v0.4.0），已不能作回归判据 | `runtime-evidence.md` R4 | 待处置。下一轮评审会把它误报成新回归 |
| P15 | `ledger verify` 在零帧实例上把"没有"说成"不能认证"（v0.3.0 记过，第三次观测） | `runtime-evidence.md` R3 | 待处置 |
| P16 | `ledger verify` 会静默把数据目录单向迁移（与 P1 同源，但改它是命令契约变更） | `runtime-evidence.md` R2-a | **未修**，改为在发布说明写死操作顺序 |
| P17 | `bootstrap` 用 `DefaultProviderCapabilities(type)` 而非默认 profile 的 Defaults，Bedrock 连接开箱少声明五项能力 | `260901.md` §5.4 | 待处置 |
