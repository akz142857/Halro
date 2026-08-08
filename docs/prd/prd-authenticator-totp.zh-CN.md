# Halro 身份验证器（TOTP）二次验证开发 PRD

- 状态：Draft
- 目标版本：待排期
- 文档语言：中文
- 适用范围：Halro Admin Console 与离线 Admin CLI

## 1. 背景

Halro 当前使用本地 Admin 用户名与 Argon2id 密码完成身份验证，密码验证成功后直接创建服务端 Admin Session。现有实现已经具备登录限流、同源校验、Secure/HttpOnly/SameSite Cookie、CSRF、Session 绝对/空闲过期、密码变更后的 Session 轮换与全量失效，以及可信 Audit 链。

Halro 管理面能够操作 Provider Credential、Gateway Key、备份及策略等高价值资产。仅依靠密码无法充分抵御密码泄露、撞库和终端凭据被窃取，因此需要增加基于开放标准 TOTP 的二次验证。

本功能不绑定 Google 或 Microsoft 品牌。产品统一称为“身份验证器”，兼容 Microsoft Authenticator、Google Authenticator、1Password 及其他支持标准 TOTP 的应用。

## 2. 目标

1. 为每个 Admin 账号提供可选的 TOTP 二次验证能力。
2. 同一个 Admin 账号支持绑定多个相互独立的身份验证器。
3. 密码和任意一个有效身份验证器均验证成功后，才签发完整 Admin Session。
4. 提供恢复码和服务停止状态下的离线 MFA 重置，避免管理员永久锁死。
5. 保持 Halro 的本地优先、无外部身份服务依赖、敏感信息不落浏览器持久化的安全边界。
6. 所有 MFA 生命周期操作可审计，并支持单个验证器撤销与验证码防重放。

## 3. 非目标

第一版不包含：

- Microsoft Entra ID、Google Account、OAuth、OIDC 或 SAML 登录；
- Microsoft Authenticator 推送确认、数字匹配或无密码登录；
- 短信、邮件、语音验证码；
- WebAuthn、Passkey 或 FIDO2 安全密钥；
- “记住此设备 30 天”等长期跳过 MFA 的设备信任凭据；
- 管理员之间代替对方重置 MFA；
- 多管理员邀请、角色和权限模型改造。

WebAuthn/Passkey 可作为后续抗实时钓鱼能力，不影响本期 TOTP 数据模型。

## 4. 核心产品决策

### 4.1 使用标准 TOTP

- 二维码使用 `otpauth://totp/...` 标准 URI；
- 默认算法：SHA-1；
- 验证码位数：6 位；
- 时间周期：30 秒；
- 校验窗口：当前时间步及前后各一个时间步；
- Issuer：`Halro`，可在后续版本支持实例显示名称；
- Account label：Admin 用户名，展示形式为 `Halro:<username>`。

SHA-1 在此处是 TOTP 兼容参数，不用于密码哈希或数据完整性保护。

### 4.2 一个账号可绑定多个独立验证器

每次添加身份验证器时生成新的独立 Secret。禁止通过产品流程让多台设备共享同一个 Secret。

独立 Secret 方案允许：

- 将验证器命名为“主手机”“备用手机”等；
- 查看每个验证器的创建时间和最近使用时间；
- 单独撤销丢失或淘汰的设备；
- 对每个验证器分别执行时间步防重放；
- 在 Audit 中识别实际使用的验证器。

每个账号最多绑定 5 个有效 TOTP 验证器。登录时，任意一个有效验证器生成的验证码通过即可。

### 4.3 远程部署策略

第一版提供实例级配置：

```yaml
admin:
  mfa_policy: "optional" # optional|required
```

- `optional`：管理员可以自行启用或关闭 MFA；默认值，保持升级兼容。
- `required`：未完成 MFA 设置的管理员通过密码后只能进入受限的 MFA 设置流程，不能访问其他 Admin API。

