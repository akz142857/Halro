import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import type { Deployment, Provider, Route } from "../types";
import { RoutesPage } from "./RoutesPage";

describe("RoutesPage", () => {
  afterEach(() => vi.restoreAllMocks());

  it("shows a route test result inside the route that was tested", async () => {
    const route = {
      id: "route_chat", public_model: "chat", deployment_id: "deployment_gpt",
      priority: 10, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "",
    } as Route;
    vi.spyOn(api, "routes").mockResolvedValue({ items: [route], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({
      items: [{ id: "deployment_gpt", name: "GPT", provider_id: "provider_openai", provider_model: "gpt-5.1" } as Deployment],
      next_cursor: "",
    });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [{ id: "provider_openai", name: "OpenAI" } as Provider], next_cursor: "" });
    vi.spyOn(api, "testRoute").mockResolvedValue({
      status: "healthy", latency_ms: 42, tested_at: "2026-08-02T12:00:00Z", revision: 2,
    });
    renderPage();

    const row = (await screen.findByText("chat")).closest("tr");
    expect(row).not.toBeNull();
    fireEvent.click(within(row!).getByRole("button", { name: "测试" }));

    await waitFor(() => expect(within(row!).getByRole("status")).toHaveTextContent("通过 · 42ms"));
    expect(screen.queryByText("通过 · 42ms")?.closest(".notice")).toBeNull();
  });

  it("restores a persisted route test result after loading the list", async () => {
    const route = {
      id: "route_chat", public_model: "chat", deployment_id: "deployment_gpt",
      priority: 10, strategy: "ordered", enabled: true, revision: 2,
      last_test_status: "healthy", last_test_revision: 2, last_test_latency_millis: 37,
      last_tested_at: "2026-08-02T12:00:00Z", created_at: "", updated_at: "",
    } as Route;
    vi.spyOn(api, "routes").mockResolvedValue({ items: [route], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [], next_cursor: "" });
    renderPage();

    expect(await screen.findByText("通过 · 37ms")).toBeInTheDocument();
  });
});

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><RoutesPage /></QueryClientProvider>);
}
