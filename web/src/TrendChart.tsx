import { useEffect, useRef } from "react";
import uPlot from "uplot";
import "uplot/dist/uPlot.min.css";
import type { Bucket } from "./types";
import { buildTrendSeries } from "./trend";

export default function TrendChart({ buckets }: { buckets: Bucket[] }) {
  const host = useRef<HTMLDivElement>(null);
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
          { label: "Requests", stroke: "#d8ff61", width: 2, fill: "rgba(216,255,97,.07)" },
          { label: "Tokens", stroke: "#58d6bc", width: 1.5, scale: "tokens" },
        ],
      },
      series.data,
      host.current,
    );
    const resize = new ResizeObserver(([entry]) => {
      chart.setSize({ width: Math.max(entry.contentRect.width, 320), height: 280 });
    });
    resize.observe(host.current);
    return () => {
      resize.disconnect();
      chart.destroy();
    };
  }, [buckets]);
  return <div className="trend-chart" ref={host} aria-label="最近七天请求与 Token 趋势图" />;
}
