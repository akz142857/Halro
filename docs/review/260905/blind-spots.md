# 盲区、阻塞与后续取证 — 2026-09-05

基线 `381743f6613607dc256828f4776b52af8bdd232c`。与 [coverage-matrix.md](coverage-matrix.md) 配套，依据[批准计划](review-plan.md)、角色报告（含新增core-and-runtime与security-secondary-adjudication）及主持本轮状态说明。这里只记录已知边界、责任归属和下一步，不提出已执行修复或已完成验收的声明。

**当前摘要：三个独立确认P1仍未关闭；核心/账务与网关无完整独立专项报告；实际offline backup/restore及kill重启已完成；浏览器局部真实旅程已执行、FE-01已复现；MFA/降权/异步未执行，30分钟smoke已完成且通过（限定smoke_only）。真实provider、真实KMS、生产、2–4小时/24小时及实际云报警均未验证。** 这不是“所有模块无主”：44模块已有逐项责任/证据映射；本轮S0–S4评审完成但带限制，不等于所有不变量成立或所有验证通过。

## 状态与独立性

采用计划的 `待评审 / 评审中 / 已验证 / 发现问题 / 阻塞 / 不适用`。源码阅读只支持静态事实；本地测试只支持其输入/调度/环境。发现状态（已确认/待复验/已关闭等）与覆盖状态分开，任何复现测试通过都不自动关闭缺陷。

| 项目 | 当前状态 | 证据归属与不能越过的边界 |
| --- | --- | --- |
| 完整Go、vet、web typecheck/test/build | 已验证（主持共享） | 主持确认本轮通过；本文件未重跑/独立检查原始日志；不推出全部功能正确 |
| race gateway/budget/ledger | 已验证（主持共享） | 未报告Go data race不等于事务交接/撤销/持久化顺序正确 |
| 官方Go/Node/Python SDK | 已验证（本地compat server） | 主持运行；非完整runtime，不调用真实provider，不覆盖每个端点/profile |
| 浏览器局部真实旅程 | 已验证（限定）/发现问题 | 最新scope：登录/UI资源创建/模型刷新、Key禁用后401、FE-01复现；MFA/降权/异步未执行 |
| 30分钟smoke | 已验证（smoke_only限定） | exit0、passed=true、61 samples、899成功/0失败；具体资源数据见最终runtime表 |
| 实际backup/restore | 已验证（小型fixture限定） | create/verify/restore成功、四类拒绝目标hash不变、seq5020保持、usage0、旧session401；kill重启结果见最终表 |
| 核心逻辑/账务专项 | 已验证（主持接手、源码+共享tests限定） | agent自动中断；主持已读budget manager、Ledger Apply、usage collector/checkpoint/rollup/offline等，不能冒称独立专项完成 |
| 网关专项 | 已验证（主持接手、源码+共享tests限定） | agent自动中断；主持已读admission/attempt/settle/retry、deferred生命周期；PROV-01独立裁决只覆盖该发现 |
| 根app架构与基础模块 | 已验证（主持源码，限定） | runtime/start/close、activation、id/durable/timezone等已读；实际部署/停机/磁盘失败仍需运行证据 |
| 真实provider/KMS/生产/云报警 | 阻塞（缺本轮外部验证） | 未执行，不将mock、历史smoke或代码注释作为替代；后续需环境/权限/明确授权 |
| 2–4小时/24小时稳定性 | 已验证（短时限定）/测试缺口 | 30分钟已完成；2–4h/24h未执行，只能算短时smoke；不是长期容量结论 |

早先cache/httptest socket/Docker/DNS限制按角色原始记录归为环境失败；已成功的后续运行保留其证据。复现脚本编译/selector错误归为审阅夹具错误，不能作为产品BUG或当前环境阻塞。各报告保留具体命令、退出码及临时日志位置。

## 已发现问题不应混入“尚未验证”后被遗忘

