import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
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
    expect(screen.getByText(/由第 2 次尝试决定/)).toBeInTheDocument();
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
    expect(screen.getByText(/从未调用上游/)).toBeInTheDocument();
    expect(screen.queryByText("HTTP 401")).not.toBeInTheDocument();
  });

  // The two identifiers a support desk asks for, kept in the ledger so they
  // outlive the process log that used to be their only home.
  it("carries the upstream's own code and request ID into the row", async () => {
    renderPanel([providerFailure]);

    expect(await screen.findByText(/invalid_api_key/)).toBeInTheDocument();
    expect(screen.getByText(/upstream-req-77/)).toBeInTheDocument();
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

    expect(await screen.findByText(/未保存服务商错误码与请求标识/)).toBeInTheDocument();
  });

  // A policy refusal has no upstream, so it gets neither the identifiers nor a
  // notice that they were not recorded — nothing was ever asked for.
  it("offers no identifier notice on a policy refusal", async () => {
    renderPanel([policyRejection]);

    await screen.findByText(/策略拒绝：预算、熔断或并发上限/);
    expect(screen.queryByText(/未保存服务商错误码/)).not.toBeInTheDocument();
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
