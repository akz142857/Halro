# V5 · 对抗验证 · B3 的 rc.1 根因与 G4 判定

> 角色：对抗验证（role-prompts.md §8）| 对象：`findings/B3.md` 的 B3-01 根因主张与 §6 的 G4 判定
> 立场：默认待证伪的主张是错的，逐条尝试推翻。
> 边界：**只读**。未打标签、未 push、未触发或重跑任何 workflow、未创建 Release、未修改仓库设置或 secrets/variables、未修改仓库任何已跟踪文件。除本报告外未写入仓库。

---

## 0. 裁决摘要

| 待证伪的主张 | 裁决 |
|---|---|
| (a) job 确实被触发了，"未触发"这个候选被证伪 | **CONFIRMED** |
| (b) 没有 `waiting` 状态 ⇒ 环境没配审批人 | **REFUTED（论证无效）**，但结论被同一条记录里的另一项证据支撑 |
| (c) 7 秒 = 失败在 `release.yml:280` 的 `test -n` 第一行 | **REFUTED** |
| (d) "那道审批闸门从未存在" | **PARTIAL**：今天成立且可验证；rc.1 当时强指示但不可直接验证 |
| (e) G4 不通过 | **CONFIRMED**，但 B3 给出的两条支撑理由中有一条是范畴错误 |
| **G4 该判"不通过"还是"无法验证"** | **不通过**（缺陷，不是证据不足）——理由见 §5 |

整体裁决：**PARTIAL**。B3-01 的**结论方向**站得住，**它给出的取证论证不站得住**；B3 §6 的 G4 判定**站得住**，但它把"不可复现构建"算进 G4 属于范畴错误，剥掉之后判定不变。

严重度评估：B3-01 作为"根因已查明"被高估了约**一档**——真实状态是"两个前置条件缺失可验证，具体失败步骤不可验证"，不是"根因已确定为缺 secret"。B3 §6 的 G4 结论**没有**被高估。

---

## 1. 先打掉几条"可能的证伪方向"——它们都不成立

任务提示给了几条推翻方向。我逐条试过，**它们救不了 B3 的对立面**，先记录下来避免重复劳动。

### 1.1 `total_count: 0` 是不是 token 权限不足？——不是

```
$ gh auth status
  ✓ Logged in to github.com account akz142857 (keyring)
  - Token scopes: 'gist', 'read:org', 'repo'

$ gh api repos/akz142857/Halro --jq '{owner:.owner.login, type:.owner.type, private:.private, permissions:.permissions}'
{"owner":"akz142857","permissions":{"admin":true,"maintain":true,"pull":true,"push":true,"triage":true},"private":false,"type":"User"}
```

`admin: true`，且两个**只有 admin 才能读**的端点都正常返回：

```
$ gh api repos/akz142857/Halro/actions/permissions
{"enabled":true,"allowed_actions":"all","sha_pinning_required":false}

$ gh api repos/akz142857/Halro/actions/secrets/public-key
{"key_id":"3380204578043523366","key":"QQA0HuA+LMasiJxj0HDrJjVpKSiCuFQGusbFZOGmWg4="}
```

而且做了阴性对照：权限不足时 GitHub 返回 403，不是空列表；不存在的资源返回 404：

```
$ gh api repos/akz142857/Halro/environments/v1-release
{"message":"Not Found", "status":"404"}
```

复核三条计数，与 B3 一致：

```
$ gh api repos/akz142857/Halro/environments   → {"total_count":0,"environments":[]}
$ gh api repos/akz142857/Halro/actions/secrets   → {"total_count":0,"secrets":[]}
$ gh api repos/akz142857/Halro/actions/variables → {"variables":[],"total_count":0}
```

**这条证伪方向失败。** B3 关于"今天"的三条观测是真的。

### 1.2 cosign 的 keyless identity 是不是其实有文档？——没有

全仓搜索，`--certificate-identity` 在评审文档之外**只出现一处**：

