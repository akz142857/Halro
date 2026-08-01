import { useTranslation } from "react-i18next";
import { applyLocale, supportedLocale, type SupportedLocale } from "./i18n";

export function LanguageSelect({ compact = false }: { compact?: boolean }) {
  const { t, i18n } = useTranslation();
  const value = supportedLocale(i18n.resolvedLanguage) || "zh-CN";
  return (
    <label className={`language-select ${compact ? "compact" : ""}`}>
      {!compact && <span>{t("auth.languageLabel")}</span>}
      <select
        aria-label={t("auth.languageLabel")}
        value={value}
        onChange={(event) => void applyLocale(event.target.value as SupportedLocale)}
      >
        <option value="zh-CN">简体中文</option>
        <option value="en-US">English</option>
      </select>
    </label>
  );
}
