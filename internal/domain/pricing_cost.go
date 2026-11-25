package domain

import (
	"errors"
	"math"
	"math/big"
	"strings"
	"time"
)

const microsPerUSD = int64(1_000_000)

// PriceCostBreakdown reports the cost of one attempt by component.
// InputCostMicrosUSD covers the whole prompt — the span billed at the ordinary
// input rate plus the cached span billed at the cache-read rate and the written
// span billed at its cache-write rate — because they are always charged against
// the same budget and the split is already reconstructible from the attempt's
// token tiers and its price snapshot.
type PriceCostBreakdown struct {
	InputCostMicrosUSD  int64 `json:"input_cost_micros_usd"`
	OutputCostMicrosUSD int64 `json:"output_cost_micros_usd"`
	FixedCostMicrosUSD  int64 `json:"fixed_cost_micros_usd"`
	TotalCostMicrosUSD  int64 `json:"total_cost_micros_usd"`
}

// BilledTokens is the token breakdown one attempt is priced on, following
// semantic.Usage: CachedInput and CacheWrite are subsets of Input, never
// additions to it, and CacheWriteLong is the subset of CacheWrite written with
// the provider's longer cache lifetime (Anthropic's 1h, OpenAI's 12h). What is
// left of the prompt is billed at the ordinary input rate.
type BilledTokens struct {
	Input          int64
	CachedInput    int64
	CacheWrite     int64
	CacheWriteLong int64
	Output         int64
}

func (t BilledTokens) Validate() error {
	if t.Input < 0 || t.CachedInput < 0 || t.CacheWrite < 0 || t.CacheWriteLong < 0 || t.Output < 0 {
		return errors.New("token counts cannot be negative")
	}
	if t.CacheWriteLong > t.CacheWrite {
		return errors.New("long-lived cache write tokens cannot exceed cache write tokens")
	}
	if t.CachedInput > t.Input || t.CacheWrite > t.Input-t.CachedInput {
		return errors.New("cached and cache write input tokens cannot exceed input tokens")
	}
	return nil
}

// CalculateUSDTokensV1 applies usd_token_v1 with exact integer arithmetic and
// rounds each priced component independently up to one micro-USD.
//
// Callers that do not know the prompt's split yet — a reservation taken before
// the provider answers, a lease recovered from its prepared bounds — do not
// pass zero tiers: they price PriceTier.ConservativeTokens, which bills the
// whole prompt at the most expensive input-side rate.
//
// selectedAt is the instant the price is being applied at, and it is a required
// argument rather than an optional refinement: a price version can bill
// different rates at different times of day, and there is no defensible answer
// to "what does this cost" that does not say when. It is unused by a version
// that carries no schedule. Callers pricing an already-settled attempt do not
// come through here at all — they call PriceSnapshot.Calculate, which reads the
// rung frozen at reservation instead of consulting a clock a second time.
func CalculateUSDTokensV1(tokens BilledTokens, price DeploymentPriceVersion, selectedAt time.Time) (PriceCostBreakdown, error) {
	if err := tokens.Validate(); err != nil {
		return PriceCostBreakdown{}, err
	}
	if err := price.Validate(); err != nil {
		return PriceCostBreakdown{}, err
	}
	if price.BillingMode == BillingModeFree {
		return PriceCostBreakdown{}, nil
	}
	return price.TierAt(selectedAt).Calculate(tokens)
}

// Calculate is the one place usd_token_v1 arithmetic lives. A price version
// resolves to a tier before pricing and a settled snapshot carries the tier it
// was pinned to, so both arrive here with literal rates and neither can drift
// from the other by a micro-USD — which matters, because a settlement whose
// amount disagrees with its reservation's is rejected outright.
func (t PriceTier) Calculate(tokens BilledTokens) (PriceCostBreakdown, error) {
	if err := tokens.Validate(); err != nil {
		return PriceCostBreakdown{}, err
	}
	if t.hasNegativeRate() {
		return PriceCostBreakdown{}, errors.New("price amounts cannot be negative")
	}
	input, err := ceilInputComponents(tokens, t)
	if err != nil {
		return PriceCostBreakdown{}, err
	}
	output, err := ceilTokenComponent(tokens.Output, t.OutputMicrosPerMillion)
	if err != nil {
		return PriceCostBreakdown{}, err
	}
	total := new(big.Int).SetInt64(input)
	total.Add(total, big.NewInt(output))
	total.Add(total, big.NewInt(t.FixedRequestMicrosUSD))
	if !total.IsInt64() || total.Sign() < 0 {
		return PriceCostBreakdown{}, errors.New("price calculation overflows int64 micro-USD")
	}
	return PriceCostBreakdown{
		InputCostMicrosUSD: input, OutputCostMicrosUSD: output,
		FixedCostMicrosUSD: t.FixedRequestMicrosUSD, TotalCostMicrosUSD: total.Int64(),
	}, nil
}

