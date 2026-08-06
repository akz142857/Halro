import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import QRCode from "qrcode";
import { MIN_PASSWORD_CHARACTERS, passwordCharacterCount } from "../password";
import { api } from "../api";
import { ErrorState, Field, Loading, Modal, PageHeader, StatusDot } from "../components";
import { useInstantFormatter } from "../format";
import { applyPreference } from "../i18n";
import { applyAppearance } from "../theme";
import { navigate, setNavigationBlocked, usePathname } from "../navigation";
import type { AccountingSettings, AdminPreferences, Appearance, InstanceUISettings, LocalePreference, SupportedLocale } from "../types";

type SettingsPane = "general" | "security" | "instance" | "diagnostics";

function paneFromPath(path: string): SettingsPane {
  const pane = path.split("/")[3];
  return pane === "security" || pane === "instance" || pane === "diagnostics" ? pane : "general";
}

export function SettingsPage({ mfaSetupRequired = false }: { mfaSetupRequired?: boolean }) {
  const { t } = useTranslation();
  const path = usePathname();
  const pane = mfaSetupRequired ? "security" : paneFromPath(path);
  const status = useQuery({
    queryKey: ["system-status"],
    queryFn: api.systemStatus,
    refetchInterval: 15_000,
    enabled: !mfaSetupRequired && pane === "diagnostics",
  });
  const settings = useQuery({ queryKey: ["settings"], queryFn: api.settings, enabled: !mfaSetupRequired && pane === "instance" });
  const accounting = useQuery({ queryKey: ["accounting-settings"], queryFn: api.accountingSettings, enabled: !mfaSetupRequired && pane === "instance" });
  const uiSettings = useQuery({ queryKey: ["ui-settings"], queryFn: api.uiSettings, enabled: !mfaSetupRequired && (pane === "general" || pane === "instance") });
  const preferences = useQuery({ queryKey: ["preferences"], queryFn: api.preferences, enabled: !mfaSetupRequired && (pane === "general" || pane === "instance") });
  const queries = pane === "diagnostics" ? [status] : pane === "instance" ? [settings, uiSettings, preferences, accounting] : pane === "general" ? [uiSettings, preferences] : [];
  const pending = queries.some((query) => query.isPending);
  const error = queries.find((query) => query.error)?.error;
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
      {mfaSetupRequired ? <div className="settings-pane"><MFASettings /></div> : (
        <div className="settings-shell">
          <nav className="settings-nav" aria-label={t("settings.sectionNavigation")}>
            {(["general", "security", "instance", "diagnostics"] as const).map((item) => (
              <a key={item} href={`/admin/settings/${item}`} className={pane === item ? "active" : ""} aria-current={pane === item ? "page" : undefined} onClick={(event) => { event.preventDefault(); navigate(`/admin/settings/${item}`); }}>{t(`settings.panes.${item}`)}</a>
            ))}
          </nav>
          <div className="settings-pane">
            {pending && <Loading />}
            {error && <ErrorState error={error} />}
            {!pending && !error && pane === "general" && uiSettings.data && preferences.data && <section aria-labelledby="general-title"><SettingsGroupHeader title={t("settings.panes.general")} description={t("settings.generalDescription")} id="general-title" /><AppearanceForm preferences={preferences.data.data} /><PersonalLanguageForm ui={uiSettings.data.data} preferences={preferences.data.data} /></section>}
            {pane === "security" && <section aria-labelledby="security-title"><SettingsGroupHeader title={t("settings.panes.security")} description={t("settings.securityDescription")} id="security-title" /><PasswordChangeForm /><MFASettings /></section>}
            {!pending && !error && pane === "instance" && uiSettings.data && preferences.data && settings.data && <section aria-labelledby="instance-title"><SettingsGroupHeader title={t("settings.panes.instance")} description={t("settings.instanceDescription")} id="instance-title" /><InstanceLanguageForm ui={uiSettings.data.data} preferences={preferences.data.data} />{accounting.data && <AccountingTimezoneForm settings={accounting.data.data} />}<RuntimeSettingsForm settings={settings.data.data} /></section>}
            {!pending && !error && pane === "diagnostics" && status.data && <DiagnosticsPane status={status.data} accountingLabels={accountingLabels} metricLabels={metricLabels} />}
          </div>
        </div>
      )}
    </>
  );
}

