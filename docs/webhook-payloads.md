# Alert webhook payloads

Heimdall v1 sends one minimal generic JSON document. Provider credentials,
Gateway keys, source IPs, prompts, response bodies, and raw errors are never
included.

```json
{
  "id": "alt_...",
  "type": "token_guard_blocked",
  "severity": "critical",
  "dedup_key": "prj_...:token_guard_blocked:tokens_per_minute",
  "summary": "Token Guard state changed",
  "project_id": "prj_...",
  "timestamp": "2026-07-31T12:00:00Z",
  "details": {
    "reason": "tokens_per_minute",
    "status": "temporarily_blocked",
    "expires_at": "2026-07-31T12:05:00Z"
  }
}
```

Webhook URLs must use HTTPS and pass Heimdall's SSRF policy. Authentication
belongs in an encrypted `Authorization` or `X-Webhook-Token` header, not in URL
query parameters.

## Platform adapters

Heimdall intentionally keeps platform-specific formatting outside the Gateway
process in v1. A small trusted relay can map the generic event as follows.

Slack:

```json
{"text":"[critical] Token Guard state changed: tokens_per_minute"}
```

Feishu:

```json
{"msg_type":"text","content":{"text":"[critical] Token Guard state changed: tokens_per_minute"}}
```

WeCom:

```json
{"msgtype":"text","text":{"content":"[critical] Token Guard state changed: tokens_per_minute"}}
```

Discord:

```json
{"content":"[critical] Token Guard state changed: tokens_per_minute"}
```

The relay should retain only the minimum fields needed by the destination and
must not add prompts, raw IP addresses, credentials, or unredacted errors.
