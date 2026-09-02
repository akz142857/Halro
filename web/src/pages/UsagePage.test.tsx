import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import { UsagePage, billedTierLabel } from "./UsagePage";

describe("UsagePage request correlation", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/usage?request_id=req_debug_1");
  });

  it("initializes the exact Request ID filter from the developer workbench link", async () => {
    const usage = vi.spyOn(api, "usage").mockResolvedValue({ items: [], next_cursor: "" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);

    expect(screen.getByRole("textbox", { name: "Request ID" })).toHaveValue("req_debug_1");
    await waitFor(() => expect(usage).toHaveBeenCalled());
    const query = usage.mock.calls[0][0] ?? "";
    expect(new URLSearchParams(query.slice(1)).get("request_id")).toBe("req_debug_1");
  });
});

// A link that lands on an unfiltered list repeats the same defect one level down.
describe("UsagePage project deep link", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/usage?project_id=project_a");
  });

  it("initializes the project filter from the link it was sent to", async () => {
    const usage = vi.spyOn(api, "usage").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "projects").mockResolvedValue({ items: [{ id: "project_a", name: "Alpha" }] as never, next_cursor: "" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);

    await waitFor(() => expect(screen.getByRole("combobox", { name: "项目" })).toHaveValue("project_a"));
    await waitFor(() => expect(usage).toHaveBeenCalled());
    expect(new URLSearchParams((usage.mock.calls[0][0] ?? "").slice(1)).get("project_id")).toBe("project_a");
  });
});

// The model filter matches exactly, so the free-text box it replaces turned a
// typo into an empty table that looked like "no calls yet". The options have to
// cover more than the current routes: history outlives a route, and a select
// whose value is not among its options renders blank.
describe("UsagePage model filter", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/usage?tab=attempts");
  });

  it("offers the route aliases and the models present in history, not a free-text box", async () => {
    vi.spyOn(api, "usage").mockResolvedValue({
      items: [{ event_id: "e1", request_id: "req_1", attempt: 1, project_id: "p", requested_model: "retired-alias",
        provider_model: "gpt-4o", provider_input_tokens: 1, provider_output_tokens: 1, latency_millis: 5,
        status: "success", completed_at: "2026-08-06T00:00:00Z" }] as never,
      next_cursor: "",
    });
    vi.spyOn(api, "projects").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "routes").mockResolvedValue({
      items: [{ id: "r1", public_model: "chat" }, { id: "r2", public_model: "embed" }] as never,
      next_cursor: "",
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);

    const select = await screen.findByRole("combobox", { name: "模型" });
    await waitFor(() => {
      const options = [...select.querySelectorAll("option")].map((option) => option.value);
      // "" is the all-models option; the alias only history knows about is kept.
      expect(options).toEqual(["", "chat", "embed", "retired-alias"]);
    });
  });
});

// The provider filter was a free-text box wanting an opaque `provider_...` ID
// that nobody has to hand. It is gone; the summary's provider row still links
// here with one, and an applied filter with no control to clear it is how a
// table comes to look empty for no reason.
describe("UsagePage provider filter", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, "usage").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "projects").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "routes").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [], next_cursor: "" });
  });

  it("offers no provider box", async () => {
    window.history.replaceState({}, "", "/admin/usage?tab=attempts");
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);

    await waitFor(() => expect(api.usage).toHaveBeenCalled());
    expect(screen.queryByPlaceholderText("provider_…")).not.toBeInTheDocument();
  });

  it("still applies a linked provider filter, and shows it as clearable", async () => {
    window.history.replaceState({}, "", "/admin/usage?tab=attempts&provider_id=provider_a");
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);

    await waitFor(() => expect(api.usage).toHaveBeenCalled());
    const query = (api.usage as unknown as { mock: { calls: [string][] } }).mock.calls[0][0] ?? "";
    expect(new URLSearchParams(query.slice(1)).get("provider_id")).toBe("provider_a");

    const chip = await screen.findByRole("button", { name: /provider_a/ });
    fireEvent.click(chip);
    await waitFor(() => {
      const calls = (api.usage as unknown as { mock: { calls: [string][] } }).mock.calls;
      const latest = calls[calls.length - 1][0] ?? "";
      expect(new URLSearchParams(latest.slice(1)).get("provider_id")).toBeNull();
    });
  });
});

