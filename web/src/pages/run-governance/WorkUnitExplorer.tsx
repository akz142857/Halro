import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { EmptyState, ErrorState, Loading, Modal } from "../../components";
import { money, useInstantFormatter } from "../../format";
import type { Outcome, Run, UsageAttempt, WorkUnit } from "../../types";
import type { OverviewFilter } from "./GovernanceOverview";
import { BudgetStatusBadge, CopyableID, CostEvidence, GovernanceBadge, RunBudgetBar, RunStatusBadge, WorkUnitStatusBadge } from "./GovernancePrimitives";
import { shortID } from "./governance-state";

function currentOutcome(unit: WorkUnit, definitionID: string, version: number, outcomes: Outcome[]) {
  return outcomes.filter((item) => item.work_unit_id === unit.id && item.definition_id === definitionID && item.definition_version === version).sort((a, b) => b.governance_sequence - a.governance_sequence)[0];
}

function OutcomeSummary({ unit, outcomes }: { unit: WorkUnit; outcomes: Outcome[] }) {
  const { t } = useTranslation();
  const refs = unit.outcome_definitions ?? [];
  if (!refs.length) return <span className="governance-muted">{t("runGovernance.noFrozenDefinitions")}</span>;
  const current = refs.map((ref) => currentOutcome(unit, ref.id, ref.version, outcomes));
  const finalCount = current.filter((item) => item && !item.provisional).length;
  const provisionalCount = current.filter((item) => item?.provisional).length;
  return <span className="governance-outcome-summary"><strong>{t("runGovernance.finalOutcomeCount", { final: finalCount, total: refs.length })}</strong>{provisionalCount > 0 && <small>{t("runGovernance.provisionalOutcomeCount", { count: provisionalCount })}</small>}{finalCount < refs.length && <GovernanceBadge tone="warning">{t("runGovernance.outcomeMissing")}</GovernanceBadge>}</span>;
}

function latestActivity(unit: WorkUnit, runs: Run[], outcomes: Outcome[]) {
  return [unit.created_at, unit.closed_at,
    ...runs.filter((run) => run.work_unit_id === unit.id).flatMap((run) => [run.created_at, run.closed_at]),
    ...outcomes.filter((outcome) => outcome.work_unit_id === unit.id).flatMap((outcome) => [outcome.observed_at, outcome.ingested_at]),
  ].filter(Boolean).sort((a, b) => Date.parse(b!) - Date.parse(a!))[0] as string;
}

function OutcomeBadge({ outcome }: { outcome?: Outcome }) {
  const { t } = useTranslation();
  if (!outcome) return <GovernanceBadge tone="warning">{t("runGovernance.outcomeMissing")}</GovernanceBadge>;
  if (outcome.provisional) return <GovernanceBadge tone="warning">{t("runGovernance.outcomeProvisional")}</GovernanceBadge>;
  const accepted = ["accepted", "true", "passed", "success"].includes(outcome.value.toLowerCase());
  const label = outcome.value === "accepted" ? t("runGovernance.outcomeAccepted") : outcome.value === "rejected" ? t("runGovernance.outcomeRejected") : outcome.value;
  return <GovernanceBadge tone={accepted ? "good" : "danger"}>{label}</GovernanceBadge>;
}

