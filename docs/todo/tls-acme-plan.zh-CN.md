# Halro TLS 部署形态与证书热重载方案

状态：**已实施**（§6 全部落地；§7 的内置 ACME 维持不做）
更新日期：2026-08-19
范围：`internal/app/runtime.go`、`internal/config`、`cmd/halro/main.go`、`deploy/observability/`、`docs/guides/operator-guide.md`、`README.md`

---

## 0. 决策

起因是部署侧的两个问题：Halro 在非回环监听时强制 TLS，而很多环境手上没有证书；以及把 HTTPS 关掉改用 HTTP 会有什么后果。

评估后的决定：

1. **只支持两种部署形态**，并把它们写成一等文档：
   - **形态 A** —— Halro 自己终止 TLS（§4）
   - **形态 B** —— 反向代理终止 TLS，Halro 全绑回环（§5，**推荐**）
   「关闭 HTTPS 用明文对外」不是第三种形态，配置校验会直接拒绝启动（§2）。
2. **实施 `SIGHUP` 热重载**（§6）。这是形态 A 的实际痛点：当前换证必须重启进程，而单写者架构下重启即全量停机。重载范围是一份封闭清单 —— 证书与私钥、Metrics 的 client CA、日志级别、日志文件句柄；授权语义相关配置明确排除（§6.3）。
   同时**支持多证书按 SNI 选择**，Gateway 与 Admin 可用不同主机名，不再要求一张证书覆盖全部名字。
3. **不做内置 ACME**（§7）。技术上可行且性能开销可忽略，但会把 CA 可达性、DNS 正确性、速率配额拉进网关自身的失败域，并新增一类长期机密与一个公网监听器。反向代理（Caddy / Traefik）在进程外解决同一问题，且失败不波及网关。重评条件见 §7.4。

端点路径与客户端接入方式与形态选择无关，单列为 §3。

本方案**不需要**重新初始化数据目录，但**需要修改 `config.yaml`**：`tls.cert_file` / `tls.key_file` 两个标量被 `tls.certificates` 列表取代（§6.5）。按 pre-1.0 的规矩旧键不保留兼容读法。

---

## 1. 当前基线：HTTPS 在哪些地方被强制

以下每条都是实际代码，不是设计意图的转述。

### 1.1 监听器校验

`internal/config/config.go:1144` 的 `validateListener`：

```go
if tlsEnabled || listenerHostIsLoopback(host) {
    return nil
}
if allowInsecurePublic && name == "server.gateway_listen" {
    return nil
}
return []error{fmt.Errorf("%s must bind loopback unless TLS is enabled", name)}
```

规则是「非回环 ⇒ 必须 TLS」，唯一逃生口是 `-allow-insecure-public-listen`，且只覆盖 `server.gateway_listen`。

| 监听器 | 默认值 | 关闭 TLS 后能否绑非回环 |
| --- | --- | --- |
| `server.gateway_listen` | `127.0.0.1:8080` | 仅在显式传入 `-allow-insecure-public-listen` 时可以 |
| `server.admin_listen` | `127.0.0.1:8081` | 不能，无 override |
| `server.metrics_listen` | `127.0.0.1:9090` | 不能；非回环还额外要求 `metrics.tls` 双向认证（`config.go:751`） |

默认值见 `internal/config/default.yaml:17-19`。

### 1.2 Admin 会话 Cookie 硬编码 `Secure`

`internal/app/admin_session.go:361` 与 `:369`：

```go
Name: adminSessionCookie, Value: token, Path: "/", Secure: true,
HttpOnly: true, SameSite: http.SameSiteStrictMode, ...
```

`Secure` 是常量而非配置。浏览器在明文 `http://` 源上会丢弃该 Cookie（`http://localhost` 是现代浏览器的 secure context 例外）。「公网 HTTP + 域名」访问控制台的结果是登录后立即掉登录态。

### 1.3 Admin 同源判定依赖 `request.TLS`

`internal/app/admin_session.go:375` 的 `adminSameOrigin`：

```go
expected := r.config.Admin.ExternalOrigin
if expected == "" {
    scheme := "http"
    if request.TLS != nil {
        scheme = "https"
    }
    expected = scheme + "://" + request.Host
}
```

**这条对形态 B 是硬性要求**：代理终止 TLS 后 Halro 收到明文连接，`request.TLS == nil`，推导出的期望源是 `http://host`，而浏览器发来的 `Origin` 是 `https://...`，两者不等，**所有 Admin 变更请求都会被判为跨源拒绝**。必须显式配置 `admin.external_origin`。

该字段本身被强制为 https（`config.go:940-947`）。

### 1.4 其他强制点

- `audit.anchor.sink: dead_man_pull` 要求 `metrics.tls.enabled`（`config.go:798`）：锚点流不得明文提供。
- `storage.master_key` 的 KMS `endpoint` 必须是 HTTPS 源（`config.go:1081`）。
- 出站方向由 `internal/safetransport` 统一强制 HTTPS，与入站监听形态无关。

### 1.5 服务端 TLS 的加载方式（本方案要改的地方）

`internal/app/runtime.go:1041-1047`：

```go
if item.name == "metrics" && r.config.Metrics.TLS.Enabled {
    err = item.server.Serve(tls.NewListener(item.listener, metricsTLSConfig.Clone()))
} else if r.config.TLS.Enabled {
    err = item.server.ServeTLS(item.listener, r.config.TLS.CertFile, r.config.TLS.KeyFile)
} else {
    err = item.server.Serve(item.listener)
}
```

Metrics 的 `tls.Config` 由 `runtime.go:1134` 的 `metricsTLSConfig()` 构造，同样是启动期一次性 `tls.LoadX509KeyPair` 加上一次性读取 client CA。

**改造前两条路径都没有 `tls.Config.GetCertificate` 回调，都不支持证书热重载，换证必须重启进程。** §6 的实施把这两处都换成了可原子替换的持有者。

---

## 2. 为什么只有两种形态

把 HTTPS 换成明文 HTTP 的后果，按严重程度：

