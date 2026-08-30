import { providerProfilesFixture } from "../test/fixtures";
import { deploymentCapabilityGroupsForTest } from "./DeploymentsPage";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api";
import i18n from "../i18n";
import { NotificationProvider } from "../notifications";
import type { AdminRole, Deployment, DeploymentPriceVersion, DeploymentVariant, InvocationTargetCatalog, Provider, ProviderCapabilities, ResolvedInvocationTarget, Session } from "../types";
import { DeploymentsPage, OVERDUE_SCHEDULED_PRICE_REFRESH_MS, PRICE_FETCH_BATCH_SIZE, clockToMinute, minuteToClock, priceFetchDelay, scheduleDraftProblem, scheduledPriceRefreshInterval } from "./DeploymentsPage";
import { DEFAULT_TIME_ZONE, isoToZonedInput } from "../timezone";

const emptyCapabilities: ProviderCapabilities = {
  chat: false, streaming: false, embeddings: false, moderations: false, images: false,
  transcriptions: false, speech: false, files: false, batches: false, rerank: false,
  async_generate: false, tools: false, vision: false, fetched_image: false, json_object: false, structured_outputs: false,
  developer_role: false, reasoning: false, stream_usage: false, provider_executed_tools: false,
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

const PRICE_BLOCKER = "该部署没有已生效的价格版本。设置价格后再启用；如模型免费，请明确选择“免费”。";

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
      requires_management_identity: false, requires_canonical_model_mapping: false, max_verification_calls: 10,
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
    vi.spyOn(api, "refreshInvocationTargets").mockResolvedValue(catalog([chatTarget, unknown]));
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
    expect(screen.getByLabelText(/^最大输出词元/)).toBeVisible();
    expect(screen.queryByText("默认")).not.toBeInTheDocument();
  });

  // "0 is automatic" dropped who enforces the limit once the deployment stops
  // declaring one, which is a billing and throttling fact, not a nicety.
  it("says what a zero token limit actually means on every limit field", async () => {
    await openCreate();
    expect(screen.getByLabelText(/^最大上下文词元/)).toHaveAccessibleDescription("0 表示不在部署层声明，运行时仍受上游限制");
    expect(screen.getByLabelText(/^最大输出词元/)).toHaveAccessibleDescription("0 表示不在部署层声明，运行时仍受上游限制；填写后不得超过最大上下文词元");
    expect(screen.getByLabelText(/^并发上限/)).toHaveAccessibleDescription("0 表示不在部署层限制并发，仅受服务商自身限制");
    expect(screen.queryByText("0 为自动")).not.toBeInTheDocument();
  });

  it("names every unmet condition next to a disabled save button", async () => {
    await openCreate();
    expect(screen.getByRole("button", { name: "保存为停用" })).toBeDisabled();
    // The conditions ride in the action bar beside the button they disable.
    const blockers = screen.getByText("尚未完成").closest(".form-footer-summary")!;
    expect(blockers.closest(".deployment-form-actions")).toContainElement(screen.getByRole("button", { name: "保存为停用" }));
    expect(blockers.textContent).toContain("填写部署名称");
    expect(blockers.textContent).toContain("选择或输入模型");
    // Nothing downstream of the model is answerable yet, so it is not listed.
    expect(blockers.textContent).not.toContain("至少保留一项核心能力");

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
    fireEvent.click((await screen.findAllByRole("button", { name: /^编辑/ }))[0]);
    expect(await screen.findByText("保存后立即热加载")).toBeVisible();
    expect(screen.getByText(/保存会立即热加载这个部署的配置/)).toBeVisible();
  });

  // An edit locks the invocation identity: detection cannot run and no variant
  // is re-resolved, so nothing but the operator can establish a capability the
  // deployment never recorded. The form used to send the widened set with no
  // mode at all, and the only possible outcome was model_capabilities_unknown
  // A capability the interface could serve and the connection has not enabled
  // used to be left out of the form entirely — so a capability added to a
  // profile after the connection was made had nowhere to be turned on. The
  // Gateway names it in the refusal, the operator opens this form, and there is
  // no box. It is drawn as unavailable now, with the step that unblocks it.
  it("offers a capability the connection has not enabled, and says where to enable it", async () => {
    // The connection carries vision but not the fetch, which is the state
    // migration 31 leaves every existing connection in.
    const seeing: ProviderCapabilities = { ...chatCapabilities, vision: true, fetched_image: false };
    vi.mocked(api.providers).mockResolvedValue({
      items: [{
        ...provider, capabilities: seeing,
        bindings: [{ id: "b-chat", profile_id: "openai.chat-embeddings.v1", enabled: true, capabilities: seeing }],
      } as Provider],
      next_cursor: "",
    });
    vi.mocked(api.deployments).mockResolvedValue({ items: [existingDeployment], next_cursor: "" });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [], next_cursor: "" });
    renderPage();

    fireEvent.click((await screen.findAllByRole("button", { name: /^编辑/ }))[0]);
    const box = await screen.findByLabelText(/远程图片抓取/);
    expect(box).toBeDisabled();
    // The reason travels with the box rather than sitting somewhere on the page:
    // several capabilities can be in this state at once, and an operator reading
    // one row must not have to guess which note belongs to it.
    expect(box.closest("label")).toHaveTextContent("先在服务商连接上启用");
    // The group counter reads as "how many of the ones you can turn on", so a box
    // belonging to another screen must not sit in its denominator.
    const modality = screen.getByLabelText(/远程图片抓取/).closest("section");
    expect(modality).toHaveTextContent("0 / 1 项启用");
    expect(modality).toHaveTextContent("1 项需先在连接上启用");
  });

  // Ticking a box past what the catalogue recorded is the claim, and the save
  // carries it under a button that names what it commits.
  it("sends an edit that widens capabilities as an operator declaration", async () => {
    const resourceCapabilities: ProviderCapabilities = { ...chatCapabilities, files: true, batches: true };
    vi.mocked(api.providers).mockResolvedValue({
      items: [{
        ...provider, capabilities: resourceCapabilities,
        bindings: [{ id: "b-chat", profile_id: "openai.chat-embeddings.v1", enabled: true, capabilities: resourceCapabilities }],
      } as Provider],
      next_cursor: "",
    });
    vi.mocked(api.deployments).mockResolvedValue({ items: [existingDeployment], next_cursor: "" });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [], next_cursor: "" });
    const update = vi.spyOn(api, "updateDeployment").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click((await screen.findAllByRole("button", { name: /^编辑/ }))[0]);
    fireEvent.click(await screen.findByLabelText("文件"));
    expect(screen.getByText("新增能力需要管理员声明")).toBeVisible();
    expect(screen.queryByText("尚未完成")).not.toBeInTheDocument();
    // No second act to perform: the tick is the claim, and the commit says so.
    expect(screen.queryByRole("button", { name: "保存并热加载" })).not.toBeInTheDocument();
    // A widening does not hot-reload into service; the bar states what it does.
    expect(screen.getByText("开启能力需要重新验证")).toBeVisible();
    expect(screen.getByText("保存后将处于停用状态。重新测试并启用后，它才能提供这项能力。")).toBeVisible();
    expect(screen.queryByText("保存后立即热加载")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "声明并保存" }));
    await waitFor(() => expect(update).toHaveBeenCalledOnce());
    expect(update).toHaveBeenCalledWith(existingDeployment.id, expect.objectContaining({
      mode: "operator_declared", capabilities: expect.objectContaining({ files: true }),
    }), expect.anything());
  });

  // The server refuses a widening while the deployment is still routed
  // (capability_expansion_requires_revalidation), and disabling the routes is
  // the only way past it. That instruction has no other home on screen, so it
  // has to be on the bar that would otherwise 409.
  it("tells a routed deployment to leave routing before a widening can save", async () => {
    const resourceCapabilities: ProviderCapabilities = { ...chatCapabilities, files: true, batches: true };
    vi.mocked(api.providers).mockResolvedValue({
      items: [{
        ...provider, capabilities: resourceCapabilities,
        bindings: [{ id: "b-chat", profile_id: "openai.chat-embeddings.v1", enabled: true, capabilities: resourceCapabilities }],
      } as Provider],
      next_cursor: "",
    });
    vi.mocked(api.deployments).mockResolvedValue({ items: [deployment("dep_1", { enabled: true })], next_cursor: "" });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [], next_cursor: "" });
    renderPage();

    fireEvent.click((await screen.findAllByRole("button", { name: /^编辑/ }))[0]);
    fireEvent.click(await screen.findByLabelText("文件"));
    expect(screen.getByText("开启能力需要重新验证")).toBeVisible();
    expect(screen.getByText(/请先停用这些路由/)).toBeVisible();
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
    }), expect.any(String));
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

  // The catalogue is a review of what a model was, and an account can behave
  // otherwise. Verifying is the operator paying to find out where the two
  // disagree — which is only worth anything if the run says what it measured and
  // what it merely carried forward.
  it("verifies a catalogued model against the provider and keeps what it never probed", async () => {
    const catalogued = { ...chatCapabilities, vision: true };
    vi.mocked(api.invocationTargets).mockResolvedValue(catalog([target("gpt-chat", [variant("gpt-chat", "b-chat", catalogued)], "resolved", "GPT Chat")]));
    const create = vi.spyOn(api, "createDeployment").mockResolvedValue({} as never);
    const detect = vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(async (_id, body) => ({
      id: "verified", status: "completed" as const, source: "verified_probe" as const,
      provider_id: provider.id, provider_model: "gpt-chat", binding_candidates: [], binding_id: "b-chat",
      provider_calls: 4, max_provider_calls: 10,
      capabilities: {
        chat: { status: "supported" as const, evidence: "verified" as const, probe_kind: "minimal_chat" },
        streaming: { status: "supported" as const, evidence: "verified" as const, probe_kind: "minimal_stream" },
        stream_usage: { status: "supported" as const, evidence: "verified" as const, probe_kind: "stream_usage" },
        tools: { status: "unsupported" as const, probe_kind: "tool_call" },
        reasoning: { status: "supported" as const, evidence: "verified" as const, probe_kind: "reasoning_effort" },
        vision: { status: "not_probed" as const, probe_kind: "risk_policy" },
      },
      baseline_capabilities: catalogued,
      recommended_capabilities: { ...catalogued, tools: false, reasoning: true },
      expires_at: "2026-08-30T00:00:00Z",
      selection_revision: (body as { selection_revision: string }).selection_revision, revision: 2,
    }));
    await openCreate();
    fireEvent.change(screen.getByLabelText("部署名称"), { target: { value: "Chat" } });
    await choose("GPT Chat");
    expect(await screen.findByText("5 项能力可用")).toBeVisible();

    await startDetection("实测校验");
    await waitFor(() => expect(detect).toHaveBeenCalledOnce());
    // It declines the answers that already exist, and runs where the catalogue
    // already says the model runs rather than paying to identify an interface.
    expect(detect.mock.calls[0][1]).toMatchObject({ force_refresh: true, binding_id: "b-chat", provider_model: "gpt-chat" });

    expect(await screen.findByText("实测完成：已验证 5 项能力")).toBeVisible();
    expect(screen.getByText("实测新增：推理")).toBeVisible();
    expect(screen.getByText("上游拒绝，已关闭：工具调用")).toBeVisible();
    // The half that is easiest to misread: a capability no probe may ask about
    // is not a capability the run found missing.
    expect(screen.getByText("本次未实测，沿用目录声明：视觉")).toBeVisible();

    fireEvent.click(screen.getByRole("button", { name: "保存为停用" }));
    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    const payload = create.mock.calls[0][0] as Record<string, unknown>;
    // The write is pinned to the verification, not to the claims it just
    // checked: the server validates the capability set against whichever one the
    // request names, and reasoning exists in only one of them.
    expect(payload).toMatchObject({ capability_detection_id: "verified", capability_detection_revision: 2 });
    expect(payload).not.toHaveProperty("resolution_revision");
    expect(payload.capabilities).toMatchObject({ chat: true, reasoning: true, vision: true, tools: false });
  });

  // "This model does not support it" is the upstream's answer, not Halro's, and
  // an operator whose model card says otherwise needs to see which field it
  // named. The identifiers are recorded per probe; the refusal used to be the
  // one outcome that stated a conclusion without showing them.
  it("shows what the provider returned when it refused a capability", async () => {
    const catalogued = { ...chatCapabilities, vision: true };
    vi.mocked(api.invocationTargets).mockResolvedValue(catalog([target("gpt-chat", [variant("gpt-chat", "b-chat", catalogued)], "resolved", "GPT Chat")]));
    vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(async (_id, body) => ({
      id: "refused-vision", status: "completed" as const, source: "verified_probe" as const,
      provider_id: provider.id, provider_model: "gpt-chat", binding_candidates: [], binding_id: "b-chat",
      provider_calls: 2, max_provider_calls: 10,
      capabilities: {
        chat: { status: "supported" as const, evidence: "verified" as const, probe_kind: "minimal_chat" },
        vision: { status: "unsupported" as const, error_class: "bad_request", provider_status: 400, provider_code: "unsupported_value:messages", probe_kind: "inline_image" },
      },
      baseline_capabilities: catalogued,
      recommended_capabilities: { ...catalogued, vision: false },
      expires_at: "2026-08-30T00:00:00Z",
      selection_revision: (body as { selection_revision: string }).selection_revision, revision: 2,
    }));
    await openCreate();
    fireEvent.change(screen.getByLabelText("部署名称"), { target: { value: "Chat" } });
    await choose("GPT Chat");
    await startDetection("实测校验");

    expect(await screen.findByText("视觉：上游明确拒绝")).toBeVisible();
    // The identifier, not the sentence beside it: which request and which field.
    expect(screen.getByText("上游 400 · unsupported_value:messages")).toBeVisible();
  });

  // The console distinguishes a verification from an ordinary detection by the
  // baseline the record carries. Nothing else does: an ordinary detection of a
  // model the catalogue does not cover has findings too, and reporting them as
  // "established beyond the catalogue" describes a comparison that never
  // happened.
  it("shows no verification result for an ordinary detection", async () => {
    vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(async (_id, body) => ({
      id: "plain", status: "completed" as const, source: "verified_probe" as const,
      provider_id: provider.id, provider_model: unknown.target_id, binding_candidates: [], binding_id: "b-chat",
      provider_calls: 1, max_provider_calls: 10,
      capabilities: { chat: { status: "supported" as const, evidence: "verified" as const, probe_kind: "minimal_chat" } },
      recommended_capabilities: { ...emptyCapabilities, chat: true },
      expires_at: "2026-08-30T00:00:00Z",
      selection_revision: (body as { selection_revision: string }).selection_revision, revision: 2,
    }));
    await openCreate();
    await choose("GPT Future");
    await startDetection();

    expect(await screen.findByLabelText("对话")).toBeChecked();
    expect(screen.queryByText(/实测完成/)).not.toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "沿用目录声明" })).not.toBeInTheDocument();
  });

  // A verification that could not finish measured nothing, so the claims it was
  // checking are still the best answer available. Manual declaration is the
  // other model's escape — for a catalogued model it drops the operator into a
  // mode with no variant pin and no detection pin, and no way back.
  it("returns a failed verification to the catalogue's claims rather than to manual declaration", async () => {
    const catalogued = { ...chatCapabilities, vision: true };
    vi.mocked(api.invocationTargets).mockResolvedValue(catalog([target("gpt-chat", [variant("gpt-chat", "b-chat", catalogued)], "resolved", "GPT Chat")]));
    const create = vi.spyOn(api, "createDeployment").mockResolvedValue({} as never);
    vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(async (_id, body) => ({
      id: "refused", status: "failed" as const, source: "verified_probe" as const,
      provider_id: provider.id, provider_model: "gpt-chat", binding_candidates: [], binding_id: "b-chat",
      provider_calls: 1, max_provider_calls: 10,
      capabilities: { chat: { status: "unauthorized" as const, error_class: "authentication", probe_kind: "minimal_chat" } },
      baseline_capabilities: catalogued,
      recommended_capabilities: emptyCapabilities,
      selection_revision: (body as { selection_revision: string }).selection_revision, revision: 2,
    }));
    await openCreate();
    fireEvent.change(screen.getByLabelText("部署名称"), { target: { value: "Chat" } });
    await choose("GPT Chat");
    await startDetection("实测校验");

    expect(await screen.findByText("未能可靠识别能力")).toBeVisible();
    expect(screen.queryByRole("button", { name: "手动配置" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "沿用目录声明" }));

    expect(await screen.findByText("5 项能力可用")).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "保存为停用" }));
    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    const payload = create.mock.calls[0][0] as Record<string, unknown>;
    expect(payload).not.toHaveProperty("capability_detection_id");
    expect(payload).toMatchObject({ resolution_revision: "sha256:gpt-chat:b-chat" });
    expect(payload.capabilities).toMatchObject({ chat: true, vision: true });
  });

  it("offers only verification and advanced onboarding for an unknown target", async () => {
    await openCreate();
    await choose("GPT Future");
    expect(await screen.findByRole("button", { name: "识别能力" })).toBeEnabled();
    expect(screen.getByText("最多 10 次低成本验证。这些控制面调用不计入项目预算、Ledger 或用量统计。")).toBeVisible();
    expect(screen.getByRole("button", { name: "手动配置" })).toBeVisible();
    expect(screen.queryByLabelText("对话")).not.toBeInTheDocument();
  });

  // "Which interface does this model speak" is the question detection exists to
  // answer, so asking it first put the operator in front of the one thing they
  // cannot know — and got it wrong irreversibly, since the target is immutable
  // after creation.
  // The window is the server's to keep, so the console cannot know whether this
  // session still holds one. It attempts without credentials and reveals the
  // fields only when the answer says it must — which keeps the consequence
  // stated on every detection while asking for a password only when the proof
  // has actually lapsed.
  it("asks for a password only once the server says the elevation lapsed", async () => {
    const detected = {
      id: "picked", status: "queued" as const, source: "verified_probe" as const, provider_id: provider.id, provider_model: unknown.target_id,
      binding_candidates: [], provider_calls: 0, max_provider_calls: 10,
      capabilities: {}, recommended_capabilities: emptyCapabilities,
      selection_revision: "", revision: 1,
    };
    const detect = vi.spyOn(api, "createModelCapabilityDetection")
      .mockRejectedValueOnce(new ApiError(401, "recent re-authentication required", "recent_reauth_required"))
      .mockImplementation(async (_id, body) => ({
        ...detected, selection_revision: (body as { selection_revision: string }).selection_revision,
      }));
    await openCreate();
    await choose("GPT Future");

    fireEvent.click(screen.getByRole("button", { name: "识别能力" }));
    const dialog = await screen.findByRole("alertdialog");
    // First attempt: consequence stated, no credentials collected.
    expect(within(dialog).queryByLabelText(/^当前密码/)).toBeNull();
    fireEvent.click(within(dialog).getByRole("button", { name: "识别能力" }));

    // The refusal is not reported as a failure; it turns into the request for
    // credentials it actually is, in the dialog the operator already has open.
    const password = await within(dialog).findByLabelText(/^当前密码/);
    fireEvent.change(password, { target: { value: "a passphrase" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "识别能力" }));

    await waitFor(() => expect(detect).toHaveBeenCalledTimes(2));
    expect(detect.mock.calls[0][3]).toEqual({ currentPassword: "", totpCode: "" });
    expect(detect.mock.calls[1][3]).toMatchObject({ currentPassword: "a passphrase" });
  });

  it("never asks which interface to use before anything has been verified", async () => {
    const detect = vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(async (_id, body) => ({
      id: "picked", status: "queued", source: "verified_probe", provider_id: provider.id, provider_model: unknown.target_id,
      binding_candidates: [], provider_calls: 0, max_provider_calls: 10,
      capabilities: {}, recommended_capabilities: emptyCapabilities,
      selection_revision: (body as { selection_revision: string }).selection_revision, revision: 1,
    }));
    await openCreate();
    await choose("GPT Future");
    expect(screen.queryByLabelText(/^调用接口/)).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "识别能力" })).toBeEnabled();

    await startDetection();
    await waitFor(() => expect(detect).toHaveBeenCalledOnce());
    expect(detect.mock.calls[0][1]).not.toHaveProperty("binding_id");
  });

  // A model answering on several interfaces is a real choice — one deployment
  // runs on one interface — so it comes back to the operator, but only after
  // detection has established what each interface actually answered.
  it("returns the interface choice only once detection found several that answer", async () => {
    const ambiguous = {
      id: "amb", status: "ambiguous" as const, source: "verified_probe" as const, provider_id: provider.id,
      provider_model: unknown.target_id, provider_calls: 2, max_provider_calls: 10,
      binding_candidates: [
        { binding_id: "b-chat", profile_id: "openai.chat-embeddings.v1", access_surface: "openai-api", model_revision: "sha256:a", capability: "chat", probe_kind: "minimal_chat", status: "supported" as const, evidence: "verified" as const, answered: true },
        { binding_id: "b-embed", profile_id: "openai.embeddings.v1", access_surface: "openai-api", model_revision: "sha256:b", capability: "embeddings", probe_kind: "embedding", status: "supported" as const, evidence: "verified" as const, answered: true },
      ],
      capabilities: {}, recommended_capabilities: emptyCapabilities, revision: 1,
    };
    const detect = vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(async (_id, body) => ({
      ...ambiguous, selection_revision: (body as { selection_revision: string }).selection_revision,
    }));
    await openCreate();
    await choose("GPT Future");
    await startDetection();

    const picker = await screen.findByLabelText(/^调用接口/);
    expect(within(picker as HTMLSelectElement).getAllByRole("option").map((option) => (option as HTMLOptionElement).value))
      .toEqual(["", "b-chat", "b-embed"]);
    // The evidence rides along, so the choice is made against what each
    // interface actually answered rather than against the operator's guess.
    expect(within(picker as HTMLSelectElement).getAllByRole("option").map((option) => option.textContent)).toEqual([
      "选择调用接口",
      "对话 · 流式 · 工具调用 — openai.chat-embeddings.v1（已验证对话）",
      "向量嵌入 — openai.embeddings.v1（已验证向量嵌入）",
    ]);

    fireEvent.change(picker, { target: { value: "b-embed" } });
    await startDetection("用这个接口继续识别");
    await waitFor(() => expect(detect).toHaveBeenCalledTimes(2));
    expect(detect.mock.calls[1][1]).toMatchObject({ binding_id: "b-embed" });
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
      provider_id: provider.id, provider_model: unknown.target_id, binding_id: "b-chat", profile_id: "openai.chat-embeddings.v1", binding_candidates: [],
      provider_calls: 2, max_provider_calls: 8, capabilities: { chat: { status: "supported" as const, evidence: "verified" as const, probe_kind: "chat" } },
      recommended_capabilities: { ...emptyCapabilities, chat: true }, selection_revision: "selection", revision: 3,
    };
    const detect = vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(async (_id, body) => ({ ...detected, selection_revision: (body as { selection_revision: string }).selection_revision }));
    await openCreate();
    await choose("GPT Future");
    expect(detect).not.toHaveBeenCalled();
    await startDetection();
    await waitFor(() => expect(detect).toHaveBeenCalledOnce());
    // No interface is asserted on the way in; the detection result names it.
    expect(detect.mock.calls[0][0]).toBe(provider.id);
    expect(detect.mock.calls[0][1]).not.toHaveProperty("binding_id");
    // The result is the capability editor itself, carrying where it came from.
    // Capabilities the model does not have are not listed: that set is just the
    // complement of what is shown, and nobody acts on it.
    expect(await screen.findByLabelText("对话")).toBeChecked();
    expect(screen.getByText("受控能力验证")).toBeVisible();
    expect(screen.queryByText("识别详情")).not.toBeInTheDocument();
    expect(screen.queryByText("明确不支持")).not.toBeInTheDocument();
  });

  // Three greys with three different next steps. Going to the connection does
  // nothing for a capability the connection already carries, and re-running
  // identification does nothing for a probe that came back refused — one label
  // for all three sent the operator somewhere with nothing to do there.
  it("names which dead end each unavailable capability is in", async () => {
    const detected = {
      id: "mcd", status: "completed" as const, source: "verified_probe" as const,
      provider_id: provider.id, provider_model: unknown.target_id, binding_id: "b-chat",
      profile_id: "openai.chat-embeddings.v1", binding_candidates: [], provider_calls: 4, max_provider_calls: 8,
      capabilities: {
        chat: { status: "supported" as const, evidence: "verified" as const, probe_kind: "chat" },
        stream_usage: { status: "inconclusive" as const, probe_kind: "stream_usage" },
        tools: { status: "unsupported" as const, probe_kind: "tool_call" },
      },
      recommended_capabilities: { ...emptyCapabilities, chat: true }, selection_revision: "selection", revision: 3,
    };
    vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(async (_id, body) => ({ ...detected, selection_revision: (body as { selection_revision: string }).selection_revision }));
    await openCreate();
    await choose("GPT Future");
    await startDetection();
    expect(await screen.findByLabelText("对话")).toBeChecked();
    // Enabled on the connection, probed, and the run settled nothing.
    expect(screen.getByRole("checkbox", { name: /^流式用量/ }).closest("label")).toHaveTextContent("本次识别没有验证出来");
    // Enabled on the connection, probed, and refused upstream.
    expect(screen.getByRole("checkbox", { name: /^工具调用/ }).closest("label")).toHaveTextContent("本次识别判定不支持");
    // The interface serves it; this connection is what never turned it on.
    expect(screen.getByRole("checkbox", { name: /^视觉/ }).closest("label")).toHaveTextContent("先在服务商连接上启用");
  });

  // "Never reached" and "reached, settled nothing" are different answers with
  // different next steps: one is fixed by identifying again, the other cannot
  // be, because there is no probe to run. The server has always told them
  // apart; the form used to collapse them back together.
  it("does not offer a re-run for a capability the plan never reached", async () => {
    const detected = {
      id: "mcd", status: "completed" as const, source: "verified_probe" as const,
      provider_id: provider.id, provider_model: unknown.target_id, binding_id: "b-chat",
      profile_id: "openai.chat-embeddings.v1", binding_candidates: [], provider_calls: 3, max_provider_calls: 8,
      capabilities: {
        chat: { status: "supported" as const, evidence: "verified" as const, probe_kind: "chat" },
        stream_usage: { status: "inconclusive" as const, probe_kind: "stream_usage", error_class: "rate_limit" },
        // The plan ran out of calls before this one — not a risk_policy row, so
        // it is an outcome the operator is owed, but not one a retry fixes.
        tools: { status: "not_probed" as const, probe_kind: "tool_call" },
      },
      recommended_capabilities: { ...emptyCapabilities, chat: true }, selection_revision: "selection", revision: 3,
    };
    vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(async (_id, body) => ({ ...detected, selection_revision: (body as { selection_revision: string }).selection_revision }));
    await openCreate();
    await choose("GPT Future");
    await startDetection();
    expect(await screen.findByLabelText("对话")).toBeChecked();
    expect(screen.getByRole("checkbox", { name: /^工具调用/ }).closest("label")).toHaveTextContent("本次识别没有探测该能力");
    expect(screen.getByRole("checkbox", { name: /^流式用量/ }).closest("label")).toHaveTextContent("本次识别没有验证出来");
    // The reason a probe settled nothing is in the same response object, and
    // used to be rendered only for a detection that failed outright.
    const banner = screen.getByText(/验证没有得出结论/).closest(".notice")!;
    expect(banner).toHaveTextContent("流式用量 → 无法确认");
    expect(banner).toHaveTextContent("上游限流，稍后重试");
    // A capability with no recorded error class adds no row.
    expect(banner).not.toHaveTextContent("工具调用 → 未检测");
  });

  // Six outcomes, six next steps. One sentence for all of them sent the
  // operator to re-run identification for cases nothing about a re-run touches.
  it("gives each probe outcome its own next step", async () => {
    const detected = {
      id: "mcd", status: "completed" as const, source: "verified_probe" as const,
      provider_id: provider.id, provider_model: unknown.target_id, binding_id: "b-chat",
      profile_id: "openai.chat-embeddings.v1", binding_candidates: [], provider_calls: 4, max_provider_calls: 8,
      capabilities: {
        chat: { status: "supported" as const, evidence: "verified" as const, probe_kind: "chat" },
        // Answered, and the answer carried no tool call. A re-run sends the
        // identical request.
        tools: { status: "assertion_failed" as const, probe_kind: "tool_call" },
        // Refused, and Halro could not read the refusal. Worth re-running.
        stream_usage: { status: "inconclusive" as const, probe_kind: "stream_usage" },
        // Not the model's answer at all.
        streaming: { status: "unavailable" as const, probe_kind: "minimal_stream", error_class: "timeout" },
      },
      recommended_capabilities: { ...emptyCapabilities, chat: true }, selection_revision: "selection", revision: 3,
    };
    vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(async (_id, body) => ({ ...detected, selection_revision: (body as { selection_revision: string }).selection_revision }));
    await openCreate();
    await choose("GPT Future");
    await startDetection();
    expect(await screen.findByLabelText("对话")).toBeChecked();
    expect(screen.getByRole("checkbox", { name: /^工具调用/ }).closest("label")).toHaveTextContent("上游应答里没有该能力的证据");
    expect(screen.getByRole("checkbox", { name: /^流式用量/ }).closest("label")).toHaveTextContent("本次识别没有验证出来");
    expect(screen.getByRole("checkbox", { name: /^流式($|[^用])/ }).closest("label")).toHaveTextContent("本次识别时上游不可达");
    // Every one of them is an outcome the capability editor cannot express, so
    // every one of them is named in the banner rather than silently dropped.
    const banner = screen.getByText(/验证没有得出结论/).closest(".notice")!;
    for (const capability of ["工具调用", "流式用量", "流式"]) {
      expect(banner).toHaveTextContent(capability);
    }
    expect(banner).toHaveTextContent("流式 → 暂时不可用");
  });

  it("adopts a reused detection even when the shared job carries an older client selection token", async () => {
    const queued = {
      id: "cached", status: "queued" as const, source: "verified_probe" as const,
      provider_id: provider.id, provider_model: unknown.target_id, binding_id: "b-chat", profile_id: "openai.chat-embeddings.v1", binding_candidates: [],
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
    await startDetection();
    expect(await screen.findByLabelText("对话")).toBeChecked();
    expect(screen.getByRole("button", { name: "保存为停用" })).toBeEnabled();
  });

  it("does not apply a late verification response after the target selection changed", async () => {
    let resolveDetection!: (value: never) => void;
    vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(() => new Promise((resolve) => { resolveDetection = resolve; }));
    await openCreate();
    await choose("GPT Future");
    await startDetection();
    await waitFor(() => expect(api.createModelCapabilityDetection).toHaveBeenCalledOnce());
    const oldSelection = (vi.mocked(api.createModelCapabilityDetection).mock.calls[0][1] as { selection_revision: string }).selection_revision;
    await choose("GPT Chat");
    await act(async () => resolveDetection({
      id: "late", status: "completed", source: "verified_probe", provider_id: provider.id, provider_model: "gpt-future",
      binding_id: "b-chat", profile_id: "openai.chat-embeddings.v1", binding_candidates: [], provider_calls: 1, max_provider_calls: 8,
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
      binding_id: "b-chat", profile_id: "openai.chat-embeddings.v1", binding_candidates: [], provider_calls: 1, max_provider_calls: 8,
      capabilities: { chat: { status: "inconclusive", probe_kind: "chat" } }, recommended_capabilities: emptyCapabilities,
      selection_revision: (body as { selection_revision: string }).selection_revision, revision: 2,
    }));
    await openCreate();
    await choose("GPT Future");
    await startDetection();
    expect(await screen.findByText("仍无法确认模型能力")).toBeVisible();
    expect(screen.queryByRole("button", { name: "重新检测" })).not.toBeInTheDocument();
    expect(screen.getByRole("button", { name: "手动配置" })).toBeVisible();
  });

  it.each(["unauthorized", "unavailable"] as const)("routes a %s verification result only to provider repair", async (status) => {
    vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(async (_id, body) => ({
      id: `repair-${status}`, status: "completed", source: "verified_probe", provider_id: provider.id, provider_model: unknown.target_id,
      binding_id: "b-chat", profile_id: "openai.chat-embeddings.v1", binding_candidates: [], provider_calls: 1, max_provider_calls: 8,
      capabilities: { chat: { status, probe_kind: "chat" } }, recommended_capabilities: emptyCapabilities,
      selection_revision: (body as { selection_revision: string }).selection_revision, revision: 2,
    }));
    await openCreate();
    await choose("GPT Future");
    await startDetection();
    expect(await screen.findByText("服务商暂时无法完成验证")).toBeVisible();
    expect(screen.getByRole("link", { name: "前往服务商配置 →" })).toHaveAttribute("href", "/admin/providers");
    expect(screen.queryByRole("button", { name: "手动配置" })).not.toBeInTheDocument();
  });

  // An image model is asked three text questions it cannot answer, because the
  // plan has no probe for generating an image. Reporting only "detection
  // failed" reads as a fault in the model; the card has to say what each
  // interface was asked, what came back, and what it could establish at all.
  it("says what each interface was asked and could verify when nothing answered", async () => {
    vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(async (_id, body) => ({
      id: "image", status: "failed" as const, source: "verified_probe" as const, provider_id: provider.id,
      provider_model: unknown.target_id, provider_calls: 3, max_provider_calls: 10,
      binding_candidates: [
        { binding_id: "b-chat", profile_id: "openai.chat-embeddings.v1", access_surface: "openai-api", model_revision: "sha256:a", verifiable: ["chat", "embeddings"], capability: "chat", probe_kind: "minimal_chat", status: "unauthorized" as const, error_class: "authentication", answered: false },
        { binding_id: "b-embed", profile_id: "openai.embeddings.v1", access_surface: "openai-api", model_revision: "sha256:b", verifiable: ["embeddings"], capability: "embeddings", probe_kind: "embedding", status: "inconclusive" as const, error_class: "bad_request", answered: false },
      ],
      capabilities: {}, recommended_capabilities: emptyCapabilities,
      selection_revision: (body as { selection_revision: string }).selection_revision, revision: 2,
    }));
    await openCreate();
    await choose("GPT Future");
    await startDetection();

    const card = (await screen.findByText("未能可靠识别能力")).closest(".notice")!;
    // Not "the model is broken" — what automatic verification can ask at all.
    expect(card.textContent).toContain("图像生成、音频转写、语音这类能力不做自动验证");
    // The rejected credential is named, because that is the actionable one.
    expect(card.textContent).toContain("对话 → 无权验证");
    expect(card.textContent).toContain("向量嵌入 → 无法确认");
    // And what each interface could ever have established.
    expect(card.textContent).toContain("此接口可自动验证：对话、向量嵌入");
    expect(within(card as HTMLElement).getByRole("button", { name: "手动配置" })).toBeVisible();
  });

  // With one interface there is nothing to identify, so identification is
  // skipped and the candidate carries no probe — which used to be the only
  // thing a failed detection showed, reading as "no probe was sent" when the
  // capability probes had in fact all run and all been refused upstream.
  it("shows why each capability probe failed when there was only one interface", async () => {
    vi.spyOn(api, "createModelCapabilityDetection").mockImplementation(async (_id, body) => ({
      id: "single", status: "failed" as const, source: "verified_probe" as const, provider_id: provider.id,
      provider_model: unknown.target_id, binding_id: "b-chat", profile_id: "openai.chat-embeddings.v1",
      provider_calls: 2, max_provider_calls: 10,
      binding_candidates: [
        { binding_id: "b-chat", profile_id: "openai.chat-embeddings.v1", access_surface: "openai-api", model_revision: "sha256:a", verifiable: ["chat", "streaming"], status: "not_probed" as const, answered: false },
      ],
      capabilities: {
        chat: { status: "inconclusive" as const, error_class: "bad_request", probe_kind: "minimal_chat" },
        streaming: { status: "not_probed" as const, probe_kind: "minimal_stream" },
        images: { status: "not_probed" as const, probe_kind: "risk_policy" },
      },
      recommended_capabilities: emptyCapabilities,
      selection_revision: (body as { selection_revision: string }).selection_revision, revision: 2,
    }));
    await openCreate();
    await choose("GPT Future");
    await startDetection();

    const card = (await screen.findByText("未能可靠识别能力")).closest(".notice")!;
    expect(card.textContent).not.toContain("未发出探测");
    expect(card.textContent).toContain("只有这一个接口，无需先做识别");
    // The reason, which is the whole point: the upstream refused the request.
    expect(card.textContent).toContain("对话 → 无法确认");
    expect(card.textContent).toContain("上游拒绝了这次探测请求");
    expect(card.textContent).toContain("流式 → 未检测");
    // A capability the plan never meant to reach is not an outcome.
    expect(card.textContent).not.toContain("图像生成 →");
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
    }), expect.any(String));
  });

  it("refreshes the invocation target catalog through the adapter capability endpoint", async () => {
    await openCreate();
    const refresh = await screen.findByRole("button", { name: "刷新" });
    expect(refresh).toHaveClass("secondary");
    expect(refresh.closest(".deployment-model-input-row")).toContainElement(screen.getByLabelText(/^模型 ID/));
    fireEvent.focus(screen.getByLabelText(/^模型 ID/));
    const listbox = await screen.findByRole("listbox", { name: "可用模型" });
    // The count sits beside the listbox, not inside it: a listbox owns options
    // and groups, and a bare div among them is read as neither.
    expect(listbox.querySelector(".deployment-model-options-meta")).toBeNull();
    const popup = listbox.closest(".deployment-model-options") as HTMLElement;
    expect(popup.querySelector(".deployment-model-options-meta")).toHaveTextContent("可用 2 个模型");
    fireEvent.click(refresh);
    await waitFor(() => expect(api.refreshInvocationTargets).toHaveBeenCalledWith(provider.id));
  });

  // The catalogue GET resolves every binding and returns a state per target, and
  // the options threw it away: a model ready to deploy and one no binding here
  // can serve rendered the same single line, so the operator learned the
  // difference by filling in the form. Banded, never filtered — a name read off
  // the upstream's own console has to still be findable, or a provider that does
  // not serve it reads as Halro's catalogue being broken.
  it("bands the model catalogue by what the operator would do next", async () => {
    vi.mocked(api.invocationTargets).mockResolvedValue(catalog([
      target("gpt-unserved", [], "no_variant"),
      target("gpt-unknown", [], "unknown"),
      target("gpt-ready", [variant("gpt-ready", "b-chat", chatCapabilities)]),
      target("gpt-conflict", [], "conflicting"),
    ]));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));
    fireEvent.change(await screen.findByLabelText(/^服务商/), { target: { value: provider.id } });
    fireEvent.focus(await screen.findByLabelText(/^模型 ID/));

    const listbox = await screen.findByRole("listbox", { name: "可用模型" });
    // Ready first, and every other model still reachable.
    expect(Array.from(listbox.querySelectorAll('[role="option"]')).map((option) => option.textContent))
      .toEqual(["gpt-ready", "gpt-unknown", "gpt-conflict", "gpt-unserved"]);
    // The band name is on the group, not only on the heading: a band that exists
    // only visually tells a screen reader nothing about why these are separated.
    expect(within(listbox).getByRole("group", { name: "可直接创建" })).toBeVisible();
    // Two bands, not one: a catalogue that disagrees with itself needs a person
    // to look, and a model no binding here serves needs a different provider.
    expect(within(listbox).getByRole("group", { name: "目录说法冲突，需要人工确认" })).toBeVisible();
    expect(within(listbox).getByRole("group", { name: "当前服务商上建不了" })).toBeVisible();
    // Arrow keys walk the order the eye walks.
    fireEvent.keyDown(screen.getByLabelText(/^模型 ID/), { key: "ArrowDown" });
    expect(screen.getByLabelText(/^模型 ID/)).toHaveAttribute("aria-activedescendant", "deployment-provider-model-option-0");
    expect(document.getElementById("deployment-provider-model-option-0")).toHaveTextContent("gpt-ready");
  });

  // The state exists to give an operator the one instruction the console could
  // not give before. What it must not do is take away the instruction they
  // already had: the catalogue can be stale, and the operator may know something
  // Halro does not, so declaring the capabilities by hand stays available.
  it("points at the interface serving the model without closing the declare path", async () => {
    const elsewhere = target("deepseek.v3.1", [], "covered_elsewhere");
    elsewhere.covered_by_profiles = ["bedrock.mantle.chat.v1"];
    vi.mocked(api.invocationTargets).mockResolvedValue(catalog([elsewhere]));
    vi.mocked(api.resolveInvocationTarget).mockResolvedValue(elsewhere);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));
    fireEvent.change(await screen.findByLabelText(/^服务商/), { target: { value: provider.id } });
    fireEvent.focus(await screen.findByLabelText(/^模型 ID/));
    fireEvent.click(await screen.findByRole("option", { name: "deepseek.v3.1" }));

    // Names the interface: without it the console can say "not here" but not
    // "there", which is the whole reason the state is worth having.
    expect(await screen.findByText("该模型由另一套接口服务")).toBeVisible();
    expect(screen.getByText(/bedrock\.mantle\.chat\.v1/)).toBeVisible();
    // And the way out the operator had under `unknown` is still on the screen.
    expect(screen.getByRole("button", { name: "手动配置" })).toBeVisible();
  });

  // Three different reasons the capability section can be empty, with three
  // different things to do about them. One panel used to answer all three with
  // "not in the catalogue, run a detection" — wrong for a model the catalogue
  // does list, and a detection cannot answer a routing question anyway.
  it("says why the capabilities are empty, and does not offer a call that cannot answer", async () => {
    const elsewhere = target("deepseek.v3.1", [], "covered_elsewhere");
    elsewhere.covered_by_profiles = ["bedrock.mantle.chat.v1"];
    vi.mocked(api.invocationTargets).mockResolvedValue(catalog([elsewhere]));
    vi.mocked(api.resolveInvocationTarget).mockResolvedValue(elsewhere);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));
    fireEvent.change(await screen.findByLabelText(/^服务商/), { target: { value: provider.id } });
    fireEvent.focus(await screen.findByLabelText(/^模型 ID/));
    fireEvent.click(await screen.findByRole("option", { name: "deepseek.v3.1" }));

    expect(await screen.findByText("目录里有它，只是不在这条接口上")).toBeVisible();
    expect(screen.getByText(/识别帮不上忙/)).toBeVisible();
    // The billable call's ceiling belongs on the panels where pressing it can
    // answer something, not on the one where it cannot.
    expect(screen.queryByText(/最多/)).toBeNull();
  });

  // The other empty reason on the same panel: the upstream returns identifiers
  // and nothing else, so refreshing forever produces no capabilities. Saying
  // "not in the catalogue" alone sends the operator back to a refresh button.
  it("separates an upstream that says nothing from a catalogue that lists nothing", async () => {
    const silent = target("openai.gpt-5.6-luna", [], "unknown");
    silent.metadata_source = "none";
    vi.mocked(api.invocationTargets).mockResolvedValue(catalog([silent]));
    vi.mocked(api.resolveInvocationTarget).mockResolvedValue(silent);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));
    fireEvent.change(await screen.findByLabelText(/^服务商/), { target: { value: provider.id } });
    fireEvent.focus(await screen.findByLabelText(/^模型 ID/));
    fireEvent.click(await screen.findByRole("option", { name: "openai.gpt-5.6-luna" }));

    expect(await screen.findByText("谁都没说过这个模型能做什么")).toBeVisible();
    expect(screen.getByText(/再刷新多少次都不会多出能力信息/)).toBeVisible();
  });

  // The narrowing editor was bounded by the variant, so a model whose catalogue
  // entry claims chat and streaming rendered two rows — and vision, which the
  // interface carries and the model has, had nowhere to be turned on. It is
  // bounded by the interface now: every row the connection can serve is there,
  // the recorded ones are ticked, the rest are marked and left to the operator.
  it("lists what the interface carries and ticks only what the catalogue recorded", async () => {
    // What the connection carries but the catalogue entry does not record. The
    // real case is vision on a Mantle model; the shape is the same.
    const claimed = { ...emptyCapabilities, chat: true, streaming: true };
    const ready = target("openai.gpt-5.6-terra", [variant("openai.gpt-5.6-terra", "b-chat", claimed)]);
    vi.mocked(api.invocationTargets).mockResolvedValue(catalog([ready]));
    vi.mocked(api.resolveInvocationTarget).mockResolvedValue(ready);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));
    fireEvent.change(await screen.findByLabelText(/^服务商/), { target: { value: provider.id } });
    fireEvent.focus(await screen.findByLabelText(/^模型 ID/));
    fireEvent.click(await screen.findByRole("option", { name: "openai.gpt-5.6-terra" }));

    fireEvent.click(await screen.findByText("高级设置：收窄能力"));
    const unrecorded = await screen.findByRole("checkbox", { name: /工具调用/ });
    // Present and selectable: an unrecorded capability is not a capability
    // found missing, so the row stays open to an operator who knows better.
    expect(unrecorded).toBeEnabled();
    expect(unrecorded).not.toBeChecked();
    expect(screen.getByRole("checkbox", { name: /对话/ })).toBeChecked();
  });

  // Ticking past the claims changes who is answering for the capability, and
  // the request has to change with it: the resolution pin binds the write to the
  // claims it was resolved from, and the server refuses a set that exceeds them.
  it("saves a capability the catalogue never recorded as the operator's own declaration", async () => {
    const claimed = { ...emptyCapabilities, chat: true, streaming: true };
    const ready = target("openai.gpt-5.6-terra", [variant("openai.gpt-5.6-terra", "b-chat", claimed)]);
    vi.mocked(api.invocationTargets).mockResolvedValue(catalog([ready]));
    vi.mocked(api.resolveInvocationTarget).mockResolvedValue(ready);
    const create = vi.spyOn(api, "createDeployment").mockResolvedValue({} as never);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));
    fireEvent.change(await screen.findByLabelText("部署名称"), { target: { value: "Terra" } });
    fireEvent.change(await screen.findByLabelText(/^服务商/), { target: { value: provider.id } });
    fireEvent.focus(await screen.findByLabelText(/^模型 ID/));
    fireEvent.click(await screen.findByRole("option", { name: "openai.gpt-5.6-terra" }));
    fireEvent.click(await screen.findByText("高级设置：收窄能力"));
    fireEvent.click(await screen.findByRole("checkbox", { name: /工具调用/ }));

    // Said before the save, not discovered in the evidence column after it.
    expect(await screen.findByText("有能力是你自己声明的")).toBeVisible();

    fireEvent.click(screen.getByRole("button", { name: "保存为停用" }));
    await waitFor(() => expect(create).toHaveBeenCalled());
    const [body] = create.mock.calls[0] as [Record<string, unknown>, string];
    expect(body.mode).toBe("operator_declared");
    // Both would be asking the server to honour a pin and break it at once.
    expect(body).not.toHaveProperty("resolution_revision");
    expect((body.capabilities as Record<string, boolean>).tools).toBe(true);
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
      requires_management_identity: false, requires_canonical_model_mapping: false, max_verification_calls: 10,
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

