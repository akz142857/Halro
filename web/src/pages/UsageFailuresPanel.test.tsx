import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
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
    // Which attempt produced the class the row shows. Without it a two-attempt
    // request reads as if either could have.
    expect(within(dialog).getByText(/由第 2 次尝试决定/)).toBeVisible();
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

  // A drawer, not a centred dialog: what is read here is a captured request
  // body, which is tall, and the drawer is the console's full-height surface.
  // Asserted because the choice is the whole reason the payload has room.
  it("opens as a full-height drawer and closes without disturbing the list", async () => {
    renderPanel([providerFailure]);
    await screen.findByText("服务商认证或权限被拒");

    const dialog = await openFailureDetail();
    expect(within(dialog).getByRole("heading", { name: "失败详情" })).toBeVisible();
    expect(dialog).toHaveClass("drawer");

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
    expect(await within(dialog).findByText(/invalid api key/)).toBeVisible();
  });

  // Not captured is the ordinary case, not a fault: capture may be off, the
  // failure may predate it, or the record may have aged out.
  it("says nothing was captured rather than reporting a fault", async () => {
    vi.spyOn(api, "usageFailurePayload").mockRejectedValue(new Error("not found"));
    renderPanel([providerFailure]);

    await screen.findByText("服务商认证或权限被拒");
    const dialog = await openFailureDetail();
    fireEvent.click(within(dialog).getByRole("button", { name: "查看" }));
    expect(await within(dialog).findByText("没有保存该请求的原始内容。")).toBeVisible();
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
