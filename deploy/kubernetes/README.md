# Kubernetes first-install sequence

These workload fragments are production-oriented examples for
`storage.master_key.mode: key_slots`. They deliberately do not grant Halro
permission to read Kubernetes Secrets through the API: kubelet projects each
secret as a file. Replace image, namespace, PVC, KMS identity, origin, TLS and
network-policy placeholders for the target cluster.

Before applying them, create the namespace, a `halro-config` Secret whose
`config.yaml` key contains the complete configuration, and the `halro-data`
PVC. Provide TLS certificate/key mounts referenced by that configuration plus
a Service and reviewed Ingress or other private access path; `containerPort`
does not expose the Admin UI. Replace every image digest placeholder, and bind
each example ServiceAccount to the intended cloud identity using the cluster's supported
mechanism (for example an EKS Pod Identity association or reviewed IRSA
annotation). The YAML names identities but cannot create cloud roles, KMS keys,
trust policies or associations. Confirm the configured
`storage.master_key.startup_deadline` is below the runtime startup probe budget
(180 seconds in the example), with enough scheduling and network headroom.
The runtime manifest assumes Halro serves TLS; change all probe schemes together
only if TLS is intentionally terminated before the Pod. Its HTTP probes arrive
at the Pod IP, so `server.gateway_listen` must bind the Pod interface (for
example `0.0.0.0:8080`), not loopback.

The required order is a deployment invariant. A Kubernetes Deployment cannot
wait for a Job by itself:

1. Keep the `halro` Deployment absent (or scaled to zero) and wait for every
   old Pod and volume attachment to disappear.
2. Apply `halro-bootstrap-default-deny-network-policy.yaml` plus a reviewed,
   cluster-specific additive egress policy.
3. Choose exactly one path. Interactive setup runs the standalone Init Job,
   revokes that Job's lifecycle identity, then deploys `halro serve` for the
   browser flow. Automated setup runs only the Bootstrap Job (its init
   container initializes storage), runs the offline verification Job, revokes
   the lifecycle identity, then deploys `halro serve`.
4. Remove install-only Jobs,
   Secret objects and external-secret synchronizers from GitOps desired state.
   Remove CSI/injector mount dependencies before revoking their external secret;
   the native optional Secret mount may remain for an initialized instance.

Prefer a PVC with `ReadWriteOncePod`. `ReadWriteOnce` still allows two Pods on
one node, so neither access mode replaces the ordering above or Halro's data
directory lock. Mount the PVC at `/var/lib/halro` and configure
`storage.data_dir: /var/lib/halro/data`, leaving room for the sibling
publication lock and atomic directory operations.

## Interactive browser setup

Set these values in the `halro-config` Secret:

```yaml
admin:
  setup_token_file: /run/secrets/halro/setup-token
  setup_token_ttl: 30m
```

Generate a fixed-format token directly into a new private file, then ask the
secret manager or Kubernetes client to ingest that file. Do not use a shell
literal, command substitution, logs, a ticket, or Git:

```bash
halro admin setup-token generate --ttl 30m --output /secure/path/setup-token
kubectl -n halro create secret generic halro-bootstrap \
  --from-file=setup-token=/secure/path/setup-token
kubectl -n halro apply -f deploy/kubernetes/halro-init-job.yaml
kubectl -n halro wait --for=condition=complete job/halro-init --timeout=10m
kubectl -n halro apply -f deploy/kubernetes/halro-aws-kms.yaml
```

Deliver the envelope's first-line Token to the initialization approver through
the organization's secret-sharing channel; keep the absolute expiry with the
deployment record. The approver needs neither Pod logs nor `pods/exec`.
`halro-bootstrap-secret.example.yaml` documents the object shape with an
intentionally invalid placeholder and must not be used to store the real value.
After the first administrator is committed, remove the Secret from desired
state and delete it. The Deployment mounts it with `optional: true`, so an
already initialized replacement Pod can still be constructed; Halro itself
does not read the missing file once an administrator exists. With a CSI driver
or injector, first roll out a Deployment with the mount dependency removed,
then revoke the external secret.

## Automated administrator bootstrap

Keep the runtime Deployment stopped. Create `halro-admin-bootstrap` from a
private password file, replace the stable operation ID and image in
`halro-bootstrap-job.yaml`, associate `ServiceAccount/halro-bootstrap` with a
temporary KMS lifecycle identity, then run:

```bash
kubectl -n halro create secret generic halro-admin-bootstrap \
  --from-file=admin-password=/secure/path/admin-password
kubectl -n halro apply -f deploy/kubernetes/halro-bootstrap-job.yaml
kubectl -n halro wait --for=condition=complete \
  job/halro-bootstrap-install-id --timeout=15m
kubectl -n halro delete job halro-bootstrap-install-id --wait=true
kubectl -n halro apply -f deploy/kubernetes/halro-bootstrap-verify-job.yaml
kubectl -n halro wait --for=condition=complete \
  job/halro-bootstrap-verify-install-id --timeout=10m
```