仅 loopback/SSH 隧道部署可以保持 `optional`。Admin 通过反向代理、VPN 或公网域名开放时，运维文档应建议使用 `required`。

## 5. 用户流程

### 5.1 添加第一个身份验证器

1. 管理员已通过密码登录。
2. 进入“设置 → 安全 → 身份验证器”。
3. 点击“添加身份验证器”。
4. 输入当前密码进行重新认证。
5. 服务端生成待确认的独立 TOTP Secret，返回二维码内容和手工输入密钥。
6. 用户使用任意兼容 TOTP 的身份验证器扫码或手工录入。
7. 用户输入当前 6 位动态验证码，并为设备填写名称。
8. 服务端验证成功后，才将该验证器标记为有效。
9. 如果这是账号的第一个有效验证器，系统生成 10 个一次性恢复码。
10. 页面只展示一次恢复码，要求用户复制或下载，并明确提示恢复码不会再次完整显示。
11. 启用成功后使该账号其他 Admin Session 全部失效，当前 Session 轮换为新 Session。

未确认的 Secret 不得启用；待确认记录有效期为 10 分钟，超时或重新发起时必须安全清除。

### 5.2 添加后续身份验证器

添加第二个及后续验证器时，必须同时完成：

1. 当前密码重新认证；
2. 使用任意一个现有有效验证器完成 TOTP 验证；
3. 扫描新验证器的独立二维码并输入新验证器生成的验证码。

不得只依赖已有浏览器 Session 添加新验证器。

### 5.3 正常登录

1. 用户提交用户名和密码。
2. 密码错误时返回统一的无效凭据响应。
3. 密码正确且账号无需 MFA 时，按现有流程创建完整 Session。
4. 密码正确且需要 MFA 时，不创建完整 Session；签发短期、单用途的 pre-auth challenge。
5. 登录页切换到验证码输入界面，并允许用户切换为恢复码输入。
6. 任意有效身份验证器的 TOTP 验证成功后，消费 challenge 并创建完整 Admin Session 与 CSRF Token。
7. 更新成功匹配验证器的 `last_used_at` 和 `last_accepted_time_step`。

pre-auth challenge：

- 使用密码学安全随机值；
- 服务端只保存其哈希；
- 有效期 5 分钟；
- 仅能用于完成对应账号的 MFA；
- 成功、过期或尝试次数耗尽后立即失效；
- 不可调用普通 Admin API；
- 不写入 Local Storage、Session Storage、IndexedDB 或 URL；
- 前端仅保存在内存中。

### 5.4 使用恢复码登录

1. 在 MFA 页面选择“使用恢复码”。
2. 输入一枚未使用的恢复码。
3. 服务端验证并以事务方式将该恢复码标记为已使用。
4. 创建完整 Admin Session。
5. UI 明确提示剩余恢复码数量，并建议检查或重新生成恢复码。

恢复码使用后不可恢复，也不能并发重复使用。

### 5.5 重命名验证器

- 仅修改显示名称；
- 要求当前有效 Session、CSRF 和 `If-Match` revision；
- 不要求再次输入 TOTP；
- 写入 Audit；
- 名称去除首尾空白，长度限制为 1～64 个 Unicode 字符。

### 5.6 撤销一个验证器

- 必须输入当前密码并使用另一个有效验证器验证；
- 不允许使用即将被撤销的验证器批准自己的撤销；
- 撤销后使账号所有既有 Admin Session 失效，当前浏览器完成重新登录；
- 写入 Audit，Audit 仅包含验证器 ID，不包含 Secret、二维码或验证码。

如果账号只有一个有效验证器，该操作属于“关闭二次验证”，遵循下一节流程。

### 5.7 关闭二次验证

仅当实例 `mfa_policy=optional` 时允许：

