import { ApiError } from "../api";
import type { TimeContext, WritePathSummary } from "../types";

/**
 * A server-supplied accounting day. Defaults to Asia/Shanghai so tests run
 * against a zone that is not the CI host's and not UTC — a fixture in UTC would
 * pass whether or not the code respects the zone it is given.
 */
export function timeContext(overrides: Partial<TimeContext> = {}): TimeContext {
  return {
    accounting_timezone: "Asia/Shanghai",
    timezone_version: 0,
    period_id: "2026-08-06",
    period_start: "2026-08-05T16:00:00Z",
    period_end: "2026-08-06T16:00:00Z",
    generated_at: "2026-08-06T01:12:33Z",
    ...overrides,
  };
}

// An idle instance: every derived mean is zero because nothing has been written
// yet, which is exactly what the server reports rather than NaN.
export function emptyWritePath(overrides: Partial<WritePathSummary> = {}): WritePathSummary {
  return {
    wal_sync_seconds: 0,
    wal_batch_size: 0,
    project_lock_wait_seconds: 0,
    project_lock_held_seconds: 0,
    project_events_per_second: 0,
    project_requests_per_second: 0,
    metadata_batch_size: 0,
    metadata_write_seconds: 0,
    ...overrides,
  };
}

// What GET /admin/api/v1/provider-profiles actually answers, generated from the
// running endpoint by TestProviderProfilesGoldenMatchesConsoleFixture and
// checked in. Hand-writing it would test an idea of the matrix rather than the
// matrix, and a console built against a wrong idea of it is exactly the drift
// serving the matrix removes. That Go test fails when the two diverge.
export { default as providerProfilesFixture } from "./provider-profiles.golden.json";

/**
 * The 401 the Admin API answers with when a step-up-guarded call arrives
 * without material and the session's re-authentication window is not open.
 *
 * The console never knows the window's state in advance — only the server does
 * — so it attempts the call with nothing and lets this refusal be the question.
 * A test that wants to see the credential fields drives the mutation to this
 * answer first, exactly as the console does.
 */
export function stepUpRequired(): ApiError {
  return new ApiError(401, "recent re-authentication required", "recent_reauth_required");
}
