# R7 + R8：发布、供应链与测试盲区独立评审

评审范围：`v0.7.1`（`84f2638e973f23935b9eda423143f65ff25a852c`）到
`222d08f84f61493fc9a273d351cc728528d6e30c`，日期 2026-09-11。

本角色没有发布、打 tag、推送、调用真实 Provider/KMS，或修改产品代码。结论来自源码、历史 tag
源码、GitHub 只读 API、当前锁文件和定向测试。协调者提供的旧二进制运行现象只作为待复核线索，
没有被当作本报告的确认依据。

## 结论

**R7/R8 判定：NO-GO。** 确认 1 个 P1 发布阻断、4 个 P2 发布工程/供应链问题：

| ID | 级别 | 状态 | 结论 |
| --- | --- | --- | --- |
| R78-01 | P1 | CONFIRMED | v0.8.0 写出的新 Parquet/Usage 归因格式没有建立旧版本启动拒绝；v0.7.1 可带着不兼容 derivative 启动，并可把 checkpoint open tail 中的新字段静默丢掉 |
| R78-02 | P2 | CONFIRMED | 文档声称的 release gates 超过 workflow 实际 gates，且 `main` 没有 required checks；主 Halro 镜像没有镜像级漏洞扫描/SBOM，dead-man 镜像缺许可证文件 |
| R78-03 | P2 | CONFIRMED | dry-run 证据 artifact 带 `-dry-run`，归档脚本只查无后缀名称，按文档无法归档彩排 |
| R78-04 | P2 | CONFIRMED | tag 创建成功、GitHub Release 创建失败时正式 run 无幂等恢复路径，重跑会卡在已存在 tag |
| R78-05 | P2 | CONFIRMED | 三套官方 SDK 测试依赖不在许可证漂移/漏洞门禁内；Python 只固定两个直接版本，没有锁定/哈希传递依赖 |

当前 exact SHA 的普通 `ci` run `34569951375` 全部成功，这对 R78-02 是重要缓解证据，但不能修复
R78-01，也不能让没有强制绑定的检查自动成为 release workflow 的 gate。

## R78-01 — 旧版本对新 derivative fail-open，并可静默丢失 checkpoint 归因

**严重度：P1 / CONFIRMED / release blocker**
违反：INV-11、INV-12；`review-plan.md` §8.3(4)、§9 的 rollback fail-closed 要求；
`release-assessment.md:64-69,134-135,161-164`。

### 可达链

1. v0.8.0 将 Parquet manifest 从 6 升到 8：
   `internal/usage/parquet.go:23-55`。`Exporter.Export` 会把任何可读的 3..7 manifest 原地提交为
   schema 8，即使没有 pending row：`internal/usage/parquet.go:191-229`。
2. v0.7.1 的上限是 6，因而 `Export`/`Verify` 明确拒绝 schema 8：
   `v0.7.1:internal/usage/parquet.go:37-46,176-200,257-264`。
3. 但 v0.7.1 Runtime 启动只调用 `NewExporterWithOptions`；构造器仅校验路径/输出格式，既不加载
   manifest，也不执行 `Verify`：
   `v0.7.1:internal/app/runtime.go:346-365`、
   `v0.7.1:internal/usage/parquet.go:158-173`。随后 `RunWithReady` 直接绑定 Gateway/Admin listener：
   `v0.7.1:internal/app/runtime.go:1307-1379`。schema 8 错误只会在后台
   `exportUsageParquet` 中被记录为 warning：
   `v0.7.1:internal/app/runtime.go:1233-1242`。
4. 因而 `doctor` 和 `usage verify` 的拒绝并不等于服务启动拒绝。旧进程继续接受请求时，Parquet
   watermark 停滞；窗口裁剪只能裁到旧 manifest 的 `LastSequence`，新 attempt 留在内存：
   `internal/app/usage.go:216-249`。Ledger 仍是权威且本路径不会删除 WAL，所以这里不是“已证明
   Ledger 丢账”，但运行时间越长，archive gap 和内存驻留越大。
5. 更关键的是，本轮给 `AttemptEvent` 增加 Offering/Profile/AccountRegion 和规范化失败字段，
   但 Usage checkpoint version 在 v0.7.1 和 main 都仍是 **13**：
   `internal/usage/aggregate.go:42-91`、`internal/usage/checkpoint.go:35-59`。
   v0.7.1 的 Go JSON decoder 会忽略这些未知字段，同时仍接受 version 13；
   `RestoreCheckpoint` 将解码后的旧结构视为可信：
   `v0.7.1:internal/usage/checkpoint.go:359-471`。
