import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { api } from "../api";
import {
  EmptyState,
  ErrorState,
  Field,
  Loading,
  Modal,
  PageHeader,
  StatusDot,
  ConfirmButton,
} from "../components";
import type { AccessSurface, Credential, CredentialScheme, Provider, ProviderCapabilities, ProviderType } from "../types";
import { useTranslation } from "react-i18next";

const providerTypes: ProviderType[] = [
  "openai", "anthropic", "azure_openai", "deepseek", "gemini", "bedrock", "openai_compatible",
];

function ProviderTypeOptions({ t }: { t: ReturnType<typeof useTranslation>["t"] }) {
  return providerTypes.map((type) => <option key={type} value={type}>{t(`providers.types.${type}`)}</option>);
}

function defaultBaseURL(type: ProviderType) {
  if (type === "gemini") return "https://generativelanguage.googleapis.com";
  if (type === "anthropic") return "https://api.anthropic.com";
  if (type === "deepseek") return "https://api.deepseek.com";
  if (type === "bedrock") return "https://bedrock-runtime.us-east-1.amazonaws.com";
  return "https://api.openai.com";
}

const bedrockProfiles = [
  "bedrock.runtime.converse.text.v1",
  "bedrock.runtime.invoke.titan-embed-text-v2.v1",
  "bedrock.runtime.invoke.titan-image-v2.v1",
  "bedrock.agent-runtime.rerank.cohere-v3-5.v1",
  "bedrock.runtime.async.nova-reel-v1.v1",
  "bedrock.mantle.openai.chat.v1",
  "bedrock.mantle.openai.responses.v1",
  "bedrock.mantle.anthropic.messages.v1",
] as const;
type BedrockProfile = typeof bedrockProfiles[number];
type BedrockCredentialSurface = "bedrock-runtime" | "bedrock-agent-runtime" | "bedrock-mantle";

const openAIChatProfile = "openai.chat-embeddings.v1";

function bedrockProfileConfig(profile: BedrockProfile): { surface: AccessSurface; scheme: CredentialScheme; baseURL: string } {
  if (profile.startsWith("bedrock.agent-runtime.")) {
    return { surface: "bedrock-agent-runtime", scheme: "aws.sigv4.explicit-session", baseURL: "https://bedrock-agent-runtime.us-east-1.amazonaws.com" };
  }
  if (profile.startsWith("bedrock.runtime.")) {
    return { surface: "bedrock-runtime", scheme: "aws.sigv4.explicit-session", baseURL: "https://bedrock-runtime.us-east-1.amazonaws.com" };
  }
  return { surface: "bedrock-mantle", scheme: "aws.bedrock.api-key", baseURL: "https://bedrock-mantle.us-east-1.api.aws" };
}

function bedrockCredentialConfig(surface: BedrockCredentialSurface) {
  if (surface === "bedrock-agent-runtime") return bedrockProfileConfig("bedrock.agent-runtime.rerank.cohere-v3-5.v1");
  if (surface === "bedrock-mantle") return bedrockProfileConfig("bedrock.mantle.openai.chat.v1");
  return bedrockProfileConfig("bedrock.runtime.converse.text.v1");
}

function isBedrockProfile(value: string): value is BedrockProfile {
  return bedrockProfiles.includes(value as BedrockProfile);
}

