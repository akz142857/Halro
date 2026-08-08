import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import { AppearanceForm } from "./AppearanceForm";
import { LanguageSettingsForm } from "./LanguageSettingsForm";
import { MFASettings } from "./MFASettings";
import { PasswordChangeForm } from "./PasswordChangeForm";
import { SettingsPage } from "./SettingsPage";
import { emptyWritePath } from "../test/fixtures";

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
      role: "administrator",
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

// 系统配置 is its own pane, a sibling of the others in the settings nav, not a
// card at the bottom of one. Moving it means the query moves too: left enabled
// on a pane that no longer renders the card it would fetch for nobody, and the
// pane that does render it would never fetch.
describe("SettingsPage system configuration pane", () => {
  afterEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/");
  });

  it("is reachable as its own menu entry and renders config.yaml open", async () => {
    const systemConfig = vi.spyOn(api, "systemConfig").mockResolvedValue({ yaml: "server:\n  address: 127.0.0.1\n" } as never);
    window.history.replaceState({}, "", "/admin/settings/config");
    renderWithClient(<SettingsPage />);

    // The nav entry sits between the instance pane and the diagnostics pane.
    const entries = [...screen.getByRole("navigation", { name: "设置分区" }).querySelectorAll("a")].map((a) => a.textContent);
    expect(entries).toEqual(["通用", "登录与安全", "管理员账户", "实例配置", "系统配置", "关于与诊断"]);

    expect(await screen.findByText("config.yaml")).toBeInTheDocument();
    expect(screen.getByText(/server:/)).toBeInTheDocument();
    // The pane's only content must not arrive folded away.
    expect(document.querySelector(".config-preview")).toHaveAttribute("open");
    expect(systemConfig).toHaveBeenCalled();
  });

  it("no longer renders it, or fetches it, under diagnostics", async () => {
    const systemConfig = vi.spyOn(api, "systemConfig").mockResolvedValue({ yaml: "should not be fetched" } as never);
    vi.spyOn(api, "systemStatus").mockResolvedValue({
      build: { version: "1.0.0", commit: "abc", date: "2026-08-07" },
      accounting_status: 0, draining: false, wal: {}, write_path: emptyWritePath(), audit: {}, alerts: {}, usage_watermark: {},
    } as never);
    window.history.replaceState({}, "", "/admin/settings/diagnostics");
    renderWithClient(<SettingsPage />);

    await screen.findByText("Heimdall 1.0.0");
    expect(screen.queryByText("config.yaml")).not.toBeInTheDocument();
    expect(systemConfig).not.toHaveBeenCalled();
  });

  // The card exists so an operator can read this instance's ceiling without
  // standing up Prometheus, so the assertion is on the rendered numbers rather
  // than on the card being present.
  it("reports the durable write path under diagnostics", async () => {
    vi.spyOn(api, "systemConfig").mockResolvedValue({ yaml: "" } as never);
    vi.spyOn(api, "systemStatus").mockResolvedValue({
      build: { version: "1.0.0", commit: "abc", date: "2026-08-07" },
      accounting_status: 0, draining: false, wal: {},
      write_path: emptyWritePath({
        wal_sync_seconds: 0.0043,
        wal_batch_size: 8.25,
        project_lock_held_seconds: 0.0221,
        project_events_per_second: 45.2,
        project_requests_per_second: 9.04,
      }),
      audit: {}, alerts: {}, usage_watermark: {},
    } as never);
    window.history.replaceState({}, "", "/admin/settings/diagnostics");
    renderWithClient(<SettingsPage />);

    // Sub-millisecond and tens-of-milliseconds have to stay readable in the same
    // table, so the unit keeps decimals rather than rounding an NVMe fsync to 0.
    expect(await screen.findByText("4.30 ms")).toBeInTheDocument();
    expect(screen.getByText("22.1 ms")).toBeInTheDocument();
    expect(screen.getByText("8.25")).toBeInTheDocument();
    expect(screen.getByText("45.2")).toBeInTheDocument();
    // The answer belongs on the collapsed row: an operator asking "how many
    // requests per second can this take" should not have to expand a card and
    // divide by five to find out.
    expect(screen.getByText("≈ 9.04 请求/秒")).toBeInTheDocument();
  });

  // A batch size of 1.0 and a saturated disk read the same from a latency graph,
  // and only the first is fixed by adding concurrency. The card has to say which
  // one it is looking at, and must stay quiet until the reading is real.
  it("says when appends are not coalescing, and only once traffic makes that real", async () => {
    vi.spyOn(api, "systemConfig").mockResolvedValue({ yaml: "" } as never);
    vi.spyOn(api, "systemStatus").mockResolvedValue({
      build: { version: "1.0.0", commit: "abc", date: "2026-08-07" },
      accounting_status: 0, draining: false, wal: { batches: 400 },
      write_path: emptyWritePath({ wal_sync_seconds: 0.004, wal_batch_size: 1 }),
      audit: {}, alerts: {}, usage_watermark: {},
    } as never);
    window.history.replaceState({}, "", "/admin/settings/diagnostics");
    renderWithClient(<SettingsPage />);
    const warning = await screen.findByText(/未合批/);
    expect(warning).toBeInTheDocument();
    // Rendered as the shared notice component rather than another paragraph, so
    // a conditional warning does not read at the same weight as the table.
    expect(warning).toHaveClass("notice", "warning");
  });

  it("stays quiet about coalescing on an instance with almost no traffic", async () => {
    vi.spyOn(api, "systemConfig").mockResolvedValue({ yaml: "" } as never);
    vi.spyOn(api, "systemStatus").mockResolvedValue({
      build: { version: "1.0.0", commit: "abc", date: "2026-08-07" },
      accounting_status: 0, draining: false, wal: { batches: 3 },
      write_path: emptyWritePath({ wal_sync_seconds: 0.004, wal_batch_size: 1 }),
      audit: {}, alerts: {}, usage_watermark: {},
    } as never);
    window.history.replaceState({}, "", "/admin/settings/diagnostics");
    renderWithClient(<SettingsPage />);
    await screen.findByText("Heimdall 1.0.0");
    expect(screen.queryByText(/未合批/)).not.toBeInTheDocument();
  });

  // An instance that has served nothing has no means to report. Zero is what the
  // server sends, and dividing by it must not reach the screen as NaN.
  it("says so when no durable write has happened yet", async () => {
    vi.spyOn(api, "systemConfig").mockResolvedValue({ yaml: "" } as never);
    vi.spyOn(api, "systemStatus").mockResolvedValue({
      build: { version: "1.0.0", commit: "abc", date: "2026-08-07" },
      accounting_status: 0, draining: false, wal: {}, write_path: emptyWritePath(),
      audit: {}, alerts: {}, usage_watermark: {},
    } as never);
    window.history.replaceState({}, "", "/admin/settings/diagnostics");
    renderWithClient(<SettingsPage />);

    await screen.findByText("Heimdall 1.0.0");
    expect(screen.queryByText(/NaN/)).not.toBeInTheDocument();
    // The rows stay on screen with no data behind them. An idle instance asking
    // this card a question should get "— over 0 barriers", which is an answer;
    // replacing the table with prose is what made the first reader ask what the
    // panel was for.
    expect(screen.getByText("每次落盘耗时")).toBeInTheDocument();
    // An empty card that does not say what would fill it sends the reader to
    // ask someone. The Admin console never writes to the Ledger, so nothing
    // done on this page can populate these figures — say so here rather than
    // leaving "no data" to be interpreted as a fault.
    expect(screen.getByText(/Gateway/)).toBeInTheDocument();
    // "this instance" was ambiguous between the data directory and the current
    // run: a restarted instance reports thousands of requests_total (replayed
    // from the Ledger) beside zero here, and the first reader to hit that asked
    // whether the card was broken.
    expect(screen.getByText(/本次启动/)).toBeInTheDocument();
  });

  it("keeps an unknown pane on general rather than blanking the page", () => {
    window.history.replaceState({}, "", "/admin/settings/not-a-pane");
    vi.spyOn(api, "uiSettings").mockResolvedValue({ data: { default_locale: "zh-CN", revision: 1 }, etag: '"1"' });
    vi.spyOn(api, "preferences").mockResolvedValue({ data: { locale: "system", appearance: "dark", revision: 1 }, etag: '"1"' });
    renderWithClient(<SettingsPage />);

    expect(screen.getByRole("link", { name: "通用" })).toHaveAttribute("aria-current", "page");
  });
});
