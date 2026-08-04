import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import { AppearanceForm, LanguageSettingsForm, MFASettings, PasswordChangeForm } from "./SettingsPage";

function renderWithClient(node: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}>{node}</QueryClientProvider>);
}

describe("AppearanceForm", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    document.documentElement.removeAttribute("data-appearance");
  });

  it("previews instantly, persists the full resource, and confirms", async () => {
    const update = vi.spyOn(api, "updatePreferences").mockResolvedValue({
      data: { locale: "system", appearance: "light", revision: 6 }, etag: '"6"',
    });
    renderWithClient(<AppearanceForm preferences={{ locale: "system", appearance: "dark", revision: 5 }} />);

    fireEvent.click(screen.getByRole("radio", { name: /浅色/ }));
    // Instant preview: theme is applied before the network settles.
    expect(document.documentElement.getAttribute("data-appearance")).toBe("light");
    expect(update).toHaveBeenCalledWith({ locale: "system", appearance: "light" }, 5);
    await waitFor(() => expect(screen.getByText("外观设置已保存")).toBeInTheDocument());
    expect(document.documentElement.getAttribute("data-appearance")).toBe("light");
  });

  it("rolls back to the confirmed theme when saving fails", async () => {
    vi.spyOn(api, "updatePreferences").mockRejectedValue(new Error("network down"));
    renderWithClient(<AppearanceForm preferences={{ locale: "system", appearance: "dark", revision: 5 }} />);

    fireEvent.click(screen.getByRole("radio", { name: /浅色/ }));
    expect(document.documentElement.getAttribute("data-appearance")).toBe("light");
    await waitFor(() => expect(screen.getByRole("alert")).toBeInTheDocument());
    // Reverted to the last server-confirmed theme.
    expect(document.documentElement.getAttribute("data-appearance")).toBe("dark");
    expect(screen.getByRole("radio", { name: /深色/ })).toBeChecked();
  });

  it("retries the failed target after refreshing server truth", async () => {
    const update = vi.spyOn(api, "updatePreferences")
      .mockRejectedValueOnce(new Error("network down"))
      .mockResolvedValueOnce({ data: { locale: "system", appearance: "light", revision: 7 }, etag: '"7"' });
    renderWithClient(<AppearanceForm preferences={{ locale: "system", appearance: "dark", revision: 5 }} />);

    fireEvent.click(screen.getByRole("radio", { name: /浅色/ }));
    await screen.findByRole("alert");
    fireEvent.click(screen.getByRole("button", { name: "重试" }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(2));
    expect(update.mock.calls[1][0]).toEqual({ locale: "system", appearance: "light" });
    expect(document.documentElement.getAttribute("data-appearance")).toBe("light");
  });

  it("serializes rapid changes so the last explicit choice wins", async () => {
    let resolveFirst!: (value: Awaited<ReturnType<typeof api.updatePreferences>>) => void;
    const first = new Promise<Awaited<ReturnType<typeof api.updatePreferences>>>((resolve) => { resolveFirst = resolve; });
    const update = vi.spyOn(api, "updatePreferences")
      .mockReturnValueOnce(first)
      .mockResolvedValueOnce({ data: { locale: "system", appearance: "dark", revision: 7 }, etag: '"7"' });
    renderWithClient(<AppearanceForm preferences={{ locale: "system", appearance: "dark", revision: 5 }} />);

    fireEvent.click(screen.getByRole("radio", { name: /浅色/ }));
    fireEvent.click(screen.getByRole("radio", { name: /深色/ }));
    expect(document.documentElement.getAttribute("data-appearance")).toBe("dark");
    resolveFirst({ data: { locale: "system", appearance: "light", revision: 6 }, etag: '"6"' });

    await waitFor(() => expect(update).toHaveBeenCalledTimes(2));
    expect(update.mock.calls[1]).toEqual([{ locale: "system", appearance: "dark" }, 6]);
    await screen.findByText("外观设置已保存");
    expect(document.documentElement.getAttribute("data-appearance")).toBe("dark");
  });
});

