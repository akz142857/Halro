# Operator Guide

This guide covers a single-node Halro installation. Run offline commands
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
./halro config check --config ./config.yaml
./halro init --config ./config.yaml
printf '%s' "$ADMIN_PASSWORD" | ./halro admin bootstrap \
  --config ./config.yaml --username admin
printf '%s' "$PROVIDER_SECRET" | ./halro bootstrap \
  --config ./config.yaml \
  --provider-type openai \
  --provider-base-url https://api.openai.com \
  --provider-model gpt-5-mini \
  --public-model chat \
  --billing-mode metered \
  --input-micros-per-million "$INPUT_MICROS_PER_MILLION" \
  --output-micros-per-million "$OUTPUT_MICROS_PER_MILLION"
./halro serve --config ./config.yaml
```

From a source checkout, the equivalent explicit initialization helper is
`make init CONFIG=./config.yaml`. Initialization is offline and fail-closed:
stop the running Halro process first, and do not use it to reset or overwrite
an existing data directory.

The bootstrap response contains the Gateway Key once. Move it directly to the
workload secret store; do not put it in shell history, source control, logs, or
browser storage. If one reaches somewhere it should not have — a ticket, a chat
transcript, a CI log — follow
[`docs/runbooks/gateway-key-compromise.md`](../runbooks/gateway-key-compromise.md)
and revoke before investigating: revocation takes effect before the request
returns, and re-issuing costs one API call. The generated Master Key must remain a regular `0600` file.
Back it up separately from both the data directory and encrypted backup key.
Bootstrap never infers that zero prices mean free. Use `--billing-mode free`
only for an intentionally free deployment; otherwise provide the reviewed
metered prices shown above. Bootstrap stores those terms as Price Version 1.

Default listeners are loopback-only. To expose Halro, use TLS and an
authenticated reverse proxy with an explicitly configured origin/trusted proxy
boundary. Admin and Metrics must never use public plaintext listeners. Both
supported shapes, and the settings each one requires, are in
[TLS and inbound exposure](#tls-and-inbound-exposure).
When Gateway proxy headers are enabled, every request received from a trusted
proxy must carry a syntactically valid `X-Forwarded-For` chain. Missing or
malformed chains are rejected with HTTP 400 so CIDR authorization and Token
Guard cannot silently lose their source-IP signal.

## Configuration reference

`halro start` writes an annotated `config.yaml` when none exists, carrying
the built-in defaults with a comment on each setting that has a consequence.
Deleting a key restores its default; a test keeps that file and the built-in
defaults from drifting apart. `configs/config.example.yaml` remains the
canonical complete v1 example, including the settings the default file leaves
out. Important groups are:

- `server`: three listener addresses, HTTP size/time limits, and graceful-shutdown budget;
- `tls`: certificate and private-key paths shared by enabled listeners;
- `storage`: data directory, bbolt filename, and Master Key path;
- `admin`: session/idle limits, login rate, and external origin;
- `usage`: timezone, WAL batching, checkpoint/Parquet cadence, retention;
- `gateway`: route/attempt/stream deadlines and active probe interval;
- `retry` and `circuit_breaker`: bounded attempt and failure policy;
- `alerts`: queue, worker, timeout, retry, and dedup bounds;
- `security`: private egress and trusted proxy policy;
- `metrics`: exporter enablement and authentication requirement;
- `logging`: level, encoding, and whether a bounded local file is kept.

Metrics also supports `credential_file`, bounded scrapes/write timeout, and a
dedicated mutual-TLS listener. The legacy Master-Key-derived token is suitable
only for loopback development and upgrade compatibility. Production must set a
versioned credential file, initialize it with `halro metrics rotate`, and
use `metrics.tls` for any non-loopback listener. Rotate by installing the new
one-time token in Prometheus, verifying two successful scrape intervals, then
revoking the retiring version. See `docs/observability/operations-runbook.md`.

Retry limits do not override Halro's ambiguity boundary. If an upstream
request might already have executed, Halro records a conservative estimated
settlement and returns the failure without retrying or switching Provider. Safe
fallback remains available for explicitly classified non-ambiguous failures.
This fail-closed behavior is not configurable in v1; changing it requires an
end-to-end idempotency contract with the upstream Provider.

### TLS and inbound exposure

There are two supported shapes, and "serve plaintext on a routable address" is
not a third — configuration validation refuses it. A listener may bind a
non-loopback address only when TLS is enabled; `-allow-insecure-public-listen`
covers the Gateway listener alone and exists for a host-local boundary such as
`docker run -p 127.0.0.1:8080:8080`, not for a network.

**Halro terminates TLS.** Certificates come from whatever already issues them
here — certbot, acme.sh, an internal CA, configuration management. Halro loads
them; it does not obtain them.

```yaml
tls:
  enabled: true
  certificates:
    - cert_file: /etc/halro/tls/fullchain.pem
      key_file: /etc/halro/tls/privkey.pem
server:
  gateway_listen: 0.0.0.0:8080
  admin_listen: 0.0.0.0:8081
  metrics_listen: 127.0.0.1:9090
admin:
  external_origin: https://halro.example.com:8081
security:
  trust_proxy_headers: false
```

`tls.certificates` is a list, and the order matters: the first entry answers
connections that arrive without SNI — a health check dialling the address rather
than the name — and every other entry is selected by the DNS names its own
certificate declares. Gateway and Admin may therefore use different hostnames
without one certificate having to cover both. A server name that matches nothing
is answered with the first entry, so the client's own name check reports which
name was missing rather than the opaque handshake failure a refusal would
produce. Two entries claiming the same name are refused at load: an ambiguous
mapping is harder to diagnose than a missing one.

`tls.certificates` replaced the older `tls.cert_file` and `tls.key_file`
scalars, and the old keys are gone rather than deprecated: unknown YAML fields
are refused, so a configuration still carrying them stops the process at load
with `field cert_file not found in type config.TLS`. Rewrite the block as the list
above — one entry with the two paths reproduces the previous behaviour exactly.
The data directory is untouched by this; only `config.yaml` needs editing.

`trust_proxy_headers` must stay `false` in this shape. Clients connect directly,
so their address is the peer address; a trusted-proxy range that no client
falls into does nothing but obscure the topology.

**A reverse proxy terminates TLS.** This is the default recommendation. The
certificate lifetime then lives outside Halro's failure domain: a renewal that
fails does not touch the Gateway process, and Caddy or Traefik will obtain and
renew certificates without Halro having to.

```yaml
tls:
  enabled: false
