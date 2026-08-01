import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { ErrorState, Field, Loading, PageHeader, StatusDot } from "../components";
import { applyPreference } from "../i18n";
import type { AdminPreferences, InstanceUISettings, LocalePreference, SupportedLocale } from "../types";

export function SettingsPage() {
  const { t } = useTranslation();
  const status = useQuery({
    queryKey: ["system-status"],
    queryFn: api.systemStatus,
    refetchInterval: 15_000,
  });
  const settings = useQuery({ queryKey: ["settings"], queryFn: api.settings });
  const uiSettings = useQuery({ queryKey: ["ui-settings"], queryFn: api.uiSettings });
  const preferences = useQuery({ queryKey: ["preferences"], queryFn: api.preferences });
  const pending = status.isPending || settings.isPending || uiSettings.isPending || preferences.isPending;
  const error = status.error || settings.error || uiSettings.error || preferences.error;
  const accountingLabels = [t("settings.healthy"), t("settings.degraded"), t("settings.unavailable"), t("settings.recoveryRequired")];
  const metricLabels: Record<string, string> = {
    batches: t("settings.batches"),
    records: t("settings.records"),
    errors: t("settings.errors"),
    queuedepth: t("settings.queueDepth"),
    queuecapacity: t("settings.queueCapacity"),
    lasthash: t("settings.lastHash"),
    bytes: t("settings.bytes"),
  };
  return (
    <>
      <PageHeader
        eyebrow={t("settings.eyebrow")}
        title={t("settings.title")}
        description={t("settings.description")}
      />
      {pending && <Loading />}
      {error && <ErrorState error={error} />}
      {uiSettings.data && preferences.data && (
        <LanguageSettingsForm ui={uiSettings.data.data} preferences={preferences.data.data} />
      )}
      {settings.data && <RuntimeSettingsForm settings={settings.data.data} />}
      <PasswordChangeForm />
      {status.data && (
        <div className="settings-grid">
          <section className="panel system-card">
            <p className="eyebrow">{t("settings.build")}</p>
            <h2>Heimdall {status.data.build.version || "development"}</h2>
            <dl>
              <div><dt>{t("settings.commit")}</dt><dd><code>{status.data.build.commit || t("common.local")}</code></dd></div>
              <div><dt>{t("settings.buildDate")}</dt><dd>{status.data.build.date || "—"}</dd></div>
              <div><dt>{t("settings.draining")}</dt><dd>{status.data.draining ? t("common.yes") : t("common.no")}</dd></div>
            </dl>
          </section>
          <section className="panel system-card">
            <p className="eyebrow">{t("settings.durability")}</p>
            <h2><StatusDot ok={status.data.accounting_status === 0} />{accountingLabels[status.data.accounting_status] || t("common.unknown")}</h2>
            <dl>
              {Object.entries(status.data.wal).slice(0, 5).map(([key, value]) => (
                <div key={key}><dt>{metricLabels[key.replaceAll("_", "").toLowerCase()] || key.replaceAll("_", " ")}</dt><dd>{value}</dd></div>
              ))}
            </dl>
          </section>
          <section className="panel system-card">
            <p className="eyebrow">{t("settings.auditHead")}</p>
            <h2>{t("settings.chainCheckpoint")}</h2>
            <dl>
              {Object.entries(status.data.audit).slice(0, 5).map(([key, value]) => (
                <div key={key}><dt>{metricLabels[key.replaceAll("_", "").toLowerCase()] || key.replaceAll("_", " ")}</dt><dd>{String(value)}</dd></div>
              ))}
            </dl>
          </section>
        </div>
      )}
    </>
  );
}

export function PasswordChangeForm() {
  const { t } = useTranslation();
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [validationError, setValidationError] = useState("");
  const queryClient = useQueryClient();
  const mutation = useMutation({
    mutationFn: () => api.changePassword(currentPassword, newPassword),
    onSettled: () => {
      setCurrentPassword("");
      setNewPassword("");
      setConfirmation("");
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["session"] }),
  });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    setValidationError("");
    if (new TextEncoder().encode(newPassword).length < 12) {
      setValidationError(t("settings.passwordTooShort"));
      return;
    }
    if (newPassword !== confirmation) {
      setValidationError(t("settings.passwordMismatch"));
      return;
    }
    mutation.mutate();
  };
  return (
    <section className="panel runtime-settings">
      <header className="panel-header">
        <div><p className="eyebrow">{t("navigation.localAdmin")}</p><h2>{t("settings.changePassword")}</h2></div>
        <span className="badge">{t("settings.sessionRotation")}</span>
      </header>
      <form onSubmit={submit} autoComplete="off">
        <Field label={t("settings.currentPassword")}><input type="password" autoComplete="current-password" required value={currentPassword} onChange={(event) => setCurrentPassword(event.target.value)} /></Field>
        <Field label={t("settings.newPassword")} hint={t("settings.newPasswordHint")}><input type="password" autoComplete="new-password" required value={newPassword} onChange={(event) => setNewPassword(event.target.value)} /></Field>
        <Field label={t("settings.confirmNewPassword")}><input type="password" autoComplete="new-password" required value={confirmation} onChange={(event) => setConfirmation(event.target.value)} /></Field>
        {validationError && <div className="notice warning" role="alert"><strong>{validationError}</strong></div>}
        {mutation.isError && <ErrorState error={mutation.error} />}
        {mutation.isSuccess && <div className="notice success" role="status"><strong>{t("settings.passwordChanged")}</strong></div>}
        <div className="form-actions"><button className="button primary" disabled={mutation.isPending || !currentPassword || !newPassword || !confirmation}>{t("settings.changePassword")}</button></div>
      </form>
    </section>
  );
}

