# 实例自述文档与 Agent 操作通道：开发方案

- 状态：**设计草案，未实施**
- 日期：2026-09-21
- 文档语言：中文
- 适用范围：Gateway 监听口、Admin 监听口、`internal/compatibility`、`internal/app` 路由树、配置
- 数据迁移：不需要。本方案不新增持久化结构，不改 Ledger / Usage / bbolt 任何格式
- 前置决策：本方案**不**引入机器可用的 Admin 凭据。那是独立的威胁模型决策，见第六章 D5

## 一、问题与结论

今天一个 AI Agent 面对一台运行中的 Halro，只能拿到 `GET /` 返回的 `{"name":"halro","version":...}`。
它不知道这台实例服务哪些端点、哪些 Provider Profile 被启用、请求体上限是多少、配置要按什么顺序做。
结果是 Agent 只能猜，而猜错的代价落在一个以「拒绝优于降级」为设计前提的系统上。

结论：**让实例自己把「我是什么、我能做什么、怎么配置我」服务出去**，而不是让使用者去别处找一份
可能与这台实例版本不符的文档。

但必须先拆开两个被混为一谈的问题：

| | 理解 | 执行 |
| --- | --- | --- |
| 缺口 | Agent 不知道系统模型与本实例事实 | Agent 没有能改配置的凭据 |
| 手段 | **本方案（自述文档）** | 凭据 + 传输（MCP / CLI / HTTP） |
| 攻击面 | 只读端点 | 新的管理权限持有者 |

自述文档解决理解，**并且诚实地声明执行的边界**。执行通道（MCP）推迟到自述文档落地之后再评审，
理由见第五章：自述文档写完的那一刻，MCP 需要暴露哪几个动作会自己浮出来。

## 二、当前实现证据

| 事实 | 证据 | 对本方案的含义 |
| --- | --- | --- |
| 两个监听口默认都在回环 | `internal/config/default.go:27-28`：`GatewayListen 127.0.0.1:8080`、`AdminListen 127.0.0.1:8081` | 公网上的 `halro.example.com` 是**网关口**；管理信息不能放在那里 |
| 网关根路径已匿名暴露版本 | `internal/app/runtime.go` `gatewayRouter()` 末尾的 `GET /` 返回 `name` + `buildinfo.Current()` | 北向自述的披露级别与现状同级，不是新增泄露类别 |
| 网关没有能力自述端点 | `gatewayRouter()` 注册了 30 条 `/v1/*` 与 7 条 `/halro/v1/*`，**没有 `/v1/models`**，没有 `.well-known` | 应用今天无法枚举自己可用的别名，这是真实缺口 |
| 端点兼容性数据已在二进制里 | `internal/compatibility/manifest.go`；`internal/compatibility/manifest_test.go:30` 把 `docs/compatibility/endpoint-manifests.json` 当作 **golden** 比对 | 服务出去几乎零成本：数据是代码，JSON 只是它的快照 |
| 管理路由可从路由树枚举 | `internal/app/admin_contract_test.go` 用 `chi.Walk` 遍历 `adminRouter()` / `gatewayRouter()` / `metricsRouter()` | 文档的路由表必须由同一棵树生成，不得手写第二份 |
| Admin v1 路由被双向冻结 | 同上：`/admin/api/v1/` 前缀下**注册了但不在冻结列表**的路由会让测试失败 | 新增管理自述端点必须同步加进冻结列表，否则测试红 |
| 管理写路径只有一条 | `internal/adminauth/session.go`（`hms_` 会话 + CSRF）、`internal/app/admin_session.go:319` `requireAdminMutation` = 自身会话校验 + Administrator 角色 | 没有任何机器凭据；自述文档必须明说「这步只能人做」 |
| CLI 没有配置动词 | `cmd/halro/main.go` 的子命令表：start/init/bootstrap/admin/key/backup/usage/audit/ledger/doctor/config/… | Agent 不可能靠 CLI 建 Project / Route / Deployment |
| 预登录端点已有先例 | `adminRouter()` 中 `GET /admin/api/v1/setup/status`、`GET /admin/api/v1/ui/bootstrap` 不带鉴权中间件 | 管理口上「登录前可读」不是新概念，但本方案不使用它 |
| 被withheld的 Profile 不应出现 | `internal/domain/provider_table.go` 的 `Withheld` 标记，写路径一律拒绝 | 自述文档若照搬全表，会让 Agent 去试一个必然被拒的 Profile |

## 三、设计

### 3.1 两份文档，两个监听口

