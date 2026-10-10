# 内存有界状态：续作交接

更新：2026-10-10 10:30（Asia/Singapore）。下次继续时从本文开始；细节与历史见[进度看板](memory-bounded-convergence-plan.zh-CN.md) §14–§26 和[总设计](memory-bounded-state-design.zh-CN.md)。

## 1. 代码在哪里

- 分支 `memory-bounded-state`，已推送到 origin（`dca5324d`，门禁第 6 轮通过）；之后的提交未推送。基于 `main` 的 `2425ae8e`。
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
| `3ea5e334` | usage collector 测试改为确定性断言（原 69% 失败） |
| `6663b8a2` | 双角色三代生命周期（两次重启）真实 Linux 接受测试 |
| `9feaf834` | 修复：物理 slot 控制 claim 不再借用调用方 workspace（失败启动后无法重启的根因之一） |
| `085929c8` | 失败启动在子进程中执行、下一个进程回放的测试（看板 §19） |
| `f06af594` | budget/ledger/usage 与 app 一样分片；CI/发布 matrix 由契约测试对齐脚本清单 |
| `2e92187e` | Audit/Governance 批编码器费用测试只在非 race 构建运行 |
| `af7f2112` | `persistent_inventory_disk_bytes` 下限 = 派生磁盘上限之和 × 2；原 2 GiB 预算改为 3586 MiB（看板 §20） |
| `0ee44582` | 修复：满压力下终结关闭的截面读取改用已付费通道 |
| `fc537e71` | caches ENOSPC 测试改为借款结束后再关闭（看板 §21） |
| `a7bef390` | 三节点满压力终结与重启的真实 Linux 接受测试 |
| `72c1bd8d`（原分支 `memory-bounded-tls-close` 的 `75fc4012`） | 对端重置时的 close_notify 不再报关闭失败（看板 §22） |
| `4363a46c` | 修复：Replica 的 credit 观察走已付费 catch-up 通道，满压力下 reserve 不再断开复制会话（看板 §23） |
| `1f23dca5` | 真实 ENOSPC 下拒绝新写入、双角色终结关闭与重启接受测试（看板 §23） |
| `ee252ff0` | 修复：credit 回复丢失时在新会话上原样重发（30 秒上限），被放弃的 ticket 由后续操作完成（看板 §24） |
| `a4fa6384` | 修复：三节点下已记录未发出的镜像先发送再释放，避免与 Replica 序号分叉（看板 §24） |
| `f62769f1` | 两节点与三节点回复丢失测试（看板 §24） |
| `d237342c`（分支 `memory-bounded-busy-head` 的 `deb9f257`） | 修复：维护 head 校验只因锁忙失败时不再锁定 accounting recovery，writer 校验退避重试至多 30 秒（看板 §25，门禁第 3 轮失败的根因） |
| `dcb6746a`（分支 `memory-bounded-usage-pack` 的 `f676433d`…`5621ee6d`） | projection 页包准入背压：无空间时整笔拒绝而非中毒、归约干净停止、按用量触发 checkpoint、网关 503 削减新业务（看板 §26） |
| `3db39877` | 修复：`PackUsage` 不再取 store 锁，避免准入轮询让 Ledger append 内的 Apply 锁忙失败（看板 §26，合入后 race 补跑发现） |
| `9c88ee24` | 测试：损坏状态检查改为损坏所选 revision 对应的缓存槽位（看板 §26，门禁第 5 轮失败的原因） |
| `8fa01a1c` | 三节点在真实 ENOSPC 下拒绝新写入、终结关闭与重启的 Linux 接受测试（看板 §27） |
| `6d8cfb8d` | 修复：parquet 写入失败时丢下的列缓冲临时文件由缓冲池回收；归档多文件发布的真实 ENOSPC 测试（看板 §28） |
| `b4747a2b` | HA 激活迁移在六个阶段的真实 ENOSPC 与恢复测试（看板 §29） |
| `e3066371` | Standalone 迁移激活加阶段钩子；11 个注入点的真实 ENOSPC 与恢复测试（看板 §30） |
| `d8a9131a` | HA 储备保护：采样延迟窗口内的真实 ENOSPC，新准入物理拒绝、已接受终结用储备完成（看板 §31） |
| `f0cf8ac5`…`574ab8f4` | 固定容量配置下的持续压力实验：预登记门槛、v1/v2 失败记录、v3 连续两次通过（看板 §32） |

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
- 完整双角色生命周期（启动、请求、checkpoint、两次重启、Close）已在 1259 MiB 下真实接受，峰值 214 MiB（看板 §19）。三节点（1 Primary + 2 Replica）满压力终结与重启已真实通过（看板 §22），需 6 GiB 镜像：`docs/verification/performance/2026-10-09-three-member/runner-6144.py`。
- 公式中其余项（accounting 128、projection 192、Metadata 映射 96 与会话的真实重叠）未再缩减，需要更大余量时再逐项论证。注意：`WithRecoveryScope` 下的 IO/Background 申请会转记到 Recovery，静态调用图只能证明“某所有者在服务进程中完全不存在”，重叠关系需运行时证据。

