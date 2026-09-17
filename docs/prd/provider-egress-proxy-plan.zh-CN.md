# Provider 显式出站代理与区域化端点方案

状态：**已实现并通过最终验证**
适用版本：未发布的当前开发版本，不保留旧 YAML 配置兼容层。

## 1. 结论

Provider 出站代理统一由 Admin 后台管理，不再写入 `config.yaml`：

- Admin 可新增、编辑、删除具名 HTTP CONNECT 代理；
- Provider 显式选择“直连”或某个代理，不设全局默认代理；
- 代理定义存入 bbolt，Basic Auth 作为内部 Credential 由 Vault 加密；
- 保存后重建 Provider 与代理运行时快照并原子切换，无需重启 Halro；
- 代理变更受到 revision、step-up、审计、引用约束和 Deployment drain 约束保护；
- 备份、恢复与 Master Key 轮换覆盖代理定义和认证材料；
- Bedrock `region` 也不再是启动配置。Admin 创建 Bedrock Credential 时填写区域，区域被展开为该 Credential 绑定的 Mantle endpoint。

`config.yaml` 只保留进程启动、监听、存储、TLS、审计等部署级设置。Provider 产品、Credential、区域 endpoint、出站代理及其认证属于应用管理数据。

## 2. 目标与非目标

### 2.1 目标

1. 为不同 Provider 连接选择不同的显式出站路径。
2. 保持直连与代理路径相同的目标地址校验、DNS 防护、TLS 主机名验证和失败语义。
3. 通过 Admin UI 完成代理的完整生命周期和 Bedrock 区域配置。
4. 代理配置修改后立即生效，同时安全排空旧连接池。
5. 不在 YAML、URL、日志、API 响应或浏览器存储中暴露代理密码。
6. 让备份、恢复、离线 doctor 与 Master Key 轮换包含新增状态。

### 2.2 非目标

- 不读取 `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY` 或 `NO_PROXY`；
- 不支持 SOCKS、PAC、透明代理或自动代理发现；
- 不允许请求按用户输入临时指定代理；
- 不代理 Provider 自己执行的工具调用或上游侧联网；
- 不实现 TLS 证书固定；仍使用原 Provider 主机名与系统信任库验证证书；
- 不把 `us-east-1` 解释为全局固定区域。它只是 Bedrock 新建表单的初始值。

## 3. 配置边界

以下配置已从 `config.yaml`、`default.yaml` 和示例配置中删除：

```yaml
providers:
  bedrock:
    region: us-east-1
  egress_proxies: []
```

不提供旧字段迁移、别名、弃用期或启动告警。该功能尚未发布，保留两套权威来源只会引入漂移。

新的唯一入口：

- `Admin → Credentials & Providers → Outbound proxies` 管理代理；
- 创建 Bedrock Credential 时填写 AWS Region；
- 创建或编辑 Provider 时选择直连或一个已存在的代理。

## 4. 持久化模型

### 4.1 ProviderEgressProxy

```go
type ProviderEgressProxy struct {
    ID                      string
    Name                    string
    Kind                    string // http_connect
    Endpoint                string // http(s)://host:port
    AllowPrivateEndpoint    bool
    AllowLoopbackEndpoint   bool
    BasicAuthCredentialID   string
    AllowCleartextBasicAuth bool
    CreatedAt               time.Time
    UpdatedAt               time.Time
    Revision                uint64
}
```

约束：

- 最多 32 个代理；
- ID 使用稳定、URL 安全的小写标识；
- endpoint 只允许 `http` 或 `https`，必须显式端口；
- 禁止 URL userinfo、path、query、fragment 和 IPv6 zone；
- Basic Auth 经 HTTP 发送时必须显式确认 `allow_cleartext_basic_auth`；
- 没有 Basic Auth 时不能保留该确认开关。

### 4.2 代理认证

代理认证复用 Credential/Vault 基础设施，但标记为内部类型 `provider_egress_proxy`：

