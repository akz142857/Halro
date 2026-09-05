# Frontend / embedded UI independent review

Baseline: `381743f6613607dc256828f4776b52af8bdd232c` (HEAD verified). Date: 2026-09-05. Reviewer: independent frontend role. Owner for proposed remediation: console/frontend maintainer; API contracts require coordination with the Admin API owner.

## Result and evidence boundary

Five bounded defects are documented below, all P2. No P0/P1 or authorization bypass is established by this review. Findings distinguish observed client behavior from server consequences inferred by tracing the actual handler. These are reviewer confirmations, not independent adversarial adjudication or completed fixes.

Read `AGENTS.md` and the approved plan. No production code, repository tests, Git state, or business data was changed. Only this report and isolated `/private/tmp/halro-frontend-review-260905/` reproduction material were written. No other reviewers' reports were read. No provider calls, browser session, full test/build gate, SDK run, or soak was performed by this role.

Parent reported frontend typecheck/test/build passed during this review. That is shared evidence, not a rerun or independently inspected log. Parent also reported initial Go cache/socket sandbox failures and an escalated rerun; this role does not classify those environmental failures as product defects or claim that rerun passed. Browser acceptance and embedded-bundle rebuild/drift evidence remain parent-owned.

## Architecture and coverage

Actual client flow: `main.tsx` creates one QueryClient → `App.tsx` loads bootstrap/setup/session and accounting context → `Layout.tsx` plus lazy page modules → `api.ts` sends same-origin, no-store requests to `/admin/api/v1`. CSRF is held in module memory and attached to writes; resource updates carry revisions/ETags. Authorization remains server-side. Developer execution uses a separate streaming response path; changing the sample Gateway URL does not redirect actual execution to an arbitrary host.

`internal/webui/webui.go` embeds `dist`; index responses are no-store, static files immutable with ETags, extensionless routes receive the SPA, and API/missing asset paths do not fall back to HTML. `web/vite.config.ts` generates the committed bundle and splits page/vendor chunks. The compressed output was not manually reviewed or rebuilt. `web/scripts/check-artifacts.mjs` scans known secret canaries, storage markers and source maps; this is not proof that arbitrary secrets cannot enter a bundle.

| Scope | Reading / checks | Status and limits |
|---|---|---|
| App, main, Layout, session, navigation, Login, Setup, api | Entry guards, MFA route, request headers, error transport, logout clearing, query keys, link consumers | Source review; navigation reproduced. Expiry/cross-account browser behavior remains open. |
| ProjectsPage | List/detail identity, keys, create/retry, project form round trips, route and policy pickers, disable/delete confirmations | Source review of mutation and form paths; pagination and byte-rounding findings. |
| ProvidersPage and provider-profile hook | Credentials/list consumers, credential binding/rotation, profile ceiling, create idempotency, URL compatibility, tab navigation | Source review; no real enumeration/credential rotation performed. |
| DeploymentsPage and deploymentCondition | List dependencies, price query scheduling/batching, price form, capability selection/resolution/detection correlation and cancellation, identity lock, manual declaration | High-risk state/mutation paths reviewed; full 2,696-line presentation and every profile combination not independently exercised. |
| RoutesPage | Alias grouping, effective deployment dependencies, revision writes, probe presentation, create form | Source review; server routing/probe execution delegated. |
| PoliciesPage / RedactionPoliciesSection | Paged queries, separate cache shapes, step-up writes, policy body preservation, rule editing and error focus | Mutation/form paths reviewed; regex semantics and enforcement belong to core reviewers. |
| DeveloperPage | JSON/form request generation, execution/cancel, response limit and decoding, Usage correlation, sample URL, plaintext key handling and retry | Source review; debug-key retry reproduced with mocks. No billable execution. |
| UsagePage / UsageSummaryPanel / UsageFailuresPanel / FailureDetailDrawer | Filter keys, summary drill-down, historical identifiers, lazy payload reveal and cache eviction, tab state | Source review; actual summary-link click reproduced in jsdom. |
| Dashboard / FirstRunChecklist / OnboardingContext | Status sources, onboarding actions, refresh timing and context links | Focused source review; no first-run browser completion claim. |
| Settings and extracted forms | Personal preferences, instance settings, timezone preview, retention acknowledgement, password/MFA/admin-user controls, custody links | Source review of relevant state/permission paths; real MFA, restore and timezone rollover deferred. |
| Shared components | Modal focus/stacking/close, ConfirmButton step-up, error rendering, field labels, paging, tabs; combobox/menus via existing tests | Pending destructive close reproduced. Assistive-technology behavior and complete combobox interaction not independently exercised. |
| i18n, theme, format, timezone, TrendChart, notifications | Locale loading, accounting-zone formatting, in-memory preferences, chart cleanup/token use, bounded notifications | Focused source review plus test mapping. Full translation copy, CSS contrast/responsive appearance, every trend aggregation branch not exhaustively reviewed. |
| Embedded UI and build | Go handler, handler tests, Vite config, artifact scan | Source review only; parent owns execution and drift check. |

