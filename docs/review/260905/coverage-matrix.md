# 覆盖矩阵 — 2026-09-05

基线 `381743f6613607dc256828f4776b52af8bdd232c`。依据[批准计划](review-plan.md)、授权交叉读取的角色报告（含新增主持接手与安全二次裁决）、主持本轮状态说明及三份机器清单。本文件汇总证据，不重新执行测试，不修改源代码。缺口见 [blind-spots.md](blind-spots.md)。

**当前不能给出无条件发布结论：SEC-01、SEC-02、PROV-01 均独立确认 P1；浏览器局部真实旅程已执行、FE-01已复现，MFA/降权/异步未执行；30分钟smoke、offline backup/restore及SIGKILL重启实测已完成（见最终runtime证据）。真实 provider/KMS/生产、2–4小时/24小时、实际云报警未验证。** 核心/账务与网关两个 agent 自动中断，主持接手源码审阅；不能称这两项已独立专项完成。独立 P1 裁决不等于全模块独立审查。

## 口径与证据等级

- 状态沿用计划：待评审 / 评审中 / 已验证 / 发现问题 / 阻塞 / 不适用；括号注明证据层。源码“已验证”只代表列出的静态事实，绝不代表运行通过。
- “已验证（限定）”只适用于列出的本地测试不变量；同一模块的未覆盖路径仍开放。“发现问题”与已有成功测试并存；复现测试退出0常常是成功证明缺陷，而不是不变量通过。
- 主持共享证据：当前完整 Go、vet、web typecheck/test/build、race gateway/budget/ledger 均通过；本文件未重跑或独立审计其原始日志。早先 Go cache/httptest socket 环境失败已由后续成功运行取代，不能归为产品缺陷。
- 主持报告 Go/Node/Python 官方 SDK 对本地 compatibility server 均通过：仅传输/序列化子集，非完整 runtime、非真实 provider。Node/Python有Anthropic流；Go仅Anthropic unary；客户端未调用 count_tokens，资源/deferred/native扩展并非全覆盖。
- 清单是覆盖分母，不是功能或测试数量：44个 `internal` 一级模块（子包折叠），不是54个Go包；153条注册行不是153个独立功能。相同path在不同listener、不同method及参数化CRUD需要保留上下文；也不能由计数推导覆盖率。

## 报告索引与独立性

| 证据 | 已完成范围 | 使用约束 |
| --- | --- | --- |
| [security](roles/security.md) | 安全专项与角色资源矩阵 | 初稿中的“裁决待完成”由下方独立报告更新 |
| [provider](roles/provider.md) | 协议/profile/模型证据与23条推理路由 | 角色SDK未运行；采用主持新状态但保留范围限制 |
| [frontend](roles/frontend.md) | 前端重点状态/交互；5个P2本地复现 | jsdom不是浏览器；未逐行认证所有UI文件 |
| [reliability](roles/reliability.md) | 告警/deadman/CLI/发布；追加配置与period/pricing独立复核 | 初稿小结不包含后来追加项；以全文为准 |
| [provider-adjudication](roles/provider-adjudication.md) | PROV-01真实适配器＋metered gateway确认P1 | 内存transport；非实际付费 |
| [security-session-adjudication](roles/security-session-adjudication.md) | SEC-02实际logout/后续HTTP/race确认P1 | 没有窃取cookie或测量攻击成功率 |
| [security-rotation-adjudication](roles/security-rotation-adjudication.md) | SEC-01 file及fake-KMS确认P1 | KEK rewrap排除；该角色未执行注册对象HTTP/backup drill；主持现已完成限定backup演练 |
| [deadman-adjudication](roles/deadman-adjudication.md) | REL-01与summary交接窗口各P2独立确认 | 不证明永久丢账/真实接收器持久性 |

| [core-and-runtime](roles/core-and-runtime.md) | 主持完整核心主链及小模块源码/测试映射 | 非独立核心专项；全故障排列未验 |
| [security-secondary-adjudication](roles/security-secondary-adjudication.md) | SEC-03/04独立确认P2；C2越权解释证伪 | 四路MFA/密码与capture HTTP/审计对照 |

## 44模块覆盖

S/T为清单生产文件数/测试文件数，只描述文件存量。每行保留局部证据和未完成边界；不把共享门禁当作逐函数独立审阅。

