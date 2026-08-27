import i18n from "./i18n";
import { useAccountingTimeZone } from "./timezone";

function locale() {
  return i18n.resolvedLanguage || "zh-CN";
}

export function compactNumber(value: number) {
  return new Intl.NumberFormat(locale(), { notation: "compact", maximumFractionDigits: 1 })
    .format(value || 0);
}

/**
 * A count read as an exact figure rather than at a glance: grouped, never
 * abbreviated. The cards abbreviate token limits because a tile has a width;
 * the details drawer is where the operator goes to read the actual number, and
 * `1050000` unseparated is the one form that is harder to read than either.
 */
export function exactNumber(value: number) {
  return new Intl.NumberFormat(locale()).format(value || 0);
}

export function money(micros: number) {
  return new Intl.NumberFormat(locale(), {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: micros > 0 && micros < 10_000 ? 4 : 2,
  }).format((micros || 0) / 1_000_000);
}

export type InstantStyle = "dateTime" | "dateTimeYear" | "date" | "full";

const INSTANT_STYLES: Record<InstantStyle, Intl.DateTimeFormatOptions> = {
  dateTime: { month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" },
  dateTimeYear: { year: "numeric", month: "2-digit", day: "2-digit", hour: "2-digit", minute: "2-digit" },
  date: { year: "numeric", month: "2-digit", day: "2-digit" },
  full: { dateStyle: "medium", timeStyle: "short" },
};

/**
 * Renders an instant in an explicitly named time zone.
 *
 * The zone is a required argument on purpose. Intl defaults to the browser's
 * zone, and that default is what put chart axes and the totals beside them on
 * different days: the figures were summed over the server's accounting day
 * while the labels were drawn in the viewer's. Every timestamp shown by the
 * console goes through here, so there is one place where that decision lives.
 */
export function formatInstant(
  value: string | number | Date | undefined,
  timeZone: string,
  style: InstantStyle = "dateTime",
): string {
  if (value === undefined || value === null || value === "") return "—";
  const parsed = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(parsed.getTime())) return "—";
  return new Intl.DateTimeFormat(locale(), { ...INSTANT_STYLES[style], timeZone }).format(parsed);
}

/**
 * Binds formatInstant to the console's current display zone. Components take
 * this rather than a bare formatter so a zone can never be left unstated.
 */
export function useInstantFormatter() {
  const timeZone = useAccountingTimeZone();
  return (value: string | number | Date | undefined, style: InstantStyle = "dateTime") =>
    formatInstant(value, timeZone, style);
}

const AGE_UNITS: [Intl.RelativeTimeFormatUnit, number][] = [
  ["year", 365 * 24 * 60 * 60_000],
  ["month", 30 * 24 * 60 * 60_000],
  ["day", 24 * 60 * 60_000],
  ["hour", 60 * 60_000],
  ["minute", 60_000],
];

/**
 * How long ago something was observed, in the coarsest unit that still says
 * something — "2 天前", not a timestamp.
 *
 * A verdict without an age is the console's most misleading element: a test
 * that passed two months ago against a model the upstream has since retired
 * renders exactly like one from this morning. The exact instant stays in the
 * details drawer, where it is read rather than scanned.
 *
 * `now` is a parameter so a caller can render a fixed instant — a component
 * reading the wall clock during a test run is the one thing that makes the
 * output untestable.
 */
export function formatAge(value: string | number | Date | undefined, now: number = Date.now()): string {
  if (value === undefined || value === null || value === "") return "";
  const parsed = value instanceof Date ? value : new Date(value);
  if (Number.isNaN(parsed.getTime())) return "";
  const elapsed = now - parsed.getTime();
  // A clock that disagrees with the server by a few seconds must not render
  // "in 3 seconds"; anything under a minute is simply just now.
  const formatter = new Intl.RelativeTimeFormat(locale(), { numeric: "auto" });
  for (const [unit, millis] of AGE_UNITS) {
    if (Math.abs(elapsed) >= millis) return formatter.format(-Math.round(elapsed / millis), unit);
  }
  return formatter.format(0, "minute");
}
