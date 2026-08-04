import { useEffect, useRef } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";
import type { Bucket } from "./types";
import { buildTrendSeries, summarizeTrend, type TrendMetric } from "./trend";
import { compactNumber } from "./format";
import { useTranslation } from "react-i18next";
import { useAppearance } from "./theme";

/**
 * Reads the semantic chart tokens from the design system. uPlot draws to a
 * canvas, so it can't inherit CSS variables — we resolve them at build time and
 * re-run when the appearance changes (PRD §8.3). Fallbacks keep charts legible
 * if a token is ever missing.
 */
function readChartTokens(host: HTMLElement) {
  const style = getComputedStyle(host);
  const read = (name: string, fallback: string) => style.getPropertyValue(name).trim() || fallback;
  return {
    series: read("--color-chart-series-1", "#58d6bc"),
    fill: read("--color-chart-series-1-fill", "rgba(88, 214, 188, 0.10)"),
    grid: read("--color-chart-grid", "#1c302c"),
    axis: read("--color-chart-axis", "#778985"),
  };
}

export default function TrendChart({ buckets, metric }: { buckets: Bucket[]; metric: TrendMetric }) {
  const { t } = useTranslation();
  const appearance = useAppearance();
  const host = useRef<HTMLDivElement>(null);
  const chartRef = useRef<uPlot | null>(null);
  const summary = summarizeTrend(buckets, metric);
  const accessibleLabel = t("dashboard.trendAria", {
    metric: t(`dashboard.trendMetrics.${metric}`),
    value: metric === "cost" ? summary.value.toFixed(2) : compactNumber(summary.value),
  });
  useEffect(() => {
    if (!host.current) return;
    const series = buildTrendSeries(buckets, metric);
    const tokens = readChartTokens(host.current);
    const chart = new uPlot(
      {
        width: Math.max(Math.floor(host.current.clientWidth), 1),
        height: 280,
        cursor: { drag: { x: false, y: false } },
        legend: { show: false },
        scales: { x: { time: true, range: series.range } },
        axes: [
          { stroke: tokens.axis, grid: { stroke: tokens.grid }, ticks: { stroke: tokens.grid }, font: "11px ui-monospace" },
          { stroke: tokens.axis, grid: { stroke: tokens.grid }, ticks: { stroke: tokens.grid }, font: "11px ui-monospace" },
        ],
        series: [
          {},
          { label: t(`dashboard.trendMetrics.${metric}`), stroke: tokens.series, width: 2, fill: tokens.fill },
        ],
      },
      series.data,
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
  }, [t, metric, appearance]);
  useEffect(() => {
    const chart = chartRef.current;
    if (!chart) return;
    const series = buildTrendSeries(buckets, metric);
    chart.setData(series.data);
    chart.setScale("x", { min: series.range[0], max: series.range[1] });
  }, [buckets, metric]);
  return <div className="trend-chart" ref={host} role="img" aria-label={accessibleLabel} />;
}
