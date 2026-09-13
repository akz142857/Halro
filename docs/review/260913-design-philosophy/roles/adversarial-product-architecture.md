# 角色 F：对角色 A 产品、架构与体验 findings 的独立反证

## 1. 范围与方法

- 目标源码：`v0.8.0@1d48ecde216ef40e653738f8bdc9657f88a717cc`。
- 被复核报告：`roles/product-architecture-ux.md` 的 PHIL-A01 至 PHIL-A06。
- 独立性：本角色不是上述 findings 的作者；先读取候选结论，再逐项寻找已有防御、不可达条件、较窄真实问题和能推翻严重度的证据。
- 允许动作：目标 SHA 静态核查、现有窄测、`/private/tmp` 精确 `git archive HEAD` 副本中的破坏实验、临时二进制与 snippet 编译。
- 禁止动作：没有修改产品源码，没有 commit / push，没有真实或计费 Provider 调用，没有外部网络调用。

## 2. 裁决摘要

| Finding | 最终状态 | 最终严重度 | 置信度 | 反证结论 |
| --- | --- | --- | --- | --- |
| PHIL-A01 文档真相分叉 | **CONFIRMED** | **P2** | HIGH | 精确 current docs 之间确有直接矛盾；准确 README / AWS 专门指南和代码 fail-closed 只能降低运行影响，不能消除错误指导。 |
| PHIL-A02 缺能力价值 / 删除证据 | **PARTIAL** | **P3** | HIGH | “Run Governance 未找到目标用户、指标和退出门”被当前 PRD 直接反驳；仅保留较窄的跨能力 portfolio adoption / support-cost / sunset 证据缺口。 |
| PHIL-A03 Runtime 复杂度只涨不缩 | **PARTIAL** | **P3**（由 P2 下调） | HIGH | 预算测试会在字段或 mutex 减少时失败并强制下调，且已有子 runtime 聚合；仍存在 747 行 `Open` 和集中生命周期，但未证明 P2 级可达故障或不可控增长。 |
| PHIL-A04 operation→primitive 无独立机械证明 | **CONFIRMED** | **P2** | HIGH | 在目标 SHA 副本交换 OpenAI / DeepSeek chat+stream primitive 后，完整 `internal/provider` 测试仍通过；证据从候选 E1 提升为当前 SHA E2 sabotage。 |
| PHIL-A05 Workbench 非流式 Go snippet 不可运行 | **CONFIRMED** | **P2** | HIGH | 精确 snippet 包进最小 Go 程序后因 `resp`、`err` 未使用编译失败；页面明确提供“复制代码”，Preview 不足以反驳基本编译要求。 |
| PHIL-A06 顶层 CLI help 不标准 | **CONFIRMED** | **P3** | HIGH | 目标 SHA 二进制实测 `halro --help` / `halro help` 均退出 1；无参数和子命令 `--help` 是有效缓解，不是否定。 |

## 3. 逐条反证

### PHIL-A01 — “当前能力”文档真相分叉

- 最终状态：**CONFIRMED**
- 最终严重度：**P2（维持）**
- 原类型：DEFECT
- 反证尝试：查找是否有更靠近入口的当前版本警告，或候选引用只是历史文档。根 `README.md:255-302` 清楚区分 served / withheld；`docs/guides/aws-surface-selection.md:1-7,28-38` 也在页首声明当前构建只开放 Mantle。这证明项目并非完全没有正确答案。
- 无法推翻的证据：同属 current guide 的 `docs/guides/operator-guide.md:1010-1027` 仍把 Bedrock Runtime / Agent Runtime 放进“Provider setup”表并描述为 Beta；`docs/guides/user-guide.zh-CN.md:163-186` 的完整配置步骤和 Provider 参数表也把 Runtime 写成可选 Beta。代码则在 `internal/domain/provider_table.go:277-299,309-315` 标记 `Withheld=true`。操作者从不同官方入口会得到相反答案。
- Governance 分叉同样成立：`docs/architecture/run-governance-data-flow.zh-CN.md:1-6` 写“生产实现尚未开始”，而当前 Runtime 注册公开 Work Unit / Outcome 路由（`internal/app/runtime.go:1706-1709`）、Admin 查询路由（`internal/app/runtime.go:1794-1798`），前端也路由到 Run Governance 页面（`web/src/App.tsx:165`）。这不是未来设计文档中的历史段落，而是页面状态行。
- current-status 反证失败：`docs/README.md:43-48` 明确把 `docs/milestones/implementation-status.md` 称为当前实现依据；该文件自称 current source tree，却更新时间停在 2026-08-04、没有 Run Governance，并仍列 `v1.0.0-rc` / `v1.0.0` 为下一关键路径（`docs/milestones/implementation-status.md:1-6,91-110`）。与此同时 `docs/milestones/release-notes-v1.0.0.md:1-7` 明确说明版本线已重置。
- 已有防御：写入路径和 served matrix 拒绝 withheld profile，准确根 README 与 AWS 专门指南能帮助谨慎读者纠偏；因此没有发现不可达 profile 被真正创建，不上调为 P1。
- 最终理由：多个标为当前或操作用途的官方入口互相矛盾，且修复成本低、作用面跨产品范围和升级审查，P2 合理。

