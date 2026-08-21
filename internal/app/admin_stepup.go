package app

import (
	"net/http"
	"sync"
	"time"

	"github.com/akz142857/Halro/internal/domain"
)

// adminStepUpFailuresPerMinute bounds how many times one account may present
// step-up material that does not verify. Without it an authenticated session is
// an offline-speed password oracle: the caller already holds a valid cookie, so
// nothing else in the request path slows a guess down, and Argon2id verifying
// honestly per attempt only makes each guess expensive for the server rather
// than for the guesser.
//
// Only failures are counted. The budget exists to bound guessing, and an
// operator who proves who they are has not guessed — charging successes too
// would throttle a legitimate clean-up of several resources while barely
// inconveniencing an attacker.
const adminStepUpFailuresPerMinute = 5

// requireDestructiveStepUp re-establishes that the operator behind an
// authenticated session is present, for actions whose damage a stolen session
// alone should not be able to do. It reads the same two fields the pricing and
// admin-account endpoints already ask for, so an operator meets one shape
// across the console rather than one per endpoint.
//
// A request without a body fails here. There is no "no body means skip"
// fallback: that would make the check optional for exactly the caller who
// omits it.
func (r *Runtime) requireDestructiveStepUp(writer http.ResponseWriter, request *http.Request) bool {
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	var input struct {
		CurrentPassword string `json:"current_password"`
		TOTPCode        string `json:"totp_code"`
	}
	if err := decodeAdminJSON(request, &input); err != nil {
		writeJSON(writer, http.StatusUnauthorized, map[string]string{
			"error": "recent re-authentication required",
			"code":  "recent_reauth_required",
		})
		return false
	}
	// No scrubbing here, deliberately. The password arrives inside a JSON body,
	// so by the time this runs the process holds it in net/http's read buffer,
	// in the decoder's buffer, and in the immutable string the decoder produced
	// — none of which this code owns or can overwrite. Copying it into a []byte
	// so that copy could be clear()ed zeroed the one instance that was about to
	// become garbage anyway, and left a line that reads like the secret had been
	// scrubbed from the process. What actually bounds this material is
	// everything around it: it is never logged, never persisted, never echoed,
	// and never leaves this call.
	return r.verifyAdminStepUp(writer, request, admin.session.Username, input.CurrentPassword, input.TOTPCode)
}

// stepUpMaterial carries the same two fields requireDestructiveStepUp reads,
// for mutations whose body is the resource itself. Those handlers cannot call
// requireDestructiveStepUp: it consumes the whole body to find the material,
// and the body is needed for the resource. Embedding this in the handler's
// input keeps one shape on the wire — an operator supplies the same two fields
// whether they are deleting a policy or editing it down to nothing.
type stepUpMaterial struct {
	CurrentPassword string `json:"current_password"`
	TOTPCode        string `json:"totp_code,omitempty"`
}

// requireStepUpMaterial applies the step-up criterion to an edit that weakens a
// protection currently in force.
//
// The criterion is the one unblockAdminProject already states: not only what
// destroys state, but what removes a protection that is in force. Applying it
// to deletes alone left an asymmetry with no defensible reading — deleting a
// redaction policy demanded re-authentication, while editing that same policy
// down to zero rules did not, and the data plane cannot tell the two apart.
// The same holds for a Token Guard policy edited to unlimited, and for a
// Provider credential whose material is replaced outright.
//
// Every such edit asks for step-up, not only the ones a comparison judges to be
// weakening. A predicate deciding "is the new state at least as strong" is
// itself security-critical, and getting it wrong fails open on exactly the edit
// that matters; it also cannot be swept, because the router cannot see which
// branch a request will take.
func (r *Runtime) requireStepUpMaterial(
	writer http.ResponseWriter, request *http.Request, material stepUpMaterial,
) bool {
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	return r.verifyAdminStepUp(writer, request, admin.session.Username, material.CurrentPassword, material.TOTPCode)
}

