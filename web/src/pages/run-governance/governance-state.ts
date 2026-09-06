export type GovernanceView = "overview" | "work-units" | "results";
export type ResultsView = "analytics" | "definitions";

// Console state is deliberately memory-only. Halro's production artifact gate
// forbids every browser persistence API; a shareable URL is the durable form,
// while this remembers navigation away and back during the current SPA session.
let rememberedProjectID = "";

export function rememberGovernanceProject(projectID: string) {
  rememberedProjectID = projectID;
}

export function rememberedGovernanceProject() {
  return rememberedProjectID;
}

export function viewFromSearch(search: string): GovernanceView {
  const value = new URLSearchParams(search).get("view");
  return value === "work-units" || value === "results" ? value : "overview";
}

export function governanceContextFromSearch(search: string) {
  const params = new URLSearchParams(search);
  return {
    projectID: params.get("project_id") ?? "",
    view: viewFromSearch(search),
    workUnitID: params.get("work_unit_id") ?? "",
    runID: params.get("run_id") ?? "",
  };
}

export function writeGovernanceContext(value: {
  projectID: string;
  view: GovernanceView;
  workUnitID: string;
  runID: string;
}, mode: "push" | "replace" = "push") {
  const params = new URLSearchParams(window.location.search);
  const assign = (key: string, next: string) => next ? params.set(key, next) : params.delete(key);
  assign("project_id", value.projectID);
  assign("view", value.view === "overview" ? "" : value.view);
  assign("work_unit_id", value.workUnitID);
  assign("run_id", value.runID);
  const url = `${window.location.pathname}${params.size ? `?${params}` : ""}${window.location.hash}`;
  window.history[mode === "replace" ? "replaceState" : "pushState"]({}, "", url);
}

export function shortID(value: string) {
  if (value.length <= 18) return value;
  return `${value.slice(0, 10)}…${value.slice(-5)}`;
}

export function inDateRange(instant: string, start: string, end: string) {
  const date = instant.slice(0, 10);
  return (!start || date >= start) && (!end || date <= end);
}

export function localDateValue(date = new Date()) {
  const offset = date.getTimezoneOffset() * 60_000;
  return new Date(date.getTime() - offset).toISOString().slice(0, 10);
}

export function isGovernanceDataStale(updatedAt: number, now: number, refetchError: boolean) {
  return Boolean(updatedAt && (refetchError || now - updatedAt > 60_000));
}
