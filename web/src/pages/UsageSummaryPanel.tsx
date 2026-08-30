import { lazy, Suspense, useMemo, useState } from "react";
import { keepPreviousData, useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { EmptyState, ErrorState, Loading, Metric, SegmentedChoice, SegmentedTabs } from "../components";
import { compactNumber, money } from "../format";
import { Link } from "../navigation";
import { periodTrendPoints, type TrendMetric } from "../trend";
import type { SummaryGroup, SummaryMetrics, UsageSummary } from "../types";

const TrendChart = lazy(() => import("../TrendChart"));

type Granularity = "day" | "month" | "year";
type SortKey = "calls" | "success_rate" | "tokens" | "cost";

// Which measure each sortable column ranks by, and which direction reads first.
// Cost and volume are asked as "who is biggest"; a success rate is asked as
// "what is worst", so it opens ascending.
const sortableColumns: Array<{ key: SortKey; ascendingFirst: boolean }> = [
  { key: "calls", ascendingFirst: false },
  { key: "success_rate", ascendingFirst: true },
  { key: "tokens", ascendingFirst: false },
  { key: "cost", ascendingFirst: false },
];
type Dimension = "" | "project" | "requested_model" | "provider" | "deployment" | "provider_model";

// The attempt-detail filter each dimension drills into. A dimension with no
// filter of its own would send the operator to an unfiltered list, which is the
// same as sending them nowhere.
const detailFilters: Record<Exclude<Dimension, "">, string> = {
  project: "project_id",
  requested_model: "model",
  provider: "provider_id",
  deployment: "deployment_id",
  provider_model: "provider_model",
};

export function UsageSummaryPanel() {
  const { t } = useTranslation();
  const [granularity, setGranularity] = useState<Granularity>("month");
  const [dimension, setDimension] = useState<Dimension>("project");
  const [metric, setMetric] = useState<TrendMetric>("cost");
  const [sort, setSort] = useState<SortKey>("cost");
  const [ascending, setAscending] = useState(false);
  const [start, setStart] = useState("");
  const [end, setEnd] = useState("");

  const query = useMemo(() => {
    const params = new URLSearchParams({ granularity });
    if (dimension) {
      params.set("group_by", dimension);
      // The ranking goes to the server because the tail is folded there: a page
      // selected by cost and then sorted here by tokens would head a list whose
      // true leader is inside __other__.
      params.set("sort", sort);
      params.set("order", ascending ? "asc" : "desc");
    }
    if (start) params.set("start", start);
    if (end) params.set("end", end);
    return `?${params}`;
  }, [granularity, dimension, sort, ascending, start, end]);
  const summary = useQuery({
    queryKey: ["usage-summary", query],
    queryFn: () => api.usageSummary(query),
    // Changing the ranking or the granularity is a re-read of the same subject,
    // not a move to another one. Without this the whole panel — totals, chart
    // and table — is replaced by a spinner on every click, and the control the
    // operator just used disappears from under the pointer.
    placeholderData: keepPreviousData,
  });

  if (summary.isPending) return <Loading />;
  if (summary.isError) return <ErrorState error={summary.error} />;
  const report = summary.data;
  const totals = report.totals;
  const requests = totals.requests ?? 0;
  const successRate = requests ? (requests - (totals.request_errors ?? 0)) / requests : 1;
  const groupedBy = (report.group_by ?? "") as Dimension;
  const rankBy = (next: SortKey) => {
    if (next === sort) {
      setAscending(!ascending);
      return;
    }
    setSort(next);
    setAscending(sortableColumns.find((column) => column.key === next)?.ascendingFirst ?? false);
  };
  const estimatedTokens = totals.estimated_input_tokens + totals.estimated_output_tokens;
  const reportedTokens = Math.max(0, totals.input_tokens + totals.output_tokens - estimatedTokens);

  return (
    // Keeping the previous answer on screen means a refetch is otherwise
    // invisible; aria-busy says it is happening without flashing the panel.
    <section className="usage-summary" aria-label={t("usage.summary.title")} aria-busy={summary.isFetching}>
      {/* The three time controls sit together because they answer one question
          — which stretch of the ledger, at what resolution — and the grouping
          dimension, which changes the table rather than the window, follows
          them. */}
      <div className="filter-bar usage-summary-controls">
        <div className="filter-field">
          <span>{t("usage.summary.granularity")}</span>
          <SegmentedChoice
            label={t("usage.summary.granularity")}
            value={granularity}
            items={(["day", "month", "year"] as const).map((key) => ({ key, label: t(`usage.summary.granularities.${key}`) }))}
            onChange={setGranularity}
          />
        </div>
        <label><span>{t("usage.start")}</span><input type="date" value={start} max={end || undefined} onChange={(event) => setStart(event.target.value)} /></label>
        <label><span>{t("usage.end")}</span><input type="date" value={end} min={start || undefined} onChange={(event) => setEnd(event.target.value)} /></label>
        <label>
          <span>{t("usage.summary.dimension")}</span>
          <select value={dimension} onChange={(event) => setDimension(event.target.value as Dimension)}>
            <option value="">{t("usage.summary.dimensionNone")}</option>
            {(Object.keys(detailFilters) as Array<Exclude<Dimension, "">>).map((key) => (
              <option key={key} value={key}>{t(`usage.summary.dimensions.${key}`)}</option>
            ))}
          </select>
        </label>
        <button type="button" className="button" onClick={() => downloadSummaryCSV(report, t("usage.summary.csvName", { start: report.start, end: report.end }))}>
          {t("usage.summary.export")}
        </button>
      </div>

      {/* An accounting timezone change splits a label's meaning: the same local
          date under two generations covers two different intervals. Adding them
          silently would produce a month nobody can reconcile. */}
      {report.timezone_changes.length > 0 && (
        <div className="notice info" role="status">
          {t("usage.summary.timezoneChanged", {
            count: report.timezone_changes.length,
            period: report.timezone_changes[0].period_id,
          })}
        </div>
      )}

      <section className="metric-grid" aria-label={t("usage.summary.totals")}>
        <Metric
          label={t("dashboard.completedRequests")}
          value={compactNumber(requests)}
          detail={t("dashboard.attempts", { count: totals.attempts })}
        />
        <Metric
          label={t("dashboard.requestSuccessRate")}
          value={`${(successRate * 100).toFixed(1)}%`}
          detail={t("dashboard.failedRequests", { count: totals.request_errors ?? 0 })}
          alert={(totals.request_errors ?? 0) > 0}
        />
        <Metric
          label={t("dashboard.reportedTokens")}
          value={compactNumber(reportedTokens)}
          detail={estimatedTokens > 0
            ? t("dashboard.estimatedTokensSeparate", { count: compactNumber(estimatedTokens) })
            : t("dashboard.tokenDetail", { input: compactNumber(totals.input_tokens), output: compactNumber(totals.output_tokens) })}
        />
        <Metric
          label={t("dashboard.knownCost")}
          value={money(totals.cost_micros_usd)}
          detail={summaryCostDetail(t, totals)}
          alert={totals.unknown_attempts > 0}
        />
      </section>

      <article className="panel chart-panel">
        <header className="panel-header dashboard-panel-header">
          <div>
            <p className="eyebrow">{t("usage.summary.trendEyebrow", { start: report.start, end: report.end })}</p>
            <h2>{t(`dashboard.trendMetrics.${metric}`)}</h2>
          </div>
          <SegmentedTabs
            id="usage-summary-trend"
            label={t("dashboard.trendMetric")}
            value={metric}
            items={(["cost", "tokens", "requests", "success_rate", "latency_p95"] as const)
              .map((key) => ({ key, label: t(`dashboard.trendMetrics.${key}`) }))}
            onChange={setMetric}
          />
        </header>
        <div id="usage-summary-trend-panel" role="tabpanel" aria-labelledby={`usage-summary-trend-tab-${metric}`}>
          {report.buckets.length === 0
            ? <EmptyState title={t("usage.summary.emptyTitle")}>{t("usage.summary.emptyDescription")}</EmptyState>
            : (
              <Suspense fallback={<Loading label={t("dashboard.loadingTrend")} />}>
                <TrendChart points={periodTrendPoints(report.buckets)} metric={metric} />
              </Suspense>
            )}
        </div>
        {/* A percentile read off a stored histogram lands inside a bucket, not
            on a value. Saying so keeps it from being compared against the exact
            figure the live dashboard shows for today. */}
        {report.buckets.length > 0 && metric === "latency_p95" && (
          <p className="panel-note">{t("usage.summary.latencyApproximate")}</p>
        )}
      </article>

      {/* Labelled by what the server says it grouped by, not by what the
          control currently reads: while a new query is in flight the two
          disagree, and the heading would name a dimension the rows are not. */}
      {groupedBy && (
        <article className="panel">
          <header className="panel-header">
            <div>
              <p className="eyebrow">{t("usage.summary.byDimension")}</p>
              <h2>{t(`usage.summary.dimensions.${groupedBy}`)}</h2>
            </div>
            {report.groups_truncated && (
              <span className="badge">{t("usage.summary.othersFolded", { count: report.groups_other_count ?? 0 })}</span>
            )}
          </header>
          <SummaryGroupTable
            dimension={groupedBy}
            groups={report.groups ?? []}
            labels={report.resource_labels ?? {}}
            range={rangeInstants(report)}
            sort={report.sort ?? sort}
            ascending={(report.order ?? "desc") === "asc"}
            onSort={rankBy}
          />
        </article>
      )}
    </section>
  );
}

function SummaryGroupTable({ dimension, groups, labels, range, sort, ascending, onSort }: {
  dimension: Exclude<Dimension, "">;
  groups: SummaryGroup[];
  labels: Record<string, string>;
  range: { start: string; end: string } | null;
  sort: string;
  ascending: boolean;
  onSort: (key: SortKey) => void;
}) {
  const { t } = useTranslation();
  const requestLevel = dimension === "project" || dimension === "requested_model";
  const headings: Record<SortKey, string> = {
    calls: requestLevel ? t("dashboard.completedRequests") : t("usage.summary.attempts"),
    success_rate: requestLevel ? t("dashboard.requestSuccessRate") : t("usage.summary.attemptSuccessRate"),
    tokens: t("dashboard.reportedTokens"),
    cost: t("dashboard.knownCost"),
  };
  return (
    <div className="table-shell">
      <table>
        <thead>
          <tr>
            <th>{t(`usage.summary.dimensions.${dimension}`)}</th>
            {sortableColumns.map((column) => (
              <th key={column.key} aria-sort={sort === column.key ? (ascending ? "ascending" : "descending") : "none"}>
                <button type="button" className="column-sort" onClick={() => onSort(column.key)}>
                  {headings[column.key]}
                  <i aria-hidden="true">{sort === column.key ? (ascending ? "↑" : "↓") : "↕"}</i>
                </button>
              </th>
            ))}
            <th />
          </tr>
        </thead>
        <tbody>
          {/* The skeleton stays when there is nothing to put in it: an empty
              table still says which questions this view answers, where a
              replacement panel says only that something is missing. */}
          {groups.length === 0 && (
            <tr><td colSpan={6}>{t("usage.summary.emptyDescription")}</td></tr>
          )}
          {groups.map((group) => {
            const total = requestLevel ? group.requests ?? 0 : group.attempts;
            const failed = requestLevel ? group.request_errors ?? 0 : group.errors;
            const rate = total ? (total - failed) / total : 1;
            const tokens = Math.max(0, group.input_tokens + group.output_tokens
              - group.estimated_input_tokens - group.estimated_output_tokens);
            // The folded tail is many keys at once, so there is nothing single
            // to filter the detail list by. Offering the link anyway would send
            // the operator to a list that answers a different question.
            const folded = group.key === "__other__";
            return (
              <tr key={group.key}>
                <td>
                  <strong>{labels[group.key] ?? (folded ? t("usage.summary.others") : group.key)}</strong>
                  {!folded && labels[group.key] && <code>{group.key}</code>}
                </td>
                <td>{compactNumber(total)}</td>
                <td>{`${(rate * 100).toFixed(1)}%`}</td>
                <td>{compactNumber(tokens)}</td>
                <td>{money(group.cost_micros_usd)}{group.unknown_attempts > 0 && <small>{t("usage.summary.unknownAttempts", { count: group.unknown_attempts })}</small>}</td>
                <td>
                  {!folded && range && (
                    <Link href={detailHref(dimension, group.key, range)}>{t("usage.summary.viewAttempts")} →</Link>
                  )}
                </td>
              </tr>
            );
          })}
        </tbody>
      </table>
    </div>
  );
}

// rangeInstants takes the absolute window from the buckets themselves. A date
// label cannot be converted back into an interval without knowing which
// generation of the accounting timezone produced it, and the server already
// answered that question when it stamped the rows.
function rangeInstants(report: UsageSummary): { start: string; end: string } | null {
  if (report.buckets.length === 0) return null;
  return { start: report.buckets[0].start, end: report.buckets[report.buckets.length - 1].end };
}

// The link carries absolute instants, not the date label: the detail view
// filters on RFC3339 timestamps, and reconstructing them from a label would
// have to guess which generation of the accounting timezone produced it.
function detailHref(dimension: Exclude<Dimension, "">, key: string, range: { start: string; end: string }) {
  const params = new URLSearchParams({
    tab: "attempts",
    [detailFilters[dimension]]: key,
    start: range.start,
    end: range.end,
  });
  return `/admin/usage?${params}`;
}

function summaryCostDetail(t: (key: string, options?: Record<string, unknown>) => string, totals: SummaryMetrics) {
  const parts: string[] = [];
  if (totals.estimated_cost_micros_usd > 0) {
    parts.push(t("dashboard.estimatedCost", { cost: money(totals.estimated_cost_micros_usd) }));
  }
  if (totals.unknown_attempts > 0) {
    parts.push(t("dashboard.unknownCostExcluded", { count: totals.unknown_attempts }));
  }
  return parts.length ? parts.join(" · ") : t("dashboard.allCostKnown");
}

// The export is built from the response already on the page, so what an
// operator opens in a spreadsheet is the same set of numbers they were looking
// at — including the folded row, without which the rows would not add up to the
// total printed beside them.
function downloadSummaryCSV(report: UsageSummary, filename: string) {
  const header = [
    "period", "start", "end", "requests", "request_errors", "attempts", "errors",
    "input_tokens", "output_tokens", "estimated_input_tokens", "estimated_output_tokens",
    "cost_micros_usd", "estimated_cost_micros_usd", "unknown_attempts",
  ];
  const rows = [header.join(",")];
  for (const bucket of report.buckets) {
    rows.push([
      bucket.period, bucket.start, bucket.end, bucket.requests ?? "", bucket.request_errors ?? "",
      bucket.attempts, bucket.errors, bucket.input_tokens, bucket.output_tokens,
      bucket.estimated_input_tokens, bucket.estimated_output_tokens,
      bucket.cost_micros_usd, bucket.estimated_cost_micros_usd, bucket.unknown_attempts,
    ].join(","));
  }
  if (report.groups?.length) {
    rows.push("");
    rows.push(["group", "requests", "attempts", "errors", "cost_micros_usd", "unknown_attempts"].join(","));
    for (const group of report.groups) {
      rows.push([
        csvCell(group.key), group.requests ?? "", group.attempts, group.errors,
        group.cost_micros_usd, group.unknown_attempts,
      ].join(","));
    }
  }
  const url = URL.createObjectURL(new Blob([rows.join("\n")], { type: "text/csv;charset=utf-8" }));
  const anchor = document.createElement("a");
  anchor.href = url;
  anchor.download = filename;
  anchor.click();
  URL.revokeObjectURL(url);
}

function csvCell(value: string) {
  return /[",\n]/.test(value) ? `"${value.replace(/"/g, '""')}"` : value;
}
