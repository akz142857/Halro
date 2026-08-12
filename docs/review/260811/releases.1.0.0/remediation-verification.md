# v1.0.0 整改核对报告（2026-08-12）

> 核对对象：`release-1.0.0-report.md` §6 的 46 个整改项（R-01/R-02/R-03p/R-04p + R-03~R-44）、
> §8 的 20 条已知限制、§9.2 的 10 条历史结论修正、§9.3 的 G7 十七条裁决，
> 在**未提交的工作区改动**上（分支 `codex/release-1.0.0-review-remediation`，
> 98 个已跟踪文件 +2907/−464，另 16 项未跟踪新增）。
>
> **核对基准是报告 §6 的关闭判据原文，不是 [progress.md](progress.md) 的自述**——
> 那份台账在本轮是被核对对象。
>
> 七个复核角色全程只读：未修改仓库任何文件，未做 git 写操作，未运行 `make reset`，
> 未触碰根目录 `data/` / `master.key` / `config.yaml`，未跑计费 smoke test，未触发 workflow。
> 缺陷态反向验证一律用 `go build -overlay` 把改过的源文件从 scratchpad 挂进包内编译，
> 或在 scratchpad 内的独立 fixture / 容器里复现，仓库文件零改动；每次注入前断言
> "搜索串恰好命中 1 次且替换确实改变了内容"（CLAUDE.md：反向验证不失败就不是证据）。

> **本文分两个阶段写成。** §0~§6 是 2026-08-12 七个角色的**只读核对结论**，
> 记录当时的状态，不因后续修复而改写——那是评审证据。§7 是**修复收尾**：核对
> 发现的每一条未符合项的处置、证据与复跑结果。读的时候把两段合起来看。

---

## 0. 结论（核对时点）

**整改的主体是扎实的，46 项里 32 项达到判据、2 项仓库侧做完只差 GitHub 操作。**
报告 §1.1 的三条独立否决理由，**两条已经真正关闭**：G2 的 fail-closed 反例消失了
（stale 按域建模 + 四域重放 + 能在缺陷态失败的负面测试），公告面从零到有
（CHANGELOG 四条 Operator impact + 20 条已知限制 + "从 rc.1 升级"整节，含 V4 那条
破坏性排查动作）。剩下的 G4 是纯 GitHub 侧动作，不在仓库里。

**但有 12 项未达判据，其中三条必须在打 tag 前处理：**

1. **R-24 是一次功能性回归**——新加的同源门会让 Admin 控制台的两条 GET 在真实浏览器里
   403，而 Go 测试之所以绿，是因为本轮把测试助手改成塞了一个浏览器不会发的头。
2. **R-03 的第三条判据被静默丢弃**——两处 lookup-miss 的 fail-open 既没改也没有任何书面裁决。
3. **R-30 引入了一条新的监控静默通道**——最需要看见 stale 的时候 critical 告警可能失声。

另有一条 pre-1.0.0 纪律违规（i18n 旧错误码与新码并存，且一条陈旧测试正把死码钉住），
和一批"判据要求实测、仓库里只有推算"的项。

| 判定 | 数量 | 项 |
|---|---:|---|
| 符合 | 32 | R-05、R-06、R-08、R-10、R-11、R-12、R-14、R-16、R-19、R-20、R-21、R-22、R-23、R-25、R-26、R-27、R-28、R-29、R-30、R-31、R-32、R-33、R-34、R-35、R-36、R-37、R-38、R-39、R-40、R-42、R-43、R-44 |
| 部分符合 | 11 | R-02、R-03、R-03p、R-04、R-07、R-09、R-13、R-15、R-17、R-18、R-41 |
| **不符合（引入回归）** | 1 | **R-24** |
| 仓库侧就绪，需外部动作 | 2 | R-01、R-04p |

---

## 1. 必须回头处理的问题

### 1.1 P1 · R-24 让 Admin 控制台的两条 GET 在真实浏览器里必然 403

**判据字面达成，实际是回归。** 链条四段，全部在仓库里可逐条核对，我已独立复核：

1. `internal/app/admin_invocation_targets.go:239-249` 新增 `requireCredentialedAdminRead`，
   在角色门之后调 `adminSameOrigin`，失败返回 403 `admin_same_origin_required`；
2. `internal/app/admin_session.go:384-392`：取 `Origin`，空则回退 `Referer`，两者都空即 false；
3. `internal/app/runtime.go:1345` 给整个 adminRouter 挂 `adminSecurityHeaders`，
   `:1474` 设 `Referrer-Policy: no-referrer`——控制台 HTML 自身（`:1462-1463` 的 `/admin`）
   也由这个 router 提供，从它发出的 fetch 因此不带 `Referer`；
4. `web/src/api.ts:77-93` 的 `request()` 对 GET 不设任何自定义头（CSRF token 只在
   `init.method !== "GET"` 时设），而浏览器对同源 GET 本就不发 `Origin`。

受影响的正是新加门的两条：`GET …/invocation-targets?refresh=true`（`api.ts:306`，
DeploymentsPage 的"刷新目录"按钮）与 `GET …/invocation-targets/{id}/resolution`
（`api.ts:313`，在 `DeploymentsPage.tsx:788-793` 是 **useQuery 自动触发**，用户键入
provider model 就会跑）。403 的 code 在 `web/src/i18n/errors.ts` 零命中，界面上会冒出
英文原句 `same-origin validation failed`——恰好是 R-11 刚修完的那类问题。

**判据被绕过的直接证据**：本轮把 `internal/app/admin_ui_settings_test.go:211` 的共用助手
`authenticatedAdminGet` 加了一行 `request.Header.Set("Origin", "http://"+request.Host)`。
测试模拟的是浏览器不会发送的头，所以 `TestReadOnlySessionCannotForceAnInvocationTargetRefresh`
全绿。

**修法在 R-24 原文里本来就给了另一半**：把这两条改成 POST 走 `requireAdminMutation`——
那条路径有 CSRF token，不依赖 `Origin`。角色门（`requireAdministratorRole`）这一半是对的，
应保留。

> 说明：浏览器端未实证（只读护栏下不能起实例配合抓包），但四个环节都是仓库里可核对的代码事实，
> 且测试助手被改这一条是直接证据。

### 1.2 P1 · R-03 的第三条判据被静默丢弃

报告 §6.2 的 R-03 明写"**同时裁决两处 lookup-miss 的 fail-open 方向**
（`redaction/engine.go:428-431`、`tokenguard/manager.go:308-312`）"。

