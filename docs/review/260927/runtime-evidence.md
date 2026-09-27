# Runtime 与门禁证据

> 本文件前半部分保留初始 Review 的证据快照；整改后的最终仓库门禁追加在末尾。初始“只新增 Markdown”和“findings 未关闭”不描述整改后的工作区。

## 基线与环境

- 范围：`v0.8.5` / `28ea173b8ccfa2a8043a767c4c4d2782018a4d1c` → `0c4a270d7aae2b3d4562c35cd60ff14907278897`
- 提交：52；变化：508 files，约 +55,090 / -2,621
- 本机：Go `1.26.6 darwin/arm64`，Node `24.18.0`，npm `11.16.0`
- Review 开始前工作区干净；本轮只新增 `docs/review/260927/` Markdown。

## 已通过的本地验证

所有 Go 结果证据使用 `-count=1`；只在并发/生命周期路径使用 `-race`。

| 范围 | 结果 |
| --- | --- |
| `go test -count=1 ./internal/routegate ./internal/gateway ./internal/gatewayapi ./internal/keylimit ./internal/advisor` | PASS |
| `go test -count=1 ./internal/config ./cmd/halro` | PASS；`cmd/halro` 首次因 sandbox 禁止 loopback 失败，原命令在允许本地监听后重跑 PASS |
| `go test -count=1 ./internal/metadatajournal ./internal/store/bolt ./internal/ledger ./internal/audit ./internal/governance` | PASS |
| `go test -race -count=1 ./internal/replication/` | PASS；首次同样受 sandbox loopback 限制，获准后原命令重跑 PASS |
| App 双 Runtime、route suspension、models、SIGKILL、TTFT 定向 race 集 | PASS |
| HA promotion/stepdown/seed/backup/reseed 与 journal classification 定向集 | PASS |
| `go test -count=1 ./deploy/kubernetes/` | PASS |
| `./deploy/observability/validate.sh` | PASS；Docker 权限允许后验证 13 recording rules、49 alert rules、rule tests 与 Alertmanager config |
| 6 个目标页面测试 | PASS，160 tests |
| design-system、i18n、read-only role | PASS，61 tests |
| `npm run typecheck` | PASS |
| Node / Go / Python 官方 SDK 黑盒兼容测试 | PASS |
| documented-gate/build-identity、dependency-license、SBOM shell syntax | PASS |

完整命令与各 Track 的更窄正则由角色报告保留；本文件记录可作为最终裁决的合并结果。现有测试通过说明已覆盖的正向与故障路径未回归，不代表缺少 oracle 的 R-001–R-014 已关闭。

## 当前 CI

精确 HEAD 的 [GitHub Actions run 36297620187](https://github.com/akz142857/Halro/actions/runs/36297620187) 为 success：Go 全量与 race、vet、KMS boundary、vulnerability scan、frontend tests/typecheck/build/bundle drift、SDK compatibility、container、SBOM、repository hygiene 和 observability jobs 均成功。

CI 结论只证明仓库门禁在该 SHA 上通过；它不是 kind/Linux/生产验收，也不能发现本轮确认但尚无回归 oracle 的生命周期问题。

## 未执行或仍 BLOCKED

- 未运行任何 billable Provider smoke；Kimi/Claude 当前线上行为与订阅条款未刷新。
- 未做真实浏览器 1440/1024/390、键盘和读屏检查。
- 未做 kind Pod deletion、PDB、endpoint 更新、SIGTERM/SSE drain、maintenance、restore/re-seed。
- 未做真实三节点非对称 partition/物理 fencing、Linux ENOSPC/只读/慢盘/性能。
- 未做相邻正式版本滚动升级与真实 schema-changing restore。
- 未做 G0–G7、72 小时 soak、真实 RTO、签名 artifact/provenance 与生产实例同 SHA 验收。

初始 Review 只新增 Markdown，因此当时没有因为文档追加重复执行已经成功的完整代码 gate。

## 整改后的最终仓库门禁

R-001–R-014 修复完成后，主 Reviewer 在允许 loopback 的本机环境执行最终门禁。第一次运行发现 endpoint compatibility golden 只改了生成物、未改 Go 权威生成源；修复生成源、增加 source-level test 并重新生成 JSON 后，得到以下最终结果：

| 命令/阶段 | 结果 |
| --- | --- |
| `go test -count=1 -shuffle=on ./...` | PASS |
| `go test -race -count=1 -timeout=20m ./...` | PASS；`internal/app` 约 611 秒 |
| `go vet ./...` | PASS |
| `cd web && npm test` | PASS；46 files、677 tests |
| `./deploy/observability/validate.sh` | PASS；13 recording rules、49 alert rules、Prometheus/Alertmanager provisioning |
| `make frontend-production-check`，Node `v22.18.0` | PASS；production build、29 artifacts secret scan、embedded bundle drift |
| `git diff --check` | PASS |

完整 `make full-check` 所在 shell 的 Node 是 `v24.18.0`，所以最后一个 target 在执行前按设计拒绝错误版本；此前普通 Go、race、vet、frontend test 和 observability 均已通过。随后仅切换 `PATH` 到已安装的 Node `v22.18.0` 并运行尚未执行的 `frontend-production-check`，两条命令之间没有代码变化。这里把组成门禁记为 PASS，不把 Node 版本拒绝伪装成一次退出码为 0 的 `make full-check`。

整改后新增的 Review Markdown 按仓库政策不触发重复代码门禁。目标环境与生产范围仍维持上节 BLOCKED，且未运行 billable Provider smoke。
