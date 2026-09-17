# Administrator bootstrap secret compromise

Use this runbook when a setup token or first-administrator password appears in
a log, ticket, chat, shell history, crash artifact, tracing system, CI output or
another unintended location. Preserve evidence without copying the secret
again.

## Determine whether bootstrap completed

Stop automatic rollout or GitOps reconciliation while establishing state.
Check the trusted administrator count, bootstrap completion marker and audit
chain with offline diagnostics while the Halro data directory has one owner.
Do not infer success from a Job exit code, an HTTP timeout or the existence of
one metadata file.

## No administrator exists

The exposed setup token is valid only in the process that loaded it, until its
TTL expires. Removing or changing a Secret does not erase the copy held by an
old Pod.

1. Stop and delete every Halro Pod that could have loaded the token.
2. Remove the compromised Secret from GitOps desired state and revoke it in the
   external secret manager.
3. Generate a new absolute-expiry token envelope with
   `halro admin setup-token generate --ttl 30m --output` and
   create a new Secret from the file. Never pass the value as a literal.
4. Start one replacement Pod, complete setup, and verify the `admin.bootstrap`
   audit record.
5. Remove the bootstrap projection and secret using the two-phase sequence in
   `deploy/kubernetes/README.md`.

If the administrator password used by an automated Job was exposed before a
completion marker exists, stop the Job, rotate the password source, and retry
only after diagnosing whether its operation committed. Reuse the same
operation ID for recovery; never choose a new ID merely to bypass a conflict.

## Administrator already exists

The setup token is invalidated when the administrator transaction commits. Its
leak still requires deleting every copy and reviewing setup, ingress, WAF/APM,
Job, application and cloud audit records.

If the first password may be known, stop the runtime and run an approved
offline password reset with a new password file. That operation invalidates all
sessions but does not reset MFA. Validate that the old password and sessions no
longer work, inspect the administrator identity and MFA state, and rotate or
revoke any credentials the suspect session could have created.

## Closeout

- Remove secrets from log indexes, artifacts and tickets according to the
  owning system's deletion process; access logs for those systems remain
  evidence.
- Revoke temporary KMS lifecycle and workload identities and review cloud audit
  events for unexpected decrypts.
- Confirm replacement Pods start after bootstrap Secret deletion and an
  administrator can log in.
- Record the incident/approval identifier outside secret-bearing fields and
  retain the Halro audit verification result.
