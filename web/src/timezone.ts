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
