# SEC-03 / SEC-04 / C2 独立证伪裁决

日期：2026-09-05。基线：`381743f6613607dc256828f4776b52af8bdd232c`（本轮重新核对）。非原作者复核；已授权读取 security 报告及原始临时 overlay。遵循 AGENTS.md 与批准计划。本轮唯一仓库写入为本报告；只在 `/private/tmp/halro-security-secondary-260905/` 创建合成测试及日志，未修改生产源码、仓库测试、git状态或业务数据，未执行全量测试、付费或真实外部调用。

## 裁决摘要

| 项目 | 独立裁决 | 严重度/置信度 | 边界 |
| --- | --- | --- | --- |
| SEC-03 管理MFA因子失败绕过失败预算/审计 | **CONFIRMED** | **P2 / 高** | 四个管理路径真实Admin router复现；密码失败控制有效。不是因子校验本身失效，也不是匿名MFA绕过 |
| SEC-04 capture读取未检查TTL | **CONFIRMED** | **P2 / 高** | 补充实际认证HTTP：一小时TTL在恰好一小时、两小时仍200；Purge后404。物理清扫最大延迟未测 |
| C2 read_only可读capture正文事实 | **CONFIRMED** | 不另赋漏洞等级 | 当前角色语义和实际HTTP均允许；审计先行、审计失败503成立 |
| C2据此认定越权漏洞 | **REFUTED（按当前明确契约）** | 无漏洞等级 | 若产品希望read_only仅看元数据，属于权限契约调整；该需求尚不能作为现有漏洞前提 |

SEC-03/04保持P2：有效认证、密码/第二因子及作用域防御仍存在；未证明匿名访问、实际账户接管、无限猜测速率或全局数据泄漏。C2产品决策仍开放，不因独立复核而替产品决定更细权限。

## SEC-03：完整guard链与反证

### 入口和现有防御

`internal/app/runtime.go:1579-1585` 将管理MFA路由注册到 self/setup-self mutation wrappers。`internal/app/admin_session.go:272-297,325-349` 读取并验证session、重读当前角色，检查同源和CSRF；普通self mutation还受required-MFA gate。此类操作只作用于session.Username；read_only管理自己的账户安全是刻意例外，不等于系统资源写权限。

`internal/app/admin_stepup.go:173-200` 的 `guardAdminCredentialCheck` 先查账户分钟失败预算，再调用callback；只有callback返回false才 `recordStepUpFailure` 和 `auditStepUp`。上限为每分钟5次失败（:21），第六次拒绝429；预算时钟可注入。成功密码不会增加失败计数。

| 管理操作 | 实际验证位置 | 漏记原因/防御 |
| --- | --- | --- |
| POST authenticators | `admin_mfa.go:114-116` callback仅VerifyPassword；`:149-153` 再verifyAnyTOTP | 已有active factor时要求TOTP，但错误因子在guard之外返回401；active最多5个限制不修复失败记账 |
| DELETE authenticators/{id} | `:463-465`仅密码；`:481-492`验证其他factor或最后factor | required策略禁止删最后一个；其余因子验证失败未回到guard。本轮用optional且一个active factor真实复现 |
| POST recovery-codes/regenerate | `:529-531`仅密码；`:539-542`另验TOTP | 无有效TOTP仍拒绝，不生成恢复码；失败预算/审计缺失 |
| DELETE security/mfa | `:570-574` required策略直接拒绝；`:589-591`仅密码；`:601-615`TOTP或recovery fallback | 错TOTP会尝试recovery hash，store找不到有效未使用码则ErrNotFound→401；这一失败也在guard之外 |

`verifyAnyTOTP`（`admin_mfa.go:361-375`）验证六位码和时间步，成功才原子调用 `AcceptAdminMFATimeStep`。不存在隐藏的失败计数或失败审计。`internal/store/bolt/store_admin.go:950-975` 的disable事务检查未使用recovery hash；无匹配直接ErrNotFound，未绕过factor，也没有补计失败预算。

**反证成立的范围：** 登录MFA不是同一漏洞。`admin_mfa.go:283` claim challenge，`:303-314`将TOTP/recovery验证本身完整放入guard；失败还标记challenge失败。普通step-up同样把完整reauthentication包在预算内。管理错密码会命中guard；required策略完全禁止disable；Argon2成本/并发限制、CSRF、session、single-use TOTP、恢复码熵仍构成有效防御。因此这里的“绕过”专指失败预算和失败审计，不是绕过因子认证。

### 独立运行