The Job's init container performs `init --if-needed`; only its main container
receives the password file and performs `admin bootstrap --if-needed`. The
administrator, operation ID, audit intent and completion marker commit in one
metadata transaction. A retry with the same operation ID and username returns
`already_completed` without replacing the password. A different operation,
different username or an administrator without a matching completion fails
closed.

The verification Job runs `halro doctor` and then `halro audit verify` against
the same offline PVC. It becomes `Complete` only when the durable completion,
admin audit backlog, authenticated Audit chain and trusted checkpoint all
pass; `audit verify` also requires the Completion's Audit Event ID, operation
ID and target to match the chain. Archive its non-secret JSON in the deployment
record if policy permits, but a human does not need Pod log access to use the
Job condition as the gate. Also verify CloudTrail names the approved lifecycle
role rather than a node role. Then delete the verification Job and password
Secret, remove their GitOps declarations, revoke the lifecycle identity,
confirm the runtime identity has only its reviewed Primary decrypt permission,
and deploy the runtime. Never solve a
volume attach, data-lock, CSI, Secret or KMS readiness error by running a second
Job concurrently.

A Kubernetes Job template is immutable, and re-applying a failed Job does not
create a new attempt. After diagnosing the failure, wait for every failed Job
Pod to terminate, delete only that named Job, and re-apply the unchanged
manifest with the same operation ID and username:

```bash
kubectl -n halro delete job halro-bootstrap-install-id
kubectl -n halro apply -f deploy/kubernetes/halro-bootstrap-job.yaml
kubectl -n halro wait --for=condition=complete \
  job/halro-bootstrap-install-id --timeout=15m
```

Never change the operation ID to make a retry pass. A completed Job may be
deleted and recreated with that same identity to exercise the
`already_completed` path during acceptance.

## GitOps lifecycle

Initialization is install-only state, not an every-sync hook. Use an explicit
pipeline stage or a one-time application that is removed after success. Do not
combine `ttlSecondsAfterFinished` with a controller that permanently declares
the same Job, or deletion will make the controller run bootstrap again. Normal
upgrades reconcile only the runtime Deployment and never recreate bootstrap
Secrets, Jobs or the temporary KMS identity.

Apply Restricted Pod Security and an egress policy appropriate to the cluster's
DNS and KMS endpoints. Standard Kubernetes NetworkPolicy cannot name an AWS KMS
FQDN, and endpoint CIDRs are cluster-specific, so this repository does not ship
a misleading allow-all-HTTPS rule. Use a private KMS endpoint with reviewed
CIDRs or a CNI FQDN policy, plus the cluster's exact DNS selector and the
selected workload-identity path (for example the EKS Pod Identity Agent
link-local endpoint, or regional STS for IRSA), and default deny every other
egress destination including EC2 IMDS. All example containers set
`AWS_EC2_METADATA_DISABLED=true` so a broken association cannot fall back to a
node role. Neither example ServiceAccount needs `secrets/get`,
`pods/log` or `pods/exec`. Treat the ability to create arbitrary Pods, patch
workloads, add ephemeral containers, snapshot the PVC or impersonate the
external secret/KMS identity as equivalent secret access.

## HA StatefulSet

`halro-ha-statefulset.yaml` is the separate three-member HA example. It does
not replace the single-PVC Deployment above. Before applying it, create:

- `halro-ha-config`, with `halro-0.yaml`, `halro-1.yaml`, and
  `halro-2.yaml`; each file must carry that member's unique `node_id`, its two
  peers and reviewed SPKI pins;
- `halro-ha-cluster-tls`, with `ca.crt` and one `<pod>.crt` / `<pod>.key`
  pair per ordinal; and
- the Master Key and application TLS mounts named by those configurations.

The headless `halro-members` Service publishes unready addresses so startup
adjudication does not depend on readiness. It also names each Pod's Metrics
port: for example, `halro-0.halro-members.halro.svc.cluster.local:9090` reaches
one member, including while it is NotReady. Configure each member's
`server.metrics_listen` on the Pod interface, with `metrics.enabled`,
`metrics.require_auth`, `metrics.tls.enabled`, and `metrics.ha_status.enabled`;
mount the separate Metrics and HA-status bearer files and Metrics TLS files
named by that member's configuration. The Metrics server certificate must
cover the exact DNS name used by both Prometheus and `halro-ha-health`.
These files and the monitoring workloads are target-specific and are not
created by this StatefulSet example.

When seeding each member PVC, copy the **complete versioned credential
source** for both Metrics and HA status: `credential_file`, its `.audit`
sidecar, and its `.revocations` sidecar when present. Keep all files private
to the Halro UID. The one-time bearer token belongs only in the corresponding
monitoring Secret; it is not a replacement for the member's credential source.
Compare the staged PVC files with the authoritative stopped source before
starting the StatefulSet, then run `halro metrics verify-audit` and
`halro ha-status verify-audit` for every member before calling rotation ready.
The 2026-09-28 local exercise initially copied only the JSON sources; the
running authorizer could read them, but `ha-status rotate` correctly refused
because the audit sidecar was missing. The stopped-source audit files were
restored byte for byte before testing rotation. Do not synthesize a new audit
chain for an already-established credential.

