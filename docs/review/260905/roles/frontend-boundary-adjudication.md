# FE-03 / FE-05 独立边界裁决

2026-09-05，独立核对基线 `381743f6613607dc256828f4776b52af8bdd232c`。按授权读取 [Frontend 原报告](frontend.md)，独立检查实际前后端调用链、权限与已有防御，并仅重跑原两项定向 fixture。没有生产源码、仓库测试、Git 状态或用户数据变更；未做真实浏览器网络故障实验、Provider 调用或全量门禁。本报告及两份汇总是本次唯一仓库写入。

| ID | 裁决 | 严重度 / 置信度 |
| --- | --- | --- |
| FE-03 | CONFIRMED：debug key 同一操作重试更换幂等身份；额外 key 记录后果为源码确认 | P2 / 高；真实 commit-disconnect HTTP 旅程未执行 |
| FE-05 | CONFIRMED：未编辑的 500-byte 上限被表单提交成 0；服务端接受与生效为源码确认 | P2 / 高；真实 API 保存及随后请求边界未执行 |

## FE-03：幂等防御没有阻止更换身份的重试

入口为 `web/src/pages/DeveloperPage.tsx:202-210,408-415`。`createDebugKey.mutationFn` 每次调用生成 `crypto.randomUUID()`、当前名称时间及新到期时间。ConfirmButton 的 submit 失败后保留对话框并解除 pending，下一次点击再次调用 mutation（`web/src/components.tsx:560-581`）。`web/src/api.ts:315-323` 将这个第三参数直接写成 Idempotency-Key，不在 API 层替换或缓存。

独立复核的服务端链路：`internal/app/runtime.go:1624` 使用 requireAdminMutation；`admin_session.go:319-350` 要求有效 Admin session、administrator、同源和 CSRF。`internal/app/admin_projects.go:234-285` 还在每次 mint 校验 step-up，按 actor/project/token 派生 key ID，再以 expectedRevision=0 写入；`internal/store/bolt/store_projects.go:61-102` 和 `store.go:2010-2021` 对已有 ID 拒绝插入。相同 token 因而会返回 `gateway_key_idempotency_replay`，不会再次发明文；不同 token 则不命中这一幂等碰撞。项目存在性、tombstone、key validation、事务内 audit intent 仍有效。

**前提与实际。** 有权 administrator 完成密码/所需 TOTP 后第一次提交已落库，但响应丢失；再次完成必要的 step-up 重试同一逻辑操作。前端发送新 UUID，因此正常服务端可以新建另一条 key，而不是引导处理第一次已提交、明文丢失的 key。原 fixture 首次 mock 401 是让真实 ConfirmButton 展示密码，第二次 mock 响应丢失，第三次重试；它比较的是后两个调用的 token。首次无密码 401 不会创建 key，也不是本 finding 的重复创建依据。

**试图证伪及结果。** requireStepUp、pending disabled 和错误显示不固定逻辑操作身份。即使 MFA 单次 TOTP 已消费，用户提交新有效 code 后仍到达该路径；不假设 MFA 被绕过。普通 Project CreateKey 的 `useRef`（`ProjectsPage.tsx:653`）正确保留 token，却不是 Developer 组件使用的状态。服务端在写入前检查 ctx，不能撤销已经完成的 commit，因此不排除“commit 后丢响应”的前提。

**原 fixture 的证据边界。** 真实 DeveloperPage/ConfirmButton 在 jsdom 中运行，session/project/API 为 mocks；密码字符串未经过真实 Argon2/TOTP，也没有真正写两个 key。独立重跑直接证明重试 token 不同；真实 store 按不同 ID 新增的因果由上述源码确认。不是浏览器攻击、已泄露明文、已证明 key 被滥用或新的授权绕过。debug key 有 24h 到期及管理入口，正常 key/project 权限、预算继续生效，因此不升级 P1。

**最小修复与回归。** 固定一次确认操作的 token 和 payload，到明确新操作才重置；保留 replay 的 revoke/reissue 恢复入口。永久回归至少断言两次已认证 retry 的身份/payload 相同，并以本地 runtime 的 commit 后响应丢失证明只一条 key。另保留正常创建、fresh TOTP retry、主动新建、普通 CreateKey 的对照。本次未实施。

## FE-05：精确 bytes 经整数 KiB 往返后改变语义

`web/src/pages/ProjectsPage.tsx:52` 要求非负整数 requestKB，`:485` 初始化 `Math.round(bytes/1024)`，`:515` 无论字段是否修改都提交 `requestKB*1024`。500/1024 舍入为 0，1500/1024 舍入为 1；这里是写值转换，不只是展示精度。`web/src/api.ts:298-299` 将整个 body PUT 到项目 API。