```
$ grep -rn "certificate-identity\|certificate-oidc-issuer" --include="*.md" --include="*.sh" --include="*.py" --include="*.json" .
（除 findings/B3.md 自身外，零命中）

$ grep -n "certificate-identity\|certificate-oidc-issuer" .github/workflows/release.yml
297:              --certificate-identity "https://github.com/${GITHUB_REPOSITORY}/.github/workflows/release.yml@${GITHUB_REF}" \
298:              --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
```

（注：B3 写作 `release.yml:299-306`，实际是 `:297-298`；小幅行号漂移，不影响结论。）

三处面向使用者的文档我逐条读了原文，确实只说"要验"不给命令：
- `docs/milestones/release-notes-v1.0.0.md:129` — "Verify `checksums.txt`, the SPDX SBOM, and Sigstore bundles."
- `docs/guides/operator-guide.md:427` — "Read release notes and verify the binary checksum and Sigstore bundle."
- `docs/guides/releasing.md:18-21, :44` — 面向发布者（"Configure the GitHub `v1-release` environment with required reviewers…"、":44 verify every downloaded blob … against `checksums.txt` and its Sigstore bundle"），不给外部验签命令。

并且我**独立复跑**了 cosign 的参数语义（用我自己造的占位 blob 与占位 bundle，未向公共 Fulcio/Rekor 写入任何记录）：

```
$ <scratchpad>/bin/cosign verify-blob --bundle v5/blob.txt.sigstore.json v5/blob.txt
Error: --certificate-identity or --certificate-identity-regexp is required for verification in keyless mode

$ <scratchpad>/bin/cosign verify-blob \
    --certificate-identity "https://github.com/akz142857/Halro/.github/workflows/release.yml@refs/tags/v1.0.0-rc.1" \
    --certificate-oidc-issuer "https://token.actions.githubusercontent.com" \
    --bundle v5/blob.txt.sigstore.json v5/blob.txt
Error: bundle does not contain cert for verification, please provide public key
```

第一条在参数校验阶段就拒绝；第二条越过参数校验、停在"bundle 是假的"上。**B3-04 复现成功，这条证伪方向失败。**

### 1.3 构建摘要漂移是不是不影响 M11 门禁？——机制上确实影响，但它不属于 G4

机制事实全部复核为真：
- `release.yml:153` ldflags 里含 `buildinfo.Date=$(date -u +%Y-%m-%dT%H:%M:%SZ)`；
- `release.yml:165` 是 `tar -C release -czf …`，无 `gzip -n` / `--sort` / `--mtime`；
- 同一文件 `release.yml:200` 与 `:206` 的 `docker save … | gzip -n` **用了** `-n`，说明是遗漏不是取舍；
- `tools/m11/release-evidence/verify.py:316-317` 确实重算并比对摘要：
  ```python
  digest = hashlib.sha256(path.read_bytes()).hexdigest()
  require(digest == artifact.get("sha256"), f"release artifact digest does not match bundle: {name}")
  ```

所以 B3-03(a)(b)(c) 的**事实层**是对的。但见 §4.3：把它算进 G4 是范畴错误。

---

## 2. 逐条走 B3-01 的取证链

### 2.1 (a)「job 确实被触发了」——CONFIRMED

deployment 记录我自己拉了一遍，与 B3 一致：

```
$ gh api repos/akz142857/Halro/deployments
5785970777 v1-release v1.0.0-rc.1 ad815d62 created=2026-08-06T23:35:08Z updated=2026-08-07T04:02:18Z
  performed_via_github_app=github-actions  task=deploy  creator=akz142857
  transient_environment=false  production_environment=false

$ gh api "repos/akz142857/Halro/deployments/5785970777/statuses?per_page=100"
16459271385 in_progress 2026-08-06T23:35:12Z  log_url=…/actions/runs/31131173718/job/92721604435
16459274423 failure     2026-08-06T23:35:19Z  log_url=…/actions/runs/31131173718/job/92721604435
16477297336 inactive    2026-08-07T04:02:18Z  log_url=…/actions/runs/31131173718/job/92721604435
```

（`log_url` 里的仓库名是 `Heimdall`——改名前的旧 URL，与 rc.1 的时间点自洽，是一条附加的真实性佐证。）

