# 阶段 2：Admin 控制台一致性审查发现

范围与口径见 [review-plan.md](review-plan.md) 阶段 2：只审前端**是否忠实表达后端约束**，不重审 UI
质量。分级同 [findings-backend.md](findings-backend.md)。编号接续（W 前缀）。

---

## 约束镜像

**W1【肯定】能力表镜像逐项核对无漂移**
前端手写镜像 `defaultProviderCapabilities`/`maxProviderCapabilities`
（`ProvidersPage.tsx:845-889`）与后端 `DefaultProviderCapabilities(ForProfile)`/
`MaxProviderCapabilitiesForProfile`（`models.go:592-673`）逐 provider、逐 profile 对照：
openai（含 chat/media 双 binding 拆分 `:964-970` 对应双 profile）、anthropic（含
`provider_executed_tools` 仅入 ceiling 不入 default，两侧一致）、deepseek、openai_compatible、
gemini（`stream_usage:false` 对应后端无 StreamUsage）、bedrock 六个 profile（含 mantle responses
无 reasoning ↔ 前端 `reasoning: profileID === chat.v1` `:876`、mantle anthropic 无 json_mode）——
**全部一致**。镜像注释明确指认权威（`:863-864`、`:881`、`:910`、`:917`、`:954`）。

**W2【肯定】数值/格式镜像一致**
`MAX_PROJECT_NAME = 128`（`ProjectsPage.tsx:35` ↔ `models.go:201`）；
`maxBedrockProjectIDLength = 128`、`proj_` 格式与 `wrkspc_` 拒绝、`default` 归一
（`ProvidersPage.tsx:912-938` ↔ `domain.NormalizeBedrockProjectID`/`ValidateBedrockProjectID`）；
beta token 上限 16/128、字符集、去重（`:941-956` ↔ `domain.ValidateAnthropicBetaTokens`）。
前端校验自述定位正确：「server stays the authority; this only keeps a refusal from arriving as a
bare 400」（`:920-922`）。

**W3【建议】镜像无 CI 守护**
上述镜像全部是手写副本，靠注释维系。任何一侧改动（如后端加 capability、调上限）不会被测试拦下，
只会在运行期变成后端 400。建议：加一个 contract 测试（Go 侧导出 JSON 表 → 前端测试断言相等，或反向），
把 W1/W2 的一致性从「本轮核对过」升级为「持续成立」。

**W4【问题】ProjectsPage 比后端更严：不允许零别名 Project**（镜像偏差）
zod schema `routes: z.array(z.string()).min(1)`（`ProjectsPage.tsx:39`），而后端
`Project.Validate` 不要求非空、`validateProjectReferences` 仅在 `len > 0` 时检查
（`admin_projects.go:438`）。后端允许「先建空 Project 再挂别名」的顺序，控制台不允许。同类偏差：
新建时零可选别名集只给告警链接（`:508`），已选中但全禁用的别名保留显示为 unavailable（`:503`）——
这半边与后端注释「disabled route stays bindable — the console surfaces it as unavailable」
（`admin_projects.go:449`）吻合；但**从未绑定过的全禁用别名不进入可选列表**
（`:415` 过滤 `enabledCount > 0 || retained`），后端允许绑定它。方向都是前端更严（fail-closed），
无安全后果，但契约上控制台无法表达两种后端合法状态。判定：若这是产品决策，在
`docs/contracts/` 记一句；若不是，放开 `min(1)` 与过滤。

**W5【肯定】表单发全量替换体，字段不静默丢失**
Route 状态开关复用编辑表单的全量体并有注释说明为何不能发部分体（`RoutesPage.tsx:26-35`、`:55-57`）；
Project 更新保留不可编辑字段（token 上限、请求体上限，`ProjectsPage.tsx:455-458`）；Credential 过期
字段显式发 `null` 清空而非缺省保留（`ProvidersPage.tsx:458-461` 注释）。

---

## 链路引导

