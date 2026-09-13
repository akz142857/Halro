# 非作者独立反证裁决

目标版本：`v0.8.0@1d48ecde216ef40e653738f8bdc9657f88a717cc`

第一轮发现者没有裁决自己的 finding。第二轮角色 F 在读取候选后，专门寻找已有防御、不可达条件、
语义误读、过宽范围与过高严重度；允许在 `/private/tmp` 的精确目标副本做故障注入或 mutation，禁止
修改产品工作树和调用真实外部服务。

> 状态说明：本文件冻结首轮非作者反证结果；下文“6 个 active P2”是整改前事实。整改候选已关闭这
> 六项，但尚未发布；当前 finding、分数和外部证据边界以
> [顺序整改复评](remediation-report.md) 和 [复评评分卡](scorecard.md) 为准。

## 1. 裁决总览

| Finding | 发现角色 | 反证角色 | 原裁决 | 最终裁决 | 严重度变化 | 关键反证 |
| --- | --- | --- | --- | --- | --- | --- |
| PHIL-A01 current capability 分叉 | 产品/架构 | 正确性/Provider | CANDIDATE P2 | CONFIRMED | P2 | 准确 README/代码 fail-closed 不能消除 current guides 和 current status 的直接矛盾 |
| PHIL-A02 价值/删除证据 | 产品/架构 | 正确性/Provider | CANDIDATE P3 | PARTIAL | P3 | Run Governance 已有目标用户、指标、停止门；只剩 portfolio adoption/support/sunset 缺口 |
| PHIL-A03 Runtime 集中 | 产品/架构 | 正确性/Provider | CANDIDATE P2 | PARTIAL | P2→P3 | budget 会向下 ratchet，已有子 runtime，未证明当前事故；747 行 Open 仍是渐进收缩信号 |
| PHIL-A04 primitive binding | 产品/架构 | 正确性/Provider | CANDIDATE P2/E1 | CONFIRMED | P2/E2 | 目标副本交换 OpenAI/DeepSeek primitive，完整 provider tests 仍通过 |
| PHIL-A05 Go snippet | 产品/架构 | 正确性/Provider | CANDIDATE P2 | CONFIRMED | P2 | 精确非流式 snippet 放入最小 Go 程序后编译失败 |
| PHIL-A06 CLI help | 产品/架构 | 正确性/Provider | CANDIDATE P3 | CONFIRMED | P3 | 目标 binary `--help`/`help` 均退出 1；无参数/subcommand help 降低影响 |
| PHIL-B01 portable stream finalization | 正确性/Provider | 安全/SRE | CONFIRMED P2 | CONFIRMED | P2 | 第二份独立 final-event fault injection 同样得到 caller error + Ledger success |
| PHIL-B02 双二进制升级/恢复 | 正确性/Provider | 安全/SRE | UNVERIFIED P2 | PARTIAL | P2→P3 | 已直接验证 schema 36→37 与 old-reader refusal；真实 Ledger/Usage、backup/restore、kill-point 仍缺 |
| PHIL-B03 真实 Provider | 正确性/Provider | 安全/SRE | UNVERIFIED P2 | CONFIRMED gap | P2→P3 | 缺口属实；SDK contract、strict decoder、no-retry 和 owner 明示边界把风险收窄到具体 profile 可用性 |
| PHIL-B04 started-before-socket | 正确性/Provider | 安全/SRE | ACCEPTED P3 | CONFIRMED constraint | P3 | 跨本地 WAL/远端 socket 无法原子；保守 recovery 正确，“一个或少量”不是严格上界 |
| PHIL-CD-001 capture queue bytes | 安全/SRE | 产品/架构 | CANDIDATE P1 | PARTIAL | P1→P2 | 未截断 backing bytes 事实成立；默认关闭、多条件入口、无 RSS/OOM 实测不足以定 P1 |
| PHIL-CD-002 read_only payload | 安全/SRE | 产品/架构 | CANDIDATE P2 | REFUTED as defect | P2→ACCEPTED | role、route、threat comment、UI 与 E2 安全测试共同明确实例级全 GET 契约 |
| PHIL-CD-003 关键退化告警 | 安全/SRE | 产品/架构 | CANDIDATE P2 | PARTIAL | P2 收窄 | pricing 已有 readiness→dead-man；只保留 shutdown-truncated 与 aged pending lease 强缺口 |
| PHIL-CD-004 无生产 SLO | 安全/SRE | 产品/架构 | UNVERIFIED P2 | REFUTED as defect | P2→ACCEPTED | admission checklist 在证据缺失时强制 No-Go，未发现 production-ready 冒充声明 |
| PHIL-E-001 `make check` 范围 | 测试/供应链 | 安全/SRE | CANDIDATE P2 | PARTIAL | P2→P3 | 技术遗漏成立，但 help/AGENTS/release assessment 没把它单独称为完整门禁，CI/release 有覆盖 |
| PHIL-E-002 Go result cache | 测试/供应链 | 安全/SRE | CANDIDATE P3 | PARTIAL | P3 | `-shuffle` 禁 cache，故 make test 反例成立；race、CI/release 仍可 cache |
| PHIL-E-003 downstream preflight | 测试/供应链 | 安全/SRE | CANDIDATE P2 | PARTIAL | P2→P3 | 自动 preflight 缺失；但有人工 checklist、渠道 saga、acceptance 后宣传和可重放恢复 |
| PHIL-E-004 release 文档漂移 | 测试/供应链 | 安全/SRE | CANDIDATE P3 | CONFIRMED | P3 | current 文档说无 drift，workflow 明确 build 后执行 diff |

