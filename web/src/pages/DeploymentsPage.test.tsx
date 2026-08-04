import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api";
import type { Deployment, Provider } from "../types";
import { DeploymentsPage } from "./DeploymentsPage";

const capabilities = {
  chat: true, streaming: true, embeddings: true, moderations: false, images: false,
  transcriptions: false, speech: false, files: false, batches: false, rerank: false,
  async_generate: false, tools: true, vision: true, json_mode: true,
  developer_role: true, reasoning: true, stream_usage: true,
  max_context_tokens: 0, max_output_tokens: 0,
};

const provider = {
  id: "provider_openai", name: "OpenAI production", type: "openai", base_url: "https://api.openai.com",
  access_surface: "openai-api", profile_id: "openai.chat-embeddings.v1", credential_scheme: "bearer.static",
  credential_id: "credential_openai", allowed_hosts: ["api.openai.com"], capabilities,
  capability_evidence: {}, max_concurrency: 8, enabled: true, revision: 1,
  created_at: "", updated_at: "",
} as Provider;

describe("deployment release workflow", () => {
  beforeEach(() => {
    vi.spyOn(api, "providers").mockResolvedValue({ items: [provider], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "routes").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "providerModels").mockResolvedValue({
      items: [{ id: "gpt-5", owned_by: "openai" }, { id: "gpt-4.1", owned_by: "openai" }],
      fetched_at: "2026-08-02T00:00:00Z",
      expires_at: "2026-08-02T00:05:00Z",
      cached: false,
    });
  });

  afterEach(() => vi.restoreAllMocks());

  it("creates a deployment disabled and keeps route policy out of the form", async () => {
    const create = vi.spyOn(api, "createDeployment").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));
    expect(screen.queryByLabelText("区域")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("默认优先级")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("权重")).not.toBeInTheDocument();
    expect(screen.getByLabelText("对话")).toBeChecked();
    expect(screen.getByText("已启用 9 项")).toBeVisible();
    expect(screen.getByRole("heading", { name: "令牌限制" })).toBeVisible();
    expect(screen.getByLabelText(/^最大上下文令牌/)).toBeVisible();
    expect(screen.getByLabelText(/^最大输出令牌/)).toBeVisible();
    expect(screen.getByLabelText("对话").closest("label")).toHaveClass("capability-option");
    expect(screen.queryByLabelText("内容审核")).not.toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("部署名称"), { target: { value: "GPT production" } });
    fireEvent.change(screen.getByLabelText(/^模型 ID/), { target: { value: "gpt-5" } });
    fireEvent.click(screen.getByRole("button", { name: "保存为停用" }));

    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    expect(create).toHaveBeenCalledWith(expect.objectContaining({
      name: "GPT production",
      provider_model: "gpt-5",
      target_kind: "model_id",
      enabled: false,
      priority: 0,
      weight: 1,
    }));
  });

  it("discovers OpenAI model IDs, refreshes the catalog, and preserves manual entry", async () => {
    vi.mocked(api.providerModels).mockResolvedValue({
      items: Array.from({ length: 12 }, (_, index) => ({ id: `gpt-model-${index}`, owned_by: "openai" })),
      fetched_at: "2026-08-02T00:00:00Z",
      expires_at: "2026-08-02T00:05:00Z",
      cached: false,
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));
    const modelInput = screen.getByLabelText(/^模型 ID/);
    await waitFor(() => expect(api.providerModels).toHaveBeenCalledWith(provider.id, ""));
    const highlightedCount = await screen.findByText("12");
    expect(highlightedCount).toHaveClass("deployment-model-count");
    expect(highlightedCount.closest('[role="status"]')).toHaveTextContent("已发现 12 个模型；也可手动输入");
    fireEvent.focus(modelInput);
    const listbox = screen.getByRole("listbox", { name: "可用模型" });
    expect(within(listbox).getAllByRole("option")).toHaveLength(12);

    fireEvent.change(modelInput, { target: { value: "gpt-model-11" } });
    expect(within(listbox).getAllByRole("option")).toHaveLength(1);
    fireEvent.click(within(listbox).getByRole("option", { name: /gpt-model-11/ }));
    expect(modelInput).toHaveValue("gpt-model-11");
    expect(screen.queryByRole("listbox", { name: "可用模型" })).not.toBeInTheDocument();
    fireEvent.click(modelInput);
    expect(screen.getByRole("listbox", { name: "可用模型" })).toBeVisible();
    fireEvent.pointerDown(document.body);
    expect(screen.queryByRole("listbox", { name: "可用模型" })).not.toBeInTheDocument();

    fireEvent.change(modelInput, { target: { value: "gpt-manual-preview" } });
    expect(modelInput).toHaveValue("gpt-manual-preview");
    fireEvent.click(screen.getByRole("button", { name: "刷新模型列表" }));
    await waitFor(() => expect(api.providerModels).toHaveBeenCalledWith(provider.id, "", true));
  });

  it("locks an existing invocation target and creates replacements as disabled deployments", async () => {
    const deployment = {
      id: "deployment_live", name: "Live GPT", provider_id: provider.id, provider_model: "gpt-5",
      target_kind: "model_id", access_surface: provider.access_surface, profile_id: provider.profile_id, region: "",
      capabilities, capability_evidence: {}, input_micros_per_million: 100,
      output_micros_per_million: 200, fixed_request_micros_usd: 0, max_concurrency: 4,
      priority: 0, weight: 1, enabled: true, revision: 5, created_at: "", updated_at: "",
    } as Deployment;
    vi.mocked(api.deployments).mockResolvedValue({ items: [deployment], next_cursor: "" });
    const create = vi.spyOn(api, "createDeployment").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "编辑" }));
    expect(screen.getByLabelText("服务商")).toBeDisabled();
    expect(screen.getByLabelText(/^模型 ID/)).toBeDisabled();
    expect(screen.getByText("调用目标创建后不可修改")).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "关闭" }));

    fireEvent.click(screen.getByLabelText("更多操作"));
    fireEvent.click(screen.getByRole("button", { name: "创建替代" }));
    expect(screen.getByRole("heading", { name: "创建替代部署" })).toBeVisible();
    expect(screen.getByLabelText(/^模型 ID/)).toHaveValue("gpt-5");
    fireEvent.change(screen.getByLabelText(/^模型 ID/), { target: { value: "gpt-5-next" } });
    fireEvent.click(screen.getByRole("button", { name: "保存替代部署" }));

    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    expect(create).toHaveBeenCalledWith(expect.objectContaining({ provider_model: "gpt-5-next", target_kind: "model_id", enabled: false }));
  });

  it("requires a successful test before quick-enabling a disabled deployment", async () => {
    const deployment = {
      id: "deployment_gpt", name: "GPT", provider_id: provider.id, provider_model: "gpt-5",
      access_surface: provider.access_surface, profile_id: provider.profile_id, region: "",
      capabilities, capability_evidence: {}, input_micros_per_million: 0,
      output_micros_per_million: 0, fixed_request_micros_usd: 0, max_concurrency: 4,
      priority: 0, weight: 1, enabled: false, revision: 3, created_at: "", updated_at: "",
    } as Deployment;
    vi.mocked(api.deployments).mockResolvedValue({ items: [deployment], next_cursor: "" });
    vi.spyOn(api, "testDeployment").mockImplementation(async () => {
      Object.assign(deployment, {
        revision: 4,
        last_test_status: "healthy",
        last_tested_at: "2026-08-02T01:00:00Z",
        last_test_latency_millis: 9,
        last_test_revision: 4,
      });
      return { status: "healthy", latency_ms: 9, tested_at: "2026-08-02T01:00:00Z", revision: 4 };
    });
    const update = vi.spyOn(api, "updateDeployment").mockResolvedValue({} as never);
    renderPage();

    const enable = await screen.findByRole("button", { name: "启用" });
    expect(enable).toBeDisabled();
    fireEvent.click(screen.getByRole("button", { name: "测试" }));
    await waitFor(() => expect(screen.getByRole("button", { name: "启用" })).toBeEnabled());
    fireEvent.click(screen.getByRole("button", { name: "启用" }));

    await waitFor(() => expect(update).toHaveBeenCalledOnce());
    expect(update).toHaveBeenCalledWith(deployment.id, expect.objectContaining({ enabled: true }), 4);
  });

  it("explains missing versioned pricing instead of reporting a revision conflict", async () => {
    const deployment = {
      id: "deployment_legacy", name: "Legacy GPT", provider_id: provider.id, provider_model: "gpt-5",
      access_surface: provider.access_surface, profile_id: provider.profile_id, region: "",
      capabilities, capability_evidence: {}, input_micros_per_million: 0,
      output_micros_per_million: 0, fixed_request_micros_usd: 0, max_concurrency: 4,
      priority: 0, weight: 1, enabled: false, revision: 4,
      last_test_status: "healthy", last_test_revision: 4, last_tested_at: "2026-08-02T01:00:00Z",
      created_at: "", updated_at: "",
    } as Deployment;
    vi.mocked(api.deployments).mockResolvedValue({ items: [deployment], next_cursor: "" });
    vi.spyOn(api, "updateDeployment").mockRejectedValue(new ApiError(
      409,
      "deployment requires an effective versioned price before enable",
      "deployment_price_unavailable",
    ));
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "启用" }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveClass("deployment-card-error");
    expect(alert).toHaveTextContent("该部署没有已生效的价格版本");
    expect(alert).not.toHaveTextContent("数据已被其他操作修改");
  });

  it("requires confirmation before disabling an enabled deployment", async () => {
    const deployment = {
      id: "deployment_live", name: "Live GPT", provider_id: provider.id, provider_model: "gpt-5",
      access_surface: provider.access_surface, profile_id: provider.profile_id, region: "",
      capabilities, capability_evidence: {}, input_micros_per_million: 0,
      output_micros_per_million: 0, fixed_request_micros_usd: 0, max_concurrency: 4,
      priority: 0, weight: 1, enabled: true, revision: 3, created_at: "", updated_at: "",
    } as Deployment;
    vi.mocked(api.deployments).mockResolvedValue({ items: [deployment], next_cursor: "" });
    const update = vi.spyOn(api, "updateDeployment").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "禁用" }));
    let dialog = screen.getByRole("alertdialog", { name: "禁用模型部署？" });
    expect(dialog).toHaveTextContent("确认禁用模型部署“Live GPT”？该部署将立即停止接收新的模型请求。");
    expect(update).not.toHaveBeenCalled();
    fireEvent.click(within(dialog).getByRole("button", { name: "取消" }));
    expect(screen.queryByRole("alertdialog", { name: "禁用模型部署？" })).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "禁用" }));
    dialog = screen.getByRole("alertdialog", { name: "禁用模型部署？" });
    fireEvent.click(within(dialog).getByRole("button", { name: "禁用" }));
    await waitFor(() => expect(update).toHaveBeenCalledOnce());
  });

  it("renders a compact searchable list and expands details on demand", async () => {
    const items = Array.from({ length: 20 }, (_, index) => ({
      id: `deployment_${index}`, name: index === 19 ? "Special Vision" : `GPT ${index + 1}`,
      provider_id: provider.id, provider_model: index === 19 ? "vision-special" : `gpt-${index + 1}`,
      access_surface: provider.access_surface, profile_id: provider.profile_id, region: "",
      capabilities, capability_evidence: {}, input_micros_per_million: 0,
      output_micros_per_million: 0, fixed_request_micros_usd: 0, max_concurrency: 4,
      priority: 0, weight: 1, enabled: index % 2 === 0, revision: 1, created_at: "", updated_at: "",
    })) as Deployment[];
    vi.mocked(api.deployments).mockResolvedValue({ items, next_cursor: "" });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [], next_cursor: "" });
    // Older servers serialized an empty nil slice as null. The details view
    // must remain usable while clients and servers are upgraded independently.
    vi.spyOn(api, "deploymentPriceProposals").mockResolvedValue({ items: null as never, next_cursor: "" });
    renderPage();

    expect(await screen.findByText("显示 20 / 20 个部署")).toBeVisible();
    expect(screen.getAllByRole("button", { name: "查看详情" })).toHaveLength(20);
    expect(screen.queryByText("输入价格")).not.toBeInTheDocument();

    fireEvent.change(screen.getByPlaceholderText("搜索名称、模型或服务商"), { target: { value: "vision-special" } });
    expect(screen.getByText("显示 1 / 20 个部署")).toBeVisible();
    expect(screen.getByText("Special Vision")).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "查看详情" }));
    expect(screen.getByText("输入价格")).toBeVisible();
    expect(await screen.findByText("没有待处理建议。")).toBeVisible();
    expect(screen.getByText("不可变价格时间线").closest("section")).toHaveClass("deployment-pricing-panel");
    expect(screen.getByText("价格建议").closest("section")).toHaveClass("deployment-pricing-panel");
    expect(screen.getByRole("button", { name: "＋ 新建价格版本" }).closest("header")).not.toBeNull();
    expect(screen.getByRole("button", { name: "导入建议" }).closest("header")).not.toBeNull();
    expect(screen.getByRole("button", { name: "收起详情" })).toHaveAttribute("aria-expanded", "true");
  });
});

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><DeploymentsPage /></QueryClientProvider>);
}
