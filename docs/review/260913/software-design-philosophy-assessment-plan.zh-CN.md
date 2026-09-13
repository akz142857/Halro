# Halro 系统设计哲学评估方案

- 日期：2026-09-13
- 方案状态：**已制定，尚未执行**
- 建立方案时的仓库快照：`v0.8.0@1d48ecde216ef40e653738f8bdc9657f88a717cc`
- 评估对象：Halro 产品边界、架构、实现、运维体系、开发者接口与演进机制

> 本文件回答“怎样专业地评估 Halro”，不是对当前版本的评估结论，也不是发布
> `GO / NO-GO`。历史评审只能提供待复核线索，不能直接充当当前证据。

## 0. 执行摘要

本轮不是照搬 Google 的规模、Redis 的实现或 OpenAI 的组织形式，而是提取其可迁移的工程原则，
再用 Halro 自己的目标和约束裁决：

> Halro 是否仍是一个小而可靠、可自托管、边界清晰的 LLM 治理网关；它增加的每一份复杂度，
> 是否都换来了用户可感知、可验证、可运维的价值？

建议采用“**哲学假设 → 可证伪命题 → 代码与运行证据 → 独立反证 → 决策**”的方法。最终产出
不是一个孤立总分，而是：

1. 一张系统与信任边界图；
2. 一份带证据等级的 100 分维度评分卡；
3. 一组经独立反证的 P0–P3 findings；
4. 一份“保留、简化、删除、补证、延后”的架构处置清单；
5. 一份按 30 / 60 / 90 天组织的整改路线图；
6. 一个与发布决定相互独立的系统设计结论。

总分不能冲销红线。即使总分很高，只要存在账务错误、越权、秘密泄漏、静默数据误读、无界资源
消耗或已接受请求被不安全重试，最终结论仍必须是 `REMEDIATION REQUIRED` 或 `UNSAFE`。

## 1. 评估立场与边界

### 1.1 以 Halro 的真实承诺为准

本轮先以当前仓库声明的边界建立评估基线：

- 单个 Go 二进制，内嵌 React Admin；
- v1 为单进程、单写者、单数据目录，不假装已经具备多写者或集群一致性；
- Project 是预算、限流、授权、运行治理和未来分片的责任边界；
- Accounting Ledger 是账务权威，Usage / Parquet 等是派生视图；
- Provider 凭据保留在本地控制边界内；
- 对上提供 OpenAI / Anthropic 兼容面，对下适配多个 Provider；
- 安全默认、能力证据、保守结算、审计与可恢复性是核心价值；
- Halro 不是 Agent 编排平台、通用工作流引擎、模型训练平台或全栈可观测平台。

这些陈述在评估开始时必须重新从 README、ADR、契约、代码和运行行为核对。文档声明不是事实本身。

### 1.2 本轮要回答的问题

1. **产品是否聚焦**：新增能力是否强化 LLM 治理边界，还是把 Halro 推向“大而全控制台”？
2. **系统是否易于推理**：关键状态、写入权威、失败语义和生命周期能否被一个工程师完整解释？
3. **正确性是否优先于便利**：认证、账务、重试、恢复、能力解析与数据升级是否在不确定时失败关闭？
4. **可靠性是否被量化**：是否有用户视角的 SLI/SLO、容量边界、故障预算和可行动告警？
5. **开发者接口是否简单**：API、配置、错误、CLI 和 Admin 是否减少专有知识与意外行为？
6. **复杂度是否有预算**：依赖、抽象、状态、配置、Provider 分支和运维步骤的增长是否受控？
7. **证据是否匹配主张**：单测、fixture、短 smoke、真实 Provider、生产运行分别证明了什么？
8. **系统能否长期演进**：升级、降级、兼容、删除、迁移、回滚和供应链是否可持续？

### 1.3 不在本轮自动授权范围内

- 修改产品代码或顺手修复 finding；
- commit、push、创建 PR、tag 或正式发布；
- 触碰生产数据、现有真实数据目录或真实客户 secret；
- 调用可能计费的真实 Provider、真实 KMS 或外部告警接收端；
- 用历史绿色 CI、旧二进制或旧报告代替目标 SHA 的当前证据。

## 2. 参照哲学如何转化为 Halro 的检查项

以下是“借镜”，不是品牌权威排名。每条原则都必须落到可证伪的问题，不能停留在口号。

