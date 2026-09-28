# HA 健康系统仓库检查点门禁（2026-09-28）

本记录只验证本地分支 `codex/ha-health-system` 的源码检查点
`02f2da143907dcdf1e64c6e58dc651317d11e355`。检查时工作树干净，Go
为 `go1.26.6 darwin/arm64`，前端使用 Node `v22.18.0`。这是仓库侧证据，
**不是 G0 正式通过**：此分支尚未推送，未取得对应远端 CI、正式制品集合、
签名或完整目标环境部署证据。后续只把精确源码提交的健康服务镜像部署到
本地 kind；三个 HA 成员仍运行较早的本地镜像，因此不能称为该提交的
集群运行验收。

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
本地 kind 镜像准备另有一条明确边界：仓库 Dockerfile 的构建在解析
`golang:1.26.6-bookworm` 时收到 Docker Hub `EOF`，并未产出标准构建镜像。
第一次离线封装的 `:02f2da14` 两个镜像虽然注入了 `02f2da14` 版本字段，
但二进制的 `go version -m` 显示实际 `vcs.revision=c8a7211d...`；这两个镜像
**身份不一致，排除在候选和验收之外**，且从未部署到 Pod。它们的本地镜像
ID 分别为 `sha256:9260cf5be7400c4898dbda9f50f2d64b2aca3c6d33a1339f7625ca2c5ba75f10`
和 `sha256:9957fe7600b2f47814d46b681ce448cbf9e0b26a01431236b730196a0e5b132e`。

随后从本地 Git 仓库独立检出干净的 `02f2da143907dcdf1e64c6e58dc651317d11e355`，
用本机 Go 1.26.6 交叉编译静态 Linux arm64 二进制，以缓存的 distroless runtime
离线封装。两个二进制的 `go version -m` 均显示
`vcs.revision=02f2da143907dcdf1e64c6e58dc651317d11e355`、
`vcs.modified=false`，容器内 `version`／`-version` 也返回相同完整提交：

| 本地镜像 | 镜像 ID | 内含二进制 SHA-256 |
| --- | --- | --- |
| `halro-ha-health-local:02f2da14-exact` | `sha256:e4b65353c23425f84796cb9cd5172951336a34df4df8e57160a433a9bb5eb686` | `fd0b72f777eb66029c2d8ef84f1a33d19fc6be73c520d282efda635123a597ee` |
| `halro-ha-health-view-local:02f2da14-exact` | `sha256:f4162d7a39d79a0c0d90c02f5b5c4319398a10aec145a0fa5b2f79fed3bf40fd` | `83f658f5291df8bf37a8eb35510ee2a36eff4d8ea6204b75b686fd6f76831232` |

准确身份的镜像已加载到本地 kind 的四个节点。约 15:06 UTC 只将健康
Deployment 的 `view` 容器更新为 `:02f2da14-exact`；Recreate rollout 成功，
实跑 imageID 与上表一致，Deployment 1/1、两个容器 Ready 且零次重启，
三成员 StatefulSet 镜像未改且仍 3/3 Ready。后续以匹配的本地操作员
测试证书和独占端口转发读取精确镜像的 `/api/health`、
`/api/event-archive`、`/api/durable-transitions` 与 15 分钟 `/api/evidence`，
四个只读请求均返回 200；三成员机器状态和持久迁移当前链在读取时完整，
总览因无近期必需确认成功保持 `unknown`。原始响应摘要与限制见
[本地验收记录](ha-health-local-acceptance-2026-09-28.md)。这没有验证
成员镜像的同一源码身份、跨主机归档或故障恢复，也未取得正式发布流水线
的镜像摘要、SBOM、签名或 provenance。离线封装只支撑本地后续验收，
不能替代上述失败的标准 Dockerfile 门禁。

正式 G0 仍需冻结最终候选 SHA，核对该 SHA 的普通 CI、发布构建、镜像及
包摘要和 provenance。G1–G7、完整 20 行 HA 健康故障矩阵、客户端最终
逻辑操作来源、独立不可变归档、相邻版本和 72 小时 HA soak 继续保持
`NOT_RUN` 或仅有[部分本地证据](ha-health-local-acceptance-2026-09-28.md)。
