package gateway

import (
	"net/http"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

// Which upstream refusals mean "this account's allowance is spent" rather than
// "you are going too fast".
//
// Six of the nine upstreams Halro speaks to put both on HTTP 429 and separate
// them only by the code in the body — Kimi's own troubleshooting page says it
// outright: "429 is not a single cause. Check the error.type in the response
// first." That is why the reason, not the status, is what routing reads.
//
// It matters because the two conditions clear on different timescales. A rate
// limit is over in seconds, and the rate_limited policy probes accordingly: one
// second, doubling to sixty. An exhausted balance is not over until somebody
// pays, and Anthropic documents its spend-cap refusal as carrying no
// retry-after and failing until access resumes. Treating the second as the
// first is a probe every minute against a condition that will not change for
// hours, on a credential the operator is being billed to hold.
//
// Everything here is grade B — each vendor's published error table, gathered in
// docs/verification/route-eligibility-refusal-matrix.zh-CN.md §4 — and that is
// exactly as far as it goes. The window lengths and whether the scope is the
// credential or the credential-and-model still wait on real refusals, and the
// design takes the conservative side of both in the meantime.

// quotaExhaustionCodes are the codes distinctive enough to read without knowing
// which upstream said them. No vendor in the matrix uses any of these spellings
// to mean an ordinary rate limit, so they need no profile gate — and a gate
// would be the thing that breaks the day a vendor is served through a profile
// nobody thought to add.
var quotaExhaustionCodes = map[string]provider.FailureReason{
	// Halro's own canonical spellings, so an adapter that concludes this itself
	// lands in the same place as one that only forwards a code.
	"subscription_quota_exhausted": provider.FailureReasonSubscriptionQuotaExhausted,
	"token_plan_quota_exhausted":   provider.FailureReasonSubscriptionQuotaExhausted,
	// OpenAI: a spent balance and the two spend ceilings an organization or a
	// project can be given. `insufficient_quota` is the older spelling of the
	// first and is very likely the one a real spent account still answers with
	// — it predates the current error-codes page and is what the ecosystem
	// documents — so leaving it out would have meant the most common OpenAI
	// case going on being read as a rate limit.
	"insufficient_quota":                provider.FailureReasonSubscriptionQuotaExhausted,
	"credit_balance_exhausted":          provider.FailureReasonSubscriptionQuotaExhausted,
	"organization_spend_limit_exceeded": provider.FailureReasonSubscriptionQuotaExhausted,
	"project_spend_limit_exceeded":      provider.FailureReasonSubscriptionQuotaExhausted,
	// Kimi / Moonshot: insufficient balance, arrears, or an expired voucher,
	// all under one 429 alongside rate_limit_reached_error.
	"exceeded_current_quota_error": provider.FailureReasonSubscriptionQuotaExhausted,
	// Anthropic: a payment problem, which arrives as 402 rather than 429. The
	// other Anthropic case — a monthly spend cap — is deliberately absent; see
	// below.
	"billing_error": provider.FailureReasonSubscriptionQuotaExhausted,
}

// quotaExhaustionByType are the codes that need to know who said them.
//
// A bare number means nothing on its own: 1113 is BigModel's arrears code and
// somebody else's anything. Reading one without the vendor would be guessing,
// and guessing in this direction suspends a working credential.
var quotaExhaustionByType = map[domain.ProviderType]map[string]provider.FailureReason{
	domain.ProviderBigModel: {
		"1113": provider.FailureReasonSubscriptionQuotaExhausted,
	},
}

// quotaExhaustionStatusByType covers the upstreams that answer exhaustion with
// a status of its own and no code worth matching. DeepSeek is the one vendor in
// the matrix that separates the two conditions this way — 402 for an
// insufficient balance, 429 for a rate limit — which is the cheap design the
// other eight did not give us.
var quotaExhaustionStatusByType = map[domain.ProviderType]map[int]provider.FailureReason{
	domain.ProviderDeepSeek: {
		http.StatusPaymentRequired: provider.FailureReasonSubscriptionQuotaExhausted,
	},
}

// Two cells of the matrix are deliberately not implemented, and both wait for a
// real response rather than for a cleverer guess.
//
// Anthropic's monthly spend cap arrives as 429 rate_limit_error, the same
// status and the same type as an ordinary rate limit, and is distinguishable
// only by the absence of a retry-after header. Inferring an indefinite
// condition from a missing header would turn a transient limit into a
// suspension a human has to clear — the most expensive direction to be wrong
// in.
//
// Gemini's daily quota is the same shape one level down. The adapter surfaces
// error.status (internal/provider/gemini/adapter.go), which for a 429 is the
// RPC name RESOURCE_EXHAUSTED — the same value for the daily quota and for the
// per-minute limit, which is precisely the ambiguity the matrix was written
// about. The distinguishing text lives somewhere this adapter does not read,
// and inventing an extraction from a structure nobody here has observed would
// be the same mistake as reading Anthropic's missing header. An entry for a
// spelling no Gemini path produces is worse than no entry: it reads as
// coverage. So the cell is empty, and what would fill it is one real Gemini
// quota refusal showing which field carries the distinction.

// quotaExhaustionReason reads one classified refusal for a statement that the
// account's allowance is spent. An empty answer means the refusal said nothing
// of the kind, not that it was a rate limit.
func quotaExhaustionReason(classified *provider.Error, target provider.Target) provider.FailureReason {
	// The code half alone. An upstream that names the field it refused arrives
	// as "code:param", and a table keyed on the whole identifier would miss
	// every refusal that happened to carry one.
	code := provider.RefusalCode(classified.ProviderCode)
	if reason, known := quotaExhaustionCodes[code]; known {
		return reason
	}
	providerType, _, registered := domain.RegisteredProviderProfile(target.ProfileID)
	if !registered {
		return ""
	}
	if reason, known := quotaExhaustionByType[providerType][code]; known {
		return reason
	}
	return quotaExhaustionStatusByType[providerType][classified.StatusCode]
}
