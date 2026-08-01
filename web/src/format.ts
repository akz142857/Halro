import i18n from "./i18n";

function locale() {
  return i18n.resolvedLanguage || "zh-CN";
}

export function compactNumber(value: number) {
  return new Intl.NumberFormat(locale(), { notation: "compact", maximumFractionDigits: 1 })
    .format(value || 0);
}

export function money(micros: number) {
  return new Intl.NumberFormat(locale(), {
    style: "currency",
    currency: "USD",
    minimumFractionDigits: micros > 0 && micros < 10_000 ? 4 : 2,
  }).format((micros || 0) / 1_000_000);
}

export function dateTime(value?: string) {
  if (!value) return "—";
  return new Intl.DateTimeFormat(locale(), {
    month: "2-digit",
    day: "2-digit",
    hour: "2-digit",
    minute: "2-digit",
  }).format(new Date(value));
}