| 模块 | S/T | 审阅责任与独立性 | 实际阅读范围 | 运行/其他证据 | 状态 | 限制/缺陷 |
| --- | ---: | --- | --- | --- | --- | --- |
| `internal/adminauth` | 3/3 | [security](roles/security.md)；[security-session-adjudication](roles/security-session-adjudication.md) | 会话/MFA/刷新撤销 | 定向防御、race＋真实 logout 路由复现 | 发现问题 | SEC-02 P1；SEC-03 P2；完整账号切换浏览器待验 |
| `internal/alert` | 1/1 | [reliability](roles/reliability.md) | dispatcher/队列/重试/关闭 | 本地定向测试 | 已验证（限定） | 云接收、业务拒绝、长期去重基数未验证 |
| `internal/anthropicapi` | 2/2 | [provider](roles/provider.md) | DTO/native/流格式 | 重复字段/native/usage 定向测试 | 已验证（限定） | 非任意 native/工具组合或真实上游 |
| `internal/app` | 69/126 | 主持＋[security](roles/security.md) / [reliability](roles/reliability.md) / [provider](roles/provider.md)；独立裁决 | runtime/start/close/activation 主持；Admin/security/provider 接缝专项 | 共享门禁＋定向路由/轮换/会话/summary 复现 | 发现问题 | 大包非全分支独立审阅；backup实际演练已完成；大规模/全故障排列为测试缺口 |
| `internal/audit` | 1/1 | [security](roles/security.md) / [reliability](roles/reliability.md) | 完整日志、意图、列表、anchor | 篡改/错 key/部分尾帧/拒绝未审计 payload | 发现问题 | SEC-05 P2 源码确认；未测大历史容量/写失败重试 |
| `internal/auth` | 2/2 | [security](roles/security.md) | Gateway key/snapshot | 撤销/旧快照/认证定向测试 | 已验证（限定） | 完整端点×双项目×同项目不同 Key 未闭合 |
| `internal/backup` | 5/1 | 主持＋[security-rotation-adjudication](roles/security-rotation-adjudication.md) / [reliability](roles/reliability.md) | 归档/对象收集/运维恢复契约 | 源码/共享Go＋offline backup/restore及负向控制 | 已验证（小型fixture限定） | restore+ready 0.504s非生产RTO；非物理断电RPO0 |
| `internal/bearercred` | 2/1 | [security](roles/security.md) | 凭据轮换/重叠/撤销/恢复 | 定向本地权限与状态测试 | 已验证（限定） | 真实外部见证恢复未验 |
| `internal/budget` | 3/6 | 主持；[reliability](roles/reliability.md)（period 独立）；[provider-adjudication](roles/provider-adjudication.md) | manager 预留/结算主持；period/resolver 数学专项 | 共享 Go/race；period 定向；真实 metered 分支复现 | 发现问题 | PROV-01 跨层；period 历史27小时日限制；非独立账务专项完成 |
| `internal/buildinfo` | 1/0 | [reliability](roles/reliability.md) | 链接版本注入/构建身份 | 源码/构建配置审阅；共享构建 | 已验证（源码） | 清单无专属 test；未逐架构产物验证 |
| `internal/circuit` | 1/1 | 主持 [core-and-runtime](roles/core-and-runtime.md)（非独立专项） | 完整manager：once lease/half-open/Abandon | 共享Go＋追加窄race退出0 | 已验证（源码+共享tests限定） | 长期target churn/map容量与全面公平性未量化 |
| `internal/compatibility` | 15/23 | [provider](roles/provider.md) | manifest/native/协议映射；含子包 | 定向合同测试；共享三语言 SDK | 已验证（限定） | SDK 子集；compatibility/openai 角色 selector 无测试命中 |
| `internal/config` | 2/4 | 主持＋[reliability](roles/reliability.md) | 载入/默认值/验证/启动契约 | 110项省略实验；共享 Go | 发现问题 | 模板省略承诺 P2 独立确认；非全部配置组合 |
| `internal/contentscan` | 1/1 | [security](roles/security.md) | 完整扫描器/拒绝边界 | 可执行/归档/非法文本定向测试 | 已验证（限定） | 上传协议端到端与大文件压力未闭合 |
| `internal/deadman` | 7/3 | [reliability](roles/reliability.md)；[deadman-adjudication](roles/deadman-adjudication.md) | 完整 engine/store/HTTP/audit/anchor | 包测试＋真实 Run 调度 overlay/race | 发现问题 | REL-01 P2 独立确认；物理 crash/真实接收器未验 |
| `internal/domain` | 24/17 | [provider](roles/provider.md) / [reliability](roles/reliability.md)；主持非provider部分 | capability/profile；pricing/timezone 数学；其余主持 | 定向 capability/price/时区及数学 oracle | 发现问题 | REL-04 P3 单边 provenance；非所有 domain 类型穷举 |
| `internal/durable` | 1/0 | 主持；[security-rotation-adjudication](roles/security-rotation-adjudication.md) / [reliability](roles/reliability.md)接缝 | SyncDirectory/持久化调用链 | 源码审阅；共享 Go 间接路径 | 已验证（源码） | 无专属 test；磁盘故障/物理断电未验 |
| `internal/failurecapture` | 1/1 | [security](roles/security.md)；[security-rotation-adjudication](roles/security-rotation-adjudication.md) | 写/读/清理/当前 key 生命周期 | 真实 store TTL/file/fake-KMS 复现 | 发现问题 | SEC-01/04；已登记资源和过期 HTTP 全场景待补 |
| `internal/gateway` | 6/18 | 主持；[provider](roles/provider.md) / [provider-adjudication](roles/provider-adjudication.md)接缝 | admission/attempt/settle/retry/deferred 主持接手 | 共享 Go/race；metered真实适配器复现 | 发现问题 | PROV-01；自动中断后无完整独立网关专项 |
| `internal/gatewayapi` | 3/6 | 主持；[provider](roles/provider.md)协议接缝 | 已注册 ingress/资源 DTO/error/SSE | 共享 Go；SDK compat server 子集 | 已验证（源码+共享tests限定） | 非完整 runtime 所有端点/权限/取消旅程 |
| `internal/hostsecurity` | 5/1 | [security](roles/security.md) | 所有平台生产文件 | Darwin 源码审阅；共享门禁 | 阻塞 | Linux prctl/容器/进程硬化需目标系统运行 |
| `internal/id` | 1/0 | 主持 | ID 生成辅助函数 | 源码审阅；共享 Go 间接使用 | 已验证（源码） | 无专属 test；不据此宣称统计碰撞实验 |
| `internal/idempotency` | 1/1 | 主持 [core-and-runtime](roles/core-and-runtime.md)（非独立专项） | 完整store.go：1–128可见ASCII；资源幂等在bolt事务 | 共享Go＋主持完整源码/调用链 | 已验证（源码+共享tests限定） | 没有第二套持久化生命周期；资源全故障排列未执行 |
| `internal/kms` | 8/6 | [security](roles/security.md)；[security-rotation-adjudication](roles/security-rotation-adjudication.md) | executor/AWS边界/fakekms/DEK/rewrap | 定向状态机/AWS mock；fake-KMS 轮换复现 | 发现问题 | SEC-01 DEK；真实 IAM/KMS/CloudTrail 阻塞 |
| `internal/ledger` | 7/13 | 主持；[provider-adjudication](roles/provider-adjudication.md)结算接缝 | Apply/账务权威主链 | 共享Go/vet/race；恢复seq5020、kill后认证5025、再次调用停机5030 | 已验证（源码+共享tests限定） | 无完整独立账务专项；崩溃/认证重放/灾备矩阵不可由 gate 代替 |
| `internal/limiter` | 1/2 | 主持 [core-and-runtime](roles/core-and-runtime.md)（非独立专项） | 完整manager：原子准入/释放、deferred槽、债务 | 共享Go＋追加窄race退出0 | 已验证（源码+共享tests限定） | 长期项目churn与全面公平性未量化 |
| `internal/logging` | 3/2 | [reliability](roles/reliability.md) | 全生产文件/轮转/回退/阈值 | 定向测试通过 | 已验证（限定） | 慢 stderr/既存不安全权限/磁盘满未验 |
| `internal/masterkey` | 3/3 | [security](roles/security.md)；[security-rotation-adjudication](roles/security-rotation-adjudication.md) | 文件/keyslot/bridge/代际 | 定向状态转换/错 key/轮换控制 | 发现问题 | SEC-01 跨层；全中断矩阵/云恢复待验 |
| `internal/modelcatalog` | 5/4 | [provider](roles/provider.md) | catalog/manager/snapshot/trust | unknown/conflict/签名/过期/sequence 测试 | 已验证（限定） | 真实签名发布物/trust root/账户能力未验证 |
| `internal/openaiapi` | 3/3 | [provider](roles/provider.md) | Chat/Responses/resources DTO | unknown/native/usage 定向测试 | 已验证（限定） | 数值极端/任意 schema fuzz 未跑 |
| `internal/provider` | 23/44 | [provider](roles/provider.md)；[provider-adjudication](roles/provider-adjudication.md) | 全部家族清点，重点传输/枚举/能力；含子包 | 定向模型/路由/边界；真实 adapter 内存 transport | 发现问题 | PROV-01 P1；全部 profile 实际上游未验 |
| `internal/redaction` | 4/9 | [security](roles/security.md) | engine/inspect/stream/jsonescape | 有界 stream/策略定向测试 | 已验证（限定） | 跨通道总量、native端到端、完整 fuzz 未验 |
| `internal/requestmeta` | 2/0 | 主持 [core-and-runtime](roles/core-and-runtime.md)（非独立专项） | 完整request/source：私有context key、Unmap/IsValid、空ID | 共享Go间接＋主持完整源码 | 已验证（源码+共享tests限定） | 无专属test；入口代理来源信任按部署验证 |
| `internal/safelog` | 1/3 | [security](roles/security.md) / [reliability](roles/reliability.md) | 完整结构化属性遮蔽 | E5及日志定向测试 | 已验证（限定） | 任意自由文本无秘密不作保证 |
| `internal/safetransport` | 1/1 | [security](roles/security.md) / [provider](roles/provider.md) | 完整传输/DNS/IP/代理/重定向 | 定向 pinning/拒绝测试 | 已验证（限定） | 真实云网络/所有签名边界未验 |
| `internal/semantic` | 5/3 | [provider](roles/provider.md) | request/content/result/event | stream validator identity/终止/tool 限制 | 已验证（限定） | 总缓冲、多通道极端/全schema未验 |
| `internal/sourcelimit` | 1/2 | 主持 [core-and-runtime](roles/core-and-runtime.md)（非独立专项） | 完整limiter：分钟窗/IPv6聚合/16384来源/overflow共享预算 | 共享Go＋追加窄race退出0 | 已验证（源码+共享tests限定） | overflow公平性为明确降级；长期churn/全面公平性缺证据 |
| `internal/sse` | 1/2 | [provider](roles/provider.md) | decoder incremental framing | io.Pipe CR-only 复现 | 发现问题 | PROV-02 P2；非真实provider使用该分帧的证明 |
| `internal/store` | 17/24 | 主持；[security](roles/security.md) / [security-rotation-adjudication](roles/security-rotation-adjudication.md) / [deadman-adjudication](roles/deadman-adjudication.md) | bolt/lock 折叠；会话/密钥/usage 接缝 | 共享 Go；会话/summary/rotation overlay | 发现问题 | SEC-02及summary交接；双进程/写失败完整矩阵待验 |
| `internal/timezone` | 1/1 | 主持；[reliability](roles/reliability.md)领域关联 | 时间区工具；period/price 接缝 | 共享 Go；领域时区定向 | 已验证（限定） | 完整历史tzdata/替换/冷启动浏览器时区未验 |
| `internal/tokenguard` | 1/4 | [security](roles/security.md) | 完整 manager/loader | atomic lease/checkpoint/缺政策拒绝 | 已验证（限定） | 多源长期洪泛/状态基数未验 |
| `internal/usage` | 8/13 | 主持；[deadman-adjudication](roles/deadman-adjudication.md) | collector/checkpoint/rollup/offline | 共享 Go；summary HTTP 1→0→1 复现 | 发现问题 | summary P2 独立确认；非持久化丢账；重建/大规模压力待验 |
| `internal/vault` | 6/2 | [security](roles/security.md)；[security-rotation-adjudication](roles/security-rotation-adjudication.md) | 全生产文件/envelope/HKDF/作用域 | tamper/binding；file/fake-KMS轮换复现 | 发现问题 | SEC-01；不宣称堆内所有副本清零 |
| `internal/webui` | 1/1 | [frontend](roles/frontend.md) | 嵌入handler/Vite/artifact扫描 | 源码；共享web/Go；实际bundle diff退出0 | 已验证（生成链/局部浏览器） | 局部浏览器已验；MFA/降权/异步、完整asset故障路径未验 |

