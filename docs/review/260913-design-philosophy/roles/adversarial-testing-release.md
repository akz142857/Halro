# 角色 F：测试证据与发布 findings 独立反证

- 反证日期：2026-09-13
- 目标源码：`v0.8.0@1d48ecde216ef40e653738f8bdc9657f88a717cc`
- 被反证对象：`PHIL-E-001` 至 `PHIL-E-004`
- 独立性：本角色不是上述 findings 的发现者；读取原角色报告后，从当前 Makefile、policy、workflow 与现行文档重新建立证据
- 边界：未修改产品代码或 workflow，未 commit/push，未调用 GitHub、Provider、KMS、package repository 或其他真实外部服务；只使用当前 checkout 与 `/private/tmp` Go cache

## 1. 裁决摘要

| Finding | 裁决 | 建议最终严重度 | 核心反证 |
| --- | --- | --- | --- |
| PHIL-E-001 | **PARTIAL** | **P3**（原 P2） | `make help` 准确列出 `check` 的组成，AGENTS 与 release assessment 从未把它单独称为完整 frontend/release gate；但它作为聚合 check 确实遗漏 production build/bundle drift |
| PHIL-E-002 | **PARTIAL** | **P3**（维持） | `make test` 的 `-shuffle=on` 本身禁用 Go result cache，不能归入“会命中 cached ok”；但 `race`、CI 与 release 的普通 `go test` 仍可缓存且与显式 `-count=1` 证据政策不一致 |
| PHIL-E-003 | **PARTIAL** | **P3**（原 P2） | workflow 自动 preflight 缺失属实；但 operator checklist 已在发布前检查变量/secret 名称，渠道被有意建模成 GitHub Release 之后的可恢复状态，网站在渠道 acceptance 前不得宣传 |
| PHIL-E-004 | **CONFIRMED** | **P3**（维持） | 标成 current 的证据文档明确说 release 不做 bundle drift，而当前 release workflow build 后立即执行该检查 |

没有 finding 被提升为 P1/P0。前三项均保留一个较窄的工程改进点，但原叙述高估了误用概率、受影响入口或用户爆炸半径。

## 2. PHIL-E-001 裁决

### 裁决：PARTIAL；建议 P3

原 finding 的技术观察成立一半：`check` 的 dependency graph 是 `fmt-check test race vet frontend-test observability-check`，其中 `frontend-test` 只执行 `npm test`；`web/package.json` 的 `test` 只是 Vitest，而 production `build` 才执行 `tsc -b`、Vite build、bundle/artifact checks。因此 `make check` 自身看不到只在 TypeScript build 或生成 bundle drift 上出现的失败（`Makefile:147-172`；`web/package.json:6-13`）。

但“仓库把 `make check` 定义成完整本地交付门禁”的前提不成立：

- `make help` 对 `check` 的说明逐项列出 fmt、Go test/race/vet、frontend tests 与 observability，没有声称包含 build/typecheck/drift（`Makefile:34-59`）。
- 当前 AGENTS 把“full frontend gate”明确定义成 typecheck、tests、production build，并要求在 push/final handoff 单独执行；web 变化还必须检查 committed bundle（`AGENTS.md:11-18,33-53,55-66`）。
- 当前 release assessment 的“full gate”写成 `make check` **加上** fresh `make frontend` 与 `git diff --exit-code -- internal/webui/dist`，没有把三者折叠成一个命令（`docs/verification/release-assessment.md:40-45`）。
- CI 和 release workflow 均执行 test、typecheck/build 与 bundle drift；production gate 不依赖 `make check` 的命名推断（`.github/workflows/ci.yml:39-58`；`.github/workflows/release.yml:194-215`）。

因此完整可达影响是：开发者忽略明确 policy/release 文档，仅凭聚合目标名自行把 `make check` 解释成完整前端交付证据，才会在本地漏报；随后 CI/release 仍会失败。它是反馈延迟与程序员界面不够统一，不是 P2 级发布控制缺失。

建议最终处置：保留 P3 DESIGN_DEBT。最小修正是增加名称明确的 `full-check` 或使发布文档引用一个统一目标；不必把日常 `check` 变成每次都重建所有制品，也无需重复单独 typecheck，因为当前 `npm run build` 已包含 `tsc -b`。

## 3. PHIL-E-002 裁决

### 裁决：PARTIAL；建议 P3

文本层面的 policy mismatch 存在：AGENTS 要求“pass 被作为证据时使用 `-count=1`”，而 Makefile 的 `test`/`race` 以及 CI/release quality 的全量 Go 命令没有显式携带它（`AGENTS.md:20-25`；`Makefile:150-164`；`.github/workflows/ci.yml:60-70`；`.github/workflows/release.yml:111-132`）。

原 finding 对 `make test` 的 cache 风险则不成立。Go 1.26.6 的 `go help test` 规定，只要出现受限 cacheable flags 以外的 test flag，就不使用成功结果缓存；`-shuffle` 不在 cacheable 列表。当前 `make test` 固定使用 `go test -shuffle=on ./...`。

本轮用空的 `/private/tmp/halro-adjudicate-cache` 连续两次运行：

```text
env GOCACHE=/private/tmp/halro-adjudicate-cache go test -shuffle=on ./internal/safelog
# 1: ok ... 1.210s
# 2: ok ... 0.349s     （未显示 cached，测试二次执行）
```

