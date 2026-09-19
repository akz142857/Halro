import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useState, type Dispatch, type KeyboardEvent, type SetStateAction } from "react";
import { api } from "../api";
import { EmptyState, ErrorState, Loading, LoadMore, Modal, PageHeader, StatusDot } from "../components";
import { compactNumber, money, useInstantFormatter } from "../format";
import { Link, navigate, useNavigationLocation } from "../navigation";
import { useTranslation } from "react-i18next";
import { accountingTimeZone, isoToZonedInput, useAccountingTimeZone, zonedInputToISO } from "../timezone";
import { FailureDetailDrawer, providerAttributionFacts, providerIdentifierFacts } from "./FailureDetailDrawer";
import { UsageFailuresPanel } from "./UsageFailuresPanel";
import { UsageSummaryPanel } from "./UsageSummaryPanel";
import { attemptFailureLabel, errorClassAdvice, upstreamStatus } from "../failure";
import type { Deployment, PriceScheduleTier, Project, UsageAttempt } from "../types";

const usageTabs = ["summary", "failures", "attempts"] as const;
type UsageTab = (typeof usageTabs)[number];
type UsageTimeRange = "all" | "1h" | "24h" | "7d" | "custom";

function useDebouncedValue<T>(value: T, delay = 300) {
  const [debounced, setDebounced] = useState(value);
  useEffect(() => {
    const timer = window.setTimeout(() => setDebounced(value), delay);
    return () => window.clearTimeout(timer);
  }, [delay, value]);
  return debounced;
}

function hasAbsoluteRange(params: URLSearchParams) {
  return Boolean(params.get("start") || params.get("end"));
}

// Which filters a drill-down link can carry. A link that names one of them is
// asking for the list it filters, so it opens there rather than on the summary
// the operator would then have to leave.
const attemptFilterParams = [
  "request_id", "project_id", "model", "provider_id", "offering_id", "provider_model", "deployment_id",
  "status", "start", "end",
];

function usageTabFromURL(): UsageTab {
  const params = new URLSearchParams(window.location.search);
  const requested = params.get("tab");
  if (usageTabs.includes(requested as UsageTab)) return requested as UsageTab;
  return attemptFilterParams.some((name) => params.get(name)) ? "attempts" : "summary";
}

const usageTabID = (tab: UsageTab) => `usage-tab-${tab}`;
const usagePanelID = (tab: UsageTab) => `usage-panel-${tab}`;

