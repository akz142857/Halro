# Halro observability deployment

This directory contains a versioned single-host Monitoring MVP. It is not a
production topology by itself.

The example uses host networking so Prometheus can scrape Halro's existing
loopback-only Metrics listener. Run it only on a single-tenant Linux host. A
shared host requires a dedicated network namespace or authenticated proxy.

## Secret preparation

Create `/run/halro-observability` outside the repository, owned by the
service account, and write:

- `metrics-token` as mode `0400` or `0440`;
- `alertmanager-webhook-url` as mode `0400` or `0440`, for operational alerts;
- `alertmanager-watchdog-url` as mode `0400` or `0440`, using an independent
  receiver/credential that alarms when Watchdog notifications stop.

Do not use environment variables or Compose interpolation for either value.
Generate the legacy development token with `halro metrics token`; production
must use the versioned credential commands documented in the operator guide.

### macOS local development

macOS keeps `/run` on a read-only system volume. For local OrbStack or Docker
Desktop use, create `/private/tmp/halro-observability` with mode `0755`, keep
the Secret files at mode `0444`, and add `compose.macos.example.yaml` to every
Compose command. The override changes only the host bind-mount source; the
containers still read `/run/secrets`. This temporary path is not a production
Secret Store.

If no real Contact Point exists yet, syntactically valid loopback webhook URLs
may be used to start the stack, but delivery will fail until a receiver is
running. Never leave literal `<...>` placeholders in the files.

## Validate

Run `./deploy/observability/validate.sh` before starting or updating the stack.
The script uses the pinned Prometheus image to validate configuration and rule
tests, validates JSON, and scans provisioned artifacts for obvious secret
fields.

## Start the Core profile

The default profile contains only Prometheus and Alertmanager:

```sh
docker compose -f deploy/observability/compose.example.yaml up -d
```

On macOS local development:

```sh
docker compose \
  -f deploy/observability/compose.example.yaml \
  -f deploy/observability/compose.macos.example.yaml \
  up -d
```

Prometheus and Alertmanager listen only on loopback and no management port is
published. Prometheus rule files are the only alert authority.

## Production boundary

Production requires Phase B: versioned Metrics credentials and mutual workload
identity; both are implemented by Halro, but must still be integrated with
the target PKI and Secret store. Images in the example are pinned to the digests validated with this
commit; update tag, digest, validation evidence, and SBOM together. Ship
audit logs to independent immutable storage, configure an external Watchdog
receiver, and fill `docs/observability/capacity-model.md` with measured values.
Use `docs/observability/admission-checklist.md` as the production evidence index.

## Independent dead-man

The repository ships a formal, separately deployable `halro-deadman`
binary. It directly checks authenticated HTTPS endpoints for Halro,
Prometheus and Alertmanager, verifies Prometheus sample freshness, persists a
hysteretic state machine and bounded notification outbox, and sends versioned
heartbeat and down/up events to an independent receiver.

The v1 binary is intentionally the official Halro Core monitor, not a
general-purpose probe. Its configuration therefore requires at least one
`halro`, one `prometheus`, and one `alertmanager` target. Omitting any of
those kinds is a configuration error rather than a way to silently reduce
coverage.

Build and validate it with:

```sh
make deadman
./bin/halro-deadman \
  -config deploy/observability/external-probe/config.example.yaml \
  -check-config
```

Deployment assets are under `external-probe/`:

- `config.example.yaml` and `config.schema.json` define the v1 structure;
  `halro-deadman -check-config` remains authoritative for cross-field and
  duration semantics, and shared tests keep both validation paths aligned;
- `event.schema.json` defines the receiver payload;
- `RECEIVER-CONTRACT.md` defines durable acknowledgement, idempotency,
  heartbeat expiry, firing and resolved behavior;
- `halro-deadman.service` is the hardened systemd template;
- `Dockerfile` and `external-probe.example.yaml` provide a separate container
  deployment.

Install the probe on a different host or failure domain, never beside the Core
stack as production evidence. Replace every example endpoint and mount the
dedicated CA, mTLS keys and/or bearer-token files named by the config. The
notification identity and final Contact Point must not be reused from
Alertmanager.

Target TLS clients, CA pools and client certificates are loaded once at probe
startup so their connection pools can be reused. Bearer tokens are still read
from their protected files for every request and can rotate without restarting
the probe. CA or client-certificate rotation requires a controlled restart;
verify target recovery and receiver heartbeat delivery after that restart.

The Alertmanager `Watchdog` route is a second signal and repeats every minute.
Its independent receiver should use an approved grace window (the local
baseline is three minutes) and must emit both missing-heartbeat firing and
recovery notifications. The Go probe also sends a heartbeat with an explicit
TTL so its own disappearance is detected.

Repository tests prove state transitions, persistence, retry, TLS/authentication
and payload contracts. They do not prove failure-domain independence. The
external host, state, credentials, network path, receiver and final Contact
Point must be demonstrated independently in the Phase D admission checklist.
