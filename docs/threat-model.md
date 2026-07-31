# Heimdall v1 threat model

## Assets

- master key;
- provider credentials;
- Gateway keys;
- Admin password and sessions;
- TLS private key;
- webhook/backup secrets;
- usage, cost, source-IP derivatives, and audit data;
- prompts and responses in transient memory.

## Trust boundaries

```text
Internal client
  → Gateway listener
  → policy/redaction/accounting
  → SafeTransport
  → external provider

Admin browser
  → TLS/Admin listener
  → Admin session + CSRF
  → metadata/vault

Process
  → bbolt + Ledger WAL + derived Parquet
```

The host root account is trusted for v1. Audit chaining detects offline record mutation but is not non-repudiation against a root attacker.

## Primary threats and controls

| Threat | Primary controls |
|---|---|
| Provider credential theft | AEAD + audience binding, no reveal, safe logs |
| Gateway key theft | one-time display, SHA-256 storage, TLS, no browser persistence |
| SSRF/credential exfiltration | SafeTransport resolve/validate/pinned dial, no env proxy, host allowlist |
| Public plaintext Admin | loopback default; never allowed by insecure Gateway override |
| Budget overspend | per-attempt durable reservation and atomic settlement |
| Crash undercount | one Ledger WAL and conservative orphan reconciliation |
| Sensitive prompt/output leakage | inbound, outbound, and mandatory telemetry redaction modes |
| Streaming boundary bypass | strict or compiler-bounded enforcement; detect-only is not a prevention claim |
| Disk failure | accounting state machine, readiness false, stop new provider calls |
| Backup disclosure | authenticated encrypted backup by default |
| Browser secret recovery | no-store, strict CSP, no service worker, no local storage |

## Default assumptions

- One active writer owns the data directory.
- Public access uses TLS.
- Admin and Metrics are loopback or protected behind a precisely trusted TLS proxy.
- Prompt/response bodies are not persisted by default.
- Unknown provider price is denied for budget-protected projects.

## Required security tests

- default listener and insecure-override matrix;
- DNS rebinding, mixed A/AAAA, redirect, proxy, mapped-IP and metadata tests;
- credential audience tampering;
- stream redaction byte-boundary and Unicode fuzzing;
- secret canary scanning across logs, errors, heap diagnostics, WAL, bbolt, Parquet, and browser artifacts;
- WAL corruption, disk-full, backup tampering, and restore path tests.
