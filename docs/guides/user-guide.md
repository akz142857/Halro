# Halro User Guide

This guide covers trying Halro locally, configuring it as an administrator,
and connecting an application to it. For production deployment, upgrades, backup
and restore, and security hardening, read the [Operator Guide](operator-guide.md)
alongside it.

A Simplified Chinese edition of this guide is available at
[user-guide.zh-CN.md](user-guide.zh-CN.md).

## 1. The objects in the system

A request travels from an internal Gateway Key to a real model like this:

```text
Application
  └─ Gateway Key
      └─ Project (budget, RPM, TPM, concurrency, allowed models, policies)
          └─ Route (public model alias, for example chat)
              └─ Deployment (upstream model, price, capabilities, concurrency)
                  └─ Provider (upstream address, type)
                      └─ Credential (the platform secret, encrypted at rest)
```

An application only ever handles a `gw_...` Gateway Key and a public model alias.
It never receives the Provider API key, and it does not need to know the real
upstream model name.

## 2. Running it locally

### 2.1 Requirements

- Go 1.26.6 or newer;
- Node.js and npm, only if you are building the React admin console from source;
- macOS, Linux, or any other Go target platform the project supports.

Get the source and enter the directory:

```bash
git clone https://github.com/akz142857/Halro.git
cd Halro
```

First run needs one command:

```bash
make start
```

It installs the frontend dependencies if needed, builds `bin/halro`, writes a
`config.yaml` that listens on the loopback address only, initializes encrypted
local storage, and starts the service. The React assets are embedded in the
binary, so there is no separate frontend process at runtime.

The `config.yaml` written on first run is annotated: every setting that has a
consequence says what it decides. Deleting a key restores its default.

### 2.2 First-time setup in the browser

The terminal prints the Admin address:

```text
Admin: http://127.0.0.1:8081/admin/setup
```

Open it and set an administrator username and a password of at least 8
characters — a memorable long passphrase is a good choice. The password is
stored only as an Argon2id hash in local metadata, never in `config.yaml`. Once
setup succeeds the entry point closes permanently, and the browser is given a
secure session into the console.

Compatibility note: the administrator password minimum changed from the older
rule of 12 UTF-8 bytes to 8 Unicode code points. This is a deliberate product
change, not an equivalent refactor: a pure-ASCII password now needs 8 characters
rather than 12, while a multi-byte password — Chinese, for instance — no longer
earns a lower character requirement by virtue of its encoding. In production,
use a passphrase noticeably longer than the minimum.

Automatic initialization creates:

- `master.key`: the local Master Key, mode `0600`;
- `data/halro.db`: metadata;
- `data/ledger/`: the authoritative usage ledger;
- `data/audit/`: the audit chain;
- Usage checkpoints and Parquet data, written as the system runs.

Back up the Master Key separately from the data directory. If it is lost,
Provider credentials cannot be recovered. Running `make start` again never
overwrites configuration, the Master Key, or data. If the state is partial —
only the Master Key, or only the metadata — Halro refuses to repair it
automatically and asks for the matching files to be restored by hand.

If Admin listens on a non-loopback address over TLS, startup also prints a
one-time Setup Token that the page must submit alongside the form. It exists
only in the running process and rotates on restart.

### 2.3 Starting it again

The same command:

```bash
make start
```

Once an administrator exists, the normal sign-in page is shown instead. Stop the
service with `Ctrl+C`.

### 2.4 Appearance and interface language

Both the setup page and the sign-in page have a language selector in the top
right. The console fully supports Simplified Chinese and English, and switching
does not reload the page.

After signing in, **Settings → General** configures the current administrator's
**appearance** and **interface language**:

- **Appearance**: light or dark, applied immediately; if saving fails the value
  the server confirmed is restored and a retry is offered;
- **My language**: stored on the administrator account, so it follows that
  account into another browser;
- **Instance default language**: used by signed-out pages, and by any
  administrator who chose "follow the instance default".

Appearance defaults to dark and offers only light and dark. It is stored on the
server with the language preference and restored when the same account signs in
from a new browser or session; signing out, the sign-in page and the setup page
are always dark. Appearance is not written to `localStorage`, `sessionStorage`,
cookies, or IndexedDB.