// The model column shows the alias, which is identical on every attempt of a
// fallback chain — so two targets of one alias on the same upstream model, the
// safest way to configure redundancy, were indistinguishable and a fallback
// could not be verified from the console at all.
describe("UsagePage deployment column", () => {
  const chain = [
    { event_id: "e1", request_id: "req_1", attempt: 1, project_id: "p", requested_model: "chat",
      deployment_id: "dep_primary", provider_model: "gpt-5.1", provider_input_tokens: 1, provider_output_tokens: 1,
      latency_millis: 5, status: "error", completed_at: "2026-08-06T00:00:00Z" },
    { event_id: "e2", request_id: "req_1", attempt: 2, project_id: "p", requested_model: "chat",
      deployment_id: "dep_fallback", provider_model: "gpt-5.1", provider_input_tokens: 1, provider_output_tokens: 1,
      latency_millis: 7, status: "success", completed_at: "2026-08-06T00:00:01Z" },
  ];

  beforeEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/usage?tab=attempts");
    vi.spyOn(api, "projects").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "routes").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "usage").mockResolvedValue({ items: chain as never, next_cursor: "" });
  });

  function renderUsage() {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);
  }

  it("tells the two attempts of one chain apart by their deployment", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [
      { id: "dep_primary", name: "Bedrock 主" }, { id: "dep_fallback", name: "Luna 备" },
    ] as never, next_cursor: "" });
    renderUsage();

    // Same alias, same upstream model, both attempts of one request. Scoped to
    // the table: the model filter offers "chat" as an option too.
    await screen.findByRole("table");
    const rows = within(screen.getByRole("table"));
    expect(rows.getAllByText("chat")).toHaveLength(2);
    expect(rows.getAllByText("gpt-5.1")).toHaveLength(2);
    // The deployment is what separates them. Still scoped to the table: the
    // deployment filter offers both names as options too.
    expect(rows.getByText("Bedrock 主")).toBeVisible();
    expect(rows.getByText("Luna 备")).toBeVisible();
    // And the ID is shown beside the name, because the ID is what the ledger
    // and the usage partitions carry.
    expect(rows.getByText("dep_primary")).toBeVisible();
    expect(rows.getByText("dep_fallback")).toBeVisible();
  });

  // History outlives a deployment: the list drops tombstones, so a name is not
  // always available and the ID has to stand on its own.
  it("falls back to the ID when the deployment is gone", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [], next_cursor: "" });
    renderUsage();

    expect(await screen.findByText("dep_primary")).toBeVisible();
    expect(screen.getByText("dep_fallback")).toBeVisible();
  });

  it("links a deployment to the list filtered to it", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({
      items: [{ id: "dep_primary", name: "Bedrock 主" }] as never, next_cursor: "",
    });
    renderUsage();

    expect(await screen.findByRole("link", { name: "Bedrock 主" }))
      .toHaveAttribute("href", "/admin/deployments?q=dep_primary");
  });
});

