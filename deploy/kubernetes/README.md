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
