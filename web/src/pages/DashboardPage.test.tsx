import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import type { Dashboard } from "../types";
import { DashboardPage } from "./DashboardPage";

vi.mock("../TrendChart", () => ({ default: () => <div role="img" aria-label="趋势图" /> }));

describe("DashboardPage", () => {
  beforeEach(() => {
    vi.spyOn(api, "credentials").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "routes").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "projects").mockResolvedValue({ items: [], next_cursor: "" });
  });

  afterEach(() => vi.restoreAllMocks());

  it("walks a never-used instance through the configuration chain", async () => {
    vi.spyOn(api, "dashboard").mockResolvedValue(dashboard());
    vi.mocked(api.credentials).mockResolvedValue({ items: [{ id: "credential_openai" }] as never, next_cursor: "" });
    vi.mocked(api.projects).mockResolvedValue({ items: [{ id: "project_a" }] as never, next_cursor: "" });
    vi.spyOn(api, "keys").mockResolvedValue({ items: [], next_cursor: "" });
    renderPage();

    const panel = (await screen.findByRole("heading", { name: "把第一条请求跑通" })).closest("section")!;
    expect(panel).toHaveTextContent("2 / 6");
    const credentialStep = within(panel).getByText("1. 保存服务商凭据").closest("li")!;
    expect(credentialStep).toHaveTextContent("已创建 1 个");
    expect(credentialStep.querySelector(".status-dot")).toHaveClass("ok");
    expect(within(credentialStep).getByRole("link", { name: "打开凭据库" })).toHaveAttribute("href", "/admin/providers?view=credentials");
    const routeStep = within(panel).getByText("4. 发布模型路由").closest("li")!;
    expect(routeStep).toHaveTextContent("尚未创建");
    expect(routeStep.querySelector(".status-dot")).toHaveClass("bad");
    expect(panel).toHaveTextContent("六步全部完成后才能发出请求");
    expect(within(panel).getByRole("link", { name: "打开开发者工作台" })).toHaveAttribute("href", "/admin/developer");
  });

  it("drops the checklist once the gateway has served traffic", async () => {
    const used = dashboard();
    used.usage.watermark_sequence = 42;
    vi.spyOn(api, "dashboard").mockResolvedValue(used);
    renderPage();

    expect(await screen.findByText("告警投递")).toBeVisible();
    expect(screen.queryByRole("heading", { name: "把第一条请求跑通" })).not.toBeInTheDocument();
    expect(api.credentials).not.toHaveBeenCalled();
  });

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