## 2. 裁决净变化

- 发现阶段：1 个 P1 候选、12 个 P2 候选/缺口、若干 P3/constraint。
- 反证后：0 个 P0、0 个 P1；6 个 active P2；其余降为 P3、accepted constraint 或从缺陷清单撤销。
- 反证不是投票。每次降级都有入口、默认值、真实防御或新的运行证据；每次确认都有当前 SHA 的
  静态完整链、编译/故障注入/mutation 或二进制复现。

首轮反证结束时的 active P2 是：PHIL-CD-001、PHIL-B01、PHIL-A01、PHIL-A04、PHIL-A05、PHIL-CD-003。

## 3. 独立运行证据

- PHIL-A04：隔离目标副本交换两个同类型 primitive 后，`go test -count=1 ./internal/provider` 仍 PASS。
- PHIL-A05：精确生成 snippet 包入最小 Go program 后编译 exit 1，报 `resp`/`err` 未使用。
- PHIL-A06：目标 binary build 成功；`halro --help` 与 `halro help` exit 1。
- PHIL-B01：第二位评审者独立注入只发生在最终 `response.completed` 的 write error，caller error 与
  Ledger success 分叉再次复现。
- PHIL-B02：v0.7.1 临时数据经 v0.8.0 迁移到 schema 37，v0.8.0 doctor PASS；v0.7.1 doctor 明确拒绝
  schema 37 / Usage manifest 8。
- PHIL-CD-001：阻塞 capture store 测试证明请求不等待、64 槽队列可填满、shutdown 可取消；静态确认
  queued RawMessage backing bytes 在 store 截断前仍存活。未做大内存/OOM 实验。
- PHIL-CD-002：真实 Runtime/router 测试创建 read_only 并读取 payload HTTP 200，同时 secret canary
  不进入 payload/响应/日志/Audit/磁盘。
- PHIL-E-002：`-shuffle=on` 连跑不显示 cached；`go test -race` 第二次显示 `(cached)`。

## 4. 首轮仍未获得的独立证据

- capture 大请求 storm 的 peak RSS、GC pause 与 p99；这决定 PHIL-CD-001 是否需要重新升 P1。
- portable Messages 的 handler-level disconnect 与两个 facade 修复后的端到端回归；当前时序和
  Responses fault injection 足以确认 P2，不能证明未来修复。
- 真实 Provider/KMS/PKI/Contact Point、独立故障域、24h soak、多架构 clean-host package install。
- 包含真实 Ledger/Usage 历史的 pre-upgrade backup、restore、kill-point 和旧 binary 复核。

## 5. 角色原始报告

- 发现：[product-architecture-ux.md](roles/product-architecture-ux.md)、
  [correctness-data-provider.md](roles/correctness-data-provider.md)、
  [security-sre-performance.md](roles/security-sre-performance.md)、
  [testing-evidence-supply-chain.md](roles/testing-evidence-supply-chain.md)。
- 反证：[adversarial-product-architecture.md](roles/adversarial-product-architecture.md)、
  [adversarial-correctness-provider.md](roles/adversarial-correctness-provider.md)、
  [adversarial-security-sre.md](roles/adversarial-security-sre.md)、
  [adversarial-testing-release.md](roles/adversarial-testing-release.md)。
