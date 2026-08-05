import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api";
import { OperationsPage } from "./OperationsPage";

function webhook(overrides: Record<string, unknown> = {}) {
  return {
    id: "whk_1", name: "Security operations", url: "https://hooks.example.com/heimdall",
    header_name: "authorization", secret_configured: true, enabled: true, revision: 1,
    ...overrides,
  };
}

function auditRecord(overrides: Record<string, unknown> = {}) {
  return {
    sequence: 7, event_id: "aud_1", occurred_at: "2026-08-05T10:00:00Z",
    actor_type: "admin_user", actor_id: "admin", action: "alert_webhook.create",
    target_type: "alert_webhook", target_id: "whk_1", outcome: "success", ...overrides,
  };
}

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><OperationsPage /></QueryClientProvider>);
}

describe("operations page", () => {
  beforeEach(() => {
    vi.spyOn(api, "alertsPage").mockResolvedValue({ items: [], next_cursor: "" } as never);
    vi.spyOn(api, "audit").mockResolvedValue({ items: [], next_cursor: "" } as never);
  });

  afterEach(() => vi.restoreAllMocks());

  it("renders the flat audit fields the list endpoint serves", async () => {
    // The stored record nests everything under `event`; reading it nested here would show
    // a column of sequence numbers next to blank rows.
    vi.mocked(api.audit).mockResolvedValue({
      items: [auditRecord({ action: "alert_webhook.delete", outcome: "failure", reason_code: "transport_error" })],
      next_cursor: "",
    } as never);

    renderPage();

    expect(await screen.findByText("alert_webhook.delete")).toBeVisible();
    expect(screen.getByText(/admin → alert_webhook whk_1/)).toBeVisible();
    expect(screen.getByText("failure")).toHaveClass("danger");
  });

  it("binds the chain badge to whether the audit replay actually succeeded", async () => {
    // Every replay re-verifies the HMAC chain server-side. A hardcoded green pill would
    // keep claiming the chain is intact next to an error panel.
    vi.mocked(api.audit).mockRejectedValue(new ApiError(503, "audit unavailable"));

    renderPage();

    // Rendered twice on purpose: the sr-only StatusDot label plus the visible pill text.
    expect(await screen.findAllByText("审计链状态未知")).toHaveLength(2);
    expect(screen.queryByText("审计链运行中")).not.toBeInTheDocument();
  });

  it("follows next_cursor so a webhook past the first page stays manageable", async () => {
    vi.mocked(api.alertsPage)
      .mockResolvedValueOnce({ items: [webhook()], next_cursor: "whk_1" } as never)
      .mockResolvedValueOnce({ items: [webhook({ id: "whk_2", name: "Pager" })], next_cursor: "" } as never);

    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "加载更多" }));
    expect(await screen.findByText("Pager")).toBeVisible();
    expect(api.alertsPage).toHaveBeenLastCalledWith("?limit=100&cursor=whk_1");
  });

  it("creates a webhook without a secret", async () => {
    // The field is labelled optional and the server accepts a webhook with no credential.
    const createAlert = vi.spyOn(api, "createAlert").mockResolvedValue({ data: webhook(), etag: '"1"' } as never);

    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建告警 Webhook" }));
    fireEvent.change(screen.getByLabelText("名称"), { target: { value: "Pager" } });
    fireEvent.change(screen.getByLabelText(/HTTPS Webhook 地址/), { target: { value: "https://hooks.example.com/x" } });
    fireEvent.click(screen.getByRole("button", { name: "保存 Webhook" }));

    await waitFor(() => expect(createAlert).toHaveBeenCalledTimes(1));
    expect(createAlert.mock.calls[0][0]).not.toHaveProperty("secret");
  });

  it("refuses a non-https address before the request leaves the browser", async () => {
    const createAlert = vi.spyOn(api, "createAlert");

    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建告警 Webhook" }));
    fireEvent.change(screen.getByLabelText("名称"), { target: { value: "Pager" } });
    fireEvent.change(screen.getByLabelText(/HTTPS Webhook 地址/), { target: { value: "http://internal.local/hook" } });
    fireEvent.click(screen.getByRole("button", { name: "保存 Webhook" }));

    expect(await screen.findByText("请输入以 https:// 开头的完整地址")).toBeVisible();
    expect(createAlert).not.toHaveBeenCalled();
  });

  it("demands the secret again before re-pointing a webhook that already holds one", async () => {
    // The console never reveals a stored secret, so silently rebinding it to a new host
    // would post a credential the operator has never seen to a destination they picked.
    vi.mocked(api.alertsPage).mockResolvedValue({ items: [webhook()], next_cursor: "" } as never);
    const updateAlert = vi.spyOn(api, "updateAlert");

    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "编辑" }));
    fireEvent.change(screen.getByLabelText(/HTTPS Webhook 地址/), { target: { value: "https://attacker.example.com/collect" } });
    fireEvent.click(screen.getByRole("button", { name: "保存 Webhook" }));

    const problem = await screen.findByRole("alert");
    expect(problem).toHaveTextContent(/更换地址或请求头时必须重新输入密钥/);
    expect(updateAlert).not.toHaveBeenCalled();
  });

  it("keeps the typed secret when saving fails", async () => {
    // Clearing it on failure turns the operator's retry from "replace the secret" into
    // "keep the old one" without saying so.
    vi.mocked(api.alertsPage).mockResolvedValue({ items: [webhook()], next_cursor: "" } as never);
    const updateAlert = vi.spyOn(api, "updateAlert").mockRejectedValue(new ApiError(412, "resource revision conflict"));

    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "编辑" }));
    const secretField = screen.getByLabelText(/新密钥/);
    fireEvent.change(secretField, { target: { value: "rotated-secret" } });
    fireEvent.click(screen.getByRole("button", { name: "保存 Webhook" }));

    await waitFor(() => expect(updateAlert).toHaveBeenCalledTimes(1));
    expect(secretField).toHaveValue("rotated-secret");
  });

  it("refreshes the audit trail after a webhook mutation", async () => {
    // The page promises every management action lands in the trail below it.
    vi.mocked(api.alertsPage).mockResolvedValue({ items: [webhook()], next_cursor: "" } as never);
    vi.mocked(api.audit).mockResolvedValue({ items: [auditRecord()], next_cursor: "" } as never);
    vi.spyOn(api, "deleteAlert").mockResolvedValue({ data: undefined, etag: "" } as never);

    renderPage();
    const row = (await screen.findByText("Security operations")).closest(".alert-row")!;
    fireEvent.click(within(row as HTMLElement).getByRole("button", { name: "删除" }));
    fireEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "删除" }));

    await waitFor(() => expect(api.audit).toHaveBeenCalledTimes(2));
  });

  it("reports a failed deletion instead of leaving the row looking untouched", async () => {
    vi.mocked(api.alertsPage).mockResolvedValue({ items: [webhook()], next_cursor: "" } as never);
    vi.spyOn(api, "deleteAlert").mockRejectedValue(new ApiError(412, "resource revision conflict"));

    renderPage();
    const row = (await screen.findByText("Security operations")).closest(".alert-row")!;
    fireEvent.click(within(row as HTMLElement).getByRole("button", { name: "删除" }));
    fireEvent.click(within(await screen.findByRole("alertdialog")).getByRole("button", { name: "删除" }));

    expect(await screen.findByText("无法完成请求")).toBeVisible();
  });

  it("keeps a test result attached to the row that produced it", async () => {
    // A page-wide notice cannot say which endpoint answered.
    vi.mocked(api.alertsPage).mockResolvedValue({
      items: [webhook(), webhook({ id: "whk_2", name: "Pager" })], next_cursor: "",
    } as never);
    vi.spyOn(api, "testAlert").mockResolvedValue({
      status: "delivered", latency_ms: 42, status_code: 200, response: '{"code":19024,"msg":"param invalid"}',
    } as never);

    renderPage();
    const first = (await screen.findByText("Security operations")).closest(".alert-row")!;
    const second = screen.getByText("Pager").closest(".alert-row")!;
    fireEvent.click(within(first as HTMLElement).getByRole("button", { name: "测试" }));

    // The measured latency has to reach the label, or it renders the raw "{{latency}}ms".
    await waitFor(() =>
      expect(within(first as HTMLElement).getAllByRole("status")[0]).toHaveTextContent("通过 · 42ms"));
    // An endpoint that answers 200 and rejects the payload in its body must not read as a
    // clean delivery.
    expect(screen.getByText(/param invalid/)).toBeVisible();
    expect(screen.getByText("接收端返回 HTTP 200")).toBeVisible();
    expect(within(second as HTMLElement).getAllByRole("status")[0]).toHaveTextContent("尚未测试");
    expect(within(second as HTMLElement).getByRole("button", { name: "测试" })).toBeEnabled();
  });
});
