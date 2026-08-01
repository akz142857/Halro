import i18n from "i18next";
import { initReactI18next } from "react-i18next";
import { enUS } from "./locales/en-US";
import { zhCN } from "./locales/zh-CN";

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

export async function applyLocale(locale: SupportedLocale) {
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

const initialLocale = resolveLocale();

void i18n.use(initReactI18next).init({
  resources: {
    "zh-CN": { translation: zhCN },
    "en-US": { translation: enUS },
  },
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

export default i18n;
