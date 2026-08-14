import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import type { Project } from "../types";
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
  max_stream_duration: 0,
  allowed_cidrs: null,
  redaction_policy_id: "",
  token_guard_policy_id: "",
  revision: 1,
  created_at: "2026-08-05T00:00:00Z",
  updated_at: "2026-08-05T00:00:00Z",
};

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

  it("resets raw JSON when the endpoint changes and supports keyboard tab navigation", async () => {
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
    expect((raw as HTMLTextAreaElement).value).toContain('"messages"');
    expect((raw as HTMLTextAreaElement).value).not.toContain('"input"');
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

  it("executes a standard response and opens the correlated usage record", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    const execute = vi.spyOn(api, "developerExecute").mockResolvedValue(new Response(
      JSON.stringify({ id: "chatcmpl_1", choices: [{ message: { content: "hello" } }] }),
      { status: 200, headers: { "Content-Type": "application/json", "X-Request-ID": "req_debug_1" } },
    ));
    vi.spyOn(api, "usageRequest").mockResolvedValue({});
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
    vi.spyOn(api, "usageRequest").mockResolvedValue({});
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
    expect(execute).toHaveBeenCalledTimes(2);
  });

  it("reports a 4xx as a failure instead of a completed request", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    vi.spyOn(api, "developerConfig").mockResolvedValue({ gateway_base_url: "http://127.0.0.1:8080" });
    vi.spyOn(api, "developerExecute").mockResolvedValue(new Response(
      JSON.stringify({ error: { message: "missing or invalid bearer token" } }),
      { status: 401, statusText: "Unauthorized", headers: { "Content-Type": "application/json" } },
    ));
    const usage = vi.spyOn(api, "usageRequest").mockResolvedValue({});
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });
    fireEvent.change(screen.getByLabelText("Gateway Key"), { target: { value: "wrong-key" } });

    fireEvent.click(screen.getByRole("button", { name: "发送请求" }));

    expect(await screen.findByText(/网关返回 401，请求未成功/)).toBeVisible();
    expect(screen.getByText(/请检查 Gateway Key 是否属于所选项目/)).toBeVisible();
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
    const createKey = vi.spyOn(api, "createKey").mockResolvedValue({
      data: { key: "hm_created_secret", metadata: { name: "工作台调试 2026-08-05 10:00:00" } },
      etag: '"1"',
    } as never);
    const localWrite = vi.spyOn(Storage.prototype, "setItem");
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);
    await screen.findByRole("option", { name: "support-chat" });
    expect(screen.getByLabelText("Gateway Key")).toHaveValue("");

    fireEvent.click(screen.getByRole("button", { name: "新建调试 Key" }));
    // Minting is gated the same way a deletion is: the key is shown once and
    // outlives this session, so the dialog asks who is asking.
    fireEvent.change(screen.getByLabelText("当前密码"), { target: { value: "a passphrase" } });
    fireEvent.click(screen.getAllByRole("button", { name: "新建调试 Key" }).at(-1)!);

    await waitFor(() => expect(screen.getByLabelText("Gateway Key")).toHaveValue("hm_created_secret"));
    // The key expires on its own so a forgotten debug key cannot stay usable.
    expect(createKey).toHaveBeenCalledWith(
      project.id, expect.stringContaining("工作台调试"), expect.any(String),
      expect.objectContaining({ currentPassword: "a passphrase" }), expect.any(String),
    );
    const expiresAt = Date.parse(createKey.mock.calls[0][4] as string);
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