| 影响 | 触发条件 | 表现 |
| --- | --- | --- |
| 进程拒绝启动 | 关闭 TLS 且任一监听器绑非回环 | `server.admin_listen must bind loopback unless TLS is enabled`，fail-closed，不是降级 |
| 控制台无法保持登录 | 公网 HTTP + 域名访问 Admin | Cookie 被浏览器丢弃，登录后立即掉登录态 |
| 配置校验失败 | `admin.external_origin: http://...` | 启动中止 |
| 审计锚点不可用 | `audit.anchor.sink: dead_man_pull` | 启动中止 |
| **凭据与内容明文暴露** | Gateway 明文对外 | `Authorization: Bearer gw_...`、全部 prompt 与响应正文在链路上可读可改 |

最后一条是本质问题而非配置问题：Gateway Key 一旦被链路中间人读到，等于取得该 Project 的全部额度与模型访问权。

`-allow-insecure-public-listen` 的设计意图是「宿主机本地边界」（如 `docker run -p 127.0.0.1:8080:8080`），文档已明确它不是公网部署选项（`docs/guides/operator-guide.md:594-600`）。**它不构成第三种生产形态。**

---

## 3. 端点与客户端接入（两种形态共用）

路径由代码决定，与部署形态无关。**不存在 `/api` 前缀** —— Gateway 的路径与 OpenAI / Anthropic 官方 API 一致，这正是「协议兼容」的含义：SDK 不改代码，只改 base URL。

### 3.1 路径清单

**Gateway 监听器**（`internal/app/runtime.go:1290` 的 `gatewayRouter`）：

| 协议面 | 路径 |
| --- | --- |
| OpenAI 兼容 | `POST /v1/chat/completions`、`/v1/responses`、`/v1/embeddings`、`/v1/moderations`、`/v1/images/generations`、`/v1/audio/speech`、`/v1/audio/transcriptions`、`/v1/rerank` |
| OpenAI 兼容（异步与批量） | `/v1/files`、`/v1/batches`、`/v1/async/invocations` 及其 GET / cancel 子路径 |
| Anthropic 兼容 | `POST /v1/messages`、`POST /v1/messages/count_tokens` |
| 探活 | `GET /health/live`、`GET /health/ready` |
| 版本信息 | `GET /` |

**Admin 监听器**（`runtime.go:1344` 的 `adminRouter`）：

| 用途 | 路径 |
| --- | --- |
| 控制台界面 | `/admin`、`/admin/*`（`runtime.go:1474-1476`，由 `internal/webui` 提供） |
| Admin API | `/admin/api/v1/*` |
| 探活 | `GET /health/live`、`GET /health/ready` |

**Metrics 监听器**（`runtime.go:1494` 的 `metricsRouter`）：`GET /metrics`、`GET /health/live`，以及启用 dead-man 锚点时的 `GET /audit/anchors`。

三者的路径前缀互不重叠（`/v1/*` 与 `/admin*`），这使得单域名按路径拆分成为可能（§5.2）。唯一重叠的是 `/health/*`，代理配置时需要显式决定它指向哪个监听器。

### 3.2 客户端配置

两个 SDK 对 base URL 的约定不同 —— OpenAI SDK 要求 base URL 含 `/v1`，Anthropic SDK 不含：

```python
# OpenAI SDK：SDK 自己拼 /chat/completions
client = OpenAI(
    base_url="https://halro.example.com:8080/v1",
    api_key="gw_...",
)
```

```python
# Anthropic SDK：SDK 自己拼 /v1/messages
client = Anthropic(
    base_url="https://halro.example.com:8080",
    api_key="gw_...",
)
```

```bash
curl https://halro.example.com:8080/v1/chat/completions \
  -H "Authorization: Bearer gw_..." \
  -H "Content-Type: application/json" \
  -d '{"model":"<公开别名>","messages":[{"role":"user","content":"hi"}]}'
```

`model` 填 Project 允许的**公开别名**，不是上游模型标识符。应用只持有 Gateway Key 与别名，永远接触不到 Provider 凭据。

现有文档示例使用 `http://127.0.0.1:8080/v1/...`（`README.md:53`、`docs/guides/user-guide.zh-CN.md:224`），换成对外地址即可。

---

## 4. 形态 A：Halro 自己终止 TLS

### 4.1 配置

```yaml
tls:
  enabled: true
  # 列表形态。单证书部署写一个元素；Gateway 与 Admin 用不同主机名时
  # 写多个元素，握手按 SNI 选择（§6.2）。
  certificates:
    - cert_file: /abs/path/fullchain.pem
      key_file: /abs/path/privkey.pem

server:
  gateway_listen: 0.0.0.0:8080
  admin_listen: 0.0.0.0:8081
  metrics_listen: 127.0.0.1:9090

admin:
  # 必须带上 Admin 的对外端口：浏览器发出的 Origin 在非默认端口时携带端口，
  # adminSameOrigin 做的是 scheme://host 的整串相等比较（§1.3）。
  external_origin: https://halro.example.com:8081

security:
  trust_proxy_headers: false
```

### 4.2 要点

- 三个监听器可绑对外地址，由 TLS 保护。Metrics 若要非回环，还需 `metrics.tls` 的双向认证（`config.go:751`），通常保持回环更简单。
- **Gateway 不需要配置域名。** 全仓库只有 `admin.external_origin` 一个「对外身份」配置项，不存在 Gateway 的 public URL / base URL 设置。原因见 §4.4。
- 当前实现（改造前）里 `tls.cert_file` 是**全局一份**，Gateway 与 Admin 共用同一张证书（`runtime.go:1045` 对两个监听器传入同一对文件），两者用不同主机名时 SAN 必须同时覆盖。§6 的改造用 `tls.certificates` 列表加 SNI 选择解除这个限制。
- **不要**开 `trust_proxy_headers`。客户端 peer 地址是真实公网 IP，不落在任何可信 CIDR 内，`internal/gatewayapi/handler.go:807` 会忽略 `X-Forwarded-For` 并采用 peer 地址。开了不危险，但属于噪音配置，且掩盖真实拓扑。
- 客户端 base URL 见 §3.2；形态 A 下带 Gateway 端口，例如 OpenAI SDK 用 `https://halro.example.com:8080/v1`。若不希望对外出现端口号，用形态 B 的路径拆分（§5.2）。
- 证书由外部工具签发（certbot、acme.sh、企业 CA），Halro 只负责加载。
- 换证目前需重启进程 —— **这正是 §6 要解决的问题**。

