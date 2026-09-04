import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { FormEvent, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { EmptyState, ErrorState, Loading, PageHeader, StatusDot } from "../components";
import { money, useInstantFormatter } from "../format";
import type { GovernanceSummary, OutcomeDefinition, Run } from "../types";

function queryOf(values: Record<string, string>) {
  return `?${new URLSearchParams(Object.fromEntries(Object.entries(values).filter(([, value]) => value)))}`;
}

function valueList(value: string) {
  return value.split(",").map((item) => item.trim()).filter(Boolean);
}

function watermark(value: { generation?: number; sequence: number; offset: number } | undefined) {
  if (!value) return "—";
  return `${value.generation == null ? "" : `g${value.generation} · `}#${value.sequence} · ${value.offset} B`;
}

function summaryReason(summary: GovernanceSummary, metric: "coverage" | "success" | "cost") {
  if (metric === "coverage" && summary.outcome_coverage == null) return summary.outcome_reason || "no_eligible_units";
  if (metric === "success" && summary.success_rate == null) return summary.outcome_reason || "no_evaluated_units";
  if (metric === "cost" && summary.cost_per_success_micros_usd == null) return summary.cost_per_success_reason || "no_successful_units";
  return "complete";
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
  const [editingDefinition, setEditingDefinition] = useState<OutcomeDefinition | null>(null);
  const [definitionName, setDefinitionName] = useState("");
  const [definitionType, setDefinitionType] = useState<"BOOLEAN" | "CATEGORICAL">("CATEGORICAL");
  const [allowedValues, setAllowedValues] = useState("accepted,rejected");
  const [successValues, setSuccessValues] = useState("accepted");
  const [definitionEnabled, setDefinitionEnabled] = useState(true);

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
    queryFn: () => api.usage(`?${new URLSearchParams({ limit: "200", run_id: selectedRun?.id ?? "" })}`),
    enabled: Boolean(selectedRun),
  });
  const definitions = useQuery({
    queryKey: ["outcome-definitions", projectID],
    queryFn: () => api.outcomeDefinitions(projectID),
    enabled: Boolean(projectID),
  });
  const outcomes = useQuery({
    queryKey: ["governance-outcomes", projectID],
    queryFn: () => api.outcomes(queryOf({ project_id: projectID })),
    enabled: Boolean(projectID),
  });
  const selectedDefinition = definitions.data?.items.find((item) => `${item.id}:${item.version}` === definitionID);
  const latestDefinitions = useMemo(() => {
    const latest = new Map<string, OutcomeDefinition>();
    for (const item of definitions.data?.items ?? []) {
      if (!latest.has(item.id) || latest.get(item.id)!.version < item.version) latest.set(item.id, item);
    }
    return latest;
  }, [definitions.data]);
  const summary = useQuery({
    queryKey: ["governance-summary", projectID, definitionID, cohortStart, cohortEnd],
    queryFn: () => api.governanceSummary(`?${new URLSearchParams({
      project_id: projectID,
      definition_id: selectedDefinition?.id ?? "",
      definition_version: String(selectedDefinition?.version ?? ""),
      cohort_start: cohortStart,
      cohort_end: cohortEnd,
    })}`),
    enabled: Boolean(projectID && selectedDefinition && cohortStart && cohortEnd),
  });

  function resetDefinitionForm() {
    setEditingDefinition(null);
    setDefinitionName("");
    setDefinitionType("CATEGORICAL");
    setAllowedValues("accepted,rejected");
    setSuccessValues("accepted");
    setDefinitionEnabled(true);
  }

  function editDefinition(item: OutcomeDefinition) {
    setEditingDefinition(item);
    setDefinitionName(item.name);
    setDefinitionType(item.data_type);
    setAllowedValues((item.allowed_values.length ? item.allowed_values : ["false", "true"]).join(","));
    setSuccessValues(item.success_values.join(","));
    setDefinitionEnabled(item.enabled);
  }

  function definitionBody(enabled = definitionEnabled) {
    return {
      name: editingDefinition?.name ?? definitionName,
      data_type: definitionType,
      allowed_values: definitionType === "BOOLEAN" ? [] : valueList(allowedValues),
      success_values: valueList(successValues),
      enabled,
    };
  }

  const saveDefinition = useMutation({
    mutationFn: () => {
      if (editingDefinition) {
        return api.createOutcomeDefinitionVersion(projectID, editingDefinition.id, editingDefinition.revision, definitionBody());
      }
      const project = projects.data?.items.find((item) => item.id === projectID);
      if (!project) throw new Error("project unavailable");
      return api.createOutcomeDefinition(projectID, project.revision, definitionBody());
    },
    onSuccess: async () => {
      resetDefinitionForm();
      await queryClient.invalidateQueries({ queryKey: ["outcome-definitions", projectID] });
    },
  });
  const toggleDefinition = useMutation({
    mutationFn: (item: OutcomeDefinition) => api.createOutcomeDefinitionVersion(projectID, item.id, item.revision, {
      name: item.name,
      data_type: item.data_type,
      allowed_values: item.allowed_values,
      success_values: item.success_values,
      enabled: !item.enabled,
    }),
    onSuccess: async () => {
      await queryClient.invalidateQueries({ queryKey: ["outcome-definitions", projectID] });
    },
  });
  function submitDefinition(event: FormEvent) {
    event.preventDefault();
    saveDefinition.mutate();
  }

  const summaryOutcomeState = summary.data?.outcome_completeness
    ?? (summary.data && summary.data.eligible_units === 0 ? "unknown"
      : summary.data && summary.data.evaluated_units < summary.data.matured_units ? "partial" : "complete");

  return (
    <>
      <PageHeader eyebrow={t("runGovernance.eyebrow")} title={t("runGovernance.title")} description={t("runGovernance.description")} />
      <section className="panel">
        <div className="filter-bar" aria-label={t("common.filters")}>
          <label><span>{t("runGovernance.project")}</span><select value={projectID} onChange={(event) => { setProjectID(event.target.value); setWorkUnitID(""); setSelectedRun(null); setDefinitionID(""); resetDefinitionForm(); }}><option value="">{t("runGovernance.chooseProject")}</option>{projects.data?.items.map((project) => <option value={project.id} key={project.id}>{project.name}</option>)}</select></label>
          <label><span>{t("runGovernance.workUnitStatus")}</span><select value={workUnitStatus} onChange={(event) => setWorkUnitStatus(event.target.value)}><option value="">{t("runGovernance.allStatuses")}</option><option value="open">{t("runGovernance.open")}</option><option value="closed">{t("runGovernance.closed")}</option></select></label>
          <label><span>{t("runGovernance.runStatus")}</span><select value={runStatus} onChange={(event) => setRunStatus(event.target.value)}><option value="">{t("runGovernance.allStatuses")}</option><option value="active">{t("runGovernance.active")}</option><option value="expired">{t("runGovernance.expired")}</option><option value="closed">{t("runGovernance.closed")}</option></select></label>
        </div>
        {!projectID && <EmptyState title={t("runGovernance.chooseProject")}>{t("runGovernance.chooseProjectDescription")}</EmptyState>}
        {(workUnits.isPending || runs.isPending) && projectID && <Loading />}
        {(workUnits.isError || runs.isError) && <ErrorState error={workUnits.error ?? runs.error} />}
      </section>

      {projectID && workUnits.data && (
        <section className="panel">
          <header className="panel-header"><div><p className="eyebrow">{t("runGovernance.workUnitsEyebrow")}</p><h2>{t("runGovernance.workUnits")}</h2></div><span className="count">{workUnits.data.items.length}</span></header>
          {workUnits.data.items.length === 0 ? <EmptyState title={t("runGovernance.noWorkUnits")}>{t("runGovernance.noWorkUnitsDescription")}</EmptyState> : <div className="table-shell"><table className="usage-table"><thead><tr><th>{t("runGovernance.id")}</th><th>{t("runGovernance.status")}</th><th>{t("runGovernance.runs")}</th><th>{t("runGovernance.committed")}</th><th>{t("runGovernance.createdAt")}</th><th /></tr></thead><tbody>{workUnits.data.items.map((item) => <tr key={item.id}><td><code>{item.id}</code><small>{item.created_by_key_id}</small></td><td><StatusDot ok={item.status === "open"} label={t(`runGovernance.${item.status}`)} /></td><td>{item.run_count ?? 0}</td><td>{money(item.committed_micros_usd ?? 0)}{(item.unknown_attempts ?? 0) > 0 && <small>{t("runGovernance.unknownAttempts", { count: item.unknown_attempts })}</small>}</td><td>{dateTime(item.created_at, "full")}</td><td><button className="button ghost" onClick={() => { setWorkUnitID(workUnitID === item.id ? "" : item.id); setSelectedRun(null); }}>{workUnitID === item.id ? t("runGovernance.showAllRuns") : t("runGovernance.filterRuns")}</button></td></tr>)}</tbody></table></div>}
        </section>
      )}

      {projectID && (
        <section className="panel">
          <header className="panel-header"><div><p className="eyebrow">{t("runGovernance.definitionsEyebrow")}</p><h2>{t("runGovernance.outcomeDefinitions")}</h2><p>{t("runGovernance.outcomeDefinitionsDescription")}</p></div><span className="count">{definitions.data?.items.length ?? 0}</span></header>
          {definitions.isPending && <Loading />}{definitions.isError && <ErrorState error={definitions.error} />}
          {definitions.data && <div className="table-shell"><table className="usage-table"><thead><tr><th>{t("runGovernance.definition")}</th><th>{t("runGovernance.version")}</th><th>{t("runGovernance.type")}</th><th>{t("runGovernance.values")}</th><th>{t("runGovernance.successValues")}</th><th>{t("runGovernance.status")}</th><th>{t("runGovernance.actions")}</th></tr></thead><tbody>{definitions.data.items.map((item) => {
            const latest = latestDefinitions.get(item.id)?.version === item.version;
            return <tr key={`${item.id}:${item.version}`}><td><strong>{item.name}</strong><small><code>{item.id}</code></small></td><td>v{item.version}</td><td>{t(`runGovernance.${item.data_type.toLowerCase()}`)}</td><td>{(item.allowed_values.length ? item.allowed_values : ["false", "true"]).join(", ")}</td><td>{item.success_values.join(", ")}</td><td><StatusDot ok={item.enabled} label={item.enabled ? t("common.enabled") : t("common.disabled")} /></td><td>{latest && <div className="form-actions"><button type="button" className="button ghost" onClick={() => editDefinition(item)}>{t("runGovernance.newVersion")}</button><button type="button" className="button ghost" disabled={toggleDefinition.isPending} onClick={() => toggleDefinition.mutate(item)}>{item.enabled ? t("common.disable") : t("common.enable")}</button></div>}</td></tr>;
          })}</tbody></table></div>}
          <form className="settings-form" onSubmit={submitDefinition}>
            <h3>{t(editingDefinition ? "runGovernance.newVersionTitle" : "runGovernance.createDefinition")}</h3>
            <p>{t("runGovernance.versionEffectHint")}</p>
            <div className="filter-bar">
              <label><span>{t("runGovernance.definitionName")}</span><input value={definitionName} pattern="[a-z][a-z0-9_]{0,63}" disabled={Boolean(editingDefinition)} onChange={(event) => setDefinitionName(event.target.value)} required /></label>
              <label><span>{t("runGovernance.type")}</span><select value={definitionType} onChange={(event) => { const next = event.target.value as "BOOLEAN" | "CATEGORICAL"; setDefinitionType(next); setAllowedValues(next === "BOOLEAN" ? "false,true" : "accepted,rejected"); setSuccessValues(next === "BOOLEAN" ? "true" : "accepted"); }}><option value="CATEGORICAL">{t("runGovernance.categorical")}</option><option value="BOOLEAN">{t("runGovernance.boolean")}</option></select></label>
              <label><span>{t("runGovernance.values")}</span><input value={allowedValues} disabled={definitionType === "BOOLEAN"} onChange={(event) => setAllowedValues(event.target.value)} required={definitionType === "CATEGORICAL"} /></label>
              <label><span>{t("runGovernance.successValues")}</span><input value={successValues} onChange={(event) => setSuccessValues(event.target.value)} required /></label>
              <label className="check-row"><input type="checkbox" checked={definitionEnabled} onChange={(event) => setDefinitionEnabled(event.target.checked)} /><span>{t("runGovernance.definitionEnabled")}</span></label>
            </div>
            {(saveDefinition.isError || toggleDefinition.isError) && <ErrorState error={saveDefinition.error ?? toggleDefinition.error} />}
            <div className="form-actions"><button className="button primary" disabled={saveDefinition.isPending}>{t(editingDefinition ? "runGovernance.saveNewVersion" : "runGovernance.createDefinition")}</button>{editingDefinition && <button type="button" className="button ghost" onClick={resetDefinitionForm}>{t("common.cancel")}</button>}</div>
          </form>
        </section>
      )}

      {projectID && definitions.data && (
        <section className="panel">
          <header className="panel-header"><div><p className="eyebrow">{t("runGovernance.cohortEyebrow")}</p><h2>{t("runGovernance.outcomeSummary")}</h2><p>{t("runGovernance.outcomeSummaryDescription")}</p></div></header>
          <div className="filter-bar"><label><span>{t("runGovernance.definition")}</span><select value={definitionID} onChange={(event) => setDefinitionID(event.target.value)}><option value="">{t("runGovernance.chooseDefinition")}</option>{definitions.data.items.map((item) => <option key={`${item.id}:${item.version}`} value={`${item.id}:${item.version}`}>{item.name} · v{item.version}</option>)}</select></label><label><span>{t("runGovernance.cohortStart")}</span><input type="date" value={cohortStart} onChange={(event) => setCohortStart(event.target.value)} /></label><label><span>{t("runGovernance.cohortEnd")}</span><input type="date" value={cohortEnd} onChange={(event) => setCohortEnd(event.target.value)} /></label></div>
          {summary.isPending && selectedDefinition && <Loading />}{summary.isError && <ErrorState error={summary.error} />}
          {summary.data && <>
            <div className={`notice ${summaryOutcomeState === "complete" && summary.data.cost_completeness === "complete" ? "success" : "warning"}`} role="status"><strong>{t(`runGovernance.summaryState.${summaryOutcomeState}`)}</strong><span>{t(`runGovernance.summaryReason.${summary.data.outcome_reason || (summaryOutcomeState === "complete" ? "complete" : "missing_outcomes")}`)}</span></div>
            <div className="metric-grid">
              <article className="metric"><span>{t("runGovernance.coverage")}</span><strong>{summary.data.outcome_coverage == null ? "—" : `${(summary.data.outcome_coverage * 100).toFixed(1)}%`}</strong><small>{summary.data.outcome_coverage == null ? t(`runGovernance.summaryReason.${summaryReason(summary.data, "coverage")}`) : `${summary.data.evaluated_units}/${summary.data.eligible_units}`}</small></article>
              <article className="metric"><span>{t("runGovernance.successRate")}</span><strong>{summary.data.success_rate == null ? "—" : `${(summary.data.success_rate * 100).toFixed(1)}%`}</strong><small>{summary.data.success_rate == null ? t(`runGovernance.summaryReason.${summaryReason(summary.data, "success")}`) : `${summary.data.successful_units}/${summary.data.evaluated_units}`}</small></article>
              <article className="metric"><span>{t("runGovernance.costPerSuccess")}</span><strong>{summary.data.cost_per_success_micros_usd == null ? "—" : money(summary.data.cost_per_success_micros_usd)}</strong><small>{summary.data.cost_per_success_reason ? t(`runGovernance.summaryReason.${summary.data.cost_per_success_reason}`) : summary.data.cost_per_success_micros_usd == null ? t(`runGovernance.summaryReason.${summaryReason(summary.data, "cost")}`) : t(`runGovernance.${summary.data.cost_completeness}`)}</small></article>
              <article className="metric"><span>{t("runGovernance.knownCost")}</span><strong>{money(summary.data.known_cost_micros_usd)}</strong><small>{t(`runGovernance.${summary.data.cost_completeness}`)}</small></article>
              <article className="metric"><span>{t("runGovernance.estimatedCost")}</span><strong>{money(summary.data.estimated_cost_micros_usd)}</strong><small>{t("runGovernance.estimatedSubset")}</small></article>
              <article className="metric"><span>{t("runGovernance.unknownCost")}</span><strong>{summary.data.unknown_attempts}</strong><small>{t("runGovernance.unknownAttemptUnit")}</small></article>
              <article className="metric"><span>{t("runGovernance.inProgressCost")}</span><strong>{money(summary.data.in_progress_cost_micros_usd)}</strong><small>{t("runGovernance.matured", { count: summary.data.matured_units })}</small></article>
            </div>
            <div className="notice"><strong>{t("runGovernance.summarySnapshot")}</strong><span>{t("runGovernance.generatedAt", { date: dateTime(summary.data.generated_at, "full") })}</span><small>{t("runGovernance.accountingWatermark", { value: watermark(summary.data.accounting_watermark) })}</small><small>{t("runGovernance.governanceWatermark", { value: watermark(summary.data.governance_watermark) })}</small></div>
          </>}
        </section>
      )}

      {projectID && <section className="panel"><header className="panel-header"><div><p className="eyebrow">{t("runGovernance.journalEyebrow")}</p><h2>{t("runGovernance.outcomes")}</h2></div><span className="count">{outcomes.data?.items.length ?? 0}</span></header>{outcomes.isPending && <Loading />}{outcomes.isError && <ErrorState error={outcomes.error} />}{outcomes.data?.items.length === 0 && <EmptyState title={t("runGovernance.noOutcomes")}>{t("runGovernance.noOutcomesDescription")}</EmptyState>}{outcomes.data && outcomes.data.items.length > 0 && <div className="table-shell"><table className="usage-table"><thead><tr><th>{t("runGovernance.id")}</th><th>{t("runGovernance.workUnit")}</th><th>{t("runGovernance.definition")}</th><th>{t("runGovernance.value")}</th><th>{t("runGovernance.revision")}</th><th>{t("runGovernance.observedAt")}</th></tr></thead><tbody>{outcomes.data.items.map((item) => <tr key={item.id}><td><code>{item.id}</code>{item.provisional && <small>{t("runGovernance.provisional")}</small>}</td><td><code>{item.work_unit_id}</code></td><td><code>{item.definition_id}</code><small>v{item.definition_version}</small></td><td>{item.value}</td><td>#{item.revision}</td><td>{dateTime(item.observed_at, "full")}</td></tr>)}</tbody></table></div>}</section>}

      {projectID && runs.data && (
        <section className="panel">
          <header className="panel-header"><div><p className="eyebrow">{t("runGovernance.runsEyebrow")}</p><h2>{t("runGovernance.runs")}</h2></div><span className="count">{runs.data.items.length}</span></header>
          {runs.data.items.length === 0 ? <EmptyState title={t("runGovernance.noRuns")}>{t("runGovernance.noRunsDescription")}</EmptyState> : <div className="table-shell"><table className="usage-table"><thead><tr><th>{t("runGovernance.id")}</th><th>{t("runGovernance.workUnit")}</th><th>{t("runGovernance.status")}</th><th>{t("runGovernance.budgetState")}</th><th>{t("runGovernance.budget")}</th><th>{t("runGovernance.remaining")}</th><th>{t("runGovernance.committed")}</th><th>{t("runGovernance.expiresAt")}</th><th /></tr></thead><tbody>{runs.data.items.map((item) => <tr key={item.id}><td><code>{item.id}</code></td><td><code>{item.work_unit_id}</code></td><td><StatusDot ok={item.status === "active"} label={t(`runGovernance.${item.status}`)} /></td><td><StatusDot ok={item.budget_state === "available"} label={t(`runGovernance.${item.budget_state}`)} /></td><td>{money(item.budget_micros_usd)}</td><td>{money(item.remaining_micros_usd)}</td><td>{money(item.committed_micros_usd)}{item.reserved_micros_usd > 0 && <small>{t("runGovernance.reserved", { amount: money(item.reserved_micros_usd) })}</small>}{item.unknown_attempts > 0 && <small>{t("runGovernance.unknownAttempts", { count: item.unknown_attempts })}</small>}</td><td>{dateTime(item.expires_at, "full")}</td><td><button className="button ghost" onClick={() => setSelectedRun(item)}>{t("runGovernance.viewAttempts")}</button></td></tr>)}</tbody></table></div>}
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
