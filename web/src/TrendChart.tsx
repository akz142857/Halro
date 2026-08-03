import { useEffect, useRef } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";
import type { Bucket } from "./types";
import { buildTrendSeries, summarizeTrend, type TrendMetric } from "./trend";
import { compactNumber } from "./format";
import { useTranslation } from "react-i18next";

export default function TrendChart({ buckets, metric }: { buckets: Bucket[]; metric: TrendMetric }) {
  const { t } = useTranslation();
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
    const chart = new uPlot(
      {
        width: Math.max(Math.floor(host.current.clientWidth), 1),
        height: 280,
        cursor: { drag: { x: false, y: false } },
        legend: { show: false },
        scales: { x: { time: true, range: series.range } },
        axes: [
          { stroke: "#778985", grid: { stroke: "#1c302c" }, ticks: { stroke: "#1c302c" }, font: "11px ui-monospace" },
          { stroke: "#778985", grid: { stroke: "#1c302c" }, ticks: { stroke: "#1c302c" }, font: "11px ui-monospace" },
        ],
        series: [
          {},
          { label: t(`dashboard.trendMetrics.${metric}`), stroke: "#d8ff61", width: 2, fill: "rgba(216,255,97,.07)" },
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
  }, [t, metric]);
  useEffect(() => {
    const chart = chartRef.current;
    if (!chart) return;
    const series = buildTrendSeries(buckets, metric);
    chart.setData(series.data);
    chart.setScale("x", { min: series.range[0], max: series.range[1] });
  }, [buckets, metric]);
  return <div className="trend-chart" ref={host} role="img" aria-label={accessibleLabel} />;
}
