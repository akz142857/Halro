# 阶段 3 · 实机验证（真实二进制、真实数据目录）

按 `review-plan.md` §8 执行。全部在一次性目录
（scratchpad 下的 `rt*`，绝对路径配置，端口 18080/18081/19090 与 18090/18091/19099），
**未触碰仓库的 `data/`、`master.key` 与 `config.yaml`**。

两个二进制：

| 名字 | 来源 | sha256 前 16 |
|---|---|---|
| `halro-v040` | `v0.4.0` worktree（`2501668`）`go build ./cmd/halro` | `014cef9afd5fe434` |
| `halro-head` | 仓库 HEAD（`cd37927`）`go build ./cmd/halro` | `33908577666ecbba` |

## R0 基线数据目录是怎么造出来的

`release-assessment.md` §1d 要求"用**上一个已发布版本**写出的数据目录"。本轮没有现成的
v0.4.0 目录，所以按 260827 的做法当场造一个，**全程只用 v0.4.0 自己的代码**：

1. `halro-v040 init` → `Halro initialized`
2. `halro-v040 bootstrap`（credential→provider→deployment→route→project→key 一次建全）
3. 在 **v0.4.0 worktree 内**写一个一次性的 `TestGenerateUsageFixture`，用 **v0.4.0 自己的
   `internal/ledger` 写者**（含 v0.4.0 的链 HMAC 密钥）向 WAL 追加
   `accepted → reservation → started → settled → finalized` 的完整生命周期：
   3 个账期日 × 8 个请求 = **120 个事件**，跨 2 个 project / 2 个 provider /
   2 个 deployment / 2 个 requested_model / 2 个 provider_model，含成功与失败。
   （这个 fixture 文件只存在于 scratchpad 的 worktree，仓库无改动。）
4. `halro-v040 start` 20 秒（`checkpoint_interval: 5s`）→ v0.4.0 自己重放 WAL 并写出
   它自己的 usage checkpoint。
5. `halro-v040 doctor` → **healthy=true，`metadata pass: bbolt schema v32`**

第一次尝试时 fixture 漏了 `EventReservationCreated`，v0.4.0 启动直接拒绝：
`replay ledger: attempt "…:1" has no accounting lease`。**这条拒绝本身是 B1 的正面证据**
——没有预留的结算进不了重放，并且是 fail-closed 到"拒绝启动"。补上预留事件后通过。

## R1 全量门禁

`make check` **exit 0**：fmt、`go test ./...`、`go test -race ./...`、`go vet`、前端 vitest、
observability 配置校验，全部通过。

`make frontend` 之后 `git diff --exit-code -- internal/webui/dist` **无漂移**——嵌入包与
`web/src` 一致。

## R2 原地升级 32 → 33（本轮最重要的一条）

| 步骤 | 命令 | 结果 |
|---|---|---|
| 升级前 doctor（HEAD 二进制、未迁移目录） | `halro-head doctor` | **healthy=false**，`metadata fail: metadata schema version 32 does not match required version 33` |
| HEAD 启动 | `halro-head start` | 正常启动并服务，退出码 0 |
| 派生物处置 | 启动日志 | **`WARN usage derivatives rebuilt from the ledger reason="usage checkpoint rejected: usage checkpoint version 7 is not supported"`** |
| 迁移结果 | 离线读 bbolt | `schema_version = 33`，`usage_rollup_state = {"version":1,"watermark":{"generation":1,"offset":74097,"sequence":120}}`，`usage_daily_rollup` **33 行** |

**结论 1（判定 A4 的预测，成立）**：真实的拒绝点是 **checkpoint 版本 7→8**，
不是 `usage rollup state is missing`。后者在这条升级路径上永远不会出现——它在第 3 个返回点
就被短路了。发布记录与升级文档若写"会看到 rollup state is missing"，运维对不上日志。

**结论 2（B8 的实证，本轮直接测的那条）**：升级后立刻 dump `usage_daily_rollup` 全部 33 行
（含 `first_sequence`、两组直方图、period 元信息），再跑 `halro usage rebuild-summary`
（全量重建路径）后再 dump 一次，**逐行逐字节相等**：

```
$ halro-head usage rebuild-summary --config …
{"watermark":{"generation":1,"offset":74097,"sequence":120},"rollup_version":1,"rollup_rows":33,"accounting_days":3}
$ diff rollup-incremental.txt rollup-fullrebuild.txt   →  无差异
```

注意这一条覆盖的是"重放一次性建成 vs 离线全量重建"。**多批增量与一次性重建的等价性由
A1 用代码级实验覆盖**（batch = 0/7/41/199/1000，237 个 `provider_model` 越过 200 上限，
`reflect.DeepEqual` 逐行比对，四种分批与一次性完全相等），此处不重复。

### R2-a 升级路径上两个运维陷阱，两个都实测复现

方案 §8 没有预设这两条；它们是 A4 的推断，本阶段拿真二进制判定，**两条都成立**。

**陷阱一：`halro ledger verify` 会静默把数据目录单向迁移。**

```
schema BEFORE:            32
$ halro-head ledger verify --config …
{"Authenticated":120,…,"ChainVerified":true}
schema AFTER ledger verify: 33
```

一个名字读起来完全只读的命令，副作用是让 v0.4.0 二进制再也打不开这个目录。运维把它当
"升级前体检"用，是很自然的动作。

**陷阱二：用 HEAD 二进制对 v0.4.0 目录做"升级前备份"——目录先被迁移，然后备份失败。**

```
schema BEFORE: 32
$ halro-head backup create --config … --output … --key-file …
halro: usage checkpoint payload does not match its watermark
schema AFTER backup create: 33
```

重跑仍然失败（checkpoint 还是 v7）。此时：

