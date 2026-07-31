import { lazy, Suspense } from "react";
import { useQuery } from "@tanstack/react-query";
import { api } from "../api";
import { ErrorState, Loading, PageHeader, StatusDot } from "../components";
import { compactNumber, money } from "../format";

const TrendChart = lazy(() => import("../TrendChart"));

export function DashboardPage() {
  const query = useQuery({
    queryKey: ["dashboard"],
    queryFn: api.dashboard,
    refetchInterval: 15_000,
  });
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
  const errorRate = today.attempts ? today.errors / today.attempts : 0;
  const averageLatency = today.attempts ? today.latency_millis / today.attempts : 0;
  const accountingHealthy = dashboard.accounting_status === 0;
  return (
    <>
      <PageHeader
        eyebrow="LIVE GATEWAY PULSE"
        title="全局态势"
        description="今天的模型流量、成本与运行健康度。数据来自本机 durable ledger。"
        action={
          <div className={`health-pill ${accountingHealthy ? "" : "warning"}`}>
            <StatusDot ok={accountingHealthy} />
            {accountingHealthy ? "账本健康" : "账本需要关注"}
          </div>
        }
      />
      <section className="metric-grid" aria-label="今日关键指标">
        <Metric label="REQUESTS" value={compactNumber(today.requests)} detail={`${today.attempts} provider attempts`} />
        <Metric
          label="TOKENS"
          value={compactNumber(reportedTokens)}
          detail={`${compactNumber(reportedInputTokens)} in / ${compactNumber(reportedOutputTokens)} out${estimatedTokens > 0 ? ` · ${compactNumber(estimatedTokens)} estimated excluded` : ""}`}
        />
        <Metric label="COST" value={money(today.cost_micros_usd)} detail="estimated + reported usage" />
        <Metric label="ERROR RATE" value={`${(errorRate * 100).toFixed(1)}%`} detail={`${today.errors} failed attempts`} alert={errorRate > 0.1} />
        <Metric label="ACTIVE" value={String(dashboard.usage.active_requests)} detail="requests in flight" />
        <Metric label="AVG LATENCY" value={`${Math.round(averageLatency)} ms`} detail="provider attempts" />
      </section>
      <section className="dashboard-grid">
        <article className="panel chart-panel">
          <header className="panel-header">
            <div><p className="eyebrow">7 DAY SIGNAL</p><h2>请求与 Token 趋势</h2></div>
            <span className="legend"><i /> Requests <i /> Tokens</span>
          </header>
          <Suspense fallback={<Loading label="正在加载趋势图" />}>
            <TrendChart buckets={dashboard.usage.hourly} />
          </Suspense>
        </article>
        <article className="panel health-panel">
          <header className="panel-header">
            <div><p className="eyebrow">SYSTEM CHANNELS</p><h2>内部状态</h2></div>
          </header>
          <StatusRow label="Accounting ledger" ok={accountingHealthy} value={accountingHealthy ? "HEALTHY" : "DEGRADED"} />
          <StatusRow label="Usage watermark" ok value={`#${dashboard.usage.watermark_sequence}`} />
          <StatusRow label="Alert delivery" ok value={statValue(dashboard.alerts)} />
          <StatusRow label="WAL queue" ok value={statValue(dashboard.wal)} />
          <div className="panel-footnote">自动刷新 · 15 秒</div>
        </article>
      </section>
    </>
  );
}

function Metric({
  label,
  value,
  detail,
  alert = false,
}: {
  label: string;
  value: string;
  detail: string;
  alert?: boolean;
}) {
  return (
    <article className={`metric ${alert ? "alert" : ""}`}>
      <span>{label}</span>
      <strong>{value}</strong>
      <small>{detail}</small>
    </article>
  );
}

function StatusRow({ label, ok, value }: { label: string; ok: boolean; value: string }) {
  return (
    <div className="status-row">
      <span><StatusDot ok={ok} />{label}</span>
      <strong>{value}</strong>
    </div>
  );
}

function statValue(stats: Record<string, number>) {
  const value = Object.values(stats).find((entry) => typeof entry === "number");
  return value === undefined ? "READY" : compactNumber(value);
}
