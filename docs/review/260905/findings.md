# 发现汇总（2026-09-05）

基线：`381743f6613607dc256828f4776b52af8bdd232c`。本文件整合角色报告与独立裁决，保留原 ID；SUM-01 为主持的 summary 候选正式编号，CFG-01 对应 reliability 末尾的独立配置复核。只做文档汇总，未修业务、未重跑门禁。详细命令、退出码、fixture、日志和覆盖边界以相对链接的角色文档为准。

**按根因去重共 18 项：P0 0、P1 3、P2 14、P3 1。** 三项 P1 均独立确认；独立证伪完成的根因共 11 项（另含 SEC-03、SEC-04、FE-01、FE-03、FE-05、REL-01、SUM-01、CFG-01）。其余 7 项保留原角色确认及证据等级，不冒称独立验证。全部仍开放，修复方向和回归均是提案，无已修复/已关闭项。P1 未处置前不支持无条件发布；例外须按计划由发布责任人书面接受风险。

去重规则：SEC-01 的 file/KMS DEK、capture/resource/input/output 是同一迁移遗漏；PROV-01 多种 decode/read 失败及源码支持的 sibling 为同一 ambiguity 根因；FE-02 各消费者合为一个分页根因；CFG-01 的启动/boolean 差异合为 omission 契约根因。SEC-01 与 SEC-04、SEC-02 与 SEC-03、FE-01 与 SUM-01、REL-02 与 SUM-01 影响可能相邻，但因果和修复不同，不能合并。独立裁决不重复计数。

## 索引

