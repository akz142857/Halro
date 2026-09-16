# Provider 显式出站代理改造方案

> 状态：Draft
> 日期：2026-09-16
> 范围：Halro 发往 Provider 的数据面、模型枚举、能力检测和连接测试流量
> 核心约束：保留 SafeTransport 的目标域名允许列表、DNS 全量校验、IP 固定拨号和 SSRF 防护；不得改用 `http.ProxyFromEnvironment`，也不得把目标域名交给代理解析。

## 1. 背景与结论

Halro 当前通过 `internal/safetransport` 创建 Provider HTTP Client：

- `http.Transport.Proxy` 固定为 `nil`，因此忽略 `HTTP_PROXY`、`HTTPS_PROXY` 和 `NO_PROXY`；
- 请求目标先经过 URL、scheme、userinfo 和 host allowlist 校验；
- 目标域名由 Halro 解析，所有 A/AAAA 结果都必须通过地址策略；
- 实际连接只拨本次校验过的一个字面 IP，避免 DNS rebinding；
- 重定向被禁止；
- `198.18.0.0/15`、云元数据、loopback、link-local、隧道地址等仍按现有规则拒绝。

这套边界是 Provider 凭据防外泄和 SSRF 防护的一部分，不能用下面这种改法替换：

```go
Proxy: http.ProxyFromEnvironment
```

标准库环境代理会引入三类无法接受的行为：

1. 进程环境可以在 Halro 配置和审计之外改变流量去向；
2. HTTPS `CONNECT` 通常携带 `hostname:port`，由代理再次解析，Halro 无法证明代理最终连接的是刚刚校验过的 IP；
3. `NO_PROXY`、大小写环境变量和运行环境注入会制造隐式直连/代理分支，故障时也难以判断真实路径。

本方案采用以下结论：

1. **默认仍为直连**，既有 Provider 和既有配置零迁移、零行为变化；
2. 在 `config.yaml` 中定义有限、具名、重启生效的 HTTP CONNECT 代理；
3. 每个 Provider 实例显式选择 `direct` 或一个代理 ID，不设置全局默认代理；
4. SafeTransport 仍先解析并验证 Provider 目标，再把**已验证的字面 IP**交给自定义 CONNECT 拨号器；
5. `http.Transport.Proxy` 在直连和代理模式下都继续保持 `nil`；
6. 代理端点本身也做独立的 DNS 固定和地址策略校验；
7. 配置了代理却不可用时 fail closed，**不得悄悄回退直连**；
8. v1 只支持 HTTP CONNECT，不支持 SOCKS5、`socks5h`、PAC、WPAD、系统代理和 TLS 中间人。

## 2. 目标与非目标

### 2.1 目标

- 让指定 Provider 的全部上游访问通过一个管理员批准的显式代理；
- 保持 Provider 目标的现有 SSRF、防重绑定、host allowlist 和重定向策略；
- 允许 Halro 运行在容器内，通过宿主机或同网络的 Mihomo HTTP/mixed 端口出站；
- Provider 创建、编辑、启停、连接测试和运行时路由使用同一个代理选择；
- 代理引用变化后复用现有 topology activation，原子替换 Provider Registry；
- 提供不泄露代理凭据的诊断、日志、审计和控制台状态；
- 为后续增加受控解析器、SOCKS5 或多代理容灾留下明确扩展点。

### 2.2 非目标

第一版不做：

- 不读取任何代理环境变量；
- 不支持代理侧 DNS、`socks5h` 或把 Provider hostname 原样交给 CONNECT；
- 不支持 SOCKS5、PAC、WPAD、NTLM、Kerberos；
- 不允许代理替换 Provider TLS 证书，不增加 Provider 自定义 CA；
- 不给签名模型目录、告警 webhook、KMS、审计锚、备份等非 Provider 流量套用该配置；
- 不做全局“所有 Provider 默认走代理”；
- 不做“代理失败后直连”或多代理自动回退；
- 不在本方案中修改 `deniedPrefixes`，尤其不放行 `198.18.0.0/15`；
- 不以真实、计费 Provider 请求作为自动测试的一部分。

## 3. 当前调用链与改造边界

当前 Provider Client 的关键路径是：

