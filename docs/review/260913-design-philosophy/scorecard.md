# 十维评分卡（第一轮，尚未形成有效总分）

目标版本：`v0.8.0@1d48ecde216ef40e653738f8bdc9657f88a717cc`

本轮十个维度都有独立角色结论，但专业方案要求的 S3/S4 端到端、故障、恢复、容量和操作者实验
尚未达到 80% 覆盖。按原方案纪律，**不输出 100 分数值总分，也不套用 90/80/70/60 分档**。
下表成熟度用于指出当前证据位置，不应相加后对外宣传。

## 1. 维度评分

| 维度 | 权重 | 当前成熟度 | 最高证据 | 估计检查覆盖 | 核心裁决 |
| --- | ---: | ---: | --- | ---: | --- |
| D1 产品边界与价值密度 | 10 | 2.0/4 | E2 | 65% | 核心窄腰与非目标清楚；当前能力文档分叉，实验能力缺价值/删除账本 |
| D2 架构清晰度与复杂度预算 | 13 | 2.0/4 | E2 | 75% | 权威、热路径、提交点清楚；`Runtime` 组合根集中，operation→primitive 缺独立机械证明 |
| D3 正确性、账务与数据耐久 | 16 | 2.0/4 | 局部 E3 | 80% | reservation/settlement/replay 强；portable stream 最终交付与 RequestFinalized 可分叉；已验最小 schema 升级/拒绝，完整恢复仍缺 |
| D4 可靠性与可运维性 | 14 | 2.0/4 | E2 | 62% | readiness、单写、backup/dead-man 设计完整；SLO/真实通知/target drill 未闭环，少量关键信号缺 actionable rule |
| D5 安全与隐私 | 16 | 2.0/4 | E2 | 70% | SafeTransport、加密、审计、rotation fail-closed 强；capture 队列缺 enqueue 前字节预算，真实 KMS/PKI 未验证 |
| D6 API、Provider 与兼容性 | 10 | 2.0/4 | E2 | 75% | malformed/ambiguous/no-fallback 与证据分层完整；portable stream P2 与真实 Provider/SDK 缺口限制成熟度 |
| D7 测试、eval 与反证能力 | 8 | 3.0/4 | E3 | 80% | 单测、fault、fuzz、race、SDK/stress 与目标 SHA CI 很强；持续 mutation、真实上游、长稳未闭环 |
| D8 性能与容量诚实度 | 5 | 1.5/4 | 局部 E3 | 35% | 有微基准和许多静态上界；缺尾延迟、RSS/FD/goroutine、慢流、容量曲线与 24h soak |
| D9 Admin、CLI、文档与开发体验 | 4 | 2.0/4 | E2 | 55% | server-backed onboarding 与操作恢复语义良好；文档漂移、Go snippet、CLI help 有缺口，未做真实浏览器旅程 |
| D10 供应链与可持续演进 | 4 | 3.0/4 | E3 | 80% | 锁文件、固定 actions、SBOM/签名/provenance 成熟；正式 v0.8.0 下游渠道未闭环，证据文档有漂移 |
| **覆盖/总分** | **100** | **N/E** | **E4：0** | **约 70%** | **低于 80% 门槛；禁止生成官方总分** |

覆盖率是按各角色对原方案检查项和必做实验的完成比例做的保守估计，再按维度权重聚合；它不是
代码覆盖率。`UNVERIFIED` 仍是有效评估结论，但只阅读代码、没有执行方案要求的运行实验时，不计作
已完成的 S3/S4 覆盖。

## 2. 红线裁决

| 红线 | 状态 | 说明 |
| --- | --- | --- |
| P0 | 未发现 | 没有形成跨租户越权、大规模秘密泄漏、不可恢复账务破坏或远程控制的完整可达链 |
| P1 | 未确认 | failure capture 内存放大最初为 P1 候选，经非作者反证降为 P2；需大载荷 RSS/GC 实验决定是否升级 |
| D3 E3 | 部分取得 | v0.7.1→v0.8.0 schema 升级与旧读者拒绝有双二进制 E3；完整 Ledger/Usage、backup/restore、kill point 与真实磁盘故障仍缺 |
| D5 E3 | 未取得 | 安全负向测试为 E2；真实 KMS/IAM/PKI、heap/core、目标身份边界和外部复核未执行 |
| 关键外部主张 | UNVERIFIED | 真实 Provider、真实 KMS、真实 Contact Point、独立 dead-man、Homebrew/APT clean-host、24h soak |

因此第一轮系统裁决是：`REMEDIATION REQUIRED / PRODUCTION UNVERIFIED`。这不是 release 的统一红绿灯；
核心 GitHub Release/GHCR、Homebrew、APT、生产准入必须分别判断。

## 3. 评分如何升级

- D3：修复 portable stream 终态时序；完成 v0.7.1→v0.8.0→拒绝/restore 的双二进制演练与 kill points。
- D4/D5：执行 target PKI/KMS、真实 Contact Point、独立 dead-man、backup/restore 和非作者 15 分钟 drill。
- D8：补 production-shaped 慢流/大正文/多 Project 曲线、RSS/GC/FD/goroutine、WAL/Journal/TSDB 增长和 24h soak。
- D1/D2/D9：收敛 current-capability 真相、验证扩展绑定、运行真实浏览器关键旅程并做 snippet compile gate。
- D7/D10：加入最小 mutation/sabotage 与发布前下游凭据 preflight；取得 clean-host 包渠道验收证据。

只有补证后重新冻结同一 SHA 或新候选 SHA，才可重算覆盖率和 100 分总分；不得把本轮与未来另一个
提交的绿色证据拼接。