server:
  gateway_listen: 127.0.0.1:8080
  admin_listen: 127.0.0.1:8081
  metrics_listen: 127.0.0.1:9090
admin:
  external_origin: https://halro.example.com
security:
  trust_proxy_headers: true
  trusted_proxy_cidrs: ["127.0.0.1/32"]
```

Two settings decide whether this works, and configuration validation checks
neither of them:

`admin.external_origin` is **required** behind a proxy. Halro checks the
browser's `Origin` against this instance's own origin; with the setting empty it
derives that origin from the connection it received, which behind a proxy is
plaintext, and compares `http://…` against the browser's `https://…`. The check
guards the sign-in itself as well as every later mutation, so the symptom is not
a subtle one — sign-in returns `origin rejected` and nobody gets in at all,
while configuration validation says nothing about why. The value must match the
browser's address bar exactly, including the port whenever it is not the scheme
default —
`https://halro.example.com` behind a proxy on 443, `https://halro.example.com:8081`
when Halro serves 8081 itself.

Setting `admin.external_origin` also makes the one-time setup token mandatory
for first-run initialization; the loopback shortcut applies only to an instance
that is not reachable by name. Plan for it on a first deployment rather than
discovering it at the console.

`trusted_proxy_cidrs` must contain the address the proxy actually connects
from. In a container network that is rarely `127.0.0.1`. Get it wrong and CIDR
authorization and Token Guard silently evaluate the proxy's address instead of
the client's — no error, just the wrong answer. With `trust_proxy_headers`
enabled, every request from a trusted proxy must also carry a syntactically
valid `X-Forwarded-For`; a missing or malformed chain is rejected with 400
rather than falling back to a source Halro cannot vouch for.

The Gateway needs no hostname configuration of its own. It authenticates with a
Gateway Key rather than a browser session, so nothing about it depends on
knowing its own external address; the hostname matters only to the certificate.
Its paths are the ones the OpenAI and Anthropic APIs use — `/v1/chat/completions`,
`/v1/messages`, and so on — with `/health/live` and `/health/ready` beside them.
The Admin listener serves `/admin` and `/admin/api/v1/*`. Because those prefixes
do not overlap, one hostname can carry both:

```
halro.example.com {
	handle /health/* {
		reverse_proxy 127.0.0.1:8080
	}
	handle /v1/* {
		reverse_proxy 127.0.0.1:8080
	}
	handle {
		reverse_proxy 127.0.0.1:8081
	}
}
```

`/health/*` exists on both listeners, so route it deliberately. Do not strip the
`/v1` prefix: Halro and every SDK work from the API's absolute paths. The console
is served under `connect-src 'self'`, so its page and its API must remain on one
origin — `/admin` and `/admin/api` cannot be split across hostnames. A proxy also
has to pass streaming through untouched: disable response buffering and set read
timeouts longer than the longest streamed response (`proxy_buffering off;` and
`proxy_read_timeout` for nginx; Caddy needs neither).

### Endpoints and client base URLs

Paths come from the code and are the same in both shapes. There is no `/api`
prefix in front of the Gateway: its paths are the OpenAI and Anthropic paths
unchanged, which is what protocol compatibility means here — an SDK changes its
base URL and nothing else.

| Listener | Paths |
| --- | --- |
| Gateway | `POST /v1/chat/completions`, `/v1/responses`, `/v1/embeddings`, `/v1/moderations`, `/v1/images/generations`, `/v1/audio/speech`, `/v1/audio/transcriptions`, `/v1/rerank` |
| Gateway (async and batch) | `/v1/files`, `/v1/batches`, `/v1/async/invocations` and their GET and cancel sub-paths |
| Gateway (Anthropic) | `POST /v1/messages`, `POST /v1/messages/count_tokens` |
| Gateway | `GET /health/live`, `GET /health/ready`, `GET /` |
| Admin | `/admin`, `/admin/*` (the console), `/admin/api/v1/*`, `GET /health/live`, `GET /health/ready` |
| Metrics | `GET /metrics`, `GET /health/live`, and `GET /audit/anchors` when the dead-man anchor sink is enabled |

The two SDKs disagree about where `/v1` belongs, and the disagreement is theirs
rather than Halro's: the OpenAI client appends `/chat/completions` to whatever
base URL it is given, while the Anthropic client appends `/v1/messages`.

```python
from openai import OpenAI
from anthropic import Anthropic

# OpenAI SDK: the base URL carries /v1
OpenAI(base_url="https://halro.example.com/v1", api_key="gw_...")

# Anthropic SDK: the base URL does not
Anthropic(base_url="https://halro.example.com", api_key="gw_...")
```

```bash
curl https://halro.example.com/v1/chat/completions \
  -H "Authorization: Bearer gw_..." \
  -H "Content-Type: application/json" \
  -d '{"model":"<public alias>","messages":[{"role":"user","content":"hi"}]}'
```

`model` is the public alias the Project allows, never the upstream model
identifier: an application holds a Gateway Key and an alias and never sees the
Provider credential behind either. Where Halro terminates TLS on its own port,
the base URL carries that port — `https://halro.example.com:8080/v1`. The
path-split proxy above is how a deployment avoids a port number in the URL.

### Reloading without a restart

`SIGHUP` replaces a fixed set of material in place. It is not "re-read the
configuration": the list below is closed, and everything outside it still
requires a restart.

| Reloaded on `SIGHUP` | Notes |
| --- | --- |
| `tls.certificates` file contents | All entries are loaded together, then published in one step |
| `metrics.tls` certificate and client CA | Published as one pair, never one without the other |
| `logging.level` | Read from the configuration file, which must still validate as a whole |
| The log file handle | For the rotate-then-signal convention external tooling uses |

Not reloaded, and why: listener addresses need a fresh bind; `server.*` timeouts
live on running HTTP servers; `security.trust_proxy_headers`,
`security.trusted_proxy_cidrs`, and `admin.external_origin` decide who is
believed and who may act, and a change to that should leave a restart in the
record rather than ride along on a signal; `storage.*` and Master Key settings
are the consistency and key boundary. The rule is that a reload replaces
material, never semantics. Note that the *paths* in `tls.certificates` are not
reloadable either — only the bytes behind them. Adding a certificate to the list
is a restart.