The coverage table describes actual depth, not blanket line-by-line certification of every file under `web/src`. Generated output, CSS/layout, some utility branches and the full Admin server authorization matrix retain explicit limitations.

## Findings

### FE-01 — Same-page Usage drill-down updates the URL but not the view

- **Type / severity / confidence:** confirmed client BUG; P2; high (source + DOM reproduction). INV-10.
- **Location:** `web/src/navigation.tsx:32-48`, `web/src/pages/UsagePage.tsx:38`, `:60-83`, `:115-119`; callers at `web/src/pages/UsageSummaryPanel.tsx:152` and `:294`. `App.tsx` keys the page boundary by pathname, not the full location.
- **Reachable trigger:** open `/admin/usage`, wait for Summary, click “查看最终失败” / View failed requests; alternatively group the summary and click View attempts for a row.
- **Expected:** switch to the requested tab, apply the linked identifiers and exact interval, fetch the corresponding records.
- **Actual:** `navigate()` changes the search string and emits `halro:navigate`, but `usePathname()` sets the unchanged pathname. Usage listens only to `popstate` for tab synchronization, and filters initialize only on mount. The reproduced failure link changed the URL to `tab=failures` while Summary remained selected and `usageFailures` was never called. Manually switching to Attempts after a grouped link still leaves the already-mounted attempt filters at their original values.
- **Existing defenses / bounds:** initial deep links on a fresh mount do work; ordinary tab buttons set local state; `popstate` updates the tab but not all filter state. Reload/open in a new tab is a workaround. No wrong server authorization or recorded billing is demonstrated.
- **Minimal direction:** subscribe to pathname plus search, synchronize tab and filters on navigation (including browser history), and deliberately handle remount versus preserving drafts. Merely changing `navigate()` URL equality is insufficient.
- **Regression:** click the real summary link inside an already-mounted App/Usage view; assert selected tab, API filter arguments and rendered records, then Back/Forward. Existing `navigation.test.ts` “treats the query as part of the destination” checks the address bar only; summary tests check hrefs, and Usage tests primarily mount directly at the destination.

### FE-02 — First-page-only consumers hide valid credentials, projects and policies

- **Type / severity / confidence:** confirmed client/API contract BUG; P2; high (source and API-client reproduction). INV-10.
- **Location:** `web/src/api.ts:292`, `:340-341`, `:499-500`, `:524-525`; `web/src/pages/ProvidersPage.tsx:139-159`, `:668-681`; `web/src/pages/DeveloperPage.tsx:58-64`; `web/src/pages/ProjectsPage.tsx:408-414`, `:620-621`; `UsagePage.tsx:43` and `UsageFailuresPanel.tsx:29`.
- **Server evidence:** `internal/app/admin_resources.go:50-63`, `:95-111`, `:246` and `:350-399`; `admin_redaction.go:45-70`. The common page writer defaults to 50 and supplies `next_cursor` when more records exist. These APIs are paginated, not complete catalogs.
- **Reachable trigger:** an instance with 51+ credentials/projects/policies, with the needed record after the first page. Search for that credential in Providers; select that project in Developer; or attach that enabled policy in the Project editor.
- **Actual:** these consumers read only `items` from the first response. The credential screen has no continuation fetch; the workbench and project policy pickers cannot offer later records. Usage also lacks later project choices/names. A limited prefix is presented as the available set.
- **Existing defenses / bounds:** providers, deployments and routes use `pageOfAll/listAll` with cursor-repeat and page-ceiling rejection. Projects, policy-management pages, keys and alerts have separate infinite-query implementations. Those correctly paged screens do not repair these independent plain-list consumers. API administration remains a workaround. This finding does **not** assert that merely saving a project silently clears a missing policy binding; that additional behavior was not reproduced.
- **Minimal direction:** reuse complete-list APIs for finite pickers, or give consumers explicit paged search/loading/error handling. Preserve currently selected but absent/disabled records with an explanation.
- **Regression:** return page one plus cursor, put the desired item on page two, and assert actual credential search/rotation, workbench project selection and both policy selectors reach it. Include continuation error and repeated cursor cases. Existing `api.test.ts` covers complete routes/providers/deployments; `ProjectsPage.test.tsx:264` covers the project management list, not these consumers.

