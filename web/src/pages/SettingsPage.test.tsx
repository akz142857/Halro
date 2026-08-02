import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import { LanguageSettingsForm, MFASettings, PasswordChangeForm } from "./SettingsPage";

describe("PasswordChangeForm", () => {
  afterEach(() => vi.restoreAllMocks());
  it("validates confirmation, rotates the session, and clears password inputs", async () => {
    const change = vi.spyOn(api, "changePassword").mockResolvedValue({
      username: "admin",
      locale: "system",
      csrf_token: "rotated",
      absolute_expires_at: "2026-01-01T00:00:00Z",
      idle_expires_at: "2026-01-01T00:00:00Z",
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><PasswordChangeForm /></QueryClientProvider>);

    const [current, next, confirmation] = screen.getAllByLabelText(/密码/);
    fireEvent.change(current, { target: { value: "old secure password" } });
    fireEvent.change(next, { target: { value: "new secure password" } });
    fireEvent.change(confirmation, { target: { value: "different password" } });
    fireEvent.click(screen.getByRole("button", { name: "变更管理员密码" }));
    expect(await screen.findByRole("alert")).toHaveTextContent("不一致");
    expect(change).not.toHaveBeenCalled();

    fireEvent.change(confirmation, { target: { value: "new secure password" } });
    fireEvent.click(screen.getByRole("button", { name: "变更管理员密码" }));
    await waitFor(() => expect(change).toHaveBeenCalledWith("old secure password", "new secure password"));
    expect(await screen.findByRole("status")).toHaveTextContent("会话已安全轮换");
    expect(current).toHaveValue("");
    expect(next).toHaveValue("");
    expect(confirmation).toHaveValue("");
  });
});

describe("LanguageSettingsForm", () => {
  afterEach(() => vi.restoreAllMocks());

  it("saves administrator and instance language independently", async () => {
    const updatePreferences = vi.spyOn(api, "updatePreferences").mockResolvedValue({
      data: { locale: "en-US", revision: 4 }, etag: '"4"',
    });
    const updateUISettings = vi.spyOn(api, "updateUISettings").mockResolvedValue({
      data: { default_locale: "en-US", revision: 8, updated_at: "2026-01-01T00:00:00Z" }, etag: '"8"',
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <LanguageSettingsForm
          preferences={{ locale: "system", revision: 3 }}
          ui={{ default_locale: "zh-CN", revision: 7, updated_at: "2026-01-01T00:00:00Z" }}
        />
      </QueryClientProvider>,
    );

    fireEvent.change(screen.getByLabelText("界面语言"), { target: { value: "en-US" } });
    fireEvent.change(screen.getByLabelText("实例默认语言"), { target: { value: "en-US" } });
    const saveInstance = screen.getByRole("button", { name: "保存实例默认语言" });
    fireEvent.click(screen.getByRole("button", { name: "保存我的界面语言" }));

    await waitFor(() => expect(updatePreferences).toHaveBeenCalledWith("en-US", 3));
    expect(updateUISettings).not.toHaveBeenCalled();

    fireEvent.click(saveInstance);
    await waitFor(() => expect(updateUISettings).toHaveBeenCalledWith("en-US", 7));
  });
});

describe("MFASettings", () => {
  afterEach(() => vi.restoreAllMocks());

  it("shows authenticator metadata and renames with its revision", async () => {
    vi.spyOn(api, "mfaStatus").mockResolvedValue({ enabled: true, policy: "optional", recovery_codes_remaining: 7, authenticators: [{ id: "mfa-1", name: "Phone", type: "totp", created_at: "2026-01-01T00:00:00Z", revision: 3 }] });
    const rename = vi.spyOn(api, "renameMFAAuthenticator").mockResolvedValue({ status: "renamed" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MFASettings /></QueryClientProvider>);
    expect(await screen.findByText("剩余恢复码：7")).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "重命名" }));
    fireEvent.change(screen.getAllByLabelText("身份验证器名称")[0], { target: { value: "Work phone" } });
    fireEvent.click(screen.getByRole("button", { name: "保存" }));
    await waitFor(() => expect(rename).toHaveBeenCalledWith("mfa-1", "Work phone", 3));
  });

  it("requires explicit confirmation before disabling optional MFA", async () => {
    vi.spyOn(api, "mfaStatus").mockResolvedValue({ enabled: true, policy: "optional", authenticators: [] });
    const disable = vi.spyOn(api, "disableMFA").mockResolvedValue({ status: "disabled" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MFASettings /></QueryClientProvider>);
    fireEvent.click(await screen.findByRole("button", { name: "关闭二次验证" }));
    const submit = screen.getAllByRole("button", { name: "关闭二次验证" })[0];
    expect(submit).toBeDisabled();
    fireEvent.click(screen.getByRole("checkbox"));
    expect(submit).toBeEnabled();
    expect(disable).not.toHaveBeenCalled();
  });
});
