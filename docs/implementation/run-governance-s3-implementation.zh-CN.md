# Run Governance S3 实现说明：业务结果、基础指标与治理导出

- 状态：**Experimental / 受控验证完成，尚未正式发布**
- 分支：`codex/run-governance-prd-review`
- 前置提交：S1 `5bdd6c7`、S2 `11d807d`
- 契约来源：PRD、ADR 0026、S0 契约矩阵

## 1. 本阶段交付

S3 把“业务验收结果发送到 FinOps”落实为最小、可审计的结构化事实，而不是把
Prompt、Response、业务原文或评价推理复制进成本系统。调用方上报的是：

- Work Unit ID；
- 创建 Work Unit 时冻结的 Outcome Definition ID/version；
- BOOLEAN 或有界 CATEGORICAL 值；
- 已认证的 reporter Gateway Key ID；
- 客户端业务观察时间与服务端接收时间；
- 可选 SHA-256 和不可执行 evidence reference；
- 当前 head 的显式 supersedes ID。

Accounting Ledger 仍是 Attempt 费用和 Run/Work Unit 归属的唯一权威；新增的
Governance Journal 只保存 Outcome 修订事实。报表在读取时用一对 watermark
连接两者，不创建第二份可加总成本。

## 2. 权威写入与故障边界

`internal/governance` 实现独立 `HGOV` v1 帧格式、HMAC-SHA256 链、sequence、
writer mutex、回放状态与 event-ID 冲突检测。链密钥从 envelope-backed Ledger key
使用独立 HKDF domain 派生，因此 Master Key 轮换不会使历史 Governance 帧失效，
也不会复用 Ledger 或 Audit 的认证域。

当前系统尚无替换 envelope-backed Ledger chain key 的在线操作，因此 `HGOV` v1
当前只有一个稳定密钥代际。未来若引入 chain key replacement，必须先增加可验证的
代际链接与恢复契约；不能直接用新密钥打开既有 Journal。

每次 acknowledged Outcome 都同步持久化 HMAC terminal anchor。非空 Journal 缺少
anchor，或 anchor 指向的终端 frame 不存在、摘要不匹配时，启动与读取均 fail closed，
不会把截断后的前缀当成完整历史。

启动时先完整验证 Journal 链。当前实现可以从 Journal 全量回放 head/idempotency；
checkpoint 只是可选的恢复加速器，不是权威数据。checkpoint 使用最多 4 MiB 的
content-addressed immutable segments，并由 HMAC 认证且绑定对应 Journal frame；版本
不匹配、领先 Journal、缺段或摘要不符时删除整个派生 checkpoint，从 Journal 重建。
Report 热路径不会为每次读取生成全量 checkpoint，当前也没有可据此宣称恢复为
O(delta) 的运行期 checkpoint writer。Journal 打不开或回放失败时 Outcome API 返回
`governance_unavailable`，但 Runtime、无 Run 推理和 Accounting Ledger 继续工作；
Admin system status 单独报告 Governance ready 与 watermark。

Outcome append 已持久但 terminal anchor 同步失败时，manager 转为 unavailable。重启后
以 Journal 和已验证 anchor 为准重建，并通过持久的 Project + operation +
idempotency-key hash 返回原 Outcome；bbolt 派生数据不能覆盖或改写 Journal 历史。

## 3. Outcome Definition 与 Work Unit 冻结

Outcome Definition 由 Admin 创建，使用 `odef_` 服务端 ID。名称、Project 和历史版本
不可变；变化通过新 version 写入同一个 family。第一版只支持 BOOLEAN 与 2–16 项的
CATEGORICAL，名称、值、描述和每 Project active 数都有硬上限。

创建 Work Unit 时最多声明 8 个 Definition ID。服务端在同一时刻解析其最新 enabled
version，并把 `{id, version}` 写入 Accounting Ledger 的 `WorkUnitCreated` 事件。
之后禁用或发布新 Definition version 不改变既有 Work Unit，也不阻止其按冻结版本
上报结果。

Admin 路由：

- `GET/POST /admin/api/v1/projects/{id}/outcome-definitions`
- `POST /admin/api/v1/projects/{id}/outcome-definitions/{definitionID}/versions`

所有创建动作经过 Admin Session、CSRF、If-Match 和 durable Audit intent。

## 4. Outcome 接收语义

公共接口为：

```http
POST /halro/v1/work-units/{workUnitID}/outcomes
Authorization: Bearer gw_...
Idempotency-Key: business-acceptance-973
Content-Type: application/json
```

它要求独立 `outcome:write` scope。Run 创建者不会自动获得该权限。写入前依次验证
Project 所有权、Work Unit 冻结声明、Definition 值域、observed time、证据字段、
关闭后 30 天窗口、当前 head 和 20 次修订上限。

