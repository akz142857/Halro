# Halro 生产验证执行记录 · 2026-09-25 · G0 重新冻结

> - 执行方案：[生产验证执行方案](production-validation-plan.zh-CN.md)
> - 上一次执行：[2026-09-18 记录](production-validation-run-260918.zh-CN.md)
> - 候选提交：`9e0d73c71535b4caec69718a6546892ad56fef02`
> - 运行版本串：`v0.8.5-37-g9e0d73c7`（**无 `-dirty`**）
> - 执行环境：单台 macOS 27.0 / Darwin arm64 开发机，Go 1.26.6，Node v22.18.0
> - **结论：G0 `CONDITIONAL PASS`**——第 1、2、4 项 PASS，第 3 项的镜像、包、SBOM、
>   签名与 provenance 仍需发布流水线，见第 4 节

## 0. 这份记录是什么

只做一件事：把 2026-09-18 记录里 **260918-PV-F-01** 指出的问题关掉。

那次冻结时工作树不干净，版本串带 `-dirty`，所以方案 §G0 的「源代码、CI、运行版本和所有制品
可追溯到同一 SHA」这句话在那个 SHA 上不成立，2.1 的 digest 与 2.2 的门禁结果都只对当时的树
有效。方案要求发布前用干净候选重跑 G0，这就是那一次。

本次**没有**推进 G1–G7 中的任何一项：它们缺的是隔离目标环境、真实 Provider 账户、真实
KMS/PKI、真实 Contact Point、连续 24 小时窗口和四方人工签署，一样都没有变化。整体结论仍然是
`NO-GO / PRODUCTION UNVERIFIED`。

## 1. 冻结前落入候选的变更

重新冻结之所以要等，是因为 2026-09-18 的修复还挂在一叠 PR 上。冻结前它们全部合入 `main`：