describe("billed price tier", () => {
  // The label is what makes two identically sized attempts with different costs
  // explainable, so it must name the rung and never silently render as blank.
  const label = (key: string, values?: Record<string, unknown>) => `${key}:${JSON.stringify(values ?? {})}`;

  it("names the window, the out-of-window rate, and the fail-closed fallback", () => {
    expect(billedTierLabel({ timezone: "Asia/Shanghai", source: "window", start_minute: 540, end_minute: 720, local_minute: 600 }, label))
      .toBe('usage.billedWindow:{"start":"09:00","end":"12:00","timezone":"Asia/Shanghai"}');
    expect(billedTierLabel({ timezone: "Asia/Shanghai", source: "base", local_minute: 780 }, label))
      .toBe('usage.billedBase:{"timezone":"Asia/Shanghai"}');
    expect(billedTierLabel({ timezone: "Asia/Shanghai", source: "zone_unavailable" }, label))
      .toBe('usage.billedZoneUnavailable:{"timezone":"Asia/Shanghai"}');
    // A window rung missing its bounds is a damaged record, not a reason to
    // claim an hour it cannot evidence.
    expect(billedTierLabel({ timezone: "Asia/Shanghai", source: "window" }, label))
      .toBe('usage.billedZoneUnavailable:{"timezone":"Asia/Shanghai"}');
  });
});

// The page answers two questions and opens on the one an operator arrives with:
// a bare visit wants "what did we spend", a drill-down link wants the calls it
// names. Landing a filtered link on the summary would drop the filter.
describe("UsagePage view selection", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    vi.spyOn(api, "projects").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "routes").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "usage").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "usageSummary").mockResolvedValue({
      granularity: "month", start: "2026-08-01", end: "2026-08-30",
      totals: emptySummaryMetrics(), buckets: [], timezone_changes: [], watermark_sequence: 1,
    } as never);
  });

  function renderUsage() {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    return render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);
  }

  it("opens on the summary when nothing was asked for", async () => {
    window.history.replaceState({}, "", "/admin/usage");
    renderUsage();

    expect(await screen.findByRole("tab", { name: "汇总", selected: true })).toBeVisible();
    expect(screen.queryByRole("textbox", { name: "Request ID" })).toBeNull();
  });

  it("opens on the attempt log when the link carries a filter", async () => {
    window.history.replaceState({}, "", "/admin/usage?deployment_id=dep_primary");
    renderUsage();

    expect(await screen.findByRole("tab", { name: "调用明细", selected: true })).toBeVisible();
    await waitFor(() => expect(api.usage).toHaveBeenCalled());
    const query = (api.usage as unknown as { mock: { calls: [string][] } }).mock.calls[0][0] ?? "";
    expect(new URLSearchParams(query.slice(1)).get("deployment_id")).toBe("dep_primary");
  });

  // Switching views pushes history, so the back button has to take the operator
  // to the view they came from. Without a popstate listener the URL changes and
  // the page does not, which is worse than not pushing at all.
  it("follows the back button between the two views", async () => {
    window.history.replaceState({}, "", "/admin/usage");
    renderUsage();
    expect(await screen.findByRole("tab", { name: "汇总", selected: true })).toBeVisible();

    fireEvent.click(screen.getByRole("tab", { name: "调用明细" }));
    await waitFor(() => expect(screen.getByRole("tab", { name: "调用明细", selected: true })).toBeVisible());
    expect(new URL(window.location.href).searchParams.get("tab")).toBe("attempts");

    act(() => {
      window.history.replaceState({}, "", "/admin/usage");
      window.dispatchEvent(new PopStateEvent("popstate"));
    });
    await waitFor(() => expect(screen.getByRole("tab", { name: "汇总", selected: true })).toBeVisible());
  });

  // A summary row links here with the interval it covered. Dropping it would
  // show the whole history under a heading that names one month.
  it("carries a summary row's absolute interval into the attempt filters", async () => {
    window.history.replaceState({}, "", "/admin/usage?project_id=p&start=2026-08-01T00:00:00Z&end=2026-09-01T00:00:00Z");
    renderUsage();

    await waitFor(() => expect(api.usage).toHaveBeenCalled());
    const query = (api.usage as unknown as { mock: { calls: [string][] } }).mock.calls[0][0] ?? "";
    const params = new URLSearchParams(query.slice(1));
    expect(params.get("start")).toBe("2026-08-01T00:00:00.000Z");
    expect(params.get("end")).toBe("2026-09-01T00:00:00.000Z");
  });
});

