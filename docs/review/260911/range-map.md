# v0.8.0 评审范围与基线

状态：S0 范围冻结完成，后续若 `main` 移动必须增量更新本文件并重新判定门禁。

## 1. 精确范围

| 项目 | 值 |
| --- | --- |
| 上一发布版本 | `v0.7.1` |
| 基线提交 | `84f2638e973f23935b9eda423143f65ff25a852c` |
| 候选分支 | `main` |
| 候选提交 | `222d08f84f61493fc9a273d351cc728528d6e30c` |
| 远程一致性 | `HEAD == origin/main` |
| 提交数量 | 12 |
| diff 规模 | 147 files, +12,862 / -1,165 |
| 生产 Go | 41 files, +3,110 / -277（排除 `*_test.go` 与生成 bundle） |
| `web/src` | 19 files, +3,237 / -516 |

建立时间：2026-09-11（Asia/Shanghai）。工作区在评审开始时只有
`docs/review/260911/` 下的本轮未提交评审文件；产品源码与 `origin/main` 一致。

工具链：

- Go `go1.26.6 darwin/arm64`
- Node `v22.22.2`
- npm `10.9.7`

候选 SHA 的 GitHub `ci` run `34569951375` 已完成且结论为 success。该结果是 S0 基线，
不是本轮独立评审门禁的替代品。

## 2. 提交清单

| 提交 | 内容 |
| --- | --- |
| `f31bc87` | 修复 Gateway Key scope 页面布局 |
| `8dc23dc` | 失败时捕获 Gateway 请求 |
| `459a572` | 新增地域化 BigModel / Z.AI 支持 |
| `a1c0541` | 建模 Provider Offering 并接入 GLM Coding Plan |
| `2bbd954` | Python Anthropic SDK 兼容依赖升级 |
| `d88e394` | Go OpenAI SDK 兼容依赖升级 |
| `55b580f` | Python OpenAI SDK 兼容依赖升级 |
| `4c044a1` | Go Anthropic SDK 兼容依赖升级 |
| `973f4dd` | Node OpenAI SDK 兼容依赖升级 |
| `629b6b7` | Node Anthropic SDK 兼容依赖升级 |
| `1c67e27` | Admin UI 依赖升级，含 Vitest 5 和重建 bundle |
| `222d08f` | AWS SDK、`x/crypto`、`x/sys` 升级 |

## 3. 模块与契约库存

改动涉及以下生产模块：

`app`、`budget`、`compatibility`、`config`、`domain`、`failurecapture`、
`gateway`、`gatewayapi`、`ledger`、`modelcatalog`、`provider`、`requestmeta`、
`usage`、`webui`，以及 `web/src` 的 hooks、i18n、pages、styles、golden 与 types。

当前静态库存：

| 对象 | 数量 | 来源 |
| --- | ---: | --- |
| Provider Profile rows | 34 | `internal/domain/provider_table.go` |
| Provider Offering rows | 14 | `internal/domain/provider_offering.go` |
| Access Surface rows | 18 | `internal/domain/provider_offering.go` |
| endpoint manifests | 30 | `docs/compatibility/endpoint-manifests.json` |
| manifests 引用的唯一 Provider Profile | 34 | 同上 |

执行评审时不能把“数量相等”当作覆盖证明；R1/R4 必须逐 ID 验证双向映射与负向门禁。

## 4. 持久格式事实

| 格式 | v0.7.1 | 候选 | 判定 |
| --- | ---: | ---: | --- |
| bbolt metadata schema | 36 | 36 | 未 bump；生产 `internal/store` 零 diff |
| Ledger frame current version | 2 | 2 | 帧布局未 bump；Event JSON payload 新增可选归因/失败字段 |
| Usage checkpoint version | 13 | 13 | 未 bump；聚合行承载字段增加，需重建等价性检查 |
| Parquet schema | 6 | 8 | schema 7 增 Offering/Profile，schema 8 増 Region/失败语义；强制 populated 升级/rollback 检查 |
| failure capture JSON | 旧请求/响应字段 | 增 GatewayRequest、归因与失败语义 | 向后读取应容忍缺字段；新写入必须满足加密、TTL、授权和大小上限 |