Personal preferences and the instance default are stored separately, so a
failure saving one never shows the other as failed. Language resolves in this
order: administrator preference, instance default, browser language, built-in
Simplified Chinese. The instance default is a global setting and changing it is
written to the Audit chain; the administrator preference carries its own
revision and uses `If-Match` so two open pages cannot overwrite each other.
Protocol fields — the Gateway API, Provider model names, error codes, audit
enumerations — are never translated at the protocol layer. Language preferences
live in server-side metadata only, never in browser storage.

### 2.5 Headless and automated deployment

```bash
./bin/halro init --config ./configs/config.example.yaml
printf '%s' "$ADMIN_PASSWORD" | ./bin/halro admin bootstrap \
  --config ./configs/config.example.yaml --username admin
./bin/halro serve --config ./configs/config.example.yaml
```

These offline commands remain the path for browserless servers, CI, automated
deployment, and emergency password recovery. They hold an exclusive lock on the
data directory, so they must be run while the service is stopped.

Default addresses:

| Service | Address | Purpose |
|---|---|---|
| Admin | `http://127.0.0.1:8081/admin` | Administration console |
| Gateway | `http://127.0.0.1:8080` | OpenAI-compatible API |
| Metrics | `http://127.0.0.1:9090/metrics` | Prometheus metrics |

Sign in to Admin with the username and password set during first-time setup.

## 3. Configuring the first model

There are two ways. Offline bootstrap is quickest for a first look; the console
is what you want for anything you intend to maintain.

### 3.1 One command for a complete OpenAI chain

This command must run while the service is stopped. It atomically creates a
Credential, Provider, Deployment, Route, Project, and one Gateway Key:

```bash
read -r -s OPENAI_API_KEY
printf '\n'
printf '%s' "$OPENAI_API_KEY" | ./bin/halro bootstrap \
  --config ./configs/config.example.yaml \
  --provider-type openai \
  --provider-base-url https://api.openai.com \
  --provider-model gpt-5-mini \
  --public-model chat \
  --billing-mode metered \
  --input-micros-per-million "$INPUT_MICROS_PER_MILLION" \
  --output-micros-per-million "$OUTPUT_MICROS_PER_MILLION" \
  --daily-budget-micros-usd 5000000
unset OPENAI_API_KEY
```

`5000000` micro-USD is 5 USD per day. The `gateway_key` the command returns is
shown once: put it straight into a secret manager, and not into chat, source
code, logs, or browser storage.

Bootstrap does not read two zero prices as "free". Use `--billing-mode free`
only when the deployment is genuinely free, permanently or by contract; a
metered model requires unit prices in micro-USD that you have checked. Bootstrap
stores those terms atomically as Price Version 1.

Run `serve` again when it finishes.

### 3.2 Full configuration in the console

In this order:

1. **Providers → Credentials**: pick the Provider type and enter the API key.
   The secret is encrypted with AES-GCM and afterwards reads only as
   "configured" — the plaintext is never shown again.
2. **Providers → Provider**: choose the type, base URL, credential and
   capability ceiling, then run the connection test.
3. **Deployments**: enter the real upstream model name. Catalog models apply
   reviewed capabilities directly; for an unknown model, explicitly confirm
   the bounded capability detection and retain only the verified capabilities
   you need. Then set concurrency and input/output prices in USD per million
   tokens. The cost and recovery boundaries are documented in the
   [capability-detection guide](model-capability-detection.zh-CN.md).
4. **Routes**: create a public model alias such as `chat`, bind it to the
   deployment, and choose ordered fallback or round robin.
5. **Projects**: set the allowed model aliases, RPM, TPM, maximum concurrency,
   daily budget, CIDRs, and security policies.
6. **Projects → Keys**: create a Gateway Key for the application. It is shown
   once; close the dialog only after storing it safely.

Provider capabilities are a ceiling and a deployment's capabilities can only be
a subset of them. Routes carry the public alias; SDK requests should never use
the real Provider model name.

### 3.3 Provider basics

| Type | Example base URL | Notes |
|---|---|---|
| OpenAI | `https://api.openai.com` | GA; chat, streaming, embeddings |
| Azure OpenAI | Azure resource endpoint | API version must be set explicitly |
| DeepSeek | `https://api.deepseek.com` | GA; does not declare embeddings by default |
| OpenAI Compatible | A reviewed HTTPS address | Declare the capabilities the platform actually has |
| Gemini | `https://generativelanguage.googleapis.com` | Beta, native adapter |
| Bedrock Runtime | `https://bedrock-runtime.<region>.amazonaws.com` | Beta, Converse text, explicit static AWS credential |
| Bedrock Mantle | `https://bedrock-mantle.<region>.api.aws` | Beta; OpenAI Chat, stateless Responses, or Anthropic Messages |

