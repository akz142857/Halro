# Heimdall observability deployment

This directory contains a versioned single-host Monitoring MVP. It is not a
production topology by itself.

The example uses host networking so Prometheus can scrape Heimdall's existing
loopback-only Metrics listener. Run it only on a single-tenant Linux host. A
shared host requires a dedicated network namespace or authenticated proxy.

## Secret preparation

Create `/run/heimdall-observability` outside the repository, owned by the
service account, and write:

- `metrics-token` as mode `0400` or `0440`;
- `grafana-admin-password` as mode `0400` or `0440`.
- `alertmanager-webhook-url` as mode `0400` or `0440`, for operational alerts;
- `alertmanager-watchdog-url` as mode `0400` or `0440`, using an independent
  receiver/credential that alarms when Watchdog notifications stop.

Do not use environment variables or Compose interpolation for either value.
Generate the legacy development token with `heimdall metrics token`; production
must use the versioned credential commands documented in the operator guide.

## Validate

Run `./deploy/observability/validate.sh` before starting or updating the stack.
The script uses the pinned Prometheus image to validate configuration and rule
tests, validates JSON, and scans provisioned artifacts for obvious secret
fields.

The Compose profile binds Grafana to loopback and does not publish Prometheus
or Alertmanager ports. Grafana is available at `http://127.0.0.1:3000`.
Anonymous access, Viewer editing, Explore, external snapshots, Gravatar, and
runtime plugin installation are disabled in this baseline. Production SRE
Explore access requires a separately reviewed RBAC profile and egress policy.

## Production boundary

Production requires Phase B: versioned Metrics credentials and mutual workload
identity; both are implemented by Heimdall, but must still be integrated with
the target PKI and Secret store. Images in the example are pinned to the digests validated with this
commit; update tag, digest, validation evidence, and SBOM together. Ship
audit logs to independent immutable storage, configure an external Watchdog
receiver, and fill `docs/observability/capacity-model.md` with measured values.
Use `docs/observability/admission-checklist.md` as the production evidence index.

## Independent dead-man probe

`external-probe.example.yaml` is deployed on a different host/failure domain,
not as another service beside the primary Compose stack. Replace both HTTPS
example URLs with approved authenticated probe endpoints. Mount a curl config
at `/run/heimdall-deadman/webhook.curl` containing the independent receiver URL
and its dedicated credential; never reuse Alertmanager's receiver or webhook
identity. The probe sends state transitions and a heartbeat every interval, so
the receiver can alarm when the probe itself disappears.

The external host, storage, network path, receiver credential, and final
contact point must all be independent. The example is intentionally invalid
production evidence until that separation is demonstrated in the admission
checklist.