// ConservativeTokens is the split to price a prompt on when nothing yet says
// how the provider will bill it. Every input-side rate is a candidate, and the
// whole prompt is assigned to the most expensive one: a cache write costs more
// than an ordinary input token, so "no split known" can no longer mean "all
// ordinary input" without under-reserving. Ties go to the ordinary input rate,
// so equal rates keep the estimated split in ordinary input.
//
// The answer is a token split rather than an amount so that a settlement built
// from it re-derives to the same micro-USD against the snapshot.
func (t PriceTier) ConservativeTokens(inputTokens, outputTokens int64) BilledTokens {
	tokens := BilledTokens{Input: inputTokens, Output: outputTokens}
	highest := t.InputMicrosPerMillion
	if t.CachedInputMicrosPerMillion > highest {
		highest = t.CachedInputMicrosPerMillion
		tokens = BilledTokens{Input: inputTokens, CachedInput: inputTokens, Output: outputTokens}
	}
	if t.CacheWriteMicrosPerMillion > highest {
		highest = t.CacheWriteMicrosPerMillion
		tokens = BilledTokens{Input: inputTokens, CacheWrite: inputTokens, Output: outputTokens}
	}
	if t.CacheWriteLongMicrosPerMillion > highest {
		tokens = BilledTokens{Input: inputTokens, CacheWrite: inputTokens, CacheWriteLong: inputTokens, Output: outputTokens}
	}
	return tokens
}

// ReservationCost bounds all possible input splits for the supplied token counts.
func (t PriceTier) ReservationCost(input, output int64) (int64, error) {
	cost, err := t.Calculate(t.ConservativeTokens(input, output))
	if err != nil {
		return 0, err
	}
	// Independent rounding can make a split cost more than rounding once at
	// the highest rate. Only occupied tiers with fractional per-token rates
	// can round up; each after the first can add at most one micro-USD.
	var fractionalTiers int64
	for _, span := range t.inputSpans(BilledTokens{}) {
		if span.rate%microsPerUSD != 0 {
			fractionalTiers++
		}
	}
	headroom := max(0, min(fractionalTiers, input)-1)
	if cost.TotalCostMicrosUSD > math.MaxInt64-headroom {
		return 0, errors.New("token reservation overflows int64 micro-USD")
	}
	return cost.TotalCostMicrosUSD + headroom, nil
}

func (t PriceTier) writesShareInputRounding() bool {
	return t.CacheWriteMicrosPerMillion == t.InputMicrosPerMillion &&
		t.CacheWriteLongMicrosPerMillion == t.InputMicrosPerMillion &&
		(t.Provenance == nil || !t.Provenance.SeparateCacheWriteRounding)
}

// CacheWriteCostEstimated reports whether the charge uses an inherited rate or
// depends on a lifetime split the provider did not reliably report.
func (t PriceTier) CacheWriteCostEstimated(tokens BilledTokens, exactSplit bool) bool {
	if tokens.CacheWrite == 0 {
		return false
	}
	if !exactSplit {
		// Equal rates can still differ by one micro-dollar when the split
		// forces separate rounding of fractional charges.
		if t.CacheWriteMicrosPerMillion != t.CacheWriteLongMicrosPerMillion ||
			(tokens.CacheWrite > 1 && !t.writesShareInputRounding() && t.CacheWriteMicrosPerMillion%microsPerUSD != 0) {
			return true
		}
	}
	shortRateAssumed := !t.ShortExplicit && t.CacheWriteMicrosPerMillion == t.InputMicrosPerMillion
	longRateAssumed := !t.LongExplicit && t.CacheWriteLongMicrosPerMillion == t.InputMicrosPerMillion
	return (shortRateAssumed && (!exactSplit || tokens.CacheWrite > tokens.CacheWriteLong)) ||
		(longRateAssumed && (!exactSplit || tokens.CacheWriteLong > 0))
}