function DiagnosticsPane({ status, accountingLabels, metricLabels }: { status: Awaited<ReturnType<typeof api.systemStatus>>; accountingLabels: string[]; metricLabels: Record<string, string> }) {
  const { t } = useTranslation();
  return <section aria-labelledby="diagnostics-title">
    <SettingsGroupHeader title={t("settings.panes.diagnostics")} description={t("settings.systemDescription")} id="diagnostics-title" />
    {(status.accounting_status !== 0 || status.draining) && <SettingsOverview status={status} />}
    <div className="settings-grid">
          <details id="system" className="panel system-card diagnostic-details" open>
            <summary><span>{t("settings.build")}</span><strong>Heimdall {status.build.version || "development"}</strong></summary>
            <dl>
              <div><dt>{t("settings.commit")}</dt><dd><code>{status.build.commit || t("common.local")}</code></dd></div>
              <div><dt>{t("settings.buildDate")}</dt><dd>{status.build.date || "—"}</dd></div>
              <div><dt>{t("settings.draining")}</dt><dd>{status.draining ? t("common.yes") : t("common.no")}</dd></div>
            </dl>
          </details>
          <details className="panel system-card diagnostic-details" open>
            <summary><span>{t("settings.durability")}</span><strong><StatusDot ok={status.accounting_status === 0} />{accountingLabels[status.accounting_status] || t("common.unknown")}</strong></summary>
            <dl>
              {Object.entries(status.wal).map(([key, value]) => (
                <div key={key}><dt>{metricLabels[key.replaceAll("_", "").toLowerCase()] || key.replaceAll("_", " ")}</dt><dd>{value}</dd></div>
              ))}
            </dl>
          </details>
          <details className="panel system-card diagnostic-details" open>
            <summary><span>{t("settings.auditHead")}</span><strong>{t("settings.chainCheckpoint")}</strong></summary>
            <dl>
              {Object.entries(status.audit).map(([key, value]) => (
                <div key={key}><dt>{metricLabels[key.replaceAll("_", "").toLowerCase()] || key.replaceAll("_", " ")}</dt><dd>{String(value)}</dd></div>
              ))}
            </dl>
          </details>
    </div>
  </section>;
}

function SettingsGroupHeader({ title, description, id }: { title: string; description: string; id: string }) {
  return <header className="settings-group-header"><h2 id={id}>{title}</h2><p>{description}</p></header>;
}

function SettingsOverview({ status }: { status: Awaited<ReturnType<typeof api.systemStatus>> }) {
  const { t } = useTranslation();
  const healthy = status.accounting_status === 0 && !status.draining;
  return (
    <section className="settings-overview" aria-labelledby="settings-overview-title">
      <div className="settings-overview-copy">
        <p className="eyebrow">{t("settings.overviewEyebrow")}</p>
        <h2 id="settings-overview-title"><StatusDot ok={healthy} />{healthy ? t("settings.systemReady") : t("settings.systemAttention")}</h2>
        <p>{healthy ? t("settings.systemReadyDescription") : t("settings.systemAttentionDescription")}</p>
      </div>
      <a className="button" href="#system">{t("settings.reviewSystemDetails")}</a>
    </section>
  );
}