- 两处代码**逐字未动**（`processString` 里 `policy, ok := e.policy(policyID); if !ok { return value, nil }`；
  `admit` 里 `policy, ok := m.policies[input.PolicyID]; if !ok { return Decision{Allowed:true, Status:StatusNormal}, nil }`）；
  这两个包在 `git status` 中不存在。
- **没有任何形式的书面裁决**：不在发布说明的已知限制、不在 `docs/runbooks/configuration-stale.md`、
  不在 `security-review-v1.md`、不在 `progress.md`（R-03 行只写了 stale 分域）。

实际采取的防线是"任一域 stale 即整机 503"这道外层门。**这个选择是合理的**——但报告要的是
一句显式结论，而不是让下一个读代码的人自己推。且它不覆盖"引用了不存在策略 ID 的 Project"
这条不经过 stale 的路径：那种情况下两个安全控制仍会静默放行。

### 1.3 P1 · R-30 引入了一条新的监控静默通道

`internal/app/metrics.go:71-74` 新增的 `ShutdownTruncatedAttempts()` 读失败会 **early return**，
而它位于 `writer.WriteHeader(http.StatusOK)`（`:88`）**之前**；`runtime.go:1512-1515` 只记日志、
不改状态码，Go 于是隐式回 **200 + 空 body**。后果链：

- Prometheus 抓取成功 → `up = 1` → `HalroTargetDown` 不响（它要求 `up == 0 or absent(up)`）；
- `halro_activation_stale` 整族缺失 → **`HalroConfigurationStale` 也不响**；
- 而"元数据存储读不出来"恰恰是 `configuration_stale` 最典型的成因之一。

即：**最需要被看见的时候，本轮刚补的 critical 告警可能失声**。`halro_metrics_render_errors_total`
这个本该说明问题的计数器只能在它自己渲染失败的那份 exposition 里读到，逻辑上闭环，且无告警。
bbolt 只读事务失败概率低，但这是结构性的，不是概率性的。

修法二选一：把该 error 降级为"省略该族并继续渲染"（与同文件 `:79-85` 对 capability gauge
**已经采用**的做法一致），或渲染失败时回 5xx 让 `up` 归零。

### 1.4 pre-1.0.0 纪律违规：i18n 旧错误码与新码并存

R-10 在 Go 侧是**就地改名**的（`admin_onboarding.go:323` `provider_blocking_model → provider_not_verified`、
`:385` `model_blocking_access → model_not_verified`），但两份 locale 字典是**在旧键下面加了新键，
旧键原样留着**：

- `web/src/i18n/locales/en-US.ts:586`（旧，已无代码路径能发射）与 `:587`（新）；`:598` 与 `:599`
- `web/src/i18n/locales/zh-CN.ts` 同样成对

两条旧文案的语义与新语义**相反**（"Connect a model service first." vs "服务已存在但未验证"）。
加重情节：`web/src/pages/DashboardPage.test.tsx:179`（本轮未改）仍断言
`detail_code: "model_blocking_access"`——**一条陈旧测试正把这个死码钉住，后端已不再发射它而 CI 照样绿**。
这正是 CLAUDE.md 点名的"错误构造与其替代品并存 + 已定义但无人发射的占位"。
修法：删掉四行旧键，并把那条测试改成新码。

边界情形（倾向违规但不严重）：`internal/store/bolt/store_admin.go:126-128` 的 `DeleteAdminUser`
现在只是 `DeleteAdminUserWithAuditIntent(..., nil)` 的薄壳，生产调用方已全部迁走，只剩它自己的
store 测试在引用。对照同文件的 `PutAdminUser`——它有三个真实非审计调用方且注释解释了并存理由，
那一条**不是**违规。

### 1.5 R-10 把 "blocking" 换成了方向相反的错话

`admin_onboarding.go:323` 在 `!connectReady` 时一律给 `provider_not_verified`，文案是
"模型服务已存在，但尚未验证当前配置"。但 `connectReady=false` **也覆盖"一个 Provider 都没有"**
（`connectDetail` 会给 `provider_missing`，可级联码不会）。原缺陷是"用 blocking 描述一个已存在的
对象"，现在变成"用 exists 描述一个不存在的对象"——判据要求的正是这一条区分。`model_not_verified`
（`:385`）同理。R-10 的主体（把成功流量作为充分证据）是达成的，这是文案方向问题。

### 1.6 判据要求实测、仓库里只有推算

| 项 | 判据原文 | 实际 |
|---|---|---|
| **R-13** | "64 并发登录时 RSS 增量不再线性" | 机制正确且无旁路（`internal/adminauth/password.go:24-35`，信号量容量 2，全仓 `argon2.IDKey` 只剩 `:35` 一处调用），四条调用点全部改走 `derivePasswordKey`。但**仓库里没有任何实测数字**——`performance-baseline.md:105-111` 新增段是解析性论证（"caps at approximately 128 MiB"）。另：信号量获取无 context/超时（`:32`），登录风暴由"内存线性"转成"排队无期限"，无快速失败 |
| **R-17** | "在只读父目录下 `halro doctor`，detail 必须指向父目录" | 实跑了两种真实形态：(a) **已初始化**数据目录 + 父目录 `chmod 555` → `data_lock` 直接 **pass**（`AcquireExistingReadOnly` 只读打开已存在的锁文件，不需要父目录可写），原 B1-04 复现路径在当前代码上已不成立；(b) **未初始化**数据目录 + 只读父目录（即 B1-01/B1-02 容器失败后运维真正会遇到的状态）→ 落入 `default` 分支，detail 是裸 ENOENT `open …/.halro.lock: no such file or directory`，**不指向父目录、也不说"该目录尚未 init"**。EACCES 分支本身写得对（实跑输出正确指向父目录并给所需权限），但判据在最可能命中的那条路径上未达成。测试只单测了格式化函数，没从 `DoctorWithOptions` 走一遍 |
| **R-03p** | "同一 tag 在两台机器上重建，`sha256sum` 与 `checksums.txt` **逐行相等**" | tar/gzip 与 Go 二进制两侧**已实证可复现**（见 §4）。但 `checksums.txt` 的 8 行里有 2 行是容器包，**实证不可复现**：两次 `--no-cache` 同源构建、同 `SOURCE_DATE_EPOCH`，镜像 ID 不同、`docker save \| gzip -n` 的 sha256 不同（`6c22017c…` vs `31551a99…`），而**镜像内的 `halro` 二进制字节相同**。根因：BuildKit 未加 `--output type=docker,rewrite-timestamp=true`（`SOURCE_DATE_EPOCH` 只改了 image config 的 `created`，改不了层内 mtime）；主 `Dockerfile:3,10,26` 三个基础镜像仍是可变 tag，而 deadman 的 Dockerfile 是 digest-pin 的——同一仓库内的不一致 |