`publish` 声明 `needs: provenance`（`release.yml:261`）。一个 deployment 只有在 publish job 真正进入 `v1-release` 环境时才会由 github-actions 创建，所以 publish 被调度过，因而 `provenance` 及其上游 `binaries`/`container`/`quality` 全部成功过。**"未触发"确实被证伪。这一条我推不翻，CONFIRMED。**

顺带做了一次时间线合理性核对，用的是**幸存的**同 sha CI run（release run 已删，CI run 还在）：

```
$ gh api repos/akz142857/Halro/actions/runs/31131173946/jobs
JOB go        started 23:28:42Z completed 23:32:07Z   （test + -race + vet）
JOB container started 23:28:41Z completed 23:29:34Z
JOB web       started 23:28:42Z completed 23:29:28Z
```

CI 在 23:28:4x 起跑，release 的 publish 在 23:35:08 到达环境——中间 6.5 分钟够 quality → binaries(×4) + container → provenance 串完。**时间线自洽，没有反例。**

### 2.2 (b)「没有 `waiting` 状态 ⇒ 没配审批人」——论证 REFUTED

这是本次证伪的主要收获。

**`waiting` 根本不是 deployment status 的合法取值。** GitHub REST 文档给出的 `state` 枚举是：

> `error`, `failure`, `inactive`, `pending`, `success`, `queued`, `in_progress`

（来源：<https://docs.github.com/en/rest/deployments/statuses>，以及 <https://docs.github.com/en/actions/how-tos/deploy/configure-and-manage-deployments/control-deployments>。）

`waiting` 是 **workflow run / job 层**的状态，不是 deployment status 层的状态。所以"中间没有 `waiting` 状态"这件事，在这个 API 里**无论环境配没配审批人都必然成立**——它是 API 形状决定的，不是配置决定的。**从一个不可能出现的状态的缺席推出配置结论，这条论证无效。**

B3 括号里补的 "`waiting`/`queued`" 里，`queued` 确实是合法值；但 Actions 在环境待审批期间是否落一条 `queued` deployment status，文档没有承诺，不能反过来当判据。

**但 B3 的结论并没有随论证一起倒掉**，因为同一条记录里有一项 B3 没有使用的、更强的证据：

```
deployment created_at   2026-08-06T23:35:08Z
first status in_progress 2026-08-06T23:35:12Z
                         ── 间隔 4 秒 ──
```

按 GitHub 文档，引用了环境的 job 在等待 required reviewers 期间，deployment 对象**已经存在**（"Get pending deployments for a workflow run" 返回 pending deployment，`POST …/pending_deployments` 审批后返回 Deployment 对象；见 <https://docs.github.com/en/rest/actions/workflow-runs>）。因此若配了 required reviewers，`created_at` 与 `in_progress` 之间会插入**人工审批延迟**。

而 `docs/runbooks/m11-production-operations.md:67` 描述的那道闸门是：四方 reviewer 下载 `release-assets` → 审 SBOM/checksum/Sigstore/bundle → 设置 secret → 批准环境。**这套动作不可能在 4 秒内完成。**

所以：**结论（这次 publish 没有被人工审批闸门拦住）成立，但成立在 B3 没写的那条论证上。B3 写出来的那条论证是无效的，必须替换。**

### 2.3 (c)「7 秒 = 失败在 `test -n` 第一行」——REFUTED

这一条我认为是 B3 里最实的错误，有三层。

**第一层：步骤顺序读错了。** `test -n` 不是 publish job 跑的第一件事，它是**第四个 step** 里的第一行。HEAD 上的 publish job：

```
release.yml:269   - uses: actions/checkout@…            # v7.0.1        ← step 1
release.yml:270-273 - uses: actions/download-artifact@… # v8.0.1, name: release-assets, path: release  ← step 2
release.yml:274   - uses: sigstore/cosign-installer@…   # v4.1.2        ← step 3
release.yml:275-278 - name: Verify final M11 evidence and release blobs ← step 4
release.yml:279       set -euo pipefail
release.yml:280       test -n "${M11_RELEASE_EVIDENCE_JSON}"
```