### FE-03 — Debug-key retries mint a new identity after an ambiguous response

- **Type / severity / confidence:** confirmed client idempotency BUG; P2; high for changed request identity, high from source for duplicate-record consequence. INV-10; credential lifecycle relevance to INV-04, but no isolation bypass established.
- **Location:** `web/src/pages/DeveloperPage.tsx:202-210`, especially `crypto.randomUUID()` at line 208. Consumer at `:408-415`.
- **Reachable trigger:** authenticate the debug-key creation, let the server commit, lose the HTTP response, then retry the same confirmation. A fresh TOTP may be needed when MFA is enabled; MFA-optional password-only administration is sufficient to reach the path.
- **Expected:** preserve the logical operation's idempotency token and receive the existing-key replay response so the operator can revoke/reissue the key whose plaintext was lost.
- **Actual:** every mutation execution creates a different token (also a new timestamp/expiry). The temporary test simulated the lost response and observed distinct tokens for the authenticated call and its retry.
- **Server evidence / existing defenses:** `internal/app/admin_projects.go:234-285` verifies step-up on every mint and derives key ID from actor, project and idempotency token; storage collision returns `gateway_key_idempotency_replay`. A new token avoids that collision. Projects' regular CreateKey dialog uses a stable ref (`ProjectsPage.tsx:653`) and is not the affected path. Debug keys expire after 24 hours and appear in key management; there is no claim an attacker learns the lost plaintext.
- **Impact:** an ambiguous retry creates another valid credential record instead of reconciling the first, leaving an unacknowledged credential and defeating the explicit client/server duplicate-prevention contract. Repeated retries can repeat the effect. Step-up and expiration bound the risk, so this is not classified as P1 credential compromise.
- **Minimal direction:** pin operation identity and payload for a confirmation attempt; reset only for an explicit new mint. Preserve the idempotency-replay recovery link.
- **Regression:** commit-then-disconnect against an isolated local runtime; retry with the same logical action; assert one stored key and a replay/recovery result. `DeveloperPage.test.tsx:433` checks successful mint, masking, expiry and no storage, but never asserts token stability. `ProjectsPage.test.tsx:400` already protects the regular key form.

### FE-04 — Destructive confirmation can close while the operation is pending

- **Type / severity / confidence:** confirmed interaction BUG; P2; high (source + DOM reproduction). INV-10.
- **Location:** `web/src/components.tsx:560-571`, `:588-595`; `Modal` defaults `closeDisabled=false` at `:379` and honors Escape at `:448-449`. A concrete consumer is project disable at `web/src/pages/ProjectsPage.tsx:259-267`.
- **Reachable trigger:** start a slow project-disable confirmation, then press Escape or click the dialog's × before the request resolves. This path requires no step-up text, so no dirty-discard prompt intervenes.
- **Actual:** confirmation disappears while the request remains live. The pending Cancel/action buttons are disabled, but `ConfirmButton` never passes `closeDisabled={pending}` to Modal. Closing does not abort the request. The temporary unresolved-promise test demonstrated dismissal before completion. With password text present, the dirty-discard prompt is an extra step, not a pending-operation lock.
- **Existing defenses / bounds:** page mutation state and API revision/step-up checks still apply. Several pages refresh and render their own outcomes, so this is not a claim that every result is lost or that every action executes twice. The actionable defect is contradictory pending/cancel semantics and loss of the confirmation's own error/retry surface.
- **Minimal direction:** keep the modal non-dismissible while pending, or explicitly support background execution with a durable outcome surface and clear wording. Guard submit paths against pending re-entry as well.
- **Regression:** hold the API promise, try Escape and ×, verify modal remains; reject and verify readable/retryable error, then succeed and verify closure. `components.confirmbutton.test.tsx:96` checks closure on success, but does not attempt dismissal while pending. Generic accessibility tests explicitly verify ordinary Escape closing, which does not establish this exception.

### FE-05 — Saving a project rounds its byte ceiling, even when untouched