| 来源 | 可迁移原则 | 对 Halro 的具体审问 |
| --- | --- | --- |
| Google SRE | 简单性是可靠性的前提；用 SLI/SLO 与 error budget 管理风险；告警面向用户症状并保持可理解 | 单进程是否真的减少了故障面？关键旅程是否有 SLO？告警是否能驱动行动？发布速度是否由证据而非信心决定？ |
| antirez / Redis | 为程序员设计简单 UI；数据结构与状态模型优先；极简构建和运维；拒绝无成本的功能、依赖与兼容性破坏 | 新增 Provider / endpoint 要改多少处？API 是否暴露实现偶然性？一个能力能否被删除？构建、部署、恢复能否保持低认知负担？ |
| OpenAI 公开工程与风险材料 | 评估驱动开发；任务特定数据；持续评估；自动指标需与人类判断校准；区分能力、缓解措施与剩余风险 | Provider 兼容性与治理策略是否有真实分布、边界和对抗样本？“实现”“缓解”“已验证”“可发布”是否被分开记录？ |
| SQLite | 明确适用边界；自包含与低运维；通过 I/O、OOM、崩溃、损坏、边界与回归测试获取可靠性 | 单写者边界是否诚实且被硬约束？断电、磁盘满、截断和损坏时是否保持认证前缀？关键错误是否都有回归测试？ |
| Linux 内核 | 用户可见接口遵守“不引入回归”；必要时优先回退风险变更；兼容接口是一扇单向门 | Gateway API、配置、指标、事件、数据格式和 CLI 的既有使用场景是否变差？是否能定位引入回归的提交并安全回退？ |
| Go 生态 | 清晰胜过机巧；小接口、显式错误、组合优先；依赖和公共 API 需要长期成本意识 | interface 是否由真实替换点产生？错误语义是否被包装丢失？包边界是否降低认知成本，还是只把调用链切碎？ |

适用原则：

- 不因 Google 擅长分布式系统就给 Halro 的单进程设计扣分；若单进程能提供更强正确性和更低运维
  成本，它是优势。
- 不因 Redis 追求简单就排斥必要的安全、账务或恢复机制；真正要问的是复杂度是否有清晰职责和
  失败语义。
- OpenAI 的公开材料在这里用于抽取“评估驱动、风险分级、缓解前后分离、独立复核”方法，
  不宣称它们构成 OpenAI 唯一或完整的软件设计哲学。
- SQLite 和 Linux 的经验用于检查可靠性与兼容性，不要求 Halro 复制其技术栈或测试规模。

## 3. 评估前冻结的系统不变量

执行 S0 时由评审负责人复核、补充并编号。至少包括：

| ID | 必须成立的命题 |
| --- | --- |
| INV-01 | 一个 Project 的准入、授权、预算、Attempt 与结算在任一时刻只有一个逻辑写入权威 |
| INV-02 | Provider I/O 前 reservation 已持久化；最终 settlement 原子；未知上游结果保守结算且不自动重试到第二个 Provider |
| INV-03 | Ledger 是账务权威；缓存、bbolt aggregate、Usage 与 Parquet 只能派生，不能反向决定余额 |
| INV-04 | 认证、授权、预算、redaction、SafeTransport、能力约束和关键持久化失败均 fail closed |
| INV-05 | Provider 凭据、Gateway Key、未授权正文和原始来源标识不得进入日志、指标、错误、审计或非预期持久层 |
| INV-06 | “模型存在”与“模型能力”来源分离；未知或手填模型不因名称、endpoint 或字段缺失获得虚构能力 |
| INV-07 | 重试和 fallback 有界；向调用方发出首个响应字节后不得切换 Provider |
| INV-08 | 配置、路由、价格、能力、Offering 和 policy revision 在请求准入时形成足够的不可变快照，历史记录不被当前配置重解释 |
| INV-09 | 新二进制读取旧格式时正确迁移或明确拒绝；旧二进制读取新格式时明确拒绝，不能静默丢字段或重写 |
| INV-10 | 所有持久写、队列、缓存、capture、日志、指标标签、重试、分页和并发均有显式上界 |
| INV-11 | Admin 展示的是服务端事实与证据等级，未知不能显示成零、支持、已验证或成功 |
| INV-12 | Governance 不能改变 Ledger 历史、释放已发生费用、绕过 Project 预算或主动触发 Provider 调用 |
| INV-13 | v1 单写者是硬约束；HA / Cluster 的 Phase 0 结构不能让两个独立进程被误部署成“可用集群” |
| INV-14 | 对外契约、README、Admin、示例和实际路由保持一致；实验能力被清楚标记且不会混入 GA 承诺 |