```text
ProviderInstance
  → loadProviderRegistryWithCatalog
  → newProviderBindingAdapter
  → newBindingClient
  → safetransport.NewClient
  → pinnedDialContext
  → net.Dialer
```

`newBindingClient` 是 Profile Binding 创建 HTTP Client 的单一入口。Bedrock Runtime 派生出的控制面 host 也在这里加入 allowlist，因此代理能力应在这里接入，而不是分别修改：

- `internal/provider/openai`
- `internal/provider/anthropic`
- `internal/provider/bedrock`
- `internal/provider/bedrockmantle`

这样，同一 Binding Client 发出的推理、模型枚举、能力检测、健康/连接测试和资源操作天然走同一路径，不会出现“测试走代理、真实请求直连”或相反的分叉。

## 4. 必须保持的安全不变量

以下条件是验收门槛，不是实现建议。

### 4.1 Provider 目标不变量

1. 请求 URL 仍先经过 `ValidateURL`；
2. Provider hostname 仍必须命中该 Provider 的 `AllowedHosts`；
3. Provider DNS 由 Halro 的目标 Resolver 完成；
4. DNS 返回的每一个地址都必须通过现有 `validateAddress`；混合公有/私有答案整体拒绝；
5. CONNECT authority 必须是本次验证过的字面 `IP:port`，不能是 hostname；
6. TLS `ServerName` 仍是原始 Provider hostname，证书仍由 Halro/Go 校验；
7. HTTP redirect 仍拒绝；
8. `security.allow_private_provider_endpoints` 只影响 Provider 目标，不因代理端点位于私网而自动放宽。

### 4.2 代理端点不变量

1. 代理只能来自启动配置中的具名条目，Gateway 请求、Provider 响应和环境变量都不能选择或改写它；
2. 代理 URL 禁止 userinfo、path、query 和 fragment；
3. 代理 hostname 也要解析、校验全部结果并固定拨号；
4. 私网代理和 loopback 代理分别需要显式 opt-in；
5. 云元数据、unspecified、multicast、link-local 和保留/隧道地址始终拒绝；
6. 代理凭据只出现在 CONNECT 握手的 `Proxy-Authorization`，不能进入 Provider 请求头；
7. 代理配置、凭据文件路径和秘密不得进入 Gateway 响应、Provider 错误正文、日志或 Prometheus label；
8. CONNECT 未建立时必须标记为“Provider 未收到请求”，避免错误计费和歧义结算。

### 4.3 路径选择不变量

- `egress_proxy_id == ""`：严格直连；
- `egress_proxy_id != ""`：严格使用对应代理；
- 引用不存在、代理配置失效、认证失败或代理不可达：该 Binding/Provider 不可用；
- 任意失败都不得从代理模式退回直连；
- `HTTP_PROXY`、`HTTPS_PROXY`、`ALL_PROXY`、`NO_PROXY` 在两种模式下都无效。

## 5. 配置与持久化模型

### 5.1 启动配置：批准哪些代理存在

代理是部署基础设施和新的信任边界，定义放在 `providers` 启动配置中，并与监听器受信代理一样归类为 restart-only semantics：

```yaml
providers:
  bedrock:
    region: us-east-2

  egress_proxies:
    - id: mihomo-aws
      name: Mihomo AWS egress
      kind: http_connect
      endpoint: http://172.18.0.1:7890
      allow_private_endpoint: true
      allow_loopback_endpoint: false
      basic_auth_file: ""
      allow_cleartext_basic_auth: false
```

建议增加配置类型：

```go
type Providers struct {
    Bedrock       BedrockProvider         `yaml:"bedrock"`
    EgressProxies []ProviderEgressProxy   `yaml:"egress_proxies"`
}

type ProviderEgressProxy struct {
    ID                       string `yaml:"id"`
    Name                     string `yaml:"name"`
    Kind                     string `yaml:"kind"`
    Endpoint                 string `yaml:"endpoint"`
    AllowPrivateEndpoint     bool   `yaml:"allow_private_endpoint"`
    AllowLoopbackEndpoint    bool   `yaml:"allow_loopback_endpoint"`
    BasicAuthFile            string `yaml:"basic_auth_file"`
    AllowCleartextBasicAuth  bool   `yaml:"allow_cleartext_basic_auth"`
}
```

