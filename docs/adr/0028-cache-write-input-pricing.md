# ADR 0028 — Price prompt cache writes by lifetime

Status: Accepted

[ADR 0022](0022-cache-read-input-pricing.md) introduced cache-read pricing. Cache
writes then settled at the ordinary input rate even when the provider billed a
premium. A Deployment Price Version now carries two additional rates. Each
time-of-day window and Price Proposal carries the same terms.

| Tier | Anthropic Messages | OpenAI Responses |
| --- | --- | --- |
| Short write | 5 minutes | 30 minutes |
| Long write | 1 hour | 12 hours |

Halro records total write tokens and the long-lived subset. It bills ordinary
input, cache reads, short writes, long writes, and output separately, rounding
each token component up to a micro-USD before adding the fixed request fee.
Ordinary input equals total input minus reads and all writes. Short writes equal
all writes minus long writes. When both write rates equal the ordinary input
rate, ordinary input and both write spans share one rounding step. The
settlement and its frozen price snapshot use the same calculation. If a
time-of-day zone is unavailable, the maximum rates are frozen with separate
rounding whenever grouping could understate a configured window's charge.

A provider's total write count is authoritative. If its lifetime counts disagree,
Halro clamps the long count to the total and logs a warning with token counts.
With only a total, Halro uses the short rate. When rendering a response, Halro
includes the lifetime breakdown only when the upstream reported it and its TTLs
match the response protocol. See the [Anthropic TTL breakdown](https://platform.claude.com/docs/en/build-with-claude/prompt-caching#1-hour-cache-duration)
and [OpenAI Responses TTL breakdown](https://developers.openai.com/api/docs/guides/prompt-caching#monitor-cache-performance).
The documented
[OpenAI Chat Completions usage](https://developers.openai.com/api/reference/resources/chat/subresources/completions/streaming-events)
and [Bedrock Mantle Chat usage](https://docs.aws.amazon.com/bedrock/latest/userguide/prompt-caching.html)
supply the write total. Bedrock Mantle's Messages and Responses adapters share the
corresponding lifetime decoders. AWS documents a [Converse TTL breakdown](https://docs.aws.amazon.com/bedrock/latest/APIReference/API_runtime_TokenUsage.html).
Halro's withheld Converse profile retains its existing total-only mapping.

The cost of a metered attempt is estimated when cache writes use an inherited
input rate or when the lifetime split is missing and could affect the charge.
Provider-reported tokens retain their usage source. The amount still follows
the pinned rate and the available token breakdown.

New prices record which write rates were supplied by the operator. An older
price without that evidence can have an inherited rate in any tier whose write
rate equals its ordinary input rate. For those rates, actual cache writes are
classified conservatively. A new price version can explicitly set an equal
rate. The same classification applies to snapshots that used maximum rates
because the schedule timezone could not be loaded. Existing settled events
retain their original amounts and cost classification.

Before the provider reports usage, budget and Token Guard bound the request using
the highest input-side rate. The bound allows for separate rounding of each
occupied tier. An estimated settlement uses the same highest rate and records
the assumed split so the Ledger can validate its cost against the snapshot.
If provider-reported usage overflows the monetary range, settlement uses the
prepared token bounds and records the charge as estimated. Synthetic resource
operation units, such as Files and Batches requests, use the ordinary input rate
for admission, settlement, and recovery. Embedding token estimates also use the
ordinary input rate. Anthropic token counting uses its fixed request fee for the
cost quote and counts prompt tokens toward token limits.

Metadata schema 43 initializes both write rates from each existing price's
ordinary input rate, including windows and Proposals. Proposal digests are
recomputed. Standalone startup authenticates and replays any unapplied
metadata journal frames before converting the prices, then publishes a new
journal epoch. Free prices remain zero. Old Ledger events and price snapshots
retain their amounts and their original reservation evidence. Recovery of
a pre-upgrade reservation uses its original ordinary-input estimate. Restore
verifies the archived pricing digest before migrating metadata and checks
historical Ledger snapshots against the migrated prices.

Usage partitions written before schema 9 read the new long-write counter as
zero. Export can resume from an unlisted older partition whose rows still
match the Ledger. Import requires a version 2 configuration bundle with both
write rates explicit for the base price and every window. Re-export older
bundles from an upgraded source.

New Admin price versions, proposals, and previews require explicit write rates.
An Admin price or proposal request from before the upgrade can replay if its
idempotency key and original request digest match the stored mutation.

Prompt-length price tiers and metered provider-executed tools remain follow-up work.
