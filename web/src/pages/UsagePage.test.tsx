import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import { UsagePage, billedTierLabel } from "./UsagePage";

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

// A link that lands on an unfiltered list repeats the same defect one level down.
describe("UsagePage project deep link", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/usage?project_id=project_a");
  });

  it("initializes the project filter from the link it was sent to", async () => {
    const usage = vi.spyOn(api, "usage").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "projects").mockResolvedValue({ items: [{ id: "project_a", name: "Alpha" }] as never, next_cursor: "" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);

    await waitFor(() => expect(screen.getByRole("combobox", { name: "项目" })).toHaveValue("project_a"));
    await waitFor(() => expect(usage).toHaveBeenCalled());
    expect(new URLSearchParams((usage.mock.calls[0][0] ?? "").slice(1)).get("project_id")).toBe("project_a");
  });
});

// The model filter matches exactly, so the free-text box it replaces turned a
// typo into an empty table that looked like "no calls yet". The options have to
// cover more than the current routes: history outlives a route, and a select
// whose value is not among its options renders blank.
describe("UsagePage model filter", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/usage");
  });

  it("offers the route aliases and the models present in history, not a free-text box", async () => {
    vi.spyOn(api, "usage").mockResolvedValue({
      items: [{ event_id: "e1", request_id: "req_1", attempt: 1, project_id: "p", requested_model: "retired-alias",
        provider_model: "gpt-4o", provider_input_tokens: 1, provider_output_tokens: 1, latency_millis: 5,
        status: "success", completed_at: "2026-08-06T00:00:00Z" }] as never,
      next_cursor: "",
    });
    vi.spyOn(api, "projects").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "routes").mockResolvedValue({
      items: [{ id: "r1", public_model: "chat" }, { id: "r2", public_model: "embed" }] as never,
      next_cursor: "",
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);

    const select = await screen.findByRole("combobox", { name: "模型" });
    await waitFor(() => {
      const options = [...select.querySelectorAll("option")].map((option) => option.value);
      // "" is the all-models option; the alias only history knows about is kept.
      expect(options).toEqual(["", "chat", "embed", "retired-alias"]);
    });
  });
});

describe("billed price tier", () => {
  // The label is what makes two identically sized attempts with different costs
  // explainable, so it must name the rung and never silently render as blank.
  const label = (key: string, values?: Record<string, unknown>) => `${key}:${JSON.stringify(values ?? {})}`;

  it("names the window, the out-of-window rate, and the fail-closed fallback", () => {
    expect(billedTierLabel({ timezone: "Asia/Shanghai", source: "window", start_minute: 540, end_minute: 720, local_minute: 600 }, label))
      .toBe('usage.billedWindow:{"start":"09:00","end":"12:00","timezone":"Asia/Shanghai"}');
    expect(billedTierLabel({ timezone: "Asia/Shanghai", source: "base", local_minute: 780 }, label))
      .toBe('usage.billedBase:{"timezone":"Asia/Shanghai"}');
    expect(billedTierLabel({ timezone: "Asia/Shanghai", source: "zone_unavailable" }, label))
      .toBe('usage.billedZoneUnavailable:{"timezone":"Asia/Shanghai"}');
    // A window rung missing its bounds is a damaged record, not a reason to
    // claim an hour it cannot evidence.
    expect(billedTierLabel({ timezone: "Asia/Shanghai", source: "window" }, label))
      .toBe('usage.billedZoneUnavailable:{"timezone":"Asia/Shanghai"}');
  });
});