describe("PasswordChangeForm", () => {
  afterEach(() => vi.restoreAllMocks());
  it("validates confirmation, rotates the session, and clears password inputs", async () => {
    const change = vi.spyOn(api, "changePassword").mockResolvedValue({
      username: "admin",
      locale: "system",
      appearance: "dark",
      csrf_token: "rotated",
      absolute_expires_at: "2026-01-01T00:00:00Z",
      idle_expires_at: "2026-01-01T00:00:00Z",
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><PasswordChangeForm /></QueryClientProvider>);

    fireEvent.click(screen.getByRole("button", { name: "变更管理员密码" }));
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
    expect(current).not.toBeInTheDocument();
    expect(next).not.toBeInTheDocument();
    expect(confirmation).not.toBeInTheDocument();
  });
});

describe("LanguageSettingsForm", () => {
  afterEach(() => vi.restoreAllMocks());

  it("applies administrator language immediately and saves instance language explicitly", async () => {
    const updatePreferences = vi.spyOn(api, "updatePreferences").mockResolvedValue({
      data: { locale: "en-US", appearance: "dark", revision: 4 }, etag: '"4"',
    });
    const updateUISettings = vi.spyOn(api, "updateUISettings").mockResolvedValue({
      data: { default_locale: "en-US", revision: 8, updated_at: "2026-01-01T00:00:00Z" }, etag: '"8"',
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <LanguageSettingsForm
          preferences={{ locale: "system", appearance: "dark", revision: 3 }}
          ui={{ default_locale: "zh-CN", revision: 7, updated_at: "2026-01-01T00:00:00Z" }}
        />
      </QueryClientProvider>,
    );

    const instanceLocale = screen.getByLabelText("实例默认语言");
    const saveInstance = screen.getByRole("button", { name: "保存实例默认语言" });
    fireEvent.change(screen.getByLabelText("界面语言"), { target: { value: "en-US" } });
    await waitFor(() => expect(updatePreferences).toHaveBeenCalledWith({ locale: "en-US", appearance: "dark" }, 3));
    fireEvent.change(instanceLocale, { target: { value: "en-US" } });
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

  it("does not claim MFA is optional while status is still loading", () => {
    vi.spyOn(api, "mfaStatus").mockReturnValue(new Promise(() => {}));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MFASettings /></QueryClientProvider>);
    expect(screen.getByText("正在加载登录安全状态")).toBeVisible();
    expect(screen.queryByText("可选")).not.toBeInTheDocument();
  });

  it("keeps recovery regeneration open after an error", async () => {
    vi.spyOn(api, "mfaStatus").mockResolvedValue({ enabled: true, policy: "required", recovery_codes_remaining: 4, authenticators: [] });
    vi.spyOn(api, "regenerateMFARecoveryCodes").mockRejectedValue(new Error("offline"));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MFASettings /></QueryClientProvider>);
    fireEvent.click(await screen.findByRole("button", { name: "生成新的恢复码" }));
    const form = screen.getByRole("button", { name: "生成新的恢复码" }).closest("form")!;
    const [password, code] = Array.from(form.querySelectorAll("input"));
    fireEvent.change(password, { target: { value: "current password" } });
    fireEvent.change(code, { target: { value: "123456" } });
    fireEvent.click(screen.getByRole("button", { name: "生成新的恢复码" }));
    expect(await screen.findByRole("alert")).toBeVisible();
    expect(screen.getByRole("heading", { name: "生成新的恢复码" })).toBeVisible();
  });

  it("restores focus after cancelling an authenticator action", async () => {
    vi.spyOn(api, "mfaStatus").mockResolvedValue({ enabled: true, policy: "required", authenticators: [{ id: "mfa-1", name: "Phone", type: "totp", created_at: "2026-01-01T00:00:00Z", revision: 3 }] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><MFASettings /></QueryClientProvider>);
    const trigger = await screen.findByRole("button", { name: "重命名" });
    fireEvent.click(trigger);
    fireEvent.click(screen.getByRole("button", { name: "取消" }));
    await waitFor(() => expect(trigger).toHaveFocus());
  });
});
