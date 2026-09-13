# Halro 系统设计哲学评估：范围与主张冻结

- 评估日期：2026-09-13
- 目标提交：`1d48ecde216ef40e653738f8bdc9657f88a717cc`
- 目标 tag：`v0.8.0`
- 分支：`main`，开始时与 `origin/main` 一致
- 方案：[`../260913/software-design-philosophy-assessment-plan.zh-CN.md`](../260913/software-design-philosophy-assessment-plan.zh-CN.md)
- 冻结状态：第一轮多角色发现与非作者反证完成；当时 S3/S4 运行覆盖约 70%，专业总分暂不成立

> 后续复评（2026-09-13）：整改候选工作树的保守加权覆盖为 83.68%（展示为 84%），总分为
> 66.5/100（展示为 67/100）；六个首轮 active P2 已在候选上关闭。候选尚未发布，真实 Provider、
> KMS/PKI、Contact Point、clean-host v0.8.0 包渠道和 24h soak 仍为 BLOCKED/UNVERIFIED，生产准入
> 继续 No-Go。当前状态见 [顺序整改复评](remediation-report.md) 和 [复评评分卡](scorecard.md)。

## 1. 工作区边界

评估开始时产品源码相对目标提交无改动。已有未提交内容只有本轮方案文件和
`docs/review/README.md` 的索引更新；后续只允许新增或修改本评估目录中的报告、索引和隔离证据。

禁止在本轮自行修改产品代码、commit、push、建 PR、tag、发布、读取现有真实数据目录，或调用可能
计费的真实 Provider、真实 KMS 和外部告警接收端。所有运行实验使用 `/private/tmp` 下的独立缓存、
数据目录和 fake service。

## 2. 建立范围时的工具链

| 工具 | 版本 |
| --- | --- |
| Go | `go1.26.6 darwin/arm64` |
| Node.js | `v24.18.0` |
| npm | `11.16.0` |
| Git | `2.50.1 (Apple Git-155)` |

仓库规模仅作为调查信号，不直接计分：

| 面 | 数量 |
| --- | ---: |
| `cmd/` + `internal/` Go 文件 | 670 |
| 其中 Go 测试文件 | 392 |
| `web/src` 文件 | 115 |
| 其中 Vitest 测试文件 | 44 |
| `docs/` Markdown 文件（含本轮新增） | 282 |
| GitHub Actions workflow | 3 |

## 3. 待验证的公开主张

以下来自 README、ADR、契约与运维资料，均是待验证命题而不是已通过结论：

1. Halro 是自托管、单 Go 二进制、security-first 的 LLM Gateway / access-control boundary。
2. 单进程、单写者、单数据目录是当前正确性边界；HA / Cluster 尚未实现多写。
3. Project 是预算、限流、并发、模型授权、Token Guard 和未来分片的责任边界。
4. Accounting Ledger 是唯一账务权威；Usage、checkpoint 和 Parquet 是可重建派生物。
5. Provider I/O 前 reservation 已持久化；未知上游结果保守结算且不产生不安全 fallback。
6. Provider credential 不离开本地控制边界；Gateway Key 不以可恢复明文持久化。
7. SafeTransport、redaction、审计、backup/restore 和密钥轮换组成 fail-closed 安全与恢复边界。
8. OpenAI Chat / Embeddings / Stateless Responses 与 Anthropic Messages 有明确兼容契约；实验资源能力不
   冒充 GA；Realtime、stateful Responses 和 `/v1/models` 不在实现范围内。
9. 模型存在性、能力声明、探测证据和未知状态被分开处理。
10. Admin、Metrics 和 dead-man 提供可操作但有界的运行证据，不把内部健康误报为外部可达。
11. 发布产物、依赖、许可证、SBOM、签名、provenance、源码和嵌入式前端能够绑定同一 SHA。
12. Halro 不是 Agent 编排、通用工作流、训练或全栈可观测平台。

## 4. 评估角色

第一轮发现型评审互不读取中间结论：

| 角色 | 独立范围 | 报告 |
| --- | --- | --- |
| A 产品、架构与体验 | D1、D2、D9 | `roles/product-architecture-ux.md` |
| B 正确性、数据与 Provider | D3、D6 | `roles/correctness-data-provider.md` |
| C/D 安全、SRE 与性能 | D4、D5、D8 | `roles/security-sre-performance.md` |
| E 测试、证据与供应链 | D7、D10 | `roles/testing-evidence-supply-chain.md` |

第二轮由非 finding 作者复核 P0/P1 与结构性 P2，统一裁决只写入
`adversarial-verdicts.md`。同一角色的重复阅读不得标为独立验证。

## 5. 证据分层

- E0：声明或设计文档；
- E1：当前目标 SHA 的静态代码和配置；
- E2：目标 SHA 的自动化、故障注入、fuzz、契约或 fake-provider 证据；
- E3：目标 SHA 的真实二进制、浏览器、恢复、性能或短稳实验；
- E4：获授权的真实 Provider/KMS、多平台、长期或外部独立证据。

本轮默认目标是专业评估档：完成 E1–E3 和独立证伪。由于没有真实凭据、生产数据和计费授权，E4
缺口必须显式保留，不得被本地绿色结果替代。
