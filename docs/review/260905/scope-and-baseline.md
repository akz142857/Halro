# 评审范围与执行基线

状态：S0–S4 带限制评审完成，2026-09-05；按已批准计划9.1的边界完成评审，不宣称全部 INV 通过。本文冻结本轮评审对象、责任范围与证据适用边界，不构成发布批准；修复与复验另行安排。依据为 [已批准计划](review-plan.md)、仓库 [AGENTS.md](../../../AGENTS.md)、当前源码、角色报告与独立裁决。架构、生产对象和不变量见 [architecture-and-invariants.md](architecture-and-invariants.md)。

## 1. 版本与环境

| 项目 | 本轮基线 |
| --- | --- |
| 仓库 | `/Users/ziy/Code/ClayCosmos/Halro` |
| HEAD | `381743f6613607dc256828f4776b52af8bdd232c`；本次文档汇总再次只读核对一致 |
| 基线采集开始 | `2026-09-05T10:01:21Z`，见临时目录 `baseline.json` |
| 工作区 | 采集时 `docs/review/README.md` 已修改、`docs/review/260905/` 未跟踪；这是评审文档工作，不能把执行时工作区称为完全干净。计划“制定时干净”描述另一时点 |
| 系统 | macOS 26.7 / arm64；原始平台字符串 `macOS-26.7-arm64-arm-64bit` |
| Go | `go1.26.6 darwin/arm64`；`CGO_ENABLED=1` |
| 前端 | Node `v24.18.0`，npm `11.16.0` |
| Go 缓存 | `GOCACHE=/Users/ziy/Library/Caches/go-build`；`GOMODCACHE=/Users/ziy/go/pkg/mod` |
| 当前存储版本 | metadata schema **35**，`internal/store/bolt/store.go:24`；版本迁移与 Ledger feature epoch 是不同门槛 |
| 实验位置 | `/private/tmp/halro-review-260905/` 及各角色专用临时目录；使用隔离数据与测试凭据 |

文档汇总只修改本文件与架构文件，不修改生产代码、测试、Git 状态或业务数据，不重复全量门禁。临时目录包含凭据与密钥：这里只引用指定 inventory、结果元数据和已脱敏报告，不归档整个目录。临时路径可能随环境清理消失；长期证据由主持择取、脱敏归档，运行结果索引见 `evidence/runtime/`，说明见 [runtime-evidence.md](runtime-evidence.md)。

## 2. 全仓覆盖归属

临时 `module-inventory.json` 按 `internal/` 第一层目录聚合出 **44 个模块**；它记录文件/行数，不是逐行审查率。`go-packages.jsonstream` 与 `dependency-edges.json` 补充嵌套包和静态依赖；文件被盘点、源码被审查、实验通过是三种不同状态。下表风险表示审查优先级，不是 finding 严重度。