// The card is a layout change, not a scope change. Two facts on it decide
// whether a deployment serves traffic at all, and a third is the reason two of
// its own buttons are disabled — a card that dropped them would look tidier and
// answer less than the row it replaced.
describe("deployment card keeps what the row decided", () => {
  beforeEach(() => {
    vi.spyOn(api, "providers").mockResolvedValue({ items: [provider], next_cursor: "" });
    vi.spyOn(api, "routes").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "invocationTargets").mockResolvedValue(catalog([]));
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [], next_cursor: "" });
  });
  afterEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/deployments");
  });

  it("carries drift, state words, routes and price on the card itself", async () => {
    vi.spyOn(api, "routes").mockResolvedValue({
      items: [{ id: "route_1", public_model: "gpt", deployment_id: "dep_card", priority: 0, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "" }],
      next_cursor: "",
    });
    vi.spyOn(api, "deployments").mockResolvedValue({
      items: [deployment("dep_card", {
        enabled: true,
        capabilities: { ...chatCapabilities, vision: true, max_context_tokens: 272_000, max_output_tokens: 16_000 },
        capability_evidence: { chat: "verified", vision: "declared" },
        capability_review: {
          state: "drifted", source: "verified_probe", status: "known", model_revision: "r1", catalog_covered: true,
        },
      })],
      next_cursor: "",
    });
    renderPage();

    const card = (await screen.findByText("Deployment dep_card")).closest("article.resource-card") as HTMLElement;
    // Enabled says "not routing" when the snapshot drifted, so the correction
    // has to sit beside the flag rather than behind an expander.
    expect(within(card).getByText("能力已不再受支持")).toBeVisible();
    // Colour is never the only signal: the state exists as words, and it is the
    // conclusion line that carries them now — a second copy in the action bar
    // put the same word on the card twice.
    expect(card.querySelector(".resource-state")).toBeNull();
    // Routed deployments cannot be disabled, and the bar's own button is what
    // says so rather than a menu the operator has to open to find out.
    const disable = within(card).getByRole("button", { name: /^禁用/ });
    expect(disable).toBeDisabled();
    expect(disable).toHaveAttribute("title", "请先停用引用该部署的模型路由");
    // A disabled button is skipped in tab order, so the reason is in the
    // accessibility tree as well as the tooltip.
    expect(card.querySelector(`#${disable.getAttribute("aria-describedby")}`)).toHaveTextContent("请先停用引用该部署的模型路由");
    // The route count is the reason those controls refuse, and it is stated at
    // the control rather than a third time as a fact on every tile; the drawer
    // keeps the count itself.
    expect(within(card).queryByText("路由依赖")).not.toBeInTheDocument();
    // Drift outranks the missing price, so the price is counted rather than
    // shown — nothing is dropped silently. The price read has to land first:
    // while it is in flight nothing is missing yet.
    expect(await within(card).findByRole("button", { name: /^\+1/ })).toBeVisible();
    // Token limits are abbreviated the way a model catalogue abbreviates them.
    expect(card).toHaveTextContent("272K/16K");
    // The model id keeps a name for an assistive reader now that its column label is gone.
    expect(within(card).getByText("上游调用目标：")).toBeInTheDocument();
  });

  // A card that carried "已设置 · 不限 · 无启用路由" spent two lines saying nothing
  // needed doing, on every card in the grid. Each of those facts is on the card
  // for a reason that only exists in its unquiet state.
  it("says nothing about facts that need nothing", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({
      items: [deployment("dep_quiet", { enabled: true, max_concurrency: 0 })],
      next_cursor: "",
    });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
    renderPage();

    const card = (await screen.findByText("Deployment dep_quiet")).closest("article.resource-card") as HTMLElement;
    await waitFor(() => expect(within(card).queryByText("价格设置")).not.toBeInTheDocument());
    expect(within(card).queryByText("并发上限")).not.toBeInTheDocument();
    expect(within(card).queryByText("路由依赖")).not.toBeInTheDocument();
    expect(card.querySelector(".resource-card-facts")).toBeNull();
    // Nothing was declared, and the absence is not written out as a fact.
    expect(within(card).queryByText("以上游为准")).not.toBeInTheDocument();
    // Provider and upstream model are one identity line now.
    expect(within(card).getByText(/OpenAI production/)).toHaveTextContent("gpt-chat");
    // The bar is edit, the way into the drawer, the state change, and the test
    // control at its far end. The menu is in the card's head and holds only
    // what the bar does not.
    expect(Array.from(card.querySelectorAll(".deployment-compact-actions > .button")).map((button) => button.textContent))
      .toEqual(["编辑", "查看详情", "禁用"]);
    const head = card.querySelector(".resource-card-head") as HTMLElement;
    expect(within(head).getByLabelText(/^更多操作/)).toBeInTheDocument();
    // The bar ends with the control, not with a word about the state: where the
    // deployment stands is the conclusion line's answer, and being disabled is
    // one of the things that line says.
    const bar = card.querySelector(".resource-card-actions") as HTMLElement;
    expect(bar.querySelector(".resource-state")).toBeNull();
    expect(bar.lastElementChild).toHaveTextContent("禁用");
    const menu = card.querySelector(".row-overflow-menu") as HTMLElement;
    expect(Array.from(menu.querySelectorAll(".button")).map((button) => button.textContent))
      .toEqual(["创建替代", "删除"]);
  });

  // Enabling and disabling are one click from the bar, not two through a menu —
  // and the enable precondition that used to live on the menu row has to travel
  // with the button, or the console offers a click the server will refuse.
  it("changes state from the card's action bar", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({
      items: [
        deployment("dep_on", { enabled: true }),
        deployment("dep_untested", { enabled: false }),
      ],
      next_cursor: "",
    });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
    const update = vi.spyOn(api, "updateDeployment").mockResolvedValue({} as never);
    renderPage();

    const enabledCard = (await screen.findByText("Deployment dep_on")).closest("article.resource-card") as HTMLElement;
    // Enabled and nothing wrong: the conclusion line has no state to report, so
    // it reports the resting verdict instead.
    expect(within(enabledCard).queryByText("已禁用")).toBeNull();
    fireEvent.click(within(enabledCard).getByRole("button", { name: /^禁用/ }));
    await waitFor(() => expect(update).toHaveBeenCalledOnce());
    expect(update).toHaveBeenCalledWith("dep_on", expect.objectContaining({ enabled: false }), 1);

    // Untested: enabling is refused, and the button says why rather than
    // sending a request the server answers with a 409.
    const untestedCard = (await screen.findByText("Deployment dep_untested")).closest("article.resource-card") as HTMLElement;
    // Disabled outranks the resting verdict: it is the reason nothing reaches
    // this deployment, whatever the last test said.
    expect(within(untestedCard).getByText("已禁用")).toBeVisible();
    const enable = within(untestedCard).getByRole("button", { name: /^启用/ });
    expect(enable).toBeDisabled();
    expect(enable).toHaveAttribute("title", "请先测试当前版本");
  });

  // Thirty nameless <article>s were thirty stops that announced nothing, and a
  // status region per card announced a bare "失败" with no way to tell which
  // card produced it.
  it("names each card and lets one region speak for the grid", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [deployment("dep_named", { enabled: true })], next_cursor: "" });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
    renderPage();

    const card = (await screen.findByText("Deployment dep_named")).closest("article.resource-card") as HTMLElement;
    const heading = within(card).getByRole("heading", { level: 3 });
    expect(heading).toHaveTextContent("Deployment dep_named");
    expect(card).toHaveAttribute("aria-labelledby", heading.id);
    // Every control on the card says which deployment it acts on, and the
    // visible word leads the accessible name so speech input still matches it.
    expect(within(card).getByRole("button", { name: "编辑 — Deployment dep_named" })).toBeVisible();
    expect(within(card).getByRole("button", { name: "测试 — Deployment dep_named" })).toBeVisible();
    // One region for the grid, not one per card.
    expect(document.querySelectorAll("[role=\"status\"][aria-live]")).toHaveLength(1);
    expect(within(card).queryByRole("status")).not.toBeInTheDocument();
  });

  // The Test control reports the verdict it produced, and only that: a probe
  // failure outranks a passing manual test on the line beside it, but the
  // button must not go red for a test that passed.
  it("colours the test control by its own verdict, not by the card's condition", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({
      items: [
        deployment("dep_pass", { enabled: true, last_test_status: "healthy", last_test_revision: 1, last_test_latency_millis: 151 }),
        deployment("dep_fail", { enabled: true, last_test_status: "unhealthy", last_test_revision: 1, last_test_error_class: "connect" }),
        deployment("dep_probe", {
          enabled: true, last_test_status: "healthy", last_test_revision: 1, last_test_latency_millis: 151,
          probe: { state: "unhealthy", observed_at: "2026-08-25T02:00:00Z", error_class: "connect" },
        }),
        // A verdict against a revision that has since moved no longer applies,
        // so it stays neutral rather than reading as current health.
        deployment("dep_stale", { enabled: true, last_test_status: "healthy", last_test_revision: 0 }),
      ],
      next_cursor: "",
    });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
    renderPage();

    await screen.findByText("Deployment dep_stale");
    const verdict = (id: string) => {
      const card = screen.getByText(`Deployment ${id}`).closest("article.resource-card") as HTMLElement;
      return within(card).getByRole("button", { name: /^测试/ }).getAttribute("data-test-state");
    };
    expect(verdict("dep_pass")).toBe("success");
    expect(verdict("dep_fail")).toBe("failure");
    expect(verdict("dep_probe")).toBe("success");
    expect(verdict("dep_stale")).toBe("stale");
  });

  // The card is a fixed number of slots so that slot n of one tile is the same
  // subgrid track as slot n of its neighbour. A conditional fifth child, or one
  // appended after the action bar, breaks that alignment for the whole band —
  // which is what a failure sentence below the bar used to do.
  it("renders the same slots whatever is wrong with the deployment", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({
      items: [
        deployment("dep_quiet_slots", { enabled: true, last_test_status: "healthy", last_test_revision: 1 }),
        deployment("dep_loud_slots", {
          enabled: true, last_test_status: "unhealthy", last_test_revision: 1, last_test_error_class: "connect",
          probe: { state: "unhealthy", observed_at: "2026-08-25T02:00:00Z", error_class: "connect" },
          capabilities: { ...chatCapabilities, max_context_tokens: 272_000 },
        }),
      ],
      next_cursor: "",
    });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
    renderPage();

    await screen.findByText("Deployment dep_loud_slots");
    const slots = (id: string) => {
      const card = screen.getByText(`Deployment ${id}`).closest("article.resource-card") as HTMLElement;
      return Array.from(card.children).map((child) => child.className.split(" ")[0]);
    };
    expect(slots("dep_quiet_slots")).toEqual(["resource-card-head", "deployment-condition", "deployment-spec", "resource-card-actions"]);
    expect(slots("dep_loud_slots")).toEqual(slots("dep_quiet_slots"));

    // The count has to hold on the path that used to break it. Both records
    // above render on mount, and neither can reach a rejected mutation — which
    // is how a fifth child came back after being removed: it appears only once
    // a write fails, and nothing here looked there.
    vi.spyOn(api, "updateDeployment").mockRejectedValue(new ApiError(409, "no price", "deployment_price_unavailable"));
    fireEvent.click(screen.getByRole("button", { name: "禁用 — Deployment dep_quiet_slots" }));
    await screen.findByText(PRICE_BLOCKER);
    expect(slots("dep_quiet_slots")).toEqual(["resource-card-head", "deployment-condition", "deployment-spec", "resource-card-actions"]);
  });

  // A tile that grew to full width pushed every card after it down the page,
  // so reading one deployment moved the rest — including the card the operator
  // was pointing at. The details open beside the grid instead: the card stays
  // exactly where it was, and closing the drawer puts focus back on the control
  // that opened it.
  it("opens the details in a drawer beside the card rather than growing the card", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [existingDeployment], next_cursor: "" });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
    renderPage();

    const open = await screen.findByRole("button", { name: /^查看详情/ });
    const card = open.closest("article.resource-card") as HTMLElement;
    // A dialog is not a disclosure: the control must not claim an expanded state.
    expect(open).not.toHaveAttribute("aria-expanded");
    // jsdom does not focus a clicked button the way a browser does, and the
    // control's focus is what the drawer has to give back.
    open.focus();
    fireEvent.click(open);

    const drawer = await screen.findByRole("dialog", { name: "Deployment dep_1 详情" });
    expect(drawer).toHaveClass("drawer");
    expect(within(drawer).getByText("价格版本")).toBeVisible();
    // The details are in the drawer, and nothing of them was left in the card.
    expect(card.querySelector(".deployment-details")).toBeNull();
    expect(card).not.toHaveClass("expanded");

    fireEvent.keyDown(document, { key: "Escape" });
    await waitFor(() => expect(screen.queryByRole("dialog")).not.toBeInTheDocument());
    expect(document.activeElement).toBe(open);
  });

  // The boxed strip that opened the drawer is gone: with everything healthy it
  // was six reassuring values and no answer to "what do I do", which is how an
  // operator reads a panel twice and still asks what it is for. What lived only
  // there — enabled state, both health verdicts, the route count — moved into
  // the section that describes how this deployment runs.
  it("keeps every fact the readiness strip held, in the runtime section", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [existingDeployment], next_cursor: "" });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /^查看详情/ }));

    const drawer = await screen.findByRole("dialog", { name: "Deployment dep_1 详情" });
    expect(drawer.querySelector(".detail-status")).toBeNull();
    const runtime = within(drawer).getByRole("heading", { name: "运行与限制" }).closest("section") as HTMLElement;
    for (const label of ["状态", "主动探测", "最近手动测试", "路由依赖", "并发上限", "上下文窗口", "输出上限"]) {
      expect(within(runtime).getByText(label), label).toBeVisible();
    }
  });

  // Two of these used to overstate what they knew: the test verdict is a manual
  // test that can be days old — the 30-second probe that actually removes a
  // deployment from the router's candidates is the state beside it — and the
  // route count only ever counted enabled routes without saying so.
  it("does not let a manual test read as health, and says which routes it counts", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({
      items: [deployment("dep_1", { enabled: true, last_test_status: "healthy", last_test_revision: 1, last_test_latency_millis: 1336, last_tested_at: "2026-08-23T02:52:00Z" })],
      next_cursor: "",
    });
    vi.spyOn(api, "routes").mockResolvedValue({
      items: [
        { id: "route_1", public_model: "gpt", deployment_id: "dep_1", priority: 0, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "" },
        { id: "route_2", public_model: "gpt-legacy", deployment_id: "dep_1", priority: 1, strategy: "ordered", enabled: false, revision: 1, created_at: "", updated_at: "" },
      ],
      next_cursor: "",
    });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /^查看详情/ }));

    const drawer = await screen.findByRole("dialog", { name: "Deployment dep_1 详情" });
    expect(within(drawer).getByText("最近手动测试")).toBeVisible();
    expect(within(drawer).getByText("通过 · 1336ms")).toBeVisible();
    // One of the two routes is disabled, and the label says which kind it counts.
    expect(within(drawer).getByText("1 条启用路由")).toBeVisible();
  });

  // The review panel used to open with two sentences, list five facts — three of
  // them a dash — and close with a third sentence, without ever saying what to
  // do. And it gave the same label and the same warning colour to two different
  // facts: "nothing supports this any more" and "the catalog disagrees with a
  // declaration we are still serving".
  it("states a capability disagreement as capabilities, and offers the one action that resolves it", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({
      items: [deployment("dep_1", {
        capabilities: { ...chatCapabilities, developer_role: true },
        capability_review: {
          state: "review_available", source: "operator_declared", status: "known", model_revision: "r1",
          catalog_covered: true, catalog_source: "builtin_catalog",
          available_for_review: ["vision", "reasoning"],
          no_longer_supported: ["developer_role"],
          reason: "catalog_establishes_less",
        },
      })],
      next_cursor: "",
    });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /^查看详情/ }));

    const drawer = await screen.findByRole("dialog", { name: "Deployment dep_1 详情" });
    const notice = drawer.querySelector(".deployment-capability-review") as HTMLElement;
    // The title covers the loudest thing in the panel, not half of it.
    expect(within(notice).getByText("目录与你的声明不一致")).toBeVisible();
    expect(within(notice).getByText("可开启")).toBeVisible();
    expect(within(notice).getByText("视觉、推理")).toBeVisible();
    // Not "no longer supported", and not a warning: it is still being served.
    expect(within(notice).getByText("目录不认可")).toBeVisible();
    expect(within(notice).getByText(/开发者角色 · 仍按你的声明运行/)).toBeVisible();
    expect(notice).not.toHaveClass("warning");
    // The rows the panel has nothing to say about are gone, along with the
    // provenance cells and the closing sentence.
    expect(within(notice).queryByText("已由管理员关闭")).not.toBeInTheDocument();
    expect(within(notice).queryByText("保存的答案来自哪里")).not.toBeInTheDocument();

    // The instruction was "turn a capability on and retest"; following it used to
    // mean closing the drawer and finding the card again.
    fireEvent.click(within(notice).getByRole("button", { name: "去编辑能力" }));
    expect(await screen.findByRole("dialog", { name: "编辑模型部署" })).toBeVisible();
    expect(screen.queryByRole("dialog", { name: "Deployment dep_1 详情" })).not.toBeInTheDocument();
  });

  // The state that decides routing today, and the one the console could not see
  // until the Admin API reported it: a failing probe takes the deployment out of
  // the router's candidates while enabled, tested and priced all still read as
  // they did. The card carries it for the same reason it carries drift.
  it("reports a failing active probe on the card and says what it costs in the drawer", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({
      items: [deployment("dep_1", {
        enabled: true, last_test_status: "healthy", last_test_revision: 1, last_test_latency_millis: 1336,
        probe: { state: "unhealthy", observed_at: "2026-08-25T02:00:00Z", error_class: "connect" },
      })],
      next_cursor: "",
    });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
    renderPage();

    const card = (await screen.findByText("Deployment dep_1")).closest("article.resource-card") as HTMLElement;
    // The probe is what decides routing, so it outranks the manual test that
    // passed — the card must not report 1336ms of health for a deployment the
    // router has already dropped. The classified reason rides the same line.
    expect(within(card).getByText(/主动探测未通过 · 无法建立到上游的连接/)).toBeVisible();
    expect(card).not.toHaveTextContent("1336ms");

    fireEvent.click(within(card).getByRole("button", { name: /^查看详情/ }));
    const drawer = await screen.findByRole("dialog", { name: "Deployment dep_1 详情" });
    expect(within(drawer).getByText("未通过")).toBeVisible();
    // The consequence, and the classified reason — the same wording a failed
    // manual test gets, never the upstream's own sentence.
    expect(within(drawer).getByText(/已移出路由候选 · 无法建立到上游的连接/)).toBeVisible();
  });

  // Not probed is not a failure: a deployment stays eligible until a probe has
  // actually said otherwise, and reporting it as unhealthy would have the
  // console claim an outage every restart.
  it("distinguishes a deployment no probe has reached yet", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({
      items: [deployment("dep_1", { enabled: true, probe: { state: "not_probed" } })],
      next_cursor: "",
    });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /^查看详情/ }));

    const drawer = await screen.findByRole("dialog", { name: "Deployment dep_1 详情" });
    expect(within(drawer).getByText("尚未探测")).toBeVisible();
    expect(within(drawer).getByText("还没跑过探测，暂不影响路由")).toBeVisible();
    expect(screen.queryByText("主动探测未通过")).not.toBeInTheDocument();
  });

  // The panel this replaced listed four rows of US$0.00 for a free deployment,
  // and called itself a timeline while rendering only the versions that had not
  // taken effect yet — so the version actually in force, and everything it
  // replaced, were the two things it did not show.
  it("states a free price once and lists every version the deployment charged under", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [existingDeployment], next_cursor: "" });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({
      items: [
        { ...activePriceVersion(), id: "price_free", version: 4, billing_mode: "free", status: "active", effective_from: "2026-08-20T00:00:00Z" },
        { ...activePriceVersion(), id: "price_old", version: 3, status: "superseded", effective_from: "2026-08-01T00:00:00Z" },
      ],
      next_cursor: "",
    });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /^查看详情/ }));

    const drawer = await screen.findByRole("dialog", { name: "Deployment dep_1 详情" });
    // A free price is stated by the version that fixed it and nowhere else: no
    // rate grid, and no "billing mode" cell repeating the row below it.
    expect(within(drawer).queryByText("输入价格")).not.toBeInTheDocument();
    expect(within(drawer).queryByText("缓存输入价格")).not.toBeInTheDocument();
    expect(within(drawer).queryByText("计费方式")).not.toBeInTheDocument();

    const timeline = drawer.querySelector("ol.detail-timeline") as HTMLElement;
    expect(within(timeline).getByText("v4")).toBeVisible();
    expect(within(timeline).getByText(/生效中/)).toBeVisible();
    // The superseded version is the history the old panel dropped.
    expect(within(timeline).getByText("v3")).toBeVisible();
    expect(within(timeline).getByText(/已被替代/)).toBeVisible();
    // manual · asserted · temporary_estimate is three identifiers from three
    // enums; the drawer says what they mean.
    expect(within(timeline).getAllByText(/临时估算 · 管理员录入 · Halro 未验证/).length).toBe(2);
  });

  // The card truncates its capabilities because a tile has a width, and the
  // identifiers used to sit behind a disclosure — which put an extra click in
  // front of the one thing the drawer gets opened for while reading a log line.
  it("lists every capability with its evidence, and states the upstream target", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({
      items: [deployment("dep_1", {
        capabilities: { ...chatCapabilities, vision: true, json_object: true, structured_outputs: true },
        capability_evidence: { chat: "verified", vision: "declared" },
        updated_at: "2026-08-23T02:52:00Z",
      })],
      next_cursor: "",
    });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /^查看详情/ }));

    const drawer = await screen.findByRole("dialog", { name: "Deployment dep_1 详情" });
    for (const heading of ["计费", "能力", "运行与限制", "连接与标识"]) {
      expect(within(drawer).getByRole("heading", { name: heading })).toBeVisible();
    }
    const list = within(drawer).getByRole("list", { name: "能力" });
    // chat, streaming, tools, stream_usage, vision, json_object, structured_outputs —
    // all seven, not six and a "+1".
    expect(within(list).getAllByRole("listitem")).toHaveLength(7);
    expect(within(within(list).getByText("对话").closest("li") as HTMLElement).getByText("已验证")).toBeVisible();
    expect(within(within(list).getByText("视觉").closest("li") as HTMLElement).getByText("已声明")).toBeVisible();
    // Nothing established this one either way; that is not the same as declared.
    expect(within(within(list).getByText("流式").closest("li") as HTMLElement).getByText("未记录证据")).toBeVisible();

    expect(within(drawer).getByText("上游调用目标")).toBeVisible();
    expect(within(drawer).getByText("gpt-chat")).toBeVisible();
    // The provider link lands on the provider itself, not on the top of the list.
    expect(within(drawer).getByRole("link", { name: /OpenAI production/ }))
      .toHaveAttribute("href", "/admin/providers#provider-provider_openai");
    expect(within(drawer).queryByText("技术详情")).not.toBeInTheDocument();
    // Internal identifiers were dropped on purpose: the profile and binding ids
    // name Halro's own wiring, and the evidence summary said in one word what
    // the capability list above now says per capability.
    // The revision counter went the same way: what an operator reads it for is
    // when the deployment last changed, which is the timestamp beside it.
    expect(within(drawer).getByText("最近更新")).toBeVisible();
    for (const dropped of ["能力配置", "绑定 ID", "部署 ID", "能力证据", "修订号"]) {
      expect(within(drawer).queryByText(dropped), dropped).not.toBeInTheDocument();
    }
  });

  it("draws modality marks from the server mapping and never as the whole capability set", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({
      items: [deployment("dep_marks", {
        capabilities: { ...chatCapabilities, vision: true },
        capability_evidence: { chat: "verified", vision: "declared" },
      })],
      next_cursor: "",
    });
    renderPage();

    const card = (await screen.findByText("Deployment dep_marks")).closest("article.resource-card") as HTMLElement;
    // Each mark names its direction, its modality and the evidence behind it,
    // so what is on screen is never the only thing carrying the fact.
    // The marks appear once the profile bundle lands: the mapping is the
    // server's, and writing them from a guess before it arrives is the drift
    // this whole arrangement exists to avoid.
    expect(await within(card).findByLabelText("输入：文本（已验证）")).toBeVisible();
    // The modality is written out, not drawn: five 16px glyphs needed a hover to
    // be understood, and evidence rides on weight rather than on colour alone.
    const marks = card.querySelector(".modality-marks") as HTMLElement;
    expect(marks).toHaveTextContent("文本");
    expect(marks).toHaveTextContent("图像");
    expect(marks.querySelector("svg")).toBeNull();
    expect(within(card).getByLabelText("输入：图像（已声明）")).toHaveAttribute("data-evidence", "declared");
    expect(within(card).getByLabelText("输入：图像（已声明）")).toBeVisible();
    expect(within(card).getByLabelText("输出：文本（已验证）")).toBeVisible();
    // streaming, tools and stream_usage express no modality at all. Without the
    // count beside the marks they would read as absent rather than as protocol
    // features the marks cannot draw.
    expect(within(card).getByText("5 项能力")).toBeVisible();
  });
});

