# V-011 — P2：延迟请求每次被放回队列都会再计一次 Token Guard 用量，严格策略下请求被判失败、Key 被临时封禁

- 基线：冻结 `main` `2e749db1`；**v0.8.5 已存在**（`deferredAdmissionRefusal` 与 Token Guard 先于限流器的准入顺序在 v0.8.5 相同）
- 段/功能：既有延迟层 × Token Guard；V-002 的整改（#428）会把它扩大到热状态拒绝
- 分类：【问题】，确定性单元复现；由 #428 的自动评审指出（"by inspection, unexecuted"），本轮执行确认
- 不在本轮原范围：原因代码早于 v0.8.5；因整改会扩大其影响而记录

## 现象

`beginRequestRun`（`internal/gateway/service.go`）的准入顺序是 Token Guard → Project 限流器 → 记账（`BeginRequest`，热状态上限在这里）。Token Guard 的 `Acquire` 放行时把这次请求的 token 与成本记进每分钟桶，并可能累计一次 violation；之后限流器或热状态拒绝时只调用 `tokenGuardLease.Release()`，而 `Release` 只减在途计数（`internal/tokenguard/manager.go:228`），不撤回桶里的样本。

同步请求被拒时由调用方自己决定是否重试，计入样本可以理解；延迟请求被放回队列后由 Halro 自己重试，**一次提交会被计成多次用量**。

## 复现（一次性测试，未提交）

策略 `temporary_block`、`tokens_per_minute: 1`、`minimum_samples: 2`、`violations_before_block: 2`：

| 情形 | 结果 |
| --- | --- |
| 对照：不受限，出队一次 | `completed` |
| 并发上限（`main` 已有的放回路径）下出队两次 | 第 1 次放回；第 2 次 `failed` / `token_guard_blocked`，上游调用 0 次；释放后仍是 `failed` |
| 热状态上限（#428 新增的放回路径）下出队两次 | 同上 |

请求从未到达上游，却被判失败、存储的请求被删除，且 Key 被封禁 `block_ttl`，期间该 Key 的其他请求也被拒。

## 影响

只在启用了严格 Token Guard 策略的 Project 上出现；放回越频繁（并发上限、热状态上限持续）越快触发。#428 之前，热状态拒绝只让一条延迟记录失败；#428 之后，持续的热状态拒绝可能封禁整个 Key——在这类配置下比修复前更糟。

## 建议

延迟层出队被"写 Ledger 之前"的限制拒绝时，撤回这次 Token Guard 准入留下的样本（token、成本、请求数，以及这次准入造成的 violation），或在出队前先探测这些限制；补一个"多次放回后仍能完成、Key 不被封禁"的测试。应在 #428 合并之前或与之一起修复。
