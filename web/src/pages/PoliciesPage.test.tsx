import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api";
import type { TokenGuardPolicy } from "../types";
import { PoliciesPage } from "./PoliciesPage";

const policy = {
  id: "tgp_test", name: "Production guard", enabled: true, action: "alert",
  request_tokens: 32000, tokens_per_minute: 200000, cost_micros_per_minute: 5000000,
  error_rate: 0.2, minimum_samples: 10, concurrency: 20, unique_ips_per_minute: 10,
  violations_before_block: 2, block_ttl_seconds: 300, cooldown_seconds: 60,
  ewma_enabled: false, ewma_alpha: 0.2, ewma_multiplier: 3, ewma_minimum_samples: 100,
  ewma_warmup_seconds: 3600, ewma_evaluation_window_seconds: 60,
  ewma_cooldown_seconds: 300, ewma_absolute_rpm: 60, ewma_absolute_tpm: 50000,
  ewma_absolute_tokens_per_request: 4000, ewma_absolute_cost_micros_per_minute: 1000000,
  revision: 1,
  bound_projects: 2,
} as TokenGuardPolicy;

describe("token guard policy workflow", () => {
  beforeEach(() => {
    vi.spyOn(api, "tokenGuardPoliciesPage").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "redactionPoliciesPage").mockResolvedValue({ items: [], next_cursor: "" });
  });

  afterEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/policies");
  });

  it("creates policies disabled and reports domain validation at the fields", async () => {
    const create = vi.spyOn(api, "createTokenGuardPolicy").mockResolvedValue({} as never);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建令牌防护策略" }));

    expect(screen.getByLabelText("启用此策略")).not.toBeChecked();
    expect(screen.getByText("启用此策略 · 禁用").closest(".sticky-form-actions")).toContainElement(screen.getByLabelText("启用此策略"));
    expect(screen.getByLabelText("高级设置")).not.toBeChecked();
    expect(screen.getByText("自适应检测默认开启")).toBeVisible();
    expect(screen.getByLabelText("高级设置").compareDocumentPosition(screen.getByLabelText("启用此策略")) & Node.DOCUMENT_POSITION_FOLLOWING).toBeTruthy();
    fireEvent.change(screen.getByLabelText(/^错误率阈值（%）/), { target: { value: "101" } });
    fireEvent.click(screen.getByRole("button", { name: "保存策略" }));

    expect(await screen.findByText("必填")).toBeVisible();
    expect(screen.getByText("必须在 0 到 100 之间")).toBeVisible();
    expect(screen.getByRole("textbox", { name: /^策略名称/ })).toHaveAttribute("aria-invalid", "true");
    expect(create).not.toHaveBeenCalled();
  });

  it("explains temporary-block escalation instead of implying immediate rejection", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建令牌防护策略" }));
    fireEvent.click(screen.getByRole("radio", { name: /临时封禁/ }));
    expect(screen.getByText("非立即拒绝")).toBeVisible();
    expect(screen.getByText(/达到样本和违规双门槛前，请求仍会放行/)).toBeVisible();
  });

  it("preserves legal zero EWMA floors while editing", async () => {
    const zeroFloorPolicy = { ...policy, ewma_enabled: true, ewma_absolute_rpm: 0, ewma_absolute_tpm: 0, ewma_absolute_tokens_per_request: 0 };
    vi.mocked(api.tokenGuardPoliciesPage).mockResolvedValue({ items: [zeroFloorPolicy], next_cursor: "" });
    const update = vi.spyOn(api, "updateTokenGuardPolicy").mockResolvedValue({ data: zeroFloorPolicy, etag: '"2"' });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "编辑" }));
    expect(screen.getByLabelText("高级设置")).toBeChecked();
    expect(screen.getByLabelText("每分钟请求数达到")).toHaveValue(0);
    expect(screen.getByLabelText("每分钟令牌数达到")).toHaveValue(0);
    expect(screen.getByLabelText("单次请求平均令牌数达到")).toHaveValue(0);
    fireEvent.change(screen.getByLabelText("当前密码"), { target: { value: "a passphrase" } });
    fireEvent.click(screen.getByRole("button", { name: "保存并立即应用" }));
    await waitFor(() => expect(update).toHaveBeenCalledWith("tgp_test", expect.objectContaining({
      ewma_absolute_rpm: 0,
      ewma_absolute_tpm: 0,
      ewma_absolute_tokens_per_request: 0,
    }), 1, expect.objectContaining({ currentPassword: "a passphrase" })));
  });

  it("uses adaptive detection with recommended settings by default", async () => {
    const create = vi.spyOn(api, "createTokenGuardPolicy").mockResolvedValue({} as never);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建令牌防护策略" }));
    fireEvent.change(screen.getByLabelText("策略名称"), { target: { value: "Adaptive guard" } });
    fireEvent.click(screen.getByRole("button", { name: "保存策略" }));

    await waitFor(() => expect(create).toHaveBeenCalledWith(expect.objectContaining({
      ewma_enabled: true,
      ewma_alpha: 0.2,
      ewma_multiplier: 3,
      ewma_evaluation_window_seconds: 60,
    })));
  });

  // Collapsing the panel used to silently reset ten tuned values to their
  // defaults. Nothing in the control says so, and there is no undo.
  it("treats the advanced panel as a disclosure that never discards tuned values", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建令牌防护策略" }));
    const advanced = screen.getByLabelText("高级设置");

    fireEvent.click(advanced);
    fireEvent.change(screen.getByLabelText("对近期流量变化的敏感度"), { target: { value: "0.5" } });
    expect(screen.getByLabelText("对近期流量变化的敏感度")).toHaveValue(0.5);
    fireEvent.click(advanced);
    expect(screen.queryByLabelText("对近期流量变化的敏感度")).not.toBeInTheDocument();
    fireEvent.click(advanced);
    expect(screen.getByLabelText("对近期流量变化的敏感度")).toHaveValue(0.5);
  });

  // The form used to send ewma_enabled: true unconditionally while the list kept
  // rendering "EWMA off" — the policy shown was not the policy saved.
  it("keeps adaptive detection off for a policy that was saved with it off", async () => {
    vi.mocked(api.tokenGuardPoliciesPage).mockResolvedValue({ items: [policy], next_cursor: "" });
    const update = vi.spyOn(api, "updateTokenGuardPolicy").mockResolvedValue({ data: policy, etag: '"2"' });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "编辑" }));

    // The switch has to be reachable, so the block opens for a policy the
    // recommended defaults cannot describe.
    expect(screen.getByLabelText("启用自适应检测")).not.toBeChecked();
    fireEvent.change(screen.getByLabelText("当前密码"), { target: { value: "a passphrase" } });
    fireEvent.click(screen.getByRole("button", { name: "保存并立即应用" }));

    await waitFor(() => expect(update).toHaveBeenCalledWith(
      "tgp_test", expect.objectContaining({ ewma_enabled: false }), 1,
      expect.objectContaining({ currentPassword: "a passphrase" })));
  });

  // The server accepts ewma_enabled: false together with zero EWMA parameters.
  // Validating those fields as if detection were on produced seven errors and a
  // "7 fields need attention" banner pointing at nothing rendered.
  it("does not refuse a policy over adaptive fields that are switched off", async () => {
    const offPolicy = {
      ...policy, ewma_enabled: false, ewma_alpha: 0, ewma_multiplier: 0,
      ewma_minimum_samples: 0, ewma_warmup_seconds: 0, ewma_evaluation_window_seconds: 0,
      ewma_cooldown_seconds: 0, ewma_absolute_rpm: 0, ewma_absolute_tpm: 0,
      ewma_absolute_tokens_per_request: 0, ewma_absolute_cost_micros_per_minute: 0,
    };
    vi.mocked(api.tokenGuardPoliciesPage).mockResolvedValue({ items: [offPolicy], next_cursor: "" });
    const update = vi.spyOn(api, "updateTokenGuardPolicy").mockResolvedValue({ data: offPolicy, etag: '"2"' });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "编辑" }));
    fireEvent.change(screen.getByLabelText("当前密码"), { target: { value: "a passphrase" } });
    fireEvent.click(screen.getByRole("button", { name: "保存并立即应用" }));

    await waitFor(() => expect(update).toHaveBeenCalledOnce());
    expect(screen.queryByText(/个字段需要修正/)).not.toBeInTheDocument();
  });

  it("reopens the adaptive panel rather than reporting an error nobody can reach", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建令牌防护策略" }));
    fireEvent.change(screen.getByLabelText("策略名称"), { target: { value: "Adaptive guard" } });
    const advanced = screen.getByLabelText("高级设置");
    fireEvent.click(advanced);
    fireEvent.change(screen.getByLabelText("对近期流量变化的敏感度"), { target: { value: "0" } });
    fireEvent.click(advanced);
    fireEvent.click(screen.getByRole("button", { name: "保存策略" }));

    expect(screen.getByLabelText("高级设置")).toBeChecked();
    expect(screen.getByText("必须大于 0 且不超过 1")).toBeVisible();
    expect(screen.getByLabelText(/^对近期流量变化的敏感度/)).toHaveValue(0);
  });

  // Thirty fields behind an Escape key and a backdrop click.
  it("asks before discarding a changed token guard draft", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建令牌防护策略" }));
    fireEvent.change(screen.getByLabelText("策略名称"), { target: { value: "Changed" } });
    fireEvent.keyDown(document, { key: "Escape" });

    expect(screen.getByRole("alert")).toHaveTextContent("关闭会丢弃此处已填写的内容");
    expect(screen.getByRole("heading", { name: "创建令牌防护策略" })).toBeVisible();
  });

  it("passes every relevant window signal to the simulator", async () => {
    vi.mocked(api.tokenGuardPoliciesPage).mockResolvedValue({ items: [policy], next_cursor: "" });
    const preview = vi.spyOn(api, "previewTokenGuardPolicy").mockResolvedValue({ violated: false, reason: "", action: "observe" });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "模拟" }));

    fireEvent.change(screen.getByLabelText("本次请求预计成本（USD）"), { target: { value: "1.25" } });
    fireEvent.change(screen.getByLabelText("最近一分钟已累计成本（USD）"), { target: { value: "2.5" } });
    fireEvent.change(screen.getByLabelText("最近一分钟已处理请求数"), { target: { value: "12" } });
    fireEvent.change(screen.getByLabelText("最近一分钟错误请求数"), { target: { value: "3" } });
    fireEvent.change(screen.getByLabelText("最近一分钟不同来源 IP 数"), { target: { value: "7" } });
    fireEvent.click(screen.getByLabelText("本次请求来自新的 IP 地址（来源数量 +1）"));
    fireEvent.click(screen.getByRole("button", { name: "运行模拟" }));

    await waitFor(() => expect(preview).toHaveBeenCalledWith("tgp_test", expect.objectContaining({
      estimated_cost_micros_usd: 1250000,
      has_new_source: false,
      window: expect.objectContaining({ requests: 12, errors: 3, unique_ips: 7, cost_micros_usd: 2500000 }),
    })));
    expect(screen.getByText("本次请求未触发防护规则")).toBeVisible();
    expect(screen.getByText("本次请求将正常放行，不执行防护处置。")).toBeVisible();
  });

  it.each([
    ["request_tokens", "单次请求令牌数超过上限"],
    ["tokens_per_minute", "一分钟内累计令牌数超过上限"],
    ["cost_per_minute", "一分钟内累计成本超过上限"],
    ["concurrency", "当前并发请求数超过上限"],
    ["unique_ip", "一分钟内来源 IP 数超过上限"],
    ["error_rate", "窗口内请求错误率超过上限"],
  ])("translates the %s rule code into an operator-facing result", async (reason, title) => {
    vi.mocked(api.tokenGuardPoliciesPage).mockResolvedValue({ items: [policy], next_cursor: "" });
    vi.spyOn(api, "previewTokenGuardPolicy").mockResolvedValue({ violated: true, reason, action: "alert" });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "模拟" }));
    if (reason === "request_tokens") {
      fireEvent.change(screen.getByLabelText("本次请求预计令牌数"), { target: { value: "32003" } });
    }
    fireEvent.click(screen.getByRole("button", { name: "运行模拟" }));

    expect(await screen.findByText(title)).toBeVisible();
    expect(screen.queryByText(`命中：${reason}`)).not.toBeInTheDocument();
    if (reason === "request_tokens") {
      expect(screen.getByText("本次请求预计使用 32003 个令牌，策略上限为 32000 个。")).toBeVisible();
    }
    expect(screen.getByText("模拟处置：放行本次请求，并发送告警")).toBeVisible();
  });

  it("locks every close path while a save is pending", async () => {
    let finish!: (value: never) => void;
    vi.spyOn(api, "createTokenGuardPolicy").mockReturnValue(new Promise((resolve) => { finish = resolve; }));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建令牌防护策略" }));
    fireEvent.change(screen.getByLabelText("策略名称"), { target: { value: "Pending guard" } });
    fireEvent.click(screen.getByRole("button", { name: "保存策略" }));

    await waitFor(() => expect(screen.getByRole("button", { name: "取消" })).toBeDisabled());
    expect(screen.getByRole("button", { name: "关闭" })).toBeDisabled();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.getByRole("heading", { name: "创建令牌防护策略" })).toBeVisible();
    finish({} as never);
  });

  it("shows enabled state and project bindings in the compact table", async () => {
    vi.mocked(api.tokenGuardPoliciesPage).mockResolvedValue({ items: [policy], next_cursor: "" });
    renderPage();
    expect(await screen.findByRole("cell", { name: "启用" })).toBeVisible();
    expect(screen.getByText("2 个项目")).toBeVisible();
  });

  // The body is a hand-written rebuild of all 25 fields. A field left out of it is
  // a threshold silently reset by an operator who only clicked "Disable", so this
  // asserts the whole object rather than a sample of it.
  it("toggles a policy from the list without changing its configuration", async () => {
    vi.mocked(api.tokenGuardPoliciesPage).mockResolvedValue({ items: [policy], next_cursor: "" });
    const update = vi.spyOn(api, "updateTokenGuardPolicy").mockResolvedValue({ data: { ...policy, enabled: false }, etag: '"2"' });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "禁用" }));
    // Switching enforcement off for every bound Project asks first, and says how
    // many Projects that is.
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(/已绑定的 2 个项目/)).toBeVisible();
    fireEvent.change(within(dialog).getByLabelText("当前密码"), { target: { value: "a passphrase" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "禁用" }));

    await waitFor(() => expect(update).toHaveBeenCalledWith("tgp_test", {
      name: "Production guard",
      enabled: false,
      action: "alert",
      request_tokens: 32000,
      tokens_per_minute: 200000,
      cost_micros_per_minute: 5000000,
      error_rate: 0.2,
      minimum_samples: 10,
      concurrency: 20,
      unique_ips_per_minute: 10,
      violations_before_block: 2,
      block_ttl_seconds: 300,
      cooldown_seconds: 60,
      ewma_enabled: false,
      ewma_alpha: 0.2,
      ewma_multiplier: 3,
      ewma_minimum_samples: 100,
      ewma_warmup_seconds: 3600,
      ewma_evaluation_window_seconds: 60,
      ewma_cooldown_seconds: 300,
      ewma_absolute_rpm: 60,
      ewma_absolute_tpm: 50000,
      ewma_absolute_tokens_per_request: 4000,
      ewma_absolute_cost_micros_per_minute: 1000000,
    }, 1, expect.objectContaining({ currentPassword: "a passphrase" })));
  });

  it("holds only the row whose status change is in flight", async () => {
    vi.mocked(api.tokenGuardPoliciesPage).mockResolvedValue({
      items: [policy, { ...policy, id: "tgp_second", name: "Second guard" }], next_cursor: "",
    });
    vi.spyOn(api, "updateTokenGuardPolicy").mockReturnValue(new Promise(() => {}) as never);
    renderPage();

    const first = (await screen.findByText("Production guard")).closest("tr")!;
    const second = screen.getByText("Second guard").closest("tr")!;
    fireEvent.click(within(first).getByRole("button", { name: "禁用" }));
    const confirmation = await screen.findByRole("alertdialog");
    fireEvent.change(within(confirmation).getByLabelText("当前密码"), { target: { value: "a passphrase" } });
    fireEvent.click(within(confirmation).getByRole("button", { name: "禁用" }));

    await waitFor(() => expect(within(first).getByRole("button", { name: "禁用" })).toBeDisabled());
    expect(within(second).getByRole("button", { name: "禁用" })).toBeEnabled();
  });

  it("reports a failed status change on the row it belongs to", async () => {
    vi.mocked(api.tokenGuardPoliciesPage).mockResolvedValue({
      items: [policy, { ...policy, id: "tgp_second", name: "Second guard" }], next_cursor: "",
    });
    vi.spyOn(api, "updateTokenGuardPolicy").mockRejectedValue(new ApiError(412, "resource revision conflict"));
    renderPage();

    const first = (await screen.findByText("Production guard")).closest("tr")!;
    const second = screen.getByText("Second guard").closest("tr")!;
    fireEvent.click(within(first).getByRole("button", { name: "禁用" }));
    const confirmation = await screen.findByRole("alertdialog");
    fireEvent.change(within(confirmation).getByLabelText("当前密码"), { target: { value: "a passphrase" } });
    fireEvent.click(within(confirmation).getByRole("button", { name: "禁用" }));

    expect(await within(first).findByText("无法完成请求")).toBeVisible();
    expect(within(second).queryByText("无法完成请求")).not.toBeInTheDocument();
  });

  it("loads the next cursor page on demand", async () => {
    vi.mocked(api.tokenGuardPoliciesPage)
      .mockResolvedValueOnce({ items: [policy], next_cursor: "cursor-1" })
      .mockResolvedValueOnce({ items: [{ ...policy, id: "tgp_second", name: "Second guard" }], next_cursor: "" });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "加载更多" }));
    expect(await screen.findByText("Second guard")).toBeVisible();
    expect(api.tokenGuardPoliciesPage).toHaveBeenLastCalledWith(expect.stringContaining("cursor=cursor-1"));
  });

  it("keeps one contextual create action in the active tab filter bar", async () => {
    renderPage();
    expect(await screen.findAllByRole("button", { name: "＋ 新建令牌防护策略" })).toHaveLength(1);
    fireEvent.click(screen.getByRole("tab", { name: "脱敏策略 0" }));
    expect(window.location.search).toBe("?tab=redaction");
    expect(screen.queryByRole("button", { name: "＋ 新建令牌防护策略" })).not.toBeInTheDocument();
    expect(screen.getAllByRole("button", { name: "＋ 新建脱敏策略" })).toHaveLength(1);
  });

  it("restores the active policy tab from the URL and follows browser history", async () => {
    window.history.replaceState({}, "", "/admin/policies?tab=redaction&project_id=project_a#policies");
    renderPage();

    expect(await screen.findByRole("tab", { name: "脱敏策略 0" })).toHaveAttribute("aria-selected", "true");
    fireEvent.click(screen.getByRole("tab", { name: "令牌防护策略 0" }));
    expect(window.location.search).toBe("?tab=token&project_id=project_a");
    expect(window.location.hash).toBe("#policies");

    window.history.replaceState({}, "", "/admin/policies?tab=redaction&project_id=project_a#policies");
    act(() => window.dispatchEvent(new PopStateEvent("popstate")));
    expect(screen.getByRole("tab", { name: "脱敏策略 0" })).toHaveAttribute("aria-selected", "true");
  });

  // APG tab behaviour. Without it the strip is two tab stops that only ever
  // activate the tab already under focus, and neither tab points at its panel.
  it("moves between policy tabs with the arrow keys and names its panel", async () => {
    renderPage();
    const tokenTab = await screen.findByRole("tab", { name: "令牌防护策略 0" });
    expect(tokenTab).toHaveAttribute("tabindex", "0");
    expect(screen.getByRole("tab", { name: "脱敏策略 0" })).toHaveAttribute("tabindex", "-1");
    expect(screen.getByRole("tabpanel")).toHaveAttribute("id", tokenTab.getAttribute("aria-controls"));
    expect(screen.getByRole("tabpanel")).toHaveAttribute("aria-labelledby", tokenTab.getAttribute("id"));

    fireEvent.keyDown(tokenTab, { key: "ArrowRight" });
    const redactionTab = screen.getByRole("tab", { name: "脱敏策略 0" });
    expect(redactionTab).toHaveAttribute("aria-selected", "true");
    expect(redactionTab).toHaveFocus();
    expect(screen.getByRole("tabpanel")).toHaveAttribute("aria-labelledby", redactionTab.getAttribute("id"));

    fireEvent.keyDown(redactionTab, { key: "ArrowLeft" });
    expect(screen.getByRole("tab", { name: "令牌防护策略 0" })).toHaveAttribute("aria-selected", "true");
  });

  it("falls back to token guard policies for an unknown tab parameter", async () => {
    window.history.replaceState({}, "", "/admin/policies?tab=unknown");
    renderPage();
    expect(await screen.findByRole("tab", { name: "令牌防护策略 0" })).toHaveAttribute("aria-selected", "true");
  });
});

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><PoliciesPage /></QueryClientProvider>);
}
