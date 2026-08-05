# Operator Guide

This guide covers a single-node Heimdall installation. Run offline commands
with the server stopped: the data directory has one exclusive owner.

## Clean install

For a local interactive installation from source, use the simplified path:

```bash
make start
```

The command builds changed assets, creates a `0600` loopback-only
`config.yaml` if absent, initializes only a provably empty instance, and
prints `/admin/setup`. Create the first administrator in that page. Repeating
the command never replaces existing configuration, the Master Key, or data.
If initialization is partial, startup fails closed and requires restoration of
the matching files.

When Admin is configured on a non-loopback TLS listener, startup also prints a
one-time setup token. The token exists only in process memory, changes after a
restart, and is required by the setup form to prevent first-user takeover.

For headless and automated installation, use a release binary and retain the
explicit offline flow below.

Use a release binary for the target OS/architecture, or build the exact source
commit with `make build`. Keep the binary, configuration, Master Key, and data
directory on persistent storage with different backup handling for the key.

```bash
cp configs/config.example.yaml ./config.yaml
./heimdall config check --config ./config.yaml
./heimdall init --config ./config.yaml
printf '%s' "$ADMIN_PASSWORD" | ./heimdall admin bootstrap \
  --config ./config.yaml --username admin
printf '%s' "$PROVIDER_SECRET" | ./heimdall bootstrap \
  --config ./config.yaml \
  --provider-type openai \
  --provider-base-url https://api.openai.com \
  --provider-model gpt-5-mini \
  --public-model chat \
  --billing-mode metered \
  --input-micros-per-million "$INPUT_MICROS_PER_MILLION" \
  --output-micros-per-million "$OUTPUT_MICROS_PER_MILLION"
./heimdall serve --config ./config.yaml
```

From a source checkout, the equivalent explicit initialization helper is
`make init CONFIG=./config.yaml`. Initialization is offline and fail-closed:
stop the running Heimdall process first, and do not use it to reset or overwrite
an existing data directory.

The bootstrap response contains the Gateway Key once. Move it directly to the
workload secret store; do not put it in shell history, source control, logs, or
browser storage. The generated Master Key must remain a regular `0600` file.
Back it up separately from both the data directory and encrypted backup key.
Bootstrap never infers that zero prices mean free. Use `--billing-mode free`
only for an intentionally free deployment; otherwise provide the reviewed
metered prices shown above. Bootstrap stores those terms as Price Version 1.

Default listeners are loopback-only. To expose Heimdall, use TLS and an
authenticated reverse proxy with an explicitly configured origin/trusted proxy
boundary. Admin and Metrics must never use public plaintext listeners.
When Gateway proxy headers are enabled, every request received from a trusted
proxy must carry a syntactically valid `X-Forwarded-For` chain. Missing or
malformed chains are rejected with HTTP 400 so CIDR authorization and Token
Guard cannot silently lose their source-IP signal.

## Configuration reference

`configs/config.example.yaml` is the canonical complete v1 example. Important
groups are:

- `server`: three listener addresses and HTTP size/time limits;
- `tls`: certificate and private-key paths shared by enabled listeners;
- `storage`: data directory, bbolt filename, and Master Key path;
- `admin`: session/idle limits, login rate, and external origin;
- `usage`: timezone, WAL batching, checkpoint/Parquet cadence, retention;
- `gateway`: route/attempt/stream deadlines and active probe interval;
- `retry` and `circuit_breaker`: bounded attempt and failure policy;
- `alerts`: queue, worker, timeout, retry, and dedup bounds;
- `security`: private egress and trusted proxy policy;
- `metrics`: exporter enablement and authentication requirement.

Metrics also supports `credential_file`, bounded scrapes/write timeout, and a
dedicated mutual-TLS listener. The legacy Master-Key-derived token is suitable
only for loopback development and upgrade compatibility. Production must set a
versioned credential file, initialize it with `heimdall metrics rotate`, and
use `metrics.tls` for any non-loopback listener. Rotate by installing the new
one-time token in Prometheus, verifying two successful scrape intervals, then
revoking the retiring version. See `docs/observability/operations-runbook.md`.

