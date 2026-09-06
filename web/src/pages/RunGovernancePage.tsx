import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useCallback, useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { EmptyState, ErrorState, Loading, PageHeader, SegmentedTabs } from "../components";
import { useInstantFormatter } from "../format";
import type { OutcomeDefinition } from "../types";
import { GovernanceOverview, type OverviewFilter } from "./run-governance/GovernanceOverview";
import { GovernanceBadge } from "./run-governance/GovernancePrimitives";
import { OutcomeWorkspace } from "./run-governance/OutcomeWorkspace";
import { WorkUnitExplorer } from "./run-governance/WorkUnitExplorer";
import { governanceContextFromSearch, isGovernanceDataStale, localDateValue, rememberedGovernanceProject, rememberGovernanceProject, type GovernanceView, writeGovernanceContext } from "./run-governance/governance-state";

function queryOf(values: Record<string, string>) {
  return `?${new URLSearchParams(Object.fromEntries(Object.entries(values).filter(([, value]) => value)))}`;
}

function latestEnabledDefinition(definitions: OutcomeDefinition[]) {
  const latest = new Map<string, OutcomeDefinition>();
  for (const item of definitions) if (!latest.has(item.id) || latest.get(item.id)!.version < item.version) latest.set(item.id, item);
  return [...latest.values()].filter((item) => item.enabled).sort((a, b) => a.name.localeCompare(b.name) || a.id.localeCompare(b.id))[0];
}