若实现已经有意改变某条不变量，必须先提交新的架构决定与迁移影响，不能在评审中悄悄降低标准。

## 4. 评分、证据与结论模型

### 4.1 成熟度分数

每个检查项按 0–4 分评定：

| 分数 | 定义 |
| --- | --- |
| 0 | 与承诺冲突、没有控制，或存在可达的严重反例 |
| 1 | 依赖人工约定或局部实现，边界和失败行为不稳定 |
| 2 | 设计与主要实现存在，有针对性自动化证据，但异常路径或运维闭环不完整 |
| 3 | 实现、负向测试、故障测试和目标 SHA 运行证据完整，剩余风险明确 |
| 4 | 在声明适用范围内长期运行、外部或独立证据充分，回归与持续监控已闭环 |

分数 4 不表示“完美”，只表示该能力在当前声明范围内形成了可持续反馈环。

### 4.2 证据等级

| 等级 | 能证明什么 | 不能证明什么 |
| --- | --- | --- |
| E0 声明 | 目标、意图、文档存在 | 代码或运行行为正确 |
| E1 静态 | 代码路径、配置、依赖和结构可见 | 分支可达、故障时行为、环境兼容 |
| E2 自动化 | 确定 fixture 下的契约、负向与故障注入可重复 | 真实上游、真实基础设施、长期资源行为 |
| E3 目标 SHA 运行 | 二进制、浏览器、恢复、性能或短稳实验在记录环境成立 | 生产长期表现或未覆盖环境 |
| E4 外部 / 长期 | 真实 Provider、真实 KMS、多平台、生产 SLO 或长稳数据支持主张 | 未纳入样本的未来变化 |

评分约束：只有 E0/E1 的检查项最高 1 分；最高证据为 E2 时最高 2 分；E3 时最高 3 分；4 分必须有
E4 或等价的长期、独立证据。缺证据标为 `UNVERIFIED`，不能自动判成 BUG，也不能给乐观默认分。

### 4.3 权重

| 维度 | 权重 | 核心裁决 |
| --- | ---: | --- |
| D1 产品边界与价值密度 | 10 | Halro 是否仍在解决治理边界，而非堆功能 |
| D2 架构清晰度与复杂度预算 | 13 | 系统能否被推理、修改和删除 |
| D3 正确性、账务与数据耐久 | 16 | 关键不变量在故障与重启下是否成立 |
| D4 可靠性与可运维性 | 14 | 用户可见可靠性、恢复与告警是否闭环 |
| D5 安全与隐私 | 16 | 信任边界、最小权限、秘密和正文生命周期是否安全 |
| D6 API、Provider 与兼容性 | 10 | 程序员 UI、方言语义与证据来源是否诚实 |
| D7 测试、eval 与反证能力 | 8 | 测试是否能推翻设计假设，而不只是证明 happy path |
| D8 性能与容量诚实度 | 5 | 热路径、尾延迟、资源上界和容量声明是否有数据 |
| D9 Admin、CLI、文档与开发体验 | 4 | 人能否安全、顺畅地完成关键任务与诊断 |
| D10 供应链与可持续演进 | 4 | 依赖、构建、发布、升级、兼容和删除成本是否受控 |
| **合计** | **100** |  |

维度得分按已完成检查项加权；总分公式为
`Σ(维度权重 × 维度成熟度 ÷ 4)`。未评估项保留 `N/E`，同时给出覆盖率；不得通过只评容易项抬高
总分。复杂度、行数、接口数和依赖数只作为调查信号，不单独决定好坏，避免为了指标而重构。

### 4.4 总体区间与红线覆盖

| 总分 | 设计判断 |
| ---: | --- |
| 90–100 | 优秀：边界、证据和反馈环高度一致 |
| 80–89 | 稳健：可继续演进，有少量明确债务 |
| 70–79 | 可用但脆弱：需要有期限的结构性整改 |
| 60–69 | 高风险：复杂度或证据债务已影响可靠演进 |
| <60 | 失配：产品承诺与系统现实存在根本偏差 |

以下任一项覆盖总分：

