# Halro 启动内存与 cgroup OOM 取证

137 表示 SIGKILL；与同一任务的内核 cgroup OOM 日志或 OOMKilled
字段关联后才确认 OOM。调高限制后 Running 只说明当前任务启动成功。
记录镜像 digest、限制、任务 ID、数据规模和连续内存曲线后判断稳定性。
`docker service update --limit-memory 2G` 是临时措施；随后部署 stack 时，
stack YAML 的旧限制可能覆盖它，需要同步部署源配置。

## 无需更新二进制的现场证据

在运行任务所在主机执行，CID 应只匹配一个 Running 任务：

```bash
CID=$(docker ps -q --filter label=com.docker.swarm.service.name=halro-prod_halro)
docker inspect "$CID" --format '{{json .State}}'
docker stats --no-stream "$CID"
PID=$(docker inspect "$CID" --format '{{.State.Pid}}')
cat /proc/"$PID"/status
cat /proc/"$PID"/smaps_rollup
cat /proc/"$PID"/cgroup
docker exec "$CID" sh -c 'cat /sys/fs/cgroup/memory.current /sys/fs/cgroup/memory.max /sys/fs/cgroup/memory.events /sys/fs/cgroup/memory.stat'
```

最后一条适用于镜像带 sh 且 cgroup v2 正常挂载的容器。否则根据
`/proc/$PID/cgroup` 和主机 mountinfo 定位对应 cgroup，读取同名文件。
v1 使用 memory.usage_in_bytes、memory.limit_in_bytes、memory.failcnt、
memory.oom_control 和 memory.stat。cgroup 使用量还包括文件缓存，不能
直接当成进程 RSS；smaps_rollup 可区分匿名与文件映射页。PID 需确认是
Halro 本身；若 PID 1 是启动包装器，改读实际 Halro PID。

## 可执行的只读采集器

[collect_live_memory.py](../tools/memory-state/collect_live_memory.py)在**运行任务所在的Linux Docker主机**执行，要求能读取Docker状态、目标/proc与cgroup；可选profile另需nsenter权限和curl。脚本不更新服务、不重启、不触发模型请求或主动GC。macOS Docker Desktop主机不能代替真实Linux/proc。

```bash
sudo python3 collect_live_memory.py \
  --service halro-prod_halro \
  --output /var/tmp/halro-memory-20261004 \
  --samples 6 --interval 5
```

`--service`必须只匹配一个本机Running任务；多任务时用已核实的`--container <完整或至少12位容器ID>`明确选择。若容器init是包装器，脚本拒绝把包装器RSS当成Halro，需另给实际Halro的**宿主机**`--pid`；该PID必须属于同一cgroup、comm为halro。选定容器/task/image、PID start ticks、可执行文件inode/sha256在曲线中保持一致；任务替换、exec更换或PID复用会留下incomplete报告，不混成一条成功曲线。

脚本从目标/proc/cgroup和主机mountinfo解析v1/v2及挂载子树；记录status、smaps_rollup和原始cgroup用量/上限/events/stat，额外支持peak/swap。输出目录必须不存在，权限0700，文件0600；report.json记录覆盖和缺口，sha256.json核对全部文件。不会采集Docker Env、命令参数、挂载清单或业务日志。

只有当前二进制已经有诊断入口时加`--pprof-port 6060`。采集器确认127.0.0.1监听socket归属该Halro PID，再在容器net namespace读取runtime/heap/allocs，单profile最多64MiB、单命令有超时和输出上限。runtime校验MemStats形状，压缩profile流式核对完整gzip/CRC并限制解压量；protobuf和符号仍需下述Go pprof分析。端口不属于Halro、入口不存在或损坏内容会标记profile缺失，不把HTTP200等同于有效profile。没有自动部署诊断镜像或修改pprof配置。

