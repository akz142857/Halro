import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api";
import type { Deployment, Provider, ProviderCapabilities, ProviderModelDescriptor } from "../types";
import { DeploymentsPage, localDateTimeValue } from "./DeploymentsPage";

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

const noCapabilities: ProviderCapabilities = {
  chat: false, streaming: false, embeddings: false, moderations: false, images: false,
  transcriptions: false, speech: false, files: false, batches: false, rerank: false,
  async_generate: false, tools: false, vision: false, json_mode: false,
  developer_role: false, reasoning: false, stream_usage: false,
  max_context_tokens: 0, max_output_tokens: 0,
};

// Unknown is the ordinary case: the builtin catalog does not seed chat model
// line-ups, so a discovered OpenAI model carries no capabilities of its own.
function unknownModel(id: string): ProviderModelDescriptor {
  return {
    id, owned_by: "openai", status: "unknown", capabilities: noCapabilities,
    capability_evidence: {}, capability_source: "unsupported", preselect: false,
    model_revision: `sha256:${id}`, profile_candidates: [],
  };
}

function knownModel(id: string, capabilities: Partial<ProviderCapabilities>): ProviderModelDescriptor {
  return {
    id, owned_by: "openai", status: "known", capabilities: { ...noCapabilities, ...capabilities },
    capability_evidence: { chat: "declared" }, capability_source: "builtin_catalog", preselect: true,
    model_revision: `sha256:known-${id}`,
    profile_candidates: [{
      binding_id: "b-chat", profile_id: "openai.chat-embeddings.v1", status: "known", selected: true,
      capabilities: { ...noCapabilities, ...capabilities }, profile_capabilities: { ...noCapabilities, ...capabilities },
    }],
  };
}

// A provider reachable through two internal interfaces. Which one serves a
// given model is the question the create form used to ask first.
const chatInterface = { ...noCapabilities, chat: true, streaming: true, stream_usage: true };
const embedInterface = { ...noCapabilities, embeddings: true };
const multiInterfaceProvider = {
  ...provider,
  id: "provider_multi",
  capabilities: { ...chatInterface, embeddings: true },
  bindings: [
    { id: "b-chat", profile_id: "openai.chat-embeddings.v1", enabled: true, capabilities: chatInterface },
    { id: "b-embed", profile_id: "openai.embeddings.v1", enabled: true, capabilities: embedInterface },
  ],
} as Provider;