- **P0**：可导致跨租户越权、秘密大规模泄漏、不可恢复数据/账务破坏或可远程控制关键边界；结论
  为 `UNSAFE`。
- **P1**：核心不变量可达地失效、静默数据误读、错误退款/重复 Provider 执行、默认不安全或无法可靠
  恢复；结论至少为 `REMEDIATION REQUIRED`。
- 未取得 D3 或 D5 的 E3 证据，或关键外部主张只有 fixture；结论必须带 `UNVERIFIED` 限制，不能写
  “生产已验证”。
- 评估覆盖率低于 80%，不输出数值总分，只输出阶段性诊断。

## 5. 十个评估工作流

### D1 产品边界与价值密度

检查：

- 为 Halro 写一句不可包含功能清单的产品定义，再让代码、导航和文档分别证明它。
- 建立全部主要能力的价值表：服务对象、用户问题、为何必须位于 Halro、替代方案、运行成本、删除成本。
- 对每个非核心能力执行“删除测试”：删除后是否损害治理边界，还是只损害“大而全”的观感？
- 检查功能状态是否清楚区分 GA、Beta、Experimental、计划和历史 PRD。
- 检查 Run Governance、business outcomes、media/resources、alerting、failure capture 等是否保持与账务、
  授权和证据边界的单向关系。

高风险信号：导航比核心心智模型更复杂；一个功能只有内部技术理由；实验能力被营销为稳定能力；
治理模块反向影响账务权威；长期存在没有用户旅程、owner 或退出标准的半成品。

### D2 架构清晰度与复杂度预算

检查：

- 绘制进程、listener、存储、Ledger、Governance Journal、Provider、Admin、Metrics 和 dead-man 的 C4
  Context / Container 图及数据所有权图。
- 从入口跟踪 6 条完整链：启动、Admin mutation、Chat unary、stream、资源异步任务、备份恢复。
- 量化包依赖、循环风险、接口/实现比、Runtime 初始化字段、全局 registry、配置键和跨包改动放大率。
- 对重复真相建立清单：Provider/Profile/Surface/Offering/capability、版本号、metrics、i18n、契约、前端
  catalog 是否各有唯一权威。
- 检查抽象是否有至少两个真实替换点或明确隔离价值；`manager/service/helper/common` 是否成为杂物层。
- 检查一个新 Provider、一个新 northbound endpoint 和一个新 durable field 各需要修改多少文件、多少
  registry、多少测试与文档。
- 建立复杂度预算：新增持久状态、后台 goroutine、依赖、配置键、公共 API、指标、告警和人工步骤时，
  必须同时声明收益、上界、故障语义和删除路径。

高风险信号：为未来 HA 提前引入可运行的多写语义；状态所有权需要跨多份文档拼接；一项新增要同步
修改多个手工表；接口只为 mock 存在；删除功能比新增更困难。

### D3 正确性、账务与数据耐久

检查：

- 为 reservation、attempt、settlement、finalization、replay、governance apply 建立状态机和非法跃迁表。
- 覆盖发送前失败、已发送未响应、首字节后失败、caller cancel、timeout、retry、fallback、进程崩溃、
  磁盘满、partial write、fsync 失败和重启恢复。
- 从 Ledger 重建所有派生视图并按 Event ID、金额、token、Provider 归因和 watermark 对账。
- 验证旧版本 → 当前版本 → 旧版本的双二进制升级/拒绝行为；检查 bbolt、WAL、Journal、Parquet、
  capture、backup 与 config 每一种 durable format。
- 做边界值、整数溢出、时间边界、跨日预算、revision race 和并发 admission 测试。
- 把历史最严重 defects 作为回归种子，但以当前代码重新复现，不能引用旧结论代替测试。

必须产出：状态机、故障矩阵、数据权威表、恢复实验日志、逐项不变量裁决。

### D4 可靠性与可运维性

检查：

- 先定义 4–6 个用户视角 SLI：准入成功率、端到端成功率、首字节/完成延迟、账务完整率、恢复时间、
  配置变更生效时间；不要从已有 metrics 反向拼 SLO。
- 为不同请求类型和部署方式设 SLO 草案，并明确哪些由上游 Provider 导致、哪些仍由 Halro 负责。
- 设计 error budget 与发布/变更策略；当前没有生产流量时先给测量方案，不伪造数字。
- 审核 golden signals、dead-man、readiness、Accounting health、日志和 Admin 是否支持 15 分钟内完成
  “发现 → 定位 → 安全动作”。
