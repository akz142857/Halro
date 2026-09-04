import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { FormEvent, useState } from "react";
import { api } from "../api";
import { EmptyState, ErrorState, Loading, PageHeader, StatusDot } from "../components";
import { money, useInstantFormatter } from "../format";
import type { Run } from "../types";
import { useTranslation } from "react-i18next";

function queryOf(values: Record<string, string>) {
  return `?${new URLSearchParams({ limit: "200", ...Object.fromEntries(Object.entries(values).filter(([, value]) => value)) })}`;
}

export function RunGovernancePage() {
  const { t } = useTranslation();
  const dateTime = useInstantFormatter();
	const queryClient = useQueryClient();
  const [projectID, setProjectID] = useState(() => new URLSearchParams(window.location.search).get("project_id") ?? "");
  const [workUnitStatus, setWorkUnitStatus] = useState("");
  const [runStatus, setRunStatus] = useState("");
  const [workUnitID, setWorkUnitID] = useState("");
  const [selectedRun, setSelectedRun] = useState<Run | null>(null);
	const [definitionID, setDefinitionID] = useState("");
	const [cohortStart, setCohortStart] = useState(() => new Date(Date.now() - 29 * 86400000).toISOString().slice(0, 10));
	const [cohortEnd, setCohortEnd] = useState(() => new Date().toISOString().slice(0, 10));
	const [definitionName, setDefinitionName] = useState("");
	const [allowedValues, setAllowedValues] = useState("accepted,rejected");
	const [successValues, setSuccessValues] = useState("accepted");
  const projects = useQuery({ queryKey: ["projects"], queryFn: api.projects });
  const workUnits = useQuery({
    queryKey: ["run-governance", "work-units", projectID, workUnitStatus],
    queryFn: () => api.workUnits(queryOf({ project_id: projectID, status: workUnitStatus })),
    enabled: Boolean(projectID),
  });
  const runs = useQuery({
    queryKey: ["run-governance", "runs", projectID, workUnitID, runStatus],
    queryFn: () => api.runs(queryOf({ project_id: projectID, work_unit_id: workUnitID, status: runStatus })),
    enabled: Boolean(projectID),
  });
  const attempts = useQuery({
    queryKey: ["run-governance", "attempts", selectedRun?.id],
    queryFn: () => api.usage(queryOf({ run_id: selectedRun?.id ?? "" })),
    enabled: Boolean(selectedRun),
  });
	const definitions = useQuery({ queryKey: ["outcome-definitions", projectID], queryFn: () => api.outcomeDefinitions(projectID), enabled: Boolean(projectID) });
	const outcomes = useQuery({ queryKey: ["governance-outcomes", projectID], queryFn: () => api.outcomes(queryOf({ project_id: projectID })), enabled: Boolean(projectID) });
	const selectedDefinition = definitions.data?.items.find((item) => `${item.id}:${item.version}` === definitionID);
	const summary = useQuery({
		queryKey: ["governance-summary", projectID, definitionID, cohortStart, cohortEnd],
		queryFn: () => api.governanceSummary(`?${new URLSearchParams({ project_id: projectID, definition_id: selectedDefinition?.id ?? "", definition_version: String(selectedDefinition?.version ?? ""), cohort_start: cohortStart, cohort_end: cohortEnd })}`),
		enabled: Boolean(projectID && selectedDefinition && cohortStart && cohortEnd),
	});
	const createDefinition = useMutation({
		mutationFn: () => {
			const project = projects.data?.items.find((item) => item.id === projectID);
			if (!project) throw new Error("project unavailable");
			return api.createOutcomeDefinition(projectID, project.revision, { name: definitionName, data_type: "CATEGORICAL", allowed_values: allowedValues.split(",").map((item) => item.trim()).filter(Boolean), success_values: successValues.split(",").map((item) => item.trim()).filter(Boolean), enabled: true });
		},
		onSuccess: async () => { setDefinitionName(""); await queryClient.invalidateQueries({ queryKey: ["outcome-definitions", projectID] }); },
	});
	function submitDefinition(event: FormEvent) { event.preventDefault(); createDefinition.mutate(); }

  return (
    <>
      <PageHeader eyebrow={t("runGovernance.eyebrow")} title={t("runGovernance.title")} description={t("runGovernance.description")} />
      <section className="panel">
        <div className="filter-bar" aria-label={t("common.filters")}>
          <label><span>{t("runGovernance.project")}</span><select value={projectID} onChange={(event) => { setProjectID(event.target.value); setWorkUnitID(""); setSelectedRun(null); }}><option value="">{t("runGovernance.chooseProject")}</option>{projects.data?.items.map((project) => <option value={project.id} key={project.id}>{project.name}</option>)}</select></label>
          <label><span>{t("runGovernance.workUnitStatus")}</span><select value={workUnitStatus} onChange={(event) => setWorkUnitStatus(event.target.value)}><option value="">{t("runGovernance.allStatuses")}</option><option value="open">{t("runGovernance.open")}</option><option value="closed">{t("runGovernance.closed")}</option></select></label>
          <label><span>{t("runGovernance.runStatus")}</span><select value={runStatus} onChange={(event) => setRunStatus(event.target.value)}><option value="">{t("runGovernance.allStatuses")}</option><option value="active">{t("runGovernance.active")}</option><option value="expired">{t("runGovernance.expired")}</option><option value="closed">{t("runGovernance.closed")}</option></select></label>
        </div>
        {!projectID && <EmptyState title={t("runGovernance.chooseProject")}>{t("runGovernance.chooseProjectDescription")}</EmptyState>}
        {(workUnits.isPending || runs.isPending) && projectID && <Loading />}
        {(workUnits.isError || runs.isError) && <ErrorState error={workUnits.error ?? runs.error} />}
      </section>

      {projectID && workUnits.data && (
        <section className="panel">
          <header className="panel-header"><div><p className="eyebrow">Work Units</p><h2>{t("runGovernance.workUnits")}</h2></div><span className="count">{workUnits.data.items.length}</span></header>
          {workUnits.data.items.length === 0 ? <EmptyState title={t("runGovernance.noWorkUnits")}>{t("runGovernance.noWorkUnitsDescription")}</EmptyState> : <div className="table-shell"><table className="usage-table"><thead><tr><th>ID</th><th>{t("runGovernance.status")}</th><th>{t("runGovernance.runs")}</th><th>{t("runGovernance.committed")}</th><th>{t("runGovernance.createdAt")}</th><th /></tr></thead><tbody>{workUnits.data.items.map((item) => <tr key={item.id}><td><code>{item.id}</code><small>{item.created_by_key_id}</small></td><td><StatusDot ok={item.status === "open"} label={t(`runGovernance.${item.status}`)} /></td><td>{item.run_count ?? 0}</td><td>{money(item.committed_micros_usd ?? 0)}{(item.unknown_attempts ?? 0) > 0 && <small>{t("runGovernance.unknownAttempts", { count: item.unknown_attempts })}</small>}</td><td>{dateTime(item.created_at, "full")}</td><td><button className="button ghost" onClick={() => { setWorkUnitID(workUnitID === item.id ? "" : item.id); setSelectedRun(null); }}>{workUnitID === item.id ? t("runGovernance.showAllRuns") : t("runGovernance.filterRuns")}</button></td></tr>)}</tbody></table></div>}
        </section>
      )}

		{projectID && (
			<section className="panel">
				<header className="panel-header"><div><p className="eyebrow">Outcome Definitions</p><h2>{t("runGovernance.outcomeDefinitions")}</h2><p>{t("runGovernance.outcomeDefinitionsDescription")}</p></div><span className="count">{definitions.data?.items.length ?? 0}</span></header>
				{definitions.isPending && <Loading />}{definitions.isError && <ErrorState error={definitions.error} />}
				{definitions.data && <div className="table-shell"><table className="usage-table"><thead><tr><th>{t("runGovernance.definition")}</th><th>{t("runGovernance.version")}</th><th>{t("runGovernance.values")}</th><th>{t("runGovernance.successValues")}</th><th>{t("runGovernance.status")}</th></tr></thead><tbody>{definitions.data.items.map((item) => <tr key={`${item.id}:${item.version}`}><td><strong>{item.name}</strong><small><code>{item.id}</code></small></td><td>v{item.version}</td><td>{item.allowed_values.join(", ")}</td><td>{item.success_values.join(", ")}</td><td><StatusDot ok={item.enabled} label={item.enabled ? t("common.enabled") : t("common.disabled")} /></td></tr>)}</tbody></table></div>}
				<form className="filter-bar" onSubmit={submitDefinition}><label><span>{t("runGovernance.definitionName")}</span><input value={definitionName} pattern="[a-z][a-z0-9_]{0,63}" onChange={(event) => setDefinitionName(event.target.value)} required /></label><label><span>{t("runGovernance.values")}</span><input value={allowedValues} onChange={(event) => setAllowedValues(event.target.value)} required /></label><label><span>{t("runGovernance.successValues")}</span><input value={successValues} onChange={(event) => setSuccessValues(event.target.value)} required /></label><button className="button primary" disabled={createDefinition.isPending}>{t("runGovernance.createDefinition")}</button></form>
				{createDefinition.isError && <ErrorState error={createDefinition.error} />}
			</section>
		)}

		{projectID && definitions.data && (
			<section className="panel">
				<header className="panel-header"><div><p className="eyebrow">work_unit_cohort</p><h2>{t("runGovernance.outcomeSummary")}</h2><p>{t("runGovernance.outcomeSummaryDescription")}</p></div></header>
				<div className="filter-bar"><label><span>{t("runGovernance.definition")}</span><select value={definitionID} onChange={(event) => setDefinitionID(event.target.value)}><option value="">{t("runGovernance.chooseDefinition")}</option>{definitions.data.items.map((item) => <option key={`${item.id}:${item.version}`} value={`${item.id}:${item.version}`}>{item.name} · v{item.version}</option>)}</select></label><label><span>{t("runGovernance.cohortStart")}</span><input type="date" value={cohortStart} onChange={(event) => setCohortStart(event.target.value)} /></label><label><span>{t("runGovernance.cohortEnd")}</span><input type="date" value={cohortEnd} onChange={(event) => setCohortEnd(event.target.value)} /></label></div>
				{summary.isPending && selectedDefinition && <Loading />}{summary.isError && <ErrorState error={summary.error} />}
				{summary.data && <div className="metric-grid"><article className="metric"><span>{t("runGovernance.coverage")}</span><strong>{summary.data.outcome_coverage == null ? "—" : `${(summary.data.outcome_coverage * 100).toFixed(1)}%`}</strong><small>{summary.data.evaluated_units}/{summary.data.eligible_units}</small></article><article className="metric"><span>{t("runGovernance.successRate")}</span><strong>{summary.data.success_rate == null ? "—" : `${(summary.data.success_rate * 100).toFixed(1)}%`}</strong><small>{summary.data.successful_units}/{summary.data.evaluated_units}</small></article><article className="metric"><span>{t("runGovernance.costPerSuccess")}</span><strong>{summary.data.cost_per_success_micros_usd == null ? "—" : money(summary.data.cost_per_success_micros_usd)}</strong><small>{t(`runGovernance.${summary.data.cost_completeness}`)}</small></article><article className="metric"><span>{t("runGovernance.inProgressCost")}</span><strong>{money(summary.data.in_progress_cost_micros_usd)}</strong><small>{t("runGovernance.matured", { count: summary.data.matured_units })}</small></article></div>}
			</section>
		)}

		{projectID && outcomes.data && outcomes.data.items.length > 0 && <section className="panel"><header className="panel-header"><div><p className="eyebrow">Governance Journal</p><h2>{t("runGovernance.outcomes")}</h2></div><span className="count">{outcomes.data.items.length}</span></header><div className="table-shell"><table className="usage-table"><thead><tr><th>ID</th><th>Work Unit</th><th>{t("runGovernance.definition")}</th><th>{t("runGovernance.value")}</th><th>{t("runGovernance.revision")}</th><th>{t("runGovernance.observedAt")}</th></tr></thead><tbody>{outcomes.data.items.map((item) => <tr key={item.id}><td><code>{item.id}</code>{item.provisional && <small>{t("runGovernance.provisional")}</small>}</td><td><code>{item.work_unit_id}</code></td><td><code>{item.definition_id}</code><small>v{item.definition_version}</small></td><td>{item.value}</td><td>#{item.revision}</td><td>{dateTime(item.observed_at, "full")}</td></tr>)}</tbody></table></div></section>}

      {projectID && runs.data && (
        <section className="panel">
          <header className="panel-header"><div><p className="eyebrow">Runs</p><h2>{t("runGovernance.runs")}</h2></div><span className="count">{runs.data.items.length}</span></header>
          {runs.data.items.length === 0 ? <EmptyState title={t("runGovernance.noRuns")}>{t("runGovernance.noRunsDescription")}</EmptyState> : <div className="table-shell"><table className="usage-table"><thead><tr><th>ID</th><th>Work Unit</th><th>{t("runGovernance.status")}</th><th>{t("runGovernance.budgetState")}</th><th>{t("runGovernance.budget")}</th><th>{t("runGovernance.remaining")}</th><th>{t("runGovernance.committed")}</th><th>{t("runGovernance.expiresAt")}</th><th /></tr></thead><tbody>{runs.data.items.map((item) => <tr key={item.id}><td><code>{item.id}</code></td><td><code>{item.work_unit_id}</code></td><td><StatusDot ok={item.status === "active"} label={t(`runGovernance.${item.status}`)} /></td><td><StatusDot ok={item.budget_state === "available"} label={t(`runGovernance.${item.budget_state}`)} /></td><td>{money(item.budget_micros_usd)}</td><td>{money(item.remaining_micros_usd)}</td><td>{money(item.committed_micros_usd)}{item.reserved_micros_usd > 0 && <small>{t("runGovernance.reserved", { amount: money(item.reserved_micros_usd) })}</small>}{item.unknown_attempts > 0 && <small>{t("runGovernance.unknownAttempts", { count: item.unknown_attempts })}</small>}</td><td>{dateTime(item.expires_at, "full")}</td><td><button className="button ghost" onClick={() => setSelectedRun(item)}>{t("runGovernance.viewAttempts")}</button></td></tr>)}</tbody></table></div>}
        </section>
      )}

      {selectedRun && (
        <section className="panel">
          <header className="panel-header"><div><p className="eyebrow"><code>{selectedRun.id}</code></p><h2>{t("runGovernance.attempts")}</h2></div></header>
          {attempts.isPending && <Loading />}{attempts.isError && <ErrorState error={attempts.error} />}
          {attempts.data?.items.length === 0 && <EmptyState title={t("runGovernance.noAttempts")}>{t("runGovernance.noAttemptsDescription")}</EmptyState>}
          {attempts.data && attempts.data.items.length > 0 && <div className="table-shell"><table className="usage-table"><thead><tr><th>{t("runGovernance.request")}</th><th>{t("runGovernance.status")}</th><th>{t("runGovernance.model")}</th><th>{t("runGovernance.cost")}</th><th>{t("runGovernance.completedAt")}</th></tr></thead><tbody>{attempts.data.items.map((item) => <tr key={item.event_id}><td><code>{item.request_id}</code><small>#{item.attempt}</small></td><td>{item.status}</td><td>{item.requested_model}<small>{item.provider_model}</small></td><td>{item.cost_micros_usd == null ? t("common.unknown") : money(item.cost_micros_usd)}</td><td>{dateTime(item.completed_at, "full")}</td></tr>)}</tbody></table></div>}
        </section>
      )}
    </>
  );
}