export function UsagePage() {
  const { t } = useTranslation();
  const navigationLocation = useNavigationLocation();
  const [tab, setTab] = useState<UsageTab>(usageTabFromURL);
  const dateTime = useInstantFormatter();
  // Every project ever billed can show up in Usage history, including a
  // disabled or since-deleted one, so this list is unfiltered — narrowing it
  // to "currently enabled" would make an old project's calls unfindable.
  const projects = useQuery({ queryKey: ["projects"], queryFn: api.projects });
  // The model filter matches the requested model exactly (internal/usage,
  // query.go), so the free-text box it replaces answered every typo with an
  // empty table and no way to tell that apart from "no calls yet". The public
  // aliases come from the routes, which is what a caller is able to ask for.
  const routes = useQuery({ queryKey: ["routes"], queryFn: api.routes });
  // Names for the deployment each attempt actually ran on. A deleted deployment
  // is absent from this list and its history is not, so every read falls back to
  // the ID — which is the value the ledger and the Parquet partitions carry, and
  // the one to correlate with.
  const deployments = useQuery({ queryKey: ["deployments"], queryFn: api.deployments });
  const projectNames = useMemo(
    () => Object.fromEntries((projects.data?.items ?? []).map((project) => [project.id, project.name])),
    [projects.data?.items],
  );
  const deploymentNames = useMemo(
    () => Object.fromEntries((deployments.data?.items ?? []).map((item) => [item.id, item.name])),
    [deployments.data?.items],
  );
  const [status, setStatus] = useState(() => new URLSearchParams(window.location.search).get("status") ?? "");
  const [model, setModel] = useState(() => new URLSearchParams(window.location.search).get("model") ?? "");
  const [providerModel, setProviderModel] = useState(() => new URLSearchParams(window.location.search).get("provider_model") ?? "");
  // No control of its own. It was a free-text box wanting an opaque
  // `provider_...` ID, which nobody has to hand — the deployment select beside
  // it answers the same question by name and identifies the target more
  // precisely, since one provider serves several. The filter still applies when
  // the summary's provider row links here, and then it is shown as something
  // that can be cleared: a filter with no visible control is how a table comes
  // to look empty for no reason.
  const [providerID, setProviderID] = useState(() => new URLSearchParams(window.location.search).get("provider_id") ?? "");
  const [offeringID, setOfferingID] = useState(() => new URLSearchParams(window.location.search).get("offering_id") ?? "");
  const [requestID, setRequestID] = useState(() => new URLSearchParams(window.location.search).get("request_id") ?? "");
  const [projectID, setProjectID] = useState(() => new URLSearchParams(window.location.search).get("project_id") ?? "");
  const [deploymentID, setDeploymentID] = useState(() => new URLSearchParams(window.location.search).get("deployment_id") ?? "");
  const [mobileFiltersOpen, setMobileFiltersOpen] = useState(false);
  const [timeRange, setTimeRange] = useState<UsageTimeRange>(() => hasAbsoluteRange(new URLSearchParams(window.location.search)) ? "custom" : "all");
  const timeZone = useAccountingTimeZone();
  // A summary row links here with the absolute interval it covered. The inputs
  // are wall-clock in the accounting zone, so the instants are converted once
  // on arrival rather than being reconstructed from a date label — the same
  // label under two generations of the zone is two different windows.
  const [start, setStart] = useState(() => isoToZonedInput(
    new URLSearchParams(window.location.search).get("start") ?? undefined, accountingTimeZone()));
  const [end, setEnd] = useState(() => isoToZonedInput(
    new URLSearchParams(window.location.search).get("end") ?? undefined, accountingTimeZone()));
  const debouncedRequestID = useDebouncedValue(requestID);
  const debouncedProviderModel = useDebouncedValue(providerModel);
  const usage = useInfiniteQuery({
    queryKey: ["usage", status, model, debouncedProviderModel, providerID, offeringID, deploymentID, debouncedRequestID, projectID, start, end, timeZone],
    initialPageParam: "",
    queryFn: ({ pageParam }) => api.usage(`?${new URLSearchParams({
      limit: "100", ...(status ? { status } : {}), ...(model ? { model } : {}), ...(debouncedRequestID ? { request_id: debouncedRequestID } : {}),
      ...(projectID ? { project_id: projectID } : {}),
      ...(providerID ? { provider_id: providerID } : {}),
      ...(offeringID ? { offering_id: offeringID } : {}),
      ...(deploymentID ? { deployment_id: deploymentID } : {}),
      ...(debouncedProviderModel ? { provider_model: debouncedProviderModel } : {}),
      ...(start ? { start: zonedInputToISO(start, timeZone) } : {}),
      ...(end ? { end: zonedInputToISO(end, timeZone) } : {}),
      ...(pageParam ? { cursor: pageParam } : {}),
    })}`),
    getNextPageParam: (page) => page.next_cursor || undefined,
  });
  const attempts = usage.data?.pages.flatMap((page) => page.items) ?? [];
  const activeFilterCount = [requestID, projectID, model, deploymentID, providerModel, status, providerID, offeringID]
    .filter(Boolean).length + (start || end ? 1 : 0);
  const clearFilters = () => {
    setRequestID("");
    setProjectID("");
    setModel("");
    setDeploymentID("");
    setProviderModel("");
    setStatus("");
    setStart("");
    setEnd("");
    setTimeRange("all");
    setProviderID("");
    setOfferingID("");
  };
  const selectTimeRange = (next: UsageTimeRange) => {
    setTimeRange(next);
    if (next === "custom") return;
    if (next === "all") {
      setStart("");
      setEnd("");
      return;
    }
    const duration = next === "1h" ? 60 * 60 * 1000 : next === "24h" ? 24 * 60 * 60 * 1000 : 7 * 24 * 60 * 60 * 1000;
    const now = Date.now();
    setStart(isoToZonedInput(new Date(now - duration).toISOString(), timeZone));
    setEnd(isoToZonedInput(new Date(now).toISOString(), timeZone));
  };
  // A route that has since been deleted still has history, and its alias would
  // otherwise be unreachable from here — the same reason the project list above
  // is left unfiltered. The models actually present in the loaded rows are
  // folded in, and so is the current selection, because a select whose value is
  // absent from its options renders blank and looks like no filter is applied.
  const models = useMemo(() => {
    const aliases = new Set<string>();
    for (const route of routes.data?.items ?? []) if (route.public_model) aliases.add(route.public_model);
    for (const page of usage.data?.pages ?? []) {
      for (const attempt of page.items) if (attempt.requested_model) aliases.add(attempt.requested_model);
    }
    if (model) aliases.add(model);
    return [...aliases].sort((left, right) => left.localeCompare(right));
  }, [routes.data?.items, usage.data?.pages, model]);
  useEffect(() => {
    const params = new URLSearchParams(window.location.search);
    setTab(usageTabFromURL());
    setStatus(params.get("status") ?? "");
    setModel(params.get("model") ?? "");
    setProviderModel(params.get("provider_model") ?? "");
    setProviderID(params.get("provider_id") ?? "");
    setOfferingID(params.get("offering_id") ?? "");
    setRequestID(params.get("request_id") ?? "");
    setProjectID(params.get("project_id") ?? "");
    setDeploymentID(params.get("deployment_id") ?? "");
    setStart(isoToZonedInput(params.get("start") ?? undefined, accountingTimeZone()));
    setEnd(isoToZonedInput(params.get("end") ?? undefined, accountingTimeZone()));
    setTimeRange(hasAbsoluteRange(params) ? "custom" : "all");
  }, [navigationLocation]);
  const selectTab = (next: UsageTab) => {
    if (next === tab) return;
    const url = new URL(window.location.href);
    url.searchParams.set("tab", next);
    navigate(`${url.pathname}${url.search}${url.hash}`);
  };
  const onTabKeys = (event: KeyboardEvent<HTMLDivElement>) => {
    const index = usageTabs.indexOf(tab);
    const next = event.key === "ArrowRight" ? (index + 1) % usageTabs.length
      : event.key === "ArrowLeft" ? (index + usageTabs.length - 1) % usageTabs.length
        : event.key === "Home" ? 0
          : event.key === "End" ? usageTabs.length - 1
            : -1;
    if (next < 0) return;
    event.preventDefault();
    selectTab(usageTabs[next]);
    document.getElementById(usageTabID(usageTabs[next]))?.focus();
  };
  const renderFilterFields = (showAdvanced: boolean) => (
    <UsageAttemptFilterFields
      projects={projects.data?.items ?? []}
      deployments={deployments.data?.items ?? []}
      models={models}
      requestID={requestID} setRequestID={setRequestID}
      projectID={projectID} setProjectID={setProjectID}
      model={model} setModel={setModel}
      status={status} setStatus={setStatus}
      timeRange={timeRange} setTimeRange={selectTimeRange}
      start={start} setStart={setStart}
      end={end} setEnd={setEnd}
      deploymentID={deploymentID} setDeploymentID={setDeploymentID}
      providerModel={providerModel} setProviderModel={setProviderModel}
      deploymentNames={deploymentNames}
      showAdvanced={showAdvanced}
    />
  );
  const appliedFilters = [
    requestID ? { key: "request", label: `${t("usage.requestID")}: ${requestID}`, clear: () => setRequestID("") } : null,
    projectID ? { key: "project", label: `${t("usage.project")}: ${projectNames[projectID] || projectID}`, clear: () => setProjectID("") } : null,
    model ? { key: "model", label: `${t("usage.model")}: ${model}`, clear: () => setModel("") } : null,
    status ? { key: "status", label: `${t("usage.status")}: ${status === "success" ? t("usage.success") : t("usage.error")}`, clear: () => setStatus("") } : null,
    start || end ? {
      key: "time", label: `${t("usage.timeRange")}: ${timeRange === "custom" ? `${start || "…"} – ${end || "…"}` : t(`usage.timeRanges.${timeRange}`)}`,
      clear: () => { setStart(""); setEnd(""); setTimeRange("all"); },
    } : null,
    deploymentID ? { key: "deployment", label: `${t("usage.deployment")}: ${deploymentNames[deploymentID] || deploymentID}`, clear: () => setDeploymentID("") } : null,
    providerModel ? { key: "provider-model", label: `${t("usage.actualModel")}: ${providerModel}`, clear: () => setProviderModel("") } : null,
    providerID ? { key: "provider", label: t("usage.providerFilter", { provider: providerID }), clear: () => setProviderID("") } : null,
    offeringID ? {
      key: "offering", label: t("usage.offeringFilter", { offering: t(`providers.offerings.${offeringID}`, { defaultValue: offeringID }) }),
      clear: () => setOfferingID(""),
    } : null,
  ].filter((filter): filter is { key: string; label: string; clear: () => void } => filter !== null);
  return (
    <>
      <PageHeader
        eyebrow={t("usage.eyebrow")}
        title={t("usage.title")}
        description={t("usage.description")}
      />
      <div className="provider-tabs-shell">
        <div className="provider-tabs" role="tablist" aria-label={t("usage.views")} onKeyDown={onTabKeys}>
          {usageTabs.map((key) => (
            <button
              key={key}
              role="tab"
              id={usageTabID(key)}
              aria-controls={usagePanelID(key)}
              aria-selected={tab === key}
              tabIndex={tab === key ? 0 : -1}
              onClick={() => selectTab(key)}
            >{t(`usage.tabs.${key}`)}</button>
          ))}
        </div>
      </div>
      {tab === "summary" && (
        <section role="tabpanel" id={usagePanelID("summary")} aria-labelledby={usageTabID("summary")}>
          <UsageSummaryPanel />
        </section>
      )}
      {tab === "failures" && (
        <section role="tabpanel" id={usagePanelID("failures")} aria-labelledby={usageTabID("failures")}>
          <UsageFailuresPanel />
        </section>
      )}
      {tab === "attempts" && (
      <section className="usage-attempts-panel" role="tabpanel" id={usagePanelID("attempts")} aria-labelledby={usageTabID("attempts")}>
      <div className="usage-filter-panel">
        <div className="usage-filter-content" id="usage-attempt-filters">
          <div className="usage-filter-toolbar">
            {renderFilterFields(false)}
            <button type="button" className="button ghost usage-filter-open" onClick={() => setMobileFiltersOpen(true)}>
              {t("usage.filtersTitle")}{activeFilterCount > 0 ? ` (${activeFilterCount})` : ""}
            </button>
          </div>
          {appliedFilters.length > 0 && (
            <div className="usage-applied-filters" aria-label={t("usage.appliedFilters")}>
              {appliedFilters.map((filter) => <button type="button" className="filter-chip" key={filter.key} onClick={filter.clear}>{filter.label}<span aria-hidden="true"> ×</span></button>)}
            </div>
          )}
        </div>
        {mobileFiltersOpen && (
          <Modal drawer title={t("usage.filtersTitle")} onClose={() => setMobileFiltersOpen(false)}>
            <div className="usage-filter-drawer">
              <div className="usage-filter-drawer-summary">
                <span>{activeFilterCount > 0 ? t("usage.activeFilters", { count: activeFilterCount }) : t("usage.noActiveFilters")}</span>
                {activeFilterCount > 0 && <button type="button" className="button ghost" onClick={clearFilters}>{t("usage.clearFilters")}</button>}
              </div>
              {renderFilterFields(true)}
              {appliedFilters.length > 0 && (
                <div className="usage-applied-filters" aria-label={t("usage.appliedFilters")}>
                  {appliedFilters.map((filter) => <button type="button" className="filter-chip" key={filter.key} onClick={filter.clear}>{filter.label}<span aria-hidden="true"> ×</span></button>)}
                </div>
              )}
              <div className="form-actions"><button type="button" className="button primary" onClick={() => setMobileFiltersOpen(false)}>{t("usage.done")}</button></div>
            </div>
          </Modal>
        )}
      </div>
      {usage.isPending && <Loading />}
      {usage.isError && <ErrorState error={usage.error} />}
      {/* Filtering to nothing rendered a table with only a header, which reads
          as a broken page rather than as an answer. Every other list here says
          so in words. */}
      {usage.data && attempts.length === 0 && (
        <EmptyState title={t("usage.emptyTitle")}>{t("usage.emptyDescription")}</EmptyState>
      )}
      {usage.data && attempts.length > 0 && (
        <div className="usage-results-panel">
          <header className="usage-results-header">
            <div>
              <h2>{t("usage.attemptsTitle")}</h2>
              <p>{t("usage.attemptsDescription")}</p>
            </div>
            <span className="usage-result-count" aria-live="polite">{t("usage.records", { count: attempts.length })}</span>
          </header>
          <div className="table-shell usage-table-shell">
          <table className="usage-table">
            <colgroup>
              <col style={{ width: "25%" }} /><col style={{ width: "31%" }} />
              <col style={{ width: "20%" }} /><col style={{ width: "24%" }} />
            </colgroup>
            <thead><tr><th scope="col">{t("usage.request")}</th><th scope="col">{t("usage.route")}</th><th scope="col">{t("usage.result")}</th><th scope="col">{t("usage.usageAndCost")}</th></tr></thead>
            <tbody>
              {attempts.map((attempt) => (
                <tr key={attempt.event_id}>
                  <td className="usage-request-cell" data-label={t("usage.request")}>
                    <code className="usage-request-id" title={attempt.request_id}>{attempt.request_id}</code>
                    <div className="usage-request-context">
                      <Link className="resource-link" href={`/admin/projects?project_id=${encodeURIComponent(attempt.project_id)}`}>{projectNames[attempt.project_id] || attempt.project_id}</Link>
                      <span aria-hidden="true">·</span>
                      <span>{t("usage.attempt", { count: attempt.attempt })}</span>
                    </div>
                  </td>
                  <td className="usage-route-cell" data-label={t("usage.route")}>
                    <div className="usage-route-primary">
                      <strong>{attempt.requested_model || "—"}</strong>
                      <span aria-hidden="true">→</span>
                      {attempt.deployment_id ? (
                        <Link className="resource-link" href={`/admin/deployments?q=${encodeURIComponent(attempt.deployment_id)}`}>
                          {deploymentNames[attempt.deployment_id] || attempt.deployment_id}
                        </Link>
                      ) : <span>—</span>}
                    </div>
                    <div className="usage-route-meta">
                      {attempt.deployment_id && deploymentNames[attempt.deployment_id] && <code>{attempt.deployment_id}</code>}
                      <span>{attempt.provider_model || "—"}</span>
                      {attempt.offering_id && <> · <span>{t(`providers.offerings.${attempt.offering_id}`, { defaultValue: attempt.offering_id })}</span></>}
                    </div>
                  </td>
                  <td className="usage-result-cell" data-label={t("usage.result")}>
                    <AttemptStatusCell attempt={attempt} />
                    <div className="usage-result-meta">
                      <strong>{attempt.latency_millis} ms</strong>
                      <span aria-hidden="true">·</span>
                      <time dateTime={attempt.completed_at}>{dateTime(attempt.completed_at, "dateTimeYear")}</time>
                    </div>
                    <AttemptDetailCell attempt={attempt} projectName={projectNames[attempt.project_id]} deploymentName={attempt.deployment_id ? deploymentNames[attempt.deployment_id] : undefined} />
                  </td>
                  <td className="usage-accounting-cell" data-label={t("usage.usageAndCost")}>
                    <div className="usage-accounting-summary">
                      <div className="usage-accounting-metric">
                        <span>{t("usage.tokens")}</span>
                        <strong>{compactNumber(attempt.provider_input_tokens + attempt.provider_output_tokens)}</strong>
                      </div>
                      <div className="usage-accounting-metric">
                        <span>{t("usage.cost")}</span>
                        <div className="usage-cost-value"><CostValue attempt={attempt} /></div>
                      </div>
                    </div>
                    <p className="usage-token-breakdown">{t("usage.inputOutput", { input: compactNumber(attempt.provider_input_tokens), output: compactNumber(attempt.provider_output_tokens) })} · {attempt.tokens_estimated ? t("usage.conservative") : t("usage.reported")}</p>
                    <CostEvidence attempt={attempt} />
                  </td>
                </tr>
              ))}
            </tbody>
          </table>
          {/* The shared control, so this list pages on scroll like the others
              rather than only on a click. */}
          {usage.hasNextPage && (
            <LoadMore label={t("common.loadMore")} busy={usage.isFetchingNextPage} onLoad={() => usage.fetchNextPage()} />
          )}
          </div>
        </div>
      )}
      </section>
      )}
    </>
  );
}