A Bedrock credential is a JSON secret:

```json
{"access_key_id":"...","secret_access_key":"...","session_token":"...","region":"us-east-1"}
```

`session_token` is optional and `region` must match the endpoint. Halro never
reads IMDS and never falls back to the host's default AWS credential chain.

A Mantle credential holds the Bedrock API key directly rather than that JSON.
Choose the Mantle access surface when creating the credential, then create a
separate Provider per protocol; one Provider binds one profile. Mantle Responses
always calls AWS with `store:false`, so it never creates 30-day stored state
that Halro cannot manage. Runtime and Mantle keep their credentials,
concurrency ceilings, and capability evidence separate.

## 4. Calling the Gateway

### 4.1 Handling the Gateway Key safely

Keep the key out of shell history:

```bash
read -r -s HALRO_GATEWAY_KEY
printf '\n'
export HALRO_GATEWAY_KEY
```

Clear it when you are done:

```bash
unset HALRO_GATEWAY_KEY
```

### 4.2 A non-streaming request with curl

Set `max_completion_tokens` explicitly. It bounds the upstream output, and it
bounds the conservative estimate Halro settles at when a call's outcome is
unclear:

```bash
curl http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $HALRO_GATEWAY_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "chat",
    "max_completion_tokens": 256,
    "messages": [
      {"role": "user", "content": "Introduce Halro in one sentence."}
    ]
  }'
```

### 4.3 A streaming request with curl

```bash
curl -N http://127.0.0.1:8080/v1/chat/completions \
  -H "Authorization: Bearer $HALRO_GATEWAY_KEY" \
  -H "Content-Type: application/json" \
  -d '{
    "model": "chat",
    "stream": true,
    "stream_options": {"include_usage": true},
    "max_completion_tokens": 256,
    "messages": [
      {"role": "user", "content": "List three security capabilities of an LLM gateway."}
    ]
  }'
```

The stream is standard SSE and ends with `data: [DONE]`. Before the first
content event, a retry or fallback is safe and may happen; once content has
started reaching the client Halro will not switch providers, because that
would splice two different answers together.

### 4.4 Embeddings

The deployment behind the route has to declare the embeddings capability and use
a real embedding model upstream:

```bash
curl http://127.0.0.1:8080/v1/embeddings \
  -H "Authorization: Bearer $HALRO_GATEWAY_KEY" \
  -H "Content-Type: application/json" \
  -d '{"model":"embedding","input":"Halro Gateway"}'
```

Chat and embeddings normally want separate deployments and separate public
routes.

Bedrock Runtime's `bedrock.runtime.invoke.titan-embed-text-v2.v1` profile is
pinned to `amazon.titan-embed-text-v2:0` and currently accepts a single string,
float output, and 256/512/1024 dimensions only. Arrays, token arrays, `base64`,
`user`, and other dimensions are refused before AWS is contacted. For batches,
call once per item from the client and control the concurrency yourself —
Halro does not fan out behind your back.

### 4.5 Python OpenAI SDK

```python
import os
from openai import OpenAI

client = OpenAI(
    api_key=os.environ["HALRO_GATEWAY_KEY"],
    base_url="http://127.0.0.1:8080/v1",
    max_retries=0,
)

response = client.chat.completions.create(
    model="chat",
    max_completion_tokens=256,
    messages=[{"role": "user", "content": "Hello"}],
)
print(response.choices[0].message.content)
```

### 4.6 Node.js OpenAI SDK

```javascript
import OpenAI from "openai";

const client = new OpenAI({
  apiKey: process.env.HALRO_GATEWAY_KEY,
  baseURL: "http://127.0.0.1:8080/v1",
  maxRetries: 0,
});

const response = await client.chat.completions.create({
  model: "chat",
  max_completion_tokens: 256,
  messages: [{ role: "user", content: "Hello" }],
});
console.log(response.choices[0].message.content);
```

