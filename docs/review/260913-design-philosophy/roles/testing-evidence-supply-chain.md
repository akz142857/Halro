# 角色 E：测试、证据与供应链独立评审

评审对象：`v0.8.0` / `1d48ecde216ef40e653738f8bdc9657f88a717cc`

负责维度：D7 测试、eval 与反证能力；D10 供应链与可持续演进。

本轮只评估，不修改产品代码、workflow、发布配置或外部服务。GitHub 运行状态是 2026-09-13
读取的外部快照；本地运行证据另见 `../runtime-evidence.md`。

## 1. 结论

Halro 的测试与发布工程明显高于一般早期项目：测试不只覆盖 happy path，还包括 fuzz、故障注入、
SDK 黑盒兼容、1000 路 SSE 压力、race、漏洞扫描、SBOM、签名和 provenance。CI actions 也固定到
完整提交 SHA。其主要问题不是“没有测试”，而是**证据语义和发布闭环没有完全对齐**：本地
`make check` 不包含仓库自己定义的完整前端门禁；正式发布在不可变 Release 与容器发布后才发现
下游凭据缺失；现行文档还错误描述 release workflow 没有 bundle drift 检查。

建议成熟度：

| 维度 | 分数 | 最高证据 | 理由 |
| --- | ---: | --- | --- |
| D7 测试、eval 与反证能力 | 3/4 | E3 | 自动化广、目标 SHA CI 通过且有本地复跑；真实 Provider/KMS、长稳、mutation/sabotage 持续门禁尚未闭环 |
| D10 供应链与可持续演进 | 3/4 | E3 | 锁文件、许可证、SBOM、签名、attestation、制品验证完整；正式发布下游同步未闭环，且预检顺序允许部分发布状态 |

## 2. 证据地图

### 2.1 测试与反证资产

- 392 个 Go 测试文件、44 个 Vitest 文件；目标树中未发现生产代码的 `TODO/FIXME/HACK/XXX`。
- 8 个显式 Go fuzz target，覆盖安全日志、脱敏、SSE、Token Guard checkpoint 和 OpenAI 请求解码；
  CI 校验 fuzz 名称存在，并分别运行 40 秒。
- benchmark 覆盖 budget、ledger、limiter、Token Guard、bbolt、governance、redaction 和 Provider 路径。
- opt-in 门禁明确隔离真实 Provider、真实 KMS、10 GiB WAL RTO、治理容量、Argon2 内存和 1000 路
  SSE 压力，避免把昂贵或环境依赖测试伪装成普通单测。
- 普通 CI 对精确 SHA 成功；发布 dry-run 对精确 SHA 成功。首次本地全量 Go 因沙箱禁止
  `httptest` 绑定 `::1` 而失败，已在允许回环监听的环境用 `-count=1` 复跑。

### 2.2 供应链资产

- `go.mod` 有 12 个直接依赖模块；前端有 10 个 runtime dependency，均由锁文件固定解析。
- CI 和 release 的 GitHub Actions 使用完整 commit SHA，而非浮动 tag。
- 发布链生成 source/dependency SBOM、released-binary SBOM、image SBOM、SHA-256、Sigstore bundle、
  GitHub build provenance，并在发布前验证 checksum、attestation 和签名。
- `v0.8.0` GitHub Release 已发布，二进制、deb、SBOM、签名材料可见；多架构容器发布 job 成功。
- 同一次正式发布的 downstream job 因 `HALRO_RELEASE_APP_CLIENT_ID` 为空失败；公开 Homebrew Formula
  在快照时仍指向 `v0.7.0`，APT 仓库为私有且当前凭据不能读取，故 APT 状态为 `UNVERIFIED`。

## 3. Findings

### PHIL-E-001 — `make check` 不是仓库所定义的完整本地门禁

- 类型：DESIGN_DEBT
- 维度与原则：D7、D9；诚实的程序员界面、唯一可执行真相
- 严重度：P2
- 置信度：HIGH
- 状态：CANDIDATE，等待非作者反证
- 入口与可达条件：开发者或自动化按习惯运行 `make check`，并把成功解释为完整交付门禁。
- 代码与文档证据：`Makefile:147-172`；`AGENTS.md:11-25,33-41`；
  `.github/workflows/ci.yml` 的 web typecheck/build/bundle drift 步骤。
- 最小复现与原始证据：读取 Make dependency graph；`check` 只包含 `frontend-test`，没有
  `npm run typecheck`、`npm run build` 或 `git diff --exit-code -- internal/webui/dist`。
- 违反的不变量或用户承诺：一次被称为完整门禁的入口，应覆盖其声明的可发布表面。
- 用户影响、爆炸半径和可恢复性：本地成功可能遗漏类型错误或生成 bundle 漂移；CI 仍会捕获，
  因此不是当前制品错误，但会延迟反馈并浪费评审/CI 时间。
- 已有防御与反证：CI 和 release workflow 确实执行这些步骤；AGENTS 明确要求 push 前检查，
  所以风险被远端门禁部分缓解。
- 建议处置：SIMPLIFY——让一个命名明确的目标复用完整 web gate；保留快速目标供迭代使用。
- 建议回归或验收：人为制造仅 typecheck 可见的错误和 bundle drift，完整目标必须失败，快速目标可不承担。
- 成本、owner、期限：S，Build/Release owner，30 天。

### PHIL-E-002 — 证据命令与 `-count=1` 政策不一致

