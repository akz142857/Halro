# Production admission evidence

Status: Target-environment execution required
Owner: SRE
Sign-off: Application, Security, SRE, Platform

This file is the evidence index for the Core admission decision.
Repository validation does not make a target-environment row pass. Store
evidence in the approved immutable audit system and link only its identifier
here; never commit tokens, certificates, customer data, or screenshots with
sensitive labels.

## Prometheus/Alertmanager Core (Phase D)

| Gate | Required evidence | Owner | Result | Evidence ID |
|---|---|---|---|---|
| Fresh Core provisioning | scrape, rule evaluation and Alertmanager routing healthy | Platform | BLOCKED: target environment | |
| Metrics mTLS | valid client succeeds; absent, wrong and expired clients fail; certificate rotation and rollback succeed | Security | BLOCKED: target PKI | |
| Credential lifecycle | overlap, atomic reload, revoke, old-token 401 and restore non-revival | Security | BLOCKED: target Secret Store | |
| Core management RBAC | Prometheus query/reload/admin and Alertmanager UI/API/silence/config/reload deny anonymous and unauthorized identities; authorized changes are least-privilege and audited | Security | BLOCKED: target identity proxy/RBAC | |
| Alert lifecycle | representative alert reaches a real Contact Point as firing and resolved; payload contains no sensitive canary | SRE | BLOCKED: Contact Point | |
| Independent dead-man | external receiver detects missing `Watchdog`; authenticated synthetic probe directly detects Prometheus and Alertmanager loss; probe heartbeat loss is also detected; stop/recover each component separately | SRE | BLOCKED: independent monitor | |
| Dead-man independence | probe, receiver, state, credentials, network path and final Contact Point do not share the Core failure domain or its only notification path | Security + SRE | BLOCKED: independent environment | |
| TSDB failure | read-only/full storage causes an independent alarm and recovery; evidence does not depend only on the failing Prometheus | SRE | BLOCKED: disposable target | |
| Core SSRF/egress | service discovery, remote write and Alertmanager webhook paths deny metadata, link-local, loopback, redirects, DNS changes and unapproved targets | Security | BLOCKED: target egress policy | |
| Audit integrity | local deletion/tamper cannot remove independent evidence and produces an integrity signal | Security | BLOCKED: immutable audit platform | |
| Core backup/restore | encrypted restore meets RPO/RTO and does not revive revoked identities; rules, config and Alertmanager state remain consistent | Platform | BLOCKED: backup platform | |
| Capacity and soak | 24h scrape/rule/query/series/storage measurements stay below admitted Core budgets | SRE | BLOCKED: production-sized soak | |
| Core upgrade and rollback | Prometheus, Alertmanager, rules and config upgrade/rollback preserve scrape, alert and dead-man service | Platform | BLOCKED: target environment | |
| Core sign-off | Application, Security, SRE and Platform approve the evidence above | All | BLOCKED: prerequisite gates | |

Core is No-Go while any required row is `BLOCKED`, lacks an evidence ID, or
lacks four-party sign-off. A waiver cannot replace the credential, mutual
identity, alert delivery, or independent dead-man gates.
