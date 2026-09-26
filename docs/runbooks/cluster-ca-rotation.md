# Cluster CA rotation

Use this runbook only for the private CA that authenticates Halro replication
members. It does not rotate the Gateway, Admin, or Metrics PKI. Every member
also pins each peer's SPKI, so a CA overlap alone is insufficient: the reviewed
peer pins and certificates must advance together.

## Preconditions

1. Confirm exactly one Primary, every Replica caught up, zero apply backlog,
   and no maintenance, seed, backup, promotion, or schema-boundary operation.
2. Export authenticated `cluster status`, current certificate subjects/SPKI
   digests, term, incarnation, and confirmed/applied indexes into the change
   record. Never record private keys or the Master Key challenge material.
3. Prepare a rollback bundle containing the old CA bundle, old member
   certificates and old peer pins. Keep it in the approved secret manager.
4. Issue one unique certificate per `node_id` from the new CA. Verify key usage,
   lifetime, name-to-node mapping, and SPKI pins offline.

## Dual-trust rollout

1. Publish a CA bundle containing old and new roots to every member while each
   still presents its old certificate. Restart one Replica and require it to
   reconnect, authenticate, catch up, and become a promotion candidate.
2. On that Replica, publish its new certificate/key and update the matching
   peer pin in every other member's configuration. Restart it again and require
   mutual connectivity in both dial directions. A pin mismatch must fail
   closed; never remove the pin to make the rollout proceed.
3. Repeat step 2 for the other Replica. Keep at least one caught-up Replica
   available throughout.
4. Run `halro cluster stepdown --to <upgraded-replica>` and verify the new
   Primary's leadership frame is confirmed. Then rotate the former Primary as
   the final Replica.
5. Require all members to report the same incarnation and term, exactly one
   Primary, zero backlog, and peer connectivity using only new certificates.

## Remove old trust

1. Prove every configured peer pin is the new SPKI and no live connection uses
   an old certificate.
2. Remove the old CA from the trust bundle one Replica at a time, then perform a
   planned stepdown and remove it from the last member.
3. From an isolated probe, prove an old-CA certificate and each retired SPKI
   fail before Master Key challenge-response. Archive secret-free results and
   the cluster Audit head.
4. Revoke and destroy old private keys under the PKI policy. Retain only the
   certificate/public-key evidence needed for audit.

## Failure and rollback

Stop immediately on role conflict, incarnation/term disagreement, failed
challenge, unexpected readiness loss, or increasing apply lag. While both CAs
remain trusted, restore the affected member's previous certificate and peer
pins and restart only that member. After old trust has been removed anywhere,
do not create an asymmetric rollback: re-add the reviewed dual bundle to every
member first, prove connectivity, then roll certificates back one at a time.
If the old CA or any member private key may be compromised, rollback is not an
acceptable recovery; externally fence the affected members and execute a new
CA rotation with fresh keys.
