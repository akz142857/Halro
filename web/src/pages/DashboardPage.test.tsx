import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { api } from "../api";
import type { Dashboard } from "../types";
import { DashboardPage } from "./DashboardPage";

vi.mock("../TrendChart", () => ({ default: () => <div role="img" aria-label="趋势图" /> }));

describe("DashboardPage", () => {
  it("shows the real alert and WAL queue state and marks failures unhealthy", async () => {
    vi.spyOn(api, "dashboard").mockResolvedValue(dashboard({
      alerts: { Accepted: 12, Delivered: 8, Failed: 2, Dropped: 1, Queued: 4 },
      wal: { Batches: 20, Records: 40, Errors: 1, QueueDepth: 3, QueueCapacity: 16 },
    }));
    renderPage();

    const alertRow = (await screen.findByText("告警投递")).closest(".status-row")!;
    expect(alertRow).toHaveTextContent("4 条排队 · 3 条失败或丢弃");
    expect(alertRow.querySelector(".status-dot")).toHaveClass("bad");

    const walRow = screen.getByText("WAL 队列").closest(".status-row")!;
    expect(walRow).toHaveTextContent("3 条排队 · 1 次错误");
    expect(walRow.querySelector(".status-dot")).toHaveClass("bad");
  });

  it("reports idle healthy channels as ready", async () => {
    vi.spyOn(api, "dashboard").mockResolvedValue(dashboard());
    renderPage();

    const alertRow = (await screen.findByText("告警投递")).closest(".status-row")!;
    const walRow = screen.getByText("WAL 队列").closest(".status-row")!;
    expect(alertRow).toHaveTextContent("就绪");
    expect(walRow).toHaveTextContent("就绪");
    expect(alertRow.querySelector(".status-dot")).toHaveClass("ok");
    expect(walRow.querySelector(".status-dot")).toHaveClass("ok");
  });
});

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><DashboardPage /></QueryClientProvider>);
}

function dashboard(overrides: Partial<Dashboard> = {}): Dashboard {
  return {
    usage: {
      today: { hour: "2026-08-03T00:00:00Z", requests: 0, attempts: 0, input_tokens: 0, output_tokens: 0, cost_micros_usd: 0, errors: 0, latency_millis: 0 },
      hourly: [], active_requests: 0, watermark_sequence: 0,
      breakdowns: { project: [], provider: [], requested_model: [], provider_model: [] },
      recent_anomalies: [],
    },
    governance: {
      policy_rejections: { rpm: 0, tpm: 0, project_concurrency: 0, provider_concurrency: 0, deployment_concurrency: 0, budget: 0, token_guard: 0, total: 0 },
      budget: { at_risk: 0, items: [] },
      capacity: { at_risk: 0, items: [] },
    },
    resource_labels: {},
    accounting_status: 0,
    alerts: { Accepted: 0, Delivered: 0, Failed: 0, Dropped: 0, Queued: 0 },
    wal: { Batches: 0, Records: 0, Errors: 0, QueueDepth: 0, QueueCapacity: 16 },
    ...overrides,
  };
}