配置规则：

- `id` 使用稳定、小写、可审计的标识，建议限制为 `[a-z0-9][a-z0-9._-]{0,62}`；
- ID 唯一，代理条目总数建议上限 32；
- v1 的 `kind` 只能是 `http_connect`；
- `endpoint` 只接受 `http://host:port` 或 `https://host:port`；端口必填；
- `endpoint` 不得携带用户名/密码；
- `allow_private_endpoint` 允许 RFC1918/CGNAT 代理地址，但不允许 loopback；
- `allow_loopback_endpoint` 单独允许 loopback，避免一个“允许私网”开关顺带扩大到本机全部端口；
- `https://` 代理使用系统根证书验证代理证书，不支持跳过校验；
- `basic_auth_file` 为空表示无认证；
- `http://` 上配置 Basic auth 默认拒绝，只有显式设置 `allow_cleartext_basic_auth: true` 才接受；
- 配置省略 `egress_proxies` 时行为与当前版本完全一致，不提升 `SchemaVersion`。

代理定义和认证材料第一版均为**重启生效**。SIGHUP 继续通过现有 `changedOutsideReloadable` 报告 `providers` 发生了 restart-only 变化，不声称已经应用。

### 5.2 代理认证文件

不把密码放进 YAML、URL、环境变量或 bbolt。可选的 `basic_auth_file` 使用独立、严格 JSON：

```json
{
  "version": 1,
  "username": "halro",
  "password": "replace-me"
}
```

加载要求：

- 只允许一个 JSON 文档和已知字段；
- 设置小尺寸上限，例如 8 KiB；
- Unix 下拒绝 group/other 可读写权限；
- 错误只报告代理 ID 与字段，不回显内容；
- Admin API 只返回 `authenticated: true/false`，不返回文件路径、用户名或密码；
- 凭据轮换在 v1 中通过原子替换文件并重启 Halro 完成。

Mihomo 无认证的本地/mixed 端口不需要该文件。

### 5.3 Provider 持久化：哪个连接使用哪个代理

在 `domain.ProviderInstance` 增加：

```go
EgressProxyID string `json:"egress_proxy_id,omitempty"`
```

语义：

- 空值表示直连；
- 非空值引用启动配置中的代理 ID；
- 字段属于 Provider connection，而不是 Credential 或 Profile Binding；
- 一个 Provider 的全部 Binding 共用同一出站路径；
- Credential audience 仍只绑定 Provider origin，切换代理不改变 audience，也不要求轮换 Provider Credential；
- 旧 bbolt JSON 没有该字段时自然读取为空，不需要数据迁移。

把代理选择放在 Provider 层有三个理由：

1. 同一连接的多个 Profile Binding 使用同一凭据和 endpoint，不应出现部分协议走另一条网络路径；
2. 路由和 Deployment 不应控制基础网络信任；
3. 现有 Provider revision、连接测试留痕和 Registry 原子替换可以直接覆盖代理变更。

### 5.4 不设置全局默认代理

配置中不提供 `default_proxy_id`。原因是：

- 新增一段基础设施配置不应静默改变全部已有 Provider；
- Operator 必须逐连接确认哪些数据可以经过该代理；
- Provider 引用丢失时可明确 fail closed，而不是猜测另一个默认路径；
- 审计可以直接回答某个 Provider 选择了什么。

## 6. SafeTransport 实现设计

### 6.1 保持 `Proxy: nil`

底层 transport 继续保持：

```go
transport := &http.Transport{
    Proxy: nil,
    // 其余现有超时、池和 HTTP/2 配置保持不变。
}
```

代理通过 SafeTransport 已有的 `Dialer` 注入点实现，而不是通过 `http.Transport.Proxy`：

```text
pinnedDialContext(target policy, target resolver, connector)
  1. 校验 target host allowlist
  2. 解析 target hostname
  3. 校验全部 target IP
  4. 选择已验证 IP:port
  5. connector.DialContext(validated IP:port)
       ├─ direct connector: 直接拨该 IP
       └─ HTTP CONNECT connector: 经固定代理隧道拨该 IP
```

