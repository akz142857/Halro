# A2 · 安全（脱敏专项）findings

范围 `v0.3.0(abfc05c)..HEAD(8bb4847)`，核心 `internal/redaction/engine.go`（+171/−25）。
全部行号按 2026-08-27 工作区 HEAD；v0.3.0 行号来自 `git show v0.3.0:<path>`。
实测手段：`go test ./internal/redaction/ -count=1`（全绿，3.2s）；四条探针测试临时置入包内运行后删除
（未留在仓库）；v0.3.0 侧用独立 worktree 对同一输入实测（Verify, never assume：两边都跑了真代码）。

---

## A2-1 三条新路径的双重约束（靶子 1）

- 位置：internal/redaction/engine.go:329-338、354-364、397-426
- 基准：B2/B3 · 【肯定】 · 不阻塞

逐路径的施加点（均经 `ProcessText` → `processRaw` → `processString`）：

| 路径 | 入口行 | 强制基线施加点 | 项目策略施加点 |
|---|---|---|---|
| `ContentProviderToolCall.Text`（action.query） | engine.go:360 | engine.go:604（入站 validateMandatory）/ 609（出站 sanitizeMandatoryOutbound） | engine.go:614-646 规则循环 |
| `Citation.URL` / `Citation.Title` | engine.go:412 / 416 | 同上 | 同上 |
| `ContentReasoning`（与 text/tool_result 同 case） | engine.go:329-330 | 同上 | 同上 |

三条路径双重约束齐全。策略缺失时 fail-closed（engine.go:625 `ErrPolicyUnavailable`，非空
policyID 找不到即拒，engine.go:614-625 注释写明快照替换窗口）。同 case 顺带确认：
`part.CallID`/`part.Name` 对所有 kind 施加强制基线（engine.go:373-377），与注释声明的
"标识符拒绝而非改写"一致。

但**同一 traversal 里有两条字符串路径两者皆漏**，见 A2-2、A2-3。

## A2-2 `ContentInputImage.Detail` 跳过全部脱敏，且是相对 v0.3.0 的回归

- 位置：internal/redaction/engine.go:339-346（case 只处理 `part.URL`，不碰 `part.Detail`）
- 基准：B2（fail-open 方向）/ B3 · 【疑似BUG】 · **阻塞候选**（fail-open 类，交对抗验证裁决）

`Detail` 是调用方自由字符串，全链无枚举校验：wire 直传
（internal/compatibility/openai/responses.go:182、mapping.go:284），`Message.Validate` 不查它
（internal/semantic/content.go:151-154），渲染时原样发给上游（mapping.go:324-325）。

实测两边：
- HEAD：`Detail: "sk-…"` 经 `ProcessInboundGenerate` **原样放行**（探针测试确认）。
- v0.3.0：同一 secret 放进 Chat wire 的 `image_url.detail`，`ProcessInboundChat` **拒绝**
  （worktree 实测 `request contains secret material`）——旧通道对 message.Content 整个 JSON 走
  `transformValue`，detail 作为 map 值被覆盖（v0.3.0 engine.go:263、425-454）。

即 engine.go:266-268 注释 "What is inspected is unchanged from that pass, member for member"
**不成立**：detail 这一 member 从被检查变成不被检查。修法极小：在 `ContentInputImage` case 对
`part.Detail` 加一次 `ProcessText`（或在 wire/semantic 校验层把它钉成 low/high/auto 枚举，
使该字段根本装不下任意文本——后者更符合"检查面=接受面"）。

## A2-3 `ContentProviderToolCall.Status` 无任何检查（基线与策略双漏）

- 位置：internal/redaction/engine.go:354-364（只处理 Text）；373-377（只查 CallID/Name）
- 基准：B2/B3 · 【问题】 · 不必然阻塞，建议随 A2-2 一并修

解码只要求非空（internal/compatibility/openai/provider_responses.go:161），语义校验只要求非空
（content.go:174），渲染原样回调用方（responses.go:306-311）。这是一条**上游控制**的自由字符串
抵达调用方而强制基线与项目策略都没跑过——与 #231 修 action.query 的缺陷同构，只是载体换成
status。实测确认 secret 放进 Status 原样通过 `ProcessOutboundGenerateResult`，同一 secret 放进
Text 则被 `[REDACTED]`。实际风险低于 action.query（OpenAI 实值是 completed/failed 一类枚举，
且 status 不承载会话内容），但既然解码端不钉枚举，检查面就小于接受面。修法同上二选一：
脱敏覆盖它，或解码端钉枚举。

## A2-4 replace 模板可清空 citation.URL → 已计费成功调用以 502 抵达（B8 同类复现）