function RuntimeSettingsForm({ settings }: { settings: { health_probe_interval_seconds: number; revision: number; updated_at?: string } }) {
  const { t } = useTranslation();
  const [interval, setInterval] = useState(settings.health_probe_interval_seconds);
  const queryClient = useQueryClient();
  useEffect(() => setInterval(settings.health_probe_interval_seconds), [settings.health_probe_interval_seconds]);
  const mutation = useMutation({
    mutationFn: () => api.updateSettings({ health_probe_interval_seconds: interval }, settings.revision),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["settings"] }),
  });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    mutation.mutate();
  };
  return (
    <section className="panel runtime-settings">
      <header className="panel-header">
        <div><p className="eyebrow">{t("settings.runtimeEyebrow")}</p><h2>{t("settings.runtimeTitle")}</h2></div>
        <span className="badge">{t("settings.revision")} {settings.revision}</span>
      </header>
      <form onSubmit={submit}>
        <Field label={t("settings.probeInterval")} hint={t("settings.probeHint")}>
          <input type="number" min="10" max="3600" required value={interval} onChange={(event) => setInterval(Number(event.target.value))} />
        </Field>
        <div className="notice warning"><strong>{t("settings.startupLocked")}</strong><span>{t("settings.startupDescription")}</span></div>
        {mutation.isError && <ErrorState error={mutation.error} />}
        {mutation.isSuccess && <div className="notice success" role="status"><strong>{t("settings.runtimeSaved")}</strong></div>}
        <div className="form-actions"><button className="button primary" disabled={mutation.isPending || interval === settings.health_probe_interval_seconds}>{t("settings.saveRuntime")}</button></div>
      </form>
    </section>
  );
}

export function LanguageSettingsForm({ ui, preferences }: { ui: InstanceUISettings; preferences: AdminPreferences }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [locale, setLocale] = useState<LocalePreference>(preferences.locale);
  const [defaultLocale, setDefaultLocale] = useState<SupportedLocale>(ui.default_locale);
  useEffect(() => setLocale(preferences.locale), [preferences.locale]);
  useEffect(() => setDefaultLocale(ui.default_locale), [ui.default_locale]);
  const preferenceMutation = useMutation({
    mutationFn: () => api.updatePreferences(locale, preferences.revision),
    onSuccess: async () => applyPreference(locale, ui.default_locale),
    onSettled: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["preferences"] }),
        queryClient.invalidateQueries({ queryKey: ["session"] }),
      ]);
    },
  });
  const instanceMutation = useMutation({
    mutationFn: () => api.updateUISettings(defaultLocale, ui.revision),
    onSuccess: async () => {
      if (preferences.locale === "system") await applyPreference(preferences.locale, defaultLocale);
    },
    onSettled: async () => {
      await Promise.all([
        queryClient.invalidateQueries({ queryKey: ["ui-settings"] }),
        queryClient.invalidateQueries({ queryKey: ["ui-bootstrap"] }),
      ]);
    },
  });
  const preferenceChanged = locale !== preferences.locale;
  const instanceChanged = defaultLocale !== ui.default_locale;
  return (
    <section className="panel runtime-settings language-settings">
      <header className="panel-header">
        <div><p className="eyebrow">{t("settings.languageEyebrow")}</p><h2>{t("settings.languageTitle")}</h2></div>
        <span className="badge">BCP 47</span>
      </header>
      <p className="panel-description">{t("settings.languageDescription")}</p>
      <form onSubmit={(event) => { event.preventDefault(); preferenceMutation.mutate(); }}>
        <Field label={t("settings.interfaceLanguage")}>
          <select value={locale} onChange={(event) => setLocale(event.target.value as LocalePreference)}>
            <option value="system">{t("settings.followInstance")}</option>
            <option value="zh-CN">{t("settings.zhCN")}</option>
            <option value="en-US">{t("settings.enUS")}</option>
          </select>
        </Field>
        {preferenceMutation.isError && <ErrorState error={preferenceMutation.error} />}
        {preferenceMutation.isSuccess && <div className="notice success" role="status"><strong>{t("settings.preferenceSaved")}</strong></div>}
        <div className="form-actions"><button className="button primary" disabled={preferenceMutation.isPending || !preferenceChanged}>{t("settings.savePreference")}</button></div>
      </form>
      <form onSubmit={(event) => { event.preventDefault(); instanceMutation.mutate(); }}>
        <Field label={t("settings.instanceLanguage")}>
          <select value={defaultLocale} onChange={(event) => setDefaultLocale(event.target.value as SupportedLocale)}>
            <option value="zh-CN">{t("settings.zhCN")}</option>
            <option value="en-US">{t("settings.enUS")}</option>
          </select>
        </Field>
        {instanceMutation.isError && <ErrorState error={instanceMutation.error} />}
        {instanceMutation.isSuccess && <div className="notice success" role="status"><strong>{t("settings.instanceLanguageSaved")}</strong></div>}
        <div className="form-actions"><button className="button primary" disabled={instanceMutation.isPending || !instanceChanged}>{t("settings.saveInstanceLanguage")}</button></div>
      </form>
    </section>
  );
}
