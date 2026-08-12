import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import type { Credential } from "../types";
import { ProvidersPage } from "./ProvidersPage";

const openAICredential: Credential = {
  id: "credential_openai",
  name: "OpenAI production",
  type: "openai",
  access_surface: "openai-api",
  scheme: "bearer.static",
  bound_base_url: "https://api.openai.com:443",
  secret_configured: true,
  key_version: 1,
  revision: 1,
};

describe("ProvidersPage profile and credential bindings", () => {
  beforeEach(() => {
    vi.spyOn(api, "credentials").mockResolvedValue({ items: [openAICredential], next_cursor: "" });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [], next_cursor: "" });
  });

  afterEach(() => {
    vi.restoreAllMocks();
    window.history.replaceState({}, "", "/admin/providers");
  });

  it("uses the tabs as the only repeated resource headings", async () => {
    renderPage();
    expect(await screen.findByRole("tab", { name: "服务商 0" })).toBeVisible();
    expect(screen.queryByRole("heading", { name: "服务商 0" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("tab", { name: "凭据库 1" }));
    expect(screen.queryByRole("heading", { name: "凭据 1" })).not.toBeInTheDocument();
  });

  it("submits the registered OpenAI provider profile instead of the northbound profile", async () => {
    const create = vi.spyOn(api, "createProvider").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 服务商" }));
    fireEvent.change(screen.getByLabelText("服务商名称"), { target: { value: "OpenAI" } });
    fireEvent.click(screen.getByRole("button", { name: "创建并热加载" }));

    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    expect(create.mock.calls[0][0]).toMatchObject({
      type: "openai",
      profile_id: "openai.chat-embeddings.v1",
      credential_id: openAICredential.id,
      bindings: expect.arrayContaining([
        expect.objectContaining({ profile_id: "openai.chat-embeddings.v1", enabled: true }),
        expect.objectContaining({ profile_id: "openai.media-resources.v1", enabled: true }),
      ]),
    });
    expect(create.mock.calls[0][0]).not.toMatchObject({ profile_id: "openai.chat-completions.v1" });
  });

  it("tests every enabled capability binding as one persisted provider snapshot", async () => {
    vi.mocked(api.providers).mockResolvedValue({
      items: [{
        id: "provider_openai", name: "OpenAI", type: "openai", base_url: "https://api.openai.com",
        access_surface: "openai-api", profile_id: "openai.chat-embeddings.v1", capability_evidence: {},
        credential_id: openAICredential.id, capabilities: { chat: true },
        max_concurrency: 0, enabled: true, revision: 1,
        bindings: [
          { id: "binding_chat", profile_id: "openai.chat-embeddings.v1", enabled: true, capabilities: {} },
          { id: "binding_media", profile_id: "openai.media-resources.v1", enabled: true, capabilities: {} },
        ],
      } as never],
      next_cursor: "",
    });
    const testProvider = vi.spyOn(api, "testProvider").mockResolvedValue({
      status: "healthy", latency_ms: 12, tested_at: "2026-08-02T12:00:00Z", revision: 2,
      healthy_targets: 2, total_targets: 2,
    });
    renderPage();

    const boundCredential = await screen.findByRole("button", { name: "OpenAI production →" });
    fireEvent.click(boundCredential);
    expect(window.location.search).toBe("?view=credentials");
    expect(screen.getByText("被 1 个服务商使用", { exact: false })).toBeInTheDocument();
    fireEvent.click(screen.getByRole("tab", { name: /服务商/ }));
    fireEvent.click(await screen.findByRole("button", { name: "测试" }));

    await waitFor(() => expect(testProvider).toHaveBeenCalledOnce());
    expect(testProvider).toHaveBeenCalledWith("provider_openai");
    expect(await screen.findByText("通过 · 12ms")).toBeInTheDocument();
  });

  it("restores a persisted provider test result and marks an older revision stale", async () => {
    vi.mocked(api.providers).mockResolvedValue({
      items: [{
        id: "provider_openai", name: "OpenAI", type: "openai", base_url: "https://api.openai.com",
        access_surface: "openai-api", profile_id: "openai.chat-embeddings.v1", capability_evidence: {},
        credential_id: openAICredential.id, capabilities: { chat: true }, max_concurrency: 0, enabled: true,
        revision: 3, last_test_status: "healthy", last_test_revision: 2, last_test_latency_millis: 18,
        last_tested_at: "2026-08-02T12:00:00Z", last_test_healthy_targets: 1, last_test_total_targets: 1,
      } as never],
      next_cursor: "",
    });
    renderPage();

    const staleResult = await screen.findByText("需重测");
    expect(staleResult).toBeInTheDocument();
    expect(staleResult.closest(".inline-test-control")).toHaveAttribute("title", "1/1 个接口正常 · 18ms");
  });

  it("toggles a provider from the row while preserving its configuration", async () => {
    const provider = {
      id: "provider_toggle", name: "Toggle provider", type: "openai", base_url: "https://api.openai.com",
      access_surface: "openai-api", profile_id: "openai.chat-embeddings.v1", credential_scheme: "bearer.static",
      capability_evidence: {}, credential_id: openAICredential.id, capabilities: { chat: true },
      max_concurrency: 4, enabled: true, revision: 7,
      bindings: [{ id: "binding_chat", profile_id: "openai.chat-embeddings.v1", enabled: true, capabilities: { chat: true } }],
    } as never;
    vi.mocked(api.providers).mockResolvedValue({ items: [provider], next_cursor: "" });
    const update = vi.spyOn(api, "updateProvider").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "禁用" }));
    const dialog = screen.getByRole("alertdialog", { name: "禁用服务商？" });
    expect(dialog).toHaveTextContent("确认禁用服务商“Toggle provider”？依赖该连接的模型部署将无法继续调用上游。");
    expect(update).not.toHaveBeenCalled();
    fireEvent.click(within(dialog).getByRole("button", { name: "禁用" }));
    await waitFor(() => expect(update).toHaveBeenCalledOnce());
    expect(update).toHaveBeenCalledWith("provider_toggle", expect.objectContaining({
      enabled: false,
      credential_id: openAICredential.id,
      profile_id: "openai.chat-embeddings.v1",
      bindings: expect.arrayContaining([expect.objectContaining({ profile_id: "openai.chat-embeddings.v1" })]),
    }), 7);
  });

  it("requires a credential before a provider can be created", async () => {
    vi.mocked(api.credentials).mockResolvedValue({ items: [], next_cursor: "" });
    renderPage();

    const addProvider = await screen.findByRole("button", { name: "＋ 服务商" });
    await waitFor(() => expect(addProvider).toBeDisabled());
    expect(screen.getByText("服务商连接必须绑定一个加密凭据；创建后才能继续配置上游。")).toBeInTheDocument();
  });

  it("waits for credentials before mounting the onboarding provider form", async () => {
    window.history.replaceState({}, "", "/admin/providers?intent=create&onboarding=first-request");
    let resolveCredentials!: (value: Awaited<ReturnType<typeof api.credentials>>) => void;
    vi.mocked(api.credentials).mockImplementation(() => new Promise((resolve) => { resolveCredentials = resolve; }));
    renderPage();

    expect(screen.queryByRole("dialog", { name: "创建服务商" })).not.toBeInTheDocument();
    await act(async () => resolveCredentials({ items: [openAICredential], next_cursor: "" }));

    const dialog = await screen.findByRole("dialog", { name: "创建服务商" });
    expect(within(dialog).getByLabelText("加密凭据")).toHaveValue(openAICredential.id);
    expect(within(dialog).getByRole("button", { name: "创建并热加载" })).toBeEnabled();
  });

  // Whether deployments may use this upstream is the state the save commits, so
  // it belongs in the bar that commits it, saying what it will do. A stylesheet
  // regression that breaks this structure is invisible to a typecheck.
  it("states the upstream's availability in the save bar rather than as a bare checkbox", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 服务商" }));

    const toggle = await screen.findByLabelText("启用服务商");
    const bar = toggle.closest(".sticky-form-actions");
    expect(bar).not.toBeNull();
    expect(bar).toContainElement(screen.getByRole("button", { name: "创建并热加载" }));
    expect(bar).toContainElement(screen.getByRole("button", { name: "取消" }));
    expect(within(bar as HTMLElement).getByText("启用服务商 · 启用")).toBeVisible();
    expect(within(bar as HTMLElement).getByText("启用后，模型部署可以使用这个上游连接。")).toBeVisible();

    // Turning it off has to change what the bar says it will do, not just the box.
    fireEvent.click(toggle);
    expect(within(bar as HTMLElement).getByText("启用服务商 · 禁用")).toBeVisible();
    expect(within(bar as HTMLElement).getByText("模型部署无法使用这个上游连接")).toBeVisible();
  });

  it("restores the active resource tab from the URL and exposes its contextual action", async () => {
    window.history.replaceState({}, "", "/admin/providers?view=credentials");
    renderPage();

    expect(await screen.findByRole("tab", { name: /凭据库/ })).toHaveAttribute("aria-selected", "true");
    expect(screen.getByRole("button", { name: "＋ 凭据" })).toBeInTheDocument();
    expect(screen.queryByRole("button", { name: "＋ 服务商" })).not.toBeInTheDocument();
  });

  it("keeps provider and credential metadata compact, searchable, and expandable", async () => {
    vi.mocked(api.providers).mockResolvedValue({
      items: [{
        id: "provider_compact", name: "Compact upstream", type: "openai", base_url: "https://api.openai.com",
        access_surface: "openai-api", profile_id: "openai.chat-embeddings.v1", capability_evidence: {},
        credential_id: openAICredential.id, capabilities: { chat: true }, max_concurrency: 4, enabled: true, revision: 1,
      } as never], next_cursor: "",
    });
    renderPage();

    expect(await screen.findByText("显示 1 / 1 项")).toBeVisible();
    expect(screen.queryByText("能力接口")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "查看详情" }));
    expect(screen.getByText("能力接口")).toBeVisible();
    fireEvent.change(screen.getByPlaceholderText("搜索名称、类型或 API 地址"), { target: { value: "missing" } });
    expect(screen.getByText("没有匹配结果")).toBeVisible();

    fireEvent.click(screen.getByRole("tab", { name: /凭据库/ }));
    expect(screen.getAllByText("https://api.openai.com").some((element) => element.textContent === "https://api.openai.com")).toBe(true);
    expect(screen.queryByText("https://api.openai.com:443")).not.toBeInTheDocument();
    expect(screen.queryByText("凭据方案")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "查看详情" }));
    expect(screen.getByText("凭据方案")).toBeVisible();
    expect(screen.getByText("规范化绑定端点")).toBeVisible();
    expect(screen.getByText("https://api.openai.com:443")).toBeVisible();
  });

  it("keeps non-default ports visible in credential rows", async () => {
    vi.mocked(api.credentials).mockResolvedValue({
      items: [{ ...openAICredential, id: "credential_custom_port", bound_base_url: "https://gateway.example:8443" }],
      next_cursor: "",
    });
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: /凭据库/ }));
    expect(screen.getAllByText("https://gateway.example:8443").length).toBeGreaterThan(0);
  });

  it("keeps dependent chat capabilities consistent in the provider form", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 服务商" }));

    const chat = screen.getByRole("checkbox", { name: "对话" });
    const streaming = screen.getByRole("checkbox", { name: "流式" });
    expect(chat).toBeChecked();
    expect(streaming).toBeChecked();

    fireEvent.click(chat);
    expect(chat).not.toBeChecked();
    expect(streaming).not.toBeChecked();

    fireEvent.click(streaming);
    expect(streaming).toBeChecked();
    expect(chat).toBeChecked();
  });

  it("creates an isolated Bedrock Agent Runtime credential for Cohere Rerank", async () => {
    const create = vi.spyOn(api, "createCredential").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: /凭据库/ }));
    fireEvent.click(await screen.findByRole("button", { name: "＋ 凭据" }));
    fireEvent.change(screen.getByLabelText("凭据名称"), { target: { value: "Rerank credential" } });
    fireEvent.change(screen.getByLabelText("服务商类型"), { target: { value: "bedrock" } });
    fireEvent.change(await screen.findByRole("combobox", { name: /^Bedrock 访问面/ }), { target: { value: "bedrock-agent-runtime" } });
    fireEvent.change(await screen.findByLabelText(/^AWS 凭据 JSON/), { target: { value: "{\"access_key_id\":\"test\"}" } });
    fireEvent.click(screen.getByRole("button", { name: "加密保存" }));

    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    expect(create.mock.calls[0][0]).toMatchObject({
      type: "bedrock",
      access_surface: "bedrock-agent-runtime",
      scheme: "aws.sigv4.explicit-session",
      base_url: "https://bedrock-agent-runtime.us-east-1.amazonaws.com",
    });
  });

  it.each([
    {
      name: "Agent Runtime",
      surface: "bedrock-agent-runtime" as const,
      scheme: "aws.sigv4.explicit-session" as const,
      boundBaseURL: "https://bedrock-agent-runtime.eu-west-1.amazonaws.com:443",
    },
    {
      name: "Mantle",
      surface: "bedrock-mantle" as const,
      scheme: "aws.bedrock.api-key" as const,
      boundBaseURL: "https://bedrock-mantle.ap-southeast-1.api.aws:443",
    },
  ])("hides the default port while rotating a Bedrock $name credential", async ({ name, surface, scheme, boundBaseURL }) => {
    const credential: Credential = {
      id: `credential_${name}`,
      name,
      type: "bedrock",
      access_surface: surface,
      scheme,
      bound_base_url: boundBaseURL,
      secret_configured: true,
      key_version: 1,
      revision: 1,
    };
    vi.mocked(api.credentials).mockResolvedValue({ items: [credential], next_cursor: "" });
    const rotate = vi.spyOn(api, "rotateCredential").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: /凭据库/ }));
    fireEvent.click(await screen.findByRole("button", { name: "轮换" }));
    expect(screen.getByLabelText("服务商类型")).toBeDisabled();
    const displayBaseURL = boundBaseURL.replace(/:443$/, "");
    expect(screen.getByLabelText(/^地址绑定/)).toHaveValue(displayBaseURL);
    fireEvent.change(screen.getByLabelText(/^新密钥/), { target: { value: "rotated-secret" } });
    // Replacing credential material asks who is doing it, like deleting it does.
    fireEvent.change(screen.getByLabelText("当前密码"), { target: { value: "a passphrase" } });
    fireEvent.click(screen.getByRole("button", { name: "安全轮换" }));

    await waitFor(() => expect(rotate).toHaveBeenCalledOnce());
    expect(rotate.mock.calls[0]).toEqual([
      credential.id,
      expect.objectContaining({
        type: "bedrock",
        base_url: displayBaseURL,
        access_surface: surface,
        scheme,
        secret: "rotated-secret",
      }),
      credential.revision,
      expect.objectContaining({ currentPassword: "a passphrase" }),
    ]);
  });

  // The Bedrock Mantle profiles are Beta and their capability set is fixed by
  // the build. The form used to offer the capability checkboxes for them, so an
  // operator could tick embeddings or reasoning on a profile that cannot serve
  // them and have the request accepted. The backend now refuses that; the form
  // must not present it as a choice in the first place.
  it.each([
    "bedrock.mantle.openai.chat.v1",
    "bedrock.mantle.openai.responses.v1",
    "bedrock.mantle.anthropic.messages.v1",
  ])("presents %s capabilities as fixed rather than selectable", async (profile) => {
    const mantleCredential: Credential = {
      id: "credential_mantle",
      name: "Mantle",
      type: "bedrock",
      access_surface: "bedrock-mantle",
      scheme: "aws.bedrock.api-key",
      bound_base_url: "https://bedrock-mantle.us-east-1.api.aws:443",
      secret_configured: true,
      key_version: 1,
      revision: 1,
    };
    vi.mocked(api.credentials).mockResolvedValue({ items: [mantleCredential], next_cursor: "" });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 服务商" }));
    fireEvent.change(screen.getByLabelText("类型"), { target: { value: "bedrock" } });
    fireEvent.change(await screen.findByRole("combobox", { name: /^能力实现/ }), { target: { value: profile } });

    expect(screen.getByText("该能力实现使用固定协议，无需额外配置。")).toBeVisible();
    expect(screen.queryByRole("checkbox", { name: "对话" })).not.toBeInTheDocument();
    expect(screen.queryByRole("checkbox", { name: "向量嵌入" })).not.toBeInTheDocument();
  });

  // The field shipped with its Chinese label set to the English string, so it
  // rendered in a Latin face beside every other label in the form and read as a
  // different component. Pinned by the label the operator actually sees.
  it("labels the Bedrock project field in the active locale", async () => {
    const mantleCredential: Credential = {
      id: "credential_mantle",
      name: "Mantle",
      type: "bedrock",
      access_surface: "bedrock-mantle",
      scheme: "aws.bedrock.api-key",
      bound_base_url: "https://bedrock-mantle.us-east-1.api.aws:443",
      secret_configured: true,
      key_version: 1,
      revision: 1,
    };
    vi.mocked(api.credentials).mockResolvedValue({ items: [mantleCredential], next_cursor: "" });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 服务商" }));
    fireEvent.change(screen.getByLabelText("类型"), { target: { value: "bedrock" } });
    fireEvent.change(await screen.findByRole("combobox", { name: /^能力实现/ }), { target: { value: "bedrock.mantle.openai.chat.v1" } });

    // The hint lives inside the same label element, so the accessible name is
    // label plus hint; anchor on the label itself.
    const project = screen.getByLabelText(/^Bedrock 项目（Project ID）/);
    expect(project).toBeVisible();
    expect(project).toHaveAttribute("placeholder", "默认项目");
    // Same wrapper and class as every other field in this form, so the label,
    // input and hint inherit one set of rules.
    expect(project.closest("label")).toHaveClass("field");
    expect(project.closest("label")?.parentElement).toBe(
      screen.getByLabelText(/基础地址/).closest("label")?.parentElement,
    );
  });

  // Testing an Anthropic Messages provider issues a real inference call, while
  // the other two Mantle implementations read model metadata. An operator
  // cannot tell those apart from the button, so the form says so.
  it("warns that connection tests are billable only on the Messages implementation", async () => {
    const mantleCredential: Credential = {
      id: "credential_mantle",
      name: "Mantle",
      type: "bedrock",
      access_surface: "bedrock-mantle",
      scheme: "aws.bedrock.api-key",
      bound_base_url: "https://bedrock-mantle.us-east-1.api.aws:443",
      secret_configured: true,
      key_version: 1,
      revision: 1,
    };
    vi.mocked(api.credentials).mockResolvedValue({ items: [mantleCredential], next_cursor: "" });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 服务商" }));
    fireEvent.change(screen.getByLabelText("类型"), { target: { value: "bedrock" } });
    const implementation = await screen.findByRole("combobox", { name: /^能力实现/ });

    fireEvent.change(implementation, { target: { value: "bedrock.mantle.anthropic.messages.v1" } });
    expect(screen.getByText("该实现的连接测试会产生费用")).toBeVisible();

    fireEvent.change(implementation, { target: { value: "bedrock.mantle.openai.chat.v1" } });
    expect(screen.queryByText("该实现的连接测试会产生费用")).not.toBeInTheDocument();
  });

  // Converse text stays operator-declared. If the fixed list grew to cover it,
  // the check above would still pass while silently removing a real choice.
  it("keeps Bedrock Converse capabilities selectable", async () => {
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 服务商" }));
    fireEvent.change(screen.getByLabelText("类型"), { target: { value: "bedrock" } });

    expect(await screen.findByRole("checkbox", { name: "对话" })).toBeInTheDocument();
  });
});

function renderPage() {
  const client = new QueryClient({
    defaultOptions: { queries: { retry: false }, mutations: { retry: false } },
  });
  return render(
    <QueryClientProvider client={client}>
      <ProvidersPage />
    </QueryClientProvider>,
  );
}