### 1.7 文档类：照抄跑不通的三处

| 项 | 问题 |
|---|---|
| **R-09** | 容器示例（`operator-guide.md:505-527`）给了 `-allow-insecure-public-listen` 与 `-p 127.0.0.1:8080:8080`，却**从未让读者把 `server.gateway_listen` 从默认 `127.0.0.1:8080` 改成 `0.0.0.0:8080`**（`internal/config/default.yaml:17`）。此时监听器绑在容器 loopback，`docker run -p` 发布的端口从宿主访问必然失败，`-allow-insecure-public-listen` 变成空转。**反证在同一工作区**：CI 新增的挂卷启动步骤必须显式 `sed` 改这一行（`.github/workflows/ci.yml:178`）才拿到 `/health/ready` 200。文档缺的正是 CI 补上的那一行。论述部分（`:540-554` 的 TLS/override 二选一、HEALTHCHECK 与外部可达性）是对的 |
| **R-18** | ① `master key already exists` 的**错误文本代码侧未动**——`internal/vault/masterkey.go:30` 仍是裸 `fmt.Errorf`，无路径、无下一步、不指向指南；B1-02 报的正是"不可行动"。② `operator-guide.md:510-511` 的 `chmod 700 ./halro-secrets` + `--user 65532:65532` 在 Linux bind mount 下 init 会 EACCES（CI 同形流程用 `chmod 0777` 绕开，`ci.yml:176`）。③ `:518` 的 `chmod 600` 是冗余的（`masterkey.go:27` 已用 `O_EXCL\|0o600` 创建）。指南里的三分支状态判断（`:532-538`）本身写得好 |
| **R-02** | 命令块内容正确——identity 字符串与 workflow 签名侧**逐字一致**（`release.yml:368`），顺序正确，`README.md:133-135` 还交代了不得换成分支身份。但**判据"没读过仓库的人照着能跑通"仍有缺口且是实测的**：`checksums.txt` 列 8 个文件，只下载自己那一个平台的人执行 `sha256sum --check checksums.txt` 会 **exit 1**（docker 内实测 `FAILED open or read`），随后 `for artifact in halro-*` 又会因缺 `.sigstore.json` 报错。两处都没写"必须下载全部资产"或 `--ignore-missing`，也没给一行 `gh release download`。另：`operator-guide.md:438` 仍只说"要验"、不给命令也不给指针 |

### 1.8 测试面：四处未闭

| # | 项 | 实证 |
|---|---|---|
| 1 | **T-3 缺 1/3** | redaction / Token Guard 两条到位且真能在缺陷态失败（注入"激活失败不标 stale"→ FAIL）；**告警侧"投递失败不得拒流"这条刻意 fail-open 方向零测试**，`activateAlertEndpoints` 全仓零测试引用。这正是 A4 说"最容易在后续重构中被顺手补成 fail-closed 而造成大范围拒流"的那条 |
| 2 | **T-8 半闭** | A4-08 点名 `MaxProviderCalls` 耗尽的**两条**分支。删掉识别阶段 `:529` → 测试 FAIL ✅；**删掉探针阶段 `admin_model_capability_detections.go:427` → 测试仍然 ok** ❌。按 A1 的裁决，这个上限是探测类支出的**唯一**上界 |
| 3 | **T-12 装饰性守护** | `TestProviderAndDeploymentDisconnectsCannotCancelTopologyActivation` 把 `cancel()` 换成 `defer cancel()`（全程不取消）后**照样通过**。被取消的 context 从未被任何激活路径看见，`activateTopologyAfterCommit()` 不收形参。它实际断言的是"禁用会传播"，与已有 Route 用例同构，信息量为零 |
| 4 | **A4-11 未动** | `TestCapabilityDetectionCreateIsIdempotentAndSingleFlight`（`internal/store/bolt/model_capability_detection_test.go:78`）五次调用仍全部顺序、无 goroutine，名字仍承诺 single-flight。A4 给的两条出路（并发化 / 改名）一条都没走 |

另有三处覆盖不足（不阻塞）：`runActivationRecovery` 本体仍零覆盖——删掉 `runtime.go:769`
的启动整行不会有任何测试失败，只有被抽出的两个 helper 有测试；auth 域的 `markStale`
（`admin_projects.go:485`）仍无负面用例；`doctor.go:409-414` 的 `admin_audit_backlog`
检查全仓零测试。

### 1.9 R-07：20 条已知限制中 6 条被写弱

20 条全部有对应条目，**⚠ 的 13/14 两条已按要求改写而非固化**（不再声称"没有指标/告警/runbook"），
第 12、16 两条因 R-30/R-35 关闭而事实已变、改写正确。但：

| 条 | 丢了什么 |
|---|---|
| **#11** | **丢掉 V6 裁决专门救回的那半句**——"真正未连上的失败（连接被拒、DNS 失败、上游 5xx）结算为 0 并全额释放预留"。报告 §9.2 第 9 条**要求保护这条正面结论** |
| #19 | 三个 C3 实测容量数字（1223 lifecycles/s、约 31 变更/s、拓扑协议 −25.3%）**全仓无落点**，`performance-baseline.md` 本次只补了 argon2 段 |
| #9 | 七个具名字段（`frequency_penalty` 等）全丢，只剩 "several optional OpenAI fields"，也没给 manifest 路径 |
| #7 | 丢 `400 unsupported_feature` 与"不预留额度、不建连" |
| #10 | 丢上界（≤12 次调用 / ≤2048B 入 / 16 token 出）与"上游账单会略高于账本合计" |
| #5 | 丢 `400 idempotency_key_required` 码与"与数据面 1–128 不同"的对照 |
| #2 | 丢 1.1.0 与 HA 提案文档指针 |

### 1.10 边角与不一致

- **systemd 未同步 R-30**：默认 `shutdown_timeout` 从 30s 提到 2m，k8s 清单同步到
  `terminationGracePeriodSeconds: 150`，但 `deploy/systemd/halro-aws-kms.service:14` 仍是
  `TimeoutStopSec=60s`（本轮未改）。systemd 部署下 60s 就 SIGKILL，后 60s 的优雅预算不可达，
  而 `halro_shutdown_truncated_attempts_total` 的持久化写入恰好只发生在预算耗尽之后
  （`runtime.go:1100-1106`）——在 systemd 上这个新指标永远写不出来。