rc.1 当时的版本结构**完全相同**（我读的是 tag 指向的那个 commit，不是 HEAD）：

```
$ git show ad815d6:.github/workflows/release.yml | sed -n '255,267p'
255:    steps:
256:      - uses: actions/checkout@v7
257:      - uses: actions/download-artifact@v8
258:        with:
259:          name: release-assets
260:          path: release
261:      - uses: sigstore/cosign-installer@v4.1.2
262:      - name: Verify final M11 evidence and release blobs
…
267:          test -n "${M11_RELEASE_EVIDENCE_JSON}"
```

（顺带：B3 引的 `release.yml:277` 是 `env:` 下的 `M11_RELEASE_EVIDENCE_JSON: ${{ secrets… }}` 那一行，不是 `test -n`。`test -n` 在 `:280`。）

**第二层：B3 的前提自我否定。** B3 原文写的是：

> 「一个只做 checkout + download-artifact + cosign-installer 再失败的 job 不会是 7 秒——7 秒是环境准入后立刻断言失败的量级。」

这句话断言那三个 step **跑不完 7 秒**。但那三个 step 排在断言**前面**。若它们跑不完 7 秒，job 就**更不可能**在 7 秒内到达断言。**这条前提推翻的正是它被用来支撑的结论。**

**第三层：定量上 7 秒确实偏紧。** 用幸存的同 sha CI run 标定这台仓库的 runner：

| 步骤 | 实测（run 31131173946） |
|---|---|
| `Set up job` | 1~2 s |
| `actions/checkout@v7` | 0~2 s |

即断言之前已消耗 1~4 s，留给 `download-artifact(release-assets)` + `cosign-installer` 的只有 3~6 s。而 `release-assets` 的体量我实测了一把（scratchpad，未写入仓库）：

```
$ GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o <sp>/sz/halro ./cmd/halro
$ GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o <sp>/sz/halro-deadman ./cmd/halro-deadman
$ ls -l <sp>/sz/
-rwxr-xr-x  29282466  halro
-rwxr-xr-x   7090338  halro-deadman
$ tar -czf sz.tar.gz sz ; ls -l sz.tar.gz
-rw-r--r--  13175670   sz.tar.gz          ← 单平台档 ≈ 13.2 MB
```

`release-assets` = 4 个平台档（≈53 MB）+ `halro-container.tar.gz` + `halro-deadman-container.tar.gz` + 源树 SPDX SBOM + `checksums.txt` + 8 份 sigstore bundle，量级 **80~100 MB**。要在 3~6 秒里下完并解包，**再**装一个 cosign 二进制，处在"极限边缘"而非"常态"。

**结论：7 秒这个数字是反对"失败在 `test -n`"的证据，不是支持它的证据。**

那到底失败在哪一步？**不可验证。** run `31131173718` 已被删除：

```
$ gh api repos/akz142857/Halro/actions/runs/31131173718        → 404 Not Found
$ gh api repos/akz142857/Halro/actions/workflows/324475184/runs --jq '.total_count'  → 0
```

日志、artifact、check run 全部随 run 消失。候选至少有四个：`Set up job`、`checkout`、`download-artifact`、`cosign-installer`，以及 B3 主张的缺 secret 断言。**我不给它一个我证不出的答案。**

### 2.4 (d)「那道审批闸门从未存在」——PARTIAL

拆成两个时点：

| 时点 | 判定 | 依据 |
|---|---|---|
| **今天** | **成立，可直接验证** | `gh api …/environments` → `total_count: 0`；`…/environments/v1-release` → 404。环境不存在，自然没有 required reviewers |
| **rc.1 当时（2026-08-06）** | **强指示，但不可直接验证** | 环境已被删除，其当时的 `protection_rules` 无法读取；run 已删，`pending_deployments`、run 层 `waiting` 状态、审批事件全部不可得。唯一的间接证据是 §2.2 的 4 秒间隔 |

所以 B3 那句"那道审批闸门**从未**存在"，措辞比证据强。可以说的是：**今天不存在（已验证）；rc.1 当时极可能不存在（4 秒间隔不容一次人工审批），但直接证据已被删除。**

