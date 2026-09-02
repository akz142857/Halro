import { lazy, Suspense, useEffect, useRef, useState, type KeyboardEvent } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "../api";
import { ErrorState, Loading, Metric, PageHeader, SegmentedTabs, StatusDot } from "../components";
import { FirstRunChecklist } from "./FirstRunChecklist";
import { compactNumber, money, useInstantFormatter } from "../format";
import { navigate } from "../navigation";
import { adoptTimeContext } from "../timezone";
import { errorClassLabel } from "../failure";
import type { GovernancePressureItem, UsageAnomaly, UsageBreakdown } from "../types";
import { hourlyTrendPoints, type TrendMetric } from "../trend";
import { useTranslation } from "react-i18next";

const TrendChart = lazy(() => import("../TrendChart"));

type BreakdownDimension = "project" | "provider" | "requested_model" | "provider_model";
type BreakdownMetric = "calls" | "cost" | "errors";

interface AttentionItem {
  key: string;
  title: string;
  detail: string;
  href?: string;
  danger?: boolean;
}

export function DashboardPage() {
  const { t } = useTranslation();
  const [trendMetric, setTrendMetric] = useState<TrendMetric>("requests");
  const [dimension, setDimension] = useState<BreakdownDimension>("project");
  const [breakdownMetric, setBreakdownMetric] = useState<BreakdownMetric>("calls");
  const query = useQuery({ queryKey: ["dashboard"], queryFn: api.dashboard, refetchInterval: 15_000 });
  const dashboardTimeContext = query.data?.time_context;
  useEffect(() => {
    if (dashboardTimeContext) adoptTimeContext(dashboardTimeContext);
  }, [dashboardTimeContext]);
  if (query.isPending) return <Loading />;
  if (query.isError) return <ErrorState error={query.error} />;

  const dashboard = query.data;
  const today = dashboard.usage.today;
  const estimatedInputTokens = today.estimated_input_tokens ?? 0;
  const estimatedOutputTokens = today.estimated_output_tokens ?? 0;
  const reportedInputTokens = Math.max(0, today.input_tokens - estimatedInputTokens);
  const reportedOutputTokens = Math.max(0, today.output_tokens - estimatedOutputTokens);
  const reportedTokens = reportedInputTokens + reportedOutputTokens;
  const estimatedTokens = estimatedInputTokens + estimatedOutputTokens;
  const estimatedCost = today.estimated_cost_micros_usd ?? 0;
  const requestSuccessRate = today.requests ? (today.requests - today.request_errors) / today.requests : 1;
  const accountingHealthy = dashboard.accounting_status === 0;
  const acceptingTraffic = dashboard.runtime?.accepting_traffic ?? accountingHealthy;
  const breakdown = dashboard.usage.breakdowns?.[dimension]?.[breakdownMetric] ?? [];
  const recentAnomalies = dashboard.usage.recent_anomalies ?? [];
  const hourly = dashboard.usage.hourly ?? [];
  const budgetItems = dashboard.governance.budget.items ?? [];
  const capacityItems = dashboard.governance.capacity.items ?? [];
  const resourceLabels = dashboard.resource_labels ?? {};
  const rejectionTotal = dashboard.governance.policy_rejections.total;
  const alertEndpoints = dashboard.alerts.Endpoints ?? 0;
  const unhealthyAlertEndpoints = dashboard.alerts.UnhealthyEndpoints ?? 0;
  const unknownAlertEndpoints = dashboard.alerts.UnknownEndpoints ?? 0;
  const alertQueueCapacity = dashboard.alerts.QueueCapacity ?? 0;
  const alertAtCapacity = alertQueueCapacity > 0 && dashboard.alerts.Queued >= alertQueueCapacity;
  const alertNeedsAttention = unhealthyAlertEndpoints > 0 || unknownAlertEndpoints > 0 || alertAtCapacity;
  const alertVerifiedHealthy = alertEndpoints > 0 && !alertNeedsAttention && dashboard.alerts.Queued === 0;
  const alertStatus = alertEndpoints === 0
    ? t("dashboard.alertNotConfigured")
    : unknownAlertEndpoints > 0
      ? t("dashboard.alertUnverified", { count: unknownAlertEndpoints })
      : alertVerifiedHealthy
      ? t("dashboard.ready")
      : t("dashboard.alertCurrentStatus", { queued: compactNumber(dashboard.alerts.Queued), unhealthy: unhealthyAlertEndpoints });
  const walAtCapacity = dashboard.wal.queue_capacity > 0 && dashboard.wal.queue_depth >= dashboard.wal.queue_capacity;
  const walHealthy = dashboard.wal.errors === 0 && !walAtCapacity;
  const durablePathHealthy = accountingHealthy && walHealthy;
  const durablePathStatus = durablePathHealthy && dashboard.wal.queue_depth === 0
    ? t("dashboard.ready")
    : t("dashboard.walStatus", { queued: compactNumber(dashboard.wal.queue_depth), errors: compactNumber(dashboard.wal.errors) });
  const firstValueReached = dashboard.first_value_reached ?? false;
  const attention = buildAttention({
    acceptingTraffic, accountingHealthy, unknownAttempts: today.unknown_attempts ?? 0,
    budgetItems, capacityItems, pricingQuarantined: dashboard.governance.pricing?.quarantined ?? 0,
    pricingUnknown: dashboard.governance.pricing?.unknown ?? 0, alertHealthy: !alertNeedsAttention, walHealthy, recentAnomalies, t,
  });

  return (
    <>
      <PageHeader
        eyebrow={t(firstValueReached ? "dashboard.eyebrow" : "dashboard.firstRun.pageEyebrow")}
        title={t(firstValueReached ? "dashboard.title" : "dashboard.firstRun.pageTitle")}
        description={t(firstValueReached ? "dashboard.description" : "dashboard.firstRun.pageDescription")}
        action={<div className={`health-pill ${acceptingTraffic ? "" : "warning"}`}><StatusDot ok={acceptingTraffic} />{acceptingTraffic ? t("dashboard.acceptingTraffic") : t("dashboard.notAcceptingTraffic")}</div>}
      />

      {!firstValueReached && <FirstRunChecklist />}
      {firstValueReached && (
        <>
          <section className="metric-grid" aria-label={t("dashboard.todayMetrics")}>
            <Metric label={t("dashboard.completedRequests")} value={compactNumber(today.requests)} detail={t("dashboard.attempts", { count: today.attempts })} />
            <Metric label={t("dashboard.requestSuccessRate")} value={`${(requestSuccessRate * 100).toFixed(1)}%`} detail={t("dashboard.failedRequests", { count: today.request_errors })} alert={today.request_errors > 0} />
            <Metric label={t("dashboard.requestP95")} value={today.request_latency_samples ? `${today.request_latency_p95_millis} ms` : "—"} detail={today.request_latency_samples ? t("dashboard.finalRequestLatency") : t("dashboard.noLatencySamples")} />
            <Metric label={t("dashboard.reportedTokens")} value={compactNumber(reportedTokens)} detail={estimatedTokens > 0 ? t("dashboard.estimatedTokensSeparate", { count: compactNumber(estimatedTokens) }) : t("dashboard.tokenDetail", { input: compactNumber(reportedInputTokens), output: compactNumber(reportedOutputTokens) })} />
            <Metric label={t("dashboard.knownCost")} value={money(today.cost_micros_usd)} detail={costDetail(t, estimatedCost, today.unknown_attempts ?? 0)} alert={(today.unknown_attempts ?? 0) > 0} />
          </section>

          <AttentionPanel items={attention} />

          <section className="dashboard-grid governance-main-grid">
            <article className="panel chart-panel">
              <header className="panel-header dashboard-panel-header">
                <div><p className="eyebrow">{t("dashboard.trendEyebrow")}</p><h2>{t(`dashboard.trendMetrics.${trendMetric}`)}</h2></div>
                <SegmentedTabs
                  id="dashboard-trend"
                  label={t("dashboard.trendMetric")}
                  value={trendMetric}
                  items={( ["requests", "success_rate", "latency_p95", "tokens", "cost"] as const).map((key) => ({ key, label: t(`dashboard.trendMetrics.${key}`) }))}
                  onChange={setTrendMetric}
                />
              </header>
              <div id="dashboard-trend-panel" role="tabpanel" aria-labelledby={`dashboard-trend-tab-${trendMetric}`}>
                <Suspense fallback={<Loading label={t("dashboard.loadingTrend")} />}><TrendChart points={hourlyTrendPoints(hourly)} metric={trendMetric} /></Suspense>
                <div className="chart-caption">{t(`dashboard.trendDescriptions.${trendMetric}`)}</div>
              </div>
            </article>

            <article className="panel governance-execution-panel">
              <header className="panel-header"><div><p className="eyebrow">{t("dashboard.controlPlane")}</p><h2>{t("dashboard.governanceExecution")}</h2></div></header>
              <PressureSection title={t("dashboard.budgetWatermark")} empty={t("dashboard.noLimitedProjects")} item={budgetItems[0]} moneyValues />
              <PressureSection title={t("dashboard.capacityWatermark")} empty={t("dashboard.noLimitedResources")} item={capacityItems[0]} />
              <div className="rejection-summary">
                <div className="governance-row-heading"><span>{t("dashboard.rejectionComposition")}</span><strong>{compactNumber(rejectionTotal)}</strong></div>
                <div className="rejection-tags">
                  <span>RPM {compactNumber(dashboard.governance.policy_rejections.rpm)}</span><span>TPM {compactNumber(dashboard.governance.policy_rejections.tpm)}</span>
                  <span>{t("dashboard.budgetShort")} {compactNumber(dashboard.governance.policy_rejections.budget)}</span>
                  <span>{t("dashboard.concurrencyShort")} {compactNumber(concurrencyRejections(dashboard.governance.policy_rejections))}</span>
                  <span>Token Guard {compactNumber(dashboard.governance.policy_rejections.token_guard)}</span>
                  {/* The one refusal here that is a configuration answer rather
                      than a pressure reading: the route is up and the key is
                      good, and no deployment behind the public model declares
                      what the request asked for. */}
                  <span>{t("dashboard.routeCapabilityShort")} {compactNumber(dashboard.governance.policy_rejections.route_capability)}</span>
                </div>
                <small>{t("dashboard.rejectionsSinceStartup")}</small>
              </div>
            </article>
          </section>

          <section className="governance-lower-grid">
            <article className="panel attribution-panel">
              <header className="panel-header dashboard-panel-header">
                <div><p className="eyebrow">{t("dashboard.usageAttribution")}</p><h2>{t("dashboard.topConsumers")}</h2></div>
                <div className="breakdown-controls">
                  <label><span className="sr-only">{t("dashboard.breakdownSort")}</span><select value={breakdownMetric} onChange={(event) => setBreakdownMetric(event.target.value as BreakdownMetric)}><option value="calls">{t("dashboard.calls")}</option><option value="cost">{t("dashboard.cost")}</option><option value="errors">{t("dashboard.errors")}</option></select></label>
                  <SegmentedTabs
                    id="dashboard-breakdown"
                    label={t("dashboard.breakdownDimension")}
                    value={dimension}
                    items={( ["project", "provider", "requested_model", "provider_model"] as const).map((key) => ({ key, label: t(`dashboard.dimensions.${key}`) }))}
                    onChange={setDimension}
                  />
                </div>
              </header>
              <div id="dashboard-breakdown-panel" role="tabpanel" aria-labelledby={`dashboard-breakdown-tab-${dimension}`}>
                <BreakdownList items={breakdown} labels={resourceLabels} dimension={dimension} metric={breakdownMetric} empty={t("dashboard.noUsageToday")} />
                <div className="panel-footnote">{t("dashboard.breakdownFootnote")}</div>
              </div>
            </article>

            <article className="panel anomaly-panel">
              <header className="panel-header"><div><p className="eyebrow">{t("dashboard.anomalyChannel")}</p><h2>{t("dashboard.recentRequestAnomalies")}</h2></div></header>
              <AnomalyList items={recentAnomalies} labels={resourceLabels} empty={t("dashboard.noAnomaliesToday")} />
            </article>
          </section>

          <section className="panel internal-status-panel" aria-label={t("dashboard.internalStatus")}>
            <div className="internal-status-title"><p className="eyebrow">{t("dashboard.channels")}</p><h2>{t("dashboard.internalStatus")}</h2></div>
            <StatusRow label={t("dashboard.trafficAcceptance")} ok={acceptingTraffic} value={acceptingTraffic ? t("dashboard.accepting") : t("dashboard.notAccepting")} />
            <StatusRow label={t("dashboard.activeRequests")} ok={dashboard.usage.active_requests === 0 ? undefined : true} value={compactNumber(dashboard.usage.active_requests)} />
            <StatusRow label={t("dashboard.alertDelivery")} ok={alertEndpoints === 0 || unknownAlertEndpoints > 0 || dashboard.alerts.Queued > 0 && !alertNeedsAttention ? undefined : !alertNeedsAttention} value={alertStatus} />
            <StatusRow label={t("dashboard.durableAccounting")} ok={durablePathHealthy} value={durablePathStatus} />
            <div className="panel-footnote">{t("dashboard.autoRefresh")}</div>
          </section>
        </>
      )}
    </>
  );
}