- 明文文档仅包含 `username` 和 `password`；
- AEAD audience 为代理 endpoint；
- endpoint 改变时必须解密并用新 audience 重新封装；
- 普通 Credential 列表、详情、修改和删除 API 不返回或操作这类记录；
- 删除代理时在同一 bbolt 事务中删除其内部 Credential；
- Master Key 轮换按现有 Credential 扫描自动重加密代理认证。

### 4.3 Provider 引用

`ProviderInstance.egress_proxy_id`：

- 空值表示显式直连；
- 非空值必须指向当前已激活的代理；
- Provider 不会因代理缺失或失败回退到直连；
- 删除仍被任一 Provider 引用的代理返回 `409 provider_egress_proxy_in_use`。

### 4.4 Bedrock Region

Provider profile API 同时返回：

- `default_base_url`：新建表单的可直接使用值；
- `base_url_template`：例如 `https://bedrock-mantle.{region}.api.aws`。

Admin 创建 Bedrock Credential 时填写区域，浏览器用模板生成 endpoint，服务端仍按完整 endpoint 校验并进行 audience binding。Credential 创建后区域不可原地迁移；换区域需要创建新的 Credential，再切换 Provider，以免同一密钥在不受控的 endpoint 间移动。

`us-east-1` 是新建表单初始值，不是运行时配置、强制值或共享默认值。

## 5. Admin API

仅认证后的 Admin 路由可访问：

| 方法 | 路径 | 语义 |
|---|---|---|
| `GET` | `/admin/api/v1/provider-egress-proxies` | 列出非敏感代理元数据和当前 runtime ID |
| `POST` | `/admin/api/v1/provider-egress-proxies` | 创建代理；要求 Idempotency-Key 与 step-up |
| `GET` | `/admin/api/v1/provider-egress-proxies/{id}` | 获取单个代理；返回 ETag |
| `PUT` | `/admin/api/v1/provider-egress-proxies/{id}` | 完整更新；要求 If-Match 与 step-up |
| `DELETE` | `/admin/api/v1/provider-egress-proxies/{id}` | 删除；要求 If-Match 与 destructive step-up |

响应只返回：ID、名称、类型、endpoint、策略开关、是否配置认证、revision，以及仅在持久化已提交但运行时仍待恢复时出现的 `activation_pending=true`。永不返回用户名、密码、密文或内部 Credential ID。

认证更新采用三态：

- 用户名和密码都省略：保留现有认证；
- 用户名和密码都提供：替换并增加 Credential key version；
- `clear_basic_auth=true`：删除认证；不能同时提供新认证。

每次变更写 Admin Audit Intent，并遵守既有 commit protocol：持久化成功是提交点；若热激活失败，管理面返回已提交状态、运行时标记 stale、数据面拒绝使用陈旧拓扑，后台恢复循环重试。

## 6. 生命周期与热激活

### 6.1 快照构建

`providerEgressRegistry` 是不可变快照。构建过程：

1. 从 bbolt 读取全部代理定义；
2. 校验定义；
3. 读取并解密内部 Credential；
4. 为每个代理创建独立的 `HTTPConnectDialer`；
5. 计算不含秘密的 fingerprint 与 registry runtime ID；
6. 用该 registry 构建新的 Provider adapter registry。

代理与 Provider registry 必须从同一轮持久化视图构建并顺序切换，不能出现新 Provider 引用旧代理表的长期状态。

### 6.2 变更保护

- 修改名称等非运行时字段可直接保存；
- 修改 endpoint、地址策略或认证前，若引用该代理的 Provider 仍有 enabled Deployment，返回冲突，要求先停用或排空；
- Provider 改选代理同样受现有 enabled Deployment 锁保护；
- 删除代理前必须解除全部 Provider 引用。

### 6.3 旧连接排空

切换后新请求只使用新快照。旧 Provider adapter 和代理连接池保留到以下较大超时再关闭：

```text
max(gateway.route_total_timeout,
    gateway.stream_max_duration,
    admin.model_capability_detection.total_timeout) + 1s
```

若旧快照没有被退役 adapter 引用，可立即关闭。

## 7. SafeTransport HTTP CONNECT

### 7.1 禁止环境代理

所有 Provider `http.Transport` 保持 `Proxy: nil`。代理通过受控 Dialer 注入，而不是 `http.ProxyFromEnvironment`，防止部署环境变量绕过 Admin 决策。

