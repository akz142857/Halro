import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { ErrorState, Loading, PageHeader, StatusDot } from "../components";

export function MasterKeyCustodyPage() {
  const { t, i18n } = useTranslation();
  const custody = useQuery({ queryKey: ["master-key-custody"], queryFn: api.masterKeyCustody });
  const date = (value?: string) => value ? new Intl.DateTimeFormat(i18n.language, { dateStyle: "medium", timeStyle: "short" }).format(new Date(value)) : t("common.never");
  return <>
    <PageHeader eyebrow={t("custody.eyebrow")} title={t("custody.title")} description={t("custody.description")} />
    {custody.isPending && <Loading />}
    {custody.isError && <ErrorState error={custody.error} />}
    {custody.data && <div className="settings-pane">
      <section className="settings-overview" aria-labelledby="custody-posture">
        <div className="settings-overview-copy">
          <p className="eyebrow">{t("custody.posture")}</p>
          <h2 id="custody-posture"><StatusDot ok={custody.data.production_ready && !custody.data.rotation_incomplete} />{custody.data.production_ready ? t("custody.ready") : t("custody.notReady")}</h2>
          <p>{custody.data.rotation_incomplete ? t("custody.rotationIncomplete", { pending: custody.data.pending_slots, retiring: custody.data.retiring_slots }) : t("custody.stable")}</p>
        </div>
        <span className="badge">{t("custody.mode")}: {custody.data.mode}</span>
      </section>
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
      <section aria-labelledby="custody-runbooks">
        <header className="settings-group-header"><h2 id="custody-runbooks">{t("custody.runbooks")}</h2><p>{t("custody.offlineOnly")}</p></header>
        <div className="panel"><code>{custody.data.lifecycle_runbook}</code><br /><code>{custody.data.recovery_runbook}</code></div>
      </section>
    </div>}
  </>;
}
