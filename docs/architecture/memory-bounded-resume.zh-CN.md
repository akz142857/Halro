# 内存有界状态：续作交接

更新：2026-10-09 09:30（Asia/Singapore）。下次继续时从本文开始；细节与历史见[进度看板](memory-bounded-convergence-plan.zh-CN.md) §14–§18 和[总设计](memory-bounded-state-design.zh-CN.md)。

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
| `ddca5b72` | HA Recovery 公式移除离线 backup 工作区与 backup/restore claim；原 2 GiB 可行区间约 128 MiB（看板 §18） |

## 2. 用户已做的裁定

1. D-04 拆分为 D-04a（本地，可关闭）与 D-04b（生产校准，随 D-01 暂缓）。
2. 允许路线 A：请求终结可进入共享池，池内终结优先。
3. 十万请求 / 1800 秒是真实目标，冷构建改造归入 **M3**。
4. 测试时限：采用方案 A（矩阵代表子集）+ 推荐的 **CI 分片**。
5. 容量路线：按推荐先做 **路线 1：重新论证 1387 MiB Recovery reserve**。
6. HA 目标拓扑为**三节点**：3 组会话（2 peer + 1 替换）保留，不按 peer 数缩减。

## 3. 当前卡点（按优先级）

### 3.1 容量：原 2 GiB HA 配置已有可行区间，完整生命周期未接受

- `ddca5b72` 后，原 2 GiB 生产预算的 Recovery 可行区间为 **1258.605 MiB（候选公式下限）～约 1386.95 MiB（物理工作向量的 normal 下限）**；改前此区间为空。依据：backup/restore 工作区只在离线命令自建的管理器中获取，VTA 调用图证明服务进程不可达（看板 §18）。
- 真实 Linux（逻辑 2 GiB、Recovery **1259 MiB**、实际 2 GiB cgroup、3 GiB ext4）：peer credit 与 online checkpoint 两个双角色测试通过；原 1387 MiB 下二者失败。夹具不经过配置门禁，旧二进制在 1259 下同样通过，所以运行时结论与门禁结论分开成立。
- **仍未接受**：启动、请求、checkpoint、两次重启、Close 的完整双角色生命周期；三节点（两个 peer 同时在线）的真实运行也没有夹具。
- 公式中其余项（accounting 128、projection 192、Metadata 映射 96 与会话的真实重叠）未再缩减，需要更大余量时再逐项论证。注意：`WithRecoveryScope` 下的 IO/Background 申请会转记到 Recovery，静态调用图只能证明“某所有者在服务进程中完全不存在”，重叠关系需运行时证据。

### 3.2 测试时限：race 分片未实测

- `internal/app` 空闲单跑 **1924.5 秒**（其中约 9 分钟与一次误跑的分片重叠，真实值略低）；1173 个测试，长尾为主，单靠缩矩阵不够，所以改为分片。
- 普通测试 4 片：按旧逐测试耗时估算最重一片约 600 秒；第 0 片实测 521.7 秒（有并发负载）通过。
- race 最重分片（第 2 片）实测 **550.6 秒通过**（墙钟 9 分 35 秒，有并发负载），远低于 20 分钟，片数不需调整。
- `budget` 包单跑 412 秒（有后台负载）；`ledger` 单跑 361 秒。其余包未单独复测。
- **完整门禁 `make full-check` 尚未在本分支跑过**，推送前必须跑一次。

### 3.3 其他已登记开放项

1. 启动失败后关闭 foundation 时，Usage 报告 `ownedfile.ErrRetirement`（“owned file actual IO retirement is uncertain”）并留下共享成员；`usage` 包内约 45 处来源，需在 Linux 上复现失败启动后逐层追踪，判断是 fail-closed 设计还是清理缺陷。
2. 同一 ext4 上前一失败测试的残留会让后一测试报“physical completion allocation unavailable”：测试隔离问题，非产品瓶颈；修复失败清理（上一项）后应消失。
3. 请求退回共享池时，HA 终结在持有工作区期间等待 `WaitConfirmed`，会串行化终结确认；仅在私有池放不下时发生。
4. Audit/Governance 批编码器并入共享池（路线 A）未做：Governance `AppendBatch` 可能在请求持有共享池时嵌套调用，合集需加槽，节约为零，且无并发证据。
5. I-06 角色 generation 切换批次：失败夹具已修（`replica_generation_retry_test.go`），本批原定的在线切换路径未继续。

## 4. 下次建议顺序

1. 确认 `make full-check`（Node 22）在 `ddca5b72` 上的结果，推送前必须通过。
2. 真实 Linux 双角色在原 2 GiB / 1259 MiB 下完成启动、请求、checkpoint、两次重启、Close 的完整生命周期接受（需新夹具或扩展现有夹具）。
3. 排查 §3.3-1 的失败启动清理。
4. 之后回到 S2 逐项接受：I-03 → I-06 → I-04/I-05 HA 部分 → I-11 → I-12 → I-07～I-10 → O-01/O-02 → V-01 → V-02；十万冷构建在 M3。

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
HALRO_HA_RECOVERY_RESERVE_MIB=1259 HALRO_RUN_PATTERN='^(TestHAPhysicalPeerCreditActualTLSPreacceptAndUnusedRelease|TestHAPhysicalOnlineCheckpointKeepsActualServicesAndTLS)$' \
  python3 docs/verification/performance/2026-10-09-recovery-offline-workspaces/runner.py ha-preflight <linux-arm64 suite.test> <新输出目录> 0
```

Linux 测试二进制：`CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c -o <path>/suite.test ./internal/app`。runner 只在本机的被忽略目录里；换机器需要从 `docs/verification/performance/2026-10-08-budget-feasibility/sharing-order/runner.py` 复制并加上 `HALRO_HA_RECOVERY_RESERVE_MIB` 与 `HALRO_RUN_PATTERN` 透传。