- **Type / severity / confidence:** confirmed form serialization BUG; P2; high (source + DOM submission reproduction). INV-10 and resource-boundary relevance to INV-08.
- **Location:** `web/src/pages/ProjectsPage.tsx:485` initializes integer KB via `Math.round(bytes / 1024)`; `:515` always writes that integer multiplied by 1024. Schema at `:52` forces integer KB.
- **Reachable trigger:** create/configure a project through the Admin API with `max_request_bytes=500`, then open Edit in the console and save without changing the ceiling (including an unrelated name edit).
- **Expected:** preserve 500 bytes unless the operator changes the limit.
- **Actual:** the form initializes 0 KB and submits `max_request_bytes=0`, removing the project's explicit ceiling and falling back to the instance limit. Other nonmultiples round up or down (e.g. 1500 → 1024). This is a persisted change, not merely display rounding.
- **Existing defenses / bounds:** `internal/app/admin_projects.go:428` copies the submitted byte value; `internal/domain/models.go:341-354` rejects negative values but does not require KiB multiples. ETag guards concurrent updates, not unintended field conversion. The instance request limit still applies. The enable/disable helper preserves exact bytes and is unaffected.
- **Minimal direction:** retain exact bytes when the field is untouched, or support exact byte/fractional-KiB editing. Do not silently map a small positive ceiling to the special zero value.
- **Regression:** open/save projects at 0, 500, 1024, 1500 and 1048576 bytes; assert byte-for-byte preservation of untouched values and correct deliberate edits. Existing project tests exercise deferred-setting preservation and form validation but do not pin this non-KiB round trip.

## Narrow execution evidence

All reproduction code and logs are in `/private/tmp/halro-frontend-review-260905/`. They contain only synthetic fixtures. `review.test.tsx` imports the baseline production components; `vitest.config.mts` uses the repository setup and jsdom, with its include restricted to this temporary test. A temporary `node_modules` symlink points to the existing web dependencies. No repository test was added or edited.

Working directory: `/Users/ziy/Code/ClayCosmos/Halro/web`. Toolchain: Node `v24.18.0`, npm `11.16.0`, Vitest `4.1.11`; Node/npm version commands exited 0. Final HEAD remained the baseline SHA. `git diff --name-only -- web internal/webui` exited 0 with no changed paths. Report whitespace/completion check passed.

```sh
./node_modules/.bin/vitest run --config /private/tmp/halro-frontend-review-260905/vitest.config.mts > /private/tmp/halro-frontend-review-260905/results-all-final.log 2>&1
```

The tests deliberately assert the faulty baseline behavior: a passing reproduction is evidence of the defect, not a passing regression after a fix.

| Attempt / log | Exit | Interpretation |
|---|---:|---|
| `initial-harness-failure.log` | 1 | Reviewer test selector matched two selected tab widgets; the pending-close reproduction passed. Harness defect, not product/environment failure. |
| `results.log` | 0 | Corrected selector: 2/2 reproductions pass (1.91 s). |
| `expanded-harness-failure.log` | 1 | Reviewer used the wrong step-up error code and reused a consumed mock Response. Corrected fixtures; not product failures. |
| `results-final.log` | 0 | 4/4 reproductions pass (1.29 s): FE-01 link/view, FE-02 API pagination, FE-03 retry token, FE-04 pending close. |
| `project-harness-failure.log` | 1 | Added project test initially used the wrong translated Save label; previous four pass. Harness defect. |
| `results-all-final.log` | 0 | 5/5 reproductions pass (1.84 s), including FE-05 open/save emitting 0 for the original 500-byte limit. |

No environment escalation was needed for these jsdom runs. Source-reading commands returned success except exploratory searches for nonexistent `admin_keys.go` / `internal/domain/project*`; these were file-discovery misses, not validation results.

## Tests mapped to invariants and browser handoff

