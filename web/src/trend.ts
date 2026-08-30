import type { AlignedData } from "uplot";
import type { Bucket, SummaryBucket } from "./types";

export const TREND_WINDOW_SECONDS = 7 * 24 * 60 * 60;
const HOUR_SECONDS = 60 * 60;

export type TrendMetric = "requests" | "success_rate" | "latency_p95" | "tokens" | "cost";

// TrendMetrics is the set of columns a chart reads. Both the dashboard's hourly
// buckets and the summary's period buckets carry these names, which is why one
// chart can draw either without a second code path per shape.
export interface TrendMetrics {
  requests?: number;
  request_errors?: number;
  request_latency_samples?: number;
  request_latency_p95_millis?: number;
  input_tokens: number;
  output_tokens: number;
  estimated_input_tokens?: number;
  estimated_output_tokens?: number;
  cost_micros_usd: number;
  estimated_cost_micros_usd?: number;
}

// A TrendPoint carries its own position on the x axis. Earlier this module
// generated positions from a fixed step, which only worked while every bucket
// was one hour: an accounting day is 23 or 25 hours across a DST change, and a
// month is not a fixed number of seconds at all.
export interface TrendPoint {
  startSeconds: number;
  metrics: TrendMetrics | null;
}

export interface TrendSeries {
  data: AlignedData;
  range: [number, number];
}

export interface TrendSummary {
  value: number;
}

// hourlyTrendPoints pins the dashboard's chart to the last seven days and fills
// every hour, so an idle gateway shows a flat week rather than an empty frame.
export function hourlyTrendPoints(buckets: Bucket[], nowMillis = Date.now()): TrendPoint[] {
  const end = Math.floor(nowMillis / 1000);
  const start = end - TREND_WINDOW_SECONDS;
  const byHour = new Map<number, Bucket>();
  for (const bucket of buckets) {
    const timestamp = Date.parse(bucket.hour) / 1000;
    if (Number.isFinite(timestamp) && timestamp >= start && timestamp <= end) {
      byHour.set(Math.floor(timestamp / HOUR_SECONDS) * HOUR_SECONDS, bucket);
    }
  }
  const points: TrendPoint[] = [];
  for (let timestamp = Math.ceil(start / HOUR_SECONDS) * HOUR_SECONDS; timestamp <= end; timestamp += HOUR_SECONDS) {
    points.push({ startSeconds: timestamp, metrics: byHour.get(timestamp) ?? null });
  }
  return points;
}

// periodTrendPoints charts accounting periods, which the server returns with
// their own absolute boundaries. A period with no traffic has no bucket at all,
// so a zero point is inserted where one period's end does not meet the next
// one's start — otherwise a quiet week would read as a missing measurement
// rather than as no spend.
export function periodTrendPoints(buckets: SummaryBucket[]): TrendPoint[] {
  const points: TrendPoint[] = [];
  let previousEnd: number | null = null;
  for (const bucket of buckets) {
    const start = Date.parse(bucket.start) / 1000;
    const end = Date.parse(bucket.end) / 1000;
    if (!Number.isFinite(start)) continue;
    if (previousEnd != null && start > previousEnd) {
      points.push({ startSeconds: previousEnd, metrics: emptyMetrics() });
    }
    points.push({ startSeconds: start, metrics: bucket });
    if (Number.isFinite(end)) previousEnd = end;
  }
  return points;
}

function emptyMetrics(): TrendMetrics {
  return { requests: 0, request_errors: 0, input_tokens: 0, output_tokens: 0, cost_micros_usd: 0 };
}

export function buildTrendSeries(points: TrendPoint[], metric: TrendMetric = "requests"): TrendSeries {
  const timestamps: number[] = [];
  const rows: Array<Array<number | null>> = Array.from({ length: trendSeriesCount(metric) }, () => []);
  for (const point of points) {
    timestamps.push(point.startSeconds);
    trendValues(point.metrics, metric).forEach((value, index) => rows[index].push(value));
  }
  const range: [number, number] = timestamps.length
    ? [timestamps[0], timestamps[timestamps.length - 1]]
    : [0, 0];
  return { data: [timestamps, ...rows] as AlignedData, range };
}

function trendSeriesCount(metric: TrendMetric) {
  return metric === "requests" || metric === "tokens" || metric === "cost" ? 2 : 1;
}

function trendValues(metrics: TrendMetrics | null, metric: TrendMetric): Array<number | null> {
  if (!metrics) {
    return metric === "success_rate" || metric === "latency_p95" ? [null] : Array(trendSeriesCount(metric)).fill(0);
  }
  const requests = metrics.requests ?? 0;
  const failed = metrics.request_errors ?? 0;
  switch (metric) {
    case "requests":
      return [Math.max(0, requests - failed), failed];
    case "success_rate":
      return [requests ? Math.max(0, requests - failed) / requests * 100 : null];
    case "latency_p95":
      return [metrics.request_latency_samples ? metrics.request_latency_p95_millis ?? 0 : null];
    case "tokens":
      return [reportedTokens(metrics), estimatedTokens(metrics)];
    case "cost": {
      const estimated = metrics.estimated_cost_micros_usd ?? 0;
      return [Math.max(0, metrics.cost_micros_usd - estimated) / 1_000_000, estimated / 1_000_000];
    }
  }
}

export function reportedTokens(metrics: TrendMetrics) {
  return Math.max(0, metrics.input_tokens + metrics.output_tokens - estimatedTokens(metrics));
}

export function estimatedTokens(metrics: TrendMetrics) {
  return (metrics.estimated_input_tokens ?? 0) + (metrics.estimated_output_tokens ?? 0);
}

export function summarizeTrend(points: TrendPoint[], metric: TrendMetric): TrendSummary {
  const { data } = buildTrendSeries(points, metric);
  if (metric === "success_rate") {
    let successful = 0;
    let requests = 0;
    for (const point of points) {
      if (!point.metrics) continue;
      const total = point.metrics.requests ?? 0;
      successful += Math.max(0, total - (point.metrics.request_errors ?? 0));
      requests += total;
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
