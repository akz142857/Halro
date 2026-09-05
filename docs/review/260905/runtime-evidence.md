# 本轮运行证据

基线 `381743f6613607dc256828f4776b52af8bdd232c`，2026-09-05；macOS 26.7 arm64、Go1.26.6、Node24.18.0、npm11.16.0。只使用隔离目录和合成请求。没有真实 LLM Key、付费调用或生产变更。归档入口见 [evidence/README.md](evidence/README.md)，专项原始运行记录见 [roles](scope-and-baseline.md)。

## 1. 集成门禁与定向实验

下表的通过代表指定命令覆盖的范围，不代表无缺陷。复现测试的 exit 0 可以是在断言缺陷存在。

| 命令 / 工作目录 | 实际结果 | 范围与原始证据 |
| --- | --- | --- |
| `go test -count=1 ./...` / 仓库根 | exit0，213.753s | 当前全 Go 集成基线；`go-full-unsandboxed.log/json` |
| `go vet ./...` / 根 | exit0，0.436s | 静态检查；`go-vet.log/json` |
| `npm run typecheck` / web | exit0，0.735s | `web-typecheck.log` |
| `npm test` / web | exit0，10.865s；43文件、531 tests | `web-test.log`；不包含角色额外复现数量 |
| `npm run build` / web | exit0，0.926s | `web-build.log` |
| `git diff --exit-code -- internal/webui/dist` / 根 | exit0，无漂移 | 本轮重建对应当前提交的内嵌bundle |
| `go test -race -count=1 ./internal/gateway ./internal/budget ./internal/ledger` | exit0，59.788s | 并发/结算主链；`concurrency-race.log/json` |
| `go test -race -count=1 ./internal/circuit ./internal/limiter ./internal/sourcelimit` | exit0，4.495s | 补充准入组件并发；`admission-race.log/json` |
| `go test -count=1 ./internal/sse -run=^$ -fuzz=^FuzzDecoderNeverPanics$ -fuzztime=15s -parallel=2` | exit0，17.334s墙钟；2,457,126次执行 | 无crash样本；不检查所有事件及时交付契约，故不能推翻PROV-02 |
| `go test -count=1 ./internal/redaction -run=^$ -fuzz=^FuzzBoundedStreamMatchesNonStream$ -fuzztime=15s -parallel=2` | exit0，18.613s墙钟；77,558次执行 | 受限stream与非stream等价；不是无限输入/所有规则组合证明 |
| `npm audit --json --registry=https://registry.npmjs.org` / web | exit0；本轮registry响应199个依赖、0个advisory | 只读扫描，`npm-audit.json`及result；不是永久无漏洞保证 |
| 固定版本 `govulncheck@v1.6.0`、观测规则/发布脚本检查 | 结果和准确命令见[可靠性报告](roles/reliability.md) | 本地依赖/规则证据；不证明远端分支保护、云告警投递或已发布镜像 |
| SEC/PROV/FE/REL/SUM/CFG临时定向复现 | 见各角色及[独立裁决](adversarial-verdicts.md) | [可移植runner](evidence/run_repros.py)不修改生产源；不要把定向fixture插入当真实流量 |

Go最初沙箱运行因缓存权限及httptest监听限制失败；允许相同本地测试所需权限后完整通过。漏洞扫描最初DNS受限后另行完成。Python最初系统3.9环境无法安装固定SDK版本，改用本机Python3.12和官方PyPI安装成功。上述属于实验环境，不纳入BUG计数。没有因为纯Markdown变化再次执行整套门禁；CI Node22与本机Node24的差异保留为环境边界。

Fuzz原始命令、耗时、进度在 `fuzz.json`、`fuzz-sse.log`、`fuzz-redaction.log`。未产生失败样本，不声称保存了不存在的最小crash输入；新interesting corpus在Go缓存而非交付中，此处记录的是限时随机运行，不能逐样本重放。

## 2. 官方 SDK 本地对照

编译仓库 `tests/compatibility/server`，仅监听 `127.0.0.1:28088`；使用仓内固定的Go/Node/Python依赖。此服务运行真实HTTP handler及compatibility fixture service，**不是完整生产Runtime/持久账务，也不是实际Provider**。