6. 下一次 checkpoint 若 reopen 最后一个未 sealed segment，会从已丢字段的内存结构重新 marshal
   整个 open tail：`v0.7.1:internal/usage/checkpoint.go:184-218`。再升级回候选后，候选仍接受
   version 13，不会自动从 Ledger 重建，于是该段的新增归因/失败字段在 checkpoint/UI 中静默为空。
   Ledger 原始记录仍可用于显式 rebuild，但正常启动没有触发 rebuild 的版本信号。

这不是单纯“旧 UI 不显示新字段”：旧二进制会发布正常 listener、继续写入，同时把同版本
checkpoint 当作可写格式。它满足本轮叫停规则中的“旧数据静默误读 / rollback 未 fail-closed”。

### 现有防御与缺失断言

- 正面：候选会按每个 Parquet row 的 schema 收窄比较，schema 6 旧行不会被伪造成新归因；
  `TestVerifyAcceptsRowsWrittenAtThePreviousSchema` 通过。
- 正面：`TestExporterUpgradesPreviousSchemaManifestRatherThanRejectingIt` 通过，证明正向 manifest
  升级覆盖 3..7。
- 缺失：没有 `NewRuntime` 面对未来 Parquet manifest 时必须在 bind 前拒绝的测试。
- 缺失：没有 `v0.7.1 → main → v0.7.1(写一轮 checkpoint) → main` 的双二进制回归；同版本
  checkpoint unknown-field rewrite 没有测试。

### 建议修复/回归

- 在旧二进制已理解的全局格式闸上建立 forward refusal。最直接的既有机制是对 bbolt schema 做
  一次显式迁移/版本提升，使 v0.7.1 在加载 Runtime 前拒绝候选写过的数据目录。只提升 checkpoint
  version 不足以解决：旧 Runtime 会把拒绝的 derivative 从 Ledger 重建成旧格式后继续启动。
- 在任何 listener bind 前验证所有 forward-incompatible derivative，并把 rollback 文档固定为
  “恢复升级前 backup”，不要建议直接运行旧二进制。
- 加跨版本黑盒测试：候选写 schema 8 和带新字段的 v13 checkpoint；旧 binary 必须非零退出且
  没有 listener；失败前后目录 hash 不变；恢复 backup 后才允许启动。

## R78-02 — release gate 与文档/保护规则不一致，主镜像供应链未闭合

**严重度：P2 / CONFIRMED**

### 实际控制

- `.github/workflows/release.yml` 只接受 default-branch `workflow_dispatch`，严格校验 semver、tag
  不存在和 CHANGELOG section：`release.yml:3-14,39-94`。
- 所有 `uses:` 均为 40 位 commit SHA；默认权限 `contents: read`，只有 provenance、publish、
  container push 和下游 dispatch job 获得其工作所需的写权限：
  `release.yml:15-16,357-363,448-455,509-516,542-559`。
- binaries 覆盖 Linux/macOS × amd64/arm64；container 覆盖 Linux amd64/arm64，并验证 non-root 和
  architecture：`release.yml:196-255,257-321`。
- provenance 生成 source/binary SPDX、attestation、checksum、Sigstore bundle 和签名 run manifest；
  publish 在 tag 前验证：`release.yml:357-496`。

### 缺口

1. `docs/guides/releasing.md:25-37` 把 fuzz、bundle drift 和“Trivy on the container”列为 release
   workflow 实际 gate；`docs/verification/assessments/v0.7.1.md:66-69` 也作了同样声明。
   实际 release workflow：
   - 没有 fuzz job；
   - web job 没有 typecheck，也没有 `git diff --exit-code -- internal/webui/dist`
     (`release.yml:173-194`)；
   - quality 没有 `check-dependency-license-review.sh` (`release.yml:96-116`)；
   - Trivy 只扫描 `halro-deadman:release`，没有扫描主 `halro` 镜像
     (`release.yml:268-313`)；
   - 两份 SBOM 的输入分别是源码树和解包后的 binary archives，不是主容器镜像
     (`release.yml:371-403`)。
   - 主 `Dockerfile:28-31` 把 LICENSE/NOTICE/THIRD_PARTY_NOTICES 放入最终镜像；dead-man 的最终
     stage 只复制 binary 和两个空数据目录，未复制任何许可证/notice：
     `deploy/observability/external-probe/Dockerfile:18-23`。这与
     `docs/verification/dependency-license-review.md` 的“每个 container image 携带三份文件”要求
     不一致。
   `docs/verification/release-run-evidence.md:41-45` 已承认 fuzz/bundle drift 不在 release graph，
   与前述“当前流程”文档互相矛盾。
