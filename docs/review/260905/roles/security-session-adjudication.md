# Independent adjudication: Admin session refresh versus logout

Baseline: `381743f6613607dc256828f4776b52af8bdd232c`; 2026-09-05. Review only. The user explicitly authorized reading `roles/security.md` for SEC-02. No other reviewer reports were read for this adjudication. No production code, repository tests, Git state or user data changed. Fixtures use isolated temporary data and a Go overlay.

## Verdict

**SEC-02 confirmed independently: P1, high confidence.** A completed ordinary logout can be undone by an already-started idle refresh. The defect restores acceptance of the same cookie on a **new** protected request; it is more than permitting an already-authorized request to finish. P1 is justified by failure of the explicit session-revocation security boundary and continued administrative access for a holder of the old session token. This is not anonymous authentication bypass or privilege escalation. Exploitation requires a valid copied/retained token, a refresh-due request overlapping logout, and the particular write ordering. No claim is made about practical attack success rate.

## Source and concurrency analysis

- `internal/adminauth/session.go:103-115`: lookup, expiry check, and user-generation comparison occur before refresh. The session and user are separate store reads.
- `internal/adminauth/session.go:117-126`: refresh is due after `min(idleTimeout/4, 1 minute)`; it updates the previously read value through `PutAdminSession`. Default idle timeout is 30 minutes, so the normal interval is one minute. Idle expiry is capped at the original absolute expiry.
- `internal/store/bolt/store_admin.go:182-195`: validation precedes a bbolt write transaction whose operation is unconditional `Bucket.Put`. There is no existence check, generation comparison, or conditional version check inside that transaction. Creation and refresh use the same upsert API.
- `internal/store/bolt/store_admin.go:198-222`: lookup uses its own View; revocation uses a later independent Update/Delete. bbolt serialization protects each transaction but does not make lookup–validation–refresh atomic with deletion.
- `internal/domain/admin.go:85-105`: session state has identity, generation and timestamps; there is **no revoked flag or tombstone** consulted by lookup or update. Delete removes the only per-session revocation evidence, allowing a stale upsert to recreate the row.
- `internal/adminauth/session.go:147-151` and `internal/app/admin_session.go:167-185`: logout deletes the session, clears step-up elevation, audits and expires the browser cookie. It does not advance user generation.
- `internal/app/admin_session.go:272-297`: middleware calls Authenticate for each request and reloads the current role. Runtime routes expose session GET and ordinary logout (`internal/app/runtime.go:1575-1576`), with protected Admin user listing at line 1586. Concurrent HTTP requests can therefore reach the interleaving without direct store access.

Reachable sequence: A reads an active refresh-due session and matching user generation; A pauses before refresh Put. B authenticates and completes real POST logout, deleting the session. A resumes its upsert and recreates the row. C sends the old cookie and authenticates successfully. B's own authentication refresh does not prevent this sequence: the fixture lets B perform that refresh and its subsequent deletion before releasing A.

## Deterministic local evidence and invariants

A newly written overlay fixture wraps the **real bbolt store**, gating exactly one `PutAdminSession` after Manager authentication checks. It uses the existing isolated app runtime/bootstrap helper and real Admin router with httptest request/response recorders. The manager creates a valid session with creation time two minutes earlier to make refresh due; no persisted production records are edited. The real logout request supplies the normal cookie, CSRF token and origin. Channel synchronization controls ordering without timing sleeps.

| Invariant / control | Observed result |
| --- | --- |
| INV-04: successful logout permanently rejects the revoked cookie | **Fails**: actual logout HTTP 200 and expired response cookie; store lookup confirms row absent; delayed refresh recreates row |
| Distinguish in-flight authorization from later access | In-flight GET returns 200; then a fresh Manager accepts the token and a new protected `/admin/api/v1/admin-users` GET returns 200 |
| User generation change still invalidates stale rows | Control increments persisted user generation then deletes its sessions; delayed upsert recreates an old-generation row, but fresh authentication rejects it and protected GET returns 401 |
| Absolute expiration remains enforced | Authentication exactly at original absolute expiry rejects; next protected GET returns 401 |
| Go shared-memory safety | Narrow test passes under race detector; the defect is transaction ordering, not a detected Go data race |

Command (stdout/stderr redirected directly; process exit captured independently):