describe("deployment connection test", () => {
  beforeEach(() => {
    vi.spyOn(api, "providers").mockResolvedValue({ items: [provider], next_cursor: "" });
    vi.spyOn(api, "routes").mockResolvedValue({ items: [], next_cursor: "" });
    // Priced, so the card's one line is about the test. A missing price outranks
    // a failed test — it is refused on every request, where a failed manual test
    // is refused on none — and an unpriced fixture would put these assertions in
    // front of a line about the price instead.
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
  });

  afterEach(() => vi.restoreAllMocks());

  // A failed test used to be a red word with nothing behind it: the response
  // carried the class and the upstream's own reply, and the row dropped both.
  it("says why a deployment connection test failed", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [existingDeployment], next_cursor: "" });
    vi.spyOn(api, "testDeployment").mockRejectedValue(new ApiError(502, "request failed (502)", "", "", {
      status: "unhealthy", error_class: "authentication", provider_status: 401,
      provider_code: "invalid_api_key", error_detail: "provider error (401): incorrect api key provided",
    }));
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /^测试/ }));
    const reason = await screen.findByText(/上游拒绝了这份凭据/);
    expect(reason).toHaveTextContent("HTTP 401");
    expect(reason).toHaveTextContent("invalid_api_key");
    expect(reason).toHaveTextContent("incorrect api key provided");
  });

  // The store keeps the class of the last failure, so a reload has to explain
  // the red word it is still showing.
  it("explains a failure the record remembers, without a test in this session", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({
      items: [deployment("dep_1", { last_test_status: "unhealthy", last_test_revision: 1, last_test_error_class: "connect" })],
      next_cursor: "",
    });
    renderPage();

    expect(await screen.findByText(/无法建立到上游的连接/)).toBeVisible();
  });

  // The record still describes the previous run when the request never reached
  // the store, so reading it alone reports the wrong verdict for the click the
  // operator just made.
  it("reports a test that never reached the store rather than the record's older verdict", async () => {
    vi.spyOn(api, "deployments").mockResolvedValue({
      items: [deployment("dep_1", { enabled: true, last_test_status: "healthy", last_test_revision: 1 })],
      next_cursor: "",
    });
    // Enabled and routed, so the resting verdict is what the card has to say. A deployment
    // nothing points at carries no traffic either, and the card says that
    // instead of reporting a passing test on something unreachable.
    vi.mocked(api.routes).mockResolvedValue({
      items: [{ id: "route_1", public_model: "gpt", deployment_id: "dep_1", priority: 0, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "" }],
      next_cursor: "",
    });
    vi.spyOn(api, "testDeployment").mockRejectedValue(
      new ApiError(409, "deployment changed during validation; test the current revision again", "", "", {
        error: "deployment changed during validation; test the current revision again",
      }),
    );
    renderPage();

    expect(await screen.findByText("通过")).toBeVisible();
    fireEvent.click(await screen.findByRole("button", { name: /^测试/ }));
    expect(await screen.findByText(/Halro 在发往上游之前拒绝了这次探测/)).toHaveTextContent("deployment changed during validation");
    expect(screen.queryByText("通过")).not.toBeInTheDocument();
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
    // Not knowing whether a price exists is not the same as knowing one is
    // missing, so the card says which and the line itself is the retry.
    const retry = await screen.findByRole("button", { name: "不可用" });
    fireEvent.click(retry);
    await waitFor(() => expect(read).toHaveBeenCalledTimes(2));
    await waitFor(() => expect(screen.queryByRole("button", { name: "不可用" })).not.toBeInTheDocument());
  });

  // The refusal is a reply to one click, not a property of the deployment, so it
  // is reported once through the notification channel and nowhere else. The card
  // used to keep a copy of it too — a conditional block rendered after the
  // action bar, which the card contract forbids by name because it takes the
  // action bars of every tile beside it off their shared line. That copy also
  // outlived what it described, needing an effect to retract it once the price
  // it demanded existed; the condition line reads `activePrice` on every render
  // and cannot go stale that way.
  it("reports a refused enable once and stops naming the price once it exists", async () => {
    vi.mocked(api.deployments).mockResolvedValue({
      items: [deployment("dep_1", { last_test_status: "healthy", last_test_revision: 1 })],
      next_cursor: "",
    });
    vi.spyOn(api, "deploymentPrices")
      .mockResolvedValueOnce({ items: [], next_cursor: "" })
      .mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
    vi.spyOn(api, "updateDeployment").mockRejectedValue(new ApiError(409, "no price", "deployment_price_unavailable"));
    const create = vi.spyOn(api, "createDeploymentPrice").mockResolvedValue(activePriceVersion());
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /^启用/ }));
    // One report, in the channel a one-time answer belongs in.
    expect(await screen.findAllByText(PRICE_BLOCKER)).toHaveLength(1);

    // The way to the form is the card's own line about the missing price, which
    // is on every card that lacks one rather than only on the card whose enable
    // was just refused.
    fireEvent.click(screen.getByRole("button", { name: "未设置价格" }));
    fireEvent.change(await screen.findByLabelText("输入 USD / 百万词元"), { target: { value: "5" } });
    fireEvent.click(screen.getByRole("button", { name: "下一步：核对" }));
    fireEvent.change(await screen.findByLabelText("当前密码"), { target: { value: "pw" } });
    fireEvent.click(screen.getByRole("button", { name: "确认并创建价格版本" }));

    await waitFor(() => expect(create).toHaveBeenCalled());
    // The price fact was on the card because it was missing. Once it exists the
    // card has nothing to say about it, and the whole cell goes.
    await waitFor(() => expect(screen.queryByText("价格设置")).not.toBeInTheDocument());
    await waitFor(() => expect(screen.queryByRole("button", { name: "未设置价格" })).not.toBeInTheDocument());
  });

  // The provider zone decides which hours a window means, and it used to be
  // typed from memory into a bare box where a typo only surfaced as a rejected
  // submission. Picked from the list, the name reaching the server is one the
  // engine already resolved.
  it("carries the zone picked from the list into the created version", async () => {
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
    const create = vi.spyOn(api, "createDeploymentPrice").mockResolvedValue(activePriceVersion());
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /^查看详情/ }));
    fireEvent.click(await screen.findByRole("button", { name: "调整价格" }));
    fireEvent.click(await screen.findByRole("checkbox", { name: /分时价位/ }));

    const zone = screen.getByLabelText("供应商时区");
    fireEvent.change(zone, { target: { value: "berlin" } });
    fireEvent.click(screen.getByRole("option", { name: /Europe\/Berlin/ }));
    expect(zone).toHaveValue("Europe/Berlin");

    fireEvent.click(screen.getByRole("button", { name: "下一步：核对" }));
    fireEvent.change(await screen.findByLabelText("当前密码"), { target: { value: "pw" } });
    fireEvent.click(screen.getByRole("button", { name: "确认并创建价格版本" }));

    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(create.mock.calls[0][1]).toMatchObject({
      schedule: { timezone: "Europe/Berlin", windows: [{ start: "09:00", end: "12:00" }] },
    });
  });

  // The cache-read rate has no server-side default: an omitted term would be a
  // 400, and a term the form quietly filled with the input rate would over-charge
  // every cached prompt. It is sent explicitly, and an adjustment opens on the
  // rate the current version already carries rather than on zero.
  it("sends the cache-read rate with every price version it creates", async () => {
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
    const create = vi.spyOn(api, "createDeploymentPrice").mockResolvedValue(activePriceVersion());
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: /^查看详情/ }));
    fireEvent.click(await screen.findByRole("button", { name: "调整价格" }));
    expect(await screen.findByLabelText(/^缓存输入 USD \/ 百万词元/)).toHaveValue("0.1");
    fireEvent.change(screen.getByLabelText(/^缓存输入 USD \/ 百万词元/), { target: { value: "0.5" } });
    fireEvent.click(screen.getByRole("button", { name: "下一步：核对" }));
    expect(await screen.findByText("$0.1 → $0.5")).toBeVisible();
    fireEvent.change(await screen.findByLabelText("当前密码"), { target: { value: "pw" } });
    fireEvent.click(screen.getByRole("button", { name: "确认并创建价格版本" }));

    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(create.mock.calls[0][1]).toMatchObject({ input_usd_per_million: "1", cached_input_usd_per_million: "0.5" });
  });

  // The detail panel is where an operator reads what a deployment charges. It
  // listed input, output and fixed and stopped there, so the rate that decides
  // most of a cache-heavy bill was invisible outside the edit form.
  it("reads the cache-read rate out on the deployment detail panel", async () => {
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /^查看详情/ }));
    const cached = (await screen.findByText("缓存输入价格")).closest("div")!;
    expect(within(cached).getByText("US$0.10")).toBeVisible();
    expect(within(cached).getByText("USD / 百万词元")).toBeVisible();
  });

  // A first price is the case with nothing to copy from, and zero is the one
  // default that would be wrong: it gives every cached prompt token away. The
  // field follows the input rate — what a cached token cost before the rate
  // existed — until the operator lowers it deliberately.
  it("defaults an untouched cache-read rate to the input rate rather than zero", async () => {
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [], next_cursor: "" });
    const create = vi.spyOn(api, "createDeploymentPrice").mockResolvedValue(activePriceVersion());
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "未设置价格" }));
    fireEvent.change(await screen.findByLabelText("输入 USD / 百万词元"), { target: { value: "5" } });
    expect(screen.getByLabelText(/^缓存输入 USD \/ 百万词元/)).toHaveValue("5");
    fireEvent.click(screen.getByRole("button", { name: "下一步：核对" }));
    fireEvent.change(await screen.findByLabelText("当前密码"), { target: { value: "pw" } });
    fireEvent.click(screen.getByRole("button", { name: "确认并创建价格版本" }));

    await waitFor(() => expect(create).toHaveBeenCalled());
    expect(create.mock.calls[0][1]).toMatchObject({ input_usd_per_million: "5", cached_input_usd_per_million: "5" });
  });

  // The server refuses any version that is not strictly later than every
  // non-cancelled one. The form used to open on "immediately" regardless, so a
  // deployment carrying a scheduled version offered a path whose only possible
  // outcome was a 409 — repeatedly, since nothing about the row explained it.
  it("keeps immediate pricing off the menu while a scheduled version outranks it", async () => {
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [scheduledPriceVersion()], next_cursor: "" });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "未设置价格" }));

    expect(await screen.findByRole("option", { name: "立即生效（从现在起按此价计费）" })).toBeDisabled();
    expect(screen.getByLabelText("何时生效")).toHaveValue("scheduled");
    expect(screen.getByText(/已有计划版本 v4 .*无法选择“立即生效”/)).toBeVisible();
    expect(screen.getByLabelText("生效时间")).toBeVisible();
  });

  it("refuses an effective time that does not follow the scheduled version", async () => {
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [scheduledPriceVersion()], next_cursor: "" });
    const create = vi.spyOn(api, "createDeploymentPrice");
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "未设置价格" }));
    fireEvent.change(await screen.findByLabelText("输入 USD / 百万词元"), { target: { value: "5" } });
    // The field reads in the accounting zone, so the value it is given has to be
    // written in that zone too.
    fireEvent.change(screen.getByLabelText("生效时间"), {
      target: { value: isoToZonedInput(new Date(Date.parse(scheduledPriceVersion().effective_from) - 60_000), DEFAULT_TIME_ZONE) },
    });

    expect(await screen.findByText(/生效时间必须晚于计划版本 v4/)).toBeVisible();
    expect(screen.getByRole("button", { name: "下一步：核对" })).toBeDisabled();
    expect(create).not.toHaveBeenCalled();
  });

  // The submit button lives in a sticky footer, so a rejection rendered into the
  // scrolled-away part of the modal: the operator saw the click do nothing and
  // clicked again, which is exactly how one refusal turned into eight identical
  // POSTs. The failure has to reach them, and say which rule was broken.
  it("brings the refused price version's reason to the operator", async () => {
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "createDeploymentPrice").mockRejectedValue(
      new ApiError(409, "price timeline conflict: effective_from must follow all non-cancelled versions (latest is v4 effective 2126-08-01T00:00:00Z)", "price_timeline_conflict"),
    );
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "未设置价格" }));
    fireEvent.change(await screen.findByLabelText("输入 USD / 百万词元"), { target: { value: "5" } });
    fireEvent.click(screen.getByRole("button", { name: "下一步：核对" }));
    fireEvent.change(await screen.findByLabelText("当前密码"), { target: { value: "pw" } });
    fireEvent.click(screen.getByRole("button", { name: "确认并创建价格版本" }));

    const alert = await screen.findByRole("alert");
    expect(within(alert).getByText(/生效时间必须晚于所有未取消的版本/)).toBeVisible();
    // The server names the blocking version, which the client's own view can be
    // too stale to know about, so the detail stays visible under the headline.
    expect(within(alert).getByText(/latest is v4 effective/)).toBeVisible();
    await waitFor(() => expect(alert.parentElement).toHaveFocus());
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
    fireEvent.click(await screen.findByRole("button", { name: /^查看详情/ }));
    // Each scheduled row carries a cancel button, so its accessible name names
    // the version rather than being one of several identical "取消".
    for (const name of ["调整价格", "确认恢复价格", "取消价格版本 v4"]) {
      const control = await screen.findByRole("button", { name });
      expect(control).toBeDisabled();
      expect(control).toHaveAttribute("title", "只读账户无法执行此操作。");
    }
  });

  // The price form opens on top of the details drawer, and both dialogs listen
  // for Escape on the document. Only the one on top may answer it: a key that
  // closed the drawer underneath would take the half-filled form with it.
  it("closes only the top dialog when the price form is open over the details drawer", async () => {
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /^查看详情/ }));
    fireEvent.click(await screen.findByRole("button", { name: "调整价格" }));
    expect(await screen.findByLabelText("输入 USD / 百万词元")).toBeVisible();

    fireEvent.keyDown(document, { key: "Escape" });

    await waitFor(() => expect(screen.queryByLabelText("输入 USD / 百万词元")).not.toBeInTheDocument());
    expect(screen.getByRole("dialog", { name: "Deployment dep_1 详情" })).toBeVisible();
  });

  // Confirming an immediate price change is the last point at which it can be
  // stopped, so the version it replaces has to be on the same screen.
  it("shows the price being replaced and that an immediate version cannot be canceled", async () => {
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePriceVersion()], next_cursor: "" });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: /^查看详情/ }));
    fireEvent.click(await screen.findByRole("button", { name: "调整价格" }));
    fireEvent.change(await screen.findByLabelText("输入 USD / 百万词元"), { target: { value: "5" } });
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
    formula_version: "usd_tokens_v1", input_micros_per_million: 1_000_000, cached_input_micros_per_million: 100_000,
    output_micros_per_million: 2_000_000,
    fixed_request_micros_usd: 0, effective_from: "2026-08-01T00:00:00Z",
    source: { type: "manual", assurance: "asserted", reference: "temporary_estimate" }, status: "active",
  };
}