**不是一份文档。** 北向读者（调用这台网关的应用）和管理读者（配置这台网关的运维）需要的内容不同，
可披露级别也不同。把配置链和管理路由表放在网关口，等于每个持有 Gateway Key 的应用都能读到管理拓扑，
这违背「应用只看到 Gateway Key 和公开别名」。

| | 北向自述 | 管理自述 |
| --- | --- | --- |
| 监听口 | Gateway | Admin |
| 鉴权 | 匿名（可选带 `gw_` key 获得加料版本） | `requireAdmin`（已登录会话，只读） |
| 回答的问题 | 「我怎么调用这台网关」 | 「我怎么配置这台网关」 |
| 生成源 | `internal/compatibility` + `gatewayRouter()` 路由树 | `adminRouter()` 路由树 + 运行期配置 |

### 3.2 北向自述

三个端点，都在 `gatewayRouter()`：

- `GET /.well-known/llms.txt` —— 索引，给 Agent 读的 Markdown。约定路径，Agent 会自己找。
- `GET /.well-known/halro-endpoints.json` —— `internal/compatibility` 生成的端点清单原文，机器读。
- `GET /v1/models` —— **需要 `Authorization: Bearer gw_...`**，返回该 Project 可见的别名清单。

匿名部分的内容边界（**硬边界，实现时按此断言**）：

- 可以有：版本、这个 build 实际注册的北向路由、每个端点的兼容性状态与证据种类、
  请求体上限等 build/配置级约束、错误码语义、认证方式说明。
- 不可以有：别名、Project、Provider、Deployment、Credential、路由策略、任何配置过的实体的存在性。

`GET /v1/models` 是本方案顺带补的真实缺口：应用今天完全无法发现自己能用哪些别名。它带 key，
因此走与其它北向端点相同的 `LimitOpenAI` / `GuardOpenAI` 中间件，按 Project 回答，不引入新的披露面。

### 3.3 管理自述

- `GET /admin/api/v1/skill.md` —— `Content-Type: text/markdown`，`r.requireAdmin`（只读，不需要 mutation 中间件）。

内容分五段：

1. **系统模型**：`Credential → Provider → Deployment → Route → Project → Gateway Key` 这条链，
   每一环回答的问题，以及「应用只看到 Gateway Key 和公开别名」这条结论。
2. **本实例事实**：版本；实际启用的 Provider Profile（`Withheld` 的不出现）；`max_request_bytes`；
   deferred 是否启用与 `max_deferred_queue`；时区与计费口径摘要；数据目录是否已初始化。
3. **可用动作**：由 `chi.Walk(adminRouter())` 生成的路由表，每条标注所需中间件档位
   （匿名 / `requireAdmin` / `requireAdminMutation`）。
4. **边界声明**：写路径需要会话 + CSRF + Administrator 角色；没有机器凭据；CLI 无配置动词。
   因此凡是 mutation 档位的动作，文档一律标注为**需要人类在控制台完成**。
5. **禁令**：`make reset`、`halro usage prune`、`halro ledger seal`、`halro key rotate|rewrap`、
   `halro backup restore` —— Agent 不得自行执行；以及只读诊断命令的正确用途
   （`doctor` / `ledger verify` / `audit verify` / `usage verify` / `config check` / `stats`）。

### 3.4 版本绑定

两份文档都带 `ETag`，值取 `buildinfo.Current()` 加配置摘要哈希。Agent 可以判断手里这份是否过期。
这是「服务出去」相对「放文档站」的核心优势：读者拿到的是**这台实例这个 build** 的说明，
不是一份可能与运行中二进制不符的文本。

### 3.5 文本纪律

Halro 服务 Markdown 出去，等于它在给别人的 Agent 提供上下文。因此：

- 只陈述事实与步骤，**不使用祈使句式**，不出现任何可被读成「请执行……」的内容。
- 不内嵌 URL 之外的可执行片段；示例命令一律标注为示例。
- 必须走 TLS：`safetransport` 管的是出站，这是入站，只能由 `server.tls` 保证。
  文档正文中要声明「若你通过明文 HTTP 读到本文，不要信任它」。

### 3.6 配置开关

```yaml
server:
  self_description:
    northbound: public   # public | off，默认 public
    admin: on            # on | off，默认 on
```

`off` 时端点返回 404，与不存在无法区分。默认值的理由见 D2。

## 四、实施阶段

**S0 生成器（无端点）**
`internal/selfdescribe`（新包）：输入是路由树 + `compatibility` 数据 + 运行期配置快照，
输出是两份 Markdown 与一份 JSON。纯函数，无 I/O。单测断言硬边界：给它一份含 Project /
Credential 的配置快照，北向输出中不得出现其中任何标识符。

