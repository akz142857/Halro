import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useEffect, useId, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { ErrorState } from "../components";
import { applyPreference } from "../i18n";
import { useNotify } from "../notifications";
import { useIsReadOnly } from "../session";
import type { AdminPreferences, InstanceUISettings, LocalePreference, SupportedLocale } from "../types";

export function LanguageSettingsForm(props: { ui: InstanceUISettings; preferences: AdminPreferences }) {
  return <><PersonalLanguageForm {...props} /><InstanceLanguageForm {...props} /></>;
}

export function PersonalLanguageForm({ ui, preferences }: { ui: InstanceUISettings; preferences: AdminPreferences }) {
  const { t, i18n } = useTranslation();
  const languageLabel = useId();
  const queryClient = useQueryClient();
  const { notify } = useNotify();
  const [locale, setLocale] = useState<LocalePreference>(preferences.locale);
  useEffect(() => setLocale(preferences.locale), [preferences.locale]);
  const preferenceMutation = useMutation({
    mutationFn: (value: LocalePreference) => api.updatePreferences({ locale: value, appearance: preferences.appearance }, preferences.revision),
    onSuccess: async (_, value) => {
      await applyPreference(value, ui.default_locale);
      notify({ tone: "success", title: i18n.t("settings.preferenceSaved") });
    },
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
        <div><h3 id={languageLabel}>{t("settings.interfaceLanguage")}</h3></div>
        <span className="badge">{t("settings.onlyYou")}</span>
      </header>
      <div className="settings-form compact-form" aria-busy={preferenceMutation.isPending}>
        <div className="field">
          <select aria-labelledby={languageLabel} value={locale} disabled={preferenceMutation.isPending} onChange={(event) => { const value = event.target.value as LocalePreference; setLocale(value); preferenceMutation.mutate(value); }}>
            <option value="system">{t("settings.followInstance")}</option>
            <option value="zh-CN">{t("common.localeZhCN")}</option>
            <option value="en-US">{t("common.localeEnUS")}</option>
          </select>
        </div>
        {preferenceMutation.isError && <ErrorState error={preferenceMutation.error} />}
      </div>
    </section>
  );
}

export function InstanceLanguageForm({ ui, preferences }: { ui: InstanceUISettings; preferences: AdminPreferences }) {
  const { t, i18n } = useTranslation();
  const languageLabel = useId();
  const readOnly = useIsReadOnly();
  const queryClient = useQueryClient();
  const { notify } = useNotify();
  const [defaultLocale, setDefaultLocale] = useState<SupportedLocale>(ui.default_locale);
  useEffect(() => setDefaultLocale(ui.default_locale), [ui.default_locale]);
  const instanceMutation = useMutation({
    mutationFn: () => api.updateUISettings(defaultLocale, ui.revision),
    onSuccess: async () => {
      if (preferences.locale === "system") await applyPreference(preferences.locale, defaultLocale);
      notify({ tone: "success", title: i18n.t("settings.instanceLanguageSaved") });
    },
    onSettled: async () => { await Promise.all([queryClient.invalidateQueries({ queryKey: ["ui-settings"] }), queryClient.invalidateQueries({ queryKey: ["ui-bootstrap"] })]); },
  });
  const instanceChanged = defaultLocale !== ui.default_locale;
  return (
    <section className="panel settings-card language-settings">
      <header className="panel-header"><div><h3 id={languageLabel}>{t("settings.instanceLanguage")}</h3></div><span className="badge">{t("settings.allAdministrators")}</span></header>
      <p className="panel-description">{t("settings.instanceLanguageDescription")}</p>
      <form className="settings-form compact-form" aria-busy={instanceMutation.isPending} onSubmit={(event) => { event.preventDefault(); instanceMutation.mutate(); }}>
        <div className="field">
          <select aria-labelledby={languageLabel} value={defaultLocale} onChange={(event) => { instanceMutation.reset(); setDefaultLocale(event.target.value as SupportedLocale); }}>
            <option value="zh-CN">{t("common.localeZhCN")}</option>
            <option value="en-US">{t("common.localeEnUS")}</option>
          </select>
        </div>
        <div className="form-actions"><button className="button primary" disabled={readOnly || instanceMutation.isPending || !instanceChanged}>{instanceMutation.isPending ? t("settings.saving") : t("settings.saveInstanceLanguage")}</button></div>
        {instanceMutation.isError && <ErrorState error={instanceMutation.error} />}
      </form>
    </section>
  );
}
