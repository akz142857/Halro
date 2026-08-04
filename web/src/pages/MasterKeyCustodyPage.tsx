import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { ErrorState, Loading, PageHeader, StatusDot } from "../components";

export function MasterKeyCustodyPage() {
  const { t, i18n } = useTranslation();
  const custody = useQuery({ queryKey: ["master-key-custody"], queryFn: api.masterKeyCustody });
  const date = (value?: string) => {
    if (!value) return t("common.never");
    const parsed = new Date(value);
    return Number.isNaN(parsed.getTime()) ? t("common.unknown") : new Intl.DateTimeFormat(i18n.language, { dateStyle: "medium", timeStyle: "short" }).format(parsed);
  };
  const retry = () => { void custody.refetch(); };
  const data = custody.data;
  const isFile = data?.mode === "file";
  const runbooksAvailable = data?.mode === "key_slots" && Boolean(data.lifecycle_runbook_url || data.recovery_runbook_url);

  return <>
    <PageHeader eyebrow={t("custody.eyebrow")} title={t("custody.title")} description={t("custody.description")} />
    {custody.isPending && <Loading />}
    {custody.isError && !data && <div className="custody-load-error">
      <ErrorState error={custody.error} />
      <button className="button ghost" type="button" disabled={custody.isFetching} onClick={retry}>{t("common.retry")}</button>
    </div>}
    {data && <div className="settings-pane">
      {custody.isError && <div className="notice warning custody-stale-notice" role="status" aria-live="polite">
        <span><strong>{t("custody.staleTitle")}</strong>{t("custody.staleDescription", { updatedAt: date(new Date(custody.dataUpdatedAt).toISOString()) })}</span>
        <button className="button ghost" type="button" disabled={custody.isFetching} onClick={retry}>{custody.isFetching ? t("custody.refreshing") : t("common.retry")}</button>
      </div>}
      <section className="settings-overview" aria-labelledby="custody-posture">
        <div className="settings-overview-copy">
          <p className="eyebrow">{t("custody.posture")}</p>
          <h2 id="custody-posture"><StatusDot ok={data.custody_state === "healthy"} />{isFile
            ? (data.local_custody_ready ? t("custody.fileKeyReady") : t("custody.fileKeyNotReady"))
            : (data.local_custody_ready ? t("custody.descriptorReady") : t("custody.descriptorNotReady"))}</h2>
          <p>{data.rotation_incomplete ? t("custody.rotationIncomplete", { pending: data.pending_slots, retiring: data.retiring_slots }) : t("custody.stable")}</p>
          <p className="panel-description">{data.production_admission === "external_evidence_required" ? t("custody.externalAdmission") : t("custody.admissionNotApplicable")}</p>
        </div>
        <span className="badge custody-mode-badge">{t("custody.mode")}: {data.mode}</span>
      </section>
      {data.custody_state === "degraded" && <div className="notice warning" role="status"><strong>{t("custody.reviewRequired")}</strong><span>{data.degraded_reasons.map((reason) => t(`custody.reasons.${reason}`)).join(" · ")}</span></div>}
      <section aria-labelledby="custody-slots">
        <header className="settings-group-header"><h2 id="custody-slots">{t("custody.slots")}</h2><p>{t("custody.redaction")}</p></header>
        {data.slots.length === 0 ? <div className={`panel custody-slots-empty ${isFile ? "" : "warning"}`} role={isFile ? undefined : "alert"}>
          <strong>{isFile ? t("custody.fileSlotsEmpty") : t("custody.keySlotsMissing")}</strong>
          <span>{isFile ? t("custody.fileSlotsEmptyDescription") : t("custody.keySlotsMissingDescription")}</span>
        </div> : <div className="settings-grid">
          {data.slots.map((slot, index) => <details className="panel system-card diagnostic-details" open key={`${slot.purpose}-${index}`}>
            <summary><span>{t(`custody.${slot.purpose}`)}</span><strong><StatusDot ok={slot.state === "active"} />{t(`custody.states.${slot.state}`)}</strong></summary>
            <dl>
              <div><dt>{t("custody.provider")}</dt><dd>{slot.provider}</dd></div>
              <div><dt>{t("custody.lastVerified")}</dt><dd>{date(slot.verified_at)}</dd></div>
            </dl>
          </details>)}
        </div>}
      </section>
      <section className="panel custody-lifecycle" aria-labelledby="custody-lifecycle">
        <h2 id="custody-lifecycle">{t("custody.lifecycle")}</h2>
        <dl>
          <div><dt>{t("custody.operation")}</dt><dd>{t(`custody.operations.${data.lifecycle_operation}`)}</dd></div>
          <div><dt>{t("custody.recoveryExpiry")}</dt><dd>{t(`custody.recoveryStatuses.${data.recovery_verification_status}`)}</dd></div>
          {data.recovery_verified_at && <div><dt>{t("custody.recoveryVerifiedAt")}</dt><dd>{date(data.recovery_verified_at)}</dd></div>}
        </dl>
      </section>
      {runbooksAvailable && <section aria-labelledby="custody-runbooks">
        <header className="settings-group-header"><h2 id="custody-runbooks">{t("custody.runbooks")}</h2><p>{t("custody.offlineOnly")}</p></header>
        <div className="panel form-actions custody-runbook-actions">
          {data.lifecycle_runbook_url && <a className="button ghost" href={data.lifecycle_runbook_url} target="_blank" rel="noreferrer">{t("custody.lifecycleRunbook")}<span className="visually-hidden"> {t("custody.newWindow")}</span></a>}
          {data.recovery_runbook_url && <a className="button ghost" href={data.recovery_runbook_url} target="_blank" rel="noreferrer">{t("custody.recoveryRunbook")}<span className="visually-hidden"> {t("custody.newWindow")}</span></a>}
        </div>
      </section>}
    </div>}
  </>;
}