function UsageAttemptFilterFields({
  projects, deployments, models,
  requestID, setRequestID, projectID, setProjectID, model, setModel, status, setStatus,
  timeRange, setTimeRange, start, setStart, end, setEnd,
  deploymentID, setDeploymentID, providerModel, setProviderModel, deploymentNames,
  showAdvanced,
}: {
  projects: Project[];
  deployments: Deployment[];
  models: string[];
  requestID: string; setRequestID: Dispatch<SetStateAction<string>>;
  projectID: string; setProjectID: Dispatch<SetStateAction<string>>;
  model: string; setModel: Dispatch<SetStateAction<string>>;
  status: string; setStatus: Dispatch<SetStateAction<string>>;
  timeRange: UsageTimeRange; setTimeRange: (range: UsageTimeRange) => void;
  start: string; setStart: Dispatch<SetStateAction<string>>;
  end: string; setEnd: Dispatch<SetStateAction<string>>;
  deploymentID: string; setDeploymentID: Dispatch<SetStateAction<string>>;
  providerModel: string; setProviderModel: Dispatch<SetStateAction<string>>;
  deploymentNames: Record<string, string>;
  showAdvanced: boolean;
}) {
  const { t } = useTranslation();
  return (
    <div className="usage-filter-fields">
      <div className="usage-filter-grid usage-filter-primary">
        <label><span>{t("usage.requestID")}</span><input autoComplete="off" value={requestID} onChange={(event) => setRequestID(event.target.value)} placeholder="req_…" /></label>
        <label>
          <span>{t("usage.project")}</span>
          <select value={projectID} onChange={(event) => setProjectID(event.target.value)}>
            <option value="">{t("usage.all")}</option>
            {projects.map((project) => <option key={project.id} value={project.id}>{project.name || project.id}</option>)}
          </select>
        </label>
        <label>
          <span>{t("usage.model")}</span>
          <select value={model} onChange={(event) => setModel(event.target.value)}>
            <option value="">{t("usage.all")}</option>
            {models.map((alias) => <option key={alias} value={alias}>{alias}</option>)}
          </select>
        </label>
        <label>
          <span>{t("usage.status")}</span>
          <select value={status} onChange={(event) => setStatus(event.target.value)}>
            <option value="">{t("usage.all")}</option>
            <option value="success">{t("usage.success")}</option>
            <option value="error">{t("usage.error")}</option>
          </select>
        </label>
        <label>
          <span>{t("usage.timeRange")}</span>
          <select value={timeRange} onChange={(event) => setTimeRange(event.target.value as UsageTimeRange)}>
            <option value="all">{t("usage.timeRanges.all")}</option>
            <option value="1h">{t("usage.timeRanges.1h")}</option>
            <option value="24h">{t("usage.timeRanges.24h")}</option>
            <option value="7d">{t("usage.timeRanges.7d")}</option>
            <option value="custom">{t("usage.timeRanges.custom")}</option>
          </select>
        </label>
      </div>
      {timeRange === "custom" && (
        <div className="usage-custom-range">
          <label><span>{t("usage.start")}</span><input autoComplete="off" type="datetime-local" value={start} onChange={(event) => setStart(event.target.value)} /></label>
          <span aria-hidden="true">–</span>
          <label><span>{t("usage.end")}</span><input autoComplete="off" type="datetime-local" value={end} onChange={(event) => setEnd(event.target.value)} /></label>
        </div>
      )}
      {showAdvanced && (
        <div className="usage-filter-grid usage-filter-advanced">
          <label>
            <span>{t("usage.deployment")}</span>
            <select value={deploymentID} onChange={(event) => setDeploymentID(event.target.value)}>
              <option value="">{t("usage.all")}</option>
              {deployments.map((item) => <option key={item.id} value={item.id}>{item.name || item.id}</option>)}
              {deploymentID && !deploymentNames[deploymentID] && <option value={deploymentID}>{deploymentID}</option>}
            </select>
          </label>
          <label><span>{t("usage.actualModel")}</span><input autoComplete="off" value={providerModel} onChange={(event) => setProviderModel(event.target.value)} /></label>
        </div>
      )}
    </div>
  );
}