补充一条 B3 说对、我复核为真的机制：GitHub 在 job 首次引用一个不存在的 environment 时会自动创建它且不带保护规则；repo 级 secret 对带 `environment:` 的 job 同样可见。因此"设一个 repo 级 `M11_RELEASE_EVIDENCE_JSON` 就能在零人工审批下直接 `gh release create`"这条路径（B3-05）成立。这一条我推不翻。

---

## 3. 复核 G4 依赖的几条硬事实

全部自己重跑，不采信 B3 的转述：

```
$ gh release list --limit 30
（无输出，退出码 0 —— 空）

$ gh api repos/akz142857/Halro/actions/workflows --jq '.workflows[] | "\(.id) \(.name) \(.path) \(.state)"'
324475179 ci                          .github/workflows/ci.yml                    active
330820641 publish signed model catalog .github/workflows/model-catalog-publish.yml active
324475184 release                     .github/workflows/release.yml               active
324475262 Dependabot Updates          dynamic/dependabot/dependabot-updates        active

$ gh api "repos/akz142857/Halro/actions/workflows/324475184/runs?per_page=5" --jq '.total_count'
0

$ git tag -l
v1.0.0-rc.1

$ git ls-remote --tags origin
fa1d971d7bce6b77b8d486d1c65b79cb02ad91c0  refs/tags/v1.0.0-rc.1
ad815d628057ddd3a87d4c3e24715d0fd34a3a82  refs/tags/v1.0.0-rc.1^{}
```

四条都成立：**零 Release、release workflow 零条幸存 run、只有 rc.1 一个 tag、rc.2 从未创建。**

---

## 4. G4 判定的独立复核

### 4.1 G4 的准确定义

`docs/review/260811/releases.1.0.0/review-plan.md:189`：

> | G4 | 发布流水线端到端产出一次可被**外部使用者**独立验签的 artifact（以 rc.2 验证），且 rc.1 publish 未运行的根因已关闭 | B3 |

两个合取项。逐条判。

### 4.2 合取项 A：产出过一次可被外部独立验签的 artifact？——**否，且是被观测到的否**

`gh release list` 为空、release workflow `total_count: 0`、rc.1 的产物随 run 删除。**从来没有任何 artifact 到达过任何外部使用者。** 这是一个可复现的观测结果，不是取证不到。

叠加 §1.2：即便有产物，三处面向使用者的文档也不给 `--certificate-identity`，而 cosign keyless 模式**强制**要求它（我独立复现）。所以 A 的两半——"有产物"和"外部能验"——**都不成立**。

### 4.3 合取项 B：rc.1 根因已关闭？——**否，且这一条不依赖 §2.3 的争议**

这里是关键：**无论 rc.1 那 7 秒具体断在哪一步，合取项 B 都为假**，因为 publish job 需要的两个 GitHub 侧前置条件今天都**可验证地不存在**：

```
$ gh api repos/akz142857/Halro/environments     → total_count 0   （无 v1-release）
$ gh api repos/akz142857/Halro/actions/secrets  → total_count 0   （无 M11_RELEASE_EVIDENCE_JSON）
```

而 `release.yml:279-280` 是 `set -euo pipefail` 后紧跟 `test -n "${M11_RELEASE_EVIDENCE_JSON}"`：secret 未设置 ⇒ 展开为空串 ⇒ `test -n` 退出非零 ⇒ step 失败 ⇒ 不产出 Release。**今天打一个新 tag，publish 必然仍然失败**，这是从文件与 API 两侧都能验证的推断，不是揣测。

"根因已关闭"要求的是关闭，现状是**连根因的具体断点都还没查明**（run 已删），更谈不上关闭。

### 4.4 剥掉 B3 的一条范畴错误

B3 §6 把"可复现构建不成立（B3-03）"列为 G4 不通过的一行依据。**这是范畴错误。** G4 的措辞是"可被外部使用者独立**验签**"——外部使用者的验签路径是 `cosign verify-blob` + `sha256sum --check checksums.txt`，它**不要求**第三方能逐字节重建产物。可复现构建是另一个属性（独立重建），属于供应链纵深，不属于 G4。

