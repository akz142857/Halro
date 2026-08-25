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