Whether the SDK retries should be the application's explicit decision. The
gateway already performs bounded retries, fallback, circuit breaking, and
accounting; unbounded SDK retries on top of that multiply cost.

## 5. Projects, budgets, and limits

Each Project sets its own:

- `Allowed Models`: the public route aliases it may reach;
- `Daily Budget`: enforced against the calendar day in `usage.timezone`;
- `RPM`: requests per minute;
- `TPM`: tokens per minute;
- `Max Concurrency`: concurrent requests for the project;
- `Allowed CIDRs`: the source network boundary;
- a Token Guard policy;
- a Redaction policy.

The accounting time zone decides when that budget day ends, and the Overview's
"today" figures cover the same period — the page names the zone and the exact UTC
interval above the metrics, and renders every timestamp in it, so the chart and
the totals beside it always describe the same day. Where summer time applies a
day is 23 or 25 hours; the budget covers that whole calendar day rather than a
fixed 24 hours. Change it under Settings → Instance;
`usage.timezone` in config.yaml only seeds it on an instance's first start. A
change is scheduled for the end of the period in progress rather than applied at
once, so the day being billed is never redefined underneath you, and nothing
already recorded is recomputed. The setting moves nothing else: usage exports,
retention, price effective times, and the audit trail are always UTC.

Deployments and their price timelines are managed separately. Create at least
one price version that has taken effect; input and output prices are `USD / 1M
tokens` and a fixed price is `USD / request`. Every historical attempt keeps the
full price snapshot that applied at the time, so a later price change never
recalculates old spending. Without a valid price the default answer is `409
price_unavailable` rather than a confident `$0.00`; only an explicit `free`
version means a known cost of zero.

A network timeout or a dropped connection can leave it genuinely unknown whether
the provider processed a request. Halro settles those conservatively, bounded
by the request's maximum output tokens and by what it actually delivered, and
marks the result `estimated`. The Dashboard's headline token count shows only
provider-reported usage, with the estimated upper bound displayed separately;
the Usage page marks them `EST.`. Applications should set
`max_completion_tokens` and the project's maximum output sensibly.

## 6. Token Guard and redaction

### 6.1 Token Guard

Create a policy on the **Policies** page and bind it to a project. A sensible
rollout:

1. run in `observe` or `alert` to collect a normal baseline;
2. set hard thresholds for per-request tokens, tokens per minute, cost,
   concurrency, error rate, and source IPs;
3. move to `temporary_block` once the false-positive rate is acceptable;
4. EWMA is for relative anomaly detection and alerting only; it never blocks on
   its own.

An administrator can lift a temporary block by hand on the Projects page. Fixed
RPM, TPM, budget, and concurrency limits always take precedence over the
experimental detectors.

### 6.2 Redaction

A redaction policy supports built-in PII and secret detectors, RE2 rules, and
dictionaries. A policy can:

- refuse requests containing sensitive content;
- redact what is sent to the provider;
- redact what the provider returns;
- detect patterns across chunk boundaries in a stream.

Before rolling one out, test it against real samples and look at both what it
misses and what it catches wrongly. Never paste production secrets into chat,
issues, test snapshots, or browser storage.

## 7. Dashboard, Usage, and Operations

- **Dashboard**: today's requests, provider attempts, provider-reported tokens,
  cost, error rate, latency, and the last seven days;
- **Usage**: per provider attempt — status, tokens, cost, latency, and whether
  the figure is a conservative estimate;
- **Operations**: alerts and the HMAC audit chain;
- **Settings**: light/dark appearance, interface language, hot-reloadable
  runtime parameters, and the administrator password;
- **System Status**: the ledger, the usage watermark, queues, and overall health.

One client request can produce several provider attempts — a retry, or a
fallback. Requests and attempts differing is normal; cost, tokens, and error
rates are recorded per attempt.

### 7.1 Failed requests

The Usage page's failure count links to a list of the requests that ended
badly — one row per **request**, not per attempt, so the number in the card and
the length of the list cannot disagree. A request that failed one target and
succeeded on the next is not a failed request, even though it left a failed
attempt behind.

Each row names the failure class in your language and keeps the upstream status
beside it, with what to check next behind a disclosure. A row carries no
provider context in two cases, and they mean different things:

- **the request never reached an upstream** — it was refused by budget, the
  circuit breaker, concurrency, Token Guard, capability filtering or redaction,
  and the row says which;