| 发现 | 当前裁决 | 未关闭部分 / 下一步 | 责任归属 |
| --- | --- | --- | --- |
| SEC-01 retained ciphertext轮换 | [独立确认P1](roles/security-rotation-adjudication.md)，file与fake-KMS DEK | 已登记Files/deferred input/output、HTTP读取、每个发布中断点与换钥对象专项恢复仍有测试缺口；本轮通用backup演练已完成。KEK rewrap不属于该机制；有旧key/原ciphertext可恢复，不称全局不可逆丢失 | key生命周期＋留存；主持backup |
| SEC-02 logout被idle refresh复活 | [独立确认P1](roles/security-session-adjudication.md) | 实际logout后新请求成功已证明；修复需两种顺序、并发刷新、fresh manager、绝对到期/代际控制。需先持有旧token，不是匿名绕过 | adminauth/store |
| PROV-01坏200被重试并零结算 | [独立确认P1](roles/provider-adjudication.md) | metered/pin/正预留本地证明；补跨目标、Responses/embeddings及各profile边界。未测真实扣费；不等于Ledger事件丢失 | provider＋gateway/budget |
| SEC-03管理MFA失败预算/审计缺口 | [独立确认P2](roles/security-secondary-adjudication.md) | delete-authenticator及四路错误密码控制已独立补证；确认路径不同威胁模型、多个factor/required变体/并发仍待补；现有cookie/password/CSRF和factor校验仍有效 | Admin MFA |
| SEC-04超期capture仍可读 | [store本地确认P2](roles/security.md) | 独立HTTP恰好TTL及两小时仍200、Purge后404已补证；失败purge/禁用capture/重启待补 | failurecapture/app |
| SEC-05审计分页全量扫描/append锁 | [源码确认P2](roles/security.md)，容量影响未测 | 大历史limit=1内存/append延迟实验；不称已复现OOM/远程DoS | audit/Admin |
| PROV-02 CR-only SSE延迟 | [pipe本地确认P2](roles/provider.md) | split CRLF、连续小事件、取消/大小边界；未证明某真实provider采用CR-only | sse/provider |
| REL-01发送先于outbox持久化 | [独立确认P2](roles/deadman-adjudication.md) | 真实进程终止/接收器持久确认、后续tick和sync失败。窗口只持续到任一路成功保存，非整个慢probe时长 | deadman |
| summary checkpoint在途增量不可见 | [独立确认P2](roles/deadman-adjudication.md) | HTTP请求数1→0→1/金额90→0→90；真实store commit并发及防双算。不是持久化丢账或错误settlement | app/usage/store |
| FE-01～FE-05 | [5项jsdom本地确认P2](roles/frontend.md) | Usage同页导航、列表分页、debug-key重试、pending确认关闭、byte limit round-trip；补真实runtime/browser请求与结果 | frontend/Admin |
| REL-02 interval stats gauge | [本地确认P2](roles/reliability.md) | 不变/上升/下降gauge及counter reset回归；原始metrics与单次stats不受该delta错误影响 | CLI/observability |
| REL-03 release文档过期门禁 | [源码确认文档P2](roles/reliability.md) | 对齐当前workflow与1.0目标；不把已退休审批流程说成现有授权绕过 | release docs |
| 配置模板省略字段承诺 | [reliability追加独立确认P2](roles/reliability.md) | 110项省略实验；文档与实际Decode/Normalize一致。全默认merge属于另一个行为变更，不能擅自视为修复 | config/docs |
| REL-04单边tier provenance校验 | [局部validator确认P3](roles/reliability.md) | 正常构造器有防御；完整调用可达性/外部影响未建立，不升级为计费漏洞 | domain pricing |

角色初稿中的“尚待独立裁决”已被对应裁决报告更新；未修改原报告。未赋ID的summary/配置条目以描述和来源定位，最终统一编号由主持汇总。没有修复提交或回归成功证据，本文件不将任何确认缺陷标为已关闭。

## 高风险未闭合场景与最低补证

