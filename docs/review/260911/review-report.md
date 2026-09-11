# Halro v0.8.0 全面评审报告

日期：2026-09-11
候选：`main@222d08f84f61493fc9a273d351cc728528d6e30c`
基线：`v0.7.1@84f2638e973f23935b9eda423143f65ff25a852c`
结论：**NO-GO — 不应以当前 SHA 发布 v0.8.0**

> 整改更新（2026-09-11）：本报告针对的原始 SHA 仍是 NO-GO；其上的本地工作区已经完成
> F-001～F-017 与 C-002～C-007 的整改，并通过完整本机 Go/前端/发布契约门禁。C-001 被明确
> 标记为 operator-declared/unverified 残余。详见 [`remediation.md`](remediation.md)。包含本文件的
> 整改提交尚无精确 SHA 的 CI、release dry-run 或正式 GO。

## 决策摘要

自动化质量信号良好：本机完整 Go/前端门禁、聚焦 race、备份恢复、三语言 SDK 证据、性能/包体，以及精确 SHA 的 CI（全量 race、fuzz、漏洞扫描、容器、SBOM）均为绿。性能没有超过 10% 的时间回退，前端 gzip 初始包体仅增长 1.30%。

但全面 review 发现并经非原作者确认了 5 个 P1：

1. v0.7.1 对 schema8/v13 新字段 fail-open，仍 ready 并可能静默重写丢失 Usage 归因；
2. 当前版本 crash recovery 的 pending lease settlement 丢 Offering/Profile/Region；
3. Provider 错误正文可把 credential 带入 failure capture 并暴露给 read_only Admin；
4. 受限产品条款 revision 更新后旧连接仍继续接流量；
5. BigModel `reasoning_effort=none` 被省略并回落到默认 thinking。

此外有 10 个确认 P2、2 个 P3 和 7 个候选问题，覆盖发布供应链、capture 上限、前端错误/导航与持久化语义 hardening。

## 建议修复顺序

1. **先修持久化版本闸 F-001**：它决定 rollback/派生数据可信度；补四段双二进制、逐字段回归。
2. **修 F-002/F-003**：恢复归因与 credential 边界直接影响账务和安全；补 durable boundary/canary 测试。
3. **修 F-004/F-005**：把 policy revision 持久化并在 loader fail closed；精确翻译 BigModel thinking policy。
4. 处置 F-006～F-015；P3 可随 UI 批次关闭。同步修复 release workflow 与归档工具契约。
5. 所有代码修复完成后，只跑受影响窄测迭代；最终 push 前一次完整 Go/前端门禁、bundle drift、SDK、fuzz/stress 与升级/恢复演练。
6. 由非修复作者重新执行 S4。只有零未关闭 P0/P1、P2 有 owner 处置，才允许 exact-SHA release dry-run。

## 证据索引

- [范围与触发矩阵](range-map.md)
- [统一 findings](findings.md)
- [运行时证据](runtime-evidence.md)
- [S4 对抗裁决](adversarial-verdicts.md)
- [阶段进度](progress.md)
- [完成度审计](completion-audit.md)
- [整改记录](remediation.md)
- [R1 架构与领域](roles/architecture-domain.md)
- [R2/R6 账务与持久化](roles/core-accounting-persistence.md)
- [R3 安全与隐私](roles/security-privacy.md)
- [R4 Provider 与兼容性](roles/provider-compatibility.md)
- [R5 前端与可用性](roles/frontend-usability.md)
- [R7/R8 发布与供应链](roles/release-supply-chain.md)