重跑原作者三个管理路径后，新增临时overlay扩展delete-authenticator，并给四条路径各增加错误密码控制。每条路径：正确密码＋错误六位因子连续7次均401，`admin.reauthentication` failure计数为0；随后错误密码5次401，第6次429。同一fixture将账户预算时钟固定，排除跨分钟重置解释；TOTP仍使用真实当前时间，不伪造生产校验时钟。全部为合成secret/session，未猜中正确码或取得新credential。

原始fixture是确定性真实router调用，不是网络洪泛；原本三个路径足以确认根因，delete控制补上其源码兄弟路径。新增wrong-password控制证明并非fixture整体未接预算机制。尚未测required策略的add/regenerate变体、多个其他authenticator、并发失败预算。Enrollment confirm（`:175-226`）检查pending状态、十分钟到期和新secret的TOTP，也未用此guard；其威胁条件不同（确认新因子），本裁决只记录源码边界，不追加同等级发现。

建议（未实施）：将每个动作完整的密码＋其特有factor/recovery条件纳入失败预算/审计，保留“删除须验其他factor”和recovery替代语义。不要简单替换成通用step-up而削弱现有规则。回归应检查错误因子、错误密码、并发、required策略、正确验证以及audit counts。

## SEC-04：TTL、HTTP可达性与清扫契约

`internal/failurecapture/failurecapture.go:232` 用store时钟写CapturedAt；文件名也携带该时间。`Get`（`:387-433`）列day、locate、读密文、以request/project解密、反序列化并返回，完全不读取当前时钟/retention比较。`locate`（`:484-502`）丢弃文件名解析出的timestamp。`ProjectOf`只定位，不补TTL校验。

`internal/app/failure_capture.go:87-148` 的handler先sync usage，再从capture定位project并Get；既不从调用方接收project授权选择，也不检查CapturedAt。解密/未找到返回404；成功读取先写审计，最后才返回正文。`syncUsageAdmin`并不触发capture Purge。

现有Purge（`failurecapture.go:438-481`）会删除 `capturedAt <= now-retain` 的文件；Open（`:175-202`）不主动purge。`app/runtime.go:961-975`在parquet维护后purge，`:990`优雅退出也purge；默认parquet interval一小时（`config/default.go:65`）。禁用capture仍打开旧目录清扫（`app/failure_capture.go:24-39`），是有效防永久遗留措施，但不是每次读取的期限门禁。清扫延迟、失败或硬重启窗口不能从这些措施推导为严格TTL。

**契约判断：** 批准计划INV-07明确包含过期约束，Purge及maintenance注释也将retain称为真实窗口，不是“随下次清扫前仍可读取”。即便物理删除允许延迟，访问时拒绝过期与物理回收是不同义务。因此接受原P2，不能用“最终会Purge”反驳超期读取。

### 独立HTTP证据

用实际Open runtime/test Admin session、实际Vault、实际failurecapture store和Admin router；仅store时钟注入，retention一小时。创建合成正文后：

1. 无cookie GET→401；错误project直接Get无法认证密文。
2. Administrator有效期内GET→200，返回正文。
3. 在真实metadata把同一账户角色改为read_only；后续请求重读角色。30分钟、恰好一小时、两小时均200并返回正文。
4. 审计中有4条对应 `usage.failure_payload.read`（administrator一次＋read_only三次）。
5. 关闭测试runtime的Audit后再读→503，正文未出现。
6. 运行实际Purge后再读→404。

这增强了原先store-only复现：TTL缺口在认证HTTP路径可达，既不是未挂载handler，也不是审计失败时偷偷返回。测试没有启动maintenance worker去测真实等待时长，而是在生产可达的“过期但尚未清扫”状态定点读取；它证明边界行为，不测最长窗口。没有操作真实文件权限或模拟物理删除失败。

建议（未实施）：读取/定位时按可信capture时间拒绝过期，独立于磁盘Purge成功与否；明确恰好TTL边界与未来时间/损坏时间策略。补并发purge、禁用capture、硬重启、精确到期和失败清扫HTTP用例。

## C2：角色语义与授权/审计链裁决

`internal/domain/admin.go:9-17` 明确两种角色：administrator拥有现有能力，read_only为所有GET、无逐端点例外。`admin_session.go:303-310`进一步说明只读约束系统数据/配置写入，自己的password/MFA/preferences是窄例外。不能把角色简单称“仅元数据”或“完全不能执行任何POST”。