### 3.2 测试时限：race 分片未实测

- `internal/app` 空闲单跑 **1924.5 秒**（其中约 9 分钟与一次误跑的分片重叠，真实值略低）；1173 个测试，长尾为主，单靠缩矩阵不够，所以改为分片。
- 普通测试 4 片：按旧逐测试耗时估算最重一片约 600 秒；第 0 片实测 521.7 秒（有并发负载）通过。
- race 最重分片（第 2 片）实测 **550.6 秒通过**（墙钟 9 分 35 秒，有并发负载），远低于 20 分钟，片数不需调整。
- `budget` 包单跑 412 秒（有后台负载）；`ledger` 单跑 361 秒。其余包未单独复测。
- 完整门禁 `make full-check` 已在 `72c1bd8d`（8716 秒）与 `c640bd40`（8752 秒，第 4 轮）上通过，见 §4-1。

### 3.3 其他已登记开放项

1. 已关闭：`persistent_inventory_disk_bytes` 下限由配置推导（看板 §20）。
2. 已关闭：两个 ENOSPC 失败（一个产品缺陷、一个过时测试，看板 §21）。
3. 请求退回共享池时，HA 终结在持有工作区期间等待 `WaitConfirmed`，会串行化终结确认；仅在私有池放不下时发生。
4. Audit/Governance 批编码器并入共享池（路线 A）未做。
5. I-06 角色 generation 切换批次：在线切换路径未继续。
6. 已关闭：会话断开导致 ticket 结果未知的问题，按用户裁定采用原样重发（看板 §24）。剩余：管理审计批次所有者在结果未知时仍被标记退休。
7. 单机压缩测试在 `Physical` 批次中偶发失败（修复后 1/7、未修复 0/4、单独 10/10；§24 最终版本的批次中未再出现），`context canceled` 原因链未归因（看板 §23-2）；门禁第 3 轮的锁忙失败是另一现象，已由 §25 修复。
8. 已关闭：突发负载写满 Usage/Ledger projection 页包（看板 §26）。
6. 原 §3.3-1/§3.3-2（失败启动清理、同一 ext4 残留）已结论为 fail-closed 设计与测试隔离问题（看板 §19）。

## 4. 下次建议顺序