### 4.3 为什么 Gateway 不需要域名配置

Admin 需要 `external_origin`，Gateway 不需要，原因是两者的信任模型不同：

| | Admin | Gateway |
| --- | --- | --- |
| 认证方式 | 会话 Cookie + CSRF + TOTP | `Authorization: Bearer gw_...` |
| 是否受浏览器同源策略约束 | 是 | 否 |
| 是否需要知道自己的对外身份 | 是（`adminSameOrigin` 要拿它比对 `Origin`/`Referer`） | 否 |

Gateway 是纯 API：调用方是 SDK 与服务端进程，不带 Cookie，没有 `Origin` 头可校验，也没有任何代码路径需要拼出「自己的公网地址」。`server.gateway_listen` 是**绑定地址**而非身份声明。

域名对 Gateway 只在一个层面有意义 —— **证书**。客户端把 base URL 指向 `https://halro.example.com:8080`，TLS 握手时校验证书 SAN 是否覆盖该主机名。这由证书本身承载，不需要 Halro 配置里再写一遍。

唯一会拼 URL 的地方是 Admin 控制台的开发者工作台（`internal/app/admin_developer.go:155-167`），它从请求的 `Host` 加 `r.config.TLS.Enabled` 推导，同样不需要额外配置。

### 4.4 副作用：配置 `external_origin` 会强制启用 setup token

`internal/app/admin_setup.go:176-179` 的 `setupRequiresToken`：

```go
func setupRequiresToken(cfg config.Config) bool {
	if cfg.Admin.ExternalOrigin != "" {
		return true
	}
	...
}
```

只要设了 `admin.external_origin`，首次初始化就**必须**凭 setup token 完成，不再走「回环地址免令牌」的快捷路径。形态 A 与形态 B 都会命中这条。这是正确的行为（对外可达的实例本就不该免令牌初始化），但首次部署要预先知道，否则会以为控制台坏了。

### 4.5 适用场景

单机部署、不希望多跑一个进程、已有成熟的证书分发流程（配置管理下发 + 定期轮换）。

---

## 5. 形态 B：反向代理终止 TLS（推荐）

### 5.1 配置

```yaml
tls:
  enabled: false

server:
  gateway_listen: 127.0.0.1:8080
  admin_listen: 127.0.0.1:8081
  metrics_listen: 127.0.0.1:9090

admin:
  external_origin: https://halro.example.com

security:
  trust_proxy_headers: true
  trusted_proxy_cidrs: ["127.0.0.1/32"]
```

### 5.2 代理拓扑：两个监听器如何共用对外入口

Gateway 与 Admin 是两个监听器，对外要么拆端口、要么拆路径、要么拆子域名。因为路径前缀不重叠（§3.1），**推荐单域名按路径拆分**，对外只暴露 443，不出现端口号。

**方案一：单域名路径拆分（推荐）**

```
halro.example.com {
	# 探活显式指向 Gateway：两个监听器都有 /health/*，不指定会落到下面的 catch-all
	handle /health/* {
		reverse_proxy 127.0.0.1:8080
	}
	handle /v1/* {
		reverse_proxy 127.0.0.1:8080
	}
	handle {
		reverse_proxy 127.0.0.1:8081
	}
}
```

对外结果：

| | URL |
| --- | --- |
| Gateway | `https://halro.example.com/v1/chat/completions` |
| 控制台 | `https://halro.example.com/admin` |
| `admin.external_origin` | `https://halro.example.com`（443 为默认端口，不写端口） |

客户端 base URL 相应去掉端口：OpenAI SDK 用 `https://halro.example.com/v1`，Anthropic SDK 用 `https://halro.example.com`。

**方案二：子域名拆分**

`api.example.com` → `127.0.0.1:8080`，`console.example.com` → `127.0.0.1:8081`，此时 `admin.external_origin: https://console.example.com`。适合希望在网络层就把数据面与控制面分开施加不同访问控制的场景（例如控制台加 IP 允许列表或额外的 SSO 前置）。

**方案三：只代理 Gateway**，Admin 保持本机回环、由运维通过 SSH 端口转发访问。控制面完全不对外，安全性最高，代价是控制台不能随手打开。此时 `admin.external_origin` 留空即可 —— 回环访问会走 §4.4 的免令牌路径。

### 5.3 要点

- `admin.external_origin` **必填**，否则 Admin 全部变更被拒（§1.3）。这是形态 B 最容易踩且最难自查的坑：表现是控制台报跨源错误，而配置校验不会提示。
- 该值要与浏览器地址栏完全一致。代理在 443 上时不写端口（`https://halro.example.com`）；代理在非默认端口时必须带端口（`https://halro.example.com:8443`）—— 比对的是 `scheme://host` 整串。
- 与形态 A 相同，Gateway 侧不需要任何域名配置（§4.3）；同样会触发 setup token 强制要求（§4.4）。
- `trusted_proxy_cidrs` 必须写代理实际发起连接的源地址。容器网络中代理通常不在 `127.0.0.1` —— 写错等于 CIDR 授权与 Token Guard 拿到的是代理地址而不是客户端地址，**静默失效，不报错**。
- 开启 `trust_proxy_headers` 后，来自可信代理的每个请求**必须**携带语法合法的 `X-Forwarded-For`，缺失或畸形一律 HTTP 400（`internal/gatewayapi/handler.go:671` 与 `:795`）。代理必须正确设置该头。
- 代理需支持 SSE：关闭响应缓冲，读写超时要长于最长流式响应。Caddy 默认可用；nginx 需 `proxy_buffering off;` 与 `proxy_read_timeout`。
- Admin 与 Metrics 是否经代理暴露单独决策。默认建议 Admin 仅经代理并叠加额外网络控制，Metrics 保持回环由本机抓取。
- 代理不得改写 `/v1/*` 路径（不要 strip prefix）：Halro 与 SDK 双方都按官方 API 的绝对路径工作，改写会直接 404。
- 控制台的 CSP 是 `connect-src 'self'`（`runtime.go:1483`），因此控制台页面与 Admin API 必须落在同一个对外源上。方案一、二都满足；不要把 `/admin` 与 `/admin/api` 拆到不同域名。

