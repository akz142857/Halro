import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { StrictMode } from "react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import { stepUpRequired } from "../test/fixtures";
import type { Project, UsageRequestDetail } from "../types";
import { DeveloperPage } from "./DeveloperPage";

const project: Project = {
  id: "project_1",
  name: "Checkout service",
  enabled: true,
  allowed_models: ["support-chat", "text-embedding"],
  rpm: 60,
  tpm: 100000,
  max_concurrency: 8,
  daily_budget_micros_usd: 50000000,
  max_input_tokens: 0,
  max_output_tokens: 0,
  max_request_bytes: 0,
  max_stream_duration: 0, deferred_responses: false, max_deferred_queue: 0,
  allowed_cidrs: null,
  redaction_policy_id: "",
  token_guard_policy_id: "",
  revision: 1,
  created_at: "2026-08-05T00:00:00Z",
  updated_at: "2026-08-05T00:00:00Z",
};

// What the ledger reports back for a settled request: the workbench reads the
// billed project and the cost out of it, so a stub without them would assert
// nothing about either.
function settlementFixture(overrides: Partial<UsageRequestDetail["summary"]> = {}, attempts: UsageRequestDetail["attempts"] = []): UsageRequestDetail {
  return {
    summary: {
      request_id: "req_debug_1", project_id: "project_1", requested_model: "support-chat",
      attempts: 1, input_tokens: 12, output_tokens: 34, cost_micros_usd: 2500,
      unknown_attempts: 0, fallbacks: 0, outcome: "succeeded",
      accepted_at: "2026-09-22T00:00:00Z", completed_at: "2026-09-22T00:00:01Z",
      ...overrides,
    },
    attempts,
  };
}

/** One settled attempt, with the two estimation flags under the caller's control. */
function attemptFixture(overrides: Partial<UsageRequestDetail["attempts"][number]> = {}): UsageRequestDetail["attempts"][number] {
  return {
    event_id: "evt_1", request_id: "req_debug_1", attempt_id: "att_1", sequence: 1, attempt: 1,
    project_id: "project_1", period_id: "2026-09-22", provider_input_tokens: 12, provider_output_tokens: 34,
    prepared_output_tokens: 0, cost_micros_usd: 2500, price_evidence_status: "recorded", cost_value_status: "known",
    input_cost_micros_usd: 0, output_cost_micros_usd: 0, fixed_cost_micros_usd: 0,
    cost_estimated: false, tokens_estimated: false, latency_millis: 10,
    started_at: "2026-09-22T00:00:00Z", completed_at: "2026-09-22T00:00:01Z", status: "succeeded",
    ...overrides,
  } as UsageRequestDetail["attempts"][number];
}

