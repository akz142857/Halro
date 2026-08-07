import { useSyncExternalStore } from "react";
import type { TimeContext } from "./types";

/**
 * The time zone every timestamp in the console is rendered in.
 *
 * It is the server's accounting time zone, not the browser's. "Today" on this
 * console is a server-side judgement — it decides when a daily budget resets
 * and which day a call is billed to — so rendering the chart axis beside those
 * totals in the viewer's own zone put the two out of step by the offset
 * between them. One zone for the whole console keeps them describing the same
 * day.
 *
 * Per-administrator display zones are a later step; until then this is
 * whatever the server reports in `time_context`.
 */

export const DEFAULT_TIME_ZONE = "UTC";

let current = DEFAULT_TIME_ZONE;
const listeners = new Set<() => void>();

/** Whether the runtime can actually format in the named zone. */
export function isSupportedTimeZone(value: string): boolean {
  if (!value) return false;
  try {
    new Intl.DateTimeFormat("en-US", { timeZone: value });
    return true;
  } catch {
    return false;
  }
}

/**
 * Adopts the server's accounting time zone. Ignores names this browser cannot
 * format: an older engine missing a recently added zone should keep rendering
 * in UTC rather than throw on every timestamp on the page.
 */
export function setAccountingTimeZone(value: string | undefined): void {
  const next = value && isSupportedTimeZone(value) ? value : DEFAULT_TIME_ZONE;
  if (next === current) return;
  current = next;
  for (const listener of listeners) listener();
}

export function accountingTimeZone(): string {
  return current;
}

/** Restores the pre-authentication default; used on logout. */
export function resetAccountingTimeZone(): void {
  setAccountingTimeZone(DEFAULT_TIME_ZONE);
}

function subscribe(listener: () => void): () => void {
  listeners.add(listener);
  return () => listeners.delete(listener);
}

/** Current display zone, re-rendering subscribers when the server's changes. */
export function useAccountingTimeZone(): string {
  return useSyncExternalStore(subscribe, accountingTimeZone, accountingTimeZone);
}

/**
 * Adopts the zone carried by any response that reports a per-day figure. Safe
 * to call on every render: it is a no-op unless the zone actually changed.
 */
export function adoptTimeContext(context: TimeContext | undefined): void {
  if (context) setAccountingTimeZone(context.accounting_timezone);
}

function zoneOffsetMillis(instant: number, timeZone: string): number {
  const parts = new Intl.DateTimeFormat("en-US", {
    timeZone, hour12: false,
    year: "numeric", month: "2-digit", day: "2-digit",
    hour: "2-digit", minute: "2-digit", second: "2-digit",
  }).formatToParts(new Date(instant));
  const value = (type: string) => Number(parts.find((part) => part.type === type)?.value);
  // hour12:false renders midnight as 24 in some engines.
  const asIfUTC = Date.UTC(
    value("year"), value("month") - 1, value("day"),
    value("hour") % 24, value("minute"), value("second"),
  );
  return asIfUTC - instant;
}

/**
 * Read a `datetime-local` value as a wall-clock reading in the accounting zone.
 *
 * The control has no zone of its own — it yields "2026-08-07T00:00" and the
 * browser's Date constructor resolves that against the viewer's zone. Every
 * other timestamp on the console is rendered in the accounting zone, so a
 * viewer in UTC filtering a Asia/Shanghai instance asked for a window eight
 * hours away from the one the column beside it displays, with nothing on screen
 * to say so. Returns "" for an incomplete value, which the caller omits.
 */
export function zonedInputToISO(value: string, timeZone: string): string {
  const match = /^(\d{4})-(\d{2})-(\d{2})T(\d{2}):(\d{2})(?::(\d{2}))?$/.exec(value);
  if (!match) return "";
  const [, year, month, day, hour, minute, second] = match;
  const wall = Date.UTC(+year, +month - 1, +day, +hour, +minute, second ? +second : 0);
  // Two passes: the first offset is the one in force at the guessed instant,
  // which is the wrong side of a DST transition for readings near one.
  let instant = wall - zoneOffsetMillis(wall, timeZone);
  instant = wall - zoneOffsetMillis(instant, timeZone);
  return new Date(instant).toISOString();
}