- 执行 kill -9、磁盘满、只读目录、损坏尾部、证书替换、Provider 慢/断流、告警端故障、重启和
  backup/restore 演练。
- 列出全部 toil：初始化、升级、轮换、证据归档、目录发布、告警配置、Provider 适配；区分能自动化的
  重复劳动和应保留的人类风险决定。

高风险信号：`healthy` 被当作外部可达；告警只描述内部组件而不说明用户影响；恢复步骤依赖作者记忆；
SLO 全部写成 100%；为了“自动化”把风险接受也自动化。

### D5 安全与隐私

检查：

- 更新资产、主体、入口、出网、存储和信任边界图；对 Gateway/Admin/Metrics/Provider/KMS/backup
  分别列攻击者能力。
- 对认证、Project scope、Admin role、CSRF、来源 CIDR、Provider credential、Gateway Key、MFA、
  审计与 restore 做权限矩阵和负向测试。
- 沿 prompt、response、failure body、source IP、run evidence 和 secret canary 跟踪内存、日志、
  metrics、WAL、bbolt、Parquet、capture、backup、Admin 与 heap 的完整生命周期。
- 复核 SafeTransport 的 DNS、IPv4/IPv6、redirect、proxy、custom endpoint、云元数据与 rebinding。
- 检查默认值、降级路径、诊断功能和 feature flag 是否在异常时扩大数据暴露。
- 对每个缓解措施写“绕过假设”，由非原作者尝试证明它无效；明确缓解前风险、缓解后风险和剩余风险。

安全 finding 必须给出完整可达链；只有可疑分支而没有入口和绕过条件时标为候选，不能直接称漏洞。

### D6 API、Provider 与兼容性

检查：

- 把 Gateway API、Admin API、CLI、config、metrics、webhook、durable format 都视为用户接口。
- 对 OpenAI / Anthropic facade 建立请求字段、SSE 生命周期、usage、错误、取消、幂等和限制的差异表。
- Provider 枚举只回答“谁存在”；能力必须来自可追溯的声明、metadata 或探测。逐条检查证据来源标签。
- 在写 decoder 前读取真实响应或已归档原始响应；自制 fixture 只能验证本方假设，不能证明上游事实。
- 使用官方 SDK 做契约对照，并明确“SDK → Halro facade 通过”不等于“真实 Provider 通过”。
- 检查未知字段、未知模型、部分响应、畸形 2xx、stream 中断、Retry-After、request ID 与错误正文泄漏。
- 审核兼容承诺：已发布行为变差即回归；确需 breaking change 时必须有版本、迁移和拒绝语义。

程序员 UI 测试：一个不了解内部包结构的工程师，能否仅凭错误、文档和 introspection 在 30 分钟内
完成接入或定位失败？

### D7 测试、eval 与反证能力

检查：

- 每个核心主张先写 eval objective，再确定数据集、指标、阈值和运行频率。
- 数据集同时包含典型、边界、历史回归、合成攻击、真实匿名样本和上游变化样本；记录来源与偏差。
- 区分确定性单测、property test、fuzz、mutation、故障注入、SDK 对照、浏览器旅程、短 smoke、soak
  和真实 Provider；每类只承担它能证明的结论。
- 检查测试是否断言用户可见结果和持久状态，而非只断言 mock 被调用。
- 对高风险控制做 mutation / sabotage：移除一次 revision 校验、改变一个错误分类、跳过一次 fsync、
  放宽一次 capability gate，测试是否必然变红。
- 从生产/真实 smoke 的新失败持续扩充 eval；自动裁决与人工复核定期校准。

反模式：泛化总分、只测 happy path、snapshot 代替语义断言、测试与实现共享同一错误常量、短 smoke
冒充长稳、用“看起来正常”代替成功标准。

### D8 性能与容量诚实度

检查：

- 以同机、同工具链、同 fixture 比较前一发布版与目标 SHA；记录原始样本和置信区间。
- 测量 p50/p95/p99、吞吐、分配、RSS、goroutine、FD、WAL/Journal 增长、恢复时间和 Admin 查询上界。
- 对 streaming 慢读者、Provider 慢响应、大 body、多 Project、capture、Parquet export、backup、审计
  与高基数输入做容量曲线，而非只测单点峰值。
