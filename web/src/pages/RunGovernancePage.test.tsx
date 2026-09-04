import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import { RunGovernancePage } from "./RunGovernancePage";

describe("RunGovernancePage", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/run-governance");
  });

  it("drills from a Project through Work Unit and Run to attributed attempts", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [{ id: "prj_a", name: "Agent plane" }] as never, next_cursor: "" });
    const workUnits = vi.spyOn(api, "workUnits").mockResolvedValue({ items: [{
      id: "wku_a", project_id: "prj_a", status: "open", created_by_key_id: "key_a",
      created_at: "2026-09-04T00:00:00Z", period_id: "prj_a:2026-09-04:UTC", period_timezone_version: 1,
    }], next_cursor: "" });
    const runs = vi.spyOn(api, "runs").mockResolvedValue({ items: [{
      id: "run_a", project_id: "prj_a", work_unit_id: "wku_a", budget_micros_usd: 5_000_000,
      committed_micros_usd: 125_000, reserved_micros_usd: 0, unknown_attempts: 0, status: "active",
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
  });
});
