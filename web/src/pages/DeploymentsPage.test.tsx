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
});

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><DeploymentsPage /></QueryClientProvider>);
}
