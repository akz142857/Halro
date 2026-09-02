import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useState, type KeyboardEvent } from "react";
import { api } from "../api";
import { EmptyState, ErrorState, Loading, LoadMore, PageHeader, StatusDot } from "../components";
import { compactNumber, money, useInstantFormatter } from "../format";
import { Link } from "../navigation";
import { useTranslation } from "react-i18next";
import { accountingTimeZone, isoToZonedInput, useAccountingTimeZone, zonedInputToISO } from "../timezone";
import { ProviderIdentifiers, UsageFailuresPanel } from "./UsageFailuresPanel";
import { UsageSummaryPanel } from "./UsageSummaryPanel";
import { attemptFailureLabel, errorClassAdvice } from "../failure";
import type { PriceScheduleTier, UsageAttempt } from "../types";

const usageTabs = ["summary", "failures", "attempts"] as const;
type UsageTab = (typeof usageTabs)[number];

// Which filters a drill-down link can carry. A link that names one of them is
// asking for the list it filters, so it opens there rather than on the summary
// the operator would then have to leave.
const attemptFilterParams = [
  "request_id", "project_id", "model", "provider_id", "provider_model", "deployment_id",
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
  const [status, setStatus] = useState("");
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
  const [requestID, setRequestID] = useState(() => new URLSearchParams(window.location.search).get("request_id") ?? "");
  const [projectID, setProjectID] = useState(() => new URLSearchParams(window.location.search).get("project_id") ?? "");
  const [deploymentID, setDeploymentID] = useState(() => new URLSearchParams(window.location.search).get("deployment_id") ?? "");
  const timeZone = useAccountingTimeZone();
  // A summary row links here with the absolute interval it covered. The inputs
  // are wall-clock in the accounting zone, so the instants are converted once
  // on arrival rather than being reconstructed from a date label — the same
  // label under two generations of the zone is two different windows.
  const [start, setStart] = useState(() => isoToZonedInput(
    new URLSearchParams(window.location.search).get("start") ?? undefined, accountingTimeZone()));
  const [end, setEnd] = useState(() => isoToZonedInput(
    new URLSearchParams(window.location.search).get("end") ?? undefined, accountingTimeZone()));
  const usage = useInfiniteQuery({
    queryKey: ["usage", status, model, providerModel, providerID, deploymentID, requestID, projectID, start, end, timeZone],
    initialPageParam: "",
    queryFn: ({ pageParam }) => api.usage(`?${new URLSearchParams({
      limit: "100", ...(status ? { status } : {}), ...(model ? { model } : {}), ...(requestID ? { request_id: requestID } : {}),
      ...(projectID ? { project_id: projectID } : {}),
      ...(providerID ? { provider_id: providerID } : {}),
      ...(deploymentID ? { deployment_id: deploymentID } : {}),
      ...(providerModel ? { provider_model: providerModel } : {}),
      ...(start ? { start: zonedInputToISO(start, timeZone) } : {}),
      ...(end ? { end: zonedInputToISO(end, timeZone) } : {}),
      ...(pageParam ? { cursor: pageParam } : {}),
    })}`),
    getNextPageParam: (page) => page.next_cursor || undefined,
  });
  const attempts = usage.data?.pages.flatMap((page) => page.items) ?? [];
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
    const syncTab = () => setTab(usageTabFromURL());
    window.addEventListener("popstate", syncTab);
    return () => window.removeEventListener("popstate", syncTab);
  }, []);
  const selectTab = (next: UsageTab) => {
    if (next === tab) return;
    setTab(next);
    const url = new URL(window.location.href);
    url.searchParams.set("tab", next);
    window.history.pushState({}, "", `${url.pathname}${url.search}${url.hash}`);
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
      <section role="tabpanel" id={usagePanelID("attempts")} aria-labelledby={usageTabID("attempts")}>
      <div className="filter-bar">
        <label><span>{t("usage.requestID")}</span><input autoComplete="off" value={requestID} onChange={(event) => setRequestID(event.target.value)} placeholder="req_…" /></label>
        <label>
          <span>{t("usage.project")}</span>
          <select value={projectID} onChange={(event) => setProjectID(event.target.value)}>
            <option value="">{t("usage.all")}</option>
            {(projects.data?.items ?? []).map((project) => <option key={project.id} value={project.id}>{project.name || project.id}</option>)}
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
          <span>{t("usage.deployment")}</span>
          <select value={deploymentID} onChange={(event) => setDeploymentID(event.target.value)}>
            <option value="">{t("usage.all")}</option>
            {(deployments.data?.items ?? []).map((item) => <option key={item.id} value={item.id}>{item.name || item.id}</option>)}
            {deploymentID && !deploymentNames[deploymentID] && <option value={deploymentID}>{deploymentID}</option>}
          </select>
        </label>
        <label><span>{t("usage.actualModel")}</span><input autoComplete="off" value={providerModel} onChange={(event) => setProviderModel(event.target.value)} /></label>
        <label>
          <span>{t("usage.status")}</span>
          <select value={status} onChange={(event) => setStatus(event.target.value)}>
            <option value="">{t("usage.all")}</option>
            <option value="success">{t("usage.success")}</option>
            <option value="error">{t("usage.error")}</option>
          </select>
        </label>
        <label><span>{t("usage.start")}</span><input autoComplete="off" type="datetime-local" value={start} onChange={(event) => setStart(event.target.value)} /></label>
        <label><span>{t("usage.end")}</span><input autoComplete="off" type="datetime-local" value={end} onChange={(event) => setEnd(event.target.value)} /></label>
        {providerID && (
          <button type="button" className="filter-chip" onClick={() => setProviderID("")}>
            {t("usage.providerFilter", { provider: providerID })}
            <span aria-hidden="true"> ×</span>
          </button>
        )}
        <span className="filter-count">{t("usage.records", { count: attempts.length })}</span>
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
        <div className="table-shell">
          <table className="usage-table">
            <colgroup>
              <col style={{ width: "14%" }} /><col style={{ width: "9%" }} /><col style={{ width: "10%" }} /><col style={{ width: "13%" }} />
              <col style={{ width: "12%" }} /><col style={{ width: "18%" }} /><col style={{ width: "6%" }} /><col style={{ width: "6%" }} />
              <col style={{ width: "12%" }} />
            </colgroup>
            <thead><tr><th>{t("usage.request")}</th><th>{t("usage.project")}</th><th>{t("usage.model")}</th><th>{t("usage.deployment")}</th><th>{t("usage.tokens")}</th><th>{t("usage.cost")}</th><th>{t("usage.latency")}</th><th>{t("usage.status")}</th><th>{t("usage.time")}</th></tr></thead>
            <tbody>
              {attempts.map((attempt) => (
                <tr key={attempt.event_id}>
                  <td><code>{attempt.request_id}</code><small>{t("usage.attempt", { count: attempt.attempt })}</small></td>
                  <td>
                    <Link className="resource-link" href={`/admin/projects?project_id=${encodeURIComponent(attempt.project_id)}`}>{projectNames[attempt.project_id] || attempt.project_id}</Link>
                    {projectNames[attempt.project_id] && <small><code>{attempt.project_id}</code></small>}
                  </td>
                  {/* The alias is what the caller asked for; it is the same on
                      every attempt of a fallback chain, which is why it cannot
                      be the only thing this row identifies the target by. */}
                  <td>
                    {attempt.requested_model || "—"}
                    <small>{attempt.provider_model}</small>
                  </td>
                  {/* Which deployment actually served this attempt. Without it
                      two targets of one alias on the same upstream model — the
                      safest way to configure redundancy — are indistinguishable
                      here, and a fallback cannot be verified from the console at
                      all. The ID is shown as well as the name because the ID is
                      what the ledger and the usage partitions carry. */}
                  <td>
                    {attempt.deployment_id ? (
                      <>
                        <Link className="resource-link" href={`/admin/deployments?q=${encodeURIComponent(attempt.deployment_id)}`}>
                          {deploymentNames[attempt.deployment_id] || attempt.deployment_id}
                        </Link>
                        {deploymentNames[attempt.deployment_id] && <small><code>{attempt.deployment_id}</code></small>}
                      </>
                    ) : "—"}
                  </td>
                  <td>{attempt.tokens_estimated ? t("usage.estimated") : ""}{compactNumber(attempt.provider_input_tokens + attempt.provider_output_tokens)}<small>{t("usage.inputOutput", { input: compactNumber(attempt.provider_input_tokens), output: compactNumber(attempt.provider_output_tokens) })} · {attempt.tokens_estimated ? t("usage.conservative") : t("usage.reported")}</small></td>
                  <td><CostCell attempt={attempt} /></td>
                  <td>{attempt.latency_millis} ms</td>
                  <td><AttemptStatusCell attempt={attempt} /></td>
                  <td>{dateTime(attempt.completed_at, "dateTimeYear")}</td>
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
      )}
      </section>
      )}
    </>
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
  const advice = errorClassAdvice(t, attempt.error_class);
  const chain = attempt.retry_count > 0 || attempt.fallback_count > 0
    ? t("usage.attemptChain", { fallback: attempt.fallback_count + 1, retry: attempt.retry_count })
    : t("usage.attemptFirstTry");
  return (
    <>
      <span className="inline-status"><StatusDot ok={false} label={t("usage.error")} />{attemptFailureLabel(t, attempt)}</span>
      {/* The status is kept apart from the class rather than folded into one
          string: an operator taking a 429 to a provider's support desk quotes
          the number, and a class alone cannot be quoted. */}
      {attempt.http_status ? <small>{t("usage.httpStatus", { status: attempt.http_status })}</small> : null}
      <details className="failure-detail">
        <summary>{t("usage.attemptDetails")}</summary>
        <small>
          {advice && <>{advice}<br /></>}
          {chain}
          <ProviderIdentifiers failure={attempt} />
        </small>
      </details>
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

// The pricing evidence disclosure shows how a settled attempt's cost was
// reached: the price snapshot it billed against and the input/output/fixed
// components that summed to it.
function CostCell({ attempt }: { attempt: UsageAttempt }) {
  const { t } = useTranslation();
  return (
    <>
      <strong>{attempt.cost_micros_usd == null ? t("usage.unknownCost") : money(attempt.cost_micros_usd)}</strong>
      {!!attempt.tags?.length && <small>{attempt.tags.map((tag) => <span className="badge" key={tag}>{tag}</span>)}</small>}
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
    </>
  );
}