```sh
go test -race -count=1 -overlay /private/tmp/halro-reliability-review/session-adjudication-overlay.json ./internal/app -run '^TestIndependentSessionRevocationAdjudication$' -v > /private/tmp/halro-reliability-review/session-adjudication.log 2>&1
```

**Exit 0**, package time 5.070s; fixture 2.93s (logout 1.43s, generation control 1.50s). Escalated execution was used because the parent had already identified cache/loopback sandbox restrictions. This specific command had no environmental failure. No full suite, provider calls, network listener, load test or benchmark was run. An initial source-inspection command used the wrong store directory; corrected to `internal/store/bolt`; this was not a test failure.

## Existing defenses and severity boundaries

Cookie Secure/HttpOnly/SameSite protections and CSRF/origin checks reduce token acquisition and cross-site mutation risks but cannot erase a copied cookie. Ordinary browser logout clearing the cookie usually leaves that browser without the credential; this does not establish server-side revocation. Current roles remain enforced, and logout clears the existing step-up elevation; the fixture does not demonstrate restoration of elevation or bypass of sensitive-action challenges.

Password change advances generation before deleting sessions (`internal/app/admin_session.go:225-231`). The store also advances generation for MFA identity/reset paths (`internal/store/bolt/store_admin.go:709,1005`). The control independently verifies the generation mechanism, not the entire password/MFA HTTP flow. A stale session row cannot bypass the next lookup's generation check. Missing/deleted users also fail authentication. Access remains bounded by the original absolute expiry (default eight hours); refresh cannot extend it indefinitely. A request whose initial lookup happens after deletion, with no stale writer recreating the row, fails normally.

## Proposed correction and regression criteria

Proposal only: separate session creation from conditional refresh. In a single write transaction refresh should require the existing live row, validate its identity/generation/expiry, and update it without inserting missing rows. Optionally check the user's current generation in that transaction as well. Merely adding a second pre-write Get recreates the same race; a process-local lock is insufficient unless every relevant writer/revoker shares it. A tombstone can work only if refresh cannot overwrite it and lookup enforces it. Preserve monotonic LastSeen/idle expiration under competing refreshes.

A regression should run both orderings: refresh before logout leaves the row deleted; logout before refresh leaves it absent and all subsequent authentications rejected. Include competing refreshes, generation changes, exact expiration and fresh Manager access. An in-flight handler finishing is a separate policy question; it must not justify restoring future authorization.

## Coverage and limitations

Read Manager create/authenticate/revoke, session domain fields/validation, store lookup/upsert/delete and generation-changing paths, HTTP middleware/logout/password flow and route registration. Independently tested actual logout, durable row deletion/reinsertion, subsequent protected access, fresh Manager behavior, generation and absolute-expiry defenses. The fixture deliberately widens a source-reachable scheduling window; it does not measure its natural frequency or demonstrate token theft. No production traffic, real browser exploit, complete MFA/password flow or broad security-report re-review was performed. Other SEC findings are outside this adjudication.

## Reproduction fixture

Save the following as `/private/tmp/halro-reliability-review/session_adjudication_test.go`. Overlay maps the absent repository test path `internal/app/session_adjudication_test.go` to this file; no repository test creation is needed. The fixture below was gofmt-formatted after the successful run; only formatting changed.

```json
{"Replace":{"/Users/ziy/Code/ClayCosmos/Halro/internal/app/session_adjudication_test.go":"/private/tmp/halro-reliability-review/session_adjudication_test.go"}}
```

