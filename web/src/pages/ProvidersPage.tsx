import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useState, type FormEvent, type KeyboardEvent } from "react";
import { api } from "../api";
import {
  EmptyState,
  ErrorState,
  Field,
  InlineTestControl,
  Loading,
  Modal,
  PageHeader,
  StatusDot,
  ConfirmButton,
  OverflowMenu,
  ResourceToolbar,
} from "../components";
import type { InlineTestState } from "../components";
import type { AccessSurface, Credential, CredentialScheme, Provider, ProviderBinding, ProviderCapabilities, ProviderType } from "../types";
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
const openAIMediaProfile = "openai.media-resources.v1";

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
  const [activeView, setActiveView] = useState<"providers" | "credentials">(() => providerViewFromURL());
  const [focusedCredentialID, setFocusedCredentialID] = useState("");
  const [focusedProviderCredentialID, setFocusedProviderCredentialID] = useState("");
  const [credentialDialog, setCredentialDialog] = useState(false);
  const [providerDialog, setProviderDialog] = useState(false);
  const [editingProvider, setEditingProvider] = useState<Provider>();
  const [providerQuery, setProviderQuery] = useState("");
  const [providerStatus, setProviderStatus] = useState<"all" | "enabled" | "disabled">("all");
  const [credentialQuery, setCredentialQuery] = useState("");
  const credentials = useQuery({ queryKey: ["credentials"], queryFn: api.credentials });
  const providers = useQuery({ queryKey: ["providers"], queryFn: api.providers });
  const pending = credentials.isPending || providers.isPending;
  const credentialItems = credentials.data?.items ?? [];
  const providerItems = providers.data?.items ?? [];
  const filteredProviders = useMemo(() => {
    const query = providerQuery.trim().toLocaleLowerCase();
    return providerItems.filter((provider) => {
      const matchesQuery = !query || [provider.name, provider.type, provider.base_url].some((value) => value.toLocaleLowerCase().includes(query));
      const matchesStatus = providerStatus === "all" || (providerStatus === "enabled" ? provider.enabled : !provider.enabled);
      return matchesQuery && matchesStatus;
    });
  }, [providerItems, providerQuery, providerStatus]);
  const filteredCredentials = useMemo(() => {
    const query = credentialQuery.trim().toLocaleLowerCase();
    return credentialItems.filter((credential) => !query || [credential.name, credential.type, credential.bound_base_url].some((value) => value.toLocaleLowerCase().includes(query)));
  }, [credentialItems, credentialQuery]);
  const canCreateProvider = credentialItems.length > 0;
  useEffect(() => {
    const syncView = () => setActiveView(providerViewFromURL());
    window.addEventListener("popstate", syncView);
    return () => window.removeEventListener("popstate", syncView);
  }, []);
  const selectView = (view: "providers" | "credentials") => {
    if (view === activeView) return;
    setActiveView(view);
    const url = new URL(window.location.href);
    if (view === "providers") url.searchParams.delete("view");
    else url.searchParams.set("view", view);
    window.history.pushState({}, "", `${url.pathname}${url.search}${url.hash}`);
  };
  const handleTabKey = (event: KeyboardEvent<HTMLButtonElement>) => {
    if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
    event.preventDefault();
    const next = event.key === "Home" || event.key === "ArrowLeft" ? "providers" : "credentials";
    selectView(next);
    document.getElementById(`${next}-tab`)?.focus();
  };
  return (
    <div className="providers-page">
      <PageHeader
        eyebrow={t("providers.eyebrow")}
        title={t("providers.title")}
        description={t("providers.description")}
        action={
          activeView === "providers"
            ? <button className="button primary" disabled={!pending && !canCreateProvider} title={!pending && !canCreateProvider ? t("providers.createCredentialFirst") : undefined} onClick={() => setProviderDialog(true)}>{t("providers.addProvider")}</button>
            : <button className="button primary" onClick={() => setCredentialDialog(true)}>{t("providers.addCredential")}</button>
        }
      />
      {pending && <Loading />}
      {(credentials.isError || providers.isError) && (
        <ErrorState error={credentials.error || providers.error} />
      )}
      {!pending && (
        <div className="provider-tabs-shell">
          <div className="provider-tabs" role="tablist" aria-label={t("providers.resourceViews")}>
            <button id="providers-tab" role="tab" tabIndex={activeView === "providers" ? 0 : -1} aria-selected={activeView === "providers"} aria-controls="providers-panel" onKeyDown={handleTabKey} onClick={() => selectView("providers")}>{t("providers.providerConnections")} <span>{providerItems.length}</span></button>
            <button id="credentials-tab" role="tab" tabIndex={activeView === "credentials" ? 0 : -1} aria-selected={activeView === "credentials"} aria-controls="credentials-panel" onKeyDown={handleTabKey} onClick={() => selectView("credentials")}>{t("providers.credentialVault")} <span>{credentialItems.length}</span></button>
          </div>
          {activeView === "providers" && <section id="providers-panel" role="tabpanel" aria-labelledby="providers-tab" className="panel provider-resource-panel">
            {!canCreateProvider && (
              <div className="dependency-notice"><div><strong>{t("providers.credentialRequired")}</strong><span>{t("providers.providerDependencyHint")}</span></div><button className="button secondary" onClick={() => { selectView("credentials"); setCredentialDialog(true); }}>{t("providers.openCredentialVault")}</button></div>
            )}
            {providerItems.length === 0 && canCreateProvider && (
              <EmptyState title={t("providers.noProviders")}>{t("providers.noProvidersDescription")}</EmptyState>
            )}
            {!!providerItems.length && <ResourceToolbar query={providerQuery} onQueryChange={setProviderQuery} queryPlaceholder={t("providers.searchProviders")} count={t("providers.resultCount", { visible: filteredProviders.length, total: providerItems.length })} status={providerStatus} onStatusChange={setProviderStatus} />}
            {!!providerItems.length && !filteredProviders.length && <EmptyState title={t("providers.noMatches")}>{t("providers.noMatchesDescription")}</EmptyState>}
            {filteredProviders.map((provider) => (
              <ProviderRow provider={provider} credential={credentialItems.find((credential) => credential.id === provider.credential_id)} highlighted={Boolean(focusedProviderCredentialID && provider.credential_id === focusedProviderCredentialID)} key={provider.id} onCredentialClick={() => { setFocusedCredentialID(provider.credential_id); selectView("credentials"); }} onEdit={() => setEditingProvider(provider)} />
            ))}
          </section>}
          {activeView === "credentials" && <section id="credentials-panel" role="tabpanel" aria-labelledby="credentials-tab" className="panel provider-resource-panel">
            {credentialItems.length === 0 && (
              <EmptyState title={t("providers.noCredentials")}>{t("providers.noCredentialsDescription")}</EmptyState>
            )}
            {!!credentialItems.length && <ResourceToolbar query={credentialQuery} onQueryChange={setCredentialQuery} queryPlaceholder={t("providers.searchCredentials")} count={t("providers.resultCount", { visible: filteredCredentials.length, total: credentialItems.length })} />}
            {!!credentialItems.length && !filteredCredentials.length && <EmptyState title={t("providers.noMatches")}>{t("providers.noMatchesDescription")}</EmptyState>}
            {filteredCredentials.map((credential) => (
              <CredentialRow key={credential.id} credential={credential} highlighted={focusedCredentialID === credential.id} useCount={providerItems.filter((provider) => provider.credential_id === credential.id).length} onUsageClick={() => { setFocusedProviderCredentialID(credential.id); selectView("providers"); }} />
            ))}
          </section>}
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
    </div>
  );
}

function providerViewFromURL(): "providers" | "credentials" {
  return new URLSearchParams(window.location.search).get("view") === "credentials" ? "credentials" : "providers";
}

function ProviderRow({ provider, credential, highlighted, onCredentialClick, onEdit }: { provider: Provider; credential?: Credential; highlighted: boolean; onCredentialClick: () => void; onEdit: () => void }) {
  const { t } = useTranslation();
  const [expanded, setExpanded] = useState(false);
  const queryClient = useQueryClient();
  const testMutation = useMutation({
    mutationFn: () => api.testProvider(provider.id),
    onSettled: () => queryClient.invalidateQueries({ queryKey: ["providers"] }),
  });
  const deleteMutation = useMutation({
    mutationFn: () => api.deleteProvider(provider.id, provider.revision),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["providers"] }),
  });
  const stateMutation = useMutation({
    mutationFn: () => api.updateProvider(provider.id, {
      name: provider.name,
      type: provider.type,
      base_url: provider.base_url,
      ...(provider.api_version ? { api_version: provider.api_version } : {}),
      credential_id: provider.credential_id,
      access_surface: provider.access_surface,
      profile_id: provider.profile_id,
      credential_scheme: provider.credential_scheme,
      capabilities: provider.capabilities,
      ...(provider.bindings?.length ? { bindings: provider.bindings } : {}),
      max_concurrency: provider.max_concurrency,
      enabled: !provider.enabled,
    }, provider.revision),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["providers"] }),
  });
  const persistedTestIsCurrent = provider.last_test_revision === provider.revision;
  const testState: InlineTestState = testMutation.isPending
    ? "running"
    : testMutation.isError
      ? "failure"
      : testMutation.isSuccess || persistedTestIsCurrent && provider.last_test_status === "healthy"
        ? "success"
        : persistedTestIsCurrent && provider.last_test_status === "unhealthy"
          ? "failure"
          : provider.last_test_status
            ? "stale"
            : "idle";
  const testLatency = testMutation.data?.latency_ms ?? provider.last_test_latency_millis;
  const healthyTargets = testMutation.data?.healthy_targets ?? provider.last_test_healthy_targets;
  const totalTargets = testMutation.data?.total_targets ?? provider.last_test_total_targets;
  return (
    <>
      <article className={`provider-row ${highlighted ? "resource-highlight" : ""}`}>
        <span className="provider-icon">{provider.type === "openai" ? "OA" : "AI"}</span>
        <div className="provider-compact-identity"><span><StatusDot ok={provider.enabled} /><strong>{provider.name}</strong></span><small>{t(`providers.types.${provider.type}`)}</small></div>
        <div className="provider-compact-fact"><small>{t("providers.endpoint")}</small><strong>{provider.base_url}</strong></div>
        <div className="provider-compact-fact"><small>{t("providers.boundCredential")}</small>{credential ? <button className="resource-link" onClick={onCredentialClick}>{credential.name} →</button> : <strong>{t("providers.missingCredential")}</strong>}</div>
        <div className="provider-compact-fact"><small>{t("providers.capabilities")}</small><strong>{t("providers.capabilityCount", { count: enabledCapabilities(provider).length })}</strong></div>
        <div className="provider-compact-status"><span className={`resource-state ${provider.enabled ? "enabled" : ""}`}>{provider.enabled ? t("providers.enabled") : t("providers.off")}</span></div>
        <div className="row-actions provider-compact-actions">
          <InlineTestControl state={testState} latency={testLatency} disabled={!provider.enabled} title={totalTargets ? t("providers.testSummary", { healthy: healthyTargets ?? 0, total: totalTargets, latency: testLatency ?? 0 }) : undefined} onTest={() => testMutation.mutate()} />
          <button className="button ghost" onClick={onEdit}>{t("common.edit")}</button>
          <button className="button ghost provider-expand" aria-expanded={expanded} aria-controls={`provider-details-${provider.id}`} onClick={() => setExpanded((value) => !value)}>{expanded ? t("providers.collapseDetails") : t("providers.expandDetails")}</button>
          {provider.enabled ? <ConfirmButton className="button ghost" label={t("common.disable")} title={t("providers.disableTitle")} confirmLabel={t("providers.disableConfirm", { name: provider.name })} disabled={stateMutation.isPending} onConfirm={() => stateMutation.mutate()} /> : <button className="button ghost" disabled={stateMutation.isPending} onClick={() => stateMutation.mutate()}>{t("common.enable")}</button>}
          <OverflowMenu label={t("providers.moreActions")}><ConfirmButton label={t("common.delete")} confirmLabel={t("providers.deleteProvider", { name: provider.name })} disabled={deleteMutation.isPending} onConfirm={() => deleteMutation.mutate()} /></OverflowMenu>
        </div>
        {expanded && <div id={`provider-details-${provider.id}`} className="provider-row-content provider-expanded-content">
          <div className="provider-facts">
            <div><small>{t("providers.endpoint")}</small><strong>{provider.base_url}</strong></div>
            <div><small>{t("providers.boundCredential")}</small>{credential ? <button className="resource-link" onClick={onCredentialClick}>{credential.name} →</button> : <strong>{t("providers.missingCredential")}</strong>}</div>
            <div><small>{t("providers.capabilities")}</small><strong>{t("providers.capabilityCount", { count: enabledCapabilities(provider).length })}</strong></div>
            <div><small>{t("providers.capacity")}</small><strong>{provider.max_concurrency || t("common.unlimited")}</strong></div>
          </div>
          <div className="capability-summary provider-capability-summary">{enabledCapabilities(provider).slice(0, 6).map((capability) => <span className="badge" key={capability}>{t(`capabilities.${capability}`)}</span>)}</div>
          <div className="technical-details provider-technical-details">
            <strong>{t("providers.technicalDetails")}</strong>
            <dl>
              <div><dt>{t("providers.capabilityInterfaces")}</dt><dd><code>{provider.bindings?.filter((binding) => binding.enabled).map((binding) => binding.profile_id).join(" · ") || provider.profile_id}</code></dd></div>
              <div><dt>{t("providers.surface")}</dt><dd>{provider.access_surface}</dd></div>
              <div><dt>{t("providers.evidence")}</dt><dd>{evidenceSummary(provider.capability_evidence)}</dd></div>
              <div><dt>ID</dt><dd><code>{provider.id}</code></dd></div>
            </dl>
          </div>
        </div>}
      </article>
      {deleteMutation.isError && <ErrorState error={deleteMutation.error} />}
      {stateMutation.isError && <ErrorState error={stateMutation.error} />}
    </>
  );
}