// Far enough out that the suite's own clock can never overtake it and turn the
// blocking version into a past one mid-test.
function scheduledPriceVersion(): DeploymentPriceVersion {
  return { ...activePriceVersion(), id: "price_2", version: 4, effective_from: "2126-08-01T00:00:00Z", status: "scheduled" };
}

async function openCreate() {
  renderPage();
  fireEvent.click(await screen.findByRole("button", { name: "＋ 新建模型部署" }));
  await screen.findByRole("button", { name: "刷新" });
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
  // Without the provider `useNotify` is a no-op, so every toast the page raises
  // went unrendered and unasserted — which is how a card kept an inline copy of
  // an error the notification channel was already reporting, with no test able
  // to see the duplicate.
  return render(
    <QueryClientProvider client={queryClient}>
      <NotificationProvider><DeploymentsPage /></NotificationProvider>
    </QueryClientProvider>,
  );
}

// Detection spends the operator's Provider credential, so the console states
// the consequence before the call goes out. It no longer collects a password up
// front: the server remembers a recent proof for a window, so the fields appear
// only if the attempt comes back asking for them. Every test that starts a
// detection goes through the same steps a person does.
async function startDetection(label = "识别能力") {
  fireEvent.click(screen.getByRole("button", { name: label }));
  const dialog = await screen.findByRole("alertdialog");
  expect(within(dialog).queryByLabelText(/^当前密码/)).toBeNull();
  fireEvent.click(within(dialog).getByRole("button", { name: label }));
}

