import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import { UsagePage } from "./UsagePage";
import type { UsageAttempt } from "../types";

function attempt(): UsageAttempt {
  return {
    event_id: "event_1", request_id: "req_1", attempt_id: "attempt_1", attempt: 1,
    provider_input_tokens: 18, provider_output_tokens: 32,
    original_cost_micros_usd: 200, adjustment_delta_micros_usd: 0, final_cost_micros_usd: 200,
    input_cost_micros_usd: 120, output_cost_micros_usd: 80, fixed_cost_micros_usd: 0,
    price_evidence_status: "versioned", completed_at: "2026-08-04T09:57:00Z", status: "success",
    latency_millis: 1996,
  } as UsageAttempt;
}

describe("UsagePage request correlation", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/usage?request_id=req_debug_1");
  });

  it("initializes the exact Request ID filter from the developer workbench link", async () => {
    const usage = vi.spyOn(api, "usage").mockResolvedValue({ items: [], next_cursor: "" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);

    expect(screen.getByRole("textbox", { name: "Request ID" })).toHaveValue("req_debug_1");
    await waitFor(() => expect(usage).toHaveBeenCalled());
    const query = usage.mock.calls[0][0] ?? "";
    expect(new URLSearchParams(query.slice(1)).get("request_id")).toBe("req_debug_1");
  });
});

// The far end of the Overview's adjustment-over-budget signal. A link that lands on
// an unfiltered list repeats the same defect one level down.
describe("UsagePage project deep link", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/usage?project_id=project_a");
  });

  it("filters to the project it was sent to and says so", async () => {
    const usage = vi.spyOn(api, "usage").mockResolvedValue({ items: [], next_cursor: "" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);

    await waitFor(() => expect(usage).toHaveBeenCalled());
    expect(new URLSearchParams((usage.mock.calls[0][0] ?? "").slice(1)).get("project_id")).toBe("project_a");

    const chip = screen.getByRole("button", { name: /项目 project_a/ });
    fireEvent.click(chip);

    await waitFor(() => expect(usage.mock.calls.length).toBeGreaterThan(1));
    const latest = usage.mock.calls[usage.mock.calls.length - 1][0] ?? "";
    expect(new URLSearchParams(latest.slice(1)).get("project_id")).toBeNull();
    // The URL follows, so a reload does not silently restore the filter.
    expect(window.location.search).toBe("");
  });
});

// Correcting one attempt's cost is the exception path, not a routine one. It belongs
// behind the pricing evidence disclosure so the list reads as the log it is; a button
// sitting in every row made the table look editable.
describe("UsagePage cost correction", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/usage");
  });

  function renderPage() {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);
  }

  it("keeps the correction behind the pricing evidence disclosure", async () => {
    vi.spyOn(api, "usage").mockResolvedValue({ items: [attempt()], next_cursor: "" });
    vi.spyOn(api, "usageAttemptAdjustments").mockResolvedValue([]);
    renderPage();

    const adjust = await screen.findByRole("button", { name: "调整成本" });
    const disclosure = adjust.closest("details");
    expect(disclosure).not.toBeNull();
    expect(disclosure!.open).toBe(false);
    expect(disclosure!.querySelector("summary")?.textContent).toBe("计价证据");
  });

  // One query per attempt the operator actually opens, not one per row rendered:
  // a page holds 100 attempts and almost none of them are ever inspected.
  it("reads the adjustment history only once the disclosure is opened", async () => {
    vi.spyOn(api, "usage").mockResolvedValue({ items: [attempt()], next_cursor: "" });
    const history = vi.spyOn(api, "usageAttemptAdjustments").mockResolvedValue([
      {
        event_id: "adjustment_1", adjustment_sequence: 1, delta_micros_usd: -50,
        net_cost_after_micros_usd: 150, posted_at: "2026-08-05T10:00:00Z",
        reason_code: "invoice_difference", reason: "服务商月账单差额", created_by: "admin",
      },
    ]);
    renderPage();

    const adjust = await screen.findByRole("button", { name: "调整成本" });
    expect(history).not.toHaveBeenCalled();

    const disclosure = adjust.closest("details")!;
    disclosure.open = true;
    fireEvent(disclosure, new Event("toggle"));

    await waitFor(() => expect(history).toHaveBeenCalledWith("attempt_1"));
    expect(await screen.findByText(/服务商月账单差额/)).toBeInTheDocument();
  });
});