### PHIL-A02 — 扩张能力缺少逐项价值与删除证据

- 最终状态：**PARTIAL**
- 最终严重度：**P3（维持）**
- 原类型：EVIDENCE_GAP
- 成功反证：候选把 Run Governance 列为优先补证对象并称未找到目标用户、采用/成功代理指标和删除阈值，但 `docs/prd/prd-run-governance-and-business-outcomes.zh-CN.md:31-65` 已给出问题、四类目标用户和七个用户故事；`:738-768` 定义 coverage、success rate、cost per success 等可观察指标；`:1016-1050` 给完整验收门；`:1059-1067` 给停止扩大条件；`:1080-1081` 明确“没有真实业务试点不得发布 Outcome 产品能力”。这已经是很强的价值与 rollout gate，不能写成“未找到”。
- 其他反证：Developer Workbench 也不是只因代码存在而永久保留；`docs/prd/developer-workbench-execution-plan.zh-CN.md:1-12` 定义具体 job，`:20-40` 给可观察请求契约和五项验收。README 的 Experimental / Withheld / Deferred 分类、已 withheld 的 Bedrock profiles 也证明团队确实会拒绝发布。
- 仍成立的较窄问题：未找到统一、按所有 GA / Experimental / Preview surface 维护的 portfolio ledger，系统性记录 adoption evidence、持续支持成本、晋级责任人与 sunset date。局部 PRD 的目标与工程验收不等于真实用户采用数据，也不能覆盖全部 33 Gateway routes / 12 Admin areas。
- 已有防御：不应把隐私友好、自托管产品强迫成默认遥测；访谈、opt-in reference deployment、支持工单同样可以补证。
- 最终理由：原 finding 的普遍断言和 Run Governance 例证被反驳，但 portfolio 级证据闭环仍缺，保留为 P3 evidence gap，而不是产品缺陷。

### PHIL-A03 — Runtime 复杂度预算不能形成收缩边界

- 最终状态：**PARTIAL**
- 最终严重度：**P3（由 P2 下调）**
- 原类型：DESIGN_DEBT
- 成功反证：`internal/app/runtime_scale_test.go:118-123` 在实际 field / mutex 低于预算时也失败并要求同步降低，因此它不仅要求增长显式，也确实形成 ratchet-down 边界。目标 SHA 运行 `GOCACHE=/private/tmp/halro-rolef-go-cache GO111MODULE=off go test -count=1 runtime_scale_test.go`，退出码 0，实际预算保持 74 / 10。
- 模块化反证：`Runtime` 已把 governance、capability resolution、reload、activation、admin elevation、route withheld 等协调状态聚合成子对象；`runtime_scale_test.go:30-74` 对每次增加给出 owner/lifecycle 理由。342 个 receiver 方法分散在多个 handler 文件中，receiver 数本身不等价于一个 342-method 单文件对象，也未找到因此产生的当前功能错误。
- 仍成立的证据：`Runtime.Open` 从 `internal/app/runtime.go:162-908` 约 747 行，一次启动 10 个后台责任（`:850-907`），并直接组装密钥、多个权威 store、派生视图、listeners 和 workers。新增跨生命周期 subsystem 仍倾向进入同一 composition root；现有测试只能迫使 reviewer 解释增加，不能自行选择下沉边界。
- 严重度反证：候选没有给出由 74 fields / 10 mutexes 导致的可达错序、竞态、泄漏、事故或显著修改失败率。启动函数长也包含大量 fail-closed cleanup 和显式顺序，简单拆函数可能只隐藏依赖而不降低复杂度。
- 最终理由：集中度是可维护性债务，但已有 tight ratchet、子组件和大量局部文件边界；在没有失败证据前按 P3 渐进简化更合适，不支持结构性 P2 的紧迫度。