The optional `halro-ha-observability-ingress.example.yaml` adds member
TCP/9090 ingress for Pods labelled `app.kubernetes.io/name=prometheus` or
`app.kubernetes.io/name=halro-ha-health`. It also allows only the health-view
Pod to use TCP/8080 for the client Service root probe and optional direct
member live/ready probes; Prometheus does not need Gateway access. This is a
port rule, not an HTTP path filter: do not mount Gateway write credentials in
the health-view Pod, and use a reviewed layer-7 route if path isolation is
required. Every
source must be in a namespace labelled
`halro.io/monitoring-access=allowed` and requires **both** its Pod label and
the namespace label. NetworkPolicies are additive: audit every other policy
selecting HA Pods for broader 9090 or 8080 allowances. Review who can create
or relabel namespaces and Pods before applying it; labels are routing
selectors, while mTLS and distinct bearer credentials authenticate Metrics
and HA-status requests. Keep the
Prometheus `/metrics` credential separate from the health service's
`/ha/status` and `/ha/transitions` credential. Confirm the monitoring Pods'
egress policy, DNS, certificate rotation, and Secret mounts independently.
An external monitoring deployment needs its own reviewed route and policy.
Verify per-member scrapes, status collection and both Gateway probes before
treating the health page as an operational source; merely applying this policy
proves no reachability.

For the dedicated local kind acceptance cluster,
`halro-ha-health-kind.example.yaml` supplies one control-plane and **three**
workers: the HA StatefulSet requires three schedulable nodes because of hard
Pod anti-affinity, and the kind control-plane is tainted. The matching
`halro-ha-health-monitoring.kind.example.yaml` runs Prometheus and the mTLS
health view in **separate Pods and RWOP PVCs**. Prometheus exposes an internal
HTTPS Service with required client certificate authentication; a NetworkPolicy
admits only the health-view Pod to TCP/9090. Provision its scrape ConfigMap,
`ha-prometheus-web-config`, `ha-prometheus-web-tls`, and the separate
`ha-health-prometheus-client` Secret from private target-specific material.
The web config should require `RequireAndVerifyClientCert` and restrict client
SANs; the Prometheus server certificate must cover the Service DNS name.
The sample YAML contains no bearer tokens, private keys or member
configuration. It leaves Prometheus admin, lifecycle and remote-write receiver
APIs disabled; native mTLS still exposes Prometheus's internal read API to the
authorized health identity. Add a reviewed path proxy if that read scope is
too broad for the target environment.

The non-root synchronizer reads projected group-readable HA-status token
files and atomically publishes owner-only `0600` copies in a memory-backed
volume. Only those copies are mounted in the health view; the view keeps its
fail-closed token permission check. A separate init container copies the
Prometheus client private key to a `0600` file before the view starts. Test
both invalidation and restoration after kubelet updates the HA-status Secret.
A Secret update is not instant: allow for kubelet projection and the
synchronizer interval, and do not report status-token rotation complete until
`/ha/status` accepts the new version and rejects the revoked one. A rotated
Prometheus client certificate/key requires restarting the view, since it
loads that pair at startup. This kind fixture uses local image tags and a
fixed node name; both Pods currently share the control-plane node, so node
failure is not isolated. Review scheduling, labels, Secret mounts, TLS
lifetimes and the actual CNI policy before using a related pattern elsewhere.

For a Prometheus client-certificate rotation, first add the new client SAN to
the Prometheus web config while retaining the old SAN, reload it with a
Prometheus rollout, and verify both client certificates through the authenticated
HTTPS Service. Replace `ha-health-prometheus-client`, restart the health view,
and verify its Prometheus-backed API. Finally remove the old SAN, roll
Prometheus again, and verify that the old certificate fails its TLS handshake
while the new certificate and health view still work. The local kind exercise
uses one short-lived CA; rotating that CA or the Prometheus server certificate
needs an additional trust-overlap plan and target-environment validation.

The client Service deliberately uses routing option (a): it selects every
Ready member, replicas answer 503 `not_primary` with `Retry-After`, and the
client retries. Halro has no
`pods/patch` permission and never mutates a role label. Label only reviewed
client or ingress-controller namespaces with `halro.io/client-access=allowed`;
add a separately reviewed narrow NetworkPolicy if remote Admin access is needed.

The startup probe allows one hour for a large seed/catch-up. Size that budget
and the 50 GiB `ReadWriteOncePod` claim from measured seed time and retained
uncompressed backlog. `OnDelete` means upgrades are one explicitly chosen Pod
at a time. Confirm the member is not the active Primary, or complete
`halro cluster stepdown`, before deleting it. Never delete two Pods together;
the PDB protects voluntary disruptions but not operator deletion or node loss.

For an offline Replica backup, enable its maintenance sentinel, wait for
readiness to fail while liveness remains healthy, run `backup create --replica`
and `backup verify` in that Pod, report the backup on the Primary, then disable
maintenance and wait for catch-up. A retained PVC removed by scale-down must be
destroyed or explicitly isolated from every future cluster incarnation.