export function ProvidersPage() {
  const { t } = useTranslation();
  const [credentialDialog, setCredentialDialog] = useState(false);
  const [providerDialog, setProviderDialog] = useState(false);
  const [editingProvider, setEditingProvider] = useState<Provider>();
  const credentials = useQuery({ queryKey: ["credentials"], queryFn: api.credentials });
  const providers = useQuery({ queryKey: ["providers"], queryFn: api.providers });
  const pending = credentials.isPending || providers.isPending;
  return (
    <>
      <PageHeader
        eyebrow={t("providers.eyebrow")}
        title={t("providers.title")}
        description={t("providers.description")}
        action={
          <div className="button-group">
            <button className="button secondary" onClick={() => setCredentialDialog(true)}>{t("providers.addCredential")}</button>
            <button className="button primary" onClick={() => setProviderDialog(true)}>{t("providers.addProvider")}</button>
          </div>
        }
      />
      {pending && <Loading />}
      {(credentials.isError || providers.isError) && (
        <ErrorState error={credentials.error || providers.error} />
      )}
      {!pending && (
        <div className="provider-grid">
          <section className="panel">
            <header className="panel-header">
              <div><p className="eyebrow">{t("providers.vault")}</p><h2>{t("providers.credentials")}</h2></div>
              <span className="count">{credentials.data?.items.length ?? 0}</span>
            </header>
            {credentials.data?.items.length === 0 && (
              <EmptyState title={t("providers.noCredentials")}>{t("providers.noCredentialsDescription")}</EmptyState>
            )}
            {credentials.data?.items.map((credential) => (
              <CredentialRow key={credential.id} credential={credential} />
            ))}
          </section>
          <section className="panel">
            <header className="panel-header">
              <div><p className="eyebrow">{t("providers.upstreams")}</p><h2>{t("providers.providers")}</h2></div>
              <span className="count">{providers.data?.items.length ?? 0}</span>
            </header>
            {providers.data?.items.length === 0 && (
              <EmptyState title={t("providers.noProviders")}>{t("providers.noProvidersDescription")}</EmptyState>
            )}
            {providers.data?.items.map((provider) => (
              <ProviderRow provider={provider} key={provider.id} onEdit={() => setEditingProvider(provider)} />
            ))}
          </section>
        </div>
      )}
      {credentialDialog && <CredentialForm onClose={() => setCredentialDialog(false)} />}
      {providerDialog && (
        <ProviderForm
          credentials={credentials.data?.items ?? []}
          onClose={() => setProviderDialog(false)}
        />
      )}
      {editingProvider && (
        <ProviderForm
          current={editingProvider}
          credentials={credentials.data?.items ?? []}
          onClose={() => setEditingProvider(undefined)}
        />
      )}
    </>
  );
}

function ProviderRow({ provider, onEdit }: { provider: Provider; onEdit: () => void }) {
  const { t } = useTranslation();
  const [result, setResult] = useState("");
  const queryClient = useQueryClient();
  const testMutation = useMutation({
    mutationFn: () => api.testProvider(provider.id),
    onSuccess: (value) => setResult(t("providers.healthy", { latency: value.latency_ms })),
    onError: () => setResult(t("providers.unhealthy")),
  });
  const deleteMutation = useMutation({
    mutationFn: () => api.deleteProvider(provider.id, provider.revision),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["providers"] }),
  });
  return (
    <>
      <div className="provider-row">
        <span className="provider-icon">{provider.type === "openai" ? "OA" : "AI"}</span>
        <div>
          <span><StatusDot ok={provider.enabled} /><strong>{provider.name}</strong></span>
          <small>{provider.base_url}</small>
          <small>{t("providers.profile")}: <code>{provider.profile_id}</code></small>
          <small>{t("providers.surface")}: {provider.access_surface} · {t("providers.evidence")}: {evidenceSummary(provider.capability_evidence)}</small>
          <small>{t("providers.concurrency", { value: provider.max_concurrency || t("common.unlimited") })}</small>
          <code>{provider.id}</code>
        </div>
        <div className="button-group">
          <button className="button ghost" onClick={onEdit}>{t("common.edit")}</button>
          <button
            className={`badge ${testMutation.isSuccess ? "good" : ""}`}
            disabled={!provider.enabled || testMutation.isPending}
            onClick={() => testMutation.mutate()}
          >
            {testMutation.isPending ? t("providers.testing") : result || (provider.enabled ? t("providers.test") : t("providers.off"))}
          </button>
          <ConfirmButton
            label={t("common.delete")}
            confirmLabel={t("providers.deleteProvider", { name: provider.name })}
            disabled={deleteMutation.isPending}
            onConfirm={() => deleteMutation.mutate()}
          />
        </div>
      </div>
      {deleteMutation.isError && <ErrorState error={deleteMutation.error} />}
      {testMutation.isError && <ErrorState error={testMutation.error} />}
    </>
  );
}