| 生产模块（完整列名） | 主责 / 风险 | 实际方式与边界 |
| --- | --- | --- |
| `app`, `config`, `domain` | 主持架构 / 高 | Runtime、配置、管理操作到快照、资源生命周期、账务组装源码逐链路；可靠性角色补充配置省略语义、时区/价格算术；安全/协议/前端交叉检查消费者。非所有 Admin 分支都有独立动态复验 |
| `budget`, `ledger`, `durable`, `store`（含 bolt、lock）, `usage` | 主持核心/持久化 / 高 | 准入、WAL、状态 Apply、重放、派生、锁和只读升级源码；全量 Go、相关 race、真实二进制升级；summary 交接窗口独立复现。小型 fixture 的备份/恢复、错误材料拒绝及 SIGKILL 后重启已完成，范围见运行证据 |
| `gateway`, `gatewayapi`, `openaiapi`, `anthropicapi`, `compatibility`, `semantic`, `sse` | 协议角色 + 主持 / 高 | HTTP→语义→治理→适配器链、SSE、Files/Batches/deferred、错误分类；本地官方 SDK 和定向 fixture。不是全端点×全 Profile 实测笛卡尔积 |
| `provider`, `modelcatalog` | Provider 角色 / 高 | Profile 绑定、枚举与能力来源、启用资格、错误与重试；PROV-01 独立适配器→网关裁决；无付费真实上游调用 |
| `auth`, `adminauth`, `bearercred`, `hostsecurity`, `safetransport` | 安全角色 / 高 | Gateway/Admin/metrics/anchor 身份域、项目与角色授权、会话并发、SSRF 与传输防御；会话退出竞态另有独立裁决 |
| `kms`, `masterkey`, `vault` | 安全角色 / 高 | 文件密钥、DEK 与 KEK 区分、密封与轮换；真实 Halro 代码加 fake KMS，轮换独立裁决；真实 AWS/KMS 未验收 |
| `audit`, `failurecapture`, `contentscan`, `redaction`, `tokenguard`, `idempotency` | 安全角色 + 主持/协议 / 高 | 审计、内容最小化、流处理、留存和幂等范围；有负向测试与源码防御链。大历史 Audit 查询容量尚未量测 |
| `limiter`, `sourcelimit`, `circuit` | 主持/协议 + 可靠性 / 高 | 配额、并发、来源限制、尝试界限、熔断与释放；共享门禁及定向测试，跨项目长期公平性不由单位测试保证 |
| `backup` | 主持持久化 / 高 | 实现/运维契约与锁边界；小型 fixture 备份/验证/恢复及拒绝矩阵已完成，不能推导生产 RTO/RPO 或全部留存对象恢复 |
| `alert`, `deadman`, `logging`, `safelog` | 可靠性 + 安全 / 高 | 有界队列、关闭、重试、日志脱敏/轮转、通知持久化；REL-01 独立裁决；无真实外部接收端闭环验收 |
| `webui`、`web/` | 前端角色 / 中高 | `web/src` 页面、组件、查询、API 消费者与嵌入组装；全前端门禁及五个定向复现。压缩 bundle 按生成链审查；主持已完成部分真实浏览器旅程，具体结果及未覆盖项见下表 |
| `buildinfo`, `id`, `requestmeta`, `timezone` | 主持 + 可靠性/协议 / 中 | 构建身份、标识/请求元数据、时区基础支持，作为调用链与静态依赖盘点；没有逐模块独立故障实验 |

以上 44 个名称均相对于 `internal/`；`web/` 为额外前端根目录，不计入 44。

| 其他生产/交付对象 | 主责 / 风险 / 已覆盖方式 |
| --- | --- |
| `cmd/halro`、`cmd/halro-deadman` | 主持/可靠性，高；CLI、诊断、升级、统计、生命周期；真实二进制局部实验 |
| `configs/`、`catalog/` | 主持/Provider，高；嵌入默认配置与示例契约、目录存在性和能力声明、签名/更新路径；本地 fixture 不证明远端样本真实性 |
| `.github/workflows/`、根构建/依赖文件、容器配置 | 可靠性，中高；三个 workflow、依赖锁、构建和发布声明源码检查；远端分支保护/审批/实际发布未操作 |
| `deploy/`、`scripts/` | 可靠性/主持，高；systemd、Kubernetes、观测与 AWS KMS/备份运行契约；云环境和全部运维脚本未执行 |
| `tools/m11`、`tools/modelcatalog`、`tools/release` | 可靠性/Provider，中高；抽样工具和发布校验链，不声称每个 smoke/云策略逐行验收 |
| `tests/compatibility/` | 主持/协议，高；官方 Go、Node、Python SDK 对本地可控服务运行；不代表所有 SDK 版本或真实上游兼容 |
| `docs/` | 各角色，中；ADR、manifest、相关 runbook/发布/产品承诺交叉核对；历史材料只作待复验线索，未穷尽每篇文档 |
| 官网、Realtime、HA | 官网只读承诺一致性抽查已完成，版本边界见下节；Realtime 是设计范围、HA 是提案范围，不作为已实现能力验收，不把单写者产品边界本身定为缺陷 |

`internal/app` 在清单中有 69 个生产 Go 文件、26,659 行，是组装、管理面与运维编排的集中处。静态依赖既有 gateway→协议类型/适配器，也有 ledger→provider；所以“协议中立”是治理职责与语义抽象目标，并非当前依赖图已完全消除协议类型。分层提炼属于架构建议；文件大小或依赖边本身不构成已确认 BUG。