| 语言 | 实际命令（地址通过环境传入） | 结果 |
| --- | --- | --- |
| Go | `HALRO_COMPAT_BASE_URL=http://127.0.0.1:28088/v1 go -C tests/compatibility/go test -count=1 -v ./...` | exit0，SDK test约1.255s；`sdk-go.log` |
| Node | 临时副本`npm ci --ignore-scripts`，再`HALRO_COMPAT_BASE_URL=... npm test --prefix <temporary-sdk-node>` | 安装与测试exit0；`sdk-node-install.log`、`sdk-node.log` |
| Python | Python3.12隔离venv安装仓内requirements，`HALRO_COMPAT_BASE_URL=... <venv>/bin/python tests/compatibility/python/test_sdk.py` | 安装与测试exit0；`sdk-python312-install.log`、`sdk-python.log`（成功时无stdout） |

逐端点/SDK声明和24个Profile的可用/withheld差异见 [Provider矩阵](roles/provider.md#endpoint-coverage)。这些脚本不覆盖所有实验性文件/批处理/媒体/deferred接口，不能用三个语言通过推导完整笛卡尔积通过。

## 3. 本地浏览器与真实Runtime

### 实验装配与可信范围

Gateway/Admin/Metrics分别监听 `127.0.0.1:28080/28081/29090`；mock TLS服务仅监听`127.0.0.1:28443`。采用完整默认配置模板，覆盖监听、临时data/master-key路径及私有上游选项；原始配置和密钥不归档。正常请求为小型合成文本，mock usage固定input3/output2，input/output价格分别每百万1,000,000/2,000,000 micro-USD，测试主项目预算1USD。

未修改的SafeTransport拒绝回环上游；为了在单机隔离环境验证UI、持久化和请求链，临时构建`halro-fixture`使用两处**测试专用Go overlay**：注册只含本次自签名证书的进程级fallback roots，并在private-provider选项为true时允许精确`127.0.0.1`。系统证书库、仓库生产源码和网络监听边界未改。其余Runtime、路由、治理、账务、存储和内嵌UI使用当前基线。故该实验不能证明未修改SafeTransport的真实TLS/SSRF端到端安全；对应真实源码单测与安全专项另列。

曾考虑将mock监听到主机LAN地址以绕过回环限制，自动审批拒绝，理由是会把模拟服务暴露到局域网。该操作没有执行；最终采用上述仅回环的测试装配。没有绕过浏览器证书警告，也没有使用真实上游凭据。

### 实际完成的旅程

使用Codex in-app browser与本地测试administrator。下面的界面行为均在实际内嵌bundle上操作并观察结果；并非只有组件fixture。

| 步骤 | 实际结果 / 限制 |
| --- | --- |
| 首次初始化与登录 | 通过API初始化合成管理员，再在浏览器登录；**首次setup表单未通过浏览器提交** |
| Provider创建/测试 | 浏览器创建`Browser Review Provider`并保存为停用，成功toast与列表相符；另一个预置mock Provider测试显示1/1接口正常 |
| 模型刷新与能力 | 创建部署表单刷新实际mock列表，返回`review-chat`；显示unknown，不能从ID列表自动得到能力；人工声明Chat/Streaming并选择Profile后，创建停用部署成功；测试前启用受阻，测试后仍显示未定价 |
| Project策略 | 浏览器创建`Browser Review Project`：review-chat别名、60RPM、1KB body、0.01USD/day、CIDR127.0.0.1/32；保存toast及列表数值正确 |
| Project Key与调用 | Key通过API创建。新项目小请求200，2KB正文400 `request_too_large`，未授权别名403 `model_not_allowed`；不是仅检查按钮状态 |
| Usage与失败诊断 | 有正常已结算调用；注入一次mock HTTP500后网关502，UI显示最终失败、上游500、mock_failure和1次尝试；点击详情可查看 |
| FE-01浏览器反例 | 汇总页“查看最终失败”改变URL至`?tab=failures...`，汇总tab仍选中；直接点击最终失败tab才切换。确认URL和UI状态不同步，不是加载等待 |
| 停用Key后调用 | 专用`Browser Lifecycle Test` Key先调用200，UI确认禁用后显示禁用toast；同Key随后401 `invalid_api_key`；soak主Key保持可用 |
| 键盘与视觉抽查 | 空白新建Project模态用Escape关闭，焦点返回新建按钮；桌面截图检查无明显遮挡/横向溢出。没有覆盖完整键盘、读屏、窄屏、200%缩放或正式WCAG合规 |

API初始化、生成Key与浏览器创建资源混合组成链路；不能声称每一步均由浏览器完成。MFA登记/降权/过期/密钥轮换/异步取消删除的**浏览器**旅程仍缺失，已有router/组件/定向后台证据见 [blind-spots.md](blind-spots.md)。测试浏览器标签已关闭。

## 4. 两种真实二进制升级演练

从本地`v0.5.0` tag归档源构建旧二进制（tag peel `32885915b876d39b43e9293d7bf031e920a2b77b`），与当前未加overlay的二进制进行比较。该tag身份与历史性能fixture基线身份应分开记录，不推定它们是同一个提交。

- 空库：旧init/doctor成功；新doctor、ledger verify、usage verify对schema33均exit1，未迁移；doctor前后数据文件hash相同，旧doctor仍成功。新start升级schema35并ready（约0.234s）；第二个serve被目录锁拒绝；停机后新doctor成功、旧doctor拒绝新schema。证据`upgrade/results.json`。
- 含配置和Ledger：旧版bootstrap建立合成项目/Provider/Route；使用保留的`.invalid`域名产生发送失败（网关502），不访问真实Provider。旧Ledger认证通过且sequence8；新三个只读命令均拒绝、所有数据文件hash不变；新start升级后Ledger仍认证通过且sequence8，usage verify成功，旧doctor拒绝。证据`upgrade-seeded-dns/results.json`。这证明非空失败请求记录保留，不代表旧成功费用、全部资源/定价迁移数据集。
- 旧版直接配置回环上游的首次补充fixture被拒绝，改为保留`.invalid`域名；这是旧版边界行为，不列新缺陷。
- 历史P15部分仍存：干净空Ledger `ledger verify`输出0 frames、ChainVerified=false并exit1；同目录`usage verify`已exit0。未把空链错误提升为账务损坏。

## 5. 备份恢复与进程中断

运行中的隔离实例拒绝`backup create`，exit1 `data directory is already locked by another process`，未产生archive。30分钟smoke结束后核对进程命令，SIGTERM停止仅本次fixture；以其数据运行未修改的`halro`离线备份/校验/恢复命令。完整结果与各命令耗时见 [restore-results.json](evidence/runtime/restore-results.json)，没有归档备份或密钥。

| 演练 | 实际结果 |
| --- | --- |
| 原账本认证→backup create→verify | 三条exit0；源Ledger sequence5020；加密备份18,203,115 bytes；创建0.289s，verify0.019s |
| 错误备份密钥、截断7字节的archive | verify均exit1；restore也均exit1 |
| 错误confirm-backup-id、错误master key | restore均exit1；与上面两种错误共四种情况，目标目录全部文件hash不变 |
| 正确restore | exit0，原目标marker保留在previous目录；恢复后Ledger认证仍5020，usage verify exit0 |
| 恢复后身份与调用 | 旧Admin cookie访问session为401；恢复保留的Gateway Key调用mock为200，符合CLI对恢复启用Gateway Key的提示 |
| 响应已确认后进程SIGKILL | 进程exit -9；未先优雅关闭；离线Ledger仍认证，sequence5025 |
| 再启动与继续调用 | ready约0.333s，调用200；随后优雅停止，Ledger认证sequence5030、usage verify exit0 |

本fixture正确restore命令约0.255s，启动到ready约0.249s，两段服务时间相加约0.504s；**不包含人工准备、离线等待及中间验证命令，不是连续事故RTO**，也不是生产恢复SLO。备份数据小、文件缓存热、运行于本机，未验证多GiB数据、全部已登记retained对象、换钥后的恢复或云KMS。观察到本次已确认调用的5个新增Ledger事件在进程kill后仍可认证；这不是物理断电测试，不能承诺生产RPO=0，未涵盖上游已执行但尚未返回确认的中断。

## 6. 30分钟运行冒烟

采用当前`tests/soak`工具，2秒一次请求、30秒一次资源采样、单个mock Chat模型。`2026-09-05T10:11:56.312279Z`开始，`10:42:01.393464Z`完成最终采样；配置负载30分钟，含结束采样墙钟约30分5秒。真实进程exit0，summary标为`smoke_only`、`passed=true`。见 [summary](evidence/runtime/soak-summary.json)、[61个资源样本](evidence/runtime/soak-samples.jsonl)、[请求计数时序](evidence/runtime/soak-requests.jsonl)、[退出记录](evidence/runtime/soak-valid-exit.json)。

| 指标 | 开始→结束 / 采样最大值 |
| --- | --- |
| 负载器请求 | 成功899、失败0；不含另外100次混合probe与浏览器/API验证调用 |
| RSS | 32,948,224→71,794,688 bytes（31.42→68.47 MiB，增长37.05 MiB）；最大128,188,416 bytes（122.25 MiB） |
| goroutine | 21→21，最大29 |
| 文件描述符 | 15→15，最大19 |
| WAL queue / capacity | 各样本queue0、capacity4096；采样没有捕获峰值不代表任意瞬时都为0 |
| WAL append errors / analytics lag / analytics queue | 各采样为0 |

工具阈值为最终RSS增长不超过64MiB、goroutine/FD增长不超过20、队列与错误等条件，均满足。RSS有峰值回落，**并未证明达到长期稳态或无泄漏**；30分钟不跨越默认每小时的全部维护周期。`summary.maximum.time`是结构保留的初始时间，不是各最大值发生时间；RSS峰值实际样本时间为`10:25:56.430322Z`。使用jsonl解释时间，不能把maximum的单一timestamp解释为所有资源同时达到峰值。

此前一次尝试因mock缺少`GET /models/{id}`，真实健康探测正确下线目标而产生503；该fixture已修复并中止旧样本，不能将其混入有效运行。有效运行使用新的输出目录。并行存在测试/浏览器工作，因此不用于同机版本性能比较。30分钟永久标为`smoke_only`，不能替代仓库规定的24小时release soak或2–4小时延长实验。

### 补充：单次与流式延迟样本

在同一隔离Runtime运行100次请求（50 unary、50 SSE）、客户端并发4，2.720秒全部200且正文/终止标记符合fixture；观察到36.77请求/秒。nearest-rank客户端完整响应延迟：unary p50/p95/p99为99.26/152.61/164.41ms，SSE为99.71/158.91/169.29ms。原始逐请求耗时见`latency-probe.json`。这是100次混合、无真实模型思考延迟的局部样本，SSE数值是完整响应而非首token，不是最大吞吐、生产SLO或版本性能比较；sample包含建立客户端连接、真实持久化和mock TLS。该短脉冲发生在30分钟smoke内，应结合资源时序解释。

## 7. 证据保存与可复现性

原始根目录为`/private/tmp/halro-review-260905`，各角色使用独立review/adjudication目录。只选择小型脱敏日志、fixture、退出码和SHA-256索引归档；不保存密钥、cookie、Provider credentials、运行数据目录或加密备份。可移植复现runner从仓库根构造Go overlay；fixture成功可能表示缺陷存在。原始环境安装问题和修正后的命令分别记录，不能覆盖失败后只保留“全绿”的叙事。

Metrics实测匿名401、错误bearer401、正确专用bearer200，见 [metrics-auth](evidence/runtime/metrics-auth.json)。本轮fixture/恢复进程、mock、compatibility server均已停止，11个使用过的测试端口无残留listener，见 [cleanup](evidence/runtime/cleanup.json)；临时浏览器页已关闭。只清理本轮识别的进程，保留临时测试材料用于排查，未触碰其他业务实例。