Payload GET 在 `runtime.go:1600`注册为 `requireAdmin`，不是`requireAdministratorRole`。这个wrapper要求有效Admin session，并在required策略下要求有active MFA；它不按项目缩窄Admin权限。Gateway Key不提供Admin session。project来自capture内部定位，AEAD绑定(request,project)，用于完整性/防换绑，并非Admin按项目授权。

`failure_capture.go:137-161`在返回任何正文之前写 `usage.failure_payload.read`，包含actor、request target和project metadata；没有Admin context或audit失败都拒绝。独立read_only HTTP与503控制证实这些实际语义。读取审计所需的内部写入不是用户取得通用系统写权限。

**结论：** “read_only可读取未过期retained正文”是当前明确授权行为。若有人将此事实解释为越权漏洞，该解释按现契约被证伪；并不否认正文敏感性。产品仍可决定增加内容读取权限、改名或加强角色说明，这属于契约演进，需新的授权矩阵和产品验收。SEC-04超期读取则独立违反retention，即使读者是administrator也仍成立；不能把它混入C2争议后搁置。

本轮不依赖历史260903/260805结论作为当前运行证据；当前domain/wrapper/handler及本地HTTP足以作上述裁决。没有验证所有Admin GET或两账户隔离、浏览器角色切换、真实MFA登录全旅程，C2结论限此路由及明确角色规则。

## 命令、退出码与证据索引

从仓库根目录执行，Go测试均 `-count=1`，stdout/stderr直接重定向，退出码来自进程而非管道。默认sandbox成功，无本轮cache/socket环境失败，无需升级权限。两条命令使用隔离fixture，可并行；没有新全量门禁。

```sh
go test -count=1 -v -overlay /private/tmp/halro-security-review-260905/overlay.json ./internal/app -run '^(TestSecurityReviewWrongManagementTOTPBudget|TestSecurityReviewExpiredCaptureRead)$' > /private/tmp/halro-security-secondary-260905/original.log 2>&1

go test -count=1 -v -overlay /private/tmp/halro-security-secondary-260905/overlay.json ./internal/app -run '^TestSecondary' > /private/tmp/halro-security-secondary-260905/independent.log 2>&1
```

| 命令/用例 | 退出/结果 | 不变量映射 |
| --- | --- | --- |
| 原作者两个selector重跑 | 0；包3.730s | 管理三路7次wrong factor无预算/审计；两小时读一小时TTL对象仍成功 |
| TestSecondaryManagementFactorBudget | 0；用例5.08s | INV-04/08：四路因子失败均不记账；四路wrong-password控制5×401＋429 |
| TestSecondaryCaptureHTTPRoleTTL | 0；用例0.48s；两用例所在包6.440s | INV-07：到期/过期HTTP仍返回；INV-04/10：read_only按契约允许；审计关闭拒绝正文、scope/匿名拒绝 |
| git rev-parse HEAD | 0；上述基线 | 证据绑定源码身份 |

成功复现是缺陷证据，不是修复后回归通过。独立source：`/private/tmp/halro-security-secondary-260905/secondary_test.go`；overlay虚拟添加 `internal/app/security_secondary_test.go`，不替换生产文件。原MFA测试包含固定预算时钟；本轮增加的控制沿用同一机制。探索性文件搜索有一次不存在路径、一次shell glob未匹配，均属导航错误，不是测试/产品失败。

## 覆盖和交接

已读完整credential failure guard及计数/审计、管理四路径/confirm/登录factor调用、TOTP消费与disable store事务、self mutation/session角色middleware、路由注册、capture Open/Put时间/ProjectOf/Get/locate/Purge、维护清扫、payload审计先行及相关现有测试；检查并定向重跑原overlay。未做新业务、修复、全套安全/浏览器/长稳、真实provider/KMS/云调用。

请主持据此更新主文档：SEC-03、SEC-04现在均为独立CONFIRMED P2；SEC-04增加认证HTTP证据，SEC-03增加delete路径及wrong-password反证；C2保留产品权限契约决策，按当前语义的越权解释REFUTED。本轮不改coverage、blind-spots或原security报告，避免越过唯一文件写入授权。

证据SHA-256（临时文件如需归档请先保留）：

- `secondary_test.go`: `5483a892b8e62e831ed7878e522a27b8f785694038b55548a4e1c29149818d8b`
- `overlay.json`: `a9752f4cb650beba39d82dfcf1a4eea518f29c2724cff8c3501fdf29d1c9b308`
- `original.log`: `e338a7743780596a0c71eee6bf72b6f935013a6054ab53256b5885cc01ad42de`
- `independent.log`: `23487f9b6fcaad7dde8a4a1c12d1d3b9dbb6e6df0497886ac899ee0ee217e53e`