function CredentialRow({ credential, useCount, highlighted, onUsageClick }: { credential: Credential; useCount: number; highlighted: boolean; onUsageClick: () => void }) {
  const { t } = useTranslation();
  const [rotating, setRotating] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const queryClient = useQueryClient();
  const remove = useMutation({
    mutationFn: () => api.deleteCredential(credential.id, credential.revision),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["credentials"] }),
  });
  return (
    <>
      <article className={`credential-row ${highlighted ? "resource-highlight" : ""}`}>
        <span className="provider-icon credential-icon" aria-hidden="true">K{credential.key_version}</span>
        <div className="credential-compact-identity"><strong>{credential.name}</strong><small>{t(`providers.types.${credential.type}`)}</small><code className="credential-identity-endpoint">{credential.bound_base_url}</code></div>
        <div className="credential-compact-fact credential-endpoint"><small>{t("providers.boundURL")}</small><strong>{credential.bound_base_url}</strong></div>
        <div className="credential-compact-fact credential-usage"><small>{t("providers.usage")}</small>{useCount > 0 ? <button className="resource-link inline" onClick={onUsageClick}>{t("providers.credentialUsage", { count: useCount })} →</button> : <strong>{t("providers.credentialUsage", { count: useCount })}</strong>}</div>
        <div className="credential-compact-fact credential-generation"><small>{t("providers.generation")}</small><strong>{t("providers.keyGeneration", { version: credential.key_version })}</strong></div>
        <div className="row-actions credential-actions">
          <button className="button ghost" onClick={() => setRotating(true)}>{t("providers.rotate")}</button>
          <button className="button ghost credential-expand" aria-expanded={expanded} aria-controls={`credential-details-${credential.id}`} onClick={() => setExpanded((value) => !value)}>{expanded ? t("providers.collapseDetails") : t("providers.expandDetails")}</button>
          <OverflowMenu label={t("providers.moreActions")}><ConfirmButton label={t("common.delete")} confirmLabel={t("providers.deleteCredential", { name: credential.name })} disabled={remove.isPending} onConfirm={() => remove.mutate()} /></OverflowMenu>
        </div>
        {expanded && <section id={`credential-details-${credential.id}`} className="credential-expanded-content" aria-label={t("providers.credentialDetailsTitle")}>
          <header className="credential-detail-header">
            <div><small>{t("providers.technicalDetails")}</small><strong>{t("providers.credentialDetailsTitle")}</strong></div>
            <p>{t("providers.credentialDetailsDescription")}</p>
          </header>
          <dl className="credential-detail-grid">
            <div><dt>{t("providers.surface")}</dt><dd><code>{credential.access_surface}</code></dd></div>
            <div><dt>{t("providers.scheme")}</dt><dd><code>{credential.scheme}</code></dd></div>
            <div><dt>{t("providers.credentialID")}</dt><dd><code>{credential.id}</code></dd></div>
          </dl>
        </section>}
      </article>
      {remove.isError && <ErrorState error={remove.error} />}
      {rotating && <CredentialForm current={credential} onClose={() => setRotating(false)} />}
    </>
  );
}

