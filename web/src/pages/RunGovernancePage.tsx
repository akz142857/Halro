import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
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
  const [projectID, setProjectID] = useState(() => new URLSearchParams(window.location.search).get("project_id") ?? "");
  const [workUnitStatus, setWorkUnitStatus] = useState("");
  const [runStatus, setRunStatus] = useState("");
  const [workUnitID, setWorkUnitID] = useState("");
  const [selectedRun, setSelectedRun] = useState<Run | null>(null);
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

      {projectID && runs.data && (
        <section className="panel">
          <header className="panel-header"><div><p className="eyebrow">Runs</p><h2>{t("runGovernance.runs")}</h2></div><span className="count">{runs.data.items.length}</span></header>
          {runs.data.items.length === 0 ? <EmptyState title={t("runGovernance.noRuns")}>{t("runGovernance.noRunsDescription")}</EmptyState> : <div className="table-shell"><table className="usage-table"><thead><tr><th>ID</th><th>Work Unit</th><th>{t("runGovernance.status")}</th><th>{t("runGovernance.budget")}</th><th>{t("runGovernance.committed")}</th><th>{t("runGovernance.expiresAt")}</th><th /></tr></thead><tbody>{runs.data.items.map((item) => <tr key={item.id}><td><code>{item.id}</code></td><td><code>{item.work_unit_id}</code></td><td><StatusDot ok={item.status === "active"} label={t(`runGovernance.${item.status}`)} /></td><td>{money(item.budget_micros_usd)}</td><td>{money(item.committed_micros_usd)}{item.unknown_attempts > 0 && <small>{t("runGovernance.unknownAttempts", { count: item.unknown_attempts })}</small>}</td><td>{dateTime(item.expires_at, "full")}</td><td><button className="button ghost" onClick={() => setSelectedRun(item)}>{t("runGovernance.viewAttempts")}</button></td></tr>)}</tbody></table></div>}
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