- **catalog 示例 5 天后会重新失败**：`catalog/unsigned-snapshot-v1.example.json` 的
  `expires_at: 2026-08-17T00:00:00Z`，`internal/modelcatalog/snapshot.go:241-242` 强制过期检查。
  8-17 之后照 runbook 模板做一遍会重新在 `verify` 处失败（换成 expiry 错误），且**无任何 CI 门禁盯这个文件**。
- **`scripts/archive-release-run.sh` 模式是 0644**，而 `release-run-evidence.md:66` 与
  `releasing.md:64-66` 写的是 `./scripts/archive-release-run.sh …`——按原文执行会 permission denied
  （同目录 `scripts/backup.sh` 与 `tools/m11/*.sh` 都是 0755）。该脚本也没有任何 CI 覆盖。
- **R-26 的契约文件本身出现新漂移**：`docs/observability/alerting-rules.md:14-27` 的 "Core groups"
  清单未更新，新增的 configuration-stale / audit / capability 三类未列入。
- **R-04 控制台形态**：`web/src/pages/SettingsPage.tsx:132` 的 `filter((d) => d.stale)` 让健康态下
  四个域的行整体消失，而服务端 `activation_state.go:117-125` 是**无条件**输出全部四个域的——数据在
  手里却被丢掉，违反"空面板保留行 + `—`"的既定约定。且控制台没有任何指向新建
  `docs/runbooks/configuration-stale.md` 的入口，D1-02 问的三件事答了两件。
- **两条新 runbook 未加入 `docs/runbooks/embed.go`**，不随二进制分发、也不经 Admin API 提供
  （现有三份事故 runbook 都是嵌入的）。
- **`ci.yml` 的 19 处 `uses:` 仍是可变 tag**（`release.yml` 是 31/31 全 pin）；仓库策略实测
  `sha_pinning_required: false`。R-32 原文是二选一，取前者已满足，但 CI 面仍有这个敞口。
- **R-25 的两处小缺**：anchor 告警的"从未发出过"分支（`last_emit == 0`）在 rule-test 里无用例；
  该告警无 `for:`，边界抖动会瞬发。
- **R-23 的残留盲区**：`writeLatencyHistogram()` 走变量名（`metrics.go:112-117,472`），静态清单
  正则匹配不到。今天这两族无条件导出所以渲染期门禁兜住了，若将来放进条件分支仍会绕过。
- **R-27 零余量**：预算 150 与下限 150 恰好相等，`group_interval` 上调 1s 门禁即红
  （这是设计意图，但意味着改配置必须同步改预算）。`validate.sh:39` 仍钉着原来那个错旋钮
  `repeat_interval`，属冗余。

---

## 2. 治理闭环：G7 与 §9.2

### 2.1 G7 的 17 条 —— **仍未通过**

**6 条 🔴（必须用户拍板）无一有裁决**，且与 `progress.md:120-122` 记的"释放负责人接受 R-16/R-30/R-33/R-43/R-44"
**不是同一批**——那五条全部来自报告 §6.3 的 P2 清单，与 G7 的 17 条无一重合。

| # | 条目 | 状态 |
|---|---|---|
| 2 🔴 | `syncUsageAdmin` 的 `WithoutCancel` + `applyMu` 不感知 ctx | **仍空白**。`admin_usage.go:553-559` 仍是 `request.Context()`；`internal/usage/collector.go:30,55,72` 仍是普通 Mutex；两处 diff 为空 |
| 3 🔴 | 两个 PUT 的 step-up | **仍空白**。`requireDestructiveStepUp` 仍只挂在 DELETE 上，且无"接受此取舍"的记录 |
| 5 🔴 | 溢出预算随 `maxTracked` 缩放 | **仍空白**。连报告点名要求的 `internal/sourcelimit/limiter.go:112` 那行取舍注释都没加 |
| 12 🔴 | rc.1 publish 根因 | 有技术方向（`release-run-evidence.md` + governance 预检），根因本体仍不可验证（run 已 404） |
| 13 🔴 | 能力选择 §15 三条门禁 | 浏览器验收的记录**评审前就存在**、非本轮新增裁决；真实 Provider 证据仍空白；`provider_metadata` 见下方勘误 |
| 16 🔴 | `security-review-v1.md` blocked | 有书面裁决（四道门重新定义，取代无定义的 "M10 recovery"，并明写 "none is considered closed by editing this document"），门仍未关 |

已闭合的：#1 argon2（修复 + 已知限制 19）、#9/#10/#11（报告本轮已出列）、#15 探测不进 Ledger
（已知限制 10 + 控制台文案，但**附条件"不得以此为先例让数据面绕过 Ledger"未见落书面**）、
#17 已知限制进发布说明。半条：#4 锚点 HMAC——告警补了（`alert-rules.yml:19`），但"锚点文件本身
未做完整性链保护"未进已知限制（20 条里 `grep -i anchor` 零命中）。仍空白：#6、#7、#8。

**另有 4 条建议"接受"的（#5/#6/#7/#8）连"理由记录在案"这一步都没做。**
未见任何"评审组代做"的越权裁决——缺口是空白，不是僭越。

### 2.2 §9.2 的 10 条历史结论修正

