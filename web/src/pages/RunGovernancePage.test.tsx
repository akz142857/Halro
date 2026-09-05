import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api";
import { RunGovernancePage } from "./RunGovernancePage";

describe("RunGovernancePage", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/run-governance");
  });

  it("drills from a Project through Work Unit and Run to attributed attempts", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [{ id: "prj_a", name: "Agent plane" }] as never, next_cursor: "" });
		vi.spyOn(api, "outcomeDefinitions").mockResolvedValue({ items: [], next_cursor: "" });
		vi.spyOn(api, "outcomes").mockResolvedValue({ items: [], next_cursor: "" });
    const workUnits = vi.spyOn(api, "workUnits").mockResolvedValue({ items: [{
      id: "wku_a", project_id: "prj_a", status: "open", created_by_key_id: "key_a",
      created_at: "2026-09-04T00:00:00Z", period_id: "prj_a:2026-09-04:UTC", period_timezone_version: 1,
    }], next_cursor: "" });
    const runs = vi.spyOn(api, "runs").mockResolvedValue({ items: [{
      id: "run_a", project_id: "prj_a", work_unit_id: "wku_a", budget_micros_usd: 5_000_000,
      committed_micros_usd: 125_000, reserved_micros_usd: 25_000, remaining_micros_usd: 4_850_000, budget_state: "available", unknown_attempts: 0, status: "active",
      created_by_key_id: "key_a", created_at: "2026-09-04T00:00:00Z", expires_at: "2026-09-05T00:00:00Z",
    }], next_cursor: "" });
    const usage = vi.spyOn(api, "usage").mockResolvedValue({ items: [{
      event_id: "evt_a", request_id: "req_a", attempt_id: "att_a", sequence: 3, attempt: 1, project_id: "prj_a",
      work_unit_id: "wku_a", run_id: "run_a", requested_model: "chat", provider_model: "gpt-test",
      provider_input_tokens: 10, provider_output_tokens: 2, cost_micros_usd: 125_000,
      input_cost_micros_usd: 100_000, output_cost_micros_usd: 25_000, fixed_cost_micros_usd: 0,
      cost_value_status: "known", price_evidence_status: "versioned", cost_estimated: false, tokens_estimated: false,
      started_at: "2026-09-04T00:00:00Z", completed_at: "2026-09-04T00:00:01Z", status: "success",
      latency_millis: 1000, retry_count: 0, fallback_count: 0,
    }], next_cursor: "" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><RunGovernancePage /></QueryClientProvider>);

    await screen.findByRole("option", { name: "Agent plane" });
    fireEvent.change(screen.getByRole("combobox", { name: "项目" }), { target: { value: "prj_a" } });
    await waitFor(() => expect(workUnits).toHaveBeenCalledWith(expect.stringContaining("project_id=prj_a")));
    await waitFor(() => expect(runs).toHaveBeenCalled());
    fireEvent.click(await screen.findByRole("button", { name: "查看调用" }));
    await waitFor(() => expect(usage).toHaveBeenCalled());
    expect(new URLSearchParams((usage.mock.calls[0][0] ?? "").slice(1)).get("run_id")).toBe("run_a");
    expect(await screen.findByText("req_a")).toBeVisible();
    expect(screen.getByText("可用")).toBeVisible();
    expect(screen.getByText(/已预留/)).toBeVisible();
  });

  it("renders complete cost evidence and creates an immutable Definition version", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [{ id: "prj_a", name: "Agent plane", revision: 7 }] as never, next_cursor: "" });
    vi.spyOn(api, "workUnits").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "runs").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "outcomes").mockResolvedValue({ items: [{
      id: "out_a", project_id: "prj_a", work_unit_id: "wku_a", definition_id: "odef_a", definition_version: 1,
      value: "accepted", reporter_key_id: "key_a", observed_at: "2026-09-04T00:00:00Z", ingested_at: "2026-09-04T00:00:01Z",
      revision: 1, governance_sequence: 4, provisional: false,
    }], next_cursor: "" });
    vi.spyOn(api, "outcomeDefinitions").mockResolvedValue({ items: [{
      id: "odef_a", project_id: "prj_a", name: "accepted", version: 1, data_type: "CATEGORICAL",
      allowed_values: ["accepted", "rejected"], success_values: ["accepted"], enabled: true,
      created_at: "2026-09-04T00:00:00Z", created_by: "admin", revision: 2,
    }], next_cursor: "" });
    vi.spyOn(api, "governanceSummary").mockResolvedValue({
      basis: "work_unit_cohort", cohort_start: "2026-09-01", cohort_end: "2026-09-30", definition_id: "odef_a", definition_version: 1,
      generated_at: "2026-09-04T01:00:00Z", accounting_watermark: { generation: 1, sequence: 20, offset: 400 }, governance_watermark: { sequence: 4, offset: 200 },
      eligible_units: 3, matured_units: 2, evaluated_units: 1, successful_units: 1, outcome_coverage: 1 / 3, success_rate: 1,
      known_cost_micros_usd: 2_000_000, in_progress_cost_micros_usd: 500_000, estimated_cost_micros_usd: 250_000,
      unknown_attempts: 3, outcome_completeness: "partial", outcome_reason: "missing_outcomes", cost_completeness: "partial", cost_per_success_micros_usd: 2_000_000,
      cost_per_success_reason: "unknown_costs_excluded",
    });
    const createVersion = vi.spyOn(api, "createOutcomeDefinitionVersion").mockResolvedValue({} as never);
    const createDefinition = vi.spyOn(api, "createOutcomeDefinition").mockResolvedValue({} as never);
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><RunGovernancePage /></QueryClientProvider>);

    await screen.findByRole("option", { name: "Agent plane" });
    fireEvent.change(screen.getByRole("combobox", { name: "项目" }), { target: { value: "prj_a" } });
    fireEvent.change(await screen.findByRole("combobox", { name: "结果定义" }), { target: { value: "odef_a:1" } });
    expect(await screen.findByText("已知模型费用")).toBeVisible();
    expect(screen.getByText("其中估算费用")).toBeVisible();
    expect(screen.getByText("费用未知调用")).toBeVisible();
    expect(screen.getByText("结果数据不完整")).toBeVisible();
    expect(screen.getByText(/Accounting 水位/)).toBeVisible();
    expect(screen.getByText(/Governance 水位/)).toBeVisible();
    expect(screen.getByText(/只包含已知费用/)).toBeVisible();

    fireEvent.click(screen.getByRole("button", { name: "禁用" }));
    await waitFor(() => expect(createVersion).toHaveBeenCalledWith("prj_a", "odef_a", 2, expect.objectContaining({
      data_type: "CATEGORICAL", allowed_values: ["accepted", "rejected"], enabled: false,
    })));
    createVersion.mockClear();

    fireEvent.click(screen.getByRole("button", { name: "新建版本" }));
    fireEvent.change(screen.getByRole("textbox", { name: "允许值" }), { target: { value: "accepted,rejected,unknown" } });
    fireEvent.click(screen.getByRole("button", { name: "保存新版本" }));
    await waitFor(() => expect(createVersion).toHaveBeenCalledWith("prj_a", "odef_a", 2, expect.objectContaining({
      data_type: "CATEGORICAL", allowed_values: ["accepted", "rejected", "unknown"], enabled: true,
    })));

    await screen.findByRole("button", { name: "创建定义" });
    fireEvent.change(screen.getByRole("textbox", { name: "定义名称" }), { target: { value: "quality_passed" } });
    fireEvent.change(screen.getByRole("combobox", { name: "类型" }), { target: { value: "BOOLEAN" } });
    fireEvent.click(screen.getByRole("button", { name: "创建定义" }));
    await waitFor(() => expect(createDefinition).toHaveBeenCalledWith("prj_a", 7, expect.objectContaining({
      name: "quality_passed", data_type: "BOOLEAN", allowed_values: [], success_values: ["true"], enabled: true,
    })));
  });

  it("shows governance unavailable as an error instead of zero business results", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [{ id: "prj_a", name: "Agent plane" }] as never, next_cursor: "" });
    vi.spyOn(api, "workUnits").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "runs").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "outcomes").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "outcomeDefinitions").mockResolvedValue({ items: [{
      id: "odef_a", project_id: "prj_a", name: "accepted", version: 1, data_type: "BOOLEAN",
      allowed_values: [], success_values: ["true"], enabled: true, created_at: "2026-09-04T00:00:00Z", created_by: "admin", revision: 1,
    }], next_cursor: "" });
    vi.spyOn(api, "governanceSummary").mockRejectedValue(new ApiError(503, "outcome governance is unavailable", "governance_unavailable"));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><RunGovernancePage /></QueryClientProvider>);

    await screen.findByRole("option", { name: "Agent plane" });
    fireEvent.change(screen.getByRole("combobox", { name: "项目" }), { target: { value: "prj_a" } });
    fireEvent.change(await screen.findByRole("combobox", { name: "结果定义" }), { target: { value: "odef_a:1" } });
    expect(await screen.findByText(/业务结果治理暂不可用/)).toBeVisible();
    expect(screen.queryByText("0.0%")).not.toBeInTheDocument();
  });
});