现有 `Options.Dialer` 可以保留为测试缝，但生产构造不应让 app 自行拼装 CONNECT。建议在 `internal/safetransport` 内提供受约束构造器：

```go
type ProxyEndpointPolicy struct {
    AllowPrivate  bool
    AllowLoopback bool
}

type HTTPConnectOptions struct {
    ID             string
    Endpoint       *url.URL
    EndpointPolicy ProxyEndpointPolicy
    Resolver       Resolver
    DirectDialer   Dialer
    BasicAuth      *BasicAuth
    ConnectTimeout time.Duration
}

func NewHTTPConnectDialer(HTTPConnectOptions) (Dialer, error)
```

app 层只按代理 ID 获取已经验证的 connector，再交给 `NewClient`；不接触代理密码，也不自己构造 `Proxy-Authorization`。

### 6.2 两次独立固定拨号

代理模式包含两个不同对象，必须分别校验：

```text
Provider URL host
  → target Resolver
  → target Policy / AllowedHosts
  → validated target IP:port

Configured proxy host
  → proxy Resolver
  → ProxyEndpointPolicy
  → validated proxy IP:port
  → CONNECT validated-target-IP:port
```

代理端点允许私网/loopback，不代表 Provider 目标也允许。两套 policy 不得复用一个 `AllowPrivate` 值。

### 6.3 CONNECT 握手

自定义拨号器执行：

1. 按代理 endpoint policy 解析并固定拨代理 IP；
2. `https://` 代理先以代理 hostname 作为 SNI 完成 TLS；
3. 写入有大小上限的 CONNECT 请求；
4. authority 与 `Host` 均使用已验证的字面 `IP:port`；
5. 如有认证，仅写 `Proxy-Authorization: Basic ...`；
6. 在 connect timeout 内读取状态行和受限响应头；
7. 仅 2xx 表示隧道建立，其他状态关闭连接并返回结构化代理错误；
8. 清除握手 deadline，返回 tunnel connection；
9. 原 `http.Transport` 在隧道上对原始 Provider hostname 做 TLS/SNI/证书校验并协商 HTTP/2。

示意报文：

```http
CONNECT 3.17.150.161:443 HTTP/1.1
Host: 3.17.150.161:443
Proxy-Authorization: Basic <redacted>
```

不得改成：

```http
CONNECT bedrock-mantle.us-east-2.api.aws:443 HTTP/1.1
```

后一种写法会把 DNS 决策交回代理，Halro 的 DNS 固定不再是端到端事实。

### 6.4 DNS 与 fake-IP

显式代理**不会**放宽 fake-IP 检查，也不会把解析交给 Mihomo：

- Halro 仍先解析 Provider hostname；
- 若系统 Resolver 返回 `198.18.0.0/15` fake IP，请求仍在拨代理之前拒绝；
- 部署侧必须让 Provider 域名得到真实 A/AAAA，例如加入 Mihomo `fake-ip-filter`、使用 `redir-host`，或给 Halro 提供不返回 fake IP 的系统 DNS；
- v1 不加入 `socks5h` 作为规避办法，因为那会绕过目标 IP 校验。

受控 DoH/DoT Resolver 可以作为后续独立阶段，但必须自行解决 resolver endpoint 的 bootstrap、TLS、地址固定和 SSRF，不能和本次 CONNECT 支持一起暗中引入。

### 6.5 连接池与关闭

- 每个 Binding 仍有自己的 `http.Client`/`http.Transport`；
- 池键保持原始 Provider origin，不按字面 IP 混池；
- 代理选择变化触发既有 Provider Registry 重建；
- 旧 adapter 按现有 grace period 排空后 `CloseIdleConnections`；
- 不在一个 transport 中动态切换 connector，避免复用到旧代理的空闲连接。

## 7. app、Admin API 与控制台

### 7.1 app wiring

在 Runtime 启动时构建只读的 `ProviderEgressRegistry`：

```text
config.Providers.EgressProxies
  → 规范化与静态校验
  → 读取认证材料
  → 构建具名 CONNECT connector
  → ProviderEgressRegistry
```

`newBindingClient` 增加 Provider 实例或 `egress_proxy_id` 参数：