2. GitHub 只读 API 在 2026-09-11 返回 `main` branch protection 404。active ruleset
   `20134825` 只有 deletion、non-fast-forward、Copilot review，没有 pull-request 或
   required-status-checks；名为 `main` 的 ruleset `20123440` 是 disabled。因此一次 direct
   fast-forward push 后可立即 dispatch release，workflow 不会等待该 SHA 的 `ci`。
3. exact SHA `222d08f...` 的 `ci` run `34569951375` 当前确实全绿，包含 root dependency-license
   drift、fuzz、bundle drift、root govulncheck、主容器启动/ready smoke、SDK compatibility 和
   observability。这只证明当前候选，不是持久的 release control。

**影响：** 当前没有发现主镜像已含漏洞；结论是其镜像级 OS/package vulnerability、secret、
misconfiguration 和 image SBOM 状态为 **UNVERIFIED**，且单独从 GHCR 分发的 dead-man image
不自带项目/第三方 notice。如果 release commit 在 ordinary CI 未完成时被触发，文档列出的多个
gate 可以缺席而仍发布。

**建议：** 将 required CI exact-SHA 校验纳入 release `prepare`/governance job，或启用 main
required checks；同步文档；在 release graph 明确加入 dependency drift、fuzz、bundle drift、
typecheck；分别扫描/SBOM 两个实际发布镜像，并为 workflow contract 写结构化断言。

## R78-03 — dry-run 证据无法按归档脚本归档

**严重度：P2 / CONFIRMED**

- dry run 把 evidence suffix 设为 `-dry-run`，artifact 名为
  `release-run-evidence-RUN_ID-ATTEMPT-dry-run`：`release.yml:76-86,420-446`。
- `scripts/archive-release-run.sh:20-29` 固定下载无后缀
  `release-run-evidence-${run_id}-${attempt}`。
- GitHub 只读 API 对最近 dry run `34090307139` 返回的真实 artifact 名为
  `release-run-evidence-34090307139-1-dry-run`，且未过期。
- 脚本在下载证据前已经创建输出目录；失败后重试同一路径还会被
  `scripts/archive-release-run.sh:15-20` 的“不覆盖”保护拒绝。
- `tools/modelcatalog/test_workflow_contract.py:62-67` 只检查 workflow 中存在 evidence 名称前缀；
  没有覆盖脚本与 dry-run suffix 的契约。

**影响：** 不影响构建出的 artifact，但直接阻断计划要求的彩排证据归档与离线验证。

**建议：** 脚本读取 run artifact 清单并只接受“无后缀（正式）或 `-dry-run`（彩排）”中的唯一
匹配项；在创建目标目录前完成 identity/availability 检查；为两种名称和歧义/缺失情况加测试。

## R78-04 — tag 已创建、Release 创建失败的恢复窗口不幂等

**严重度：P2 / CONFIRMED**

- publish 先执行 `git tag`/`git push`，下一步才执行 `gh release create`：
  `release.yml:484-507`。
- 若 tag push 成功而 GitHub Release 创建因瞬时 API/权限问题失败，`gh run rerun RUN_ID --failed`
  会从 publish job 开头重跑。tag step 没有“远端同名 tag 必须指向本 run SHA 则跳过”的幂等分支，
  会因 tag 已存在而失败。
- 新开一次 workflow 也会在 `prepare` 的 tag-exists check 被拒绝：`release.yml:64-68`。
- `release-assessment.md:178-187` 还错误声称“release 失败不留下 tag”并称 push tag 也是入口；实际
  workflow 从 `release.yml:3-14` 起只有 `workflow_dispatch`。相比之下，
  `docs/guides/releasing.md:288-318` 只覆盖“tag 前失败”和“tag 与 Release 都存在”，没有覆盖
  “tag 存在但 Release 不存在”。

**影响：** 不会发布错误 SHA，但会把一个已经占用的版本卡在半发布状态，必须靠未文档化的人工
操作恢复，且容易诱发删除/重建 tag 的危险处置。

**建议：** tag step 在远端 tag 已存在时，验证 annotated tag peeled commit 等于
`GITHUB_SHA` 后幂等通过，否则拒绝；Release step继续使用现有幂等判断。为 tag-only 状态加 workflow
contract/受控 bare-repository 测试，并统一三份发布文档的入口与恢复说明。

## R78-05 — SDK compatibility 依赖不在 drift/audit gate 内

**严重度：P2 / CONFIRMED（已知漏洞结果为负面证据，不是漏洞 finding）**

- 本轮 #282–#287 修改：
  `tests/compatibility/{go/go.mod,go/go.sum,node/package.json,node/package-lock.json,python/requirements.txt}`。
