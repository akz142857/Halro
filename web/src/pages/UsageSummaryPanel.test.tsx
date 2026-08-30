import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import { UsageSummaryPanel } from "./UsageSummaryPanel";
import type { SummaryGroup, SummaryMetrics, UsageSummary } from "../types";

vi.mock("../TrendChart", () => ({ default: () => <div role="img" aria-label="趋势图" /> }));

function metrics(overrides: Partial<SummaryMetrics> = {}): SummaryMetrics {
  return {
    attempts: 0, errors: 0, input_tokens: 0, output_tokens: 0,
    estimated_input_tokens: 0, estimated_output_tokens: 0,
    provider_cached_input_tokens: 0, provider_cache_write_input_tokens: 0, provider_reasoning_tokens: 0,
    cost_micros_usd: 0, estimated_cost_micros_usd: 0, unknown_attempts: 0, latency_millis: 0,
    attempt_latency_samples: 0, attempt_latency_p95_millis: 0, latency_approximate: true,
    ...overrides,
  };
}

function summary(overrides: Partial<UsageSummary> = {}): UsageSummary {
  return {
    granularity: "month",
    start: "2026-08-01",
    end: "2026-08-31",
    totals: metrics({ requests: 10, request_errors: 1, attempts: 12, cost_micros_usd: 4_500_000 }),
    buckets: [{
      period: "2026-08",
      start: "2026-08-01T00:00:00Z",
      end: "2026-09-01T00:00:00Z",
      ...metrics({ requests: 10, request_errors: 1, attempts: 12, cost_micros_usd: 4_500_000 }),
    }],
    timezone_changes: [],
    watermark_sequence: 42,
    ...overrides,
  };
}

function renderPanel() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><UsageSummaryPanel /></QueryClientProvider>);
}

describe("UsageSummaryPanel", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
  });

  it("reports the range totals with the cost qualified, not rounded into one number", async () => {
    vi.spyOn(api, "usageSummary").mockResolvedValue(summary({
      totals: metrics({
        requests: 10, request_errors: 1, attempts: 12,
        cost_micros_usd: 4_500_000, estimated_cost_micros_usd: 500_000, unknown_attempts: 2,
      }),
    }));
    renderPanel();

    expect(await screen.findByText(/4\.50/)).toBeVisible();
    expect(screen.getByText("90.0%")).toBeVisible();
    // The estimated share and the unpriced attempts stay visible beside the
    // figure: a cost that hides them reads as exact when it is not.
    expect(screen.getByTitle(/估算/)).toBeVisible();
    expect(screen.getByTitle(/未知/)).toBeVisible();
  });

  // A drill-down has to carry the interval the row covered. A date label cannot
  // be turned back into one, so the link uses the instants the server stamped.
  it("links a group to the attempts it covers, with absolute boundaries", async () => {
    vi.spyOn(api, "usageSummary").mockResolvedValue(summary({
      group_by: "project",
      groups: [{ key: "prj_a", ...metrics({ requests: 10, attempts: 12, cost_micros_usd: 4_500_000 }) } as SummaryGroup],
      resource_labels: { prj_a: "结算服务" },
    }));
    renderPanel();

    const link = await screen.findByRole("link", { name: /查看明细/ });
    const href = new URL(link.getAttribute("href") ?? "", "http://localhost");
    expect(href.pathname).toBe("/admin/usage");
    expect(href.searchParams.get("tab")).toBe("attempts");
    expect(href.searchParams.get("project_id")).toBe("prj_a");
    expect(href.searchParams.get("start")).toBe("2026-08-01T00:00:00Z");
    expect(href.searchParams.get("end")).toBe("2026-09-01T00:00:00Z");
  });

  // The folded row is many keys at once, so there is nothing single to filter
  // by. Offering the link would send the operator to a different question.
  it("does not offer a drill-down for the folded tail", async () => {
    vi.spyOn(api, "usageSummary").mockResolvedValue(summary({
      group_by: "provider",
      groups: [
        { key: "prov_a", ...metrics({ attempts: 9, cost_micros_usd: 3_000_000 }) } as SummaryGroup,
        { key: "__other__", ...metrics({ attempts: 3, cost_micros_usd: 1_500_000 }) } as SummaryGroup,
      ],
      groups_truncated: true,
      groups_other_count: 4,
    }));
    renderPanel();

    const table = within(await screen.findByRole("table"));
    expect(table.getByText("其余合计")).toBeVisible();
    expect(table.getAllByRole("link")).toHaveLength(1);
    expect(screen.getByText("另有 4 项已合并")).toBeVisible();
  });

  // A provider row has no request identity to report, so the column it shows is
  // the attempt rate. Labelling attempts as requests would overstate what the
  // number means.
  it("names the attempt rate on a dimension with no request identity", async () => {
    vi.spyOn(api, "usageSummary").mockResolvedValue(summary({
      group_by: "provider",
      groups: [{ key: "prov_a", ...metrics({ attempts: 10, errors: 2, cost_micros_usd: 1_000_000 }) } as SummaryGroup],
    }));
    renderPanel();

    const table = within(await screen.findByRole("table"));
    expect(table.getByRole("columnheader", { name: "尝试成功率" })).toBeVisible();
    expect(table.getByText("80.0%")).toBeVisible();
  });

  // Two generations of the accounting timezone give the same date label two
  // different intervals. Summing them without saying so produces a month
  // nobody can reconcile against the ledger.
  it("says when the range straddles an accounting timezone change", async () => {
    vi.spyOn(api, "usageSummary").mockResolvedValue(summary({
      timezone_changes: [{ period_id: "2026-08-15", from_version: 1, to_version: 2 }],
    }));
    renderPanel();

    expect(await screen.findByText(/2026-08-15/)).toBeVisible();
  });

  // The granularity shows and hides nothing — it changes what the panel asks
  // the server for. Marked up as tabs it put a second tab strip on a page that
  // already has real ones, and promised panels that do not exist.
  it("offers the granularity as one labelled group of choices, not a second tab strip", async () => {
    vi.spyOn(api, "usageSummary").mockResolvedValue(summary());
    renderPanel();

    const group = await screen.findByRole("radiogroup", { name: "粒度" });
    const choices = within(group).getAllByRole("radio");
    expect(choices).toHaveLength(3);
    expect(within(group).getByRole("radio", { name: "按月" })).toBeChecked();
    expect(screen.queryByRole("tablist", { name: "粒度" })).toBeNull();
  });

  it("re-asks the server when the granularity changes", async () => {
    const request = vi.spyOn(api, "usageSummary").mockResolvedValue(summary());
    renderPanel();

    fireEvent.click(await screen.findByRole("radio", { name: "按天" }));
    await waitFor(() => expect(request).toHaveBeenCalledTimes(2));
    const query = new URLSearchParams((request.mock.calls[1][0] ?? "").slice(1));
    expect(query.get("granularity")).toBe("day");
  });

  it("asks the server for the selected granularity and dimension", async () => {
    const request = vi.spyOn(api, "usageSummary").mockResolvedValue(summary());
    renderPanel();

    await waitFor(() => expect(request).toHaveBeenCalled());
    const query = new URLSearchParams((request.mock.calls[0][0] ?? "").slice(1));
    expect(query.get("granularity")).toBe("month");
    expect(query.get("group_by")).toBe("project");
  });
});
