# Configuration snapshot stale

Owner: SRE  
Alert: `HalroConfigurationStale`  
Impact: the instance is not ready and intentionally returns
`503 configuration_stale` for every data-plane request.

## Why Halro refuses traffic

An Admin mutation is durable once it commits to the metadata store. Its live
topology, authentication, redaction, or Token Guard snapshot is activated
afterward. If that activation fails, continuing with the older snapshot could
authorize a revoked key, route removed traffic, expose unredacted content, or
bypass a newer admission policy. Halro therefore fails closed until every
affected domain catches up.

## Diagnose

1. Confirm `/health/live` remains `200` and `/health/ready` returns `503` with
   an `activation` object. Do not remove the instance's existing readiness
   routing protection.
2. Read `/admin/api/v1/system/status`. Under `activation.domains`, record each
   entry whose `stale` field is true, its `stale_since`, and its bounded reason.
3. Check `halro_activation_stale_seconds` and the runtime logs for the first
   activation error. Investigate metadata-store I/O, Vault/Master Key access,
   and policy validation according to the named domain. Do not print secrets or
   Provider credentials while collecting evidence.
4. The runtime retries all four snapshot domains every five seconds. After the
   underlying failure clears, require `halro_activation_stale == 0`, readiness
   `200`, and all four status domains to report current before returning the
   instance to service.

## Recovery and restart safety

- Do not bypass the stale gate or manually edit the database. That converts an
  explicit outage into a silent authorization or policy failure.
- A restart is safe only after confirming the durable metadata store and Master
  Key/Vault are readable. Startup rebuilds every live snapshot from that store
  and fails if it cannot do so.
- If the reason remains unchanged through three recovery intervals, remove the
  instance from traffic, preserve logs and the status response, correct the
  named storage/key/policy failure, and perform a controlled restart. Require
  readiness and both activation metrics to recover before rejoining traffic.
- If multiple instances share an upstream load balancer, compare their status;
  do not treat another node's healthy snapshot as proof that this node caught
  up.
