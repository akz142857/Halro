import type { TimeContext } from "../types";

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
