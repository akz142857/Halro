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
