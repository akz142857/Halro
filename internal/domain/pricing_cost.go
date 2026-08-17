package domain

import (
	"errors"
	"math"
	"math/big"
	"strings"
)

const microsPerUSD = int64(1_000_000)

// PriceCostBreakdown reports the cost of one attempt by component.
// InputCostMicrosUSD covers the whole prompt — the span billed at the ordinary
// input rate plus the cached span billed at the cache-read rate — because the
// two are always charged against the same budget and the split is already
// reconstructible from the attempt's cached token count and its price snapshot.
type PriceCostBreakdown struct {
	InputCostMicrosUSD  int64 `json:"input_cost_micros_usd"`
	OutputCostMicrosUSD int64 `json:"output_cost_micros_usd"`
	FixedCostMicrosUSD  int64 `json:"fixed_cost_micros_usd"`
	TotalCostMicrosUSD  int64 `json:"total_cost_micros_usd"`
}

// CalculateUSDTokensV1 applies usd_token_v1 with exact integer arithmetic and
// rounds each priced component independently up to one micro-USD.
//
// cachedInputTokens is a subset of inputTokens, following semantic.Usage: the
// span the provider served from cache is billed at the cache-read rate and the
// remainder at the ordinary input rate. Callers that do not know the split yet —
// a reservation taken before the provider answers, a lease recovered from its
// prepared bounds — pass zero and are charged the higher rate throughout.
func CalculateUSDTokensV1(inputTokens, cachedInputTokens, outputTokens int64, price DeploymentPriceVersion) (PriceCostBreakdown, error) {
	if inputTokens < 0 || cachedInputTokens < 0 || outputTokens < 0 {
		return PriceCostBreakdown{}, errors.New("token counts cannot be negative")
	}
	if cachedInputTokens > inputTokens {
		return PriceCostBreakdown{}, errors.New("cached input tokens cannot exceed input tokens")
	}
	if err := price.Validate(); err != nil {
		return PriceCostBreakdown{}, err
	}
	if price.BillingMode == BillingModeFree {
		return PriceCostBreakdown{}, nil
	}
	input, err := ceilInputComponents(inputTokens, cachedInputTokens, price.InputMicrosPerMillion, price.CachedInputMicrosPerMillion)
	if err != nil {
		return PriceCostBreakdown{}, err
	}
	output, err := ceilTokenComponent(outputTokens, price.OutputMicrosPerMillion)
	if err != nil {
		return PriceCostBreakdown{}, err
	}
	total := new(big.Int).SetInt64(input)
	total.Add(total, big.NewInt(output))
	total.Add(total, big.NewInt(price.FixedRequestMicrosUSD))
	if !total.IsInt64() || total.Sign() < 0 {
		return PriceCostBreakdown{}, errors.New("price calculation overflows int64 micro-USD")
	}
	return PriceCostBreakdown{
		InputCostMicrosUSD: input, OutputCostMicrosUSD: output,
		FixedCostMicrosUSD: price.FixedRequestMicrosUSD, TotalCostMicrosUSD: total.Int64(),
	}, nil
}

// ceilInputComponents prices the uncached and cached spans of one prompt,
// rounding each up independently before adding them. Rounding per tier rather
// than on the sum keeps the result identical whichever caller computes it —
// the gateway's settlement and the ledger's re-derivation must agree to the
// micro-USD or the settlement is rejected as not matching its price snapshot.
func ceilInputComponents(inputTokens, cachedInputTokens, microsPerMillion, cachedMicrosPerMillion int64) (int64, error) {
	uncached, err := ceilTokenComponent(inputTokens-cachedInputTokens, microsPerMillion)
	if err != nil {
		return 0, err
	}
	cached, err := ceilTokenComponent(cachedInputTokens, cachedMicrosPerMillion)
	if err != nil {
		return 0, err
	}
	sum := new(big.Int).Add(big.NewInt(uncached), big.NewInt(cached))
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