### 5.4 适用场景

**默认推荐。** 证书生命周期完全在 Halro 进程之外：续期失败不拖累 Gateway，换证不重启 Halro，CA 不可达不影响计费与路由。已有 Ingress / 网关的环境更是零额外成本。

`docs/guides/operator-guide.md:67` 已把这一形态列为推荐做法，本节是它的可执行版本。

---

## 6. 证书热重载（实施项）

### 6.1 目标与非目标

**目标**：向运行中的进程发送 `SIGHUP` 后，重新读取一份**明确列举**的可重载材料（证书与私钥、Metrics 的 client CA、日志级别、日志文件句柄），无需重启即可生效，且不中断在途连接。

**目标（多证书）**：Gateway 与 Admin 使用不同主机名时，按 SNI 选择证书，不再要求一张证书的 SAN 覆盖全部名字。

**非目标**：自动发现文件变化（不做 inotify / 轮询）、自动申请证书（见 §7）、变更监听地址或端口（属于重启范畴）、重载授权语义相关配置（理由见 §6.3）。

### 6.2 设计

**触发方式：`SIGHUP`。**

理由：显式、幂等、与既有运维工具链一致（systemd `ExecReload=/bin/kill -HUP $MAINPID`，certbot / acme.sh 的 `--deploy-hook`）。不选文件监听，是因为证书与私钥是两个文件，写入之间存在窗口，监听会读到半更新状态。不选 Admin API 端点，是因为证书出问题时恰恰可能登不上控制台，且会无谓扩大 Admin 变更面。

`syscall.SIGHUP` 在 Windows 上有定义但永不投递，因此无需构建标签；文档需写明该平台只能重启换证。

**证书持有者（多证书 + SNI）**：

配置形状随之改变。按 pre-1.0 的原位修正规矩，标量 `cert_file` / `key_file` 被**替换**为列表，而不是在旁边并列一组新键：

```yaml
tls:
  enabled: true
  certificates:
    - cert_file: /abs/path/api.fullchain.pem
      key_file: /abs/path/api.privkey.pem
    - cert_file: /abs/path/console.fullchain.pem
      key_file: /abs/path/console.privkey.pem
```

单证书部署写成一个元素的列表。`metrics.tls` 保持单证书 —— 它是一个固定名字的 mTLS 抓取端点，没有多名字场景，不做无谓泛化。

```go
// 一次发布的全部内容，整体不可变。
type certificateBundle struct {
	byName  map[string]*tls.Certificate // 精确名与通配名（键为小写）
	fallback *tls.Certificate           // 列表第一项，供无 SNI 的连接使用
}

type certificateHolder struct {
	sources []certificateSource // {certFile, keyFile}，顺序即配置顺序
	current atomic.Pointer[certificateBundle]
}

func (h *certificateHolder) reload() error {
	bundle, err := buildBundle(h.sources) // 全部加载并建索引；任一失败即整体失败
	if err != nil {
		return err                        // 不 Store，旧 bundle 继续服务
	}
	h.current.Store(bundle)               // 单次原子发布
	return nil
}

func (h *certificateHolder) get(hello *tls.ClientHelloInfo) (*tls.Certificate, error) {
	bundle := h.current.Load()
	name := strings.ToLower(strings.TrimSuffix(hello.ServerName, "."))
	if name == "" {
		return bundle.fallback, nil       // 按 IP 直连、探活、旧客户端
	}
	if certificate, ok := bundle.byName[name]; ok {
		return certificate, nil
	}
	if index := strings.IndexByte(name, '.'); index >= 0 {
		if certificate, ok := bundle.byName["*"+name[index:]]; ok {
			return certificate, nil
		}
	}
	return bundle.fallback, nil           // 见 §6.6 对未匹配 SNI 的取舍
}
```

`buildBundle` 在加载时校验：每张证书能解析、私钥匹配、SAN 非空；**同一个名字被两张证书声明即拒绝整份 bundle** —— 歧义的 SNI 映射比缺证书更难排查。索引在重载时一次建好，握手路径只做 map 查找，不做字符串解析以外的工作。

**Metrics 持有者（证书与 client CA 一次发布）**：

Metrics 侧改用 `GetConfigForClient` 返回整份 `tls.Config`（含 `Certificates` 与 `ClientCAs`），持有者内部同样是一次 `atomic.Pointer` 发布。证书与 CA 池**共用同一次 `SIGHUP`、同一次发布**：两者要么一起生效，要么一起不动，不存在「新证书配旧 CA」的中间态。

Go 在每次握手时调用一次 `GetConfigForClient` 并使用返回的配置完成该连接，因此已建立的连接不受后续替换影响，替换瞬间也不会出现半新半旧的组合。

真正的风险不在代码而在轮换顺序：若先把 client CA 换成只含新 CA，尚未换证书的抓取端会立刻被拒。文档需写明标准的两阶段轮换 —— **先把新旧 CA 拼进同一个 PEM 并 `SIGHUP`，再逐个更换抓取端的客户端证书，最后移除旧 CA 再 `SIGHUP` 一次**。

**并发正确性**：读路径是一次 `atomic.Pointer.Load`，写路径是全部解析成功后的一次 `Store`。不存在部分更新的中间态：解析失败时根本不 `Store`。Gateway/Admin 与 Metrics 是两个独立持有者，一个重载失败不影响另一个。

**性能**：证书查找发生在 TLS 握手而非每个请求。改动后每次握手一次原子指针读取，比当前 `cfg.Certificates` 切片的 SNI 匹配更少，与握手本身的 ECDSA/RSA 开销相比可忽略。此项不引入任何请求路径开销。

### 6.3 `SIGHUP` 的重载范围

`SIGHUP` 重载的内容必须是一份**封闭的清单**，而不是「重新读一遍配置文件」。开放式重载会让「运行中的语义」与「磁盘上的 config.yaml」之间的关系变得不可推理，这比省下的一次重启贵得多。

**可重载（允许清单）**

