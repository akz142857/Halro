package domain

import (
	"encoding/base64"
	"errors"
	"strings"
	"time"
)

// RouteSuspension is an admission-gate suspension that outlives the process.
//
// Most of what the gate holds is deliberately not here. Probe verdicts refill
// within one probe interval, counters resetting on restart is ordinary, and an
// availability window is thirty seconds where a restart is longer — storing one
// buys nothing. What earns a row is the long refusal: a credential the upstream
// will not accept until an operator replaces it, and an exhausted quota whose
// window reaches hours. Re-admitting those on every restart is the tax this
// whole design removed.
//
// It carries no upstream prose. Status and code are what the upstream said,
// already narrowed by the adapter, and the reason is Halro's own enumeration.
type RouteSuspension struct {
	ScopeKind string `json:"scope_kind"`
	// ScopeKey is the gate's own key. For a credential-and-model scope its two
	// halves are joined by a NUL, which is exactly why ScopeID exists.
	ScopeKey           string    `json:"scope_key"`
	Reason             string    `json:"reason"`
	ProviderStatus     int       `json:"provider_status,omitempty"`
	ProviderCode       string    `json:"provider_code,omitempty"`
	ObservedAt         time.Time `json:"observed_at"`
	CredentialRevision uint64    `json:"credential_revision,omitempty"`
	// Until is when the window ends, zero for a suspension no clock will end.
	Until      time.Time `json:"until,omitempty"`
	Indefinite bool      `json:"indefinite"`
	// WindowSeconds is the window this suspension is currently serving, so the
	// doubling ladder resumes where it stopped rather than restarting at the
	// policy's opening figure.
	WindowSeconds int64 `json:"window_seconds,omitempty"`
}

func (s RouteSuspension) Validate() error {
	if strings.TrimSpace(s.ScopeKind) == "" {
		return errors.New("route suspension requires a scope kind")
	}
	if strings.TrimSpace(s.ScopeKey) == "" {
		return errors.New("route suspension requires a scope key")
	}
	if strings.TrimSpace(s.Reason) == "" {
		// A suspension with no canonical reason is an availability window, and
		// those are never stored. A row without one is therefore a row nothing
		// can decide a policy for, and admitting it would restore a suspension
		// the gate cannot end.
		return errors.New("route suspension requires a reason")
	}
	if s.ObservedAt.IsZero() {
		return errors.New("route suspension requires an observation time")
	}
	if !s.Indefinite && s.Until.IsZero() {
		return errors.New("a route suspension that is not indefinite requires an end time")
	}
	return nil
}

// ScopeID is the handle an operator passes back to clear this suspension.
//
// A scope is a pair rather than an identifier, and one half of one kind of pair
// contains a NUL byte. Rendering that into a URL path is a choice between
// lossy and unreadable, so the suspension listing hands out an opaque handle
// instead and the clear takes it verbatim. Base64url has no characters a path
// segment has to escape.
func (s RouteSuspension) ScopeID() string {
	return EncodeRouteScopeID(s.ScopeKind, s.ScopeKey)
}

// RouteScopeSeparator joins the two halves inside a scope handle. It is not the
// NUL the credential-and-model key uses internally, so decoding cannot mistake
// one for the other.
const routeScopeSeparator = "\x1f"

func EncodeRouteScopeID(kind, key string) string {
	return base64.RawURLEncoding.EncodeToString([]byte(kind + routeScopeSeparator + key))
}

// DecodeRouteScopeID reads a handle back into the pair it names. An
// unparseable handle is not an error worth distinguishing from a scope that is
// not suspended: both mean there is nothing here to clear.
func DecodeRouteScopeID(scopeID string) (kind, key string, ok bool) {
	decoded, err := base64.RawURLEncoding.DecodeString(strings.TrimSpace(scopeID))
	if err != nil {
		return "", "", false
	}
	kind, key, found := strings.Cut(string(decoded), routeScopeSeparator)
	if !found || kind == "" || key == "" {
		return "", "", false
	}
	return kind, key, true
}
