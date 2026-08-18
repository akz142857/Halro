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
 * Render an instant as a `datetime-local` value in the accounting zone — the
 * inverse of zonedInputToISO.
 *
 * Without it a field written through zonedInputToISO reopens as the browser's
 * wall clock and the operator sees a different reading than the one they saved,
 * and than the row beside it displays. Returns "" for a missing or unparsable
 * value, which the caller renders as an empty control.
 */
export function isoToZonedInput(value: string | number | Date | undefined, timeZone: string): string {
  if (value === undefined || value === null || value === "") return "";
  const instant = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(instant.getTime())) return "";
  const parts = new Intl.DateTimeFormat("en-US", {
    timeZone, hour12: false,
    year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit",
  }).formatToParts(instant).reduce<Record<string, string>>((all, part) => {
    all[part.type] = part.value;
    return all;
  }, {});
  // en-US renders midnight as hour 24 under hour12:false.
  const hour = parts.hour === "24" ? "00" : parts.hour;
  if (!parts.year || !parts.month || !parts.day || !hour || !parts.minute) return "";
  return `${parts.year}-${parts.month}-${parts.day}T${hour}:${parts.minute}`;
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

/**
 * Every IANA zone name this browser can offer for selection.
 *
 * The server is the authority on which names are acceptable — it validates
 * against its own tzdata — but it has no way to publish the list: Go's
 * embedded database is a zip with no enumeration API, and reading the host's
 * zoneinfo directory is exactly the dependency embedding it removes. The
 * browser already carries a full copy for Intl, so the picker is built from
 * that and costs the bundle nothing.
 *
 * Returns an empty list on an engine without `supportedValuesOf`, which the
 * caller renders as a plain text field rather than an empty menu.
 */
export function supportedTimeZones(): string[] {
  if (zoneNames) return zoneNames;
  try {
    zoneNames = Intl.supportedValuesOf?.("timeZone") ?? [];
  } catch {
    zoneNames = [];
  }
  return zoneNames;
}

let zoneNames: string[] | undefined;

/**
 * The zone's current UTC offset, as "UTC+08:00".
 *
 * A list of 400-odd names is only searchable if you already know the name. The
 * offset is what an operator checking a provider's billing hours actually
 * recognises, so it is rendered beside every option. Cached because the picker
 * asks for the same zone on every keystroke; a session left open across a DST
 * transition keeps showing the offset that was in force when it first asked,
 * which is a label being an hour stale, not a stored value being wrong.
 */
export function zoneOffsetLabel(zone: string): string {
  const cached = offsetLabels.get(zone);
  if (cached !== undefined) return cached;
  let label = "";
  try {
    const name = new Intl.DateTimeFormat("en-US", { timeZone: zone, timeZoneName: "longOffset" })
      .formatToParts(new Date())
      .find((part) => part.type === "timeZoneName")?.value ?? "";
    // longOffset yields "GMT+08:00", and "GMT" alone at zero offset.
    label = name === "GMT" ? "UTC+00:00" : name.replace("GMT", "UTC");
  } catch {
    label = "";
  }
  offsetLabels.set(zone, label);
  return label;
}

const offsetLabels = new Map<string, string>();
