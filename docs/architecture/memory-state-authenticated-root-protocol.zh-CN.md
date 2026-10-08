# D-03：认证根、按需验证与增量启动协议草案

状态：2026-10-02，三角色复核已完成；以下缺口已纳入草案，生产协议尚未闭合。本文把 [主方案 §4.1.1](memory-bounded-state-design.zh-CN.md) 的要求展开为候选协议，不代表生产格式已冻结或 I-11 已实现。存储后端及参数仍受 [D-02/D-04 决策](memory-state-storage-decision.zh-CN.md) 约束。

## 1. 选择与范围

候选采用“认证根＋不可变、可验证的有序页”，普通启动验证固定大小的 seal 与角色允许位置，查询或恢复遍历时逐页验证。运行更新产生新路径/新根，不能只认证旧快照后信任任意已改变的普通 DB。所有业务决定只能使用已验证的路径和值。

一个普通 bbolt 索引可以作为物理定位提示，但 Get 返回 nil 不能证明业务键不存在，Cursor 也不能证明没跳过记录。缺失的被引用页必须报损坏，不能解释为零余额、无重复事件或没有 pending。若用普通 DB 直接承载安全关键值，必须提供相同的认证查询/完整性协议，否则仅可用于完整重建的过渡模式。

本候选使用 SHA256、HMAC-SHA256、HKDF 的既有原语，不设计新的密码算法。页格式、有序索引和存储生命周期是需要独立评审与原型验证的系统协议；Merkle 根能确认发布者承诺的内容，不能证明发布者计算账务无 bug。

全历史未被读取就无法声称每个字节都已重新验证。为保持现有历史文件缺失检测，v1 候选启动门禁如下；不以按需校验默默放宽 readiness：

- 启动必检：seal/身份/允许前缀、活动日志及待回放后缀、可信目录中的全部权威日志文件存在与长度、metadata 认证基线和 epoch 关联、实际参与恢复/就绪决定的索引路径与值。目录检查用有界 cursor，不加载完整文件名列表；损坏或缺失未解决前不开放业务。
- 旧日志 frame 与未访问投影子树的完整字节审计仍是独立任务。认证根承诺内容，按需读路径必须验摘要；不能声称未读字节此刻已验证。记录审计 cut、已验证范围、进展及最后完成时间。D-04 必须规定完整审计期限、最低保留 I/O 预算及压力下超期处理；参数未决定前不得启用生产增量路径。审计不能无限让位于业务压力。
- 启动或运行发现任何安全关键内容缺失、摘要失败或日志谱系不一致，设置全局业务 readiness=false，拒绝新计费准入及依赖受损状态的查询/恢复；不返回假空或旧缓存答案。已接受工作的终结只能按已有故障/终结储备协议处理，不能继续使用不可信余额。独立诊断/故障说明接口仍可访问。

全部历史文件存在检查仍有 O(segment 数) 元数据 I/O；完整字节审计仍有 O(历史字节) I/O。它们与常驻内存有界、事实 decode/replay 增量是不同证明。D-03/I-11 必须分别报告这些成本；尚不能声称整个启动时间与目录规模无关。如果最终启动门禁要求进一步消除此线性目录成本，需要额外协议和明确语义决策，不能将本候选当作最终目标已经完成。

## 2. Seal 与密钥

seal 使用版本化、定长字段为主的规范二进制编码，整数为 big endian，末尾为 32-byte HMAC；不用可歧义的 JSON/map 序列化计算 MAC。解码先检查 magic、版本、长度与硬上限，再分配；未知关键版本/组件拒绝，不忽略。建议 v1 envelope 最多 16 KiB，属于安全解码上限，尚非吞吐/缓存预算承诺。

认证覆盖至少以下字段，全部在同一次发布中绑定：

| 字段 | 含义与校验 |
| --- | --- |
| format/schema/apply_algorithm/index_codec/key_derivation 版本 | 对应实际 reader；未知拒绝，升级有明确兼容门禁 |
| instance/cluster/member/incarnation | 来自既有启动身份与 Member Key Slot/受控备份规则；不能只与同一份未认证 DB 的任意值比较后称已验身份 |
| projection_generation 与 root_revision | reader/cursor/pin 的身份；根不可原地修改，旧 revision 不重新使用 |
| publisher role/term | 记录生成角色；可用性由当前认证仲裁的日志谱系和允许前缀判定，不能仅凭旧 role/term 开放业务 |
| common ordering cut 与其认证关联 | HA ordering header/baseline、index/offset/MAC/term、四个 store cursors/heads 与 Ledger Roll 进度；Standalone 明确无 ordering/peer，不编造确认 |
| 每个权威 store 的位置 | Ledger generation/sequence/offset/frame epoch/chain hash；Metadata epoch/file revision/sequence/offset/hash 与完整基线 ID；Governance 和 Audit sequence/offset/hash |
| 各完整逻辑索引的根与数量 | accounting、metadata、usage、governance、日志/segment 目录等安全关键键集合；不能把未覆盖组件声明为可跳过历史的起点 |
| auth coverage 与 legacy 状态 | 沿用 Ledger ADR 0016 的 authenticated/mixed/legacy 边界，不给旧 CRC-only 事实追授 frame MAC 证明 |
| tree/page/value 上限与 hash scheme | reader 用更严格的编译硬限及运行预算，不接受 seal 自己放宽上限 |

每个索引 root 包含组件 ID、索引 ID、页格式、完整键数及 PageRef；一个组件可有多个不同索引 root。PageRef 只含固定大小文件/pack 身份、偏移、长度与 32-byte 摘要，不包含任意路径。空树采用协议定义的组件特定空根表示，不能以“读不到 root 页”代表空树。

候选根认证 key 从**已保存 envelope 的稳定 Ledger key**经独立 HKDF 域派生，例如 `halro:projection:v1` / `halro:projection-hmac-key:v1`；不用每次从当前 Master Key 直接推导。Master Key rotation 对 Ledger key 仅重包既有 envelope，根认证 key 保持稳定；metadata 中 credential/MFA 密文也会改变，必须走 §5.2 的整文件发布协议，不能因此恢复旧 metadata 根；真正 Ledger key/purpose 升级则需要历史 key 读取/重新封装协议，不能默默使旧根失效。验证采用恒时 MAC 比较；清理和日志不输出 key 或内容。该设计需 key lifecycle 复核后才接入 runtime。

根 MAC 提供真实性，不单独提供最新性。选择旧合法根作起点必须完整保留并回放之后权威事实；日志尾部删除按既有 terminal anchor/HA confirmed-prefix 检测。回滚整个数据目录及所有本地证据时，无法仅凭同目录 root MAC 证明更晚事实曾存在；沿用外部 anchor/受控备份与 HA 仲裁边界，不能宣传新的抗整目录回滚保证。

## 3. 有序页与已验证查询