- `scripts/check-dependency-license-review.sh` 和文档 hash 只绑定 `go.mod`、`go.sum`、
  `web/package.json`、`web/package-lock.json`；SDK 五个输入不在 drift gate。
- CI/release 的 SDK job 安装后只运行兼容契约，没有 npm audit、nested-module govulncheck、
  pip-audit 或 license policy：`ci.yml:340-380`、`release.yml:118-159`。
- Node 有 lock integrity，Go 有 `go.sum`；Python requirements 只固定 `openai==3.8.0` 和
  `anthropic==1.4.0`，没有传递锁或 hash，因此不同时间的 release run 可以执行不同的传递依赖。

本次补充只读扫描：

- Node compatibility lock：`npm audit --ignore-scripts --audit-level=moderate`，0 vulnerabilities；
- nested Go compatibility module：`govulncheck@v1.6.0 ./...`，No vulnerabilities found；
- Python 两个直接固定版本：`pip-audit --disable-pip --no-deps`，No known vulnerabilities。
  该命令明确没有解析传递依赖，所以 Python 完整环境仍是 **UNVERIFIED**。

**建议：** 将 SDK dependency inputs 加入 drift/hash review；CI 对 Node/Go/Python 三套环境分别运行
漏洞与许可证检查；Python 生成带 hash 的完整 lock/constraints，避免测试供应链随时间漂移。

## 负面证据（未发现问题）

- `HEAD == origin/main == 222d08f84f61493fc9a273d351cc728528d6e30c`。
- exact SHA ordinary CI run `34569951375`：所有 job success。
- `sh scripts/check-dependency-license-review.sh`：通过；四个 blob hash 与文档一致。
- `git diff --check v0.7.1..HEAD`：仅报告本轮既有 PRD 的 Markdown trailing spaces；未发现产品
  源码 whitespace error。
- workflow 所有第三方 Actions 都绑定完整 commit SHA。
- `python3 -m unittest tools.modelcatalog.test_workflow_contract tools.release.test_run_evidence
  tools.release.test_verify_environment`：17 tests passed。
- `shellcheck` 对发布脚本只有 `archive-sha256.txt` 自身管道的 SC2094 info；`find` 明确排除了该
  文件名，未确认实际缺陷。
- binary archive 构建使用 `-trimpath`、commit timestamp、sorted tar、numeric owner、`gzip -n`；
  四个平台 archive 的可复现控制清晰。
- 主/sidecar container 均声明 non-root；binary archives 与主 image 携带 LICENSE/NOTICE/
  THIRD_PARTY_NOTICES（dead-man image 的缺口已列入 R78-02）。
- 当前 v0.8.0 tag 不存在。

## 候选项与未验证边界

- **已知、文档化限制：** 主 Dockerfile 的 Node/Go/distroless base 仍为浮动 tag，container archive
  也未做到 byte-reproducible；`docs/guides/releasing.md:402-430` 已明确记录。本报告不把它重复
  定为新 finding，但 v0.8.0 owner 仍须接受 dry-run 与正式 run 可能重建出不同 image bytes。
- 没有运行 v0.8.0 release dry run：当前 `CHANGELOG.md` 无 `## [0.8.0]`，web package/version 和
  README 仍为 0.7.1，assessment 尚未建立；这是 S6 尚未开始，不是 workflow failure。
- 未验证 GitHub App secret/variable、APT protected Environment、下游三仓 workflow/clean-host
  matrix、GHCR push 权限；dry run 本来也不会触发这些 publish-only credentials。
- 未下载并离线验证本轮 v0.8.0 的 archives、SBOM、Sigstore bundle、attestation 或 run-evidence；
  只有实际 exact-SHA dry run 才能产生这些证据。
- 未运行真实 KMS、真实 Provider、正式包仓库或生产 cluster；本角色无此授权。

## R7/R8 放行条件

1. 修复 R78-01，并用双二进制 populated-data 往返实验证明旧 binary 在写入前、bind 前拒绝；
2. 修复或由 owner 明确处置 R78-02～05；其中主镜像 scan/SBOM、dry-run evidence 归档必须有
   当前候选证据；
3. 完成 release commit 后，先等待其 exact-SHA ordinary CI 全绿，再执行同 SHA dry run；
4. 归档并验证该 run 的 archives、container、SBOM、checksums、Sigstore、attestations 和 manifest；
5. 再次确认 `origin/main` 未移动，且正式 run 的 `headSha` 等于已评审/彩排 SHA，才可由 owner
   作出 GO。任何后续代码变更使本签字失效。