- 找出最先饱和资源与拒绝行为；容量声明必须说明硬件、配置、数据形状和可靠性代价。
- 前端比较首屏 gzip、lazy chunks、渲染大表和弱设备交互；包体增长必须能追溯到用户价值。

### D9 Admin、CLI、文档与开发体验

检查：

- 走完首次初始化、创建 Provider → Deployment → Route → Project → Key、调用、诊断、禁用、备份、
  恢复、轮换和升级旅程。
- 验证危险动作的预防、确认、审计和恢复；错误必须说明发生了什么、影响什么、下一步是什么。
- 检查键盘、焦点、读屏、200% 缩放、320/768/1440px、深浅主题与中英文语义一致性。
- 文档按“契约、当前事实、历史方案、待办、验证证据”分类；过期方案不能看起来像现行行为。
- 让一名非作者按文档从零完成部署与恢复，记录每一个需要口头补充的知识点。

### D10 供应链与可持续演进

检查：

- 审核直接/间接依赖、许可证、漏洞、锁文件、构建可复现性、SBOM、签名、provenance 和归档证据。
- 建立所有“单向门”清单：公共字段、稳定 ID、metric、event kind、schema、config key、CLI flag。
- 检查小步、可二分、可回退；生成物必须由源码重建，不能手工合并。
- 为依赖、后台任务、持久格式和公共表面设增加/删除趋势；每季度解释净增长。
- 对 roadmap 中 HA/Cluster、更多 Provider、更多资源 API 执行“最后负责任时刻”评审，避免为未证明需求
  预付永久复杂度。

## 6. 必做端到端场景

| 场景 | 注入 / 操作 | 必须观察的结果 |
| --- | --- | --- |
| Fresh start | 空临时目录启动与 Web setup | 安全默认、明确下一步、无隐式外部依赖 |
| Governed call | 完成最小配置后调用 fake Provider | auth、capability、budget、attempt、settlement、usage 全链一致 |
| Accepted unknown | Provider 接受后返回畸形 2xx 或断流 | 不 fallback、不重复执行、保守结算、错误可诊断且不泄密 |
| Crash matrix | reservation/started/settlement/checkpoint 各边界 kill | 重启后不漏账、不双计、权威与派生视图可对账 |
| Storage faults | ENOSPC、partial write、fsync error、损坏尾部 | 拒绝新 Provider I/O，保留认证前缀，恢复步骤明确 |
| Secret canary | key-like、prompt、tool args、上游 prose 注入全链 | 仅允许的加密位置可见；日志/指标/错误/非授权 Admin 为零泄漏 |
| Upgrade / rollback | 上一 tag 数据 → 当前二进制 → 上一二进制 | 正确迁移或明确拒绝；原数据不被静默污染 |
| Resource pressure | 慢流、大正文、高并发、队列满、capture 满 | 上界生效，拒绝可解释，恢复后资源回落 |
| Operator incident | 给非作者一个失败症状 | 15 分钟内凭告警、状态、日志和 runbook 定位并选择安全动作 |
| Extension test | 纸面模拟新增 Provider/endpoint/durable field | 改动面有限，唯一真相清楚，负向测试默认纳管 |

全部实验使用隔离临时目录、fake service 和无效域名。真实 Provider / KMS / 外部接收端单列为可选的
E4 阶段，取得明确授权后执行。

## 7. 执行阶段与退出条件

| 阶段 | 工作 | 产物 | 退出条件 |
| --- | --- | --- | --- |
| S0 范围与主张冻结 | 记录目标 SHA、tag、工作区、工具链、功能表面、历史开放项、公开承诺 | `scope-and-claims.md` | 评估对象唯一；文档事实与待验证主张分开 |
| S1 系统建模 | C4、数据权威、状态机、信任边界、关键旅程、复杂度清单 | `system-map.md` | 每项持久状态、入口、出网与 owner 有位置 |
| S2 独立静态评审 | 各维度先独立阅读代码和文档，不共享中间 finding | `roles/*.md`、候选 findings | 每个判断有文件/行号/调用链或明确缺证据 |
| S3 定向反例与自动化 | 先跑能看见目标的窄测，再做 fault/fuzz/mutation/SDK/browser | `evidence/gates.json`、原始日志 | 核心不变量有正向、拒绝、故障和恢复证据 |
| S4 目标 SHA 运行评估 | 真实二进制、升级恢复、性能容量、运维旅程、短稳 | `runtime-evidence.md` | 每项有环境、命令、退出码、样本、限制；不以 fixture 冒充外部证据 |
| S5 独立证伪 | 非 finding 作者从入口尝试反驳 P0/P1 与结构性 P2 | `adversarial-verdicts.md` | 裁决为 CONFIRMED / REFUTED / PARTIAL / UNVERIFIED 并重新定级 |
| S6 综合与决策 | 评分、覆盖率、红线、复杂度价值表、处置优先级 | `assessment-report.md`、`scorecard.md` | 分数可追溯；缺证据不伪装成通过；设计结论明确 |
| S7 路线图 | 把处置分为保留、简化、删除、补证、延后 | `roadmap.md` | 每项有 owner、成本、风险、验证方式与 30/60/90 天窗口 |

