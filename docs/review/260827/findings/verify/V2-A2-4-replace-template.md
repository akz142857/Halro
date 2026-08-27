# V2 · A2-4 证伪：replace 模板可清空 citation.URL

## 裁决：CONFIRMED

原发现的五个环节逐一独立复核，**全部成立**，且实测复现。未找到任何一道拦截防御——
从 Admin 表单到域校验到 `compileRule` 到出站脱敏到 `RenderResponseResult`，
没有一处检查 replace 模板的展开结果。

---

## 证据

### 1. `"$1"` 展开为空——前提成立，且比原报告更宽

独立最小程序（`regexp.ExpandString`，Go 1.26，非记忆断言）实测：

| 正则 | 模板 | NumSubexp | 展开结果 |
|---|---|---|---|
| `https?://\S+` | `$1` | 0 | `""` |
| `https?://\S+` | `${1}` | 0 | `""` |
| `https?://\S+` | `$2x` | 0 | `""` |
| `https?://\S+` | `$name` | 0 | `""` |
| `(?:x)?(foo)?bar` | `$1`（组未参与匹配） | 1 | `""` |
| `(a)(b)` | `$3`（越界引用） | 2 | `""` |
| `https?://\S+` | `$0` | 0 | `"https://example.com/x"`（非空） |
| `https?://\S+` | `$$` | 0 | `"$"`（非空） |

即：**无捕获组时 `$n` 展开为空**；**有组但该组未参与匹配时同样为空**；
**引用越界或引用不存在的命名组也为空**。原报告只举了"无捕获组"一种，
实际触发面是四种。展开点：`internal/redaction/engine.go:836`
（`r.pattern.ExpandString(result, replacement, value, location)`）；
调用方 `internal/redaction/engine.go:639`（`case "replace"`）。

### 2. 配置路径与校验层——三层全部只做 TrimSpace 非空

运维路径：Admin 控制台 → 脱敏策略 → 规则 kind=`regex`、action=`replace`、
scope 含 `outbound`、pattern 能整串命中 URL、replacement 填 `$1`。三道可能的关口：

- **前端**：`web/src/pages/RedactionPoliciesSection.tsx:61`
  `if (rule.action === "replace" && !rule.replacement?.trim()) problems.push("replacement")`
  ——只查 trim 非空。输入框 `:516` 仅有 `maxLength={256}`，无模板校验。
- **域校验**：`internal/domain/redaction.go:126`（trim 非空）与 `:129`（≤256 字节）。
  无第三条与 Replacement 有关的规则。
- **编译期**：`internal/redaction/engine.go:748-791` 的 `compileRule` 只
  `regexp.Compile` + `syntax.Parse`（算 `ComputedMaxMatchBytes`），
  **完全不读 `rule.Replacement`**——不校验捕获组数量、不做模板与正则的配对校验。
  `CompilePolicy`（`engine.go:129-159`）在其之上只加了 bounded_stream 宽度与
  detect_only_stream 动作两条约束，同样不碰模板。
  保存链路：`internal/app/admin_redaction.go:264` 原样透传 → `:267` `redaction.CompilePolicy`。
- **`redaction_policy.test` 试跑端点**（`admin_redaction.go:230-241`）存在，但是**可选的**
  预览工具，返回 `matches` 计数，不是保存前的强制 dry-run，也不检查替换结果。

**结论：不存在第二道校验。** `$1` 一路直达运行时。

### 3. 作用域能选中 citation.URL——能，且没有字段级 target

规则只有 `Scopes`（inbound/outbound）这一个维度，**没有字段级 target**
（`domain.RedactionRule` 无此字段，`internal/domain/redaction.go:9-22`）。
出站遍历 `processCitations`（`internal/redaction/engine.go:397-424`）对每条 citation
无条件调用 `e.ProcessText(policyID, scope, citation.URL)`（`:412`）与 `.Title`（`:416`）。
即：**任何 outbound replace 规则只要命中 URL 字符串就会改写它**。路径可达。

前置条件（收窄触发面，但不阻断）：citation 只由 OpenAI Responses **上游 profile** 产生
（`internal/compatibility/openai/provider_responses.go:195-215`，`decodeProviderAnnotations`，
唯一的 `semantic.Citation{}` 构造点）。需要：#231 新增的 Responses profile 路由 +
调用方请求 `web_search` + 路由已接受 provider-executed tools + 上游返回 `url_citation`。
另需规则**整串**命中 URL（部分命中只会得到非空残串，Validate 照过）。

### 4. `Message.Validate` 对空 URL：失败，不是丢弃

`internal/semantic/content.go:141-143`：

```go
if citation.URL == "" || citation.StartIndex < 0 || citation.EndIndex < citation.StartIndex ||
    citation.EndIndex > len(part.Text) {
    return errors.New("semantic citation does not point into its text")
}
```

**报错返回，没有任何"丢弃这条 citation"的分支。** 与 `decodeProviderAnnotations` 的处理
对称性相反：解码侧对越界 span 是 clamp（`provider_responses.go:207-209`），对空 URL 是拒绝
（`:201`）——但那是**入站**拒绝上游脏数据；出站这条空 URL 是**网关自己造出来的**。

### 5. 账目状态——实测：账本记 success/200，调用方收 502

写了 gateway 包内探针（已删除），让 `RenderResponseResult` 在
`generate()` 返回**之后**失败，观察 `f.accounting.AddObserver` 的全部 ledger 记录：

