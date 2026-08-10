import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api";
import i18n from "../i18n";
import type { AdminRole, Deployment, DeploymentPriceVersion, DeploymentVariant, InvocationTargetCatalog, Provider, ProviderCapabilities, ResolvedInvocationTarget, Session } from "../types";
import { DeploymentsPage, OVERDUE_SCHEDULED_PRICE_REFRESH_MS, PRICE_FETCH_BATCH_SIZE, priceFetchDelay, scheduledPriceRefreshInterval } from "./DeploymentsPage";

const emptyCapabilities: ProviderCapabilities = {
  chat: false, streaming: false, embeddings: false, moderations: false, images: false,
  transcriptions: false, speech: false, files: false, batches: false, rerank: false,
  async_generate: false, tools: false, vision: false, json_mode: false,
  developer_role: false, reasoning: false, stream_usage: false,
  max_context_tokens: 0, max_output_tokens: 0,
};

const chatCapabilities: ProviderCapabilities = {
  ...emptyCapabilities, chat: true, streaming: true, tools: true, stream_usage: true,
};

const provider = {
  id: "provider_openai", name: "OpenAI production", type: "openai", base_url: "https://api.openai.com",
  access_surface: "openai-api", profile_id: "openai.chat-embeddings.v1", credential_scheme: "bearer.static",
  credential_id: "credential_openai", allowed_hosts: ["api.openai.com"], capabilities: chatCapabilities,
  capability_evidence: {}, max_concurrency: 8, enabled: true, revision: 1, created_at: "", updated_at: "",
  bindings: [
    { id: "b-chat", profile_id: "openai.chat-embeddings.v1", enabled: true, capabilities: chatCapabilities },
    { id: "b-embed", profile_id: "openai.embeddings.v1", enabled: true, capabilities: { ...emptyCapabilities, embeddings: true } },
  ],
} as Provider;

function deployment(id: string, overrides: Partial<Deployment> = {}): Deployment {
  return {
    id, name: `Deployment ${id}`, provider_id: provider.id, binding_id: "b-chat", provider_model: "gpt-chat",
    target_kind: "model_id", access_surface: "openai-api", profile_id: "openai.chat-embeddings.v1",
    capabilities: chatCapabilities, capability_evidence: {}, region: "", max_concurrency: 0,
    priority: 0, weight: 1, enabled: false, revision: 1, created_at: "", updated_at: "",
    ...overrides,
  } as Deployment;
}

const existingDeployment = deployment("dep_1");

function variant(targetID: string, bindingID: string, capabilities: ProviderCapabilities): DeploymentVariant {
  return {
    id: bindingID, binding_id: bindingID,
    profile_id: bindingID === "b-embed" ? "openai.embeddings.v1" : "openai.chat-embeddings.v1",
    target: {
      target_id: targetID, target_kind: "model_id", display_name: targetID, canonical_model_ref: targetID,
      lifecycle: "active", metadata: {}, metadata_source: "none", availability: "available", fetched_at: "2026-08-10T00:00:00Z",
    },
    capabilities,
    capability_claims: Object.entries(capabilities).filter(([, value]) => value === true).map(([name]) => ({
      capability_id: name, status: "supported", evidence: "declared", source: "builtin_catalog",
      scope: { provider_id: provider.id, target_kind: "model_id", target_id: targetID, binding_id: bindingID, profile_id: bindingID === "b-embed" ? "openai.embeddings.v1" : "openai.chat-embeddings.v1" },
      observed_at: "2026-08-10T00:00:00Z", revision: `sha256:${targetID}:${bindingID}:${name}`,
    })),
    resolution_state: "resolved", revision: `sha256:${targetID}:${bindingID}`,
  };
}

function target(targetID: string, variants: DeploymentVariant[], state: ResolvedInvocationTarget["resolution_state"] = variants.length ? "resolved" : "unknown", displayName = targetID, owner = ""): ResolvedInvocationTarget {
  return {
    target_id: targetID, target_kind: "model_id", display_name: displayName, owned_by: owner || undefined,
    canonical_model_ref: targetID, lifecycle: "active", metadata: {}, metadata_source: "none",
    availability: "available", fetched_at: "2026-08-10T00:00:00Z", variants,
    resolution_state: state, resolution_revision: `sha256:resolution:${targetID}`,
  };
}

function catalog(items: ResolvedInvocationTarget[], overrides: Partial<InvocationTargetCatalog> = {}): InvocationTargetCatalog {
  return {
    items,
    discovery: {
      target_kinds: ["model_id"], can_enumerate: true, can_describe: true, can_verify: true,
      requires_management_identity: false, requires_canonical_model_mapping: false,
    },
    catalog_revision: "sha256:catalog", provider_revision: 1,
    fetched_at: "2026-08-10T00:00:00Z", expires_at: "2026-08-10T00:05:00Z", cached: false,
    ...overrides,
  };
}