### 官网只读一致性抽查

主持已完成 `Halro-website` 本地源码只读抽查：HEAD 为 `79c2b63dcbff505a21f1b217768bbf31b5171610`，工作区存在用户的 `.gitignore` 修改，未触碰。读取范围为 `src/content/docs/docs/index.mdx`、`src/content/docs/docs/guides/version-v0.7-preview.md`、`src/data/contracts.meta.json`、`src/config/docs-version.ts`、`src/pages/story.astro:69`；这是主持提供的抽查证据，不是本文作者重复执行的线上站点验收。

站点明确区分 **v0.7 预览**与**稳定 v0.6**；预览契约对应分支 `codex/run-governance-prd-review`、source SHA `8f185de7674c52f4eab364a7e1a6c61039b0c418`，不是本轮 Halro 的 `381743f6613607dc256828f4776b52af8bdd232c`。`story.astro:69` 明确 Standalone 单写者，未宣称 Realtime WS/WebRTC 可用；所抽查的架构描述与本轮单写者/Realtime 非已实现验收范围没有冲突。

因此不以该预览的 **7 个 Governance endpoint** 对当前评审基线报“缺功能”，也不以网站历史 **148/148** 验收代替本轮测试或当前代码可用性证据。此结论仅覆盖列出的版本标识、契约来源和架构文案，不代表整个网站、线上部署或所有链接/功能均已验收。

## 3. 入口与契约盘点

`route-inventory.json` 从 `internal/app/runtime.go` 提取 **153 条显式 method 注册**（GET 64、POST 56、DELETE 18、PUT 14、PATCH 1）。这是三个 listener 中的注册出现次数，包括重复 health；不是 153 个唯一公网功能，也不计 wildcard mount、middleware、隐式 HEAD/OPTIONS。源位置见 `runtime.go:1505`、`:1562`、`:1718`。

| 入口族 | 边界与覆盖 |
| --- | --- |
| Gateway health、Chat、Responses、Embeddings、Messages/count_tokens | 依次区分健康检查、认证/准入和具体 Profile 支持；HTTP 路由存在不代表任意模型可用 |
| Moderations、Images、Audio、Files、Batches、Rerank、Async、deferred CRUD | 按 manifest 标注实验性、资源归属、幂等及存储约束；部分南向 Profile withheld，不能由北向注册推定新配置可选 |
| Admin setup/login/logout/MFA、Provider/Deployment/Route/Project/Key、策略、价格、Usage、Audit、资源与设置 | cookie、origin/CSRF、角色及 step-up；前端可见性不是授权。相关资源和快照边界见架构文件 |
| Admin 模型/目标列表与刷新 | `GET /admin/api/v1/providers/{id}/invocation-targets` 使用 `requireAdmin`，同路径 `POST` 刷新及 wildcard `POST` 解析使用 `requireAdminMutation`；另有受保护的 `provider-profiles`、`routes` 列表。这些不等于北向 `/v1/models` |
| Metrics/anchor 与嵌入 UI | metrics/anchor 各自的 bearer 策略与限额；UI 路由 fallback 不获得 API 权限 |

当前 Gateway **不服务 `GET /v1/models`**：`runtime.go:1505-1559` 没有该注册，`gatewayapi/handler.go:1039-1057` 的 NotFound 返回 HTTP 404 / `endpoint_not_implemented` 并提示使用 Project 公共 alias。正常已注册 OpenAI 组按 stale→LimitOpenAI→GuardOpenAI 执行，Anthropic 组使用对应 middleware；未注册路径的 NotFound 不能被描述为受该分组的认证链保护。Admin invocation-targets 列表/刷新（`runtime.go:1642,1647-1648`）与南向 Provider 的 models 枚举/描述又是独立层次；不能把北向缺少路由推导成上游没有模型列表。