```text
newBindingClient
  → 扩展 Bedrock control-plane allowlist（现有行为）
  → 按 Provider.EgressProxyID 选择 direct / connector
  → safetransport.NewClient
```

Bedrock Runtime 派生控制面 host 必须使用同一个 connector；不能让 Runtime 推理走代理而模型枚举直连。

### 7.2 Admin API

Provider create/update input 增加：

```json
{
  "egress_proxy_id": "mihomo-aws"
}
```

校验规则：

- 空值允许；
- 非空值必须存在于 Runtime 的只读代理注册表；
- 不存在时返回稳定错误码 `provider_egress_proxy_not_found`；
- 配置存在但构建失败应在启动期拒绝配置，而不是等 Provider 保存后失败；
- Provider 更新成功后复用 `activateTopologyAfterCommit`；
- 更新响应返回 ID，不返回代理 credential/path。

增加只读端点：

```http
GET /admin/api/v1/provider-egress-proxies
```

建议响应：

```json
{
  "items": [
    {
      "id": "mihomo-aws",
      "name": "Mihomo AWS egress",
      "kind": "http_connect",
      "endpoint_scheme": "http",
      "endpoint_host": "172.18.0.1",
      "endpoint_port": 7890,
      "authenticated": false
    }
  ]
}
```

该端点不主动拨代理；真实连通性由 Provider connection test 验证，避免“代理端口能连”被误报成“Provider 可用”。

### 7.3 已有 Provider 引用了被删除的代理

配置文件与 bbolt 生命周期不同，启动时可能出现 Provider 仍引用、配置已删除的情况。处理方式：

- 不把它解释为 direct；
- Provider/Binding 在 Registry load 中以 `egress_proxy_unavailable` 排除；
- 相关 route withholding 保持可见，Gateway 对无健康目标返回现有 503；
- Admin API 仍返回原 `egress_proxy_id`，控制台显示“配置缺失”；
- Operator 可恢复配置或把 Provider 改回 direct/另一个代理。

这属于可修复的运维配置错误，不应让 Admin 面也无法启动；但数据面必须 fail closed。

### 7.4 控制台

Provider 表单增加“出站路径”：

- `直接连接（默认）`；
- 启动配置中批准的代理列表；
- 文案明确：代理端只看到 TCP 目标与 TLS 元数据，不能解密 Provider HTTPS；
- 代理配置缺失时保留旧 ID、显示错误，不静默切到直接连接；
- Provider 列表/详情显示 `直接` 或代理显示名；
- connection test 继续测试完整 Provider 请求路径。

前端所有回写 Provider 的路径都必须保留 `egress_proxy_id`，尤其是当前列表里的“启用/禁用”快捷更新；否则一次启停会把代理选择清空。

## 8. 错误语义、重试和可观测性

### 8.1 结构化代理错误

在 `safetransport` 定义不会携带秘密的错误类型，例如：

```go
type ProxyError struct {
    ProxyID  string
    Stage    ProxyStage
    Status   int
    Retry    bool
    Cause    error
}
```

`Stage` 使用封闭枚举：

- `resolve_proxy`
- `validate_proxy_address`
- `dial_proxy`
- `tls_proxy`
- `write_connect`
- `read_connect`
- `proxy_auth`
- `proxy_refused`
- `proxy_protocol`

错误字符串不得包含：

- `Proxy-Authorization`；
- 用户名或密码；
- credential file 路径；
- 完整 CONNECT 响应头；
- 代理返回 body。

### 8.2 Unsent 与 ambiguity

CONNECT 拨号器返回 tunnel 之前，Provider HTTP 请求体不可能到达上游。因此：

- 代理解析、校验、拨号、TLS、407、非 2xx、协议错误均标记为 unsent；
- Gateway 不为这类失败做保守计费；
- 临时网络错误可以尝试其他健康 Deployment；
- 407、配置拒绝、协议不兼容为非重试；
- proxy 502/503/504、拨号超时等为可重试；
- CONNECT 成功、返回 connection 之后继续沿用现有保守 ambiguity 规则。

不能滥用当前 `ErrRefusedBeforeSend` 表示所有代理错误，因为 407 是代理给出的网络结果，不是 Halro policy refusal。建议新增通用、窄语义的“Provider 请求尚未发送”标记或接口，并让 `provider.Unsent` 识别它。