### 7.2 两段独立校验

代理模式包含两段地址解析和固定拨号：

1. 解析代理 endpoint，按代理自己的 private/loopback 开关校验每个地址，然后固定拨到获准 IP；
2. 解析 Provider 原始主机，按 Provider endpoint policy 校验每个地址，选择获准字面 IP 作为 CONNECT authority。

任何阶段都不能把已校验主机名交回系统代理或二次 DNS 解析。fake-IP、回环、链路本地、私网和其他特殊地址继续由 SafeTransport policy 控制。

### 7.3 CONNECT 与 TLS

- 向代理发送 `CONNECT <provider-ip>:<port>`；
- 代理 endpoint 为 HTTPS 时先对代理建立 TLS，并验证代理主机名；
- CONNECT 成功后，在隧道内以原 Provider hostname 建立 TLS；
- Provider certificate 仍按原 hostname 验证；
- `Proxy-Authorization` 只发给代理，不进入 Provider 请求头；
- 限制代理响应头大小和握手时间，非 2xx 状态失败关闭。

代理可观察目标 IP、端口和 TLS SNI。系统信任库若已安装代理控制的 CA，仍可能接受其签发的 Provider 证书；本方案不声称证书固定。

## 8. 错误、重试与诊断

结构化代理阶段仅使用有限枚举，例如：

- `proxy_dns`
- `proxy_connect`
- `proxy_tls`
- `proxy_auth`
- `proxy_status`
- `connect_tunnel`

Admin 连接测试可返回 `proxy_stage` 与数字 `proxy_status`，但不得返回代理 ID、endpoint、用户名、响应 reason phrase 或响应体。

Provider、Deployment 与 Route 的成功测试证据必须记录实际使用的出站路径 fingerprint。启用或展示“当前”状态时，同时比较资源 revision 与该 fingerprint；切换代理、修改代理 endpoint/地址策略/认证，或在测试期间发生上述变化，均使旧证据失效。修改无关代理或仅重命名当前代理，不应误伤这条证据。Provider 存在多个已启用能力接口时，服务商级测试必须覆盖全部接口，否则只能分别测试 Deployment，不能把单个接口的结果写成整个 Provider 健康。

失败重试遵守现有“是否已向 Provider 发送字节”边界：CONNECT 隧道建立前的失败通常是 unsent；隧道建立后或请求可能已写入时按 ambiguous 处理，不能因为“代理错误”就扩大重试。

`halro doctor` 离线验证：

- bbolt 代理定义可读且合法；
- 内部 Credential 可读取、类型与 audience 正确、可解密；
- Provider 引用的代理存在；
- 不执行 DNS、TCP、CONNECT 或真实 Provider 请求。

真实连通性只通过 Admin 的 Provider connection test 验证。

## 9. 控制台交互

`Credentials & Providers` 页面包含三个页签：

1. Providers
2. Credential vault
3. Outbound proxies

Outbound proxies 页面：

- 展示名称、完整 endpoint、是否认证、引用数量；
- 新增/编辑表单支持 private、loopback 与 HTTP 明文认证显式确认；
- 已配置的密码永不回显；留空表示保留，显式选择才清除；
- 被 Provider 引用时禁用删除并由服务端再次校验；
- 所有写操作按需弹出 step-up；
- 成功文案明确“已保存并自动应用，无需重启”，但不得在运行时激活失败、尚待恢复循环重试时宣称已经应用完成。

Provider 表单只列出 Admin 管理的代理，默认仍是直连。信任边界说明必须指出代理可见信息、Provider TLS 验证方式，以及 Provider 自行联网不经过 Halro。

Bedrock Credential 表单显示 AWS Region。基础 URL 仍作为最终绑定结果展示，便于审计和高级自定义 endpoint；Region 不是隐藏的全局配置。

## 10. 备份、恢复与密钥轮换

代理定义位于 bbolt，认证位于 Vault Credential，因此完整加密备份自动包含二者。恢复后：