function CredentialRow({ credential }: { credential: Credential }) {
  const { t } = useTranslation();
  const [rotating, setRotating] = useState(false);
  const queryClient = useQueryClient();
  const remove = useMutation({
    mutationFn: () => api.deleteCredential(credential.id, credential.revision),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["credentials"] }),
  });
  return (
    <>
      <div className="credential-row">
        <span className="vault-lock" aria-hidden="true">◆</span>
        <div>
          <strong>{credential.name}</strong>
          <small>{credential.type} · {credential.access_surface} · {credential.scheme}</small>
          <small>{t("providers.keyGeneration", { version: credential.key_version })}</small>
          <code>{credential.id}</code>
        </div>
        <div className="row-actions">
          <button className="button ghost" onClick={() => setRotating(true)}>{t("providers.rotate")}</button>
          <ConfirmButton
            label={t("common.delete")}
            confirmLabel={t("providers.deleteCredential", { name: credential.name })}
            disabled={remove.isPending}
            onConfirm={() => remove.mutate()}
          />
        </div>
      </div>
      {remove.isError && <ErrorState error={remove.error} />}
      {rotating && <CredentialForm current={credential} onClose={() => setRotating(false)} />}
    </>
  );
}

function evidenceSummary(evidence: Record<string, string>) {
  const values = [...new Set(Object.values(evidence).filter((value) => value !== "unsupported"))];
  return values.length ? values.join(" / ") : "—";
}

function CredentialForm({
  current,
  onClose,
}: {
  current?: Credential;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const [name, setName] = useState(current?.name ?? "");
  const [type, setType] = useState<ProviderType>(current?.type ?? "openai");
  const [baseURL, setBaseURL] = useState(current?.bound_base_url ?? defaultBaseURL(current?.type ?? "openai"));
  const [bedrockSurface, setBedrockSurface] = useState<BedrockCredentialSurface>(
    current?.access_surface === "bedrock-mantle" || current?.access_surface === "bedrock-agent-runtime"
      ? current.access_surface
      : "bedrock-runtime",
  );
  const [secret, setSecret] = useState("");
  const queryClient = useQueryClient();
  const mutation = useMutation({
    mutationFn: () => {
      const bedrockBinding = type === "bedrock"
        ? bedrockCredentialConfig(bedrockSurface)
        : undefined;
      const value = {
        name,
        type,
        base_url: baseURL,
        ...(bedrockBinding ? { access_surface: bedrockBinding.surface, scheme: bedrockBinding.scheme } : {}),
        ...(secret ? { secret } : {}),
      };
      return current
        ? api.rotateCredential(current.id, value, current.revision)
        : api.createCredential({ ...value, secret });
    },
    onSettled: () => setSecret(""),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["credentials"] });
      onClose();
    },
  });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (name.trim() && baseURL.trim() && (current || secret)) mutation.mutate();
  };
  return (
    <Modal title={current ? t("providers.rotateCredential") : t("providers.saveCredential")} onClose={onClose}>
      <form onSubmit={submit} autoComplete="off">
        <Field label={t("providers.credentialName")}><input autoFocus value={name} onChange={(event) => setName(event.target.value)} /></Field>
        <Field label={t("providers.providerType")}>
          <select value={type} disabled={Boolean(current)} onChange={(event) => {
            const next = event.target.value as ProviderType;
            setType(next);
            setBaseURL(defaultBaseURL(next));
            setBedrockSurface("bedrock-runtime");
          }}>
            <ProviderTypeOptions t={t} />
          </select>
        </Field>
        {type === "bedrock" && (
          <Field label={t("providers.bedrockSurface")} hint={t("providers.bedrockSurfaceHint")}>
            <select value={bedrockSurface} disabled={Boolean(current)} onChange={(event) => {
              const next = event.target.value as BedrockCredentialSurface;
              setBedrockSurface(next);
              const config = bedrockCredentialConfig(next);
              setBaseURL(config.baseURL);
            }}>
              <option value="bedrock-runtime">{t("providers.bedrockRuntime")}</option>
              <option value="bedrock-agent-runtime">{t("providers.bedrockAgentRuntime")}</option>
              <option value="bedrock-mantle">{t("providers.bedrockMantle")}</option>
            </select>
          </Field>
        )}
        <Field label={t("providers.boundURL")} hint={t("providers.boundURLHint")}>
          <input inputMode="url" value={baseURL} onChange={(event) => setBaseURL(event.target.value)} />
        </Field>
        <Field
          label={current ? t("providers.newSecret") : type === "bedrock" && bedrockSurface !== "bedrock-mantle" ? t("providers.awsCredentialJSON") : t("providers.providerSecret")}
          hint={current
            ? t("providers.secretConfigured")
            : type === "bedrock" && bedrockSurface !== "bedrock-mantle"
              ? t("providers.bedrockHint")
              : type === "bedrock"
                ? t("providers.bedrockMantleHint")
              : t("providers.secretHint")}
        >
          <input
            type="password"
            autoComplete="new-password"
            value={secret}
            onChange={(event) => setSecret(event.target.value)}
          />
        </Field>
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions">
          <button type="button" className="button ghost" onClick={onClose}>{t("common.cancel")}</button>
          <button className="button primary" disabled={mutation.isPending || (!current && !secret)}>
            {current ? t("providers.rotateSecurely") : t("providers.saveEncrypted")}
          </button>
        </div>
      </form>
    </Modal>
  );
}

