import { lazy, Suspense, useState } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "../api";
import { ErrorState, Loading, PageHeader, StatusDot } from "../components";
import { FirstRunChecklist } from "./FirstRunChecklist";
import { compactNumber, money, useInstantFormatter } from "../format";
import { adoptTimeContext } from "../timezone";
import type { GovernancePressureItem, UsageAnomaly, UsageBreakdown } from "../types";
import type { TrendMetric } from "../trend";
import { useTranslation } from "react-i18next";

const TrendChart = lazy(() => import("../TrendChart"));

type BreakdownDimension = "project" | "provider" | "requested_model" | "provider_model";

export function DashboardPage() {
  const { t } = useTranslation();
  const formatInstant = useInstantFormatter();
  const [trendMetric, setTrendMetric] = useState<TrendMetric>("requests");
  const [dimension, setDimension] = useState<BreakdownDimension>("project");
  const query = useQuery({
    queryKey: ["dashboard"],
    queryFn: api.dashboard,
    refetchInterval: 15_000,
  });
  if (query.isPending) return <Loading />;
  if (query.isError) return <ErrorState error={query.error} />;
  const dashboard = query.data;
  // Adopted here as well as at startup so a change to the server's accounting
  // zone reaches the page that is most sensitive to it without a reload.
  adoptTimeContext(dashboard.time_context);
  const today = dashboard.usage.today;
  const estimatedInputTokens = today.estimated_input_tokens ?? 0;
  const estimatedOutputTokens = today.estimated_output_tokens ?? 0;
  const reportedInputTokens = Math.max(0, today.input_tokens - estimatedInputTokens);
  const reportedOutputTokens = Math.max(0, today.output_tokens - estimatedOutputTokens);
  const reportedTokens = reportedInputTokens + reportedOutputTokens;
  const estimatedTokens = estimatedInputTokens + estimatedOutputTokens;
  const estimatedCost = today.estimated_cost_micros_usd ?? 0;
  const errorRate = today.attempts ? today.errors / today.attempts : 0;
  const averageLatency = today.attempts ? today.latency_millis / today.attempts : 0;
  const accountingHealthy = dashboard.accounting_status === 0;
  const breakdown = dashboard.usage.breakdowns?.[dimension] ?? [];
  const recentAnomalies = dashboard.usage.recent_anomalies ?? [];
  const hourly = dashboard.usage.hourly ?? [];
  const budgetItems = dashboard.governance.budget.items ?? [];
  const capacityItems = dashboard.governance.capacity.items ?? [];
  const resourceLabels = dashboard.resource_labels ?? {};
  const rejectionTotal = dashboard.governance.policy_rejections.total;
  const alertIssues = dashboard.alerts.Failed + dashboard.alerts.Dropped;
  const alertHealthy = alertIssues === 0;
  const alertStatus = alertHealthy && dashboard.alerts.Queued === 0
    ? t("dashboard.ready")
    : t("dashboard.alertStatus", { queued: compactNumber(dashboard.alerts.Queued), issues: compactNumber(alertIssues) });
  const walAtCapacity = dashboard.wal.QueueCapacity > 0 && dashboard.wal.QueueDepth >= dashboard.wal.QueueCapacity;
  const walHealthy = dashboard.wal.Errors === 0 && !walAtCapacity;
  const walStatus = walHealthy && dashboard.wal.QueueDepth === 0
    ? t("dashboard.ready")
    : t("dashboard.walStatus", { queued: compactNumber(dashboard.wal.QueueDepth), errors: compactNumber(dashboard.wal.Errors) });
  return (
    <>
      <PageHeader
        eyebrow={t("dashboard.eyebrow")}
        title={t("dashboard.title")}
        description={t("dashboard.description")}
        action={
          <div className={`health-pill ${accountingHealthy ? "" : "warning"}`}>
            <StatusDot ok={accountingHealthy} />
            {accountingHealthy ? t("dashboard.ledgerHealthy") : t("dashboard.ledgerWarning")}
          </div>
        }
      />
      {/* Nothing has ever been served, so the panels below are all zeroes and "no data".
          The chain that produces the first request is more useful than an empty chart. */}
      {dashboard.usage.watermark_sequence === 0 && today.attempts === 0 && <FirstRunChecklist />}
      {/* A boundary about to move belongs where people actually look. The
          settings page shows it too, but nobody visits settings to read
          today's numbers. */}
      {dashboard.time_context.pending_timezone && dashboard.time_context.pending_effective_at && (
        <div className="notice warning" role="status">
          <strong>{t("dashboard.pendingTimezoneTitle", { timezone: dashboard.time_context.pending_timezone })}</strong>
          <span>{t("dashboard.pendingTimezoneDetail", {
            at: formatInstant(dashboard.time_context.pending_effective_at, "full"),
          })}</span>
        </div>
      )}
      {/* Which day these figures cover. Without it the numbers are ambiguous
          to anyone whose own day starts at a different hour than the server's. */}
      <p className="metric-grid-scope" title={t("dashboard.accountingTimezoneHint")}>
        <span>{t("dashboard.accountingTimezone", { timezone: dashboard.time_context.accounting_timezone })}</span>
        <small>{t("dashboard.accountingPeriod", {
          start: dashboard.time_context.period_start,
          end: dashboard.time_context.period_end,
        })}</small>
      </p>
      <section className="metric-grid" aria-label={t("dashboard.todayMetrics")}>
        <Metric label={t("dashboard.requests")} value={compactNumber(today.requests)} detail={t("dashboard.attempts", { count: today.attempts })} />
        <Metric
          label={t("dashboard.tokens")}
          value={compactNumber(reportedTokens)}
          detail={`${t("dashboard.tokenDetail", { input: compactNumber(reportedInputTokens), output: compactNumber(reportedOutputTokens) })}${estimatedTokens > 0 ? t("dashboard.estimatedExcluded", { count: compactNumber(estimatedTokens) }) : ""}`}
        />
        <Metric label={t("dashboard.cost")} value={money(today.cost_micros_usd)} detail={estimatedCost > 0 ? t("dashboard.estimatedCost", { cost: money(estimatedCost) }) : t("dashboard.usageDetail")} />
        <Metric label={t("dashboard.attemptErrorRate")} value={`${(errorRate * 100).toFixed(1)}%`} detail={t("dashboard.failedAttempts", { count: today.errors })} alert={errorRate > 0.1} />
        <Metric label={t("dashboard.active")} value={String(dashboard.usage.active_requests)} detail={t("dashboard.inFlight")} />
        <Metric label={t("dashboard.averageLatency")} value={`${Math.round(averageLatency)} ms`} detail={t("dashboard.providerAttempts")} />
      </section>

      <section className="governance-strip" aria-label={t("dashboard.governancePressure")}>
        <div className="governance-strip-title">
          <p className="eyebrow">{t("dashboard.governanceSignal")}</p>
          <strong>{t("dashboard.governancePressure")}</strong>
        </div>
        <GovernanceSignal label={t("dashboard.policyRejections")} value={compactNumber(rejectionTotal)} detail={t("dashboard.sinceStartup")} alert={rejectionTotal > 0} />
        <GovernanceSignal label={t("dashboard.budgetRisk")} value={String(dashboard.governance.budget.at_risk)} detail={t("dashboard.projectsAtRisk")} alert={dashboard.governance.budget.at_risk > 0} />
        <GovernanceSignal label={t("dashboard.recentAnomalies")} value={String(recentAnomalies.length)} detail={t("dashboard.todayEvents")} alert={recentAnomalies.length > 0} />
        <GovernanceSignal label={t("dashboard.capacityRisk")} value={String(dashboard.governance.capacity.at_risk)} detail={t("dashboard.resourcesAtRisk")} alert={dashboard.governance.capacity.at_risk > 0} />
		<GovernanceSignal label={t("dashboard.pricingQuarantine")} value={String(dashboard.governance.pricing?.quarantined ?? 0)} detail={t("dashboard.pricingQuarantineDetail")} alert={(dashboard.governance.pricing?.quarantined ?? 0) > 0} />
		<GovernanceSignal label={t("dashboard.unknownPricing")} value={String(dashboard.governance.pricing?.unknown ?? 0)} detail={t("dashboard.unknownPricingDetail")} alert={(dashboard.governance.pricing?.unknown ?? 0) > 0} />
		<GovernanceSignal label={t("dashboard.adjustmentOverBudget")} value={String(dashboard.governance.pricing?.adjustment_over_budget ?? 0)} detail={t("dashboard.adjustmentOverBudgetDetail")} alert={(dashboard.governance.pricing?.adjustment_over_budget ?? 0) > 0} />
      </section>

      <section className="dashboard-grid governance-main-grid">
        <article className="panel chart-panel">
          <header className="panel-header dashboard-panel-header">
            <div><p className="eyebrow">{t("dashboard.trendEyebrow")}</p><h2>{t(`dashboard.trendMetrics.${trendMetric}`)}</h2></div>
            <DashboardTabs
              label={t("dashboard.trendMetric")}
              value={trendMetric}
              items={(["requests", "tokens", "cost", "errors"] as const).map((key) => ({ key, label: t(`dashboard.trendMetrics.${key}`) }))}
              onChange={setTrendMetric}
            />
          </header>
          <Suspense fallback={<Loading label={t("dashboard.loadingTrend")} />}>
            <TrendChart buckets={hourly} metric={trendMetric} />
          </Suspense>
          <div className="chart-caption">{t(`dashboard.trendDescriptions.${trendMetric}`)}</div>
        </article>

        <article className="panel governance-execution-panel">
          <header className="panel-header">
            <div><p className="eyebrow">{t("dashboard.controlPlane")}</p><h2>{t("dashboard.governanceExecution")}</h2></div>
          </header>
          <PressureSection title={t("dashboard.budgetWatermark")} empty={t("dashboard.noLimitedProjects")} item={budgetItems[0]} moneyValues />
          <PressureSection title={t("dashboard.capacityWatermark")} empty={t("dashboard.noLimitedResources")} item={capacityItems[0]} />
          <div className="rejection-summary">
            <div className="governance-row-heading"><span>{t("dashboard.rejectionComposition")}</span><strong>{compactNumber(rejectionTotal)}</strong></div>
            <div className="rejection-tags">
              <span>RPM {compactNumber(dashboard.governance.policy_rejections.rpm)}</span>
              <span>TPM {compactNumber(dashboard.governance.policy_rejections.tpm)}</span>
              <span>{t("dashboard.budgetShort")} {compactNumber(dashboard.governance.policy_rejections.budget)}</span>
              <span>{t("dashboard.concurrencyShort")} {compactNumber(concurrencyRejections(dashboard.governance.policy_rejections))}</span>
              <span>Token Guard {compactNumber(dashboard.governance.policy_rejections.token_guard)}</span>
            </div>
            <small>{t("dashboard.rejectionsSinceStartup")}</small>
          </div>
        </article>
      </section>

      <section className="governance-lower-grid">
        <article className="panel attribution-panel">
          <header className="panel-header dashboard-panel-header">
            <div><p className="eyebrow">{t("dashboard.usageAttribution")}</p><h2>{t("dashboard.topConsumers")}</h2></div>
            <DashboardTabs
              label={t("dashboard.breakdownDimension")}
              value={dimension}
              items={(["project", "provider", "requested_model", "provider_model"] as const).map((key) => ({ key, label: t(`dashboard.dimensions.${key}`) }))}
              onChange={setDimension}
            />
          </header>
          <BreakdownList items={breakdown} labels={resourceLabels} empty={t("dashboard.noUsageToday")} />
          <div className="panel-footnote">{t("dashboard.breakdownFootnote")}</div>
        </article>

        <article className="panel anomaly-panel">
          <header className="panel-header">
            <div><p className="eyebrow">{t("dashboard.anomalyChannel")}</p><h2>{t("dashboard.anomaliesAndGovernance")}</h2></div>
          </header>
          <AnomalyList items={recentAnomalies} labels={resourceLabels} empty={t("dashboard.noAnomaliesToday")} />
        </article>
      </section>

      <section className="panel internal-status-panel" aria-label={t("dashboard.internalStatus")}>
        <div className="internal-status-title"><p className="eyebrow">{t("dashboard.channels")}</p><h2>{t("dashboard.internalStatus")}</h2></div>
        <StatusRow label={t("dashboard.accountingLedger")} ok={accountingHealthy} value={accountingHealthy ? t("dashboard.healthy") : t("dashboard.degraded")} />
        <StatusRow label={t("dashboard.usageWatermark")} ok value={`#${dashboard.usage.watermark_sequence}`} />
        <StatusRow label={t("dashboard.alertDelivery")} ok={alertHealthy} value={alertStatus} />
        <StatusRow label={t("dashboard.walQueue")} ok={walHealthy} value={walStatus} />
        <div className="panel-footnote">{t("dashboard.autoRefresh")}</div>
      </section>
    </>
  );
}