export function MFASettings() {
  const { t } = useTranslation();
  const formatInstant = useInstantFormatter();
  const queryClient = useQueryClient();
  const status = useQuery({ queryKey: ["mfa"], queryFn: api.mfaStatus });
  const [name, setName] = useState("");
  const [password, setPassword] = useState("");
  const [approvalCode, setApprovalCode] = useState("");
  const [confirmCode, setConfirmCode] = useState("");
  const [enrollment, setEnrollment] = useState<Awaited<ReturnType<typeof api.createMFAAuthenticator>> | null>(null);
  const [recovery, setRecovery] = useState<string[]>([]);
  const [copyStatus, setCopyStatus] = useState("");
  const [addingAuthenticator, setAddingAuthenticator] = useState(false);
  const [recoverySaved, setRecoverySaved] = useState(false);
  const [recoveryConfirmed, setRecoveryConfirmed] = useState(false);
  const [enrollmentExpired, setEnrollmentExpired] = useState(false);
  const [revokeState, setRevokeState] = useState({ id: "", password: "", code: "" });
  const [regenerateState, setRegenerateState] = useState({ open: false, password: "", code: "" });
  const [disableState, setDisableState] = useState({ open: false, password: "", code: "", confirmed: false });
  const [renameState, setRenameState] = useState({ id: "", name: "", revision: 0 });
  const recoveryRegion = useRef<HTMLDivElement>(null);
  const actionTrigger = useRef<HTMLButtonElement>(null);
  const actionWasOpen = useRef(false);

  useEffect(() => {
    const blocked = recovery.length > 0;
    setNavigationBlocked(blocked, t("settings.recoveryLeaveWarning"));
    const beforeUnload = (event: BeforeUnloadEvent) => { if (blocked) event.preventDefault(); };
    window.addEventListener("beforeunload", beforeUnload);
    return () => { window.removeEventListener("beforeunload", beforeUnload); setNavigationBlocked(false); };
  }, [recovery.length, t]);
  useEffect(() => { if (recovery.length) recoveryRegion.current?.focus(); }, [recovery.length]);
  useEffect(() => {
    if (!enrollment) return;
    const delay = Math.max(0, new Date(enrollment.expires_at).getTime() - Date.now());
    const timer = window.setTimeout(() => { setEnrollment(null); setConfirmCode(""); setEnrollmentExpired(true); }, delay);
    return () => window.clearTimeout(timer);
  }, [enrollment]);

  const refresh = async () => { await queryClient.invalidateQueries({ queryKey: ["mfa"] }); await queryClient.invalidateQueries({ queryKey: ["session"] }); };
  const create = useMutation({ mutationFn: () => api.createMFAAuthenticator(name, password, approvalCode), onSettled: () => { setPassword(""); setApprovalCode(""); }, onSuccess: (value) => { setAddingAuthenticator(false); setEnrollmentExpired(false); setEnrollment(value); } });
  const confirm = useMutation({ mutationFn: () => api.confirmMFAAuthenticator(enrollment!.id, confirmCode), onSettled: () => setConfirmCode(""), onSuccess: async (value) => { setRecoverySaved(false); setRecoveryConfirmed(false); setRecovery(value.recovery_codes || []); setEnrollment(null); setName(""); await refresh(); } });
  const cancelEnrollment = useMutation({ mutationFn: () => api.cancelPendingMFAAuthenticator(enrollment!.id), onSuccess: () => { setEnrollment(null); setConfirmCode(""); restoreActionFocus(); }, onError: () => { /* keep the secret visible until server cancellation succeeds */ } });
  const restoreActionFocus = () => {
    const target = actionTrigger.current || document.getElementById("mfa-title");
    actionTrigger.current = null;
    window.setTimeout(() => target?.focus(), 0);
  };
  const revoke = useMutation({ mutationFn: () => api.deleteMFAAuthenticator(revokeState.id, revokeState.password, revokeState.code), onSuccess: async () => { setRevokeState({ id: "", password: "", code: "" }); await refresh(); restoreActionFocus(); } });
  const regenerate = useMutation({ mutationFn: () => api.regenerateMFARecoveryCodes(regenerateState.password, regenerateState.code), onSuccess: async (value) => { setRegenerateState({ open: false, password: "", code: "" }); setRecoverySaved(false); setRecoveryConfirmed(false); setRecovery(value.recovery_codes); await refresh(); } });
  const disable = useMutation({ mutationFn: () => api.disableMFA(disableState.password, disableState.code), onSuccess: async () => { setDisableState({ open: false, password: "", code: "", confirmed: false }); await refresh(); restoreActionFocus(); } });
  const rename = useMutation({ mutationFn: () => api.renameMFAAuthenticator(renameState.id, renameState.name, renameState.revision), onSuccess: async () => { setRenameState({ id: "", name: "", revision: 0 }); await queryClient.invalidateQueries({ queryKey: ["mfa"] }); restoreActionFocus(); } });
  const copyRecovery = async () => { try { await navigator.clipboard.writeText(recovery.join("\n")); setRecoverySaved(true); setCopyStatus(t("settings.recoveryCopied")); } catch { setCopyStatus(t("settings.recoveryCopyFailed")); } };
  const formatDate = (value?: string) => value ? formatInstant(value, "full") : t("common.never");

  const actionOpen = Boolean(addingAuthenticator || enrollment || recovery.length || renameState.id || revokeState.id || regenerateState.open || disableState.open);
  useEffect(() => {
    if (actionWasOpen.current && !actionOpen) restoreActionFocus();
    actionWasOpen.current = actionOpen;
  }, [actionOpen]);
  return <section className="panel settings-card mfa-settings" aria-labelledby="mfa-title" aria-busy={status.isPending}>
    <header className="panel-header"><div><p className="eyebrow">{t("settings.security")}</p><h3 id="mfa-title" tabIndex={-1}>{t("settings.mfaTitle")}</h3><p>{t("settings.mfaDescription")}</p></div>{status.data ? <span className={`badge ${status.data.enabled ? "good" : ""}`}>{status.data.enabled ? t("settings.mfaEnabled") : t(`settings.mfaPolicy.${status.data.policy}`)}</span> : <span className="badge">{t("common.loading")}</span>}</header>
    {status.isPending && <Loading label={t("settings.loadingMFA")} />}
    {status.isError && <ErrorState error={status.error} />}
    {enrollmentExpired && <div className="notice warning" role="status">{t("settings.enrollmentExpired")}</div>}
    {status.data && <div className="mfa-summary"><strong>{status.data.enabled ? t("settings.mfaEnabled") : t("settings.mfaNotEnabled")}</strong><span>{t("settings.authenticatorCount", { count: status.data.authenticators.length })}</span>{status.data.recovery_codes_remaining !== undefined && <span>{t("settings.recoveryRemaining", { count: status.data.recovery_codes_remaining })}</span>}</div>}
    {status.data && <section className="mfa-subsection" aria-labelledby="devices-title"><header><h4 id="devices-title">{t("settings.authenticatorDevices")}</h4><p>{t("settings.authenticatorDevicesDescription")}</p></header>
      {status.data.authenticators.length === 0 ? <p className="settings-empty">{t("settings.noAuthenticators")}</p> : <div className="authenticator-list">{status.data.authenticators.map((a) => <article key={a.id}><div><strong>{a.name}</strong><span>{t("settings.createdAt")}: {formatDate(a.created_at)} · {t("settings.lastUsed")}: {formatDate(a.last_used_at)}</span></div><div className="form-actions"><button type="button" className="button ghost" disabled={actionOpen} onClick={(event) => { actionTrigger.current = event.currentTarget; setRenameState({ id: a.id, name: a.name, revision: a.revision }); }}>{t("settings.renameAuthenticator")}</button><button type="button" className="button danger" disabled={actionOpen} onClick={(event) => { actionTrigger.current = event.currentTarget; setRevokeState({ id: a.id, password: "", code: "" }); }}>{t("settings.revokeAuthenticator")}</button></div></article>)}</div>}
    </section>}
    {renameState.id && <form className="settings-form credential-form action-panel" aria-label={t("settings.renameAuthenticator")} onSubmit={(e) => { e.preventDefault(); rename.mutate(); }}><Field label={t("settings.deviceName")}><input autoFocus required maxLength={64} value={renameState.name} onChange={(e) => setRenameState((v) => ({ ...v, name: e.target.value }))} /></Field>{rename.isError && <ErrorState error={rename.error} />}<div className="form-actions"><button className="button primary" disabled={rename.isPending}>{t("common.save")}</button><button type="button" className="button ghost" onClick={() => setRenameState({ id: "", name: "", revision: 0 })}>{t("common.cancel")}</button></div></form>}
    {revokeState.id && <Modal dangerous title={t("settings.confirmRevokeNamed", { name: status.data?.authenticators.find((a) => a.id === revokeState.id)?.name || "" })} onClose={() => setRevokeState({ id: "", password: "", code: "" })}><form className="settings-form credential-form danger-panel" onSubmit={(e) => { e.preventDefault(); revoke.mutate(); }}><p>{t("settings.revokeWarning")}</p><Field label={t("settings.currentPassword")}><input autoFocus required type="password" autoComplete="current-password" value={revokeState.password} onChange={(e) => setRevokeState((v) => ({ ...v, password: e.target.value }))} /></Field><Field label={t("auth.authenticatorCode")} hint={t("settings.otherAuthenticatorHint")}><input required inputMode="numeric" pattern="[0-9]*" maxLength={8} autoComplete="one-time-code" value={revokeState.code} onChange={(e) => setRevokeState((v) => ({ ...v, code: e.target.value }))} /></Field>{revoke.isError && <ErrorState error={revoke.error} />}<div className="form-actions"><button className="button danger" disabled={revoke.isPending}>{t("settings.confirmRevoke")}</button><button type="button" className="button ghost" onClick={() => setRevokeState({ id: "", password: "", code: "" })}>{t("common.cancel")}</button></div></form></Modal>}
    {status.data && !actionOpen && <section className="mfa-subsection setting-row" aria-labelledby="add-device-title"><header><h4 id="add-device-title">{t("settings.addAuthenticator")}</h4><p>{t("settings.addAuthenticatorDescription")}</p></header><button type="button" className="button" onClick={(event) => { actionTrigger.current = event.currentTarget; setAddingAuthenticator(true); }}>{t("settings.addAction")}</button></section>}
    {status.data && addingAuthenticator && <form className="settings-form credential-form action-panel" aria-busy={create.isPending} onSubmit={(e) => { e.preventDefault(); create.mutate(); }}><h4>{t("settings.addAuthenticator")}</h4><Field label={t("settings.deviceName")}><input autoFocus required maxLength={64} value={name} onChange={(e) => { create.reset(); setName(e.target.value); }} /></Field><Field label={t("settings.currentPassword")}><input required type="password" autoComplete="current-password" value={password} onChange={(e) => { create.reset(); setPassword(e.target.value); }} /></Field>{status.data.enabled && <Field label={t("auth.authenticatorCode")} hint={t("settings.existingCodeHint")}><input required inputMode="numeric" pattern="[0-9]*" maxLength={8} autoComplete="one-time-code" value={approvalCode} onChange={(e) => { create.reset(); setApprovalCode(e.target.value); }} /></Field>}{create.isError && <ErrorState error={create.error} />}<div className="form-actions"><button type="button" className="button ghost" onClick={() => setAddingAuthenticator(false)}>{t("common.cancel")}</button><button className="button primary" disabled={create.isPending}>{create.isPending ? t("settings.saving") : t("settings.continue")}</button></div></form>}
    {enrollment && <form className="settings-form credential-form action-panel enrollment-form" aria-busy={confirm.isPending || cancelEnrollment.isPending} onSubmit={(e) => { e.preventDefault(); confirm.mutate(); }}><div className="notice warning"><strong>{t("settings.scanAuthenticator")}</strong><EnrollmentQR uri={enrollment.otpauth_uri} /><span>{t("settings.manualSecret")}: <code>{enrollment.secret}</code></span><small>{t("settings.enrollmentExpires", { value: formatDate(enrollment.expires_at) })}</small></div><Field label={t("auth.authenticatorCode")} hint={t("settings.authenticatorCodeHint")}><input autoFocus required inputMode="numeric" pattern="[0-9]*" maxLength={8} autoComplete="one-time-code" value={confirmCode} onChange={(e) => setConfirmCode(e.target.value)} /></Field>{confirm.isError && <ErrorState error={confirm.error} />}{cancelEnrollment.isError && <ErrorState error={cancelEnrollment.error} />}<div className="form-actions"><button className="button primary" disabled={confirm.isPending||cancelEnrollment.isPending}>{t("auth.verify")}</button><button type="button" className="button ghost" disabled={cancelEnrollment.isPending} onClick={()=>cancelEnrollment.mutate()}>{t("settings.cancelEnrollment")}</button></div></form>}
    {recovery.length > 0 && <div ref={recoveryRegion} tabIndex={-1} role="region" aria-labelledby="recovery-codes-title" className="notice warning recovery-codes"><strong id="recovery-codes-title">{t("settings.saveRecoveryCodes")}</strong><pre>{recovery.join("\n")}</pre><div aria-live="polite">{copyStatus}</div><div className="form-actions"><button type="button" className="button" onClick={() => void copyRecovery()}>{t("common.copy")}</button><button type="button" className="button" onClick={() => { downloadRecoveryCodes(recovery); setRecoverySaved(true); }}>{t("common.download")}</button></div><label className="check-row"><input type="checkbox" disabled={!recoverySaved} checked={recoveryConfirmed} onChange={(event) => setRecoveryConfirmed(event.target.checked)} /> {t("settings.confirmRecoverySaved")}</label><button type="button" className="button primary" disabled={!recoverySaved || !recoveryConfirmed} onClick={() => { setRecovery([]); setCopyStatus(""); setRecoverySaved(false); setRecoveryConfirmed(false); }}>{t("settings.savedRecoveryCodes")}</button></div>}
    {status.data?.enabled && !actionOpen && <section className="mfa-subsection" aria-labelledby="recovery-title"><header><h4 id="recovery-title">{t("settings.recoveryMethods")}</h4><p>{t("settings.recoveryMethodsDescription")}</p></header><button type="button" className="button" onClick={() => setRegenerateState((v) => ({ ...v, open: true }))}>{t("settings.regenerateRecovery")}</button></section>}
    {regenerateState.open && <Modal dangerous title={t("settings.regenerateRecovery")} onClose={() => setRegenerateState({ open: false, password: "", code: "" })}><form className="settings-form credential-form" onSubmit={(e) => { e.preventDefault(); regenerate.mutate(); }}><p>{t("settings.regenerateRecoveryWarning")}</p><Field label={t("settings.currentPassword")}><input autoFocus required type="password" autoComplete="current-password" value={regenerateState.password} onChange={(e) => setRegenerateState((v) => ({ ...v, password: e.target.value }))} /></Field><Field label={t("auth.authenticatorCode")}><input required inputMode="numeric" pattern="[0-9]*" maxLength={8} autoComplete="one-time-code" value={regenerateState.code} onChange={(e) => setRegenerateState((v) => ({ ...v, code: e.target.value }))} /></Field>{regenerate.isError && <ErrorState error={regenerate.error} />}<div className="form-actions"><button className="button danger" disabled={regenerate.isPending}>{t("settings.regenerateRecovery")}</button><button type="button" className="button ghost" onClick={() => setRegenerateState({ open: false, password: "", code: "" })}>{t("common.cancel")}</button></div></form></Modal>}
    {status.data?.enabled && status.data.policy === "optional" && !recovery.length && <section className="mfa-subsection danger-zone" aria-labelledby="danger-title"><header><h4 id="danger-title">{t("settings.dangerZone")}</h4><p>{t("settings.disableMFAWarning")}</p></header>{!disableState.open ? <button type="button" className="button danger" disabled={actionOpen} onClick={() => setDisableState((v) => ({ ...v, open: true }))}>{t("settings.disableMFA")}</button> : <Modal dangerous title={t("settings.disableMFA")} onClose={() => setDisableState({ open: false, password: "", code: "", confirmed: false })}><form className="settings-form credential-form danger-panel" onSubmit={(e) => { e.preventDefault(); disable.mutate(); }}><p>{t("settings.disableMFAWarning")}</p><Field label={t("settings.currentPassword")}><input autoFocus required type="password" autoComplete="current-password" value={disableState.password} onChange={(e) => setDisableState((v) => ({ ...v, password: e.target.value }))} /></Field><Field label={t("auth.authenticatorCode")}><input required inputMode="numeric" pattern="[0-9]*" maxLength={8} autoComplete="one-time-code" value={disableState.code} onChange={(e) => setDisableState((v) => ({ ...v, code: e.target.value }))} /></Field><label className="check-row"><input type="checkbox" checked={disableState.confirmed} onChange={(e) => setDisableState((v) => ({ ...v, confirmed: e.target.checked }))} /> {t("settings.confirmDisableMFA")}</label>{disable.isError && <ErrorState error={disable.error} />}<div className="form-actions"><button className="button danger" disabled={!disableState.confirmed || disable.isPending}>{t("settings.disableMFA")}</button><button type="button" className="button ghost" onClick={() => setDisableState({ open: false, password: "", code: "", confirmed: false })}>{t("common.cancel")}</button></div></form></Modal>}</section>}
  </section>;
}