```go
package app

import (
	"context"
	"errors"
	"github.com/akz142857/Halro/internal/adminauth"
	"github.com/akz142857/Halro/internal/domain"
	boltstore "github.com/akz142857/Halro/internal/store/bolt"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

type adjudicationStore struct {
	*boltstore.Store
	armed           atomic.Bool
	entered, resume chan struct{}
}

func (s *adjudicationStore) PutAdminSession(ctx context.Context, v domain.AdminSession) error {
	if s.armed.CompareAndSwap(true, false) {
		close(s.entered)
		select {
		case <-s.resume:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.Store.PutAdminSession(ctx, v)
}
func TestIndependentSessionRevocationAdjudication(t *testing.T) {
	for _, mode := range []string{"logout", "generation_change"} {
		t.Run(mode, func(t *testing.T) {
			r, login := stepUpTestRuntime(t)
			ctx := context.Background()
			user, e := r.store.GetAdminUser(ctx, "admin")
			if e != nil {
				t.Fatal(e)
			}
			gate := &adjudicationStore{Store: r.store, entered: make(chan struct{}), resume: make(chan struct{})}
			var release sync.Once
			unblock := func() { release.Do(func() { close(gate.resume) }) }
			defer unblock()
			manager, e := adminauth.NewManager(gate, make([]byte, 32), 8*time.Hour, 30*time.Minute)
			if e != nil {
				t.Fatal(e)
			}
			r.adminSessions.Close()
			r.adminSessions = manager
			created, e := manager.Create(ctx, user, time.Now().Add(-2*time.Minute))
			if e != nil {
				t.Fatal(e)
			}
			login.cookie.Value = created.Token
			login.csrf = created.CSRFToken
			router := r.adminRouter()
			get := func(path string) *httptest.ResponseRecorder {
				req := adminRequest(t, http.MethodGet, path, nil)
				req.AddCookie(login.cookie)
				res := httptest.NewRecorder()
				router.ServeHTTP(res, req)
				return res
			}
			gate.armed.Store(true)
			done := make(chan int, 1)
			go func() { done <- get("/admin/api/v1/session").Code }()
			select {
			case <-gate.entered:
			case <-time.After(5 * time.Second):
				t.Fatal("refresh not reached")
			}
			if mode == "logout" {
				req := adminMutationRequest(t, http.MethodPost, "/admin/api/v1/session/logout", login, nil)
				res := httptest.NewRecorder()
				router.ServeHTTP(res, req)
				if res.Code != 200 {
					t.Fatalf("logout=%d %s", res.Code, res.Body.String())
				}
				cookies := res.Result().Cookies()
				if len(cookies) != 1 || cookies[0].MaxAge >= 0 {
					t.Fatal("logout did not expire cookie")
				}
				t.Log("real logout HTTP 200 with expired cookie")
			} else {
				user.SessionGeneration++
				if _, e = r.store.PutAdminUser(ctx, user, user.Revision); e != nil {
					t.Fatal(e)
				}
				if e = r.store.DeleteAdminSessionsForUser(ctx, user.Username); e != nil {
					t.Fatal(e)
				}
			}
			if _, e = r.store.GetAdminSession(ctx, created.Session.IDHash); !errors.Is(e, boltstore.ErrNotFound) {
				t.Fatalf("row not absent after revocation: %v", e)
			}
			t.Log("session row absent before delayed refresh resumes")
			unblock()
			select {
			case code := <-done:
				t.Logf("already-in-flight request=%d", code)
			case <-time.After(5 * time.Second):
				t.Fatal("refresh stuck")
			}
			if _, e = r.store.GetAdminSession(ctx, created.Session.IDHash); e != nil {
				t.Fatalf("expected delayed upsert to reinsert: %v", e)
			}
			fresh, e := adminauth.NewManager(r.store, make([]byte, 32), 8*time.Hour, 30*time.Minute)
			if e != nil {
				t.Fatal(e)
			}
			defer fresh.Close()
			_, freshErr := fresh.Authenticate(ctx, created.Token, time.Now())
			res := get("/admin/api/v1/admin-users")
			t.Logf("new manager auth=%v; new protected HTTP GET=%d", freshErr, res.Code)
			if mode == "logout" {
				if freshErr != nil || res.Code != 200 {
					t.Fatal("resurrection not reproduced")
				}
				if _, e = fresh.Authenticate(ctx, created.Token, created.Session.AbsoluteExpiresAt); !errors.Is(e, adminauth.ErrInvalidSession) {
					t.Fatal("absolute expiry bypassed")
				}
				if get("/admin/api/v1/admin-users").Code != 401 {
					t.Fatal("expiry failed")
				}
			} else if !errors.Is(freshErr, adminauth.ErrInvalidSession) || res.Code != 401 {
				t.Fatal("generation defense bypassed")
			}
		})
	}
}
```

Evidence SHA-256:

- `session_adjudication_test.go`: `2edd11b6b3785e63639d7e3f44e651a04f6440b8d2841337dcafbd9cfe849cdf`
- `session-adjudication-overlay.json`: `627b9d453a638991389a4a03c85526093ba90950d3a074286cc3874027b061e0`
- `session-adjudication.log`: `af095053a303a5fc67408cac87401ee12f084209fe51f2e3c632f75caea1a549`