- **the upstream answered and the request failed afterwards** — the answer could
  not be rendered or outbound redaction refused it. There is no upstream failure
  to point at, so none is shown.

A row may also say the record predates the fields it would otherwise show, which
is not the same as an upstream that gave none.

If `gateway.failure_capture` is switched on, a failed request whose payload was
captured offers the request and the upstream reply. Opening it writes an audit
record: it is the only place in the console that shows what a caller wrote.

### 7.2 The console's window

Settings → Instance sets how far back the request and failure lists reach. It
starts from `usage.console_window_days` and is a runtime setting after that;
Parquet archives keep their own, longer retention, so shortening this changes
what the console can show and not what has been kept. Shortening it asks for
confirmation, because the records outside the new window are dropped from memory
at the next maintenance pass.

### 7.3 Deferred responses

Where a Project allows it, a caller can submit `POST /v1/responses` with
`background: true`, receive an identifier straight away, and collect the answer
later. The switch is on the Project page. Answers are held for at most 24 hours,
sealed on disk, and for 15 minutes after the first collection — see the Operator
Guide for what that means for this instance's data directory.

On an instance that has never served a request, the Dashboard leads with the
six-step configuration chain instead of empty charts, and ends at the developer
workbench, where a real request proves the chain end to end.

## 8. Metrics

A bearer token is required by default. It is derived from the Master Key and is
never stored in YAML:

```bash
./bin/halro metrics token --config ./configs/config.example.yaml
```

Calling it:

```bash
read -r -s HALRO_METRICS_TOKEN
printf '\n'
curl http://127.0.0.1:9090/metrics \
  -H "Authorization: Bearer $HALRO_METRICS_TOKEN"
unset HALRO_METRICS_TOKEN
```

The full list is in the [Metrics reference](../contracts/metrics-reference.md).
Metric labels deliberately exclude high-cardinality or sensitive values such as
project, key, request ID, original model, and IP.

In production, set `metrics.credential_file` so the metrics credential can be
rotated and revoked independently of the Master Key:

```bash
./bin/halro metrics rotate --config ./config.yaml --overlap 10m
./bin/halro metrics list --config ./config.yaml
./bin/halro metrics revoke --config ./config.yaml --version 1
```

`rotate` prints the new token once on standard output; write it straight into
the secret file and keep it out of shell history, environment variables, and
logs. A non-loopback metrics listener additionally requires `metrics.tls` with
mutual TLS and a client CA.

## 9. Key lifecycle

- Provider secrets are only ever managed as Credentials, encrypted with
  audience-bound AEAD;
- Gateway Keys begin with `gw_` and are stored only as a SHA-256 representation;
- a Gateway Key is displayed once, at creation;
- if you suspect a leak, disable the old key on the Projects page and create a
  new one immediately;
- do not delete the only key an application still uses before disabling it;
- changing the administrator password rotates the session and CSRF material and
  invalidates existing sessions;
- **Settings → Security** accepts several independent authenticators, compatible
  with Microsoft Authenticator, Google Authenticator, 1Password, and other
  standard TOTP apps. Each can be revoked on its own, and any valid code signs
  you in;
- enabling it the first time produces 10 recovery codes, shown once. A recovery
  code is consumed on use; store them offline and not beside the administrator
  password;
- codes depend on accurate time. If they keep failing, check automatic time sync
  on both the server and the phone.

Internal keys can also be created or disabled offline while the service is
stopped:

```bash
./bin/halro key create --config ./configs/config.example.yaml \
  --project-id prj_... --name team-a

./bin/halro key disable --config ./configs/config.example.yaml \
  --key-id key_...
```

## 10. Phase 2 media and resource APIs

The console offers a separate "media and resources" profile for OpenAI, and
Titan Image, Cohere Rerank, or Nova Reel Async profiles for Bedrock. Do not
combine several protocol capabilities into one Provider. Files, Batches, and
Async creation requests must carry an `Idempotency-Key`; file uploads must also
carry `Halro-Route`. A resource ID is visible only inside the project that
created it, and reads and deletes always return to the original Provider,
Deployment, profile, and region. Bedrock async jobs cannot currently be
cancelled: the API answers `provider_cancel_unsupported` rather than reporting a
success that did not happen.