| # | 是否执行 |
|---|---|
| 1 F-03 状态改为"已修但语义未闭合" | **代码已闭合，台账未更新**——`carry-forward.md:88` 仍写"已关闭（有守护）"，该文件工作区零改动 |
| 2 260807 边界表已过期、不得再引用 | **未执行**——`docs/review/260805/progress.md:95-116` 的边界表原样保留，无过期标注 |
| 3 `crash-recovery-matrix.md:21` | **已执行且超出要求**——换成实存的 `TestMetadataNewerSchemaIsRejectedWithoutMutation`（`store_test.go:736`），同表另两行"未指名测试"的也一并具名化，四个新引用的测试名逐条 grep 确认存在 |
| 4 rc.1 证据链改为 run id + artifact 摘要 | **机制侧已执行**（`release-run-evidence.md` + `tools/release/run_evidence.py` + 签名 manifest + 90 天 artifact），实际 run 仍不存在 |
| 5 `reset_` 是两条不是三条 | **已执行**——CHANGELOG 逐字写 "These are two reset migrations, not three." |
| 6 B1 的四处统一改补丁不应采纳 | **已遵守**——R-08 只改 operator-guide；Dockerfile 唯一改动是 `SOURCE_DATE_EPOCH`，路径未动；systemd/k8s 路径未动 |
| 7 B3 两条无效论证作废 | **部分**——报告 §3.4 已记（冻结件），但 `progress.md` 的 errata 段只列 V2/V3/V6，**未记 B3 的两条**，而报告要求"记录在本报告**与** progress.md" |
| 8 A3-01 撤销 + **正面结论保护** | **已执行，天花板保证完好**——`gemini/adapter.go:25-30` 与 `bedrock/adapter.go:24-31` 的 `Options` **仍无 `Capabilities` 字段**，`git diff --stat -- internal/provider/` **为空** |
| 9 B1-09 核心主张撤销 | **已执行** |
| 10 A5 加边界说明 / 归口方法论教训 | **未执行**——工作区无任何记录 |

**一条新勘误（不在 §9.2 里）**：报告 §9.3 #13 与 `carry-forward.md:37` 称 `provider_metadata`
"代码里只有枚举值与校验、无任何 Adapter 发射它"——**这是事实错误**。在评审 HEAD `2cd24a7` 上，
`gemini/adapter.go:251`、`bedrock/models.go:153`、`anthropic/adapter.go:192` 都在发射
`domain.ClaimSourceProviderMetadata`，且有具名测试。因此 #13 的第三项建议基于错误前提、
不需要裁决——但这一点也没有被任何文件写下来。

---

## 3. G1~G9 现状

| 判据 | 报告原判 | 现在 | 依据 |
|---|---|---|---|
| G1 | 通过（窄读）/不通过（宽读） | **窄读通过；宽读 P0 数 2 → 1** | B3-03 已修（可复现构建，见 §1.6 的容器例外）；B3-01 仍存续 |
| G2 | **不通过**（fail-closed 有反例） | **通过** | stale 按域建模（`activation_state.go:36-60,100`）+ 四域重放（`:200-214`）+ 缺陷态可失败的负面测试。残余两点：lookup-miss 方向无显式书面结论（§1.2）；调用点侧负面测试只做了 2/3（§1.8） |
| G3 | 通过（但 §3.3 至今是空占位符） | **可复核部分全绿** | 实跑 `go vet ./...` exit 0、`gofmt -l` 空、`go build ./...` exit 0、`tsc --noEmit` exit 0、`validate.sh` 全绿、`go test ./internal/app/ -count=1` ok 156s 无回归。**注意报告 §3.3 的 `MAKE_CHECK_PLACEHOLDER` 从未填入**——原判"G3 通过"没有可回查的一手记录 |
| G4 | **不通过** | **仍不通过** | 今日只读查询：`releases` = 0、`environments` = 0、`actions/secrets` = 0、release workflow runs = 0。仓库侧控制已到位，但 G4 问"是否已产出过"，答案仍是可观测的否 |
| G5 | 通过 | **形式维持，证据已陈旧** | 本轮改了 `internal/backup/archive.go` 与 `internal/app/backup.go`（R-34/35/36），C1 的实跑证据产生于旧代码，需重跑一次才算新鲜 |
| G6 | **不通过（严格口径）** | **通过（严格口径）** | 三条 m11 runbook 顶部均有 release-blocked 标注并链回发布说明；catalog 示例 `profile_id` 已改为注册值且**实跑 verify 通过**；`gateway-key-compromise.md:103` 已补 `--config`。两条新 runbook + 链接断言进 CI |
| G7 | 未通过 | **仍未通过** | 见 §2.1 |
| G8 | 未完成 | **通过** | `release-notes:193-265` 共 20 条，⚠ 的三条按要求改写而非固化（措辞强度问题见 §1.9） |
| G9 | 通过 | **维持通过** | `release_24h` 仍未归档，报告已明确该项在 G9 外 |

**报告 §1.1 的三条否决理由现在只剩一条（G4），另加 G7 未闭合。** 性质变了：剩下的两条都不再是
"代码里有东西没修"，一条是纯 GitHub 侧操作（建 Environment、设 environment secret、走一次 rc.2
四方审批），一条是用户在 6 个产品取舍上签字。

**今天打一个 tag，publish 依然必然失败**——但断点前移了：不再是 `release.yml:348` 的
`test -n "${M11_RELEASE_EVIDENCE_JSON}"`，而是 `release-governance` job 在 `:324` 的
`gh api .../environments/v1-release` 上拿 404 直接失败，`publish` 因 `needs` 未满足而 skip。
这在两个方向上都是改进：失败信息从"某个 secret 是空的"变成"受保护的发布环境不存在"；
且因为 publish 从不被调度，GitHub 也就不会替你自动创建一个**不带保护规则**的 `v1-release`
（rc.1 那次正是 publish 跑起来后被自动创建的，deployment 记录 `5785970777` 可证）。

---

## 4. 关键实跑证据

### 4.1 缺陷态反向注入 14 次（`go build -overlay`，仓库零改动）

| 注入 | 结果 |
|---|---|
| `markCurrent` 清空全部域 | T-1 FAIL、T-4 FAIL ✅ |
| 恢复只重放 topology+auth | T-2 FAIL ✅ |
| Anthropic group 去掉 stale 中间件 | T-9/anthropic FAIL（400 而非 503）✅ |
| `Idempotency-Key` 改回可选 | T-5 三条子用例全 FAIL ✅ |
| 删探针阶段 `:427` ceiling | **T-8 仍 ok ← 缺口** |
| 删识别阶段 `:529` ceiling | T-8 FAIL ✅ |
| 迁移 25/26 都不再 DeleteBucket | T-7 FAIL（三桶各留 1 条）✅ |
| 启动排空失败不再拒启 / 整体移除 | T-6a、T-6b FAIL ✅ |
| `updateAdminRoute` 去掉 `adminTopologyMu` | T-10 FAIL ✅ |
| 去掉 binding 引用守卫 | T-11 FAIL ✅ |
| 漂移不再 withhold | T-9 别名测试 FAIL（502 而非 404）✅ |
| redaction 激活失败不标 stale | T-3/redaction FAIL ✅ |
| T-12 全程不取消 context | **仍 ok ← 装饰性守护** |

### 4.2 可复现构建（两个独立 `ubuntu:24.04` 容器，刻意制造"两台机器"差异）