各 Provider adapter 当前对 transport error 的 retryable 推导需要收敛到一个公共辅助函数，避免 OpenAI、Anthropic、Bedrock 对同一个 407 得出不同结果。

### 8.3 日志、连接测试与指标

日志可增加：

- `egress_mode=direct|proxy`
- `proxy_id`（仅配置 ID，不是 endpoint）
- `proxy_stage`
- `proxy_status`（如 407/502）

Gateway 对调用方的错误仍使用现有安全外壳，不泄露内部代理拓扑。Admin connection test 可以返回受控诊断字段：

```json
{
  "error_class": "connect",
  "egress_mode": "proxy",
  "proxy_stage": "proxy_auth",
  "proxy_status": 407
}
```

Prometheus 不以 `proxy_id`、Provider ID、endpoint 或 IP 为 label。若新增指标，只使用有界字段，例如：

```text
halro_provider_egress_connect_total{mode="proxy",stage="dial_proxy",result="failure"}
```

## 9. 文件级改造清单

### 9.1 配置

- `internal/config/config.go`
  - 增加代理配置类型、Normalize 与 Validate；
  - 明确 omitted = direct-compatible；
  - endpoint、ID、数量、auth transport 和地址策略校验。
- `internal/config/default.go`
  - 默认 `egress_proxies` 为空。
- `internal/config/default.yaml`
- `configs/config.example.yaml`
  - 增加双语/运维注释与无秘密示例。
- `internal/config/providers_test.go`
  - 兼容、规范化、重复 ID、无效 URL、认证与地址 opt-in 测试。

### 9.2 SafeTransport

- `internal/safetransport/transport.go`
  - 保持 Provider target policy 行为；
  - 抽出可复用的“解析全部、全部校验、固定拨一个”逻辑；
  - target 与 proxy endpoint 使用不同 policy 类型；
  - `Proxy` 永远为 nil。
- 新增 `internal/safetransport/http_connect.go`
  - 代理 endpoint 固定拨号、可选代理 TLS、CONNECT、受限响应读取、认证与错误类型。
- `internal/safetransport/transport_test.go`
- 新增 `internal/safetransport/http_connect_test.go`
  - 覆盖 §11 的安全矩阵。

### 9.3 Domain、Store 与 app

- `internal/domain/models.go`
  - `ProviderInstance.EgressProxyID` 与语法校验。
- Bolt store
  - JSON 零值兼容，不做迁移；增加 round-trip/旧记录测试。
- `internal/app/runtime.go`
  - 启动时构建只读 egress registry。
- `internal/app/providers.go`
  - `newBindingClient` 选择 direct/代理 connector；
  - 缺失引用进入明确 exclusion/withholding；
  - Bedrock control plane 使用同一路径。
- `internal/app/admin_providers.go`
  - 输入、验证、返回、审计、连接测试诊断。
- Admin router/resource registration
  - 增加只读代理列表端点。
- `internal/provider/provider.go`
  - 识别代理握手阶段的 unsent/retryable 语义。

### 9.4 Web

- `web/src/types.ts`
  - Provider 与代理描述类型。
- `web/src/api.ts`
  - 获取已批准代理。
- `web/src/pages/ProvidersPage.tsx`
  - 代理选择、缺失配置、详情展示、快捷启停字段保留。
- 对应 i18n 与 focused tests。

### 9.5 文档

实现时同步更新：

- `docs/architecture/threat-model.md`
  - SSRF 控制改为“SafeTransport target pinning + explicit pinned CONNECT”；
- 运维配置/部署文档
  - Docker 到宿主机 Mihomo 的地址、端口与防火墙示例；
  - fake-IP 必须返回真实 Provider IP 的要求；
  - 重启生效与回滚步骤。

## 10. 分阶段实施

### Phase 0：安全契约与失败语义

- 固化本文件 §4 的不变量；
- 先写 target/proxy 两套 policy 的反向测试；
- 定义 unsent、retryable、ambiguous 的代理错误语义；
- 确认 v1 只支持 HTTP CONNECT。

完成标准：删掉任一 target 校验或把 CONNECT 改回 hostname，测试必须失败。