`complete=true`只表示选定当前任务的proc/cgroup曲线完成；没有请求或完整取得profile时`profile_complete=false`。它不证明更早任务的137由OOM造成，也不证明数据实体规模、完整容量门槛或生产接受；须结合同任务内核日志/历史任务状态、相同镜像二进制及数据规模另行核对。原始文件保存在受控现场目录，不能直接作为公开仓库交付。

采集器本地验证见[记录](verification/performance/2026-10-04-live-memory-collector/README.md)。

## 新增的可选诊断入口

本次源码新增 `HALRO_PPROF_LISTEN=127.0.0.1:6060`。
只有包含此改动的新二进制支持；仅给旧镜像设置环境变量不会增加入口。
默认关闭，只接受字面量 loopback IP 和非零端口，绑定失败会阻止启动。
监听在主数据恢复之前打开，因此可以观察启动期，而不依赖 Admin 已就绪。
采用独立 mux；不会把该入口挂到 Gateway/Admin/Metrics 上。
入口随 start 命令生命周期关闭。profile 可能包含敏感运行信息，取证文件
仅保存在受控目录。不要发布 6060；容器 loopback 不等于宿主机 loopback。

在主机通过运行容器的网络命名空间访问（主机需 nsenter 与 curl）：

```bash
umask 077
mkdir -p memory-evidence
nsenter -t "$PID" -n curl --fail --silent --show-error \
  http://127.0.0.1:6060/debug/runtime -o memory-evidence/runtime.json
nsenter -t "$PID" -n curl --fail --silent --show-error \
  http://127.0.0.1:6060/debug/pprof/heap -o memory-evidence/heap.pb.gz
nsenter -t "$PID" -n curl --fail --silent --show-error \
  'http://127.0.0.1:6060/debug/pprof/heap?gc=1' -o memory-evidence/heap-after-gc.pb.gz
nsenter -t "$PID" -n curl --fail --silent --show-error \
  http://127.0.0.1:6060/debug/pprof/allocs -o memory-evidence/allocs.pb.gz
```

分别在启动期、就绪后、首次 Usage 导出前后、持续运行一段时间后采样，
文件名加阶段或时间，避免覆盖。`gc=1` 会主动执行 GC，先抓未经主动 GC
的 profile；紧急临界内存时不要反复强制 GC。

```bash
go tool pprof -top -sample_index=inuse_space ./halro memory-evidence/heap-after-gc.pb.gz
go tool pprof -top -cum -sample_index=inuse_space ./halro memory-evidence/heap-after-gc.pb.gz
go tool pprof -top -sample_index=alloc_space ./halro memory-evidence/allocs.pb.gz
```

使用与镜像相同构建的二进制做符号分析。heap 给出采样分配栈和存活量，
不能直接给出对象引用链或证明是谁持有对象；结合代码确认保留路径。
alloc_space 是累计分配，不能把它当成当前工作集。

## 当前源码的优先验证路径

- `internal/app/runtime.go`：Ledger 全量回放到 State，随后恢复 Usage
  checkpoint 并补回放。缺失或不可用 checkpoint 可能触发完整 Usage 重建。
- `internal/ledger/event.go`：State 持有 eventDigests、settled、leaseRecords
  等历史 map。它们用于幂等和恢复，不能直接清空或随意加 LRU。
- `internal/usage/checkpoint.go`：恢复历史 attempts 与 summaries；
  `internal/usage/aggregate.go` 的 Snapshot 会复制两个完整 slice。
- `internal/usage/parquet.go`：导出又构建 pending、按日期分组以及转换行。
  `runUsageMaintenance` 只在导出后的周期裁剪窗口，首次周期前可能保留较多历史。

这些是源码确认的消费者与放大路径，还不是本次生产事故的定量结论。
不要在没有 profile、幂等/账本恢复契约验证的情况下删除历史索引。

