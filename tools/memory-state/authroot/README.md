# D-03 认证有序页隔离原型

本包验证页编码、内容认证、点查/不存在、完整范围分页、规范空根、隔离磁盘 COW/根发布、流式构建和有界原子批次。没有导出 API；生产 runtime 不依赖本包。原型 root envelope **不是生产 Seal**，没有 instance/cluster/role、日志位置、metadata baseline、密钥生命周期或发布资格。

使用 SHA256 验证页、HMAC-SHA256 认证原型 root；所有密钥都是测试构造数据。PageRef 只含 pack ID、offset、length、digest；大小/偏移先验证，再读入固定 4 KiB 缓冲并验摘要，然后解码。leaf/branch 严格排序、边界不重叠、数量无溢出；子引用和根认证准确高度，访问 child 时验证实际首末键、数量和高度。不存在只由认证分支界限/叶内容证明；缺失页不等于空树。

规范空根使用组件特定空承诺，count=0 与唯一 ref 绑定；点查/分页不读取物理页即可返回确定空。范围查询沿认证树遍历，不依赖普通 DB Cursor；发生错误时丢弃整批结果，不返回部分业务答案。原型续读键只可用于同一 root，尚未实现生产 cursor 认证/期限/pin 协议。

硬限（实验格式，不是生产预算）：页 4 KiB、每页 8 条、key 64 B、value 256 B、路径深度 16、每次查询最多 1,024 页、范围结果最多 32 行及 8 KiB key/value 字节。输出字节不含 HTTP/JSON 编码放大。pageSource 必须按受限缓冲读取并在返回前释放物理事务；测试使用小型内存 fixture 与独立 append-only 磁盘 pack，**不能证明大历史内存、存储或 cgroup 容量有界**。

```bash
GOCACHE=/tmp/halro-go-cache go test -count=1 -v ./tools/memory-state/authroot
GOCACHE=/tmp/halro-go-cache GOMAXPROCS=2 go test -count=1 ./tools/memory-state/authroot -run '^$' -fuzz '^FuzzPageDecoder$' -fuzztime=5s -parallel=2
```

[原始验证记录](../../../docs/verification/performance/2026-10-02-memory-state-authroot/README.md) 与 [候选协议](../../../docs/architecture/memory-state-authenticated-root-protocol.zh-CN.md)。独立账务/内容审阅发现的空根 P2 已修复并复核闭合。

磁盘实验采用独立目录、进程独占锁、单 pack byte ceiling、COW 路径更新与 root 原子发布。新页先 sync，root 临时文件 sync/rename 后 directory sync，成功后才可见。错误后 poison，重开不提升未发布 append tail；保留旧 root 内容，当前没有 GC。page codec 为 2、实验 root envelope 已升为 HMPROOT3，绑定 KV mutation 的合成 sequence/hash；它不是原生事实 cursor。旧实验格式不兼容；此变更不用于任何生产数据。

[磁盘实验原始证据](../../../docs/verification/performance/2026-10-02-memory-state-authroot-disk/README.md)：完整包/race/Linux 测试、6 阶段进程 SIGKILL、32 轮非空缩根再分裂通过，HA/容量独立源码复核通过。没有断电证明或生产容量承诺。

新增流式 bulkBuild（单 leaf＋每层固定 child 组）和 32 行/8 KiB applyBatch；私有中间根一次发布内容与合成位置，no-op 也推进位置。输入错误或第二批中途 quota 失败不改变先前提交内容。源流验证/事实耐久性仍由生产协议负责。

[大历史与批次证据](../../../docs/verification/performance/2026-10-02-memory-state-authroot-build/README.md)：10k/100k/1m/10m 样本完整逐行验证通过，但未启用 cache advice 的 10m cgroup 达到 256 MiB 上限；memory.stat 指向文件缓存压力。Go heap 较小不代表容量通过。

[FD 缓存策略对照](../../../docs/verification/performance/2026-10-02-memory-state-authroot-cache/README.md)：仅 opt-in Linux 实验启用 RANDOM＋周期 Sync＋DONTNEED，写侧提示新增已同步区间，读侧固定 16 槽记录实际访问窗口；每次提示至多 quantum。第二轮 10m 样本 cgroup peak 从 256MiB 降到 18.613MiB，无 max/OOM，内容/合成链/重开/批次全部通过；完整扫描从 10.267s 增至 11.096s。第一轮仍触顶的证据保留。advice 不保证回收，参数不是生产预算；范围槽有限也不保证 I/O 等待时长。提示失败不提前发布或返回假空，关闭等待 FD advice，避免 FD 复用。

未实现：生产日志事实的批量 Apply/确认接入、生产可信 publisher/Seal、metadata 首次基线/整文件发布、角色仲裁、原生日志增量初始化、断电/目标文件系统耐久性、共享预算、生产缓存/GC/根 pin、完整磁盘预算与域状态的百万历史容量接受。单 pack ceiling 不含 orphan staging 文件/目录项或锁/根；共享预算与清理须另验。root 签名 helper 只验证格式和 MAC，不对不可信生产者生成的业务状态授予真实性；Merkle 承诺也不证明生产者计算无误。D-03/I-11 均未完成，不接入 runtime。

2026-10-04 增加独立的私有 `HMPSEAL1` 编码实验，已有 `HMPROOT3` pack/root 格式不变。固定1802B envelope 将五类组件根、四类 native cut、身份/版本、Metadata baseline 和 ordering/roll 承诺绑定；reader 校验独立输入的身份和支持 schema。测试覆盖规范编码、正确MAC下的无效字段/重复组件、真实Ledger cut与缩小余额页、后续事实回放以及实际Vault重包后的稳定域分离key。其它组件/基线/locator仍为合成数据，不能据此声明完整生产关联、全组件覆盖、整文件轮换安全或可信增量启动；详见[Seal实验记录](../../../docs/verification/performance/2026-10-04-state-seal/README.md)。
