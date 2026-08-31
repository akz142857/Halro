import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api";
import { OperationsPage } from "./OperationsPage";

function webhook(overrides: Record<string, unknown> = {}) {
  return {
    id: "whk_1", name: "Security operations", url: "https://hooks.example.com/halro",
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

    // Read in the operator's language, with the server's identifier kept on the
    // element: the record is forensic, so the wire name has to stay reachable.
    expect(await screen.findByText("删除告警回调")).toHaveAttribute("title", "alert_webhook.delete");
    expect(screen.getByText(/admin → 告警回调 whk_1/)).toBeVisible();
    expect(screen.getByText("失败")).toHaveClass("danger");
    expect(screen.getByText("失败")).toHaveAttribute("title", "failure");
    // transport_error has no copy in either bundle. An action, outcome or reason
    // added upstream has to degrade to what this page printed before the audit
    // vocabulary existed — never to a raw i18next key.
    expect(screen.getByText(/transport_error/)).toBeVisible();
  });

  it("prints an untranslated action as the server's own identifier", async () => {
    vi.mocked(api.audit).mockResolvedValue({
      items: [auditRecord({ action: "future_resource.invented", target_type: "future_resource" })],
      next_cursor: "",
    } as never);

    renderPage();

    expect(await screen.findByText("future_resource.invented")).toBeVisible();
    expect(screen.getByText(/admin → future_resource whk_1/)).toBeVisible();
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
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "删除" }));

    await waitFor(() => expect(api.audit).toHaveBeenCalledTimes(2));
  });

  it("reports a failed deletion instead of leaving the row looking untouched", async () => {
    vi.mocked(api.alertsPage).mockResolvedValue({ items: [webhook()], next_cursor: "" } as never);
    vi.spyOn(api, "deleteAlert").mockRejectedValue(new ApiError(412, "resource revision conflict"));

    renderPage();
    const row = (await screen.findByText("Security operations")).closest(".alert-row")!;
    fireEvent.click(within(row as HTMLElement).getByRole("button", { name: "删除" }));
    const dialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(dialog).getByRole("button", { name: "删除" }));

    expect(await screen.findByText("无法完成请求")).toBeVisible();
  });

  // "Failed" alone sent the operator to a log that only repeats it: the
  // dispatcher had already classified the failure and the endpoint had already
  // answered, and the row showed neither.
  it("says how a webhook delivery test failed and what the endpoint answered", async () => {
    vi.mocked(api.alertsPage).mockResolvedValue({ items: [webhook()], next_cursor: "" } as never);
    vi.spyOn(api, "testAlert").mockRejectedValue(new ApiError(502, "alert delivery test failed", "http_client_error", '{"error":"unknown channel"}', {
      error: "alert delivery test failed", code: "http_client_error",
      status_code: 404, response: '{"error":"unknown channel"}',
    }));

    renderPage();
    const row = (await screen.findByText("Security operations")).closest(".alert-row")!;
    fireEvent.click(within(row as HTMLElement).getByRole("button", { name: "测试" }));

    const reason = await screen.findByText(/Webhook 端点拒绝了这次投递/);
    expect(reason).toHaveTextContent("HTTP 404");
    expect(reason).toHaveTextContent("unknown channel");
    // One box, not the classified sentence beside a generic "request failed".
    expect(screen.queryByText("无法完成请求")).not.toBeInTheDocument();
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
    // A cell in its own row's grid, not a panel-wide banner and not a stray box beside it.
    const reply = screen.getByText(/param invalid/).closest(".alert-reply")!;
    expect(reply).toBeVisible();
    expect(reply.parentElement).toBe(first);
    expect(within(reply as HTMLElement).getByText("接收端返回 HTTP 200")).toBeVisible();
    expect(screen.queryByText(/param invalid/)?.closest(".notice")).toBeFalsy();
    expect(within(second as HTMLElement).getAllByRole("status")[0]).toHaveTextContent("尚未测试");
    expect(within(second as HTMLElement).getByRole("button", { name: "测试" })).toBeEnabled();
  });
});