物理存储候选为 immutable pages（可存于受限 bbolt blob 层或 append-only packs）。节点/页数量在磁盘增长，常驻 cache、打开 pack/DB 数、路径栈及构建批次受 RAM/磁盘预算限制。bbolt Open/freelist 与 pack 目录均须额外验证有界，不能以逻辑 Merkle 树代替 D-02 的存储工作集决策。

- 页认证覆盖完整编码，包括 kind、组件/codec、边界、条目数量、键/值或 child refs。每次读取先按 PageRef 检查长度/偏移溢出与固定页上限，再计算摘要，最后解码；摘要不符或短读失败封闭。页/Blob 未经验证不能驱动索引选择或被 MAC 重新包装。
- leaf 以版本化 key codec 编码完整键，严格排序、无重复；value 内联或使用认证的有限 BlobRef。大 value 以受限块流式读取/验证，不先构造完整历史/无限块列表；领域解码另有尺寸/深度上限。
- branch 的 child 范围严格有序、无重叠，包含精确首尾键、数量及摘要引用；计数求和须无溢出。构建/修改时由已验证子树计算这些摘要与边界，不能从普通辅助索引猜测。
- `VerifiedGet(root,key)` 从受信任 root 沿验证后的范围到 leaf；只有完整路径证明 key 不在此范围或 leaf 中不存在，才返回 NotFound。物理定位失败/解码失败/证明失败属于 Corrupt/Unavailable。
- `VerifiedRange(root,[lo,hi),after,max_rows,max_bytes)` 按已验证树结构枚举，不先用普通 Cursor 找一批行再逐个验证 membership；后者无法证明中间没有被隐藏的行。止步与续读边界由真实结构决定，保留范围完整性。
- cursor 绑定 root generation/revision、组件、查询范围/tenant、codec 与最后已返回 key，解析有尺寸上限；root 被回收时返回 stale，而非悄悄转到新根。reader 每页释放物理事务/句柄，事务外编码/HTTP 等待；逻辑 root pin 的期限和数量纳入 I-03。
- 恢复保留一个固定输入根/高水位，分别枚举 pending attempts 和未 finalized requests；每批终结在新活动根上提交。输入根的保留不等于跨批持有 bbolt 读事务，也不允许无限积累 generation。记录 checkpoint 时只推进已可靠应用/确认的续读位置。

Ledger 真实域候选现有的事件首次物理位置 `event_positions` 与 request→attempt 关联 `attempts_by_request` 都属于安全关键键集合：前者驱动尾窗淘汰后的幂等判定，后者驱动两级恢复对关联终态的完整枚举。认证格式须绑定其值、完整键集合及与事件摘要/lease/settlement 的关系；删除或辅助 cursor 漏行不得成为“新事件”或“没有关联 attempt”的证明。当前普通 bbolt 过渡实现没有这些认证信用，见 [两级恢复候选证据](../verification/performance/2026-10-02-projected-recovery/README.md)。

页/值/树高的格式硬限、每请求解码量、cache/reader/pin 数量及磁盘上限分别记录。格式安全硬限不能变成默认生产预算；D-04 固定实际参数。随机读取、扫描、churn、压力下 cache/page-cache、生成/GC I/O 均要测。

## 4. Apply 与根发布状态机

选择全局已持久化/允许的前缀 N，所有需要参与此根的模块完成截至 N 的事实应用，并保存各原生 cursor。Usage 等 lagging 组件不能拿不同位置的最新数据冒充同一个一致 cut；若保留单独较旧的组件根，必须在 seal 明确位置与查询时效，并为安全关键恢复定义不可缺少的完整分支。首次 v1 优先使用一致 cut，减少跨组件解释空间。

| 状态 | 允许动作 | 崩溃后处理 |
| --- | --- | --- |
| FactsDurable(N) | 事实由各权威日志持久化；不先推进派生水位 | 从上一可用认证根回放；无根时只从合格认证基线＋日志有界重建，缺少基线则失败 |
| Building(N) | 在已验证旧路径上构造 COW 页，或从认证日志有界 bulk build；所有权明确 | 未发布页为本次所有的 orphan；受空间门禁后安全清理/重试，不删权威事实 |
| PagesDurable(N) | 本次新增页/Blob/必要目录已同步；旧引用继承已认证承诺；组装 seal | 没有有效活动 seal 就不能用新根；按旧根＋增量恢复 |
| SealPublished(N) | seal MAC、完整内容与位置绑定；临时文件同步、原子替换活动 seal、目录同步 | 只接受完整 MAC、发布耐久性及已使用路径校验通过的旧/新根；不宣称所有未访问页已作物理审计；部分发布拒绝 |
| RoleEligible(N) | 当前启动仲裁允许该谱系/前缀；Primary 本地已应用可合法领先 confirmed | Replica/晋升不得用越过允许前缀的根，重建新 generation |
| BusinessReady | 完成有界两级恢复、适用目标前缀确认、各位置/pin/预算校验 | 再次启动重新验证；先前 ready 不能授予本次服务权 |

发布时只验证本次新增页和变更路径，旧未访问子树继承认证引用；不得每次遍历所有可达页而重新引入 O(历史) Apply。发布不证明旧页此刻全部物理存在，检测与全局故障门禁按 §1 执行。页发布与 seal 发布之间允许 orphan，不允许“seal 指向未同步数据”。存储封装在同一事务时仍需证明 commit/根与页原子性；跨文件时页先 durable、seal 后发布，且恢复不能重用未发布数据作账务决定。生成期间写入可以继续，但新根身份与固定 cut 不改变；积压和同时保留根的数量有上限，不能无界 snapshot backlog。

写入前读取/修改的每个旧值、child ref 和不存在结论都必须已验证。禁止从被篡改的普通 DB 读取旧余额，然后计算一个新 seal 将错误重新签成可信内容。cache key 含根身份/页摘要，切换后不复用旧可见值；缓存失败不能留下领先于磁盘的余额。

活动根、构建根、恢复输入根、reader/备份 pin 计入同时保留空间。回收仅删除已证明不被任何合法 root/pin 引用且由本节点拥有的派生页；删除、mark/sweep、目录遍历均有界，并有中断幂等协议。权威日志的保留/压缩不因派生页回收而改变。GC 不能靠删除唯一恢复根或原始事实解决 ENOSPC。

## 5. 原生日志的增量初始化

不能直接把未经验证的位置传给 Open。为每个 store 定义由验证过 seal 派生的内部 resume token，绑定 log 类型、身份/epoch/generation、sequence/offset/chain hash、认证模式与当前允许前缀；普通 CLI caller 不能凭数值自行构造可信 token。

