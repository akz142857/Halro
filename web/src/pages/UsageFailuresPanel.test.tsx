import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "../api";
import { navigate } from "../navigation";
import { UsageFailuresPanel } from "./UsageFailuresPanel";
import type { RequestFailure } from "../types";

const providerFailure: RequestFailure = {
  request_id: "req_failed", project_id: "project_a", requested_model: "chat",
  outcome: "provider_error", sequence: 12,
  accepted_at: "2026-08-21T10:01:00Z", completed_at: "2026-08-21T10:01:02Z",
  attempts: 2, fallbacks: 1,
  last_failure: {
    attempt_id: "att_2", attempt: 2, error_class: "authentication", provider_status: 401,
    provider_id: "provider_b", deployment_id: "dep_b", provider_model: "gpt-4o",
    offering_id: "bigmodel.coding-plan", profile_id: "bigmodel.cn.coding.chat.v1",
    provider_code: "invalid_api_key", provider_request_id: "upstream-req-77",
    failure_phase: "provider", completed_at: "2026-08-21T10:01:02Z",
  },
};

const policyRejection: RequestFailure = {
  request_id: "req_rejected", project_id: "project_a", requested_model: "chat",
  outcome: "rejected", sequence: 20,
  accepted_at: "2026-08-21T10:02:00Z", completed_at: "2026-08-21T10:02:00Z",
  attempts: 0, fallbacks: 0,
};

// The row shows a summary; everything else is one click away in a dialog.
async function openFailureDetail() {
  fireEvent.click(screen.getByRole("button", { name: "失败详情" }));
  return screen.findByRole("dialog");
}

function renderPanel(items: RequestFailure[]) {
  vi.spyOn(api, "usageFailures").mockResolvedValue({ items, next_cursor: "" });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><UsageFailuresPanel /></QueryClientProvider>);
}