function evidenceSummary(evidence: Record<string, string>) {
  const values = [...new Set(Object.values(evidence).filter((value) => value !== "unsupported"))];
  return values.length ? values.join(" / ") : "—";
}

function enabledCapabilities(provider: Provider) {
  return capabilityNames.filter((capability) => provider.capabilities?.[capability]);
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
  const [baseURL, setBaseURL] = useState(current?.base_url ?? defaultBaseURL(initialType));
  const [apiVersion, setAPIVersion] = useState(current?.api_version ?? "");
  const [maxConcurrency, setMaxConcurrency] = useState(current?.max_concurrency ?? 0);
  const [enabled, setEnabled] = useState(current?.enabled ?? true);
  const [capabilities, setCapabilities] = useState<ProviderCapabilities>(current?.capabilities ?? defaultProviderCapabilities(initialType));
  const capabilityCeiling = defaultProviderCapabilities(type, profileID);
  const fixedCapabilities = isStrictCapabilityProfile(type, profileID);
  const visibleCapabilities = capabilityNames.filter((capability) => capabilities[capability]);
  const configurableCapabilities = capabilityNames.filter((capability) => capabilityCeiling[capability] || capabilities[capability]);
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
      } : type === "openai" ? {
        // profile_id remains during the rolling backend migration. New runtimes
        // route each operation through the matching capability binding.
        profile_id: openAIChatProfile,
        bindings: openAIBindings(openAIChatProfile, capabilities),
      } : {}),
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
    <Modal wide title={current ? t("providers.editProvider") : t("providers.createProvider")} onClose={onClose}>
      {credentials.length === 0 ? (
        <div className="notice warning">
          <strong>{t("providers.credentialRequired")}</strong>
          <span>{t("providers.credentialRequiredDescription")}</span>
        </div>
      ) : (
        <form className="provider-form"
          onSubmit={(event) => {
            event.preventDefault();
            if (name && credentialID) mutation.mutate();
          }}
        >
          <section className="provider-form-section" aria-labelledby="provider-connection-title">
            <header><h3 id="provider-connection-title">{t("providers.connectionSection")}</h3><p>{t("providers.connectionSectionDescription")}</p></header>
            <div className="form-grid">
          <Field label={t("providers.providerName")}><input autoFocus value={name} onChange={(event) => setName(event.target.value)} /></Field>
          <Field label={t("providers.type")}>
            <select value={type} onChange={(event) => {
              const next = event.target.value as ProviderType;
              setType(next);
              setBaseURL(defaultBaseURL(next));
              setProfileID("bedrock.runtime.converse.text.v1");
              setCredentialID(credentials.find((credential) => credential.type === next && (next !== "bedrock" || credential.access_surface === "bedrock-runtime"))?.id ?? "");
              setCapabilities(defaultProviderCapabilities(next));
            }}>
              <ProviderTypeOptions t={t} />
            </select>
          </Field>
          {type === "bedrock" && (
            <Field label={t("providers.capabilityImplementation")} hint={t("providers.bedrockProfileHint")}>
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
          <Field label={t("providers.baseURL")}><input value={baseURL} onChange={(event) => setBaseURL(event.target.value)} /></Field>
          {type === "azure_openai" && (
            <Field label={t("providers.apiVersion")} hint={t("providers.apiVersionHint")}>
              <input value={apiVersion} onChange={(event) => setAPIVersion(event.target.value)} required />
            </Field>
          )}
            </div>
            <div className="provider-capabilities-group" aria-labelledby="provider-capabilities-title">
              <header><h4 id="provider-capabilities-title">{t("providers.capabilitySummary")}</h4><p>{t(fixedCapabilities ? "providers.fixedCapabilityDescription" : "providers.capabilitySectionDescription")}</p></header>
              <div className="capability-summary" aria-label={t("providers.capabilitySummary")}>
                {visibleCapabilities.map((capability) => <span className="badge" key={capability}>{t(`capabilities.${capability}`)}</span>)}
              </div>
              {!fixedCapabilities && (
                <div className="capability-disclosure capability-advanced">
                  <header><span>{t("providers.advancedCapabilities")}</span><strong>{t("providers.selectedCapabilities", { count: visibleCapabilities.length })}</strong></header>
                  <p className="capability-advanced-note">{t("providers.advancedCapabilitiesHint")}</p>
                  <div className="capability-grid">{configurableCapabilities.map((capability) => { const unavailable = !capabilityCeiling[capability]; return <label className={`capability-option ${unavailable ? "unavailable" : ""}`} key={capability}><input type="checkbox" disabled={unavailable && !capabilities[capability]} checked={capabilities[capability]} onChange={(event) => setCapabilities(updateCapabilitySelection(capabilities, capability, event.target.checked))} /><span>{t(`capabilities.${capability}`)}{unavailable && <small>{t("providers.unsupportedByInterface")}</small>}</span></label>; })}</div>
                  <div className="form-grid capability-limits"><Field label={t("providers.maxContext")} hint={t("providers.maxContextHint")}><input min="0" type="number" value={capabilities.max_context_tokens} onChange={(event) => setCapabilities({ ...capabilities, max_context_tokens: Number(event.target.value) })} /></Field><Field label={t("providers.maxOutput")} hint={t("providers.maxOutputHint")}><input min="0" type="number" value={capabilities.max_output_tokens} onChange={(event) => setCapabilities({ ...capabilities, max_output_tokens: Number(event.target.value) })} /></Field></div>
                </div>
              )}
            </div>
          </section>
          <section className="provider-form-section" aria-labelledby="provider-capacity-title">
            <header><h3 id="provider-capacity-title">{t("providers.capacitySection")}</h3><p>{t("providers.capacitySectionDescription")}</p></header>
            <div className="form-grid">
          <Field label={t("providers.maxConcurrency")} hint={t("providers.maxConcurrencyHint")}>
            <input
              type="number"
              min="0"
              value={maxConcurrency}
              onChange={(event) => setMaxConcurrency(Number(event.target.value))}
            />
          </Field>
          <Field label={t("providers.encryptedCredential")}>
            <select value={credentialID} onChange={(event) => setCredentialID(event.target.value)}>
              {matchingCredentials.map((credential) => (
                <option value={credential.id} key={credential.id}>{credential.name} · {credential.type}</option>
              ))}
            </select>
          </Field>
            </div>
            <label className="project-enable-row"><span><strong>{t("providers.enable")}</strong><small>{t("providers.enableDescription")}</small></span><input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} /></label>
          </section>
          {mutation.isError && <ErrorState error={mutation.error} />}
          <div className="form-actions provider-form-actions">
            <button type="button" className="button ghost" onClick={onClose}>{t("common.cancel")}</button>
            <button className="button primary" disabled={mutation.isPending || !credentialID || !hasEnabledCapability(capabilities)}>{current ? t("providers.save") : t("providers.createAndLoad")}</button>
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

function updateCapabilitySelection(current: ProviderCapabilities, capability: typeof capabilityNames[number], enabled: boolean): ProviderCapabilities {
  const next = { ...current, [capability]: enabled };
  const chatFeatures = ["streaming", "tools", "vision", "json_mode", "developer_role", "reasoning", "stream_usage"] as const;
  if (capability === "chat" && !enabled) {
    for (const feature of chatFeatures) next[feature] = false;
  } else if (capability !== "chat" && chatFeatures.includes(capability as typeof chatFeatures[number]) && enabled) {
    next.chat = true;
  }
  return next;
}

function isStrictCapabilityProfile(type: ProviderType, profileID: BedrockProfile) {
  if (type === "openai") return false;
  if (type !== "bedrock") return false;
  return [
    "bedrock.runtime.invoke.titan-embed-text-v2.v1",
    "bedrock.runtime.invoke.titan-image-v2.v1",
    "bedrock.agent-runtime.rerank.cohere-v3-5.v1",
    "bedrock.runtime.async.nova-reel-v1.v1",
  ].includes(profileID);
}

function defaultProviderCapabilities(type: ProviderType, profileID: BedrockProfile = "bedrock.runtime.converse.text.v1"): ProviderCapabilities {
  const value: ProviderCapabilities = {
    chat: true, streaming: true, embeddings: false, tools: false, vision: false,
    moderations: false, images: false, transcriptions: false, speech: false, files: false, batches: false, rerank: false, async_generate: false,
    json_mode: false, developer_role: false, reasoning: false, stream_usage: false,
    max_context_tokens: 0, max_output_tokens: 0,
  };
  if (type === "openai" || type === "azure_openai") {
    const chat = { ...value, embeddings: true, tools: true, vision: true, json_mode: true, developer_role: true, reasoning: true, stream_usage: true };
    if (type === "openai") return { ...chat, moderations: true, images: true, transcriptions: true, speech: true, files: true, batches: true };
    return chat;
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

const openAIChatCapabilities = new Set<keyof ProviderCapabilities>([
  "chat", "streaming", "embeddings", "tools", "vision", "json_mode", "developer_role", "reasoning", "stream_usage",
  "max_context_tokens", "max_output_tokens",
]);
const openAIMediaCapabilities = new Set<keyof ProviderCapabilities>([
  "moderations", "images", "transcriptions", "speech", "files", "batches",
]);

function bindingCapabilities(source: ProviderCapabilities, allowed: Set<keyof ProviderCapabilities>): ProviderCapabilities {
  return Object.fromEntries(Object.entries(source).map(([name, value]) => [
    name,
    allowed.has(name as keyof ProviderCapabilities) ? value : (typeof value === "number" ? 0 : false),
  ])) as unknown as ProviderCapabilities;
}

function hasEnabledCapability(capabilities: ProviderCapabilities) {
  return capabilityNames.some((name) => capabilities[name]);
}

function openAIBindings(chatProfileID: string, capabilities: ProviderCapabilities): ProviderBinding[] {
  const chat = bindingCapabilities(capabilities, openAIChatCapabilities);
  const media = bindingCapabilities(capabilities, openAIMediaCapabilities);
  return [
    { profile_id: chatProfileID, enabled: hasEnabledCapability(chat), capabilities: chat },
    { profile_id: openAIMediaProfile, enabled: hasEnabledCapability(media), capabilities: media },
  ];
}