Retry limits do not override Heimdall's ambiguity boundary. If an upstream
request might already have executed, Heimdall records a conservative estimated
settlement and returns the failure without retrying or switching Provider. Safe
fallback remains available for explicitly classified non-ambiguous failures.
This fail-closed behavior is not configurable in v1; changing it requires an
end-to-end idempotency contract with the upstream Provider.

Unknown YAML fields and invalid durations are rejected. Listener, TLS, storage,
egress, proxy, and Metrics-auth changes require restart. The Admin Settings page
only changes the explicitly writable runtime settings. Always run `config check`
before restart.

Admin localization is metadata, not YAML configuration. The public
`GET /admin/api/v1/ui/bootstrap` endpoint exposes only the instance default and
supported locale identifiers so the setup/login shell can render before
authentication. Authenticated administrators can update their own preference
through `/admin/api/v1/preferences`; the instance default uses
`/admin/api/v1/settings/ui`. Both mutation paths require CSRF protection and an
`If-Match` revision, are audited, and never affect Gateway protocol payloads.
Existing databases are initialized with `zh-CN`; older administrator records
without a locale are interpreted as `system`.

## Offline diagnostics and break-glass access

Stop Heimdall, then run the read-only diagnostic before upgrades, restores, or
when startup fails:

```bash
./heimdall doctor --config ./config.yaml
```

The JSON report verifies configuration safety, exclusive data ownership, file
permissions, the exact bbolt schema, Master Key/Vault binding, the WAL checksum
and complete tail, Parquet manifests/checksums, usage timezone, free disk space,
and Provider/Deployment/Route references. A partial WAL is reported as a
failure but is never truncated or repaired. Network Provider probes are
intentionally skipped; run audited connection tests in Admin after startup.
The diagnostic acquires the already initialized lock file through a read-only
descriptor and does not rewrite its PID metadata. Regression tests hash every
file under the data directory before and after a successful run.

If the local Admin password is lost, keep the server stopped and pipe a new
password through standard input:

```bash
printf '%s' "$NEW_ADMIN_PASSWORD" | ./heimdall admin reset-password \
  --config ./config.yaml --username admin
```

The reset verifies the Master Key first, replaces the Argon2id verifier,
increments the session generation, invalidates every existing session, and
appends `admin.password.reset` to the trusted Audit chain. The password is not
accepted as a command-line argument.

### Developer workbench

Set `admin.developer_workbench` to `enabled` (default) or `disabled`. While
enabled, the Admin listener also serves real Gateway calls: an administrator can
send billed inference requests through `/admin/api/v1/developer/execute` using a
Gateway Key. Authentication is unchanged — the Gateway Key still decides the
project, and project budgets, rate limits, redaction, and Token Guard all still
apply — but network controls that are applied only to the Gateway listener do
not cover this path. Set `disabled` when the two listeners are isolated at the
network layer. Every execution appends `developer.execute` to the Audit chain
with the acting administrator, endpoint, HTTP status, and Request ID.

### Authenticator two-factor authentication

Set `admin.mfa_policy` to `optional` (the upgrade-compatible default) or
`required`. Remote Admin deployments should use `required`. Heimdall implements
standard 6-digit, 30-second TOTP and works with Microsoft Authenticator, Google
Authenticator, 1Password, and other compatible applications. Production hosts
must keep UTC time synchronized.

If every authenticator and recovery code is lost, stop Heimdall and run:

```bash
./heimdall admin reset-mfa --config ./config.yaml --username admin
```

This removes all factors and recovery codes, invalidates sessions and pending
challenges, and appends `admin.mfa.reset_offline` to the trusted Audit chain.
With `mfa_policy: required`, the next password login is restricted to setup.

## Master Key rotation

The `--new-key-file` procedure below applies to `storage.master_key.mode: file`.
For `key_slots`, Heimdall generates the new Master Key only in memory and
requires a stable, non-secret operation ID so a command interrupted after
publication can be retried without accidentally creating another generation:

```bash
./heimdall key rotate --config ./config.yaml \
  --operation-id change-2026-08-03-001
```

Normal KMS KEK replacement is a distinct `key rewrap` operation and does not
change the Master Key or Vault ciphertext. Suspected KMS Key, Grant, policy, or
Decrypt-identity compromise must use DEK rotation, not rewrap. The complete
procedure, interruption matrix, and historical-backup disposition checklist
are in [the M11 KMS key lifecycle runbook](../runbooks/m11-kms-key-lifecycle.md).
AWS IAM/Key Policy、Kubernetes/systemd 加固、KMS Audit/Metrics/告警和事故响应见
[M11 生产运行 Runbook](../runbooks/m11-production-operations.md)。AWS KMS 模式只有在
M11 真实 AWS 矩阵、独立恢复演练和四方发布签署完成后才能标记为 production-ready。

Rotation is an offline operation. First create and verify an encrypted backup,
stop Heimdall, retain the old Master Key with backups created under it, and
generate a separate replacement key with mode `0600`:

```bash
umask 077
openssl rand 32 > /secure/path/new-master.key
./heimdall key rotate --config ./config.yaml \
  --new-key-file /secure/path/new-master.key
```

The command advances the persistent versioned keyring, rewrites every
Credential into a copy-on-write bbolt image,
increments its key version, invalidates all Admin sessions, preserves the
existing Audit HMAC chain under a new authenticated envelope, compacts retired
ciphertext pages, and atomically publishes the database and active Master Key.
Only SHA-256 fingerprints and record counts are printed.

If the host or command stops during rotation, do not restore individual files
or choose another replacement key. Rerun the exact command with the same
`--new-key-file`; the authenticated temporary recovery bridge makes both the
“new DB/old key” and “new DB/new key” states recoverable. The bridge is removed
through a second compacted COW publication only after all records and the Audit
key verify with the new Master Key. After successful startup and backup
verification, retain the replacement key according to key-custody policy and
remove unnecessary duplicate copies. Older backups still require their
recorded old Master Key.

## Provider setup

Create an encrypted Credential first, then a Provider, Deployment, and public
Route in the Admin console. Credential audience binds provider type plus the
normalized endpoint origin; changing either requires a matching credential
rotation. Keep private endpoint access disabled unless the deployment genuinely
needs it and the hostname/IP boundary has been reviewed.

| Type | Base URL example | Secret format | Declared v1 profile |
|---|---|---|---|
| OpenAI | `https://api.openai.com` | API key | GA chat/embeddings or isolated Phase 2 media/resources profile |
| Azure OpenAI | resource endpoint | API key plus explicit API version on Provider | GA deployment paths |
| DeepSeek | `https://api.deepseek.com` | API key | GA chat/stream profile |
| OpenAI-compatible | reviewed HTTPS origin | API key | conservative capabilities; opt in extras |
| Gemini | `https://generativelanguage.googleapis.com` | API key | Beta text chat/stream and float embeddings |
| Bedrock Runtime | `https://bedrock-runtime.us-east-1.amazonaws.com` | JSON below | Beta Converse, Titan Embeddings/Image, or Nova Reel Async profile |
| Bedrock Agent Runtime | `https://bedrock-agent-runtime.us-east-1.amazonaws.com` | JSON below | Beta Cohere Rerank 3.5 profile only |
| Bedrock Mantle | `https://bedrock-mantle.us-east-1.api.aws` | Bedrock API key | Beta OpenAI Chat, stateless Responses, or Anthropic Messages |

Bedrock JSON is one encrypted secret. `session_token` is optional and `region`
must match the endpoint hostname:

```json
{"access_key_id":"...","secret_access_key":"...","session_token":"...","region":"us-east-1"}
```