| Invariant / journey | Existing tests inspected or located (parent gate owns results) | Additional exact check |
|---|---|---|
| INV-10 navigation and usage diagnosis | `navigation.test.ts`, `UsageSummaryPanel.test.tsx`, `UsagePage.test.tsx`, `UsageFailuresPanel.test.tsx` | Open Summary; click failure count and a grouped row; inspect selected tab, interval/ID API query; Back/Forward and sidebar Usage must agree with URL. FE-01. |
| INV-10 resource completeness | `api.test.ts`, Projects/Policies/Operations paging tests | Local fixtures with 51+ credentials/projects/policies; reach the last record through every picker, not only management pages. FE-02. |
| INV-04 presentation + INV-10 key lifecycle | `pages/readOnlyRole.test.tsx`, `components.confirmbutton.test.tsx`, `ProjectsPage.test.tsx`, `DeveloperPage.test.tsx` | Administrator/read-only accounts; create debug key with commit-then-response-loss; retry and inspect key count. Confirm current password/TOTP behavior; no real provider required. FE-03/04. |
| INV-08/10 limits | `ProjectsPage.test.tsx`, server project validation tests | API-created exact byte ceiling; console rename/save; GET project and verify unchanged bytes. FE-05. |
| INV-05 evidence presentation | `DeploymentsPage.test.tsx` unknown target, manual declaration, enumeration refresh, late/reused detection responses; `hooks/useProviderProfiles.test.ts` | Use a local controlled provider; refresh then change provider/model during a delayed response; verify no stale capability adoption. Preserve unknown/declared/detected distinctions. No real paid verification. |
| INV-10 price/time | `DeploymentsPage.test.tsx` price scheduling/cache-read rates, `AccountingTimezoneForm.test.tsx`, `format.timezone.test.ts`, `timezone.test.ts`, `TrendChart.timezone.test.tsx` | Browser zone different from accounting zone; cold deep link with absolute interval and delayed `/system/status`; scheduled price boundary; verify display and submitted instants match. This review did not validate the late-time-context cold-load case. |
| INV-07/10 captured payload | `UsageFailuresPanel.test.tsx`, drawer source | Open failure, verify no payload call before reveal; reveal, close, reopen; deny read / expire session / return 500 and distinguish permission/transport failure from absent capture. Current drawer maps all payload errors to no-payload wording; error taxonomy remains a test gap. |
| INV-10 first-run and session | `App.test.tsx`, `Setup.test.tsx`, `Login.test.tsx`, `SettingsPage.test.tsx`, `MasterKeyCustodyPage.test.tsx` | Setup race → login → required MFA enrollment/recovery acknowledgement → provider/deployment/route/project/key → local call → Usage → revoke → denied call. Expire/revoke session on an already-open page, attempt mutation, then re-login as read-only. Verify stale client caches/secrets do not survive account switch. |
| Accessibility/i18n | `accessibility.test.tsx`, combobox/confirm/notification tests, `i18n/i18n.test.tsx`, `design-system.test.ts` | Keyboard-only at 200% zoom and narrow width, both languages/themes; nested price drawer focus/escape; dirty form Cancel/×; one-time secret/recovery close guard; long localized errors and screen-reader announcements. Parent owns real browser evidence. |
| INV-10 embedded shell | `internal/webui/webui_test.go` three handler tests | Built binary serves fresh index and immutable hashed assets; direct nested URL; missing JS must 404; stale deployment tab/chunk failure and recovery; parent verifies build drift. |

## Defenses, disputed candidates and remaining limitations

- **Not a defect:** Developer's selected project does not itself determine billing. The visible `developer.projectFilterHint` explicitly says Gateway Key determines the charged project; the actual key is the authorization input. Selecting a different project while keeping a key is not, by itself, cross-project authorization bypass.
- **Not a defect:** read-only users may edit their own appearance/language/password/MFA; do not equate personal settings with instance mutation. Server authorization is the controlling defense. A missing disabled UI control alone would be presentation inconsistency, not a privilege escalation.
- **Current defenses, not historical certification:** pagination guards for routes/providers/deployments; distinct plain/paged query keys; stable idempotency refs in regular create forms; selection revision checks for detections; explicit price and retention acknowledgements; lazy failure-payload reveal with `gcTime: 0`; memory-only secrets and logout `queryClient.clear()` are present in the source inspected. Comments describing earlier fixes were not used as proof that complete journeys now work.
- **Session recovery needs browser evidence:** `api.request` throws API errors without globally refreshing session; App refresh behavior and its 60-second session staleness are separate from a page's 401. No global 401 handler should indiscriminately log out on `recent_reauth_required`, which is also a 401. Test that distinction before any proposed fix.
- **Architecture proposal, not defect:** centralize full-location subscription and typed query key/list semantics so resource pages do not independently implement divergent navigation and pagination contracts. No new router/library is required to close the specific bugs.
- Deferred Responses/Files/Batches cancellation/deletion is not exposed as a full management UI here. Developer offers Responses/Chat/Embeddings execution and transport cancellation. Do not label missing management pages an implementation bug without a product commitment; API lifecycle acceptance remains parent/core scope.
- Full CSS contrast, tablet layout, assistive-technology behavior, all i18n prose, live role/session transition, real MFA, server-side two-account isolation, complete provider/profile matrix, timezone rollover, real KMS/restore, cold-load chunk failures, and production operation are **not independently verified** by this report. No unconditional release recommendation follows from this role's source review or the shared frontend gate.