```
$ halro-v040 backup create …   → metadata schema version 33 is newer than this build supports (32)
$ halro-v040 start …           → 同上
```

**即：这个目录已经没有任何一个二进制能给它做备份。** 唯一出路是先用 HEAD `start` 一次，
让派生物重建，之后备份立刻成功（已验证：同一份目录跑过 HEAD start 之后
`backup create` 产出完整 manifest，`schema_version: 33`，8 个文件各自 sha256）。

根因在 `internal/app/backup.go:594-603`：

```go
checkpointAggregate, restoreErr := usage.RestoreCheckpoint(checkpointPayload)
if restoreErr != nil || checkpointAggregate.Snapshot().Watermark != checkpoint {
    return backup.Manifest{}, errors.New("usage checkpoint payload does not match its watermark")
}
```

`restoreErr != nil`（**陈旧但合法的版本 7**）和"payload 与水位不符"（**损坏**）被合成同一条
错误。启动路径对同一状态的判断是"重建"（`runtime.go:834`），备份路径的判断是"致命"。
两条路径对同一份磁盘状态给出相反的结论，且报错文本把陈旧说成了损坏。

## R3 全新数据目录（HEAD）

| 步骤 | 结果 |
|---|---|
| `halro-head init` | `Halro initialized` |
| `halro-head bootstrap --provider-type minimax` | 六个 ID + 一次性 `gw_` key，**MiniMax 连接可以从 CLI 建出来** |
| `halro-head start` | 正常启动并服务 |
| `halro-head doctor` | **healthy=true，23 项检查**；两条非 pass：`ledger unverified`（零帧）、`provider_connectivity warn`（离线 doctor 跳过网络探测） |
| `halro-head ledger verify` | 零帧实例上以 **`ledger chain could not be authenticated`** 退出 |
| `backup create` | 成功，`schema_version: 33`，5 个文件 |
| `backup verify` | 通过 |
| `backup restore`（另一个 scratch 目录） | `Restore complete; previous data directory was preserved for rollback.`，旧目录保留为 `.halro-pre-restore-*` |
| 从恢复出的目录启动 + doctor | 启动无 error，**healthy=true** |

**顺带记两条：**

1. **`ledger verify` 在零帧实例上把"没有"说成"不能认证"**——v0.3.0 评估记过、260827 复核过、
   本轮在 HEAD 上第三次观测到同一条措辞。不是回归，是一直没修。
2. **`bootstrap` 的 `--provider-type` 帮助文本已过时**：`cmd/halro/main.go:315` 写的是
   "openai, azure_openai, deepseek, openai_compatible, gemini, or bedrock"，
   而 `minimax`（本轮新增）和 `anthropic` 都能被接受。这正是 `provider_table.go:557` 那段注释
   讲的"第三份类型清单"的另一份——只是这一份在帮助文本里，没有任何测试盯着它。

## R4 抽样基准（HEAD 与 v0.4.0 背靠背，同主机同 Go）

方案 §8.4 只要求两组：Ledger append/replay 与路由解析。**关键是背靠背**——
`CLAUDE.md` 要求"把两边都构建出来，跑同一个输入"，而不是拿今天的数字去比一份 2026-07-31 的表。

`-benchtime=1s`，路由 `-count=5`、WAL `-count=3`：

| 基准 | HEAD | v0.4.0 | 判定 |
|---|---|---|---|
| `BenchmarkRegistryResolveCandidates` | 5053 / 5045 / 5115 / 5138 / 5107 ns/op | 5017 / 5050 / 5057 / 5093 / 5065 ns/op | **无回归**（中位数 +0.9%，落在噪声内） |
| 同上，分配 | 15,888 B/op，41 allocs | 15,888 B/op，41 allocs | **逐字节相同** |
| `BenchmarkReplayLargeWAL` | 505.2 / 506.6 / 505.6 ms/op | 507.5 / 508.3 / 518.1 ms/op | **无回归**（HEAD 略快） |
| 同上，分配 | ~230.78 MB/op，3.000M allocs | ~230.78 MB/op，3.000M allocs | 相同 |

**本轮范围内没有性能回归**，与 §3 触发表第四行"热路径未动"的判断一致。

**但发布记录要记一条既有漂移**：`docs/verification/performance-baseline.md`（2026-07-31）
对路由解析记的是 **405 ns/op、2,880 B/op、1 alloc**，而**今天的 v0.4.0 与 HEAD 两侧都是
约 5,050 ns/op、15,888 B/op、41 allocs**。差 12 倍。这个漂移**早于 v0.4.0**，不是本轮引入的
——260827 的实机记录已经点过"performance-baseline.md 对路由解析一项已知过时"，本轮在
两个版本上同时复核，确认它仍然过时。WAL replay 一项同理：表里是 401 ms / 57.07 MB/s /
173 MB/op / 2.4M allocs，两侧实测都是约 506 ms / 61 MB/s / 231 MB/op / 3.0M allocs
（吞吐反而更高，说明记录变大了）。

**这份基线表已经不能用作回归判据。** 要么在发布时按当前主机重新测一版，要么在表里注明
它对应的是哪个 commit——否则下一轮评审会把一个两个版本都有的旧漂移误报成新回归。

## R5 MiniMax 真账号冒烟：不重跑

按方案 §8.5，复核 #250 留下的证据而不是重新计费。复核结论见 `range-map.md` 表 4 与
`260901.md` 的 A6 部分。此处只记一条与本阶段有关的：**`docs/verification/provider-real-matrix.md`
自己写明这两轮"carried no `-commit` binding and produced no archived evidence file"**
——也就是说 MiniMax 的实测结论目前只存在于那份 Markdown 的叙述里，没有可复核的证据文件。
Responses profile 与大陆主机 `api.minimaxi.com` 两项，记为**未实测**。