### PHIL-A04 — Provider Profile operation→primitive 缺独立机械证明

- 最终状态：**CONFIRMED**
- 最终严重度：**P2（维持）**
- 原类型：EVIDENCE_GAP
- 反证尝试：检查 profile validation、constant orphan、profile-specific wiring 和 golden tests 是否能从 adapter 独立事实证明 binding。`internal/provider/profile.go:47-58` 的 validate 仍调用由同一 `profileOperationTable` 推导的允许关系；`internal/provider/registration_guard_test.go:11-71` 能发现未绑定 constant 或 semantic primitive orphan，不能发现两项对调；一些 profile tests 有硬编码 expected，但没有覆盖所有 profile / operation。
- 当前 SHA sabotage：从 `git archive HEAD` 创建 `/private/tmp/halro-rolef-a04.V6FKXI`，仅在副本把 `ProfileOpenAIChatEmbeddings` 的 chat / stream primitive 与 `ProfileDeepSeekChat` 对调，保留所有 constant 被引用和 operation 类型不变。运行 `env GOCACHE=/private/tmp/halro-rolef-go-cache go test -count=1 ./internal/provider`，允许本机 loopback 后退出码 **0**：`ok .../internal/provider 1.354s`。首次受限 sandbox 运行仅因 `listen tcp 127.0.0.1:0: operation not permitted` 退出 1，不计产品失败。
- 影响解释：破坏实验确认“现有测试会不会发现错误 binding”的答案是否定的；它不证明 v0.8.0 当前表里已有错绑。若未来改错，manifest、primitive 过滤与 attempt attribution 会共同接受同一个错误事实，可能在 Provider I/O 后才暴露。
- 已有防御：profile table 已收敛平行清单，profile-specific tests 对 Mantle/Titan/resources 等高风险面有硬编码保护，文档 `docs/contracts/adding-a-platform.md:246-275` 也诚实披露缺口。
- 最终理由：目标 SHA 的独立 mutation 证据把候选从 E1 提升到 E2，结构性 P2 成立；建议仍是从 adapter entry point 取得独立 expected，而不是再从同一表生成测试答案。

### PHIL-A05 — Developer Workbench 非流式 Go snippet 不可运行

- 最终状态：**CONFIRMED**
- 最终严重度：**P2（维持）**
- 原类型：DEFECT
- 反证尝试：检查页面是否只把文本称作伪代码、是否隐藏复制入口、或测试已证明 Go 生成物可用。相反，页面明确提供 integration code 和“复制代码”按钮（`web/src/pages/DeveloperPage.tsx:556-582`）；Preview badge 只降低成熟度，不会把复制代码变成伪代码。
- 代码证据：非流式 Go 分支在 `web/src/pages/DeveloperPage.tsx:831` 以 `resp, err := http.DefaultClient.Do(req)` 结束，没有使用两变量、处理 error、关闭 body、检查 status 或读取响应。流式分支在 `:827` 已实现 error handling、close 和 scanner，证明非流式缺尾部不是刻意的统一 snippet 风格。
- 当前 SHA 编译复现：将生成器输出逐字置于最小 `package main` 函数体，运行 `GO111MODULE=off go test -count=1`，退出码 **1**：`declared and not used: resp`、`declared and not used: err`。复现文件只在 `/private/tmp/halro-rolef-go-snippet.34ySi9`。
- 测试反证：`npx vitest run src/pages/DeveloperPage.test.tsx` 退出码 **0**，19 tests 通过；但 `DeveloperPage.test.tsx:227-245,477-514` 只覆盖 curl/Python/Java 和 tab 行为，没有 Go non-stream golden 或编译检查，因此全绿不能反驳缺陷。
- 已有防御：Workbench 自身执行路径、curl/Python/Java 以及 Go stream 分支仍可用；影响仅为复制出的 Go non-stream 样例，不影响 Gateway。
- 最终理由：所有默认非流式 Go 用户都能稳定遇到，且页面明确承诺复制代码，维持 P2；修复范围小，不需要引入新框架。