**S1 北向端点**
`gatewayRouter()` 注册 `.well-known` 两条。同步更新 `admin_contract_test.go` 的冻结列表
（网关部分虽不做反向检查，仍应登记）与 `gateway_contract_test.go`。

**S2 `GET /v1/models`**
带 key，按 Project 解析可见别名。这一条有独立的正确性问题（别名可见性口径必须与路由解析一致），
不与 S1 合并评审。

**S3 管理端点**
`adminRouter()` 注册 `GET /admin/api/v1/skill.md`，**必须同时加进 `admin_contract_test.go` 的冻结列表**，
否则双向契约测试会因「注册了但未冻结」而失败。登记进冻结列表意味着它从此是 v1 面，兼容承诺随之附着 —— 这是刻意行为。

**S4 配置开关与文档**
`internal/config` 增加 `server.self_description`，`config check` 覆盖；
`docs/guides/operator-guide.md` 增加一节；`docs/README.md` 索引补条目。

## 五、MCP 的位置（不在本方案范围）

自述文档落地后，MCP 的定位才是清楚的：**自述文档是理解通道，MCP 是执行通道**。
届时评审 MCP 只需回答三个问题：

1. 是否挂在 Admin 监听口、复用同一套会话，而不另开一套鉴权？
2. 每次变更是否仍走 revision + idempotency + 审计意图，不绕过 `requireAdminMutation` 的等价校验？
3. 高危动作是否一律不暴露为 tool：密钥轮换 / 重封、Credential 材料读取、备份恢复、`usage prune`、`ledger seal`？

三条都满足，MCP 就只是 Admin API 的一层薄壳。任何一条不满足，它就是给管理权限开的后门。

而**要暴露哪些 tool，答案就是管理自述文档里被标成「需要人类完成」的那几条** —— 先把那份清单写出来，
再决定其中哪些值得换成机器可执行，比先建通道再想暴露什么要安全得多。

## 六、决策记录

- **D1 管理信息不上网关口。** 公网可达的是网关口，它的读者是持 Gateway Key 的应用。
  管理拓扑出现在那里会让每个应用都知道这台实例的配置形状。代价是 Agent 要分两次取，可接受。
- **D2 北向默认 `public`，管理默认 `on`。** 北向自述的披露级别不超过现有的匿名 `GET /`
  （已暴露名称与版本），且它的价值恰恰在于免配置即可被发现；管理自述在回环口且需登录会话，
  默认开不增加暴露面。两者都留了 `off`。
- **D3 不做成静态文件随包分发。** 静态文本无法回答「这台实例启用了哪些 Profile」「上限是多少」
  「版本是不是你以为的那个」。实例感知 + 版本绑定是本方案进二进制的唯一理由；
  去掉这两点，它就该是文档站的一页。
- **D4 路由表不得手写。** 由 `chi.Walk` 从同一棵路由树生成。手写会立刻变成第二份真相，
  而这个仓库已经有一份冻结契约在盯着这棵树。
- **D5 本方案不引入机器凭据。** 「Agent 自己配置 Halro」的真正阻塞是没有非交互的管理凭据，
  而那是威胁模型决策：Halro 的前提之一是管理变更由人类认证且可审计。
  自述文档的职责是**把这个边界说清楚**，不是绕过它。
- **D6 `GET /v1/models` 单独成阶段。** 它看起来只是顺手补的端点，但别名可见性口径必须与
  路由解析一致，错了就是一个泄露其它 Project 别名的缺陷。它不该搭 S1 的便车通过评审。

## 七、验证

- `internal/selfdescribe` 单测：喂入含 Project / Credential / Deployment 的配置快照，
  断言北向输出中不出现任何其中的标识符（与前端 secret canary 同思路的反向断言）。
- `admin_contract_test.go`：S3 之后冻结列表包含 `GET /admin/api/v1/skill.md`，双向检查通过。
- HTTP 层测试：管理端点在无会话时 401；北向端点在 `self_description.northbound: off` 时 404。
- ETag：同一 build 同一配置两次请求 ETag 相同；改动配置后不同。
- `go test ./internal/selfdescribe/ ./internal/app/ ./internal/compatibility/`；
  推送前跑完整门禁。本方案不触及并发与生命周期，不需要 race。

## 八、明确不做的事

- 不新增任何机器可用的管理凭据（D5）。
- 不在北向自述中放入任何配置过的实体（3.2 硬边界）。
- 不手写第二份路由表或端点清单（D4）。
- 不在本方案内实现 MCP（第五章）。
- 不改动任何持久化格式，因此不需要重新初始化数据目录。