The Bedrock Runtime profiles do not read environment credentials or IMDS. The
Converse profile does not declare embeddings, tools, vision, or JSON mode. The
separate `bedrock.runtime.invoke.titan-embed-text-v2.v1` profile declares
embeddings only, pins `amazon.titan-embed-text-v2:0`, and accepts one string with
float output and 256/512/1024 dimensions. It never exposes arbitrary InvokeModel
JSON or hidden batch fan-out. Connection tests are audited; they never return
upstream response bodies or credentials.

Phase 2 resource creation requires `Idempotency-Key`. Files also require the
`Heimdall-Route` header because multipart file creation has no `model` field.
File bytes are kept under `storage.data_dir/provider-objects` with private
permissions. TTL maintenance uses the recorded owner to remove an upstream file
before deleting its local object and bbolt record; active batches and async jobs
retain their owner mapping past the nominal TTL. Configure
`fixed_request_micros_usd` on media/resource deployments so budget admission is
not treated as free. Bedrock async output must be an explicit `s3://` URI; its
cancel endpoint intentionally returns `provider_cancel_unsupported` after
ownership validation because Bedrock Runtime exposes no cancellation call.
The built-in content scanner is a format admission gate, not antivirus. A
deployment that requires malware detection must provide a dedicated scanner and
fail closed when that scanner is unavailable.

The opt-in real-provider test accepts `HEIMDALL_SMOKE_OPERATION=embeddings` with
`HEIMDALL_SMOKE_MODEL=amazon.titan-embed-text-v2:0`; it incurs one AWS inference
request.

For Mantle, select the Mantle access surface while creating the Credential and
choose exactly one wire profile per Provider. OpenAI profiles send the Bedrock
API key as Bearer authentication; the Anthropic profile sends it as `x-api-key`.
The Responses profile always sends `store:false`. Runtime and Mantle use
different credentials and Provider instances even when they belong to the same
AWS account. Only regional `bedrock-mantle.<region>.api.aws` origins are accepted.

## Alerts

Create an encrypted webhook header credential when the receiver requires one,
then configure a Generic JSON endpoint and test it from Operations. Delivery is
bounded, retried with jitter, deduplicated, and routed through SafeTransport.
Webhook payloads are telemetry-redacted and intentionally omit prompts,
responses, credentials, raw IPs, and detailed provider topology. See
`docs/contracts/webhook-payloads.md` for Slack, Discord, Feishu, and WeCom receiver
examples.

## Upgrade and rollback

Pricing selection uses a persisted per-Deployment high-water mark. Keep host
time synchronized and configure `gateway.pricing_clock_rollback_tolerance` and
`gateway.pricing_clock_forward_tolerance`; a material rollback, unexplained
forward jump, or incoherent restored high-water enters pricing quarantine and
blocks that Deployment. `gateway.pricing_unknown_policy` defaults to `reject`.
The only opt-in value, `allow_without_cost_governance`, still rejects unknown
pricing for Projects with a daily budget or a cost-dimension Token Guard. Watch
`heimdall_pricing_quarantined_deployments`, Accounting Lease recovery metrics,
readiness, and `heimdall doctor` after every restart or restore.

Price changes create future-effective immutable versions; never edit old
prices to correct history. Use Usage cost adjustments for historical
corrections. Each adjustment records the original Settlement, signed delta,
final cost, service period, posted period, actor, reason, and evidence. Unknown
cost remains null until a reprice adjustment supplies complete correction
evidence.

Pricing Proposals are a separate review queue. The interactive Admin API accepts
only asserted official-URL evidence; `verified_api` and `signed_import` are
reserved for trusted adapters that verify transport or signatures before
constructing a Proposal. Evidence retains model, region, tier, match status,
warnings and expiry. A Proposal is inert: it cannot affect admission, budgets,
settlement, or the Gateway. Adoption requires recent Admin re-authentication
and creates a new immutable Price Version; ambiguous or expired Proposals
cannot be adopted.