function ProviderForm({
  current,
  credentials,
  onClose,
}: {
  current?: Provider;
  credentials: Credential[];
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const initialType = current?.type ?? "openai";
  const [name, setName] = useState(current?.name ?? "");
  const [type, setType] = useState<ProviderType>(initialType);
  const initialProfile = current?.profile_id && isBedrockProfile(current.profile_id) ? current.profile_id : "bedrock.runtime.converse.text.v1";
  const [profileID, setProfileID] = useState<BedrockProfile>(initialProfile);
  const [openAIProfileID, setOpenAIProfileID] = useState(current?.profile_id === "openai.media-resources.v1" ? "openai.media-resources.v1" : openAIChatProfile);
  const [baseURL, setBaseURL] = useState(current?.base_url ?? defaultBaseURL(initialType));
  const [apiVersion, setAPIVersion] = useState(current?.api_version ?? "");
  const [maxConcurrency, setMaxConcurrency] = useState(current?.max_concurrency ?? 0);
  const [enabled, setEnabled] = useState(current?.enabled ?? true);
  const [capabilities, setCapabilities] = useState<ProviderCapabilities>(current?.capabilities ?? defaultProviderCapabilities(initialType));
  const selectedSurface = type === "bedrock" ? bedrockProfileConfig(profileID).surface : undefined;
  const matchingCredentials = credentials.filter((credential) => credential.type === type && (!selectedSurface || credential.access_surface === selectedSurface));
  const [credentialID, setCredentialID] = useState(current?.credential_id ?? credentials.find((credential) => credential.type === initialType && (initialType !== "bedrock" || credential.access_surface === bedrockProfileConfig(initialProfile).surface))?.id ?? "");
  const queryClient = useQueryClient();
  const mutation = useMutation({
    mutationFn: () => {
      const value = {
      name, type, base_url: baseURL,
      ...(type === "bedrock" ? {
        profile_id: profileID,
        access_surface: bedrockProfileConfig(profileID).surface,
        credential_scheme: bedrockProfileConfig(profileID).scheme,
      } : type === "openai" ? { profile_id: openAIProfileID } : {}),
      ...(type === "azure_openai" ? { api_version: apiVersion } : {}),
      credential_id: credentialID, capabilities, max_concurrency: maxConcurrency, enabled,
      };
      return current
        ? api.updateProvider(current.id, value, current.revision)
        : api.createProvider(value);
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["providers"] });
      onClose();
    },
  });
  return (
    <Modal title={current ? t("providers.editProvider") : t("providers.createProvider")} onClose={onClose}>
      {credentials.length === 0 ? (
        <div className="notice warning">
          <strong>{t("providers.credentialRequired")}</strong>
          <span>{t("providers.credentialRequiredDescription")}</span>
        </div>
      ) : (
        <form
          onSubmit={(event) => {
            event.preventDefault();
            if (name && credentialID) mutation.mutate();
          }}
        >
          <Field label={t("providers.providerName")}><input autoFocus value={name} onChange={(event) => setName(event.target.value)} /></Field>
          <Field label={t("providers.type")}>
            <select value={type} onChange={(event) => {
              const next = event.target.value as ProviderType;
              setType(next);
              setBaseURL(defaultBaseURL(next));
              setProfileID("bedrock.runtime.converse.text.v1");
              setOpenAIProfileID(openAIChatProfile);
              setCredentialID(credentials.find((credential) => credential.type === next && (next !== "bedrock" || credential.access_surface === "bedrock-runtime"))?.id ?? "");
              setCapabilities(defaultProviderCapabilities(next));
            }}>
              <ProviderTypeOptions t={t} />
            </select>
          </Field>
          {type === "bedrock" && (
            <Field label={t("providers.profile")} hint={t("providers.bedrockProfileHint")}>
              <select value={profileID} onChange={(event) => {
                const next = event.target.value as BedrockProfile;
                const config = bedrockProfileConfig(next);
                setProfileID(next);
                setBaseURL(config.baseURL);
                setCredentialID(credentials.find((credential) => credential.type === "bedrock" && credential.access_surface === config.surface)?.id ?? "");
                setCapabilities(defaultProviderCapabilities("bedrock", next));
              }}>
                {bedrockProfiles.map((profile) => <option value={profile} key={profile}>{t(`providers.bedrockProfiles.${profile}`)}</option>)}
              </select>
            </Field>
          )}
          {type === "openai" && (
            <Field label={t("providers.profile")}>
              <select value={openAIProfileID} onChange={(event) => {
                const next = event.target.value;
                setOpenAIProfileID(next);
                setCapabilities(defaultProviderCapabilities("openai", undefined, next));
              }}>
                <option value={openAIChatProfile}>{t("providers.openAIProfiles.chat")}</option>
                <option value="openai.media-resources.v1">{t("providers.openAIProfiles.media")}</option>
              </select>
            </Field>
          )}
          <Field label={t("providers.baseURL")}><input value={baseURL} onChange={(event) => setBaseURL(event.target.value)} /></Field>
          {type === "azure_openai" && (
            <Field label={t("providers.apiVersion")} hint={t("providers.apiVersionHint")}>
              <input value={apiVersion} onChange={(event) => setAPIVersion(event.target.value)} required />
            </Field>
          )}
          <Field label={t("providers.maxConcurrency")} hint={t("providers.maxConcurrencyHint")}>
            <input
              type="number"
              min="0"
              value={maxConcurrency}
              onChange={(event) => setMaxConcurrency(Number(event.target.value))}
            />
          </Field>
          <fieldset className="form-grid">
            <legend>{t("providers.capabilityLimit")}</legend>
            {capabilityNames.map((capability) => (
              <label className="check-row" key={capability}>
                <input
                  type="checkbox"
                  checked={capabilities[capability]}
                  onChange={(event) => setCapabilities({ ...capabilities, [capability]: event.target.checked })}
                />
                <span>{t(`capabilities.${capability}`)}</span>
              </label>
            ))}
            <Field label={t("providers.maxContext")} hint={t("providers.maxContextHint")}>
              <input min="0" type="number" value={capabilities.max_context_tokens} onChange={(event) => setCapabilities({ ...capabilities, max_context_tokens: Number(event.target.value) })} />
            </Field>
            <Field label={t("providers.maxOutput")} hint={t("providers.maxOutputHint")}>
              <input min="0" type="number" value={capabilities.max_output_tokens} onChange={(event) => setCapabilities({ ...capabilities, max_output_tokens: Number(event.target.value) })} />
            </Field>
          </fieldset>
          <Field label={t("providers.encryptedCredential")}>
            <select value={credentialID} onChange={(event) => setCredentialID(event.target.value)}>
              {matchingCredentials.map((credential) => (
                <option value={credential.id} key={credential.id}>{credential.name} · {credential.type}</option>
              ))}
            </select>
          </Field>
          <label className="check-row"><input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />{t("providers.enable")}</label>
          {mutation.isError && <ErrorState error={mutation.error} />}
          <div className="form-actions">
            <button type="button" className="button ghost" onClick={onClose}>{t("common.cancel")}</button>
            <button className="button primary" disabled={mutation.isPending || !credentialID}>{current ? t("providers.save") : t("providers.createAndLoad")}</button>
          </div>
        </form>
      )}
    </Modal>
  );
}

