# ADR 0022: Pricing the cached span of a prompt

- Status: Accepted
- Date: 2026-08-17
- Supersedes: nothing
- Related: PRD "Versioned model pricing" §5 (which listed per-cache-tier pricing
  as a first-version non-goal and required that adding it extend the price
  formula rather than reuse the ordinary input token field)

## Context

Every provider Halro speaks to reports how much of a prompt it served from its
own cache, and every one of them bills that span far below the ordinary input
rate — a tenth of it on OpenAI's published table ($0.50 against $5.00 per
million for the model in the screenshot that started this work).

Halro already recorded the count. `semantic.Usage.CachedInputTokens` normalises
`prompt_tokens_details.cached_tokens`, `input_tokens_details.cached_tokens`,
`cache_read_input_tokens` and Gemini's `cachedContentTokenCount` onto one
convention — a subset of `InputTokens`, never an addition to it — and the count
reached the ledger event and the Parquet partition.

Nothing priced it. `DeploymentPriceVersion` carried one input rate, and both
`PriceSnapshot.Calculate` and the gateway's `EstimateCostMicros` multiplied the
whole prompt by it. A cache-heavy workload was billed up to ten times what the
provider charged for that span. The code said so: `recordUsageTiers` set
`CostEstimated` on any attempt with a cache tier, with a comment describing the
number as a deliberate upper bound "until a pricing version can express the
tiers".

## Decision

`DeploymentPriceVersion`, `DeploymentPriceProposal` and `PriceSnapshot` gain
`cached_input_micros_per_million`, and the cost formula becomes

    (input_tokens - cached_input_tokens) x input_rate
  + cached_input_tokens                 x cached_input_rate
  + output_tokens                       x output_rate
  + fixed_request_cost

with each token component rounded up independently to one micro-USD, exactly as
before. `PriceCostBreakdown.InputCostMicrosUSD` reports the whole prompt: the
split stays reconstructible from the attempt's cached token count and its frozen
snapshot, and no ledger or Parquet column had to be added to keep it.

Three consequences are deliberate.

**The term is required, not optional.** A missing rate would read as zero, which
prices cached tokens free — the one wrong answer this field exists to prevent.
`PriceSnapshot.Validate` refuses a versioned snapshot without it, and the Admin
API refuses a create whose `cached_input_usd_per_million` is absent rather than
defaulting it.

**A rate nobody stated is the input rate, never zero.** The Admin console's
cache-read field follows the input field until the operator edits it, and the
bootstrap CLI's `-cached-input-micros-per-million` bills cached tokens at the
input rate when it is left unset (`BootstrapOptions` carries a `*int64`, so an
omitted field cannot mean "free"). Every default therefore lands on what a cached
token cost before this change, and lowering it is a deliberate act.

**Anything that does not yet know the split pays the higher rate.** Reservations
are taken before the provider answers, Token Guard bounds a request before it
runs, and a lease recovered after a crash knows only its prepared bounds. All
three price the whole prompt at the ordinary input rate. Over-reserving and
conservatively settling an ambiguous outcome are the existing rules; nothing
here relaxes them.

**Cache *writes* are still unpriced.** Anthropic bills a cache write at a
premium over the input rate (1.25x on its published table) and Halro has no term
for it, so those tokens are charged at the ordinary input rate — an under-charge.
`recordUsageTiers` therefore keeps setting `CostEstimated` when a write tier is
reported, and stops setting it for cache reads, which are now charged at what
they cost. `CostEstimated` remains the marker identifying rows worth re-rating
once a write term exists.

## What this exposed on the native Anthropic path

A rate is only worth having if the tiers reach it. The native `/v1/messages`
paths — both unary and streaming — built their settlement usage straight from
`message.Usage.InputTokens`, which on that API *excludes* both cache tiers, and
never carried `CachedInputTokens` or `CacheWriteInputTokens` at all. A request
whose prompt was 95% cache-read was recorded as the 5% that was not, so the
ledger under-reported the prompt and the new rate had nothing to apply to. The
compatibility mapping had recovered the full span with `Usage.PromptTokens()`
since it was written; the native path was reading the same field with the other
meaning. Both now translate through one helper, `nativeAnthropicUsage`.

## Consequences for existing data

Metadata migrates; the ledger does not.

Metadata schema 30 (`deployment_price_cached_input_rate`) reconstructs the term
on stored price versions and proposals by copying the input rate onto it. That
is not an invention: until this change a cached token *was* billed at the input
rate, so a migrated price charges exactly what it charged the day before, and an
operator lowers it deliberately. Proposals are re-digested, since their digest
covers their billing terms.

Ledger events are immutable and carry their price snapshot inline. An event
written before this change has no cache-read term, so `Event.Validate` refuses
it on replay and the process will not open that WAL. **An instance with settled
attempts in its ledger must re-initialise its data directory.** Pre-1.0.0 this
is the accepted cost of fixing the term in place rather than carrying a second
read path that treats a missing rate as "bill it as input" forever.

## Alternatives rejected

**Decode a missing rate as the input rate.** Keeps every existing WAL readable,
and permanently keeps two meanings for one field — precisely the pre-1.0.0 debt
the repository refuses to accumulate.

**A separate `cached_input_cost_micros_usd` on the ledger event and Parquet
row.** More columns, a format bump for the usage partitions, and no information
that the cached token count plus the snapshot does not already give.

**Leave it and rely on `CostEstimated`.** The flag says the number is wrong; it
does not make the number right, and a cache-heavy workload's bill was wrong by
most of its prompt cost.