describe("capability grouping stays complete", () => {
  // Which group a capability is drawn in is a judgement this file makes, but the
  // set of capabilities is the server's. A capability the server offers and no
  // group names has nowhere to be drawn: the deployment form would simply never
  // show it, and the operator would have no way to tell it was missing. The
  // subject is the endpoint's real answer, generated by
  // TestProviderProfilesGoldenMatchesConsoleFixture.
  it("draws every capability the server offers, in exactly one group", () => {
    const served = providerProfilesFixture.capability_names.filter(
      (name) => name !== "max_context_tokens" && name !== "max_output_tokens",
    );
    const drawn = deploymentCapabilityGroupsForTest.flatMap((group) => group.capabilities as readonly string[]);

    const missing = served.filter((name) => !drawn.includes(name));
    expect(missing, "capabilities the server offers that no group draws").toEqual([]);

    const unknown = drawn.filter((name) => !served.includes(name));
    expect(unknown, "capabilities drawn that the server does not offer").toEqual([]);

    const duplicated = drawn.filter((name, index) => drawn.indexOf(name) !== index);
    expect(duplicated, "capabilities drawn in more than one group").toEqual([]);
  });
});

describe("time-of-day price schedule editing", () => {
  const window = (start: string, end: string) => ({ start, end, input: "0.4", cachedInput: "0.04", output: "1.6", fixed: "0" });

  it("reads and writes clock times only as whole HH:MM, with 24:00 as the end of the day", () => {
    expect(clockToMinute("09:00")).toBe(540);
    expect(clockToMinute("24:00")).toBe(1440);
    expect(minuteToClock(1080)).toBe("18:00");
    // A wrapping span is two windows, so nothing past the end of the day parses.
    expect(clockToMinute("24:01")).toBeNull();
    expect(clockToMinute("9:00")).toBeNull();
    expect(clockToMinute("09:60")).toBeNull();
    expect(clockToMinute("")).toBeNull();
  });

  it("blocks the table the server would reject, and names which rule failed", () => {
    const valid = { timezone: "Asia/Shanghai", windows: [window("09:00", "12:00"), window("14:00", "18:00")] };
    expect(scheduleDraftProblem(valid)).toBeUndefined();
    // The provider zone has no defensible default, so an empty one blocks.
    expect(scheduleDraftProblem({ ...valid, timezone: "  " })).toBe("timezone");
    expect(scheduleDraftProblem({ ...valid, windows: [] })).toBe("windows");
    expect(scheduleDraftProblem({ ...valid, windows: [window("12:00", "09:00")] })).toBe("time");
    expect(scheduleDraftProblem({ ...valid, windows: [window("09:00", "09:00")] })).toBe("time");
    expect(scheduleDraftProblem({ ...valid, windows: [{ ...window("09:00", "12:00"), output: "-1" }] })).toBe("rate");
    expect(scheduleDraftProblem({ ...valid, windows: [{ ...window("09:00", "12:00"), input: "0", cachedInput: "0", output: "0", fixed: "0" }] })).toBe("rate");
    // Overlap is caught whichever order the rows happen to be in on screen.
    expect(scheduleDraftProblem({ ...valid, windows: [window("09:00", "12:00"), window("11:00", "18:00")] })).toBe("overlap");
    expect(scheduleDraftProblem({ ...valid, windows: [window("14:00", "18:00"), window("09:00", "15:00")] })).toBe("overlap");
    // Touching windows are disjoint: the end is exclusive.
    expect(scheduleDraftProblem({ ...valid, windows: [window("09:00", "12:00"), window("12:00", "18:00")] })).toBeUndefined();
  });
});
