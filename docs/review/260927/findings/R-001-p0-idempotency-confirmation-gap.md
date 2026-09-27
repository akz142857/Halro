# R-001 — P0 CONFIRMED：`in_flight` 未确认即调用 Provider

- 基线：`0c4a270d7aae2b3d4562c35cd60ff14907278897`
- 影响：F05、F12、X03、INV-02、INV-04
- 裁决：CONFIRMED；阻断发布与 HA 启用

## 契约与实际路径

调用方幂等承诺的是上游 at-most-once。`noteInferenceDispatch` 会在 Provider I/O 前把记录由 `reserved` 改为 `in_flight`（`internal/gateway/inference_idempotency.go:50-73`），随后 Chat、stream 和 Embeddings 立即调用 Provider（`internal/gateway/service.go:1397-1402,2455-2461,2642-2646`）。

该更新经普通 `PutProviderResource` 完成（`internal/gateway/inference_resources_store.go:119-128`）。`provider_resources` 与 `provider_resource_idempotency` 虽属于 authoritative journal，但不在 `metadataOpRequiresConfirmation` 的同步确认集合中（`internal/store/bolt/journal_tx.go:248-312`）；`updateContext` 只有在该标志为真时才等待 `WaitConfirmed`（`internal/store/bolt/journal_entry.go:41-46`）。

因此存在以下可达窗口：本机 `in_flight` 已提交但尚未获得 Replica ACK → Provider 已收到调用 → Primary 被 fence → Replica 只提升 confirmed 前缀，仍看到旧实例的 `reserved` → `classifyIdempotency` 把它视为可回收（`internal/gateway/inference_resources_store.go:106-115`）→ 同一 key 再次触发 Provider。

这直接满足本轮 P0 定义中的“重复 Provider 副作用”，且没有可靠运维绕过。

## 现有防御为何不足

- Reservation 和 `AttemptStarted` 会确认，但不能表达“Provider 即将收到本次调用”。
- store 写失败会在 Provider 前 fail closed，但“本地成功、quorum 未确认”不是写失败。
- `TestCallerIdempotencyReplicatesWithTheJournal` 只证明 bucket 被 journal，不证明调用前同步确认。

## 关闭条件

根因修复必须让 Provider I/O 之前的不可重放状态属于 confirmed 前缀，或采用等价且可证明的切主协议。新增双 Runtime 故障测试：阻断该 frame 的 ACK，记录首次 Provider 调用，fence/promote 后用相同 key 重试；Provider 调用总数必须保持 1。

本轮未制造真实重复计费调用；裁决来自两名独立 Reviewer 对同一完整时序的交叉验证和源码可达性证明。
