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

  // Whether the alias answers requests is the state the save commits, so it
  // belongs in the bar that commits it, saying what it will do. A stylesheet
  // regression that breaks this structure is invisible to a typecheck.
  it("states the routing state in the save bar rather than as a bare checkbox", async () => {
    const route = {
      id: "route_chat", public_model: "chat", deployment_id: "deployment_gpt",
      priority: 10, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "",
    } as Route;
    vi.spyOn(api, "routes").mockResolvedValue({ items: [route], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({
      items: [{ id: "deployment_gpt", name: "GPT", provider_id: "provider_openai", provider_model: "gpt-5.1", enabled: true } as Deployment],
      next_cursor: "",
    });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [{ id: "provider_openai", name: "OpenAI" } as Provider], next_cursor: "" });
    renderPage();

    const row = (await screen.findByText("chat")).closest("tr");
    fireEvent.click(within(row!).getByRole("button", { name: "编辑" }));

    const toggle = await screen.findByLabelText("启用模型路由");
    const bar = toggle.closest(".sticky-form-actions");
    expect(bar).not.toBeNull();
    expect(bar).toContainElement(screen.getByRole("button", { name: "保存并热加载" }));
    expect(bar).toContainElement(screen.getByRole("button", { name: "取消" }));
    expect(within(bar as HTMLElement).getByText("启用模型路由 · 启用")).toBeVisible();
    expect(within(bar as HTMLElement).getByText("应用可以用“chat”请求这个部署")).toBeVisible();

    // Turning it off has to change what the bar says it will do, not just the box.
    fireEvent.click(toggle);
    expect(within(bar as HTMLElement).getByText("启用模型路由 · 禁用")).toBeVisible();
    expect(within(bar as HTMLElement).getByText("应用请求这个别名会被拒绝")).toBeVisible();
  });

  // The route update is a full replacement. A toggle that sent only `enabled`
  // would blank the deployment the alias routes to, which the row cannot show.
  it("switches a route off from the list without dropping the rest of the route", async () => {
    vi.spyOn(api, "routes").mockResolvedValue({ items: [enabledRoute()], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [], next_cursor: "" });
    const update = vi.spyOn(api, "updateRoute").mockResolvedValue({ data: enabledRoute(), etag: "\"2\"" });
    renderPage();

    const row = (await screen.findByText("chat")).closest("tr");
    expect(within(row!).getByText("启用")).toHaveClass("resource-state", "enabled");
    fireEvent.click(within(row!).getByRole("button", { name: "禁用" }));

    // The last enabled route for an alias is the one whose removal stops the
    // alias answering, so the dialog has to say that rather than "are you sure".
    const consequence = await screen.findByText("确认禁用模型路由“chat”？这是该别名最后一条已启用路由，应用请求“chat”会被拒绝。");
    expect(consequence).toBeVisible();
    fireEvent.click(within(consequence.closest(".confirmation-dialog")!).getByRole("button", { name: "禁用" }));

    await waitFor(() => expect(update).toHaveBeenCalledWith(
      "route_chat",
      { public_model: "chat", deployment_id: "deployment_gpt", priority: 10, strategy: "ordered", enabled: false },
      1,
    ));
  });

  // Disabling one of several routes on an alias is a capacity change, not an
  // outage, and the dialog has to distinguish the two.
  it("counts the routes still serving the alias before switching one off", async () => {
    const sibling = { ...enabledRoute(), id: "route_chat_backup", deployment_id: "deployment_claude", priority: 20 };
    vi.spyOn(api, "routes").mockResolvedValue({ items: [enabledRoute(), sibling], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [], next_cursor: "" });
    renderPage();

    const row = (await screen.findAllByText("chat"))[0].closest("tr");
    fireEvent.click(within(row!).getByRole("button", { name: "禁用" }));

    expect(await screen.findByText("确认禁用模型路由“chat”？该别名还有 1 条已启用路由继续承接请求。")).toBeVisible();
  });

  it("offers a disabled route the way back on", async () => {
    vi.spyOn(api, "routes").mockResolvedValue({ items: [{ ...enabledRoute(), enabled: false }], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [], next_cursor: "" });
    const update = vi.spyOn(api, "updateRoute").mockResolvedValue({ data: enabledRoute(), etag: "\"2\"" });
    renderPage();

    const row = (await screen.findByText("chat")).closest("tr");
    expect(within(row!).getByText("禁用")).toHaveClass("resource-state");
    expect(within(row!).getByText("禁用")).not.toHaveClass("enabled");
    fireEvent.click(within(row!).getByRole("button", { name: "启用" }));

    await waitFor(() => expect(update).toHaveBeenCalledWith(
      "route_chat",
      expect.objectContaining({ enabled: true, deployment_id: "deployment_gpt" }),
      1,
    ));
  });
});

function enabledRoute(): Route {
  return {
    id: "route_chat", public_model: "chat", deployment_id: "deployment_gpt",
    priority: 10, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "",
  } as Route;
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><RoutesPage /></QueryClientProvider>);
}
