# AWS KMS production policy templates

These templates are intentionally account-neutral. Replace every `${...}`
placeholder, render the files, validate them in a non-production account, and
review the effective identity policy, Key Policy, SCP, permissions boundary,
VPC endpoint policy, and any explicit deny together.

- The normal runtime role receives only `Decrypt` on the exact Primary Key.
- The Recovery role receives only `Decrypt` on the exact Recovery Key and is
  not associated with the normal workload. Its association/assumption is a
  time-bounded, approved break-glass action and must be revoked afterward.
- The lifecycle role is offline and separately approved. It receives
  `Encrypt`/`Decrypt` only for initialization, verification, rewrap, and rotate.
- Key administrators are not application runtime roles. Keep Recovery Key
  administration in an independent policy/identity failure domain.
- Encryption Context values are non-secret SHA-256 bindings. Policies require
  the instance binding and the complete five-key context set; never put a
  credential or plaintext identifier in Encryption Context because CloudTrail
  records it.

For EKS Pod Identity, create the association outside Kubernetes; the Service
Account itself has no role annotation. Restrict node IMDS and confirm the Pod
cannot obtain the node role. AWS references:

- https://docs.aws.amazon.com/eks/latest/userguide/pod-id-association.html
- https://docs.aws.amazon.com/kms/latest/developerguide/least-privilege.html
- https://docs.aws.amazon.com/kms/latest/developerguide/encrypt_context.html
- https://docs.aws.amazon.com/kms/latest/developerguide/key-policies.html

Do not run `PutKeyPolicy`, create a Grant, or schedule Key deletion from
Heimdall. Those are external administrative operations with separate approval.