1. 输入当前密码；
2. 使用当前任意有效 TOTP 或一枚恢复码完成验证；
3. 显式确认将删除全部验证器和恢复码；
4. 服务端删除全部 TOTP Secret 与恢复码记录；
5. 使账号所有 Session 与未完成 challenge 失效；
6. 写入 Audit。

`mfa_policy=required` 时，UI 和 API 均拒绝关闭最后一个验证器。

### 5.8 重新生成恢复码

- 要求当前密码与任意有效 TOTP；
- 原有未使用恢复码全部立即失效；
- 新生成 10 个恢复码并只展示一次；
- 轮换当前 Session、使其他 Session 失效；
- 写入 Audit。

### 5.9 丢失全部验证器

运维人员停止 Halro 服务后，通过标准输入执行：

```bash
./halro admin reset-mfa --config ./config.yaml --username admin
```

命令行为：

- 必须遵循现有离线 Admin 操作的数据目录锁定与安全打开规则；
- 清除目标账号的全部验证器、待确认 Secret、恢复码和 pre-auth challenge；
- 增加 Session generation 并删除目标账号所有 Session；
- 向可信 Audit 链写入 `admin.mfa.reset_offline`；
- 不在命令输出中显示 Secret 或恢复码；
- 当实例策略为 `required` 时，管理员下次密码登录后只能进入重新绑定流程。

## 6. 页面与交互需求

### 6.1 登录页

新增三个状态：

- 用户名与密码；
- 身份验证器验证码；
- 恢复码。

要求：

- 支持粘贴包含空格的验证码，提交前只移除允许的格式分隔符；
- 6 位码输入完成后不自动提交，避免误操作和辅助技术问题；
- 支持返回密码步骤，但返回时必须废弃原 challenge；
- 页面刷新后不恢复 challenge，用户需重新输入密码；
- 不显示账号是否存在、是否启用 MFA、匹配了哪个验证器；
- 错误文案使用统一的“用户名、密码或验证码无效”；
- 验证器步骤可提示“验证码通常每 30 秒更新一次，请确认服务器和手机时间正确”；
- 保持现有中英文语义键完整对等。

### 6.2 安全设置页

展示：

- MFA 当前状态；
- 实例 MFA 策略；
- 验证器列表：名称、创建时间、最近使用时间；
- 添加、重命名、撤销操作；
- 剩余恢复码数量，不展示恢复码内容；
- 重新生成恢复码；
- 在策略允许时关闭 MFA。

二维码与手工 Secret 只在待确认步骤显示，离开页面后不可重新读取；用户必须废弃本次设置并重新开始。

### 6.3 恢复码展示

- 每个恢复码采用易于离线抄写的分组格式；
- 提供“复制全部”和下载纯文本文件；
- 文件不得包含用户名之外的其他实例敏感数据；
- 页面设置 `Cache-Control: no-store`；
- 不通过浏览器持久化、遥测、错误上报或日志保存；
- 用户明确确认“我已保存恢复码”后才能离开完成页。

## 7. 数据模型

不将单个 TOTP Secret 直接添加到 `AdminUser`。新增独立记录：

### 7.1 AdminMFAAuthenticator

```text
ID                    string
Username              string
Name                  string
Type                  "totp"
SecretCiphertext      bytes
SecretKeyVersion      uint32
Status                "pending" | "active" | "revoked"
CreatedAt             timestamp
ConfirmedAt           timestamp?
LastUsedAt            timestamp?
LastAcceptedTimeStep  int64?
ExpiresAt             timestamp?  # pending only
Revision              uint64
```

要求：

- Secret 使用现有 Master Key/Vault 的版本化加密能力保存；
- Secret 不得被哈希替代，因为 TOTP 验证需要恢复原值；
- Master Key rotation 必须覆盖 TOTP Secret；
- Secret 绝不进入普通 Admin API 响应；只有 pending enrollment 的一次性响应可返回二维码 URI 和手工密钥；
- revoked 记录可保留非敏感元数据用于审计，但必须清除 Secret 密文。