不同工作目录 / 不同文件 mtime / 不同 umask，用 `release.yml:167-169` 的逐字命令：

```
A.tar.gz  4c5de4f7fc2b888c32e4f6aae68684154be774cbe96bb101f40de0cfadf90a3b
B.tar.gz  4c5de4f7fc2b888c32e4f6aae68684154be774cbe96bb101f40de0cfadf90a3b   ← 相同
```

同输入用修复前的命令（`tar -C release -czf`）作对照 → `a96f42ee…` vs `4025619d…`，**不同**。
即修复是 load-bearing 的。Go 二进制连打两次 → `bbae969f…` 两次相同。
容器包两次 → `6c22017c…` vs `31551a99…`，**不同**（见 §1.6）。

### 4.3 其他实证

- **catalog 全链**（scratchpad 内一次性 ed25519 演练签名者）：`prepare` → 签名 → `assemble` → `verify` **exit 0**；
  把 `profile_id` 改回旧值 → `catalog profile "openai_chat_embeddings" is not registered`，exit 1
  —— 与 C1 原始发现逐字一致。附带发现：`prepare` 本身**不**校验 profile_id，拦截点在 `verify`。
- **R-21 反向条件**：本地自建镜像实跑，正例 `/health/ready` 1 秒内 200 且 `docker inspect` healthy；
  反例把 `data_dir` 改回挂载点 → `open publication lock: … permission denied` exit 1 → **CI 必红**。
- **rule-test 有效性**：把 `HalroConfigurationStale` 的 `for: 1m` 改成 `5m` → promtool 立刻
  `FAILED: … got:[]`，说明断言真的绑住了 1m 语义。
- **R-14 门禁**：隔离 fixture 里改 `go-chi` 版本 → exit 1；改 `web/package.json` → exit 1。**真的会红。**
- **R-23 门禁方向**：删掉 `halro_audit_anchor_interval_seconds` 的文档行 → 静态清单立刻报缺，
  对条件导出分支**不再是盲的**。
- **R-40**：`gateway_listen: 0.0.0.0:8080` + TLS 关闭时 `halro init` exit 0，而 `serve`/`start`
  仍拒绝——校验确实留在了 start/serve。
- **R-44 字阶门禁**：把判定逻辑提取成独立脚本喂合成 CSS——新选择器字面值、allowlist 内换数值、
  `font:` 简写、`@media` 内非首条声明**全部能红**。两处窄逃逸口：`@media` 块内首条嵌套规则的
  首条声明被外层正则吞掉（但低于 12px 的仍被地板测试全文件扫描抓到）；正则无 `i` 标志，
  `FONT-SIZE:` 大写逃逸（仓库无此写法）。
- **前端**：`npm run build` exit 0 后 8 个产物 SHA-256 与构建前**逐字节一致，dist 零漂移**；
  `design-system.test.ts` 14 passed；`components/SettingsPage/DeploymentsPage` 70 passed；
  密钥金丝雀扫描 8 files clean。
- **错误码逐字比对 5/5 一致**：`provider_` / `deployment_` / `route_` / `project_` /
  `gateway_key_idempotency_replay`，五个 deep-link 锚点目标真实存在；服务端为 Gateway Key
  新增了 `project_id` 字段（`admin_projects.go:262-267`）供前端拼链接——是前后端配套改动。
- **`make observability-check` 绿**：13 recording + **30 alert rules**（原 24 → +6）、
  promtool test rules SUCCESS、amtool SUCCESS、runbook-link go test PASS。
- **依赖 license**：直接 Go 依赖实测 **12**，文档写 12 且逐行版本与 `go.mod` 一致；
  前端 runtime 依赖 11 与 `package.json` 一致；lockfile 实测 CC-BY **0** 条、MPL-2.0 12 条，与文档一致。
- **`NOTICE`**：新增两段与 module cache 中 `aws-sdk-go-v2@v1.43.3/NOTICE.txt`、
  `smithy-go@v1.27.6/NOTICE` **逐字一致**；对着 `go version -m` 的 29 个已链接模块逐一比对，无遗漏。

---

## 5. 仓库洁净性

**干净。** `git status --porcelain --untracked-files=all` 共 111 项，未跟踪 16 个，逐个核对
全部是本次整改的正当产物。**没有** `data/`、`master.key`、`.env`、`.hmbk` 备份、Provider 证据、
`zzz_*_bench_test.go`、`.patch/.orig/.rej/.log`。`tools/release/__pycache__` 被 `.gitignore` 覆盖。
`internal/webui/dist` 的删除/新增与 `index.html` 的引用自洽。

两点提示：

- **`progress.md` 本身是未跟踪的**——若提交时漏掉它，整份整改台账就没进版本库。
- `.claude/worktrees/fix+stale-price-blocker`（在 `d1d901a`）是个遗留工作树，
  只靠 `.git/info/exclude` 排除（**不是** `.gitignore`）。

---

## 6. 无法验证的项（不用推测填空）

