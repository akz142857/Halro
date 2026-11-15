package domain

import (
	"fmt"
	"math"
	"strings"
	"testing"
	"time"
)

func TestPricingAuditCanKeepAllRatesInALargeSchedule(t *testing.T) {
	schedule := PriceSchedule{Timezone: "America/Argentina/Buenos_Aires"}
	for start := 0; start < minutesPerDay; start += minutesPerDay / maxPriceWindows {
		schedule.Windows = append(schedule.Windows, PriceWindow{
			StartMinute: start, EndMinute: start + minutesPerDay/maxPriceWindows,
			InputMicrosPerMillion: math.MaxInt64, CachedInputMicrosPerMillion: math.MaxInt64,
			CacheWriteMicrosPerMillion: math.MaxInt64, CacheWriteLongMicrosPerMillion: math.MaxInt64,
			OutputMicrosPerMillion: math.MaxInt64, FixedRequestMicrosUSD: math.MaxInt64,
		})
	}
	if err := schedule.Validate(BillingModeMetered); err != nil {
		t.Fatal(err)
	}

	rate := int64(math.MaxInt64)
	intent := PricingAuditIntent{
		EventID: "audit_price", OccurredAt: time.Now().UTC(), ActorID: "admin",
		Action: "deployment_price.create", TargetType: "deployment_price_version", TargetID: "price",
		RequestSHA256: "sha256:" + strings.Repeat("a", 64),
		SourceType:    PriceSourceManual, SourceAssurance: PriceAssuranceAsserted,
		ChangeSummary: fmt.Sprintf("before=none after={billing:metered,input:%d,cached_input:%d,cache_write:%d,cache_write_long:%d,output:%d,fixed:%d}",
			rate, rate, rate, rate, rate, rate) + schedule.AuditSummary(),
	}
	if err := intent.Validate(); err != nil {
		t.Fatalf("audit rejected a complete %d-byte change summary: %v", len(intent.ChangeSummary), err)
	}
	intent.ChangeSummary = strings.Repeat("x", maxPricingChangeSummaryBytes+1)
	if err := intent.Validate(); err == nil {
		t.Fatal("audit accepted an oversized change summary")
	}
}