export function WorkUnitExplorer({
  workUnits,
  runs,
  outcomes,
  selectedWorkUnitID,
  selectedRunID,
  initialFilter,
  attempts,
  attemptsPending,
  attemptsError,
  onSelectWorkUnit,
  onSelectRun,
  onCloseDetail,
}: {
  workUnits: WorkUnit[];
  runs: Run[];
  outcomes: Outcome[];
  selectedWorkUnitID: string;
  selectedRunID: string;
  initialFilter: OverviewFilter;
  attempts?: UsageAttempt[];
  attemptsPending: boolean;
  attemptsError: unknown;
  onSelectWorkUnit: (id: string) => void;
  onSelectRun: (id: string) => void;
  onCloseDetail: () => void;
}) {
  const { t } = useTranslation();
  const dateTime = useInstantFormatter();
  const [search, setSearch] = useState("");
  const [status, setStatus] = useState(initialFilter === "open" ? "open" : "");
  const selectedUnit = workUnits.find((item) => item.id === selectedWorkUnitID);
  const selectedRun = runs.find((item) => item.id === selectedRunID && item.work_unit_id === selectedWorkUnitID);
  const selectedUnitRuns = runs.filter((item) => item.work_unit_id === selectedWorkUnitID);
  const selectedOutcomes = outcomes.filter((item) => item.work_unit_id === selectedWorkUnitID).sort((a, b) => b.governance_sequence - a.governance_sequence);
  const filtered = useMemo(() => workUnits.filter((unit) => {
    const unitRuns = runs.filter((run) => run.work_unit_id === unit.id);
    if (status && unit.status !== status) return false;
    if (search && !`${unit.id} ${unit.created_by_key_id}`.toLowerCase().includes(search.toLowerCase())) return false;
    if (initialFilter === "active" && !unitRuns.some((run) => run.status === "active")) return false;
    if (initialFilter === "at-risk" && !unitRuns.some((run) => run.budget_state !== "available")) return false;
    if (initialFilter === "missing-outcome" && !(unit.status === "closed" && (unit.outcome_definitions ?? []).some((ref) => !outcomes.some((item) => item.work_unit_id === unit.id && item.definition_id === ref.id && item.definition_version === ref.version && !item.provisional)))) return false;
    return true;
  }), [initialFilter, outcomes, runs, search, status, workUnits]);

  return <section className="governance-workspace" id="run-governance-panel" role="tabpanel" aria-labelledby="run-governance-tab-work-units">
    <div className="governance-toolbar" aria-label={t("common.filters")}>
      <label className="governance-search"><span>{t("runGovernance.searchWorkUnits")}</span><input type="search" value={search} onChange={(event) => setSearch(event.target.value)} placeholder={t("runGovernance.searchWorkUnitsPlaceholder")} /></label>
      <label><span>{t("runGovernance.workUnitStatus")}</span><select value={status} onChange={(event) => setStatus(event.target.value)}><option value="">{t("runGovernance.allStatuses")}</option><option value="open">{t("runGovernance.open")}</option><option value="closed">{t("runGovernance.closed")}</option></select></label>
      {initialFilter !== "all" && <GovernanceBadge tone="neutral">{t(`runGovernance.filter.${initialFilter}`)}</GovernanceBadge>}
      <span className="governance-result-count">{t("runGovernance.resultCount", { count: filtered.length })}</span>
    </div>
    {filtered.length === 0 ? <EmptyState title={t("runGovernance.noWorkUnits")}>{t("runGovernance.noWorkUnitsDescription")}</EmptyState> : <div className="governance-table-shell"><table className="governance-table"><thead><tr><th>{t("runGovernance.workUnit")}</th><th>{t("runGovernance.lifecycle")}</th><th>{t("runGovernance.runs")}</th><th>{t("runGovernance.outcome")}</th><th>{t("runGovernance.costEvidence")}</th><th>{t("runGovernance.lastActivity")}</th><th><span className="sr-only">{t("runGovernance.actions")}</span></th></tr></thead><tbody>{filtered.map((unit) => {
      const unitRuns = runs.filter((run) => run.work_unit_id === unit.id);
      const active = unitRuns.filter((run) => run.status === "active").length;
      const activity = latestActivity(unit, runs, outcomes);
      return <tr key={unit.id}><td data-label={t("runGovernance.workUnit")}><strong>{t("runGovernance.workUnit")}</strong><CopyableID value={unit.id} label={t("runGovernance.workUnit")} /><small>{t("runGovernance.createdByShort", { id: unit.created_by_key_id })}</small></td><td data-label={t("runGovernance.lifecycle")}><WorkUnitStatusBadge status={unit.status} /></td><td data-label={t("runGovernance.runs")}><strong>{unitRuns.length}</strong><small>{t("runGovernance.activeCount", { count: active })}</small></td><td data-label={t("runGovernance.outcome")}><OutcomeSummary unit={unit} outcomes={outcomes} /></td><td data-label={t("runGovernance.costEvidence")}><strong>{unit.committed_micros_usd == null ? "—" : money(unit.committed_micros_usd)}</strong><small>{(unit.unknown_attempts ?? 0) > 0 ? t("runGovernance.unknownAttempts", { count: unit.unknown_attempts }) : unitRuns.length > 0 ? t("runGovernance.inspectAttemptEvidence") : t("runGovernance.noSettledAttempts")}</small></td><td data-label={t("runGovernance.lastActivity")}><time dateTime={activity}>{dateTime(activity, "full")}</time></td><td><button type="button" className="button ghost" onClick={() => onSelectWorkUnit(unit.id)}>{t("runGovernance.viewDetails")}</button></td></tr>;
    })}</tbody></table></div>}

    {selectedUnit && <Modal drawer title={`${t("runGovernance.workUnitDetails")} · ${shortID(selectedUnit.id)}`} onClose={onCloseDetail}>
      <div className="governance-detail">
        <header className="governance-detail-summary"><div><span>{t("runGovernance.workUnit")}</span><CopyableID value={selectedUnit.id} label={t("runGovernance.workUnit")} /></div><WorkUnitStatusBadge status={selectedUnit.status} /></header>
        <section>
          <div className="governance-detail-heading"><div><h3>{t("runGovernance.lifecycle")}</h3><p>{t("runGovernance.lifecycleDescription")}</p></div></div>
          <div className="governance-lifecycle-panel">
            <ol className="governance-lifecycle-track" aria-label={t("runGovernance.lifecycle")}>
              <li className="complete"><span aria-hidden="true">1</span><div><strong>{t("runGovernance.createdAt")}</strong><time dateTime={selectedUnit.created_at}>{dateTime(selectedUnit.created_at, "full")}</time></div></li>
              <li className={selectedUnit.closed_at ? "complete" : "current"}><span aria-hidden="true">2</span><div><strong>{selectedUnit.closed_at ? t("runGovernance.closedAt") : t("runGovernance.inProgress")}</strong>{selectedUnit.closed_at ? <time dateTime={selectedUnit.closed_at}>{dateTime(selectedUnit.closed_at, "full")}</time> : <small>{t("runGovernance.awaitingClose")}</small>}</div></li>
            </ol>
            <dl className="governance-lifecycle-context"><div><dt>{t("runGovernance.createdBy")}</dt><dd><CopyableID value={selectedUnit.created_by_key_id} label={t("runGovernance.createdBy")} /></dd></div><div><dt>{t("runGovernance.frozenDefinitions")}</dt><dd>{selectedUnit.outcome_definitions?.length ? <ul className="governance-definition-refs">{selectedUnit.outcome_definitions.map((item) => <li key={`${item.id}:${item.version}`}><code title={item.id}>{shortID(item.id)}</code><span>v{item.version}</span></li>)}</ul> : "—"}</dd></div></dl>
          </div>
        </section>
        <section><div className="governance-detail-heading"><div><h3>{t("runGovernance.outcome")}</h3><p>{t("runGovernance.outcomeDescription")}</p></div><span className="governance-detail-count">{t("runGovernance.outcomeCount", { count: selectedOutcomes.length })}</span></div>{selectedOutcomes.length === 0 ? <p className="governance-muted">{t("runGovernance.noOutcomesDescription")}</p> : <ol className="governance-timeline">{selectedOutcomes.map((item) => <li key={item.id}><div className="governance-outcome-main"><OutcomeBadge outcome={item} /><strong>{item.value}</strong></div><div className="governance-outcome-definition"><span>{t("runGovernance.definition")}</span><code title={item.definition_id}>{shortID(item.definition_id)} · v{item.definition_version}</code></div><div className="governance-outcome-meta"><span>#{item.revision}</span><time dateTime={item.observed_at}>{dateTime(item.observed_at, "full")}</time><CopyableID value={item.id} label={t("runGovernance.outcome")} /></div></li>)}</ol>}</section>
        <section><div className="governance-detail-heading"><h3>{t("runGovernance.runs")}</h3><span className="governance-count">{selectedUnitRuns.length}</span></div>{selectedUnitRuns.length === 0 ? <p className="governance-muted">{t("runGovernance.noRunsDescription")}</p> : <div className="governance-run-list">{selectedUnitRuns.map((run) => <article key={run.id} className={selectedRunID === run.id ? "selected" : ""}><div className="governance-run-identity"><CopyableID value={run.id} label={t("runGovernance.run")} /><RunStatusBadge status={run.status} /></div><div className="governance-run-evidence"><span><small>{t("runGovernance.committed")}</small><strong>{money(run.committed_micros_usd)}</strong></span><span><small>{t("runGovernance.budgetState")}</small><BudgetStatusBadge status={run.budget_state} /></span></div><button type="button" className="button secondary" aria-pressed={selectedRunID === run.id} onClick={() => onSelectRun(run.id)}>{t("runGovernance.viewAttempts")}</button></article>)}</div>}</section>
        {selectedRun && <section className="governance-run-detail"><div className="governance-detail-heading"><div className="governance-detail-heading-copy"><span>{t("runGovernance.runDetails")}</span><CopyableID value={selectedRun.id} label={t("runGovernance.run")} /></div><BudgetStatusBadge status={selectedRun.budget_state} /></div><div className="governance-budget-card"><RunBudgetBar committed={selectedRun.committed_micros_usd} reserved={selectedRun.reserved_micros_usd} budget={selectedRun.budget_micros_usd} /></div><dl className="governance-detail-list governance-run-facts"><div><dt>{t("runGovernance.remaining")}</dt><dd>{money(selectedRun.remaining_micros_usd)}</dd></div><div><dt>{t("runGovernance.expiresAt")}</dt><dd>{dateTime(selectedRun.expires_at, "full")}</dd></div><div><dt>{t("runGovernance.closeReason")}</dt><dd><code>{selectedRun.close_reason || "—"}</code></dd></div></dl><div className="governance-subsection-heading"><h4>{t("runGovernance.attempts")}</h4>{attempts && <span className="governance-detail-count">{t("runGovernance.attemptCount", { count: attempts.length })}</span>}</div>{attemptsPending && <Loading />}{Boolean(attemptsError) && <ErrorState error={attemptsError} />}{attempts && attempts.length === 0 && <p className="governance-muted">{t("runGovernance.noAttemptsDescription")}</p>}{attempts && attempts.length > 0 && <div className="governance-attempts">{attempts.map((attempt) => <article key={attempt.event_id}><header><div className="governance-attempt-status"><GovernanceBadge tone={attempt.status === "success" ? "good" : "danger"}>{attempt.status === "success" ? t("runGovernance.attemptSuccess") : t("runGovernance.attemptFailed")}</GovernanceBadge><span>{t("runGovernance.attemptOrdinal", { count: attempt.attempt })}</span></div><CopyableID value={attempt.request_id} label={t("runGovernance.request")} /></header><div className="governance-attempt-route"><div><span>{t("runGovernance.requestedModel")}</span><strong>{attempt.requested_model || "—"}</strong></div><span className="governance-route-arrow" aria-hidden="true">→</span><div><span>{t("runGovernance.providerModel")}</span><strong>{attempt.provider_model || "—"}</strong></div></div><div className="governance-attempt-evidence"><dl><div className="governance-token-metric"><dt>{t("runGovernance.tokenUsage")}</dt><dd><strong>{attempt.provider_input_tokens + attempt.provider_output_tokens}</strong><div className="governance-token-breakdown-group" aria-label={t("runGovernance.tokenBreakdown")}><span>{t("runGovernance.tokenBreakdown")}</span><div className="governance-token-breakdown"><span><small>{t("runGovernance.inputTokens")}</small><b>{attempt.provider_input_tokens}</b></span><span><small>{t("runGovernance.outputTokens")}</small><b>{attempt.provider_output_tokens}</b></span></div></div></dd></div><div><dt>{t("runGovernance.latency")}</dt><dd><strong>{attempt.latency_millis == null ? "—" : `${attempt.latency_millis} ms`}</strong></dd></div><div><dt>{t("runGovernance.completedAt")}</dt><dd><time dateTime={attempt.completed_at}>{dateTime(attempt.completed_at, "full")}</time></dd></div></dl><aside><span>{t("runGovernance.costEvidence")}</span><CostEvidence attempt={attempt} /></aside></div></article>)}</div>}</section>}
        <details className="governance-audit"><summary>{t("runGovernance.auditDetails")}</summary><dl className="governance-detail-list"><div><dt>{t("runGovernance.workUnit")}</dt><dd><code>{selectedUnit.id}</code></dd></div><div><dt>{t("runGovernance.period")}</dt><dd><code>{selectedUnit.period_id}</code></dd></div>{selectedRun && <div><dt>{t("runGovernance.run")}</dt><dd><code>{selectedRun.id}</code></dd></div>}</dl></details>
      </div>
    </Modal>}
  </section>;
}