- 不需要另外恢复 YAML 或认证文件；
- 必须仍具备到代理 endpoint 的网络路径、DNS、Firewall 与 TLS trust root；
- Runtime 在监听前校验代理定义与认证；
- Provider 绝不因恢复环境缺少代理网络而退回直连。

Master Key 轮换覆盖内部代理 Credential，沿用 copy-on-write、key version、验证和崩溃恢复协议。普通 Credential 数量与轮换报告可能包含内部代理认证记录，但 Admin Credential 列表不显示它们。

## 11. Schema 与回滚边界

bbolt schema 38 创建 `provider_egress_proxies` bucket，并作为旧二进制兼容 fence：schema 37 二进制不知道 `egress_proxy_id`，若读取新数据可能错误直连，因此必须拒绝打开。

当前功能尚未发布，直接修改 schema 38 的未发布 migration，不新增“YAML 到数据库”的迁移。升级前仍需创建并验证完整备份；降级只能恢复由旧版本创建的完整备份，不能让旧二进制读取 schema 38 数据目录。

## 12. 测试矩阵

### Domain / Store

- endpoint、端口、userinfo、path、scheme、cleartext auth 约束；
- revision 冲突与 32 条上限；
- 代理定义与内部 Credential 原子提交；
- Credential 被代理引用时不可单独删除；
- Provider 引用存在时代理不可删除；
- schema 38 创建 bucket 且不重写 Provider JSON。

### SafeTransport

- HTTP/HTTPS proxy、Basic Auth、407、非 2xx、超大 header、超时；
- 代理 DNS 与 Provider DNS 独立 policy；
- fixed-IP CONNECT authority、原 hostname TLS、无环境代理；
- 连接关闭、错误分级与敏感信息红action。

### App / API

- Admin CRUD、Idempotency-Key、If-Match、step-up、审计；
- 密码密文不含明文且普通 Credential API 不可见；
- create/update/delete 后 runtime ID 与 connector 立即变化；
- 激活失败时写操作仍明确返回已提交状态，并以 `activation_pending` 阻止 UI 误报已经生效；
- endpoint 改变时认证重新 audience bind；
- Provider、Deployment、Route 测试证据绑定所用代理 fingerprint，切换或修改该代理后自动失效；
- enabled Deployment 阻止运行时代理变更；
- 引用阻止删除；
- Provider 无代理时直连，代理缺失时 withholding 且不回退；
- doctor、backup/restore 与 Master Key rotation 覆盖新增数据。

### Web

- 三页签键盘导航；
- 代理列表和 CRUD 表单；
- Basic Auth 保留/替换/清除；
- private、loopback、cleartext 风险确认；
- Bedrock Region 生成正确 endpoint；
- Provider 代理选择与 enabled Deployment 锁；
- 中英文文案与错误码映射。

### 最终 gate

```bash
go test -count=1 ./...
cd web
npm run typecheck
npm test -- --run
npm run build
cd ..
git diff --exit-code -- internal/webui/dist
```

不在自动测试中执行计费的真实 Provider smoke。

## 13. 验收标准

- `config.example.yaml`、默认配置和 Config 类型中不存在 `providers` 代理或 Bedrock region；
- Admin 可完整管理代理且无需重启；
- 代理认证只以 Vault 密文持久化，任何列表/详情/API/日志不回显；
- Provider 明确绑定代理，失败时不回落直连；
- 修改代理或 Provider 出站边界前必须排空 enabled Deployment；
- SafeTransport 在代理和目标两段都执行地址校验，并保持原 Provider TLS 主机名；
- Bedrock 区域可在 Admin 创建 Credential 时配置，每份 Credential 独立绑定；
- backup/restore、doctor、Master Key rotation 和 schema fence 覆盖新状态；
- focused tests 与最终 gate 全部通过。

## 14. 后续可选演进

- 代理健康状态和只读诊断页；
- 多代理 failover（必须显式建模，不能静默直连）；
- mTLS 代理认证；
- 外部 Secret Manager-backed 内部 Credential；
- 分节点代理可用性与拓扑调度。

这些能力均不改变本方案的核心边界：代理由 Admin 管理、Provider 显式引用、秘密由 Vault 持有、运行时原子热切换、失败绝不隐式改变出站路径。