| 项 | 机制 | 请求路径开销 |
| --- | --- | --- |
| `tls.certificates` 的**文件内容** | `atomic.Pointer[certificateBundle]` 一次发布 | 握手时一次原子读 + 一次 map 查找 |
| `metrics.tls` 的证书与 client CA **文件内容** | `GetConfigForClient` 返回整份配置，一次发布 | 握手时一次原子读 |
| `logging.level` | `slog.HandlerOptions.Level` 由固定 `slog.Level` 改为 `*slog.LevelVar` | 每条日志一次原子读；slog 本就要做 `Enabled` 判定，量级不变 |
| 日志文件句柄（重开） | `internal/logging.Sink` 增加 `Reopen()`，在既有写锁内换文件 | 无（写日志本就持该锁） |

日志文件重开是 `SIGHUP` 的传统语义，配合外部 `logrotate` 使用：logrotate 改名后发 `SIGHUP`，进程重开新文件。当前 `Sink` 只有 `Close`（`internal/logging/sink.go:181`），需要补 `Reopen`。

注意允许清单里全部是**文件内容或标量**，不含**路径与拓扑**：改 `tls.certificates` 的文件路径、日志文件路径、监听地址，仍然要重启。

**不可重载（排除清单，及理由）**

| 项 | 为什么排除 |
| --- | --- |
| `server.*_listen` | 需要重新 bind，本质是重启 |
| `server.read_header_timeout` 等超时 | 值挂在运行中的 `http.Server` 结构体字段上，就地修改是数据竞争 |
| `security.trust_proxy_headers`、`trusted_proxy_cidrs` | 已构造进 `gatewayapi.Handler`（`handler.go:365-366`）。技术上可以原子发布，但它直接决定**谁的 `X-Forwarded-For` 被相信** —— 授权边界的变更应当是显式的、留下审计的，不该藏在一个信号里 |
| `admin.external_origin` | 同上，是 CSRF / 同源边界 |
| `storage.*`、`master_key.*`、`data_dir` | 单写者与密钥边界，任何运行时变更都可能破坏一致性前提 |
| `audit.*`、`metrics.require_auth` | 安全可观测面的开关，同样应当显式重启并留痕 |

判定规则一句话：**`SIGHUP` 只重载「材料」，不重载「语义」。** 材料指证书、CA、日志目的地这类可以换掉而不改变谁被允许做什么的东西。

**性能影响：可忽略，且不是取舍所在。**

- **重载路径**本身是运维触发的稀有事件，代价是几次文件读取加解析，几十毫秒量级，完全不在请求路径上。
- **请求路径**新增的开销只有上表右列那几个原子读，纳秒量级。TLS 那一项改动后**比现状更省** —— 现在每次握手是在 `cfg.Certificates` 上做 SNI 切片扫描，改成 map 查找后更快。
- 真正的成本不是 CPU，而是**推理成本**：一旦某个值可以在运行中变化，读到它的每一处都不能再假设它自启动起恒定。这正是允许清单必须短的原因，也是排除清单里那几项即使「技术上能做」也不做的原因。

**可观测性要求（因为配置不再必然等于磁盘内容）**：`/admin/api/v1/system/config` 与 `/admin/api/v1/system/status` 必须能报告**当前生效值**与最后一次成功重载的时间戳，而不是重新读一遍 config.yaml 后当作现状汇报。否则排障时会拿着磁盘上的文件解释一个并不在运行的配置。

**`halro doctor` 不在此列。** 它要求独占离线访问（`internal/app/doctor.go:79` 的 `lock.AcquireExistingReadOnly`），实例在跑时拿不到锁，因此无法报告运行中进程的实时状态。它能做的是**离线检查证书文件本身** —— 用与监听器相同的构建函数加载配置里的 keypair，报出名字与剩余有效期。该检查放在取锁之前，所以在实例运行时执行 `halro doctor` 仍会得到这一项结果（随后在 `data_lock` 处失败）。

### 6.4 已验证的设计前提

以下用独立探针程序在 Go 1.26.6 上实测，不是从文档推断：

| 前提 | 结果 |
| --- | --- |
| `ServeTLS(l, "", "")` 配合 `TLSConfig.GetCertificate` 能正常提供服务 | 通过 |
| `tls.LoadX509KeyPair` 返回的 `Certificate.Leaf` 已被填充 | 通过（可直接读 `NotAfter`，无需二次解析 DER） |
| 运行中替换 `atomic.Pointer` 后，新连接拿到新证书（指纹变化） | 通过 |
| 改动后 HTTP/2 仍然协商成功 | 通过（`ServeTLS` 与 `Serve(tls.NewListener(...))` 两条路径均为 `HTTP/2.0`） |
| 多证书按 SNI 选择（精确名与 `*.` 通配名） | 通过，三个不同 SNI 各自拿到对应证书 |
| `GetCertificate` 返回 error 时握手的表现 | 客户端收到 `remote error: tls: internal error` —— Go 无法发送 `unrecognized_name` 告警，见 §6.6 的取舍 |

HTTP/2 那条是重点：`net/http` 在 `Serve` 与 `ServeTLS` 两条路径上都会自动完成 HTTP/2 装配，因此本改动**不会**退化成 HTTP/1.1。实施时仍需在仓库内补一个等价断言，防止后续改动回退。

### 6.5 改动点

| 文件 | 位置 | 改动 |
| --- | --- | --- |
| `internal/app/runtime.go` | `:1272` `server()` | 为 TLS 启用时的 server 设置 `TLSConfig` |
| `internal/app/runtime.go` | `:1041-1047` | `ServeTLS(listener, certFile, keyFile)` 改为 `ServeTLS(listener, "", "")`；metrics 分支改用带 `GetConfigForClient` 的配置 |
| `internal/app/runtime.go` | `:1134` `metricsTLSConfig()` | 改为构造可重载的持有者，而不是返回一次性 `*tls.Config` |
| `internal/app/runtime.go` | 新增 | `Runtime.ReloadTLS() error`，依次重载两个持有者并汇总错误 |
| `cmd/halro/main.go` | `:910` 附近 | 在既有 `signal.NotifyContext` 之外新增 `SIGHUP` 通道与循环，调用 `ReloadTLS` |
| `internal/app/` | 新增 | 重载结果的 Prometheus 指标与日志 |
| `internal/config/config.go` | `:123` `TLS` 结构体 | `CertFile` / `KeyFile` 两个标量**替换**为 `Certificates []TLSCertificate`；校验列表非空、每项两个文件都给、路径绝对、解析后名字不重复 |
| `internal/config/default.yaml` | `:12` 附近 | 同步注释与默认形状 |
| `internal/logging/logger.go` | `:26` | `options.Level` 由 `cfg.Logging.SlogLevel()` 改为 `*slog.LevelVar`，并把该变量交给 Runtime 持有 |
| `internal/logging/sink.go` | `:181` 附近 | 新增 `Reopen()`，在既有写锁内关旧文件、开新文件 |
| `internal/app/runtime.go` | 新增 | `Runtime.Reload() error` 作为总入口：TLS 持有者、Metrics 持有者、日志级别、日志文件重开，逐项执行并汇总错误 |

