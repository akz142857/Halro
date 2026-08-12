import type { AlignedData } from "uplot";
import type { Bucket } from "./types";

export const TREND_WINDOW_SECONDS = 7 * 24 * 60 * 60;
const HOUR_SECONDS = 60 * 60;

export type TrendMetric = "requests" | "success_rate" | "latency_p95" | "tokens" | "cost";

export interface TrendSeries {
  data: AlignedData;
  range: [number, number];
}

export interface TrendSummary {
  value: number;
}

export function buildTrendSeries(
  buckets: Bucket[],
  metric: TrendMetric = "requests",
  nowMillis = Date.now(),
): TrendSeries {
  const end = Math.floor(nowMillis / 1000);
  const start = end - TREND_WINDOW_SECONDS;
  const byHour = new Map<number, Bucket>();
  for (const bucket of buckets) {
    const timestamp = Date.parse(bucket.hour) / 1000;
    if (Number.isFinite(timestamp) && timestamp >= start && timestamp <= end) {
      byHour.set(Math.floor(timestamp / HOUR_SECONDS) * HOUR_SECONDS, bucket);
    }
  }

  const timestamps: number[] = [];
  const rows: Array<Array<number | null>> = Array.from({ length: trendSeriesCount(metric) }, () => []);
  for (let timestamp = Math.ceil(start / HOUR_SECONDS) * HOUR_SECONDS; timestamp <= end; timestamp += HOUR_SECONDS) {
    timestamps.push(timestamp);
    const values = trendValues(byHour.get(timestamp), metric);
    values.forEach((value, index) => rows[index].push(value));
  }

  return { data: [timestamps, ...rows] as AlignedData, range: [start, end] };
}

function trendSeriesCount(metric: TrendMetric) {
  return metric === "requests" || metric === "tokens" || metric === "cost" ? 2 : 1;
}

function trendValues(bucket: Bucket | undefined, metric: TrendMetric): Array<number | null> {
  if (!bucket) {
    return metric === "success_rate" || metric === "latency_p95" ? [null] : Array(trendSeriesCount(metric)).fill(0);
  }
  switch (metric) {
    case "requests":
      return [Math.max(0, bucket.requests - bucket.request_errors), bucket.request_errors];
    case "success_rate":
      return [bucket.requests ? Math.max(0, bucket.requests - bucket.request_errors) / bucket.requests * 100 : null];
    case "latency_p95":
      return [bucket.request_latency_samples ? bucket.request_latency_p95_millis : null];
    case "tokens":
      return [reportedTokens(bucket), estimatedTokens(bucket)];
    case "cost": {
      const estimated = bucket.estimated_cost_micros_usd ?? 0;
      return [Math.max(0, bucket.cost_micros_usd - estimated) / 1_000_000, estimated / 1_000_000];
    }
  }
}

export function reportedTokens(bucket: Bucket) {
  return Math.max(0, bucket.input_tokens + bucket.output_tokens - estimatedTokens(bucket));
}

export function estimatedTokens(bucket: Bucket) {
  return (bucket.estimated_input_tokens ?? 0) + (bucket.estimated_output_tokens ?? 0);
}

export function summarizeTrend(
  buckets: Bucket[],
  metric: TrendMetric,
  nowMillis = Date.now(),
): TrendSummary {
  const { data } = buildTrendSeries(buckets, metric, nowMillis);
  if (metric === "success_rate") {
    let successful = 0;
    let requests = 0;
    for (const bucket of bucketsInWindow(buckets, nowMillis)) {
      successful += Math.max(0, bucket.requests - bucket.request_errors);
      requests += bucket.requests;
    }
    return { value: requests ? successful / requests * 100 : 0 };
  }
  if (metric === "latency_p95") {
    return { value: Math.max(0, ...data[1].map((point) => point ?? 0)) };
  }
  let value = 0;
  for (let seriesIndex = 1; seriesIndex < data.length; seriesIndex++) {
    for (const point of data[seriesIndex]) value += point ?? 0;
  }
  return { value };
}

function bucketsInWindow(buckets: Bucket[], nowMillis: number) {
  const end = nowMillis;
  const start = end - TREND_WINDOW_SECONDS * 1000;
  return buckets.filter((bucket) => {
    const timestamp = Date.parse(bucket.hour);
    return Number.isFinite(timestamp) && timestamp >= start && timestamp <= end;
  });
}
