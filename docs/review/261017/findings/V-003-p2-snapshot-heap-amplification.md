# V-003 — P2：状态快照的瞬时堆约为热状态的 6 倍，热状态上限不能约束进程堆

- 基线：冻结 `main` `2e749db1`
- 段/功能：D 段 M02（#421）× M05（#423）；交叉点 X18
- 分类：【问题】，已测量

## 现象

`ledger.max_hot_state_bytes`（默认 128 MiB）被描述为"超过即拒绝新工作，而不是让进程增长到内存耗尽"。但快照写入器每分钟（状态有变化时，`internal/app/ledger_state.go:29,333`）对整个状态做一次全量复制和 JSON 编码：

- `marshalSnapshotWithQueue`（`internal/ledger/state_snapshot.go:123` 起）在读锁下把每个 map 复制成切片，再整体 `json.Marshal`；
- `WriteStateSnapshot` 写入时 `append(encoded, MAC...)`（`:358`）又复制一次编码结果。

## 测量

一次性测试（冻结 SHA 的 scratch worktree，未提交）：在 `State` 中放入 20,000 个计价预留（与 `TestHotStateEstimateTracksTheHeap` 相同的事件形状），然后调用 `MarshalSnapshot` 并以 2 ms 间隔采样 `HeapAlloc`：

| 量 | 值 |
| --- | --- |
| 状态实际堆 | 60.4 MiB |
| `HotStateBytes` 估算 | 70.6 MiB |
| 一次快照累计分配 | 394.2 MiB |
| 编码结果 | 97.1 MiB |
| 快照期间峰值 `HeapAlloc` | **380.7 MiB（状态堆的 6.3 倍）** |

按此比例，状态停在 128 MiB 上限附近时，每次快照会把堆推到约 700–800 MiB。

## 影响

上限仍然有界（约为配置值的 6 倍），所以不是无界增长；但 128 MiB 这个数字不能当作进程内存预算，按它为容器设内存限制会在快照时 OOM。文档与 CHANGELOG 都把它表述为防止内存耗尽的界。

## 建议

流式编码快照到文件（不先构造完整切片副本和完整字节切片），或至少把快照的倍数计入估算与文档；在 E06 那样的上限压测中记录快照时的 RSS。
