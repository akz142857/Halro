import { useEffect, useRef } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";
import type { Bucket } from "./types";
import { buildTrendSeries } from "./trend";
import { useTranslation } from "react-i18next";

export default function TrendChart({ buckets }: { buckets: Bucket[] }) {
  const { t } = useTranslation();
  const host = useRef<HTMLDivElement>(null);
  const chartRef = useRef<uPlot | null>(null);
  useEffect(() => {
    if (!host.current) return;
    const series = buildTrendSeries(buckets);
    const chart = new uPlot(
      {
        width: Math.max(host.current.clientWidth, 320),
        height: 280,
        cursor: { drag: { x: false, y: false } },
        legend: { show: false },
        scales: { x: { time: true, range: series.range }, tokens: { auto: true } },
        axes: [
          { stroke: "#778985", grid: { stroke: "#1c302c" }, ticks: { stroke: "#1c302c" }, font: "11px ui-monospace" },
          { stroke: "#778985", grid: { stroke: "#1c302c" }, ticks: { stroke: "#1c302c" }, font: "11px ui-monospace" },
        ],
        series: [
          {},
          { label: t("dashboard.requests"), stroke: "#d8ff61", width: 2, fill: "rgba(216,255,97,.07)" },
          { label: t("dashboard.tokens"), stroke: "#58d6bc", width: 1.5, scale: "tokens" },
        ],
      },
      series.data,
      host.current,
    );
    chartRef.current = chart;
    const resize = new ResizeObserver(([entry]) => {
      chart.setSize({ width: Math.max(entry.contentRect.width, 320), height: 280 });
    });
    resize.observe(host.current);
    return () => {
      chartRef.current = null;
      resize.disconnect();
      chart.destroy();
    };
  }, [t]);
  useEffect(() => {
    const chart = chartRef.current;
    if (!chart) return;
    const series = buildTrendSeries(buckets);
    chart.setData(series.data);
    chart.setScale("x", { min: series.range[0], max: series.range[1] });
  }, [buckets]);
  return <div className="trend-chart" ref={host} aria-label={t("dashboard.trendAria")} />;
}