| PR | 内容 | 对应发现 |
| --- | --- | --- |
| [#359](https://github.com/akz142857/Halro/pull/359) | 确定性 advisor：把 Halro 自己已有的数字摆到一起 | — |
| [#360](https://github.com/akz142857/Halro/pull/360) | HA Phase 0a：让 `halro.db` 成为 metadata journal 的投影 | [#315](https://github.com/akz142857/Halro/issues/315) |
| [#361](https://github.com/akz142857/Halro/pull/361) | 流式首字节埋点 | 260918-PV-F-06 |
| [#362](https://github.com/akz142857/Halro/pull/362) | 真 `SIGKILL` 注入与不可写目录覆盖 | 260918-PV-F-21 |
| [#363](https://github.com/akz142857/Halro/pull/363) | 审计乱序/断链测试与「变更必被审计」穷举 | 260918-PV-F-18 / F-19 |
| [#364](https://github.com/akz142857/Halro/pull/364) | 文档指名的门禁与 Makefile 对齐，并加防漂移门禁 | 260918-PV-F-14 |
| [#365](https://github.com/akz142857/Halro/pull/365) | 对已有的项目日预算告警，规则文件里不写阈值 | 260918-PV-F-07 |
| [#366](https://github.com/akz142857/Halro/pull/366) | 配置键完整性与校验边界，两道门禁 | — |
| [#375](https://github.com/akz142857/Halro/pull/375) | 版本 stamp 成为构建输入 | **260925-PV-F-01**（见第 3 节） |

每个 PR 合并前都重新 rebase 到当时的 `main` 并等 CI 重跑，因为 #359 与这条链从未一起测过。
合并后逐个用 `git merge-base --is-ancestor` 核对提交确实在 `main` 上——PR 显示 MERGED 不等于
代码到了 `main`，这是本仓库出过的事故。

## 2. 冻结的验证单元

| 字段 | 本次值 |
| --- | --- |
| `CANDIDATE_SHA` | `9e0d73c71535b4caec69718a6546892ad56fef02` |
| 工作树状态 | **干净**（`git status --porcelain` 冻结时、完整门禁后各验一次，均为空） |
| `RELEASE_VERSION` | `v0.8.5-37-g9e0d73c7`（未发布；最近的 release 是 v0.8.5） |
| `ARTIFACT_DIGESTS` | `bin/halro` `sha256:3f2d2146995389c486e635c1dc8f99622e4692129f533851b975a0ceb4782c20`<br>`bin/halro-deadman` `sha256:c95d4834a2310867a4eda448b43dee35f5903b5ba21e63a33dda7e97c37b159d`<br>（本机构建，`SOURCE_DATE_EPOCH=1790318611`，即候选提交自身的时间戳） |
| `TEST_PLAN_REVISION` | `9e0d73c7` |
| `TARGET_ID` / `CONFIG_DIGEST` | 无；本次不涉及目标环境 |

## 3. G0 逐项

### 3.1 冻结候选、确认工作树与构建输入 · PASS

工作树干净，版本串不带 `-dirty`。这正是 260918-PV-F-01 缺的那一半。

### 3.2 对精确 SHA 跑完整门禁与普通 CI · PASS

| 检查 | 命令 | 结果 |
| --- | --- | --- |
| 仓库完整门禁 | `make full-check`（Node 22） | 退出 0；125 个 Go 包 ok（`test` 与 `race` 两轮），typecheck、生产构建与内嵌 bundle 漂移检查均通过 |
| 普通 CI | GitHub Actions run [`36104167829`](https://github.com/akz142857/Halro/actions/runs/36104167829)，head `9e0d73c7` | 10 个作业全部 success：`go`、`web`、`fuzz`、`container`、`observability`、`repository-hygiene`、`sdk-compatibility`、`deadman-sbom`、`observability-sbom`（prometheus / alertmanager 两例） |

门禁跑完后工作树仍为空——即**无未解释的生成物漂移**，内嵌的 `internal/webui/dist` 与源码一致。

### 3.3 从该 SHA 构建制品并记录 digest、SBOM、签名与 provenance · PARTIAL

二进制这一半做完了，见第 2 节的 digest。并且核实了 Makefile 里那句「`SOURCE_DATE_EPOCH`
能让 stamp 可复现」不是声明而是事实：固定该变量后连续两次 `rm -rf bin && make build`，
两个二进制的 sha256 逐字节一致。

**没有做的**：容器镜像、`.deb`/`.rpm` 包、SBOM、Sigstore 签名与 provenance。这些只由受保护的
发布流水线产出，需要一个 tag（本仓库的 tag 由 workflow 创建，不手工 push）与发布授权，而发布
授权本身是 G6 的阻塞项。CI 的 `container` 与 `*-sbom` 作业在本 SHA 上确实跑过并通过，但它们
是门禁而不是留痕的发布制品。

**因此 G0 不能判为完整 PASS。**

### 3.4 验证运行时版本信息回指同一 SHA · PASS（修复后）

```
$ ./bin/halro version
{"build":{"version":"v0.8.5-37-g9e0d73c7","commit":"9e0d73c7","date":"2026-09-25T06:43:31Z"}, ...}
$ ./bin/halro-deadman -version
{"version":"v0.8.5-37-g9e0d73c7","commit":"9e0d73c7","date":"2026-09-25T06:43:31Z"}
```

这一项第一次跑的时候是**不通过**的，见下。

## 4. 本次新发现

### 260925-PV-F-01（P2）：`make build` 会留下一个自称来自不干净树的旧二进制 · 已修

第一次执行 3.4 时，在 `cf154e89` 的干净检出上：

```
$ ./bin/halro version
{"build":{"version":"v0.8.5-36-gcf154e89","commit":"cf154e89", ...}}
$ ./bin/halro-deadman -version
{"version":"v0.8.3-12-g42e48090-dirty","commit":"42e48090","date":"2026-09-18T14:20:53Z"}
```

一个来自 **2026-09-18**、带 `-dirty` 后缀的 describe 串——那天的树不干净。没有任何东西报错。
`ls -l bin/` 说明了原因：文件根本没被重写。

**成因**：`bin/halro-deadman` 只依赖 `$(DEADMAN_SOURCES) go.mod go.sum`，而这些在 HEAD 移动时
不动，于是 make 认为一个带着一周前身份的二进制是最新的。`bin/halro` 有同样的缺陷，只是它的
源码在多数提交里都会变，所以一直因为别的原因被重链接而把问题盖住了。

G0 的判据就是「所有制品可追溯到同一 SHA」。这是这个答案变成「否」而沿途没有任何东西报警的
路径，所以它值一道门禁而不是一行修补。

**修法**（[#375](https://github.com/akz142857/Halro/pull/375)）：版本 stamp 成为它自己的前置依赖，
写入前先 `cmp`，所以未变的检出仍然不重链接；`RELEASE_DATE` 刻意不进 stamp——它每次调用都变，
会把这条规则变成「永远重建」，那是同一个失败的另一条路径，而且会逼人停止使用该目标。
`tools/gates/test_build_identity_contract.py` 守住两半，已进 CI，两半都做过反向验证。

**影响范围**：发布流水线从未受影响，它在 CI 里全新构建并注入三个值。受影响的是每一个从检出
构建出来的二进制——也就是 G0 第 3 项量的那个东西，以及运营者从 `halro version` 读回的那个值。

## 5. 结论与下一步

| 项 | 结论 |
| --- | --- |
| 260918-PV-F-01 | **CLOSED**。干净候选、干净版本串、同一 SHA 上的完整门禁与 CI 均已留痕 |
| G0 | `CONDITIONAL PASS`：第 1、2、4 项 PASS，第 3 项 PARTIAL（缺发布制品与签名） |
| G1–G7 | 不变，仍按 2026-09-18 记录 |
| 整体 | `NO-GO / PRODUCTION UNVERIFIED` |

G0 剩下的那半项与 G6 是同一个阻塞源：发布授权。在拿到它之前，G0 只能停在
`CONDITIONAL PASS`，而这已经足以解除[HA 架构设计](../todo/halro-ha-architecture.zh-CN.md) §1.3
里「Standalone 已走完 G0」这一条对 Phase 0b 的**仓库侧**约束——但 §1.3 要的是 G0–G7 全部走完
至少一次，所以 HA Phase 0b 仍然不具备开工条件。
