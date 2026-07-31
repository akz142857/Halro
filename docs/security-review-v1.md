# v1 Security Review

Date: 2026-07-31

Scope: Gateway/Admin HTTP surfaces, local persistence, Provider/Webhook egress, authentication, secrets, usage accounting, streaming redaction, dependency exposure, and release CI. This is a source-and-test review of the current single-node build; it is not a third-party penetration test.

## Outcome

No known reachable dependency vulnerability remains after remediation. The review found one high-value dependency issue and fixed it:

- `GO-2025-3770`: Host header injection/open redirect in `github.com/go-chi/chi/v5@v5.2.1`.
- Remediation: upgraded to `v5.2.2` and reran `govulncheck`; reachable findings are now zero.

The frontend advisory scan reports zero vulnerabilities. CI now reruns both Go and npm advisory scans on every push and pull request.

## Controls verified

| Boundary | Evidence | Result |
|---|---|---|
| Admin authentication | Argon2id password verification, signed server-side session, fixation rotation, absolute/idle expiry tests | Pass |
| Admin mutations | Same-origin session plus CSRF middleware and `If-Match` revision checks | Pass |
| Gateway keys | One-time plaintext response, SHA-256 hash-only persistence, revocation snapshot refresh | Pass |
| Provider/Webhook secrets | AES-GCM envelope with HKDF-derived keys and audience-bound AAD; API views never expose ciphertext | Pass |
| Master Key rotation | Persistent versioned keyring, offline COW per-record re-encryption, atomic key publication, authenticated crash bridge, bbolt compaction, Admin-session invalidation, stable protected Audit HMAC key, nine publication kill points | Pass |
| Bedrock Beta authentication | strict credential JSON, region-bound HTTPS audience, SigV4 headers, optional session token, no ambient credential/IMDS lookup, upstream bodies excluded from errors | Pass |
| Egress/SSRF | Shared SafeTransport, HTTPS default, DNS/IP validation, redirect revalidation, private/metadata rejection tests | Pass |
| Usage abuse | RPM/TPM/concurrency/budget reservations, Token Guard temporary block, race and 1,000-request budget tests | Pass |
| PII/secrets | Inbound/outbound rules, bounded cross-chunk stream redaction, reject semantics, persistence/API/heap profile canaries, value-blind panic recovery, and production browser artifact scan | Pass |
| Durability | Append-only accounting ledger, replay/checkpoint tests, audit hash chain, encrypted backup verification | Pass |
| Browser | CSP and security headers, `no-store`, no browser-persistent secret state, accessible modal/landmark baseline | Pass |
| Dependency exposure | `govulncheck v1.6.0`, `npm audit`, direct license inventory | Pass after remediation |

## Commands executed

```text
go test ./...
go test -race ./...
go vet ./...
go run golang.org/x/vuln/cmd/govulncheck@v1.6.0 ./...
npm audit --audit-level=moderate
go test ./internal/redaction -run '^$' -fuzz ...
```

The two redaction fuzz targets completed about 3.0 million executions without a crash or mismatch. Official Python, Node, and Go OpenAI SDK black-box contracts passed.

## Residual risks and release requirements

- Active probes can consume Provider control-plane quota; OpenAI-compatible uses a non-generating endpoint, Azure accepts a non-generating method check, and Bedrock sends a signed `HEAD` to Converse. Operators must validate behavior for future adapters.
- A single process intentionally has no multi-node failover. Crash recovery and 24-hour soak gates remain required before the final release.
- Experimental EWMA Token Guard state is advisory only: fixed limits run first,
  anomalous/cooldown windows cannot train the baseline, policy revisions discard
  stale state, corrupt checkpoints fall back to fixed limits, and EWMA decisions
  have no code path to rejection or temporary blocking.
- `govulncheck` also reports vulnerabilities in required modules that are not reachable through current call graphs. CI must remain mandatory because future code can make a previously unreachable symbol reachable.
- Filesystem confidentiality depends on host permissions and master-key custody. Backups must retain authenticated encryption and restore verification.
- Old encrypted backups remain bound to their recorded Master Key fingerprint; operators must not destroy an old key while retaining backups that require it.
- Future ambient AWS credential support would require a new IMDSv2/default-chain threat review; the Bedrock v1 Beta profile intentionally permits only encrypted explicit credentials.

## Release decision

Core security controls are suitable to proceed to RC hardening. Final v1 release remains blocked on the M10 recovery, soak, packaging, and release-signing gates.
