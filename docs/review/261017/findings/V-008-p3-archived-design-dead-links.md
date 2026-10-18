# V-008 — P3：归档的 v1 设计稿有 46 个死链

- 基线：冻结 `main` `2e749db1`
- 段/功能：D 段 #405（`a58103d2`）
- 分类：【建议】

`docs/prd/memory-bounded-state-design-v1.zh-CN.md` 引用了 46 个仓库中不存在的相对路径（如 `memory-bounded-convergence-plan.zh-CN.md`、`../verification/performance/2026-10-08-ha-resource-vector/README.zh-CN.md`、`../contracts/memory-bounded-maintenance.zh-CN.md`）。这些文件只存在于已放弃的 `memory-bounded-state` 分支。建议在文件头注明链接指向该分支，或改为指向该分支上固定提交的绝对链接。

同次全仓链接检查中，`docs/reference-analysis/` 下两个文件的绝对本机路径链接在 v0.8.5 之前就存在，不在本范围。
