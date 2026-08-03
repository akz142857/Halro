# Production admission evidence

Status: Target-environment execution required
Owner: SRE
Sign-off: Application, Security, SRE, Platform

This checklist is the Phase D evidence index. Repository validation does not
make any row pass. Store evidence in the approved immutable audit system and
link its identifier here; never commit tokens, certificates, customer data, or
screenshots containing sensitive labels.

| Gate | Required evidence | Owner | Result | Evidence ID |
|---|---|---|---|---|
| Fresh install and provisioning | startup log, provisioned UIDs, raw/query comparison | Platform | BLOCKED: target environment | |
| Metrics mTLS | valid client succeeds; absent, wrong and expired clients fail | Security | BLOCKED: target PKI | |
| Credential lifecycle | overlap, reload, revoke, old-token 401, restore non-revival | Security | BLOCKED: target secret store | |
| Grafana and Prometheus RBAC | anonymous, Viewer Explore/export, datasource proxy and admin API denials | Security | BLOCKED: target SSO/RBAC | |
| Alert lifecycle | firing, notification delivery and resolved notification | SRE | BLOCKED: contact point | |
| Independent dead-man | stop Prometheus and Alertmanager separately; external path fires | SRE | BLOCKED: independent monitor | |
| TSDB failure | read-only/full storage causes independent alarm and recovery | SRE | BLOCKED: disposable target | |
| SSRF/egress | metadata, link-local, loopback, redirect and DNS-change denials | Security | BLOCKED: target egress policy | |
| Audit integrity | local deletion/tamper does not remove independent evidence | Security | BLOCKED: immutable audit platform | |
| Backup/restore | encrypted restore meets RPO/RTO and does not revive revoked identities | Platform | BLOCKED: backup platform | |
| Capacity and soak | 24h scrape/query/series/storage measurements below admitted budget | SRE | BLOCKED: production-sized soak | |
| Upgrade and rollback | dashboard/rule/image upgrade and rollback preserve service | Platform | BLOCKED: target environment | |

No-Go is the default while any row is `BLOCKED` or lacks an evidence ID and
all four sign-offs. A waiver cannot replace the Phase B credential or mutual
identity gates.
