package openai

import (
	"encoding/json"
	"net/http"
	"strconv"

	"github.com/akz142857/Halro/internal/provider"
)

// MiniMax answers some failures with HTTP 200 and an error inside the body.
//
// Measured 2026-08-31 against api.minimax.io, api.minimaxi.com and
// api.minimax.chat with no credential:
//
//	POST /v1/embeddings {}  ->  200  {"base_resp":{"status_code":1004,
//	                                  "status_msg":"login fail: ..."}}
//
// The three chat-shaped routes answered that same request with a proper 401, so
// the practice is established on this host without being established for every
// route on it. That is exactly the case a guard is for: the cost of running it
// where it is not needed is one map lookup, and the cost of not running it where
// it is needed is a failed generation settled as a success — the reservation
// released, the cost committed, the audit trail recording a call that worked,
// and the caller holding an empty answer.
//
// Halro's OpenAI adapter reads the HTTP status and nothing else, so without this
// the whole class is invisible.

// miniMaxBaseResp is the envelope MiniMax attaches to a response body. A
// status_code of 0 is success; every other value is a refusal wearing a 200.
type miniMaxBaseResp struct {
	BaseResp *struct {
		StatusCode int64  `json:"status_code"`
		StatusMsg  string `json:"status_msg"`
	} `json:"base_resp"`
}

// checkMiniMaxBaseResp turns a 200-with-an-error into the provider error it
// already is.
//
// It is deliberately tolerant of a body that does not carry the envelope: most
// MiniMax responses on the OpenAI-shaped routes do not, and a missing envelope
// is not evidence of anything. Only a present envelope with a non-zero code is.
func checkMiniMaxBaseResp(payload []byte) *provider.Error {
	var envelope miniMaxBaseResp
	if json.Unmarshal(payload, &envelope) != nil || envelope.BaseResp == nil || envelope.BaseResp.StatusCode == 0 {
		return nil
	}
	return classifyMiniMaxStatus(envelope.BaseResp.StatusCode)
}

// classifyMiniMaxStatus maps a MiniMax status code onto the error class the rest
// of Halro reasons with.
//
// The class is not cosmetic and this is not a message-formatting table. Retry
// bounding, the circuit breaker and route failover all decide from
// provider.Error's class and its Retryable and Ambiguous flags. A refusal
// returned without a class — or with the wrong one — is how a rate-limited route
// keeps looking healthy to failover while every request on it is being turned
// away, which is the specific failure a 200-wrapped 1002 would otherwise cause.
//
// Codes from MiniMax's published error table (read 2026-08-31). An unlisted code
// is treated as an upstream failure and marked ambiguous rather than assumed
// harmless: an unknown outcome after the request was sent is exactly what
// ambiguous means, and settling it as free would hide a charge.
func classifyMiniMaxStatus(code int64) *provider.Error {
	result := &provider.Error{ProviderCode: "minimax_" + strconv.FormatInt(code, 10), StatusCode: http.StatusOK}
	switch code {
	case 1002:
		result.Class = provider.ErrorRateLimit
		result.Retryable = true
		result.Message = "MiniMax rate limit"
	case 1004:
		result.Class = provider.ErrorAuthentication
		result.Message = "MiniMax authentication failed"
	case 1008:
		// Not retryable and not a rate limit, though it arrives looking like one.
		// Retrying an insufficient balance cannot succeed and each attempt is
		// another call on the operator's credential.
		result.Class = provider.ErrorAuthentication
		result.Message = "MiniMax account balance is insufficient"
	case 1001:
		result.Class = provider.ErrorTimeout
		result.Retryable = true
		result.Ambiguous = true
		result.Message = "MiniMax request timed out"
	case 1000, 1013:
		result.Class = provider.ErrorProvider5xx
		result.Retryable = true
		result.Ambiguous = true
		result.Message = "MiniMax server error"
	case 1027, 1039, 2013:
		// The caller's request is the problem, so it must not be retried and must
		// not count against the deployment's health.
		result.Class = provider.ErrorBadRequest
		result.Message = "MiniMax rejected the request"
	default:
		result.Class = provider.ErrorProvider5xx
		result.Ambiguous = true
		result.Message = "MiniMax returned an unrecognised status code"
	}
	return result
}