| 路径 | 当前源码行为 | 最终 resume 分支必须完成 |
| --- | --- | --- |
| Ledger OpenWithOptions | 载入/检查所有 segment，活跃文件从 seal tail 起扫描 | 认证 segment 根及 active generation 起点；从可信 cursor 验证增量/部分尾部，不丢 Roll/order-aware 修复；历史页/文件按需与完整审计检查 |
| Governance OpenWithOptions | scanFrom 从空 summary 开始扫描，随后 state checkpoint/replay | 从可信 summary/hash/offset 验证增量；同时 root 覆盖完整状态，而非只签旧 checkpoint 位置 |
| Audit OpenWithOptions | 从空 summary 验证文件，anchor 另行核对 | 从可信终端位置验证增量，保留尾部删除/部分 frame 规则及完整审计 |
| MetadataJournal Open/Attach | Open 从 header/trim anchor 扫描，Attach 对当前 bbolt 的 applied 位置协调 | 先认证 epoch/file revision/trim/resume 锚点和匹配的完整基线，再重放 suffix；不得从未认证 live DB 的 applied sequence 跳过事实 |
| OrderingJournal Open / StoreCursorsThrough / RecoverLocalCommits | Open 验证全日志；store cursor 与恢复从 index 1 重建 | 使用认证 header/baseline 与 cut 的 index/offset/MAC/term、四 store cursors/heads、Ledger Roll 进度验证后缀；允许前缀仍由当前 state/peer 仲裁，不由 seal 授予 |

Audit 的 `Summary.LastHash` 不能直接当成可独立验证的完整历史根：每帧携带前一帧 hash，但没有前一帧的物理定位；末帧可证明自己引用了某个 predecessor hash，却不能单独证明从 genesis 到该 hash 的每一帧仍存在且有效。`audit.Log.SealReference` 现在接受独立提供的 Summary cut，在固定 cut 上流式重验完整前缀，再返回最终帧偏移/长度/hash、基于首帧的 key-derived lineage ID 和原生位置；路径在扫描前后都必须仍指向同一文件，workspace 从共享 Manager 收取固定 512 KiB RAM。空前缀使用显式 domain-separated root。缓存 Summary、只认证末帧 MAC 或只比较文件长度都不能替代该全链校验。完整前缀扫描的 I/O 随历史字节增长，必须单独计入 D-04 审计期限/预算，不能被算作增量启动已经完成。此组件接口尚无 Runtime caller，不能作为已完成的 Audit 增量 Open。

Governance 的 `Summary.LastHash` 同样不是独立完整历史证明。`governance.Log.SealReference` 接受独立 Summary cut，在固定字节前缀上逐帧重验 sequence、previous hash、HMAC 和事件语义，返回 final-frame locator、key-derived lineage 和原生位置；工作集由共享预算的固定 512 KiB permit 覆盖，路径扫描前后必须仍映射同一 inode。空前缀使用显式 domain-separated root。该根仅覆盖 Outcome 原生事实，不覆盖可重建投影；全前缀 I/O 仍随历史增长，尚无 Runtime caller、可信增量 Open 或已授权跨重启恢复。

`governance.BuildAuthenticatedGovernanceProjection` 在上述 journal cut 上再从完整认证日志冷重建 Outcomes、revision/head、idempotency、project、work-unit 与 definition 索引；校验投影 Summary 与 source 完全相等后关闭 fresh bbolt，并把它的完整物理 image 流式导入独立认证根 index 2。返回的 index 1 原生日志 Seal 与 index 2 投影内容 Seal 使用相同 `SealPosition`，投影根的 scope/cut 同时绑定组件身份、Journal lineage/摘要/位置。scratch bbolt 的有限 disk ceiling 必须由同一 Manager 的 permit 覆盖到实际 inode 删除，复制进根 store 的 page 与 reducer workspace 另计。该构建是冷重建构件，不提供普通投影重开权、不形成其它组件的 common cut，也未接入 Runtime。

Metadata 的完整 root 范围至少包括所有权威/安全关键配置、价格/pin/幂等和 schema 相关键集合，并明确 node-derived/session/key-envelope 的覆盖与重建规则；现有 class 表是清单起点，不等于这些内容已经认证。未覆盖且仍参与安全决定的 live DB 不能作为新的可信快照起点。Vault 的 AEAD envelope/key-check 是解锁认证 key 的 bootstrap 例外，沿用现有验证及 Member Key Slot 规则，不信任任意未认证身份/位置。

Metadata 的认证备份基线在 index 1/codec 2 中保留一个专用 `0xff` 键，值为规范化的 `BaselineEvidence`；普通 `RowKey` 始终以 `0x01` 开始，因此该收据不会与 bbolt 路径键冲突。收据由已选中根的 pin 读取，`OpenFromRoot` 将其与调用方独立提供的备份/身份资格逐字段比较。收据行计入 COW 根完整键集合，seal schema 因此显式采用 codec 2。它保存来源证据，不选择 Seal、不建立角色许可，也不允许普通 Runtime 自动恢复。

无合格 root、legacy/版本不兼容、目录/页损坏、日志谱系冲突时，只能从实际存在的认证基线和权威事实有界重建；不能重建时明确失败，不 silently 接受新文件/空库。Replica 的允许前缀尚未仲裁前可构造受限对象/启动复制，但不开放业务；HA 维护复制先运行，再恢复与显式确认，不在离线准备阶段等待 peer。

### 5.1 Metadata 认证基线与首次迁移

现有 MetadataJournal epoch header 不包含整库初始内容；迁移、restore、CompactSnapshot 和 Master Key rotation 的 staged writes 不记录普通 journal。因此验证整个 journal 也不能证明 epoch 起始时未修改的预算、权限、价格等键。不能将当前 live DB 直接签名后称为“完整认证重建”。

每个 epoch 必须关联完整认证 baseline ID/root、schema、覆盖范围、稳定身份及密钥世代，并保存可重建材料；journal suffix 只认证该基线之后的事实。StartEpoch、trim、schema migration、整文件 restore 均保留这一关联。Trim 即使 epoch 不变也可能重写物理 offset，必须增加认证 file revision/trim locator 或可验证的逻辑位置映射；旧 token 不得盲用旧 offset。基线缺失、不匹配或覆盖遗漏则不获得可信起点。

已有部署的首次建根只有两类候选来源：已认证完整备份，或明确的受控基线接受流程。后者需要冻结写入、完整校验、记录来源/操作者/范围/摘要与接受证据，并明确仅建立接受时刻后的内容信任，不证明迁移前历史真实性。该流程及身份来源尚待安全/运维决策；不能在自动迁移中默默接受 live DB。隔离原型不执行此生产接受动作。