```
caller sees: status=502 code="provider_error" message="provider response cannot be rendered safely"
provider calls = 1
kind=4 (attempt settle): committed_micros_usd=20, provider_input_tokens=10,
        provider_output_tokens=5, outcome="success", http_status=200
kind=5 (run finalize):   outcome="success"
```

逐条对照原报告的断言：

- attempt outcome = **`success`**，且 `http_status: 200` ✔
- run outcome = **`success`** ✔
- 钱**已记**：`committed_micros_usd: 20`（保留 43 → 结算 20）✔
- 调用方状态码 = **502**（`internal/gateway/service.go:1110` → `:3050-3057` `returnFailure`，
  `gatewayError("provider_error", ..., 502, cause)`）✔

顺序：`attempt.finish`（`service.go:1035`）→ `run.finalize(outcome)`（`:1046`）→
返回 `semanticResponse` → `Responses()` 调 `RenderResponseResult`（`:1109`）→ 失败 → `:1110` 502。
`generate()` 内**没有任何 `result.Validate()`**（全文件唯一的 `.Validate()` 在 `:2443`，与此无关）。

**按判定标准**：上游真花了钱，"不退款"确实是正确的；问题不在退款，而在
**账本写 `outcome=success` / `http_status=200`，调用方却收到 502 `provider_error`**——
这是账目与事实不自洽，且 502 `provider_error` 把网关自身的缺陷归因给上游，排障会指错方向。

### 6. 其他防线：无

- 前端不阻止（`:61` 见上）；
- 保存无强制 dry-run（`redaction_policy.test` 是可选预览）；
- `regexp.Compile` 不会失败（`$1` 在**模板**里，不在正则里，正则本身合法）；
- `mask` 动作不受影响（`engine.go:640-642` 的 `masked()` 恒非空）；
- 字面量 replace 不受影响（trim 非空已保证）；
- 出站脱敏后**无** `Message.Validate` / `GenerateResult.Validate` 兜底。

---

## 与原报告的差异

1. **触发面比原报告宽**。原报告只列"无捕获组时写 `$1`"。实测还有三种同样通过全部校验、
   同样展开为空的写法：捕获组存在但未参与匹配（`(foo)?` 之类）、`$n` 越界引用、
   引用不存在的命名组 `$name`。修复若只判"无捕获组却引用 `$n`"，会漏掉后两种。
2. **原报告未点明前置条件的窄度**。citation 只能来自 OpenAI Responses 上游 profile 的
   `url_citation` 注解（唯一构造点 `provider_responses.go:212`），需要 web_search 链路成立；
   且规则必须**整串**命中 URL。这不改变"是缺陷"的判定，但影响严重度定级——
   触发需要"特定 profile + web_search + 运维写错模板"三者同时成立。
3. **账目形态原报告是从代码推的，本轮是实测的**，结论一致：`kind=4 outcome=success
   http_status=200 committed=20` 与 502 并存。多一条原报告没说的细节：attempt 记录里
   `http_status` 明确写着 `200`，比 `outcome` 字段更直白地记录了一个从未发生的成功响应。
4. **不只是 citation.URL**。同一台机制在入站会清空 `ContentInputImage.URL`
   （`engine.go:339-345`），使 Validate 的 "semantic image content is missing url"
   条件成立（`content.go:151-153`）。但入站发生在预留之前，且 `generate()` 入站侧同样
   没有 Validate，实际后果是把 `url:""` 发给上游 → provider_error → 保守记账，
   不构成同一个"已计费 success + 502"缺陷。附带记录，不并入本条。

---

## 若成立，最小修复建议

两处，第一处是根治，第二处是操作面上的早失败。

**(1) 出站脱敏后补一次 `Validate`，落在 `finalize` 之前**（根治整类，不止本条）。
`internal/gateway/service.go:1038-1044`，把

```go
semanticResponse, err = s.redactor.ProcessOutboundGenerateResult(...)
outcome := "success"
if err != nil { outcome = "policy_rejected" }
```

改为在 `err == nil` 时追加 `err = semanticResponse.Validate()`，落到同一个
`policy_rejected` 分支。效果：attempt 仍记 success 并计费（上游确实花了钱，正确），
run 记 `policy_rejected`，调用方收 422 `sensitive_output_detected` ——
与 A2-5 描述的出站 default 分支同一种 fail-closed 形态，一致且诚实。
这同时覆盖 #231 修过的 span 半边、本条的 URL 半边，以及将来任何一个
"脱敏把某字段改成 Validate 不接受的值" 的新字段。同一处修改需同步
`service.go:1327/1337`、`1520/1530`、`2050/2065` 等其余 finish/finalize 配对点。

**(2) `compileRule` 拒绝会展开为空的 replace 模板**（`engine.go:748-791`，
在 `regexp.Compile` 成功之后）。判定不能只查"有没有捕获组"——按上面第 1 条，
未参与匹配的组同样展开为空。可行的保守判定：解析 `Replacement` 中的所有
`$n` / `${n}` / `$name` / `${name}` 引用，要求

- 每个数字引用 `n <= pattern.NumSubexp()`，每个命名引用在 `pattern.SubexpNames()` 中存在；
- **且** 模板在所有引用之外至少含一个非空白字面字节（保证展开结果恒非空）。

第二条是必要的：只做引用有效性检查挡不住 `(foo)?` 这种可选组。
拒绝信息应明说"replace 模板可能展开为空串"。这条让运维在保存时就看到问题，
而不是等到线上一次已计费的 502。