const unknown = target("gpt-future", [], "unknown", "GPT Future", "system-owner");
const chatTarget = target("gpt-chat", [variant("gpt-chat", "b-chat", chatCapabilities)], "resolved", "GPT Chat");

describe("deployment invocation target workflow", () => {
  beforeEach(() => {
    vi.spyOn(api, "providers").mockResolvedValue({ items: [provider], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "routes").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "invocationTargets").mockResolvedValue(catalog([chatTarget, unknown]));
    vi.spyOn(api, "resolveInvocationTarget").mockImplementation(async (_providerID, targetID) => target(targetID, [], "unknown"));
  });

  afterEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/deployments");
  });

  it("waits for providers before mounting an onboarding create form", async () => {
    window.history.replaceState({}, "", "/admin/deployments?intent=create&onboarding=first-request");
    let resolveProviders!: (value: Awaited<ReturnType<typeof api.providers>>) => void;
    vi.mocked(api.providers).mockImplementation(() => new Promise((resolve) => { resolveProviders = resolve; }));
    renderPage();
    expect(screen.queryByRole("dialog", { name: "创建模型部署" })).not.toBeInTheDocument();
    await act(async () => resolveProviders({ items: [provider], next_cursor: "" }));
    expect(await screen.findByRole("dialog", { name: "创建模型部署" })).toBeVisible();
  });

  it("keeps purpose buttons and the expanded capability matrix out of the ordinary flow", async () => {
    await openCreate();
    expect(screen.queryByText("这个模型主要用于什么？")).not.toBeInTheDocument();
    expect(screen.queryByLabelText("对话")).not.toBeInTheDocument();
    expect(screen.getByText("选择模型以查看能力")).toBeVisible();
    expect(screen.getByText("选择服务商和实际调用的模型；能力接口默认由 Halro 自动确定。")).toBeVisible();
    expect(screen.getByText("这里保存模型实际开放的能力；识别后可以继续关闭不需要的能力。")).toBeVisible();
    expect(screen.getByText("运行限制").closest("details")).toBeNull();
    expect(screen.getByLabelText(/^最大输出令牌/)).toBeVisible();
    expect(screen.queryByText("默认")).not.toBeInTheDocument();
  });

  // "0 is automatic" dropped who enforces the limit once the deployment stops
  // declaring one, which is a billing and throttling fact, not a nicety.
  it("says what a zero token limit actually means on every limit field", async () => {
    await openCreate();
    expect(screen.getByLabelText(/^最大上下文令牌/)).toHaveAccessibleDescription("0 表示不在部署层声明，运行时仍受上游限制");
    expect(screen.getByLabelText(/^最大输出令牌/)).toHaveAccessibleDescription("0 表示不在部署层声明，运行时仍受上游限制；填写后不得超过最大上下文令牌");
    expect(screen.getByLabelText(/^并发上限/)).toHaveAccessibleDescription("0 表示不在部署层限制并发，仅受服务商自身限制");
    expect(screen.queryByText("0 为自动")).not.toBeInTheDocument();
  });

  it("names every unmet condition next to a disabled save button", async () => {
    await openCreate();
    expect(screen.getByRole("button", { name: "保存为停用" })).toBeDisabled();
    const blockers = screen.getByText("尚未完成").closest("div")!;
    expect(within(blockers).getByText("填写部署名称")).toBeVisible();
    expect(within(blockers).getByText("选择或输入模型")).toBeVisible();
    // Nothing downstream of the model is answerable yet, so it is not listed.
    expect(within(blockers).queryByText("至少保留一项核心能力")).not.toBeInTheDocument();

    fireEvent.change(screen.getByLabelText("部署名称"), { target: { value: "Chat" } });
    await choose("GPT Chat");
    await waitFor(() => expect(screen.queryByText("尚未完成")).not.toBeInTheDocument());
    expect(screen.getByRole("button", { name: "保存为停用" })).toBeEnabled();
  });

  it("states the hot-reload consequence when editing and the disabled-on-save rule when creating", async () => {
    await openCreate();
    expect(screen.getByText("保存为停用部署")).toBeVisible();
    expect(screen.getByText("新建的部署保存后处于停用状态，测试通过后才能启用并承接流量。")).toBeVisible();
    expect(screen.queryByText("保存后立即热加载")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "取消" }));
    vi.mocked(api.deployments).mockResolvedValue({ items: [existingDeployment], next_cursor: "" });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [], next_cursor: "" });
    renderPage();
    fireEvent.click((await screen.findAllByRole("button", { name: "编辑" }))[0]);
    expect(await screen.findByText("保存后立即热加载")).toBeVisible();
    expect(screen.getByText(/保存会立即热加载这个部署的配置/)).toBeVisible();
  });

  it("treats legacy null collection fields as empty instead of crashing", async () => {
    vi.mocked(api.invocationTargets).mockResolvedValue({ ...catalog([]), items: null as never });
    await openCreate();
    expect(screen.getByRole("dialog", { name: "创建模型部署" })).toBeVisible();
    expect(screen.getByText("选择模型以查看能力")).toBeVisible();
  });

  it("renders a single-column target list and searches only the visible target name", async () => {
    await openCreate();
    const input = screen.getByLabelText(/^模型 ID/);
    fireEvent.focus(input);
    const listbox = await screen.findByRole("listbox", { name: "可用模型" });
    const options = within(listbox).getAllByRole("option");
    expect(options).toHaveLength(2);
    expect(options[0]).not.toHaveTextContent("能力未知");
    expect(options[0]).not.toHaveTextContent("system-owner");

    fireEvent.change(input, { target: { value: "system-owner" } });
    expect(within(listbox).queryAllByRole("option")).toHaveLength(0);
    fireEvent.change(input, { target: { value: "future" } });
    expect(within(listbox).getByRole("option", { name: "GPT Future" })).toBeVisible();
  });

  it("auto-selects one server variant, shows a read-only summary, and saves its revision", async () => {
    const create = vi.spyOn(api, "createDeployment").mockResolvedValue({} as never);
    await openCreate();
    fireEvent.change(screen.getByLabelText("部署名称"), { target: { value: "Chat" } });
    await choose("GPT Chat");
    const summary = (await screen.findByText("4 项能力可用")).closest("div.deployment-capability-summary") as HTMLElement;
    expect(within(summary).getByText("对话")).toBeVisible();
    expect(within(summary).getByText("Halro 已评审模型目录")).toBeVisible();
    expect(screen.queryByText(/builtin_catalog|verified_probe|provider_metadata/)).not.toBeInTheDocument();
    const advanced = screen.getByText("高级设置：收窄能力").closest("details");
    expect(advanced).not.toHaveAttribute("open");
    fireEvent.click(screen.getByRole("button", { name: "保存为停用" }));
    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    expect(create).toHaveBeenCalledWith(expect.objectContaining({
      provider_model: "gpt-chat", binding_id: "b-chat", resolution_revision: "sha256:gpt-chat:b-chat",
      capabilities: expect.objectContaining({ chat: true }), enabled: false,
    }));
  });

  it("loads a changed resolution and requires explicit confirmation after a 409", async () => {
    const changed = target("gpt-chat", [variant("gpt-chat", "b-chat", { ...chatCapabilities, tools: false })]);
    const create = vi.spyOn(api, "createDeployment")
      .mockRejectedValueOnce(new ApiError(409, "changed", "resolution_changed", "", {
        code: "resolution_changed", mismatches: ["resolution_revision"], resolution: changed,
      }))
      .mockResolvedValueOnce({} as never);
    await openCreate();
    fireEvent.change(screen.getByLabelText("部署名称"), { target: { value: "Chat" } });
    await choose("GPT Chat");
    fireEvent.click(screen.getByRole("button", { name: "保存为停用" }));
    expect(await screen.findByText("模型信息已更新")).toBeVisible();
    expect(screen.getByRole("button", { name: "保存为停用" })).toBeDisabled();
    fireEvent.click(screen.getByRole("radio", { name: /对话/ }));
    fireEvent.click(screen.getByRole("button", { name: "保存为停用" }));
    await waitFor(() => expect(create).toHaveBeenCalledTimes(2));
  });

  it("requires an explicit choice when the resolver returns multiple variants and saves only one", async () => {
    const multi = target("dual-model", [
      variant("dual-model", "b-chat", chatCapabilities),
      variant("dual-model", "b-embed", { ...emptyCapabilities, embeddings: true }),
    ]);
    vi.mocked(api.invocationTargets).mockResolvedValue(catalog([multi]));
    const create = vi.spyOn(api, "createDeployment").mockResolvedValue({} as never);
    await openCreate();
    fireEvent.change(screen.getByLabelText("部署名称"), { target: { value: "Dual" } });
    await choose("dual-model");
    expect(await screen.findByText("这个调用目标可通过 2 种接口部署")).toBeVisible();
    expect(screen.getByRole("button", { name: "保存为停用" })).toBeDisabled();
    fireEvent.click(screen.getByLabelText("向量嵌入"));
    fireEvent.click(screen.getByRole("button", { name: "保存为停用" }));
    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    const payload = create.mock.calls[0][0] as Record<string, unknown>;
    expect(payload.binding_id).toBe("b-embed");
    expect(payload.resolution_revision).toBe("sha256:dual-model:b-embed");
    expect(payload).not.toHaveProperty("operation_bindings");
  });

  it("gives equivalent variants distinct accessible interface labels", async () => {
    const equivalent = target("equivalent-model", [
      variant("equivalent-model", "b-chat", chatCapabilities),
      variant("equivalent-model", "b-embed", chatCapabilities),
    ]);
    vi.mocked(api.invocationTargets).mockResolvedValue(catalog([equivalent]));
    await openCreate();
    await choose("equivalent-model");
    expect(await screen.findByRole("radio", { name: "对话 · 调用接口 1" })).toBeVisible();
    expect(screen.getByRole("radio", { name: "对话 · 调用接口 2" })).toBeVisible();
  });

  it("shows the provider configuration exit for zero variants without offering verification", async () => {
    vi.mocked(api.invocationTargets).mockResolvedValue(catalog([target("blocked", [], "no_variant")]));
    await openCreate();
    await choose("blocked");
    expect(await screen.findByText("当前服务商尚未启用可承载此目标的调用接口")).toBeVisible();
    expect(screen.getByRole("link", { name: "前往服务商配置 →" })).toHaveAttribute("href", "/admin/providers");
    expect(screen.queryByRole("button", { name: "识别能力" })).not.toBeInTheDocument();
  });

  it("offers only verification and advanced onboarding for an unknown target", async () => {
    await openCreate();
    await choose("GPT Future");
    expect(await screen.findByRole("button", { name: "识别能力" })).toBeDisabled();
    expect(screen.getByText("最多 8 次低成本验证")).toBeVisible();
    expect(screen.getByRole("button", { name: "手动配置" })).toBeVisible();
    expect(screen.queryByLabelText("对话")).not.toBeInTheDocument();
  });

  // Detection pins the interface it ran on and the deployment target is
  // immutable afterwards, so taking bindings[0] silently means the wrong choice
  // can only be undone by rebuilding the deployment.
  it("makes the operator name the invocation interface when the provider enables several", async () => {
    const detect = vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(async (_id, body) => ({
      id: "picked", status: "queued", source: "verified_probe", provider_id: provider.id, provider_model: unknown.target_id,
      binding_id: "b-embed", profile_id: "openai.embeddings.v1", provider_calls: 0, max_provider_calls: 8,
      capabilities: {}, recommended_capabilities: emptyCapabilities,
      selection_revision: (body as { selection_revision: string }).selection_revision, revision: 1,
    }));
    await openCreate();
    await choose("GPT Future");
    const picker = await screen.findByLabelText(/^调用接口/);
    expect((picker as HTMLSelectElement).value).toBe("");
    expect(within(picker as HTMLSelectElement).getAllByRole("option").map((option) => (option as HTMLOptionElement).value))
      .toEqual(["", "b-chat", "b-embed"]);
    expect(screen.getByRole("button", { name: "识别能力" })).toBeDisabled();

    fireEvent.change(picker, { target: { value: "b-embed" } });
    fireEvent.click(screen.getByRole("button", { name: "识别能力" }));
    await waitFor(() => expect(detect).toHaveBeenCalledOnce());
    expect(detect).toHaveBeenCalledWith(provider.id, expect.objectContaining({ binding_id: "b-embed" }), expect.any(String));
  });

  it("does not ask which interface to use when the provider enables exactly one", async () => {
    vi.mocked(api.providers).mockResolvedValue({ items: [{ ...provider, bindings: [provider.bindings![0]] }], next_cursor: "" });
    await openCreate();
    await choose("GPT Future");
    expect(await screen.findByRole("button", { name: "识别能力" })).toBeEnabled();
    expect(screen.queryByLabelText(/^调用接口/)).not.toBeInTheDocument();
  });

  it("fails closed with retry and provider exits when target resolution is unavailable", async () => {
    vi.mocked(api.resolveInvocationTarget).mockRejectedValue(new ApiError(503, "unavailable", "catalog_unavailable"));
    await openCreate();
    fireEvent.change(screen.getByLabelText(/^模型 ID/), { target: { value: "unlisted-model" } });
    expect(await screen.findByText("暂时无法解析模型能力")).toBeVisible();
    expect(screen.getByRole("link", { name: "前往服务商配置 →" })).toHaveAttribute("href", "/admin/providers");
    expect(screen.queryByText(/能力来自内置模型目录/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "手动配置" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "重新解析" }));
    await waitFor(() => expect(api.resolveInvocationTarget).toHaveBeenCalledTimes(2));
  });

  it("runs verification only after explicit confirmation", async () => {
    const detected = {
      id: "mcd", status: "completed" as const, source: "verified_probe" as const,
      provider_id: provider.id, provider_model: unknown.target_id, binding_id: "b-chat", profile_id: "openai.chat-embeddings.v1",
      provider_calls: 2, max_provider_calls: 8, capabilities: { chat: { status: "supported" as const, evidence: "verified" as const, probe_kind: "chat" } },
      recommended_capabilities: { ...emptyCapabilities, chat: true }, selection_revision: "selection", revision: 3,
    };
    const detect = vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(async (_id, body) => ({ ...detected, selection_revision: (body as { selection_revision: string }).selection_revision }));
    await openCreate();
    await choose("GPT Future");
    expect(detect).not.toHaveBeenCalled();
    await pickDetectionInterface();
    fireEvent.click(screen.getByRole("button", { name: "识别能力" }));
    await waitFor(() => expect(detect).toHaveBeenCalledOnce());
    expect(detect).toHaveBeenCalledWith(provider.id, expect.objectContaining({ binding_id: "b-chat" }), expect.any(String));
    expect(await screen.findByText("已识别 1 项能力")).toBeVisible();
    const details = screen.getByText("已识别 1 项能力").closest("details")!;
    expect(within(details).getByText("识别详情")).toBeVisible();
    expect(screen.getByLabelText("对话")).toBeChecked();
  });

  it("adopts a reused detection even when the shared job carries an older client selection token", async () => {
    const queued = {
      id: "cached", status: "queued" as const, source: "verified_probe" as const,
      provider_id: provider.id, provider_model: unknown.target_id, binding_id: "b-chat", profile_id: "openai.chat-embeddings.v1",
      provider_calls: 0, max_provider_calls: 8, capabilities: {}, recommended_capabilities: emptyCapabilities,
      selection_revision: "selection-from-another-client", revision: 1,
    };
    vi.spyOn(api, "createModelCapabilityDetection").mockResolvedValue(queued);
    vi.spyOn(api, "modelCapabilityDetection").mockResolvedValue({
      ...queued,
      status: "completed",
      provider_calls: 1,
      capabilities: { chat: { status: "supported", evidence: "verified", probe_kind: "chat" } },
      recommended_capabilities: { ...emptyCapabilities, chat: true },
      revision: 2,
    });
    await openCreate();
    fireEvent.change(screen.getByLabelText("部署名称"), { target: { value: "Cached detection" } });
    await choose("GPT Future");
    await pickDetectionInterface();
    fireEvent.click(screen.getByRole("button", { name: "识别能力" }));
    expect(await screen.findByText("已识别 1 项能力")).toBeVisible();
    expect(screen.getByRole("button", { name: "保存为停用" })).toBeEnabled();
  });

  it("does not apply a late verification response after the target selection changed", async () => {
    let resolveDetection!: (value: never) => void;
    vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(() => new Promise((resolve) => { resolveDetection = resolve; }));
    await openCreate();
    await choose("GPT Future");
    await pickDetectionInterface();
    fireEvent.click(screen.getByRole("button", { name: "识别能力" }));
    await waitFor(() => expect(api.createModelCapabilityDetection).toHaveBeenCalledOnce());
    const oldSelection = (vi.mocked(api.createModelCapabilityDetection).mock.calls[0][1] as { selection_revision: string }).selection_revision;
    await choose("GPT Chat");
    await act(async () => resolveDetection({
      id: "late", status: "completed", source: "verified_probe", provider_id: provider.id, provider_model: "gpt-future",
      binding_id: "b-chat", profile_id: "openai.chat-embeddings.v1", provider_calls: 1, max_provider_calls: 8,
      capabilities: { embeddings: { status: "supported", probe_kind: "embed" } },
      recommended_capabilities: { ...emptyCapabilities, embeddings: true }, selection_revision: oldSelection, revision: 1,
    } as never));
    const summary = (await screen.findByText("4 项能力可用")).closest("div.deployment-capability-summary") as HTMLElement;
    expect(within(summary).getByText("对话")).toBeVisible();
    expect(within(summary).queryByText("向量嵌入")).not.toBeInTheDocument();
  });

  it("ends an inconclusive verification at advanced onboarding without an immediate retry CTA", async () => {
    vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(async (_id, body) => ({
      id: "inconclusive", status: "completed", source: "verified_probe", provider_id: provider.id, provider_model: unknown.target_id,
      binding_id: "b-chat", profile_id: "openai.chat-embeddings.v1", provider_calls: 1, max_provider_calls: 8,
      capabilities: { chat: { status: "inconclusive", probe_kind: "chat" } }, recommended_capabilities: emptyCapabilities,
      selection_revision: (body as { selection_revision: string }).selection_revision, revision: 2,
    }));
    await openCreate();
    await choose("GPT Future");
    await pickDetectionInterface();
    fireEvent.click(screen.getByRole("button", { name: "识别能力" }));
    expect(await screen.findByText("仍无法确认模型能力")).toBeVisible();
    expect(screen.queryByRole("button", { name: "重新检测" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "手动配置" })).toBeVisible();
  });

  it.each(["unauthorized", "unavailable"] as const)("routes a %s verification result only to provider repair", async (status) => {
    vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(async (_id, body) => ({
      id: `repair-${status}`, status: "completed", source: "verified_probe", provider_id: provider.id, provider_model: unknown.target_id,
      binding_id: "b-chat", profile_id: "openai.chat-embeddings.v1", provider_calls: 1, max_provider_calls: 8,
      capabilities: { chat: { status, probe_kind: "chat" } }, recommended_capabilities: emptyCapabilities,
      selection_revision: (body as { selection_revision: string }).selection_revision, revision: 2,
    }));
    await openCreate();
    await choose("GPT Future");
    await pickDetectionInterface();
    fireEvent.click(screen.getByRole("button", { name: "识别能力" }));
    expect(await screen.findByText("服务商暂时无法完成验证")).toBeVisible();
    expect(screen.getByRole("link", { name: "前往服务商配置 →" })).toHaveAttribute("href", "/admin/providers");
    expect(screen.queryByRole("button", { name: "手动配置" })).not.toBeInTheDocument();
  });

  it("makes advanced onboarding choose a real interface and only narrow its ceiling", async () => {
    const create = vi.spyOn(api, "createDeployment").mockResolvedValue({} as never);
    await openCreate();
    fireEvent.change(screen.getByLabelText("部署名称"), { target: { value: "Manual" } });
    await choose("GPT Future");
    fireEvent.click(screen.getByRole("button", { name: "手动配置" }));
    const interfaceDetails = screen.getByText("能力接口").closest("details")!;
    fireEvent.click(within(interfaceDetails).getByText("能力接口"));
    const select = within(interfaceDetails).getByLabelText(/^能力接口/);
    fireEvent.change(select, { target: { value: "b-chat" } });
    expect(screen.getByLabelText("对话")).toBeChecked();
    fireEvent.click(screen.getByLabelText("工具调用"));
    fireEvent.click(screen.getByRole("button", { name: "保存为停用" }));
    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    expect(create).toHaveBeenCalledWith(expect.objectContaining({
      mode: "operator_declared", binding_id: "b-chat", capabilities: expect.objectContaining({ chat: true, tools: false }),
    }));
  });

  it("refreshes the invocation target catalog through the adapter capability endpoint", async () => {
    await openCreate();
    const refresh = await screen.findByRole("button", { name: "刷新" });
    expect(refresh).toHaveClass("secondary");
    expect(refresh.closest(".deployment-model-input-row")).toContainElement(screen.getByLabelText(/^模型 ID/));
    fireEvent.focus(screen.getByLabelText(/^模型 ID/));
    const listbox = await screen.findByRole("listbox", { name: "可用模型" });
    expect(listbox.querySelector(".deployment-model-options-meta")).toHaveTextContent("可用 2 个模型");
    fireEvent.click(refresh);
    await waitFor(() => expect(api.invocationTargets).toHaveBeenCalledWith(provider.id, "", true));
  });

  it("keeps the model refresh control visible while the initial catalog is loading", async () => {
    let resolveCatalog!: (value: InvocationTargetCatalog) => void;
    vi.mocked(api.invocationTargets).mockImplementation(() => new Promise((resolve) => { resolveCatalog = resolve; }));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));

    const loadingRefresh = await screen.findByRole("button", { name: "刷新中…" });
    expect(loadingRefresh).toBeDisabled();
    expect(loadingRefresh).toHaveAttribute("aria-busy", "true");
    expect(loadingRefresh.querySelector(".deployment-model-refresh-spinner")).toBeInTheDocument();
    expect(loadingRefresh.closest(".deployment-model-input-row")).toContainElement(screen.getByLabelText(/^模型 ID/));

    await act(async () => resolveCatalog(catalog([chatTarget, unknown])));
    const refresh = await screen.findByRole("button", { name: "刷新" });
    expect(refresh).toBeEnabled();
    expect(refresh).toHaveAttribute("aria-busy", "false");
  });

  // The combobox affordance and the refresh button used to be driven by two
  // different conditions, so a provider that cannot enumerate got a dropdown
  // arrow with no dropdown behind it.
  it("shows no catalog affordance at all for a provider that cannot enumerate", async () => {
    vi.mocked(api.invocationTargets).mockResolvedValue(catalog([], { discovery: {
      target_kinds: ["model_id"], can_enumerate: false, can_describe: true, can_verify: true,
      requires_management_identity: false, requires_canonical_model_mapping: false,
    } }));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));
    const input = await screen.findByLabelText(/^模型 ID/);
    await waitFor(() => expect(screen.queryByRole("button", { name: "刷新" })).not.toBeInTheDocument());
    expect(input).not.toHaveAttribute("role", "combobox");
    fireEvent.focus(input);
    expect(screen.queryByRole("listbox", { name: "可用模型" })).not.toBeInTheDocument();
  });

  it("uses the active locale's capability labels", async () => {
    await i18n.changeLanguage("en-US");
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ New deployment" }));
    await screen.findByRole("button", { name: "Refresh" });
    const input = screen.getByLabelText(/^Model ID/);
    fireEvent.focus(input);
    fireEvent.click(within(await screen.findByRole("listbox", { name: "Available models" })).getByRole("option", { name: "GPT Chat" }));
    const summary = (await screen.findByText("4 capabilities ready")).closest("div.deployment-capability-summary") as HTMLElement;
    expect(within(summary).getByText("Chat")).toBeVisible();
    expect(within(summary).getByText("Streaming")).toBeVisible();
    expect(within(summary).getByText("Halro's reviewed model catalog")).toBeVisible();
  });

});

