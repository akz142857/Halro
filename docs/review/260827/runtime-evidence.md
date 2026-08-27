# 阶段 3 · 真实二进制验证

按 `review-plan.md` §7 执行。环境为一次性目录
（scratchpad/rt，绝对路径配置，端口 18080/18081/19090），**未触碰仓库的 `data/` 与 `master.key`**。
基线二进制由 v0.3.0 worktree（`abfc05c`）构建，被测二进制为 `make build` 产出的
`v0.3.0-3-g8bb4847-dirty`（含本轮五处修复）。

R6（真账号 Provider 冒烟）按 owner 决定**不跑**，相关剧本记为未验证，见 §5。

## R1 全量门禁

`make check` 在**修复前后各跑一次，两次都 exit 0**：fmt、Go 测试、race、vet、前端 vitest、
observability 配置校验；`make frontend` 后 `git diff --exit-code -- internal/webui/dist` 无漂移。

## R2 原地升级：31 → 32（真实二进制，真实数据目录）

步骤与实测结果：

| 步骤 | 命令 | 结果 |
|---|---|---|
| v0.3.0 初始化 | `v030 init --config rt/config.yaml` | `Halro initialized` |
| v0.3.0 建链 | `v030 bootstrap …`（credential→provider→deployment→route→project→key 一次建全） | 六个 ID + 一次性 `gw_` key |
| v0.3.0 自检 | `v030 doctor` | **healthy=true**，`metadata pass: bbolt schema v31` |
| HEAD 自检（迁移前） | `bin/halro doctor` | **healthy=false**，`metadata fail: metadata schema version 31 does not match required version 32` |
| HEAD 启动 | `bin/halro start` | 正常启动并服务，退出码 0 |
| HEAD 自检（迁移后） | `bin/halro doctor` | **healthy=true**，20/23 pass |

**结论：不需要重新初始化数据目录。** 一个由 v0.3.0 自己建出来的、含完整链路的真实数据目录，
被 HEAD 打开后干净迁移到 32 并正常服务。这与 A7 在包级别用真实 31 库得到的结论一致，此处是
端到端复核。

**观测 · 升级前的 doctor 是一条硬 fail，且不告诉运维下一步。** 发布流程 §1c 让运维在升级时先跑
`doctor`，而只读的 doctor 无法迁移，于是它对一个完全健康、只是还没启动过新版的数据目录报
`metadata fail`。措辞是"版本不匹配"，读起来像数据有问题，而正确的下一步（启动一次新版即自动迁移）
一个字都没有。这与 A7-7 记录的降级方向同源，此处证实升级方向也一样。

## R3 迁移 32 的语义（字节级实测）

| 观测 | 迁移前 | 迁移后 |
|---|---|---|
| `halro.db` 中 `json_mode` 字节出现次数 | 2 | **0** |
| `structured_outputs` | 0 | 6 |
| `json_object` | 0 | 6 |

迁移确实发生且旧词表被彻底搬走。

**H1 的完整答案（本轮最重要的运行时结论）**：

- **整个启动日志 26 行里，提到 migration / schema / capability 的行数是 0。** 迁移完全静默。
- 迁移后的 `doctor` 确实多出一条 `capability_drift warn`：
  `1 deployment(s) have catalog capabilities available for review; they keep serving…`
  ——但它说的是"**有目录能力可供复核**"，不是"你的两半 JSON 能力被迁移关掉了"。方向说反了。
- 本轮的部署用的是 `gpt-4o-mini`，在内置目录内，所以这条 warn 会出现；A7 用的是**不在目录内的
  自声明模型**，那种部署连这条 warn 都没有。

合起来：**目录覆盖的部署会得到一条措辞说反了的提示，自声明的部署零信号。** 这正是前端 W11 在
控制台一侧看到的同一个问题（"目录变多了"而非"你的被关了"），两侧独立观测到同一处措辞倒置。
A7 留给本阶段的那个问题（目录 entry revision 是否必然触发 warn）由此得到答案：目录内的会，
目录外的不会。

## R4 恢复通道

