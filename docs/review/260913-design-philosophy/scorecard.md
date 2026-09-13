# 十维评分卡（顺序整改复评）

评估基线：`v0.8.0@1d48ecde216ef40e653738f8bdc9657f88a717cc`

整改候选：已提交的 `b7edd4d24152582e5978cf58d1917fedd803e542` 加当前未提交工作树；尚未形成新的
候选提交 SHA，也未发布。E3 使用的候选 executable SHA-256 见
[E3 completion evidence](evidence/e3-completion/README.md)。

本次复评的保守加权检查覆盖为 **84%**，达到方案规定的 80% 计分门槛，因此首次给出可追溯总分：
**67/100（高风险）**。这表示当前架构方向可以保留，但生产证据仍不足；它不等于生产准入通过。
这是所记录整改工作树的评估分，不是 exact-SHA release 分：候选提交冻结后仍须重跑受影响门禁，才能
把结果绑定到可发布 SHA。

## 1. 维度评分

| 维度 | 权重 | 成熟度 | 加权分 | 最高证据 | 检查覆盖 | 复评裁决 |
| --- | ---: | ---: | ---: | --- | ---: | --- |
| D1 产品边界与价值密度 | 10 | 2.5/4 | 6.25 | E3-local | 80% | 核心窄腰、非目标和 current capability 已统一；实验能力仍缺长期价值/删除台账 |
| D2 架构清晰度与复杂度预算 | 13 | 2.5/4 | 8.13 | E3-local | 90% | operation→primitive 有独立 oracle、durable attribution 与 adapter guard；真实 Provider 尚未逐 profile 证明 |
| D3 正确性、账务与数据耐久 | 16 | 3.0/4 | 12.00 | E3 | 92% | portable final-event 分叉已修；双二进制 Ledger/Usage 升级、拒绝与恢复通过；OS kill-point/KMS 未测 |
| D4 可靠性与可运维性 | 14 | 2.5/4 | 8.75 | E3-local | 72% | accepted-work 告警规则、runbook 和 promtool 语义通过；真实 Contact Point/dead-man/target drill 未闭环 |
| D5 安全与隐私 | 16 | 2.5/4 | 10.00 | E3-local | 80% | capture enqueue 前 byte bound、审计和 fail-closed 路径已证；真实 KMS/IAM/PKI 仍缺 E4 |
| D6 API、Provider 与兼容性 | 10 | 2.5/4 | 6.25 | E3-local | 88% | portable/API/SDK contracts 和 primitive 归属加强；真实上游兼容性仍按 profile 为 UNVERIFIED |
| D7 测试、eval 与反证能力 | 8 | 3.0/4 | 6.00 | E3 | 90% | fresh-cache、race、fault、fuzz、SDK、stress 与独立 sabotage 边界完整；真实上游和长稳未闭环 |
| D8 性能与容量诚实度 | 5 | 2.5/4 | 3.13 | E3-local | 70% | 8 轮交替 benchstat 与 1000 SSE cleanup 已执行；production-shaped 曲线和 24h soak 未测 |
| D9 Admin、CLI、文档与开发体验 | 4 | 3.0/4 | 3.00 | E3-local | 90% | 八页真实数据浏览器旅程、响应式复测、snippet compile 和顶层 help 已通过；辅助技术实机未测 |
| D10 供应链与可持续演进 | 4 | 3.0/4 | 3.00 | E3 | 82% | 完整本地门禁与下游 preflight 已补；v0.8.0 Homebrew/APT clean-host 尚未闭环 |
| **合计** | **100** |  | **66.5 ≈ 67** | **E4：0** | **83.68% ≈ 84%** | **高风险；生产准入 No-Go** |

分数使用原方案公式 `Σ(维度权重 × 维度成熟度 ÷ 4)`，按未四舍五入值计算为
`66.5`，展示为 `67/100`。覆盖率不是代码覆盖率；各维度覆盖百分比是对原方案检查项和必做实验的
保守完成度判断，再按同一维度权重聚合：
`10×80% + 13×90% + 16×92% + 14×72% + 16×80% + 10×88% + 8×90% + 5×70% + 4×90% + 4×82% = 83.68`，
即 `83.68% ≈ 84%`。成熟度与覆盖率是两条独立轴，不能相乘后再次折减总分。

`83.68%` 是已给定各维度覆盖判断的算术结果，不表示覆盖输入精确到小数点后两位；输入仍是评审者
对检查项和实验完成度的保守判断。决策展示使用 84%，避免制造不存在的测量精度。

`E3-local` 是对 E3 的范围限定，只证明本机真实进程、文件或浏览器路径；它不是新增证据等级，也不能
替代 target/production 的 E4。

## 2. 红线与准入裁决

| 红线 | 状态 | 说明 |
| --- | --- | --- |
| P0 | 未发现 | 没有形成跨租户越权、大规模秘密泄漏、不可恢复账务破坏或远程控制的完整可达链 |
| P1 | 未确认 | 没有活动 P1；capture byte bound 已在入队前和 Store 边界双重执行 |
| P2 | 无活动项 | 首轮六个 P2 均已实现整改或以独立证据关闭；详细状态见复评报告 |
| D3 E3 | 本地取得 | 完整 Ledger/Usage 双二进制升级、旧读者拒绝、backup/restore 与 rollback restore 通过；OS kill/KMS 不在此结论内 |
| D5 E3/E4 | 部分取得 | 文件和进程级负向路径已证；真实 KMS/IAM/PKI、heap/core 与外部复核未执行 |
| 关键外部主张 | BLOCKED / UNVERIFIED | v0.8.0 clean-host 包渠道因 Homebrew stale、APT 503 和缺有效 App 凭据而 BLOCKED；真实 Provider、KMS、Contact Point、独立 dead-man、辅助技术实机和 24h soak 的主张仍 UNVERIFIED |

最终系统裁决是：

**`ORDERED REMEDIATION COMPLETE / EXTERNAL EVIDENCE BLOCKED / PRODUCTION UNVERIFIED`**

67 分只解除“低于 80% 时不得计分”的程序性限制，没有解除生产 No-Go。架构方向保留，但只有在 D3/D5
外部红线和渠道验收取得可验证 evidence ID 后，才可重新讨论生产准入。

## 3. 重新评分条件

- 在新的精确提交 SHA 上重跑本评分卡所引用的完整本地门禁；不得把脏工作树结果当 release 证据。
- 对真实 Provider、KMS/PKI、Contact Point、package clean-host 和 24h workload 分别保留原始输出、时间、
  target identity 和不可变 evidence ID。
- 外部实验必须记录失败注入、回滚结果和观察者；“命令退出 0”不能单独提升为 E4。
- 新证据只升级对应维度，不能用本地 E3 推断未执行的 production E4。
