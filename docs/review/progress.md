# 评审整改进度

> [260805.md](260805.md) 是一份有日期的发现记录，不改动。本文件是它的**活的对照表**：哪些做了、哪些没做、以及做的过程中改变了对原结论的判断。
>
> 编号沿用 260805.md 第十章的修复清单。最后更新：2026-08-06（P2 第一批合并后）。

## 一句话状态

清单共 23 项，已完成 16 项。P0 四项全部完成；P1 八项完成七项，剩下的 P1-7 卡在一个部署决策上、ADR 已写好；P2 十一项完成五项（第 13、14、15、18、22 项）。**全部已合并进 main，没有悬空分支。** 整改过程中原报告的四条结论被修正，另外发现四项报告里没有的问题，其中三项已修完。

## 已完成

每项都按同一套流程验证：写测试 → **把代码改回缺陷状态确认测试真的失败** → 恢复 → 目标包 race 检测 → 全仓无缓存套件。反向验证是必需步骤，不是可选的——它是唯一能证明"测试守护的是真问题"的手段。前端项另加 `make frontend` 重建 + `npx tsc --noEmit` + 全量 vitest。

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
| P1-10 | Modal 脏检查上提 + Dashboard 首启 checklist + `routeBlocked` 接线 | `4476d93` `f7aa889` |
| P1-11 | 删除两个从未被调用的抽象（净删约 420 行，包数 57→56） | `61f8bd0` |
| P1-12 | 字体栈补 CJK、去掉从未打包的 Inter、8/9px 收敛到 12px | `da36316` |
| P2-13 | XFF 读全部 header 行 + Workbench 可达时启动告警 | `874eef0` |
| P2-14 | `startAttempt` 后的五条本地失败路径统一走 `abort()` | `28abffb` |
| P2-15 | 删除策略时，被停用项目持有的引用同样算引用 | `4f55cea` |
| P2-18 | SSRF deny 前缀表 + 告警 fan-out 上限 + 无效 CIDR 拒绝启动 | `1edc14c` |
| P2-22 | dead-man 探测移出状态锁 + 告警退避可取消 + SSE 裸 `\r` | `7dd39c3` |

## 未完成

### P1

| 编号 | 内容 | 备注 |
|---|---|---|
| P1-7 | 审计外部锚点 + 推荐 KMS 模式 | **ADR 已写：[adr/0015](../adr/0015-audit-chain-external-anchoring.md)，状态 Proposed**。机制部分已定（锚点内容、验证是一等操作、发射 fail-open）；**锚点推到哪里仍待决定**——dead-man 探测自带审计文件 / 远端 syslog / S3 Object Lock 三选一，买到的独立性和运维成本不同，是部署决策不是实现细节。决定后才动代码 |

### P2

剩六项，详见 260805.md 第十章：

| 编号 | 内容 | 备注 |
|---|---|---|
| P2-16 | ledger 帧升级为 HMAC + hash 链 | **改格式，风险最大**。要和 [adr/0014](../adr/0014-ledger-wal-backup-compatibility.md) 的 v1/v2 帧版本约定对齐，需要迁移路径和备份 manifest 联动，应先写 ADR |
| P2-17 | CI 加 fuzz 作业 + 语料入库；补齐中止路径测试 | |
| P2-19 | 拆分 `internal/app`、`store.go` 按数据域拆、"phase2" 重命名 | 大重构 |
| P2-20 | `ServiceOptions.Now` 注入、`reserved` 幂等超时/对账、流式中断按已投递量计费、预算超限首次即短路 | |
| P2-21 | 抽 `.data-row` 基类、焦点态收敛、Light 主题层级、尺寸 token lint 基线 | 前端 |
| P2-23 | 首个 git tag + CHANGELOG + 版本注入、config 注释模板、英文 user-guide、Parquet 降级、Admin 两级 RBAC | 多子项，RBAC 单独最大 |

## 整改过程中对原结论的修正

这是本文件存在的主要理由——报告是 8 月 5 日的判断，实现过程中有三处发生了变化。

**P1-5 的 `WriteTimeout` 建议是错的，没有照做。** `http.Server.WriteTimeout` 从读到请求头开始计时，挂上去会切断每一条 SSE 流。代码里没有它不是疏漏——流式路径用 `SetWriteDeadline` 逐写设限，那才是流式服务器的正确机制。但评审指出的底层问题为真：**非流式响应确实没有任何写超时**。这一条已拆为独立项并**已修完**（`df05301`）：在网关路由上包一层 writer，在响应真正开始时才 arm 截止时间——在请求进来时 arm 同样是错的，上游有权花掉 `routeTimeout`，那比写预算长得多。

**P1-5 的全局限流未做。** 需要新增配置项（速率、突发、校验、文档），应独立成一个改动而不是塞进认证前置里。

**Redaction/TokenGuard 悬空引用 fail-open 已被证伪**（对抗验证第十一章 #2），从 P0 降为 P2 加固项。攻击链在"重新启用项目"时被 409 拦截，另有两层 fail-closed 纵深。