describe("deployment price lifecycle refresh", () => {
  it("refreshes immediately after the nearest scheduled price becomes effective", () => {
    const now = Date.parse("2026-08-10T06:00:00Z");
    expect(scheduledPriceRefreshInterval([{
      status: "scheduled",
      effective_from: "2026-08-10T06:01:00Z",
    } as DeploymentPriceVersion], now)).toBe(60_250);
    expect(scheduledPriceRefreshInterval([{
      status: "active",
      effective_from: "2026-08-10T05:59:00Z",
    } as DeploymentPriceVersion], now)).toBe(false);
  });

  // A browser clock a few seconds ahead of the server, or a tab that just woke
  // up, made every scheduled entry look already-past. Returning false there
  // stopped polling for good and froze the row on "scheduled".
  it("keeps a short retry while the server still calls a past-due version scheduled", () => {
    const now = Date.parse("2026-08-10T06:00:00Z");
    expect(scheduledPriceRefreshInterval([{
      status: "scheduled",
      effective_from: "2026-08-10T05:59:59Z",
    } as DeploymentPriceVersion], now)).toBe(OVERDUE_SCHEDULED_PRICE_REFRESH_MS);
    expect(scheduledPriceRefreshInterval([
      { status: "scheduled", effective_from: "2026-08-10T05:59:59Z" } as DeploymentPriceVersion,
      { status: "scheduled", effective_from: "2026-08-10T06:00:30Z" } as DeploymentPriceVersion,
    ], now)).toBe(30_250);
    expect(scheduledPriceRefreshInterval([{
      status: "scheduled",
      effective_from: "not a timestamp",
    } as DeploymentPriceVersion], now)).toBe(false);
  });

  // The interval above is only worth anything if the row actually polls on it.
  // Deleting the refetchInterval line left the unit test above green.
  it("polls the deployment price endpoint on that interval", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      vi.spyOn(api, "providers").mockResolvedValue({ items: [provider], next_cursor: "" });
      vi.spyOn(api, "deployments").mockResolvedValue({ items: [existingDeployment], next_cursor: "" });
      vi.spyOn(api, "routes").mockResolvedValue({ items: [], next_cursor: "" });
      const read = vi.spyOn(api, "deploymentPrices").mockResolvedValue({
        items: [{ status: "scheduled", effective_from: "2000-01-01T00:00:00Z" } as DeploymentPriceVersion],
        next_cursor: "",
      });
      renderPage();
      await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(1));
      await act(async () => { await vi.advanceTimersByTimeAsync(OVERDUE_SCHEDULED_PRICE_REFRESH_MS + 500); });
      expect(read.mock.calls.length).toBeGreaterThan(1);
    } finally {
      vi.useRealTimers();
    }
  });

  // Fifty rows used to mean fifty simultaneous price reads on page load, each
  // one running a lifecycle derivation server-side.
  it("loads collapsed-row prices in bounded batches", async () => {
    vi.useFakeTimers({ shouldAdvanceTime: true });
    try {
      const many = Array.from({ length: PRICE_FETCH_BATCH_SIZE * 3 }, (_, index) => deployment(`dep_${index}`));
      vi.spyOn(api, "providers").mockResolvedValue({ items: [provider], next_cursor: "" });
      vi.spyOn(api, "deployments").mockResolvedValue({ items: many, next_cursor: "" });
      vi.spyOn(api, "routes").mockResolvedValue({ items: [], next_cursor: "" });
      const read = vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [], next_cursor: "" });
      renderPage();
      await vi.waitFor(() => expect(read).toHaveBeenCalled());
      expect(read).toHaveBeenCalledTimes(PRICE_FETCH_BATCH_SIZE);
      await act(async () => { await vi.advanceTimersByTimeAsync(priceFetchDelay(PRICE_FETCH_BATCH_SIZE * 3)); });
      await vi.waitFor(() => expect(read).toHaveBeenCalledTimes(many.length));
    } finally {
      vi.useRealTimers();
    }
  });
});

