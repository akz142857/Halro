import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api";
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

    const row = (await screen.findByText("route_chat")).closest("tr");
    expect(row).not.toBeNull();
    fireEvent.click(within(row!).getByRole("button", { name: "测试" }));

    await waitFor(() => expect(within(row!).getByRole("status")).toHaveTextContent("通过 · 42ms"));
    expect(screen.queryByText("通过 · 42ms")?.closest(".notice")).toBeNull();
  });

  // The status column read `enabled`, which is what the operator asked for
  // rather than what the gateway is doing. A route the registry refused was
  // shown as Enabled while it served nothing.
  it("shows a withheld route as withheld rather than enabled, and says why", async () => {
    const route = {
      id: "route_chat", public_model: "chat", deployment_id: "deployment_gpt",
      priority: 10, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "",
      withheld: { kind: "reference", reason: "deployment_unavailable" },
    } as Route;
    vi.spyOn(api, "routes").mockResolvedValue({ items: [route], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [], next_cursor: "" });
    renderPage();

    const row = (await screen.findByText("route_chat")).closest("tr")!;
    expect(within(row).getByText("已扣留")).toBeVisible();
    expect(within(row).getByText("该路由指向的模型部署已停用或已删除。")).toBeVisible();
    expect(within(row).queryByText("已启用")).not.toBeInTheDocument();
  });

  // A reason class this console has no copy for must still say the route is not
  // routing; falling back to the raw class would be better than the old lie, and
  // saying nothing would put the lie back.
  it("still marks a route withheld for a reason it cannot name", async () => {
    const route = {
      id: "route_chat", public_model: "chat", deployment_id: "deployment_gpt",
      priority: 10, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "",
      withheld: { kind: "reference", reason: "something_this_build_predates" },
    } as unknown as Route;
    vi.spyOn(api, "routes").mockResolvedValue({ items: [route], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [], next_cursor: "" });
    renderPage();

    const row = (await screen.findByText("route_chat")).closest("tr")!;
    expect(within(row).getByText("已扣留")).toBeVisible();
    expect(within(row).getByText(/原因未知/)).toBeVisible();
  });

  // The list is stored sorted by route ID, so two routes on one alias landed
  // wherever their random IDs put them — with an unrelated alias in between and
  // nothing saying the two rows were one failover chain.
  it("groups the rows by alias and orders them the way the engine tries them", async () => {
    const routes = [
      { id: "rte_aaa", public_model: "chat", deployment_id: "deployment_gpt", priority: 20, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "" },
      { id: "rte_bbb", public_model: "zeta", deployment_id: "deployment_gpt", priority: 10, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "" },
      { id: "rte_ccc", public_model: "chat", deployment_id: "deployment_azure", priority: 8, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "" },
    ] as Route[];
    vi.spyOn(api, "routes").mockResolvedValue({ items: routes, next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [
      { id: "deployment_gpt", name: "GPT", provider_id: "p", provider_model: "m", enabled: true },
      { id: "deployment_azure", name: "Azure", provider_id: "p", provider_model: "m", enabled: true },
    ] as Deployment[], next_cursor: "" });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [], next_cursor: "" });
    renderPage();

    await screen.findByText("rte_aaa");
    // Alias order, and within the alias the engine's own order: priority
    // ascending, so the store's route-ID order (aaa, bbb, ccc) is not it.
    const ids = Array.from(document.querySelectorAll("tr[id^='route-']"), (row) => row.id);
    expect(ids).toEqual(["route-rte_ccc", "route-rte_aaa", "route-rte_bbb"]);
    // And the two chat rows are under one heading, with zeta's own heading after.
    const headings = Array.from(document.querySelectorAll(".route-group-heading strong"), (cell) => cell.textContent);
    expect(headings).toEqual(["chat", "zeta"]);
    expect(screen.getByText("2 个目标 · 顺序回退")).toBeVisible();
    expect(screen.getByText("单一目标 · 无回退")).toBeVisible();
  });

  // Priority decides which target is tried first, and nothing on the page said
  // which one that was — an operator who typed 8 instead of 20 silently made
  // the new route primary.
  it("marks the primary target under ordered failover and not under round robin", async () => {
    const ordered = [
      { id: "rte_first", public_model: "chat", deployment_id: "deployment_gpt", priority: 8, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "" },
      { id: "rte_second", public_model: "chat", deployment_id: "deployment_azure", priority: 10, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "" },
    ] as Route[];
    vi.spyOn(api, "routes").mockResolvedValue({ items: ordered, next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [
      { id: "deployment_gpt", name: "GPT", provider_id: "p", provider_model: "m", enabled: true },
      { id: "deployment_azure", name: "Azure", provider_id: "p", provider_model: "m", enabled: true },
    ] as Deployment[], next_cursor: "" });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [], next_cursor: "" });
    const page = renderPage();

    await screen.findByText("rte_first");
    expect(within(document.getElementById("route-rte_first")!).getByText("主")).toBeVisible();
    expect(within(document.getElementById("route-rte_second")!).queryByText("主")).toBeNull();

    // Round robin rotates the starting point every request, so no row is the
    // one tried first and saying otherwise would name a precedence that is not
    // there.
    page.unmount();
    vi.mocked(api.routes).mockResolvedValue({
      items: ordered.map((route) => ({ ...route, strategy: "round_robin" })) as Route[], next_cursor: "",
    });
    renderPage();

    await screen.findByText("rte_first");
    expect(screen.queryByText("主")).toBeNull();
  });

  // The engine reads only the highest-priority target's strategy. The others
  // are stored, editable, and have no effect — which the page has to say, or
  // an operator changes one and waits for a behaviour that never arrives.
  it("warns when the effective candidates disagree on strategy", async () => {
    vi.spyOn(api, "routes").mockResolvedValue({ items: [
      { id: "rte_first", public_model: "chat", deployment_id: "deployment_gpt", priority: 8, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "" },
      { id: "rte_second", public_model: "chat", deployment_id: "deployment_azure", priority: 10, strategy: "round_robin", enabled: true, revision: 1, created_at: "", updated_at: "" },
    ] as Route[], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [
      { id: "deployment_gpt", name: "GPT", provider_id: "p", provider_model: "m", enabled: true },
      { id: "deployment_azure", name: "Azure", provider_id: "p", provider_model: "m", enabled: true },
    ] as Deployment[], next_cursor: "" });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [], next_cursor: "" });
    renderPage();

    expect(await screen.findByText("策略不一致")).toBeVisible();
    // The heading states the one in force, which is the first target's.
    expect(screen.getByText("2 个目标 · 顺序回退")).toBeVisible();
  });

  // The group summary derives from the deployments read, so a failed one must
  // not be reported as "no usable target" — the same rule the project form got.
  it("does not report a failed deployments read as a group with no target", async () => {
    vi.spyOn(api, "routes").mockResolvedValue({ items: [
      { id: "route_chat", public_model: "chat", deployment_id: "deployment_gpt", priority: 10, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "" },
    ] as Route[], next_cursor: "" });
    vi.spyOn(api, "deployments").mockRejectedValue(new ApiError(500, "request failed (500)", "", ""));
    vi.spyOn(api, "providers").mockResolvedValue({ items: [], next_cursor: "" });
    renderPage();

    expect(await screen.findByText("目标状态读取失败")).toBeVisible();
    expect(screen.queryByText("没有可用目标")).toBeNull();
  });

  // The confirmation counted enabled rows, and a withheld row is enabled and
  // serving nothing. It therefore told the operator another route would keep
  // answering while this same page showed that route as withheld, and the alias
  // went dark on the next request.
  it("does not count a withheld sibling as one that keeps serving", async () => {
    const serving = {
      id: "route_serving", public_model: "chat", deployment_id: "deployment_gpt",
      priority: 10, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "",
    } as Route;
    const withheld = {
      id: "route_withheld", public_model: "chat", deployment_id: "deployment_azure",
      priority: 20, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "",
      withheld: { kind: "capability_drift", reason: "profile_narrowed" },
    } as Route;
    vi.spyOn(api, "routes").mockResolvedValue({ items: [serving, withheld], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [], next_cursor: "" });
    renderPage();

    await screen.findAllByText("chat");
    const row = document.getElementById("route-route_serving")!;
    fireEvent.click(within(row).getByRole("button", { name: "禁用" }));

    expect(await screen.findByText(
      "确认禁用模型路由“chat”？这是该别名最后一条已启用路由，应用请求“chat”会被拒绝。",
    )).toBeVisible();
  });

  // And the arithmetic still has to be right from the withheld row's own side:
  // disabling a route that was never serving does not take the alias down when
  // a healthy sibling is still answering.
  it("does not claim the alias goes dark when the withheld route is the one being disabled", async () => {
    const serving = {
      id: "route_serving", public_model: "chat", deployment_id: "deployment_gpt",
      priority: 10, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "",
    } as Route;
    const withheld = {
      id: "route_withheld", public_model: "chat", deployment_id: "deployment_azure",
      priority: 20, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "",
      withheld: { kind: "reference", reason: "binding_unavailable" },
    } as Route;
    vi.spyOn(api, "routes").mockResolvedValue({ items: [serving, withheld], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [], next_cursor: "" });
    renderPage();

    await screen.findAllByText("chat");
    const row = document.getElementById("route-route_withheld")!;
    fireEvent.click(within(row).getByRole("button", { name: "禁用" }));

    expect(await screen.findByText(
      "确认禁用模型路由“chat”？该别名还有 1 条路由继续承接请求。",
    )).toBeVisible();
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
  // A failed route test showed a red word and dropped the class and upstream
  // reply the response carried, leaving the operator with nothing to act on.
  it("says why a route test failed", async () => {
    const route = {
      id: "route_chat", public_model: "chat", deployment_id: "deployment_gpt",
      priority: 10, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "",
    } as Route;
    vi.spyOn(api, "routes").mockResolvedValue({ items: [route], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "testRoute").mockRejectedValue(new ApiError(502, "request failed (502)", "", "", {
      status: "unhealthy", error_class: "rate_limit", provider_status: 429,
      provider_code: "rate_limit_exceeded", error_detail: "provider error (429): too many requests",
    }));
    renderPage();

    const row = (await screen.findByText("route_chat")).closest("tr")!;
    fireEvent.click(within(row).getByRole("button", { name: "测试" }));

    const reason = await within(row).findByText(/上游限流/);
    expect(reason).toHaveTextContent("HTTP 429");
    expect(reason).toHaveTextContent("too many requests");
  });

  it("explains a route failure the record remembers, without a test in this session", async () => {
    const route = {
      id: "route_chat", public_model: "chat", deployment_id: "deployment_gpt",
      priority: 10, strategy: "ordered", enabled: true, revision: 2,
      last_test_status: "unhealthy", last_test_revision: 2, last_test_error_class: "timeout",
      created_at: "", updated_at: "",
    } as Route;
    vi.spyOn(api, "routes").mockResolvedValue({ items: [route], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [], next_cursor: "" });
    renderPage();

    expect(await screen.findByText(/等待上游响应超时/)).toBeVisible();
  });

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

    const row = (await screen.findByText("route_chat")).closest("tr");
    fireEvent.click(within(row!).getByRole("button", { name: "编辑" }));

    const toggle = await screen.findByLabelText("启用模型路由");
    const bar = toggle.closest(".sticky-form-actions");
    expect(bar).not.toBeNull();
    expect(bar).toContainElement(screen.getByRole("button", { name: "保存并热加载" }));
    expect(bar).toContainElement(screen.getByRole("button", { name: "取消" }));
    expect(within(bar as HTMLElement).getByText("启用模型路由 · 已启用")).toBeVisible();
    expect(within(bar as HTMLElement).getByText("应用可以用“chat”请求这个部署")).toBeVisible();

    // Turning it off has to change what the bar says it will do, not just the box.
    fireEvent.click(toggle);
    expect(within(bar as HTMLElement).getByText("启用模型路由 · 已禁用")).toBeVisible();
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

    const row = (await screen.findByText("route_chat")).closest("tr");
    expect(within(row!).getByText("已启用")).toHaveClass("resource-state", "enabled");
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

    const row = (await screen.findByText("route_chat")).closest("tr");
    fireEvent.click(within(row!).getByRole("button", { name: "禁用" }));

    expect(await screen.findByText("确认禁用模型路由“chat”？该别名还有 1 条路由继续承接请求。")).toBeVisible();
  });

  it("offers a disabled route the way back on", async () => {
    vi.spyOn(api, "routes").mockResolvedValue({ items: [{ ...enabledRoute(), enabled: false }], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [], next_cursor: "" });
    const update = vi.spyOn(api, "updateRoute").mockResolvedValue({ data: enabledRoute(), etag: "\"2\"" });
    renderPage();

    const row = (await screen.findByText("route_chat")).closest("tr");
    expect(within(row!).getByText("已禁用")).toHaveClass("resource-state");
    expect(within(row!).getByText("已禁用")).not.toHaveClass("enabled");
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
