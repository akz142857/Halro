package app

import (
	"net/http"
	"time"
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
	password := []byte(input.CurrentPassword)
	defer clear(password)
	input.CurrentPassword = ""
	return r.verifyAdminStepUp(writer, request, admin.session.Username, string(password), input.TOTPCode)
}

// verifyAdminStepUp is verifyReauthenticationMaterial with the two things a
// credential check owes an operator once it guards more than a handful of
// endpoints: a bound on how fast it can be attempted, and a record that it was.
// It is the only entry point; the primitive underneath is unbounded and silent.
func (r *Runtime) verifyAdminStepUp(writer http.ResponseWriter, request *http.Request, username, password, totpCode string) bool {
	now := time.Now()
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
	now := time.Now()
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