## 分母之外的交付面

| 范围 | 证据/归属 | 状态与边界 |
| --- | --- | --- |
| `web/` | frontend；共享typecheck/test/build | 发现问题 FE-01～05；真实浏览器局部已验，完整旅程未完成 |
| `cmd/halro`、`cmd/halro-deadman` | reliability CLI阅读/定向stats；主持start | 发现问题 REL-02；离线命令执行不因dispatch源码阅读自动成立 |
| `tests/compatibility` | provider读脚本；主持三语言SDK运行 | 已验证（本地harness限定）；非全部端点/profile矩阵 |
| `catalog/`、模型发布工具 | provider签名/信任边界；reliability发布脚本 | 已验证（局部本地）；真实发布物/能力真实性阻塞 |
| `deploy/`、observability、工作流、release工具 | reliability规则验证、Python、govulncheck | 已验证（本地结构/规则限定）；REL-03文档冲突；云交付未验 |
| configs/runbooks/docs/历史性能档案 | reliability＋主持；rotation裁决 | 配置省略承诺发现问题；历史源hash可核对，不是新负载成绩 |

## 依赖接缝

`dependency-edges.json`有152条原始内部/资源依赖边。下表按44模块折叠子包、去掉模块内自边；这是静态依赖邻接，不是动态调用次数、故障注入覆盖或架构缺陷判定。configs/runbooks保留为非internal资源边。app集中装配；provider/compatibility有子包；这些事实本身不构成BUG。

