import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "./api";
import { App } from "./App";

describe("App first-run routing", () => {
  beforeEach(() => {
    vi.spyOn(api, "uiBootstrap").mockResolvedValue({ default_locale: "zh-CN", supported_locales: ["zh-CN", "en-US"] });
  });
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
    document.documentElement.removeAttribute("data-appearance");
    window.history.replaceState({}, "", "/");
  });

  it("shows setup before attempting a login session", async () => {
    vi.spyOn(api, "setupStatus").mockResolvedValue({
      instance_initialized: true,
      setup_required: true,
      token_required: false,
    });
    const session = vi.spyOn(api, "session");
    renderApp();
    expect(await screen.findByRole("heading", { name: "设置管理员账户" })).toBeVisible();
    expect(session).not.toHaveBeenCalled();
  });

  it("uses the normal login flow after setup is complete", async () => {
    vi.spyOn(api, "setupStatus").mockResolvedValue({
      instance_initialized: true,
      setup_required: false,
      token_required: false,
    });
    vi.spyOn(api, "session").mockRejectedValue(new ApiError(401, "not authenticated"));
    renderApp();
    await waitFor(() => expect(screen.getByRole("heading", { name: "进入控制台" })).toBeVisible());
  });

  it("applies an authenticated Light preference and resets unauthenticated screens to Dark", async () => {
    vi.spyOn(api, "setupStatus").mockResolvedValue({ instance_initialized: true, setup_required: false, token_required: false });
    vi.spyOn(api, "session").mockResolvedValue({
      username: "admin", role: "administrator", locale: "system", appearance: "light", csrf_token: "csrf",
      absolute_expires_at: "x", idle_expires_at: "x",
    });
    renderApp();
    await waitFor(() => expect(document.documentElement).toHaveAttribute("data-appearance", "light"));

    cleanup();
    vi.restoreAllMocks();
    vi.spyOn(api, "uiBootstrap").mockResolvedValue({ default_locale: "zh-CN", supported_locales: ["zh-CN", "en-US"] });
    vi.spyOn(api, "setupStatus").mockResolvedValue({ instance_initialized: true, setup_required: false, token_required: false });
    vi.spyOn(api, "session").mockRejectedValue(new ApiError(401, "not authenticated"));
    renderApp();
    await screen.findByRole("heading", { name: "进入控制台" });
    expect(document.documentElement).toHaveAttribute("data-appearance", "dark");
  });

  it("renders only the restricted MFA setup surface when policy requires enrollment", async () => {
    window.history.replaceState({}, "", "/admin/providers");
    vi.spyOn(api, "setupStatus").mockResolvedValue({ instance_initialized: true, setup_required: false, token_required: false });
    vi.spyOn(api, "session").mockResolvedValue({ username: "admin", role: "administrator", locale: "system", appearance: "dark", csrf_token: "csrf", absolute_expires_at: "x", idle_expires_at: "x", mfa_setup_required: true });
    vi.spyOn(api, "mfaStatus").mockResolvedValue({ enabled: false, policy: "required", required: true, authenticators: [] });
    renderApp();
    expect(await screen.findByRole("heading", { name: "必须设置二次验证" })).toBeVisible();
    expect(screen.queryByRole("link", { name: /服务商/ })).not.toBeInTheDocument();
    expect(screen.queryByText("更改登录密码")).not.toBeInTheDocument();
  });

  it("keeps one-time recovery codes visible until the operator saves them", async () => {
    window.history.replaceState({}, "", "/admin/settings/security");
    vi.spyOn(api, "setupStatus").mockResolvedValue({ instance_initialized: true, setup_required: false, token_required: false });
    const requiredSession = {
      username: "new-admin", role: "administrator" as const, locale: "system" as const, appearance: "dark" as const,
      csrf_token: "csrf", absolute_expires_at: "x", idle_expires_at: "x", mfa_setup_required: true,
    };
    const session = vi.spyOn(api, "session")
      .mockResolvedValueOnce(requiredSession)
      .mockResolvedValue({ ...requiredSession, mfa_setup_required: false });
    vi.spyOn(api, "systemStatus").mockResolvedValue({ time_context: { accounting_timezone: "UTC" } } as never);
    vi.spyOn(api, "mfaStatus").mockResolvedValue({ enabled: false, policy: "required", required: true, authenticators: [] });
    vi.spyOn(api, "createMFAAuthenticator").mockResolvedValue({
      id: "mfa-new", name: "Phone", secret: "ABCDEFGHIJKLMNOP",
      otpauth_uri: "otpauth://totp/Halro:new-admin?secret=ABCDEFGHIJKLMNOP",
      expires_at: new Date(Date.now() + 60_000).toISOString(), revision: 1,
    });
    vi.spyOn(api, "confirmMFAAuthenticator").mockResolvedValue({
      status: "enabled", recovery_codes: ["RECOVERY-ONE", "RECOVERY-TWO"],
    });
    const copy = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText: copy } });

    const { client } = renderApp();
    expect(await screen.findByRole("heading", { name: "必须设置二次验证" })).toBeVisible();
    fireEvent.click(await screen.findByRole("button", { name: "添加…" }));
    fireEvent.change(screen.getByLabelText("身份验证器名称"), { target: { value: "Phone" } });
    fireEvent.change(screen.getByLabelText("当前密码"), { target: { value: "correct horse battery staple" } });
    fireEvent.click(screen.getByRole("button", { name: "继续" }));
    fireEvent.change(await screen.findByRole("textbox", { name: /验证码/ }), { target: { value: "123456" } });
    fireEvent.click(screen.getByRole("button", { name: "验证" }));

    expect(await screen.findByText(/RECOVERY-ONE/)).toBeVisible();
    // Simulate the focus/background refresh that used to observe the now-open
    // session and unmount this one-time response before it could be saved.
    await client.invalidateQueries({ queryKey: ["session"] });
    await waitFor(() => expect(session).toHaveBeenCalledTimes(2));
    expect(screen.getByText(/RECOVERY-ONE/)).toBeVisible();
    expect(screen.getByRole("heading", { name: "必须设置二次验证" })).toBeVisible();

    fireEvent.click(screen.getByRole("button", { name: "复制" }));
    await waitFor(() => expect(copy).toHaveBeenCalledWith("RECOVERY-ONE\nRECOVERY-TWO"));
    fireEvent.click(screen.getByRole("checkbox", { name: /我已将这些恢复码安全保存/ }));
    fireEvent.click(screen.getByRole("button", { name: "我已保存恢复码" }));

    await waitFor(() => expect(screen.queryByRole("heading", { name: "必须设置二次验证" })).not.toBeInTheDocument());
    expect(await screen.findByRole("button", { name: "更改登录密码" })).toBeVisible();
  });

  it("redirects the former root-key URL into Settings & Status", async () => {
    window.history.replaceState({}, "", "/admin/master-key");
    vi.spyOn(api, "setupStatus").mockResolvedValue({ instance_initialized: true, setup_required: false, token_required: false });
    vi.spyOn(api, "session").mockResolvedValue({ username: "admin", role: "administrator", locale: "system", appearance: "dark", csrf_token: "csrf", absolute_expires_at: "x", idle_expires_at: "x" });
    vi.spyOn(api, "systemStatus").mockResolvedValue({ time_context: { accounting_timezone: "UTC" } } as never);
    vi.spyOn(api, "masterKeyCustody").mockResolvedValue({
      mode: "file", local_custody_ready: true, custody_state: "healthy", production_admission: "not_applicable",
      rotation_incomplete: false, lifecycle_operation: "none", pending_slots: 0, retiring_slots: 0,
      recovery_verification_status: "not_applicable", degraded_reasons: [], slots: [],
    });

    renderApp();

    await waitFor(() => expect(window.location.pathname).toBe("/admin/settings/custody"));
    expect(await screen.findByRole("heading", { name: "根密钥状态" })).toBeVisible();
    await waitFor(() => expect(screen.getByRole("link", { name: "设置与状态" })).toHaveAttribute("aria-current", "page"));
    expect(screen.getByRole("link", { name: "根密钥状态" })).toHaveAttribute("aria-current", "page");
  });
});

function renderApp() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  const view = render(
    <QueryClientProvider client={client}>
      <App />
    </QueryClientProvider>,
  );
  return { ...view, client };
}
