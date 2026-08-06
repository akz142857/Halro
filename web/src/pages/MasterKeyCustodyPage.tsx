import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { ErrorState, Loading, PageHeader, StatusDot } from "../components";
import { useInstantFormatter } from "../format";

export function MasterKeyCustodyPage() {
  const { t } = useTranslation();
  const custody = useQuery({ queryKey: ["master-key-custody"], queryFn: api.masterKeyCustody });
  const formatInstant = useInstantFormatter();
  const date = (value?: string) => {
    if (!value) return t("common.never");
    const formatted = formatInstant(value, "full");
    return formatted === "—" ? t("common.unknown") : formatted;
  };
  const retry = () => { void custody.refetch(); };
  const data = custody.data;
  const isFile = data?.mode === "file";
  const lastUpdated = custody.dataUpdatedAt > 0 ? date(new Date(custody.dataUpdatedAt).toISOString()) : t("common.unknown");
  const runbooksAvailable = data?.mode === "key_slots" && Boolean(data.lifecycle_runbook_url || data.recovery_runbook_url);

  return <div className="custody-page">
    <PageHeader
      eyebrow={t("custody.eyebrow")}
      title={t("custody.title")}
      description={isFile ? t("custody.fileDescription") : t("custody.description")}
      action={data && <div className="custody-refresh">
        <span>{t("custody.lastUpdated", { value: lastUpdated })}</span>
        <button className="button ghost" type="button" disabled={custody.isFetching} onClick={retry}>
          {custody.isFetching ? t("custody.refreshing") : t("custody.refresh")}
        </button>
      </div>}
    />
    {custody.isPending && <Loading />}
    {custody.isError && !data && <div className="custody-load-error">
      <ErrorState error={custody.error} />
      <button className="button ghost" type="button" disabled={custody.isFetching} onClick={retry}>{t("common.retry")}</button>
    </div>}
    {data && <div className="settings-pane custody-pane">
      {custody.isError && <div className="notice warning custody-stale-notice" role="status" aria-live="polite">
        <span><strong>{t("custody.staleTitle")}</strong>{t("custody.staleDescription", { updatedAt: lastUpdated })}</span>
        <button className="button ghost" type="button" disabled={custody.isFetching} onClick={retry}>{custody.isFetching ? t("custody.refreshing") : t("common.retry")}</button>
      </div>}

      <section className={`panel custody-summary ${data.custody_state}`} aria-labelledby="custody-posture">
        <div className="custody-summary-primary">
          <div>
            <p className="eyebrow">{t("custody.posture")}</p>
            <h2 id="custody-posture"><StatusDot ok={data.custody_state === "healthy"} />{isFile
              ? (data.local_custody_ready ? t("custody.fileKeyReady") : t("custody.fileKeyNotReady"))
              : (data.custody_state === "degraded" ? t("custody.kmsReviewRequired") : (data.local_custody_ready ? t("custody.descriptorReady") : t("custody.descriptorNotReady")))}</h2>
            <p>{data.rotation_incomplete ? t("custody.rotationIncomplete", { pending: data.pending_slots, retiring: data.retiring_slots }) : t("custody.stable")}</p>
          </div>
          <span className="badge custody-mode-badge">{isFile ? t("custody.modeFile") : t("custody.modeKeySlots")}</span>
        </div>
        <dl className={`custody-facts ${isFile ? "file" : ""}`}>
          {!isFile && <div><dt>{t("custody.operation")}</dt><dd>{t(`custody.operations.${data.lifecycle_operation}`)}</dd></div>}
          {!isFile && <div><dt>{t("custody.recoveryExpiry")}</dt><dd>{t(`custody.recoveryStatuses.${data.recovery_verification_status}`)}</dd></div>}
          <div><dt>{t("custody.updated")}</dt><dd>{lastUpdated}</dd></div>
        </dl>
        <p className="custody-admission">{data.production_admission === "external_evidence_required" ? t("custody.externalAdmission") : t("custody.admissionNotApplicable")}</p>
      </section>

      {!isFile && data.custody_state === "degraded" && <div className="notice warning custody-review" role="status">
        <strong>{t("custody.reviewRequired")}</strong>
        <span>{data.degraded_reasons.map((reason) => t(`custody.reasons.${reason}`)).join(" · ")}</span>
      </div>}

      {!isFile && <section className="custody-section" aria-labelledby="custody-slots">
        <header className="settings-group-header custody-section-header">
          <div><h2 id="custody-slots">{t("custody.slots")}</h2><p>{t("custody.redaction")}</p></div>
          <span className="badge">{t("custody.slotCount", { count: data.slots.length })}</span>
        </header>
        {data.slots.length === 0 ? <div className="panel custody-slots-empty warning" role="alert">
          <strong>{t("custody.keySlotsMissing")}</strong>
          <span>{t("custody.keySlotsMissingDescription")}</span>
        </div> : <div className="panel custody-slot-list">
          {data.slots.map((slot, index) => <article className="custody-slot-row" key={`${slot.purpose}-${index}`}>
            <div className="custody-slot-identity">
              <h3>{t(`custody.${slot.purpose}`)}</h3>
              <span><span className="visually-hidden">{t("custody.provider")}: </span>{slot.provider}</span>
            </div>
            <dl>
              <div><dt>{t("custody.lastVerified")}</dt><dd>{date(slot.verified_at)}</dd></div>
              <div><dt>{t("custody.state")}</dt><dd className="custody-slot-state"><StatusDot ok={slot.state === "active"} />{t(`custody.states.${slot.state}`)}</dd></div>
            </dl>
          </article>)}
        </div>}
        {data.recovery_verified_at && <p className="custody-recovery-note">{t("custody.recoveryVerifiedSummary", { value: date(data.recovery_verified_at) })}</p>}
      </section>}

      {runbooksAvailable && <aside className="panel custody-runbook-bar" aria-labelledby="custody-runbooks">
        <div><h2 id="custody-runbooks">{t("custody.runbooks")}</h2><p>{t("custody.offlineOnly")}</p></div>
        <nav aria-label={t("custody.runbooks")}>
          {data.lifecycle_runbook_url && <a className="button ghost" href={data.lifecycle_runbook_url} target="_blank" rel="noreferrer">{t("custody.lifecycleRunbook")}<span className="visually-hidden"> {t("custody.newWindow")}</span></a>}
          {data.recovery_runbook_url && <a className="button ghost" href={data.recovery_runbook_url} target="_blank" rel="noreferrer">{t("custody.recoveryRunbook")}<span className="visually-hidden"> {t("custody.newWindow")}</span></a>}
        </nav>
      </aside>}
    </div>}
  </div>;
}
