# 内存有界状态：续作交接

更新：2026-10-09 01:10（Asia/Singapore）。下次继续时从本文开始；细节与历史见[进度看板](memory-bounded-convergence-plan.zh-CN.md) §14–§16 和[总设计](memory-bounded-state-design.zh-CN.md)。

## 1. 代码在哪里

- 分支 `memory-bounded-state`，**未推送**。基于 `main` 的 `2425ae8e`。
- 本地证据目录 `docs/verification/performance/` 与 `docs/architecture/evidence/` 均被 `.gitignore` 忽略，不在提交里。
- 原任务计数：**25 项中 5 完成 / 20 开放**（D-04 已拆为 D-04a 本地容量、D-04b 生产校准；D-01、R-01、D-04b 按用户指示暂缓）。

| 提交 | 内容 |
| --- | --- |
| `dd3ca6f2`…`c1550229` | 原工作树按模块拆分的 11 个提交（中间提交不保证单独可编译） |
| `170299ca` | S1-1：请求终结并入 HA 共享 RAM 池（优先成员、私有优先回退、私有 Disk）；预检补计 Audit/Governance 批编码器 |
| `71c4b46d` | S0：`budget` 遗留恢复矩阵默认代表子集；SIGKILL 子进程期限 30 秒 → 2 分钟；门禁加 `-timeout=20m` |
| `27150aea` | S1-2：Audit/Governance HA 批次精确预分配、就地编码、共享只读缓冲；编码器费用 64 → 32 MiB |
| `ceddc1d1` | `internal/app` 测试分片、9 个崩溃时点矩阵代表子集、发布压力任务跑完整矩阵、契约测试与 fuzz 列表修复、`runtime_repack` 时序竞争修复 |
| `2b9027f5`（及其修订） | 本交接文档、进度看板 §17、总设计开头入口 |

## 2. 用户已做的裁定

1. D-04 拆分为 D-04a（本地，可关闭）与 D-04b（生产校准，随 D-01 暂缓）。
2. 允许路线 A：请求终结可进入共享池，池内终结优先。
3. 十万请求 / 1800 秒是真实目标，冷构建改造归入 **M3**。
4. 测试时限：采用方案 A（矩阵代表子集）+ 推荐的 **CI 分片**。
5. 容量路线：按推荐先做 **路线 1：重新论证 1387 MiB Recovery reserve**。

## 3. 当前卡点（按优先级）

### 3.1 容量：原 2 GiB HA 配置仍不成立

- 预检必要 normal **661.046 MiB**，原 normal 661 MiB，逻辑缺口 **48128 B**。
- 但这只覆盖必要 owner。真实 Linux（逻辑 2 GiB、Recovery 1387 MiB、实际 2 GiB cgroup、3 GiB ext4）运行：Replica 已越过 Metadata 接收窗口，随后在**运行阶段读取 Replica Metadata** 时被拒绝；此时 normal 已用 **614.587 / 661 MiB**，与预检向量吻合，剩余不足一次普通读取工作区。
- 1387 MiB **没有富余**：它正是用原配置代入 `ValidateReplicationCandidates` 公式得到的最小值（1453961408 B 取整），见 `docs/verification/performance/2026-10-07-memory-state-authentication-reuse/ha-budget-derived-plan.zh-CN.md`。
- 公式最大项是 `3 × (candidate + queue + read)` = 3 × 256.06 = **768.2 MiB**。这个 3 是 `replicationlimits.MaxBudgetedPeerSessions`（注释：“two peers and one replacement/control session”），即按**三节点**集群设计。配置允许 1 或 2 个 peer（`config.go` `validateReplication`），两节点会给出“失去任一成员后无法确认写入”的警告。

**待用户回答：目标拓扑是两节点还是三节点？**

- 两节点：会话数按 `len(peers)+1` 推导，可减少约 256 MiB Recovery，2 GiB 出现真实余量。需同时改 `ValidateReplicationCandidates`、`validateSelectedHASessionBudget`、`NewPeerSessionBudget(…, sessions)` 的实参，并证明第 3 个会话在两节点下不可能出现（槽满时的行为须 fail-closed）。`NativeReaderSets = 3`（一组默认 + 每 peer 一组）同理。
- 三节点：这 256 MiB 省不下来，需要继续论证公式中各项的真实同时活跃关系（三次重建是否同时达峰、accounting recovery 128 MiB 与会话是否重叠等），或考虑调整 HA 容量目标（例如 2.5 GiB）。

