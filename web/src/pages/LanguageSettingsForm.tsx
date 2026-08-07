import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { ErrorState, Field } from "../components";
import { applyPreference } from "../i18n";
import { useIsReadOnly } from "../session";
import type { AdminPreferences, InstanceUISettings, LocalePreference, SupportedLocale } from "../types";

export function LanguageSettingsForm(props: { ui: InstanceUISettings; preferences: AdminPreferences }) {
  return <><PersonalLanguageForm {...props} /><InstanceLanguageForm {...props} /></>;
}

export function PersonalLanguageForm({ ui, preferences }: { ui: InstanceUISettings; preferences: AdminPreferences }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [locale, setLocale] = useState<LocalePreference>(preferences.locale);
  useEffect(() => setLocale(preferences.locale), [preferences.locale]);
  const preferenceMutation = useMutation({
    mutationFn: (value: LocalePreference) => api.updatePreferences({ locale: value, appearance: preferences.appearance }, preferences.revision),
    onSuccess: async (_, value) => applyPreference(value, ui.default_locale),
    onSettled: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["preferences"] }),
        queryClient.invalidateQueries({ queryKey: ["session"] }),
      ]);
    },
  });
  return (
    <section className="panel settings-card language-settings">
      <header className="panel-header">
        <div><p className="eyebrow">{t("settings.personalScope")}</p><h3>{t("settings.interfaceLanguage")}</h3></div>
        <span className="badge">{t("settings.onlyYou")}</span>
      </header>
      <p className="panel-description">{t("settings.personalLanguageDescription")}</p>
      <div className="settings-form compact-form" aria-busy={preferenceMutation.isPending}>
        <Field label={t("settings.interfaceLanguage")}>
          <select value={locale} disabled={preferenceMutation.isPending} onChange={(event) => { const value = event.target.value as LocalePreference; setLocale(value); preferenceMutation.mutate(value); }}>
            <option value="system">{t("settings.followInstance")}</option>
            <option value="zh-CN">{t("settings.zhCN")}</option>
            <option value="en-US">{t("settings.enUS")}</option>
          </select>
        </Field>
        {preferenceMutation.isError && <ErrorState error={preferenceMutation.error} />}
        {preferenceMutation.isSuccess && <div className="notice success" role="status"><strong>{t("settings.preferenceSaved")}</strong></div>}
      </div>
    </section>
  );
}

export function InstanceLanguageForm({ ui, preferences }: { ui: InstanceUISettings; preferences: AdminPreferences }) {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const queryClient = useQueryClient();
  const [defaultLocale, setDefaultLocale] = useState<SupportedLocale>(ui.default_locale);
  useEffect(() => setDefaultLocale(ui.default_locale), [ui.default_locale]);
  const instanceMutation = useMutation({
    mutationFn: () => api.updateUISettings(defaultLocale, ui.revision),
    onSuccess: async () => { if (preferences.locale === "system") await applyPreference(preferences.locale, defaultLocale); },
    onSettled: async () => { await Promise.all([queryClient.invalidateQueries({ queryKey: ["ui-settings"] }), queryClient.invalidateQueries({ queryKey: ["ui-bootstrap"] })]); },
  });
  const instanceChanged = defaultLocale !== ui.default_locale;
  return (
    <section className="panel settings-card language-settings">
      <header className="panel-header"><div><p className="eyebrow">{t("settings.instanceScope")}</p><h3>{t("settings.instanceLanguage")}</h3></div><span className="badge warning">{t("settings.allAdministrators")}</span></header>
      <p className="panel-description">{t("settings.instanceLanguageDescription")}</p>
      <form className="settings-form compact-form" aria-busy={instanceMutation.isPending} onSubmit={(event) => { event.preventDefault(); instanceMutation.mutate(); }}>
        <Field label={t("settings.instanceLanguage")}>
          <select value={defaultLocale} onChange={(event) => { instanceMutation.reset(); setDefaultLocale(event.target.value as SupportedLocale); }}>
            <option value="zh-CN">{t("settings.zhCN")}</option>
            <option value="en-US">{t("settings.enUS")}</option>
          </select>
        </Field>
        {instanceMutation.isError && <ErrorState error={instanceMutation.error} />}
        {instanceMutation.isSuccess && <div className="notice success" role="status"><strong>{t("settings.instanceLanguageSaved")}</strong></div>}
        <div className="form-actions"><button className="button primary" disabled={readOnly || instanceMutation.isPending || !instanceChanged}>{instanceMutation.isPending ? t("settings.saving") : t("settings.saveInstanceLanguage")}</button></div>
      </form>
    </section>
  );
}