**Developer Workbench 的源 IP 伪造在默认配置下被证伪**，从 P0 降为 P2。绕过网络隔离一条成立，但需要已认证管理员身份。

**`startAttempt` 资源泄漏的后果被证伪**，从 P0 降为 P2。缺 defer 清理属实，但三处是不可达死代码，两处被现有 adapter 绕过。修的时候这一点被再次确认：想通过 `ChatStream` 走到那条分支是走不通的——严格模式的流式拒绝发生在 attempt 创建**之前**（`AllowsStreaming` 早于 `startAttempt`），所以测试直接测共用的 `abort()` helper 本身。

**Developer Workbench 没有改默认关闭，改成了"可达时告警"。** 评审给的是"默认关闭**或**文档明示"，文档已经写了。对抗验证把它收窄为"已认证管理员的横向能力"，而绕过只在 Admin 监听器可被别处访问时才成立；loopback-only 既是 quickstart，也是首启 checklist 送人去的地方（P1-10 的动线终点就是工作台）。所以默认保持 enabled，改为在"工作台开着 + Admin 监听非 loopback"时启动告警——把取舍摆到该看见的人面前，而不是默认掰断一条正在用的路径。

**P1-12 的范围按"只动 8px/9px"收窄。** 报告写的是"最小字号提到 12px"，但 `styles.css` 里小于 12px 的字号声明共 139 处（8px×9、9px×35、10px×48、11px×47）。按字面执行会把 12px 以下的四级层级压平成一级，且没有任何测试守护字号、只能靠肉眼验证。经确认后只改 8/9px 共 44 处，`--font-size-xs` 从 11px 提到 12px，10px/11px 保持不动——**实际地板是 10px 而不是 12px**，这是有意的取舍，不是遗漏。

## 整改过程中新发现的问题（不在原报告内）

| 问题 | 位置 | 状态 |
|---|---|---|
| deadman 测试偶发挂死 10 分钟 | `internal/deadman/deadman_test.go` | **已修** `c1b814d`。根因不是"排队心跳未到期"这么简单：`NextAttempt` 与比较它的 `e.now()` 都取 `time.Now().UTC()`，丢掉了单调钟，墙钟回拨就会让 `drainOne` 早返回、接收端 handler 永不进入。修法两条：把队头 `NextAttempt` 显式打到过去让投递路径确定发生，再给 `<-entered` 加 10 秒兜底——兜底里必须先放开接收端再 `t.Fatal`，否则 `httptest.Close` 等待在途请求会二次死锁 |
| 非流式响应无写超时 | `internal/gatewayapi/handler.go` | **已修** `df05301`。见上方 P1-5 修正 |
| `admin_developer.go` 未通过 gofmt | `internal/app/admin_developer.go` | **已修** `655678d`。CI 跑 `go vet` 但不跑 gofmt，所以没人拦住它 |
| 数据面无全局按源限流 | `internal/app/runtime.go` `gatewayRouter` | 未做。从 P1-5 拆出，需新增配置项 |

## 顺带修正的既有问题

- `internal/deadman` 的 logger 原本完全绕过 safelog，而它持有每个探测目标的 bearer token（随 P0-2 修复）
- `internal/idempotency` 原有 70 行测试全部只覆盖已删除的死代码，其中 `TestUnknownIsTerminalAndKeysAreBounded` 名不副实——它没有测试任何 bounding，因为那个 store 根本没有（随 P1-11 修复）
- HA 设计文档指向的 `internal/authority.Mutation` 已随包删除，文档同步更新为说明其去向（随 P1-11 修复）
- `AlertForm` 的脏检查原本用 `window.confirm`，且它自己重述了一遍每个字段的默认值；改为 `useDirty`（与首渲染值比较）后，默认值只写一处，不会再和比较逻辑各自漂移（随 P1-10 修复）

## 已知的未验证面

P1-10、P1-12 都是纯视觉/交互改动，**没有在真实浏览器里逐页核对过**，只有 jsdom 断言和类型检查守护：

- 8/9px → 12px 涉及侧栏、徽章、工具栏等固定宽度容器，可能有被撑破的地方；
- 首启 checklist 只在"用量水位从未推进过"的实例上出现，jsdom 测了显示/隐藏与链接目标，没测过真实布局；
- Modal 脏检查用 `.modal.discarding > :not(header):not(.discard-prompt) { display: none }` 遮住表单，靠的是 CSS 而不是卸载子树——这是"取消后每个字段原样还在"的前提，但也意味着任何绕开这条选择器的子元素会漏出来。

## 仍未推送

main 领先 `origin/main` 46 个提交，全部是本地合并，尚未 push。这些工作要同步到远端需要显式决定。

仓库仍无任何 git tag（P2 第 23 项）。这一项越早做越便宜：改默认值——Developer Workbench 关闭、metrics 端口避开 9090、推荐 KMS 托管——在首个 tag 之前只是改默认，之后就成了需要迁移说明的破坏性变更。