describe("DeveloperPage", () => {
  beforeEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/developer");
  });

  it("builds integration examples from project public routes without sending a request", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);

    expect(await screen.findByRole("heading", { name: "开发者工作台" })).toBeVisible();
    expect(await screen.findByRole("option", { name: "support-chat" })).toBeVisible();
    expect(screen.getByRole("textbox", { name: /Gateway 地址/ })).toHaveValue("http://127.0.0.1:8080");
    expect(screen.getByText("/v1/responses")).toBeVisible();

    // The code area ships collapsed, so only the footnote mentions the variable until
    // the sample is opened.
    expect(screen.getAllByText(/HALRO_API_KEY/)).toHaveLength(1);
    fireEvent.click(screen.getByRole("button", { name: "展开代码" }));
    expect(screen.getAllByText(/HALRO_API_KEY/)).toHaveLength(2);
    expect(screen.getByRole("button", { name: "发送请求" })).toBeDisabled();
    expect(screen.getByText("等待真实请求")).toBeVisible();

    fireEvent.change(screen.getByLabelText("API 协议"), { target: { value: "chat" } });
    expect(screen.getByText("/v1/chat/completions")).toBeVisible();
    expect(screen.getByText(/messages/)).toBeVisible();
  });

  it("exposes the complete first-phase UI without persisting the Gateway Key", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    const localWrite = vi.spyOn(Storage.prototype, "setItem");
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });

    const key = screen.getByLabelText("Gateway Key");
    fireEvent.change(key, { target: { value: "hm_test_secret" } });
    expect(key).toHaveAttribute("type", "password");
    expect(localWrite).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("tab", { name: "原始 JSON" }));
    const raw = screen.getByRole("textbox", { name: /原始请求 JSON/ });
    fireEvent.change(raw, { target: { value: "{" } });
    expect(screen.getByText("请输入有效的 JSON 对象。")).toBeVisible();
    expect(screen.getByRole("button", { name: "复制代码" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "发送请求" })).toBeDisabled();
    fireEvent.change(raw, { target: { value: '{"model":"raw-model","input":"hello","stream":false}' } });
    fireEvent.click(screen.getByRole("button", { name: "展开代码" }));
    expect(screen.getAllByText(/raw-model/)).toHaveLength(2);

    expect(screen.getByText("HTTP 状态")).toBeVisible();
    expect(screen.getByText("Request ID")).toBeVisible();
    expect(screen.getByText("延迟")).toBeVisible();
    expect(screen.getByRole("button", { name: "查看用量记录" })).toBeDisabled();
    fireEvent.click(screen.getByRole("tab", { name: "响应头" }));
    expect(screen.getByText(/发送请求后.*Content-Type/)).toBeVisible();

    fireEvent.click(screen.getByRole("tab", { name: "表单模式" }));
    fireEvent.change(screen.getByLabelText("API 协议"), { target: { value: "embeddings" } });
    expect(screen.getByRole("button", { name: "SSE 流式" })).toBeDisabled();
  });

  it("sends a remote image URL as multimodal content on both chat contracts", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    const execute = vi.spyOn(api, "developerExecute").mockResolvedValue(new Response(
      JSON.stringify({ id: "resp_1" }), { status: 200, headers: { "Content-Type": "application/json" } },
    ));
    vi.spyOn(api, "usageRequest").mockRejectedValue(new Error("no usage"));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });

    fireEvent.change(screen.getByLabelText("图片输入"), { target: { value: "ftp://example.com/photo.png" } });
    fireEvent.click(screen.getByRole("button", { name: "添加" }));
    expect(screen.getByRole("alert")).toHaveTextContent("请输入 http(s) 图片地址");

    fireEvent.change(screen.getByLabelText("图片输入"), { target: { value: "https://example.com/a/photo.png" } });
    fireEvent.click(screen.getByRole("button", { name: "添加" }));
    expect(screen.getByRole("list", { name: "已添加的图片" })).toHaveTextContent("photo.png");
    // Nothing remote is fetched to build a preview; the CSP forbids it and reaching the
    // image from the console would not prove the provider can reach it either.
    expect(document.querySelector(".developer-image-list img")).toBeNull();

    fireEvent.change(screen.getByLabelText("Gateway Key"), { target: { value: "gw_debug_secret" } });
    fireEvent.click(screen.getByRole("button", { name: "发送请求" }));
    await waitFor(() => expect(execute).toHaveBeenCalled());
    expect(execute.mock.calls[0][2]).toEqual({
      model: "support-chat",
      stream: false,
      input: [{
        type: "message",
        role: "user",
        content: [
          { type: "input_text", text: "用一句话说明 Halro 如何路由这个请求。" },
          { type: "input_image", image_url: "https://example.com/a/photo.png" },
        ],
      }],
    });

    fireEvent.change(screen.getByLabelText("API 协议"), { target: { value: "chat" } });
    fireEvent.click(screen.getByRole("button", { name: "发送请求" }));
    await waitFor(() => expect(execute).toHaveBeenCalledTimes(2));
    expect(execute.mock.calls[1][2]).toEqual({
      model: "support-chat",
      stream: false,
      messages: [{
        role: "user",
        content: [
          { type: "text", text: "用一句话说明 Halro 如何路由这个请求。" },
          { type: "image_url", image_url: { url: "https://example.com/a/photo.png" } },
        ],
      }],
    });

    // Embeddings has no multimodal input, so the picture must not follow it there.
    fireEvent.change(screen.getByLabelText("API 协议"), { target: { value: "embeddings" } });
    expect(screen.queryByLabelText("图片输入")).toBeNull();
    fireEvent.click(screen.getByRole("button", { name: "发送请求" }));
    await waitFor(() => expect(execute).toHaveBeenCalledTimes(3));
    expect(execute.mock.calls[2][2]).toEqual({ model: "support-chat", input: "用一句话说明 Halro 如何路由这个请求。" });
  });

  it("carries a local file as a data URL but keeps the base64 out of the code sample", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080", max_request_bytes: 10 << 20 });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });

    const file = screen.getByLabelText("选择本地文件");
    fireEvent.change(file, { target: { files: [new File(["not-a-real-png"], "shot.png", { type: "image/png" })] } });
    expect(await screen.findByText("shot.png")).toBeVisible();
    expect(document.querySelector(".developer-image-list img")).toHaveAttribute("src", expect.stringContaining("data:image/png;base64,"));

    fireEvent.click(screen.getByRole("button", { name: "展开代码" }));
    const code = view.container.querySelector(".developer-code")?.textContent ?? "";
    expect(code).toContain("data:image/png;base64,<BASE64_OF_shot.png>");
    expect(code).not.toContain(btoa("not-a-real-png"));

    // Raw JSON is what gets sent, so it holds the real bytes rather than the placeholder.
    fireEvent.click(screen.getByRole("tab", { name: "原始 JSON" }));
    const raw = screen.getByRole("textbox", { name: /原始请求 JSON/ }) as HTMLTextAreaElement;
    expect(raw.value).toContain(btoa("not-a-real-png"));
    expect(raw.value).not.toContain("BASE64_OF_");
  });

  it("refuses a file that is not an image and a body past the instance request limit", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080", max_request_bytes: 512 });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });
    fireEvent.change(screen.getByLabelText("Gateway Key"), { target: { value: "gw_debug_secret" } });

    fireEvent.change(screen.getByLabelText("选择本地文件"), {
      target: { files: [new File(["report"], "report.pdf", { type: "application/pdf" })] },
    });
    expect(await screen.findByRole("alert")).toHaveTextContent("只能选择图片文件。");
    expect(screen.queryByRole("list", { name: "已添加的图片" })).toBeNull();

    // A file too large to send is refused before it is read, not after it has been
    // turned into base64.
    fireEvent.change(screen.getByLabelText("选择本地文件"), {
      target: { files: [new File(["x".repeat(2048)], "big.png", { type: "image/png" })] },
    });
    expect(await screen.findByRole("alert")).toHaveTextContent("超过实例请求体上限 512 B");

    // A URL small enough to attach can still push the body past the limit.
    fireEvent.change(screen.getByLabelText("图片输入"), { target: { value: `https://example.com/${"p".repeat(400)}.png` } });
    fireEvent.click(screen.getByRole("button", { name: "添加" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "发送请求" })).toBeDisabled());
    expect(screen.getByText(/超过实例上限 512 B/)).toBeVisible();
  });

  // A hand-written body is the only way to send a field the form has no control
  // for, and switching tabs or endpoints used to overwrite it from the form
  // with nothing said. The form's body stays one click away instead.
  it("keeps a hand-written JSON body across an endpoint change, and refills it on request", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });

    const formTab = screen.getByRole("tab", { name: "表单模式" });
    formTab.focus();
    fireEvent.keyDown(formTab, { key: "ArrowRight" });
    const jsonTab = screen.getByRole("tab", { name: "原始 JSON" });
    await waitFor(() => expect(jsonTab).toHaveFocus());
    const raw = screen.getByRole("textbox", { name: /原始请求 JSON/ });
    fireEvent.change(raw, { target: { value: '{"model":"custom","input":"hello","stream":true}' } });
    fireEvent.change(screen.getByLabelText("API 协议"), { target: { value: "chat" } });
    expect((raw as HTMLTextAreaElement).value).toContain('"custom"');

    fireEvent.click(screen.getByRole("button", { name: "用表单内容覆盖" }));
    expect((raw as HTMLTextAreaElement).value).toContain('"messages"');
    expect((raw as HTMLTextAreaElement).value).not.toContain('"custom"');
    // Refilled means no longer hand-written, so an endpoint change follows the
    // form again.
    fireEvent.change(screen.getByLabelText("API 协议"), { target: { value: "embeddings" } });
    expect((raw as HTMLTextAreaElement).value).not.toContain('"messages"');
  });

  // A refusal is a perfectly readable JSON body, so a sample that prints
  // whatever came back reports the gateway's error envelope as an answer. Go
  // and Python checked; JavaScript and Java did not, and they are the two most
  // likely to be pasted straight into a service.
  it("checks the HTTP status in every language and asks for the stream when streaming", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });
    fireEvent.click(screen.getByRole("button", { name: "展开代码" }));
    const sample = () => view.container.querySelector(".developer-code")?.textContent ?? "";

    const statusChecks: [string, RegExp][] = [
      ["JavaScript", /if \(!response\.ok\)/],
      ["Python", /raise_for_status\(\)/],
      ["Go", /resp\.StatusCode < 200/],
      ["Java", /response\.statusCode\(\) < 200/],
    ];
    for (const streaming of [false, true]) {
      fireEvent.click(screen.getByRole("button", { name: streaming ? "SSE 流式" : "普通响应" }));
      for (const [language, check] of statusChecks) {
        fireEvent.click(screen.getByRole("tab", { name: language }));
        expect(sample(), `${language} ${streaming ? "streaming" : "standard"}`).toMatch(check);
      }
      // curl prints the refusal where the operator is already looking, so it
      // states the stream it wants and nothing more.
      for (const language of ["curl", "JavaScript", "Python", "Go", "Java"]) {
        fireEvent.click(screen.getByRole("tab", { name: language }));
        expect(sample().includes("text/event-stream"), `${language} accept`).toBe(streaming);
      }
    }
  });

  it("shell-quotes curl examples and keeps Python JSON strings intact", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });

    fireEvent.change(screen.getByLabelText("输入内容"), { target: { value: "What's true?'; touch /tmp/pwned; #" } });
    fireEvent.click(screen.getByRole("button", { name: "展开代码" }));
    const curlCode = view.container.querySelector(".developer-code")?.textContent ?? "";
    expect(curlCode).toContain("'\"'\"'");
    expect(curlCode).toContain("--data-binary");

    fireEvent.click(screen.getByRole("tab", { name: "Python" }));
    const pythonCode = view.container.querySelector(".developer-code")?.textContent ?? "";
    expect(pythonCode).toContain("json.loads");
    expect(pythonCode).toContain("What's true?");
    expect(pythonCode).not.toContain("What's True?");
  });

  // The app renders inside React.StrictMode, whose mount/cleanup/remount left a
  // cleanup-only ref latched false — and with it every settlement read.
  it("still reads settlement under StrictMode", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    vi.spyOn(api, "developerExecute").mockResolvedValue(new Response(
      JSON.stringify({ id: "chatcmpl_1" }),
      { status: 200, headers: { "Content-Type": "application/json", "X-Request-ID": "req_debug_1" } },
    ));
    vi.spyOn(api, "usageRequest").mockResolvedValue(settlementFixture());
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<StrictMode><QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider></StrictMode>);
    await screen.findByRole("option", { name: "support-chat" });
    fireEvent.change(screen.getByLabelText("Gateway Key"), { target: { value: "gw_debug_secret" } });
    fireEvent.click(screen.getByRole("button", { name: "发送请求" }));

    expect(await screen.findByText("12 / 34")).toBeVisible();
  });

  // The routes page links here with the alias in the URL. The reconciliation
  // effect ran before the projects query resolved, when the allowed list is
  // empty, and threw the alias away — so a pasted link or a refresh sent the
  // request through a different alias with no notice.
  it("keeps a deep-linked alias through a cold load", async () => {
    window.history.replaceState({}, "", "/admin/developer?model=text-embedding");
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });

    expect(screen.getByLabelText("公共模型别名")).toHaveValue("text-embedding");
    window.history.replaceState({}, "", "/admin/developer");
  });

  // Switching the tab is the other way into the JSON editor, and it had the
  // same overwrite the endpoint change did.
  it("does not overwrite a hand-written body when the mode is switched back", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });

    fireEvent.click(screen.getByRole("tab", { name: "原始 JSON" }));
    const raw = screen.getByRole("textbox", { name: /原始请求 JSON/ }) as HTMLTextAreaElement;
    fireEvent.change(raw, { target: { value: '{"model":"custom","input":"kept"}' } });
    fireEvent.click(screen.getByRole("tab", { name: "表单模式" }));
    fireEvent.click(screen.getByRole("tab", { name: "原始 JSON" }));

    expect((screen.getByRole("textbox", { name: /原始请求 JSON/ }) as HTMLTextAreaElement).value).toContain("kept");
  });

  // The gateway names its own refusal, and the status alone cannot tell a
  // capability rejection from a malformed body.
  it("explains a refusal by the gateway's own error code", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    vi.spyOn(api, "developerExecute").mockResolvedValue(new Response(
      JSON.stringify({ error: { message: "model is not allowed for this project", type: "invalid_request_error", code: "model_not_allowed" } }),
      { status: 403, statusText: "Forbidden", headers: { "Content-Type": "application/json" } },
    ));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });
    fireEvent.change(screen.getByLabelText("Gateway Key"), { target: { value: "gw_debug_secret" } });
    fireEvent.click(screen.getByRole("button", { name: "发送请求" }));

    // The code's own repair, not the generic 403 sentence.
    expect(await screen.findByText(/这个项目没有被授权调用它/)).toBeVisible();
  });

  // The aggregate answers with the in-flight accumulator for a request it has
  // seen but not finalized: 200, no outcome, zeros throughout. Accepting that
  // as a settlement printed "this call cost $0.00" and kept it there.
  it("keeps waiting while the ledger answers with an unfinalized record", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    vi.spyOn(api, "developerExecute").mockResolvedValue(new Response(
      JSON.stringify({ id: "chatcmpl_1" }),
      { status: 200, headers: { "Content-Type": "application/json", "X-Request-ID": "req_debug_1" } },
    ));
    const usage = vi.spyOn(api, "usageRequest")
      .mockResolvedValueOnce(settlementFixture({ outcome: "", attempts: 0, input_tokens: 0, output_tokens: 0, cost_micros_usd: 0 }))
      .mockResolvedValue(settlementFixture());
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });
    fireEvent.change(screen.getByLabelText("Gateway Key"), { target: { value: "gw_debug_secret" } });
    fireEvent.click(screen.getByRole("button", { name: "发送请求" }));
    expect(await screen.findByText("请求已完成")).toBeVisible();

    // The unfinalized read is not shown as a settlement of zero…
    expect(screen.queryByText("0 / 0")).not.toBeInTheDocument();
    // …and the next read, which carries an outcome, is.
    expect(await screen.findByText("12 / 34")).toBeVisible();
    expect(usage.mock.calls.length).toBeGreaterThan(1);
  });

  // Auth and validation refusals never produce a usage record, so "awaiting
  // settlement" was a promise the page could not keep.
  it("says there is no usage record once the reads are spent", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    vi.spyOn(api, "developerExecute").mockResolvedValue(new Response(
      JSON.stringify({ id: "chatcmpl_1" }),
      { status: 200, headers: { "Content-Type": "application/json", "X-Request-ID": "req_debug_1" } },
    ));
    vi.spyOn(api, "usageRequest").mockRejectedValue(new Error("no usage record"));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });
    fireEvent.change(screen.getByLabelText("Gateway Key"), { target: { value: "gw_debug_secret" } });
    fireEvent.click(screen.getByRole("button", { name: "发送请求" }));

    expect(await screen.findByText("没有用量记录", {}, { timeout: 5000 })).toBeVisible();
    expect(screen.queryByText("等待结算")).not.toBeInTheDocument();
  }, 10000);

  // A conservative stand-in read as a measurement is the one reading that
  // cannot be corrected later, and a cost with unknown attempts behind it is
  // not a complete figure — in the history row as much as in the panel.
  it("states whose numbers these are and how complete the cost is", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    vi.spyOn(api, "developerExecute").mockResolvedValue(new Response(
      JSON.stringify({ id: "chatcmpl_1" }),
      { status: 200, headers: { "Content-Type": "application/json", "X-Request-ID": "req_debug_1" } },
    ));
    vi.spyOn(api, "usageRequest").mockResolvedValue(settlementFixture(
      { unknown_attempts: 1, attempts: 2 },
      [attemptFixture({ tokens_estimated: true, cost_estimated: true })],
    ));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });
    fireEvent.change(screen.getByLabelText("Gateway Key"), { target: { value: "gw_debug_secret" } });
    fireEvent.click(screen.getByRole("button", { name: "发送请求" }));

    expect(await screen.findByText("保守上限")).toBeVisible();
    // The incompleteness is stated in the panel and in the history row, never
    // in only one of them.
    expect(screen.getAllByText(/1 次调用费用未知/)).toHaveLength(2);
    expect(screen.getAllByText(/含估算/).length).toBeGreaterThan(0);
  });

  // An HTTP 200 says the call was accepted, not what it cost or who paid for
  // it — and the project picker only filters aliases, so the Gateway Key can
  // bill a different project than the one on screen.
  it("reports what the ledger settled and flags a call billed to another project", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [
      project,
      { ...project, id: "project_2", name: "Batch jobs" },
    ], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    vi.spyOn(api, "developerExecute").mockResolvedValue(new Response(
      JSON.stringify({ id: "chatcmpl_1" }),
      { status: 200, headers: { "Content-Type": "application/json", "X-Request-ID": "req_debug_1" } },
    ));
    vi.spyOn(api, "usageRequest").mockResolvedValue(settlementFixture({ project_id: "project_2", cost_micros_usd: 2500 }));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });

    fireEvent.change(screen.getByLabelText("Gateway Key"), { target: { value: "gw_debug_secret" } });
    fireEvent.click(screen.getByRole("button", { name: "发送请求" }));

    expect(await screen.findByText("请求已完成")).toBeVisible();
    expect(await screen.findByText("12 / 34")).toBeVisible();
    // Once in the response panel, once in this session's history.
    expect(screen.getAllByText(/0\.0025/)).toHaveLength(2);
    expect(screen.getByText(/本次调用计入项目“Batch jobs”/)).toBeVisible();
  });

  // Debugging is comparing this call against the last one, and the page used to
  // forget the last one the moment the next was sent.
  it("keeps this session's requests and loads one back into the editor", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    vi.spyOn(api, "developerExecute").mockResolvedValue(new Response(
      JSON.stringify({ id: "chatcmpl_1" }),
      { status: 200, headers: { "Content-Type": "application/json", "X-Request-ID": "req_debug_1" } },
    ));
    vi.spyOn(api, "usageRequest").mockResolvedValue(settlementFixture());
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });

    fireEvent.change(screen.getByLabelText("输入内容"), { target: { value: "first call" } });
    fireEvent.change(screen.getByLabelText("Gateway Key"), { target: { value: "gw_debug_secret" } });
    fireEvent.click(screen.getByRole("button", { name: "发送请求" }));
    expect(await screen.findByText("请求已完成")).toBeVisible();

    fireEvent.change(screen.getByLabelText("输入内容"), { target: { value: "second call" } });
    fireEvent.click(screen.getByRole("button", { name: "载入这条请求" }));

    // It lands in JSON mode because the stored body is what was sent, fields
    // the form cannot express included.
    const raw = screen.getByRole("textbox", { name: /原始请求 JSON/ }) as HTMLTextAreaElement;
    expect(raw.value).toContain("first call");
    expect(raw.value).not.toContain("second call");
  });

  // Settlement lands after the answer does, so the read retries — and it used
  // to keep retrying after the page was gone, because `finally` clears the
  // controller the unmount cleanup aborts. The next page's spy then saw a call
  // for a request nobody was looking at.
  it("stops reading settlement once the page is unmounted", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    vi.spyOn(api, "developerExecute").mockResolvedValue(new Response(
      JSON.stringify({ id: "chatcmpl_1" }),
      { status: 200, headers: { "Content-Type": "application/json", "X-Request-ID": "req_unmount" } },
    ));
    // Never settles, so every attempt of the retry is taken.
    const usage = vi.spyOn(api, "usageRequest").mockRejectedValue(new Error("not settled yet"));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });
    fireEvent.change(screen.getByLabelText("Gateway Key"), { target: { value: "gw_debug_secret" } });
    fireEvent.click(screen.getByRole("button", { name: "发送请求" }));
    expect(await screen.findByText("请求已完成")).toBeVisible();

    view.unmount();
    const callsAtUnmount = usage.mock.calls.length;
    // Pinned, not merely compared with itself: a slow render can spend the
    // whole retry budget before the unmount, and then nothing could follow it
    // whether the guard works or not.
    expect(callsAtUnmount).toBe(1);
    await new Promise((resolve) => setTimeout(resolve, 600));
    expect(usage.mock.calls.length).toBe(callsAtUnmount);
  });

  it("executes a standard response and opens the correlated usage record", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    const execute = vi.spyOn(api, "developerExecute").mockResolvedValue(new Response(
      JSON.stringify({ id: "chatcmpl_1", choices: [{ message: { content: "hello" } }] }),
      { status: 200, headers: { "Content-Type": "application/json", "X-Request-ID": "req_debug_1" } },
    ));
    vi.spyOn(api, "usageRequest").mockResolvedValue(settlementFixture());
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });

    fireEvent.change(screen.getByLabelText("Gateway Key"), { target: { value: "gw_debug_secret" } });
    fireEvent.click(screen.getByRole("button", { name: "普通响应" }));
    fireEvent.click(screen.getByRole("button", { name: "发送请求" }));

    expect(await screen.findByText("请求已完成")).toBeVisible();
    expect(screen.getByText("req_debug_1")).toBeVisible();
    expect(screen.getByText(/chatcmpl_1/)).toBeVisible();
    expect(execute).toHaveBeenCalledWith("responses", "gw_debug_secret", expect.objectContaining({ model: "support-chat", stream: false }), false, expect.any(AbortSignal));
    fireEvent.click(screen.getByRole("tab", { name: "响应头" }));
    expect(screen.getByText(/content-type: application\/json/)).toBeVisible();
    await waitFor(() => expect(screen.getByRole("button", { name: "查看用量记录" })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: "查看用量记录" }));
    expect(window.location.pathname).toBe("/admin/usage");
    expect(new URLSearchParams(window.location.search).get("request_id")).toBe("req_debug_1");
  });

  // "The body is empty" is a finding, and in flight it is the wrong one: nothing
  // has arrived yet. A standard response shows nothing at all until it completes,
  // so the panel used to read as an answer for the whole of a slow call.
  it("says it is waiting rather than reporting an empty body", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    let answer: (value: Response) => void = () => {};
    vi.spyOn(api, "developerExecute").mockImplementation(() => new Promise<Response>((resolve) => { answer = resolve; }));
    vi.spyOn(api, "usageRequest").mockRejectedValue(new Error("no usage"));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });
    fireEvent.change(screen.getByLabelText("Gateway Key"), { target: { value: "gw_debug_secret" } });

    fireEvent.click(screen.getByRole("button", { name: "发送请求" }));
    expect(await screen.findByText("等待上游返回")).toBeVisible();
    expect(screen.queryByText("响应体为空")).not.toBeInTheDocument();

    answer(new Response(JSON.stringify({ id: "resp_1" }), {
      status: 200, headers: { "Content-Type": "application/json" },
    }));
    expect(await screen.findByText(/resp_1/)).toBeVisible();
    expect(screen.queryByText("等待上游返回")).not.toBeInTheDocument();
  });

  // The response is the thing an operator takes away — into a bug report, a
  // ticket, a message to a provider. Selecting it out of a scrolling pre is
  // work, and work at the end of a debugging session is where a detail gets
  // dropped.
  it("copies the pane on screen, and only once there is something to copy", async () => {
    const clipboard = vi.fn().mockResolvedValue(undefined);
    Object.assign(navigator, { clipboard: { writeText: clipboard } });
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    vi.spyOn(api, "developerExecute").mockResolvedValue(new Response(
      JSON.stringify({ id: "resp_copy" }),
      { status: 200, headers: { "Content-Type": "application/json", "X-Request-ID": "req_copy" } },
    ));
    vi.spyOn(api, "usageRequest").mockRejectedValue(new Error("no usage"));
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });

    // Nothing has run, so there is nothing to offer.
    expect(screen.queryByRole("button", { name: "复制响应体" })).toBeNull();

    fireEvent.change(screen.getByLabelText("Gateway Key"), { target: { value: "gw_debug_secret" } });
    fireEvent.click(screen.getByRole("button", { name: "发送请求" }));
    expect(await screen.findByText("请求已完成")).toBeVisible();

    fireEvent.click(screen.getByRole("button", { name: "复制响应体" }));
    await waitFor(() => expect(clipboard).toHaveBeenCalledWith(expect.stringContaining("resp_copy")));

    // The headers pane offers its own, and copies what that pane shows rather
    // than whatever was copied last.
    fireEvent.click(screen.getByRole("tab", { name: "响应头" }));
    fireEvent.click(screen.getByRole("button", { name: "复制响应头" }));
    await waitFor(() => expect(clipboard).toHaveBeenLastCalledWith(expect.stringContaining("req_copy")));
    expect(clipboard).toHaveBeenLastCalledWith(expect.not.stringContaining("resp_copy"));
  });

  it("renders SSE data and cancels an in-flight request", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    const execute = vi.spyOn(api, "developerExecute")
      .mockResolvedValueOnce(new Response("event: response.created\ndata: {\"type\":\"response.created\"}\n\n", {
        status: 200, headers: { "Content-Type": "text/event-stream", "X-Request-ID": "req_stream_1" },
      }))
      .mockImplementationOnce((_endpoint, _key, _body, _streaming, signal) => new Promise((_resolve, reject) => {
        signal.addEventListener("abort", () => reject(new DOMException("aborted", "AbortError")), { once: true });
      }));
    vi.spyOn(api, "usageRequest").mockResolvedValue(settlementFixture());
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });
    fireEvent.change(screen.getByLabelText("Gateway Key"), { target: { value: "gw_debug_secret" } });

    fireEvent.click(screen.getByRole("button", { name: "发送请求" }));
    expect(await screen.findByText("请求已完成")).toBeVisible();
    expect(screen.getByText(/event: response.created/)).toBeVisible();

    fireEvent.click(screen.getByRole("button", { name: "发送请求" }));
    fireEvent.click(await screen.findByRole("button", { name: "取消请求" }));
    expect(await screen.findByText("请求已取消")).toBeVisible();
    // "The body is empty" is a finding about a response that arrived. A request
    // cancelled before one did has none to report on, and saying it is empty
    // reads as an answer about the provider.
    expect(screen.getByText("已取消，未收到响应体")).toBeVisible();
    expect(screen.queryByText("响应体为空")).not.toBeInTheDocument();
    expect(execute).toHaveBeenCalledTimes(2);
  });

  it("reports a 4xx as a failure instead of a completed request", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    vi.spyOn(api, "developerExecute").mockResolvedValue(new Response(
      // The shape the gateway actually sends: writeError always fills `code`.
      JSON.stringify({ error: { message: "missing or invalid bearer token", type: "authentication_error", code: "invalid_api_key" } }),
      { status: 401, statusText: "Unauthorized", headers: { "Content-Type": "application/json" } },
    ));
    const usage = vi.spyOn(api, "usageRequest").mockResolvedValue(settlementFixture());
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });
    fireEvent.change(screen.getByLabelText("Gateway Key"), { target: { value: "wrong-key" } });

    fireEvent.click(screen.getByRole("button", { name: "发送请求" }));

    expect(await screen.findByText(/网关返回 401，请求未成功/)).toBeVisible();
    expect(screen.getByText(/Gateway Key 无效、已禁用，或不属于所选项目/)).toBeVisible();
    expect(screen.queryByText("请求已完成")).not.toBeInTheDocument();
    expect(usage).not.toHaveBeenCalled();
  });

  it("keeps sending available when the Gateway URL is unusable and never refills it", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });

    // Missing key is the real blocker, and the page must say so.
    expect(screen.getByRole("button", { name: "发送请求" })).toBeDisabled();
    expect(screen.getByText(/还需要填写：.*Gateway Key/)).toBeVisible();

    fireEvent.change(screen.getByRole("textbox", { name: /Gateway 地址/ }), { target: { value: "" } });
    expect(screen.getByRole("textbox", { name: /Gateway 地址/ })).toHaveValue("");

    fireEvent.change(screen.getByLabelText("Gateway Key"), { target: { value: "gw_debug_secret" } });
    expect(screen.getByRole("button", { name: "发送请求" })).toBeEnabled();
  });

  it("locks the model picker in JSON mode so the sent body always matches the screen", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });

    expect(screen.getByLabelText(/^公共模型/)).toBeEnabled();
    fireEvent.click(screen.getByRole("tab", { name: "原始 JSON" }));

    expect(screen.getByLabelText(/^公共模型/)).toBeDisabled();
    expect(screen.getByText("JSON 模式下，模型与参数以下方 JSON 为准。")).toBeVisible();
  });

  it("does not expose cancel until a request is running", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });
    fireEvent.change(screen.getByLabelText("Gateway Key"), { target: { value: "gw_debug_secret" } });

    // A single toggling button turned a double click into "send, then abort what was billed".
    expect(screen.queryByRole("button", { name: "取消请求" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "发送请求" })).toBeEnabled();
  });

  it("fills the field from a freshly created debug key without persisting it", async () => {
    // Existing keys are stored as SHA-256 hashes, so creation is the only moment the
    // plaintext exists. It goes straight into the field and nowhere else.
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    const createKey = vi.spyOn(api, "createKey")
      .mockRejectedValueOnce(stepUpRequired())
      .mockResolvedValue({
        data: { key: "hm_created_secret", metadata: { name: "工作台调试 2026-08-05 10:00:00" } },
        etag: '"1"',
      } as never);
    const localWrite = vi.spyOn(Storage.prototype, "setItem");
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });
    expect(screen.getByLabelText("Gateway Key")).toHaveValue("");

    fireEvent.click(screen.getByRole("button", { name: "新建调试 Key" }));
    // Minting stays outside the re-authentication window — the key is shown once
    // and outlives this session — so the server asks every time, and the dialog
    // opens its fields as soon as the first attempt comes back asking.
    fireEvent.click(screen.getAllByRole("button", { name: "新建调试 Key" }).at(-1)!);
    fireEvent.change(await screen.findByLabelText(/^当前密码/), { target: { value: "a passphrase" } });
    fireEvent.click(screen.getAllByRole("button", { name: "新建调试 Key" }).at(-1)!);

    await waitFor(() => expect(screen.getByLabelText("Gateway Key")).toHaveValue("hm_created_secret"));
    // The key expires on its own so a forgotten debug key cannot stay usable.
    expect(createKey).toHaveBeenLastCalledWith(
      project.id, expect.stringContaining("工作台调试"), expect.any(String),
      expect.objectContaining({ currentPassword: "a passphrase" }), expect.any(String),
    );
	const firstAttempt = createKey.mock.calls[0];
	const retry = createKey.mock.calls[1];
	expect(retry[1]).toBe(firstAttempt[1]);
	expect(retry[2]).toBe(firstAttempt[2]);
	expect(retry[4]).toBe(firstAttempt[4]);
    const expiresAt = Date.parse(createKey.mock.calls.at(-1)![4] as string);
    expect(expiresAt).toBeGreaterThan(Date.now() + 23 * 60 * 60 * 1000);
    expect(expiresAt).toBeLessThanOrEqual(Date.now() + 24 * 60 * 60 * 1000);
    expect(screen.getByLabelText("Gateway Key")).toHaveAttribute("type", "password");
    expect(screen.getByText(/已创建/)).toBeVisible();
    expect(localWrite).not.toHaveBeenCalled();
  });

  it("opens the collapsed code area when a language tab is picked", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });
    expect(view.container.querySelector(".developer-code")).toBeNull();

    // Choosing a language only makes sense if the sample becomes visible.
    fireEvent.click(screen.getByRole("tab", { name: "Python" }));

    expect(view.container.querySelector(".developer-code")?.textContent).toContain("requests.post");
    expect(screen.getByRole("button", { name: "收起代码" })).toBeVisible();
  });

  it("generates a Java example that reads the key from the environment", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });
    fireEvent.click(screen.getByRole("button", { name: "展开代码" }));

    fireEvent.click(screen.getByRole("tab", { name: "Java" }));

    const javaCode = view.container.querySelector(".developer-code")?.textContent ?? "";
    expect(javaCode).toContain("HttpRequest.newBuilder()");
    expect(javaCode).toContain('System.getenv("HALRO_API_KEY")');
    expect(javaCode).toContain("http://127.0.0.1:8080/v1/responses");
    expect(javaCode).toContain("BodyHandlers.ofString()");
    expect(javaCode).not.toContain("support-chat\\\"");

    // Streaming has to be read line by line rather than buffered whole.
    fireEvent.click(screen.getByRole("button", { name: "SSE 流式" }));
    const streamingJava = view.container.querySelector(".developer-code")?.textContent ?? "";
    expect(streamingJava).toContain("BodyHandlers.ofLines()");
    expect(streamingJava).toContain("HttpResponse<Stream<String>>");
  });

  it("generates Go examples that handle construction, transport, status, body, and stream errors", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    const view = render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });
    fireEvent.click(screen.getByRole("button", { name: "展开代码" }));
    fireEvent.click(screen.getByRole("tab", { name: "Go" }));

    const unary = view.container.querySelector(".developer-code")?.textContent ?? "";
    expect(unary).toContain("req, err := http.NewRequest");
    expect(unary).toContain("resp, err := http.DefaultClient.Do(req)");
    expect(unary).toContain("defer resp.Body.Close()");
    expect(unary).toContain("io.LimitReader(resp.Body, 1<<20)");
    expect(unary).toContain("resp.StatusCode < 200");
    expect(unary).toContain("fmt.Println(string(responseBody))");

    fireEvent.click(screen.getByRole("button", { name: "SSE 流式" }));
    const streaming = view.container.querySelector(".developer-code")?.textContent ?? "";
    expect(streaming).toContain("scanner.Buffer(make([]byte, 64<<10), 1<<20)");
    expect(streaming).toContain("if err := scanner.Err(); err != nil");
  });

  it("survives a project whose allowed_models came back as null", async () => {
    // The admin API accepts projects without allowed_models and serialises them as null;
    // dereferencing it used to throw during render and blank the whole console.
    vi.spyOn(api, "projects").mockResolvedValue({
      items: [{ ...project, id: "project_null", name: "NoRoutes", allowed_models: null as never }, project],
      next_cursor: "",
    });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });

    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);

    expect(await screen.findByRole("option", { name: "support-chat" })).toBeVisible();
    expect(screen.queryByRole("option", { name: "NoRoutes" })).not.toBeInTheDocument();
  });
});