| 项 | 为什么 |
|---|---|
| R-24 回归的浏览器端实证 | 只读护栏下不能起实例配合抓包。结论由四段可核对的代码事实 + 测试助手被改这一直接证据推出 |
| R-09 / R-18 的实跑验收（"从宿主 curl 得到 200"、"空卷首启按文档能走通"） | 未构建镜像逐字跑指南。结论来自读码 + 同工作区 CI 脚本必须做而文档没做的两处偏差（`ci.yml:176,178`）作对照 |
| R-13 的判据本身（64 并发 RSS 不再线性） | 需起实例、bootstrap 管理员、绕过 `login_rpm` 并采样 RSS。信号量的结构性上界已按代码确认 |
| `data_lock` 的 EWOULDBLOCK 分支端到端 | 需另起一个持锁进程并绑定端口；该分支只有单测覆盖 |
| `runActivationRecovery` 的 5s ticker 真实行为 | 只验证了它调用的两个函数体，未跑真实循环 |
| stale 的端到端注入（真二进制 → 真 Prometheus → 真 Alertmanager 1m 后 firing） | 需初始化数据目录与 Admin 会话并驱动一次真实失败的 mutation。替代证据是三段独立实证的拼接，中间"Prometheus 真的抓到并把 `for` 走完"是推断 |
| `v1-release` Environment 的保护规则内容、preflight 能否阻止自动创建 | 环境不存在；`verify_environment.py` 只在合成 fixture 上验证过，未见真实 GitHub 响应体 |
| `gh attestation verify` / environments API 在 job 授予的 permissions 下是否可用 | 无法在本地复现 GITHUB_TOKEN 作用域 |
| `scripts/archive-release-run.sh` 端到端 | release workflow 历史 run 数为 0，任何 run id 都 404；只做了 `sh -n` 与逐行阅读 |
| cosign 端到端验签 | 无法在本地产生 Fulcio 证书；命令块可执行性是逐字比对 + shell 语义分析 |
| 四个二进制归档在真实"两台机器"上的一致性 | 已证的是打包步骤在不同路径/mtime/umask 下确定、Go 构建在同机确定；跨 runner 镜像版本未验证 |
| G5 的备份/恢复实跑是否仍成立 | `archive.go` / `backup.go` 本轮有改动，C1 的证据产生于旧代码 |
| `go test ./...` 与 `-race` 全量 | 各角色只跑了各自子集（`internal/app` 整包已跑，ok 156s 无回归）；全量与 race 留给推送前的完整门禁 |
| `git diff --exit-code -- internal/webui/dist` 这条 CI 门禁 | 整棵树未提交，提交前给不出有意义的结果；只核对了 `index.html` 引用与实存文件自洽 |
| 真实浏览器下的视觉与对比度 | 未启动实例；AA 合规仅由 `design-system.test.ts:96-136` 的计算断言间接覆盖 |
| SDK 兼容矩阵 workflow | 需拉取 npm/pip/go SDK，本轮只做静态核对 |
| rc.1 publish 具体断在哪一步 | run `31131173718` 已 404（与 V5 结论一致，非本轮新增的不确定性） |

---

## 7. 修复收尾（2026-08-12）

§1 的每一条都已处理。下表是处置与证据；仓库侧没有留下"待办"，剩下的只有
GitHub 侧动作与需要用户拍板的产品取舍。

### 7.1 §1 的八条

| # | 项 | 处置 | 证据 |
|---|---|---|---|
| 1 | **R-24 回归** | ✅ **已修**。两条花费凭据的调用改为 POST 走 `requireAdminMutation`（角色 + CSRF + Origin，浏览器在 POST 上确实会发 Origin）；纯缓存读保持 GET。`requireCredentialedAdminRead` 整个删除，不与替代品并存。测试助手那行 `Origin` 已撤销，并新增 `TestConsoleReadsSucceedWithNeitherOriginNorReferer`——请求断言自己既无 Origin 也无 Referer，正是浏览器的形状 | `internal/app/admin_invocation_targets.go:77-95,126-131`、`internal/app/runtime.go:1411-1417`、`internal/app/admin_ui_settings_test.go:208-214`、`internal/app/admin_invocation_targets_test.go`；前端 `web/src/api.ts:300-320`、`web/src/pages/DeploymentsPage.tsx:777` |
| 2 | **R-03 第三条判据** | ✅ **已修，且不是只写一句裁决**。命名策略在快照中缺失时两处都改为 fail-closed（空 ID 仍是"该项目没有策略"这个决定，保持放行）；Gateway 在授权阶段一次性拒绝这类 Project，返回 503 `configuration_stale`，错误信息说清是哪一类策略没装载。两条负面测试直接钉住 V1 探针当时看到的现象 | `internal/gateway/service.go:170-199`、`internal/redaction/engine.go:45-48,428-440`、`internal/tokenguard/manager.go:24-26,309-322`；测试 `TestAMissingPolicyRefusesInsteadOfPassingTheTextThrough`、`TestAMissingPolicyRefusesInsteadOfAdmittingUnconditionally` |
| 3 | **R-30 监控静默通道** | ✅ **已修**。改成与同文件 capability gauge 一致的写法：读失败只省略该族并继续渲染，其余族照常导出，200 之后不再中途中止。附带在 store 侧补了"损坏计数器必须报错而不是返回 0"的测试——省略只有在读确实报告失败时才是正确答案 | `internal/app/metrics.go:60-76,183-187`；`internal/store/bolt/operational_counters_test.go` 的 `TestShutdownTruncatedAttemptsReportsACorruptCounterRatherThanZero` |
| 4 | **pre-1.0.0 违规（i18n 死码）** | ✅ **已修**。`provider_blocking_model` / `model_blocking_access` 四行从两份 locale 删除；`DashboardPage.test.tsx` 那条把死码钉住的夹具改成新码 | `web/src/i18n/locales/{en-US,zh-CN}.ts`、`web/src/pages/DashboardPage.test.tsx` |
| 5 | **R-10 级联码方向** | ✅ **已修**。上游未就绪时不再一律说"存在但未验证"，而是把上游自己的判定透传（`credential_missing` / `provider_missing` / `deployment_missing` …），只有真的"存在但没验证过"才用 `*_not_verified` | `internal/app/admin_onboarding.go:320-336,388-400` |
| 6 | **R-13 缺实测** | ✅ **已补实测**。64 并发登录峰值堆增长 **256 MiB（每并发 4.0 MiB）**，无上限时约 4,096 MiB。测量以 `HALRO_MEASURE_ARGON2=1` 开关隔离在常规套件之外，结构性上界仍由无条件测试守住 | `internal/adminauth/password_test.go` 的 `TestArgon2MemoryUnderAConcurrentLoginStorm`；数字归档在 `docs/verification/performance-baseline.md` |
| 7 | **R-17 未初始化目录** | ✅ **已修**。ENOENT 单列一支，明说"该目录尚未初始化"并给出 `halro init` 与父目录；新测试走真实 `DoctorWithOptions`，不再只测格式化函数 | `internal/app/doctor.go:272-292`；`TestDoctorReportsAnUninitializedDataDirAsSuch` |
| 8 | **R-04 控制台形态** | ✅ **已修**。四个激活域的行常在，健康时显示"与持久化存储一致"；stale 时面板给出恢复说明并点名 `docs/runbooks/configuration-stale.md`。新测试断言健康态下四行都在、且恢复说明不出现 | `web/src/pages/SettingsPage.tsx:131-170`；`SettingsPage.test.tsx` 的"keeps every activation domain on screen when nothing is stale" |

### 7.2 测试面四条（§1.8）

