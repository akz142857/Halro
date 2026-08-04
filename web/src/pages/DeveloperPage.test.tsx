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
  allowed_routes: ["support-chat", "text-embedding"],
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
    expect(screen.getAllByText(/HEIMDALL_API_KEY/)).toHaveLength(2);
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
});