启动期行为不变：证书加载失败仍然阻止启动（fail-closed）。

**这是一次破坏性的配置变更**：`tls.cert_file` / `tls.key_file` 被 `tls.certificates` 列表取代，旧键不再接受。按 pre-1.0 的规矩不保留兼容读法。需要改 `config.yaml`，**不需要**重新初始化数据目录。

### 6.6 失败语义

这是本改动唯一需要小心的地方 —— **此处的 fail-closed 正确解读是「拒绝接受坏证书」，不是「拒绝服务」**：

| 情形 | 行为 |
| --- | --- |
| 启动时证书不可加载 | 拒绝启动（与当前一致） |
| `SIGHUP` 后新证书解析失败 / 证书与私钥不匹配 | **保留旧证书继续服务**，记录 error 日志，失败计数 +1 |
| `SIGHUP` 后新证书可加载但已过期 | 接受并发布，同时记 error 日志与告警 —— 拒绝会让操作者失去用重载修复的手段，而是否可用应由客户端的信任决策决定 |
| 新证书剩余有效期 < 30 天 | 接受，记 warn 日志 |
| bundle 中任一张证书加载失败 | **整份 bundle 不发布**，旧 bundle 全部继续服务。部分发布会造成「一半新一半旧」的不可解释状态 |
| 两张证书声明同一个名字 | 拒绝该次重载（启动时则拒绝启动）：歧义的 SNI 映射比缺证书更难排查 |
| 各持有者之间一个成功一个失败 | 成功的生效，失败的保留旧值，`Reload` 返回聚合错误并逐项记录 |
| 日志级别取值非法 | 保持原级别，记 error；不影响同一次 `SIGHUP` 的证书重载 |

一次错误的换证不得把在线服务打掉。

**未匹配 SNI 的取舍**（实测见 §6.4）：客户端给了 SNI 但没有任何证书声明该名字时，有两种做法 ——

| 做法 | 客户端看到 | 评价 |
| --- | --- | --- |
| 返回 error 拒绝握手 | `remote error: tls: internal error` | 语义上更"干净"，但错误信息对排障毫无帮助，Go 无法发送更贴切的 `unrecognized_name` 告警 |
| **返回 fallback 证书**（推荐） | `x509: certificate is valid for api.example.com, not foo.example.com` | 诊断信息直接指出问题。证书是公开信息，返回一张名字不匹配的证书不构成任何泄露，客户端的名字校验照常拒绝连接 |

采用后者。同时在服务端记一条 warn，带上请求的 `ServerName` —— 但**只进日志，不进指标标签**：该值由客户端控制，作为标签会造成无界基数。

### 6.7 可观测性

新增指标（标签基数有界，符合仓库对高基数标签的约束）：

- `halro_tls_certificate_expiry_seconds{scope="serving|metrics",name="<配置中的主名>"}` —— 证书 `NotAfter` 的 Unix 时间戳。`name` 取**配置里第一个 SAN**，基数由配置决定而非请求决定；绝不使用握手中的 `ServerName`
- `halro_reload_total{item="tls|metrics_tls|log_level|log_file",result="success|failure"}` —— 覆盖 `SIGHUP` 允许清单里的每一项，而不只是证书
- `halro_reload_last_success_timestamp_seconds{item=...}` —— 支撑「当前生效值来自何时」的排障问题

配套：

- `deploy/observability/` 增加「证书 30 天内过期」与「重载失败」两条告警规则；`make observability-check` 会校验该配置。
- `halro doctor` 增加 `tls_certificates` 与 `metrics_tls_certificate` 两项离线检查：加载失败即 fail，剩余有效期不足 30 天即 warn，已过期即 fail。
- `/admin/api/v1/system/config` 与 `/admin/api/v1/system/status` 报告**当前生效值**与每项最后一次成功重载时间，不得重新读一遍 config.yaml 当作现状（§6.3 末尾）。
- 证书轮换属于运维事件而非租户事件，走日志与指标即可，**不进** append-only 审计流。日志只记文件路径、`NotAfter`、指纹前缀，不记私钥任何字节。

### 6.8 验证计划

按 `AGENTS.md` 的分层策略，迭代期只跑受影响范围：

```bash
go test ./internal/app/ -run TestTLS -v
go test -race ./internal/app/ -count=1        # 换证与握手并发，必跑
go vet ./internal/app/
```

推送前跑完整 gate（`make check`）。

**真机验证不可省**（仅单元测试通过不算数）：

1. 用证书 A 启动实例，`openssl s_client -connect ... | openssl x509 -fingerprint -noout` 记录指纹。
2. 原地替换为证书 B，发送 `kill -HUP <pid>`。
3. 再次取指纹，确认已变为 B。
4. 期间保持一条 SSE 流式请求在途，确认未被中断。
5. 换上一份损坏的 key 再发 `SIGHUP`，确认服务仍以证书 B 正常握手，且日志出现失败记录、失败计数 +1。
6. 配置两张不同名字的证书，用 `openssl s_client -servername api.example.com` 与 `-servername console.example.com` 分别握手，确认各自拿到对应证书；再用一个未声明的名字，确认拿到 fallback 且客户端报名字不匹配。
7. 把 `logging.level` 从 `info` 改为 `debug` 并 `SIGHUP`，确认新级别立即生效且**不需要**重启；同一次信号里证书也完成了重载。
8. 用 `mv halro.log halro.log.1 && kill -HUP <pid>` 模拟 logrotate，确认新记录写进重新创建的 `halro.log` 而不是被改名的旧文件。
9. Metrics 两阶段 CA 轮换：新旧 CA 合并 PEM → `SIGHUP` → 旧客户端证书仍可抓取 → 换客户端证书 → 移除旧 CA → `SIGHUP` → 旧证书被拒、新证书通过。

