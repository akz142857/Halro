# Recover a pricing clock quarantine

`wall_clock_forward_jump` and `wall_clock_rollback` stop price selection after the wall clock drifts from the monotonic clock beyond configured tolerances. The quarantine is durable; restarting does not clear it. Correct the host/VM time before recovery. Do not disable clock checks or change historical prices to get traffic through.

## Admin console

Open the affected deployment's pricing warning and choose **Review and restore pricing**. Review server UTC time, the previous pricing timestamp, the currently effective price version, its rates and any scheduled tiers. Confirm the review, then complete password/MFA verification. The server requires administrator role, same-origin/CSRF and step-up authentication.

The confirmation is valid for five minutes and is bound to the quarantine revision, reason and effective price ID. A changed price or quarantine, an expired review, another quarantine type or a missing/deleted deployment is refused. Refresh the review after a conflict. This action does not create, edit, cancel or delete a price.

`GET /admin/api/v1/deployments/{id}/prices/clock-confirm` reads the review. `POST` to the same path accepts `confirmation` from that review plus `current_password` and `totp_code` when enrolled. Existing restore-confirm behavior remains limited to restored-backup quarantines.

Recovery exclusively locks the deployment's pricing gate, atomically resets the durable high-water mark to the reviewed current price/time together with a `deployment_price.clock_confirm` audit intent, then clears the old in-memory clock anchor before releasing the gate. Historical pins, leases, prices and bills are unchanged. A subsequent rollback, metered-price forward anomaly or effective price-version change still quarantines the deployment. An unchanged immutable `free` version is the sole forward-gap exception described below. Audit delivery is retryable through the existing durable outbox.

## Offline operator recovery

This is a privileged Standalone maintenance path, equivalent in access requirements to offline key/route administration: stop Halro, take a consistent data backup, hold the exclusive data-directory lock, and possess the configured Master Key. It does not create an Admin session, reset MFA, start listeners, run background workers or call model providers. Cluster members use the authenticated Primary Admin action instead.

Inspect one deployment first:

```sh
halro pricing recover-clock --config /etc/halro/config.yaml --deployment DEPLOYMENT_ID
```

After reviewing the returned price and UTC time, apply only that snapshot:

```sh
halro pricing recover-clock --config /etc/halro/config.yaml --deployment DEPLOYMENT_ID \
  --apply --revision REVIEW_REVISION --reason wall_clock_forward_jump \
  --price-id REVIEW_PRICE_ID --reviewed-at REVIEW_UTC_TIME
```

The apply operation records actor type `local_cli`, uses the same guarded store operation, and commits authenticated audit delivery before reporting success. If delivery fails after commit, retain the error and restart the new binary to drain the pending intent; do not edit the database or retry with made-up revisions.

Restart and verify `/health/ready`, the deployment's quarantine status and routing eligibility. A remaining quarantine elsewhere can still keep instance readiness at 503. A paid model request is separate acceptance and is not required to prove the recovery transaction. Retain the pre-recovery backup for rollback: older binaries may not recognize the new audit action.

## Suspend and VM observation gaps

Go's monotonic clock can stop during sleep. A Mac-hosted Linux VM may also pause both `CLOCK_MONOTONIC` and `CLOCK_BOOTTIME`, so changing to BOOTTIME alone does not prove that a resume gap is a real wall-clock step. Wi-Fi changes do not themselves change prices; the first request after a host/VM pause can expose a stale observation anchor.

Pricing now captures the wall and monotonic readings from one clock sample, avoiding a pair captured on opposite sides of a pause. If a forward observation gap exceeds the existing configured tolerance, it may continue **only** when the currently selected immutable price has the exact previously observed ID and validates as `billing_mode=free` (all four components zero, no time-of-day schedule). No version transition is accepted by this exception, even a free-to-free transition. Metered prices, effective/scheduled version changes, rollbacks, malformed prices and existing quarantines keep their gates. The tolerance values are unchanged.

The accepted free-price gap is preserved as `clock_forward_gap_nanos` in that attempt's durable price pin and logged as `pricing clock observation gap accepted for unchanged free price`; it is evidence of an observation discrepancy, not an assertion that host time is correct. The normal successful pin rebases the paired anchor, so a later request does not repeatedly diagnose the same gap. Historical prices, bills and prior pins remain unchanged. Already persisted quarantines require an explicit reviewed recovery rather than being silently released.