// verifyAdminStepUp is verifyReauthenticationMaterial with the two things a
// credential check owes an operator once it guards more than a handful of
// endpoints: a bound on how fast it can be attempted, and a record that it was.
// It is the only entry point; the primitive underneath is unbounded and silent.
func (r *Runtime) verifyAdminStepUp(writer http.ResponseWriter, request *http.Request, username, password, totpCode string) bool {
	// The injectable clock, for the reason the login limiter already documents:
	// the window is a wall-clock minute, so a test that spends its budget
	// without pinning this depends on whether the attempts happen to straddle a
	// minute boundary — which is how it failed roughly once per full-package run
	// while passing every time on its own.
	now := r.clockNow()
	if locked, firstReject := r.stepUpExhausted(username, now); locked {
		// Audited once per window rather than once per attempt, or the audit
		// append becomes the amplifier the limit was added to remove.
		if firstReject {
			r.auditStepUp(username, "throttled", "rate_limited")
		}
		writer.Header().Set("Retry-After", "60")
		writeJSON(writer, http.StatusTooManyRequests, map[string]string{
			"error": "too many failed re-authentication attempts",
			"code":  "reauth_rate_limited",
		})
		return false
	}
	if !r.verifyReauthenticationMaterial(writer, request, username, password, totpCode) {
		r.recordStepUpFailure(username, now)
		r.auditStepUp(username, "failure", "reauthentication_failed")
		return false
	}
	return true
}

// guardAdminCredentialCheck puts the failure budget and the audit record
// around a credential check that keeps its own verification rules.
//
// Several endpoints ask for the account's password without going through
// step-up, and they do so for reasons that are not interchangeable: changing a
// password deliberately does not demand TOTP, deleting an authenticator must
// verify against the *other* authenticators, and disabling MFA accepts a
// recovery code in place of a code. Routing them through verifyAdminStepUp
// would have replaced each of those rules with step-up's, weakening two of
// them and breaking one outright. What they were actually missing is what
// step-up adds around the check rather than inside it — so that is what this
// lends them, leaving verify to decide what "correct" means.
//
// verify must not write to writer. answered reports whether this already sent
// the response (it does so only for a throttled caller, whose 429 is the same
// everywhere); on a plain verification failure the caller still writes the
// wording its own endpoint owes.
func (r *Runtime) guardAdminCredentialCheck(
	writer http.ResponseWriter, username, action string, verify func() bool,
) (ok bool, answered bool) {
	now := r.clockNow()
	if locked, firstReject := r.stepUpExhausted(username, now); locked {
		if firstReject {
			r.auditStepUp(username, "throttled", "rate_limited")
		}
		writer.Header().Set("Retry-After", "60")
		writeJSON(writer, http.StatusTooManyRequests, map[string]string{
			"error": "too many failed credential attempts",
			"code":  "reauth_rate_limited",
		})
		return false, true
	}
	if !verify() {
		r.recordStepUpFailure(username, now)
		r.auditStepUp(username, "failure", action)
		return false, false
	}
	return true, false
}

// stepUpExhausted reports whether this account has already spent its failure
// budget for the current minute, and whether this is the first request refused
// on that basis within the window.
func (r *Runtime) stepUpExhausted(username string, now time.Time) (bool, bool) {
	minute := now.UTC().Truncate(time.Minute)
	r.adminStepUpMu.Lock()
	defer r.adminStepUpMu.Unlock()
	window := r.adminStepUp[username]
	if window.minute != minute || window.attempts < adminStepUpFailuresPerMinute {
		return false, false
	}
	firstReject := !window.rejectAudited
	window.rejectAudited = true
	r.adminStepUp[username] = window
	return true, firstReject
}

func (r *Runtime) recordStepUpFailure(username string, now time.Time) {
	minute := now.UTC().Truncate(time.Minute)
	r.adminStepUpMu.Lock()
	defer r.adminStepUpMu.Unlock()
	window := r.adminStepUp[username]
	if window.minute != minute {
		window = adminLoginWindow{minute: minute}
	}
	window.attempts++
	r.adminStepUp[username] = window
	// Bounded like the login map: an instance can hold many account names over
	// time, and a map that only ever grows is its own denial of service.
	if len(r.adminStepUp) > 4096 {
		for key, value := range r.adminStepUp {
			if value.minute.Before(minute) {
				delete(r.adminStepUp, key)
			}
		}
	}
}