| ID | 严重度 | 发现 | 证据状态 |
| --- | --- | --- | --- |
| [SEC-01](#sec-01) | P1 | Master Key/DEK 轮换遗漏留存密文 | 独立确认 |
| [SEC-02](#sec-02) | P1 | Admin 会话刷新重新插入已注销会话 | 独立确认 |
| [PROV-01](#prov-01) | P1 | 已接受但不可解码的响应被重试并按零费用结算 | 独立确认 |
| [SEC-03](#sec-03) | P2 | MFA 管理路径的错误第二因子未计入失败预算及审计 | 独立确认（实际 Admin router） |
| [SEC-04](#sec-04) | P2 | Failure capture 读取不检查到期时间 | 独立确认（实际 Admin router） |
| [SEC-05](#sec-05) | P2 | Audit 分页全历史扫描并占用 append 锁 | 原角色确认；未独立证伪 |
| [PROV-02](#prov-02) | P2 | CR-only SSE 完整事件等待 LF/EOF | 原角色确认；未独立证伪 |
| [FE-01](#fe-01) | P2 | Usage 同页跳转只改 URL 不更新 tab/filter | 主持真实浏览器独立确认 |
| [FE-02](#fe-02) | P2 | 只读取第一页的资源消费者隐藏后续项目/凭据/策略 | 原角色确认；未独立证伪 |
| [FE-03](#fe-03) | P2 | Debug key 重试重建幂等操作身份 | 独立确认（组件重跑 + 后端源码） |
| [FE-04](#fe-04) | P2 | 破坏性确认可在请求 pending 时关闭 | 原角色确认；未独立证伪 |
| [FE-05](#fe-05) | P2 | 项目编辑损失精确字节上限 | 独立确认（组件重跑 + 后端源码） |
| [REL-01](#rel-01) | P2 | Dead-man 出站先于序列持久化 | 独立确认 |
| [REL-02](#rel-02) | P2 | stats interval 将 gauge 当作 counter 求差 | 原角色确认；未独立证伪 |
| [REL-03](#rel-03) | P2 | 发布证据指南仍承诺已退役审批门禁 | 原角色确认；未独立证伪 |
| [REL-04](#rel-04) | P3 | Pricing provenance 接受单侧 minute bound | 原角色确认；未独立证伪 |
| [SUM-01](#sum-01) | P2 | Usage summary 在 checkpoint 交接间漏掉已结算增量 | 独立确认 |
| [CFG-01](#cfg-01) | P2 | 模板承诺删除字段恢复默认但 Load 未全量合并默认 | 独立确认 |

<a id="sec-01"></a>

## SEC-01 — Master Key/DEK 轮换遗漏留存密文

- **严重度 / 置信度 / 责任领域：** P1 / 高 / 密钥生命周期（具体负责人及排期由主持指定）。
- **前提 / 位置：** `internal/app/key_rotation.go:257,433`、`kms_key_lifecycle.go:975`、`internal/vault/vault.go:108,127,182`。存在未过期 capture、本地文件或 deferred 输入/结果，按流程离线更换 Master Key/DEK 后重启。
- **实际 / 证据：** 轮换报告成功，但对象字节未迁移，新密钥认证失败；旧密钥可解。文件模式和 fake-KMS DEK 路径已复现。capture HTTP 404、local file/completed response 503、queued input 无法恢复执行为源码追踪，未完成注册资源端到端复现。
- **已有防御 / 影响边界：** 离线锁、元数据 COW、bridge、slot 验证及 Audit/Ledger 密钥保护仍有效，但不迁移外部对象。相同 DEK 的 KEK rewrap 不受此根因影响；无明文泄露或必然不可恢复丢失证据，旧密钥及适当备份可提供恢复材料。
- **最小修复方向：** 将所有依赖密文纳入可恢复迁移，或保留受保护的代际解密能力；临时方案是在有活跃对象时于任何修改前拒绝轮换。
- **回归验收：** file/fake-KMS DEK 下注册的 capture、queued/completed response、file 跨轮换读取；中断恢复、旧钥退役、备份恢复；保留 same-DEK rewrap 对照。
- **来源 / 证伪者：** [原始证据](roles/security.md)；独立 rotation reviewer：[独立裁决](roles/security-rotation-adjudication.md)，CONFIRMED。

<a id="sec-02"></a>

## SEC-02 — Admin 会话刷新重新插入已注销会话

- **严重度 / 置信度 / 责任领域：** P1 / 高 / adminauth / bbolt（具体负责人及排期由主持指定）。
- **前提 / 位置：** `internal/adminauth/session.go:103-126,147-151`、`internal/store/bolt/store_admin.go:182-222`。持有原有效 cookie，刷新到期请求与正常 logout 重叠，刷新写入晚于删除。
- **实际 / 证据：** 真实 logout HTTP 200 且行已删除；延迟 upsert 重建行，新 Manager 和随后 protected GET 仍接受旧 cookie。独立 HTTP router/bbolt 屏障测试及 race 通过；不是仅允许原请求完成。
- **已有防御 / 影响边界：** CSRF、cookie 属性、当前角色、绝对过期和 generation 检查有效；清除浏览器 cookie 不撤销副本。密码/MFA generation 变化对照拒绝旧会话；未恢复 step-up elevation，也不是匿名绕过或提权。
- **最小修复方向：** 分离创建和条件刷新，在同一事务内验证现存会话、generation/expiry 后更新；不得插入缺失行。
- **回归验收：** 注销前后两种刷新顺序、并行刷新、generation 变化、绝对到期、新 Manager 及后续 HTTP 请求；不能以再做一次事务外 Get 代替原子操作。
- **来源 / 证伪者：** [原始证据](roles/security.md)；独立 reliability/session reviewer：[独立裁决](roles/security-session-adjudication.md)，CONFIRMED。

<a id="prov-01"></a>

## PROV-01 — 已接受但不可解码的响应被重试并按零费用结算

- **严重度 / 置信度 / 责任领域：** P1 / 高 / Provider / Gateway 账务（具体负责人及排期由主持指定）。
- **前提 / 位置：** `internal/provider/openai/adapter.go:411-415,514-521`、`internal/provider/primitive.go:175-181`、`internal/gateway/service.go:2478-2489,2874-2916`。正常已授权 unary Chat 获得上游 HTTP 200，但 JSON、必需 envelope、body 读取或大小检查失败。
- **实际 / 证据：** 错误缺少 Ambiguous=true；真实适配器及桥接产生两次出站，committed/reserved=0/0。独立 metered snapshot、已提交 price pin 和每次 50 micro-USD 正预留仍复现；仅改 ambiguity 的对照是一调用、50/0。
- **已有防御 / 影响边界：** 认证、能力、预算预留、attempt 上限、circuit 和超时仍有效。权威 HTTP 400 对照一调用零费；后续 semantic normalization 已有 ambiguity。不是无限重试、未授权执行、Ledger 损坏或真实收费证明；记录仍存在，错误的是可能费用被当成零。
- **最小修复方向：** 在已接受推理响应的不可用结果产生处标注不确定性；区分未发送、权威拒绝、非计费 discovery，不要一律禁用 fallback。
- **回归验收：** 四种 body/envelope 故障经真实 adapter+metered gateway 应一调用且保守估算；拒绝对照零费；第二 target 不 fallback。Responses/embeddings 同根分支需各自补证；Mantle 首事件流仍待验证。
- **来源 / 证伪者：** [原始证据](roles/provider.md)；独立 Provider adjudicator：[独立裁决](roles/provider-adjudication.md)，CONFIRMED。

<a id="sec-03"></a>

## SEC-03 — MFA 管理路径的错误第二因子未计入失败预算及审计

- **严重度 / 置信度 / 责任领域：** P2 / 高 / Admin MFA（具体负责人及排期由主持指定）。
- **前提 / 位置：** `internal/app/admin_mfa.go:101-156,450-483,529-540,585-607`。有效已登记 session、CSRF/origin、正确密码，但连续错误六位 TOTP。
- **实际 / 证据：** 独立真实 Admin router 覆盖新增/删除 authenticator、重生成 recovery、关闭 optional MFA 四路径：各七次正确密码+错误六位因子均 401，无 429、无 reauthentication failure audit；各路径错误密码控制为五次401、第六次429，证明预算机制本身已接入。
- **已有防御 / 影响边界：** 登录 MFA 自有 challenge/预算，错误密码管理请求有预算；required policy 禁止关闭 MFA，其他路径仍存在。Argon2/并发、TOTP 单用及恢复码强度仍有效；不构成匿名 MFA 绕过或已证实接管。
- **最小修复方向：** 将完整密码/第二因子校验放入共享失败 guard，保留各操作因子语义。
- **回归验收：** 每个管理操作正确密码+错误因子、共享预算、并发猜测、audit 数量；required policy 与登录路径对照。
- **来源 / 证伪者：** [原始证据](roles/security.md)；独立 security secondary adjudicator：[独立裁决](roles/security-secondary-adjudication.md)，CONFIRMED / P2；实际 Admin router 定向实验，未运行洪泛、真实浏览器或生产流量。

<a id="sec-04"></a>

## SEC-04 — Failure capture 读取不检查到期时间

- **严重度 / 置信度 / 责任领域：** P2 / 高 / 留存隐私（具体负责人及排期由主持指定）。
- **前提 / 位置：** `internal/failurecapture/failurecapture.go:387-433,484-502`、`internal/app/failure_capture.go:114-161`。有授权 payload reader 在 TTL 后、清扫前读取。
- **实际 / 证据：** 独立实际认证 HTTP：一小时 TTL 的对象在恰好一小时、两小时仍200且返回正文，Purge后404；administrator/read_only读取均有审计。无cookie 401、错误project无法解密、audit关闭503且不返回正文的控制有效。store时钟注入，未测清扫最大延迟。
- **已有防御 / 影响边界：** 认证、审计、AEAD scope、周期和关闭清扫存在，不能保证每次读的 TTL。慢 export、删除失败或硬重启可延长窗口；不是匿名披露，亦不能保证最多仅多留一小时。与 C2 的谁能读是不同根因。
- **最小修复方向：** 基于可信 capture 时间在读路径拒绝到期对象，物理删除独立执行。
- **回归验收：** 精确过期边界、重启、清扫失败/延迟仍拒绝读；区分逻辑 TTL 与删除 SLA。
- **来源 / 证伪者：** [原始证据](roles/security.md)；独立 security secondary adjudicator：[独立裁决](roles/security-secondary-adjudication.md)，CONFIRMED / P2；实际 Admin router 定向实验，未运行洪泛、真实浏览器或生产流量。

<a id="sec-05"></a>

## SEC-05 — Audit 分页全历史扫描并占用 append 锁

- **严重度 / 置信度 / 责任领域：** P2 / 中（影响）；高（源码机制） / Audit / Admin 查询（具体负责人及排期由主持指定）。
- **前提 / 位置：** `internal/app/admin_resources.go:324-350`、`internal/audit/log.go:243-254`。已认证 Admin 请求 `GET /admin/api/v1/audit?limit=1`，历史持续增长。
- **实际 / 证据：** 先 Replay 全历史并收集匹配项、反转后截页；扫描占用 AppendBatch 同一互斥锁，查询解码/内存 O(history)。源码确认，未做容量压测。
- **已有防御 / 影响边界：** 认证、页大小、帧认证及 64 KiB 单帧上限不限制总工作；4096 去重索引不限制历史。不能写成已复现 OOM、匿名 DoS 或量化停顿。
- **最小修复方向：** 扫描最多保留目标页，避免历史解码持有 append 锁；必要时增加可定位索引。
- **回归验收：** 大历史 limit=1 的峰值内存、append 延迟、并发查询和 cursor 正确性。
- **来源 / 证伪者：** [原始证据](roles/security.md)；尚无独立证伪报告；本次汇总不替代独立复现。

<a id="prov-02"></a>

## PROV-02 — CR-only SSE 完整事件等待 LF/EOF

- **严重度 / 置信度 / 责任领域：** P2 / 高 / SSE framing（具体负责人及排期由主持指定）。
- **前提 / 位置：** `internal/sse/sse.go:91-117`。SSE upstream flush `data: one\r\r` 后保持连接，使用已承诺支持的 bare CR 终止符。
- **实际 / 证据：** 先 ReadSlice LF 才拆 CR；open-pipe 在观察窗口内不出事件，关闭后立即返回；多个小事件还可能被当作过大的累计行。
- **已有防御 / 影响边界：** 字节上限、取消/超时存在，LF/CRLF 正常。未证明任何指定真实 Provider 当前使用 CR-only；不是所有流式请求都会阻塞。
- **最小修复方向：** 增量识别 CR/LF，正确跨读取合并 CRLF，并按真实 line/event 限长。
- **回归验收：** 保持管道打开的早交付、跨块 CRLF、多条有界 CR-only 事件总量超限场景。
- **来源 / 证伪者：** [原始证据](roles/provider.md)；尚无独立证伪报告；本次汇总不替代独立复现。

<a id="fe-01"></a>

## FE-01 — Usage 同页跳转只改 URL 不更新 tab/filter

- **严重度 / 置信度 / 责任领域：** P2 / 高 / Console 导航（具体负责人及排期由主持指定）。
- **前提 / 位置：** `web/src/navigation.tsx:32-48`、`pages/UsagePage.tsx:38,60-83,115-119`。已挂载 Usage 点击 Summary failure/group drill-down。
- **实际 / 证据：** URL 变为 tab=failures 但仍选 Summary，未调用 usageFailures；已挂载过滤器也未随链接更新。原角色 DOM 复现；主持于本轮对隔离 full runtime 的当前内嵌 bundle 独立浏览器确认：Summary 显示 216 完成、1 最终失败，点击“查看最终失败→”后 URL 含 tab=failures/start/end，AX 显示 Summary 仍选中；直接点最终失败 tab 才出现 HTTP 500/mock_failure/1 次尝试的记录与详情。该浏览器证据直接确认 failure-link/tab 路径，grouped filter 仍为原角色证据。
- **已有防御 / 影响边界：** 初次深链接挂载、普通 tab 按钮可用，刷新可绕过；不是服务器授权或账务错误。
- **最小修复方向：** 订阅完整 pathname+search，同步 tab/filter 与历史导航，明确草稿保留策略。
- **回归验收：** 真实同页链接后的 tab、API interval/ID、记录、Back/Forward；不能只断言地址栏或 href。
- **来源 / 证伪者：** [原始证据](roles/frontend.md)；主持真实浏览器独立确认（2026-09-05 本轮直接补充，隔离 loopback fixture、模拟 Provider）；详细角色复现见前链。浏览器原始 trace/日志索引由主持保管，本次未自行复跑。

<a id="fe-02"></a>

## FE-02 — 只读取第一页的资源消费者隐藏后续项目/凭据/策略

- **严重度 / 置信度 / 责任领域：** P2 / 高 / Console 分页契约（具体负责人及排期由主持指定）。
- **前提 / 位置：** `web/src/api.ts:292,340-341,499-500,524-525`；Providers/Developer/Project policy picker/Usage 消费者。所需项位于 51 条以后。
- **实际 / 证据：** 服务端默认 50 且返回 next_cursor，plain-list 消费者只用 items，无后续请求，无法选择/查找后页资源。API-client 复现及消费者源码。
- **已有防御 / 影响边界：** 其他 listAll/infinite-query 页面有分页，不修复这些独立消费者；API 管理可绕过。未证明保存项目会清空缺失 policy binding。
- **最小修复方向：** 有限选择器复用完整列表或显式分页搜索，保留已选但不在当前页的项及说明。
- **回归验收：** 第二页目标项在凭据、workbench 项目、两类 policy picker 可达；续页失败、重复 cursor、已选禁用项。
- **来源 / 证伪者：** [原始证据](roles/frontend.md)；尚无独立证伪报告；本次汇总不替代独立复现。

<a id="fe-03"></a>

## FE-03 — Debug key 重试重建幂等操作身份

- **严重度 / 置信度 / 责任领域：** P2 / 高 / Console 凭据创建（具体负责人及排期由主持指定）。
- **前提 / 位置：** `web/src/pages/DeveloperPage.tsx:202-210`、`internal/app/admin_projects.go:234-285`。已通过 step-up 的 mint 已提交但响应丢失，用户重试同一确认。
- **实际 / 证据：** 每次 mutation 新 UUID/时间/到期，mock 观察到不同 token；服务端按 actor+project+token 派生 ID，源码支持产生额外有效记录。未做真实 commit-disconnect HTTP 复现。
- **已有防御 / 影响边界：** 每次 step-up、24h expiry、key 管理存在；普通 CreateKey 稳定 ref 不受影响。无丢失明文被攻击者获知或权限突破证据。
- **最小修复方向：** 同一确认操作固定身份和 payload，只在明确新 mint 时重置，保留 replay recovery 引导。
- **回归验收：** 本地真实提交后断响应再重试，只一 key 且提示 replay/revoke/reissue。
- **来源 / 证伪者：** [原始证据](roles/frontend.md)；独立 frontend boundary adjudicator：[裁决](roles/frontend-boundary-adjudication.md)，CONFIRMED / P2。重跑原组件定向 fixture 并核对后端防御；非真实 HTTP commit-disconnect/持久化及 gateway 边界运行。

<a id="fe-04"></a>

## FE-04 — 破坏性确认可在请求 pending 时关闭

- **严重度 / 置信度 / 责任领域：** P2 / 高 / Console 确认交互（具体负责人及排期由主持指定）。
- **前提 / 位置：** `web/src/components.tsx:560-571,588-595,379,448-449`。慢 project-disable 提交后按 Escape/×。
- **实际 / 证据：** Cancel/action 禁用而 Modal 未传 closeDisabled，确认框关闭，请求仍运行；未决 promise 的 DOM 复现。
- **已有防御 / 影响边界：** 页面 mutation、revision/step-up 仍有效，部分页面另有结果展示。不是已证实重复执行或所有结果丢失；有密码的 dirty 提示只增加一步。
- **最小修复方向：** pending 时禁止关闭，或明确支持后台执行并提供持续结果入口；同时防重入。
- **回归验收：** 持有请求尝试 Escape/×，拒绝后显示错误及可重试，成功后关闭。
- **来源 / 证伪者：** [原始证据](roles/frontend.md)；尚无独立证伪报告；本次汇总不替代独立复现。

<a id="fe-05"></a>

## FE-05 — 项目编辑损失精确字节上限

- **严重度 / 置信度 / 责任领域：** P2 / 高 / Console 配置序列化（具体负责人及排期由主持指定）。
- **前提 / 位置：** `web/src/pages/ProjectsPage.tsx:485,515`。API 创建 max_request_bytes=500 后 UI 无修改保存或仅改名。
- **实际 / 证据：** 整数 KiB 四舍五入再乘 1024，500→0，撤掉项目显式上限；1500→1024。独立 DOM 重跑捕获未编辑任何字段即提交 0（原测试名虽称 name-only，实际未改名）；服务端接受及 snapshot→gateway 生效由源码确认，实例 hard limit 仍在。
- **已有防御 / 影响边界：** ETag 防并发不防舍入；实例级限额仍在，0 使用实例限制，enable/disable helper 保留精确值。不是无限请求体。
- **最小修复方向：** 未编辑时保留原始 bytes，或支持精确 byte/小数 KiB，不将小正值静默变为特殊零值。
- **回归验收：** 0/500/1024/1500/1048576 的未改 round-trip 和明确编辑；API GET 确认持久值。
- **来源 / 证伪者：** [原始证据](roles/frontend.md)；独立 frontend boundary adjudicator：[裁决](roles/frontend-boundary-adjudication.md)，CONFIRMED / P2。重跑原组件定向 fixture 并核对后端防御；非真实 HTTP commit-disconnect/持久化及 gateway 边界运行。

<a id="rel-01"></a>

## REL-01 — Dead-man 出站先于序列持久化

- **严重度 / 置信度 / 责任领域：** P2 / 高 / Dead-man outbox（具体负责人及排期由主持指定）。
- **前提 / 位置：** `internal/deadman/engine.go:77-100,125-143,319-363`。receiver 接受新 event 后，在 Tick 或 delivery 任一路径成功保存前崩溃，同 probe ID 重启。
- **实际 / 证据：** 独立 real Engine.Run + fake transport/race：disk 0/send 1、disk 41/send 42，loadState+enqueue 重用 ID 而 observation 改变。无真实进程 kill/receiver 耐久性实验。
- **已有防御 / 影响边界：** FIFO、锁、in-flight 保护、原子 sync 保存、replay 拒绝有效但不能提前持久化。delivery 自己成功保存即可关闭窗口，不必等待探测结束；后续更高序列通常恢复。无永久 paging 丢失证据。
- **最小修复方向：** 在交给 sender 前持久化 sequence/outbox，同时保留心跳不被慢探测阻塞。
- **回归验收：** 接受后中断、初始/后续 tick、write/sync 失败、慢 probe/receiver；重试同观察或使用新 ID，不得同 ID 新观察。
- **来源 / 证伪者：** [原始证据](roles/reliability.md)；独立 dead-man adjudicator：[独立裁决](roles/deadman-adjudication.md)，CONFIRMED。

<a id="rel-02"></a>

## REL-02 — stats interval 将 gauge 当作 counter 求差

- **严重度 / 置信度 / 责任领域：** P2 / 高 / CLI / 观测（具体负责人及排期由主持指定）。
- **前提 / 位置：** `cmd/halro/stats.go:61-70,91-92`、`internal/app/metrics.go:301-304`。运行 interval stats，queue depth 不变或上升。
- **实际 / 证据：** 10→15/capacity 1024 输出 WAL queue 5 / 0；稳定非空输出 0/0。精确 delta/report 函数已复现。
- **已有防御 / 影响边界：** 下降分支用新值，raw Prometheus 和单次 snapshot 正确；没有实际队列容量变零。
- **最小修复方向：** 区分 counter/gauge，gauge 始终报告后一个样本。
- **回归验收：** 不变/增长/下降/新出现 gauge、capacity，以及单调和 reset counter。
- **来源 / 证伪者：** [原始证据](roles/reliability.md)；尚无独立证伪报告；本次汇总不替代独立复现。

<a id="rel-03"></a>

## REL-03 — 发布证据指南仍承诺已退役审批门禁

- **严重度 / 置信度 / 责任领域：** P2 / 高 / 发布文档（具体负责人及排期由主持指定）。
- **前提 / 位置：** `docs/verification/release-run-evidence.md:3-19,46-48`、`.github/workflows/release.yml:421-477`。操作者按指南推 tag 并期待独立 approval pause。
- **实际 / 证据：** 当前 publish 无 environment，也无 release-governance/M11 secret gate；另一指南已说明属于 1.0.0 target。部分 fuzz/bundle drift 也错写成 release graph 的门禁。静态对照确认。
- **已有防御 / 影响边界：** 现有测试、扫描、签名、attestation/checksum 真实存在；v0.x 治理门禁由 owner 决定退役。是操作文档不一致，不是应强加新门禁或已证实权限绕过。
- **最小修复方向：** 入口处标注目标/历史，保留当前可用 artifact 归档流程，准确区分 CI 与 release 依赖图。
- **回归验收：** 每条现在时门禁可定位现行 job/step，目标显式标注；文档核对即可，不新增无意义业务测试。
- **来源 / 证伪者：** [原始证据](roles/reliability.md)；尚无独立证伪报告；本次汇总不替代独立复现。

<a id="rel-04"></a>

## REL-04 — Pricing provenance 接受单侧 minute bound

- **严重度 / 置信度 / 责任领域：** P3 / 高（局部）；外部影响未验证 / Domain pricing（具体负责人及排期由主持指定）。
- **前提 / 位置：** `internal/domain/pricing_schedule.go:110`。直接验证 Source=base、Timezone=UTC、StartMinute=60、LocalMinute=30 等仅一侧指针的 provenance。
- **实际 / 证据：** 用 AND 检测边界存在，只拒绝双侧，单侧 Validate 返回 nil。局部 overlay 已复现。
- **已有防御 / 影响边界：** 正常 TierAt/snapshot 构造产生成对或无边界；价格使用冻结 rate 而非单侧 provenance。无远程注入、少计费或伪造 Ledger 被接受证据。
- **最小修复方向：** 禁止任何边界的分支使用 OR，要求完整 window 的分支继续 AND。
- **回归验收：** base/zone-unavailable 各一侧 pointer、两侧、均无及正常 window；扩大影响前先证明外部调用链。
- **来源 / 证伪者：** [原始证据](roles/reliability.md)；尚无独立证伪报告；本次汇总不替代独立复现。

<a id="sum-01"></a>

## SUM-01 — Usage summary 在 checkpoint 交接间漏掉已结算增量

- **严重度 / 置信度 / 责任领域：** P2 / 高 / Usage checkpoint / Admin summary（具体负责人及排期由主持指定）。
- **前提 / 位置：** `internal/app/runtime.go:1014-1044`、`internal/usage/checkpoint.go:194-195`、`internal/app/admin_usage_summary.go:289-292`。summary GET 与 drain→编码→bbolt commit 重叠。
- **实际 / 证据：** TakeCheckpoint 清空 pending，snapshot 尚未持久化，summary 只合并 store+PendingRollup。独立 authenticated handler 复现 requests 1→0→1、cost 90→0→90（micro-USD）。
- **已有防御 / 影响边界：** CatchUp 不会重放已消费记录；bbolt 原子 checkpoint、aggregate 锁、失败 ReturnCheckpoint 保护耐久性，未保护跨介质读的一致视图。暂时漏报，不是永久账务丢失或错误结算。fixture 暂停真实交接边界，未执行实际并发 bbolt commit。
- **最小修复方向：** 提供 stored/in-flight/pending 的一致读取协议或共享同步，避免补上 in-flight 后重复计数。
- **回归验收：** 实际成功/失败 commit 两侧屏障、跨读窗口、新增量并发，请求/费用不降不重；与直接 aggregate dashboard 对照。
- **来源 / 证伪者：** [原始证据](roles/deadman-adjudication.md)；独立 dead-man/summary adjudicator：[独立裁决](roles/deadman-adjudication.md)，CONFIRMED。

<a id="cfg-01"></a>

## CFG-01 — 模板承诺删除字段恢复默认但 Load 未全量合并默认

- **严重度 / 置信度 / 责任领域：** P2 / 高 / 配置 / 文档（具体负责人及排期由主持指定）。
- **前提 / 位置：** `internal/config/default.yaml:3`、`internal/config/config.go:683-699,1125`、`cmd/halro/main.go:205`。按模板承诺删除单个键后重启/加载。
- **实际 / 证据：** 独立 110 项单键删除 census：20 validation failures、77 normalized 相同、13不同。删除 read_header_timeout 拒绝启动；metrics.require_auth 删除在默认 loopback 无 credential 文件时变 false。
- **已有防御 / 影响边界：** 13 个结构差异并非全为行为差异：部分 accessor 恢复默认、disabled anchor 惰性。非 loopback metrics 强制 TLS/credential，credential 又要求 require_auth；无公网暴露或 payload/key 泄露证据。启动先校验，不是运行中的 timeout 静默关闭。
- **最小修复方向：** 最小修正文案，说明必填与真实 omission 规则；全局 default merge 属额外行为变更，需要明确 zero/false 兼容性。
- **回归验收：** 完整模板与单键/整节删除；显式 zero/false、loopback/nonloopback metrics、credential、anchor enabled；不能仅测 intact default 等价。
- **来源 / 证伪者：** [原始证据](roles/reliability.md)；独立 reliability/config reviewer：[独立裁决](roles/reliability.md)，CONFIRMED。

## 不计入上述缺陷数的争议、边界与候选

| 项目 | 状态与处理 | 证据 |
| --- | --- | --- |
| 历史 C2：read_only 是否应可读 failure payload | 当前授权行为 CONFIRMED；按现契约认定越权的解释 REFUTED，不赋漏洞等级。独立实际 router 证实 read_only 正文200及read审计，audit失败503不返回正文；产品可另行调整内容读取权限，决策仍开放。与SEC-04超期读取根因分开，不增加缺陷/独立根因计数。 | [独立安全裁决](roles/security-secondary-adjudication.md)、[Security](roles/security.md) |
| 历史 Casey 27 小时日 | 已证实 `2023-03-08T12:00:00Z` 的 PeriodAt 被 22–26h guard 拒绝；作为 P3 文档/历史支持边界保留，不新增生产缺陷 ID 或计数。2026 的 598 区域×366 日采样无异常，当前外部历史重算路径未建立。需修正“任何 IANA 区域都不触发”的断言并明确支持范围。 | [Reliability 后续 period 审查](roles/reliability.md) |
| Mantle Responses 首事件前 malformed/EOF ambiguity | UNVERIFIED，同根审计候选，未把 unary Chat 复现推广为该流式路径已确认。 | [Provider](roles/provider.md)、[独立裁决](roles/provider-adjudication.md) |
| Missing-zone 最高价 fallback、cache rounding 预留边界、timezone generation/epoch 极值 | 局部行为/调用前提与测试缺口；未建立预算突破、当前生产故障或外部可达非法状态。 | [Reliability](roles/reliability.md) |
| 临时 CLI empty | 按主持要求暂不纳入发现或裁决计数，等待主持最终决定。 | 无定级 |

其他资源耗尽、真实 KMS/receiver、完整对象迁移、实际浏览器和生产验收缺口保留在角色报告，不因门禁通过自动关闭。本文不新增历史事故断言，也不把架构建议、未提供功能或未执行云验收当作当前漏洞。

主持另补充当前内嵌 bundle 的部署正向流程：刷新模拟 `/models` 返回 `review-chat`，unknown 明确无能力且需声明；手动声明 Chat/Streaming 后保存为停用，toast 成功；启用先要求测试，测试后仍未设置价格。此为本轮直接提供的模拟 Provider 浏览器证据，支持未知能力提示与测试前置的观察，不证明真实上游能力、已定价、已成功启用或端到端推理验收，不新增 finding。