Rotating a certificate:

```bash
# write both files first: the pair is read together
install -m 0600 new-fullchain.pem /etc/halro/tls/fullchain.pem
install -m 0600 new-privkey.pem   /etc/halro/tls/privkey.pem
systemctl reload halro     # or: kill -HUP $(pidof halro)
```

Under systemd, `ExecReload=/bin/kill -HUP $MAINPID`. With certbot or acme.sh, do
it from the deploy hook (`--deploy-hook`), so the signal follows the files. In
flight connections are not interrupted; new handshakes use the new certificate.

Each item is applied independently, and a failure keeps that item's previous
value. A keypair that does not load leaves the old certificate serving and
records `halro_reload_total{item="tls",status="error"}` — the instance stays up.
That is deliberate: refusing to serve because a *replacement* was bad would turn
a fixable mistake into an outage. `halro doctor --config <path>` runs the same
certificate check offline and can be used while the instance is serving.

Each published certificate is logged with its first DNS name, its expiry, and a
prefix of its SHA-256 — the same digest `openssl s_client -connect host:port |
openssl x509 -fingerprint -noout` reads back, so a rotation can be confirmed
from either end. A certificate with less than 30 days left is logged as a
warning and one already expired as an error; both are still served, because
refusing them would remove the mechanism needed to replace them. The same
fingerprint appears under `reload.certificates` in
`/admin/api/v1/system/status`, which answers "which file is in force" when the
path has been written over since startup.

A client asking for a name no certificate declares is answered with the first
entry and logged as a warning naming what was asked for. That record is rate
limited to one per minute with a count of what it stood in for: the name is
chosen by whoever dialled the port, and an exposed listener under a scanner
would otherwise push everything else out of a size-bounded log.

Rotating the Metrics client CA takes two signals, in this order:

1. Concatenate the old and new CA into `metrics.tls.client_ca_file`, `SIGHUP`.
   Both old and new scrapers are now accepted.
2. Move each scraper to its new client certificate.
3. Reduce the file to the new CA alone, `SIGHUP`. The old identity is refused.

Doing step 3 first refuses every scraper that has not moved yet.

For the log file, have the rotator signal after it renames:

```
/var/lib/halro/logs/halro.log {
    postrotate
        /bin/kill -HUP $(cat /run/halro.pid)
    endscript
}
```

`SIGHUP` is defined but never delivered on Windows; that platform changes
certificates by restarting.

Because the running configuration can now differ from the file on disk, the
console reports what is actually in force. Settings → Diagnostics carries a
Certificates and reloading card: one row per certificate with its expiry and
fingerprint, and one row per reloadable item with when it was last applied —
items this deployment has nothing to reload keep their row and say so, so
"never ran" and "not configured" do not read alike. The same data is on
`/admin/api/v1/system/status`, and `/admin/api/v1/system/config` renders the
effective values rather than re-reading the file. `halro_reload_last_success_timestamp_seconds`
carries the same answer to Prometheus. A reload also logs, once, which
configuration sections changed on disk in ways it could not apply — the restart
list, discovered at the moment someone expected the edit to take.

### Logging

The process log is JSON on stderr at `info` by default, which suits systemd and
Docker and leaves nothing behind for an operator running the binary directly.
Set `logging.output` to `file` or `both` to also keep a copy on this host —
`logging.file` defaults to `logs/halro.log` inside the data directory, rotated
at `logging.max_size_mb` and kept for `logging.max_files` generations, counting
the file being written. Rotated generations are `halro.log.1`, `halro.log.2`,
and so on; the file and its directory are created 0600/0700.

Records are redacted before they are written, whatever the level: no
authorization headers, Provider or Gateway keys, prompts, response bodies, or
raw source IPs. Raising the level to `debug` changes how much is written, never
what is allowed into a record. If the log file cannot be written — a full disk,
a revoked permission — records go to stderr with one notice explaining why,
rather than being dropped silently.

A failed Provider attempt is logged at `WARN` as `provider attempt failed`,
carrying the request ID, public model, deployment, provider and binding IDs, and
the error class that decided whether it was retried. The upstream's own sentence
is not written: it is a response body, and the one thing an upstream is most
likely to quote back is the credential it just refused. A refusal Halro produced
itself — a transport policy rejection, a response it would not decode — carries
no Provider body and is logged with its cause, which is the case where an
operator most needs it.

A request that ends badly is logged once, at `ERROR`, as `request failed`. That
is a different event from the line above, and the difference is the point: a
request that failed one target and succeeded on the next leaves a
`provider attempt failed` behind and is not a failed request. The terminal
record carries `request_id`, the ledger `outcome`, the `phase` the failure
happened in, the error class, the upstream status, the Provider error code and
Provider request ID where the upstream supplied them, the deployment and
provider, how many attempts and fallbacks the request spent, its latency, and
`accounting_recorded` — which is `false` when the ledger could not take the
terminal record, and therefore when this log line is the only account of the
request that exists anywhere.

**`request failed` is written for two of the six non-success outcomes**,
`provider_error` and `accounting_error` — with one exclusion inside the first:
a request the caller cancelled writes none. A client hanging up is driven
entirely from outside Halro, and a frontend deploy or a gateway restart cancels
every request in flight at once, which is the same flood the four policy
outcomes are excluded to prevent. A deadline that expired is not this, and does
write a record. The other four — `rejected` (budget,
circuit breaker, target concurrency), `token_guard_rejected`,
`unsupported_feature` and `policy_rejected` — are a policy working as
configured. A client in a retry loop produces them at its own rate rather than
at the rate things break, so writing them would fill a bounded error file in
minutes and push the incident's first real error out of it. They are still
failed requests: they count toward `request_errors`, they appear in the
console's failed-request list, and the rate-limit and breaker figures explain
them.

The consequence is worth stating plainly, because two numbers that nearly match
invite being used to check each other: **the count of `request failed` records
is a subset of the failed-request count the console reports, never equal to it.**
Neither number is evidence about the other. Note also that both exclude
everything refused before admission — an invalid Gateway Key, an unrouted model,
an RPM/TPM refusal — because those return before a ledger request exists at all;
the HTTP metrics and the audit log account for those.