// auditStepUp records a refused re-authentication. A failure here is a signal
// worth keeping — it means someone holding a live session could not prove they
// are the operator it belongs to — but it must never turn into a reason to fail
// the surrounding request differently, so the error is logged and dropped.
func (r *Runtime) auditStepUp(username, outcome, reason string) {
	if err := r.appendAdminAudit(
		"admin_user", username, "admin.reauthentication", "admin_session", "",
		outcome, reason,
	); err != nil {
		r.logger.Error("admin re-authentication audit failed", "error", err)
	}
}

// adminElevationGrant is one session's proven re-authentication, remembered for
// as long as the configured window.
//
// Generation is carried with it because a session identifier alone does not say
// the session is still the one that proved itself: an account whose password
// changes bumps the generation, and the grant made before that must not answer
// for the session after it.
// adminElevationState is the lock and the grants it guards, kept together so
// neither can be reached without the other.
type adminElevationState struct {
	mu     sync.Mutex
	grants map[[32]byte]adminElevationGrant
}

type adminElevationGrant struct {
	expiresAt  time.Time
	generation uint64
}

// requireDetectionStepUp is requireStepUpMaterial with the answer remembered
// for the configured window.
//
// Capability detection is guarded because it spends the Provider credential
// outside project accounting and writes capability evidence a Deployment
// adopts. Neither becomes untrue on the second detection, so the guard is not
// dropped — it is amortised. An operator configuring six Deployments proves
// themselves once instead of typing a password and a TOTP code six times, and a
// session that has not proved itself inside the window still proves itself now.
//
// Only this endpoint reads the window. Deletes and the edits that weaken a
// protection in force keep asking every time, because those are the actions
// whose damage is done the moment the request lands, where detection's is
// bounded by MaxProviderCalls and visible in the audit trail either way.
func (r *Runtime) requireDetectionStepUp(
	writer http.ResponseWriter, request *http.Request, material stepUpMaterial,
) bool {
	admin := request.Context().Value(adminContextKey{}).(adminRequestContext)
	if r.detectionElevated(admin.session, r.clockNow()) {
		return true
	}
	if !r.requireStepUpMaterial(writer, request, material) {
		return false
	}
	r.elevateDetection(admin.session, r.clockNow())
	return true
}

// detectionElevated reports whether this exact session proved itself recently
// enough. Every uncertainty answers false: an absent grant, an expired one, one
// belonging to a superseded generation, and a window configured to zero.
func (r *Runtime) detectionElevated(session domain.AdminSession, now time.Time) bool {
	window := r.detectionElevationWindow()
	if window <= 0 {
		return false
	}
	r.adminElevation.mu.Lock()
	defer r.adminElevation.mu.Unlock()
	grant, ok := r.adminElevation.grants[session.IDHash]
	if !ok || grant.generation != session.Generation || !now.Before(grant.expiresAt) {
		return false
	}
	return true
}

// elevateDetection records a proven re-authentication. The window runs from the
// proof rather than from the last use, so a long sitting asks again instead of
// holding itself open.
func (r *Runtime) elevateDetection(session domain.AdminSession, now time.Time) {
	window := r.detectionElevationWindow()
	if window <= 0 {
		return
	}
	r.adminElevation.mu.Lock()
	defer r.adminElevation.mu.Unlock()
	// Swept here rather than on a timer: the map is only ever written on a
	// successful step-up, so this runs rarely and keeps the map proportional to
	// the sessions actually elevating.
	for id, grant := range r.adminElevation.grants {
		if !now.Before(grant.expiresAt) {
			delete(r.adminElevation.grants, id)
		}
	}
	r.adminElevation.grants[session.IDHash] = adminElevationGrant{
		expiresAt:  now.Add(window),
		generation: session.Generation,
	}
}

// clearDetectionElevation drops a session's grant. Signing out ends the
// elevation with the session that earned it rather than leaving it to expire.
func (r *Runtime) clearDetectionElevation(session domain.AdminSession) {
	r.adminElevation.mu.Lock()
	defer r.adminElevation.mu.Unlock()
	delete(r.adminElevation.grants, session.IDHash)
}

func (r *Runtime) detectionElevationWindow() time.Duration {
	window := r.config.Admin.ModelCapabilityDetection.ElevationWindow
	if window == nil {
		return 0
	}
	return window.Value()
}