### 7.2 AdminMFARecoveryCode

```text
ID          string
Username    string
CodeHash    bytes
CreatedAt   timestamp
UsedAt      timestamp?
Generation  uint64
```

- 生成 10 个、每个至少 128 bit 随机熵；
- 仅保存带服务端域分离的密码学哈希，不保存明文；
- 验证必须使用常量时间比较；
- 消费操作必须原子化；
- 重新生成时增加 generation，使旧码整体失效。

### 7.3 AdminMFAChallenge

```text
IDHash            bytes
Username          string
Purpose           "login" | "enrollment" | "reauth"
CreatedAt         timestamp
ExpiresAt         timestamp
AttemptsRemaining uint8
PasswordGeneration uint64
```

- challenge token 本身只返回客户端一次，服务端只保存哈希；
- 与账号当前密码/Session generation 绑定；
- 密码变更、离线密码重置、MFA 重置和 Master Key rotation 后全部失效；
- 后台定期清理过期 challenge，验证路径同时 fail closed。

## 8. API 草案

路径沿用 `/admin/api/v1`：

### 8.1 登录

```text
POST /admin/api/v1/session/login
POST /admin/api/v1/session/mfa/totp
POST /admin/api/v1/session/mfa/recovery-code
DELETE /admin/api/v1/session/mfa/challenge
```

`session/login` 在需要 MFA 时返回 `202 Accepted`：

```json
{
  "mfa_required": true,
  "challenge_token": "一次性明文 token",
  "expires_at": "..."
}
```

此响应不得设置完整 Admin Session Cookie，也不得返回 CSRF Token。

TOTP 或恢复码验证成功后，响应与当前登录成功响应一致，并设置正式 Session Cookie。

### 8.2 已认证的 MFA 管理

```text
GET    /admin/api/v1/security/mfa
POST   /admin/api/v1/security/mfa/authenticators
POST   /admin/api/v1/security/mfa/authenticators/{id}/confirm
PATCH  /admin/api/v1/security/mfa/authenticators/{id}
DELETE /admin/api/v1/security/mfa/authenticators/{id}
POST   /admin/api/v1/security/mfa/recovery-codes/regenerate
DELETE /admin/api/v1/security/mfa
```

所有 mutation 继续要求：

- 正式 Admin Session；
- 同源校验；
- CSRF；
- 适用资源的 `If-Match` revision；
- `Cache-Control: no-store`；
- 对敏感动作执行本 PRD 规定的密码和 MFA 重新认证。

具体请求体在实现前以 OpenAPI/契约测试固定，错误响应不得包含 Secret、验证码、恢复码、challenge token 或验证器匹配信息。

## 9. 服务端验证规则

### 9.1 TOTP 校验

1. 校验输入严格归一化为 6 位 ASCII 数字。
2. 获取服务器当前 UTC 时间步。
3. 对账号所有有效验证器计算允许窗口内的候选值。
4. 对候选值执行常量时间比较，避免基于验证器顺序的明显时序差异。
5. 最多允许一个验证器成功；如果由于配置异常出现多个匹配，按 fail closed 处理并写入安全 Audit。
6. 原子检查并更新该验证器的 `last_accepted_time_step`；小于或等于已接受时间步的验证码拒绝重放。
7. Secret 解密或存储异常时 fail closed，不创建 Session。

实现必须避免在日志、Metric label、Trace、panic、错误响应和 Audit Details 中写入用户提交的验证码或 Secret。

### 9.2 限流

限流至少覆盖：

- 来源 IP 的密码登录；
- 账号维度的连续失败；
- 单个 pre-auth challenge 的尝试次数；
- MFA enrollment/reauth 验证；
- 恢复码验证。

建议初始值：

