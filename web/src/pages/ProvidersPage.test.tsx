import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "../api";
import { stepUpRequired } from "../test/fixtures";
import type { Credential } from "../types";
import { NotificationProvider } from "../notifications";
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
  // jsdom has no scrollIntoView, and several tests here assert the form brings a
  // rejection into view. Stubbed once and restored once: a stub left on the
  // prototype, or a pinned clock left running, leaks into every later test in
  // the file.
  const realScrollIntoView = Element.prototype.scrollIntoView;

  beforeEach(() => {
    vi.spyOn(api, "credentials").mockResolvedValue({ items: [openAICredential], next_cursor: "" });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [], next_cursor: "" });
  });

  afterEach(() => {
    vi.restoreAllMocks();
    vi.useRealTimers();
    Element.prototype.scrollIntoView = realScrollIntoView;
    window.history.replaceState({}, "", "/admin/providers");
  });

  it("uses the tabs as the only repeated resource headings", async () => {
    renderPage();
    expect(await screen.findByRole("tab", { name: "服务商 0" })).toBeVisible();
    expect(screen.queryByRole("heading", { name: "服务商 0" })).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("tab", { name: "凭据库 1" }));
    expect(screen.queryByRole("heading", { name: "凭据 1" })).not.toBeInTheDocument();
  });

  // What the form submits is one flat capability set and nothing about profiles.
  // An OpenAI connection spans two of them, and which capability each one serves
  // is the server's answer — this form knowing it is what let the two drift.
  it("submits one flat capability set and no profile split", async () => {
    const create = vi.spyOn(api, "createProvider").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 服务商" }));
    fireEvent.change(screen.getByLabelText("服务商名称"), { target: { value: "OpenAI" } });
    // Offered and left off: the media capabilities belong to a profile this
    // credential can also reach, and turning one on is the operator's move.
    expect(screen.getByRole("checkbox", { name: /图像/ })).not.toBeChecked();
    fireEvent.click(screen.getByRole("button", { name: "创建并热加载" }));

    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    const submitted = create.mock.calls[0][0] as Record<string, unknown>;
    expect(submitted).toMatchObject({ type: "openai", credential_id: openAICredential.id });
    expect(submitted.bindings).toBeUndefined();
    expect(submitted.profile_id).toBeUndefined();
    // A new connection starts with what the profile it lands on declares, not
    // with the union across the credential's profiles: the media capabilities
    // are offered and left off, so nothing is advertised to the router that the
    // operator did not choose.
    expect(submitted.capabilities).toMatchObject({ chat: true, embeddings: true, images: false });
    // The numeric bounds belong to the profile that serves the capability, so
    // the form neither shows nor sends them.
    expect(submitted.capabilities).toMatchObject({ max_context_tokens: 0, max_output_tokens: 0 });
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

    const boundCredential = await screen.findByRole("button", { name: "OpenAI production" });
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

  // A failed test used to be a red word with nothing behind it: the console
  // showed "失败", the response carried the class and the upstream's own reply,
  // and neither reached the operator.
  it("says why a provider connection test failed", async () => {
    const failing = {
      id: "provider_openai", name: "OpenAI", type: "openai", base_url: "https://api.openai.com",
      access_surface: "openai-api", profile_id: "openai.chat-embeddings.v1", capability_evidence: {},
      credential_id: openAICredential.id, capabilities: { chat: true }, max_concurrency: 0, enabled: true,
      revision: 2,
    } as never;
    vi.mocked(api.providers).mockResolvedValue({ items: [failing], next_cursor: "" });
    vi.spyOn(api, "testProvider").mockRejectedValue(new ApiError(502, "request failed (502)", "", "", {
      status: "unhealthy", error_class: "authentication", provider_status: 403,
      provider_code: "AccessDeniedException", error_detail: "provider error (403): not authorized to call this project",
    }));
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "测试" }));

    const reason = await screen.findByText(/上游拒绝了这份凭据/);
    expect(reason).toHaveTextContent("HTTP 403 · AccessDeniedException · provider error (403): not authorized to call this project");
  });

  // A bad_request Halro produced itself carries no upstream status, and calling
  // it an upstream rejection sends the operator to audit a credential and a
  // network that were never involved.
  it("distinguishes a refusal Halro made from one the upstream made", async () => {
    const failing = {
      id: "provider_anthropic", name: "Anthropic", type: "anthropic", base_url: "https://api.anthropic.com",
      access_surface: "anthropic-api", profile_id: "anthropic.messages.2023-06-01", capability_evidence: {},
      credential_id: openAICredential.id, capabilities: { chat: true }, max_concurrency: 0, enabled: true,
      revision: 2,
    } as never;
    vi.mocked(api.providers).mockResolvedValue({ items: [failing], next_cursor: "" });
    vi.spyOn(api, "testProvider").mockRejectedValue(new ApiError(502, "request failed (502)", "", "", {
      status: "unhealthy", error_class: "bad_request",
      error_detail: "this profile has no model catalog to test against; bind an enabled deployment and test that",
    }));
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "测试" }));

    expect(await screen.findByText(/Halro 在发往上游之前拒绝了这次探测/)).toBeVisible();
    expect(screen.queryByText(/上游拒绝了这次探测请求/)).toBeNull();
  });

  // A refusal Halro made before probing answers 400 with a plain message and no
  // class. That message used to be dropped, and the stale class the store still
  // remembered from an earlier test was shown in its place — the operator read a
  // sentence about the upstream for a request that never left the process.
  it("shows the message behind a refusal that never reached the upstream", async () => {
    const failing = {
      id: "provider_anthropic", name: "Anthropic", type: "anthropic", base_url: "https://api.anthropic.com",
      access_surface: "anthropic-api", profile_id: "anthropic.messages.2023-06-01", capability_evidence: {},
      credential_id: openAICredential.id, capabilities: { chat: true }, max_concurrency: 0, enabled: true,
      revision: 4, last_test_status: "unhealthy", last_test_revision: 4, last_test_error_class: "authentication",
      last_tested_at: "2026-08-13T12:00:00Z",
    } as never;
    vi.mocked(api.providers).mockResolvedValue({ items: [failing], next_cursor: "" });
    vi.spyOn(api, "testProvider").mockRejectedValue(
      new ApiError(400, "request failed (400)", "", "", { error: "provider binding adapter is unavailable" }),
    );
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "测试" }));

    const reason = await screen.findByText(/provider binding adapter is unavailable/);
    expect(reason).toHaveTextContent("Halro 在发往上游之前拒绝了这次探测");
    expect(reason).not.toHaveTextContent("上游拒绝了这份凭据");
  });

  // The class is all the store keeps, so a reload still explains the failure
  // even though the upstream's sentence is long gone.
  it("keeps explaining a persisted test failure after a reload", async () => {
    vi.mocked(api.providers).mockResolvedValue({
      items: [{
        id: "provider_openai", name: "OpenAI", type: "openai", base_url: "https://api.openai.com",
        access_surface: "openai-api", profile_id: "openai.chat-embeddings.v1", capability_evidence: {},
        credential_id: openAICredential.id, capabilities: { chat: true }, max_concurrency: 0, enabled: true,
        revision: 3, last_test_status: "unhealthy", last_test_revision: 3, last_test_error_class: "connect",
        last_tested_at: "2026-08-02T12:00:00Z",
      } as never],
      next_cursor: "",
    });
    renderPage();

    expect(await screen.findByText(/无法建立到上游的连接/)).toBeVisible();
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
      capabilities: expect.objectContaining({ chat: true }),
    }), 7);
    // Nothing about the profile split travels back: the server refuses a
    // bindings array, so sending one would turn every row toggle into a 400.
    expect((update.mock.calls[0][1] as Record<string, unknown>).bindings).toBeUndefined();
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
    expect(within(bar as HTMLElement).getByText("启用服务商 · 已启用")).toBeVisible();
    expect(within(bar as HTMLElement).getByText("启用后，模型部署可以使用这个上游连接。")).toBeVisible();

    // Turning it off has to change what the bar says it will do, not just the box.
    fireEvent.click(toggle);
    expect(within(bar as HTMLElement).getByText("启用服务商 · 已禁用")).toBeVisible();
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

  // Some upstream keys have a stated lifetime — a Bedrock API key, an STS
  // session, a provider rotation policy — and the first sign used to be a 401 in
  // production. The date is optional and advisory; what it buys is the rotation
  // being visible before it happens.
  it("records an optional credential expiry and counts down to it in the row", async () => {
    const create = vi.spyOn(api, "createCredential").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: /凭据库/ }));
    fireEvent.click(await screen.findByRole("button", { name: "＋ 凭据" }));
    fireEvent.change(screen.getByLabelText("凭据名称"), { target: { value: "OpenAI" } });
    fireEvent.change(await screen.findByLabelText(/^服务商密钥/), { target: { value: "sk-test" } });
    // Optional: nothing typed in the expiry, and the save still goes through
    // with an explicit "no declared end" rather than a silent omission.
    fireEvent.click(screen.getByRole("button", { name: "加密保存" }));

    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    expect(create.mock.calls[0][0]).toMatchObject({ expires_at: null });

    fireEvent.click(await screen.findByRole("button", { name: "＋ 凭据" }));
    fireEvent.change(screen.getByLabelText("凭据名称"), { target: { value: "Bedrock API key" } });
    fireEvent.change(await screen.findByLabelText(/^服务商密钥/), { target: { value: "sk-test" } });
    fireEvent.change(screen.getByLabelText(/^到期时间/), { target: { value: "2027-03-01T12:00" } });
    fireEvent.click(screen.getByRole("button", { name: "加密保存" }));

    await waitFor(() => expect(create).toHaveBeenCalledTimes(2));
    // Read in the accounting zone (UTC by default here), not the browser's, so
    // the instant sent is the one the row renders back.
    const sent = create.mock.calls[1][0] as { expires_at: string };
    expect(sent.expires_at).toBe("2027-03-01T12:00:00.000Z");
  });

  it("says how long a credential has left, and when it is already gone", async () => {
    vi.setSystemTime(new Date("2026-08-12T00:00:00Z"));
    vi.mocked(api.credentials).mockResolvedValue({
      items: [
        { ...openAICredential, id: "credential_soon", name: "Expiring", expires_at: "2026-08-20T00:00:00Z" },
        { ...openAICredential, id: "credential_gone", name: "Gone", expires_at: "2026-07-01T00:00:00Z" },
        { ...openAICredential, id: "credential_open", name: "Open ended" },
      ],
      next_cursor: "",
    });
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: /凭据库/ }));
    expect(screen.getByText(/^8 天后到期/)).toBeVisible();
    expect(screen.getByText(/^已于 .* 过期/)).toBeVisible();
    // A secret with no declared end says nothing in the row rather than
    // claiming something about a date that was never given.
    const rows = screen.getAllByText("Open ended")[0].closest(".credential-row");
    expect(rows?.querySelector(".credential-expiry")).toBeNull();
    // The full instant stays available where the rest of the technical detail is.
    fireEvent.click(within(rows as HTMLElement).getByRole("button", { name: "查看详情" }));
    expect(within(rows as HTMLElement).getByText("未设置到期时间")).toBeVisible();
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
    const rotate = vi.spyOn(api, "rotateCredential")
      .mockRejectedValueOnce(stepUpRequired())
      .mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("tab", { name: /凭据库/ }));
    fireEvent.click(await screen.findByRole("button", { name: "轮换" }));
    expect(screen.getByLabelText("服务商类型")).toBeDisabled();
    const displayBaseURL = boundBaseURL.replace(/:443$/, "");
    expect(screen.getByLabelText(/^地址绑定/)).toHaveValue(displayBaseURL);
    fireEvent.change(screen.getByLabelText(/^新密钥/), { target: { value: "rotated-secret" } });
    // Replacing credential material asks who is doing it, like deleting it does
    // — once the re-authentication window has closed, which the first attempt
    // finds out.
    fireEvent.click(screen.getByRole("button", { name: "安全轮换" }));
    fireEvent.change(await screen.findByLabelText(/^当前密码/), { target: { value: "a passphrase" } });
    fireEvent.click(screen.getByRole("button", { name: "安全轮换" }));

    await waitFor(() => expect(rotate).toHaveBeenCalledTimes(2));
    expect(rotate.mock.calls[1]).toEqual([
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
    "bedrock.mantle.chat.v1",
    "bedrock.mantle.openai.chat.v1",
    "bedrock.mantle.responses.v1",
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

  // A rejected save used to look like a dead button: the notice rendered at the
  // bottom of a form that scrolls behind a sticky footer, so the operator saw
  // nothing happen and clicked again. The failure now comes to them, carries
  // the reason in their own language, and takes focus so it is announced.
  it("brings a rejected provider save into view and explains it", async () => {
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
    // A value the form itself accepts, so what is under test is the server's
    // refusal travelling back to the operator rather than the local check.
    vi.spyOn(api, "createProvider").mockRejectedValue(
      new ApiError(400, "bedrock project id must be `proj_` followed by alphanumerics", "bedrock_project_id_invalid"),
    );
    const scrolled: unknown[] = [];
    Element.prototype.scrollIntoView = function scrollIntoView(this: Element, options?: unknown) {
      scrolled.push(this);
      void options;
    };
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 服务商" }));
    fireEvent.change(screen.getByLabelText("服务商名称"), { target: { value: "AwsBedrockMantle" } });
    fireEvent.change(screen.getByLabelText("类型"), { target: { value: "bedrock" } });
    fireEvent.change(await screen.findByRole("combobox", { name: /^能力实现/ }), { target: { value: "bedrock.mantle.openai.chat.v1" } });
    fireEvent.change(screen.getByLabelText(/^Bedrock 项目/), { target: { value: "proj_wahool1" } });
    fireEvent.click(screen.getByRole("button", { name: "创建并热加载" }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("这里要填 AWS 项目的 id（proj_ 开头），不是项目名称");
    const container = alert.closest(".form-submit-error");
    expect(container).not.toBeNull();
    await waitFor(() => expect(scrolled).toContain(container));
    await waitFor(() => expect(document.activeElement).toBe(container));
  });

  // A missing name and a malformed project ID both used to end at a submit
  // handler that returned without doing anything: no request, no message, and a
  // button that looked broken. Both now name the field that stopped the save.
  it("names the fields that stop a provider save instead of doing nothing", async () => {
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
    const create = vi.spyOn(api, "createProvider").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 服务商" }));
    fireEvent.change(screen.getByLabelText("类型"), { target: { value: "bedrock" } });
    fireEvent.change(await screen.findByRole("combobox", { name: /^能力实现/ }), { target: { value: "bedrock.mantle.openai.chat.v1" } });
    fireEvent.change(screen.getByLabelText(/^Bedrock 项目/), { target: { value: "5amaxg" } });
    fireEvent.click(screen.getByRole("button", { name: "创建并热加载" }));

    expect(create).not.toHaveBeenCalled();
    expect(await screen.findByText(/必填；模型部署选择上游连接时看到的就是这个名称/)).toBeVisible();
    expect(screen.getByText(/必须是 AWS 签发的 proj_ 开头加字母数字的 ID/)).toBeVisible();
    expect(screen.getByLabelText(/^服务商名称/)).toHaveAttribute("aria-invalid", "true");
    // The reason sits on the field, so the field is what the form scrolls to
    // and focuses; no second summary line repeats it near the footer.
    await waitFor(() => expect(document.activeElement).toBe(screen.getByLabelText(/^服务商名称/)));
    expect(document.querySelector(".form-submit-error")).toBeNull();

    // A workspace identifier from the neighbouring product is the likeliest
    // paste, so it is refused by name rather than as a generic format error.
    fireEvent.change(screen.getByLabelText(/^Bedrock 项目/), { target: { value: "wrkspc_abc123" } });
    fireEvent.change(screen.getByLabelText(/^服务商名称/), { target: { value: "AwsBedrockMantle" } });
    fireEvent.click(screen.getByRole("button", { name: "创建并热加载" }));

    expect(create).not.toHaveBeenCalled();
    expect(await screen.findByText(/Claude Platform on AWS 的工作区标识/)).toBeVisible();

    // `default` is AWS's name for the account default project, which is what an
    // empty value already means, so it is normalised away rather than sent.
    fireEvent.change(screen.getByLabelText(/^Bedrock 项目/), { target: { value: " default " } });
    fireEvent.click(screen.getByRole("button", { name: "创建并热加载" }));

    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    expect(create.mock.calls[0][0]).toMatchObject({ name: "AwsBedrockMantle", bedrock_project_id: "" });
    // The save closed its own modal, so its confirmation has no anchor left on
    // the page and belongs in the notification column.
    expect(await screen.findByText("服务商已创建并热加载")).toBeVisible();
  });

  // Every field that failed keeps its message, and the first one takes focus.
  // Clearing one of them is a keystroke in that field: re-running the focus move
  // on it sent the caret to the next still-invalid control, so the rest of what
  // was being typed landed in the wrong field.
  it("does not move focus out of the field being corrected", async () => {
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
    Element.prototype.scrollIntoView = function scrollIntoView() {};
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 服务商" }));
    fireEvent.change(screen.getByLabelText("类型"), { target: { value: "bedrock" } });
    fireEvent.change(await screen.findByRole("combobox", { name: /^能力实现/ }), { target: { value: "bedrock.mantle.openai.chat.v1" } });
    // Two refusals at once: an empty name and a malformed project id.
    fireEvent.change(screen.getByLabelText(/^Bedrock 项目/), { target: { value: "5amaxg" } });
    fireEvent.click(screen.getByRole("button", { name: "创建并热加载" }));

    // The move is scheduled on an animation frame, so each step waits one out —
    // otherwise the assertion reads the caret before anything could move it.
    const frame = () => act(async () => { await new Promise((resolve) => requestAnimationFrame(() => resolve(null))); });
    const nameField = screen.getByLabelText(/^服务商名称/);
    await frame();
    expect(document.activeElement).toBe(nameField);

    fireEvent.change(nameField, { target: { value: "A" } });
    await waitFor(() => expect(screen.queryByText(/必填；模型部署选择上游连接时看到的就是这个名称/)).toBeNull());
    await frame();
    expect(document.activeElement).toBe(nameField);
    // The other field keeps its own message; it just did not steal the caret.
    expect(screen.getByText(/必须是 AWS 签发的 proj_ 开头加字母数字的 ID/)).toBeVisible();
  });

  // A provider with no capability can carry no deployment. The save button used
  // to be disabled, which said nothing about why; the refusal now has to say it,
  // and it is the one refusal with no field of its own to carry it.
  it("refuses a provider with every capability switched off and says so", async () => {
    const create = vi.spyOn(api, "createProvider").mockResolvedValue({} as never);
    const scrolled: unknown[] = [];
    Element.prototype.scrollIntoView = function scrollIntoView(this: Element) {
      scrolled.push(this);
    };
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 服务商" }));
    fireEvent.change(screen.getByLabelText("服务商名称"), { target: { value: "OpenAI" } });
    for (const capability of screen.getAllByRole("checkbox")) {
      if ((capability as HTMLInputElement).checked && capability.getAttribute("aria-label") !== "启用") {
        fireEvent.click(capability);
      }
    }
    fireEvent.click(screen.getByRole("button", { name: "创建并热加载" }));

    expect(create).not.toHaveBeenCalled();
    const alert = await screen.findByText(/至少启用一项能力/);
    const container = alert.closest(".form-submit-error");
    expect(container).not.toBeNull();
    await waitFor(() => expect(scrolled).toContain(container));
    await waitFor(() => expect(document.activeElement).toBe(container));
  });

  // errors.anthropicBetas was a dead key: nothing set it, so an illegal token
  // reached the operator as a generic banner with no field highlighted.
  it("names the anthropic-beta field when a token is malformed", async () => {
    const anthropicCredential: Credential = {
      id: "credential_anthropic",
      name: "Anthropic production",
      type: "anthropic",
      access_surface: "anthropic-api",
      scheme: "anthropic.x-api-key",
      bound_base_url: "https://api.anthropic.com:443",
      secret_configured: true,
      key_version: 1,
      revision: 1,
    };
    vi.mocked(api.credentials).mockResolvedValue({ items: [anthropicCredential], next_cursor: "" });
    const create = vi.spyOn(api, "createProvider").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 服务商" }));
    fireEvent.change(screen.getByLabelText("服务商名称"), { target: { value: "Anthropic" } });
    fireEvent.change(screen.getByLabelText("类型"), { target: { value: "anthropic" } });
    const betas = await screen.findByLabelText(/^Anthropic Beta 允许列表/);
    fireEvent.change(betas, { target: { value: "Context-Management-2025-06-27" } });
    fireEvent.click(screen.getByRole("button", { name: "创建并热加载" }));

    expect(create).not.toHaveBeenCalled();
    expect(await screen.findByText(/只能包含小写字母/)).toBeVisible();
    expect(betas).toHaveAttribute("aria-invalid", "true");
  });

  // Upstream egress is a decision the operator makes per connection, so the
  // capability has to be offered — and offered switched off.
  it("offers provider-executed tools on an Anthropic connection without enabling it", async () => {
    const anthropicCredential: Credential = {
      id: "credential_anthropic",
      name: "Anthropic production",
      type: "anthropic",
      access_surface: "anthropic-api",
      scheme: "anthropic.x-api-key",
      bound_base_url: "https://api.anthropic.com:443",
      secret_configured: true,
      key_version: 1,
      revision: 1,
    };
    vi.mocked(api.credentials).mockResolvedValue({ items: [anthropicCredential], next_cursor: "" });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 服务商" }));
    fireEvent.change(screen.getByLabelText("类型"), { target: { value: "anthropic" } });
    const egress = await screen.findByRole("checkbox", { name: /服务商侧执行工具/ });
    expect(egress).not.toBeChecked();
    expect(egress).not.toBeDisabled();
  });

  // Every other capability decides what Halro relays; this one decides who else
  // gets to make requests, and the traffic it admits never passes through
  // Halro's transport. A checkbox row conveys none of that, so the consequence
  // is stated where it is accepted — in what it means, not what it is called.
  it("states what provider-executed tools admits, at the moment it is turned on", async () => {
    const anthropicCredential: Credential = {
      id: "credential_anthropic",
      name: "Anthropic production",
      type: "anthropic",
      access_surface: "anthropic-api",
      scheme: "anthropic.x-api-key",
      bound_base_url: "https://api.anthropic.com:443",
      secret_configured: true,
      key_version: 1,
      revision: 1,
    };
    vi.mocked(api.credentials).mockResolvedValue({ items: [anthropicCredential], next_cursor: "" });
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 服务商" }));
    fireEvent.change(screen.getByLabelText("类型"), { target: { value: "anthropic" } });
    const egress = await screen.findByRole("checkbox", { name: /服务商侧执行工具/ });
    expect(screen.queryByText(/这部分流量不经过 Halro/)).not.toBeInTheDocument();

    fireEvent.click(egress);

    expect(await screen.findByText(/这部分流量不经过 Halro/)).toBeVisible();
    // Named in the reader's language, not as the implementation spells it.
    expect(screen.queryByText(/provider_executed_tools/)).not.toBeInTheDocument();
  });

  // Without the matrix the forms cannot open at all, which makes a failed fetch
  // the difference between editing connections and not. Reloading the page is
  // not an answer an operator should have to find on their own.
  it("offers a retry when the capability matrix cannot be read", async () => {
    vi.mocked(api.providerProfiles).mockRejectedValueOnce(new ApiError(503, "request failed (503)"));
    renderPage();

    const retry = await screen.findByRole("button", { name: "重试" });
    vi.mocked(api.providerProfiles).mockClear();
    fireEvent.click(retry);

    await waitFor(() => expect(api.providerProfiles).toHaveBeenCalled());
    await waitFor(() => expect(screen.queryByRole("button", { name: "重试" })).not.toBeInTheDocument());
    // And the connection form opens again once it arrives.
    fireEvent.click(await screen.findByRole("button", { name: "＋ 服务商" }));
    expect(await screen.findByLabelText("服务商名称")).toBeVisible();
  });

  // A credential is encrypted against the endpoint it was saved for, so a base
  // URL edited afterwards invalidates the pairing. The refusal used to reach the
  // operator as "credential type or audience does not match provider", which
  // names neither URL and does not say which of the two to change.
  it("refuses a credential sealed to a different base URL and names both endpoints", async () => {
    const mantleCredential: Credential = {
      id: "credential_mantle",
      name: "AWS-EAST2-365",
      type: "bedrock",
      access_surface: "bedrock-mantle",
      scheme: "aws.bedrock.api-key",
      bound_base_url: "https://bedrock-mantle.us-east-1.api.aws:443",
      secret_configured: true,
      key_version: 1,
      revision: 1,
    };
    vi.mocked(api.credentials).mockResolvedValue({ items: [mantleCredential], next_cursor: "" });
    const create = vi.spyOn(api, "createProvider").mockResolvedValue({} as never);
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 服务商" }));
    fireEvent.change(screen.getByLabelText("服务商名称"), { target: { value: "AwsBedrockMantle" } });
    fireEvent.change(screen.getByLabelText("类型"), { target: { value: "bedrock" } });
    fireEvent.change(await screen.findByRole("combobox", { name: /^能力实现/ }), { target: { value: "bedrock.mantle.openai.chat.v1" } });
    // The endpoint the credential is sealed to is in the option itself, so the
    // choice can be made without leaving the form.
    expect(screen.getByRole("option", { name: "AWS-EAST2-365 · https://bedrock-mantle.us-east-1.api.aws" })).toBeInTheDocument();

    fireEvent.change(screen.getByLabelText(/^基础地址/), { target: { value: "https://bedrock-mantle.us-east-2.api.aws" } });
    fireEvent.click(screen.getByRole("button", { name: "创建并热加载" }));

    expect(create).not.toHaveBeenCalled();
    // Short on the field that is wrong, with the endpoint to reconcile stated
    // on the field that has to change.
    expect(await screen.findByText("地址绑定不匹配")).toBeVisible();
    expect(screen.getByLabelText(/^加密凭据/)).toHaveAttribute("aria-invalid", "true");
    expect(screen.getByText("所选凭据绑定的是 https://bedrock-mantle.us-east-1.api.aws。")).toBeVisible();

    // A default port on one side and none on the other is the same endpoint,
    // which is how the server compares them too.
    fireEvent.change(screen.getByLabelText(/^基础地址/), { target: { value: "https://bedrock-mantle.us-east-1.api.aws:443" } });
    fireEvent.click(screen.getByRole("button", { name: "创建并热加载" }));
    await waitFor(() => expect(create).toHaveBeenCalledOnce());
  });

  // The same refusal made server-side — a base URL changed in another tab, say —
  // arrives with both endpoints as data, so it is explained rather than reduced
  // to "the request is invalid" over an English sentinel.
  it("explains a server-side credential endpoint mismatch in the reader's language", async () => {
    vi.spyOn(api, "createProvider").mockRejectedValue(
      Object.assign(
        new ApiError(
          400,
          "credential is bound to https://api.openai.com:443, this provider's base URL is https://gateway.example:8443",
          "credential_base_url_mismatch",
          "",
          {
            code: "credential_base_url_mismatch",
            credential_base_url: "https://api.openai.com:443",
            provider_base_url: "https://gateway.example:8443",
          },
        ),
      ),
    );
    Element.prototype.scrollIntoView = function scrollIntoView() {};
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 服务商" }));
    fireEvent.change(screen.getByLabelText("服务商名称"), { target: { value: "OpenAI" } });
    fireEvent.click(screen.getByRole("button", { name: "创建并热加载" }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("这份凭据加密时绑定的是 https://api.openai.com:443");
    expect(alert).toHaveTextContent("当前连接的基础地址是 https://gateway.example:8443");
    expect(alert).not.toHaveTextContent("credential is bound to");
  });

  // The localized sentence names both endpoints, so it is only usable when both
  // arrived. A refusal built while the provider's own base URL could not be
  // parsed carries an empty one, and rendering that would leave a sentence with
  // a blank in it instead of the generic message and the server's own line.
  it("falls back to the generic refusal when an endpoint is missing from the payload", async () => {
    vi.spyOn(api, "createProvider").mockRejectedValue(
      new ApiError(
        400,
        "credential is bound to https://api.openai.com:443, this provider's base URL is ",
        "credential_base_url_mismatch",
        "",
        { code: "credential_base_url_mismatch", credential_base_url: "https://api.openai.com:443", provider_base_url: "" },
      ),
    );
    Element.prototype.scrollIntoView = function scrollIntoView() {};
    renderPage();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 服务商" }));
    fireEvent.change(screen.getByLabelText("服务商名称"), { target: { value: "OpenAI" } });
    fireEvent.click(screen.getByRole("button", { name: "创建并热加载" }));

    const alert = await screen.findByRole("alert");
    expect(alert).toHaveTextContent("请求内容无效，请检查后重试。");
    expect(alert).not.toHaveTextContent("这份凭据加密时绑定的是");
    // The server's own sentence stays visible, because nothing localized replaced it.
    expect(alert).toHaveTextContent("credential is bound to https://api.openai.com:443");
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
      <NotificationProvider>
        <ProvidersPage />
      </NotificationProvider>
    </QueryClientProvider>,
  );
}
