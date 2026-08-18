# ADR 0023: Pricing by time of day

- Status: Accepted
- Date: 2026-08-18
- Supersedes: nothing
- Related: PRD "分时价位（按日时段折扣）" (docs/prd/time-of-day-pricing-review.zh-CN.md),
  which is the review this implements; [ADR 0022](0022-cache-read-input-pricing.md),
  the same shape of change one term earlier; [ADR 0012](0012-pricing-selection-cross-store-consistency.md),
  which fixed price selection to the reservation instant

## Context

`DeploymentPriceVersion` could express a timeline — from this instant, charge
these rates — and nothing else. A provider that charges one rate in the morning
and another in the afternoon had no representation at all.

DeepSeek is the first upstream Halro speaks to that does this: peak from 09:00
to 12:00 and 14:00 to 18:00 Beijing time, off-peak at half the published rate,
already in force. An operator could only enter one number, so every deployment
pointed at it was mis-accounted, and *which direction* was decided by which
number the operator happened to pick — the peak rate over-charges the off-peak
hours, the off-peak rate under-charges the peak ones and lets budgets admit
traffic against a cost that is too low. A weighted average is wrong on every
single attempt and only converges over a month, which is no help to an
enforcement path that runs per request.

This is not a DeepSeek problem. It is a gap in how prices are expressed, and
the rule is shared by every provider once it exists.

## Decision

`DeploymentPriceVersion` and `DeploymentPriceProposal` gain an optional
`schedule`:

    schedule = { timezone, windows[] }
    window   = { start_minute, end_minute, input, cached_input, output, fixed }

Windows are half-open `[start, end)` minute offsets from midnight in the
schedule's own zone, disjoint, sorted, and bounded to one day. The version's
existing four terms become **the rates that apply outside every window**, so a
price with no schedule bills exactly as it always did, and a schedule is an
addition to a price rather than a replacement for one.

`PriceSnapshot` gains `schedule_tier`, naming the rung an attempt was pinned to.
Its four rate terms already hold that rung's rates, so everything that prices
against a snapshot — settlement, replay, the Parquet row, the budget path —
needed no change to keep working.

Five things are deliberate.

**The rule lives inside the price version, and it had to.** A settlement's
snapshot is re-derived from the version it names plus the instant it was pinned
at: `validateSnapshotAgainstPrice` re-runs `NewVersionedPriceSnapshot` and
demands digest equality, the Ledger compares a settlement's snapshot byte for
byte with its reservation's, and replay depends on the same identity. The
system already assumed *snapshot = f(price version, selectedAt)*. A schedule
inside the immutable version keeps `f` pure — it merely reads one more dimension
of `selectedAt`. A schedule held beside the version and referenced by it would
be mutable state inside `f`, and every past snapshot would stop being
reproducible the moment it changed. The cost is accepted: changing a discount
means a new price version, and rule tables are not reusable across versions.

**The rung is chosen at reservation and never re-read.** An attempt that starts
at 11:59 and finishes at 12:01 settles at the 11:59 rung. This is not a
convenience; a settlement that re-read the clock would produce a snapshot
differing from its reservation's and be rejected outright by the Ledger. The
reservation instant was already durable before any provider call, so there was
nothing new to persist.

**The timezone is the provider's, and it is nobody else's.** It is not the
instance's accounting timezone, which decides where a billing period starts, and
not the operator's display timezone. An instance keeping its books in UTC still
bills Beijing peak hours. It is stored as an IANA name and resolved through the
zone database, so a provider in a summer-time region transitions correctly —
a fixed UTC offset could not express that. `ValidateAccountingTimezone` and the
schedule's check now share one IANA rule (`validateIANAZoneName`) with distinct
labels, so they read the same and are never each other's default.

**An undecidable rung bills the dearest rate, it does not refuse.** If the zone
cannot be resolved at request time, `TierAt` returns the per-term maximum across
the base rates and every window. Refusing would take a deployment down over a
missing zone file; guessing an hour would silently under-bill. Taking the
maximum *of each term* rather than of some tier's total is what makes the result
at least as expensive as any rung the attempt could have landed on, whatever mix
of prompt, cached prompt and completion tokens it turns out to have. An
over-charge is visible to whoever reads the usage; an under-charge is not.
`halro doctor` reports the condition as `pricing_schedule_zone`, because nothing
else would — billing continues, so no error surfaces on its own.

**Window rates are absolute, not discount factors.** A factor would put a
second, derived way to express a rate beside the literal one and make every
billed amount depend on where that division rounds. Windows carry the same exact
micro-USD integers the base terms do.

## What this changed beyond the price form

`CalculateUSDTokensV1` now takes the instant it is pricing at. There is no
defensible answer to "what does this cost" that does not say when, and the
signature says so at every call site rather than letting one silently use the
base rates. The arithmetic moved to `PriceTier.Calculate`, the single
implementation both a resolved version and a frozen snapshot reach, so the
gateway's settlement and the ledger's re-derivation cannot drift by a micro-USD.

`POST /prices/preview` gained `at` and now returns every rung priced against the
same token counts, with the one answering `at` marked. Returning a single number
for a price that has several was the thing to fix, not a field to add.

The console's usage detail names the rung each attempt was billed at. Two
attempts with identical token counts settling at different amounts is now normal,
and without the rung that difference has no explanation anywhere an operator can
reach.

## Consequences for existing data

Neither metadata nor the ledger migrates, and no data directory has to be
re-initialised.

`schedule` and `schedule_tier` are absent-when-nil. A price version written
before this change decodes with no schedule, which *is* the round-the-clock
behaviour it had; a snapshot written before this change decodes with no tier and
re-encodes to the same bytes, so its digest is unchanged and a price pin
persisted by the previous build still matches. This is the difference from
ADR 0022, where the new term had to be required — a missing cache-read rate
would have read as "free", whereas a missing schedule reads as "one rate all
day", which is both correct and what the record meant.

Usage partitions are untouched: the tier rides inside the existing
`price_snapshot_json` column, so `parquetSchemaVersion` stays at 4.

## Alternatives rejected

**Schedule an external job to switch price versions twice a day.** Uses only
what already exists, and stakes accounting correctness on a timer outside the
process. Its failure is silent — the switch does not run, billing continues at
the previous rung, and no layer reports anything — which is the opposite of
fail-closed. It also produces two price versions a day, over seven hundred a
year, turning the immutable timeline into something no one can audit.

**Store the rule as a separate object the version references.** Reusable across
versions, and it breaks snapshot re-derivation the first time it is edited. See
the first decision above.

**Express windows as a discount factor on the base rates.** Compact, matches how
providers publish the discount, and introduces rounding into rate selection plus
a second representation of a rate.

**Extend this to volume tiers while the structure is open.** A volume tier's
rate depends on accumulated usage within a period, so it is not a function of
the price version and the instant. It would break the purity the first decision
rests on, and it needs its own review.
