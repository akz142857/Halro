import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNotify } from "../notifications";
import QRCode from "qrcode";
import { api } from "../api";
import { ErrorState, Field, Loading, Modal } from "../components";
import { useInstantFormatter } from "../format";
import { setNavigationBlocked } from "../navigation";

export function MFASettings({ username = "" }: { username?: string }) {
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

  const { notify } = useNotify();
  const refresh = async () => { await queryClient.invalidateQueries({ queryKey: ["mfa"] }); await queryClient.invalidateQueries({ queryKey: ["session"] }); };
  const create = useMutation({ mutationFn: () => api.createMFAAuthenticator(name, password, approvalCode), onSettled: () => { setPassword(""); setApprovalCode(""); }, onSuccess: (value) => { setAddingAuthenticator(false); setEnrollmentExpired(false); setEnrollment(value); } });
  const confirm = useMutation({ mutationFn: () => api.confirmMFAAuthenticator(enrollment!.id, confirmCode), onSettled: () => setConfirmCode(""), onSuccess: async (value) => { setRecoverySaved(false); setRecoveryConfirmed(false); setRecovery(value.recovery_codes || []); setEnrollment(null); setName(""); await refresh(); } });
  const cancelEnrollment = useMutation({ mutationFn: () => api.cancelPendingMFAAuthenticator(enrollment!.id), onSuccess: () => { setEnrollment(null); setConfirmCode(""); restoreActionFocus(); notify({ tone: "success", title: t("settings.notifyEnrollmentCancelled") }); }, onError: () => { /* keep the secret visible until server cancellation succeeds */ } });
  const restoreActionFocus = () => {
    const target = actionTrigger.current || document.getElementById("mfa-title");
    actionTrigger.current = null;
    window.setTimeout(() => target?.focus(), 0);
  };
  const revoke = useMutation({ mutationFn: () => api.deleteMFAAuthenticator(revokeState.id, revokeState.password, revokeState.code), onSuccess: async () => { setRevokeState({ id: "", password: "", code: "" }); await refresh(); restoreActionFocus(); notify({ tone: "success", title: t("settings.notifyAuthenticatorRevoked") }); } });
  const regenerate = useMutation({ mutationFn: () => api.regenerateMFARecoveryCodes(regenerateState.password, regenerateState.code), onSuccess: async (value) => { setRegenerateState({ open: false, password: "", code: "" }); setRecoverySaved(false); setRecoveryConfirmed(false); setRecovery(value.recovery_codes); await refresh(); } });
  const disable = useMutation({ mutationFn: () => api.disableMFA(disableState.password, disableState.code), onSuccess: async () => { setDisableState({ open: false, password: "", code: "", confirmed: false }); await refresh(); restoreActionFocus(); notify({ tone: "warning", title: t("settings.notifyMFADisabled"), description: t("settings.disableMFAWarning") }); } });
  const rename = useMutation({ mutationFn: () => api.renameMFAAuthenticator(renameState.id, renameState.name, renameState.revision), onSuccess: async () => { const renamed = renameState.name; setRenameState({ id: "", name: "", revision: 0 }); await queryClient.invalidateQueries({ queryKey: ["mfa"] }); restoreActionFocus(); notify({ tone: "success", title: t("settings.notifyAuthenticatorRenamed"), description: renamed }); } });
  const copyRecovery = async () => { try { await navigator.clipboard.writeText(recovery.join("\n")); setRecoverySaved(true); setCopyStatus(t("settings.recoveryCopied")); } catch { setCopyStatus(t("settings.recoveryCopyFailed")); } };
  const formatDate = (value?: string) => value ? formatInstant(value, "full") : t("common.never");

  const actionOpen = Boolean(addingAuthenticator || enrollment || recovery.length || renameState.id || revokeState.id || regenerateState.open || disableState.open);
  useEffect(() => {
    if (actionWasOpen.current && !actionOpen) restoreActionFocus();
    actionWasOpen.current = actionOpen;
  }, [actionOpen]);
  return <section className="panel settings-card mfa-settings" aria-labelledby="mfa-title" aria-busy={status.isPending}>
    <header className="panel-header"><div><p className="eyebrow">{t("settings.currentAccount")}{username ? ` · ${username}` : ""}</p><h3 id="mfa-title" tabIndex={-1}>{t("settings.mfaTitle")}</h3><p>{t("settings.mfaDescription")}</p></div>{status.data ? <span className={`badge ${status.data.enabled ? "good" : ""}`}>{status.data.enabled ? t("settings.mfaEnabled") : t(`settings.mfaPolicy.${status.data.policy}`)}</span> : <span className="badge">{t("common.loading")}</span>}</header>
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

function downloadRecoveryCodes(codes:string[]){const url=URL.createObjectURL(new Blob([`Halro recovery codes\n\n${codes.join("\n")}\n`],{type:"text/plain"}));const link=document.createElement("a");link.href=url;link.download="halro-recovery-codes.txt";link.click();URL.revokeObjectURL(url)}
