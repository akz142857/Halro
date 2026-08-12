import i18n from "i18next";
import { initReactI18next } from "react-i18next";

export const supportedLocales = ["zh-CN", "en-US"] as const;
export type SupportedLocale = (typeof supportedLocales)[number];
export type LocalePreference = SupportedLocale | "system";

export function supportedLocale(value?: string | null): SupportedLocale | undefined {
  if (!value) return undefined;
  const normalized = value.replace("_", "-").toLowerCase();
  if (normalized === "zh" || normalized.startsWith("zh-cn") || normalized.startsWith("zh-hans")) return "zh-CN";
  if (normalized === "en" || normalized.startsWith("en-")) return "en-US";
  return undefined;
}

function browserLocale() {
  return (navigator.languages || [navigator.language]).map(supportedLocale).find(Boolean);
}

export function resolveLocale(preference?: string, instanceDefault?: string): SupportedLocale {
  return supportedLocale(preference) || supportedLocale(instanceDefault) || browserLocale() || "zh-CN";
}

// One locale per chunk. An operator reads the console in one language, so the
// other one is weight every session pays and no session uses. Splitting it is
// only safe because i18n.test.tsx pins the two key sets in exact parity: a key
// present in one locale and absent from the other would otherwise fall through
// to a fallback bundle that is no longer in memory. If that test is ever
// relaxed, the fallback has to be loaded eagerly again.
const loaders: Record<SupportedLocale, () => Promise<Record<string, unknown>>> = {
  "zh-CN": () => import("./locales/zh-CN").then((module) => module.zhCN),
  "en-US": () => import("./locales/en-US").then((module) => module.enUS),
};

export async function loadLocale(locale: SupportedLocale) {
  if (i18n.hasResourceBundle(locale, "translation")) return;
  i18n.addResourceBundle(locale, "translation", await loaders[locale]());
}

export async function applyLocale(locale: SupportedLocale) {
  await loadLocale(locale);
  await i18n.changeLanguage(locale);
  document.documentElement.lang = locale;
  document.documentElement.dir = i18n.dir(locale);
}

export async function applyPreference(preference: string, instanceDefault?: string) {
  if (preference === "system") {
    return applyLocale(resolveLocale(undefined, instanceDefault));
  }
  return applyLocale(resolveLocale(preference, instanceDefault));
}

let started: Promise<typeof i18n> | undefined;

// Awaited before the first render: a tree rendered against an empty store would
// paint raw translation keys and then swap them for text.
export function initI18n() {
  started ??= start();
  return started;
}

async function start() {
  const initialLocale = resolveLocale();
  await i18n.use(initReactI18next).init({
    resources: { [initialLocale]: { translation: await loaders[initialLocale]() } },
    lng: initialLocale,
    fallbackLng: "en-US",
    supportedLngs: [...supportedLocales],
    interpolation: { escapeValue: false },
    returnNull: false,
  });
  document.documentElement.lang = initialLocale;
  document.documentElement.dir = i18n.dir(initialLocale);
  i18n.on("languageChanged", (locale) => {
    document.documentElement.lang = locale;
    document.documentElement.dir = i18n.dir(locale);
  });
  return i18n;
}

export default i18n;