精确 endpoint×Profile×操作状态沿用 [Provider 报告](roles/provider.md) 与仓库 [endpoint manifest](../../compatibility/endpoint-manifests.json)，不要从单个适配器方法推导可用性。PROV-01 最终范围以 [独立裁决](roles/provider-adjudication.md) 为准；初稿中的“等待复核”已被后续裁决取代。

## 4. 当前证据账本

原始共享目录为 `/private/tmp/halro-review-260905/`，仓内摘录为 [gate-results.json](evidence/gate-results.json)。本文件引用已有执行，不表示文档作者重跑；`-count=1` 表示不是用 Go 测试缓存作为成功证据。全量运行仍可能包含显式 skip/环境选择，不等于所有可选集成测试执行。

| 证据 | 命令/结果 | 能说明什么 / 不能说明什么 |
| --- | --- | --- |
| 初次 Go 环境失败 | `go test -count=1 ./...`，exit 1，19.32s；`go-full.json` | 主持确认缓存/httptest socket 沙箱限制；与源码失败分列，不再算未通过产品门禁 |
| 提权后的 Go 全量 | 同命令，exit **0**，213.75s；`go-full-unsandboxed.json` | 当前基线既有自动化通过；不是没有未覆盖缺陷 |
| Go vet | `go vet ./...`，exit **0**，0.44s；`go-vet.json` | 静态检查通过，不替代生命周期测试 |
| 前端三门禁 | `npm run typecheck` / `npm test` / `npm run build`，exit **0/0/0**，0.73/10.87/0.93s；`web-gate.json` | 当前 TS、测试、打包通过；嵌入产物另有 `git diff --exit-code -- internal/webui/dist` exit **0**、无漂移，见 [runtime-evidence.md](runtime-evidence.md) |
| 相关并发检查 | `go test -race -count=1 ./internal/gateway ./internal/budget ./internal/ledger`，exit **0**，59.79s；`concurrency-race.json` | 选中运行无 race 报告；不证明没有业务时序竞态 |
| 官方三个语言 SDK | 主持确认 Go、Node、Python 本地通过；`sdk-go.log` 有 `TestOfficialGoSDK` PASS，Node 日志为脚本入口，Python 日志为空 | [runtime-evidence.md](runtime-evidence.md) 已记录三语言命令/exit 0；Python 成功无 stdout。服务是 compatibility fixture HTTP handler，不是完整生产 Runtime/持久账务 |
| 独立定向实验 | 各 [roles](#5-角色报告及裁决优先级) 内有命令、exit、临时 overlay 与断言 | 包括“测试成功复现缺陷”；不能把 exit 0 误读为不变量满足 |
| 真实二进制升级/只读 | `upgrade/results.json` 与 `upgrade-seeded-dns/results.json`；详见下段 | 空库及含配置/失败请求 Ledger 的两种演练，不是所有历史 schema/大数据/密钥模式升级矩阵 |
| 真实浏览器局部旅程 | 主持最新执行回报：实际登录；UI 创建停用 Provider/Deployment、Project；刷新模型、Key 禁用、Usage 失败诊断 | 首次 setup 与 Key 生成通过 API，非全 UI 初始化/凭据创建验收。细节见下段；统一运行证据见 [runtime-evidence.md](runtime-evidence.md) |
| 两个短时 fuzz | 主持确认 SSE no-panic 与 redaction stream 等价各运行 **15 秒并通过** | 局部性质验证；不证明 CR-only SSE 增量性已修复或全部脱敏输入正确；准确命令、执行量与日志见 [runtime-evidence.md](runtime-evidence.md)；没有失败样本，新 interesting corpus 未交付，不能逐样本重放 |
| 备份/恢复/进程中断 | **已完成小型 fixture 演练**：create/verify/正确 restore exit0；四类拒绝 exit1 且目标 hash 不变；恢复与 SIGKILL 后认证/Usage 验证通过 | 具体 sequence、会话失效与耗时见下段及 [runtime-evidence.md](runtime-evidence.md)；不代表物理断电或全部资源迁移矩阵 |
| 30 分钟运行冒烟 | **已完成，smoke_only**：exit0、passed=true；61 samples、899 success、0 fail | goroutines 21→21、FDs 15→15，采样队列均0；RSS与采样局限见下段及 [runtime-evidence.md](runtime-evidence.md)；不是发布长稳门禁或容量承诺 |

主持回报的浏览器证据具体为：首次 setup 用 API，随后实际浏览器登录；UI 成功创建停用 Provider；刷新已有 mock 返回 `review-chat`，unknown 模型未自动获得能力，手动声明后 UI 成功创建停用 Deployment。UI 创建 Project，设置每日预算 **$0.01**、正文上限 **1 KB**、来源 **127.0.0.1/32 CIDR**、alias **review-chat**。Key 经 API 生成后正常调用 HTTP **200**，2 KB 正文 HTTP **400 / request_too_large**，未授权 alias HTTP **403**。另一专用 Key 的调用从 **200 → UI 禁用成功 toast → 再调用401**，因有服务端拒绝复核，证据不止于 toast。Project 配置保存不等于已验证预算耗尽或非允许来源拒绝。

Usage summary 的 failure link 已在真实浏览器复现 **FE-01**；主动点击对应 tab 后，可读取 mock 500 的失败详情。这是缺陷复现与绕行诊断成功，不能记为 drilldown 通过。**MFA、权限降级、异步浏览器旅程尚未执行，仅有 router/组件测试证据**；其余 FE-02–05 仍按角色定向复现范围记录。

真实二进制演练使用 v0.5.0 初始化的 schema 33，当前启动升级至 35。旧版 init/doctor exit 0；新版 doctor、ledger、usage 在旧 schema 上均 exit 1；只读前后 metadata、空 Ledger、Audit 哈希一致，旧版 doctor 仍 exit 0。随后新版启动 ready，第二 writer exit 1，新版 doctor exit 0，旧版 doctor 在新 schema 上 exit 1。这里的拒绝是预期防御结果，不是测试失败。

补充的**非空升级已完成**：`/private/tmp/halro-review-260905/upgrade-seeded-dns/results.json` 记录旧 v0.5.0 init/bootstrap exit 0，建立合成 Provider/Project/Route 元数据；使用保留 `.invalid` 域名故意产生 DNS 发送失败，网关返回502。旧 `ledger verify` exit 0，`Authenticated=8`、`ChecksumOnly=0`、`ChainVerified=true`、sequence **8**。新 doctor、ledger verify、usage verify 在旧 schema 上均 exit 1，实验比对的所有数据文件 hash 不变。新 start 升级后 `ledger verify` 与 `usage verify` 均 exit 0，Ledger 仍为8条认证记录、sequence8，旧 doctor 拒绝新 schema（exit1）。旧/新 ledger 日志的 head、offset、chain hash 也一致。此证据覆盖非空失败请求记录保留，**没有真实上游成功费用或全部资源/定价迁移数据集**；详见 [runtime-evidence.md](runtime-evidence.md)。

升级后**空 Ledger 的 `ledger verify` exit 1、`usage verify` exit 0**，按原始结果如实保留。这说明历史 P15 的空输入诊断一致性仍有局部限制；本轮把它列为文档/验收边界，不新增高危，不推导非空 Ledger 被损坏或未经认证重建。该演练没有证明完整对象快照恢复、磁盘满/断电迁移恢复、RPO/RTO 或降级受支持。

### 最终备份、恢复与运行冒烟证据

以下为主持最终执行结果，原始索引归档在 `evidence/runtime/`，统一说明见 [runtime-evidence.md](runtime-evidence.md)。备份 `create`/`verify` 均 exit **0**。错误 backup key、错误 confirmation、截断备份、错误 master key 的四种 restore 均 exit **1**，各次目的文件 hash 不变；正确 restore exit **0**，恢复 Ledger sequence **5020** 与 source 相同且认证通过，usage verify exit **0**，旧 session 请求 **401**。已完成的是该 fixture 的恢复与拒绝检查，不能外推所有对象/密钥模式。

恢复后调用 **200**，再执行 **SIGKILL -9**；Ledger 仍认证通过，sequence **5025**。重启约 **0.333秒**达到 ready，再调用 **200**，最终 Ledger sequence **5030** 认证通过、usage verify exit **0**。中断发生在成功响应之后，不能用此证明所有在途发送/settle 中断窗口；SIGKILL 也不是物理断电。另一次正确 restore **0.255秒**加 ready **0.249秒**合计 **0.504秒**，只是小型本地 fixture 指标，不是生产 RTO/RPO 承诺。

30分钟冒烟最终为 **smoke_only / exit0 / passed=true**，**61次资源采样、899次成功、0次失败**。RSS 起点 **32,948,224 bytes**，终点 **71,794,688 bytes**，峰值 **128,188,416 bytes**；goroutines **21→21**、FDs **15→15**，队列采样值均 **0**。起终点与离散采样不能证明中间没有短峰或长期无内存增长，RSS终点也没有回到起点。实验使用本地 mock、并行存在其他评审负载，不是生产容量、跨项目公平性或同机版本性能比较；不替代24小时 release soak或更长压力实验。

真实 cloud、物理断电及已登记留存对象跨轮换的完整实机矩阵未验证；已有 SEC-01 定向加密/生命周期证据仍成立，不能由备份恢复成功关闭。上述未覆盖项随本轮评审结项保留，不再标为正在执行。

## 5. 角色报告及裁决优先级

| 报告 | 本文使用范围 |
| --- | --- |
| [security](roles/security.md) | 授权矩阵、密钥/留存、MFA、Audit 查询；SEC-03/04/05 按各自证据强度保留 |
| [security-session-adjudication](roles/security-session-adjudication.md) | SEC-02 P1 独立确认；已退出 cookie 在特定刷新交错后可用于新请求，非匿名绕过 |
| [security-rotation-adjudication](roles/security-rotation-adjudication.md) | SEC-01 P1 独立确认；换 Master Key/DEK 遗漏留存密文，**不包括保持 DEK 的 KEK rewrap** |
| [provider](roles/provider.md)、[provider-adjudication](roles/provider-adjudication.md) | PROV-01 P1 确认在真实 OpenAI Chat profile 桥接路径；PROV-02 CR-only SSE 增量性 P2；上游实际收费未验证 |
| [frontend](roles/frontend.md) | 五项 P2：同页 Usage URL 状态、第一页下拉、debug key 重试身份、pending 确认框退出、字节配额舍入；FE-01 已在真实浏览器复现，其余浏览器复核未执行，作为结项覆盖限制保留 |
| [reliability](roles/reliability.md) | dead-man、stats gauge、发布文档；配置删除承诺反例；定价 provenance P3 与历史时区局限；性能档案适用性 |
| [deadman-adjudication](roles/deadman-adjudication.md) | REL-01 P2 缩窄到 send 后任一保存完成前的崩溃窗口；Usage summary checkpoint 在途遗漏 P2，非永久账务损失 |

严重发现的最终范围优先采用独立裁决，不能叠加初稿更宽的推论；当前源码与新证据优先于历史“已修复”。本轮未产生修复，建议均为提案。所有 finding 的统一编号、去重、发布处置由主持维护 [findings.md](findings.md) 与 [adversarial-verdicts.md](adversarial-verdicts.md)，本文不另立一套总裁决。

## 6. 尚不能外推的结论

- 没有真实付费 Provider、真实 KMS、生产云部署、真实告警接收端验收；本地官方 SDK 只证明所执行的序列化/HTTP 契约。
- 浏览器仅上述局部旅程已有主持实测；MFA、降权、异步及其余未执行流程不标通过。小型 fixture 备份/恢复与30分钟 smoke_only 已完成；短时 fuzz 和冒烟不代表长期容量/SLA。
- 性能角色核验归档 source hash 的适用性，不是重跑性能。旧主机/旧 frame 10 GiB 恢复结果不充当本轮 epoch-4 大规模恢复或 RTO。
- 大数据 Audit 查询、长期元数据/日志增长、所有故障点物理断电、跨项目公平性与全 Profile 错误矩阵仍有空白。
- 全量门禁、race 和覆盖文件数不能关闭已复现逻辑缺陷；INV-01–10 的具体支持、反例与未验证项见架构文件。
