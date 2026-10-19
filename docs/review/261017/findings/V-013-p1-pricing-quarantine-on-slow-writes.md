# V-013 — P1：元数据写入变慢就会把 deployment 的计价永久隔离，之后所有请求 503

- 基线：**v0.8.5 已存在**（生产版本），冻结 `main` `2e749db1` 与 `17972e41` 相同
- 段/功能：A 段之前即有 / 计价选择（`internal/store/bolt/store_pricing.go` `PrepareDeploymentPricePin`，ADR 0012 2026-08-07 修订）
- 分类：【问题】，真实二进制现象（E05b）+ 确定性单元复现
- 发现方式：E05b 8 小时长跑第 7 小时
- 状态：修复 PR #434（`fix/pricing-clock-drift`）；E05d 为真实二进制确认

## 现象（E05b，真实二进制）

前 7 小时全部 200。23:33:00Z 起每个请求都返回 503 `accounting_unavailable`（日志 `outcome=accounting_error`，attempts 0），直到结束：普通请求 562,134 个、治理请求 30,455 个。重启后 `/health/ready` 一直不通过（脚本等满 1200 s 放弃）。`ledger/usage/audit verify` 均 exit 0。

从长跑结束后的 `halro.db` 读出唯一 deployment 的高水位记录：

```
"latest_selected_at":"2026-10-18T23:33:00.944198504Z","quarantined":true,"quarantine_reason":"wall_clock_rollback","revision":4016864
```

主机 `pmset` 在此时段没有睡眠或唤醒，时钟没有跳变。故障前几分钟吞吐已从约 8.9k 降到 4.5k 请求/分钟（21 GB 用量历史与 Ledger 压缩同盘），首个失败请求延迟 2,058 ms。

## 原因

请求在取价时记下 `pricing_selected_at`，然后在 `db.Batch` 里等待写入。回退检查拿它与**已提交的最新**选择时间比较：落后超过 `pricing_clock_rollback_tolerance`（2 s）即判为墙上时钟回退并隔离。从取时间到提交 pin 的间隔包含等待 `db.Batch` 的时间，没有任何上界；磁盘压力下一个选择在更新的选择之后 2 s 以上才提交，就被当成了时钟回退。隔离是持久的，此后该 deployment 的每个请求都失败。

前跳检查有镜像缺陷：比较用的 `observedAt` 在事务**内**读取，写入停顿超过 30 s 时，停顿后的第一个新选择会被判为时钟前跳。

ADR 0012 选用 1 s 容差下限的前提是"取时间到提交的间隔小于下限"；E05b 证明这个前提不成立。

## 确定性复现

- `TestDeploymentPricePinReorderedCommitIsNotARollback`：两个按顺序取的选择相隔 300 ms、倒序提交，容差 200 ms——main 上 3/3 失败（隔离）；
- `TestDeploymentPricePinWriteStallIsNotAForwardJump`：持写锁 500 ms，前跳容差 300 ms——main 上 3/3 失败。

## 影响

生产（v0.8.5）中任何一次超过 2 s 的元数据写入停顿——磁盘抖动、同盘备份、主机过载——都可能让 deployment 永久停止服务，且因 V-014 无法在产品内恢复。账务不受影响（请求在上游调用前被拒）。

## 修复（#434）

网关在取 `pricing_selected_at` 的同一时刻记 `time.Now()`（`UTC()` 会去掉单调时钟读数），存储按"墙上时钟相对单调时钟的漂移"判断跳变：`drift = (selectedAt − anchor.SelectedAt) − (observedAt − anchor.ObservedAt)`。等待与提交顺序在两边同时出现，互相抵消。没有内存锚点时（启动后的第一批选择）仍用持久高水位判断，所以停机期间的时钟回退仍会被发现。四个变异体各被一个测试捕获；ADR 0012 已追加修订。
