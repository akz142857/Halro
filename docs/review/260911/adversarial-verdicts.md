# S4 对抗裁决汇总

基线：`main@222d08f84f61493fc9a273d351cc728528d6e30c`，回滚基线 `v0.7.1@84f2638e973f23935b9eda423143f65ff25a852c`。

S4 采用非原 finding 作者复核；评审者必须回到入口、持久化边界、wire body 或独立运行时实验尝试证伪。完整证据见：

- [adversarial-accounting-reviewer.md](roles/adversarial-accounting-reviewer.md)
- [adversarial-security-reviewer.md](roles/adversarial-security-reviewer.md)

| Finding | 非原作者裁决 | 最终级别 | 关键独立证据 |
| --- | --- | --- | --- |
| F-001 旧版 rollback/schema/checkpoint fail-open | 两位均 **CONFIRMED** | P1 / 高 | 两套 tag 构建实验均确认 schema8 下 old `serve` ready=200；显式 Verify 才拒绝；checkpoint v13 前向字段无版本闸 |
| F-002 pending lease 恢复丢归因 | 安全评审者 **CONFIRMED** | P1 / 高 | overlay 同时覆盖 not_started/started，reservation 三项完整而 settlement 三项为空 |
| F-003 Provider credential 进入 capture | 账务评审者 **CONFIRMED** | P1 / 高 | 独立 canary 进入 capture；调用链确认 payload GET 仅 requireAdmin，read_only 可读 |
| F-004 条款 revision 后旧连接继续装载 | 两位均 **CONFIRMED** | P1 / 高 | durable models 无 revision；真实 loader 成功装载无 persisted revision 的 restricted provider |
| F-005 BigModel `none` 静默丢弃 | 两位均 **CONFIRMED** | P1 / 高 | wire body 同时缺 `thinking`/`reasoning_effort`；官方默认 thinking 语义使 omission 改变调用者意图 |

## 裁决修正

R4 原始报告把 `glm-4.7` 描述为只能拒绝 `none` 的“强制推理”路径。两位对抗评审查到当前官方契约允许对该模型显式发送 `thinking.type=disabled`。因此最终 finding 是：**Halro 不得静默省略 `none` 并回落到默认 thinking**；合格修复可以是精确翻译为 disabled，而不一定拒绝请求。对真正不可关闭或 unknown 的目标仍应在 Provider I/O/额度预留前 fail closed。

## S4 结论

五个 P1 都没有被证伪，且每项至少有一位非原作者给出入口级或运行时独立证据。按 review-plan 的停止规则，当前 SHA 不得进入 release dry-run、tag 或正式发布。P2/P3 尚未做逐项第二评审，不影响 NO-GO；应在修复批次中分别关闭或由具名 owner 接受。