### PHIL-A06 — 安装后二进制缺少标准顶层 help

- 最终状态：**CONFIRMED**
- 最终严重度：**P3（维持）**
- 原类型：DESIGN_DEBT
- 反证尝试：目标 SHA 构建 `/private/tmp/halro-rolef-bin`，退出码 0。构建曾打印 Go module stat-cache 写权限 warning，但二进制成功产生；这是工具环境限制，不计产品失败。
- 目标 SHA 运行结果：
  - `/private/tmp/halro-rolef-bin --help` → 退出码 **1**，`unknown command "--help"`；
  - `/private/tmp/halro-rolef-bin help` → 退出码 **1**，`unknown command "help"`；
  - 无参数 → 退出码 **1**，但输出列出 18 个顶层命令的 usage；
  - `init --help` → 退出码 **0**，正常列出该子命令 flags。
- 代码证据：`cmd/halro/main.go:129-138,865-870` 没有 help / `--help` 顶层 case；每个子命令自己的 `flag.FlagSet` 仍能处理已知 command 的 `--help`。Makefile 默认 help（`Makefile:13,34-71`）只对源码 checkout 用户可用。
- 已有防御：无参数可发现所有顶层命令；Operator Guide 完整；已知子命令支持 `--help`。因此这不是功能不可用或事故阻断，只是标准发现入口和单一帮助真相缺失。
- 最终理由：候选描述与二进制行为完全一致，但影响是局部认知成本，P3 合理；不需要 Cobra 等依赖，小型 descriptor table 足够。

## 4. 原始命令与退出结果

| 验证 | 命令 | 退出码 | 结论 |
| --- | --- | ---: | --- |
| Runtime 预算 | `GOCACHE=/private/tmp/halro-rolef-go-cache GO111MODULE=off go test -count=1 runtime_scale_test.go`（`internal/app`） | 0 | gate 当前有效；源码证明实际缩小时也要求下调预算。 |
| primitive sabotage | `go test -count=1 ./internal/provider`（交换后的精确 HEAD 副本） | 0 | 交换 OpenAI / DeepSeek primitives 未被 provider package 测试发现。 |
| Go snippet 编译 | `GO111MODULE=off go test -count=1`（最小 wrapper） | 1 | 预期失败，确认 `resp` / `err` 未使用。 |
| Workbench 现有测试 | `npx vitest run src/pages/DeveloperPage.test.tsx` | 0 | 19 tests 通过，但没有 Go non-stream 断言。 |
| CLI build | `go build -trimpath -o /private/tmp/halro-rolef-bin ./cmd/halro` | 0 | 目标 SHA 二进制可运行。 |
| CLI 顶层 help | `halro --help`; `halro help`; 无参数；`halro init --help` | 1; 1; 1; 0 | 顶层标准 help 缺失；无参数列表和子命令 help 是缓解。 |

## 5. 给主评审的合并建议

1. 保留 PHIL-A01、A04、A05 为结构性 / 用户可见 P2；其中 A04 已有本轮目标 SHA sabotage，不再只是历史实验。
2. 将 PHIL-A02 改写为“缺跨能力 portfolio adoption/support-cost/sunset ledger”，删掉“Run Governance 未找到目标用户/指标/退出门”的表述，状态 PARTIAL、P3。
3. 将 PHIL-A03 改写为“Runtime lifecycle 仍集中，但 tight ratchet 与子 runtime 已形成有效防御”，状态 PARTIAL，并从 P2 降为 P3；只有出现具体错序、竞态、变更失败或预算继续上涨时再升级。
4. 保留 PHIL-A06 为 P3；无参数和 subcommand help 已让它不是阻断项。
5. 这组反证没有支持微服务、通用 DI、Cobra 或默认遥测。优先修正重复真相、补独立 executable contracts，并用最小结构解决可复现问题。
