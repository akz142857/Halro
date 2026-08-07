import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import { UsagePage } from "./UsagePage";

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
