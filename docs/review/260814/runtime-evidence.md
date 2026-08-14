# 阶段 3：真实二进制运行时验证

环境：一次性数据目录（scratchpad，未触碰仓库 `data/` 与 `master.key`），本地构建 `bin/halro`
（含本轮全部修复与 `allowed_models` 改名），监听 18080/18081/19090。建链走真实 CLI：
`halro init` → `halro bootstrap`（credential→provider→deployment→route→project→key 一次建全，
gateway key 一次性打印）→ `halro admin bootstrap` → `halro serve`。Admin API 以脚本驱动
（登录 Cookie + CSRF + Origin 头；管理员未注册 TOTP，step-up 按设计只需密码）。

## 前置问题的裁决（方案第五节预留的两问）

1. **Admin 脚本化**：可行，无需浏览器。注意点：登录 Cookie 是 `__Host-` 前缀 + `Secure`，
   http 下 python cookiejar 拒发，需手动带 `Cookie` 头；所有会话/变更请求要 `Origin` 头
   （DNS-rebinding 防护，`admin_session.go`）。
2. **safetransport 下打桩**：loopback **无条件**拒绝（`safetransport/transport.go`
   `validateAddress`，与 `allow_private_provider_endpoints` 无关）；私网地址在
   `allow_private_provider_endpoints: true` 下放行。macOS 平台证书验证器不认 `SSL_CERT_FILE`，
   自签上游不可信。**按方案预案收缩**：上游指向无监听的私网端口，每次 attempt 在拨号处失败——
   验证边界为"直到 Provider 请求发出及其失败结算"，上游成功路径未验证（如实记录，不以静态推断顶替）。

## 剧本结果（15/15）

| 检查 | 结果 | 证据 |
|---|---|---|
| rename/allowed_models-served | PASS | Admin API 项目对象只含 `allowed_models`，无 `allowed_routes` |
| R1 全链放行至 Provider attempt | PASS | 502 `provider_error`；ledger 落 44 帧、doctor `accounting_leases pending=0`（失败 attempt 保守结算，无悬挂预留） |
| R1 响应不泄漏 | PASS | 响应体无凭据、无上游模型名 `fake-model`、无上游地址 |
| R8 过期 revision | PASS | 陈旧 If-Match → 412 |
| R2 在用凭据删除 | PASS | 409 `credential is still referenced` |
| R6 凭据过期 advisory | PASS | `expires_at` 置 2020 后轮换成功，网关仍发起上游 attempt（502），不据此拒流量——与 `models.go:125` 注释一致 |
| R5 跨项目隔离 | PASS | 项目 B 的 key 请求项目 A 的别名 → 403 `model_not_allowed`（先于存在性检查，不泄漏别名是否存在） |
| R3 route 停用即时生效 | PASS | Admin PUT 后立刻 404 `model_not_found`，无窗口期 |
| R3 route 复用 | PASS | 503 `provider_unavailable`"all provider circuits are open"——五连拨号失败后熔断开路，有界重试按设计生效 |
| R7 key 停用 / key 过期 / project 停用 | PASS ×3 | 三种全部 401，快照即时换代 |
| F1 修复回归（真实二进制） | PASS | 墓碑 route PUT → 404 |
| 日志不含秘密 | PASS | serve.log 无凭据明文、无 gateway key |
| 运维侧失败可见 | PASS | `provider attempt failed` 行带 request_id/deployment/binding/error_class，上游正文不落盘 |

收尾：`halro doctor` 全绿——`bbolt schema v29`（迁移 29 在真实目录执行成功）、ledger 链 44 帧验证
通过、admin 审计无积压、topology 引用完整、pricing 高水位无隔离。

## 运行时阶段的新发现（静态审查未触及）

**F23【问题】健康过滤的空候选被误报为 `unsupported_feature` 400**
主动探测把 deployment 标不健康后，`resolveCandidatesLocked`（`provider.go:434-437`）先按健康剔除，
`resolveRequest`（`service.go:235-239`）看到"别名有 target 但候选为空"，一律归因为"操作不支持"，
对客户端说 `model route does not support chat completions`（400）。实际语义是"上游不健康"——能力
没变、请求没错，正确形状应是 5xx（同熔断开路的 503 一类）。误导两头：客户端以为请求写错，运维
按能力问题排障。复现：探测失败一轮后任何该别名请求。建议修法：`Registry` 把"健康剔除后为空"与
"操作过滤后为空"区分返回，facade 分别映射 503/400。列入下一批整改。

**观察（非缺陷）**：`health_probe_interval` 的运行时设置（bbolt 存储）优先于 config 文件
（`health.go:24-30`）——测试中改 config 不生效即由此；行为符合代码注释设计，但排障时容易踩。

## 剧本留档

`scenarios.py` 与 serve 日志在会话 scratchpad `runtime260814/`（会话结束即弃，凭据均为假值）。