| 检查项 | 当前证据 / 状态 | 最低下一步及可接受结果 | 责任 |
| --- | --- | --- | --- |
| INV-01单写者、离线锁 | 主持源码/共享Go；第二writer拒绝及offline backup/restore已实测；已验证（限定） | 其它离线命令组合、并发与系统调用故障排列是测试缺口 | 主持runtime/backup |
| INV-02认证Ledger派生 | Apply/offline/collector源码＋共享Go/race、restore/kill后认证；已验证（限定） | 错key、篡改、截断、缺段、checkpoint不可信、取消重建；持久派生前后摘要对照，拒绝不可信输入 | 主持账务 |
| INV-03/06成功/未知/重启结算 | P1已确认，其余主链阅读；发现问题 | 发送前失败、发送后不确定、首token前后、重复完成/取消、进程终止；attempt/余额/pin/最终事件对照 | gateway/budget/ledger |
| 留存完整快照 | rotation缺陷复现＋完整offline backup/restore实测；发现问题/已验证（限定） | 本轮错backup/master key等拒绝及目标hash不变已验；扩展全对象/规模/换钥代际矩阵；0.504s仅小fixture，不称生产RTO或物理断电RPO0 | 主持backup |
| 轮换/恢复中断 | metadata防御及file/fake-KMS局部验证；发现问题 | 每个publish/bridge/slot边界中断与重试；保留对象可读且没有混代；不能用新generation metadata检查替代对象认证 | key生命周期 |
| INV-04全资源授权矩阵 | Admin角色扫查/部分owner路径；已验证（限定）/测试缺口 | 双项目与同项目不同Key逐Files/Batches/deferred/async读取/取消/删除；按已声明project或key边界验收，不自设更强隔离 | 主持gateway＋security |
| 留存登记/清扫竞态 | 生命周期源码；已验证（限定）/测试缺口 | 写入到登记、完成到删除、TTL/取消与worker并发、硬重启；残留受清扫界限且不删除他人有效对象 | retained资源 |
| 审计写失败 | tamper/partial-tail测试；已验证（限定）/测试缺口 | append部分写/sync失败后重试、意图重投、长寿命dedup淘汰；保持可验证链及恰当拒绝语义 | audit/app |
| stream总资源上界 | channel/rolling buffer局部测试；发现问题 | 多choice/tool/channel合计内存、UTF-8/escape断点、异常尾JSON、慢客户端/取消；RSS/FD/goroutine释放证据 | gateway/redaction/sse |
| 限流与公平性 | token/limiter局部通过；已验证（限定）/测试缺口 | 单项目洪泛＋正常项目、并发上限/释放、长期subject基数和时序指标 | 主持负载 |
| alert长期状态与关闭 | 有界queue/fanout/backoff测试；已验证（限定）/测试缺口 | 项目/原因高基数churn、满队列慢首次发送下退出期限；dedup map长期量化 | reliability |
| deadman物理故障/磁盘容量 | Run interleaving/race已验；发现问题 | 真实receiver接受后进程kill；状态写/rename/sync失败；JSONL保留/轮换与满盘；不以默认单service声称多进程锁成立 | reliability |
| fuzz范围 | 主持SSE no-panic/redaction等价各15秒通过；已验证（限定） | 归档命令/seed/执行量，其它目标与延长campaign未执行；不证明增量性或全部输入正确 | 协议/安全 |

## 协议、SDK与真实上游盲区

- [provider](roles/provider.md)列出24个profile与23条manifest路由；计数不是端点×profile×字段值×SDK的笛卡尔积通过。三语言client只对受控harness验证部分Chat/Responses/Messages/embeddings；`count_tokens`没有client调用，Go Anthropic流未测，deferred retrieve/cancel/delete、实验资源、native betas及扩展tool块缺SDK覆盖。最低补证是选定每个承诺组合的序列化、错误、流终止与取消用例，并标明完整runtime还是compat server。
- 六个withheld profile仍在编译注册/manifest中；新建门禁与旧记录读语义不同。Rerank/async存在路由不能推出当前可创建可用deployment。异步取消明确拒绝属于当前契约，不能当“缺实现BUG”。推广withheld之前必须补真实route-specific证据。
- 枚举与能力分离已有本地正向证据，但真实账户列表/字段/权限/地域、签名catalog发布物与运行trust-root均未验。MiniMax/Kimi/Mantle的历史测量只能解释历史决策；无本轮真实响应样本就不得认证当前decoder真实性或账户能力。
- `{}`/null/缺失list envelope的不同decoder行为是源码观察到的硬化/测试缺口，尚无完整刷新导致可见模型丢失的复现；补刷新失败、旧结果保留和成功空列表区分。一个200探测不证明实际模型能力。
- PROV-01的Responses/embeddings兄弟分支及Mantle首事件前错误需要单独profile/transport复现；当前独立裁决只证明OpenAI unary Chat四类post-acceptance失败。不能由共享代码推定每种Azure/MiniMax/Kimi/Mantle操作已独立确认。
- 真实provider调用、IAM/SigV4/CloudTrail、KMS策略/Grant与灾难恢复均阻塞。后续需隔离账户、明确模型/区域/权限/成本授权；本轮没有付费调用授权，不为补矩阵擅自调用。

## 前端与运维验证边界

