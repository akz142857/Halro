import { useMemo } from "react";
import { useTranslation } from "react-i18next";
import { EmptyState } from "../../components";
import { money, useInstantFormatter } from "../../format";
import type { Outcome, Run, WorkUnit } from "../../types";
import { CopyableID, GovernanceBadge, WorkUnitStatusBadge } from "./GovernancePrimitives";

export type OverviewFilter = "all" | "open" | "active" | "at-risk" | "missing-outcome";

function missingFinalDefinitions(unit: WorkUnit, outcomes: Outcome[]) {
  const refs = unit.outcome_definitions ?? [];
  if (!refs.length) return [];
  return refs.filter((ref) => !outcomes.some((item) => item.work_unit_id === unit.id && item.definition_id === ref.id && item.definition_version === ref.version && !item.provisional));
}

function latestActivity(unit: WorkUnit, runs: Run[], outcomes: Outcome[]) {
  return [unit.created_at, unit.closed_at,
    ...runs.filter((run) => run.work_unit_id === unit.id).flatMap((run) => [run.created_at, run.closed_at]),
    ...outcomes.filter((outcome) => outcome.work_unit_id === unit.id).flatMap((outcome) => [outcome.observed_at, outcome.ingested_at]),
  ].filter(Boolean).sort((a, b) => Date.parse(b!) - Date.parse(a!))[0] as string;
}