The price version's fixed USD-per-request applies to media, rerank, and resource
operations; configure it from the upstream price list. When the price is
unknown, the default is to refuse before any provider I/O rather than to record
a minimal placeholder. File content is stored in a private object directory
inside the data directory and is removed on delete or TTL reclamation.

If a file, batch, or async creation is interrupted before the provider was ever
contacted — the process died mid-request — its idempotency key is reclaimed
after a restart and the request can simply be retried. If the provider had
already been called, the key stays refused, because retrying a call that may
have created something upstream is how you end up with two.

## 11. Backup and restore

Create an encrypted backup offline:

```bash
umask 077
openssl rand 32 > backup.key
./bin/halro backup create \
  --config ./configs/config.example.yaml \
  --output ./halro.hmbk \
  --key-file ./backup.key
```

Verify it:

```bash
./bin/halro backup verify \
  --file ./halro.hmbk \
  --key-file ./backup.key
```

A backup does not contain the Master Key. Three things must be kept separately:

1. the encrypted backup file;
2. the backup key;
3. the Master Key that was in use when the backup was made.

Stop the service before restoring, and follow
[Backup and restore](backup-restore.md) exactly.

## 12. Diagnostics and common problems

Run the read-only diagnostic with the service stopped:

```bash
./bin/halro doctor --config ./configs/config.example.yaml
```

Common errors:

| Symptom | Check first |
|---|---|
| `401 invalid_api_key` | Whether the Gateway Key is complete, enabled, unexpired, and not rotated |
| `403 model_not_allowed` | Whether the project's allowed models include the public route alias |
| `403 budget_exceeded` | The daily budget, the deployment price, and the conservative estimate ceiling |
| `409 price_unavailable` | Whether the deployment has a price version in effect; whether unknown pricing is refused by the budget or by a cost-based Token Guard |
| `429` | Project RPM/TPM/concurrency, Provider/Deployment concurrency, Token Guard, or upstream rate limiting |
| `502 provider_error` | The provider endpoint, credential, model name, network, and the connection test |
| `EST.` on the Dashboard | Provider usage was missing or the call's outcome was unclear; it is not confirmed consumption |
| Cost shows `$0.00` | Whether the price version is explicitly `free`; unknown prices are not counted as known cost |
| Data directory locked | Another Halro process or offline command holds it; do not delete the lock file to get around it |
| Readiness failing | Accounting, pricing quarantine, WAL append errors, disk space, and usage lag |

### 12.1 Price versions, historical cost, and price proposals

- Deployment prices are immutable versions with an effective time. Changing the
  current price creates a new version; it never recalculates old requests.
- Every provider attempt binds a price snapshot before the upstream call. The
  Usage page can expand to show the pricing evidence behind a call's cost;
  unknown cost is shown as blank rather than as `$0`.
- A "price proposal" is a separate item awaiting review. An LLM or an import
  tool may only submit a proposal carrying a source digest, a model and region
  match, warnings, and an expiry — it cannot change a price.
- After checking the source, an administrator re-authenticates with the current
  password (plus TOTP where MFA is enabled) and adopts it explicitly. Halro
  then creates a new immutable price version and writes it to the audit chain.
  An ambiguous or expired proposal cannot be adopted.
- If an old backup is restored after a scheduled price has passed its effective
  time, the deployment enters pricing quarantine and serves no traffic until an
  administrator has reviewed and confirmed it.

## 13. Security notes

- The example configuration listens on `127.0.0.1` only; a public listener
  requires TLS and a reviewed reverse-proxy boundary;
- Admin and Metrics must never be exposed in plaintext over the public internet;
- never pass a secret in a URL query, a command argument, Git, logs, or chat;
- custom provider endpoints and webhooks must use HTTPS and are subject to the
  SafeTransport/SSRF policy;
- never run two processes against one data directory;
- stop the service before changing the Master Key, restoring a backup, or
  running offline key commands;
- verify the audit chain, backups, disk space, WAL/usage watermarks, and alert
  delivery regularly.

Further operational material:

- [Operator Guide](operator-guide.md)
- [Backup and restore](backup-restore.md)
- [Usage storage](../contracts/usage-storage.md)
- [Metrics reference](../contracts/metrics-reference.md)
- [Webhook payloads](../contracts/webhook-payloads.md)
- [Token Guard EWMA](../architecture/token-guard-ewma.md)
- [Threat model](../architecture/threat-model.md)