相对地，`race` 没有非 cacheable test flag，连续两次结果为：

```text
env GOCACHE=/private/tmp/halro-adjudicate-race-cache go test -race ./internal/safelog
# 1: ok ... 1.720s
# 2: ok ... (cached)
```

因此剩余可达面是：`make race`、CI、release quality 在相同 test binary/输入/可观察环境下可能复用结果；GitHub setup-go 又显式启用 build cache。内容寻址会在源码或被跟踪输入改变后 invalidation，所以这不是“错误代码拿到旧源码的绿灯”；风险集中在环境敏感、未被 Go cache key 观察的外部状态，以及证据记录与 repository policy 的字面不一致。

建议最终处置：保留 P3，但将 finding 标题/入口收窄为 `race、CI 与 release quality 未显式禁用 Go test result cache`。在证据入口统一加 `-count=1`；可保留开发者快速命令的缓存行为，只需命名和文档清楚。

## 4. PHIL-E-003 裁决

### 裁决：PARTIAL；建议 P3

自动化链的时序判断成立：`publish` 创建 tag/GitHub Release 后，`container-push` 发布 GHCR，`downstream-package-repositories` 才调用 GitHub App token action；变量或 secret 为空会在不可变 Release 已存在后失败（`.github/workflows/release.yml:537-570,572-623`）。dry run 也不会运行 publish/container/downstream，因此自身不能验证 App credential。

但已有三层防御使原 P2 影响过高：

1. operator checklist 在 dry run 和 formal release 之前有独立步骤，要求列出四仓库的 App variable/secret 名称、APT protected environment 及其 signing variable/secret，并明确提示 dry run 不验证这些凭据（`docs/guides/releasing.md:130-188`）。这不能证明 secret 内容有效，但会发现原报告所述“client ID 为空”这类缺项；该事件同时说明人工步骤可以漏做，不等于系统完全没有 preflight。
2. 架构明确把 GitHub Release 定义成下游渠道的 immutable source of truth；publisher failure 只延迟该渠道，不撤回或修改核心 Release（`docs/guides/releasing.md:70-80`）。这不是要求四渠道原子提交，而是有意的可恢复 saga。
3. website 只有在 Homebrew/APT clean-host acceptance 后才可宣传对应渠道；指南要求逐渠道确认，失败后修复 credential 并 rerun original failed job，或以 exact version/full SHA 重放下游（`docs/guides/releasing.md:247-329`）。因此正常遵循文档的用户不会把旧 Formula 当成 v0.8.0 已发布成功。

本轮没有访问 run `34598236997`、Homebrew、APT 或 website，故不重新确认原作者的 2026-09-13 外部状态。静态 finding 不依赖那次状态即可成立；具体渠道滞后仅保留为原作者 E3 快照，不能由本裁决升级。

建议最终处置：降为 P3 DESIGN_DEBT。将一个无副作用的 credential preflight 放在不可逆动作前：检查变量/secret 非空、创建短期 App token，并只读验证目标 repositories/permissions；仍保留 GitHub Release 后的 downstream dispatch、per-channel acceptance 和 exact-SHA replay。自动 preflight 不能取代 package repo 自己的签名、安装与生产 environment approval。

## 5. PHIL-E-004 裁决

### 裁决：CONFIRMED；建议 P3

这是无需外部状态即可裁决的当前树矛盾：

- `docs/verification/release-run-evidence.md:3-5` 声明自身描述当前 v0.x workflow；`:41-45` 明确称 release graph 不比较 committed web bundle drift。
- `.github/workflows/release.yml:194-215` 的 `web` job 依次执行 `npm test`、audit、`npm run typecheck`、`npm run build`，随后执行 `git diff --exit-code -- internal/webui/dist`。

防御只能降低影响，不能反驳事实：workflow 是可执行权威，所以实际发布不会漏掉 bundle drift；错误文档会低估而非高估控制。不过该文件用于 evidence/archival procedure，评审者可能据此制定重复工作或给出错误覆盖结论，因此仍是 current documentation defect。

建议最终处置：维持 P3 DEFECT（文档），修正为“release 不运行 fuzz；release web job 会在 build 后比较 committed bundle”。无需为这一句引入复杂的 workflow-doc 自动生成；若增加门禁，做一个小型 YAML step-name/command presence contract 即可。

## 6. 运行证据与限制

| 证据 | 结果 | 边界 |
| --- | --- | --- |
| 当前 Makefile、AGENTS、web scripts、CI/release YAML、release assessment 与 releasing guide 的逐行对照 | 完成 | E1，能裁决命令图与文档冲突 |
| 两次 `-shuffle=on` safelog test | 均实际执行 | E2，只证明当前 Go 工具链的 cache 行为与此 package |
| 两次 `-race` safelog test | 第二次 `(cached)` | E2，证明缺 `-count=1` 的 race 命令可命中 result cache |
| 外部 run、GitHub App、Homebrew、APT、website | 未调用 | 原角色的外部快照不被本报告重新验证 |

本次没有执行全量 gate：findings 针对命令图、cache 语义和发布时序，窄 package test 足以验证唯一需要运行的反证；全量测试无法证明或反驳这些命题。
