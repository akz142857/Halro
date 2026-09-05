# S0–S4 完成核对

核对对象为 [原评审计划](review-plan.md)。**S0–S4评审于2026-09-05完成，结论带明确限制，S5未开始。** 本轮逐项形成证据、问题或明确缺口；没有把评审完成等同于实现验收。计划9.1允许带限制结论，9.2禁止未处置P1时无条件发布。以下核对包含最终备份、进程中断和30分钟运行结果，不以报告文件存在替代实际运行证据。

## 原计划要求到证据

| 原要求 | 当前证据及裁决 | 退出核对 |
| --- | --- | --- |
| S0 SHA、工作区、工具链、模块、路由、历史问题 | scope-and-baseline；44模块、153显式注册，非逐行覆盖率；旧P5/10/14/15/16及C2/C7/C9有当前映射 | 已记录；最终交付再检查SHA与文档外diff |
| S1依赖方向、核心边界、隐式状态、所有权 | architecture-and-invariants及core-and-runtime；依赖邻接、Runtime Open/Close、typed context、idempotency退役旧store、集中组装的具体代价 | 已审查；不以文件长度直接定缺陷 |
| S1信任边界、数据权威、持久化对象、状态机 | 架构对象族/时序表，安全角色矩阵；Ledger、metadata、Usage、Audit、密封对象、密钥及运行快照分别说明 | 已记录证据与断裂接缝 |
| INV-01 | 真实第二进程/离线备份锁竞争、只读命令不迁移；store/lock测试；恢复前错误材料不替换目录 | 有限定正向证据；不外推所有平台/存储锁 |
| INV-02 | 认证后才派生的实际源码/现存负向测试；nonzero旧Ledger升级后认证 | 有限定正向证据；不把checksum-only当认证 |
| INV-03、INV-06 | Provider→bridge→带metered snapshot/pin Gateway独立复现；预算并发/race | **PROV-01反例，发现问题**；严重结论已裁决 |
| INV-04 | Admin/Gateway/Project角色与资源矩阵；会话race、MFA因子budget、read_only审计；UI禁用Key后401 | **SEC-02/03反例**；C2越权解释已证伪，产品契约开放 |
| INV-05 | 24 Profile/能力来源、枚举provenance定向测试、UI刷新unknown | 本地正向证据；真实provider样本显式阻塞 |
| INV-07 | deferred/capture/blob作用域与TTL、换钥、清扫；独立capture HTTP | **SEC-01/04反例**；物理存储所有中断点未全面注入 |
| INV-08 | 队列/来源上限/lease释放/短fuzz及race；Audit扫描/SSE复现；61个资源时序样本 | **SEC-05/PROV-02问题**；30m仅smoke，长期/大规模证据缺口明确 |
| INV-09 | file及fake-KMS DEK独立复现、same-DEK控制；33→35升级、dead-man序列；完整backup/restore及SIGKILL | **SEC-01/REL-01问题**；错误材料拒绝、正确恢复保持5020事件及认证；真实云/物理断电/全留存对象未验 |
| INV-10 | 全路由/manifest/Profile/角色/页面对照、SDK与真实浏览器 | **FE-01–05问题**；withheld与已服务分开；完整MFA/异步/无障碍浏览器验收不足 |
| 5.1数值、时区、价格、并发、取消、幂等、熔断、deferred/Files/Batches | reliability pricing/timezone、core-and-runtime、provider矩阵与现存测试映射；SUM-01/REL-04/FE-05 | 已专项审查；历史27h支持边界、C7/C9与未测排列保留 |
| 5.2身份、SSRF、路径、恢复、密钥、脱敏、审计、洪泛 | security全文及4份安全相关独立裁决；补充MFA/capture HTTP对照 | 已形成判断；真实云/大历史资源消耗上限非已验收 |
| 5.3端点、协议、每个Profile、枚举、错误、SDK | provider的23个manifest端点与24个profile表；完整显式路由表补Admin/metrics；3语言SDK | 已记录当前证据；不能由本地harness外推所有真实组合 |
| 5.4预留/WAL/恢复/错材料/升级/备份/RTO/RPO | 架构中断时序、旧新真实二进制、完整Go负向测试；runtime第5节 | 备份/restore/kill成功；restore+ready两段约0.504s，非连续事故RTO；本次ack事件保留，非物理断电RPO承诺 |
| 5.5关键浏览器旅程 | 登录、Provider/Deployment/Project创建、刷新与能力声明、调用/Usage/停用Key；setup与mint部分API | **带限制完成**；实际UI反例与缺失MFA/降权/异步/读屏旅程显式记录，不写全浏览器验收 |
| 5.6性能、观测、供应链、发布、运维边界 | 历史组件benchmark源hash核对；100次mixed E2E延迟、30m时序；观测规则/脚本、govulncheck及npm audit | 30m进程exit0、899成功0失败；RSS增加仍在工具阈值内，不证明无泄漏；云contact point/24h/发布物/生产SLO未验 |
| 6集成门禁一次、针对性race、有限fuzz、不调用付费provider | runtime命令表及退出码/日志；完整Go和web各一次，pure-doc不重跑；仅局部race | 已遵守；plan中的命令示例不是要求重复每一个包门禁，make check未盲目执行 |
| 7严重发现和账务/授权/契约争议独立证伪 | adversarial-verdicts；全部3P1独立确认，SUM/CFG/FE03/FE05/SEC03/SEC04/C2另有独立裁决 | 已取得所列裁决；未独立复核的其他P2/P3明确标注，不冒称全量双评 |
| 8九项Markdown交付及脱敏复现 | scope、architecture、coverage、findings、adversarial、runtime、blind-spots、review-report、progress；evidence | 九项交付齐全；归档九个selector实际编译运行，保留打包失败及修正过程；链接/哈希/泄漏检查见final-validation.json |
| S4去重、定级、责任、排期与关闭标准 | 18个确认根因、不建议发布；progress按维护领域/修复启动T0排优先级；提交/关闭日期均无 | 已记录；未冒领自然人批准、风险接受或S5修复 |
| S5修复复验 | 不在本轮授权中 | 不适用；没有修改业务实现来隐藏反例 |

## 外部或扩大验收的开放项

真实LLM/Profile账户、真实KMS/IAM/接收端、生产部署与24h发布soak没有完成，本轮不以模拟数据代替；所需条件与下一步见 [blind-spots.md](blind-spots.md)。完整任意输入/所有故障排列不能由有限评审证明，已具体列出缺口。此处不是把缺陷关闭，而是完成计划要求的可复现评审结论和后续处置。

## 最终交付检查

- [运行退出和清理](evidence/runtime/cleanup.json)：本轮相关实验均有实际终态，11个测试端口无listener，临时浏览器标签已关闭。
- [归档fixture验证](evidence/packaging-validation.json)：9项最终版本均实际通过；8项在all运行通过，抽取时漏import的file项修正后单独通过，没有虚构一次全绿all运行。
- [最终检查](final-validation.json)：44模块、9份计划产物、18个确认根因与11个独立裁决；本地文档链接、归档哈希、实际临时密钥未入库、业务/生成文件未变。
- HEAD保持计划基线，无staged改动；仅review目录和review索引发生本轮变更，官网已有`.gitignore`修改未触碰。没有commit、push、发布或业务修复。
- 未满足的产品不变量保留为开放finding；其余未测场景留在blind-spots和progress中。最终结论“不建议发布”，不是无条件验收。
