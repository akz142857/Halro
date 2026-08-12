import { useEffect, useRef } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";
import type { Bucket } from "./types";
import { buildTrendSeries, summarizeTrend, type TrendMetric } from "./trend";
import { compactNumber } from "./format";
import { useTranslation } from "react-i18next";
import { useAppearance } from "./theme";
import { useAccountingTimeZone } from "./timezone";

function readChartTokens(host: HTMLElement) {
  const style = getComputedStyle(host);
  const read = (name: string, fallback: string) => style.getPropertyValue(name).trim() || fallback;
  return {
    primary: read("--color-chart-series-1", "currentColor"),
    secondary: read("--color-chart-series-2", "currentColor"),
    fill: read("--color-chart-series-1-fill", "transparent"),
    grid: read("--color-chart-grid", "currentColor"),
    axis: read("--color-chart-axis", "currentColor"),
  };
}

function formatValue(metric: TrendMetric, value: number | null) {
  if (value == null) return "—";
  if (metric === "cost") return `$${value.toFixed(value < 10 ? 3 : 2)}`;
  if (metric === "success_rate") return `${value.toFixed(1)}%`;
  if (metric === "latency_p95") return `${Math.round(value)} ms`;
  return compactNumber(value);
}

function seriesKeys(metric: TrendMetric) {
  if (metric === "requests") return ["successful", "failed"];
  if (metric === "tokens" || metric === "cost") return ["confirmed", "estimated"];
  return [metric];
}

export default function TrendChart({ buckets, metric }: { buckets: Bucket[]; metric: TrendMetric }) {
  const { t } = useTranslation();
  const appearance = useAppearance();
  const timeZone = useAccountingTimeZone();
  const host = useRef<HTMLDivElement>(null);
  const chartRef = useRef<uPlot | null>(null);
  const summary = summarizeTrend(buckets, metric);
  const accessibleLabel = t("dashboard.trendAria", {
    metric: t(`dashboard.trendMetrics.${metric}`),
    value: formatValue(metric, summary.value),
  });

  useEffect(() => {
    if (!host.current) return;
    const chartData = buildTrendSeries(buckets, metric);
    const tokens = readChartTokens(host.current);
    const keys = seriesKeys(metric);
    const chart = new uPlot(
      {
        width: Math.max(Math.floor(host.current.clientWidth), 1),
        height: 280,
        cursor: { drag: { x: false, y: false } },
        legend: { show: true, live: true },
        tzDate: (timestamp) => uPlot.tzDate(new Date(timestamp * 1000), timeZone),
        scales: {
          x: { time: true, range: chartData.range },
          y: metric === "success_rate" ? { range: [0, 100] } : {},
        },
        axes: [
          { stroke: tokens.axis, grid: { stroke: tokens.grid }, ticks: { stroke: tokens.grid }, font: "11px ui-monospace" },
          {
            stroke: tokens.axis, grid: { stroke: tokens.grid }, ticks: { stroke: tokens.grid }, font: "11px ui-monospace",
            values: (_chart, values) => values.map((value) => formatValue(metric, value)),
          },
        ],
        series: [
          {},
          ...keys.map((key, index) => ({
            label: t(`dashboard.trendSeries.${key}`),
            stroke: index === 0 ? tokens.primary : tokens.secondary,
            width: 2,
            fill: keys.length === 1 ? tokens.fill : undefined,
            spanGaps: false,
            value: (_chart: uPlot, raw: number | null) => formatValue(metric, raw),
          })),
        ],
      },
      chartData.data,
      host.current,
    );
    chartRef.current = chart;
    const resize = new ResizeObserver(([entry]) => {
      chart.setSize({ width: Math.max(Math.floor(entry.contentRect.width), 1), height: 280 });
    });
    resize.observe(host.current);
    return () => {
      chartRef.current = null;
      resize.disconnect();
      chart.destroy();
    };
  }, [t, metric, appearance, timeZone]);

  useEffect(() => {
    const chart = chartRef.current;
    if (!chart) return;
    const series = buildTrendSeries(buckets, metric);
    chart.setData(series.data);
    chart.setScale("x", { min: series.range[0], max: series.range[1] });
  }, [buckets, metric]);

  return <div className="trend-chart" ref={host} role="img" aria-label={accessibleLabel} />;
}