- challenge 最多 5 次失败；
- 5 分钟过期；
- 失败后使用现有统一登录限流窗口；
- 响应不得通过状态码、文案或显著时间差暴露用户名、MFA 状态或恢复码是否存在。

不能通过反复完成密码步骤获取新 challenge 来无限重置 MFA 失败计数。

### 9.3 时钟

- TOTP 依赖准确时间，生产部署文档必须要求 NTP/系统时间同步；
- 服务端只使用 UTC 计算，不受 `usage.timezone` 影响；
- 不自动扩大校验窗口来容忍严重漂移；
- 时间异常必须通过运维可观测性暴露，但 Metric 不包含用户标识。

## 10. Session 与安全状态变化

以下操作必须增加账号 Session generation、删除该账号所有现有 Session 和 pre-auth challenge，并按流程为当前客户端重新签发或要求重新登录：

- 第一次启用 MFA；
- 添加或撤销验证器；
- 关闭 MFA；
- 重新生成恢复码；
- 离线重置 MFA；
- 密码变更或离线密码重置；
- Master Key rotation。

这保证安全配置变化不会被旧 Session 长期绕过。

## 11. Audit 事件

至少新增：

```text
admin.mfa.enrollment.started
admin.mfa.authenticator.added
admin.mfa.authenticator.renamed
admin.mfa.authenticator.revoked
admin.mfa.enabled
admin.mfa.disabled
admin.mfa.challenge.failed
admin.mfa.login.success
admin.mfa.recovery_code.used
admin.mfa.recovery_codes.regenerated
admin.mfa.reset_offline
```

Audit 可包含：Admin 用户名、验证器稳定 ID、动作结果、受控原因枚举。不得包含 Secret、二维码 URI、TOTP、恢复码、challenge token、Cookie 或可用于重建敏感值的数据。

为避免暴力尝试放大 Audit 存储，失败事件需要遵循有界写入/聚合策略，但成功、配置变更、恢复码使用与离线重置不得丢失。

## 12. 配置、升级与回滚

### 12.1 升级

- 新版本默认 `mfa_policy=optional`；
- 现有管理员升级后仍可使用密码登录；
- 数据存储 migration 必须幂等且支持现有 bbolt 数据库；
- 启用 `required` 前，配置检查应明确提示下次登录需要完成 MFA 设置。

### 12.2 备份与恢复

- 加密备份必须包含验证器密文、恢复码哈希和必要 generation；
- 不需要备份过期 challenge；恢复时应丢弃所有 challenge；
- 恢复到另一实例后，现有 TOTP 应继续工作，但所有 Admin Session 必须失效；
- 备份验证和 secret-canary gate 必须覆盖 MFA 数据，确保明文不泄漏。

### 12.3 回滚

一旦账号启用 MFA，不允许直接回滚到不理解 MFA 数据的旧二进制继续对外提供 Admin 登录，否则可能绕过第二因素。发布说明必须声明：

- 回滚前先使用新版本安全关闭 MFA，或执行经验证的离线迁移；
- 数据库存在有效 MFA 记录时，旧版本启动应 fail closed，不能静默忽略未知安全状态。

## 13. 测试与验收标准

### 13.1 单元测试

- RFC TOTP 测试向量；
- 当前、前一、后一时间步验证；
- 超出窗口拒绝；
- 每个验证器的同时间步重放拒绝；
- 多验证器中任意一个可成功；
- 独立撤销不影响其他验证器；
- Secret 加密、错误 Master Key 和 key rotation；
- 恢复码哈希、一次性消费、并发消费和 generation 失效；
- challenge 过期、尝试耗尽、单用途消费和 Session generation 绑定；
- 敏感值不会出现在错误和日志中。

### 13.2 API/集成测试

