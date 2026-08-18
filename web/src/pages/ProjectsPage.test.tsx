import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api";
import { PoliciesPage } from "./PoliciesPage";
import { NotificationProvider } from "../notifications";
import { ProjectsPage } from "./ProjectsPage";

const tokenGuardPolicy = {
  id: "tgp_1", name: "Production guard", enabled: true, action: "alert", revision: 1,
};
const redactionPolicy = {
  id: "rp_1", name: "Strict redaction", enabled: true, mode: "strict", rules: [], revision: 1,
};

function project(overrides: Record<string, unknown> = {}) {
  return {
    id: "prj_1", name: "Inference", enabled: true, allowed_models: ["chat"], rpm: 60, tpm: 1000,
    max_concurrency: 8, daily_budget_micros_usd: 0, max_input_tokens: 0, max_output_tokens: 0,
    max_request_bytes: 0, max_stream_duration: 0, allowed_cidrs: [], redaction_policy_id: "",
    token_guard_policy_id: "", revision: 1, created_at: "", updated_at: "", ...overrides,
  };
}

function gatewayKey(overrides: Record<string, unknown> = {}) {
  return {
    id: "key_1", project_id: "prj_1", name: "service-a", enabled: true,
    created_at: "2026-01-01T00:00:00Z", revision: 1, ...overrides,
  };
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><NotificationProvider><ProjectsPage /></NotificationProvider></QueryClientProvider>);
}