describe("UsageFailuresPanel", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/usage?tab=failures");
    vi.spyOn(api, "projects").mockResolvedValue({ items: [{ id: "project_a", name: "Alpha" }] as never, next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [{ id: "dep_b", name: "Backup" }] as never, next_cursor: "" });
  });

  it("explains a provider failure with the attempt that decided it", async () => {
    renderPanel([providerFailure]);

    expect(await screen.findByText("服务商认证或权限被拒")).toBeVisible();
    expect(screen.getByText("HTTP 401")).toBeVisible();
    // The deployment that actually failed, by name, so the operator can go
    // straight to it rather than reading an ID off a chain elsewhere.
    expect(screen.getByRole("link", { name: "Backup" })).toBeVisible();
    // The Request ID opens the attempt list already filtered to it, where the
    // whole chain lives — rather than a second renderer of the same record.
    const request = screen.getByRole("link", { name: /req_failed/ });
    const url = new URL(request.getAttribute("href") ?? "", "https://console.test");
    expect(url.searchParams.get("tab")).toBe("attempts");
    expect(url.searchParams.get("request_id")).toBe("req_failed");
  });

  // The row with nothing upstream to blame. Naming a class or a deployment here
  // would send the operator to audit a provider that was never called.
  it("names a policy rejection as one, with no provider context", async () => {
    renderPanel([policyRejection]);

    expect(await screen.findByText(/策略拒绝：预算、熔断或并发上限/)).toBeVisible();
    expect(screen.getByText(/未选定目标/)).toBeVisible();
    expect(screen.queryByText("HTTP 401")).not.toBeInTheDocument();
    // And the detail explains why there is nothing upstream to name.
    const dialog = await openFailureDetail();
    expect(within(dialog).getByText(/从未调用上游/)).toBeVisible();
  });

  // The two identifiers a support desk asks for, kept in the ledger so they
  // outlive the process log that used to be their only home.
  it("carries the upstream's own code and request ID into the detail", async () => {
    renderPanel([providerFailure]);
    await screen.findByText("服务商认证或权限被拒");
    const dialog = await openFailureDetail();

    expect(within(dialog).getByText("invalid_api_key")).toBeVisible();
    expect(within(dialog).getByText("upstream-req-77")).toBeVisible();
		expect(within(dialog).getByText("GLM Coding Plan（订阅）")).toBeVisible();
		expect(within(dialog).getByText("GLM Coding Plan · Chat（中国大陆 /api/coding/paas/v4）")).toBeVisible();
    // Which attempt produced the class the row shows. Without it a two-attempt
    // request reads as if either could have.
    expect(within(dialog).getByText(/由第 2 次尝试决定/)).toBeVisible();
  });

  it("distinguishes recorded false failure semantics from an old unrecorded row", async () => {
    renderPanel([{
      ...providerFailure,
      last_failure: {
        ...providerFailure.last_failure!,
        provider_failure_reason: "invalid_credential",
        failure_semantics_recorded: true,
        retryable: false,
        ambiguous: false,
      },
    }]);
    await screen.findByText("服务商认证或权限被拒");
    const dialog = await openFailureDetail();

    expect(within(dialog).getByText("凭据无效")).toBeVisible();
    expect(within(dialog).getByText("不可重试")).toBeVisible();
    expect(within(dialog).getByText("已确认上游未执行且不会计费")).toBeVisible();
    expect(within(dialog).queryByText("旧记录未采集")).not.toBeInTheDocument();
  });

  // A row from before those fields were kept says so, rather than showing a
  // blank that reads as "the upstream named none".
  it("says a row predates the identifiers rather than showing them empty", async () => {
    renderPanel([{
      ...providerFailure,
      last_failure: {
        attempt_id: "att_2", attempt: 2, error_class: "authentication",
        provider_status: 401, deployment_id: "dep_b",
        completed_at: "2026-08-21T10:01:02Z",
      },
    }]);
    await screen.findByText("服务商认证或权限被拒");
    const dialog = await openFailureDetail();

    expect(within(dialog).getByText(/未保存服务商错误码与请求标识/)).toBeVisible();
  });

  // A policy refusal has no upstream, so it gets neither the identifiers nor a
  // notice that they were not recorded — nothing was ever asked for.
  it("offers no identifier notice on a policy refusal", async () => {
    renderPanel([policyRejection]);
    await screen.findByText(/策略拒绝：预算、熔断或并发上限/);
    const dialog = await openFailureDetail();

    expect(within(dialog).queryByText(/未保存服务商错误码/)).not.toBeInTheDocument();
  });

  // The filter this list is most often opened with: a caller reports an ID from
  // a failed call, and the operator needs that one request rather than a
  // population to narrow. Matched exactly by the server, so the box is only
  // useful if what is typed reaches it verbatim.
  it("filters by Request ID and carries one it was linked with", async () => {
    window.history.replaceState({}, "", "/admin/usage?tab=failures&request_id=req_failed");
    renderPanel([providerFailure]);

    expect(await screen.findByRole("textbox", { name: "Request ID" })).toHaveValue("req_failed");
    await waitFor(() => expect(api.usageFailures).toHaveBeenCalled());
    const query = (api.usageFailures as unknown as { mock: { calls: [string][] } }).mock.calls[0][0] ?? "";
    expect(new URLSearchParams(query.slice(1)).get("request_id")).toBe("req_failed");
  });

  it("resynchronizes every filter after same-tab query navigation", async () => {
    window.history.replaceState({}, "", "/admin/usage?tab=failures&request_id=req_old");
    renderPanel([]);
    expect(await screen.findByRole("textbox", { name: "Request ID" })).toHaveValue("req_old");

    act(() => navigate("/admin/usage?tab=failures&request_id=req_new&project_id=project_new&deployment_id=dep_new&offering_id=bigmodel.coding-plan&start=2026-09-01T00%3A00%3A00Z&end=2026-09-02T00%3A00%3A00Z"));

    await waitFor(() => expect(screen.getByRole("textbox", { name: "Request ID" })).toHaveValue("req_new"));
    const calls = (api.usageFailures as unknown as { mock: { calls: [string][] } }).mock.calls;
    await waitFor(() => {
      const params = new URLSearchParams((calls.at(-1)?.[0] ?? "").slice(1));
      expect(Object.fromEntries(params)).toMatchObject({
        request_id: "req_new",
        project_id: "project_new",
        deployment_id: "dep_new",
        offering_id: "bigmodel.coding-plan",
        start: "2026-09-01T00:00:00.000Z",
        end: "2026-09-02T00:00:00.000Z",
      });
    });
  });

  it("carries a linked Offering filter and exposes it as a clearable product name", async () => {
    window.history.replaceState({}, "", "/admin/usage?tab=failures&offering_id=bigmodel.coding-plan");
    renderPanel([providerFailure]);

    await waitFor(() => expect(api.usageFailures).toHaveBeenCalled());
    const calls = (api.usageFailures as unknown as { mock: { calls: [string][] } }).mock.calls;
    expect(new URLSearchParams((calls.at(-1)?.[0] ?? "").slice(1)).get("offering_id"))
      .toBe("bigmodel.coding-plan");
    const chip = await screen.findByRole("button", { name: /GLM Coding Plan（订阅）/ });
    fireEvent.click(chip);
    await waitFor(() => {
      const latest = (api.usageFailures as unknown as { mock: { calls: [string][] } }).mock.calls.at(-1)?.[0] ?? "";
      expect(new URLSearchParams(latest.slice(1)).get("offering_id")).toBeNull();
    });
  });

  // A drawer, not a centred dialog: what is read here is a captured request
  // body, which is tall, and the drawer is the console's full-height surface.
  // Asserted because the choice is the whole reason the payload has room.
  it("opens as a full-height drawer and closes without disturbing the list", async () => {
    renderPanel([providerFailure]);
    await screen.findByText("服务商认证或权限被拒");

    const dialog = await openFailureDetail();
    expect(within(dialog).getByRole("heading", { name: "失败详情" })).toBeVisible();

    // The header × and the footer button share the name, which is what a
    // reader hears twice and what a test has to disambiguate; the footer one is
    // the one an operator reaches at the end of a long dialog.
    const closers = within(dialog).getAllByRole("button", { name: "关闭" });
    fireEvent.click(closers[closers.length - 1]);
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(screen.getByText("服务商认证或权限被拒")).toBeVisible();
  });

  // The payload is behind a click. A row that fetched it on render would file
  // an audit record for every failure an operator merely scrolled past — the
  // server audits every read, because this is the only thing on the page that
  // holds material a caller wrote.
  it("does not fetch the captured payload until it is asked for", async () => {
    const payload = vi.spyOn(api, "usageFailurePayload").mockResolvedValue({
      request_id: "req_failed", project_id: "project_a", outcome: "provider_error",
      captured_at: "2026-08-21T10:01:02Z",
	  gateway_request: { model: "chat", messages: [{ role: "user", content: "hello" }], max_tokens: 8 },
      request: { model: "chat", messages: [{ role: "user", content: "hello" }] },
      response: { provider_status: 401, body: "invalid api key" },
    });
    renderPanel([providerFailure]);

    await screen.findByText("服务商认证或权限被拒");
    expect(payload).not.toHaveBeenCalled();

    // Two deliberate steps: the detail opens, and only then is there a control
    // that files an audit record.
    const dialog = await openFailureDetail();
    expect(payload).not.toHaveBeenCalled();
    // The warning is on screen before the control, not after the prompt is
    // already rendered.
    expect(within(dialog).getByText(/每次查看都会记入审计日志/)).toBeVisible();

    fireEvent.click(within(dialog).getByRole("button", { name: "查看" }));
	await waitFor(() => expect(payload).toHaveBeenCalledWith("req_failed"));
	expect(await within(dialog).findByRole("heading", { name: "Gateway 收到的请求" })).toBeVisible();
	expect(within(dialog).getByText(/"max_tokens": 8/)).toBeVisible();
	expect(await within(dialog).findByText(/invalid api key/)).toBeVisible();
  });

  it.each([
    [new ApiError(404, "not found", "failure_capture_not_found"), "没有保存该请求的原始内容。"],
    [new ApiError(404, "disabled", "failure_capture_disabled"), "失败载荷捕获当前未启用。"],
    [new ApiError(503, "audit unavailable", "audit_unavailable"), "审计日志当前不可用，因此载荷正文被安全扣留。"],
    [new ApiError(401, "expired"), "管理会话已失效。请重新登录后再查看。"],
    [new ApiError(403, "forbidden"), "当前账号没有查看失败载荷的权限。"],
    [new Error("network down"), "暂时无法读取失败载荷。请检查连接后重试。"],
  ])("explains payload read failures without calling all of them a capture miss", async (error, expected) => {
    vi.spyOn(api, "usageFailurePayload").mockRejectedValue(error);
    renderPanel([providerFailure]);

    await screen.findByText("服务商认证或权限被拒");
    const dialog = await openFailureDetail();
    fireEvent.click(within(dialog).getByRole("button", { name: "查看" }));
    expect(await within(dialog).findByText(expected)).toBeVisible();
  });

  it("can retry a transient audited payload read", async () => {
    const read = vi.spyOn(api, "usageFailurePayload")
      .mockRejectedValueOnce(new ApiError(503, "audit unavailable", "audit_unavailable"))
      .mockResolvedValueOnce({
        request_id: "req_failed", project_id: "project_a", outcome: "provider_error",
        captured_at: "2026-08-21T10:01:02Z", request: { model: "chat" },
      });
    renderPanel([providerFailure]);

    await screen.findByText("服务商认证或权限被拒");
    const dialog = await openFailureDetail();
    fireEvent.click(within(dialog).getByRole("button", { name: "查看" }));
    fireEvent.click(await within(dialog).findByRole("button", { name: "重试" }));

    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
    expect(await within(dialog).findByRole("heading", { name: "Halro 规范化后的请求" })).toBeVisible();
  });

  it("marks all three missing attribution fields as unknown only for legacy rows", async () => {
    renderPanel([{
      ...providerFailure,
      last_failure: {
        attempt_id: "att_old", attempt: 1, deployment_id: "dep_b", provider_status: 500,
        error_class: "provider", completed_at: "2026-08-21T10:01:02Z",
      },
    }]);
    await screen.findByRole("button", { name: "失败详情" });
    const dialog = await openFailureDetail();

    const unknowns = within(dialog).getAllByText("旧记录未采集");
    expect(unknowns).toHaveLength(5);
  });

  it("does not invent an unknown region for a current regionless offering", async () => {
    renderPanel([{
      ...providerFailure,
      last_failure: {
        ...providerFailure.last_failure!,
        offering_id: "openai.api-platform",
        profile_id: "openai.chat-embeddings.v1",
        account_region_id: undefined,
      },
    }]);
    await screen.findByText("服务商认证或权限被拒");
    const dialog = await openFailureDetail();

    expect(within(dialog).getByText("OpenAI API 平台")).toBeVisible();
    expect(within(dialog).queryByText("账号地域")).not.toBeInTheDocument();
    expect(within(dialog).getAllByText("旧记录未采集")).toHaveLength(2);
  });

  // A policy refusal never reached an upstream, so there is nothing to show and
  // no reason to offer an audited read.
  it("offers no payload on a policy refusal", async () => {
    renderPanel([policyRejection]);
    await screen.findByText(/策略拒绝：预算、熔断或并发上限/);
    const dialog = await openFailureDetail();

    expect(within(dialog).queryByText("原始请求与响应")).not.toBeInTheDocument();
    expect(within(dialog).queryByRole("button", { name: "查看" })).not.toBeInTheDocument();
  });

  // The summary card links here with the interval it covered; dropping it would
  // show the whole history under a heading naming one month.
  it("carries the interval it was linked with into the query", async () => {
    window.history.replaceState({}, "", "/admin/usage?tab=failures&start=2026-08-01T00:00:00Z&end=2026-09-01T00:00:00Z");
    renderPanel([]);

    await waitFor(() => expect(api.usageFailures).toHaveBeenCalled());
    const query = (api.usageFailures as unknown as { mock: { calls: [string][] } }).mock.calls[0][0] ?? "";
    const params = new URLSearchParams(query.slice(1));
    expect(params.get("start")).toBe("2026-08-01T00:00:00.000Z");
    expect(params.get("end")).toBe("2026-09-01T00:00:00.000Z");
  });

  it("says so in words when nothing failed outright", async () => {
    renderPanel([]);
    expect(await screen.findByText("该范围内没有最终失败的请求")).toBeVisible();
  });
});
