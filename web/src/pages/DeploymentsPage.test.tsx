import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api";
import i18n from "../i18n";
import type { DeploymentVariant, InvocationTargetCatalog, Provider, ProviderCapabilities, ResolvedInvocationTarget } from "../types";
import { DeploymentsPage } from "./DeploymentsPage";

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
    expect(screen.getByText("先选择模型")).toBeVisible();
  });

  it("treats legacy null collection fields as empty instead of crashing", async () => {
    vi.mocked(api.invocationTargets).mockResolvedValue({ ...catalog([]), items: null as never });
    await openCreate();
    expect(screen.getByRole("dialog", { name: "创建模型部署" })).toBeVisible();
    expect(screen.getByText("先选择模型")).toBeVisible();
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
    expect(await screen.findByText("已识别")).toBeVisible();
    expect(screen.getByText(/可用于：对话/)).toBeVisible();
    expect(screen.getByText("可信证据：Halro 已评审模型目录")).toBeVisible();
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
    expect(screen.queryByRole("button", { name: "确认模型并识别能力" })).not.toBeInTheDocument();
  });

  it("offers only verification and advanced onboarding for an unknown target", async () => {
    await openCreate();
    await choose("GPT Future");
    expect(await screen.findByRole("button", { name: "确认模型并识别能力" })).toBeDisabled();
    expect(screen.getByLabelText(/^能力接口/)).toHaveValue("");
    expect(screen.getByText("该服务商启用了多个接口。请选择将实际调用并接受低成本验证请求的接口。")).toBeVisible();
    expect(screen.getByRole("button", { name: "高级接入" })).toBeVisible();
    expect(screen.queryByLabelText("对话")).not.toBeInTheDocument();
  });

  it("fails closed with retry and provider exits when target resolution is unavailable", async () => {
    vi.mocked(api.resolveInvocationTarget).mockRejectedValue(new ApiError(503, "unavailable", "catalog_unavailable"));
    await openCreate();
    fireEvent.change(screen.getByLabelText(/^模型 ID/), { target: { value: "unlisted-model" } });
    expect(await screen.findByText("暂时无法解析模型能力")).toBeVisible();
    expect(screen.getByRole("link", { name: "前往服务商配置 →" })).toHaveAttribute("href", "/admin/providers");
    expect(screen.queryByText(/能力来自内置模型目录/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "高级接入" })).not.toBeInTheDocument();
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
    chooseDetectionInterface();
    fireEvent.click(screen.getByRole("button", { name: "确认模型并识别能力" }));
    await waitFor(() => expect(detect).toHaveBeenCalledOnce());
    expect(detect).toHaveBeenCalledWith(provider.id, expect.objectContaining({ binding_id: "b-chat" }), expect.any(String));
    expect(await screen.findByText("已识别 1 项能力")).toBeVisible();
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
    chooseDetectionInterface();
    fireEvent.click(screen.getByRole("button", { name: "确认模型并识别能力" }));
    expect(await screen.findByText("已识别 1 项能力")).toBeVisible();
    expect(screen.getByRole("button", { name: "保存为停用" })).toBeEnabled();
  });

  it("does not apply a late verification response after the target selection changed", async () => {
    let resolveDetection!: (value: never) => void;
    vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(() => new Promise((resolve) => { resolveDetection = resolve; }));
    await openCreate();
    await choose("GPT Future");
    chooseDetectionInterface();
    fireEvent.click(screen.getByRole("button", { name: "确认模型并识别能力" }));
    await waitFor(() => expect(api.createModelCapabilityDetection).toHaveBeenCalledOnce());
    const oldSelection = (vi.mocked(api.createModelCapabilityDetection).mock.calls[0][1] as { selection_revision: string }).selection_revision;
    await choose("GPT Chat");
    await act(async () => resolveDetection({
      id: "late", status: "completed", source: "verified_probe", provider_id: provider.id, provider_model: "gpt-future",
      binding_id: "b-chat", profile_id: "openai.chat-embeddings.v1", provider_calls: 1, max_provider_calls: 8,
      capabilities: { embeddings: { status: "supported", probe_kind: "embed" } },
      recommended_capabilities: { ...emptyCapabilities, embeddings: true }, selection_revision: oldSelection, revision: 1,
    } as never));
    expect(await screen.findByText(/可用于：对话/)).toBeVisible();
    expect(screen.queryByText(/可用于：向量嵌入/)).not.toBeInTheDocument();
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
    chooseDetectionInterface();
    fireEvent.click(screen.getByRole("button", { name: "确认模型并识别能力" }));
    expect(await screen.findByText("仍无法确认模型能力")).toBeVisible();
    expect(screen.queryByRole("button", { name: "重新检测" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "高级接入" })).toBeVisible();
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
    chooseDetectionInterface();
    fireEvent.click(screen.getByRole("button", { name: "确认模型并识别能力" }));
    expect(await screen.findByText("服务商暂时无法完成验证")).toBeVisible();
    expect(screen.getByRole("link", { name: "前往服务商配置 →" })).toHaveAttribute("href", "/admin/providers");
    expect(screen.queryByRole("button", { name: "高级接入" })).not.toBeInTheDocument();
  });

  it("makes advanced onboarding choose a real interface and only narrow its ceiling", async () => {
    const create = vi.spyOn(api, "createDeployment").mockResolvedValue({} as never);
    await openCreate();
    fireEvent.change(screen.getByLabelText("部署名称"), { target: { value: "Manual" } });
    await choose("GPT Future");
    fireEvent.click(screen.getByRole("button", { name: "高级接入" }));
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
    fireEvent.click(await screen.findByRole("button", { name: "刷新模型列表" }));
    await waitFor(() => expect(api.invocationTargets).toHaveBeenCalledWith(provider.id, "", true));
  });

  it("uses the active locale's capability-list separator", async () => {
    await i18n.changeLanguage("en-US");
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ New deployment" }));
    await screen.findByText(/Discovered/);
    const input = screen.getByLabelText(/^Model ID/);
    fireEvent.focus(input);
    fireEvent.click(within(await screen.findByRole("listbox", { name: "Available models" })).getByRole("option", { name: "GPT Chat" }));
    expect(await screen.findByText(/Available for: Chat, Streaming/)).toBeVisible();
    expect(screen.queryByText(/Chat、Streaming/)).not.toBeInTheDocument();
    expect(screen.getByText("Trusted evidence: Halro's reviewed model catalog")).toBeVisible();
  });

});

async function openCreate() {
  renderPage();
  fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));
  await screen.findByText(/已发现/);
}

async function choose(name: string) {
  const input = screen.getByLabelText(/^模型 ID/);
  fireEvent.change(input, { target: { value: "" } });
  fireEvent.focus(input);
  const listbox = await screen.findByRole("listbox", { name: "可用模型" });
  fireEvent.click(within(listbox).getByRole("option", { name }));
}

function chooseDetectionInterface(bindingID = "b-chat") {
  fireEvent.change(screen.getByLabelText(/^能力接口/), { target: { value: bindingID } });
}

function renderPage() {
  const queryClient = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={queryClient}><DeploymentsPage /></QueryClientProvider>);
}