候选仍声明 `parquetSchemaMinReadable = 3`。这意味着候选应能读取 schema 6 并升级；
v0.7.1 对已写 schema 8 的目录如何处理必须用旧二进制实测，不从常量猜结论。

## 5. 关键数据流

### 5.1 Provider 身份

```text
Provider type + Offering + Region
  -> Access Surface
  -> Provider Profile / Connection Group
  -> credential + endpoint
  -> adapter / primitive binding
  -> route target
  -> OfferingID + ProfileID + AccountRegionID snapshot
```

Offering `Kind` 只用于展示与治理，不应进入路由、鉴权、预算或定价决策。

### 5.2 请求、失败与账务

```text
Gateway decoded request
  -> requestmeta context (unredacted body, no headers)
  -> semantic request after policy/redaction
  -> target selection and durable reservation
  -> provider attempt
  -> canonical failure descriptor
  -> settlement + Ledger Event
  -> Usage checkpoint / Parquet
  -> encrypted failure capture (only configured terminal outcomes)
  -> Admin Usage / failure detail
```

评审重点是身份与失败语义在整条链上冻结且一致，以及未脱敏 GatewayRequest 不逃出
failure capture 的加密、租户、TTL、大小和审计边界。

## 6. 明确未改但仍需边界复读的模块

以下范围在 `v0.7.1..main` 没有文件 diff：

- `internal/store`
- `internal/auth`、`internal/adminauth`
- `internal/redaction`、`internal/contentscan`
- `internal/safetransport`
- `internal/tokenguard`
- `internal/limiter`、`internal/circuit`、`internal/idempotency`
- `internal/backup`
- `.github/workflows`

不逐行全面复审这些模块，但要复读与新数据流相接的边界：SafeTransport 接收的新 endpoint、
Admin 对 capture 正文的授权、backup 对新 Parquet/capture 的覆盖、Ledger 对新 Event 字段的
认证，以及 release workflow 对新依赖和 bundle 的真实门禁。

## 7. 历史结论与待复验边界

2026-09-05 全面评审的 18 项确认问题已有源码修复和本地回归。历史“已修复”不作为当前范围
自动通过证据；与本轮相接的以下项目必须复验：

- accepted-but-malformed Provider response 的 ambiguous / no-fallback / conservative settlement；
- failure capture TTL、正文读取权限和换钥期间 retained ciphertext；
- Usage checkpoint 在途可见性与 Ledger/Parquet 重建；
- SDK compatibility、浏览器 Provider/Deployment/Project/Usage 旅程；
- release evidence、bundle、依赖许可证与供应链。

仍缺外部或扩大验收的历史边界：真实 Provider、真实云 KMS、实际告警接收端、完整浏览器
MFA/降权/异步/读屏矩阵、2–4 小时与 24 小时稳定性、目标 Linux 生产硬化。本轮只能在获得
环境与明确授权后关闭；否则必须作为边界写入最终 assessment。

## 8. 深度检查判定

- Provider wire / semantic：触发。
- 请求热路径与 budget：触发，性能对比强制。
- Ledger / Usage / durable format：触发，升级/回滚/备份恢复强制。
- 安全：按新未脱敏正文和 endpoint 数据流触发。
- 前端：触发完整 typecheck/test/build、真实浏览器与 bundle drift。
- 供应链：触发 Go/npm/SDK 许可证、漏洞、SBOM、签名和 release dry run 检查。
- 计费真实 Provider：技术上触发，但未获本轮授权；当前状态为“待授权/未验证”，不是通过。

## 9. S0 退出核对

- [x] 基线 tag 与候选 SHA 唯一且 `HEAD == origin/main`。
- [x] 工作区中的非产品 diff 已识别。
- [x] 提交、文件、模块、Profile、Offering、Surface、manifest 和格式版本已列出。
- [x] 通用发布触发表已逐行判定。
- [x] 历史未闭环外部证据已转交本轮，不因时间经过自动关闭。
- [x] 禁止真实 Provider、真实 KMS、生产数据和正式发布的边界已记录。
