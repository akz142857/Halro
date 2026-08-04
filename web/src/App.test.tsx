import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { cleanup, render, screen, waitFor } from "@testing-library/react";
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
      username: "admin", locale: "system", appearance: "light", csrf_token: "csrf",
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
    vi.spyOn(api, "session").mockResolvedValue({ username: "admin", locale: "system", appearance: "dark", csrf_token: "csrf", absolute_expires_at: "x", idle_expires_at: "x", mfa_setup_required: true });
    vi.spyOn(api, "mfaStatus").mockResolvedValue({ enabled: false, policy: "required", authenticators: [] });
    renderApp();
    expect(await screen.findByRole("heading", { name: "必须设置二次验证" })).toBeVisible();
    expect(screen.queryByRole("link", { name: /服务商/ })).not.toBeInTheDocument();
    expect(screen.queryByText("变更管理员密码")).not.toBeInTheDocument();
  });
});

function renderApp() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <App />
    </QueryClientProvider>,
  );
}
