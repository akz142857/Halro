# M11-07 Production Readiness Evidence — 2026-08-03

状态：本地实现与自动化门禁通过；真实 AWS 14 项矩阵、RC artifact/Sigstore 证据和四方签署待补，因此不是 production-ready。

本地交付：

- KMS provider call/request ID 与 Halro `security.kms.call` Audit 关联，不记录 ARN、ciphertext、payload 或身份材料。
- 低基数 KMS call/duration/unlock/zero-fallback/descriptor/Slot/Recovery/pending-rotation Metrics 与 Prometheus contract tests。
- Primary unwrap error、Recovery not-ready/expired/used、Vault mismatch、pending rotation 告警与 promtool fixtures。
- 启动前 `RLIMIT_CORE=0` fail-closed；Linux `PR_SET_DUMPABLE=0`；无 pprof/crash uploader；明确 Go managed heap 无法保证 mlock、确定性擦除或安全的 per-slice `MADV_DONTDUMP`。
- AWS IAM/Primary/Recovery Key Policy、Recovery/Lifecycle Role 模板；Kubernetes `replicas=1`/`Recreate` 与 systemd hardening 基线。
- 现有 release workflow 构建 Accepted ADR 的单 artifact，执行 Test/Race/Vet/vulnerability scan，并生成 SPDX SBOM、checksums 和每个 blob 的 keyless Sigstore bundle。
- `publish` job 在 `v1-release` Environment approval 后仍 fail closed：要求 `M11_RELEASE_EVIDENCE_JSON`，绑定 signed tag 的 commit/tag 和实际 artifact SHA-256，执行 checksum 与每个 blob 的 Sigstore verification 后才允许创建 GitHub Release。

本地门禁（2026-08-03）：

- `go test ./...`
- `go test -race ./...`
- `go vet ./...`
- `go build ./...`
- `./deploy/observability/validate.sh`（22 条 alert rules 与 fixtures 通过）
- `./tools/m11/check-kms-boundaries.sh`
- `./tools/m11/check-production-assets.sh`
- `python3 -B -m unittest tools/m11/release-evidence/test_verify.py`（最终 release-evidence bundle fail-closed gate）
- Linux/amd64 `internal/hostsecurity` 交叉编译通过
- `GOTOOLCHAIN=go1.26.5 go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...`：0 个 reachable vulnerability

仍需真实归档：

- M11 真实 AWS 测试矩阵全部 14 项和 CloudTrail metadata；
- EKS identity-not-ready/deny/disabled-Key CrashLoop 实测；
- VM 与 Kubernetes 独立部署恢复；
- logs/errors/Audit/Metrics/bbolt/backup/heap Secret Canary 在最终 RC commit 重跑；
- 发布 RC 的 SBOM 审核、checksums 和 Sigstore verification 输出；
- Security、Backend、SRE、Release 负责人、UTC 时间、结论与例外编号。

最终脱敏 evidence bundle 使用 `tools/m11/release-evidence/verify.py` 验证。该 gate 要求同一完整 commit SHA 上的 14 项真实 AWS 场景、完整 Secret Canary、EKS/VM、独立操作者恢复、Recovery 撤权、全部 release artifacts/Sigstore verification 和四方签署；任何缺项、占位值、raw AWS ARN 或跨 commit 签署均 fail closed。

## 四方签署（待填写，禁止预签）

| 角色 | Reviewer | UTC | commit/tag | 结论 | 例外/证据 |
| --- | --- | --- | --- | --- | --- |
| Security | — | — | — | Pending | — |
| Backend | — | — | — | Pending | — |
| SRE | — | — | — | Pending | — |
| Release | — | — | — | Pending | — |
