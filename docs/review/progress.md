# 评审整改进度

> [260805.md](260805.md) 是一份有日期的发现记录，不改动。本文件是它的**活的对照表**：哪些做了、哪些没做、以及做的过程中改变了对原结论的判断。
>
> 编号沿用 260805.md 第十章的修复清单。最后更新：2026-08-06。

## 一句话状态

P0 四项全部完成并合并。P1 十二项中已完成六项，其中一项待合并。整改过程中原报告的三条结论被修正，另外发现四项报告里没有的问题。

## 已完成

每项都按同一套流程验证：写测试 → **把代码改回缺陷状态确认测试真的失败** → 恢复 → 目标包 race 检测 → 全仓无缓存套件。反向验证是必需步骤，不是可选的——它是唯一能证明"测试守护的是真问题"的手段。

| 编号 | 内容 | 提交 |
|---|---|---|
| P0-1 | 熔断器三态化：本地拒绝不再冒充上游健康 | `e989dbb` |
| P0-2 | safelog 覆盖 slog 的全部属性形状 + deadman logger | `83ebf07` |
| P0-3 | 登录限流前置 + 审计按分钟聚合 + 去重索引有界 | `4871de8` |
| P0-4 | 拆开"是否可重试"与"是否该计费" | `d116f77` |
| P1-5 | 认证前置到解析之前（部分，见下） | `effef81` |
| P1-6 | 让被拒绝的请求说清楚哪个字段错了 + 404/405 信封 | `203f5e5` |
| P1-8 | 首启体验：去 npm 依赖、master.key 警告、CLI 可读报错 | `d03c32e` |
| P1-9 | apply 失败后拒绝继续追加 + 共享状态标记不可用 | `3c2bc31` |
| P1-11 | 删除两个从未被调用的抽象 | `61f8bd0`（**待合并**） |

## 未完成

### P1

| 编号 | 内容 | 备注 |
|---|---|---|
| P1-7 | 审计外部锚点 + 推荐 KMS 模式 | **建议先写 ADR**。对抗验证已确认：File 模式成立、KMS 模式被证伪。修法涉及"锚点推到哪里"（syslog / S3 Object Lock / deadman 通道）的部署决策，不该由实现者单方面选定 |
| P1-10 | Modal 脏检查、Dashboard 空态 checklist、`routeBlocked` 文案接线 | 前端。`deployments.routeBlocked` 文案已写好但至今零引用 |
| P1-12 | 字体栈补 CJK + 最小字号提到 12px | 前端。默认语言是中文，字体栈却无任何 CJK 字族；`styles.css` 仍有 44 处 8px/9px。改后需 `make frontend` 重建并提交 `internal/webui/dist`（CI 会校验一致性） |

### P2

十一项未开始，详见 260805.md 第十章。另有三项因对抗验证结果从 P0 降级至此（见下节）。

## 整改过程中对原结论的修正

这是本文件存在的主要理由——报告是 8 月 5 日的判断，实现过程中有三处发生了变化。

**P1-5 的 `WriteTimeout` 建议是错的，没有照做。** `http.Server.WriteTimeout` 从读到请求头开始计时，挂上去会切断每一条 SSE 流。代码里没有它不是疏漏——流式路径用 `SetWriteDeadline` 逐写设限，那才是流式服务器的正确机制。但评审指出的底层问题为真：**非流式响应确实没有任何写超时**。这一条已拆为独立项（见下）。

**P1-5 的全局限流未做。** 需要新增配置项（速率、突发、校验、文档），应独立成一个改动而不是塞进认证前置里。

**Redaction/TokenGuard 悬空引用 fail-open 已被证伪**（对抗验证第十一章 #2），从 P0 降为 P2 加固项。攻击链在"重新启用项目"时被 409 拦截，另有两层 fail-closed 纵深。

**Developer Workbench 的源 IP 伪造在默认配置下被证伪**，从 P0 降为 P2。绕过网络隔离一条成立，但需要已认证管理员身份。

**`startAttempt` 资源泄漏的后果被证伪**，从 P0 降为 P2。缺 defer 清理属实，但三处是不可达死代码，两处被现有 adapter 绕过。

## 整改过程中新发现的问题（不在原报告内）

| 问题 | 位置 | 说明 |
|---|---|---|
| deadman 测试偶发挂死 10 分钟 | `internal/deadman/deadman_test.go` `TestSlowReceiverDoesNotBlockProbeTick` | 完整套件中出现过一次，单独跑 8 次全通过。根因是测试的时序假设：若排队心跳的 `NextAttempt` 仍在未来，`drainOne` 早返回不发送，接收端 handler 永不进入，测试卡在 `<-entered` 且无超时兜底，只能等包级 10 分钟超时。修法：给 `<-entered` 加 select 超时分支 |
| 非流式响应无写超时 | `internal/gatewayapi/handler.go` | 从 P1-5 拆出。慢读取客户端可长期占用 goroutine。要用 ResponseController 逐响应设限，不能用 server 级 `WriteTimeout` |
| 数据面无全局按源限流 | `internal/app/runtime.go` `gatewayRouter` | 从 P1-5 拆出。需新增配置项 |
| `admin_developer.go` 未通过 gofmt | `internal/app/admin_developer.go` | main 上的既有问题，与本次整改无关，未动 |

## 顺带修正的既有问题

- `internal/deadman` 的 logger 原本完全绕过 safelog，而它持有每个探测目标的 bearer token（随 P0-2 修复）
- `internal/idempotency` 原有 70 行测试全部只覆盖已删除的死代码，其中 `TestUnknownIsTerminalAndKeysAreBounded` 名不副实——它没有测试任何 bounding，因为那个 store 根本没有（随 P1-11 修复）
- HA 设计文档指向的 `internal/authority.Mutation` 已随包删除，文档同步更新为说明其去向（随 P1-11 修复）

## 仍未推送

main 领先 `origin/main` 若干提交，全部为本地合并，尚未 push。仓库仍无任何 git tag（P2 第 23 项）。