#### How far back the console pages

`usage.console_window_days` (default 30) bounds the attempt log and the
failed-request list. **The file seeds it once and has no say afterwards**: the
value lives in the metadata store and is changed under Settings → Instance,
where it is versioned, audited, and — because shortening it discards history —
confirmed. An operator who edits the file on a running install and restarts will
find the console still showing what they set in the console, which is the point.

It is a different setting from `usage.retention_days` (default 90), and the
difference is worth knowing before either is changed:

| Setting | Bounds | Cost of a long value |
| --- | --- | --- |
| `usage.console_window_days` | The in-memory aggregate the two console tabs read | Memory, about 1 KB per attempt, plus the same again for the checkpoint on disk |
| `usage.retention_days` | The Parquet archive on disk | Disk only; partitions are columnar and one day is measured in kilobytes |

Both have a floor of 7 days, and it is the same floor: the console window may
not go below seven days, and it may not exceed the archive, so an archive
shorter than that leaves the pair with no value that satisfies both. A file that
sets a short retention and never mentions the window gets a window equal to the
retention rather than the usual thirty days.

So a long archive is cheap and a long console window is not. Binding them to one
value would make an operator who needs ninety days of archive pay for it in
memory; they are separate for that reason. The console window must be at least 7
days, because the overview's chart reads seven days out of the same aggregate,
and no longer than the archive, because the screen should not promise history the
archive no longer holds.

**Size it against your throughput, not against how much history you would like.**
An attempt costs about a kilobyte twice over — once resident, because the
console reads the aggregate out of memory, and once on disk, because the
checkpoint holds the same records so a restart does not have to replay the whole
WAL to get them back:

| Throughput | Attempts in a 30-day window | Resident memory | Checkpoint on disk |
| --- | --- | --- | --- |
| 0.1/s | 260 thousand | ~0.27 GB | 0.28 GB |
| 1/s | 2.6 million | ~2.7 GB | 2.8 GB |
| 10/s | 26 million | ~27 GB | 27.1 GB |
| 100/s | 260 million | ~272 GB | 271 GB |

**Memory is the binding constraint, and it is a floor rather than a spike.** The
aggregate is resident for as long as the process runs, so an instance at ten
requests a second wants tens of gigabytes of RAM for a thirty-day window whether
or not anything is being written. Size the container against the resident column
and shorten the window until it fits; the archive keeps the history either way,
and `halro usage` can export it. The shipped Kubernetes manifests set a memory
limit for a small install — raise it deliberately rather than discovering the
ceiling as an OOM kill.

What a checkpoint *tick* costs is no longer part of that decision. The
checkpoint is a head plus a series of immutable record segments, and a tick
rewrites the head and the one open segment — bounded at four mebibytes, whatever
the window's length — so its cost follows what arrived since the last tick, not
what the window holds. Three metrics say whether that is still true:
`halro_usage_checkpoint_bytes` is the whole checkpoint,
`halro_usage_checkpoint_open_segment_bytes` is what a tick rewrites, and
`halro_usage_checkpoint_segments` is how many segments hold it. The first two
converging would mean a tick is writing the window again.

The one exception is the first checkpoint after a cold start with no usable
checkpoint — an upgrade that changed the format, a rebuild, a `halro usage
rebuild-summary`. That one pass encodes the whole window, in segments, and every
tick after it is incremental again.

Shortening the window is the one destructive change in that screen. What falls
outside it is trimmed out of memory on the next export tick and the two tabs can
no longer reach it; the records themselves stay in the Parquet archive, and
`halro usage` can still export them, but lengthening the window again does not
bring them back — that takes a rebuild by replaying the ledger. The console asks
for an explicit confirmation on the way down and none on the way up.

The window is trimmed on the Parquet export tick, and only up to what that
export has actually written. If the export stalls, the trimming stalls with it
and the aggregate grows — deliberately: an aggregate that grows is a problem,
one that discards history the archive never received is a defect. Nothing is
lost to the trim that is not already in a partition.

#### Keeping the ledger from being one file that only grows

The accounting write-ahead log is replayed from byte zero on every start, so
nothing in it can be deleted. Until sealing, that meant one file that grew for
the life of the instance — about 5 KB per request, or roughly 4 GB a day at ten
requests per second, forever.

`ledger.seal.enabled` (off by default) changes the shape of that. Once the
active generation passes `ledger.seal.max_active_bytes` (default 8 GiB) it is
rolled off whole: renamed to `ledger-<N>.wal`, recorded in
`<data_dir>/ledger/segments.json`, and replaced by an empty successor whose
first frame continues the same hash chain. On a later maintenance tick, once
everything in that generation has reached both the Parquet archive and the
durable usage checkpoint, `ledger.seal.compress` replaces it with
`ledger-<N>.seg.gz` — measured at 5.4x on a real `ledger.wal`, with the
plaintext length and checksum verified before anything points at the compressed
copy.

Nothing is deleted. Replay reads the sealed generations in order and then the
active file, so balances still rebuild from the whole history, and
`halro ledger verify` and `halro doctor` authenticate every generation rather
than only the one being written. What sealing gives you is a bounded active
file, a 5x smaller archive, and — because a sealed generation is immutable and
named — the ability to move old generations onto cheaper storage yourself.

`halro ledger seal` draws the boundary on demand. It takes the data directory
lock, so it is an offline command: use it before copying a data directory or
handing an auditor an archive, when you want the cut made now rather than
whenever the file next crosses the threshold.

Three things follow from a sealed data directory, and all three are enforced:

- **A generation that goes missing is refused, not skipped.** Deleting
  `ledger-1.seg.gz` because it looked like a leftover makes the instance fail to
  start rather than start with a shorter history.
- **A backup carries every generation.** `.hmbk` archives stage the segment
  files and the manifest beside the active WAL; a restore puts them back.
- **A roll is interruptible.** The rename is the commit point, and an open that
  finds a half-finished roll decides from the files which side it is on.

#### Capturing what a failed call carried

`gateway.failure_capture.enabled` keeps the request a failed call sent upstream
and the answer that came back, so a failure can be reproduced rather than
guessed at. It is off by default, and turning it on is a decision about what
this instance's data directory contains rather than a verbosity setting.