| 项 | 处置 |
|---|---|
| T-3 缺告警侧 | ✅ 补 `TestFailedAlertActivationDoesNotRefuseTraffic`——刻意的 fail-open 方向现在有了守护，后续把它"顺手改成 fail-closed"会直接红 |
| T-8 半闭 | ✅ 补 `TestProbesBeyondTheCallCeilingAreLeftUnprobedRatherThanUnsupported`：单候选让识别阶段不花钱，预算全部由第一个探针吃掉，断言其余能力回 `not_probed` 而非 `unsupported`。**反向验证**：用 overlay 删掉 `admin_model_capability_detections.go:427` 的上限判断，该测试 FAIL |
| T-12 装饰性 | ✅ 改名为 `TestDisablingAProviderOrDeploymentRemovesItsRoutingCandidates`，并写清"断连不可取消激活"是编译期性质（函数不收 context），不是这条测试能失败的东西 |
| A4-11 名不副实 | ✅ 补真正并发的断言：8 个 goroutine 同时对同一 target 创建，恰好 1 条新建、7 条 replay，且全部落在同一个 ID 上 |

### 7.3 文档与边角

| 项 | 处置 |
|---|---|
| R-09 | ✅ 容器小节写明 `server.gateway_listen` 必须改成 `0.0.0.0:8080`，并解释为什么它和 `-allow-insecure-public-listen` 必须成对出现 |
| R-18 | ✅ `internal/vault/masterkey.go` 的两条 `init` 失败都带上路径与下一步（EEXIST 说清两种读法并警告不要删；EACCES 点明是容器 `--user` 的 uid 而非宿主用户）；指南改用 `chown 65532:65532` + 0700，删掉冗余的 `chmod 600` |
| R-02 | ✅ README 与发布说明都加 `gh release download`、`--ignore-missing`、缺 bundle 时跳过，以及 cosign ≥ v2.2 与 `gh` 需已登录的前提；`operator-guide.md` 的"要验签"一句改为指向完整命令块 |
| R-07 | ✅ 被写弱的 6 条补回：#11 补上 V6 救回的"真正未连上的失败结算为 0 并全额释放"、#9 补七个具名字段与 manifest 路径、#10 补上界与"上游账单会略高"、#7 补 `400 unsupported_feature` 与不预留额度、#5 补 `400 idempotency_key_required` 与 1–128 对照、#2 补 1.1.0 与 HA 提案指针；#19 补齐三个容量数字并指向基线文档 |
| §8-19 容量数字无落点 | ✅ `docs/verification/performance-baseline.md` 新增"单机容量，2026-08-11"一节，四项数字连同"形状可迁移、绝对值不可迁移"的口径一起归档 |
| carry-forward F-03 | ✅ 该行标注修订：R-03 之后重新闭合，并写明中间那次改判的理由 |
| progress.md errata | ✅ 补 §9.2 第 7 条（B3 两条无效论证作废、B3-03 移出 G4）与第 10 条（归口约定会让独立性失效的方法论教训） |
| §9.2 第 2 条 | ✅ `docs/review/260805/progress.md` 的 08-07 边界表顶部加"已过期，不要再引用"标注，并写明现在的量值 |
| systemd | ✅ `TimeoutStopSec` 60s → 150s，与 k8s 清单和 2 分钟的默认预算一致 |
| catalog 示例过期 | ✅ `expires_at` 推到 2030-01-01，并新增 `TestTheAuthoringExampleIsNotAboutToExpire`：距过期不足 90 天即失败，让它在仓库里先红而不是在 runbook 里红 |
| 两条新 runbook 未嵌入 | ✅ `configuration-stale.md` 与 `file-master-key-rotation.md` 进 `docs/runbooks/embed.go` 并各有 Admin 路由；后者在 key_slots 模式下 404（它讲的是 file 模式） |
| `archive-release-run.sh` 不可执行 | ✅ `chmod +x`（`check-dependency-license-review.sh` 一并处理） |
| `alerting-rules.md` Core groups | ✅ 补入 configuration activation / audit anchoring / capability evidence 三类 |
| R-25 两处小缺 | ✅ 锚点告警加 `for: 2m`（并相应调整 rule-test 的时间窗），补一组"从未发出过锚点"的 firing 断言 |
| R-23 残留盲区 | ✅ 静态清单正则扩展到 `writeLatencyHistogram`，两个直方图族纳入门禁（86 → 88 族） |
| R-32 敞口 | ✅ `ci.yml` 的 19 处 `uses:` 全部 SHA-pin，并在 `check-production-assets.sh` 加门禁。**反向验证**：把任意一处改回 tag，该门禁 exit 1 |
| R-03p 容器不可复现 | ⚠️ **按范围声明处理，不做未经验证的流水线改动**。`docs/guides/releasing.md` 新增"Reproducibility scope"：四个二进制归档逐字节可复现（已实测），两个容器包不可复现，写明两条根因（BuildKit 未 `rewrite-timestamp`、主 Dockerfile 基础镜像未 digest-pin）与实测数字。修复需要在真实 RC 上验证一次，按用户"Docker 验证可暂缓"的指示留待那次 |

### 7.4 复跑门禁（2026-08-12）

| 门禁 | 结果 |
|---|---|
| `go test ./...` | 全绿 |
| `go test -race -count=1`（本次改动涉及的 7 个包） | 全绿，`internal/app` 318s |
| `go vet ./...` / `gofmt -l ./cmd ./internal ./tools` | 干净 |
| 前端 typecheck / 32 文件 297 测试 / `npm run build` | 全绿；密钥金丝雀扫描 8 files clean |
| `internal/webui/dist` | 已随 `web/src` 改动重建 |
| `deploy/observability/validate.sh` | 13 recording + 30 alert rules、promtool rule tests、amtool 全绿 |
| `tools/m11/check-production-assets.sh` | 绿（含新增的 Action pinning 门禁） |

### 7.5 仓库侧无法关闭的（不是遗留，是边界）

| 项 | 为什么 |
|---|---|
| G4 / R-01 / R-04p | 建 `v1-release` Environment + required reviewers + Prevent self-review、把 secret 装成 environment secret、跑通一次 rc.2 publish 并保留该 run。仓库侧控制与校验器都已就位，剩下的全在 GitHub 设置里 |
| G7 的 6 条 🔴 | 产品取舍，必须由用户拍板：`syncUsageAdmin` 的 ctx 语义、两个 PUT 的 step-up、溢出预算随 `maxTracked` 缩放、rc.1 根因、能力选择 §15 三条门禁、`security-review-v1.md` 的四道门 |
| R-03p 的容器可复现 | 需要在真实 RC 上验证一次流水线改动（用户指示 Docker 验证暂缓） |
| S1 真实 Provider 证据、24h soak | 计费/长跑，需要单独授权 |
