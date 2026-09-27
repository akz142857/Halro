# 架构与不变量裁决

| 不变量 | 裁决 | 依据 |
| --- | --- | --- |
| INV-01 单一确认写者 | PASS（仓库）/ BLOCKED（真实分区） | term/promise/demotion/freeze 定向测试通过；外部 fencing 与非对称分区未验 |
| INV-02 单一合法 Provider 副作用角色 | **FAIL** | R-001、R-003 |
| INV-03 Ledger 权威与保守恢复 | PARTIAL | reservation/attempt confirmation 与 recovery tests 通过；完整切主时序及重复外部副作用未关闭 |
| INV-04 mutation 不丢、撤权不复活 | **FAIL** | R-001、R-004 |
| INV-05 Replica 是 Primary 持久化前缀 | PASS（仓库） | partial tail、same-index digest、reconstruct、confirmed-only apply tests 通过 |
| INV-06 metadata journal 完整决定复制投影 | PARTIAL | 写入口与分类门禁通过；confirmation 策略遗漏 R-001/R-004 |
| INV-07 授权不被新路径绕过 | **FAIL** | R-002；Admin route clear 自身 RBAC/MFA/CSRF 正确 |
| INV-08 最小泄露 | PASS（审查范围） | 未发现正文、credential 或跨 Project alias 进入响应/指标；真实黑盒外部环境未验 |
| INV-09 资源与基数有界 | PARTIAL | 固定容量与封闭 labels 基本成立；R-014、长稳和生产基数未关闭 |
| INV-10 版本/损坏状态 fail closed | PARTIAL | schema/state 坏路径有测试；R-006、相邻二进制与真实 restore 未关闭 |

关键时序结论：

1. Ledger 的 `ReservationCreated` / `AttemptStarted` 会在请求继续前等待 replication confirmation。
2. metadata journal 的普通 authoritative mutation 只保证本地 fsync + bbolt commit；只有 `metadataOpRequiresConfirmation` 选中的 mutation 会同步等待 ACK。
3. `in_flight`、Admin session delete、MFA challenge/step consumption 不在该集合，因此“调用方成功/Provider 即将被调用”与 promotion 可见前缀之间存在空窗。
4. planned stepdown 的 coordinator freeze 防新 append，但没有把已经通过 admission 的 handler drain 到安全边界。

这说明底层复制机制本身大量测试通过，但生命周期分类和外部副作用边界仍可使系统违反更高层不变量。