1. 门禁记录：`72c1bd8d` 上通过（8716 秒）；`21df8609` 上第 2 轮因诊断代码混入工作树中止；第 3 轮 race 分片失败（锁忙，看板 §25）；`c640bd40` 上第 4 轮于 2026-10-10 00:51 启动，8752 秒，`exit=0`，无 `FAIL`/`panic`。随后合入 `memory-bounded-busy-head`（`d237342c`）与 `memory-bounded-usage-pack`（`dcb6746a`），针对性回归（新增 app 测试及其 race、ledger/authenticatedstate race、authenticatedstate/usage/ledger 整包）发现并修复 `PackUsage` 锁忙缺陷（`3db39877`），复跑受影响测试后在 `88bff47f` 上起第 5 轮门禁（10:50），39 分钟后在 app 第 4 分片失败，原因是测试写死缓存槽位（看板 §26，`9c88ee24` 修复）。第 6 轮门禁在 `dca5324d` 上于 2026-10-10 11:37 启动，9326 秒，`exit=0`，无 `FAIL`/`panic`；该提交已按用户确认推送到 origin。日志在被忽略的 `docs/verification/performance/2026-10-10-final-gate-6/full-check.log`，判读方法：末尾 `exit=0` 为通过；`grep -E '^(FAIL|--- FAIL|panic)'` 查失败；末尾是 `gate process ended without exit line` 表示进程被中断，需要重跑。门禁运行期间不得改动主工作区。之后的新提交推送前仍需完整门禁与用户确认。ENOSPC 测试需小镜像：`docs/verification/performance/2026-10-09-enospc-regression/` 下的 `runner-512.py` / `runner-1536.py`。
2. I-03 已补真实 ENOSPC 下的双端终结与重启（§23）、回复丢失的有界解决（§24）、三节点真实 ENOSPC（§27）、归档多文件发布 ENOSPC（§28）、HA 激活迁移 ENOSPC（§29）、Standalone 迁移 ENOSPC（§30）、HA 储备保护（§31）与固定容量配置下的持续压力（§32）；I-03 整项仍开放：历史规模（D-04/P2 阻断）与 HA 物理储备下的持续压力、动态输出盘、所有权移交与有限等待的原有缺口；另登记崩溃后 `.archive-*.tmp` 无人回收（§28）。
3. 之后继续 S2 逐项接受：I-03 → I-06 → I-04/I-05 HA 部分 → I-11 → I-12 → I-07～I-10 → O-01/O-02 → V-01 → V-02；十万冷构建在 M3。

## 5. 常用命令

```bash
# 分片自检与运行
scripts/go-test-shard.sh check-all
scripts/go-test-shard.sh rest -count=1 -timeout=20m
scripts/go-test-shard.sh shard ./internal/ledger 0 2 -count=1 -timeout=20m
scripts/go-test-shard.sh all -count=1 -timeout=20m   # make test 的全部内容
# 完整矩阵（发布压力门禁同款）
HALRO_STRESS=1 go test ./internal/budget ./internal/app -run 'TestLegacyRecovery|ActualSIGKILL|SIGKILLResumes|PendingPublicationResumes|TestHAMemoryActivationActualSIGKILLResume' -count=1 -timeout=60m
# 预检向量与真实 Manager 对账
go test ./internal/config -run TestHAPhysical -count=1 -v
# 真实 Linux 双角色（需要 Docker；runner 位于被忽略的本地证据目录）
HALRO_HA_RECOVERY_RESERVE_MIB=1259 HALRO_RUN_PATTERN='^(TestHAPhysicalPeerCreditActualTLSPreacceptAndUnusedRelease|TestHAPhysicalOnlineCheckpointKeepsActualServicesAndTLS)$' \
  python3 docs/verification/performance/2026-10-09-recovery-offline-workspaces/runner.py ha-preflight <linux-arm64 suite.test> <新输出目录> 0
```

Linux 测试二进制：`CGO_ENABLED=0 GOOS=linux GOARCH=arm64 go test -c -o <path>/suite.test ./internal/app`。runner 只在本机的被忽略目录里；换机器需要从 `docs/verification/performance/2026-10-08-budget-feasibility/sharing-order/runner.py` 复制并加上 `HALRO_HA_RECOVERY_RESERVE_MIB` 与 `HALRO_RUN_PATTERN` 透传。