| 场景 | 状态 | 下一步与边界 |
| --- | --- | --- |
| 浏览器配置/调用/撤销 | 已验证（局部）/发现问题 | API setup/API建Key；实际浏览器登录/UI建停用Provider/Deployment/Project；mock刷新unknown不自动获能力；200、超正文400、未授权alias403、UI禁用Key后401；FE-01复现；MFA/降权/异步未执行 |
| 会话过期/降权/账号切换/step-up 401 | 已验证（源码/现有测试限定）/额外浏览器测试缺口 | 区分recent_reauth_required与session失效；检查缓存/secret清理；服务端角色仍是授权权威 |
| 51+记录picker/ambiguous mint/500-byte项目保存 | 发现问题 | FE本地复现已足够确认客户端根因；补真实Admin API往返与幂等重放，不将DOM测试当服务端提交证据 |
| 冷启动时区、跨日价格、后台延迟状态 | 测试缺口（额外浏览器场景未执行） | 浏览器区不同于账务区、absolute interval深链、迟到system/status；比对显示/请求instants |
| 无障碍/双语言/缩放/窄屏/键盘焦点 | 测试缺口（额外浏览器场景未执行） | 实际浏览器200%/窄屏/键盘/屏幕阅读器；静态截图/既有a11y测试不是全面认证 |
| 嵌入bundle | 已验证（生成链限定） | 主持实际`git diff --exit-code -- internal/webui/dist`退出0；完整asset缺失/冷chunk/deep-link故障路径仍需额外证据 |
| 观测闭环 | 阻塞（实际云报警未验） | 规则/schema/promtool/amtool/compose本地通过；补scrape→rule→Alertmanager→实际contact point→独立receiver和故障runbook闭环 |
| Linux/container生产硬化 | 阻塞（目标环境未运行） | Darwin源码不能证明prctl/权限/seccomp/PV布局生效；在目标系统核对单实例/停机/重启 |
| 发布与供应链 | 已验证（本地结构/扫描限定） | govulncheck无reachable漏洞不等于无advisory；远程branch/environment设置、逐架构SBOM/签名/容器扫描及可复现构建未验 |
| 30分钟与更长容量 | 已验证（smoke_only）/测试缺口 | 61samples/899成功0失败及资源数据已记录；2–4h/24h与全面公平性/容量未验。历史benchmark不替代长期稳定 |

## 契约争议与不应升级的候选

- **C2 read_only读取failure正文**：security源码和角色定义支持实例级read_only读取，且先审计；是未决内容访问权限产品契约，不是已发现绕过。需产品选择明确授权语义或新增权限，再做角色/正文矩阵，不能仅因敏感就自行判P1。
- **历史时区27小时日**：reliability在2023 Casey确认PeriodAt拒绝真实27小时日；2026的598区抽样无异常。属于历史支持范围/注释问题（该报告P3边界），尚未证明当前外部操作因它失败。缺失tzdata时TierAt回退也未证明整个snapshot constructor可用。
- **价格helper前置条件**：正常price/snapshot验证限制负值等；单独helper缺检查、cache rate/独立舍入观察不能直接称可达超预算。需主持追完整prepared reservation链与具体数值。
- **设计目标**：Realtime、HA、更多资源管理UI、v1.0审批目标不因尚未实现就记为当前BUG。注册profile withheld字段缺失是呈现契约缺口，不能偷换为运行绕过。
- **架构建议**：集中导航/query语义、事务刷新、稳定对象密钥/代际迁移是建议；没有实现或验证。app体积/依赖集中本身不是缺陷证据。

## 交接与关闭规则

1. 本轮smoke/backup/kill实测已完成，最终结果由主持归档；交接不再等待这些运行。额外浏览器/规模/外部环境项目按测试缺口或阻塞交接，不误示源码仍未审阅。
2. 核心/账务、网关独立性缺口应明确保留，或另行完成独立评审；不能用已有P1证伪报告或完整门禁抵扣整个专项。
3. 每个确认缺陷要有责任人、修复与回归证据；风险接受另记理由/缓解/期限/重评条件。这里的责任是模块角色，尚未替发布负责人指定个人或期限。
4. 本轮S0–S4评审按计划9.1以带限制结论完成，缺陷和测试/环境边界显式交接；不等于全部验证通过。未处置P1仍不得给无条件发布建议，修复/风险接受由发布责任人决定。

## 最新证据与独立性补充

[core-and-runtime](roles/core-and-runtime.md)完整审阅idempotency/circuit/limiter/sourcelimit/requestmeta，状态为已验证（源码+共享tests限定）。追加circuit/limiter/sourcelimit窄race退出0、4.495s，见`/private/tmp/halro-review-260905/admission-race.json`。长期高基数churn、全面公平性与所有故障排列是实验缺口，不能将已审模块写成仍未评审；idempotency只是格式验证，真实资源幂等在bolt事务。

[最新scope](scope-and-baseline.md)的schema33→35、旧schema只读拒绝且hash不变、第二writer拒绝已执行，不重复列为未测；本轮offline backup/restore也已完成，完整大规模/全对象组合仍为测试缺口。浏览器已保存$0.01/day、1KB、来源CIDR与alias不等于预算耗尽/非允许来源实测；FE-01仍失败，手动切tab诊断为绕行成功。30分钟smoke已按最终结果记录为通过（smoke_only限定）。

[安全二次独立裁决](roles/security-secondary-adjudication.md)：SEC-03/04确认P2（新增四路MFA错误密码控制、capture实际HTTP/TTL/审计控制）。C2当前read_only正文授权行为成立，越权解释REFUTED；产品权限契约调整仍开放，不升级为漏洞。

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