若评估期间代码或目标分支移动，静态结论可以保留，但所有目标 SHA 运行证据和最终评分必须重新绑定到
新的 SHA。不要把两个候选的证据拼成一个“绿色”结论。

## 8. 角色与独立性

推荐最少 4 名评审者，允许一人承担多个角色，但以下独立性不可省略：

| 角色 | 责任 |
| --- | --- |
| A 产品与架构 | D1、D2、D9；产品边界、心智模型、复杂度与演进 |
| B 正确性与数据 | D3、D6；状态机、账务、Provider 语义、升级恢复 |
| C SRE 与性能 | D4、D8；SLI/SLO、故障、容量、告警与 toil |
| D 安全与隐私 | D5；威胁模型、权限、秘密、正文与供应链攻击面 |
| E 测试与证据 | D7、D10；eval 设计、证据质量、持续门禁与发布链 |
| F 对抗裁决者 | 不参与对应 finding 的发现或修复，专门寻找防御、不可达条件和反例 |

单人执行时按角色顺序分轮完成，并明确写“未取得人员独立复核”；不能把同一个人的两次阅读称为
交叉验证。严重度不因多个评审者感觉相同而提高，只因完整调用链、复现和影响范围而提高。

## 9. Finding 与裁决格式

每条 finding 使用以下固定字段：

```markdown
### PHIL-XXX — 简短标题

- 类型：DEFECT / DESIGN_DEBT / EVIDENCE_GAP / ACCEPTED_CONSTRAINT
- 维度与原则：D?；对应参照原则
- 严重度：P0 / P1 / P2 / P3
- 置信度：HIGH / MEDIUM / LOW
- 状态：CANDIDATE / CONFIRMED / REFUTED / PARTIAL / UNVERIFIED / ACCEPTED
- 入口与可达条件：
- 代码与文档证据：文件:行号
- 最小复现与原始证据：
- 违反的不变量或用户承诺：
- 用户影响、爆炸半径和可恢复性：
- 已有防御与反证：
- 建议处置：KEEP / SIMPLIFY / DELETE / PROVE / DEFER
- 建议回归或验收：
- 成本、owner、期限：
```

分类纪律：

- 缺少真实 Provider 凭据是 `EVIDENCE_GAP`，不是自动的产品 BUG；
- 文档与代码不符是独立 finding，不能选择更乐观的一边；
- 已知边界只在明确、强制、可观测且用户不会误解时才是 `ACCEPTED_CONSTRAINT`；
- 建议新增抽象、依赖或服务前，必须先证明现状的具体失败以及较小方案为何不够；
- 修复建议不能比问题本身引入更大的永久复杂度。

## 10. 最终交付目录

建议保持当前 `docs/review/` 约定，在执行时使用新的日期目录，不覆盖本方案或 260911 历史报告：

```text
docs/review/<YYMMDD>-design-philosophy/
  scope-and-claims.md
  system-map.md
  scorecard.md
  findings.md
  adversarial-verdicts.md
  runtime-evidence.md
  assessment-report.md
  roadmap.md
  roles/
  evidence/
```

`assessment-report.md` 建议固定包含：

1. 一页结论：系统设计 verdict、总分、覆盖率、红线和五项最高杠杆决定；
2. “Halro 应该是什么 / 不应该是什么”的边界裁决；
3. 十维评分与证据等级；
4. 复杂度投入与用户价值对照；
5. 经反证后的 P0–P3 findings；
6. 已验证、未验证、外部依赖和具名风险接受；
7. 30 / 60 / 90 天路线图；
8. 与 release readiness 的独立关系。