- 密码正确且启用 MFA 时不创建 Session Cookie；
- TOTP 成功后才创建 Session 与 CSRF；
- challenge 不能访问普通 Admin API；
- challenge 刷新、返回密码页、成功或失败耗尽后失效；
- 添加首个、添加后续、重命名、撤销、关闭和重新生成恢复码的完整授权矩阵；
- `required` 策略下未设置 MFA 的受限流程；
- `required` 策略下拒绝删除最后一个验证器；
- 登录/MFA 限流无法通过新 challenge 绕过；
- 密码变更、Master Key rotation、备份恢复和离线 reset-mfa 后状态正确；
- 所有 mutation 的 Origin、CSRF、revision 和 no-store 行为；
- Admin route contract 更新，避免新增端点静默消失。

### 13.3 前端测试

- 登录状态机与刷新/返回行为；
- 二维码、手工密钥、恢复码只展示一次；
- 多验证器列表与单独撤销；
- 中英文资源 parity；
- 键盘操作、焦点管理、屏幕阅读器标签和错误提示；
- 390px 窄屏无横向溢出；
- 浏览器产物扫描确认不存在 Secret 持久化、source map、CDN 或 service worker。

### 13.4 安全测试

- TOTP Secret、恢复码和 challenge canary 扫描覆盖日志、Audit、bbolt、WAL、备份、Admin HTML/API、浏览器构建产物和 panic/error；
- 登录枚举与响应时序检查；
- 并发验证码重放只能成功一次；
- 并发恢复码消费只能成功一次；
- enrollment race、撤销 race、Session rotation race；
- 时钟边界和系统时间回拨行为；
- 存储损坏或 Secret 解密失败时 fail closed。

### 13.5 完成定义

功能只有在以下条件全部满足时才算完成：

1. Google Authenticator、Microsoft Authenticator 和至少一个第三方标准 TOTP 应用完成真实扫码验证。
2. 一个账号绑定至少两个独立验证器，均可登录并可分别撤销。
3. 恢复码与离线 `reset-mfa` 完成锁死恢复演练。
4. 全量 Go test、Race、Vet 与 Web typecheck/test/build 通过。
5. 备份/恢复、Master Key rotation、Session invalidation 与 Audit 完成回归。
6. Threat Model、Operator Guide、User Guide、Implementation Status、配置示例和 Release Notes 同步更新。
7. 安全评审确认没有完整 Session 提前签发、Secret 明文持久化、验证码重放或旧版本回滚绕过。

## 14. 建议实施顺序

### Phase 1：领域与存储

- TOTP、验证器、恢复码和 challenge 领域模型；
- bbolt bucket/migration；
- Vault 加密与 Master Key rotation；
- 原子防重放和恢复码消费；
- 单元测试。

### Phase 2：认证状态机与 CLI

- 两阶段登录；
- pre-auth challenge 与组合限流；
- MFA 管理 API；
- Session generation 联动；
- 离线 `reset-mfa`；
- Audit 与契约测试。

### Phase 3：Admin Console

- 登录 MFA 状态；
- 安全设置与多验证器管理；
- 一次性二维码/恢复码界面；
- 中英文、本地化、可访问性和窄屏测试。

### Phase 4：系统安全闭环

- 备份/恢复和 Master Key rotation；
- secret-canary、安全 race 与回滚保护；
- 运维/用户/威胁模型文档；
- 真实 Authenticator 兼容性验收。

## 15. 待实现阶段确认的问题

以下问题不阻塞 PRD，但在编码前应形成 ADR 或明确实现选择：

1. pre-auth challenge 使用受限 HttpOnly Cookie 还是响应体中的内存 token；两者都不得具备普通 Admin Session 权限。本 PRD 默认响应体内存 token。
2. 恢复码的可读格式与哈希算法，需要在“高随机熵、易抄写、常量时间验证、低运维成本”之间确定具体实现。
3. bbolt 中 revoked 验证器非敏感元数据的保留周期；Secret 密文必须立即清除。
4. `required` 策略从 optional 切换时，是否在配置检查之外增加启动期显式告警 Metric。