export function RunGovernancePage() {
  const { t } = useTranslation();
  const dateTime = useInstantFormatter();
  const queryClient = useQueryClient();
  const initial = useMemo(() => governanceContextFromSearch(window.location.search), []);
  const [projectID, setProjectID] = useState(initial.projectID);
  const [view, setView] = useState<GovernanceView>(initial.view);
  const [workUnitID, setWorkUnitID] = useState(initial.workUnitID);
  const [runID, setRunID] = useState(initial.runID);
  const [overviewFilter, setOverviewFilter] = useState<OverviewFilter>("all");
  const [definitionID, setDefinitionID] = useState("");
  const [cohortStart, setCohortStart] = useState(() => localDateValue(new Date(Date.now() - 29 * 86400000)));
  const [cohortEnd, setCohortEnd] = useState(() => localDateValue());
  const [clock, setClock] = useState(Date.now());

  const updateContext = useCallback((next: Partial<{ projectID: string; view: GovernanceView; workUnitID: string; runID: string }>, mode: "push" | "replace" = "push") => {
    const value = { projectID: next.projectID ?? projectID, view: next.view ?? view, workUnitID: next.workUnitID ?? workUnitID, runID: next.runID ?? runID };
    setProjectID(value.projectID);
    setView(value.view);
    setWorkUnitID(value.workUnitID);
    setRunID(value.runID);
    writeGovernanceContext(value, mode);
  }, [projectID, runID, view, workUnitID]);

  useEffect(() => {
    const restore = () => {
      const value = governanceContextFromSearch(window.location.search);
      setDefinitionID("");
      setProjectID(value.projectID);
      setView(value.view);
      setWorkUnitID(value.workUnitID);
      setRunID(value.runID);
    };
    window.addEventListener("popstate", restore);
    return () => window.removeEventListener("popstate", restore);
  }, []);
  useEffect(() => {
    const id = window.setInterval(() => setClock(Date.now()), 15_000);
    return () => window.clearInterval(id);
  }, []);

  const projects = useQuery({ queryKey: ["projects"], queryFn: api.projects });
  const governanceProjects = useMemo(() => (projects.data?.items ?? []).filter((project) => project.run_governance?.enabled), [projects.data]);
  useEffect(() => {
    if (!projects.data) return;
    if (projectID && governanceProjects.some((project) => project.id === projectID)) {
      rememberGovernanceProject(projectID);
      return;
    }
    const remembered = rememberedGovernanceProject();
    const next = governanceProjects.some((project) => project.id === remembered) ? remembered : governanceProjects.length === 1 ? governanceProjects[0].id : "";
    if (projectID !== next) updateContext({ projectID: next, workUnitID: "", runID: "" }, "replace");
  }, [governanceProjects, projectID, projects.data, updateContext]);

  const runs = useQuery({ queryKey: ["run-governance", "runs", projectID], queryFn: () => api.runs(queryOf({ project_id: projectID })), enabled: Boolean(projectID), refetchOnWindowFocus: true });
  const hasActiveRuns = runs.data?.items.some((item) => item.status === "active") ?? false;
  const polling = hasActiveRuns ? 15_000 : false;
  const workUnits = useQuery({ queryKey: ["run-governance", "work-units", projectID], queryFn: () => api.workUnits(queryOf({ project_id: projectID })), enabled: Boolean(projectID), refetchInterval: polling, refetchOnWindowFocus: true });
  useEffect(() => {
    if (!polling) return;
    const id = window.setInterval(() => { void runs.refetch(); }, polling);
    return () => window.clearInterval(id);
  }, [polling, runs.refetch]);
  const definitions = useQuery({ queryKey: ["outcome-definitions", projectID], queryFn: () => api.outcomeDefinitions(projectID), enabled: Boolean(projectID), refetchOnWindowFocus: true });
  const outcomes = useQuery({ queryKey: ["governance-outcomes", projectID], queryFn: () => api.outcomes(queryOf({ project_id: projectID })), enabled: Boolean(projectID), refetchInterval: polling, refetchOnWindowFocus: true });
  const selectedRun = runs.data?.items.find((item) => item.id === runID && item.work_unit_id === workUnitID);
  const attempts = useQuery({ queryKey: ["run-governance", "attempts", selectedRun?.id], queryFn: () => api.usageAll(queryOf({ run_id: selectedRun?.id ?? "" })), enabled: Boolean(selectedRun), refetchInterval: selectedRun?.status === "active" ? 15_000 : false, refetchOnWindowFocus: true });
  useEffect(() => {
    if (view !== "work-units" || !workUnits.data || !runs.data) return;
    if (workUnitID && !workUnits.data.items.some((item) => item.id === workUnitID)) {
      updateContext({ workUnitID: "", runID: "" }, "replace");
      return;
    }
    if (runID && !runs.data.items.some((item) => item.id === runID && item.work_unit_id === workUnitID)) updateContext({ runID: "" }, "replace");
  }, [runID, runs.data, updateContext, view, workUnitID, workUnits.data]);

  useEffect(() => {
    if (definitionID || !definitions.data?.items.length) return;
    const latest = latestEnabledDefinition(definitions.data.items);
    if (latest) setDefinitionID(`${latest.id}:${latest.version}`);
  }, [definitionID, definitions.data]);
  const selectedDefinition = definitions.data?.items.find((item) => `${item.id}:${item.version}` === definitionID);
  const summary = useQuery({ queryKey: ["governance-summary", projectID, definitionID, cohortStart, cohortEnd], queryFn: () => api.governanceSummary(queryOf({ project_id: projectID, definition_id: selectedDefinition?.id ?? "", definition_version: String(selectedDefinition?.version ?? ""), cohort_start: cohortStart, cohort_end: cohortEnd })), enabled: Boolean(projectID && selectedDefinition && cohortStart && cohortEnd), refetchInterval: polling, refetchOnWindowFocus: true });

  const refreshAll = async () => {
    await Promise.all([projects.refetch(), workUnits.refetch(), runs.refetch(), definitions.refetch(), outcomes.refetch(), selectedDefinition ? summary.refetch() : Promise.resolve(), selectedRun ? attempts.refetch() : Promise.resolve()]);
  };
  const dataTimes = [workUnits.dataUpdatedAt, runs.dataUpdatedAt, definitions.dataUpdatedAt, outcomes.dataUpdatedAt, summary.dataUpdatedAt].filter(Boolean);
  // "Updated" describes the consistent workspace, so show the oldest success
  // among its resources rather than overstating freshness from one fast query.
  const updatedAt = dataTimes.length ? Math.min(...dataTimes) : 0;
  const isRefreshing = [workUnits, runs, definitions, outcomes, summary, attempts].some((query) => query.isFetching);
  const hasRefetchError = [workUnits, runs, definitions, outcomes, summary, attempts].some((query) => query.isRefetchError);
  const isStale = isGovernanceDataStale(updatedAt, clock, hasRefetchError);
  const resourceError = [workUnits, runs, definitions, outcomes].find((query) => query.isError && !query.data)?.error;
  const resourcePending = workUnits.isPending || runs.isPending || definitions.isPending || outcomes.isPending;

  const saveDefinition = useMutation({
    mutationFn: ({ source, body }: { source: OutcomeDefinition | null; body: { name: string; data_type: "BOOLEAN" | "CATEGORICAL"; allowed_values: string[]; success_values: string[]; enabled: boolean } }) => {
      if (source) return api.createOutcomeDefinitionVersion(projectID, source.id, source.revision, body);
      const project = projects.data?.items.find((item) => item.id === projectID);
      if (!project) throw new Error("project unavailable");
      return api.createOutcomeDefinition(projectID, project.revision, body);
    },
    onSuccess: async () => { await Promise.all([queryClient.invalidateQueries({ queryKey: ["outcome-definitions", projectID] }), queryClient.invalidateQueries({ queryKey: ["governance-summary", projectID] })]); },
  });
  const toggleDefinition = useMutation({
    mutationFn: (item: OutcomeDefinition) => api.createOutcomeDefinitionVersion(projectID, item.id, item.revision, { name: item.name, data_type: item.data_type, allowed_values: item.allowed_values, success_values: item.success_values, enabled: !item.enabled }),
    onSuccess: async () => { await Promise.all([queryClient.invalidateQueries({ queryKey: ["outcome-definitions", projectID] }), queryClient.invalidateQueries({ queryKey: ["governance-summary", projectID] })]); },
  });

  function selectProject(id: string) {
    setDefinitionID("");
    setOverviewFilter("all");
    rememberGovernanceProject(id);
    updateContext({ projectID: id, view: "overview", workUnitID: "", runID: "" });
  }
  function selectView(next: GovernanceView) { updateContext({ view: next, workUnitID: next === "work-units" ? workUnitID : "", runID: next === "work-units" ? runID : "" }); }
  function openWorkUnits(filter: OverviewFilter, id = "") { setOverviewFilter(filter); updateContext({ view: "work-units", workUnitID: id, runID: "" }); }

  const selectedProject = governanceProjects.find((project) => project.id === projectID);
  const pageAction = <div className="governance-page-actions" role="group" aria-label={t("runGovernance.actions")}>
    <button type="button" className="button ghost governance-page-action" disabled={!projectID || isRefreshing} onClick={() => void refreshAll()}>
      <svg aria-hidden="true" viewBox="0 0 24 24"><path d="M20 12a8 8 0 1 1-2.34-5.66L20 8M20 3v5h-5" /></svg>
      <span>{isRefreshing ? t("runGovernance.refreshing") : t("runGovernance.refresh")}</span>
    </button>
    <a className="button ghost governance-page-action" href={projectID ? `/admin/projects?project_id=${encodeURIComponent(projectID)}` : "/admin/projects"}>
      <svg aria-hidden="true" viewBox="0 0 24 24"><path d="M4 6h6m4 0h6M10 3v6M4 12h10m4 0h2M14 9v6M4 18h3m4 0h9M7 15v6" /></svg>
      <span>{t("runGovernance.projectSettings")}</span>
    </a>
  </div>;

  return <>
    <PageHeader eyebrow={t("runGovernance.eyebrow")} title={t("runGovernance.title")} description={t("runGovernance.description")} action={pageAction} />
    <section className="governance-shell">
      <div className="governance-context-bar"><label><span>{t("runGovernance.projectContext")}</span><select value={projectID} onChange={(event) => selectProject(event.target.value)}><option value="">{t("runGovernance.chooseProject")}</option>{governanceProjects.map((project) => <option value={project.id} key={project.id}>{project.name}</option>)}</select></label>{selectedProject && <div className="governance-project-status"><GovernanceBadge tone="good">{t("runGovernance.governanceEnabled")}</GovernanceBadge><span>{selectedProject.name}</span></div>}<div className="governance-freshness" aria-live="polite"><span>{updatedAt ? t("runGovernance.updatedAt", { date: dateTime(new Date(updatedAt).toISOString(), "full") }) : t("runGovernance.awaitingData")}</span>{hasActiveRuns && <small>{t("runGovernance.autoRefreshActive")}</small>}</div></div>
      {!projects.isPending && governanceProjects.length === 0 && <EmptyState title={t("runGovernance.noGovernanceProjects")} action={<a className="button primary" href="/admin/projects">{t("runGovernance.configureProject")}</a>}>{t("runGovernance.noGovernanceProjectsDescription")}</EmptyState>}
      {governanceProjects.length > 1 && !projectID && <EmptyState title={t("runGovernance.chooseProject")}>{t("runGovernance.chooseProjectDescription")}</EmptyState>}
      {projects.isPending && <Loading />}{projects.isError && <ErrorState error={projects.error} />}
      {projectID && <><div className="governance-primary-nav"><SegmentedTabs id="run-governance" label={t("runGovernance.views")} value={view} items={[{ key: "overview", label: t("runGovernance.overview") }, { key: "work-units", label: t("runGovernance.workUnits") }, { key: "results", label: t("runGovernance.resultsAndDefinitions") }]} onChange={selectView} /></div>{isStale && <div className="notice warning governance-stale" role="status"><span>{hasRefetchError ? t("runGovernance.cachedSnapshot") : t("runGovernance.dataMayBeOutdated")}</span><button type="button" className="button ghost" disabled={isRefreshing} onClick={() => void refreshAll()}>{t("runGovernance.refresh")}</button></div>}{resourcePending && <Loading />}{resourceError && <ErrorState error={resourceError} action={<button className="button ghost" onClick={() => void refreshAll()}>{t("runGovernance.tryAgain")}</button>} />}{workUnits.data && runs.data && definitions.data && outcomes.data && !resourceError && <>{view === "overview" && <GovernanceOverview workUnits={workUnits.data.items} runs={runs.data.items} outcomes={outcomes.data.items} onOpenWorkUnits={openWorkUnits} />}{view === "work-units" && <WorkUnitExplorer workUnits={workUnits.data.items} runs={runs.data.items} outcomes={outcomes.data.items} selectedWorkUnitID={workUnitID} selectedRunID={runID} initialFilter={overviewFilter} attempts={attempts.data?.items} attemptsPending={attempts.isPending && Boolean(selectedRun)} attemptsError={attempts.error} onSelectWorkUnit={(id) => updateContext({ view: "work-units", workUnitID: id, runID: "" })} onSelectRun={(id) => updateContext({ view: "work-units", runID: id })} onCloseDetail={() => updateContext({ workUnitID: "", runID: "" })} />}{view === "results" && <OutcomeWorkspace key={projectID} definitions={definitions.data.items} outcomes={outcomes.data.items} workUnits={workUnits.data.items} definitionID={definitionID} cohortStart={cohortStart} cohortEnd={cohortEnd} summary={summary.data} summaryPending={summary.isPending} summaryError={summary.error} mutationError={saveDefinition.error ?? toggleDefinition.error} mutationPending={saveDefinition.isPending || toggleDefinition.isPending} onDefinitionChange={setDefinitionID} onCohortStartChange={setCohortStart} onCohortEndChange={setCohortEnd} onSaveDefinition={(source, body) => saveDefinition.mutateAsync({ source, body }).then(() => undefined)} onToggleDefinition={(item) => toggleDefinition.mutateAsync(item).then(() => undefined)} />}</>}</>}
    </section>
  </>;
}