Everything else Halro persists is metadata it produced itself — identifiers,
counts, classes, costs. This is the only store that holds material a caller
wrote, and the rule that prompts and response bodies never reach a log, a
metric or an audit record is unchanged: this is a separate, narrower act.

```yaml
gateway:
  failure_capture:
    enabled: true
    max_bytes: 65536          # each side truncated separately
    max_records_per_day: 1000
    retain: 24h               # 1h to 720h
```

What is captured, and what is not:

| Outcome | Captured | Why |
| --- | --- | --- |
| `provider_error` | yes | The request and the upstream's reply are the diagnosis. Except when the caller cancelled: nobody hung up on Halro's account, and one frontend deploy cancels every request in flight at once. |
| `unsupported_feature` | yes | Which field the target could not serve is only visible in the request. |
| `policy_rejected` | **no** | Storing the content redaction just refused would make the capture the leak the policy prevents. |
| `rejected`, `token_guard_rejected` | no | Never reached an upstream; nothing to reproduce, and a runaway client produces them at its own rate. |
| `accounting_error` | no | The payload says nothing about the ledger being unavailable. |
| success | no | This is what keeps the store a small tail of traffic rather than a copy of it. |

The guarantees the store is built on, each of which is worth checking against
your own compliance position before enabling it:

- **Encrypted and bound.** Every record is sealed under the master key with the
  request ID and project ID as associated data, so a record cannot be renamed
  onto another request, opened under another project, or lifted into another
  install's directory.
- **Post-redaction.** Capture happens after the project's redaction policy has
  run, so what is stored is what went upstream and not what the caller sent.
- **Bounded.** Each side is truncated at `max_bytes` and flagged as truncated;
  each day is capped at `max_records_per_day`, past which capture stops for the
  day and logs one line.
- **Expiring.** Each record is removed once it is older than `retain`, swept on
  the Parquet export tick and again at shutdown. This is the answer to "how long
  is caller content kept", enforced rather than promised — so the sweep runs
  whether or not capture is currently enabled. Switching it off stops new
  writes; it does not strand what is already there.
- **Audited on read.** `GET /admin/api/v1/usage/failures/{requestID}/payload`
  is the only admin GET that writes an audit record —
  `usage.failure_payload.read` — because it is the only one that returns a
  prompt. In the console it is inside the failure-detail drawer, behind its own
  Show button, and nothing is cached: opening the drawer does not read a
  payload, so browsing failures files no audit records.
- **Best-effort.** A capture that cannot be written is dropped. It never changes
  what the caller is told, and never fails a request that has already failed.

Files are created 0600 in a 0700 directory. `halro backup` stages the metadata
database and the Ledger WAL by name rather than copying the data directory, so
captures are **not** in an archive — a restore comes back with no captured
payloads, which is the right default for material with an expiry on it. Note
that a restore renames the previous data directory aside rather than deleting
it, so the captures it held sit in `.halro-pre-restore-*` until you remove it;
nothing sweeps a directory Halro no longer owns.

Two more things worth knowing before you turn it on:

- **Enabling or disabling it needs a restart.** `SIGHUP` reloads certificate
  bytes and the log level and nothing else, so a reload will report success and
  change nothing here. The same is true of `logging.error_file`.
- **Rotating the master key makes existing captures unreadable.** Each record is
  sealed under the key in force when it was written, and key rotation rewraps
  the credential store, not this one. Every "Show" then answers "nothing was
  captured for this request" until the pre-rotation records age out of `retain`.

Sizing: the worst case is `max_records_per_day` × `max_bytes` × 2 sides × the
number of days in `retain`. At the defaults that is about 250 MB; at the
configuration maxima it is far more than the ledger this shares a disk with, so
raise them deliberately.

#### Errors-only file

`logging.error_file.enabled` writes a second copy of the log holding `ERROR`
records alone, beside the ordinary log rather than instead of it. Setting
`logging.level: error` would get the same file by throwing away everything that
made the ordinary log worth keeping — an expiring certificate, a failed probe,
an attempt retried before its request succeeded. This keeps both: stderr stays
at `info` or `warn`, and the second file collects the errors.

```yaml
logging:
  level: info
  output: stderr
  error_file:
    enabled: true
    file: ""          # default: <data_dir>/logs/halro-error.log
    max_size_mb: 32
    max_files: 10
```

Its level is fixed at `ERROR` and its encoding at JSON. Neither follows
`logging.level` or `logging.format`: a threshold that could be lowered would
make it a second copy of the main log, and the one destination that exists to
be grepped and pasted into a ticket should not be the harder of the two to
parse. `logging.error_file.file` must not name the same path as `logging.file`
— two sinks on one path each hold their own offset and rotate on their own
count, so they would overwrite each other's records — and startup refuses that
configuration. Both files are created 0600 in a 0700 directory, both fall back
to stderr with one notice if they cannot be written, and one `SIGHUP` reopens
both, so a logrotate rule that moves the pair aside is answered by a single
signal.

Because of the rule above, this file holds fewer records than the console
reports as failed requests. That difference is the design working, not a gap.

Unknown YAML fields and invalid durations are rejected. Listener, storage,
egress, proxy, and Metrics-auth changes require restart, as do the certificate
*paths* — `SIGHUP` reloads the bytes behind them and the log level, and nothing
else. The Admin Settings page only changes the explicitly writable runtime
settings. Always run `config check` before restart.

`server.shutdown_timeout` is shared by the Gateway, Admin, and Metrics
listeners and must be at least `gateway.route_total_timeout`. When omitted by
an older configuration it inherits the effective route timeout. If the budget
expires, Halro records still-active Provider attempts in the durable
`halro_shutdown_truncated_attempts_total` counter and then forcibly closes the
remaining connections. Service managers and orchestrators must grant extra
termination headroom beyond this value; the shipped Kubernetes manifest uses
150 seconds for the default 120-second Halro budget.

Admin localization is metadata, not YAML configuration. The public
`GET /admin/api/v1/ui/bootstrap` endpoint exposes only the instance default and
supported locale identifiers so the setup/login shell can render before
authentication. Authenticated administrators can update their own preference
through `/admin/api/v1/preferences`; the instance default uses
`/admin/api/v1/settings/ui`. Both mutation paths require CSRF protection and an
`If-Match` revision, are audited, and never affect Gateway protocol payloads.
Existing databases are initialized with `zh-CN`; older administrator records
without a locale are interpreted as `system`.