const capabilityNames = [
  "chat", "streaming", "embeddings", "moderations", "images", "transcriptions", "speech", "files", "batches", "rerank", "async_generate", "tools", "vision", "json_mode",
  "developer_role", "reasoning", "stream_usage",
] as const;

function defaultProviderCapabilities(type: ProviderType, profileID: BedrockProfile = "bedrock.runtime.converse.text.v1", openAIProfileID = openAIChatProfile): ProviderCapabilities {
  const value: ProviderCapabilities = {
    chat: true, streaming: true, embeddings: false, tools: false, vision: false,
    moderations: false, images: false, transcriptions: false, speech: false, files: false, batches: false, rerank: false, async_generate: false,
    json_mode: false, developer_role: false, reasoning: false, stream_usage: false,
    max_context_tokens: 0, max_output_tokens: 0,
  };
  if (type === "openai" || type === "azure_openai") {
    if (type === "openai" && openAIProfileID === "openai.media-resources.v1") return { ...value, chat: false, streaming: false, moderations: true, images: true, transcriptions: true, speech: true, files: true, batches: true };
    return { ...value, embeddings: true, tools: true, vision: true, json_mode: true, developer_role: true, reasoning: true, stream_usage: true };
  }
  if (type === "anthropic") return { ...value, tools: true, vision: true, reasoning: true, stream_usage: true };
  if (type === "deepseek") return { ...value, tools: true, json_mode: true, reasoning: true, stream_usage: true };
  if (type === "openai_compatible") return { ...value, embeddings: true };
  if (type === "gemini") return { ...value, embeddings: true, developer_role: true, stream_usage: false };
  if (type === "bedrock") {
    if (profileID === "bedrock.runtime.converse.text.v1") return { ...value, stream_usage: true };
    if (profileID === "bedrock.runtime.invoke.titan-embed-text-v2.v1") return { ...value, chat: false, streaming: false, embeddings: true, max_context_tokens: 8192 };
    if (profileID === "bedrock.runtime.invoke.titan-image-v2.v1") return { ...value, chat: false, streaming: false, images: true };
    if (profileID === "bedrock.agent-runtime.rerank.cohere-v3-5.v1") return { ...value, chat: false, streaming: false, rerank: true };
    if (profileID === "bedrock.runtime.async.nova-reel-v1.v1") return { ...value, chat: false, streaming: false, async_generate: true };
    if (profileID === "bedrock.mantle.anthropic.messages.v1") return { ...value, tools: true, vision: true, reasoning: true, stream_usage: true };
    return { ...value, tools: true, vision: true, json_mode: true, developer_role: true, reasoning: profileID === "bedrock.mantle.openai.chat.v1", stream_usage: true };
  }
  return value;
}