- 位置：internal/redaction/engine.go:836（`ExpandString` 按模板展开）＋
  internal/domain/redaction.go:126（只查 `TrimSpace(Replacement) != ""`）＋
  internal/compatibility/openai/responses.go:222-224（`RenderResponseResult` 先 `result.Validate()`）
- 基准：B8 · 【疑似BUG】 · **阻塞候选**（触发面窄：需运维自建特定规则；但缺陷类别正是 #231 刚修过的）

链条：`Replacement: "$1"`（无捕获组）通过 domain 校验（"$1" TrimSpace 非空），但
`ExpandString` 展开为空串；规则整串命中 `citation.URL` 后 URL 变 ""；`Message.Validate` 要求
citation.URL 非空（content.go:141-143）→ `RenderResponseResult` 报错 → facade 以 502 返回
（service.go:1110 "provider response cannot be rendered safely"）——此时 `attempt.finish`
（service.go:1035）与 `run.finalize("success")`（1046）都已提交。**这正是 #231 提交信息里
span 缺陷的账目形态（"billed reached the caller as 502"），经另一个字段复现。**

实测确认（探针测试：URL 展开为 `""`，Validate 报 "semantic citation does not point into its
text"）。mask 与非模板 replace 不受影响（`[MASKED]`/字面替换非空）。修法建议其一：
compileRule 对 replace 规则拒绝会展开为空的模板（无捕获组却引用 `$n`）；或 processCitations
在 URL 被改写为空时按 reject 处理（fail-closed 在计费语义上已无救，但至少错误诚实）。

## A2-5 default 拒绝分支不可绕过（靶子 2）

- 位置：internal/redaction/engine.go:365-371
- 基准：B2 · 【肯定】 · 不阻塞

semantic 内容种类全集共 6 个（internal/semantic/content.go:68-80），逐一落位：
`text`/`tool_result`/`reasoning` → engine.go:329；`input_image` → 339；`tool_call` → 348；
`provider_tool_call` → 354。今日 default 不可达。

将来新增种类的到达序：wire 解码器先拒未知 part/item 类型（responses.go:142-143、183-184）→
`Message.Validate` default 拒未知 kind（content.go:178-179，解码时即调用，responses.go:187）→
redaction default 兜底。即新种类必须同时改解码器与 Validate 才能到达 redaction；届时若漏加
case，入站在预留之前 400（service.go:967-969），出站在 finish/finalize 之后 422（1052-1054，
保守记账不退款）——两个方向都 fail-closed，不会被静默吞掉。native 与流式路径不走
processContent，但按原始 JSON 整树遍历（inspect.go:43-61，连 member 名与数字都查），新种类
结构性覆盖。

小疵（建议级）：入站命中 default 时错误映射为 `sensitive_data_detected` /
"request contains secret material"（service.go:969）——对"遍历不认识这个种类"是错误的说法，
排障时会误导。

## A2-6 clone 覆盖完整；v0.3.0 曾就地改写，HEAD 修复（靶子 3）

- 位置：internal/redaction/engine.go:283-284、324-325、408-409
- 基准：B5 相关 · 【肯定】 · 不阻塞

共享可变结构逐点核对：messages 槽拷贝（283-284）→ Content 槽拷贝（324-325）→ Citations 槽
拷贝（408-409）；`Citation` 元素是纯值类型（string/int，content.go:88-93），槽拷贝即深拷贝；
`Arguments` 是不可变 string；`Tools`、`OutputFormat.Schema`（[]byte，可变）被遍历**不改写**，
无共享写入点。错误中途返回原 `parts`/`citations`，不留半改状态。重试场景：入站脱敏在 attempt
循环之前一次完成（service.go:967），各 attempt 路由同一份已脱敏请求，出站遍历只碰
per-attempt 的 result——两个方向都不存在"第一次 attempt 改写第二次要路由的 message"。

反面对照：v0.3.0 `ProcessInboundChat` 确实就地改写调用方 slice
（v0.3.0 engine.go:260-262 `message := &request.Messages[index]`）。HEAD 的
TestOutboundRedactionDoesNotMutateTheCallersCitations（provider_tool_content_test.go:114）
守住 citations 一点。

## A2-7 span 归零必过 Validate；H4 的语义合并成立且未落文档（靶子 4）

- 位置：internal/redaction/engine.go:421-423；internal/semantic/content.go:140-145
- 基准：疑问（H4） · 【肯定＋疑问】 · 不阻塞

归零后 `(0,0)` 对任意文本（含空串）满足 Validate 全部条件（0≥0、0≥0、0≤len），实测三种长度
通过——"span 起点超出新文本长度"不可能发生，两端都被置 0。B8 边界（S6）在 span 这条线上封住。