describe("deployment price panel", () => {
  beforeEach(() => {
    vi.spyOn(api, "providers").mockResolvedValue({ items: [provider], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [existingDeployment], next_cursor: "" });
    vi.spyOn(api, "routes").mockResolvedValue({ items: [], next_cursor: "" });
  });

  afterEach(() => vi.restoreAllMocks());

  // A failed price read used to render four static characters: no way back, and
  // nothing an assistive reader was told about.
  it("announces a failed price read and offers a retry", async () => {
    const read = vi.spyOn(api, "deploymentPrices")
      .mockRejectedValueOnce(new ApiError(503, "unavailable", "store_unavailable"))
      .mockResolvedValue({ items: [], next_cursor: "" });
    renderPage();
    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText("不可用")).toBeVisible();
    fireEvent.click(within(alert).getByRole("button", { name: "重试" }));
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryByRole("alert")).not.toBeInTheDocument());
  });

  it("gates every price write in the detail panel on the write role", async () => {
    vi.mocked(api.deployments).mockResolvedValue({
      items: [deployment("dep_1", { pricing_quarantined: true, pricing_quarantine_reason: "restored" })],
      next_cursor: "",
    });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({
      items: [
        activePriceVersion(),
        { ...activePriceVersion(), id: "price_2", version: 4, status: "scheduled", effective_from: "2099-01-01T00:00:00Z" },
      ],
      next_cursor: "",
    });
    renderPage("read_only");
    fireEvent.click(await screen.findByRole("button", { name: "查看详情" }));
    for (const name of ["调整价格", "确认恢复价格", "取消"]) {
      const control = await screen.findByRole("button", { name });
      expect(control).toBeDisabled();
      expect(control).toHaveAttribute("title", "只读账户无法执行此操作。");
    }
  });

  // Confirming an immediate price change is the last point at which it can be
  // stopped, so the version it replaces has to be on the same screen.
  it("shows the price being replaced and that an immediate version cannot be canceled", async () => {
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "查看详情" }));
    fireEvent.click(await screen.findByRole("button", { name: "调整价格" }));
    fireEvent.change(await screen.findByLabelText("输入 USD / 百万令牌"), { target: { value: "5" } });
    fireEvent.click(screen.getByRole("button", { name: "下一步：核对" }));

    expect(await screen.findByText("当前版本：v3")).toBeVisible();
    expect(screen.getByText("$1 → $5")).toBeVisible();
    // Output is unchanged, so it stays a plain amount rather than a fake diff.
    expect(screen.getByText("$2")).toBeVisible();
    expect(screen.getByText("立即生效的价格版本无法取消，只能由之后新建的版本覆盖。")).toBeVisible();
    expect(screen.getByText("立即生效（从现在起按此价计费）")).toBeVisible();
  });
});