**剥掉这一行之后，G4 的判定不变**——因为 §4.2 与 §4.3 各自独立地足以让它不通过。B3-03 作为 P0/P1 级供应链发现是否成立，我不在本次范围内裁决；我只指出它不该出现在 G4 的判定表里。

---

## 5. 对「G4 该判不通过、还是判无法验证」的单独表态

**判：不通过（缺陷）。不是无法验证。**

理由，按任务要求把两者的区别摆清楚：

1. **G4 是一个"正向成就"判据。** 它问的是"流水线**是否已经**产出过一次可外部验签的 artifact"。答案是可观测的**否**——零 Release、零幸存 run、零 artifact。这是一个**被观测到的否定事实**，不是一个证据空洞。

2. **"无法验证"的形状不是这样。** 无法验证应当是："产物存在，但我们没能检验它是否可验签"。这里根本没有产物可检验。把"东西不存在"记成"没查清"，等于把系统的缺陷记到评审的头上。

3. **合取项 B 独立成立，且不受本次证伪影响。** 我推翻了 B3 对 7 秒的归因，但那只影响"rc.1 具体断在哪一步"这个**取证问题**；G4 问的是"根因已关闭"，而两个已知前置（environment、secret）今天都可验证地缺失，publish 今天仍会失败。**所以即使 B3 的根因论证全部作废，G4 依然不通过。**

4. **需要如实交代的一点公平性**：review-plan.md:189 与 role-prompts.md §5 B3 第 1 条都把"打 rc.2"指定为 G4 的验证载体，而 B3 在只读边界下没有打。所以 G4 **从未被真正执行过一次**。但"未执行"救不了它：合取项 A 问的是"是否已经发生"（否），合取项 B 独立为假。若要重新评估 G4，正确动作是**先关掉两个前置条件再打 rc.2**，而不是把判定改标签。

5. **确实"无法验证"的是一个更窄的问题**——rc.1 publish job 究竟断在哪一个 step。这一条我明确记为**无法验证（run 已删除，日志/artifact/check run 全部不可得）**。G4 不依赖它。

**对评分的影响**：B3 的 4.0 分锚在"存在 CONFIRMED 的 P0"，援引 B3-01 与 B3-03。按本次裁决：
- B3-01 的**具体根因归因**不是 CONFIRMED，应降为"两个前置条件缺失（CONFIRMED）+ 具体失败步骤不可验证"；
- B3-03 不应计入 G4；
- 但 B3-01 改写后的**可操作内容**（今天没有环境、没有 secret、没有审批闸门、打 tag 必然再次失败）仍然是 CONFIRMED 的阻塞项。

所以 **P0 存续，分档不必大改，但 B3 §4 的 B3-01 证据段与 §6 的 G4 依据表必须重写**：删掉"无 `waiting` 状态"与"7 秒 = 第一行断言"两条论证，替换为"created_at → in_progress 仅 4 秒"与"两个前置条件今天可验证地缺失"，并把 B3-03 从 G4 判定表移出。

---

## 6. 附录

### 6.1 读过的文件

- `docs/review/260811/releases.1.0.0/role-prompts.md`（§1、§8，全文）
- `docs/review/260811/releases.1.0.0/findings/B3.md`（全文 803 行）
- `docs/review/260811/releases.1.0.0/review-plan.md`（G4 定义 `:189`，及 `:9 :15 :62 :102`）
- `docs/review/260811/releases.1.0.0/scoring-rubric.md`（§1~§5，封顶规则）
- `.github/workflows/release.yml`（HEAD，314 行；重点 `:120-314`）
- `.github/workflows/release.yml` @ `ad815d6`（rc.1 指向的 commit，`:250-301`）
- `tools/m11/release-evidence/verify.py`（`:275-330`，重点 `verify_artifact_files` `:309-318`）
- `docs/milestones/release-notes-v1.0.0.md`（`:125-140`）
- `docs/guides/operator-guide.md`（`:424-430`）
- `docs/guides/releasing.md`（`:18-50`）
- `docs/runbooks/m11-production-operations.md`（`:67`）

