package domain

import (
	"errors"
	"math"
	"math/big"
	"strings"
)

const microsPerUSD = int64(1_000_000)

type PriceCostBreakdown struct {
	InputCostMicrosUSD  int64 `json:"input_cost_micros_usd"`
	OutputCostMicrosUSD int64 `json:"output_cost_micros_usd"`
	FixedCostMicrosUSD  int64 `json:"fixed_cost_micros_usd"`
	TotalCostMicrosUSD  int64 `json:"total_cost_micros_usd"`
}

// CalculateUSDTokensV1 applies usd_token_v1 with exact integer arithmetic and
// rounds the input and output components independently up to one micro-USD.
func CalculateUSDTokensV1(inputTokens, outputTokens int64, price DeploymentPriceVersion) (PriceCostBreakdown, error) {
	if inputTokens < 0 || outputTokens < 0 {
		return PriceCostBreakdown{}, errors.New("token counts cannot be negative")
	}
	if err := price.Validate(); err != nil {
		return PriceCostBreakdown{}, err
	}
	if price.BillingMode == BillingModeFree {
		return PriceCostBreakdown{}, nil
	}
	input, err := ceilTokenComponent(inputTokens, price.InputMicrosPerMillion)
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
