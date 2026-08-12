# v1.0.0 放行评审方案（2026-08-11）

本目录是**方案**，不是报告。执行后的产出按第 8 节落到同目录。

| 文件 | 内容 |
|---|---|
| [review-plan.md](review-plan.md) | 主方案：本轮性质、基线与增量、继承核对、15 个角色、必须有结论清单、对抗验证、放行判据、执行方式与产出物 |
| [scoring-rubric.md](scoring-rubric.md) | 评分体系：八个维度的 0~10 锚点、权重、封顶规则、分数与放行判定的映射 |
| [role-prompts.md](role-prompts.md) | 可直接使用的角色提示词：通用前置块、15 个角色各自的目标与下限、拒答自检与处置 |
| [kickoff.md](kickoff.md) | 派发说明：执行顺序、每一步的提示词原文、各步之间的判定点 |

执行后的产出（按方案 §8 落在本目录）：

| 文件 | 内容 |
|---|---|
| [release-1.0.0-report.md](release-1.0.0-report.md) | 放行评审报告（冻结件，评审后不再改动） |
| [carry-forward.md](carry-forward.md) / [progress.md](progress.md) | 继承台账 / 整改台账（可变） |
| [remediation-verification.md](remediation-verification.md) | 整改核对（只读复核 → 修复收尾 → 再核对收尾，三阶段） |
| [post-remediation-scorecard.md](post-remediation-scorecard.md) | 整改后按同一套 rubric 重新评分，并给出当前系统的能力边界 |

上游框架（角色定义、方法论、历史评审）见 [`docs/review/README.md`](../../README.md)。

三件事先说清楚，避免读者误判本轮的位置：

1. **[260809 的放行评审方案已写但未执行](../../260809/review-plan.md)**——该目录下只有方案与一份 CI 门禁记录，没有 `260809.md` 报告。本轮是它的继任者，继承它的未闭环项，不重复它的框架论证。
2. 那之后实际发生的是两轮聚焦评审：[260810 视觉评审](../../260810/260810.md)（7.2/10）与 [260811 API 全链路评审](../provider-to-project-api-chain.zh-CN.md)（7 条 finding + 3 项子项全部关闭）。两者的结论进入本轮的**继承核对**，作者自述的"已关闭"要独立复核。
3. 本方案预期由 Fable 5 执行。第 4.3 节给出模型合规与拒答处置，`role-prompts.md` 的通用前置块把这些约束写成了提示词本身的一部分——那一节不是背景说明，是执行要求。