外部文档（只读查阅，未做任何写操作）：
- <https://docs.github.com/en/rest/deployments/statuses> — deployment status `state` 枚举
- <https://docs.github.com/en/rest/actions/workflow-runs> — pending deployments / review pending deployments
- <https://docs.github.com/en/actions/how-tos/deploy/configure-and-manage-deployments/control-deployments> — 环境审批与 deployment 对象生命周期
- <https://docs.github.com/en/actions/reference/workflows-and-actions/deployments-and-environments>

### 6.2 运行过的命令（全部只读）

**gh（全部 GET）**
```
gh auth status
gh api repos/akz142857/Halro --jq '{owner,type,private,permissions}'
gh api repos/akz142857/Halro/actions/permissions
gh api repos/akz142857/Halro/actions/secrets/public-key
gh api repos/akz142857/Halro/environments
gh api repos/akz142857/Halro/environments/v1-release            # → 404（阴性对照）
gh api repos/akz142857/Halro/actions/secrets
gh api repos/akz142857/Halro/actions/variables
gh api repos/akz142857/Halro/deployments
gh api repos/akz142857/Halro/deployments/5785970777
gh api "repos/akz142857/Halro/deployments/5785970777/statuses?per_page=100"
gh api repos/akz142857/Halro/actions/runs/31131173718           # → 404（已删除）
gh api repos/akz142857/Halro/actions/runs/31131173946/jobs      # 标定 runner 步骤耗时
gh api repos/akz142857/Halro/actions/workflows
gh api "repos/akz142857/Halro/actions/workflows/324475184/runs?per_page=5" --jq '.total_count'
gh release list --limit 30
```

**git（只读）**
```
git status --porcelain
git remote -v
git tag -l ; git ls-remote --tags origin
git show ad815d6:.github/workflows/release.yml
git diff --stat HEAD -- go.mod go.sum
```

**本地只读检索 / 实跑（输出全部落在 scratchpad，未写入仓库）**
```
grep -rn "certificate-identity|certificate-oidc-issuer" --include=*.md --include=*.sh --include=*.py --include=*.json .
grep -rn "cosign" docs/ README.md
grep -n "environment: v1-release|test -n|certificate-identity|buildinfo.Date=|tar -C release -czf|docker save" .github/workflows/release.yml
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o <sp>/sz/halro ./cmd/halro
GOOS=linux GOARCH=amd64 CGO_ENABLED=0 go build -trimpath -ldflags "-s -w" -o <sp>/sz/halro-deadman ./cmd/halro-deadman
tar -C <sp> -czf <sp>/sz.tar.gz sz ; ls -l          # 单平台档体量标定
<sp>/bin/cosign version
<sp>/bin/cosign verify-blob --bundle <占位 bundle> <占位 blob>                       # 复现参数缺失报错
<sp>/bin/cosign verify-blob --certificate-identity … --certificate-oidc-issuer … …   # 复现越过参数校验
```

**未做的操作**：未打标签、未 push、未触发或重跑任何 workflow、未创建 Release、未修改仓库设置或 secrets/variables、未修改仓库任何已跟踪文件、未向公共 Sigstore 透明日志写入任何记录、未触碰 `data/` `master.key` `config.yaml`。`go build` 使用默认 `-mod=readonly`，`git diff --stat HEAD -- go.mod go.sum` 为空，未污染依赖文件。

### 6.3 仓库洁净性

```
$ git status --porcelain
?? docs/review/260811/releases.1.0.0/
```

唯一一行是本轮评审目录本身，**在我开工前的第一条命令里就已存在**（未跟踪，由本轮评审编排创建），本报告写入其中。已跟踪文件零改动。

### 6.4 所用模型与拒答情况

**Opus 5（1M context）**，模型 ID `claude-opus-5[1m]`。

**未遇到拒答**，未收到 `stop_reason: "refusal"`，无空响应或截断。全部裁决均带 `file:line` 或原始命令输出。