function EnrollmentQR({uri}:{uri:string}) { const {t}=useTranslation(); const [source,setSource]=useState(""); useEffect(()=>{let active=true;void QRCode.toDataURL(uri,{errorCorrectionLevel:"M",margin:2,width:240}).then((value)=>{if(active)setSource(value)});return()=>{active=false;setSource("")}},[uri]);return source?<img src={source} width={240} height={240} alt={t("settings.enrollmentQRAlt")}/>:<Loading/> }

function downloadRecoveryCodes(codes:string[]){const url=URL.createObjectURL(new Blob([`Heimdall recovery codes\n\n${codes.join("\n")}\n`],{type:"text/plain"}));const link=document.createElement("a");link.href=url;link.download="heimdall-recovery-codes.txt";link.click();URL.revokeObjectURL(url)}

export function PasswordChangeForm() {
  const { t } = useTranslation();
  const [editing, setEditing] = useState(false);
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [validationError, setValidationError] = useState("");
  const queryClient = useQueryClient();
  const mutation = useMutation({
    mutationFn: () => api.changePassword(currentPassword, newPassword),
    onSuccess: () => {
      setCurrentPassword("");
      setNewPassword("");
      setConfirmation("");
      setEditing(false);
      void queryClient.invalidateQueries({ queryKey: ["session"] });
    },
  });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    setValidationError("");
    if (passwordCharacterCount(newPassword) < MIN_PASSWORD_CHARACTERS) {
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
    <section className="panel settings-card password-settings">
      <header className="panel-header">
        <div><p className="eyebrow">{t("navigation.localAdmin")}</p><h3>{t("settings.changePassword")}</h3><p>{t("settings.otherSessionsEnd")}</p></div>
        {!editing && <button type="button" className="button" onClick={() => setEditing(true)}>{t("settings.changePassword")}</button>}
      </header>
      {mutation.isSuccess && <div className="notice success" role="status"><strong>{t("settings.passwordChanged")}</strong></div>}
      {editing && <form className="settings-form credential-form action-panel" aria-busy={mutation.isPending} onSubmit={submit} autoComplete="off">
        <Field label={t("settings.currentPassword")}><input type="password" autoComplete="current-password" required value={currentPassword} onChange={(event) => { mutation.reset(); setCurrentPassword(event.target.value); }} /></Field>
        <Field label={t("settings.newPassword")} hint={t("settings.newPasswordHint")}><input type="password" autoComplete="new-password" required value={newPassword} onChange={(event) => { mutation.reset(); setNewPassword(event.target.value); }} /></Field>
        <Field label={t("settings.confirmNewPassword")}><input type="password" autoComplete="new-password" required value={confirmation} onChange={(event) => { mutation.reset(); setConfirmation(event.target.value); }} /></Field>
        {validationError && <div className="notice warning" role="alert"><strong>{validationError}</strong></div>}
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions"><button type="button" className="button ghost" disabled={mutation.isPending} onClick={() => { setEditing(false); setValidationError(""); }}>{t("common.cancel")}</button><button className="button primary" disabled={mutation.isPending || !currentPassword || !newPassword || !confirmation}>{mutation.isPending ? t("settings.saving") : t("settings.changePassword")}</button></div>
      </form>}
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
    <section className="panel settings-card runtime-settings">
      <header className="panel-header">
        <div><p className="eyebrow">{t("settings.runtimeEyebrow")}</p><h3>{t("settings.runtimeTitle")}</h3></div>
        <span className="badge">{t("settings.hotApplied")}</span>
      </header>
      <form className="settings-form runtime-form" aria-busy={mutation.isPending} onSubmit={submit}>
        <Field label={t("settings.probeInterval")} hint={t("settings.probeHint")}>
          <input type="number" min="10" max="3600" required value={interval} onChange={(event) => { mutation.reset(); setInterval(Number(event.target.value)); }} />
        </Field>
        <p className="runtime-note"><strong>{t("settings.startupLocked")}</strong><span>{t("settings.startupDescription")}</span></p>
        {mutation.isError && <ErrorState error={mutation.error} />}
        {mutation.isSuccess && <div className="notice success" role="status"><strong>{t("settings.runtimeSaved")}</strong></div>}
        <div className="form-actions"><button className="button primary" disabled={mutation.isPending || interval === settings.health_probe_interval_seconds}>{mutation.isPending ? t("settings.saving") : t("settings.saveRuntime")}</button></div>
      </form>
    </section>
  );
}