第 5 步是本改动的核心保障，第 9 步是唯一能证明「共用一次 `SIGHUP` 不会打断抓取端」的手段，都不能只靠代码审查确认。

**实机验证结果**（真实二进制，`serve` + `SIGHUP`，非单元测试）：

| 项 | 结果 |
| --- | --- |
| SNI 选择：无 SNI / `halro.test` / `console.test` / 未知名 | 分别得到 certA / certA / console 证书 / certA（fallback），符合设计 |
| 换证 + `SIGHUP`：新连接指纹 | `e86a1d17…` → `b67dfa5e…`，与替换后的文件一致 |
| 换证期间的在途连接 | 重载前后同一条 TLS 连接上的两次请求均 `HTTP 200`，未中断 |
| 私钥损坏后 `SIGHUP` | 仍以旧证书握手；日志出现 `reload item failed`，`item="tls"` |
| `logging.level` info→debug→warn | `log level changed` 记录出现；升到 warn 后后续 INFO 记录确实不再产生 |
| 日志文件改名后 `SIGHUP` | 新 `halro.log` 被创建，后续记录写入新文件 |
| 非可重载键改动（`server.max_header_bytes`） | 出现 `configuration changed in ways a reload cannot apply`，`sections=[server]` |
| `/metrics` 暴露 | 三个新序列齐全；一次 `SIGHUP` 后 `tls` / `log_level` / `log_file` 各 +1，`metrics_tls` 保持 0（未配置） |
| `halro doctor` 在实例运行时 | `tls_certificates` 报 warn（29 天），随后在 `data_lock` 失败 —— 与 §6.7 描述一致 |

并发正确性另有 `-race` 下 8 读 goroutine × 20 次重载的测试；反向注入一个未同步的影子字段确认该测试会报 `DATA RACE`。

---

## 7. 不做内置 ACME 的决定

### 7.1 性能不是理由

先排除一个常见顾虑：**内置 ACME 不会让系统变慢。**

- 证书查找在 TLS 握手而非每请求；改成 `GetCertificate` 后是一次原子指针读取，量级为纳秒，而握手本身 ECDSA 约 0.3ms、RSA 约 1ms。
- 续期是后台 goroutine，日级唤醒检查、60 天一次网络 I/O，内存开销数 KB，对请求路径零影响。

否决理由完全在别处。

### 7.2 真实成本

| # | 成本 | 说明 |
| --- | --- | --- |
| 1 | 继承一份别人在改的规范 | ACME 是活规范（ARI、短周期证书正在推进），CA 策略与 `x/crypto/acme` API 都会变。这份长期维护义务与 Halro 的领域（记账、路由、安全边界）无关 |
| 2 | 启动顺序变脆弱 | 证书私钥若入 vault，TLS 监听须等 master key 加载、data lock 获取、bolt 打开、`verifyVaultKeyCheck`（`runtime.go:934`）通过。任一环失败都表现为「端口不监听」，排障难度上升 |
| 3 | 备份正确性面积变大 | `backup.Create` 收显式文件清单（`internal/backup/archive.go:97`），不是目录遍历。ACME 材料漏进清单 = 恢复出的实例静默丢证书；加进清单 = 归档多一类机密，恢复演练与 `backup-restore.md` 都要跟改 |
| 4 | 失败域被拉进来 | CA 可达性、DNS 正确性、LE 速率配额（每注册域名每周 50 张、相同证书每周 5 张）成为网关可用性的输入。退避状态必须持久化，否则反复重启即可撞满配额 |
| 5 | 攻击面 | 公网 :80 明文挑战口（HTTP-01），或进程持有 ACME 账户私钥 —— 后者失窃等于可为该域名签发任意证书。二者当前都不存在 |
| 6 | 部署形态锁定 | TLS-ALPN-01 要求 Halro 直接持有 443，前面不能有 L7 代理。即该特性**只服务形态 A**，对形态 B 用户是纯负担 |

另有两项设计约束在实施前必须先决策，本身也说明这不是自包含模块：

- `safetransport` 的白名单在 `NewClient` 中构建期固化（`transport.go:102`），而 ACME 的订单、授权、证书下载 URL 是运行时由 CA 目录返回的 host。换 CA 或 LE 变更域名会直接得到 `ErrRefusedBeforeSend`（`transport.go:185`）。`AllowPrivate: false` 与 `deniedPrefixes`（`transport.go:217-226`）还意味着内网 ACME CA 默认不可达。
- ACME 账户私钥与证书私钥是新的一类长期机密，需新增独立 audience；复用 `vault` 那条会让它与 Provider 凭据在密码学上不可区分。

### 7.3 收益评估

收益只有一条：「单二进制，不用再跑一个反向代理」。

这个论点站不住 —— Caddy 也是单二进制，一行配置即有自动证书，且证书失败不拖累 Halro 进程。用户省下的不是「一个进程」，是「一行配置」。用永久维护负担、外部失败域与一次启动顺序重排去换一行配置，不划算。

### 7.4 重评条件

三条同时成立时再议：

1. 1.0.0 之后，且形态 A 确实是主流部署方式；
2. 有实际用户反馈无法运行反向代理（受限环境、单机嵌入式、合规要求进程数最小化）；
3. §6 的证书热重载已上线并稳定运行。

届时的最小可行形态：TLS-ALPN-01 + 底层 `x/crypto/acme`（不用 `autocert`，它自带的 HTTP-01 handler 与 `DirCache` 明文落盘与上述约束全冲突）+ 证书私钥入 vault（新 audience）+ 复用 §6 的热换机制 + 备份清单显式包含 + 三级过期状态机 + 持久化退避。

配置形状届时按 pre-1.0 规矩原位修正为 `tls.mode: manual | acme | disabled`，**不得**在 `tls` 旁并列一组 `acme` 字段。

---

## 8. 文档交付

以下缺口是当前就会踩的坑，与 §6 的代码改动并列交付：

