import type { AlignedData } from "uplot";
import type { Bucket } from "./types";

export const TREND_WINDOW_SECONDS = 7 * 24 * 60 * 60;

export interface TrendSeries {
  data: AlignedData;
  range: [number, number];
}

export function buildTrendSeries(buckets: Bucket[], nowMillis = Date.now()): TrendSeries {
  const end = Math.floor(nowMillis / 1000);
  const start = end - TREND_WINDOW_SECONDS;
  const points = buckets
    .map((bucket) => ({ bucket, timestamp: Date.parse(bucket.hour) / 1000 }))
    .filter(({ timestamp }) => Number.isFinite(timestamp) && timestamp >= start && timestamp <= end)
    .sort((left, right) => left.timestamp - right.timestamp);

  if (points.length === 0) {
    return { data: [[start, end], [0, 0], [0, 0]], range: [start, end] };
  }

  return {
    data: [
      points.map(({ timestamp }) => timestamp),
      points.map(({ bucket }) => bucket.requests),
      points.map(({ bucket }) => reportedTokens(bucket)),
    ],
    range: [start, end],
  };
}

export function reportedTokens(bucket: Bucket) {
  return Math.max(
    0,
    bucket.input_tokens + bucket.output_tokens -
      (bucket.estimated_input_tokens ?? 0) - (bucket.estimated_output_tokens ?? 0),
  );
}
