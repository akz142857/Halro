import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api";
import { RunGovernancePage } from "./RunGovernancePage";
import { governanceGuideSteps } from "./run-governance/GovernanceFirstRunGuide";
import { CostEvidence } from "./run-governance/GovernancePrimitives";
import { isGovernanceDataStale, rememberGovernanceProject } from "./run-governance/governance-state";

describe("RunGovernancePage", () => {
  beforeEach(() => {
    vi.useRealTimers();
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/run-governance");
    rememberGovernanceProject("");
    vi.spyOn(api, "keys").mockResolvedValue({ items: [], next_cursor: "" });
  });

  it("guides the first governed business run with verified progress and executable API examples", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [{ id: "prj_a", name: "Agent plane", revision: 7, run_governance: { enabled: true } }] as never, next_cursor: "" });
    vi.spyOn(api, "workUnits").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "runs").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "outcomes").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "outcomeDefinitions").mockResolvedValue({ items: [{
      id: "odef_a", project_id: "prj_a", name: "accepted", version: 1, data_type: "CATEGORICAL",
      allowed_values: ["accepted", "rejected"], success_values: ["accepted"], enabled: true,
      created_at: "2026-09-04T00:00:00Z", created_by: "admin", revision: 2,
    }], next_cursor: "" });
    vi.mocked(api.keys).mockResolvedValue({ items: [
      { id: "key_orchestrator", project_id: "prj_a", name: "orchestrator", enabled: true, scopes: ["inference", "work_unit:create", "run:create", "run:attach"], created_at: "2026-09-04T00:00:00Z", revision: 1 },
      { id: "key_acceptance", project_id: "prj_a", name: "acceptance", enabled: true, scopes: ["outcome:write"], created_at: "2026-09-04T00:00:00Z", revision: 1 },
    ], next_cursor: "" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    render(<QueryClientProvider client={client}><RunGovernancePage /></QueryClientProvider>);

    expect(await screen.findByRole("heading", { name: "完成第一条运行治理闭环" })).toBeVisible();
    expect(screen.getByRole("progressbar", { name: "运行治理接入进度" })).toHaveAttribute("aria-valuenow", "3");
    expect(screen.getByText("业务系统 API 闭环")).toBeVisible();
    expect(screen.getAllByText(/odef_a/)).toHaveLength(2);
    expect(screen.getByText(/X-Halro-Run-ID: run_xxx/)).toBeVisible();
    expect(screen.getByText(/-d '\{\}'/)).toBeVisible();

    fireEvent.click(screen.getByRole("button", { name: "隐藏向导" }));
    expect(screen.queryByRole("heading", { name: "完成第一条运行治理闭环" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "接入向导" }));
    expect(screen.getByRole("heading", { name: "完成第一条运行治理闭环" })).toBeVisible();
  });

  it("treats legacy and expired keys as insufficient governance authority", () => {
    const steps = governanceGuideSteps({
      projectEnabled: true,
      definitions: [{ id: "odef_a", enabled: true }] as never,
      keys: [
        { id: "legacy", enabled: true },
        { id: "expired", enabled: true, scopes: ["inference", "work_unit:create", "run:create", "run:attach", "outcome:write"], expires_at: "2026-09-05T00:00:00Z" },
      ] as never,
      workUnits: [], runs: [], outcomes: [], now: Date.parse("2026-09-06T00:00:00Z"),
    });

    expect(steps.map((step) => step.state)).toEqual(["complete", "complete", "current", "blocked", "blocked", "blocked"]);
  });

  it("takes the Definition step directly to Definition management", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [{ id: "prj_a", name: "Agent plane", revision: 7, run_governance: { enabled: true } }] as never, next_cursor: "" });
    vi.spyOn(api, "workUnits").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "runs").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "outcomes").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "outcomeDefinitions").mockResolvedValue({ items: [], next_cursor: "" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><RunGovernancePage /></QueryClientProvider>);

    fireEvent.click(await screen.findByRole("button", { name: "创建结果定义" }));

    expect(screen.getByRole("tab", { name: "结果与口径" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("tab", { name: "Definition 管理" })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("button", { name: "创建定义" })).toBeVisible();
  });

  it("retires the automatic guide after the first complete lifecycle and keeps it available on demand", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [{ id: "prj_a", name: "Agent plane", revision: 7, run_governance: { enabled: true } }] as never, next_cursor: "" });
    vi.spyOn(api, "outcomeDefinitions").mockResolvedValue({ items: [{ id: "odef_a", enabled: true, version: 1, success_values: ["accepted"] }] as never, next_cursor: "" });
    vi.mocked(api.keys).mockResolvedValue({ items: [{ id: "key_all", enabled: true, scopes: ["inference", "work_unit:create", "run:create", "run:attach", "outcome:write"] }] as never, next_cursor: "" });
    vi.spyOn(api, "workUnits").mockResolvedValue({ items: [{ id: "wku_a", status: "closed", created_at: "2026-09-06T00:00:00Z", period_id: "2026-09-06" }] as never, next_cursor: "" });
    vi.spyOn(api, "runs").mockResolvedValue({ items: [{ id: "run_a", work_unit_id: "wku_a", status: "closed", budget_state: "available" }] as never, next_cursor: "" });
    vi.spyOn(api, "outcomes").mockResolvedValue({ items: [{ id: "out_a", work_unit_id: "wku_a", definition_id: "odef_a", definition_version: 1, value: "accepted", provisional: false }] as never, next_cursor: "" });
    vi.spyOn(api, "governanceSummary").mockRejectedValue(new Error("not needed on overview"));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    render(<QueryClientProvider client={client}><RunGovernancePage /></QueryClientProvider>);

    await screen.findByLabelText("当前治理概况");
    expect(screen.queryByRole("heading", { name: "完成第一条运行治理闭环" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "接入向导" }));
    expect(screen.getByRole("progressbar", { name: "运行治理接入进度" })).toHaveAttribute("aria-valuenow", "6");
    expect(screen.getByText("第一条治理闭环已建立")).toBeVisible();
  });

  it("drills from a Project through Work Unit and Run to attributed attempts", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [{ id: "prj_a", name: "Agent plane", revision: 7, run_governance: { enabled: true } }] as never, next_cursor: "" });
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
    const usage = vi.spyOn(api, "usageAll").mockResolvedValue({ items: [{
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
    await waitFor(() => expect(workUnits).toHaveBeenCalledWith(expect.stringContaining("project_id=prj_a")));
    expect(await screen.findByText("1 个 Run")).toBeVisible();
    expect(screen.getByText("当前无需处理")).toBeVisible();
    fireEvent.click(screen.getByRole("tab", { name: "Work Units" }));
    await waitFor(() => expect(runs).toHaveBeenCalled());
    fireEvent.click(await screen.findByRole("button", { name: "查看详情" }));
    expect(screen.getByRole("heading", { name: "生命周期" })).toBeVisible();
    expect(screen.getByText(/从创建到关闭的关键节点/)).toBeVisible();
    expect(screen.getByRole("heading", { name: "业务结果" })).toBeVisible();
    fireEvent.click(await screen.findByRole("button", { name: "查看调用" }));
    await waitFor(() => expect(usage).toHaveBeenCalled());
    expect(new URLSearchParams((usage.mock.calls[0][0] ?? "").slice(1)).get("run_id")).toBe("run_a");
    expect(await screen.findByText("req_a")).toBeVisible();
    expect(screen.getByText("请求模型")).toBeVisible();
    expect(screen.getByText("实际模型")).toBeVisible();
    expect(screen.getByLabelText("Token 构成")).toHaveTextContent("输入 Token10");
    expect(screen.getByLabelText("Token 构成")).toHaveTextContent("输出 Token2");
    expect(screen.getByText("1 次调用")).toBeVisible();
    expect(screen.getAllByText("可用").length).toBeGreaterThan(0);
    expect(screen.getByText(/已归集 US\$0\.13 · 已预留 US\$0\.03/)).toBeVisible();
  });

  it("renders complete cost evidence and creates an immutable Definition version", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [{ id: "prj_a", name: "Agent plane", revision: 7, run_governance: { enabled: true } }] as never, next_cursor: "" });
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
    fireEvent.click(await screen.findByRole("tab", { name: "结果与口径" }));
    await screen.findByRole("combobox", { name: "Definition 版本" });
    expect(await screen.findByText("已知模型费用")).toBeVisible();
    expect(screen.getByText("其中估算费用")).toBeVisible();
    expect(screen.getByText("费用未知调用")).toBeVisible();
    expect(screen.getByText("结果数据不完整")).toBeVisible();
    fireEvent.click(screen.getByText("数据与审计详情"));
    expect(screen.getByText(/Accounting 水位/)).toBeVisible();
    expect(screen.getByText(/Governance 水位/)).toBeVisible();
    expect(screen.getByText(/只包含已知费用/)).toBeVisible();

    fireEvent.click(screen.getByRole("tab", { name: "Definition 管理" }));
    fireEvent.click(screen.getAllByRole("button", { name: "禁用" }).at(-1)!);
    fireEvent.click(screen.getAllByRole("button", { name: "禁用" }).at(-1)!);
    await waitFor(() => expect(createVersion).toHaveBeenCalledWith("prj_a", "odef_a", 2, expect.objectContaining({
      data_type: "CATEGORICAL", allowed_values: ["accepted", "rejected"], enabled: false,
    })));
    createVersion.mockClear();

    fireEvent.click(screen.getByRole("button", { name: "新建版本" }));
    expect(document.activeElement).toBe(screen.getByRole("combobox", { name: "类型" }));
    fireEvent.change(screen.getByRole("textbox", { name: "向允许值添加值" }), { target: { value: "unknown" } });
    fireEvent.click(screen.getAllByRole("button", { name: "添加" })[0]);
    fireEvent.click(screen.getByRole("button", { name: "保存新版本" }));
    await waitFor(() => expect(createVersion).toHaveBeenCalledWith("prj_a", "odef_a", 2, expect.objectContaining({
      data_type: "CATEGORICAL", allowed_values: ["accepted", "rejected", "unknown"], enabled: true,
    })));

    fireEvent.click(await screen.findByRole("button", { name: "创建定义" }));
    fireEvent.change(screen.getByRole("textbox", { name: "定义名称" }), { target: { value: "quality_passed" } });
    fireEvent.change(screen.getByRole("combobox", { name: "类型" }), { target: { value: "BOOLEAN" } });
    fireEvent.click(screen.getAllByRole("button", { name: "创建定义" }).at(-1)!);
    await waitFor(() => expect(createDefinition).toHaveBeenCalledWith("prj_a", 7, expect.objectContaining({
      name: "quality_passed", data_type: "BOOLEAN", allowed_values: [], success_values: ["true"], enabled: true,
    })));
  });

  it("shows governance unavailable as an error instead of zero business results", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [{ id: "prj_a", name: "Agent plane", revision: 1, run_governance: { enabled: true } }] as never, next_cursor: "" });
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
    fireEvent.click(await screen.findByRole("tab", { name: "结果与口径" }));
    expect(await screen.findByText(/业务结果治理暂不可用/)).toBeVisible();
    expect(screen.queryByText("0.0%")).not.toBeInTheDocument();
  });

  it("does not guess a project when more than one governed project is available", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [
      { id: "prj_a", name: "Alpha", run_governance: { enabled: true } },
      { id: "prj_b", name: "Beta", run_governance: { enabled: true } },
    ] as never, next_cursor: "" });
    const workUnits = vi.spyOn(api, "workUnits");
    const runs = vi.spyOn(api, "runs");
    vi.spyOn(api, "outcomeDefinitions");
    vi.spyOn(api, "outcomes");
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    render(<QueryClientProvider client={client}><RunGovernancePage /></QueryClientProvider>);

    expect(await screen.findByRole("heading", { name: "选择项目" })).toBeVisible();
    expect(workUnits).not.toHaveBeenCalled();
    expect(runs).not.toHaveBeenCalled();
    expect(window.location.search).toBe("");
  });

  it("restores valid URL context and updates it during Work Unit drill-down", async () => {
    window.history.replaceState({}, "", "/admin/run-governance?project_id=prj_a&view=work-units");
    vi.spyOn(api, "projects").mockResolvedValue({ items: [{ id: "prj_a", name: "Agent plane", run_governance: { enabled: true } }] as never, next_cursor: "" });
    vi.spyOn(api, "outcomeDefinitions").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "outcomes").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "workUnits").mockResolvedValue({ items: [{ id: "wku_url", project_id: "prj_a", status: "closed", created_by_key_id: "key_a", created_at: "2026-09-04T00:00:00Z", period_id: "p1", period_timezone_version: 1 }], next_cursor: "" });
    vi.spyOn(api, "runs").mockResolvedValue({ items: [], next_cursor: "" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    render(<QueryClientProvider client={client}><RunGovernancePage /></QueryClientProvider>);

    expect(await screen.findByRole("tab", { name: "Work Units" })).toHaveAttribute("aria-selected", "true");
    fireEvent.click(await screen.findByRole("button", { name: "查看详情" }));
    await waitFor(() => expect(new URLSearchParams(window.location.search).get("work_unit_id")).toBe("wku_url"));
    expect(screen.getByRole("dialog", { name: /wku_url/ })).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "关闭" }));
    await waitFor(() => expect(new URLSearchParams(window.location.search).get("work_unit_id")).toBeNull());
  });

  it("distinguishes free, known-zero, estimated, and unknown cost evidence", () => {
    const base = { cost_micros_usd: 0, cost_value_status: "known", cost_estimated: false, lease_mode: "metered" };
    render(<div>
      <CostEvidence attempt={{ ...base, lease_mode: "free" } as never} />
      <CostEvidence attempt={{ ...base, lease_mode: "metered" } as never} />
      <CostEvidence attempt={{ ...base, cost_estimated: true } as never} />
      <CostEvidence attempt={{ ...base, cost_micros_usd: null, cost_value_status: "unknown" } as never} />
    </div>);

    expect(screen.getByText("免费价格版本")).toBeVisible();
    expect(screen.getByText("含估算")).toBeVisible();
    expect(screen.getByText("未知")).toBeVisible();
    expect(screen.getAllByText("US$0.00")).toHaveLength(3);
  });

  it("evaluates final Outcomes for every frozen Definition", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [{ id: "prj_a", name: "Agent plane", run_governance: { enabled: true } }] as never, next_cursor: "" });
    vi.spyOn(api, "outcomeDefinitions").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "runs").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "workUnits").mockResolvedValue({ items: [{ id: "wku_multi", project_id: "prj_a", status: "closed", created_by_key_id: "key_a", created_at: "2026-09-06T17:00:00Z", closed_at: "2026-09-07T00:00:00Z", period_id: "2026-09-07", period_timezone_version: 1, outcome_definitions: [{ id: "odef_a", version: 1 }, { id: "odef_b", version: 2 }] }], next_cursor: "" } as never);
    vi.spyOn(api, "outcomes").mockResolvedValue({ items: [{ id: "out_a", project_id: "prj_a", work_unit_id: "wku_multi", definition_id: "odef_a", definition_version: 1, value: "accepted", reporter_key_id: "key_a", observed_at: "2026-09-07T00:00:00Z", ingested_at: "2026-09-07T00:00:01Z", revision: 9, governance_sequence: 10, provisional: false }], next_cursor: "" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><RunGovernancePage /></QueryClientProvider>);

    const metric = await screen.findByRole("button", { name: /已关闭但缺少必需的最终 Outcome/ });
    expect(metric).toHaveTextContent("1");
    fireEvent.click(metric);
    expect(await screen.findByText("1 / 2 个最终结果")).toBeVisible();
  });

  it("revalidates the selected Definition when browser history changes projects", async () => {
    window.history.replaceState({}, "", "/admin/run-governance?project_id=prj_a&view=results");
    vi.spyOn(api, "projects").mockResolvedValue({ items: [
      { id: "prj_a", name: "Alpha", revision: 1, run_governance: { enabled: true } },
      { id: "prj_b", name: "Beta", revision: 1, run_governance: { enabled: true } },
    ] as never, next_cursor: "" });
    vi.spyOn(api, "workUnits").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "runs").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "outcomes").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "outcomeDefinitions").mockImplementation(async (projectID) => ({ items: [{ id: `odef_${projectID}`, project_id: projectID, name: `Definition ${projectID}`, version: 1, data_type: "CATEGORICAL", allowed_values: ["yes", "no"], success_values: ["yes"], enabled: true, created_at: "2026-09-01T00:00:00Z", created_by: "admin", revision: 1 }], next_cursor: "" }));
    vi.spyOn(api, "governanceSummary").mockResolvedValue({ basis: "work_unit_cohort", cohort_start: "2026-09-01", cohort_end: "2026-09-30", definition_id: "", definition_version: 1, generated_at: "2026-09-06T00:00:00Z", accounting_watermark: { generation: 1, sequence: 1, offset: 1 }, governance_watermark: { sequence: 1, offset: 1 }, eligible_units: 0, matured_units: 0, evaluated_units: 0, successful_units: 0, outcome_coverage: null, success_rate: null, known_cost_micros_usd: 0, in_progress_cost_micros_usd: 0, estimated_cost_micros_usd: 0, unknown_attempts: 0, outcome_completeness: "unknown", outcome_reason: "no_eligible_units", cost_completeness: "complete", cost_per_success_micros_usd: null, cost_per_success_reason: "no_successful_units" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><RunGovernancePage /></QueryClientProvider>);

    const project = await screen.findByRole("combobox", { name: "当前项目" });
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Definition 版本" })).toHaveValue("odef_prj_a:1"));
    fireEvent.change(project, { target: { value: "prj_b" } });
    window.history.back();
    window.dispatchEvent(new PopStateEvent("popstate"));
    await waitFor(() => expect(screen.getByRole("combobox", { name: "当前项目" })).toHaveValue("prj_a"));
    await waitFor(() => expect(screen.getByRole("combobox", { name: "Definition 版本" })).toHaveValue("odef_prj_a:1"));
  });

  it("refreshes a provisional Outcome into its final state and marks old snapshots stale", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [{ id: "prj_a", name: "Agent plane", run_governance: { enabled: true } }] as never, next_cursor: "" });
    vi.spyOn(api, "outcomeDefinitions").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "runs").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "workUnits").mockResolvedValue({ items: [{ id: "wku_a", project_id: "prj_a", status: "closed", created_by_key_id: "key_a", created_at: "2026-09-06T00:00:00Z", period_id: "2026-09-06", period_timezone_version: 1, outcome_definitions: [{ id: "odef_a", version: 1 }] }], next_cursor: "" } as never);
    const provisional = { id: "out_a", project_id: "prj_a", work_unit_id: "wku_a", definition_id: "odef_a", definition_version: 1, value: "accepted", reporter_key_id: "key_a", observed_at: "2026-09-06T00:00:00Z", ingested_at: "2026-09-06T00:00:01Z", revision: 1, governance_sequence: 1, provisional: true };
    const outcomes = vi.spyOn(api, "outcomes").mockResolvedValueOnce({ items: [provisional], next_cursor: "" }).mockResolvedValue({ items: [{ ...provisional, governance_sequence: 2, provisional: false }], next_cursor: "" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><RunGovernancePage /></QueryClientProvider>);

    fireEvent.click(await screen.findByRole("tab", { name: "Work Units" }));
    expect(await screen.findByText("0 / 1 个最终结果")).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "刷新" }));
    await waitFor(() => expect(outcomes).toHaveBeenCalledTimes(2));
    expect(await screen.findByText("1 / 1 个最终结果")).toBeVisible();

    expect(isGovernanceDataStale(Date.now() - 75_000, Date.now(), false)).toBe(true);
    expect(isGovernanceDataStale(Date.now(), Date.now(), true)).toBe(true);
  });

  it("does not auto-select an enabled historical version after its latest version is disabled", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [{ id: "prj_a", name: "Agent plane", run_governance: { enabled: true } }] as never, next_cursor: "" });
    vi.spyOn(api, "workUnits").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "runs").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "outcomes").mockResolvedValue({ items: [], next_cursor: "" });
    const base = { id: "odef_a", project_id: "prj_a", name: "accepted", data_type: "CATEGORICAL", allowed_values: ["accepted", "rejected"], success_values: ["accepted"], created_at: "2026-09-04T00:00:00Z", created_by: "admin", revision: 1 };
    vi.spyOn(api, "outcomeDefinitions").mockResolvedValue({ items: [{ ...base, version: 1, enabled: true }, { ...base, version: 2, enabled: false }], next_cursor: "" } as never);
    const summary = vi.spyOn(api, "governanceSummary");
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><RunGovernancePage /></QueryClientProvider>);

    fireEvent.click(await screen.findByRole("tab", { name: "结果与口径" }));
    expect(await screen.findByRole("combobox", { name: "Definition 版本" })).toHaveValue("");
    expect(summary).not.toHaveBeenCalled();
  });
});