**W6【肯定】四级建链顺序全部强制且断链有指路**
Credential 缺失 → 禁用「新增 Provider」+ 提示（`ProvidersPage.tsx:178`）；Deployment 表单只列
enabled Provider（`DeploymentsPage.tsx:739`，保留 current 防编辑自锁）、只列 enabled binding
（`:710`）；Route 表单只列 enabled Deployment（`RoutesPage.tsx:211`，同样保留 current），零可选时
给告警+跳转链接（`:249-250`）；Project 表单别名集尊重后端规则（见 W4）、零别名时告警+跳转
（`:508`）。每处断链都是「解释 + 去上一跳的链接」，不是裸禁用。

**W7【肯定】别名容量语义在 UI 里成立**
关路由前统计同别名兄弟数用于确认文案（`RoutesPage.tsx:68-75`）；Project 别名选项显示
enabledCount 与 strategy（`ProjectsPage.tsx:503`）——与后端「别名粒度授权 + 候选组容量」模型一致
（对应后端 F19 的语义半边）。

**W8【建议】前端已经把这个字段叫「模型」了**
i18n 标签是 `allowedModels: "Allowed model aliases"`（`en-US.ts:837`），表单字段名却仍是
`routes`/`allowed_routes`（`ProjectsPage.tsx:39`、`:450`）。UI 文案先于 schema 承认了 F19 的判定。
F19 改名落地时前端只需动 wire 字段名，文案已就位——把此项并入 F19 整改。

---

## revision 冲突

**W9【肯定】无静默覆盖路径**
所有 update/delete 走 `If-Match`（`api.ts:88`，revision 来自已加载记录 `:243`、`RoutesPage.tsx:58`
等）；ETag 从响应回传（`:114`）；409/412/428 统一映射为「The data was changed by another operation.
Refresh and try again.」（`errors.ts:94` → `en-US.ts:56`），400/409/422 的服务端原句以 detail 形式
保留在标题下（`errors.ts:102-130` 注释说明取舍）。带业务码的冲突（`route_referenced_by_project`、
`binding_referenced_by_deployment`、幂等回放族）各有专句并带跳转动作（`components.tsx:133`）。
创建用 `useRef(crypto.randomUUID())` 幂等键，一表单一键，重试同键、二次创建新键
（`RoutesPage.tsx:219-222` 注释）。

**W10【建议】冲突后不自动刷新**
412 后文案让用户「Refresh and try again」，但表单不自动重拉最新 revision/字段 diff。低频操作可接受；
若要改，优先级低于 W3。

---

## 秘密驻留

**W11【肯定】三类秘密全部内存态，出口有 CI 闸**
CSRF token：模块级变量（`api.ts:47`），仅登录/MFA 响应赋值（`:152`/`:160`/`:164`），不落任何存储；
Gateway Key 明文：只存在于可自守关闭的对话框内，列表列「never carries the secret」
（`ProjectsPage.tsx:578-598` 注释），Enter 连发防重复铸键（`:632`）；Credential secret：仅请求体，
`onSettled` 即清 state（`ProvidersPage.tsx:466`），step-up TOTP 同处清空；主题偏好注释明言五种
浏览器持久化全禁（`theme.ts:9-11`）。产物层有 canary 扫描兜底
（`web/scripts/check-artifacts.mjs:20-24`：provider-secret/gw_plaintext/csrf/password 四类标记）。
grep 全源无 `localStorage`/`sessionStorage`/`IndexedDB`/`document.cookie` 写入。

---

## 汇总

| 级别 | 条目 |
|---|---|
| 疑似BUG | 无 |
| 问题 | W4（零别名 Project 与禁用别名绑定：前端比后端严，两种后端合法状态不可表达） |
| 建议 | W3（镜像缺 CI 守护）、W8（并入 F19）、W10（冲突后不自动刷新） |
| 肯定 | W1、W2、W5、W6、W7、W9、W11 |

**契约漂移交叉引用**（方案要求单列）：
- W4 ↔ 后端 `admin_projects.go:449`：后端注释描述的控制台行为只对「已绑定」的别名成立，新绑定被
  前端收窄。轻微漂移，方向安全。
- W8 ↔ F19：同一个名实问题的两侧。前端文案层已完成改名，schema 层待后端 F19 一起动。
- W1/W2 当前无漂移，但属 W3 描述的无守护镜像，漂移风险敞口在。

阶段 2 无需进对抗验证的条目（无可达性争议）；W4 进设计裁决（与 F19 同批）。