The console window is metadata in the same sense. `GET`/`PUT
/admin/api/v1/settings/usage` read and change how far back the attempt log and
the failed-request list can page, with the same CSRF, `If-Match` and audit
discipline. A `PUT` that shortens the window is refused with
`console_window_trim_unacknowledged` unless it carries `acknowledge_trim`,
because the trim discards attempt history the moment the next export tick runs;
lengthening it needs no acknowledgement, because nothing is lost by it.

### Time zones

The accounting time zone decides one thing: where a day ends. It takes an IANA
name, and two surfaces follow it — only two:

- daily budgets — a project's `daily_budget_micros_usd` resets at midnight in
  this zone, and the period a call is charged to is that zone's calendar date;
- the "today" figures on the Admin Console overview, which are summed over the
  same period. Every response reporting one carries a `time_context` naming the
  zone and the exact UTC interval, and the console renders every timestamp in
  that zone so the charts and the totals describe the same day.

Everything else is UTC and stays UTC regardless of this setting: Parquet
partition dates, retention pruning, price-version effective times, audit
records, backup manifests, and authenticator codes. Storage layout is not an
accounting judgement — partitioning by a configurable zone would move a single
attempt between partitions whenever the setting changed.

`usage.retention_days` is therefore a floor rather than an exact age. Partitions
are dated in UTC while the promise is read in the operator's own day, so pruning
keeps one extra day; an instance at UTC+8 never loses a local day it was told it
still had.

For the same reason, do not reconcile the Usage summary against the Parquet
files by counting them. A partition is a UTC day; the summary — like the daily
budget and the dashboard's "today" — is an accounting day, and on any instance
away from UTC the two contain different calls at their edges. The summary reads
neither the partitions nor their dates: it reads a rollup keyed by the
accounting period each event was stamped with when its request was admitted, so
a request accepted at 23:59 and settled at 00:02 is reported on the day it
reserved budget against and appears in the *next* day's partition.

Only IANA names are accepted; a fixed offset such as `UTC+08:00` cannot express
summer time and would make the days on either side of a transition the wrong
length. A day is 23 or 25 hours where summer time applies, and the daily budget
covers that whole calendar day rather than a fixed 24 hours.

`usage.timezone` in config.yaml **seeds** this setting on an instance's first
start and has no say afterwards. The stored value is versioned and audited;
editing the file later does not move the boundary. `halro doctor` reports the
drift as an `accounting_timezone` warning when the two disagree.

Change it under Settings → Instance, or through
`PUT /admin/api/v1/settings/accounting`. A change never applies immediately: it
is scheduled for the end of the period in progress, because redefining a day
that is already being billed would change what budgets already enforced against
it meant. Until then it shows as pending and can be cancelled. Nothing already
recorded is recomputed — every ledger event carries the zone, the version and
the exact UTC interval it was filed under, so a charge can be re-derived from
the record alone.

Each applied change increments a timezone version that forms part of the ledger
balance key, so periods either side of a change are separate balances and can
never merge. A request that outlives a boundary settles in the period it began
in, matching how its price snapshot is pinned.

The rules themselves come from the IANA database embedded in the binary, so zone
resolution does not depend on what the host ships. `halro version` and the
`tzdata` check in `halro doctor` report the source, release, and a
fingerprint of the transition table; the same values are exported as
`halro_tzdata_info`. Across a fleet those fingerprints must match — nodes
resolving different rules would place the same instant in different accounting
periods, and nothing else would reveal it.

## Offline diagnostics and break-glass access

Stop Halro, then run the read-only diagnostic before upgrades, restores, or
when startup fails:

```bash
./halro doctor --config ./config.yaml
```

The JSON report verifies configuration safety, exclusive data ownership, file
permissions, the exact bbolt schema, Master Key/Vault binding, the WAL checksum
and complete tail, Parquet manifests/checksums, the accounting timezone and the
time zone database behind it, free disk space, and Provider/Deployment/Route
references. A partial WAL is reported as a
failure but is never truncated or repaired. Network Provider probes are
intentionally skipped; run audited connection tests in Admin after startup.
The diagnostic acquires the already initialized lock file through a read-only
descriptor and does not rewrite its PID metadata. Regression tests hash every
file under the data directory before and after a successful run.

If the local Admin password is lost, keep the server stopped and pipe a new
password through standard input:

```bash
printf '%s' "$NEW_ADMIN_PASSWORD" | ./halro admin reset-password \
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

The default stays `enabled` because a loopback-only Admin listener — the
quickstart, and where the first-run checklist sends you to prove the chain end
to end — exposes it to nobody else. On an Admin listener bound to a routable
address the trade is real, and Halro says so at startup:

```
WARN developer workbench serves Gateway calls on a non-loopback Admin listener;
     network controls applied only to the Gateway listener do not cover this path
```

Treat that warning as a decision to make, not noise: either isolate the Admin
listener or set `developer_workbench: "disabled"`.

### Authenticator two-factor authentication

Set `admin.mfa_policy` to `optional` (the upgrade-compatible default) or
`required`. Remote Admin deployments should use `required`. Halro implements
standard 6-digit, 30-second TOTP and works with Microsoft Authenticator, Google
Authenticator, 1Password, and other compatible applications. Production hosts
must keep UTC time synchronized.

If every authenticator and recovery code is lost, stop Halro and run:

```bash
./halro admin reset-mfa --config ./config.yaml --username admin
```

This removes all factors and recovery codes, invalidates sessions and pending
challenges, and appends `admin.mfa.reset_offline` to the trusted Audit chain.
With `mfa_policy: required`, the next password login is restricted to setup.

### Re-authentication for destructive actions

Deleting a Route, Deployment, Provider, Project, Gateway Key, webhook or policy
— and any edit that replaces credential material or takes a protection out of
force — asks for the administrator's own password, plus a TOTP code where the
account has an authenticator. A session cookie alone is not enough to do them.

`admin.reauth_elevation_window` (default `10m`) is how long one such proof keeps
counting. Inside the window the console stops asking; the confirmation dialog
still states what is about to happen, and the credential fields appear only once
the window has closed. The grant belongs to the one session that earned it: a
second browser inherits nothing, signing out ends it, and changing the account
password invalidates it. Set `0s` to be asked on every action.

Two groups stay outside the window and are asked every time — the admin-account
endpoints (password change, authenticator removal, disabling MFA, creating or
deleting an administrator) and minting a Gateway Key. Those hand out or preserve
access rather than ending it, which is what an intruder holding a stolen session
would reach for first.

Five failed attempts per account per minute are refused with `429`
`reauth_rate_limited`; every failure and every throttled window is appended to
the Audit chain as `admin.reauthentication`.

## Master Key rotation

The `--new-key-file` procedure below applies to `storage.master_key.mode: file`.
Use the complete [file-mode Master Key rotation runbook](../runbooks/file-master-key-rotation.md)
for prerequisites, interruption recovery, validation, and rollback evidence.
For `key_slots`, Halro generates the new Master Key only in memory and
requires a stable, non-secret operation ID so a command interrupted after
publication can be retried without accidentally creating another generation:

```bash
./halro key rotate --config ./config.yaml \
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
stop Halro, retain the old Master Key with backups created under it, and
generate a separate replacement key with mode `0600`:

```bash
umask 077
openssl rand 32 > /secure/path/new-master.key
./halro key rotate --config ./config.yaml \
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
| OpenAI | `https://api.openai.com` | API key | GA chat/embeddings, GA Responses, or isolated Phase 2 media/resources profile |
| Azure OpenAI | resource endpoint | API key plus explicit API version on Provider | GA deployment paths |
| DeepSeek | `https://api.deepseek.com` | API key | GA chat/stream profile |
| OpenAI-compatible | reviewed HTTPS origin | API key | conservative capabilities; opt in extras |
| Gemini | `https://generativelanguage.googleapis.com` | API key | Beta text chat/stream and float embeddings |
| Bedrock Runtime | `https://bedrock-runtime.us-east-1.amazonaws.com` | JSON below | Beta Converse, Titan Embeddings/Image, or Nova Reel Async profile |
| Bedrock Agent Runtime | `https://bedrock-agent-runtime.us-east-1.amazonaws.com` | JSON below | Beta Cohere Rerank 3.5 profile only |
| Bedrock Mantle | `https://bedrock-mantle.us-east-1.api.aws` | Bedrock API key | Beta OpenAI Chat, stateless Responses, or Anthropic Messages |

An OpenAI connection on the Responses profile addresses `/v1/responses`
instead of `/v1/chat/completions`. It is the same account and the same
credential; what it adds is `web_search`, a tool OpenAI runs itself. That
capability is at the profile's ceiling and off in its defaults, so it takes a
deliberate tick per connection — and what you are accepting when you tick it is
that the provider makes network calls on your behalf that never pass through
Halro's host allowlist and never appear in its audit trail. The profile does not
stream and does not serve embeddings; keep the chat/embeddings connection for
those.

Bedrock JSON is one encrypted secret. `session_token` is optional and `region`
must match the endpoint hostname:

```json
{"access_key_id":"...","secret_access_key":"...","session_token":"...","region":"us-east-1"}
```

Model discovery for the Converse profile reads the regional Bedrock control
plane, which is a different host from the runtime endpoint that serves traffic:
`bedrock-runtime.<region>.amazonaws.com` implies `bedrock.<region>.amazonaws.com`
(and the matching FIPS, China, and dual-stack forms). Halro derives it from the
endpoint you approved and never leaves that partition or region, but it is still
an outbound call, so it must satisfy the Provider's allowed-hosts policy. If that
policy lists only the runtime host, or the endpoint is a PrivateLink or Agent
Runtime host with no control plane to derive, discovery reports the binding
degraded and the console falls back to entering the model ID by hand. Nothing
else about the deployment changes. The profiles that accept exactly one model —
Titan Embeddings, Titan Image, Nova Reel, Cohere Rerank — answer from that pin
and make no call at all. Discovery requires `bedrock:ListFoundationModels`; it is
read-only and lists no customised or provisioned resources.

The Bedrock Runtime profiles do not read environment credentials or IMDS. The
Converse profile does not declare embeddings, tools, vision, or either JSON
output capability. The
separate `bedrock.runtime.invoke.titan-embed-text-v2.v1` profile declares
embeddings only, pins `amazon.titan-embed-text-v2:0`, and accepts one string with
float output and 256/512/1024 dimensions. It never exposes arbitrary InvokeModel
JSON or hidden batch fan-out. Connection tests are audited; they never return
upstream response bodies or credentials.

Phase 2 resource creation requires `Idempotency-Key`. Files also require the
`Halro-Route` header because multipart file creation has no `model` field.
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

The opt-in real-provider test accepts `HALRO_SMOKE_OPERATION=embeddings` with
`HALRO_SMOKE_MODEL=amazon.titan-embed-text-v2:0`; it incurs one AWS inference
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

`halro stats` prints this instance's durable write path — mean Ledger fsync,
records per fsync, per-project accounting lock wait and hold, metadata write
coalescing — without requiring a Prometheus install; `-interval 10s` reports a
window instead of the lifetime average. The same summary is on
Settings → Diagnostics, and the underlying series are on the Metrics endpoint.
Those numbers are what bound this instance's request rate, so read them before
quoting any capacity figure measured on another host: fsync cost differs by one
to two orders of magnitude between filesystems. See
`docs/verification/standalone-capacity-baseline.md`.

Pricing selection uses a persisted per-Deployment high-water mark. Keep host
time synchronized and configure `gateway.pricing_clock_rollback_tolerance` and
`gateway.pricing_clock_forward_tolerance`; a material rollback, unexplained
forward jump, or incoherent restored high-water enters pricing quarantine and
blocks that Deployment. Both keys take their default when omitted, and the
rollback tolerance has a 1s floor: concurrent selections on one Deployment can
reach their durable pin out of the order they read the clock, so a tolerance
tighter than that would quarantine a Deployment for a rollback the gateway itself
caused. Tightening it below the floor is refused at startup. `gateway.pricing_unknown_policy` defaults to `reject`.
The only opt-in value, `allow_without_cost_governance`, still rejects unknown
pricing for Projects with a daily budget or a cost-dimension Token Guard. Watch
`halro_pricing_quarantined_deployments`, Accounting Lease recovery metrics,
readiness, and `halro doctor` after every restart or restore.

Price changes create future-effective immutable versions; never edit old
prices to correct history. Unknown cost remains null — there is no historical
correction path.