2026-10-04 已实现[认证备份 Metadata 内容基线构造器](../contracts/authenticated-metadata-backup-baseline.md)：完整加密来源二次认证后，仅选中 Metadata 文件；独立身份和 manifest/真实 TxID/位置匹配，全分类 raw rows、嵌套/空桶及 sequence 流式入认证内容根。实际 bbolt Close、源后验校验与 owned 清理完成后才返回候选。[本地 gate 与失败记录](../verification/performance/2026-10-04-metadata-backup-baseline/README.md) 支持继续 epoch 关联实现。该内容根尚未绑定当前 journal epoch/header/trim/resume、角色许可或完整 Seal；不授予 Runtime 启动资格，不能把它记为本节全部基线迁移要求完成。

后续[epoch 与 file revision 关联](../contracts/authenticated-metadata-epoch-binding.md)已接显式库接口：private root 的两项 bookkeeping 原子归一到新 epoch/sequence0，然后由 format2 native header MAC 绑定实际完整内容根；trim 保留 binding 并递增物理 revision，旧 revision/普通 Open/未限定的 HA entry 不获资格。默认 format1 writer/Runtime 未切换；源历史归约、trim cut 状态根、跨进程 selection/resume、物理 rewrite owner/费用及整文件发布仍未完成。该进展不替代首次生产接受或完整 §5.1 条件。

再后续[认证范围删除](../contracts/authenticated-metadata-range-delete.md)已提供 callback 内单次结构性剪裁：认证边界路径和父节点计数，完整覆盖子树不枚举 value，旧 pin 保持原引用；codec1 桶范围与真实 bbolt 删除结果一致。完整删除子树内的 bytes 没有因此完成历史审计；完整 Metadata native reducer、单帧接受契约及默认 Runtime 仍未接入。

后续[Metadata 整帧 delta](../contracts/authenticated-metadata-frame-delta.md)已有显式库接口：四类操作完整归约、既有 sequence 保留/新桶零值、整帧 bookkeeping、独立 1024×32 root mutation profile；先完成内容候选再 native durable 最后 root publish。静态 domain 与全源认证包含排除尾部，只应用显式允许前缀。扫描仍 O(历史字节)，生产格式迁移/跨进程选择/trim cut 根及完整 Seal/角色/默认 Runtime 未完成，不授予生产接受。

### 5.2 整文件发布与密钥轮换

稳定根 key 解决旧 MAC 可验证性，不能解决新 metadata 内容的真实性或密钥材料兼容性。必须从已认证输入构造 staging，发布新 epoch baseline/root；credential、Admin MFA、key envelopes 的重加密结果均在认证范围内。旧根只可作为变换输入，不得覆盖已轮换材料。

候选多文件协议绑定一个 durable operation ID、旧/新 metadata baseline、epoch、密钥世代及各文件摘要，状态依次为 `OldValidated → StagedDurable → PublishIntentDurable → MetadataPublished → KeyMaterialPublished → SealPublished → Complete`。所有 staging 与恢复所需旧/新密钥封装先同步；intent 经稳定认证 key 认证并绑定文件清单，先同步 intent 再变更活动文件。发布步骤和每次目录同步后记录进度；启动解锁只允许验证过 intent/既有 key-slot 规则所允许的旧/新封装，不从任意未认证文件猜测身份。

任何中断先恢复这个操作再进入日志增量和业务就绪，只能选完整可验证的旧状态或完整新状态，不能混合 epoch、密文、Master Key 与根。完整发布后才释放旧恢复材料；状态不清或无法解锁/验证时失败封闭。现有 rotation bridge 能提供哪些耐久证据、如何与此 intent 组合仍需实现前逐项证明；此处不是已完成的 crash-safe 协议。restore/compact/schema 整文件发布同样适用，不能只补 rotation 一条路径。

## 6. 备份、恢复、版本与故障验收

备份固定一个认证 cut/root，保存其全部可达数据、原生权威日志、原始价格/配置、ordering/密钥 material 与必要 reader。复制结束前 root pin 不释放；完整验证备份可达集合及日志关联后才发布备份 manifest。复制中的失败/中断只留下本次拥有的 staging，不得发布半备份。导出与备份的 reader/期限/空间按主方案 O-02 管理。

恢复先按既有 key-slot/identity 规则解锁，在当前角色允许谱系上选择合格根。拒绝将 Primary 的本地派生根直接当作 Replica 的本地事实；Replica 依本节点权威日志构建。旧根只作较早增量起点；不回滚后来事实。新 schema/codec reader 兼容矩阵和整体 HA 维护窗口在发布前固定。

D-03/I-11 的最少验收：

1. seal 任意身份/根/位置/版本字段或 MAC 改动、截断、非规范编码、未知组件与计数/偏移溢出均拒绝。
2. 单值篡改、删除关键叶、缺失 child/Blob、替换边界/数量、辅助 Cursor 隐藏一行；余额/去重/恢复/分页不返回假空，不漏行。
3. 合法旧 seal＋不匹配的已修改 DB、整文件替换、混合根/代际、cache 旧值均不变成新可信数据。
4. 事实 append/fsync、页/Blob 写与同步、seal rename/目录同步及 cache 可见更新各阶段 SIGKILL；新旧根均可安全恢复、不重计费用。
5. Master Key rotation 旧 MAC 可验证但旧密文根不越过密钥世代恢复；metadata/Master/seal 发布前后 SIGKILL、首次基线接受、epoch/trim file revision、restore/compact/schema 发布均验证；未认证整库不被重签。
6. local_applied 超过 confirmed、允许前缀缩小、Replica/晋升、整体维护中断与两级恢复；根/位置/确认边界与主方案一致。
7. 正常启动实测包括 ordering 与所有原生日志；历史事实 decode/replay、目录 stat I/O、全字节审计成本分别记录，不以有界 heap 宣称启动成本不增长。原型未消除线性目录检查前，最终启动门禁不计完成；后台审计在持续压力下仍有进展且按期完成。
8. 删除旧未访问子树后更新另一键并发布，随后查询/完整审计必须触发 §1 全局故障门禁；GC 与 build/publish/pin 获取/过期并发、中断时不误删新发布或仍被读取的页。
9. 100 万及增长历史的点查、缺失查、范围分页、bulk build/update/GC、长 pin、随机读、损坏解码、ENOSPC；记录 heap/RSS/cgroup/磁盘/时间与共享预算。

## 7. 索引候选调查与未决项

外部代码仅供候选调查，未修改根 go.mod、未导入生产依赖：

