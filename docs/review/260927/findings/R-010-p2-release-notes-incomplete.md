# R-010 — P2 CONFIRMED：当前 Unreleased 不能完整生成发布说明

`CHANGELOG.md` 的 Unreleased 在本范围中只更新到第 27 个提交；其后 25 个提交没有同步，缺少 Kimi/Claude subscription、Advisor、metadata journal、流式首字节/预算/审计、build/config gates 和 HA 等主要变化。

发布指南说明门禁只检查目标版本 section 存在，不检查完整性（`docs/guides/releasing.md:37-45`），同时 GitHub Release notes 又直接来自该 section（同文件 `82-85`），且流程要求每个 PR 写 changelog（`160-176`）。因此现有门禁可以发布一份形式合法但实质缺项的说明。

关闭条件：补齐 delta 清单，并让 release preparation 至少核对本次范围中的 user/ops-facing changes 是否有归属或显式 `no changelog` 决策。