Pricing Proposals are a separate review queue. The interactive Admin API accepts
only asserted official-URL evidence; `verified_api` and `signed_import` are
reserved for trusted adapters that verify transport or signatures before
constructing a Proposal. Evidence retains model, region, tier, match status,
warnings and expiry. A Proposal is inert: it cannot affect admission, budgets,
settlement, or the Gateway. Adoption requires recent Admin re-authentication
and creates a new immutable Price Version; ambiguous or expired Proposals
cannot be adopted.

1. Read release notes and verify the binary checksum and Sigstore bundle. The
   complete, copyable command sequence — including the exact
   `--certificate-identity` for the tag — is in `README.md` under "Verify
   release downloads" and in the release notes; do not verify against an
   unsigned `checksums.txt`.
2. Stop Halro and confirm the process released the data-directory lock.
3. Create and verify an encrypted backup; preserve the current binary/config.
4. Run the new binary's `config check` against a copy of the configuration.
5. While the service is stopped, run
   `halro pricing migrate --config <config> --dry-run --report <report.json>`.
   Resolve every enabled zero-price Deployment in a schema-v1 resolution file
   bound to the report's `metadata_sha256`, then run
   `halro pricing migrate --config <config> --resolution-file <file> --apply`.
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

- Startup says the data directory is locked: another Halro or offline
  command owns it. Find the owner; do not delete lock files to bypass it.
- Readiness is false: inspect accounting state and WAL errors first. Halro
  deliberately stops new provider calls when durable accounting is unsafe.
- Provider test is unhealthy: verify HTTPS hostname, credential audience/type,
  model deployment, Azure API version, and Bedrock region. Upstream bodies are
  intentionally hidden; use the provider control plane for detailed diagnosis.
- Requests return 502 `provider_error`: the response deliberately carries no
  upstream detail. Read `provider attempt failed` in the log for the error class
  and, when Halro produced the refusal itself, its cause. Set
  `logging.output: both` first if nothing is collecting stderr.
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
UID/GID, and healthcheck metadata. Tagged releases push
`ghcr.io/akz142857/halro` and `ghcr.io/akz142857/halro-deadman` as
multi-architecture images (`linux/amd64`, `linux/arm64`) and also attach signed,
checksummed `halro-container-<arch>.tar.gz` archives; load one with
`gzip -dc halro-container-amd64.tar.gz | docker load`.

The registry images themselves are not cosign-signed and carry no registry
attestation — the signed, attested artifacts are those archives. Pull the
published image by digest, or verify an archive and mirror it into a registry
governed by your deployment. Either way, replace the shipped Kubernetes
manifest's `ghcr.io/OWNER/halro@sha256:REPLACE_WITH_REVIEWED_DIGEST`
placeholder with the digest you actually reviewed. Do not deploy the
placeholder verbatim.

```bash
docker build -t halro:v1.0.0 .

# config.yaml must set all three of these for the container shape:
#   storage.data_dir:         /var/lib/halro/data     (a child of the mount)
#   storage.master_key.file:  /run/secrets/halro-master.key
#   server.gateway_listen:    0.0.0.0:8080            (see below)
mkdir -p ./halro-secrets
# The container runs as uid 65532 and a bind mount keeps the host's ownership,
# so that uid must be able to write here for `init` to create the key.
# Preferred (needs root on the host):   sudo chown 65532:65532 ./halro-secrets && chmod 700 ./halro-secrets
# Otherwise, widen it only for init:    chmod 777 ./halro-secrets
sudo chown 65532:65532 ./halro-secrets && chmod 700 ./halro-secrets
docker volume create halro-data
docker run --rm --user 65532:65532 \
  -v "$PWD/config.yaml:/etc/halro/config.yaml:ro" \
  -v "$PWD/halro-secrets:/run/secrets" \
  -v halro-data:/var/lib/halro \
  halro:v1.0.0 init --config /etc/halro/config.yaml
# init creates the key 0600 and owned by 65532; nothing further is needed.

docker run --rm --user 65532:65532 \
  -v "$PWD/config.yaml:/etc/halro/config.yaml:ro" \
  -v "$PWD/halro-secrets:/run/secrets:ro" \
  -v halro-data:/var/lib/halro \
  -p 127.0.0.1:8080:8080 \
  halro:v1.0.0 serve --config /etc/halro/config.yaml \
    -allow-insecure-public-listen
```

**`server.gateway_listen` must be `0.0.0.0:8080` for this example to work.** The
shipped default is `127.0.0.1:8080`, which inside a container binds the
*container's* loopback: `docker run -p` then publishes a port nothing is
listening on, and every request from the host is refused while `docker ps` still
reports `healthy` (see the healthcheck note below). Binding a non-loopback
address is what makes `-allow-insecure-public-listen` necessary rather than
decorative — the two go together, and neither alone is enough.

Mount the persistent **parent** at `/var/lib/halro` and configure its child
`/var/lib/halro/data` as `storage.data_dir`; the mount point itself must not be
the data directory because Halro creates an atomic-publication lock beside it.
The first command deliberately mounts `/run/secrets` writable so `init` can
create the file-mode Master Key. Every later `serve`, backup, and restore mounts
that secret directory read-only. If both the Master Key and initialized data
already exist, skip `init`; if only the key exists while the data directory is
empty, do not delete or replace it blindly—verify whether it belongs to a
previous instance, then either restore that instance's complete data set or
start with a separately named key and empty data directory.

The example publishes only the Gateway on host loopback and therefore uses
`-allow-insecure-public-listen`; that override applies only to Gateway
plaintext and is suitable for a host-local development boundary, not a public
network. Admin and Metrics remain container-loopback-only in this shape. For a
network-reachable deployment, enable TLS, bind the required listener to
`0.0.0.0` inside the container, mount its certificate/key (and Metrics client
CA), and publish the port only through the approved network control. Never use
the Gateway override to weaken Admin or Metrics validation.

The built-in healthcheck runs inside the container and calls a loopback
readiness URL. `healthy` therefore proves process readiness, not that a
published host port, certificate name, firewall, or reverse proxy is reachable.
When TLS is enabled set `HALRO_HEALTH_URL` to an HTTPS name covered by the
mounted certificate and resolvable inside the container, then separately probe
the external address from the host/load balancer.