| 起点 | 折叠后的直接依赖 | 接缝证据边界 |
| --- | --- | --- |
| `internal/adminauth` | `internal/domain` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/app` | `configs`, `docs/runbooks`, `internal/adminauth`, `internal/alert`, `internal/audit`, `internal/auth`, `internal/backup`, `internal/bearercred`, `internal/budget`, `internal/buildinfo`, `internal/compatibility`, `internal/config`, `internal/domain`, `internal/durable`, `internal/failurecapture`, `internal/gateway`, `internal/gatewayapi`, `internal/id`, `internal/kms`, `internal/ledger`, `internal/masterkey`, `internal/modelcatalog`, `internal/provider`, `internal/redaction`, `internal/safelog`, `internal/safetransport`, `internal/sourcelimit`, `internal/store`, `internal/timezone`, `internal/tokenguard`, `internal/usage`, `internal/vault`, `internal/webui` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/auth` | `internal/domain`, `internal/id` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/backup` | `internal/buildinfo`, `internal/durable`, `internal/ledger`, `internal/store` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/budget` | `internal/domain`, `internal/id`, `internal/ledger` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/compatibility` | `internal/anthropicapi`, `internal/domain`, `internal/openaiapi`, `internal/semantic` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/config` | `internal/domain` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/failurecapture` | `internal/durable` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/gateway` | `internal/anthropicapi`, `internal/auth`, `internal/budget`, `internal/circuit`, `internal/compatibility`, `internal/contentscan`, `internal/domain`, `internal/durable`, `internal/failurecapture`, `internal/id`, `internal/idempotency`, `internal/ledger`, `internal/limiter`, `internal/openaiapi`, `internal/provider`, `internal/redaction`, `internal/requestmeta`, `internal/semantic`, `internal/tokenguard` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/gatewayapi` | `internal/anthropicapi`, `internal/gateway`, `internal/id`, `internal/openaiapi`, `internal/provider`, `internal/requestmeta`, `internal/sse` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/ledger` | `internal/domain`, `internal/durable`, `internal/provider` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/limiter` | `internal/domain` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/logging` | `internal/config`, `internal/safelog` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/masterkey` | `internal/config`, `internal/vault` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/modelcatalog` | `internal/domain`, `internal/safetransport` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/provider` | `internal/anthropicapi`, `internal/compatibility`, `internal/domain`, `internal/openaiapi`, `internal/safetransport`, `internal/semantic`, `internal/sse` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/redaction` | `internal/domain`, `internal/openaiapi`, `internal/semantic` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/store` | `internal/domain`, `internal/ledger`, `internal/masterkey` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/tokenguard` | `internal/domain` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/usage` | `internal/domain`, `internal/durable`, `internal/ledger` | 对照模块行；import存在不证明事务/生命周期契约 |
| `internal/vault` | `internal/durable` | 对照模块行；import存在不证明事务/生命周期契约 |

重点接缝：app→gateway→provider 的 post-acceptance 错误/重试/结算为 PROV-01；app→adminauth→store 的刷新/删除为 SEC-02；app→vault→failurecapture/provider对象的代际为 SEC-01；usage→store 与 app summary 的交接为独立 P2。budget→ledger 的原子性、usage offline认证重建、durable与backup完整快照现有小型fixture实测；完整故障排列不能由静态边、共享race或单次演练完全闭合。

## 所有注册路由关联

清单有153条、按 method/path 去重150组；保留153条以避免不同listener的health被误合并。Gateway 26条（23条推理/资源、2条health、1条根路径），Admin 124条，metrics 3条。它们是注册条目，不是独立功能数量。源码还用 `Handle` 挂载 `/admin` 与 `/admin/*`，不在该method提取清单中，文末补列。端点注册不保证当前配置监听、授权或存在可选profile。

协议类别与证据代码：

| 代码 | 合约/归属 | 可认定状态 |
| --- | --- | --- |
| G | provider端点/profile矩阵＋主持gateway/gatewayapi | 已验证（注册/局部协议）；完整runtime逐端点运行仍有测试缺口 |
| D | G＋主持deferred生命周期＋security-rotation-adjudication | 实验性；SEC-01发现问题；完整已登记资源读/取消/重启待验 |
| X | G实验性media/files/batches/Bedrock资源 | 已验证（注册）；本地SDK未覆盖；上游真实行为阻塞 |
| A | security全Admin角色路由扫查＋frontend相关UI＋主持app | 已验证（注册/角色wrapper限定）；额外业务旅程为测试缺口。关联报告不是每一handler独立复现声明 |
| AP | A＋provider的枚举/能力/profile证据 | 已验证（源码/局部fixture）；真实枚举/探测阻塞 |
| AS | A＋security及session裁决 | SEC-02/03涉及会话/MFA；其余仅定向防御与静态矩阵 |
| U | A＋deadman-adjudication(summary)、security(capture/audit)、frontend(usage) | 发现问题按具体端点备注；其它查询仅局部证据 |
| O | reliability＋security的metrics/anchor授权；主持runtime | 已验证（源码/局部规则）；云报警与全runtime旅程未验 |

每行标明已验证的证据层；已核对源码/注册/角色但未逐端点完整运行的，标“已验证（限定）＋测试缺口”，不表示仍未审阅，也不表示该端点端到端通过。源码链接均为 `internal/app/runtime.go` 注册行。

| Listener | 方法/路径 | 注册行 | 类别 | 协议/角色报告关联 | 状态/具体证据限制 |
| --- | --- | ---: | --- | --- | --- |
| Gateway | `GET /health/live` | [L1512](../../../internal/app/runtime.go#L1512) | O | [reliability](roles/reliability.md) / [security](roles/security.md) | 已验证（注册/授权源码限定）；实际云观测阻塞 |
| Gateway | `GET /health/ready` | [L1513](../../../internal/app/runtime.go#L1513) | O | [reliability](roles/reliability.md) / [security](roles/security.md) | 已验证（注册/授权源码限定）；实际云观测阻塞 |
| Gateway | `POST /v1/chat/completions` | [L1522](../../../internal/app/runtime.go#L1522) | G | [provider](roles/provider.md) / [provider-adjudication](roles/provider-adjudication.md) | 发现问题 PROV-01 P1；metered本地复现，非HTTP ingress运行 |
| Gateway | `POST /v1/responses` | [L1523](../../../internal/app/runtime.go#L1523) | G | [provider](roles/provider.md) | 已验证（注册/局部协议）；SDK不等于该端点完整runtime |
| Gateway | `GET /v1/responses/{responseID}` | [L1524](../../../internal/app/runtime.go#L1524) | D | [provider](roles/provider.md) / [security-rotation-adjudication](roles/security-rotation-adjudication.md) | 发现问题 SEC-01相关；实验性；生命周期主持源码审阅 |
| Gateway | `POST /v1/responses/{responseID}/cancel` | [L1525](../../../internal/app/runtime.go#L1525) | D | [provider](roles/provider.md) / [security-rotation-adjudication](roles/security-rotation-adjudication.md) | 发现问题 SEC-01相关；实验性；生命周期主持源码审阅 |
| Gateway | `DELETE /v1/responses/{responseID}` | [L1526](../../../internal/app/runtime.go#L1526) | D | [provider](roles/provider.md) / [security-rotation-adjudication](roles/security-rotation-adjudication.md) | 发现问题 SEC-01相关；实验性；生命周期主持源码审阅 |
| Gateway | `POST /v1/embeddings` | [L1527](../../../internal/app/runtime.go#L1527) | G | [provider](roles/provider.md) | 已验证（注册/局部协议）；SDK不等于该端点完整runtime |
| Gateway | `POST /v1/moderations` | [L1528](../../../internal/app/runtime.go#L1528) | X | [provider](roles/provider.md) | 已验证（注册/源码限定，实验性）；SDK测试缺口、真实上游阻塞 |
| Gateway | `POST /v1/images/generations` | [L1529](../../../internal/app/runtime.go#L1529) | X | [provider](roles/provider.md) | 已验证（注册/源码限定，实验性）；SDK测试缺口、真实上游阻塞 |
| Gateway | `POST /v1/audio/speech` | [L1530](../../../internal/app/runtime.go#L1530) | X | [provider](roles/provider.md) | 已验证（注册/源码限定，实验性）；SDK测试缺口、真实上游阻塞 |
| Gateway | `POST /v1/audio/transcriptions` | [L1531](../../../internal/app/runtime.go#L1531) | X | [provider](roles/provider.md) | 已验证（注册/源码限定，实验性）；SDK测试缺口、真实上游阻塞 |
| Gateway | `POST /v1/rerank` | [L1532](../../../internal/app/runtime.go#L1532) | X | [provider](roles/provider.md) | 已验证（注册/源码限定，实验性）；SDK测试缺口、真实上游阻塞；唯一profile withheld |
| Gateway | `POST /v1/async/invocations` | [L1533](../../../internal/app/runtime.go#L1533) | X | [provider](roles/provider.md) | 已验证（注册/源码限定，实验性）；SDK测试缺口、真实上游阻塞；唯一profile withheld |
| Gateway | `GET /v1/async/invocations/{asyncID}` | [L1534](../../../internal/app/runtime.go#L1534) | X | [provider](roles/provider.md) | 已验证（注册/源码限定，实验性）；SDK测试缺口、真实上游阻塞；唯一profile withheld |
| Gateway | `POST /v1/async/invocations/{asyncID}/cancel` | [L1535](../../../internal/app/runtime.go#L1535) | X | [provider](roles/provider.md) | 已验证（注册/源码限定，实验性）；SDK测试缺口、真实上游阻塞；唯一profile withheld |
| Gateway | `POST /v1/files` | [L1536](../../../internal/app/runtime.go#L1536) | X | [provider](roles/provider.md) / [security-rotation-adjudication](roles/security-rotation-adjudication.md) | 已验证（注册/源码限定，实验性）；SDK测试缺口、真实上游阻塞；本地对象轮换受SEC-01影响 |
| Gateway | `GET /v1/files/{fileID}` | [L1537](../../../internal/app/runtime.go#L1537) | X | [provider](roles/provider.md) / [security-rotation-adjudication](roles/security-rotation-adjudication.md) | 已验证（注册/源码限定，实验性）；SDK测试缺口、真实上游阻塞；本地对象轮换受SEC-01影响 |
| Gateway | `GET /v1/files/{fileID}/content` | [L1538](../../../internal/app/runtime.go#L1538) | X | [provider](roles/provider.md) / [security-rotation-adjudication](roles/security-rotation-adjudication.md) | 已验证（注册/源码限定，实验性）；SDK测试缺口、真实上游阻塞；本地对象轮换受SEC-01影响 |
| Gateway | `DELETE /v1/files/{fileID}` | [L1539](../../../internal/app/runtime.go#L1539) | X | [provider](roles/provider.md) / [security-rotation-adjudication](roles/security-rotation-adjudication.md) | 已验证（注册/源码限定，实验性）；SDK测试缺口、真实上游阻塞；本地对象轮换受SEC-01影响 |
| Gateway | `POST /v1/batches` | [L1540](../../../internal/app/runtime.go#L1540) | X | [provider](roles/provider.md) / [security-rotation-adjudication](roles/security-rotation-adjudication.md) | 已验证（注册/源码限定，实验性）；SDK测试缺口、真实上游阻塞；本地对象轮换受SEC-01影响 |
| Gateway | `GET /v1/batches/{batchID}` | [L1541](../../../internal/app/runtime.go#L1541) | X | [provider](roles/provider.md) / [security-rotation-adjudication](roles/security-rotation-adjudication.md) | 已验证（注册/源码限定，实验性）；SDK测试缺口、真实上游阻塞；本地对象轮换受SEC-01影响 |
| Gateway | `POST /v1/batches/{batchID}/cancel` | [L1542](../../../internal/app/runtime.go#L1542) | X | [provider](roles/provider.md) / [security-rotation-adjudication](roles/security-rotation-adjudication.md) | 已验证（注册/源码限定，实验性）；SDK测试缺口、真实上游阻塞；本地对象轮换受SEC-01影响 |
| Gateway | `POST /v1/messages` | [L1548](../../../internal/app/runtime.go#L1548) | G | [provider](roles/provider.md) | 已验证（注册/局部协议）；SDK不等于该端点完整runtime |
| Gateway | `POST /v1/messages/count_tokens` | [L1549](../../../internal/app/runtime.go#L1549) | G | [provider](roles/provider.md) | 已验证（注册）；SDK客户端未调用；仅direct Anthropic合约 |
| Gateway | `GET /` | [L1551](../../../internal/app/runtime.go#L1551) | O | [reliability](roles/reliability.md) / [security](roles/security.md) | 已验证（注册/授权源码限定）；实际云观测阻塞 |
| Admin | `GET /health/live` | [L1566](../../../internal/app/runtime.go#L1566) | O | [reliability](roles/reliability.md) / [security](roles/security.md) | 已验证（注册/授权源码限定）；实际云观测阻塞 |
| Admin | `GET /health/ready` | [L1567](../../../internal/app/runtime.go#L1567) | O | [reliability](roles/reliability.md) / [security](roles/security.md) | 已验证（注册/授权源码限定）；实际云观测阻塞 |
| Admin | `GET /admin/api/v1/setup/status` | [L1568](../../../internal/app/runtime.go#L1568) | AS | [security](roles/security.md) / [security-session-adjudication](roles/security-session-adjudication.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/ui/bootstrap` | [L1569](../../../internal/app/runtime.go#L1569) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/setup/admin` | [L1570](../../../internal/app/runtime.go#L1570) | AS | [security](roles/security.md) / [security-session-adjudication](roles/security-session-adjudication.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/session/login` | [L1571](../../../internal/app/runtime.go#L1571) | AS | [security](roles/security.md) / [security-session-adjudication](roles/security-session-adjudication.md) / [frontend](roles/frontend.md) | 发现问题 SEC-02链路；其它会话操作按角色报告限定 |
| Admin | `POST /admin/api/v1/session/mfa/totp` | [L1572](../../../internal/app/runtime.go#L1572) | AS | [security](roles/security.md) / [security-session-adjudication](roles/security-session-adjudication.md) / [frontend](roles/frontend.md) | 发现问题 SEC-02链路；其它会话操作按角色报告限定 |
| Admin | `POST /admin/api/v1/session/mfa/recovery-code` | [L1573](../../../internal/app/runtime.go#L1573) | AS | [security](roles/security.md) / [security-session-adjudication](roles/security-session-adjudication.md) / [frontend](roles/frontend.md) | 发现问题 SEC-02链路；其它会话操作按角色报告限定 |
| Admin | `DELETE /admin/api/v1/session/mfa/challenge` | [L1574](../../../internal/app/runtime.go#L1574) | AS | [security](roles/security.md) / [security-session-adjudication](roles/security-session-adjudication.md) / [frontend](roles/frontend.md) | 发现问题 SEC-02链路；其它会话操作按角色报告限定 |
| Admin | `GET /admin/api/v1/session` | [L1575](../../../internal/app/runtime.go#L1575) | AS | [security](roles/security.md) / [security-session-adjudication](roles/security-session-adjudication.md) / [frontend](roles/frontend.md) | 发现问题 SEC-02链路；其它会话操作按角色报告限定 |
| Admin | `POST /admin/api/v1/session/logout` | [L1576](../../../internal/app/runtime.go#L1576) | AS | [security](roles/security.md) / [security-session-adjudication](roles/security-session-adjudication.md) / [frontend](roles/frontend.md) | 发现问题 SEC-02链路；其它会话操作按角色报告限定 |
| Admin | `POST /admin/api/v1/session/password` | [L1577](../../../internal/app/runtime.go#L1577) | AS | [security](roles/security.md) / [security-session-adjudication](roles/security-session-adjudication.md) / [frontend](roles/frontend.md) | 发现问题 SEC-02链路；其它会话操作按角色报告限定 |
| Admin | `GET /admin/api/v1/security/mfa` | [L1578](../../../internal/app/runtime.go#L1578) | AS | [security](roles/security.md) / [security-session-adjudication](roles/security-session-adjudication.md) / [frontend](roles/frontend.md) | 发现问题 SEC-03相关管理操作；非所有动作均复现 |
| Admin | `POST /admin/api/v1/security/mfa/authenticators` | [L1579](../../../internal/app/runtime.go#L1579) | AS | [security](roles/security.md) / [security-session-adjudication](roles/security-session-adjudication.md) / [frontend](roles/frontend.md) | 发现问题 SEC-03相关管理操作；非所有动作均复现 |
| Admin | `POST /admin/api/v1/security/mfa/authenticators/{id}/confirm` | [L1580](../../../internal/app/runtime.go#L1580) | AS | [security](roles/security.md) / [security-session-adjudication](roles/security-session-adjudication.md) / [frontend](roles/frontend.md) | 发现问题 SEC-03相关管理操作；非所有动作均复现 |
| Admin | `DELETE /admin/api/v1/security/mfa/authenticators/{id}/pending` | [L1581](../../../internal/app/runtime.go#L1581) | AS | [security](roles/security.md) / [security-session-adjudication](roles/security-session-adjudication.md) / [frontend](roles/frontend.md) | 发现问题 SEC-03相关管理操作；非所有动作均复现 |
| Admin | `PATCH /admin/api/v1/security/mfa/authenticators/{id}` | [L1582](../../../internal/app/runtime.go#L1582) | AS | [security](roles/security.md) / [security-session-adjudication](roles/security-session-adjudication.md) / [frontend](roles/frontend.md) | 发现问题 SEC-03相关管理操作；非所有动作均复现 |
| Admin | `DELETE /admin/api/v1/security/mfa/authenticators/{id}` | [L1583](../../../internal/app/runtime.go#L1583) | AS | [security](roles/security.md) / [security-session-adjudication](roles/security-session-adjudication.md) / [frontend](roles/frontend.md) | 发现问题 SEC-03相关管理操作；非所有动作均复现 |
| Admin | `POST /admin/api/v1/security/mfa/recovery-codes/regenerate` | [L1584](../../../internal/app/runtime.go#L1584) | AS | [security](roles/security.md) / [security-session-adjudication](roles/security-session-adjudication.md) / [frontend](roles/frontend.md) | 发现问题 SEC-03相关管理操作；非所有动作均复现 |
| Admin | `DELETE /admin/api/v1/security/mfa` | [L1585](../../../internal/app/runtime.go#L1585) | AS | [security](roles/security.md) / [security-session-adjudication](roles/security-session-adjudication.md) / [frontend](roles/frontend.md) | 发现问题 SEC-03相关管理操作；非所有动作均复现 |
| Admin | `GET /admin/api/v1/admin-users` | [L1586](../../../internal/app/runtime.go#L1586) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/admin-users` | [L1587](../../../internal/app/runtime.go#L1587) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `DELETE /admin/api/v1/admin-users/{username}` | [L1588](../../../internal/app/runtime.go#L1588) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/dashboard` | [L1589](../../../internal/app/runtime.go#L1589) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/onboarding/readiness` | [L1590](../../../internal/app/runtime.go#L1590) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/master-key/custody` | [L1591](../../../internal/app/runtime.go#L1591) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/master-key/runbooks/lifecycle` | [L1592](../../../internal/app/runtime.go#L1592) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/master-key/runbooks/recovery` | [L1593](../../../internal/app/runtime.go#L1593) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/runbooks/gateway-key-compromise` | [L1594](../../../internal/app/runtime.go#L1594) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/runbooks/configuration-stale` | [L1595](../../../internal/app/runtime.go#L1595) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/runbooks/file-master-key-rotation` | [L1596](../../../internal/app/runtime.go#L1596) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/usage` | [L1597](../../../internal/app/runtime.go#L1597) | U | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/usage/summary` | [L1598](../../../internal/app/runtime.go#L1598) | U | [security](roles/security.md) / [frontend](roles/frontend.md) / [deadman-adjudication](roles/deadman-adjudication.md) | 发现问题 summary P2；真实认证handler定向复现 |
| Admin | `GET /admin/api/v1/usage/failures` | [L1599](../../../internal/app/runtime.go#L1599) | U | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/usage/failures/{requestID}/payload` | [L1600](../../../internal/app/runtime.go#L1600) | U | [security](roles/security.md) / [frontend](roles/frontend.md) / [security-rotation-adjudication](roles/security-rotation-adjudication.md) | 发现问题 SEC-01/04；C2权限契约争议；SEC-04已独立HTTP复现；SEC-01部分源码推导 |
| Admin | `GET /admin/api/v1/usage/requests/{requestID}` | [L1601](../../../internal/app/runtime.go#L1601) | U | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/system/status` | [L1602](../../../internal/app/runtime.go#L1602) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/system/config` | [L1603](../../../internal/app/runtime.go#L1603) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/developer/config` | [L1604](../../../internal/app/runtime.go#L1604) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/developer/execute/{endpoint}` | [L1605](../../../internal/app/runtime.go#L1605) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/settings` | [L1606](../../../internal/app/runtime.go#L1606) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `PUT /admin/api/v1/settings` | [L1607](../../../internal/app/runtime.go#L1607) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/settings/ui` | [L1608](../../../internal/app/runtime.go#L1608) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `PUT /admin/api/v1/settings/ui` | [L1609](../../../internal/app/runtime.go#L1609) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/settings/usage` | [L1610](../../../internal/app/runtime.go#L1610) | U | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `PUT /admin/api/v1/settings/usage` | [L1611](../../../internal/app/runtime.go#L1611) | U | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/settings/accounting` | [L1612](../../../internal/app/runtime.go#L1612) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `PUT /admin/api/v1/settings/accounting` | [L1613](../../../internal/app/runtime.go#L1613) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `DELETE /admin/api/v1/settings/accounting/pending` | [L1614](../../../internal/app/runtime.go#L1614) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/preferences` | [L1615](../../../internal/app/runtime.go#L1615) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `PUT /admin/api/v1/preferences` | [L1616](../../../internal/app/runtime.go#L1616) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/projects` | [L1617](../../../internal/app/runtime.go#L1617) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/projects` | [L1618](../../../internal/app/runtime.go#L1618) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/projects/{id}` | [L1619](../../../internal/app/runtime.go#L1619) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `PUT /admin/api/v1/projects/{id}` | [L1620](../../../internal/app/runtime.go#L1620) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `DELETE /admin/api/v1/projects/{id}` | [L1621](../../../internal/app/runtime.go#L1621) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/projects/{id}/unblock` | [L1622](../../../internal/app/runtime.go#L1622) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/projects/{id}/keys` | [L1623](../../../internal/app/runtime.go#L1623) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/projects/{id}/keys` | [L1624](../../../internal/app/runtime.go#L1624) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/projects/{id}/keys/{keyID}` | [L1625](../../../internal/app/runtime.go#L1625) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `PUT /admin/api/v1/projects/{id}/keys/{keyID}` | [L1626](../../../internal/app/runtime.go#L1626) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `DELETE /admin/api/v1/projects/{id}/keys/{keyID}` | [L1627](../../../internal/app/runtime.go#L1627) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/credentials` | [L1628](../../../internal/app/runtime.go#L1628) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/credentials` | [L1629](../../../internal/app/runtime.go#L1629) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/credentials/{id}` | [L1630](../../../internal/app/runtime.go#L1630) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `PUT /admin/api/v1/credentials/{id}` | [L1631](../../../internal/app/runtime.go#L1631) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `DELETE /admin/api/v1/credentials/{id}` | [L1632](../../../internal/app/runtime.go#L1632) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/providers` | [L1633](../../../internal/app/runtime.go#L1633) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/provider-profiles` | [L1637](../../../internal/app/runtime.go#L1637) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/model-catalog` | [L1638](../../../internal/app/runtime.go#L1638) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/model-catalog/refresh` | [L1639](../../../internal/app/runtime.go#L1639) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/providers` | [L1640](../../../internal/app/runtime.go#L1640) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/providers/{id}` | [L1641](../../../internal/app/runtime.go#L1641) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/providers/{id}/invocation-targets` | [L1642](../../../internal/app/runtime.go#L1642) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/providers/{id}/invocation-targets` | [L1647](../../../internal/app/runtime.go#L1647) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/providers/{id}/invocation-targets/*` | [L1648](../../../internal/app/runtime.go#L1648) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `PUT /admin/api/v1/providers/{id}` | [L1649](../../../internal/app/runtime.go#L1649) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `DELETE /admin/api/v1/providers/{id}` | [L1650](../../../internal/app/runtime.go#L1650) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/providers/{id}/test` | [L1651](../../../internal/app/runtime.go#L1651) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/providers/{id}/model-capability-detections` | [L1652](../../../internal/app/runtime.go#L1652) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/model-capability-detections/{id}` | [L1653](../../../internal/app/runtime.go#L1653) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `DELETE /admin/api/v1/model-capability-detections/{id}` | [L1654](../../../internal/app/runtime.go#L1654) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/deployments` | [L1655](../../../internal/app/runtime.go#L1655) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/deployments` | [L1656](../../../internal/app/runtime.go#L1656) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/deployments/{id}` | [L1657](../../../internal/app/runtime.go#L1657) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `PUT /admin/api/v1/deployments/{id}` | [L1658](../../../internal/app/runtime.go#L1658) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `DELETE /admin/api/v1/deployments/{id}` | [L1659](../../../internal/app/runtime.go#L1659) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/deployments/{id}/test` | [L1660](../../../internal/app/runtime.go#L1660) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/deployments/{id}/capabilities/preflight` | [L1661](../../../internal/app/runtime.go#L1661) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/deployments/{id}/prices` | [L1662](../../../internal/app/runtime.go#L1662) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/deployments/{id}/prices` | [L1663](../../../internal/app/runtime.go#L1663) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/deployments/{id}/prices/preview` | [L1664](../../../internal/app/runtime.go#L1664) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/deployments/{id}/prices/restore-confirm` | [L1665](../../../internal/app/runtime.go#L1665) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/deployments/{id}/prices/{priceID}/cancel` | [L1666](../../../internal/app/runtime.go#L1666) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/deployments/{id}/price-proposals` | [L1667](../../../internal/app/runtime.go#L1667) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/deployments/{id}/price-proposals` | [L1668](../../../internal/app/runtime.go#L1668) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/deployments/{id}/price-proposals/{proposalID}/adopt` | [L1669](../../../internal/app/runtime.go#L1669) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/deployments/{id}/price-proposals/{proposalID}/reject` | [L1670](../../../internal/app/runtime.go#L1670) | AP | [provider](roles/provider.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/routes` | [L1671](../../../internal/app/runtime.go#L1671) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/routes` | [L1672](../../../internal/app/runtime.go#L1672) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/routes/{id}` | [L1673](../../../internal/app/runtime.go#L1673) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `PUT /admin/api/v1/routes/{id}` | [L1674](../../../internal/app/runtime.go#L1674) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `DELETE /admin/api/v1/routes/{id}` | [L1675](../../../internal/app/runtime.go#L1675) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/routes/{id}/test` | [L1676](../../../internal/app/runtime.go#L1676) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/token-guard-policies` | [L1677](../../../internal/app/runtime.go#L1677) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/token-guard-policies` | [L1678](../../../internal/app/runtime.go#L1678) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/token-guard-policies/{id}` | [L1679](../../../internal/app/runtime.go#L1679) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `PUT /admin/api/v1/token-guard-policies/{id}` | [L1680](../../../internal/app/runtime.go#L1680) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `DELETE /admin/api/v1/token-guard-policies/{id}` | [L1681](../../../internal/app/runtime.go#L1681) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/token-guard-policies/{id}/test` | [L1682](../../../internal/app/runtime.go#L1682) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/redaction-policies` | [L1683](../../../internal/app/runtime.go#L1683) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/redaction-policies` | [L1684](../../../internal/app/runtime.go#L1684) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/redaction-policies/{id}` | [L1685](../../../internal/app/runtime.go#L1685) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `PUT /admin/api/v1/redaction-policies/{id}` | [L1686](../../../internal/app/runtime.go#L1686) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `DELETE /admin/api/v1/redaction-policies/{id}` | [L1687](../../../internal/app/runtime.go#L1687) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `POST /admin/api/v1/redaction-policies/{id}/test` | [L1688](../../../internal/app/runtime.go#L1688) | A | [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（角色/注册限定）；测试缺口：未逐端点完整运行 |
| Admin | `GET /admin/api/v1/alerts` | [L1689](../../../internal/app/runtime.go#L1689) | A | [reliability](roles/reliability.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（本地dispatcher/规则限定）；云送达阻塞 |
| Admin | `POST /admin/api/v1/alerts` | [L1690](../../../internal/app/runtime.go#L1690) | A | [reliability](roles/reliability.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（本地dispatcher/规则限定）；云送达阻塞 |
| Admin | `POST /admin/api/v1/alerts/test` | [L1691](../../../internal/app/runtime.go#L1691) | A | [reliability](roles/reliability.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（本地dispatcher/规则限定）；云送达阻塞 |
| Admin | `GET /admin/api/v1/alerts/{id}` | [L1692](../../../internal/app/runtime.go#L1692) | A | [reliability](roles/reliability.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（本地dispatcher/规则限定）；云送达阻塞 |
| Admin | `PUT /admin/api/v1/alerts/{id}` | [L1693](../../../internal/app/runtime.go#L1693) | A | [reliability](roles/reliability.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（本地dispatcher/规则限定）；云送达阻塞 |
| Admin | `DELETE /admin/api/v1/alerts/{id}` | [L1694](../../../internal/app/runtime.go#L1694) | A | [reliability](roles/reliability.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（本地dispatcher/规则限定）；云送达阻塞 |
| Admin | `POST /admin/api/v1/alerts/{id}/test` | [L1695](../../../internal/app/runtime.go#L1695) | A | [reliability](roles/reliability.md) / [security](roles/security.md) / [frontend](roles/frontend.md) | 已验证（本地dispatcher/规则限定）；云送达阻塞 |
| Admin | `GET /admin/api/v1/audit` | [L1696](../../../internal/app/runtime.go#L1696) | U | [security](roles/security.md) / [frontend](roles/frontend.md) | 发现问题 SEC-05；源码确认，未测大历史容量 |
| Metrics | `GET /health/live` | [L1721](../../../internal/app/runtime.go#L1721) | O | [reliability](roles/reliability.md) / [security](roles/security.md) | 已验证（注册/授权源码限定）；实际云观测阻塞 |
| Metrics | `GET /audit/anchors` | [L1723](../../../internal/app/runtime.go#L1723) | O | [reliability](roles/reliability.md) / [security](roles/security.md) | 已验证（注册/授权源码限定）；实际云观测阻塞；仅Anchor启用且deadman_pull注册 |
| Metrics | `GET /metrics` | [L1725](../../../internal/app/runtime.go#L1725) | O | [reliability](roles/reliability.md) / [security](roles/security.md) | 已验证（注册/授权源码限定）；实际云观测阻塞 |

清单外已服务挂载：`Handle /admin`（runtime.go:1698）、`Handle /admin/*`（:1699），Admin SPA/静态资源，归 frontend/security/webui；源码已核对，浏览器局部已验，全旅程未完成。该通配挂载不是无限多个已测试功能。metrics listener及anchor受配置控制；Admin、Gateway health重复注册不重复计为功能。

### Profile状态不可从路由注册推断

完整24个注册profile及23条manifest推理路由对应见 [provider报告](roles/provider.md#profile-coverage-including-withheldexperimental)。19个generation profiles、embeddings子集和native模式均有限制。6个withheld：Bedrock Converse、Titan Embed、Titan Image、Cohere rerank、Nova Reel async、Kimi Responses；新建/展示被拒绝，旧记录可读不能推出永不执行。Rerank/async路由存在但仅有withheld profile，async cancel明确fail closed。Gemini为beta text子集；media/files/batches/deferred为experimental；OpenAI/MiniMax Responses unary不因北向stream字段自动支持流。Mantle/MiniMax/Kimi各southbound前缀和枚举decoder不能混同。Manifest缺withheld字段是呈现/契约缺口，不是已确认绕过。

## INV逐项证据与尚未闭合部分

| INV | 当前证据 | 状态 | 尚需证据 |
| --- | --- | --- | --- |
| 01 单写者 | 主持源码/共享Go；schema33→35二进制演练中第二writer exit1 | 已验证（本地限定） | offline backup/restore已验；其余锁/系统调用故障排列为测试缺口 |
| 02 Ledger权威 | 主持Apply/usage源码、共享Go/race；restore/kill后Ledger认证及usage verify实测 | 已验证（源码+共享tests限定） | 篡改/截断/缺段与认证重建前后摘要，不能以源码代替 |
| 03 attempt结算 | 主持manager/attempt/settle；PROV-01 metered独立复现 | 发现问题 | 修复前后同目标/跨目标/中断与余额事件对照；summary P2不是持久化丢账 |
| 04 授权 | security角色扫查/认证；SEC-02 logout新请求独立复现 | 发现问题 | 完整双项目/同项目不同Key×资源端点；降权/MFA浏览器 |
| 05 能力证据 | provider unknown/签名/过期/来源门控定向通过 | 已验证（本地限定） | 真实列表与capability来源、发布信任根、所有profile矩阵 |
| 06 不确定重试 | PROV-01独立控制：坏200两次零结算、ambiguity控制一次估算 | 发现问题 | Responses/embeddings/Mantle首事件及多目标回退扩展 |
| 07 留存 | SEC-01轮换、SEC-04超期读取；资源生命周期源码 | 发现问题 | 已登记对象HTTP、写入/登记崩溃、TTL/清扫失败/孤儿矩阵 |
| 08 资源有界 | limiter/token/redaction/alert局部测试；PROV-02、SEC-05 | 发现问题 | 慢客户端、总channel缓冲、公平性、长期RSS/FD/队列数据 |
| 09 恢复轮换 | SEC-01独立确认；REL-01独立确认；元数据防御测试 | 发现问题 | 小型fixture完整offline恢复/kill已验；全对象同代迁移/故障排列测试缺口，真实KMS阻塞 |
| 10 UI/API一致 | 23推理manifest/注册、角色扫查、SDK子集/web门禁 | 发现问题 | FE-01～05、REL-02/03、配置省略承诺；浏览器局部已验、FE-01复现；MFA/降权/异步未验 |

## 输入可追溯性

临时清单可能被清理，下面保留哈希及计数；本表模块与路由明细已固化，无需依赖临时文件才能了解覆盖。

| 输入 | SHA-256 | 计数 |
| --- | --- | ---: |
| `/private/tmp/halro-review-260905/module-inventory.json` | `95db3cc9cfd2124b3069ec733d90a043b5dcc05b73c746141fdba981219ee8e7` | 44 |
| `/private/tmp/halro-review-260905/route-inventory.json` | `df407a2404b377bba0eaa29091f19e3785167a9682f5e07e1112bfbe8cb96fd0` | 153 |
| `/private/tmp/halro-review-260905/dependency-edges.json` | `8d6210dadf40270387161684c59084437ef4be1811cbccf90c666cb31c3e483a` | 152 |

## 最新运行证据补充（主持提供，非本作者重跑）

[core-and-runtime](roles/core-and-runtime.md)已完整审阅五个小模块；追加 `go test -race -count=1 ./internal/circuit ./internal/limiter ./internal/sourcelimit` 退出0、4.495s（`/private/tmp/halro-review-260905/admission-race.json`）。无全排列/长期公平性实验是证据缺口，不代表这些模块仍未审阅。主持实际 `git diff --exit-code -- internal/webui/dist` 退出0；旧scope“待归档”不等于未执行。

[最新scope](scope-and-baseline.md)：API setup后真实浏览器登录、UI创建停用Provider；mock刷新review-chat，unknown不自动获能力，手动声明后UI创建停用Deployment；UI建Project（$0.01/day、1KB、127.0.0.1 CIDR、review-chat）。Key经API生成后调用200，2KB→400 request_too_large，未授权alias→403；专用Key 200→UI禁用→401。预算/CIDR保存不等于预算耗尽或不允许来源拒绝已验。FE-01浏览器复现，主动切tab能读mock500详情；不是drilldown通过。MFA、降权、异步浏览器未执行。

两个15秒fuzz（SSE no-panic、redaction等价）主持确认通过，不证明PROV-02修复或全输入正确。schema33→35升级/只读hash不变/第二writer拒绝为已执行局部证据；backup、30分钟smoke及kill重启现已完成；最终数据见下表，主持归档runtime证据。

## 最终runtime证据（主持执行，本文件汇总）

本轮S0–S4评审已完成，结论带明确限制；依计划9.1记录验证、发现和阻塞，不代表所有验证通过或缺陷关闭。以下采用主持最终回报，结果由主持归档到runtime证据目录；本作者未重跑或独立复算原始采样。

| 实验 | 最终结果 | 适用边界 |
| --- | --- | --- |
| 30分钟 `smoke_only` | exit **0**，`passed=true`；**61 samples、899 success、0 failure** | 本地受控负载短时smoke，不是2–4h/24h或生产容量/SLA |
| 资源采样 | RSS起始 **32,948,224 B**、结束 **71,794,688 B**、峰值 **128,188,416 B**；goroutine **21→21**、FD **15→15**；采样队列为 **0** | RSS结束高于起点；无goroutine/FD净增长不等于无泄漏，采样队列0不证明采样间从未积压 |
| offline backup | create/verify各exit **0** | 完整离线流程在本次小型fixture执行，非所有对象/历史schema/key模式组合 |
| 恢复拒绝控制 | 错backup key、截断、确认门禁、错master key的restore各exit **1**；目标hash均不变 | 预期失败/无目标改写，不是产品门禁失败；不代表所有损坏/磁盘故障排列 |
| 正确restore | exit **0**；Ledger seq **5020**保持，usage verify **0**；旧Admin session **401** | 恢复一致性与旧session失效已实测；不关闭SEC-01旧密文轮换缺陷 |
| kill与重启 | 恢复后call **200**→`SIGKILL -9`；Ledger认证seq **5025**；重启ready **0.333s**，再次call **200**；停机Ledger认证seq **5030**、usage verify **0** | 本次kill观察未见已确认请求丢失；不是物理断电或存储设备故障，不得称RPO=0 |
| 小型fixture恢复耗时 | restore命令 **0.255s**＋ready **0.249s**＝**0.504s** | 仅本次fixture实测，不是生产RTO，也不含任意规模数据/人工响应时间 |

本轮不再等待smoke/backup完成。后续事项是缺陷修复/复验、明确列出的额外测试和外部环境验证；真实provider/KMS/生产/实际云报警仍阻塞，MFA/降权/异步等额外浏览器矩阵及2–4h/24h未执行。