function Metric({ label, value, detail, alert = false }: { label: string; value: string; detail: string; alert?: boolean }) {
  return <article className={`metric ${alert ? "alert" : ""}`}><span>{label}</span><strong>{value}</strong><small>{detail}</small></article>;
}

function GovernanceSignal({ label, value, detail, alert }: { label: string; value: string; detail: string; alert: boolean }) {
  return <div className={`governance-signal ${alert ? "warning" : ""}`}><span>{label}</span><strong>{value}</strong><small>{detail}</small></div>;
}

function DashboardTabs<T extends string>({ label, value, items, onChange }: { label: string; value: T; items: { key: T; label: string }[]; onChange: (value: T) => void }) {
  return <div className="dashboard-tabs" role="tablist" aria-label={label}>{items.map((item) => <button type="button" role="tab" aria-selected={value === item.key} key={item.key} onClick={() => onChange(item.key)}>{item.label}</button>)}</div>;
}

function PressureSection({ title, empty, item, moneyValues = false }: { title: string; empty: string; item?: GovernancePressureItem; moneyValues?: boolean }) {
  const { t } = useTranslation();
  if (!item) return <div className="pressure-section"><div className="governance-row-heading"><span>{title}</span><strong>—</strong></div><small>{empty}</small></div>;
  const utilization = Math.max(0, item.utilization * 100);
  return <div className="pressure-section">
    <div className="governance-row-heading"><span>{title}</span><strong>{Math.round(utilization)}%</strong></div>
    <div className="pressure-context"><span>{item.name || item.id}</span><span>{moneyValues ? `${money(item.current)} / ${money(item.limit)}` : `${compactNumber(item.current)} / ${compactNumber(item.limit)}`}</span></div>
    <div className={`pressure-bar ${utilization >= 80 ? "warning" : ""}`}><i style={{ width: `${Math.min(100, utilization)}%` }} /></div>
    <small>{moneyValues ? t("dashboard.committedAndReserved") : t(`dashboard.capacityScopes.${item.scope}`)}</small>
  </div>;
}

