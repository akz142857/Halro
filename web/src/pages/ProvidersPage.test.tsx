import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
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

  afterEach(() => vi.restoreAllMocks());

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
    });
    expect(create.mock.calls[0][0]).not.toMatchObject({ profile_id: "openai.chat-completions.v1" });
  });

  it("creates an isolated Bedrock Agent Runtime credential for Cohere Rerank", async () => {
    const create = vi.spyOn(api, "createCredential").mockResolvedValue({} as never);
    renderPage();

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
  ])("preserves the bound Base URL when rotating a Bedrock $name credential", async ({ name, surface, scheme, boundBaseURL }) => {
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

    fireEvent.click(await screen.findByRole("button", { name: "轮换" }));
    expect(screen.getByLabelText("服务商类型")).toBeDisabled();
    expect(screen.getByLabelText(/^绑定的基础地址/)).toHaveValue(boundBaseURL);
    fireEvent.change(screen.getByLabelText(/^新密钥/), { target: { value: "rotated-secret" } });
    fireEvent.click(screen.getByRole("button", { name: "安全轮换" }));

    await waitFor(() => expect(rotate).toHaveBeenCalledOnce());
    expect(rotate.mock.calls[0]).toEqual([
      credential.id,
      expect.objectContaining({
        type: "bedrock",
        base_url: boundBaseURL,
        access_surface: surface,
        scheme,
        secret: "rotated-secret",
      }),
      credential.revision,
    ]);
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