现有 /metrics 增加 Sys、HeapSys、HeapInuse、HeapIdle、HeapReleased、
StackInuse、NextGC；/debug/runtime 返回完整 MemStats 与 goroutine 数。
Go runtime 的 Sys 不是 RSS，Sys - HeapReleased 也不是 cgroup 总使用量。
若 GC 后 HeapAlloc 仍持续上涨，追查 profile 中的长期对象；若 heap
稳定而 RSS/cgroup 高，检查 runtime 保留、文件映射、文件缓存和其他原生内存。

GOMEMLIMIT 是 Go runtime 的软限制，不能约束 mmap、原生分配或所有 cgroup
内存，也不能让超过预算的长期存活对象消失。不要把它当成根因修复。
必要时用 GODEBUG=gctrace=1 配合 profile 观察，诊断完成后关闭。

官方依据：[pprof 文档](https://pkg.go.dev/net/http/pprof)、
[Go GC guide](https://go.dev/doc/gc-guide)。

## 可选预算下的 failure capture 与关闭等待

`resource_budget` 默认关闭。启用时 `capture_ram_bytes` 是独立 Background
费用：在诊断 JSON 编码前申请，涵盖截断前的编码峰值、截断副本、明文与
密封缓冲；至少覆盖 `request_ram_bytes` 和三个 `max_bytes` 下限，且
`max_owners` 至少为 3。下限校验不能替代目标环境的编码/加密峰值校准。
额度不足只丢弃 best-effort 诊断，保留原有请求结果和账务终结。

取消 capture queue 会清除尾部记录并退账；若密封文件 writer 仍在系统调用中，
其 RAM 费用保留到真正退出。Runtime 等待 Store 的实际 IO 完成后才关闭
vault 和目录锁。`shutdown_timeout` 触发 queue 取消，但不能中断内核中的
普通文件 IO；实际 Store join 可能超过此时限。关闭延迟需同时检查磁盘和
文件系统，不能以 queue 已结束或逻辑额度退还推断写入已结束。

默认 Standalone/Primary 两级账务恢复现在使用同一 Manager 的独立 Recovery workspace：启用共享预算时需显式配置 `accounting_recovery_ram_bytes`，结构下限 128MiB 并实测宽记录峰值。费用在枚举前取得，覆盖有限页、结算编码、native IO 与确认等待，直到整个恢复调用和维护 writer Close/join 完成。取消不提前退账，费用不足不能发布就绪。普通角色至少预留4份 Recovery claim，HA至少11份；同时校验所有 envelope、类别与储备。具体范围和开放项见[契约](contracts/accounting-recovery-resources.md)与[本地证据](verification/performance/2026-10-04-accounting-recovery-resources/README.md)。

默认三角色账务候选构造也使用同额 Recovery workspace，先申请再删除旧派生文件/扫描完整native源。Standalone/Primary的Ledger重建与后续两级恢复串行取得费用；Replica保持费用到共同Ledger/Usage构造或失败清理完成。预算拒绝保留旧派生文件，取消/panic不会提前退账；成功返回只退还临时构造费用，不代表长期DB/cache/mmap已受预算覆盖。应按两个阶段较大峰值校准，详见[构造记录](verification/performance/2026-10-04-accounting-startup-workspace/README.md)。

当前仍未收费的历史 map/cache、固定对象、磁盘与确认窗口，以及生产
恢复/终结 owner，继续按主方案实施。逻辑 RAM 指标不代表 GC 或 OS 已回收。

默认 Standalone/Primary 的 Usage 完整回放、Governance 外部锚校验/构建和 Usage 启动归档窗口核对已串行复用 `accounting_recovery_ram_bytes`。校准取 Ledger/Usage/Governance/窗口/两级恢复中的最大峰值；未增加字段或并行 claim。预算拒绝发生在旧派生文件删除或窗口核对/prune 之前；失败/panic 清理候选后退账，借用的业务 Ledger 和 lease 保留原所有者。见[本地投影启动证据](verification/performance/2026-10-04-projection-startup-workspace/README.md)。原生 Open、exporter 构造、长期 DB/cache/mmap、Metadata 初始化和物理容量仍需独立治理，不能以这份临时费用宣布整体启动有界。
