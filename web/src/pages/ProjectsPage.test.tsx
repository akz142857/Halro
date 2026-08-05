import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api";
import { PoliciesPage } from "./PoliciesPage";
import { ProjectsPage } from "./ProjectsPage";

const tokenGuardPolicy = {
  id: "tgp_1", name: "Production guard", enabled: true, action: "alert", revision: 1,
};
const redactionPolicy = {
  id: "rp_1", name: "Strict redaction", enabled: true, mode: "strict", rules: [], revision: 1,
};

function project(overrides: Record<string, unknown> = {}) {
  return {
    id: "prj_1", name: "Inference", enabled: true, allowed_routes: ["chat"], rpm: 60, tpm: 1000,
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
  return render(<QueryClientProvider client={client}><ProjectsPage /></QueryClientProvider>);
}

describe("projects page", () => {
  beforeEach(() => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "projectsPage").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "keysPage").mockResolvedValue({ items: [], next_cursor: "" } as never);
    vi.spyOn(api, "routes").mockResolvedValue({ items: [], next_cursor: "" });
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

  it("renders a project whose allowed_routes came back as null", async () => {
    // The admin API accepts a project without allowed_routes and serialises it as null;
    // dereferencing it used to throw during render and blank the whole console.
    vi.mocked(api.projectsPage).mockResolvedValue({
      items: [project({ name: "NoRoutes", allowed_routes: null })],
      next_cursor: "",
    } as never);

    renderPage();

    expect(await screen.findByRole("heading", { name: /NoRoutes/ })).toBeVisible();
    expect(screen.getByText("允许的模型").nextSibling).toHaveTextContent("无");
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
    fireEvent.click(within(dialog).getByRole("button", { name: "删除" }));

    expect(await screen.findByText("无法完成请求")).toBeVisible();
  });

  it("surfaces the server's reason for a rejected project", async () => {
    // A localized "the request is invalid" alone cannot tell name from CIDR from policy.
    vi.mocked(api.projectsPage).mockResolvedValue({ items: [], next_cursor: "" } as never);
    vi.spyOn(api, "routes").mockResolvedValue({
      items: [{ id: "rt_1", public_model: "chat", enabled: true, revision: 1 }], next_cursor: "",
    } as never);
    vi.spyOn(api, "createProject").mockRejectedValue(
      new ApiError(409, "allowed_routes references unknown model alias chatt"),
    );

    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "创建第一个项目" }));
    fireEvent.change(screen.getByLabelText("名称"), { target: { value: "Inference" } });
    fireEvent.click(await screen.findByRole("checkbox", { name: /chat/ }));
    fireEvent.click(screen.getByRole("button", { name: "创建项目" }));

    expect(await screen.findByText("allowed_routes references unknown model alias chatt")).toBeVisible();
  });

  it("rejects an unparsable CIDR before the request leaves the browser", async () => {
    vi.mocked(api.projectsPage).mockResolvedValue({ items: [], next_cursor: "" } as never);
    vi.spyOn(api, "routes").mockResolvedValue({
      items: [{ id: "rt_1", public_model: "chat", enabled: true, revision: 1 }], next_cursor: "",
    } as never);
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