describe("deployment release workflow", () => {
  beforeEach(() => {
    vi.spyOn(api, "providers").mockResolvedValue({ items: [provider], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "routes").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "providerModels").mockResolvedValue({
      items: [unknownModel("gpt-5"), unknownModel("gpt-4.1")],
      catalog_revision: "sha256:catalog",
      fetched_at: "2026-08-02T00:00:00Z",
      expires_at: "2026-08-02T00:05:00Z",
      cached: false,
    });
  });

  afterEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/deployments");
  });

  it("waits for providers before mounting the onboarding deployment form", async () => {
    window.history.replaceState({}, "", "/admin/deployments?intent=create&onboarding=first-request");
    let resolveProviders!: (value: Awaited<ReturnType<typeof api.providers>>) => void;
    vi.mocked(api.providers).mockImplementation(() => new Promise((resolve) => { resolveProviders = resolve; }));
    renderPage();

    expect(screen.queryByRole("dialog", { name: "创建模型部署" })).not.toBeInTheDocument();
    await act(async () => resolveProviders({ items: [provider], next_cursor: "" }));

    const dialog = await screen.findByRole("dialog", { name: "创建模型部署" });
    expect(within(dialog).getByRole("heading", { name: "模型能力" })).toBeVisible();
    // The deep-linked onboarding form is the same form: it opens with nothing
    // declared rather than assuming the provider ceiling describes the model.
    expect(within(dialog).getByLabelText("对话")).not.toBeChecked();
  });

  it("creates a deployment disabled and keeps route policy out of the form", async () => {
    const create = vi.spyOn(api, "createDeployment").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));
    expect(screen.queryByLabelText("区域")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("默认优先级")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("权重")).not.toBeInTheDocument();
    // A new deployment starts with nothing enabled: the provider ceiling is no
    // longer an answer about what this model does.
    expect(screen.getByLabelText("对话")).not.toBeChecked();
    expect(screen.getByText("已启用 0 项")).toBeVisible();
    expect(screen.getByRole("heading", { name: "令牌限制" })).toBeVisible();
    expect(screen.getByLabelText(/^最大上下文令牌/)).toBeVisible();
    expect(screen.getByLabelText(/^最大输出令牌/)).toBeVisible();
    expect(screen.getByLabelText("对话").closest("label")).toHaveClass("capability-option");
    expect(screen.queryByLabelText("内容审核")).not.toBeInTheDocument();
    fireEvent.change(screen.getByLabelText("部署名称"), { target: { value: "GPT production" } });
    fireEvent.change(screen.getByLabelText(/^模型 ID/), { target: { value: "gpt-5" } });
    fireEvent.click(screen.getByLabelText("对话"));
    fireEvent.click(screen.getByRole("button", { name: "保存为停用" }));

    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    expect(create).toHaveBeenCalledWith(expect.objectContaining({
      name: "GPT production",
      provider_model: "gpt-5",
      target_kind: "model_id",
      // A hand-typed model is not catalogued, so what the operator ticked is a
      // declaration and has to say so.
      mode: "operator_declared",
      enabled: false,
      priority: 0,
      weight: 1,
    }));
  });

  it("carries a catalogued model's capabilities and does not call it a declaration", async () => {
    vi.mocked(api.providerModels).mockResolvedValue({
      items: [knownModel("catalogued-embedder", { embeddings: true, max_context_tokens: 8192 })],
      catalog_revision: "sha256:catalog",
      fetched_at: "2026-08-02T00:00:00Z",
      expires_at: "2026-08-02T00:05:00Z",
      cached: false,
    });
    const create = vi.spyOn(api, "createDeployment").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));
    fireEvent.change(screen.getByLabelText("部署名称"), { target: { value: "Embedder" } });
    const modelInput = screen.getByLabelText(/^模型 ID/);
    fireEvent.focus(modelInput);
    const listbox = await screen.findByRole("listbox", { name: "可用模型" });
    fireEvent.click(await within(listbox).findByRole("option", { name: /catalogued-embedder/ }));

    // The catalog answered, so the capability arrives checked and the notice
    // says where it came from.
    expect(screen.getByLabelText("向量嵌入")).toBeChecked();
    expect(screen.getByText(/能力来自内置模型目录/)).toBeVisible();

    fireEvent.click(screen.getByRole("button", { name: "保存为停用" }));
    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    const payload = create.mock.calls[0][0] as Record<string, unknown>;
    expect(payload).not.toHaveProperty("mode");
    expect(payload.model_revision).toBe("sha256:known-catalogued-embedder");
    expect(payload.capabilities).toMatchObject({ embeddings: true, chat: false });
  });

  it("requires an explicit declaration for a model the catalog does not cover", async () => {
    const create = vi.spyOn(api, "createDeployment").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));
    fireEvent.change(screen.getByLabelText("部署名称"), { target: { value: "GPT" } });
    const modelInput = screen.getByLabelText(/^模型 ID/);
    fireEvent.focus(modelInput);
    const listbox = await screen.findByRole("listbox", { name: "可用模型" });
    fireEvent.click(await within(listbox).findByRole("option", { name: /gpt-5/ }));

    // Nothing is established about it, so nothing arrives checked and the form
    // says the operator is the one making the claim.
    expect(screen.getByLabelText("对话")).not.toBeChecked();
    expect(screen.getByText(/该模型的能力未收录/)).toBeVisible();

    fireEvent.click(screen.getByLabelText("对话"));
    fireEvent.click(screen.getByRole("button", { name: "保存为停用" }));
    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    const payload = create.mock.calls[0][0] as Record<string, unknown>;
    expect(payload.mode).toBe("operator_declared");
    expect(payload.model_revision).toBe("sha256:gpt-5");
    expect(payload.capabilities).toMatchObject({ chat: true });
  });

  it("discovers OpenAI model IDs, refreshes the catalog, and preserves manual entry", async () => {
    vi.mocked(api.providerModels).mockResolvedValue({
      items: Array.from({ length: 12 }, (_, index) => unknownModel(`gpt-model-${index}`)),
      catalog_revision: "sha256:catalog",
      fetched_at: "2026-08-02T00:00:00Z",
      expires_at: "2026-08-02T00:05:00Z",
      cached: false,
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));
    const modelInput = screen.getByLabelText(/^模型 ID/);
    // One aggregate call across every enabled interface, with no binding
    // filter: choosing an interface is no longer a step the operator takes.
    await waitFor(() => expect(api.providerModels).toHaveBeenCalledWith(provider.id));
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
    renderPage();

    expect(await screen.findByText("显示 20 / 20 个部署")).toBeVisible();
    expect(screen.getAllByRole("button", { name: "查看详情" })).toHaveLength(20);
    expect(screen.queryByText("输入价格")).not.toBeInTheDocument();

    fireEvent.change(screen.getByPlaceholderText("搜索名称、模型或服务商"), { target: { value: "vision-special" } });
    expect(screen.getByText("显示 1 / 20 个部署")).toBeVisible();
    expect(screen.getByText("Special Vision")).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "查看详情" }));
    expect(screen.getByText("输入价格")).toBeVisible();
    expect(await screen.findByText("不可变价格时间线")).toBeVisible();
    expect(screen.getByText("不可变价格时间线").closest("section")).toHaveClass("deployment-pricing-panel");
    expect(screen.getByRole("button", { name: "设置价格" }).closest("header")).not.toBeNull();
    expect(screen.queryByRole("button", { name: "导入建议" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "收起详情" })).toHaveAttribute("aria-expanded", "true");
  });

  it("sets a manual price in two steps without asking for or sending a content digest", async () => {
    const deployment = {
      id: "deployment_price", name: "Price GPT", provider_id: provider.id, provider_model: "gpt-5",
      access_surface: provider.access_surface, profile_id: provider.profile_id, region: "",
      capabilities, capability_evidence: {}, input_micros_per_million: 0,
      output_micros_per_million: 0, fixed_request_micros_usd: 0, max_concurrency: 4,
      priority: 0, weight: 1, enabled: false, revision: 1, created_at: "", updated_at: "",
    } as Deployment;
    vi.mocked(api.deployments).mockResolvedValue({ items: [deployment], next_cursor: "" });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [], next_cursor: "" });
    const createPrice = vi.spyOn(api, "createDeploymentPrice").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "查看详情" }));
    fireEvent.click(await screen.findByRole("button", { name: "设置价格" }));
    expect(screen.getByRole("heading", { name: "设置价格" })).toBeVisible();
    expect(screen.queryByText("来源内容 SHA-256")).not.toBeInTheDocument();
	expect(screen.queryByLabelText("当前密码")).not.toBeInTheDocument();
	expect(screen.getByLabelText("价格依据")).toHaveValue("temporary_estimate");
	fireEvent.change(screen.getByLabelText("价格依据"), { target: { value: "official_public_price" } });
	expect(screen.getByText("选择官方公开价或合同价时，请填写可供复核的来源说明。")).toBeVisible();
	expect(screen.getByRole("button", { name: "下一步：核对" })).toBeDisabled();
	fireEvent.change(screen.getByLabelText("价格依据"), { target: { value: "temporary_estimate" } });

	fireEvent.change(screen.getByLabelText("输入 USD / 百万令牌"), { target: { value: "2.5" } });
    fireEvent.change(screen.getByLabelText("输出 USD / 百万令牌"), { target: { value: "10" } });
    fireEvent.click(screen.getByRole("button", { name: "下一步：核对" }));

    expect(screen.getByText("请核对价格")).toBeVisible();
    await waitFor(() => expect(screen.getByText("请核对价格").closest("section")).toHaveFocus());
    expect(screen.getByText("管理员声明 · Halro 未验证 · 未归档")).toBeVisible();
    expect(screen.getByLabelText("当前密码")).toBeVisible();
    fireEvent.change(screen.getByLabelText("当前密码"), { target: { value: "admin-password" } });
    fireEvent.click(screen.getByRole("button", { name: "确认并创建价格版本" }));

    await waitFor(() => expect(createPrice).toHaveBeenCalledOnce());
    expect(createPrice).toHaveBeenCalledWith(deployment.id, expect.objectContaining({
      billing_mode: "metered",
      input_usd_per_million: "2.5",
      output_usd_per_million: "10",
      source: {
        type: "manual",
        reference: "temporary_estimate",
        note: "",
        asserted_without_archive: true,
      },
      current_password: "admin-password",
    }), expect.any(String));
    expect((createPrice.mock.calls[0][1] as { source: Record<string, unknown> }).source).not.toHaveProperty("content_sha256");
  });

  it("formats datetime-local defaults in the administrator's timezone", () => {
    const singaporeDate = {
      getTime: () => Date.UTC(2026, 7, 5, 12, 30),
      getTimezoneOffset: () => -480,
    } as Date;
    expect(localDateTimeValue(singaporeDate)).toBe("2026-08-05T20:30");
  });

  it("says why disable and delete are blocked while an enabled route references the deployment", async () => {
    const deployment = {
      id: "deployment_referenced", name: "Referenced GPT", provider_id: provider.id, provider_model: "gpt-5",
      access_surface: provider.access_surface, profile_id: provider.profile_id, region: "",
      capabilities, capability_evidence: {}, input_micros_per_million: 0,
      output_micros_per_million: 0, fixed_request_micros_usd: 0, max_concurrency: 4,
      priority: 0, weight: 1, enabled: true, revision: 1, created_at: "", updated_at: "",
    } as Deployment;
    vi.mocked(api.deployments).mockResolvedValue({ items: [deployment], next_cursor: "" });
    vi.mocked(api.routes).mockResolvedValue({
      items: [{
        id: "route_gpt", public_model: "gpt", deployment_id: deployment.id, priority: 0,
        strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "",
      }],
      next_cursor: "",
    });
    renderPage();

    const disable = await screen.findByRole("button", { name: "禁用" });
    expect(disable).toBeDisabled();
    expect(disable).toHaveAccessibleDescription("请先停用引用该部署的模型路由");
    fireEvent.click(screen.getByLabelText("更多操作"));
    const remove = screen.getByRole("button", { name: "删除" });
    expect(remove).toBeDisabled();
    expect(remove).toHaveAccessibleDescription("请先停用引用该部署的模型路由");
  });

  it("retries an ambiguous price result with the exact payload and idempotency key", async () => {
    const deployment = {
      id: "deployment_ambiguous", name: "Ambiguous GPT", provider_id: provider.id, provider_model: "gpt-5",
      access_surface: provider.access_surface, profile_id: provider.profile_id, region: "",
      capabilities, capability_evidence: {}, input_micros_per_million: 0,
      output_micros_per_million: 0, fixed_request_micros_usd: 0, max_concurrency: 4,
      priority: 0, weight: 1, enabled: false, revision: 1, created_at: "", updated_at: "",
    } as Deployment;
    vi.mocked(api.deployments).mockResolvedValue({ items: [deployment], next_cursor: "" });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [], next_cursor: "" });
    const createPrice = vi.spyOn(api, "createDeploymentPrice").mockRejectedValue(new ApiError(500, "ambiguous upstream result"));
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "查看详情" }));
    fireEvent.click(await screen.findByRole("button", { name: "设置价格" }));
    fireEvent.change(screen.getByLabelText("输入 USD / 百万令牌"), { target: { value: "1" } });
    fireEvent.click(screen.getByRole("button", { name: "下一步：核对" }));
    fireEvent.change(screen.getByLabelText("当前密码"), { target: { value: "admin-password" } });
    fireEvent.click(screen.getByRole("button", { name: "确认并创建价格版本" }));

    expect(await screen.findByText(/上次结果不明确/)).toBeVisible();
    expect(screen.queryByRole("button", { name: "返回修改" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "按原内容重试" }));
    await waitFor(() => expect(createPrice).toHaveBeenCalledTimes(2));
    expect(createPrice.mock.calls[1][1]).toEqual(createPrice.mock.calls[0][1]);
    expect(createPrice.mock.calls[1][2]).toBe(createPrice.mock.calls[0][2]);
  });

  // Capability state is derived by the server, so the console must show what the
  // server says rather than assuming a saved deployment is still supported.
  it("shows why a drifted deployment stopped routing, and which capabilities went", async () => {
    const deployment = {
      id: "deployment_drifted", name: "Drifted GPT", provider_id: provider.id, provider_model: "gpt-5",
      access_surface: provider.access_surface, profile_id: provider.profile_id, region: "",
      capabilities, capability_evidence: {}, input_micros_per_million: 0,
      output_micros_per_million: 0, fixed_request_micros_usd: 0, max_concurrency: 4,
      priority: 0, weight: 1, enabled: true, revision: 6, created_at: "", updated_at: "",
      capability_review: {
        state: "drifted", source: "builtin_catalog", status: "known", model_revision: "sha256:old",
        catalog_covered: true, catalog_source: "builtin_catalog", catalog_status: "known",
        catalog_model_revision: "sha256:new", no_longer_supported: ["vision"],
        reason: "catalog_establishes_less",
      },
    } as Deployment;
    vi.mocked(api.deployments).mockResolvedValue({ items: [deployment], next_cursor: "" });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [], next_cursor: "" });
    renderPage();

    // Visible without expanding: a drifted deployment is not routing, whatever
    // the enabled column says.
    expect(await screen.findByText("能力已不再受支持")).toBeVisible();

    fireEvent.click(screen.getByRole("button", { name: /查看详情/ }));
    expect(await screen.findByText(/模型目录现在确立的能力少于/)).toBeVisible();
    // Scoped to the review panel: the capability badge strip lists names too,
    // and a match there would not prove the review named what it lost.
    const facts = within(document.querySelector(".deployment-review-facts") as HTMLElement);
    expect(facts.getByText("内置模型目录")).toBeVisible();
    expect(facts.getByText("视觉")).toBeVisible();
    expect(screen.getByText(/该部署已停止承接流量/)).toBeVisible();
  });

  it("keeps serving and offers the new capabilities when the catalog moved forward", async () => {
    const deployment = {
      id: "deployment_review", name: "Reviewable GPT", provider_id: provider.id, provider_model: "gpt-5",
      access_surface: provider.access_surface, profile_id: provider.profile_id, region: "",
      capabilities, capability_evidence: {}, input_micros_per_million: 0,
      output_micros_per_million: 0, fixed_request_micros_usd: 0, max_concurrency: 4,
      priority: 0, weight: 1, enabled: true, revision: 6, created_at: "", updated_at: "",
      capability_review: {
        state: "review_available", source: "operator_declared", status: "partial",
        model_revision: "sha256:declared", catalog_covered: true, catalog_source: "builtin_catalog",
        catalog_status: "known", catalog_model_revision: "sha256:new",
        available_for_review: ["reasoning"], reason: "catalog_now_covers_model",
      },
    } as Deployment;
    vi.mocked(api.deployments).mockResolvedValue({ items: [deployment], next_cursor: "" });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [], next_cursor: "" });
    renderPage();

    expect(await screen.findByText("有可复核的新能力")).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: /查看详情/ }));
    const facts = within(await waitFor(() => document.querySelector(".deployment-review-facts") as HTMLElement));
    expect(facts.getByText("管理员声明")).toBeVisible();
    expect(facts.getByText("推理")).toBeVisible();
    // The offer must not read as something already in effect.
    expect(screen.getByText(/它仍然只做今天在做的事/)).toBeVisible();
  });

  // Narrowing is allowed in place, so the console has to say which routes it
  // would strand before it writes rather than after traffic starts failing.
  it("preflights a capability narrowing and requires confirmation when a route loses its only candidate", async () => {
    const deployment = {
      id: "deployment_narrow", name: "Narrowing GPT", provider_id: provider.id, provider_model: "gpt-5",
      target_kind: "model_id", access_surface: provider.access_surface, profile_id: provider.profile_id, region: "",
      capabilities, capability_evidence: {}, input_micros_per_million: 0,
      output_micros_per_million: 0, fixed_request_micros_usd: 0, max_concurrency: 4,
      priority: 0, weight: 1, enabled: true, revision: 7, created_at: "", updated_at: "",
    } as Deployment;
    vi.mocked(api.deployments).mockResolvedValue({ items: [deployment], next_cursor: "" });
    const preflight = vi.spyOn(api, "preflightDeploymentCapabilities").mockResolvedValue({
      removed_capabilities: ["tools"],
      added_capabilities: [],
      affected_routes: [{ route_id: "route_1", public_model: "gpt-4o", capability: "tools", sole_candidate: true }],
      blocking: true,
    });
    const update = vi.spyOn(api, "updateDeployment").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "编辑" }));
    fireEvent.click(screen.getByRole("checkbox", { name: /工具调用/ }));

    // The first submit asks the server, it does not write.
    fireEvent.click(screen.getByRole("button", { name: "检查路由影响" }));
    await waitFor(() => expect(preflight).toHaveBeenCalledOnce());
    expect(update).not.toHaveBeenCalled();

    expect(await screen.findByText("部分路由将失去唯一候选")).toBeVisible();
    expect(screen.getByText("gpt-4o")).toBeVisible();
    expect(screen.getByText("没有其他部署可以承接")).toBeVisible();

    // Only an explicit confirmation writes.
    fireEvent.click(screen.getByRole("button", { name: "仍然保存并热加载" }));
    await waitFor(() => expect(update).toHaveBeenCalledOnce());
    expect(update).toHaveBeenCalledWith(deployment.id, expect.objectContaining({
      capabilities: expect.objectContaining({ tools: false }),
    }), 7);
  });

  // Nothing is stranded, so there is nothing to confirm: the save goes through
  // on the preflight's own answer rather than making the operator click twice.
  it("saves a narrowing straight through when no route is affected", async () => {
    const deployment = {
      id: "deployment_safe", name: "Safe GPT", provider_id: provider.id, provider_model: "gpt-5",
      target_kind: "model_id", access_surface: provider.access_surface, profile_id: provider.profile_id, region: "",
      capabilities, capability_evidence: {}, input_micros_per_million: 0,
      output_micros_per_million: 0, fixed_request_micros_usd: 0, max_concurrency: 4,
      priority: 0, weight: 1, enabled: true, revision: 2, created_at: "", updated_at: "",
    } as Deployment;
    vi.mocked(api.deployments).mockResolvedValue({ items: [deployment], next_cursor: "" });
    vi.spyOn(api, "preflightDeploymentCapabilities").mockResolvedValue({
      removed_capabilities: ["tools"], added_capabilities: [], affected_routes: [], blocking: false,
    });
    const update = vi.spyOn(api, "updateDeployment").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "编辑" }));
    fireEvent.click(screen.getByRole("checkbox", { name: /工具调用/ }));
    fireEvent.click(screen.getByRole("button", { name: "检查路由影响" }));

    await waitFor(() => expect(update).toHaveBeenCalledOnce());
  });

  // Widening does not remove a candidate from anything, so it must not pay the
  // cost of a preflight round trip.
  it("does not preflight when capabilities only widen", async () => {
    const narrow = { ...capabilities, tools: false };
    const deployment = {
      id: "deployment_widen", name: "Widening GPT", provider_id: provider.id, provider_model: "gpt-5",
      target_kind: "model_id", access_surface: provider.access_surface, profile_id: provider.profile_id, region: "",
      capabilities: narrow, capability_evidence: {}, input_micros_per_million: 0,
      output_micros_per_million: 0, fixed_request_micros_usd: 0, max_concurrency: 4,
      priority: 0, weight: 1, enabled: true, revision: 3, created_at: "", updated_at: "",
    } as Deployment;
    vi.mocked(api.deployments).mockResolvedValue({ items: [deployment], next_cursor: "" });
    const preflight = vi.spyOn(api, "preflightDeploymentCapabilities").mockResolvedValue({
      removed_capabilities: [], added_capabilities: ["tools"], affected_routes: [], blocking: false,
    });
    const update = vi.spyOn(api, "updateDeployment").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "编辑" }));
    fireEvent.click(screen.getByRole("checkbox", { name: /工具调用/ }));
    fireEvent.click(screen.getByRole("button", { name: "保存并热加载" }));

    await waitFor(() => expect(update).toHaveBeenCalledOnce());
    expect(preflight).not.toHaveBeenCalled();
  });

  // Turning a capability on drops the deployment out of routing until it is
  // retested, so the form has to say that before the operator saves.
  // §7.1 puts the internal interface out of the ordinary flow: the operator
  // names a model, and which interface serves it follows from the model and the
  // capabilities. It stays reachable for diagnostics, collapsed and automatic.
  it("creates a deployment without asking which internal interface to use", async () => {
    vi.mocked(api.providers).mockResolvedValue({ items: [multiInterfaceProvider], next_cursor: "" });
    vi.mocked(api.providerModels).mockResolvedValue({
      items: [unknownModel("gpt-5")], catalog_revision: "sha256:catalog",
      fetched_at: "2026-08-02T00:00:00Z", expires_at: "2026-08-02T00:05:00Z", cached: false,
    });
    const create = vi.spyOn(api, "createDeployment").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));
    fireEvent.change(screen.getByLabelText("部署名称"), { target: { value: "GPT" } });

    const interfaceSelect = screen.getByLabelText(/^能力接口/);
    const disclosure = interfaceSelect.closest("details");
    expect(disclosure).not.toHaveAttribute("open");
    expect(interfaceSelect).toHaveValue("");
    expect(disclosure?.querySelector("summary")).toHaveTextContent("自动选择");

    const modelInput = screen.getByLabelText(/^模型 ID/);
    fireEvent.focus(modelInput);
    const listbox = await screen.findByRole("listbox", { name: "可用模型" });
    fireEvent.click(await within(listbox).findByRole("option", { name: /gpt-5/ }));
    fireEvent.click(screen.getByLabelText("对话"));
    fireEvent.click(screen.getByRole("button", { name: "保存为停用" }));

    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    const payload = create.mock.calls[0][0] as Record<string, unknown>;
    expect(payload).not.toHaveProperty("binding_id");
  });

  // The interface ceiling is not an invitation to add what the model does not
  // do. A catalogued model may only be narrowed.
  it("offers only the capabilities the catalog establishes for a known model", async () => {
    vi.mocked(api.providers).mockResolvedValue({ items: [multiInterfaceProvider], next_cursor: "" });
    // The chat interface carries streaming and stream usage; the catalog
    // establishes only that this model does chat.
    vi.mocked(api.providerModels).mockResolvedValue({
      items: [knownModel("catalogued-chat", { chat: true })],
      catalog_revision: "sha256:catalog",
      fetched_at: "2026-08-02T00:00:00Z", expires_at: "2026-08-02T00:05:00Z", cached: false,
    });
    const create = vi.spyOn(api, "createDeployment").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));
    fireEvent.change(screen.getByLabelText("部署名称"), { target: { value: "Chat" } });
    const modelInput = screen.getByLabelText(/^模型 ID/);
    fireEvent.focus(modelInput);
    const listbox = await screen.findByRole("listbox", { name: "可用模型" });
    fireEvent.click(await within(listbox).findByRole("option", { name: /catalogued-chat/ }));

    expect(screen.getByLabelText("对话")).toBeChecked();
    // The interface would carry these; the catalog does not establish them for
    // this model, so they are not on offer.
    expect(screen.queryByLabelText("流式")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("流式用量")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "保存为停用" }));
    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    // The catalog named the interface, so this one is sent.
    expect((create.mock.calls[0][0] as Record<string, unknown>).binding_id).toBe("b-chat");
  });

  // A deployment runs on one interface. Selecting across two is refused by the
  // server, so the form names the reason rather than letting it come back as a
  // bare rejection.
  it("says when a selection no single interface carries has been made", async () => {
    vi.mocked(api.providers).mockResolvedValue({ items: [multiInterfaceProvider], next_cursor: "" });
    vi.mocked(api.providerModels).mockResolvedValue({
      items: [unknownModel("gpt-5")], catalog_revision: "sha256:catalog",
      fetched_at: "2026-08-02T00:00:00Z", expires_at: "2026-08-02T00:05:00Z", cached: false,
    });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));
    fireEvent.click(screen.getByLabelText("对话"));
    expect(screen.queryByText("没有单一能力接口能承载全部所选能力")).not.toBeInTheDocument();

    fireEvent.click(screen.getByLabelText("向量嵌入"));
    expect(await screen.findByText("没有单一能力接口能承载全部所选能力")).toBeVisible();
  });

  it("warns that enabling a capability will take the deployment out of routing", async () => {
    const narrow = { ...capabilities, vision: false };
    const deployment = {
      id: "deployment_expand", name: "Expanding GPT", provider_id: provider.id, provider_model: "gpt-5",
      target_kind: "model_id", access_surface: provider.access_surface, profile_id: provider.profile_id, region: "",
      capabilities: narrow, capability_evidence: {}, input_micros_per_million: 0,
      output_micros_per_million: 0, fixed_request_micros_usd: 0, max_concurrency: 4,
      priority: 0, weight: 1, enabled: true, revision: 9, created_at: "", updated_at: "",
    } as Deployment;
    vi.mocked(api.deployments).mockResolvedValue({ items: [deployment], next_cursor: "" });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "编辑" }));
    expect(screen.queryByText("开启能力需要重新验证")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("checkbox", { name: /视觉/ }));

    expect(await screen.findByText("开启能力需要重新验证")).toBeVisible();
    // It is routed, so the console names the thing that has to happen first.
    expect(screen.getByText(/请先停用这些路由/)).toBeVisible();
  });
});

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><DeploymentsPage /></QueryClientProvider>);
}