/**
 * The accounting timezone: where a day ends for daily budgets and for every
 * "today" figure in the console.
 *
 * Deliberately verbose about consequences. A change here moves money between
 * days, so the confirmation states the exact instant it takes effect, that the
 * period in progress is untouched, and that nothing already recorded is
 * recomputed — an administrator should not have to infer any of that.
 */
export function AccountingTimezoneForm({ settings }: { settings: AccountingSettings }) {
  const { t } = useTranslation();
  const formatInstant = useInstantFormatter();
  const queryClient = useQueryClient();
  const [timezone, setTimezone] = useState(settings.pending_timezone || settings.timezone);
  const [confirming, setConfirming] = useState(false);
  // Fetched only once the administrator opens the confirmation, so the warning
  // describes the zone actually about to be committed.
  const preview = useQuery({
    queryKey: ["accounting-preview", timezone.trim()],
    queryFn: () => api.previewAccountingTimezone(timezone.trim()),
    enabled: confirming && timezone.trim() !== "" && timezone.trim() !== settings.timezone,
  });
  useEffect(() => setTimezone(settings.pending_timezone || settings.timezone), [settings.pending_timezone, settings.timezone]);
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["accounting-settings"] });
  const schedule = useMutation({
    mutationFn: () => api.scheduleAccountingTimezone(timezone.trim(), settings.revision),
    onSuccess: () => { setConfirming(false); return refresh(); },
  });
  const cancel = useMutation({
    mutationFn: () => api.cancelAccountingTimezoneChange(settings.revision),
    onSuccess: refresh,
  });
  const changed = timezone.trim() !== "" && timezone.trim() !== settings.timezone;
  const busy = schedule.isPending || cancel.isPending;
  return (
    <section className="panel settings-card">
      <header className="panel-header">
        <div><p className="eyebrow">{t("settings.accountingEyebrow")}</p><h3>{t("settings.accountingTitle")}</h3></div>

      </header>
      <dl className="settings-facts">
        <div><dt>{t("settings.accountingCurrentZone")}</dt><dd><code>{settings.timezone}</code></dd></div>
        <div>
          <dt>{t("settings.accountingVersionLabel")}</dt>
          <dd>
            {settings.timezone_version}
            <small>{t("settings.accountingVersionHint")}</small>
          </dd>
        </div>
        <div>
          <dt>{t("settings.accountingCurrentPeriod")}</dt>
          <dd>{t("settings.accountingPeriodRange", {
            start: formatInstant(settings.current_period.period_start, "full"),
            end: formatInstant(settings.current_period.period_end, "full"),
          })}</dd>
        </div>
        {settings.tzdata && (
          <div>
            <dt>{t("settings.accountingRules")}</dt>
            <dd>{t("settings.accountingRulesValue", { source: settings.tzdata.source, version: settings.tzdata.version })}<br /><code>{settings.tzdata.fingerprint}</code></dd>
          </div>
        )}
      </dl>
      {/* config.yaml seeds the zone once; after that the stored setting decides.
          Saying so prevents an operator editing the file and assuming it took. */}
      {!settings.config_file_in_effect && (
        <div className="notice warning" role="status">
          <strong>{t("settings.accountingConfigIgnoredTitle")}</strong>
          <span>{t("settings.accountingConfigIgnored", { timezone: settings.config_file_timezone })}</span>
        </div>
      )}
      {settings.pending_timezone && settings.pending_effective_at && (
        <div className="notice" role="status">
          <strong>{t("settings.accountingPendingTitle", { timezone: settings.pending_timezone })}</strong>
          <span>{t("settings.accountingPendingDetail", { at: formatInstant(settings.pending_effective_at, "full") })}</span>
          <button className="button ghost" disabled={busy} onClick={() => cancel.mutate()}>{t("settings.accountingCancelPending")}</button>
        </div>
      )}
      <form className="settings-form" aria-busy={busy} onSubmit={(event) => { event.preventDefault(); setConfirming(true); }}>
        <Field label={t("settings.accountingZoneLabel")} hint={t("settings.accountingZoneHint")}>
          <input
            required
            value={timezone}
            spellCheck={false}
            onChange={(event) => { schedule.reset(); setTimezone(event.target.value); }}
          />
        </Field>
        {schedule.isError && <ErrorState error={schedule.error} />}
        {cancel.isError && <ErrorState error={cancel.error} />}
        <div className="form-actions">
          <button className="button primary" disabled={busy || !changed}>{t("settings.accountingSchedule")}</button>
        </div>
      </form>
      {confirming && (
        <Modal title={t("settings.accountingConfirmTitle")} onClose={() => setConfirming(false)}>
          {/* The modal only pads a child it recognises; bare content runs to
              the edges. .confirmation-dialog is that child. */}
          <div className="confirmation-dialog">
          <p>{t("settings.accountingConfirmLead", { from: settings.timezone, to: timezone.trim() })}</p>
          <ul className="settings-consequences">
            <li>{t("settings.accountingConfirmEffective", {
              at: formatInstant(settings.current_period.period_end, "full"),
              utc: settings.current_period.period_end,
            })}</li>
            <li>{t("settings.accountingConfirmInProgress")}</li>
            <li>{t("settings.accountingConfirmNoRecompute")}</li>
          </ul>
          {/* The switch starts a fresh balance, so the daily budget resets again
              this soon afterwards — often well under a day. Nobody works that
              out from the zone names alone. */}
          {preview.data?.data.switch_preview && (
            <div className="notice warning" role="status">
              <strong>{t("settings.accountingResetWindowTitle", {
                hours: Math.round(preview.data.data.switch_preview.next_reset_in_hours),
              })}</strong>
              <span>{t("settings.accountingResetWindowDetail", {
                at: formatInstant(preview.data.data.switch_preview.next_reset_at, "full"),
                utc: preview.data.data.switch_preview.next_reset_at,
              })}</span>
            </div>
          )}
          <div className="form-actions">
            <button className="button" onClick={() => setConfirming(false)}>{t("common.cancel")}</button>
            <button className="button primary" disabled={busy} onClick={() => schedule.mutate()}>
              {schedule.isPending ? t("settings.saving") : t("settings.accountingConfirmAction")}
            </button>
          </div>
          </div>
        </Modal>
      )}
    </section>
  );
}