1. Read release notes and verify the binary checksum and Sigstore bundle.
2. Stop Heimdall and confirm the process released the data-directory lock.
3. Create and verify an encrypted backup; preserve the current binary/config.
4. Run the new binary's `config check` against a copy of the configuration.
5. While the service is stopped, run
   `heimdall pricing migrate --config <config> --dry-run --report <report.json>`.
   Resolve every enabled zero-price Deployment in a schema-v1 resolution file
   bound to the report's `metadata_sha256`, then run
   `heimdall pricing migrate --config <config> --resolution-file <file> --apply`.
   The tool migrates a consistent staging snapshot, atomically publishes it,
   and retains the prior metadata path printed on success.
6. Start the new binary. Remaining migrations are versioned and applied during open.
7. Check `/health/live`, `/health/ready`, Admin system status, Metrics, a
   non-stream request, a stream request, and usage settlement.

For a restore drill, also record the Backup ID, metadata schema, Ledger reader
gate, Settlement/Adjustment Parquet watermarks and pricing-state digest. Test a
future Price Version whose effective time passes after the backup: restoration
must quarantine that Deployment. Review the restored source and current
Provider terms, then use the Admin restore-confirm action with recent re-auth,
or create a correct successor version. Traffic must remain blocked until then.

Do not downgrade a migrated data directory in place. If validation fails, stop
the new binary and follow `docs/guides/backup-restore.md`; restore preserves the old
live directory as a rollback directory. Never replace only bbolt while keeping
a WAL/Audit/Parquet set from another epoch.

## Troubleshooting

- Startup says the data directory is locked: another Heimdall or offline
  command owns it. Find the owner; do not delete lock files to bypass it.
- Readiness is false: inspect accounting state and WAL errors first. Heimdall
  deliberately stops new provider calls when durable accounting is unsafe.
- Provider test is unhealthy: verify HTTPS hostname, credential audience/type,
  model deployment, Azure API version, and Bedrock region. Upstream bodies are
  intentionally hidden; use the provider control plane for detailed diagnosis.
- Requests return 403: check Project enabled state, key revocation, allowed
  routes/CIDRs, daily budget, Token Guard block, and redaction reject policy.
- Requests return 429: distinguish Project RPM/TPM/concurrency, Provider or
  Deployment concurrency, Token Guard, and upstream rate limits using metrics.
- Restore fails before switch: use the exact backup key, live Master Key, and
  verified Backup ID. The live directory remains unchanged on preflight failure.
- Disk usage grows: inspect retention settings, Parquet manifest verification,
  WAL/checkpoint progress, and backup copies. See `docs/contracts/usage-storage.md`.

Operational references: `docs/contracts/metrics-reference.md`, `docs/guides/backup-restore.md`,
`docs/verification/crash-recovery-matrix.md`, `docs/contracts/usage-storage.md`,
`docs/architecture/token-guard-ewma.md`, `docs/verification/security-review-v1.md`, and
`docs/guides/releasing.md`.

## Optional container image

`Dockerfile` builds the React console and a static Go binary, then copies only
the binary and an owned data directory into the non-root distroless runtime.
No shell or package manager is present in the final image.

The CI workflow rebuilds the image and verifies its version command, runtime
UID/GID, and healthcheck metadata. Tagged releases also attach a signed,
checksummed `heimdall-container.tar.gz`; load it with
`gzip -dc heimdall-container.tar.gz | docker load`.

```bash
docker build -t heimdall:v1.0.0 .
docker run --rm --user 65532:65532 \
  -v "$PWD/config.yaml:/etc/heimdall/config.yaml:ro" \
  -v "$PWD/master.key:/run/secrets/heimdall-master.key:ro" \
  -v heimdall-data:/var/lib/heimdall \
  -p 8080:8080 -p 8081:8081 heimdall:v1.0.0
```

Container configuration must use `/var/lib/heimdall` for `storage.data_dir`
and `/run/secrets/heimdall-master.key` for the Master Key. A listener exposed
outside the container must follow the same TLS rules as a bare-metal install;
do not weaken Admin/Metrics listener validation for Docker. The built-in
healthcheck calls only a loopback HTTP(S) readiness URL, follows no redirects,
and can be changed with `HEIMDALL_HEALTH_URL` when TLS is enabled. Ensure its
hostname is covered by the mounted certificate.