- 类型：DESIGN_DEBT
- 维度与原则：D7；可重复、可解释证据
- 严重度：P3
- 置信度：HIGH
- 状态：CANDIDATE，等待非作者反证
- 入口与可达条件：使用 `make test`、`make check`、普通 CI 或 release quality 的 Go pass 作为当前运行证据。
- 代码与文档证据：`AGENTS.md:20-25` 要求证据运行使用 `-count=1`；`Makefile:154-164` 及
  CI/release 的全量 `go test ./...` 未带该参数。
- 最小复现与原始证据：静态检查命令行即可；本评估自己的全量复跑显式使用 `-count=1`。
- 用户影响、爆炸半径和可恢复性：Go cache 是内容寻址且 clean runner 降低误判概率，但命令本身
  不能保证 fresh execution，证据说明与执行入口不一致。
- 已有防御与反证：GitHub hosted runner 与新 checkout 通常无可复用 test result；`-shuffle` 还能发现顺序依赖。
- 建议处置：SIMPLIFY——证据目标显式使用 `-count=1`；如保留缓存型快速目标，应在名称和文档中区分。
- 建议回归或验收：目标输出显示 fresh run，连续执行不能出现 cached `ok`。
- 成本、owner、期限：XS，Build owner，30 天。

### PHIL-E-003 — 下游发布凭据在不可变制品发布之后才被验证

- 类型：DESIGN_DEBT / EVIDENCE_GAP
- 维度与原则：D10、D4；预检先于不可逆动作、分层报告发布状态
- 严重度：P2
- 置信度：HIGH
- 状态：CANDIDATE，等待非作者反证
- 入口与可达条件：执行 `release.yml` 且 `dry_run=false`，下游 GitHub App variable/secret 缺失或无效。
- 代码与文档证据：`.github/workflows/release.yml:605-623` 显示 downstream job 依赖
  `publish` 和 `container-push`；`docs/guides/releasing.md:86-111` 说明所需凭据与下游闭环。
- 最小复现与原始证据：正式 run `34598236997` 中 publish、container-push 成功，
  `Create a repository-scoped release automation token` 因空 `client-id` 失败；公开 Homebrew Formula
  仍是 0.7.0。
- 违反的不变量或用户承诺：一次发布操作应在不可逆发布前验证完成承诺所需的关键控制面能力，
  或明确把渠道发布建模为独立、可恢复状态机。
- 用户影响、爆炸半径和可恢复性：GitHub Release/GHCR 可用，但 Homebrew/APT 与网站可能滞后；
  不破坏核心制品，修复凭据后可重放 downstream job，爆炸半径限于渠道一致性和用户认知。
- 已有防御与反证：发布指南明确称下游失败只延迟渠道、不撤回 Release；workflow 把 downstream
  独立成最后 job，避免污染不可变制品。该设计使故障可恢复，但没有消除可预检的部分发布。
- 建议处置：PROVE / SIMPLIFY——在 publish 前做只读凭据/安装/仓库权限 preflight；保留发布后的
  精确 SHA dispatch 与可重放 downstream 状态，并在 release summary 显示各渠道状态。
- 建议回归或验收：缺 variable、缺 secret、App 未安装、权限不足四种场景必须在 publish 前失败；
  downstream transient failure 可从同一 release version/commit 安全重放。
- 成本、owner、期限：S，Release owner，30 天。

### PHIL-E-004 — 现行 release 证据文档与 workflow 冲突

- 类型：DEFECT（文档）
- 维度与原则：D9、D10；文档必须描述当前可执行事实
- 严重度：P3
- 置信度：HIGH
- 状态：CANDIDATE，等待非作者反证
- 入口与可达条件：评审者或发布负责人以 `release-run-evidence.md` 判断 release graph 的覆盖范围。
- 代码与文档证据：`docs/verification/release-run-evidence.md:41-45` 声称 release 不比较 web bundle
  drift；`.github/workflows/release.yml:194-214` 明确 build 后执行该比较。
- 最小复现与原始证据：逐行比较即可。
- 违反的不变量或用户承诺：标记为 current 的操作文档不能与当前 workflow 相反。
- 用户影响、爆炸半径和可恢复性：不会降低实际门禁，反而会低估已有控制；但会污染审计、计划和
  release readiness 判断。
- 已有防御与反证：workflow 是最终可执行事实，正常发布不会因该句而漏跑检查。
- 建议处置：SIMPLIFY——修正文档为“release 不跑 fuzz，但会检查 bundle drift”，并增加轻量文档漂移检查。
- 建议回归或验收：文档列出的 workflow steps 与 YAML 解析结果一致。
- 成本、owner、期限：XS，Docs/Release owner，30 天。

## 4. 未被证明的主张

- 没有执行真实 Provider、真实 KMS、真实外部告警投递；这些保持 E4 缺口，不据此判产品缺陷。
- 没有执行 24 小时 soak、多架构实机安装、clean-host Homebrew/APT 验收。
- 仓库有 fuzz 和 fault tests，但未发现持续 mutation/sabotage 门禁；“关键控制被删除时测试一定变红”
  仍为 `UNVERIFIED`。
- APT 仓库状态因权限不可见而未验证；不能从 Homebrew 滞后推断 APT 必然滞后。

## 5. 保留的设计

- KEEP：将真实外部服务测试保持 opt-in，避免本地和 CI 默默产生费用或泄露凭据。
- KEEP：生成 bundle 与源代码同提交，并用 drift check 验证，而不是手工合并。
- KEEP：签名、SBOM、attestation 与 checksum 在 publish 前互相绑定并验证。
- KEEP：将核心制品、容器、Homebrew、APT、网站分别报告，禁止用一个绿色/红色吞掉部分状态。