function AttentionPanel({ items }: { items: AttentionItem[] }) {
  const { t } = useTranslation();
  return (
    <section className={`panel attention-panel ${items.length ? "has-issues" : ""}`} aria-label={t("dashboard.attentionTitle")}>
      <header><div><p className="eyebrow">{t("dashboard.operatorQueue")}</p><h2>{t("dashboard.attentionTitle")}</h2></div><span className="attention-count">{items.length ? t("dashboard.issueCount", { count: items.length }) : t("dashboard.noAttentionNeeded")}</span></header>
      {items.length > 0 && <div className="attention-list">{items.map((item) => (
        <button type="button" key={item.key} className={item.danger ? "danger" : ""} onClick={() => item.href && navigate(item.href)} disabled={!item.href}>
          <StatusDot ok={false} /><span><strong>{item.title}</strong><small>{item.detail}</small></span>{item.href && <i aria-hidden="true">→</i>}
        </button>
      ))}</div>}
    </section>
  );
}

function PressureSection({ title, empty, item, moneyValues = false }: { title: string; empty: string; item?: GovernancePressureItem; moneyValues?: boolean }) {
  const { t } = useTranslation();
  if (!item) return <div className="pressure-section"><div className="governance-row-heading"><span>{title}</span><strong>—</strong></div><small>{empty}</small></div>;
  const utilization = Math.max(0, item.utilization * 100);
  const href = pressureHref(item);
  return <div className="pressure-section">
    <div className="governance-row-heading"><span>{title}</span><strong>{Math.round(utilization)}%</strong></div>
    <div className="pressure-context"><button type="button" className="resource-link inline" onClick={() => navigate(href)}>{item.name || item.id}</button><span>{moneyValues ? `${money(item.current)} / ${money(item.limit)}` : `${compactNumber(item.current)} / ${compactNumber(item.limit)}`}</span></div>
    <div className={`pressure-bar ${utilization >= 80 ? "warning" : ""}`}><i style={{ width: `${Math.min(100, utilization)}%` }} /></div>
    <small>{moneyValues ? t("dashboard.committedAndReserved") : t(`dashboard.capacityScopes.${item.scope}`)}</small>
  </div>;
}