### 3.2 测试时限：race 分片未实测

- `internal/app` 空闲单跑 **1924.5 秒**（其中约 9 分钟与一次误跑的分片重叠，真实值略低）；1173 个测试，长尾为主，单靠缩矩阵不够，所以改为分片。
- 普通测试 4 片：按旧逐测试耗时估算最重一片约 600 秒；第 0 片实测 521.7 秒（有并发负载）通过。
- **race 8 片未实测**：最重一片（第 2 片）估算普通模式约 398 秒，race 通常 2–5 倍，可能接近或超过 20 分钟。下次先跑：
  ```bash
  time scripts/go-test-shard.sh app 2 8 -race -count=1 -timeout=60m
  ```
  若超过约 15 分钟，把 race 片数提高（`scripts/go-test-shard.sh check N` 先自检，同时改 CI/release 的 matrix、Makefile `race` 目标和 `tools/release/test_release_workflow_contract.py` 中的命令字符串）。
- `budget` 包单跑 412 秒（有后台负载）；`ledger` 单跑 361 秒。其余包未单独复测。
- **完整门禁 `make full-check` 尚未在本分支跑过**，推送前必须跑一次。

### 3.3 其他已登记开放项

1. 启动失败后关闭 foundation 时，Usage 报告 `ownedfile.ErrRetirement`（“owned file actual IO retirement is uncertain”）并留下共享成员；`usage` 包内约 45 处来源，需在 Linux 上复现失败启动后逐层追踪，判断是 fail-closed 设计还是清理缺陷。
2. 同一 ext4 上前一失败测试的残留会让后一测试报“physical completion allocation unavailable”：测试隔离问题，非产品瓶颈；修复失败清理（上一项）后应消失。
3. 请求退回共享池时，HA 终结在持有工作区期间等待 `WaitConfirmed`，会串行化终结确认；仅在私有池放不下时发生。
4. Audit/Governance 批编码器并入共享池（路线 A）未做：Governance `AppendBatch` 可能在请求持有共享池时嵌套调用，合集需加槽，节约为零，且无并发证据。
5. I-06 角色 generation 切换批次：失败夹具已修（`replica_generation_retry_test.go`），本批原定的在线切换路径未继续。

## 4. 下次建议顺序

1. 回答 §3.1 的拓扑问题。
2. 实测 race 最重分片（§3.2 命令），必要时调整片数；然后跑一次 `make full-check`。
3. 按拓扑结论推进 Recovery reserve 论证（两节点：会话数按 peer 推导 + 真实 Linux 双角色在原 2 GiB 下完成启动、请求、checkpoint、两次重启、Close）。
4. 排查 §3.3-1 的失败启动清理。
5. 之后回到 S2 逐项接受：I-03 → I-06 → I-04/I-05 HA 部分 → I-11 → I-12 → I-07～I-10 → O-01/O-02 → V-01 → V-02；十万冷构建在 M3。

## 5. 常用命令

```bash
# 分片自检与运行
scripts/go-test-shard.sh check 4 && scripts/go-test-shard.sh check 8
scripts/go-test-shard.sh rest -count=1 -timeout=20m
scripts/go-test-shard.sh app 0 4 -count=1 -timeout=20m
# 完整矩阵（发布压力门禁同款）
HALRO_STRESS=1 go test ./internal/budget ./internal/app -run 'TestLegacyRecovery|ActualSIGKILL|SIGKILLResumes|PendingPublicationResumes|TestHAMemoryActivationActualSIGKILLResume' -count=1 -timeout=60m
# 预检向量与真实 Manager 对账
go test ./internal/config -run TestHAPhysical -count=1 -v
# 真实 Linux 双角色（需要 Docker；runner 位于被忽略的本地证据目录）
HALRO_HA_RECOVERY_RESERVE_MIB=1387 HALRO_RUN_PATTERN='^TestHAPhysicalPeerCreditActualTLSPreacceptAndUnusedRelease$' \
  python3 docs/verification/performance/2026-10-09-s1-request-shared-lane/runner.py ha-preflight <linux-arm64 suite.test> <新输出目录> 0
```

Linux 测试二进制：`CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c -o <path>/suite.test ./internal/app`。runner 只在本机的被忽略目录里；换机器需要从 `docs/verification/performance/2026-10-08-budget-feasibility/sharing-order/runner.py` 复制并加上 `HALRO_HA_RECOVERY_RESERVE_MIB` 与 `HALRO_RUN_PATTERN` 透传。