// The console held every field needed to say why a call failed — the class, the
// upstream status, which rung of the retry chain it was — and rendered the word
// "错误". An operator could see that something broke and had to go to the host's
// log to learn what, which is the gap this whole design exists to close.
describe("UsagePage failure detail", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/usage?tab=attempts");
    vi.spyOn(api, "projects").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "routes").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [], next_cursor: "" });
  });

  // The row shows the class and the status; everything else is one click away
  // in the same drawer the failed-request list opens.
  async function openAttemptDetail() {
    fireEvent.click(screen.getByRole("button", { name: "失败详情" }));
    return screen.findByRole("dialog");
  }

  const failedAttempt = (overrides: Record<string, unknown>) => ({
    event_id: "e1", request_id: "req_1", attempt_id: "att_1", attempt: 1, project_id: "p",
    requested_model: "chat", provider_model: "gpt-4o", provider_input_tokens: 1,
    provider_output_tokens: 0, cost_micros_usd: 0, latency_millis: 5, status: "provider_error",
    completed_at: "2026-08-06T00:00:00Z", retry_count: 0, fallback_count: 0, ...overrides,
  });

  it("names the class and the upstream status instead of the word error", async () => {
    vi.spyOn(api, "usage").mockResolvedValue({
      items: [failedAttempt({ error_class: "authentication", http_status: 401 })] as never,
      next_cursor: "",
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);

    expect(await screen.findByText("服务商认证或权限被拒")).toBeVisible();
    // The number is kept apart from the class because it is the part an
    // operator quotes to a provider's support desk.
    expect(screen.getByText("HTTP 401")).toBeVisible();
    // What to check next, in the drawer, so a wide table stays readable.
    const dialog = await openAttemptDetail();
    expect(within(dialog).getByText(/检查凭据状态/)).toBeVisible();
  });

  // A class the server starts sending before this bundle knows about it must
  // degrade to the identifier, never to a broken key.
  it("falls back to the identifier the server sent for an unknown class", async () => {
    vi.spyOn(api, "usage").mockResolvedValue({
      items: [failedAttempt({ error_class: "quota_exhausted_beta" })] as never,
      next_cursor: "",
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);

    expect(await screen.findByText("quota_exhausted_beta")).toBeVisible();
  });

  // Which rung of the chain this attempt was. Without it a fallback that failed
  // and a first try that failed read identically, and the retry/fallback counts
  // were in the payload all along.
  it("says which target and which retry produced the failure", async () => {
    vi.spyOn(api, "usage").mockResolvedValue({
      items: [failedAttempt({ error_class: "timeout", retry_count: 1, fallback_count: 1 })] as never,
      next_cursor: "",
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);

    await screen.findByText("上游响应超时");
    const dialog = await openAttemptDetail();
    expect(within(dialog).getByText(/第 2 个目标 · 第 1 次重试/)).toBeVisible();
  });

  // What a support desk asks for. These reach the console through the ledger
  // now, so a ticket raised a week after the failure can still name the request
  // the upstream saw.
  it("shows the upstream's own code and request ID", async () => {
    vi.spyOn(api, "usage").mockResolvedValue({
      items: [failedAttempt({
        error_class: "bad_request", http_status: 400, failure_phase: "provider",
        provider_code: "invalid_image_url:messages[0].content[1].image_url",
        provider_request_id: "upstream-req-42",
      })] as never,
      next_cursor: "",
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);

    await screen.findByText("上游拒绝了请求形状或参数");
    const dialog = await openAttemptDetail();
    expect(within(dialog).getByText("invalid_image_url:messages[0].content[1].image_url")).toBeVisible();
    expect(within(dialog).getByText("upstream-req-42")).toBeVisible();
  });

  // A record written before those fields were kept says so. A blank there and a
  // blank on a record that had none are different answers, and one rendering
  // for both talks the operator out of chasing a code that does exist upstream.
  it("says a record predates the fields rather than showing an empty code", async () => {
    vi.spyOn(api, "usage").mockResolvedValue({
      // No failure_phase: every failure classified since these fields were
      // added carries one, so its absence dates the record.
      items: [failedAttempt({ error_class: "bad_request", http_status: 400 })] as never,
      next_cursor: "",
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);

    await screen.findByText("上游拒绝了请求形状或参数");
    const dialog = await openAttemptDetail();
    expect(within(dialog).getByText(/未保存服务商错误码与请求标识/)).toBeVisible();
  });

  // An upstream that named no code on a record that would have kept one gets
  // neither the code nor the "not recorded" notice: nothing is missing.
  it("says nothing when the upstream named no code", async () => {
    vi.spyOn(api, "usage").mockResolvedValue({
      items: [failedAttempt({ error_class: "timeout", failure_phase: "provider" })] as never,
      next_cursor: "",
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);

    expect(await screen.findByText("上游响应超时")).toBeVisible();
    const dialog = await openAttemptDetail();
    expect(within(dialog).queryByText(/未保存服务商错误码/)).not.toBeInTheDocument();
  });

  // A failure this side raised about an answer the upstream gave. It settles
  // with HTTP 200, so the row used to show a red dot beside "HTTP 200" — the
  // upstream answered fine and the console still called it an error — and the
  // drawer used to say the record predated fields it was written with.
  it("names a local refusal by its outcome, not by the upstream's 200", async () => {
    vi.spyOn(api, "usage").mockResolvedValue({
      items: [failedAttempt({
        status: "policy_rejected", http_status: 200, failure_phase: "response_render",
      })] as never,
      next_cursor: "",
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);

    expect(await screen.findByText(/脱敏策略拦截了上游输出/)).toBeVisible();
    expect(screen.queryByText("HTTP 200")).not.toBeInTheDocument();

    const dialog = await openAttemptDetail();
    expect(within(dialog).queryByText(/未保存服务商错误码/)).not.toBeInTheDocument();
    expect(within(dialog).queryByText("HTTP 200")).not.toBeInTheDocument();
  });

  // The same for a target that could not serve the request's shape: no upstream
  // was called, so there is no class and no status to show.
  it("names an unserved capability by its outcome", async () => {
    vi.spyOn(api, "usage").mockResolvedValue({
      items: [failedAttempt({ status: "unsupported_feature", failure_phase: "pre_provider" })] as never,
      next_cursor: "",
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);

    expect(await screen.findByText(/目标不支持该请求形状/)).toBeVisible();
    const dialog = await openAttemptDetail();
    expect(within(dialog).queryByText(/未保存服务商错误码/)).not.toBeInTheDocument();
  });

  // A successful row must not grow a disclosure promising an explanation of a
  // failure that did not happen.
  it("leaves a successful attempt alone", async () => {
    vi.spyOn(api, "usage").mockResolvedValue({
      items: [failedAttempt({ status: "success" })] as never,
      next_cursor: "",
    });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><UsagePage /></QueryClientProvider>);

    expect(await screen.findByText("成功")).toBeVisible();
    expect(screen.queryByRole("button", { name: "失败详情" })).not.toBeInTheDocument();
  });
});

function emptySummaryMetrics() {
  return {
    attempts: 0, errors: 0, input_tokens: 0, output_tokens: 0,
    estimated_input_tokens: 0, estimated_output_tokens: 0,
    provider_cached_input_tokens: 0, provider_cache_write_input_tokens: 0, provider_reasoning_tokens: 0,
    cost_micros_usd: 0, estimated_cost_micros_usd: 0, unknown_attempts: 0, latency_millis: 0,
    attempt_latency_samples: 0, attempt_latency_p95_millis: 0, latency_approximate: true,
  };
}