服务端 `internal/app/runtime.go:1620` 要求 requireAdminMutation，仍有 administrator/session/CSRF/origin；`internal/app/admin_projects.go:105-163` 验证 revision、非 tombstone、references，构建 replacement、PutProject 并 activateAuthSnapshot。`:428` 直接复制输入 MaxRequestBytes；`internal/domain/models.go:341-354` 只拒绝负数，不要求 KiB 整数倍，因此 500 是合法配置，0 也是合法值。ETag 不会阻止同 revision 内无意舍入；服务端也没有“未编辑字段”信息来恢复 500。

实际资源边界沿 `internal/app/runtime.go:504-519` 的 AuthorizeKey 从当前 project snapshot 提供 bytes 到 handler；`internal/gatewayapi/handler.go:1023` 仅在 projectBytes>0 且小于实例上限时收紧，实例 MaxBytesReader 仍执行（如 `:769`）。因此 0 表示撤去项目更小的显式上限，并非无限制或绕过实例上限。

**原 fixture 校验与独立观察。** mock projectsPage 返回合法 500-byte 项目及可用 route；真实 ProjectsPage 打开编辑并立即点击“保存并热加载”，mock updateProject 捕获 body.max_request_bytes=0。尽管测试名说 name-only，源码实际没有修改名称：它更直接证明完全未编辑就发生转换。不要将它描述为已经真实执行了改名/持久化。没有 dirty-only 提交拦截；本次定向重跑再次通过断言。mock update 的回包仍是原对象，不影响捕获的出站参数，却也不能当作已验证实际热加载证据。

**范围与裁决。** 需要有权管理员编辑已有精确 byte 项目；read_only 控件与 server mutation role gate 存在，不是未授权修改。小上限可以通过 API 合法配置，不能用 UI 只支持整数 KiB 来否定服务端的精确 bytes 契约。当前实例上限有效，未演示资源耗尽。保留 P2、高置信度；实际 API 保存与下一次大于500 bytes请求的接受/拒绝尚待集成回归。

**最小修复与回归。** 未触碰字段时保留原 bytes，或提供精确 bytes/小数 KiB 编辑，不把小正值静默变特殊 0。覆盖 0、500、1024、1500、1048576 无改保存、仅改名称和明确改限额；实际 API GET 和认证 gateway body boundary 应保持目标值。保留 ETag 冲突、read_only 拒绝与实例硬上限对照。本次未实施。

## 命令、结果与不变量

工作目录：`/Users/ziy/Code/ClayCosmos/Halro/web`。原 temporary fixture 与 config 已全文检查，测试只导入基线实际组件，通过 mocks 隔离 API。原脚本包含五测试，本次 selector 仅运行两项。

```sh
./node_modules/.bin/vitest run --config /private/tmp/halro-frontend-review-260905/vitest.config.mts -t 'reproduces debug key retry|reproduces name-only project edit' > /private/tmp/halro-frontend-review-260905/boundary-adjudication.log 2>&1
```

实际进程 **exit 0**；Vitest 4.1.11，1 file passed，**2 passed / 3 skipped**；总耗时 1.18s，tests 295ms。无 sandbox/网络失败，无升级权限，无全量 suite。`git rev-parse HEAD` exit 0 为上述基线。

| 不变量 / 反命题 | 独立结果 |
| --- | --- |
| 同一 debug mint 重试保留幂等身份 | 违反：真实组件传入的后两个 token 不同。 |
| 服务端幂等碰撞保护本身无效 | 不成立：真实写路径以相同派生 ID 拒绝重复；失效的是客户端没有复用 ID 输入。 |
| 未编辑项目 bytes 应原样往返 | 违反：500 被提交成 0。 |
| FE-05 可以绕过实例上限或 read_only 授权 | 无依据：源码保留实例 hard limit 及 server role/CSRF gate。 |

复现通过表示成功观察到缺陷，不表示修复完成。日志 SHA-256：`21f6618885791f5ad96b84cb028cce90bf66fa172ed8598b1dc32b2560963d11`。原 `review.test.tsx` SHA-256：`99680014afd2e7ffac76e3ee37948c9ecca2595b289f37878b8b77b5a58a303a`。fixture 路径 `/private/tmp/halro-frontend-review-260905/review.test.tsx`；未改原 fixture。

覆盖包含两个 UI mutation、ConfirmButton retry、API 参数传输、route/middleware、step-up、key ID/事务碰撞、project replacement/validation、auth snapshot 到请求体上限。没有重新验证完整 MFA、角色矩阵、browser 网络断线、实际 API mutation、gateway 体积故障或生产接受度。两项独立确认以“组件定向重跑 + 服务端源码”标注，不能宣传为完整端到端运行。