function BreakdownList({ items, labels, dimension, metric, empty }: { items: UsageBreakdown[]; labels: Record<string, string>; dimension: BreakdownDimension; metric: BreakdownMetric; empty: string }) {
  const { t } = useTranslation();
  if (!items.length) return <div className="dashboard-empty">{empty}</div>;
  const maximum = Math.max(...items.map((item) => breakdownValue(item, metric)), 1);
  return <div className="breakdown-list">
    <div className="breakdown-heading"><span>{t("dashboard.consumer")}</span><span>{t("dashboard.calls")}</span><span>{t("dashboard.cost")}</span><span>{t("dashboard.errorRate")}</span></div>
    {items.map((item) => <button type="button" className="breakdown-row" key={item.key} onClick={() => navigate(breakdownHref(dimension, item.key))}>
      <div className="breakdown-identity"><strong title={item.key}>{labels[item.key] || item.key}</strong><div><i style={{ width: `${breakdownValue(item, metric) / maximum * 100}%` }} /></div></div>
      <span>{compactNumber(item.calls)}</span><span>{money(item.cost_micros_usd)}</span><span>{item.calls ? `${(item.errors / item.calls * 100).toFixed(1)}%` : "0.0%"}</span>
    </button>)}
  </div>;
}

function AnomalyList({ items, labels, empty }: { items: UsageAnomaly[]; labels: Record<string, string>; empty: string }) {
  const { t } = useTranslation();
  const dateTime = useInstantFormatter();
  if (!items.length) return <div className="dashboard-empty">{empty}</div>;
  return <div className="anomaly-list">{items.map((item, index) => {
    // The class is named in the reader's language here for the same reason it
    // is in the attempt table: it is the same identifier, and a console that
    // translates it on one screen and not the other reads as two products.
    const title = item.status !== "success" ? errorClassLabel(t, item.error_class) || `${t("dashboard.httpError")} ${item.http_status || "—"}` : item.fallback_count > 0 ? t("dashboard.routeFallback") : t("dashboard.requestRetry");
    const context = [labels[item.project_id] || item.project_id, labels[item.provider_id || ""] || item.provider_id, item.provider_model || item.requested_model].filter(Boolean).join(" · ");
    return <button type="button" className="anomaly-row" key={item.request_id || `${item.completed_at}-${index}`} onClick={() => navigate(`/admin/usage?request_id=${encodeURIComponent(item.request_id)}`)}><StatusDot ok={false} /><span><strong>{title}</strong><small>{context || t("dashboard.unknownContext")}</small></span><time>{dateTime(item.completed_at)}</time></button>;
  })}</div>;
}

