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
import type { Credential, Provider, ProviderCapabilities, ProviderType } from "../types";
import { useTranslation } from "react-i18next";

const providerTypes: ProviderType[] = [
  "openai", "azure_openai", "deepseek", "gemini", "bedrock", "openai_compatible",
];

function ProviderTypeOptions({ t }: { t: ReturnType<typeof useTranslation>["t"] }) {
  return providerTypes.map((type) => <option key={type} value={type}>{t(`providers.types.${type}`)}</option>);
}

function defaultBaseURL(type: ProviderType) {
  if (type === "gemini") return "https://generativelanguage.googleapis.com";
  if (type === "deepseek") return "https://api.deepseek.com";
  if (type === "bedrock") return "https://bedrock-runtime.us-east-1.amazonaws.com";
  return "https://api.openai.com";
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
  const [baseURL, setBaseURL] = useState(defaultBaseURL(current?.type ?? "openai"));
  const [secret, setSecret] = useState("");
  const queryClient = useQueryClient();
  const mutation = useMutation({
    mutationFn: () => current
      ? api.rotateCredential(current.id, {
        name,
        type,
        base_url: baseURL,
        ...(secret ? { secret } : {}),
      }, current.revision)
      : api.createCredential({ name, type, base_url: baseURL, secret }),
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
          <select value={type} onChange={(event) => {
            const next = event.target.value as ProviderType;
            setType(next);
            setBaseURL(defaultBaseURL(next));
          }}>
            <ProviderTypeOptions t={t} />
          </select>
        </Field>
        <Field label={t("providers.boundURL")} hint={t("providers.boundURLHint")}>
          <input inputMode="url" value={baseURL} onChange={(event) => setBaseURL(event.target.value)} />
        </Field>
        <Field
          label={current ? t("providers.newSecret") : type === "bedrock" ? t("providers.awsCredentialJSON") : t("providers.providerSecret")}
          hint={current
            ? t("providers.secretConfigured")
            : type === "bedrock"
              ? t("providers.bedrockHint")
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
  const [baseURL, setBaseURL] = useState(current?.base_url ?? defaultBaseURL(initialType));
  const [apiVersion, setAPIVersion] = useState(current?.api_version ?? "");
  const [maxConcurrency, setMaxConcurrency] = useState(current?.max_concurrency ?? 0);
  const [enabled, setEnabled] = useState(current?.enabled ?? true);
  const [capabilities, setCapabilities] = useState<ProviderCapabilities>(current?.capabilities ?? defaultProviderCapabilities(initialType));
  const [credentialID, setCredentialID] = useState(current?.credential_id ?? credentials.find((credential) => credential.type === initialType)?.id ?? "");
  const matchingCredentials = credentials.filter((credential) => credential.type === type);
  const queryClient = useQueryClient();
  const mutation = useMutation({
    mutationFn: () => {
      const value = {
      name, type, base_url: baseURL,
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
              setCredentialID(credentials.find((credential) => credential.type === next)?.id ?? "");
              setCapabilities(defaultProviderCapabilities(next));
            }}>
              <ProviderTypeOptions t={t} />
            </select>
          </Field>
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
  "chat", "streaming", "embeddings", "tools", "vision", "json_mode",
  "developer_role", "reasoning", "stream_usage",
] as const;

function defaultProviderCapabilities(type: ProviderType): ProviderCapabilities {
  const value: ProviderCapabilities = {
    chat: true, streaming: true, embeddings: false, tools: false, vision: false,
    json_mode: false, developer_role: false, reasoning: false, stream_usage: false,
    max_context_tokens: 0, max_output_tokens: 0,
  };
  if (type === "openai" || type === "azure_openai") {
    return { ...value, embeddings: true, tools: true, vision: true, json_mode: true, developer_role: true, reasoning: true, stream_usage: true };
  }
  if (type === "deepseek") return { ...value, tools: true, json_mode: true, reasoning: true, stream_usage: true };
  if (type === "openai_compatible") return { ...value, embeddings: true };
  if (type === "gemini") return { ...value, embeddings: true, developer_role: true, stream_usage: false };
  if (type === "bedrock") return { ...value, stream_usage: true };
  return value;
}