// What a failed attempt actually says. Every field here was already in the
// response and none of it was shown: the cell read "error" and the operator was
// left to guess whether a credential, a quota, a timeout or a malformed payload
// produced it — which is the whole reason the ledger classifies failures at all.
//
// The class and the upstream status are the headline because they are what
// separates "go look at the provider" from "go look at the request". Everything
// that needs a second to read — what to check, which rung of the retry chain
// this was — goes behind the disclosure, so a table of successful calls does
// not grow a column of prose.
function AttemptStatusCell({ attempt }: { attempt: UsageAttempt }) {
  const { t } = useTranslation();
  if (attempt.status === "success") {
    return <span className="inline-status"><StatusDot ok label={t("usage.success")} />{t("usage.success")}</span>;
  }
  return (
    <>
      <span className="inline-status"><StatusDot ok={false} label={t("usage.error")} />{attemptFailureLabel(t, attempt)}</span>
      {/* The status is kept apart from the class rather than folded into one
          string: an operator taking a 429 to a provider's support desk quotes
          the number, and a class alone cannot be quoted. */}
      {upstreamStatus(attempt.http_status) ? <small>{t("usage.httpStatus", { status: attempt.http_status })}</small> : null}
    </>
  );
}

// The same drawer the failed-request list opens, on the row that explains one
// call rather than one request. The detail used to expand inside the status
// cell, which grew the row under the operator's pointer and put a request body
// in a column five of whose cells are one line tall.
function AttemptDetailCell({ attempt, projectName, deploymentName }: {
  attempt: UsageAttempt; projectName?: string; deploymentName?: string;
}) {
  const { t } = useTranslation();
  const dateTime = useInstantFormatter();
  const [open, setOpen] = useState(false);
  if (attempt.status === "success") return null;
  const identifiers = providerIdentifierFacts(t, attempt);
  const attribution = providerAttributionFacts(t, attempt);
  const chain = attempt.retry_count > 0 || attempt.fallback_count > 0
    ? t("usage.attemptChain", { fallback: attempt.fallback_count + 1, retry: attempt.retry_count })
    : t("usage.attemptFirstTry");
  return (
    <>
      <button type="button" className="resource-link failure-detail-open" onClick={() => setOpen(true)}>
        {t("usage.attemptDetails")}
      </button>
      {open && (
        <FailureDetailDrawer
          title={t("usage.attemptDetailsTitle", { count: attempt.attempt })}
          onClose={() => setOpen(false)}
          advice={errorClassAdvice(t, attempt.error_class) || t("usage.failures.noAdvice")}
          identifiersUnrecorded={identifiers.unrecorded}
          // Keyed by the request, because that is what a payload belongs to. An
          // attempt of a request that went on to succeed has none, and the
          // panel says so rather than implying capture failed.
          requestID={attempt.request_id}
          links={
            <Link href={`/admin/usage?tab=attempts&request_id=${encodeURIComponent(attempt.request_id)}`}>
              {t("usage.failures.viewAttemptChain")} →
            </Link>
          }
          facts={[
            { label: t("usage.failures.cause"), value: attemptFailureLabel(t, attempt), emphasis: true },
            { label: t("usage.time"), value: dateTime(attempt.completed_at, "full") },
            { label: t("usage.requestID"), value: attempt.request_id, code: true },
            { label: t("usage.project"), value: projectName || attempt.project_id },
            { label: t("usage.model"), value: attempt.requested_model },
            { label: t("usage.deployment"), value: attempt.deployment_id ? deploymentName || attempt.deployment_id : undefined },
            ...attribution,
            { label: t("usage.actualModel"), value: attempt.provider_model },
            { label: t("usage.status"), value: upstreamStatus(attempt.http_status) ? t("usage.httpStatus", { status: attempt.http_status }) : undefined },
            ...identifiers.facts,
            { label: t("usage.failures.providerReasonLabel"), value: attempt.provider_failure_reason ? t(`usage.failures.providerReasons.${attempt.provider_failure_reason}`, { defaultValue: attempt.provider_failure_reason }) : undefined },
            { label: t("usage.failures.retryabilityLabel"), value: attempt.failure_semantics_recorded ? t(attempt.retryable ? "usage.failures.retryable" : "usage.failures.notRetryable") : t("usage.failures.notRecorded") },
            { label: t("usage.failures.ambiguityLabel"), value: attempt.failure_semantics_recorded ? t(attempt.ambiguous ? "usage.failures.ambiguous" : "usage.failures.unambiguous") : t("usage.failures.notRecorded") },
            { label: t("usage.latency"), value: `${attempt.latency_millis} ms` },
            { label: t("usage.failures.chainLabel"), value: chain },
          ]}
        />
      )}
    </>
  );
}

