import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import type { Dashboard, OnboardingReadiness } from "../types";
import { timeContext } from "../test/fixtures";
import { DashboardPage } from "./DashboardPage";

vi.mock("../TrendChart", () => ({ default: () => <div role="img" aria-label="趋势图" /> }));

describe("DashboardPage", () => {
  beforeEach(() => {
    vi.spyOn(api, "onboardingReadiness").mockResolvedValue(onboarding());
  });

  afterEach(() => vi.restoreAllMocks());

  it("walks a never-used instance through the configuration chain", async () => {
    vi.spyOn(api, "dashboard").mockResolvedValue(dashboard({ first_value_reached: false }));
    renderPage();

    expect(await screen.findByRole("heading", { name: "开始使用 Halro" })).toBeVisible();
    const panel = (await screen.findByRole("heading", { name: "完成第一条真实请求" })).closest("section")!;
    expect(panel).toHaveTextContent("1 / 4");
    expect(within(panel).getAllByRole("listitem")).toHaveLength(4);
    expect(within(panel).getByRole("progressbar")).toHaveAttribute("aria-valuenow", "1");
    expect(panel.querySelector('li[aria-current="step"]')).toHaveTextContent("发布可调用模型");
    expect(within(panel).getByRole("link", { name: "配置模型与路由" })).toHaveAttribute("href", "/admin/deployments?intent=create&onboarding=first-request");
    expect(screen.queryByLabelText("今日关键指标")).not.toBeInTheDocument();
  });

  it("drops the checklist only after the first successful request", async () => {
    const used = dashboard();
    used.usage.watermark_sequence = 42;
    vi.spyOn(api, "dashboard").mockResolvedValue(used);
    renderPage();

    expect(await screen.findByText("告警投递")).toBeVisible();
    expect(screen.queryByRole("heading", { name: "完成第一条真实请求" })).not.toBeInTheDocument();
    expect(api.onboardingReadiness).not.toHaveBeenCalled();
  });

  it("keeps a failed first request visible with recovery evidence", async () => {
    const failed = dashboard({ first_value_reached: false });
    failed.usage.watermark_sequence = 42;
    failed.usage.today.attempts = 1;
    vi.spyOn(api, "dashboard").mockResolvedValue(failed);
    vi.mocked(api.onboardingReadiness).mockResolvedValue(onboarding({
      state: "verify_failed",
      completed_goals: 3,
      goals: [
        { key: "connect_provider", state: "complete", detail_code: "provider_ready", action_href: "/admin/providers" },
        { key: "publish_model", state: "complete", detail_code: "model_ready", action_href: "/admin/deployments" },
        { key: "grant_access", state: "complete", detail_code: "access_ready", action_href: "/admin/projects" },
        { key: "verify_request", state: "error", detail_code: "request_failed", action_href: "/admin/developer?onboarding=first-request" },
      ],
      last_verification: {
        outcome: "provider_error", request_id: "request_failed", http_status: 502,
        error_class: "upstream_timeout", completed_at: "2026-08-08T10:00:00Z",
      },
    }));
    renderPage();

    const panel = (await screen.findByRole("heading", { name: "完成第一条真实请求" })).closest("section")!;
    expect(panel).toHaveTextContent("上一条验证请求未成功");
    expect(panel).toHaveTextContent("request_failed");
    expect(panel).toHaveTextContent("upstream_timeout");
    expect(within(panel).getByRole("link", { name: "打开开发者工作台" })).toBeVisible();
  });

  it("shows the real alert and WAL queue state and marks failures unhealthy", async () => {
    vi.spyOn(api, "dashboard").mockResolvedValue(dashboard({
      alerts: { Accepted: 12, Delivered: 8, Failed: 2, Dropped: 1, Queued: 4 },
      wal: { batches: 20, records: 40, errors: 1, syncs: 20, sync_seconds: 0.08, queue_depth: 3, queue_capacity: 16 },
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


  // Anomaly timestamps are rendered in that same zone, not the browser's, so
  // they line up with the totals above them.
  it("renders anomaly timestamps in the accounting zone", async () => {
    const data = dashboard();
    data.time_context = timeContext({ accounting_timezone: "Asia/Shanghai" });
    data.usage.recent_anomalies = [{
      completed_at: "2026-08-05T17:30:00Z",
      project_id: "project_a",
      status: "error",
      error_class: "upstream_timeout",
      retry_count: 0,
      fallback_count: 0,
    }];
    vi.spyOn(api, "dashboard").mockResolvedValue(data);
    renderPage();

    // 17:30Z is 01:30 the next day in Shanghai; in UTC it would read 08-05 17:30.
    const stamp = await screen.findByText(/08\/06 01:30/);
    expect(stamp).toBeInTheDocument();
  });



  it("renders empty states when collection fields are null", async () => {
    const empty = dashboard();
    empty.usage.hourly = null as unknown as Dashboard["usage"]["hourly"];
    empty.usage.recent_anomalies = null as unknown as Dashboard["usage"]["recent_anomalies"];
    empty.governance.budget.items = null as unknown as Dashboard["governance"]["budget"]["items"];
    empty.governance.capacity.items = null as unknown as Dashboard["governance"]["capacity"]["items"];
    vi.spyOn(api, "dashboard").mockResolvedValue(empty);
    renderPage();

    expect(await screen.findByText("异常事件")).toBeInTheDocument();
    expect(screen.getByText("今天没有失败、重试或路由回退")).toBeInTheDocument();
  });
});

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["session"], {
    username: "admin", role: "administrator", locale: "system", appearance: "dark",
    csrf_token: "csrf", absolute_expires_at: "x", idle_expires_at: "x",
  });
  return render(<QueryClientProvider client={client}><DashboardPage /></QueryClientProvider>);
}

function dashboard(overrides: Partial<Dashboard> = {}): Dashboard {
  return {
    first_value_reached: true,
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
    time_context: timeContext(),
    alerts: { Accepted: 0, Delivered: 0, Failed: 0, Dropped: 0, Queued: 0 },
    wal: { batches: 0, records: 0, errors: 0, syncs: 0, sync_seconds: 0.08, queue_depth: 0, queue_capacity: 16 },
    ...overrides,
  };
}

function onboarding(overrides: Partial<OnboardingReadiness> = {}): OnboardingReadiness {
  return {
    version: 1,
    state: "configuring",
    completed_goals: 1,
    total_goals: 4,
    evaluated_at: "2026-08-08T10:00:00Z",
    goals: [
      { key: "connect_provider", state: "complete", detail_code: "provider_ready", action_href: "/admin/providers" },
      { key: "publish_model", state: "current", detail_code: "deployment_missing", action_href: "/admin/deployments?intent=create&onboarding=first-request" },
      { key: "grant_access", state: "blocked", detail_code: "model_blocking_access", action_href: "/admin/projects" },
      { key: "verify_request", state: "blocked", detail_code: "request_ready", action_href: "/admin/developer?onboarding=first-request" },
    ],
    ...overrides,
  };
}
