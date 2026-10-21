# Cluster CA rotation

Use this runbook only for the private CA that authenticates Halro replication
members. It does not rotate the Gateway, Admin, or Metrics PKI. This in-place
procedure deliberately keeps every member's existing private key and SPKI:
authenticated membership stores one immutable pin per peer. Rotating member
keys requires a new incarnation and re-seed; editing only configuration is
rejected at startup.

## Preconditions

1. Confirm exactly one Primary, every Replica caught up, zero apply backlog,
   and no maintenance, seed, backup, promotion, or schema-boundary operation.
2. Export authenticated `cluster status`, current certificate subjects/SPKI
   digests, term, incarnation, and confirmed/applied indexes into the change
   record. Never record private keys or the Master Key challenge material.
3. Prepare a rollback bundle containing the old CA bundle and old member
   certificates. Keep it in the approved secret manager.
4. Reissue one certificate per `node_id` from the new CA using that member's
   existing public key. Verify key usage, lifetime, name-to-node mapping, and
   that every SPKI digest is unchanged. If policy requires fresh keys, stop:
   this repository has no authenticated membership-change operation for a new
   pin. Isolate all old members, establish a new incarnation offline, and
   re-seed every Replica as documented in `ha-operations.md`; a key/pin change
   is never an in-place or hot rotation.

## Dual-trust rollout

### Load dual trust before changing any certificate

1. Publish a CA bundle containing old and new roots to the Secret source while
   every member still presents its old certificate. The Kubernetes manifest
   mounts these files with `subPath`, so no running Pod receives an in-place
   Secret update.
2. Restart each Replica, one at a time, to load the dual bundle. After each
   restart require it to reconnect, authenticate, catch up, and become a
   promotion candidate.
3. Perform a planned stepdown to a restarted Replica and verify the new
   Primary's leadership frame is confirmed. Restart the former Primary as the
   final Replica. At this point every live member must prove that it loaded the
   dual bundle while all members still present old-CA certificates. Do not
   advance if even one member still trusts only the old root.

### Roll certificates with unchanged keys and pins

1. On one Replica, publish its new certificate with the unchanged private key
   and unchanged peer configuration. Restart it and require mutual connectivity
   in both dial directions. A pin mismatch means the certificate was issued for
   the wrong key: stop; never edit or remove the authenticated pin to make the
   rollout proceed.
2. Repeat for the other Replica. Keep at least one caught-up Replica available
   throughout.
3. Perform a planned stepdown to a member already presenting its new
   certificate. Rotate and restart the former Primary as the final Replica.
4. Require all members to report the same incarnation and term, exactly one
   Primary, zero backlog, unchanged authenticated pins, and peer connectivity
   using only new certificates.

## Remove old trust

1. Prove every configured peer pin still equals its authenticated membership
   pin and no live connection uses an old certificate.
2. Publish the new-only CA bundle to the Secret source. Because `subPath`
   retains the old projection until restart, restart each Replica one at a
   time, then perform a planned stepdown and restart the former Primary last.
3. From an isolated probe, prove an old-CA certificate fails trust validation
   before Master Key challenge-response even though its SPKI is unchanged.
   Archive secret-free results and the cluster Audit head.
4. Revoke and retire the old certificates under the PKI policy. The member
   private keys are deliberately unchanged and remain active; retain their
   normal protected copies plus the certificate/public-key evidence needed for
   audit. If destroying those keys is required, use the new-incarnation path.

## Failure and rollback

Stop immediately on role conflict, incarnation/term disagreement, changed
SPKI, failed challenge, unexpected readiness loss, or increasing apply lag.
While both CAs remain trusted, restore the affected member's previous
certificate and restart only that member. After old trust has been removed anywhere,
do not create an asymmetric rollback: re-add the reviewed dual bundle to every
member first, prove connectivity, then roll certificates back one at a time.
If the old CA or any member private key may be compromised, rollback and this
in-place procedure are not acceptable; externally fence the affected members,
create a new incarnation with fresh keys, and re-seed every member.