function clockOf(minute: number) {
  return `${String(Math.floor(minute / 60)).padStart(2, "0")}:${String(minute % 60).padStart(2, "0")}`;
}

// Names the rung a settled attempt was billed at. The zone-unavailable case is
// called out rather than smoothed over: it means the attempt was billed at the
// dearest rate the version could express because its zone could not be
// resolved, and that is worth someone looking into.
export function billedTierLabel(tier: PriceScheduleTier, t: (key: string, values?: Record<string, unknown>) => string) {
  if (tier.source === "window" && tier.start_minute != null && tier.end_minute != null) {
    return t("usage.billedWindow", { start: clockOf(tier.start_minute), end: clockOf(tier.end_minute), timezone: tier.timezone });
  }
  if (tier.source === "base") return t("usage.billedBase", { timezone: tier.timezone });
  return t("usage.billedZoneUnavailable", { timezone: tier.timezone });
}

// The amount and its classification belong in the scan line. The arithmetic
// that produced it is evidence, not another metric, so it remains in a separate
// disclosure below the token and cost summary.
function CostValue({ attempt }: { attempt: UsageAttempt }) {
  const { t } = useTranslation();
  return (
    <>
      <strong>{attempt.cost_micros_usd == null ? t("usage.unknownCost") : money(attempt.cost_micros_usd)}</strong>
      {!!attempt.tags?.length && (
        <span className="usage-cost-tags">
          {attempt.tags.map((tag) => <span className="badge" key={tag}>{tag}</span>)}
        </span>
      )}
    </>
  );
}

// The pricing evidence disclosure shows how a settled attempt's cost was
// reached: the price snapshot it billed against and the input/output/fixed
// components that summed to it.
function CostEvidence({ attempt }: { attempt: UsageAttempt }) {
  const { t } = useTranslation();
  return (
      <details className="cost-evidence">
        <summary>{t("usage.costEvidence")}</summary>
        <small>
          {attempt.price_snapshot?.price_version_id ? `${attempt.price_snapshot.price_version_id} · v${attempt.price_snapshot.price_version}` : attempt.price_evidence_status}<br />
          {/* Two attempts with identical token counts can settle at different
              amounts once a price bills by time of day. Without the rung, that
              difference has no explanation anywhere the operator can reach. */}
          {attempt.price_snapshot?.schedule_tier && <>{billedTierLabel(attempt.price_snapshot.schedule_tier, t)}<br /></>}
          {attempt.input_cost_micros_usd == null ? "" : t("usage.formulaComponents", { input: money(attempt.input_cost_micros_usd), output: money(attempt.output_cost_micros_usd ?? 0), fixed: money(attempt.fixed_cost_micros_usd ?? 0) })}
        </small>
      </details>
  );
}