function activePriceVersion(): DeploymentPriceVersion {
  return {
    id: "price_1", deployment_id: "dep_1", version: 3, revision: 1, billing_mode: "metered", currency: "USD",
    formula_version: "usd_tokens_v1", input_micros_per_million: 1_000_000, output_micros_per_million: 2_000_000,
    fixed_request_micros_usd: 0, effective_from: "2026-08-01T00:00:00Z",
    source: { type: "manual", assurance: "asserted", reference: "temporary_estimate" }, status: "active",
  };
}

async function openCreate() {
  renderPage();
  fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));
  await screen.findByRole("button", { name: "刷新" });
}

async function pickDetectionInterface(value = "b-chat") {
  fireEvent.change(await screen.findByLabelText(/^调用接口/), { target: { value } });
}

async function choose(name: string) {
  const input = screen.getByLabelText(/^模型 ID/);
  fireEvent.change(input, { target: { value: "" } });
  fireEvent.focus(input);
  const listbox = await screen.findByRole("listbox", { name: "可用模型" });
  fireEvent.click(within(listbox).getByRole("option", { name }));
}

function renderPage(role: AdminRole = "administrator") {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  queryClient.setQueryData(["session"], {
    username: "admin", role, locale: "system", appearance: "dark", csrf_token: "csrf",
    absolute_expires_at: "2026-08-08T00:00:00Z", idle_expires_at: "2026-08-07T01:00:00Z",
  } satisfies Session);
  return render(<QueryClientProvider client={queryClient}><DeploymentsPage /></QueryClientProvider>);
}