### Phase 1：配置与代理注册表

- 增加 `providers.egress_proxies`；
- 严格解析代理 endpoint 与认证文件；
- Runtime 构建不可变代理注册表；
- 保证旧配置、空配置仍启动且全部直连。

完成标准：定义代理本身不会改变任何 Provider 路径。

### Phase 2：SafeTransport HTTP CONNECT

- 实现代理端点固定拨号；
- CONNECT 到已验证 target IP；
- 保留原目标 TLS/SNI/HTTP2；
- 加入结构化安全错误与 header/timeout 上限。

完成标准：测试代理看到的是字面 IP，Provider TLS 看到的是原 hostname，环境代理仍无效。

### Phase 3：Provider 引用与运行时激活

- Provider 模型/API 增加 `egress_proxy_id`；
- `newBindingClient` 接入 connector；
- 所有 Binding、Bedrock 控制面、连接测试统一路径；
- 配置缺失 fail closed、路由 withholding 可见；
- 代理变更通过现有 topology activation 原子生效。

完成标准：代理失效不会产生一次直连；直连 Provider 不会触碰代理端口。

### Phase 4：Admin UI、诊断与文档

- 代理列表端点和 Provider 表单；
- 列表、详情、连接测试显示安全诊断；
- 日志/指标不泄密且 label 有界；
- 更新 threat model 与部署文档。

### Phase 5：开发环境灰度

1. 在 dev 配置定义 `mihomo-aws`，但不绑定 Provider；
2. 重启 Halro，确认既有 Provider 行为不变；
3. 只把一个非关键 Provider 切到代理；
4. 先运行 Halro connection test；
5. 再由人工授权执行一次计费 Provider smoke；
6. 核对 Halro 容器出口 IP、代理日志、Halro request ID 和上游 request ID；
7. 观察 breaker、连接池、流式响应和模型枚举；
8. 再逐个迁移其他 Provider。

回滚只需把 Provider 切回 direct；如管理面不可用，则恢复数据库前不是首选，优先恢复代理配置并重启，使原引用重新可解析。

## 11. 测试矩阵

### 11.1 SafeTransport 单元测试

- direct 与 proxy 模式的 `Transport.Proxy` 都为 nil；
- 设置全部常见代理环境变量仍不改变路径；
- Provider target 在拨代理前完成 allowlist 与 DNS 校验；
- mixed public/private target DNS 整体拒绝，代理未被拨；
- `198.18.0.0/15` target 拒绝，代理未被拨；
- CONNECT authority 是已验证 IP，不含 Provider hostname；
- Provider TLS SNI/证书校验仍使用原 hostname；
- proxy hostname 的全部地址先校验再固定拨号；
- private、loopback proxy opt-in 相互独立；
- 元数据和保留地址在所有 opt-in 下仍拒绝；
- 代理 URL 的 userinfo/path/query/fragment 拒绝；
- CONNECT redirect、407、403、502、超时、截断状态行、超大响应头拒绝；
- Basic auth 只到代理，不到 Provider；
- 错误、日志和格式化输出不含认证 canary；
- CONNECT 前失败被 `provider.Unsent` 识别；
- CONNECT 成功后的失败继续按现有保守规则处理；
- context cancel、deadline 和 connection close 不泄漏 goroutine/连接。

### 11.2 app 集成测试

- 旧 Provider JSON 读取为 direct；
- create/update/enable/disable round-trip 保留 `egress_proxy_id`；
- 未知代理 ID 返回稳定 400；
- 配置删除后 Provider 被排除且不直连；
- 代理选择变化重建 Registry，旧 adapter 排空并关闭；
- 同一 Provider 的全部 Binding 使用同一 connector；
- Bedrock Runtime 与派生 control-plane host 都使用同一 connector；
- Provider connection test、模型枚举、能力检测和 Gateway 请求没有绕过；
- 407 为 non-retryable unsent，502/超时为 retryable unsent；
- Gateway 公共错误不暴露 endpoint/credential；
- Admin 测试响应只返回受控 stage/status；
- 审计记录 direct ↔ proxy 和代理 ID 变化，不记录秘密。

### 11.3 前端 focused tests

