import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import QRCode from "qrcode";
import { api } from "../api";
import { ErrorState, Field, Loading, PageHeader, StatusDot } from "../components";
import { applyPreference } from "../i18n";
import { setNavigationBlocked } from "../navigation";
import type { AdminPreferences, InstanceUISettings, LocalePreference, SupportedLocale } from "../types";

export function SettingsPage({ mfaSetupRequired = false }: { mfaSetupRequired?: boolean }) {
  const { t } = useTranslation();
  const status = useQuery({
    queryKey: ["system-status"],
    queryFn: api.systemStatus,
    refetchInterval: 15_000,
    enabled: !mfaSetupRequired,
  });
  const settings = useQuery({ queryKey: ["settings"], queryFn: api.settings, enabled: !mfaSetupRequired });
  const uiSettings = useQuery({ queryKey: ["ui-settings"], queryFn: api.uiSettings, enabled: !mfaSetupRequired });
  const preferences = useQuery({ queryKey: ["preferences"], queryFn: api.preferences, enabled: !mfaSetupRequired });
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
        title={mfaSetupRequired?t("settings.mfaRequiredTitle"):t("settings.title")}
        description={mfaSetupRequired?t("settings.mfaRequiredDescription"):t("settings.description")}
      />
      {!mfaSetupRequired&&pending && <Loading />}
      {error && <ErrorState error={error} />}
      {!mfaSetupRequired&&uiSettings.data && preferences.data && (
        <LanguageSettingsForm ui={uiSettings.data.data} preferences={preferences.data.data} />
      )}
      {!mfaSetupRequired&&settings.data && <RuntimeSettingsForm settings={settings.data.data} />}
      {!mfaSetupRequired&&<PasswordChangeForm />}
      <MFASettings />
      {!mfaSetupRequired&&status.data && (
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

export function MFASettings() {
  const { t, i18n } = useTranslation();
  const queryClient = useQueryClient();
  const status = useQuery({ queryKey: ["mfa"], queryFn: api.mfaStatus });
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");
  const [approvalCode, setApprovalCode] = useState("");
  const [confirmCode, setConfirmCode] = useState("");
  const [enrollment, setEnrollment] = useState<Awaited<ReturnType<typeof api.createMFAAuthenticator>> | null>(null);
  const [recovery, setRecovery] = useState<string[]>([]);
  const [copyStatus, setCopyStatus] = useState("");
  const [enrollmentExpired, setEnrollmentExpired] = useState(false);
  const [revokeState, setRevokeState] = useState({ id: "", password: "", code: "" });
  const [regenerateState, setRegenerateState] = useState({ password: "", code: "" });
  const [disableState, setDisableState] = useState({ open: false, password: "", code: "", confirmed: false });
  const [renameState, setRenameState] = useState({ id: "", name: "", revision: 0 });

  useEffect(() => {
    const blocked = recovery.length > 0;
    setNavigationBlocked(blocked);
    const beforeUnload = (event: BeforeUnloadEvent) => { if (blocked) event.preventDefault(); };
    window.addEventListener("beforeunload", beforeUnload);
    return () => { window.removeEventListener("beforeunload", beforeUnload); setNavigationBlocked(false); };
  }, [recovery.length]);
  useEffect(() => {
    if (!enrollment) return;
    const delay = Math.max(0, new Date(enrollment.expires_at).getTime() - Date.now());
    const timer = window.setTimeout(() => { setEnrollment(null); setConfirmCode(""); setEnrollmentExpired(true); }, delay);
    return () => window.clearTimeout(timer);
  }, [enrollment]);

  const refresh = async () => { await queryClient.invalidateQueries({ queryKey: ["mfa"] }); await queryClient.invalidateQueries({ queryKey: ["session"] }); };
  const create = useMutation({ mutationFn: () => api.createMFAAuthenticator(name, password, approvalCode), onSettled: () => { setPassword(""); setApprovalCode(""); }, onSuccess: (value) => { setEnrollmentExpired(false); setEnrollment(value); } });
  const confirm = useMutation({ mutationFn: () => api.confirmMFAAuthenticator(enrollment!.id, confirmCode), onSettled: () => setConfirmCode(""), onSuccess: async (value) => { setRecovery(value.recovery_codes || []); setEnrollment(null); setName(""); await refresh(); } });
  const cancelEnrollment = useMutation({ mutationFn: () => api.cancelPendingMFAAuthenticator(enrollment!.id), onSuccess: () => { setEnrollment(null); setConfirmCode(""); }, onError: () => { /* keep the secret visible until server cancellation succeeds */ } });
  const revoke = useMutation({ mutationFn: () => api.deleteMFAAuthenticator(revokeState.id, revokeState.password, revokeState.code), onSettled: () => setRevokeState((v) => ({ ...v, password: "", code: "" })), onSuccess: async () => { setRevokeState({ id: "", password: "", code: "" }); await refresh(); } });
  const regenerate = useMutation({ mutationFn: () => api.regenerateMFARecoveryCodes(regenerateState.password, regenerateState.code), onSettled: () => setRegenerateState({ password: "", code: "" }), onSuccess: async (value) => { setRecovery(value.recovery_codes); await refresh(); } });
  const disable = useMutation({ mutationFn: () => api.disableMFA(disableState.password, disableState.code), onSettled: () => setDisableState((v) => ({ ...v, password: "", code: "" })), onSuccess: async () => { setDisableState({ open: false, password: "", code: "", confirmed: false }); await refresh(); } });
  const rename = useMutation({ mutationFn: () => api.renameMFAAuthenticator(renameState.id, renameState.name, renameState.revision), onSuccess: async () => { setRenameState({ id: "", name: "", revision: 0 }); await queryClient.invalidateQueries({ queryKey: ["mfa"] }); } });
  const copyRecovery = async () => { try { await navigator.clipboard.writeText(recovery.join("\n")); setCopyStatus(t("settings.recoveryCopied")); } catch { setCopyStatus(t("settings.recoveryCopyFailed")); } };
  const formatDate = (value?: string) => value ? new Intl.DateTimeFormat(i18n.language, { dateStyle: "medium", timeStyle: "short" }).format(new Date(value)) : t("common.never");

  return <section className="panel runtime-settings" aria-labelledby="mfa-title">
    <header className="panel-header"><div><p className="eyebrow">{t("settings.security")}</p><h2 id="mfa-title">{t("settings.mfaTitle")}</h2><p>{t("settings.mfaDescription")}</p></div><span className="badge">{t(`settings.mfaPolicy.${status.data?.policy || "optional"}`)}</span></header>
    {status.isError && <ErrorState error={status.error} />}
    {enrollmentExpired && <div className="notice warning" role="status">{t("settings.enrollmentExpired")}</div>}
    {status.data?.recovery_codes_remaining !== undefined && <p>{t("settings.recoveryRemaining", { count: status.data.recovery_codes_remaining })}</p>}
    {status.data?.authenticators.map((a) => <div className="notice" key={a.id}><strong>{a.name}</strong><span>{t("settings.createdAt")}: {formatDate(a.created_at)}</span><span>{t("settings.lastUsed")}: {formatDate(a.last_used_at)}</span><div className="form-actions"><button type="button" className="button ghost" onClick={() => setRenameState({ id: a.id, name: a.name, revision: a.revision })}>{t("settings.renameAuthenticator")}</button><button type="button" className="button ghost" onClick={() => setRevokeState({ id: a.id, password: "", code: "" })}>{t("settings.revokeAuthenticator")}</button></div></div>)}
    {renameState.id && <form onSubmit={(e) => { e.preventDefault(); rename.mutate(); }}><Field label={t("settings.deviceName")}><input autoFocus required maxLength={64} value={renameState.name} onChange={(e) => setRenameState((v) => ({ ...v, name: e.target.value }))} /></Field>{rename.isError && <ErrorState error={rename.error} />}<div className="form-actions"><button className="button primary" disabled={rename.isPending}>{t("common.save")}</button><button type="button" className="button ghost" onClick={() => setRenameState({ id: "", name: "", revision: 0 })}>{t("common.cancel")}</button></div></form>}
    {revokeState.id && <form onSubmit={(e) => { e.preventDefault(); revoke.mutate(); }}><Field label={t("settings.currentPassword")}><input required type="password" autoComplete="current-password" value={revokeState.password} onChange={(e) => setRevokeState((v) => ({ ...v, password: e.target.value }))} /></Field><Field label={t("auth.authenticatorCode")} hint={t("settings.otherAuthenticatorHint")}><input required inputMode="numeric" autoComplete="one-time-code" value={revokeState.code} onChange={(e) => setRevokeState((v) => ({ ...v, code: e.target.value }))} /></Field>{revoke.isError && <ErrorState error={revoke.error} />}<div className="form-actions"><button className="button danger" disabled={revoke.isPending}>{t("settings.confirmRevoke")}</button><button type="button" className="button ghost" onClick={() => setRevokeState({ id: "", password: "", code: "" })}>{t("common.cancel")}</button></div></form>}
    {!enrollment && !recovery.length && <form onSubmit={(e) => { e.preventDefault(); create.mutate(); }}><Field label={t("settings.deviceName")}><input required maxLength={64} value={name} onChange={(e) => setName(e.target.value)} /></Field><Field label={t("settings.currentPassword")}><input required type="password" autoComplete="current-password" value={password} onChange={(e) => setPassword(e.target.value)} /></Field>{status.data?.enabled && <Field label={t("auth.authenticatorCode")} hint={t("settings.existingCodeHint")}><input required inputMode="numeric" autoComplete="one-time-code" value={approvalCode} onChange={(e) => setApprovalCode(e.target.value)} /></Field>}{create.isError && <ErrorState error={create.error} />}<div className="form-actions"><button className="button primary" disabled={create.isPending}>{t("settings.addAuthenticator")}</button></div></form>}
    {enrollment && <form onSubmit={(e) => { e.preventDefault(); confirm.mutate(); }}><div className="notice warning"><strong>{t("settings.scanAuthenticator")}</strong><EnrollmentQR uri={enrollment.otpauth_uri} /><span>{t("settings.manualSecret")}: <code>{enrollment.secret}</code></span><small>{t("settings.enrollmentExpires", { value: formatDate(enrollment.expires_at) })}</small></div><Field label={t("auth.authenticatorCode")}><input autoFocus required inputMode="numeric" autoComplete="one-time-code" value={confirmCode} onChange={(e) => setConfirmCode(e.target.value)} /></Field>{confirm.isError && <ErrorState error={confirm.error} />}{cancelEnrollment.isError && <ErrorState error={cancelEnrollment.error} />}<div className="form-actions"><button className="button primary" disabled={confirm.isPending||cancelEnrollment.isPending}>{t("auth.verify")}</button><button type="button" className="button ghost" disabled={cancelEnrollment.isPending} onClick={()=>cancelEnrollment.mutate()}>{t("settings.cancelEnrollment")}</button></div></form>}
    {recovery.length > 0 && <div className="notice warning"><strong>{t("settings.saveRecoveryCodes")}</strong><pre>{recovery.join("\n")}</pre><div aria-live="polite">{copyStatus}</div><div className="form-actions"><button type="button" className="button" onClick={() => void copyRecovery()}>{t("common.copy")}</button><button type="button" className="button" onClick={() => downloadRecoveryCodes(recovery)}>{t("common.download")}</button><button type="button" className="button ghost" onClick={() => { setRecovery([]); setCopyStatus(""); }}>{t("settings.savedRecoveryCodes")}</button></div></div>}
    {status.data?.enabled && !revokeState.id && !recovery.length && <form onSubmit={(e) => { e.preventDefault(); regenerate.mutate(); }}><h3>{t("settings.regenerateRecovery")}</h3><Field label={t("settings.currentPassword")}><input required type="password" autoComplete="current-password" value={regenerateState.password} onChange={(e) => setRegenerateState((v) => ({ ...v, password: e.target.value }))} /></Field><Field label={t("auth.authenticatorCode")}><input required inputMode="numeric" autoComplete="one-time-code" value={regenerateState.code} onChange={(e) => setRegenerateState((v) => ({ ...v, code: e.target.value }))} /></Field>{regenerate.isError && <ErrorState error={regenerate.error} />}<div className="form-actions"><button className="button" disabled={regenerate.isPending}>{t("settings.regenerateRecovery")}</button></div></form>}
    {status.data?.enabled && status.data.policy === "optional" && !recovery.length && <div>{!disableState.open ? <button type="button" className="button danger" onClick={() => setDisableState((v) => ({ ...v, open: true }))}>{t("settings.disableMFA")}</button> : <form onSubmit={(e) => { e.preventDefault(); disable.mutate(); }}><p>{t("settings.disableMFAWarning")}</p><Field label={t("settings.currentPassword")}><input required type="password" autoComplete="current-password" value={disableState.password} onChange={(e) => setDisableState((v) => ({ ...v, password: e.target.value }))} /></Field><Field label={t("auth.authenticatorCode")}><input required inputMode="numeric" autoComplete="one-time-code" value={disableState.code} onChange={(e) => setDisableState((v) => ({ ...v, code: e.target.value }))} /></Field><label><input type="checkbox" checked={disableState.confirmed} onChange={(e) => setDisableState((v) => ({ ...v, confirmed: e.target.checked }))} /> {t("settings.confirmDisableMFA")}</label>{disable.isError && <ErrorState error={disable.error} />}<div className="form-actions"><button className="button danger" disabled={!disableState.confirmed || disable.isPending}>{t("settings.disableMFA")}</button><button type="button" className="button ghost" onClick={() => setDisableState({ open: false, password: "", code: "", confirmed: false })}>{t("common.cancel")}</button></div></form>}</div>}
  </section>;
}

function EnrollmentQR({uri}:{uri:string}) { const {t}=useTranslation(); const [source,setSource]=useState(""); useEffect(()=>{let active=true;void QRCode.toDataURL(uri,{errorCorrectionLevel:"M",margin:2,width:240}).then((value)=>{if(active)setSource(value)});return()=>{active=false;setSource("")}},[uri]);return source?<img src={source} width={240} height={240} alt={t("settings.enrollmentQRAlt")}/>:<Loading/> }

function downloadRecoveryCodes(codes:string[]){const url=URL.createObjectURL(new Blob([`Heimdall recovery codes\n\n${codes.join("\n")}\n`],{type:"text/plain"}));const link=document.createElement("a");link.href=url;link.download="heimdall-recovery-codes.txt";link.click();URL.revokeObjectURL(url)}

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