1. `docs/guides/operator-guide.md` 增加形态 A 与形态 B 的完整可用配置，逐项说明每个键为什么必须那样填。
2. 同处**明确写出** `admin.external_origin` 在形态 B 下是必填而非可选 —— §1.3 的失败模式（Admin 全部变更被拒）目前没有任何文档提示，只表现为控制台跨源错误。
3. 同处写明 `trusted_proxy_cidrs` 必须匹配代理真实源地址，以及写错的后果是授权信号静默失效而非报错。
4. 写明 `admin.external_origin` 在非默认端口时必须带端口，并说明它只描述 Admin 的对外身份 —— Gateway 不需要、也没有对应配置项（§4.3）。
5. 写明设置 `admin.external_origin` 会强制首次初始化使用 setup token（§4.4），避免首部署时被误判为控制台故障。
6. 增加端点路径清单与客户端 base URL 说明（§3）—— 现有文档只给了 `127.0.0.1:8080` 的 curl 示例，没有一处集中说明「Gateway 是 `/v1/*`、Admin 是 `/admin*`、不存在 `/api`」。
7. 增加单域名路径拆分的代理配方（§5.2），含 `/health/*` 归属与「不要 strip prefix」两条易错点。
8. 新增证书轮换操作说明：替换文件 + `SIGHUP`，systemd `ExecReload` 示例，certbot / acme.sh 的 `--deploy-hook` 示例，以及 Windows 只能重启的说明。
9. 新增 **`SIGHUP` 重载范围**说明（§6.3）：哪些改动 `SIGHUP` 即生效、哪些必须重启，并说明「只重载材料、不重载语义」的判定规则。这一节要写得足够明确，否则操作者会默认 `SIGHUP` 等于重读配置文件。
10. 新增 **Metrics client CA 两阶段轮换**步骤（§6.2 末尾）：合并新旧 CA → `SIGHUP` → 换抓取端证书 → 移除旧 CA → `SIGHUP`。顺序错了会当场打断抓取。
11. 新增 **多证书 / SNI** 配置说明与 `tls.certificates` 的迁移提示：旧的 `cert_file` / `key_file` 不再接受，升级时必须改 `config.yaml`（§6.5）。
12. logrotate 配合方式（`postrotate` 里发 `SIGHUP`），以及 `logging.level` 可在线调整这一能力。
13. `README.md` 的 TLS 段落明确「没有内置证书申请」，避免操作者去找一个不存在的开关。

---

## 9. 实施记录：与方案的偏差

实施过程中三处与本文档原稿不同，均已在上文改正，此处列出以便对照：

1. **指标标签用 `status` 而不是 `result`。** 仓库既有的结果类序列一律是 `status="success|error"`（`halro_requests_total` 等），`metrics_contract_test.go` 的标签白名单也据此校验。为一致性改用 `status`，取值沿用 `success` / `error`。
2. **未匹配 SNI 返回 fallback 证书**，而不是拒绝握手。原因见 §6.6：Go 发不出 `unrecognized_name`，拒绝只会让客户端看到 `tls: internal error`。
3. **`halro doctor` 只做离线证书检查**，不报告运行中进程的重载状态 —— 它需要独占锁，见 §6.7。
4. **`Runtime` 字段用一个 `reloadRuntime` 子结构承载**，而不是平铺五个字段。`runtime_scale_test.go` 的宽度闸门按「一个子系统一个字段」计数，预算从 68 提到 69 并写明理由。
5. **`SIGHUP` 监听器改到 `ready` 回调里安装**。原先在 `app.Open` 之后立即安装，而证书是在 `RunWithReady` 里加载的 —— 这中间到达的信号会与启动写入并发读同一份材料。`ready` 在证书加载完、监听器绑定后、开始 Serve 前调用，是唯一正确的时点。

**实机验证中发现并修复的一个回归**：改造前 `metrics` 监听器在 `metrics.tls` 关闭而全局 `tls.enabled` 打开时，会复用全局证书走 TLS（旧代码的 `else if r.config.TLS.Enabled` 分支覆盖了它）。第一版改造把它变成了明文 —— 而 `cmd/halro/stats.go:275` 与 `main.go:926` 仍按 `cfg.TLS.Enabled` 拼 `https://.../metrics`。这个问题单元测试没有暴露，是用真实二进制 curl `/metrics` 拿到 `status=000` 才发现的。已修复并补 `TestMetricsListenerFallsBackToTheServerCertificate` 钉住，反向注入旧分支确认该测试会失败。

## 10. 未决问题

原先的三条已决策，结论并入 §6：`SIGHUP` 一并重载允许清单内的其他材料（§6.3）、Metrics 的证书与 client CA 共用同一次发布（§6.2）、支持多证书按 SNI 选择（§6.2）。由此产生的新问题：

1. ~~**`Reload` 的部分失败如何对外表达。**~~ 已定：逐项列出。`/admin/api/v1/system/status` 的 `reload.items` 每项带 `applies` / `successes` / `failures` / `last_success`，`applies` 把「这套部署没有这一项」与「从未成功过」分开。`halro doctor` 不参与（见 §6.7）。
2. **fallback 证书的选取规则。** 当前定为「列表第一项」。多证书部署下这意味着配置顺序有语义，容易被后续编辑无意改变。是否要改成显式 `default: true` 标记？倾向保持隐式并在文档中写明，但需要在校验里对「列表为空」以外的歧义情形给出明确报错。
3. **`logging.level` 可在运行中变化后，日志本身的可追溯性。** 排障时看到一段 info 级日志，无法判断当时是否处于 debug 期。是否在每次级别变更时写一条不可降级的 info 记录（含新旧级别与时间）？倾向要。
4. **Windows 上的等价手段。** `SIGHUP` 在该平台永不投递（§6.2），意味着 Windows 部署只能重启换证。是否需要一个平台特定的触发方式（命名管道 / 服务控制码），还是文档写明限制即可？倾向后者 —— 该平台不是本项目的主要部署目标。
5. **`tls.certificates` 列表变更（增删条目、改路径）是否要求重启。** 当前定为要求重启（§6.3 只重载文件内容，不重载路径与拓扑）。但「加一张新证书」是相当常见的操作，全量重启的代价与它的频率是否匹配，值得在实施后按实际使用情况复审。