- 默认选择 direct；
- 编辑时回显代理；
- 快捷启停不会清空代理；
- 缺失代理 ID 显示错误且不会自动切 direct；
- API 错误码本地化；
- read-only Admin 能查看但不能修改。

### 11.4 最终 gate

迭代中按仓库策略运行最小覆盖：

```bash
go test -count=1 ./internal/safetransport/
go test -count=1 ./internal/config/
go test -count=1 ./internal/domain/
go test -count=1 ./internal/app/ -run 'Provider|Egress|Bedrock'
cd web && npx vitest run src/pages/ProvidersPage.test.tsx
```

涉及并发 Registry/connection 生命周期时，对受影响包补 `-race`。发布前只运行一次完整 Go、frontend 和 embedded bundle gate。自动测试不调用真实 Provider；真实 smoke 必须再次取得人工授权。

## 12. 运维注意事项

### 12.1 Docker/Swarm 到宿主机 Mihomo

- 容器内的 `127.0.0.1` 指向容器自身，不是宿主机；
- 应使用明确的宿主机 gateway、同 overlay network 的代理服务名，或经评审的 host networking；
- Mihomo 只监听所需接口，宿主防火墙只允许 Halro 所在 Docker 网段访问代理端口；
- 若配置 `172.18.0.1` 等地址，必须设置 `allow_private_endpoint: true`；
- 不要为了访问本地代理而放宽 Provider target 的 private endpoint policy。

### 12.2 DNS

部署前先在 Halro 容器内确认 Provider hostname 返回真实地址，而不是 `198.18.0.0/15`：

```bash
getent ahosts bedrock-mantle.us-east-2.api.aws
```

这项检查通过后，CONNECT 代理才能在不牺牲 DNS 固定的前提下工作。若仍是 fake IP，应修改 Mihomo DNS 策略；不要修改 Halro 的保留地址拒绝表。

### 12.3 出口验证

“代理端口可连”不等于“Provider 流量走了代理”。灰度时至少核对：

- Halro connection test 成功；
- Mihomo 记录到目标字面 IP 的 CONNECT；
- Provider 请求拿到上游 request ID；
- 宿主/容器出口 IP 符合预期；
- 停止代理后 Provider 失败且没有直连流量。

## 13. 验收标准

全部满足才可认为改造完成：

- [ ] 旧配置、旧数据库升级后所有 Provider 保持 direct；
- [ ] `http.Transport.Proxy` 在所有 Provider Client 中仍为 nil；
- [ ] 代理模式 CONNECT 的目标是 SafeTransport 本次验证的字面 IP；
- [ ] Provider hostname allowlist、全量 DNS 地址校验、固定拨号和 redirect 拒绝仍生效；
- [ ] private/loopback proxy 权限与 private Provider 权限完全分离；
- [ ] 环境变量、PAC、系统代理不能改变出站路径；
- [ ] 引用缺失、认证失败、代理不可达时绝不直连；
- [ ] Bedrock 数据面与派生控制面走同一出站路径；
- [ ] Provider 测试、模型枚举、能力检测和真实 Gateway 请求路径一致；
- [ ] 代理握手失败被正确标为 unsent，重试和计费语义正确；
- [ ] Provider TLS 仍由 Halro 以原 hostname 端到端验证；
- [ ] Admin API、日志、审计、指标和 UI 不泄露代理凭据；
- [ ] fake-IP 返回 `198.18.0.0/15` 时继续 fail closed；
- [ ] 自动测试无真实 Provider 消费，最终全量 gate 只在发布前执行一次。

## 14. 后续可选演进

以下项目不应阻塞 v1，也不能以未评审方式顺带加入：

1. 受控 DoH/DoT Resolver，解决不能调整系统 fake-IP DNS 的部署；
2. SOCKS5（只允许传已验证字面 IP，明确不支持 remote DNS）；
3. 代理凭据无重启原子轮换；
4. 多代理 failover，但仍禁止自动 direct fallback；
5. 共享代理健康状态/熔断，避免多个 Provider 对同一故障代理重复探测；
6. mTLS HTTPS proxy；
7. 代理选择的策略化批量迁移和 dry-run。

这些演进都必须继续满足本文件 §4 的不变量。