## 11. 投入方案

| 档位 | 投入 | 能得到什么 | 明确缺失 |
| --- | ---: | --- | --- |
| 快速诊断 | 2–3 人日 | S0–S2、关键链静态检查、候选风险和初步复杂度图 | 无完整运行、独立证伪或可信总分 |
| 专业评估（推荐） | 8–12 人日 | S0–S7、本地隔离运行、故障矩阵、性能基线、独立证伪与正式报告 | 不含付费 Provider、真实 KMS、24h soak、多架构实机 |
| 外部验收级 | 15–25 人日 | 推荐档 + 授权的真实 Provider/KMS/告警、多平台、24h soak、非作者操作演练 | 仍不是无限期生产 SLO 历史 |

3–5 名评审者可并行缩短日历时间，但不能压缩故障实验、soak 或独立裁决所需的真实等待时间。

## 12. 执行纪律

- 先读调用链和不变量，再跑测试；一个看不到改动或主张的测试不构成证据。
- 迭代阶段运行最窄的受影响测试；完整 Go / frontend gate 在目标 SHA 上集中运行一次。
- Go 证据必须使用 `-count=1`；不要把缓存的 `ok` 当作当前证据。
- `web/` 变化必须重建并检查 `internal/webui/dist`，生成 bundle 不手工编辑或合并。
- 原始命令、环境、工具版本、退出码、耗时和日志位置全部结构化记录；不要通过管道误读退出码。
- 测试环境失败与产品失败分开记录；loopback、cache、Docker socket、DNS、凭据权限等先验证环境边界。
- 历史评审 finding 只能作为 regression seed；当前裁决必须绑定当前代码和复现。
- 不在评估报告里隐藏局限，也不把 owner 接受的风险改写成技术验证通过。

## 13. 完成定义

只有同时满足以下条件，才能称本轮“专业评估完成”：

- 目标 SHA、范围、公开承诺和工具链已经冻结；
- 十个维度全部有结论，评分覆盖率至少 80%，未评估项可见；
- 关键不变量均有 E2 以上证据，D3/D5 至少达到 E3 或明确写入限制；
- 所有 P0/P1 和结构性 P2 均完成非原作者证伪；
- 每项实验可复现，历史证据与当前证据分离；
- 最终报告同时给出肯定、问题、已有防御、剩余风险和不建议做的事；
- 路线图包含删除与简化项，而不只是新增代码；
- 系统设计 verdict 与 release `GO / NO-GO` 分别陈述；
- 没有用总分、自动化绿色或品牌类比掩盖红线问题。

## 14. 一手资料

以下链接于 2026-09-13 核对。它们提供方法来源，不替代 Halro 自己的代码与运行证据。

- Google SRE：[Simplicity](https://sre.google/sre-book/simplicity/)、
  [Embracing Risk](https://sre.google/sre-book/embracing-risk/)、
  [Monitoring Distributed Systems](https://sre.google/sre-book/monitoring-distributed-systems/)、
  [Testing for Reliability](https://sre.google/sre-book/testing-reliability/)
- antirez：[Programmers are not different, they need simple UIs](https://antirez.com/news/107)、
  [Writing system software: code comments](https://antirez.com/news/124)、
  [Disque 1.0 RC1](https://antirez.com/news/100)
- Redis：[RESP protocol specification](https://redis.io/docs/latest/develop/reference/protocol-spec/)
- OpenAI：[Evaluation best practices](https://developers.openai.com/api/docs/guides/evaluation-best-practices)、
  [Updated Preparedness Framework](https://openai.com/index/updating-our-preparedness-framework/)、
  [External testing](https://openai.com/index/strengthening-safety-with-external-testing/)
- SQLite：[How SQLite Is Tested](https://www.sqlite.org/testing.html)、
  [Appropriate Uses For SQLite](https://www.sqlite.org/whentouse.html)、
  [About SQLite](https://sqlite.org/about.html)
- Linux Kernel：[Reporting regressions](https://www.kernel.org/doc/html/v6.9/admin-guide/reporting-regressions.html)、
  [Getting the code right](https://cdn.kernel.org/doc/html/latest/process/4.Coding.html)
- Go：[Effective Go](https://go.dev/doc/effective_go)、
  [Code Review Comments](https://go.dev/wiki/CodeReviewComments)、
  [Module release and versioning workflow](https://go.dev/doc/modules/release-workflow)
