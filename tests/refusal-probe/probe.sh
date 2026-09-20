#!/usr/bin/env bash
# C 列取证：用一个被改坏的 key 打每家，记录拒绝形态。
#
# 证据归 docs/verification/route-eligibility-refusal-matrix.zh-CN.md（#318 / #319）。
# 这里放的是取证工具，不是测试：`go test ./...` 不会碰它，CI 只做语法检查。
# 上游的拒绝形态会变，所以留着重跑比留一份结论更有用——2026-09-20 首次执行时，
# 它测出 MiniMax 的 OpenAI 面用的是 Anthropic 形状的错误信封，文档里没有这件事。
#
# 不花任何 token：每家都在鉴权阶段就被拒，不会产生一次推理。
# 只打印状态码、错误体里的 code/type 字段、以及 Retry-After 是否存在。
# 不打印 key，不打印响应体全文。
#
# 用法：把你有的那几家的 key 填进环境变量，没填的会跳过。
#   OPENAI_KEY=sk-... ANTHROPIC_KEY=sk-ant-... ./refusal-probe.sh
#
# 想同时取 D 列（订阅未开通），把 key 换成真 key、模型换成你没买的那个再跑一遍。

set -u
BODY=$(mktemp); HDR=$(mktemp)
trap 'rm -f "$BODY" "$HDR"' EXIT

# 把 key 改坏：保留前缀形状，换掉尾部，避免"格式非法"和"密钥无效"两种拒绝混在一起
break_key() { printf '%s' "${1%??????}ZZZZZZ"; }

RAN=0
probe() {
  local name=$1 url=$2; shift 2
  RAN=$((RAN + 1))
  printf '\n=== %s ===\n' "$name"
  local code
  code=$(curl -sS -o "$BODY" -D "$HDR" -w '%{http_code}' --max-time 20 "$@" "$url" 2>&1) || {
    printf '  传输失败: %s\n' "$code"; return
  }
  printf '  HTTP %s\n' "$code"
  # 只取枚举字段，不取 message（可能含账号信息）
  python3 - "$BODY" <<'PY'
import json, sys
try:
    d = json.load(open(sys.argv[1]))
except Exception:
    print('  body: 非 JSON 或为空'); raise SystemExit
def pick(o, path=''):
    out = []
    if isinstance(o, dict):
        for k, v in o.items():
            if k in ('type', 'code', 'status', 'status_code', 'error_code', 'reason'):
                if not isinstance(v, (dict, list)):
                    out.append(f'{path}{k}={v!r}')
            out += pick(v, f'{path}{k}.') if isinstance(v, (dict, list)) else []
    elif isinstance(o, list):
        for i, v in enumerate(o[:3]):
            out += pick(v, f'{path}{i}.')
    return out
found = pick(d)
print('  枚举字段: ' + ('; '.join(found) if found else '（无 type/code 字段）'))
PY
  local ra
  ra=$(grep -i '^retry-after' "$HDR" | tr -d '\r')
  printf '  Retry-After: %s\n' "${ra:-无}"
}

J='-H content-type:application/json'
CHAT='{"model":"%s","max_tokens":1,"messages":[{"role":"user","content":"hi"}]}'

[ -n "${OPENAI_KEY:-}" ] && probe OpenAI https://api.openai.com/v1/chat/completions \
  -X POST $J -H "authorization: Bearer $(break_key "$OPENAI_KEY")" \
  -d "$(printf "$CHAT" gpt-4o-mini)"

[ -n "${ANTHROPIC_KEY:-}" ] && probe Anthropic https://api.anthropic.com/v1/messages \
  -X POST $J -H "x-api-key: $(break_key "$ANTHROPIC_KEY")" -H 'anthropic-version: 2023-06-01' \
  -d "$(printf "$CHAT" claude-sonnet-5)"

[ -n "${DEEPSEEK_KEY:-}" ] && probe DeepSeek https://api.deepseek.com/v1/chat/completions \
  -X POST $J -H "authorization: Bearer $(break_key "$DEEPSEEK_KEY")" \
  -d "$(printf "$CHAT" deepseek-chat)"

[ -n "${MINIMAX_KEY:-}" ] && {
  probe 'MiniMax (OpenAI 面)' https://api.minimax.io/v1/chat/completions \
    -X POST $J -H "authorization: Bearer $(break_key "$MINIMAX_KEY")" \
    -d "$(printf "$CHAT" MiniMax-M2)"
  probe 'MiniMax (Anthropic 面)' https://api.minimax.io/anthropic/v1/messages \
    -X POST $J -H "authorization: Bearer $(break_key "$MINIMAX_KEY")" -H 'anthropic-version: 2023-06-01' \
    -d "$(printf "$CHAT" MiniMax-M2)"
}

[ -n "${KIMI_KEY:-}" ] && {
  probe 'Kimi (OpenAI 面)' https://api.moonshot.ai/v1/chat/completions \
    -X POST $J -H "authorization: Bearer $(break_key "$KIMI_KEY")" \
    -d "$(printf "$CHAT" kimi-k2-0905-preview)"
  probe 'Kimi (Anthropic 面)' https://api.moonshot.ai/anthropic/v1/messages \
    -X POST $J -H "authorization: Bearer $(break_key "$KIMI_KEY")" -H 'anthropic-version: 2023-06-01' \
    -d "$(printf "$CHAT" kimi-k2-0905-preview)"
}

[ -n "${BIGMODEL_KEY:-}" ] && probe BigModel https://open.bigmodel.cn/api/paas/v4/chat/completions \
  -X POST $J -H "authorization: Bearer $(break_key "$BIGMODEL_KEY")" \
  -d "$(printf "$CHAT" glm-4.6)"

[ -n "${GEMINI_KEY:-}" ] && probe Gemini \
  'https://generativelanguage.googleapis.com/v1beta/models/gemini-2.5-flash:generateContent' \
  -X POST $J -H "x-goog-api-key: $(break_key "$GEMINI_KEY")" \
  -d '{"contents":[{"parts":[{"text":"hi"}]}]}'

if [ "$RAN" -eq 0 ]; then
  printf '\n没有打任何一家：没有设置 key 变量，七家全被跳过了。\n'
  printf '把 key 写在命令前面再跑，例如：\n'
  printf '  KIMI_KEY=sk-xxx MINIMAX_KEY=xxx %s\n' "$0"
  printf '可用变量：OPENAI_KEY ANTHROPIC_KEY DEEPSEEK_KEY MINIMAX_KEY KIMI_KEY BIGMODEL_KEY GEMINI_KEY\n'
  exit 1
fi
printf '\n打了 %d 家。把 HTTP 状态与枚举字段填进 docs/verification/route-eligibility-refusal-matrix.zh-CN.md 的 C 列，等级标 A。\n' "$RAN"
