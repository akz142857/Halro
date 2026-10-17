# C 段：#404 回退完整性

> 对象：#404（`dc704030`，11 个提交）回退 #360、#378、#379、#384、#385、#386，以及 `c8b97b1b` 的 HA 部分、#362 的 journal 部分。结论针对冻结 `main` `2e749db1`；"回退前"指 `098a3214`（即 `origin/ha`）。

## 结论

**回退干净：无 HA 残留，无误删的非 HA 修复，无行为倒退。** 唯一发现是 #405 归档的设计稿留下大量死链（V-010，P3），与 #404 本身无关。

## 1. 残留检查

| 面 | 方法 | 结果 |
| --- | --- | --- |
| Go 代码 | `git grep` `metadatajournal`、`internal/replication`、`hahealth`、`halro_replication`、`halro_ha_`、`halro-ha`、`stepdown`、`JournalEpoch`（排除文档） | 全部 0 处。`Replica`/`promote` 的命中均与 HA 无关：Kubernetes `replicas: 1`、Prometheus `prometheus_replica: standalone`、时区说明、会计时区变更"promote"、帧 epoch 提升 |
| 配置 | `internal/config/default.yaml` 顶层键对比 v0.8.5 | 仅 `version` 1→2、`circuit_breaker`→`routing`、新增 `provider_subscriptions`；无 replication/cluster 段 |
| CLI | `cmd/halro` 的子命令 case 对比 v0.8.5 | 仅新增 `config check|migrate`、`route suspensions|clear-suspension`；无 cluster/promote/seed |
| 告警 | `alert-rules.yml` 告警名对比 v0.8.5 | 新增 11 条，均为非 HA（审计积压 3、凭据、热状态 2、预算 2、配额、Provider 支出）。260927 时的 49 条告警降为 45 条，差额是 HA 告警 |
| 指标引用 | 告警、记录规则、runbook、observability 文档、metrics reference 中的每个 `halro_*` 与代码导出名比对 | 全部有导出（4 个是通配前缀）；无已删除指标的悬空引用 |
| CHANGELOG | `[Unreleased]` 中搜 replica/HA/journal/promote/seed | 0 处（HA 条目由 `52c45f42`、`687f8d6a` 删除） |
| 运维文档 | `docs/guides`、`docs/runbooks`、`docs/contracts`、`docs/observability` 中搜 metadata journal/cluster/replica | 仅"必须 `replicas: 1`"一类单副本约束，正确 |
| 部署资产 | `deploy/kubernetes` | 仅单副本清单；HA StatefulSet 已删除 |
| schema 编号 | `098a3214` 与 `origin/ha` 的 `schemaVersion` | 均为 40；`main` 的 41 是用量清理迁移，不存在同一编号两种含义。HA 将来回到 `main` 时从 42 起编号即可 |

## 2. 误删检查

方法：取回退前后有差异的 428 个文件；对其中也被非回退提交改过的 87 个文件，逐个看非 HA 内容是否仍在。重点是混合提交 `c8b97b1b`（260927 整改）：对它改过的每个文件，用 `git apply -R --check` 判断其补丁在回退后的树上是否仍完整存在。

| `c8b97b1b` 的文件 | 数量 | 结论 |
| --- | --- | --- |
| 补丁仍完整存在 | 86 | 非 HA 整改保留 |
| 已删除 | 12 | 全部是 HA/journal 文件：`replication_runtime.go`、三个 replication/seed 测试、`journal_tx.go`、`journal_entry_test.go`、HA runbook 与门禁文档等 |
| 有改动 | 7 | `runtime.go`（只去掉 `installPlannedStepdownDrain`，即 R-003）、`shutdown_test.go`（stepdown drain 测试）、`provider_resource_idempotency_test.go`（只去掉 journal 确认测试，即 R-001）、`route_suspensions.go`/`_test.go`（离线 clear 改回直接开库，同事务意图保留，见 R-007）、`CHANGELOG.md` 与 HA 设计文档（文本） |

`c8b97b1b` 新增的 23 个测试函数中，15 个仍在且全部通过；缺失的 8 个都是 HA/journal 测试（R-001、R-003、R-004、R-012）。逐条复核见 `revalidation-260927.md`。

#362 的混合部分：`crash_sigkill_test.go`、`unwritable_directory_test.go` 保留；`journal_write_failure_test.go` 与 `metadatajournal/log.go` 的改动随 journal 删除，正确。

## 3. 行为倒退检查

- 离线路由挂起清除：从"journal 记录的 store 事务"改回"直接 bbolt 事务"，同事务审计意图仍在，测试守护（R-007 反向验证）；
- 单机下 `in_flight` 的持久性不依赖 journal（R-001 单机分析）；
- 回退不改变任何已发布数据目录：v0.8.5 不含被回退的代码，`E01` 用已发布 v0.8.5 写出的目录验证升级路径（见 `runtime-evidence.md`）。
