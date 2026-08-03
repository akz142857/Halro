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
  return <>
    <PageHeader eyebrow={t("custody.eyebrow")} title={t("custody.title")} description={t("custody.description")} />
    {custody.isPending && <Loading />}
    {custody.isError && <ErrorState error={custody.error} />}
    {custody.data && <div className="settings-pane">
      <section className="settings-overview" aria-labelledby="custody-posture">
        <div className="settings-overview-copy">
          <p className="eyebrow">{t("custody.posture")}</p>
          <h2 id="custody-posture"><StatusDot ok={custody.data.custody_state === "healthy"} />{custody.data.descriptor_ready ? t("custody.descriptorReady") : t("custody.descriptorNotReady")}</h2>
          <p>{custody.data.rotation_incomplete ? t("custody.rotationIncomplete", { pending: custody.data.pending_slots, retiring: custody.data.retiring_slots }) : t("custody.stable")}</p>
          <p className="panel-description">{custody.data.production_admission === "external_evidence_required" ? t("custody.externalAdmission") : t("custody.admissionNotApplicable")}</p>
        </div>
        <span className="badge">{t("custody.mode")}: {custody.data.mode}</span>
      </section>
      {custody.data.custody_state === "degraded" && <div className="notice warning" role="status"><strong>{t("custody.reviewRequired")}</strong><span>{custody.data.degraded_reasons.map((reason) => t(`custody.reasons.${reason}`)).join(" · ")}</span></div>}
      <section aria-labelledby="custody-slots">
        <header className="settings-group-header"><h2 id="custody-slots">{t("custody.slots")}</h2><p>{t("custody.redaction")}</p></header>
        <div className="settings-grid">
          {custody.data.slots.map((slot, index) => <details className="panel system-card diagnostic-details" open key={`${slot.purpose}-${index}`}>
            <summary><span>{t(`custody.${slot.purpose}`)}</span><strong><StatusDot ok={slot.state === "active"} />{t(`custody.states.${slot.state}`)}</strong></summary>
            <dl>
              <div><dt>{t("custody.provider")}</dt><dd>{slot.provider}</dd></div>
              <div><dt>{t("custody.lastVerified")}</dt><dd>{date(slot.verified_at)}</dd></div>
            </dl>
          </details>)}
        </div>
      </section>
      <section className="panel" aria-labelledby="custody-lifecycle">
        <h2 id="custody-lifecycle">{t("custody.lifecycle")}</h2>
        <dl>
          <div><dt>{t("custody.operation")}</dt><dd>{t(`custody.operations.${custody.data.lifecycle_operation}`)}</dd></div>
          <div><dt>{t("custody.recoveryExpiry")}</dt><dd>{custody.data.recovery_verification_expired ? t("custody.expired") : t("custody.current")}</dd></div>
        </dl>
      </section>
      <section aria-labelledby="custody-runbooks">
        <header className="settings-group-header"><h2 id="custody-runbooks">{t("custody.runbooks")}</h2><p>{t("custody.offlineOnly")}</p></header>
        <div className="panel form-actions">
          <a className="button ghost" href={custody.data.lifecycle_runbook_url} target="_blank" rel="noreferrer">{t("custody.lifecycleRunbook")}</a>
          <a className="button ghost" href={custody.data.recovery_runbook_url} target="_blank" rel="noreferrer">{t("custody.recoveryRunbook")}</a>
        </div>
      </section>
    </div>}
  </>;
}
