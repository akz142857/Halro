# Observability security RFC

Status: Accepted for implementation
Owner: Security
Implementers: Application, Platform

## Trust boundaries

Plain HTTP is permitted only over loopback in a single-tenant host/network
namespace. Loopback does not isolate other local processes. Shared hosts and
multi-tenant runners require a dedicated network namespace, dedicated host, or
an authenticated local proxy.

Cross-namespace and cross-host production scrape traffic requires TLS 1.2+
with mutual workload identity. Supported forms are direct metrics-listener
mTLS, an authenticated service mesh, or an authenticated loopback proxy.
Private networks and NetworkPolicy are defense in depth, not identity.

## Credentials

Production uses a versioned credential file containing only SHA-256 token
hashes and lifecycle metadata. A rotation atomically creates a new active token
and moves the previous active token to retiring. During the bounded overlap,
both authenticate. Revocation immediately removes authorization without
rotating the Master Key. Plain tokens are returned once and never persisted.

Rotation and revocation are serialized across processes with a dedicated OS
file lock, preventing concurrent administrators from reusing an epoch or
overwriting another rotation.

The credential file is `0600`, atomically replaced, excluded from Git and
diagnostic artifacts, and backed up as security state. Restores must not use a
snapshot older than the credential revocation watermark.

Every rotation and revocation appends a secret-free, SHA-256 hash-chained event
to `<credential_file>.audit`. `halro metrics verify-audit` detects deletion,
rewriting, truncation, reordering, or missing lifecycle events. Production must
forward events and periodically anchor the latest chain hash in an independent
immutable audit platform; the local chain is tamper-evident, not an independent
trust domain.

The legacy Master-Key-derived token is allowed only when no credential file is
configured and the Metrics listener is loopback. It is not production-admitted.

## Management and egress

- Core does not expose Prometheus or Alertmanager UI/API directly. Prometheus
  admin/lifecycle APIs are disabled unless a separately authenticated,
  least-privilege automation path requires them. Alertmanager silence,
  configuration and reload operations require authorization; every silence
  records owner, reason and expiry.
- Core service discovery, remote write and Alertmanager webhook destinations
  use explicit allowlists and restricted egress. Link-local, cloud metadata,
  unapproved loopback/Unix sockets, management networks, redirects and DNS
  changes are denied.
- Administrative audit events are forwarded to independent append-only or
  immutable storage. A monitoring administrator cannot delete that evidence.

## Independent dead-man identity

The external dead-man authenticates Prometheus and Alertmanager readiness
endpoints over TLS and validates server identity. Probe client identity, its
webhook credential and the external receiver's missing-heartbeat state are not
stored in Core. The Alertmanager `Watchdog` receiver uses a dedicated
credential distinct from operational alerts and from the direct probe.

The probe host/process, storage, network route, credential authority, receiver
and final Contact Point must survive loss or compromise of the Core host and
its only notification route. The receiver treats missing `Watchdog` and
missing probe heartbeat as stateful alarms and audits firing and recovery.

## Sign-off evidence

Core Security sign-off requires credential rotation/revocation, expired and
wrong certificate, management authorization, local unauthorized process, Core
SSRF, independent dead-man identity/failure-domain, restore and audit-tamper
tests.