function StatusRow({ label, ok, value }: { label: string; ok?: boolean; value: string }) {
  return <div className="status-row"><span>{ok !== undefined && <StatusDot ok={ok} />}{label}</span><strong>{value}</strong></div>;
}

function costDetail(t: (key: string, options?: Record<string, unknown>) => string, estimated: number, unknown: number) {
  if (unknown > 0) return t("dashboard.unknownCostExcluded", { count: unknown, estimated: estimated > 0 ? money(estimated) : "" });
  return estimated > 0 ? t("dashboard.estimatedCost", { cost: money(estimated) }) : t("dashboard.allCostKnown");
}

function buildAttention(input: { acceptingTraffic: boolean; accountingHealthy: boolean; unknownAttempts: number; budgetItems: GovernancePressureItem[]; capacityItems: GovernancePressureItem[]; pricingQuarantined: number; pricingUnknown: number; alertHealthy: boolean; walHealthy: boolean; recentAnomalies: UsageAnomaly[]; t: (key: string, options?: Record<string, unknown>) => string }): AttentionItem[] {
  const items: AttentionItem[] = [];
  if (!input.acceptingTraffic) items.push({ key: "traffic", title: input.t("dashboard.attention.traffic.title"), detail: input.t("dashboard.attention.traffic.detail"), href: "/admin/settings/diagnostics", danger: true });
  if (!input.accountingHealthy) items.push({ key: "ledger", title: input.t("dashboard.attention.ledger.title"), detail: input.t("dashboard.attention.ledger.detail"), href: "/admin/settings/diagnostics", danger: true });
  if (!input.walHealthy) items.push({ key: "wal", title: input.t("dashboard.attention.wal.title"), detail: input.t("dashboard.attention.wal.detail"), href: "/admin/settings/diagnostics", danger: true });
  if (!input.alertHealthy) items.push({ key: "alerts", title: input.t("dashboard.attention.alerts.title"), detail: input.t("dashboard.attention.alerts.detail"), href: "/admin/operations" });
  if (input.unknownAttempts > 0) items.push({ key: "unknown-cost", title: input.t("dashboard.attention.unknownCost.title", { count: input.unknownAttempts }), detail: input.t("dashboard.attention.unknownCost.detail"), href: "/admin/usage" });
  if (input.budgetItems[0]?.utilization >= .8) items.push({ key: "budget", title: input.t("dashboard.attention.budget.title", { name: input.budgetItems[0].name || input.budgetItems[0].id }), detail: input.t("dashboard.attention.budget.detail", { value: Math.round(input.budgetItems[0].utilization * 100) }), href: pressureHref(input.budgetItems[0]) });
  if (input.capacityItems[0]?.utilization >= .8) items.push({ key: "capacity", title: input.t("dashboard.attention.capacity.title", { name: input.capacityItems[0].name || input.capacityItems[0].id }), detail: input.t("dashboard.attention.capacity.detail", { value: Math.round(input.capacityItems[0].utilization * 100) }), href: pressureHref(input.capacityItems[0]) });
  if (input.pricingQuarantined > 0 || input.pricingUnknown > 0) items.push({ key: "pricing", title: input.t("dashboard.attention.pricing.title"), detail: input.t("dashboard.attention.pricing.detail", { quarantined: input.pricingQuarantined, unknown: input.pricingUnknown }), href: "/admin/deployments" });
  if (input.recentAnomalies.length > 0) items.push({ key: "anomalies", title: input.t("dashboard.attention.anomalies.title", { count: input.recentAnomalies.length }), detail: input.t("dashboard.attention.anomalies.detail"), href: `/admin/usage?request_id=${encodeURIComponent(input.recentAnomalies[0].request_id)}` });
  return items.slice(0, 6);
}

function pressureHref(item: GovernancePressureItem) {
  if (item.scope === "project") return `/admin/usage?project_id=${encodeURIComponent(item.id)}`;
  if (item.scope === "provider") return `/admin/usage?provider_id=${encodeURIComponent(item.id)}`;
  return `/admin/deployments?q=${encodeURIComponent(item.id)}`;
}

function breakdownHref(dimension: BreakdownDimension, key: string) {
  const name = dimension === "requested_model" ? "model" : dimension === "project" ? "project_id" : dimension === "provider" ? "provider_id" : "provider_model";
  return `/admin/usage?${name}=${encodeURIComponent(key)}`;
}

function breakdownValue(item: UsageBreakdown, metric: BreakdownMetric) {
  if (metric === "cost") return item.cost_micros_usd;
  if (metric === "errors") return item.errors;
  return item.calls;
}

function concurrencyRejections(rejections: { project_concurrency: number; provider_concurrency: number; deployment_concurrency: number }) {
  return rejections.project_concurrency + rejections.provider_concurrency + rejections.deployment_concurrency;
}