export function LanguageSettingsForm(props: { ui: InstanceUISettings; preferences: AdminPreferences }) {
  return <><PersonalLanguageForm {...props} /><InstanceLanguageForm {...props} /></>;
}

const APPEARANCE_OPTIONS: Appearance[] = ["light", "dark"];

export function AppearanceForm({ preferences }: { preferences: AdminPreferences }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [selected, setSelected] = useState<Appearance>(preferences.appearance);
  const [status, setStatus] = useState<"idle" | "saving" | "saved" | "error">("idle");
  const [errorMessage, setErrorMessage] = useState<string>("");
  // Refs hold the last server-confirmed truth so the async save chain never
  // reads a stale closure. Nothing here touches browser storage (PRD §10.2).
  const confirmed = useRef<Appearance>(preferences.appearance);
  const revision = useRef<number>(preferences.revision);
  const locale = useRef<LocalePreference>(preferences.locale);
  const seq = useRef(0); // monotonic request id; only the latest may win
  const saving = useRef(false);
  const queued = useRef<Appearance | null>(null); // last explicit choice while saving
  const failedTarget = useRef<Appearance | null>(null);

  // Re-sync to server truth when the preferences query changes and we are idle.
  useEffect(() => {
    if (saving.current) return;
    confirmed.current = preferences.appearance;
    revision.current = preferences.revision;
    locale.current = preferences.locale;
    setSelected(preferences.appearance);
  }, [preferences.appearance, preferences.revision, preferences.locale]);

  const rollback = async (message: string, target: Appearance) => {
    saving.current = false;
    queued.current = null;
    failedTarget.current = target;
    setSelected(confirmed.current);
    applyAppearance(confirmed.current);
    setErrorMessage(message);
    // The server may have advanced its revision even when it returned an
    // error (for example a compensated Audit failure), or another tab may have
    // won a revision conflict. Re-fetch before enabling Retry.
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ["preferences"] }),
      queryClient.invalidateQueries({ queryKey: ["session"] }),
    ]);
    setStatus("error");
  };

  const runSave = (target: Appearance) => {
    saving.current = true;
    const id = ++seq.current;
    setStatus("saving");
    void api
      .updatePreferences({ locale: locale.current, appearance: target }, revision.current)
      .then((result) => {
        if (id !== seq.current && queued.current === null) return; // superseded response
        confirmed.current = result.data.appearance;
        revision.current = result.data.revision;
        // A newer explicit choice arrived while this save was in flight: chain it
        // so the final server state matches the admin's last selection.
        if (queued.current !== null && queued.current !== result.data.appearance) {
          const next = queued.current;
          queued.current = null;
          runSave(next);
          return;
        }
        queued.current = null;
        failedTarget.current = null;
        saving.current = false;
        setStatus("saved");
        void queryClient.invalidateQueries({ queryKey: ["preferences"] });
        void queryClient.invalidateQueries({ queryKey: ["session"] });
      })
      .catch((err) => {
        if (id !== seq.current && queued.current === null) return;
        void rollback(err instanceof Error ? err.message : t("settings.appearance.error"), target);
      });
  };

  const retry = () => {
    const target = failedTarget.current;
    if (!target) return;
    setSelected(target);
    applyAppearance(target);
    setErrorMessage("");
    runSave(target);
  };

  const choose = (value: Appearance) => {
    if (value === selected && status !== "error") return;
    setSelected(value);
    applyAppearance(value); // instant preview (PRD §4.5)
    setErrorMessage("");
    if (saving.current) {
      queued.current = value; // last explicit choice wins
    } else {
      runSave(value);
    }
  };

  return (
    <section className="panel settings-card appearance-settings">
      <header className="panel-header">
        <div><p className="eyebrow">{t("settings.personalScope")}</p><h3>{t("settings.appearance.title")}</h3></div>
        <span className="badge">{t("settings.onlyYou")}</span>
      </header>
      <p className="panel-description">{t("settings.appearance.description")}</p>
      <fieldset className="appearance-choices" aria-busy={status === "saving"}>
        <legend className="sr-only">{t("settings.appearance.legend")}</legend>
        {APPEARANCE_OPTIONS.map((value) => (
          <label key={value} className={`appearance-option ${selected === value ? "selected" : ""}`}>
            <input
              type="radio"
              name="appearance"
              value={value}
              checked={selected === value}
              onChange={() => choose(value)}
            />
            <span className={`appearance-preview appearance-preview-${value}`} aria-hidden="true">
              <span className="appearance-preview-bar" />
              <span className="appearance-preview-body" />
            </span>
            <span className="appearance-option-name">
              {t(`settings.appearance.${value}`)}
              {selected === value && <span className="appearance-check" aria-hidden="true">✓</span>}
            </span>
          </label>
        ))}
      </fieldset>
      <div className="appearance-status" aria-live="polite">
        {status === "saving" && <span className="muted">{t("settings.appearance.saving")}</span>}
        {status === "saved" && <div className="notice success" role="status"><strong>{t("settings.appearance.saved")}</strong></div>}
        {status === "error" && (
          <div className="notice error" role="alert">
            <strong>{t("settings.appearance.error")}</strong>
            {errorMessage && <span> — {errorMessage}</span>}
            <button type="button" className="button ghost small" onClick={retry}>{t("settings.appearance.retry")}</button>
          </div>
        )}
      </div>
    </section>
  );
}

function PersonalLanguageForm({ ui, preferences }: { ui: InstanceUISettings; preferences: AdminPreferences }) {
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

function InstanceLanguageForm({ ui, preferences }: { ui: InstanceUISettings; preferences: AdminPreferences }) {
  const { t } = useTranslation();
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
        <div className="form-actions"><button className="button primary" disabled={instanceMutation.isPending || !instanceChanged}>{instanceMutation.isPending ? t("settings.saving") : t("settings.saveInstanceLanguage")}</button></div>
      </form>
    </section>
  );
}