`evidence_ref` 最多 128 字符，拒绝 URL、控制字符、换行和常见 credential marker；
Halro 不对它发起网络请求。`evidence_sha256` 只接受 64 个小写十六进制字符。请求
schema 不含 comment、reasoning、Prompt、Response 或原始业务结果，unknown field
由严格 JSON decoder 拒绝。

首次写入必须没有 supersedes；修订必须精确引用 current head。同一 head 的并发修订
只有一个能在 Journal writer 临界区胜出。`observed_at` 不参与排序，sequence 是唯一
修订顺序。open Work Unit 或仍有 pending/inflight Attempt 的结果返回
`provisional=true`。

## 5. Cohort 指标

`GET /admin/api/v1/governance/summary` 固定 `basis=work_unit_cohort`，按 Work Unit
创建账期选择最多 90 天、100,000 个 Work Units。查询从内存中的 Ledger read model
读取 Work Unit、Run、pending lease 和 settled Attempt，不扫描 WAL。服务端有 2 秒
deadline，超限要求使用 export。

返回 eligible、matured、evaluated、successful、coverage、success rate、已知模型费用、
进行中模型费用、estimated 子集、unknown Attempt 数、cost completeness 和
cost-per-success。零分母返回 `null`。`CostMicrosUSD` 已含 estimated 子集，聚合时不会
重复相加。Outcome 晚到或修订会重述旧 cohort，因此响应总是带生成时间和 Accounting /
Governance 双 watermarks；它不被命名为 ROI 或总业务成本。

Admin 还提供 `GET /admin/api/v1/governance/outcomes` 查看 reporter、证据摘要、两种时间
和完整修订链。Console 会沿稳定 cursor 读取完整的 Work Unit、Run、Definition 与 Outcome
列表；cursor 不前进或超过客户端安全页数时失败关闭，不把首个分页显示成总量。Run
Governance 页面支持 BOOLEAN/CATEGORICAL Definition、不可变新版本和启停，并提示版本
只影响新 Work Unit。Summary 同屏展示 coverage、success rate、known/estimated/unknown
费用、单位成功成本、completeness/reason、生成时间与双 watermarks，并明确显示
provisional 和 partial。

## 6. Governance export 与备份恢复

`POST /admin/api/v1/governance/export` 在 data directory 下生成四个规范化 NDJSON：

- `work_units.ndjson`
- `runs.ndjson`
- `outcomes.ndjson`
- `outcome_definitions.ndjson`

manifest v1 记录 dataset、schema、format、SHA-256、record count、Outcome sequence
range 和双 watermarks。Attempt 金额仍只存在现有 Usage export；四个 Governance
dataset 不导出 `committed_micros_usd` 或 `cost_micros_usd` 可加总列。

backup manifest 升级为 v3，加入 Governance format、sequence、offset、terminal hash
并强制包含 `data/governance/governance.journal`。Restore 在 staging 中使用独立派生
key 验证完整 HMAC chain、逐事件回放和 manifest head，失败时不会切换数据目录。
旧 v1/v2 backup 继续可读。

## 7. 已验证边界

- Journal 错 key、单字节篡改、sequence/revision 断链；
- current-head 并发修订只成功一个；
- Outcome 幂等重放返回同一 ID，不同 fingerprint 冲突；
- evidence URL、换行和 credential canary 在持久化前拒绝；
- Work Unit 冻结 Definition version，旧版本不被新版本改写；
- acknowledged Outcome 的 terminal anchor 同步、非空 Journal 缺 anchor 及截断拒绝；
- 可选 segmented checkpoint 跨多段 round-trip、frame 绑定与整体 reset；
- Governance 损坏时 Runtime 和 Accounting append 仍可用；
- cohort 的 matured/evaluated/successful 与零成本分母；
- export 四数据集、checksum、count 及无重复成本列；
- backup/restore 对 Governance Journal 的包含、链校验和 head 核对；
- northbound/Admin route 双向契约、Runtime breadth、前端类型，以及 Definition
  创建/版本化、Summary partial 与双水位、分页和 unavailable 状态的页面测试。

## 8. 发布边界

代码与受控 fixture 只能证明协议、持久化、恢复和计算口径按冻结契约工作，不能证明
某个业务的 Definition 真正代表价值。根据 PRD 的发布门槛，仍需业务负责人提供首个
试点的 Work Unit 边界、Definition、验收方、观察窗口和决策用途，并在脱敏数据副本上
核对 coverage 与手工账。完成真实设备/真实调用方的试点验收前，本能力保持
Experimental，不宣称正式发布、ROI 或完整业务成本。
