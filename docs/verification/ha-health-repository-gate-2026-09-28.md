# HA 健康系统仓库检查点门禁（2026-09-28）

本记录只验证本地分支 `codex/ha-health-system` 的源码检查点
`02f2da143907dcdf1e64c6e58dc651317d11e355`。检查时工作树干净，Go
为 `go1.26.6 darwin/arm64`，前端使用 Node `v22.18.0`。这是仓库侧证据，
**不是 G0 正式通过**：此分支尚未推送，未取得对应远端 CI、正式制品集合、
签名或目标环境部署证据。当前 kind Pod 使用此前本地构建的镜像，不能称为
该提交的运行验收。

| 检查 | 结果 | 可复核材料 |
| --- | --- | --- |
| `GOCACHE=/tmp/halro-go-cache go test -count=1 ./...` | 通过，退出码 0 | `/tmp/halro-ha-health-kind-20260928/evidence/commit-02f2da14-go-test.log`；SHA-256 `8b2966945728409e3a197392bc520c0add7cbdf20a6720757b9e42c9fa40e6c1` |
| `go vet ./...`、`make fmt-check`、`git diff --cached --check` | 通过 | vet 日志为空且命令退出码 0；fmt 和 staged diff 无错误 |
| Node 22 `make frontend-production-check` | 通过 | typecheck、生产构建、30 文件浏览器制品扫描及已提交 `internal/webui/dist` 逐文件无漂移 |
| Node 22 `npm test` | 48 文件、702 项通过 | `/tmp/halro-ha-health-kind-20260928/evidence/commit-02f2da14-web-test.log`；SHA-256 `f18b850539981f360f47af59bb64f68e7d732ffa6519dde2a9d6cbcb670fbf28` |
| `deploy/observability/validate.sh` | 通过 | 14 条记录规则、64 条告警规则、promtool 测试及 Alertmanager 配置；日志 SHA-256 `7531d7e062fb832df1c178f582d7fe0dcba3617991633622a62c40abe0e84c58` |
| 发布工作流契约测试 | 15 项运行，1 项既有跳过，其余通过 | `python3 -m unittest tools/release/test_release_workflow_contract.py` |
| 本地夹具 | 通过 | deadman 接收器 1 项、HA 健康采样器 3 项 Python 单测；合成 mTLS 四轮采样另见[本地记录](ha-health-local-acceptance-2026-09-28.md) |
| 独立健康服务本机二进制 | 构建并返回提交身份 | `-version` 为 `v0.8.5-55-g02f2da14` / `02f2da14`；二进制 SHA-256 `fa5a4de466492fec3aef6130415ede498326e7081ca8b4251f783e19ff935a57` |

提交前，同一 Go 源码已经通过 `internal/app`、`internal/replication`、
`cmd/halro-ha-health`、`internal/hahealth` 和 `internal/deadman` 的受影响包
`-race -count=1`；此后只增加了 Python 采样器、测试和文档，并在提交后完成
上表的全仓门禁。未重复对未变更的 Go 源码运行耗时竞态测试。

日志目前只在本机 `/tmp`，没有独立不可变留存；哈希不能代替归档副本读回。
正式 G0 仍需冻结最终候选 SHA，核对该 SHA 的普通 CI、发布构建、镜像及
包摘要和 provenance。G1–G7、完整 20 行 HA 健康故障矩阵、客户端最终
逻辑操作来源、独立不可变归档、相邻版本和 72 小时 HA soak 继续保持
`NOT_RUN` 或仅有[部分本地证据](ha-health-local-acceptance-2026-09-28.md)。