export function GovernanceOverview({
  workUnits,
  runs,
  outcomes,
  onOpenWorkUnits,
}: {
  workUnits: WorkUnit[];
  runs: Run[];
  outcomes: Outcome[];
  onOpenWorkUnits: (filter: OverviewFilter, workUnitID?: string) => void;
}) {
  const { t } = useTranslation();
  const dateTime = useInstantFormatter();
  const openUnits = workUnits.filter((item) => item.status === "open");
  const activeRuns = runs.filter((item) => item.status === "active");
  const atRiskRuns = runs.filter((item) => item.budget_state !== "available");
  // The list API has no authoritative matured flag. Keep this deliberately
  // named and calculated as closed, and evaluate every frozen Definition.
  const missingOutcomeUnits = workUnits.filter((item) => item.status === "closed" && missingFinalDefinitions(item, outcomes).length > 0);
  const recentUnits = [...workUnits].sort((a, b) => Date.parse(latestActivity(b, runs, outcomes)) - Date.parse(latestActivity(a, runs, outcomes))).slice(0, 6);
  const attention = useMemo(() => {
    const rows: { key: string; workUnitID: string; title: string; detail: string; tone: "warning" | "danger" }[] = [];
    for (const run of atRiskRuns) rows.push({ key: `budget:${run.id}`, workUnitID: run.work_unit_id, title: t("runGovernance.attentionBudget", { state: t(`runGovernance.${run.budget_state}`) }), detail: run.id, tone: run.budget_state === "depleted" ? "danger" : "warning" });
    for (const unit of workUnits.filter((item) => (item.unknown_attempts ?? 0) > 0)) rows.push({ key: `cost:${unit.id}`, workUnitID: unit.id, title: t("runGovernance.attentionUnknownCost", { count: unit.unknown_attempts }), detail: unit.id, tone: "warning" });
    for (const unit of missingOutcomeUnits) rows.push({ key: `outcome:${unit.id}`, workUnitID: unit.id, title: t("runGovernance.attentionMissingOutcome", { count: missingFinalDefinitions(unit, outcomes).length }), detail: unit.id, tone: "warning" });
    return rows.slice(0, 8);
  }, [atRiskRuns, missingOutcomeUnits, outcomes, t, workUnits]);

  const metrics: { key: OverviewFilter; label: string; value: number; tone: "neutral" | "warning" }[] = [
    { key: "open", label: t("runGovernance.openWorkUnits"), value: openUnits.length, tone: "neutral" },
    { key: "active", label: t("runGovernance.activeRuns"), value: activeRuns.length, tone: "neutral" },
    { key: "at-risk", label: t("runGovernance.atRiskRuns"), value: atRiskRuns.length, tone: atRiskRuns.length ? "warning" : "neutral" },
    { key: "missing-outcome", label: t("runGovernance.missingFinalOutcomes"), value: missingOutcomeUnits.length, tone: missingOutcomeUnits.length ? "warning" : "neutral" },
  ];

  return <section className="governance-workspace" id="run-governance-panel" role="tabpanel" aria-labelledby="run-governance-tab-overview">
    <div className="governance-metrics" aria-label={t("runGovernance.overviewMetrics")}>
      {metrics.map((metric) => <button type="button" key={metric.key} className={`governance-metric ${metric.tone}`} onClick={() => onOpenWorkUnits(metric.key)}><span>{metric.label}</span><strong>{metric.value}</strong><small>{t("runGovernance.openFilteredView")}</small></button>)}
    </div>
    <div className="governance-overview-grid">
      <section className="governance-section">
        <header className="governance-section-header"><div><p className="eyebrow">{t("runGovernance.recentActivity")}</p><h2>{t("runGovernance.recentWorkUnits")}</h2></div><button type="button" className="button ghost governance-section-action" onClick={() => onOpenWorkUnits("all")}>{t("runGovernance.viewAll")}<span aria-hidden="true">→</span></button></header>
        {recentUnits.length === 0 ? <EmptyState title={t("runGovernance.noWorkUnits")}>{t("runGovernance.noWorkUnitsDescription")}</EmptyState> : <div className="governance-list">{recentUnits.map((unit) => {
          const unitRuns = runs.filter((run) => run.work_unit_id === unit.id);
          const activity = latestActivity(unit, runs, outcomes);
          return <article className="governance-list-row" key={unit.id}>
            <div className="governance-recent-identity"><span>{t("runGovernance.workUnit")}</span><CopyableID value={unit.id} label={t("runGovernance.workUnit")} /></div>
            <div className="governance-recent-actions"><WorkUnitStatusBadge status={unit.status} /><button type="button" className="button ghost governance-row-action" onClick={() => onOpenWorkUnits("all", unit.id)}>{t("runGovernance.viewDetails")}<span aria-hidden="true">→</span></button></div>
            <dl className="governance-recent-facts"><div><dt>{t("runGovernance.runs")}</dt><dd>{t("runGovernance.runCount", { count: unitRuns.length })}</dd></div><div><dt>{t("runGovernance.costEvidence")}</dt><dd>{unit.committed_micros_usd == null ? "—" : money(unit.committed_micros_usd)}</dd></div><div><dt>{t("runGovernance.lastActivity")}</dt><dd><time dateTime={activity}>{dateTime(activity, "full")}</time></dd></div></dl>
          </article>;
        })}</div>}
      </section>
      <section className="governance-section">
        <header className="governance-section-header"><div><p className="eyebrow">{t("runGovernance.attentionEyebrow")}</p><h2>{t("runGovernance.attentionQueue")}</h2></div><span className="governance-count">{attention.length}</span></header>
        {attention.length === 0 ? <div className="governance-clear-state" role="status"><span className="governance-clear-icon" aria-hidden="true"><svg viewBox="0 0 24 24"><path d="m6.5 12.5 3.5 3.5 7.5-8" /></svg></span><div><strong>{t("runGovernance.noAttentionNeeded")}</strong><p>{t("runGovernance.noAttentionDescription")}</p></div></div> : <div className="governance-list">{attention.map((item) => <article className="governance-attention-row" key={item.key}><GovernanceBadge tone={item.tone}>{item.title}</GovernanceBadge><CopyableID value={item.detail} label={t("runGovernance.workUnit")} /><button type="button" className="button ghost" onClick={() => onOpenWorkUnits("all", item.workUnitID)}>{t("runGovernance.viewDetails")}</button></article>)}</div>}
      </section>
    </div>
  </section>;
}