function BreakdownList({ items, labels, empty }: { items: UsageBreakdown[]; labels: Record<string, string>; empty: string }) {
  const { t } = useTranslation();
  if (!items.length) return <div className="dashboard-empty">{empty}</div>;
  const maxCalls = Math.max(...items.map((item) => item.calls), 1);
  return <div className="breakdown-list">
    <div className="breakdown-heading"><span>{t("dashboard.consumer")}</span><span>{t("dashboard.calls")}</span><span>{t("dashboard.cost")}</span><span>{t("dashboard.errorRate")}</span></div>
    {items.map((item) => <div className="breakdown-row" key={item.key}>
      <div className="breakdown-identity"><strong title={item.key}>{labels[item.key] || item.key}</strong><div><i style={{ width: `${item.calls / maxCalls * 100}%` }} /></div></div>
      <span>{compactNumber(item.calls)}</span><span>{money(item.cost_micros_usd)}</span><span>{item.calls ? `${(item.errors / item.calls * 100).toFixed(1)}%` : "0.0%"}</span>
    </div>)}
  </div>;
}

function AnomalyList({ items, labels, empty }: { items: UsageAnomaly[]; labels: Record<string, string>; empty: string }) {
  const { t } = useTranslation();
  const dateTime = useInstantFormatter();
  if (!items.length) return <div className="dashboard-empty">{empty}</div>;
  return <div className="anomaly-list">{items.map((item, index) => {
    const title = item.status !== "success" ? item.error_class || `${t("dashboard.httpError")} ${item.http_status || "—"}` : item.fallback_count > 0 ? t("dashboard.routeFallback") : t("dashboard.requestRetry");
    const context = [labels[item.project_id] || item.project_id, labels[item.provider_id || ""] || item.provider_id, item.provider_model || item.requested_model].filter(Boolean).join(" · ");
    return <div className="anomaly-row" key={`${item.completed_at}-${index}`}><StatusDot ok={false} /><div><strong>{title}</strong><small>{context || t("dashboard.unknownContext")}</small></div><time>{dateTime(item.completed_at)}</time></div>;
  })}</div>;
}

function StatusRow({ label, ok, value }: { label: string; ok: boolean; value: string }) {
  return <div className="status-row"><span><StatusDot ok={ok} />{label}</span><strong>{value}</strong></div>;
}

function concurrencyRejections(rejections: { project_concurrency: number; provider_concurrency: number; deployment_concurrency: number }) {
  return rejections.project_concurrency + rejections.provider_concurrency + rejections.deployment_concurrency;
}
