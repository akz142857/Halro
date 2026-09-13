# 目标 SHA 运行证据

评估对象：`v0.8.0` / `1d48ecde216ef40e653738f8bdc9657f88a717cc`

快照时间：2026-09-13（Asia/Singapore）。本文件区分本地、托管 CI、正式发布与下游渠道，
不把其中任一层的结果外推为另一层。

> 后续状态（2026-09-13）：本文件第 1–4 节是冻结 SHA 的首轮证据。整改工作树新增了 fresh full gate、
> 真实 Ledger/Usage 双二进制恢复、Admin 浏览器旅程、1000 SSE cleanup 和 8 轮交替 benchstat；原始摘要
> 与文件见 [E3 completion evidence](evidence/e3-completion/README.md)。该工作树尚未形成新的提交 SHA，
> Homebrew 仍为 v0.7.0、APT `InRelease` 返回 503，真实 Provider/KMS/Contact Point/24h soak 未执行，
> 因而不能把新增本地证据表述为 v0.8.0 渠道或生产证明。

## 1. 环境

- 本地：macOS arm64；Go 1.26.6；Node v24.18.0；npm 11.16.0；Git 2.50.1。
- GitHub Actions：由对应 workflow 固定的 runner 与 Go/Node 版本决定，详情以链接中的 job 为准。
- 产品代码在评估开始时与目标 SHA 一致；工作区变更仅为本次评估文档。
- 未授权也未执行真实 Provider、真实 KMS、外部告警、生产部署、24 小时 soak 或付费 smoke。

## 2. 本地门禁

| 检查 | 命令 | 结果 | 解释边界 |
| --- | --- | --- | --- |
| Go 全量，首次沙箱运行 | `GOCACHE=/private/tmp/halro-philosophy-go-cache go test -count=1 ./...` | ENVIRONMENT_BLOCK | `cmd/halro` 的 `httptest` 绑定 `[::1]:0` 被沙箱拒绝，非断言失败；随后在允许回环监听的环境复跑 |
| 前端全量，首次并发运行 | `npm --prefix web test` | FAIL：40/44 files 通过，598/604 tests 通过 | 与全量 Go 并发；4 个失败为 5 秒超时或异步数据未及时就绪。必须隔离复跑后再分类，不能直接称代码回归 |
| 前端失败面隔离复跑 | `npx vitest run <4 failed files> --maxWorkers=1` | PASS：4/4 files，74/74 tests | 首次失败面在资源隔离后全部通过，支持“本机并发/超时噪声”解释 |
| 前端全量，隔离复跑 | `npm --prefix web test` | PASS：44/44 files，604/604 tests | 本地 Node 24 的补充行为证据；受支持工具链仍以 CI Node 22 为准 |
| 前端类型检查 | `npm --prefix web run typecheck` | PASS | TypeScript project build 无错误 |
| 前端生产构建 | `npm --prefix web run build` | PASS；29 个浏览器产物 secret scan clean | 本机 Node 24 能构建，不证明与 Node 22 byte-for-byte 一致 |
| 嵌入 bundle drift，本机 Node 24 | `git diff --exit-code -- internal/webui/dist` | ENVIRONMENT_MISMATCH | content hash 与提交产物不同；`CONTRIBUTING.md` 和 CI 明确要求 Node 22，本机是 Node 24。生成变化已完整恢复，正式零漂移证据采用 exact-SHA CI Node 22 |
| Go 全量，允许回环监听 | `GOCACHE=... go test -count=1 ./...` | FAIL：除 `internal/deadman` 外全部通过 | `TestSlowReceiverDoesNotBlockProbeTick` 用时约 375ms，超过 250ms 阈值，并出现临时目录清理竞态；需看隔离重复结果 |
| dead-man 失败面隔离复跑 | `go test -count=10 ./internal/deadman -run '^TestSlowReceiverDoesNotBlockProbeTick$'` | PASS：10/10 | 未稳定复现产品阻塞；将全量失败归类为资源敏感的本地证据不稳定，不冒充全量绿色 |
| 双二进制 schema 演练 | v0.7.1 binary 初始化/bootstrap；v0.8.0 binary 打开/doctor；旧 binary 再 doctor | PARTIAL PASS | 直接证明 schema 36→37 与旧读者对 schema 37/Usage manifest 8 的明确拒绝；临时数据无真实 Ledger/Usage，未做 backup/restore 与 kill-point |
原始终端输出未纳入 Git，避免把数万行临时日志当成长期文档。构建生成的 Node 24 bundle 已恢复为
目标提交内容；产品目录最终保持无改动。

## 3. 托管 CI 与发布证据

| 层级 | 运行 | 结果 | 能证明什么 | 不能证明什么 |
| --- | --- | --- | --- | --- |
| 普通 CI | [run 34593011762](https://github.com/akz142857/Halro/actions/runs/34593011762) | SUCCESS | 精确 SHA 的 CI graph 通过 | 生产、真实 Provider/KMS、长期稳定性 |
| release dry-run | [run 34593630337](https://github.com/akz142857/Halro/actions/runs/34593630337) | SUCCESS | 不发布路径的 release 构建与验证通过 | 正式发布后的下游权限与渠道验收 |
| 正式 release | [run 34598236997](https://github.com/akz142857/Halro/actions/runs/34598236997) | FAILURE（部分成功） | quality、web、SDK、stress、容器、二进制、deb、provenance、publish、container-push 成功 | downstream-package-repositories 失败，不能称完整渠道发布成功 |
| GitHub Release | [v0.8.0](https://github.com/akz142857/Halro/releases/tag/v0.8.0) | PUBLISHED | 不可变 Release 和附件已发布 | Homebrew/APT/网站已同步 |
| GHCR | 正式 run 的 `container-push` job | SUCCESS | workflow 报告多架构 manifests 已推送 | 外部集群 pull 与生产运行 |
| Homebrew | `halro-ai/homebrew-tap/Formula/halro.rb` 快照 | STALE | 公开 Formula 仍指向 v0.7.0 | 未来何时修复或历史用户安装状态 |
| APT | 私有 `halro-ai/apt-repository` | UNVERIFIED | 当前凭据不足以读取仓库内容 | 不能推断成功或失败 |

正式 release 唯一失败 job 的失败点是 `Create a repository-scoped release automation token`；
`client-id` 输入为空，对应 `HALRO_RELEASE_APP_CLIENT_ID` 不可用。发布工作流先完成 `publish` 和
`container-push`，随后才运行该凭据步骤，因此这不是核心制品构建失败，而是可预检的下游控制面失败。

## 4. 证据限制

- GitHub 成功结果是托管短运行证据（E3），不是生产 SLO 或长稳证据（E4）。
- fixture Provider、SDK facade 和本地 fake service 不能证明真实上游行为。
- 本地首次失败保留为环境/资源噪声样本；只有隔离复跑仍失败且可稳定复现，才升级为产品 finding。
- 评估文档不会把 release、container、Homebrew、APT、网站聚合成一个模糊的“发布成功”。
