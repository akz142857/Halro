# Heimdall

[![CI](https://github.com/akz142857/Heimdall/actions/workflows/ci.yml/badge.svg)](https://github.com/akz142857/Heimdall/actions/workflows/ci.yml)
[![Go version](https://img.shields.io/github/go-mod/go-version/akz142857/Heimdall)](go.mod)
[![License](https://img.shields.io/badge/license-Apache--2.0-blue.svg)](LICENSE)

Heimdall is a single-binary, security-first LLM gateway for unified model access, provider credential custody, internal key distribution, usage control, anomaly protection, and redaction.

Start with the [Chinese user guide](docs/user-guide.zh-CN.md). For production
installation, upgrades, recovery, and hardening, see the
[Operator Guide](docs/operator-guide.md).

## Highlights

- OpenAI-compatible chat, streaming, and embeddings APIs;
- encrypted Provider credential vault and hash-only internal Gateway keys;
- Project budgets, RPM, TPM, concurrency, CIDR, and model authorization;
- bounded retry, fallback, circuit breaking, and capability-aware routing;
- Token Guard anomaly containment and streaming-aware redaction;
- durable local accounting, Parquet analytics, audit integrity, and Prometheus metrics;
- embedded React Admin console in one Go binary, with no external database or cache.

## Documentation

- [中文使用手册](docs/user-guide.zh-CN.md)
- [Operator Guide](docs/operator-guide.md)
- [OpenAI compatibility contract](docs/contracts/openai-compatibility.md)
- [Security model](docs/threat-model.md)
- [Distributed evolution and state ownership](docs/distributed-state-ownership.md)
- [Gateway idempotency contract](docs/idempotency-contract.md)
- [Backup and restore](docs/backup-restore.md)
- [Contributing](CONTRIBUTING.md)
- [Security policy](SECURITY.md)

## Development

Requirements:

- Go 1.26.5 or later

Validate the current foundation:

```bash
go test ./...
go test -race ./...
go vet ./...
```

The Admin console is a React build-time dependency and is embedded into the Go
binary. It does not require Node.js at runtime:

```bash
cd web
npm ci --ignore-scripts
npm test
npm run build
cd ..
go build -trimpath -o bin/heimdall ./cmd/heimdall
```

After starting Heimdall, open the Admin listener at `/admin`. Bootstrap the
first local administrator with `heimdall admin bootstrap` before signing in.

Initialize and bootstrap a local instance:

```bash
go run ./cmd/heimdall init --config ./configs/config.example.yaml
printf '%s' "$ADMIN_PASSWORD" | go run ./cmd/heimdall admin bootstrap \
  --config ./configs/config.example.yaml --username admin
printf '%s' "$OPENAI_API_KEY" | go run ./cmd/heimdall bootstrap \
  --config ./configs/config.example.yaml \
  --provider-model gpt-5-mini \
  --public-model chat
go run ./cmd/heimdall serve --config ./configs/config.example.yaml
```

`bootstrap` reads the Provider key from standard input, encrypts it with the
local master key, and prints a `gw_...` Gateway key exactly once. It atomically
creates the first Provider, Deployment, Route, Project, and Gateway key. Run this offline
command while the server is stopped because Heimdall exclusively owns its data
directory.

Use the one-time Gateway key as a Bearer token with
`POST /v1/chat/completions` or `POST /v1/embeddings`. Clients use the public
model alias (`chat` above), never the Provider model or Provider key.

## Provider profiles

- OpenAI: Bearer authentication and standard `/v1` chat, stream, and embedding endpoints.
- Azure OpenAI: `api-key` authentication and deployment-scoped data-plane paths. Set `--provider-api-version` during bootstrap, or `api_version` through the Admin API; Heimdall intentionally does not silently select or upgrade it.
- DeepSeek: OpenAI-compatible chat/stream profile with embeddings disabled by default.
- Generic OpenAI-compatible: chat, stream, and embeddings by default; optional capabilities can be declared through the Admin API.
- Gemini (Beta): native `generateContent`, SSE, and float embedding translation for the text-only subset; API keys use the `x-goog-api-key` header.
- AWS Bedrock (Beta): native Converse and ConverseStream translation for text chat, AWS EventStream checksum validation, usage normalization, and SigV4 authentication. Embeddings, tools, vision, JSON mode, and the AWS default credential chain are not declared in this Beta profile.

Provider capabilities are enforced before an upstream call. An Admin connection
test uses `/models` where available. Azure tests the configured deployment path
without generating tokens. Test outcomes are audited, while upstream bodies,
credentials, and authorization headers are never returned.

Real Provider smoke tests are disabled unless explicitly enabled. They perform
small billable chat calls, so run them only in a controlled account:

```bash
HEIMDALL_REAL_PROVIDER_SMOKE=1 \
HEIMDALL_SMOKE_PROFILE=openai \
HEIMDALL_SMOKE_BASE_URL=https://api.openai.com \
HEIMDALL_SMOKE_API_KEY="$OPENAI_API_KEY" \
HEIMDALL_SMOKE_MODEL=gpt-5-mini \
go test ./internal/provider/openai -run TestRealProviderSmoke -count=1
```

For Azure, also set `HEIMDALL_SMOKE_API_VERSION`; set
`HEIMDALL_SMOKE_EMBEDDING_MODEL` to opt into the embedding contract.

For day-to-day use, SDK examples, and the Admin resource workflow, see the
[Chinese user guide](docs/user-guide.zh-CN.md). For installation,
configuration, provider setup, alerts, upgrades, and troubleshooting, see
[the Operator Guide](docs/operator-guide.md).

Bedrock credentials are stored as one encrypted JSON secret. The region must
match the runtime endpoint hostname:

```json
{"access_key_id":"...","secret_access_key":"...","session_token":"...","region":"us-east-1"}
```

`session_token` is optional. The v1 Beta profile intentionally uses explicit
static credentials only; it does not contact IMDS or read an ambient AWS
credential chain. An opt-in billable smoke test is available in
`internal/provider/bedrock/real_smoke_test.go`.

Additional internal keys can be issued and disabled offline:

```bash
go run ./cmd/heimdall key create --config ./configs/config.example.yaml \
  --project-id prj_... --name team-a
go run ./cmd/heimdall key disable --config ./configs/config.example.yaml \
  --key-id key_...
```

Rotate the Master Key only while Heimdall is stopped. If interrupted, rerun
the same command with the same replacement key; the operation is recoverable
and idempotent:

```bash
umask 077
openssl rand 32 > /secure/path/new-master.key
go run ./cmd/heimdall key rotate --config ./configs/config.example.yaml \
  --new-key-file /secure/path/new-master.key
```

See the Operator Guide before retiring the old key because older backups retain
their original Master Key fingerprint.

Encrypted offline backups:

```bash
umask 077
openssl rand 32 > backup.key
go run ./cmd/heimdall backup create --config ./configs/config.example.yaml \
  --output /secure-backups/heimdall.hmbk --key-file ./backup.key
go run ./cmd/heimdall backup verify --file /secure-backups/heimdall.hmbk \
  --key-file ./backup.key
# Copy the bkp_... ID from verify, stop Heimdall, then restore:
go run ./cmd/heimdall backup restore --config ./configs/config.example.yaml \
  --file /secure-backups/heimdall.hmbk --key-file ./backup.key \
  --confirm-backup-id bkp_...
```

The Master Key is deliberately excluded. See
[backup and restore](./docs/backup-restore.md) for the consistency and key
custody requirements.

The example configuration binds all listeners to loopback. Public listeners require TLS; the Admin and Metrics listeners can never be exposed over plaintext.

## Community

Bug reports and feature proposals are welcome through GitHub Issues. Please
read [CONTRIBUTING.md](CONTRIBUTING.md), [SUPPORT.md](SUPPORT.md), and our
[Code of Conduct](CODE_OF_CONDUCT.md) before participating. Security
vulnerabilities must follow [SECURITY.md](SECURITY.md) and must not be posted
in a public issue.

## License

Heimdall is licensed under the [Apache License 2.0](LICENSE). Third-party
attributions are documented in [NOTICE](NOTICE) and
[THIRD_PARTY_NOTICES.md](THIRD_PARTY_NOTICES.md).