describe("projects page", () => {
  beforeEach(() => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "projectsPage").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "keysPage").mockResolvedValue({ items: [], next_cursor: "" } as never);
    vi.spyOn(api, "routes").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "allRoutes").mockResolvedValue([]);
    vi.spyOn(api, "tokenGuardPolicies").mockResolvedValue({ items: [tokenGuardPolicy], next_cursor: "" } as never);
    vi.spyOn(api, "redactionPolicies").mockResolvedValue({ items: [redactionPolicy], next_cursor: "" } as never);
    vi.spyOn(api, "tokenGuardPoliciesPage").mockResolvedValue({ items: [tokenGuardPolicy], next_cursor: "" } as never);
    vi.spyOn(api, "redactionPoliciesPage").mockResolvedValue({ items: [redactionPolicy], next_cursor: "" } as never);
  });

  afterEach(() => vi.restoreAllMocks());

  it("opens the create form after the policies page filled the shared cache", async () => {
    // The policies page pages the same resources through useInfiniteQuery. Sharing a query
    // key would hand this page {pages} instead of {items} and blank the whole console.
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const policies = render(<QueryClientProvider client={client}><PoliciesPage /></QueryClientProvider>);
    expect(await screen.findByText("Production guard")).toBeVisible();
    policies.unmount();

    render(<QueryClientProvider client={client}><ProjectsPage /></QueryClientProvider>);
    fireEvent.click(await screen.findByRole("button", { name: "创建第一个项目" }));

    expect(screen.getByRole("heading", { name: "创建项目" })).toBeVisible();
    await waitFor(() => expect(screen.getByRole("option", { name: /Production guard/ })).toBeInTheDocument());
    expect(screen.getByRole("option", { name: /Strict redaction/ })).toBeInTheDocument();
  });

  it("authorizes one public model alias instead of exposing each candidate route", async () => {
    vi.mocked(api.allRoutes).mockResolvedValue([
      { id: "rt_openai", public_model: "chat", strategy: "ordered", enabled: true },
      { id: "rt_azure", public_model: "chat", strategy: "ordered", enabled: true },
      { id: "rt_disabled", public_model: "chat", strategy: "ordered", enabled: false },
      { id: "rt_embed", public_model: "embedding", strategy: "round_robin", enabled: true },
    ] as never);

    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "创建第一个项目" }));

    expect(await screen.findByText("允许的模型别名（路由入口）")).toBeVisible();
    expect(screen.queryByRole("searchbox", { name: "搜索模型别名或路由 ID" })).not.toBeInTheDocument();
    expect(await screen.findAllByRole("checkbox", { name: /chat/ })).toHaveLength(1);
    expect(screen.getByRole("checkbox", { name: /chat/ })).toHaveAccessibleName(/2 条已启用路由/);
    expect(screen.queryByRole("checkbox", { name: /rt_openai/ })).not.toBeInTheDocument();
    expect(screen.getByRole("checkbox", { name: /embedding/ })).toBeVisible();
  });

  // The count already ignored disabled routes; the strategy beside it did not,
  // so one retired row with the other strategy blanked the label rather than
  // describing the routes that actually serve the alias.
  it("describes the alias by its enabled routes alone", async () => {
    vi.mocked(api.allRoutes).mockResolvedValue([
      { id: "rt_live", public_model: "chat", strategy: "round_robin", enabled: true },
      { id: "rt_live_two", public_model: "chat", strategy: "round_robin", enabled: true },
      { id: "rt_retired", public_model: "chat", strategy: "ordered", enabled: false },
    ] as never);

    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "创建第一个项目" }));

    expect(await screen.findByText("允许的模型别名（路由入口）")).toBeVisible();
    expect(await screen.findByRole("checkbox", { name: /chat/ })).toHaveAccessibleName(/2 条已启用路由 · 轮询/);
  });

  it("places project enablement in the footer action area", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "创建第一个项目" }));

    const enable = screen.getByRole("checkbox", { name: /启用项目/ });
    const actions = screen.getByRole("button", { name: "创建项目" }).closest<HTMLElement>(".sticky-form-actions")!;
    expect(actions).toContainElement(enable);
    expect(within(actions).getByText("启用项目 · 启用")).toBeVisible();
  });

  it("renders a project whose allowed_models came back as null", async () => {
    // The admin API accepts a project without allowed_models and serialises it as null;
    // dereferencing it used to throw during render and blank the whole console.
    vi.mocked(api.projectsPage).mockResolvedValue({
      items: [project({ name: "NoRoutes", allowed_models: null })],
      next_cursor: "",
    } as never);

    renderPage();

    expect(await screen.findByRole("heading", { name: /NoRoutes/ })).toBeVisible();
    expect(screen.getByText("允许的模型别名").nextSibling).toHaveTextContent("无");
  });

  it("keeps the policies page working after the project form filled the shared cache", async () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const projects = render(<QueryClientProvider client={client}><ProjectsPage /></QueryClientProvider>);
    fireEvent.click(await screen.findByRole("button", { name: "创建第一个项目" }));
    await waitFor(() => expect(screen.getByRole("option", { name: /Production guard/ })).toBeInTheDocument());
    projects.unmount();

    render(<QueryClientProvider client={client}><PoliciesPage /></QueryClientProvider>);
    expect(await screen.findByText("Production guard")).toBeVisible();
  });

  it("follows next_cursor so a project past the first page stays reachable", async () => {
    // An unpaged list is truncated at 50 by the server. A project that never renders is a
    // project whose budget and keys nobody can administer.
    vi.mocked(api.projectsPage)
      .mockResolvedValueOnce({ items: [project()], next_cursor: "prj_1" } as never)
      .mockResolvedValueOnce({ items: [project({ id: "prj_2", name: "Batch" })], next_cursor: "" } as never);

    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "加载更多" }));
    expect(await screen.findByText("Batch")).toBeVisible();
    expect(api.projectsPage).toHaveBeenLastCalledWith("?limit=50&cursor=prj_1");
  });

  it.each([
    [true, "禁用", false],
    [false, "启用", true],
  ])("switches project status from the detail actions without changing its policy (%s)", async (enabled, actionLabel, nextEnabled) => {
    const current = project({
      enabled,
      allowed_models: ["chat"],
      allowed_cidrs: ["10.0.0.0/8"],
      max_input_tokens: 128_000,
      max_output_tokens: 16_384,
      max_request_bytes: 1_048_576,
      max_stream_duration: 600_000_000_000,
      redaction_policy_id: "rp_1",
      token_guard_policy_id: "tgp_1",
    });
    vi.mocked(api.projectsPage).mockResolvedValue({ items: [current], next_cursor: "" } as never);
    const update = vi.spyOn(api, "updateProject").mockResolvedValue({ ...current, enabled: nextEnabled } as never);

    renderPage();
    expect(await screen.findByText(enabled ? "启用" : "禁用", { selector: ".detail-title .badge" })).toHaveClass(enabled ? "good" : "muted");
    fireEvent.click(await screen.findByRole("button", { name: actionLabel }));
    // Disabling a project revokes every gateway key under it at once, so it asks
    // first; enabling one restores nothing that was not already configured.
    if (enabled) {
      const dialog = await screen.findByRole("alertdialog");
      fireEvent.click(within(dialog).getByRole("button", { name: "禁用" }));
    }

    await waitFor(() => expect(update).toHaveBeenCalledWith("prj_1", {
      name: "Inference",
      enabled: nextEnabled,
      allowed_models: ["chat"],
      rpm: 60,
      tpm: 1000,
      max_concurrency: 8,
      daily_budget_micros_usd: 0,
      max_input_tokens: 128_000,
      max_output_tokens: 16_384,
      max_request_bytes: 1_048_576,
      max_stream_duration_seconds: 600,
      allowed_cidrs: ["10.0.0.0/8"],
      redaction_policy_id: "rp_1",
      token_guard_policy_id: "tgp_1",
    }, '"1"'));
  });

  it("states how many gateway keys a project disable takes down", async () => {
    vi.mocked(api.projectsPage).mockResolvedValue({ items: [project()], next_cursor: "" } as never);
    vi.mocked(api.keysPage).mockResolvedValue({
      items: [gatewayKey(), gatewayKey({ id: "key_2", name: "service-b" })], next_cursor: "",
    } as never);

    renderPage();
    // Every key row carries its own "禁用" too, so the project-level one is taken
    // from the detail header.
    const header = (await screen.findByRole("heading", { name: /Inference/ })).closest(".detail-title")!;
    await waitFor(() => expect(screen.getByText("service-b")).toBeVisible());
    fireEvent.click(within(header as HTMLElement).getByRole("button", { name: "禁用" }));

    expect(await screen.findByText(/该项目下已创建的 2 个网关密钥/)).toBeVisible();
  });

  it("mints one key even when Enter submits the create form repeatedly", async () => {
    // The submit button is disabled while in flight, but Enter inside a field bypasses it.
    // Each extra key is live and its plaintext is never shown to anyone.
    vi.mocked(api.projectsPage).mockResolvedValue({ items: [project()], next_cursor: "" } as never);
    let resolve: (value: unknown) => void = () => {};
    const createKey = vi.spyOn(api, "createKey").mockReturnValue(
      new Promise((settle) => { resolve = settle; }) as never,
    );

    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 创建密钥" }));
    const nameField = screen.getByLabelText(/密钥名称/);
    fireEvent.change(nameField, { target: { value: "service-a" } });
    fireEvent.change(screen.getByLabelText(/^当前密码/), { target: { value: "a passphrase" } });
    const form = nameField.closest("form")!;
    fireEvent.submit(form);
    await waitFor(() => expect(createKey).toHaveBeenCalledTimes(1));
    fireEvent.submit(form);
    fireEvent.submit(form);

    await waitFor(() => expect(createKey).toHaveBeenCalledTimes(1));
    resolve({ data: { key: "gw_secret", metadata: gatewayKey() }, etag: "" });
  });

  it("carries one idempotency key so a retried create cannot mint a second credential", async () => {
    vi.mocked(api.projectsPage).mockResolvedValue({ items: [project()], next_cursor: "" } as never);
    const createKey = vi.spyOn(api, "createKey").mockRejectedValue(new ApiError(503, "upstream unavailable"));

    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 创建密钥" }));
    fireEvent.change(screen.getByLabelText(/密钥名称/), { target: { value: "service-a" } });
    // Minting now asks for the operator's password, so the form cannot submit
    // until it is supplied.
    fireEvent.change(screen.getByLabelText(/^当前密码/), { target: { value: "a passphrase" } });
    fireEvent.click(screen.getByRole("button", { name: "生成密钥" }));
    await screen.findByText("无法完成请求");
    fireEvent.click(screen.getByRole("button", { name: "生成密钥" }));
    await waitFor(() => expect(createKey).toHaveBeenCalledTimes(2));

    const [, , firstToken] = createKey.mock.calls[0];
    const [, , retryToken] = createKey.mock.calls[1];
    expect(firstToken).toBeTruthy();
    expect(retryToken).toBe(firstToken);
  });

  it("reports a failed key revocation instead of leaving the row looking untouched", async () => {
    // Silence here reads as success, and the operator walks away from a live credential.
    vi.mocked(api.projectsPage).mockResolvedValue({ items: [project()], next_cursor: "" } as never);
    vi.mocked(api.keysPage).mockResolvedValue({ items: [gatewayKey()], next_cursor: "" } as never);
    vi.spyOn(api, "deleteKey").mockRejectedValue(new ApiError(412, "resource revision conflict"));

    renderPage();
    const row = (await screen.findByText("service-a")).closest(".key-row")!;
    fireEvent.click(within(row as HTMLElement).getByRole("button", { name: "删除" }));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.change(within(dialog).getByLabelText(/当前密码/), { target: { value: "correct horse battery staple" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "删除" }));

    expect(await screen.findByText("无法完成请求")).toBeVisible();
  });

  it("surfaces the server's reason for a rejected project", async () => {
    // A localized "the request is invalid" alone cannot tell name from CIDR from policy.
    vi.mocked(api.projectsPage).mockResolvedValue({ items: [], next_cursor: "" } as never);
    vi.mocked(api.allRoutes).mockResolvedValue([
      { id: "rt_1", public_model: "chat", enabled: true, revision: 1 },
    ] as never);
    vi.spyOn(api, "createProject").mockRejectedValue(
      new ApiError(409, "allowed_models references unknown model alias chatt"),
    );

    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "创建第一个项目" }));
    fireEvent.change(screen.getByLabelText("名称"), { target: { value: "Inference" } });
    fireEvent.click(await screen.findByRole("checkbox", { name: /chat/ }));
    fireEvent.click(screen.getByRole("button", { name: "创建项目" }));

    expect(await screen.findByText("allowed_models references unknown model alias chatt")).toBeVisible();
  });

  // The dialog closes on success, so the acknowledgement has nothing left on
  // the page to attach to and goes to the notification column.
  it("confirms a created project in the notification column", async () => {
    vi.mocked(api.projectsPage).mockResolvedValue({ items: [], next_cursor: "" } as never);
    vi.mocked(api.allRoutes).mockResolvedValue([
      { id: "rt_1", public_model: "chat", enabled: true, revision: 1 },
    ] as never);
    const createProject = vi.spyOn(api, "createProject").mockResolvedValue({ data: project(), etag: '"1"' } as never);

    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "创建第一个项目" }));
    fireEvent.change(screen.getByLabelText("名称"), { target: { value: "Inference" } });
    fireEvent.click(await screen.findByRole("checkbox", { name: /chat/ }));
    fireEvent.click(screen.getByRole("button", { name: "创建项目" }));

    await waitFor(() => expect(createProject).toHaveBeenCalledOnce());
    const confirmation = await screen.findByText("项目已创建");
    expect(confirmation.closest("[aria-live]")).toHaveAttribute("aria-live", "polite");
    expect(screen.getByText("Inference")).toBeVisible();
  });

  it("rejects an unparsable CIDR before the request leaves the browser", async () => {
    vi.mocked(api.projectsPage).mockResolvedValue({ items: [], next_cursor: "" } as never);
    vi.mocked(api.allRoutes).mockResolvedValue([
      { id: "rt_1", public_model: "chat", enabled: true, revision: 1 },
    ] as never);
    const createProject = vi.spyOn(api, "createProject");

    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "创建第一个项目" }));
    fireEvent.change(screen.getByLabelText("名称"), { target: { value: "Inference" } });
    fireEvent.click(await screen.findByRole("checkbox", { name: /chat/ }));
    fireEvent.change(screen.getByLabelText(/允许的 CIDR/), { target: { value: "999.1.1.1/24" } });
    fireEvent.click(screen.getByRole("button", { name: "创建项目" }));

    expect(await screen.findByText("存在无法解析的 IP 或 CIDR，请检查每一项")).toBeVisible();
    expect(createProject).not.toHaveBeenCalled();
  });

  it("holds the one-time plaintext until the operator confirms it was stored", async () => {
    vi.mocked(api.projectsPage).mockResolvedValue({ items: [project()], next_cursor: "" } as never);
    vi.spyOn(api, "createKey").mockResolvedValue({
      data: { key: "gw_plaintext", metadata: gatewayKey() }, etag: "",
    } as never);

    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 创建密钥" }));
    fireEvent.change(screen.getByLabelText(/密钥名称/), { target: { value: "service-a" } });
    fireEvent.change(screen.getByLabelText(/^当前密码/), { target: { value: "a passphrase" } });
    fireEvent.click(screen.getByRole("button", { name: "生成密钥" }));

    expect(await screen.findByText("gw_plaintext")).toBeVisible();
    expect(screen.getByRole("button", { name: "关闭" })).toBeDisabled();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.getByText("gw_plaintext")).toBeVisible();

    fireEvent.click(screen.getByRole("checkbox", { name: "我已将密钥保存到安全的密钥管理器" }));
    expect(screen.getByRole("button", { name: "关闭" })).toBeEnabled();
    fireEvent.click(screen.getByRole("button", { name: "完成并清除明文" }));
    await waitFor(() => expect(screen.queryByText("gw_plaintext")).not.toBeInTheDocument());
  });

  it("marks an expired key as dead even while it is still enabled", async () => {
    vi.mocked(api.projectsPage).mockResolvedValue({ items: [project()], next_cursor: "" } as never);
    vi.mocked(api.keysPage).mockResolvedValue({
      items: [gatewayKey({ expires_at: "2020-01-01T00:00:00Z" })], next_cursor: "",
    } as never);

    renderPage();

    expect(await screen.findByText(/已于.*过期/)).toBeVisible();
  });
  it("pulls the remaining pages before filtering so a match cannot hide behind a cursor", async () => {
    // /projects rejects every parameter except cursor and limit, so the filter runs in the
    // browser. Filtering only the first page would report "no matches" for a real project.
    vi.mocked(api.projectsPage)
      .mockResolvedValueOnce({ items: [project()], next_cursor: "prj_1" } as never)
      .mockResolvedValueOnce({ items: [project({ id: "prj_2", name: "Batch" })], next_cursor: "" } as never);

    renderPage();
    const list = () => screen.getByRole("region", { name: "项目列表" });
    await waitFor(() => expect(within(list()).getByText("Inference")).toBeVisible());
    fireEvent.change(screen.getByRole("searchbox", { name: /搜索/ }), { target: { value: "batch" } });

    await waitFor(() => expect(within(list()).getByText("Batch")).toBeVisible());
    expect(within(list()).queryByText("Inference")).not.toBeInTheDocument();
    expect(api.projectsPage).toHaveBeenCalledTimes(2);
  });

  it("filters by status and says so when nothing matches", async () => {
    vi.mocked(api.projectsPage).mockResolvedValue({
      items: [project(), project({ id: "prj_2", name: "Batch" })], next_cursor: "",
    } as never);

    renderPage();
    const list = () => screen.getByRole("region", { name: "项目列表" });
    await waitFor(() => expect(within(list()).getByText("Inference")).toBeVisible());
    fireEvent.change(screen.getByRole("combobox", { name: /状态/ }), { target: { value: "disabled" } });

    expect(await screen.findByText("没有匹配结果")).toBeVisible();
    expect(within(list()).queryByText("Batch")).not.toBeInTheDocument();
  });

  it("reports how much of the list is loaded", async () => {
    vi.mocked(api.projectsPage).mockResolvedValue({ items: [project()], next_cursor: "" } as never);

    renderPage();

    expect(await screen.findByText("已加载全部 1 个项目")).toBeVisible();
  });
  it("keeps quotas folded away so gateway keys lead the panel", async () => {
    vi.mocked(api.projectsPage).mockResolvedValue({ items: [project()], next_cursor: "" } as never);

    renderPage();

    const summary = await screen.findByText("配额与策略");
    const block = summary.closest("details")!;
    expect(block.open).toBe(false);
    // The digest has to carry the headline numbers, or collapsing hides them entirely.
    expect(block).toHaveTextContent("1 个模型 · 60 RPM · 不限");

    // The affordance is the visible control, not the browser's default marker.
    expect(within(block).getByText("查看详情")).toBeInTheDocument();

    fireEvent.click(summary);
    expect(block.open).toBe(true);
    expect(within(block).getByText("令牌防护策略")).toBeVisible();
  });
});
