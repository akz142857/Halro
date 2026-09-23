package app

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/akz142857/Halro/internal/domain"
	"github.com/akz142857/Halro/internal/provider"
)

// A Claude subscription sign-in does not yield one secret. It yields an access
// token that expires, a refresh token that outlives it, and an expiry instant —
// so the stored material is a JSON document, the way an AWS credential is,
// rather than the bare string every static-header scheme carries.
//
// Only AccessToken is read today. RefreshToken is accepted and sealed because it
// is part of what the operator has and discarding it would make a later refresh
// path require them to sign in again; nothing reads it, and the design question
// it belongs to — a refresh must not look like an admin rotation, see §7.2 of
// docs/prd/provider-offering-subscription-access-plan.zh-CN.md — is open. The
// moment something does read it, that question has to be answered first.
//
// ExpiresAt earns its place immediately: it is mapped onto the credential's own
// ExpiresAt, so a subscription credential dies through the expiry machinery
// Halro already has, visibly, instead of turning into upstream 401s.
type claudeSubscriptionCredential struct {
	AccessToken  string `json:"access_token"`
	RefreshToken string `json:"refresh_token,omitempty"`
	ExpiresAt    string `json:"expires_at,omitempty"`
}

// The prefixes a Claude sign-in produces, read first-hand from a live credential
// store on 2026-09-22 and matched without their version digits: `oat01` is a
// format revision, and pinning it is the exact-equality mistake this repository
// keeps re-learning.
const (
	claudeOAuthAccessPrefix  = "sk-ant-oat"
	claudeOAuthRefreshPrefix = "sk-ant-ort"
	claudeConsoleKeyPrefix   = "sk-ant-api"
)

// parseClaudeSubscriptionCredential validates the document and returns it. The
// errors name the member at fault and never echo material.
func parseClaudeSubscriptionCredential(plaintext []byte) (claudeSubscriptionCredential, error) {
	var parsed claudeSubscriptionCredential
	decoder := json.NewDecoder(strings.NewReader(string(plaintext)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&parsed); err != nil {
		return claudeSubscriptionCredential{}, errors.New(
			"a Claude subscription credential is a JSON document with an " +
				"access_token, and optionally a refresh_token and an expires_at",
		)
	}
	parsed.AccessToken = strings.TrimSpace(parsed.AccessToken)
	parsed.RefreshToken = strings.TrimSpace(parsed.RefreshToken)
	parsed.ExpiresAt = strings.TrimSpace(parsed.ExpiresAt)

	switch {
	case parsed.AccessToken == "":
		return claudeSubscriptionCredential{}, errors.New("access_token is required")
	// The mirror of the refusal on the metered product, and the reason both
	// exist: the two products share a host, so nothing but the credential itself
	// says which one an operator meant. A Console key here would spend the wrong
	// balance rather than fail, which is the worse of the two outcomes.
	case strings.HasPrefix(parsed.AccessToken, claudeConsoleKeyPrefix):
		return claudeSubscriptionCredential{}, claudeCredentialProductError{code: "anthropic_console_key_refused", err: errors.New(
			"this is an Anthropic Console API key, not a Claude subscription sign-in: " +
				"create it on the Claude Console product instead, or it would bill the " +
				"wrong account",
		)}
	case !strings.HasPrefix(parsed.AccessToken, claudeOAuthAccessPrefix):
		return claudeSubscriptionCredential{}, errors.New(
			"access_token is not a Claude subscription access token",
		)
	}
	if parsed.RefreshToken != "" && !strings.HasPrefix(parsed.RefreshToken, claudeOAuthRefreshPrefix) {
		return claudeSubscriptionCredential{}, errors.New(
			"refresh_token is not a Claude subscription refresh token",
		)
	}
	if parsed.ExpiresAt != "" {
		if _, err := parsed.expiry(); err != nil {
			return claudeSubscriptionCredential{}, err
		}
	}
	return parsed, nil
}

// expiry reads the declared instant. RFC 3339 is the spelling; a millisecond
// epoch is accepted too because that is what a Claude credential store holds,
// and making the operator convert it by hand would be a transcription step whose
// only possible outcome is a wrong number.
func (c claudeSubscriptionCredential) expiry() (time.Time, error) {
	if c.ExpiresAt == "" {
		return time.Time{}, nil
	}
	if instant, err := time.Parse(time.RFC3339, c.ExpiresAt); err == nil {
		return instant.UTC(), nil
	}
	var millis int64
	if _, err := fmt.Sscanf(c.ExpiresAt, "%d", &millis); err == nil && millis > 0 {
		return time.UnixMilli(millis).UTC(), nil
	}
	return time.Time{}, errors.New("expires_at must be an RFC 3339 instant or a millisecond epoch")
}

// claudeSubscriptionAuthorizer presents the access token the way the upstream
// accepts it. Measured 2026-09-22: the subscription credential is served by the
// public /v1/messages as a Bearer token, and refused as `x-api-key` — the
// opposite of the metered product, which is why this profile cannot share the
// Console profile's authorizer.
func claudeSubscriptionAuthorizer(ctx adapterBuildContext) (provider.Authorizer, error) {
	parsed, err := parseClaudeSubscriptionCredential(ctx.Plaintext)
	if err != nil {
		return nil, err
	}
	return provider.NewStaticHeaderAuthorizer(
		domain.CredentialAnthropicOAuth, "Authorization", "Bearer ", []byte(parsed.AccessToken), "x-api-key",
	)
}

// claudeCredentialProductError marks the two refusals whose cause is the
// operator having reached for the wrong one of two products that share a host.
// Typed so the console can answer in the reader's own language, and so the
// sentence lives in one place rather than being re-argued in the browser, which
// §7.2 forbids deciding product identity in anyway.
type claudeCredentialProductError struct {
	code string
	err  error
}

func (e claudeCredentialProductError) Error() string { return e.err.Error() }
func (e claudeCredentialProductError) Unwrap() error { return e.err }