| 项 | 结果 |
|---|---|
| `ledger verify` | 零帧实例上以 `ledger chain could not be authenticated` 退出——**v0.3.0 评估记录过的同一条措辞问题（把"没有"说成"损坏"）在 HEAD 仍在** |
| `backup create` | 成功。清单含 `schema_version: 32`、`minimum_ledger_reader_version: v4`、五个文件各自 sha256、master key 指纹、`pricing_state_sha256` |
| `backup verify` | 通过，清单与创建时逐字段一致 |
| `backup restore`（错误的 config） | **拒绝**：`configured Master Key fingerprint … does not match backup manifest fingerprint …; configure the Master Key generation recorded for this backup` |
| `backup restore`（正确的 config） | 成功，`schema_version_before/after = 32/32`，旧数据目录保留为 `.halro-pre-restore-*` 供回滚，恢复出 1 个启用的 Gateway Key |
| restore 后 `doctor` | healthy=true，20/23 pass（同上三条 warn/unverified） |

restore 的那次拒绝值得单列为肯定项：它发生在写入之前，错误信息**指名了不匹配的是什么、该配什么**，
是本轮见到的最可行动的一条错误消息。

## R5 基准对比

HEAD 与 v0.3.0 在**同一主机、同一 Go、背靠背**运行，`-benchtime=1s`：

| 基准 | HEAD | v0.3.0 | 判定 |
|---|---|---|---|
| 严格脱敏 | 67 676 ns/op · 13 758 B · 32 allocs | 68 159 ns/op · 13 743 B · 32 allocs | 噪声内 |
| 滚动脱敏 | 15 611 ns/op · 17 968 B · 79 allocs | 15 361 ns/op · 17 956 B · 79 allocs | 噪声内 |
| Token Guard fixed | 153.8 ns/op · 2 allocs | 154.6 ns/op · 2 allocs | 噪声内 |
| Token Guard ewma | 169.5 ns/op · 2 allocs | 166.2 ns/op · 2 allocs | 噪声内 |

**无热路径退化，分配数逐条相同。**

**补充的多 part 用例**（A5 就 H6 提出的：改动消掉两处整轮遍历、换来每 content part 的 JSON 操作，
单 part 样本两边都看不出来）。新增 `BenchmarkOutboundGenerateResultManyParts`，HEAD 实测：

| parts | ns/op | B/op | allocs/op |
|---:|---:|---:|---:|
| 1 | 2 349 | 3 377 | 18 |
| 8 | 18 967 | 26 343 | 130 |
| 64 | 148 808 | 210 385 | 1 028 |

**按 part 数线性，无超线性项**——这是这次要确认的东西。它**不是**跨版本对比：v0.3.0 没有语义遍历，
只有 wire 字节遍历，同名基准在两棵树上量的是两件不同的事，所以只作为 HEAD 侧的基线数留档。

**顺带确认 v0.3.0 评估的一条遗留**：`performance-baseline.md`（2026-07-31）对路由解析一项已知过时，
本轮未重测该项，仍应按 §9 清单重测重注日期。

## 剧本执行情况

| 编号 | 状态 |
|---|---|
| S1–S5、S8、S10 | **未验证**——都需要真实上游应答（R6 未跑）。静态与单元层面的对应结论见 A1/A3/A6 与新增回归测试，不以推断顶替运行时证据 |
| S6（脱敏改长度不产生已计费 502） | **由回归测试覆盖**：`TestUnrenderableAnswerIsNotRecordedAsSuccess` 等三条，并做过反向验证（见 `fixes.md` 修复一）。运行时未单独复现 |
| S7（data URL 脱敏改变路由需求，预留前拒绝） | 已由范围内既有测试 `service_test.go:972` 覆盖（A4-8 判为本范围质量最高的新增测试） |
| S9（重试路径 citations 不被就地改写） | 由 `internal/redaction` 既有测试覆盖；A2 复核 clone 完整 |

## 未验证清单（不得以静态推断顶替）

1. **R6 真账号 Provider 冒烟**：OpenAI Chat/Responses、Bedrock Mantle 三个表面、Anthropic、Gemini、
   Bedrock 全部未跑。owner 决定不跑（计费项）。
2. **`web_search` 的真实应答**（S4）：`web_search_call` 与 `url_citation` 的真实形状、以及承载不了
   引用的档案是否真的拒绝而非抹掉来源——未验证。
3. **探测记录被迁移清空**：本轮真实数据目录里没有探测记录（探测需真实上游调用），所以"三个探测桶
   被清空"只有 A7 的包级证据，没有真实二进制证据。A4-2 指出单元测试在这一点上是空覆盖，此处
   同样未能补上。
4. **签名远端目录**：V5 实证 `ProductionTrustRoots()` 返回 0 个根，端到端未验证。