H4 实答：脱敏归零（engine.go:421-423）与入站解码 clamp（provider_responses.go:208-216）产出
同一形状 `(0,0)`；渲染加 offset 后仍是零长（responses.go:299）。下游（调用方 wire、审计、前端）
**无法区分**"来源准确但位置因脱敏不可知"、"上游给的位置本来就放不下"与"上游真给了 0,0"。
两处注释互引（"which is what the inbound decoder already does"），合并是有意的取舍而非疏忽；
但该合并只活在代码注释里，openai-compatibility.md / endpoint-manifests 未写"零长 span 表示
位置不可信"。建议补一句对外说明；不构成阻塞。

## A2-8 H5：reasoning 脱敏取舍未落笔，且代码注释与实现相反（靶子 5）

- 位置：internal/redaction/engine.go:274-277（注释）vs 329（实现）
- 基准：疑问/B3 · 【问题】 · 不阻塞，但注释必须改

实现：`ContentReasoning` 与普通文本同 case 脱敏（329）。#231 提交信息写明取舍（"redacted like
any other model-written text, because the alternative was writing down a known leak as
deliberate"）。但**同一提交**的 `ProcessInboundGenerate` 头注释写着 "Reasoning text is not
inspected, which is also unchanged"——与代码相反，且把 fail 方向说反（安全注释声称存在一个
实际不存在的豁免）。文档面：`docs/contracts/provider-capabilities.md`、`openai-compatibility.md`、
`docs/architecture/threat-model.md` 均无 reasoning 脱敏策略的落笔（grep 零命中），该产品决定
目前只存在于提交信息里。要做两件事：删掉/改写 engine.go:274-277 那段注释；在
provider-capabilities.md（或 threat-model）加一句"推理内容按模型输出文本施加同一脱敏策略"。

## A2-9 #231 fail-open 反面复核：422 的发出点与账目状态（靶子 6）

- 位置：internal/gateway/service.go:1052-1054（422 发出）；1032-1035、1046（账目）
- 基准：B1/B8 · 【肯定】 · 不阻塞

reject 规则命中 action.query 的完整链：engine.go:360 → processString 634-637 返回 `MatchError`
→ service.go:1039-1044 置 outcome=`policy_rejected` → `run.finalize` 先提交（1046-1050）→
**422 在 service.go:1052-1054 发出**（`sensitive_output_detected`）。发出时账目状态：attempt 已按
成功结算（1032-1035，`settlementForResult` 以上游真实用量计费），run 以 `policy_rejected` 收尾，
无静默退款——与 B1"语义不明/策略拒绝保守记账"一致，且拒绝先于任何响应字节到达客户端。
泄漏面：`MatchError.Error()` 固定文案（engine.go:57-59），`Error.Message` 不携带命中内容
（service.go:58-60）。回归测试在 provider_tool_content_test.go:49（query 与 citation 双向覆盖）。
S5 的"答案 [REDACTED]、action.query 原文"不对称在此版不复存在。

## A2-10 测试与到达性备注

- 位置：internal/redaction/provider_tool_content_test.go（5 个测试全为 Outbound 方向）
- 基准：疑问 · 【建议】 · 不阻塞

入站方向 `ContentProviderToolCall`/`Citations` 目前**不可达**：Responses 入站回放只接受
message/function_call/function_call_output 三种 item（responses.go:117-143，default 拒绝
web_search_call），入站消息解码也不产 citations（173-186）。processContent 的入站分支是休眠的
纵深防御，现状 fail-closed 正确。两点顺带：一，这意味着带 web_search 的多轮会话无法把上一轮
输出原样回放进该 stateless facade（属 A6 契约面，仅记录）；二，若将来放开回放，入站用例现在
一条都没有，建议先补（inbound 的 query/citation reject + mandatory 各一条）。

---

## 汇总

| 编号 | 分级 | 基准 | 阻塞 |
|---|---|---|---|
| A2-1 | 肯定 | B2/B3 | 否 |
| A2-2 | 疑似BUG | B2/B3 | **候选** |
| A2-3 | 问题 | B2/B3 | 否（建议随 A2-2 修） |
| A2-4 | 疑似BUG | B8 | **候选** |
| A2-5 | 肯定（含一条建议） | B2 | 否 |
| A2-6 | 肯定 | B5 | 否 |
| A2-7 | 肯定＋疑问 | H4 | 否 |
| A2-8 | 问题 | 疑问/B3 | 否 |
| A2-9 | 肯定 | B1/B8 | 否 |
| A2-10 | 建议 | 疑问 | 否 |
