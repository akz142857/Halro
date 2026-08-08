# Experimental EWMA Token Guard

EWMA is an optional v1 detect-only layer above Token Guard fixed limits. It
detects relative changes in completed evaluation windows for RPM, TPM, average
estimated tokens per accepted request, and estimated cost rate. It is not a
rate limiter and cannot reject, throttle, disable, or temporarily block a Key.

Each enabled policy must configure:

- `alpha` in `(0, 1]` and a multiplier greater than `1`;
- at least 10 baseline samples;
- a warmup at least as long as the evaluation window;
- a 10-second-aligned evaluation window between 10 seconds and 5 minutes;
- a positive alert cooldown;
- one or more explicit absolute floors for RPM, TPM, average tokens/request, or
  cost micros/minute.

A relative alert requires both the absolute floor and `baseline × multiplier`
to be exceeded after warmup and minimum samples. Fixed thresholds are evaluated
first. Requests marked anomalous by fixed thresholds, EWMA-anomalous completed
windows, and windows inside the EWMA cooldown are excluded from training; a
single excluded sample taints and freezes the complete window. This prevents a
known anomaly from ratcheting its own baseline upward.

Only completed windows train or evaluate the baseline. Empty windows are
ignored. After more than one hour without trustworthy in-memory buckets, the
old suffix is skipped while the last persisted baseline remains available.
Policy revision changes discard the old baseline rather than applying it to new
semantics.

The versioned baseline checkpoint is stored in bbolt and included in normal
encrypted backups through the metadata snapshot. It contains aggregate rates,
sample counts, stable Project/Key IDs, and timestamps—never plaintext Gateway
Keys, Provider credentials, prompts, responses, source IPs, or detector match
values. Invalid or unsupported state is deleted during startup and Halro
continues with fixed Token Guard limits. Because EWMA is advisory, checkpoint
loss can reduce detection continuity but cannot weaken hard enforcement.

Admin events use `token_guard_ewma_detected`, a finite reason enum
(`ewma_rpm`, `ewma_tpm`, `ewma_tokens_per_request`, `ewma_cost_rate`), and the
existing deduplicated webhook/audit pipeline. The UI labels the feature
“detect-only” even when the policy's fixed-threshold action is
`temporary_block`.