- [celestiaorg/smt](https://github.com/celestiaorg/smt) 提供 KV Sparse Merkle proof API，但维护者已于 2023-02-27 archive；不作为当前生产依赖选择。KV membership API本身也不能自动提供本方案的业务有序范围完整性。
- [cosmos/iavl](https://github.com/cosmos/iavl) 提供 persistent Merkle KV tree；仍需核对当前 API 的读路径认证、range 完整性、cache/版本保留和数据库 batch 原子性，不能把 Merkle 功能列表直接当作 Halro 的有界可信 reader 证明。
- 本候选的 immutable ordered pages 可对照 [Merkle B-tree 研究](https://arxiv.org/abs/2006.01994)，但不是已审核/采用的现成库，页格式、更新、proof/完整性与回收需原型和独立复核。普通整文件 MAC 可用于离线备份/过渡验证，不能解决最终增量启动的全量读取问题。

复核记录：账务与 HA 均确认 metadata 整库基线/whole-file publication 为生产 P1；容量确认历史缺失检测/readiness 一致性和继承引用与物理审计的区分，HA 指出 ordering 增量路径遗漏。上述修订补入候选契约，但未完成生产协议证明。三角色允许隔离页/Seal/VerifiedGet/Range 原型；不允许据此计 D-03 完成。

待闭合：可信身份来源、首次基线接受、整文件/密钥原子恢复及完整 metadata scope、legacy 与损坏检测范围、具体有序页格式/实现、resume token 与原生日志改造、跨组件 cut、key/reader/root 发布及回收边界。D-03 只有这些协议闭合且复核通过才计完成；原型不授予生产可信起点。

隔离实现与证据：[authroot 原型](../../tools/memory-state/authroot/README.md)、[本地验证](../verification/performance/2026-10-02-memory-state-authroot/README.md)。原型 root 并非本文生产 Seal，已增加隔离单 pack COW/根发布与六阶段进程中断测试（[磁盘证据](../verification/performance/2026-10-02-memory-state-authroot-disk/README.md)），未实施生产基线/整文件发布/HA/增量启动；页查询及规范空根局部复核通过不计 D-03/I-11 完成。

后续隔离原型已增加流式构建及有限 batch，将合成 KV sequence/hash 与内容在同一 root 发布（[证据](../verification/performance/2026-10-02-memory-state-authroot-build/README.md)）。该位置不是原生事实/ordering cut，不满足本节 Seal 的生产关联要求。10m KV 完整验证通过但 cgroup 触顶，缓存/分区门禁未过；不能由原型格式或局部资源曲线提前冻结生产协议。

## 7. Seal 编码与来源绑定实验（2026-10-04）

[独立 Seal 编码实验](../verification/performance/2026-10-04-state-seal/README.md)补充 §2 的可执行规范验证。私有 `HMPSEAL1` 为固定1802B：big-endian版本元组、四个最多64B且零填充的身份、角色/coverage、generation/revision/schema/publisher term、baseline ID/root、四类native cut、ordering/roll承诺、五个固定有序组件root以及末尾HMAC。硬解码上限16KiB；长度/版本先检验，正确MAC后才解码，未知组件、重复/遗漏组件、非法位置和非规范身份拒绝，不返回部分对象。reader须提供独立可信身份及支持schema，不能从同一payload自行取出身份作为比较依据。

publisher term单独绑定，不以ordering cut上的term代替：新任期可以发布较旧前缀，因此允许ordering term较旧，禁止其领先publisher term。Standalone保持无term。初版1794B遗漏publisher term的源码/测试切面保留，最终1802B版本由新的定向race/fuzz验证。

key从已保存Ledger envelope key经实验专用HKDF域派生；实际Vault AEAD重包测试保持稳定，未增加从当前Master Key重复派生的路径。该测试不是生产rotation bridge/Metadata密文/epoch多文件协议的故障验收。

真实Ledger fixture经过原生写入、完整认证和fresh磁盘投影，读取真实余额，绑定实际generation/sequence/offset/hash/epoch；验证余额页改写被拒绝，较旧合法Seal仍需回放后来事实。该fixture只填一项余额，其它组件、baseline及locator为合成数据，不证明完整键集合、Metadata初始真实性或HA ordering与四store关联。Seal格式/密钥域仍为隔离实验，没有导出API或Runtime依赖，没有产生原生日志resume token，也不改变D-03未闭合、I-11未实现和生产增量启用门禁。


## 8. 可复用认证读取组件与完整 Ledger 域 fixture（2026-10-04）

[认证读取契约](../contracts/authenticated-state-reader.md)及[本地证据](../verification/performance/2026-10-04-authenticated-state-reader/README.md)增加 `internal/authenticatedstate` 候选组件：固定160B根、完整键/Blob值、严格有序子树边界/数量/高度及逐路径校验，Get不存在证明与完整Range分页不借助普通DB cursor。游标MAC绑定根/范围/最后键；缺页或篡改拒绝整页并锁定该reader。构建、常驻reader、单查询和持久bytes接入显式共享逻辑预算，Close后持久费用保留，替换路径不误删。

真实Ledger经过105条原生事实、Roll/Compact、完整认证与fresh投影重建；29个bucket的447条实际键值流式写入并逐项对照重开分页，包含余额、event_positions和未完成request/attempt关联。该fixture验证完整Ledger域表示，既有普通source DB在生成期间被恶意改写的认证链仍未实现；不能据“fresh”或根MAC签名自动将它变成合格生产publisher。没有Runtime caller、resume token或跨组件Seal，没有改动默认启动。单pack格式/参数也未获生产冻结。

本轮仅完成认证读写基础组件的局部证据；D-03继续进行中，I-11的生产全组件快照/增量恢复尚未开始。下一步必须闭合可信source publisher与认证旧路径上的增量Apply/原子根发布，随后才能接原生日志初始化与全局故障门禁。Metadata基线、HA允许前缀、pin/GC/空间及D-04物理容量接受继续适用，不以此组件关闭任务。

## 9. 认证旧路径上的增量更新与组件根发布（2026-10-04）

[增量更新契约](../contracts/authenticated-state-cow.md)和[本地证据](../verification/performance/2026-10-04-authenticated-state-cow/README.md)补齐候选 Store 的内容循环：Transaction 从认证树读取旧值和不存在结论，在私有根上最多修改64次；只追加变更路径，将 cut 摘要、revision 与完整内容一起发布到固定240B活动根。页 Sync 先于根 Sync/Close/rename 和目录 Sync，失败不交付 receipt；旧 pin 保留旧内容，发布或完整性故障锁定全部 pin。重开独立核对 scope/cut 和 pack 身份，在 writer 锁内恢复合法未发布根及尾部；不完整或 foreign staging 保留并失败封闭。

共享预算保留整 pack 及固定控制区磁盘预约；关闭 writer 后持久费用不退，重开必须原子 retain 同 Manager 的合格预约。独立 reopened writer 真实排除清理，四个固定 pin 及并发 Close 的实际 join 已有 race 证据。100/10,000行单键替换分别读取7/10对象、写5/8节点、追加1,230/2,289B；这是局部更新算法证据。六阶段真实子进程 SIGKILL 后重开选择完整旧或新根；不代表断电或目标 Linux 接受。

本轮没有 Runtime 调用方，也没有将普通 live DB 签成生产源。Ledger 全部领域依赖、归约和索引尚未迁入该 Transaction，opaque scope/cut 不是生产身份或原生位置校验器，跨进程持久 owner 继承、根 inode 交接及 GC 未完成。D-03 仍开放，I-11 生产全组件快照和原生日志增量初始化仍未实现；不能依据组件 MAC 或 receipt 授予业务 ready 或 HA 晋升权限。

## 10. 真实 Ledger 原生事实直接构建认证状态（2026-10-04）

[Ledger 原生来源契约](../contracts/authenticated-ledger-native.md)及[本地证据](../verification/performance/2026-10-04-authenticated-ledger-native/README.md)进一步复用既有完整归约器：Bolt 和认证 Transaction 共用有界依赖读取、原有 State.Apply 及全部领域/索引更新。候选仅由规范 genesis 常量和完整认证的实际原生日志构建，没有普通 DB 输入。余额、幂等首位置、request/attempt 恢复、pending 时间/统计、WU/Run、cohort 与列表/export 索引随单个事实原子提交。新增认证 Floor 沿子树边界查找 cut 前最新 cohort 版本，无历史枚举；读取失败始终与不存在分离。

真实 fixture 包含149条原生事实、Roll/Compact/空active、29个bucket/795条键值、32个Run、别名/重复、先finalized后settlement、未解决request/attempt及超过Bolt键宽的身份。全量及六个允许cut逐行双向对照fresh原生磁盘参考，余额另作移除Run归属的旧State独立对照（旧State不能直接作为迟后归属恢复完整oracle）。错report/hash、frame内cut、excluded suffix不完整、压力、append限额、取消、foreign目录、pack损坏及Close/持久费用/真实Discard均有本地证据。源IO单独预约32MiB，与事务64MiB工作集同时计费；逻辑额度尚未物理校准。

候选的全部中间组件根留在私有新目录；完整源、实际允许边界及最终物理head确认后才可查询。此ready仅表示候选构建完成，不能成为业务readiness或HA授权。候选没有saved-state reopen、native resume或Runtime调用方；生产身份/typed全组件Seal、Metadata基线、全原生增量Open、完整业务查询、global故障门禁、跨进程owner/GC/原生最宽依赖与terminal储备及物理接受仍开放。D-03继续进行中，I-11与M3没有因此启用。

## 11. Live Ledger 认证位置追赶与完整后缀屏障（2026-10-04）

[原生追赶证据](../verification/performance/2026-10-04-authenticated-ledger-advance/README.md)补齐 live 候选的来源循环：从已认证的位置出发，同一 native 锁冻结两遍剩余后缀，先核对允许 record/roll cut，再仅归约允许事实，最后复核全部剩余 native 尾部。追加、压缩Roll与空active的物理位置可以推进，不开放任意Record Apply或saved-store reopen。追赶期间查询零排队拒绝；第二阶段任何失败永久退役候选，未发布的组件中间根不能成为接受的checkpoint。源IO、owner/native Close取消与真实退出后的退账均有本地race证据；已有Replica applier回归通过。

50/500条active历史各追加一条，live追赶均只解码两条后缀事实（两遍），旧事实没有重新归约。此计数不包含原生Open的全历史认证、目录元数据读取、压缩seek和旧字节审计，不能作为最终启动成本接受。候选现已明确拒绝任何epoch1–3 checksum-only来源；后续MAC不能认证之前CRC内容，伪造report计数也不能交付候选。默认兼容恢复保留，独立legacy基线接受协议仍待完成。D-03/生产Seal、Metadata基线、全原生增量Open及I-11/M3门禁继续开放，仍21项未闭环。


## 12. 合法宽 Run 定义与终身计数（2026-10-04）

[宽定义证据](../verification/performance/2026-10-04-ledger-run-dependencies/README.md)修复共享归约器的一条确定性额度缺口：RunCreated 不再为32-run限制读写全部旧Run，而使用从原生WorkUnitCreated/RunCreated事实生成的0～32计数，与定义、关联索引、digest和position在同一事务发布。关闭Run不释放终身容量；别名和重复不增加计数，缺失/非法计数及冲突seed失败回滚。WorkUnit/Run仅在对应mutation事件写入。候选scope升级为codec1:apply2，29桶不变；普通Bolt仍只fresh重建，没有导入或重信旧DB的路径。

实际MAC日志包含一个900KiB KeyID的WorkUnit与32个各900KiB KeyID的Run，全部行集双向核对fresh Bolt，完整State独立核对定义及生命周期；满容量别名/重复、关闭和独立WorkUnit的live追赶及Run33拒绝有本地证据。新计数只增加磁盘派生行，不新增历史常驻map。本轮不证明所有合法native依赖均满足额度，也不授予saved-state reopen/业务readiness；D-03仍进行中，I-11/M3及Linux物理容量接受仍开放，仍21项未闭环。


## 13. 固定认证输入根与两路恢复枚举（2026-10-04）

[恢复输入契约](../contracts/authenticated-ledger-recovery.md)与[原生证据](../verification/performance/2026-10-04-authenticated-recovery-view/README.md)增加live候选的固定根恢复视图：同一个认证pin读取position，分别完整分页pending attempts与未终结requests，request→attempt关联及lease/pending/settlement点依赖也来自该根。当前根在页间追赶并终结旧attempt/接受新请求时，输入两路保持原cut；不从pending空集推断request恢复完成，不用普通cursor作缺失证明。

真实原生fixture的输入sequence20含6个pending和7个未终结request，包含finalized但pending、无attempt及仅settled的request；current推进到23后两路输入及关联与旧cut的fresh参考一致。回调只占一个固定槽和64MiB逻辑workspace，页最多64行/8MiB及一次lookahead；实际Close先取消，再join整回调、查询、pin物理关闭与费用退役，不能提前退账。修改真实pending Blob不能交付部分页/假缺失，忽略错误/候选归约阶段advance失败/Complete后owner取消均不能返回成功scope。Too-small页/错cursor/入口压力只拒绝相应调用，不冒充内容损坏。

Complete只证明输入枚举及owner可用，不能证明终结写入、关联全部处理、HA许可或恢复业务ready。该阶段尚无Runtime消费者、saved-state reopen或完整Seal；后续组件重开进展见下节。生产deadline/pin回收/accepted-work终结储备和物理校准仍待接入。D-03继续进行中，M3未启用，仍21项未闭环。

## 认证 Ledger 组件重开：2026-10-04 本地实施

[组件重开契约](../contracts/authenticated-ledger-component-reopen.md)与[本地证据](../verification/performance/2026-10-04-authenticated-reopen/README.md)记录显式维护入口：保存根认证完整行树/键集合和cut，同cut position由认证点读验证；完整native MAC审计证明saved和当前许可边界，然后仅归约许可delta。真实跨进程新Manager、压缩/空roll/Replica显式许可与旧cut配新根等已本地验证。SourceVerified仍随完整历史增长，DeltaVerified与AppliedRecords另外报告；这不是§5原生日志增量Open已完成。

原发布者在borrowed后继实际成功Close后，经同Manager/namespace/pack/scope/lock/root receipt核对才可接管删除身份；借用者不获得删除权，Disk至实际目录回收/同步后退账。Discard在删除第一文件前检查整个固定namespace，替换根或外来名字不会导致先删pages。

这里只实现组件信任链，不替代完整Seal、当前身份/incarnation/角色-term仲裁、跨组件cut、持久根选择与发布、Metadata baseline/整文件轮换、历史审计时限和生产空间对账。普通Runtime不调用该入口；checksum-only遗留历史未追授MAC信用。I-11开始组件实施，生产增量路径/M3保持未启用，仍21项未闭环。


后续[Metadata 独立保存根重开](../verification/performance/2026-10-04-metadata-reopen/README.md)新增实际跨进程新 Manager 验证：独立保存完整 root identity、原生完整/保存/允许 head 与 backup/epoch binding，完整来源与排除尾部资格检查在候选归约之前；不匹配完整根先于 orphan repair 拒绝，partial native tail 保留。只归约许可 delta，native 已落盘而根未发布可补应用且重试不重复；同 Manager 两份持久费用借用、整个 factory IO 费用与真实关闭、成功后继清理 receipt 和失败后继拒绝已有本地证据。当前只接受未 trim epoch/revision1；trim cut 根/resume、完整 Seal/角色-term/密钥资格、持久 inventory/生产接入、所有原生日志增量初始化、历史审计和 Linux/生产接受仍开放。I-11 保持进行中，21 项未闭环，M3 未启用。


后续[Metadata trim cut-state 与物理 revision 关联](../verification/performance/2026-10-04-metadata-trim-state/README.md)已补齐：只裁已应用认证根的前缀，完整 native/域认证后将原 root identity、旧 frame start/end/hash/revision 绑定到新 anchor；内容根保持不变，保留未应用尾部，新 revision 的后续 delta 使用新物理位置。完整根与实际 anchor/独立 TrimState 一致才可重开，缺关联和落后于已删前缀拒绝；连续 trim/全裁后追加、实际 publisher 子进程退出后新 Manager 重开、真实 replacement 创建 receipt/成功后继接管、post-rename 取消与panic及实际Close join/整rewrite Disk费用已有本地证据。完整 Seal/角色-term/密钥资格、持久 inventory和跨进程生产删除、整文件发布/格式迁移、所有原生日志增量初始化/历史审计、完整故障与Linux/生产接受仍开放。I-11保持进行中，21项未闭环，M3未启用。

## 跨组件 Seal 编码候选（2026-10-05）

`internal/authenticatedstate/seal_candidate.go` 增加独立于 COW 根的 v1 候选 envelope：固定版本/身份/角色和 ordering cut 字段、五类 state cut、排序且有界的多索引逻辑根、Metadata baseline/file revision、coverage 状态、编码端 16 KiB 上限、独立 reader hard limits，并要求 caller 提供与当前 schema 匹配的完整根清单；Decode 拒绝 MAC 有效但缺少任一预期索引的 seal。另有 domain-separated HMAC-SHA256、严格拒绝未知版本/尾随字节/重复索引/不完整 state cut/不匹配 role-term。空逻辑根用显式标志及组件摘要表达，不伪装成缺失页。定向 codec 用例覆盖 canonical round-trip、多索引、根清单遗漏、MAC 篡改、截断、尾随内容、显式空根、reader 上限与 HA term 绑定。

此类型只证明候选格式可规范编码和解码；还没有 Runtime publisher/selector、真实 identity/key envelope 来源、Ordering/四 store common-cut 交叉校验、来源并发冻结、atomic publish/recovery、完整键集合审计或 role eligibility。它未冻结为生产格式，也不提供任何 resume token 或 readiness 权限。D-03/I-11 仍进行中，M3 不启用。

## Seal 候选的有界持久发布原语（2026-10-05）

`internal/authenticatedstate.SealCandidatePublisher` 将候选 envelope 推进到真实文件生命周期：要求持有同一 DataDir 的活跃 `.halro.lock`，从共享预算保留 active seal 与单个 replacement 的逻辑磁盘 ceiling；发布前为有界编码申请 RAM，独占创建 pending 文件，写入并 Sync 后才原子建立首个 active link 或按捕获 inode 替换旧 seal，最后 Sync 目录。相同身份严格递增 revision；新 generation/身份要求 revision 从 1 开始。第二个同进程 publisher、越序 seal、源 active 被外部替换均拒绝。

重开先认证当前文件并检查 hard limits/schema 清单，自动清理由 active 同 inode 的首次 link 中断遗留 pending；另外可在完整 MAC/schema 认证、严格后继校验与 active/pending inode 最终复核后删除尚未发布的完整后继 pending，绝不将 pending 提升为 active。部分、篡改、越序或不合格 pending 失败封闭并保留文件。rename/link 后的不确定错误锁住当前 handle，必须重开判定磁盘事实。关闭后可转交持久 Disk permit；退休要求仍持目录锁、active inode 未变、无 pending 文件，先 unlink/Sync 再释放费用。故障测试覆盖写入后/rename 后注入中断、重开选择完整旧或新 seal、重复 live publisher、序号跳跃、foreign active 替换和 foreign pending 保留。

`ValidateSealEligibility`/`SealCandidatePublisher.Select` 增加显式选择门：只有 Seal 的稳定身份、版本、当前 role/term、ordering 字段、coverage 和五个组件 cut 与调用方独立验证后提供的期望完全相同才会返回；root schema 仍由 reader hard limits 逐项核对。该首版采取 exact-cut，旧 seal 即使 MAC 正确也必须走独立后缀认证/恢复，不会自动选作起点。期望对象的来源仍由调用方负责，比较本身不验证成员状态、ordering MAC 或 native 日志。

`Store.SealRootReference` 现在从 MAC 验证过的当前 COW commitment 导出实际 pack ID、root page offset/length/digest 和完整行数；规范空 Store 输出显式 empty ref。它不扫描或验证所有页面，也不能替调用方证明所给 IndexID 对应了完整业务键空间；调用方需在静态 schema 清单中将其定义成 Store 的完整逻辑命名空间。

`ledger.AuthenticatedLedgerProjection.SealReference` 进一步把该引用接到真实 Ledger 候选：在 projection read lock 下读取认证的 native ReplayAnchor，再读取同一 COW root reference 与 RootIdentity，并核对 scope/cut/pack/行数/revision 后一次返回。Ledger 的 index 1/codec 1 明确代表由 schema-bound ordinal keys 构成的完整复合命名空间，不冒充各 bucket 独立 root。并发 Advance/Close 不能拆开 root 与位置；方法仍不接 Runtime，也不声称 Governance、Audit、Metadata、Usage/Ordering cut 已共同认证。

这是 Seal 文件的持久发布及候选匹配构件，不是跨组件 source-cut 证明器，也不核对五类日志 common-cut 的建立、可信 identity/key 来源、角色/term 仲裁、原生日志 resume、多 root 一体提交的生产目录方案或物理磁盘空间；尚未被 Runtime 调用。不能宣称 D-03/I-11 或可信启动闭环。详见[本地证据](../verification/performance/2026-10-05-seal-candidate-publisher/README.md)。

## Seal root 对 COW 重开的前置绑定（2026-10-05）

`StoreOptions.ExpectedSealRoot` 允许经独立 Seal 选择器批准的逻辑根，直接约束 `OpenStore` 实际接受的 COW commitment；`ExpectedSealSchema` 必须由本地版本化清单独立提供。Open 在任何 pending 清理或 pack orphan-tail 截断前，比较根的 component/index/codec、rows/empty、pack ID 与 root page offset/length/digest。候选根或 schema 不符返回恢复拒绝，不修复、不退费；完全匹配的根才继续原有 COW reopen。Ledger 组件重开入口可接收被选择的 Ledger 根，并由代码内固定的 Ledger index/codec schema 约束 Store 打开。

短回归覆盖每个根字段不匹配、独立 schema 不匹配、拒绝前保留尾部字节和磁盘费用，以及空根/非空根的成功重开；Ledger 与 Metadata 组件 reopen 都验证 selected root 错误拒绝且不清理尾部，正确根才能继续许可的 delta 恢复。authenticatedstate、Metadata 全包测试通过，OpenStore/Ledger/Metadata 接线定向 race 与相关 vet 通过。Ledger 全包测试曾发现两个既有目录索引测试夹具未适配当前严格 sequence range 和持久 sidecar 只读语义；已修正夹具并重跑对应测试，但本轮未重跑 130 秒的全 Ledger 包。该接口仍要求调用方已独立验证 Seal、scope、cut、key、角色与源资格；不执行候选选择、跨组件 common-cut 或 Runtime 恢复。它关闭“已选 seal 但 COW 打开可接受同 cut 另一逻辑根”的组件边界缺口，不能作为 D-03/I-11 整体验收。

## 认证分块镜像输出与真实崩溃复核（2026-10-05）

`Store.WriteImage` 从独立选定的完整逻辑根输出 ordinal 分块命名空间；固定 component/index/codec、精确当前根、行数与连续 chunk 长度共同约束输出。Usage/Governance 的 `WriteProjectionImage` 使用代码中固定 schema 与 64KiB chunk，调用方必须提供完整输出 bytes ceiling。没有普通 DB 输入，也不接受缺失 ordinal 或非规范中间块。选定根已落后、调用取消、限额不足、短写及目的 writer 错误均返回错误；失败输出由调用方丢弃，不能发布为成功镜像。

整个 writer job 持有同根 pin、reader RAM 和额外 chunk workspace，直到目的 writer 实际返回才释放；Store.Close 等待 writer 和 pin 的真实关闭。这里不能强制中止不响应取消的外部 writer。现有 Usage/Governance 实际 native fixture 输出为 bbolt 文件后，通过各自完整业务 bucket、位置及 Outcome 字段验证；这证明组件镜像可复原，不提供 Seal 选择、整文件发布或 Runtime 恢复权限。

Seal publisher 的 IO admission 现在先于 active 文件读取；重开 pending reconciliation 也在同一费用内。实际子进程 SIGKILL 覆盖初次 file Sync、link、rename/目录 Sync，以及 replacement file Sync、rename/目录 Sync，共 7 个时点；新进程只接受已发布的完整旧或新 envelope，完整但未发布后继可以认证后清理。MAC 不成立的 pending 不会被自动删除。

本地 macOS 与无网络、只读根、2GiB cgroup 的 Linux 容器功能复核通过；固定源 fingerprint 的矩阵结果及完整门禁见[本轮核对记录](../verification/performance/2026-10-05-memory-bounded-reconciliation/README.md)。镜像导出不是恢复初始化：Usage/Governance trusted reopen、增量 native Open、Metadata 整文件发布、Runtime 独立身份/密钥/角色与 common-cut 仍待实现和接受。

## 完整旧根选择和 live 物化（2026-10-06）

候选 wire v2 在每个 State 根中加入 scope、cut、完整 root-record digest、pack identity、revision、extent、height；独立 schema inventory 也绑定 Kind，不能删掉字段后降级为 native 根。完整记录、空 pack、root page 和声明的 page/height 限额均在 pending cleanup/orphan trim 前认证；v1 原型没有自动生产迁移权限。

Open 将 independently selected logical root 和实际 physical latest 分开。旧逻辑根控制读取集合，pending reconciliation/orphan trim 仍以认证后的 physical latest 为界。旧根上的新分支从 physical extent 后追加、使用 physical latest+1 revision，不覆盖 component 已领先的页。publisher cleanup takeover 绑定实际 physical successor；该机制没有授予跨 pack GC 或跨组件选择权威。

Usage/Governance 镜像保持原始 exclusive 输出 FD，bbolt opener 一次性移交该 FD，拒绝路径被替换或 truncation；conservative mmap/working RAM 保持到实际 DB Close，Disk 到 owned inode 删除。源 cut、所有 bucket/schema、DB 水位及同 Manager ownership 必须一致。增量恢复先认证 old→permitted→physical tail，再以有限批只应用 permitted delta；排除的 durable tail 也必须合法。应用之后发生错误则 generation 永久撤销，Close 取消并 join 实际 source job 和 reader/writer。当前 native source cut 证明仍完整扫描历史，不构成正常启动复杂度接受。

Metadata 从选定根恢复完整二进制 nested/empty buckets、sequence、原始 values 和 bookkeeping。整文件发布要求当前完整 receipt、同父路径、原始 candidate inode/content digest、独立捕获的旧 target inode 和 data lock。rename 成功后立即记为已发布，即使目录 Sync 报错也保留 uncertain commit 和 Disk owner，不能当未发布文件删除；调用者必须先退休旧 reader/writer。本接口不建立 HA ordering 权限或 common-cut。

Ledger 目录根完整收录选定 cut 内每个 sealed generation 与 active anchor，支持 ordinal point read、authenticated absence 和 future Roll 后重开旧 cut。Open 仍以完整 native Replay 证明 selected position；它不替代 `segments.json` 权威、默认 locator、文件删除许可或目录增量维护。

受影响回归、完整包结果及具名故障矩阵见[本轮记录](../verification/performance/2026-10-06-memory-bounded-resume/README.md)。Runtime identity/key/role selector、全组件 publisher、初始可信 baseline、native Open 增量和旧字节审计政策仍须实现/接受；不能据此关闭 D-03/I-11 或开启生产 M3。