type inputPriceSpan struct{ tokens, rate int64 }

func (t PriceTier) inputSpans(tokens BilledTokens) [4]inputPriceSpan {
	ordinary := tokens.Input - tokens.CachedInput - tokens.CacheWrite
	short := inputPriceSpan{tokens.CacheWrite - tokens.CacheWriteLong, t.CacheWriteMicrosPerMillion}
	long := inputPriceSpan{tokens.CacheWriteLong, t.CacheWriteLongMicrosPerMillion}
	// Migration 43 backfills writes at the ordinary input rate. Those tokens
	// shared one rounded component before the upgrade, so keep them together
	// whenever both write rates match the input rate.
	if t.writesShareInputRounding() {
		ordinary += tokens.CacheWrite
		short, long = inputPriceSpan{}, inputPriceSpan{}
	}
	return [4]inputPriceSpan{
		{ordinary, t.InputMicrosPerMillion},
		{tokens.CachedInput, t.CachedInputMicrosPerMillion},
		short,
		long,
	}
}

// ceilInputComponents rounds each priced component up to a micro-USD.
func ceilInputComponents(tokens BilledTokens, t PriceTier) (int64, error) {
	sum := new(big.Int)
	for _, span := range t.inputSpans(tokens) {
		cost, err := ceilTokenComponent(span.tokens, span.rate)
		if err != nil {
			return 0, err
		}
		sum.Add(sum, big.NewInt(cost))
	}
	if !sum.IsInt64() {
		return 0, errors.New("token price component overflows int64 micro-USD")
	}
	return sum.Int64(), nil
}

func ceilTokenComponent(tokens, microsPerMillion int64) (int64, error) {
	if tokens == 0 || microsPerMillion == 0 {
		return 0, nil
	}
	product := new(big.Int).Mul(big.NewInt(tokens), big.NewInt(microsPerMillion))
	divisor := big.NewInt(1_000_000)
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(product, divisor, remainder)
	if remainder.Sign() != 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsInt64() {
		return 0, errors.New("token price component overflows int64 micro-USD")
	}
	return quotient.Int64(), nil
}

// ParseUSDMicros converts a non-negative decimal USD string to exact
// micro-USD. More than six fractional digits are rejected rather than rounded.
func ParseUSDMicros(value string) (int64, error) {
	value = strings.TrimSpace(value)
	if value == "" || strings.HasPrefix(value, "+") || strings.HasPrefix(value, "-") {
		return 0, errors.New("USD amount must be a non-negative decimal string")
	}
	parts := strings.Split(value, ".")
	if len(parts) > 2 || parts[0] == "" || (len(parts) == 2 && parts[1] == "") {
		return 0, errors.New("USD amount is invalid")
	}
	if len(parts) == 2 && len(parts[1]) > 6 {
		return 0, errors.New("USD amount supports at most six fractional digits")
	}
	for _, part := range parts {
		for _, char := range part {
			if char < '0' || char > '9' {
				return 0, errors.New("USD amount is invalid")
			}
		}
	}
	fraction := ""
	if len(parts) == 2 {
		fraction = parts[1]
	}
	fraction += strings.Repeat("0", 6-len(fraction))
	whole := new(big.Int)
	if _, ok := whole.SetString(parts[0], 10); !ok {
		return 0, errors.New("USD amount is invalid")
	}
	whole.Mul(whole, big.NewInt(microsPerUSD))
	if fraction != "" {
		fractionValue := new(big.Int)
		fractionValue.SetString(fraction, 10)
		whole.Add(whole, fractionValue)
	}
	if !whole.IsInt64() || whole.Sign() < 0 || whole.Cmp(big.NewInt(math.MaxInt64)) > 0 {
		return 0, errors.New("USD amount overflows int64 micro-USD")
	}
	return whole.Int64(), nil
}
