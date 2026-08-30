import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState, type FormEvent, type KeyboardEvent } from "react";
import { api } from "../api";
import {
  ConfirmButton,
  EmptyState,
  ErrorState,
  Field,
  InlineTestControl,
  isStepUpPrompt,
  Loading,
  Modal,
  OverflowMenu,
  PageHeader,
  ReauthFields,
  ResourceToolbar,
  StatusDot,
  useDirty,
  useStepUpPrompt,
  useTestFailureReason,
  type ReauthValues,
} from "../components";
import type { InlineTestState } from "../components";
import { useInstantFormatter } from "../format";
import { isoToZonedInput, useAccountingTimeZone, zonedInputToISO } from "../timezone";
import { useNotify } from "../notifications";
import type { AccessSurface, Credential, CredentialScheme, Provider, ProviderCapabilities, ProviderProfilesCatalog, ProviderType } from "../types";
import {
  anyCapabilityEnabled,
  booleanCapabilityNames,
  capabilityNeedsOptInWarning,
  combinableProfiles,
  connectionCeiling,
  connectionDefaults,
  defaultProfileID,
  findProfile,
  profilesForType,
  unservableCapabilities,
  updateCapabilitySelection,
  useProviderProfiles,
} from "../hooks/useProviderProfiles";
import { useTranslation } from "react-i18next";
import { useIsReadOnly } from "../session";
import { hasOnboardingCreateIntent, OnboardingContextBanner } from "../OnboardingContext";

const providerTypes: ProviderType[] = [
  "openai", "anthropic", "azure_openai", "deepseek", "gemini", "bedrock", "openai_compatible",
];

function ProviderTypeOptions({ t }: { t: ReturnType<typeof useTranslation>["t"] }) {
  return providerTypes.map((type) => <option key={type} value={type}>{t(`providers.types.${type}`)}</option>);
}

// The server normalizes an endpoint to scheme, host and an always-explicit
// port; `origin` collapses the default port on both sides, so two values that
// compare equal here are the two the server compares.
function urlOrigin(value: string) {
  try {
    return new URL(value.trim()).origin;
  } catch {
    return "";
  }
}

function displayBoundBaseURL(value: string) {
  return urlOrigin(value) || value;
}

// A credential's declared expiry is operator-supplied and advisory: nothing in
// the request path reads it. What it is for is seeing the rotation coming, so
// the row states it in the terms the operator plans in — already gone, or how
// many days are left — rather than only as a timestamp.
const credentialExpiryWarningDays = 30;

function credentialExpiry(value: string | undefined, now = Date.now()) {
  if (!value) return undefined;
  const at = new Date(value).getTime();
  if (Number.isNaN(at)) return undefined;
  const days = Math.ceil((at - now) / 86_400_000);
  return { expired: at <= now, days, soon: at > now && days <= credentialExpiryWarningDays };
}

// Which profile a Bedrock credential is created for.
//
// The form used to ask, because Bedrock's surfaces have different hosts and
// different credential schemes. The served matrix answers it now: this build
// offers Mantle alone, so the type's default profile is the only binding a new
// credential can take, and a question with one answer is not asked.
function bedrockCredentialProfile(catalog: ProviderProfilesCatalog) {
  return findProfile(catalog, "bedrock", defaultProfileID(catalog, "bedrock"));
}

// The anthropic-beta header is comma separated, so the form takes one comma
// separated string and stores the token set. Splitting here (rather than asking
// the operator for one row per token) keeps copy-paste from Anthropic's docs
// working, which is how these values actually arrive.
function parseBetaTokens(value: string): string[] {
  return value.split(",").map((token) => token.trim()).filter(Boolean);
}

// The endpoint offered for a type is the one its default profile carries,
// already resolved for this deployment by the server.
function endpointForType(catalog: ProviderProfilesCatalog, type: ProviderType) {
  return findProfile(catalog, type, defaultProfileID(catalog, type))?.default_base_url ?? "";
}

function isBedrockProfile(catalog: ProviderProfilesCatalog, value: string) {
  return profilesForType(catalog, "bedrock").some((profile) => profile.id === value);
}

export function ProvidersPage() {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const [activeView, setActiveView] = useState<"providers" | "credentials">(() => providerViewFromURL());
  const [focusedCredentialID, setFocusedCredentialID] = useState("");
  const [focusedProviderCredentialID, setFocusedProviderCredentialID] = useState("");
  const createFromOnboarding = hasOnboardingCreateIntent();
  const [credentialDialog, setCredentialDialog] = useState(() => !readOnly && createFromOnboarding && providerViewFromURL() === "credentials");
  const [providerDialog, setProviderDialog] = useState(() => !readOnly && createFromOnboarding && providerViewFromURL() === "providers");
  const [editingProvider, setEditingProvider] = useState<Provider>();
  const [providerQuery, setProviderQuery] = useState("");
  const [providerStatus, setProviderStatus] = useState<"all" | "enabled" | "disabled">("all");
  const [credentialQuery, setCredentialQuery] = useState("");
  const credentials = useQuery({ queryKey: ["credentials"], queryFn: api.credentials });
  const providers = useQuery({ queryKey: ["providers"], queryFn: api.providers });
  // What this build can serve. The forms cannot decide what to offer without it,
  // so they wait for it; the listing below does not, and stays readable either
  // way.
  const catalog = useProviderProfiles();
  const pending = credentials.isPending || providers.isPending || catalog.isPending;
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
            ? <button className="button primary" disabled={readOnly || catalog.isError || (!pending && !canCreateProvider)} title={catalog.isError ? t("providers.matrixUnavailable") : !pending && !canCreateProvider ? t("providers.createCredentialFirst") : undefined} onClick={() => setProviderDialog(true)}>{t("providers.addProvider")}</button>
            : <button className="button primary" disabled={readOnly || catalog.isError} title={catalog.isError ? t("providers.matrixUnavailable") : undefined} onClick={() => setCredentialDialog(true)}>{t("providers.addCredential")}</button>
        }
      />
      <OnboardingContextBanner />
      {pending && <Loading />}
      {/* The forms need the matrix and there is no offline copy of it, so a
          failed fetch is the difference between editing connections and not.
          That makes a retry part of the message rather than something the
          operator has to reload the page to reach — a session that expired
          mid-visit comes back on one click. */}
      {(credentials.isError || providers.isError || catalog.isError) && (
        <ErrorState
          error={credentials.error || providers.error || catalog.error}
          action={
            <button
              className="button ghost"
              disabled={credentials.isFetching || providers.isFetching || catalog.isFetching}
              onClick={() => {
                if (credentials.isError) credentials.refetch();
                if (providers.isError) providers.refetch();
                if (catalog.isError) catalog.refetch();
              }}
            >
              {t("common.retry")}
            </button>
          }
        />
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
              <ProviderRow provider={provider} credential={credentialItems.find((credential) => credential.id === provider.credential_id)} editable={catalog.isSuccess} highlighted={Boolean(focusedProviderCredentialID && provider.credential_id === focusedProviderCredentialID)} key={provider.id} onCredentialClick={() => { setFocusedCredentialID(provider.credential_id); selectView("credentials"); }} onEdit={() => setEditingProvider(provider)} />
            ))}
          </section>}
          {activeView === "credentials" && <section id="credentials-panel" role="tabpanel" aria-labelledby="credentials-tab" className="panel provider-resource-panel">
            {credentialItems.length === 0 && (
              <EmptyState title={t("providers.noCredentials")}>{t("providers.noCredentialsDescription")}</EmptyState>
            )}
            {!!credentialItems.length && <ResourceToolbar query={credentialQuery} onQueryChange={setCredentialQuery} queryPlaceholder={t("providers.searchCredentials")} count={t("providers.resultCount", { visible: filteredCredentials.length, total: credentialItems.length })} />}
            {!!credentialItems.length && !filteredCredentials.length && <EmptyState title={t("providers.noMatches")}>{t("providers.noMatchesDescription")}</EmptyState>}
            {filteredCredentials.map((credential) => (
              <CredentialRow key={credential.id} credential={credential} catalog={catalog.data} highlighted={focusedCredentialID === credential.id} useCount={providerItems.filter((provider) => provider.credential_id === credential.id).length} onUsageClick={() => { setFocusedProviderCredentialID(credential.id); selectView("providers"); }} />
            ))}
          </section>}
        </div>
      )}
      {/* The forms decide what to offer from the served matrix, and their initial
          state is built when they mount, so they mount only once it has arrived.
          There is no fallback on purpose: a form built from a guess is how the
          console and the server drifted apart. */}
      {credentialDialog && catalog.isSuccess && (
        <CredentialForm catalog={catalog.data} onClose={() => setCredentialDialog(false)} />
      )}
      {providerDialog && credentials.isSuccess && catalog.isSuccess && (
        <ProviderForm
          credentials={credentials.data?.items ?? []}
          catalog={catalog.data}
          onClose={() => setProviderDialog(false)}
        />
      )}
      {editingProvider && catalog.isSuccess && (
        <ProviderForm
          current={editingProvider}
          credentials={credentials.data?.items ?? []}
          catalog={catalog.data}
          onClose={() => setEditingProvider(undefined)}
        />
      )}
    </div>
  );
}

function providerViewFromURL(): "providers" | "credentials" {
  return new URLSearchParams(window.location.search).get("view") === "credentials" ? "credentials" : "providers";
}

function ProviderRow({ provider, credential, editable, highlighted, onCredentialClick, onEdit }: { provider: Provider; credential?: Credential; editable: boolean; highlighted: boolean; onCredentialClick: () => void; onEdit: () => void }) {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const [expanded, setExpanded] = useState(false);
  const queryClient = useQueryClient();
  const { notify } = useNotify();
  const testMutation = useMutation({
    mutationFn: () => api.testProvider(provider.id),
    onSettled: () => queryClient.invalidateQueries({ queryKey: ["providers"] }),
  });
  const deleteMutation = useMutation({
    mutationFn: (reauth: ReauthValues) => api.deleteProvider(provider.id, provider.revision, reauth),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["providers"] });
      notify({ tone: "success", title: t("providers.notifyDeleted"), description: provider.name });
    },
  });
  const stateMutation = useMutation({
    mutationFn: () => api.updateProvider(provider.id, {
      name: provider.name,
      type: provider.type,
      base_url: provider.base_url,
      ...(provider.api_version ? { api_version: provider.api_version } : {}),
      ...(provider.bedrock_project_id ? { bedrock_project_id: provider.bedrock_project_id } : {}),
      credential_id: provider.credential_id,
      access_surface: provider.access_surface,
      profile_id: provider.profile_id,
      credential_scheme: provider.credential_scheme,
      // Enabling or disabling must not restate the connection. Bindings are the
      // server's answer to the capability set and are not sent back at all; the
      // token limits are dropped for the same reason the form drops them — the
      // stored summary reports the loosest bound across the bindings, so echoing
      // it hands one profile's bound to the others.
      capabilities: { ...provider.capabilities, max_context_tokens: 0, max_output_tokens: 0 },
      max_concurrency: provider.max_concurrency,
      enabled: !provider.enabled,
    }, provider.revision),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["providers"] });
      notify({ tone: "success", title: t(provider.enabled ? "providers.notifyDisabled" : "providers.notifyEnabled"), description: provider.name });
    },
    // No onError: this mutation renders an ErrorState in place, which carries the
    // reason. A second copy in the notification column says less and, on the
    // confirm-gated path, appears above a modal whose Tab trap cannot reach it.
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
  const testFailureReason = useTestFailureReason(testMutation.error, persistedTestIsCurrent ? provider.last_test_error_class : undefined);
  const testLatency = testMutation.data?.latency_ms ?? provider.last_test_latency_millis;
  const healthyTargets = testMutation.data?.healthy_targets ?? provider.last_test_healthy_targets;
  const totalTargets = testMutation.data?.total_targets ?? provider.last_test_total_targets;
  return (
    <>
      <article id={`provider-${provider.id}`} className={`provider-row ${highlighted ? "resource-highlight" : ""}`}>
        <span className="provider-icon">{provider.type === "openai" ? "OA" : "AI"}</span>
        <div className="resource-identity"><span><StatusDot ok={provider.enabled} /><strong>{provider.name}</strong></span><small>{t(`providers.types.${provider.type}`)}</small></div>
        <div className="resource-fact provider-fact-endpoint"><small>{t("providers.endpoint")}</small><strong>{provider.base_url}</strong></div>
        <div className="resource-fact"><small>{t("providers.boundCredential")}</small>{credential ? <button className="resource-link" onClick={onCredentialClick}>{credential.name}</button> : <strong>{t("providers.missingCredential")}</strong>}</div>
        <div className="resource-fact provider-fact-capabilities"><small>{t("providers.capabilities")}</small><strong>{t("providers.capabilityCount", { count: enabledCapabilities(provider).length })}</strong></div>
        <div className="resource-row-state provider-compact-status"><span className={`resource-state ${provider.enabled ? "enabled" : ""}`}>{provider.enabled ? t("providers.enabled") : t("providers.off")}</span></div>
        <div className="row-actions provider-compact-actions">
          <InlineTestControl state={testState} latency={testLatency} disabled={!provider.enabled} title={totalTargets ? t("providers.testSummary", { healthy: healthyTargets ?? 0, total: totalTargets, latency: testLatency ?? 0 }) : undefined} onTest={() => testMutation.mutate()} />
          {/* Editing opens a form built from the served matrix. Without it the
              click would set state and render nothing, so the reason is on the
              button — the same treatment the create and rotate buttons get. */}
          <button className="button ghost" disabled={readOnly || !editable} title={!editable ? t("providers.matrixUnavailable") : undefined} onClick={onEdit}>{t("common.edit")}</button>
          <button className="button ghost provider-expand" aria-expanded={expanded} aria-controls={`provider-details-${provider.id}`} onClick={() => setExpanded((value) => !value)}>{expanded ? t("providers.collapseDetails") : t("providers.expandDetails")}</button>
          {provider.enabled ? <ConfirmButton className="button ghost" label={t("common.disable")} title={t("providers.disableTitle")} confirmLabel={t("providers.disableConfirm", { name: provider.name })} disabled={stateMutation.isPending} onConfirm={() => stateMutation.mutateAsync()} /> : <button className="button ghost" disabled={stateMutation.isPending} onClick={() => stateMutation.mutate()}>{t("common.enable")}</button>}
          <OverflowMenu label={t("providers.moreActions")}><ConfirmButton label={t("common.delete")} confirmLabel={t("providers.deleteProvider", { name: provider.name })} disabled={deleteMutation.isPending} requireStepUp onConfirm={(reauth) => deleteMutation.mutateAsync(reauth)} /></OverflowMenu>
        </div>
        {/* The reason belongs in the row that failed, not behind an expander:
            the operator is looking at the button they just pressed. */}
        {testState === "failure" && testFailureReason && (
          <p className="row-test-failure" role="status">{testFailureReason}</p>
        )}
        {expanded && <div id={`provider-details-${provider.id}`} className="provider-row-content provider-expanded-content">
          <div className="provider-facts">
            <div><small>{t("providers.endpoint")}</small><strong>{provider.base_url}</strong></div>
            <div><small>{t("providers.boundCredential")}</small>{credential ? <button className="resource-link" onClick={onCredentialClick}>{credential.name}</button> : <strong>{t("providers.missingCredential")}</strong>}</div>
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
      {deleteMutation.isError && !isStepUpPrompt(deleteMutation.error) && <ErrorState error={deleteMutation.error} />}
      {stateMutation.isError && <ErrorState error={stateMutation.error} />}
    </>
  );
}

function CredentialRow({ credential, useCount, highlighted, catalog, onUsageClick }: { credential: Credential; useCount: number; highlighted: boolean; catalog?: ProviderProfilesCatalog; onUsageClick: () => void }) {
  const { t } = useTranslation();
  const [rotating, setRotating] = useState(false);
  const [expanded, setExpanded] = useState(false);
  const queryClient = useQueryClient();
  const dateTime = useInstantFormatter();
  const displayBaseURL = displayBoundBaseURL(credential.bound_base_url);
  const expiry = credentialExpiry(credential.expires_at);
  const { notify } = useNotify();
  const remove = useMutation({
    mutationFn: (reauth: ReauthValues) => api.deleteCredential(credential.id, credential.revision, reauth),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["credentials"] });
      notify({ tone: "success", title: t("providers.notifyCredentialDeleted"), description: credential.name });
    },
  });
  return (
    <>
      <article className={`credential-row ${highlighted ? "resource-highlight" : ""}`}>
        <span className="provider-icon credential-icon" aria-hidden="true">K{credential.key_version}</span>
        <div className="resource-identity credential-compact-identity">
          <strong>{credential.name}</strong>
          <small>{t(`providers.types.${credential.type}`)}</small>
          {/* Only stated once there is something to state: a secret with no
              declared end says nothing here, and the two cases worth acting on
              carry their own tone. */}
          {expiry && (
            <small className={`credential-expiry${expiry.expired ? " expired" : expiry.soon ? " expiring" : ""}`}>
              {expiry.expired
                ? t("providers.credentialExpired", { date: dateTime(credential.expires_at, "date") })
                : t("providers.credentialExpiresIn", { count: expiry.days, date: dateTime(credential.expires_at, "date") })}
            </small>
          )}
          <code className="credential-identity-endpoint">{displayBaseURL}</code>
        </div>
        <div className="resource-fact credential-endpoint"><small>{t("providers.boundURL")}</small><strong>{displayBaseURL}</strong></div>
        <div className="resource-fact credential-usage"><small>{t("providers.usage")}</small>{useCount > 0 ? <button className="resource-link" onClick={onUsageClick}>{t("providers.credentialUsage", { count: useCount })} →</button> : <strong>{t("providers.credentialUsage", { count: useCount })}</strong>}</div>
        <div className="resource-fact credential-generation"><small>{t("providers.generation")}</small><strong>{t("providers.keyGeneration", { version: credential.key_version })}</strong></div>
        <div className="row-actions credential-actions">
          {/* Rotating opens the same form, which needs the matrix; without it the
              click would set state and render nothing. Say so on the button
              rather than letting it look broken. */}
          <button className="button ghost" disabled={!catalog} title={!catalog ? t("providers.matrixUnavailable") : undefined} onClick={() => setRotating(true)}>{t("providers.rotate")}</button>
          <button className="button ghost credential-expand" aria-expanded={expanded} aria-controls={`credential-details-${credential.id}`} onClick={() => setExpanded((value) => !value)}>{expanded ? t("providers.collapseDetails") : t("providers.expandDetails")}</button>
          <OverflowMenu label={t("providers.moreActions")}><ConfirmButton label={t("common.delete")} confirmLabel={useCount > 0
              ? t("providers.deleteCredentialInUse", { name: credential.name, count: useCount })
              : t("providers.deleteCredential", { name: credential.name })} disabled={remove.isPending} requireStepUp onConfirm={(reauth) => remove.mutateAsync(reauth)} /></OverflowMenu>
        </div>
        {expanded && <section id={`credential-details-${credential.id}`} className="credential-expanded-content" aria-label={t("providers.credentialDetailsTitle")}>
          <header className="credential-detail-header">
            <div><small>{t("providers.technicalDetails")}</small><strong>{t("providers.credentialDetailsTitle")}</strong></div>
            <p>{t("providers.credentialDetailsDescription")}</p>
          </header>
          <dl className="credential-detail-grid">
            <div><dt>{t("providers.normalizedBoundURL")}</dt><dd><code>{credential.bound_base_url}</code></dd></div>
            <div><dt>{t("providers.surface")}</dt><dd><code>{credential.access_surface}</code></dd></div>
            <div><dt>{t("providers.scheme")}</dt><dd><code>{credential.scheme}</code></dd></div>
            <div><dt>{t("providers.credentialID")}</dt><dd><code>{credential.id}</code></dd></div>
            <div><dt>{t("providers.credentialExpiry")}</dt><dd>{credential.expires_at ? dateTime(credential.expires_at, "dateTimeYear") : t("providers.credentialNeverExpires")}</dd></div>
          </dl>
        </section>}
      </article>
      {remove.isError && !isStepUpPrompt(remove.error) && <ErrorState error={remove.error} />}
      {rotating && catalog && <CredentialForm current={credential} catalog={catalog} onClose={() => setRotating(false)} />}
    </>
  );
}

function evidenceSummary(evidence: Record<string, string>) {
  const values = [...new Set(Object.values(evidence).filter((value) => value !== "unsupported"))];
  return values.length ? values.join(" / ") : "—";
}

// Read from the connection's own record rather than from the served matrix, so
// the listing keeps working when that request has not landed. Only the forms
// need to know what may be turned on; showing what a saved connection already
// has needs nothing but the connection.
function enabledCapabilities(provider: Provider) {
  const capabilities = provider.capabilities;
  if (!capabilities) return [];
  return (Object.keys(capabilities) as (keyof ProviderCapabilities)[]).filter(
    (capability) => capability !== "max_context_tokens" && capability !== "max_output_tokens" && capabilities[capability],
  );
}

function CredentialForm({
  current,
  catalog,
  onClose,
}: {
  current?: Credential;
  catalog: ProviderProfilesCatalog;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const { notify } = useNotify();
  const [name, setName] = useState(current?.name ?? "");
  const [type, setType] = useState<ProviderType>(current?.type ?? "openai");
  const [baseURL, setBaseURL] = useState(
    current ? displayBoundBaseURL(current.bound_base_url) : endpointForType(catalog, "openai"),
  );
  const [secret, setSecret] = useState("");
  // datetime-local has no zone of its own. Read and written in the accounting
  // zone, which is the zone every timestamp the console displays is rendered in
  // — including this credential's expiry in the row behind the form. Reading it
  // as the browser's wall clock made the form and the row disagree by the offset
  // between the two zones.
  const timeZone = useAccountingTimeZone();
  const [expiresAt, setExpiresAt] = useState(isoToZonedInput(current?.expires_at, timeZone));
  // Replacing the material an existing credential holds is the same
  // trust-boundary change as deleting it; the server asks on both. Creating one
  // establishes new material and does not ask. Asked on demand: inside the
  // re-authentication window the server is already satisfied, so the fields
  // appear only if this rotation comes back asking for them.
  const stepUp = useStepUpPrompt();
  const queryClient = useQueryClient();
  const mutation = useMutation({
    mutationFn: () => {
      // Sent on creation only. A rotation leaves the pair out so the server keeps
      // whatever the credential was sealed to — including a surface this build no
      // longer offers, which must be deleted rather than silently re-pointed at
      // the one that is.
      const bedrockBinding = type === "bedrock" && !current
        ? bedrockCredentialProfile(catalog)
        : undefined;
      const value = {
        name,
        type,
        base_url: baseURL,
        ...(bedrockBinding
          ? { access_surface: bedrockBinding.access_surface, scheme: bedrockBinding.credential_scheme }
          : {}),
        ...(secret ? { secret } : {}),
        // Always sent, including as null: the stored expiry is whatever the
        // form says, so clearing the field clears it rather than silently
        // keeping the old date through a rotation.
        expires_at: zonedInputToISO(expiresAt, timeZone) || null,
      };
      return current
        ? api.rotateCredential(current.id, value, current.revision, stepUp.values)
        : api.createCredential({ ...value, secret });
    },
    onMutate: stepUp.begin,
    // The typed secret survives the console's own step-up question, and only
    // that. Clearing it there would leave the retry rotating to nothing, having
    // silently thrown away material the operator cannot retype from memory.
    onError: (error) => { if (!stepUp.absorb(error)) setSecret(""); },
    onSuccess: () => {
      setSecret("");
      queryClient.invalidateQueries({ queryKey: ["credentials"] });
      notify({ tone: "success", title: t(current ? "providers.notifyCredentialRotated" : "providers.notifyCredentialSaved"), description: name });
      onClose();
    },
  });
  // Same reason as the provider form below: the rejection must reach the
  // operator who clicked, not sit in a scrolled-away part of the modal.
  const submitError = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!mutation.isError || stepUp.probing) return;
    requestAnimationFrame(() => {
      submitError.current?.scrollIntoView?.({ block: "center" });
      submitError.current?.focus();
    });
  }, [mutation.isError, mutation.error, stepUp.probing]);
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (name.trim() && baseURL.trim() && (current || secret) && (!stepUp.asked || stepUp.values.currentPassword)) mutation.mutate();
  };
  const dirty = useDirty({ name, type, baseURL, secret, expiresAt });
  return (
    <Modal title={current ? t("providers.rotateCredential") : t("providers.saveCredential")} dirty={dirty} onClose={onClose}>
      {/* Like the other modal forms: the form drops the modal's margin so the
          footer can stick to both edges, and the body carries the padding. */}
      <form className="provider-credential-form" onSubmit={submit} autoComplete="off">
        <div className="provider-credential-form-body">
        <Field label={t("providers.credentialName")}><input autoComplete="off" autoFocus value={name} onChange={(event) => setName(event.target.value)} /></Field>
        <Field label={t("providers.providerType")}>
          <select value={type} disabled={Boolean(current)} onChange={(event) => {
            const next = event.target.value as ProviderType;
            setType(next);
            setBaseURL(endpointForType(catalog, next));
          }}>
            <ProviderTypeOptions t={t} />
          </select>
        </Field>
        <Field label={t("providers.boundURL")} hint={t("providers.boundURLHint")}>
          <input autoComplete="off" inputMode="url" value={baseURL} onChange={(event) => setBaseURL(event.target.value)} />
        </Field>
        <Field
          label={current ? t("providers.newSecret") : t("providers.providerSecret")}
          hint={current
            ? t("providers.secretConfigured")
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
        <Field label={t("providers.credentialExpiry")} hint={t("providers.credentialExpiryHint")}>
          <input autoComplete="off" type="datetime-local" value={expiresAt} onChange={(event) => setExpiresAt(event.target.value)} />
        </Field>
        {mutation.isError && !stepUp.probing && (
          <div ref={submitError} tabIndex={-1} className="form-submit-error"><ErrorState error={mutation.error} /></div>
        )}
        {stepUp.asked && <ReauthFields values={stepUp.values} onChange={stepUp.setValues} description={t("auth.stepUpSecurityControl")} />}
        </div>
        <div className="form-actions sticky-form-actions">
          <button type="button" className="button ghost" onClick={onClose}>{t("common.cancel")}</button>
          <button className="button primary" disabled={mutation.isPending || (!current && !secret) || (stepUp.asked && !stepUp.values.currentPassword)}>
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
  catalog,
  onClose,
}: {
  current?: Provider;
  credentials: Credential[];
  catalog: ProviderProfilesCatalog;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const { notify } = useNotify();
  const initialType = current?.type ?? "openai";
  const [name, setName] = useState(current?.name ?? "");
  const [type, setType] = useState<ProviderType>(initialType);
  const bedrockDefaultProfile = defaultProfileID(catalog, "bedrock");
  const initialProfile = current?.profile_id && isBedrockProfile(catalog, current.profile_id)
    ? current.profile_id
    : bedrockDefaultProfile;
  const [profileID, setProfileID] = useState(initialProfile);
  const [baseURL, setBaseURL] = useState(current?.base_url ?? endpointForType(catalog, initialType));
  const [apiVersion, setAPIVersion] = useState(current?.api_version ?? "");
  const [bedrockProjectID, setBedrockProjectID] = useState(current?.bedrock_project_id ?? "");
  const [anthropicBetas, setAnthropicBetas] = useState((current?.allowed_anthropic_betas ?? []).join(", "));
  const [maxConcurrency, setMaxConcurrency] = useState(current?.max_concurrency ?? 0);
  const [enabled, setEnabled] = useState(current?.enabled ?? true);
  const [capabilities, setCapabilities] = useState<ProviderCapabilities>(
    current?.capabilities ?? connectionDefaults(catalog, initialType, defaultProfileID(catalog, initialType)),
  );
  // Both come from the server. The ceiling is what an operator may turn on, the
  // defaults are what a new connection starts with, and they are different
  // questions — provider_executed_tools sits above the defaults and inside the
  // ceiling, because the profile supports it and enabling it accepts upstream
  // egress Halro never sees.
  const anchorProfile = type === "bedrock" ? profileID : defaultProfileID(catalog, type);
  const capabilityCeiling = connectionCeiling(catalog, type, anchorProfile);
  const capabilityNames = booleanCapabilityNames(catalog);
  // A profile whose set is fixed by the build offers no checkboxes to widen.
  const fixedCapabilities = combinableProfiles(catalog, type, anchorProfile).every((profile) => profile.immutable);
  const visibleCapabilities = capabilityNames.filter((capability) => capabilities[capability]);
  const configurableCapabilities = capabilityNames.filter((capability) => capabilityCeiling[capability] || capabilities[capability]);
  const selectedSurface = type === "bedrock" ? findProfile(catalog, "bedrock", profileID)?.access_surface : undefined;
  // What is ticked that this connection cannot serve. The server refuses these
  // too, and names them; catching it here points at the checkbox instead.
  const unservable = unservableCapabilities(catalog, type, anchorProfile, capabilities);
  // Ticked capabilities whose consequence is not visible in a checkbox.
  const warnedCapabilities = capabilityNames.filter(
    (capability) => capabilities[capability] && capabilityNeedsOptInWarning(catalog, capability),
  );
  // The header is only ever sent by the native Anthropic Messages path, which is
  // a property of the profile rather than the surface: Bedrock Mantle also
  // carries OpenAI chat and responses profiles, and a token stored on one of
  // those would be kept and never sent.
  const supportsAnthropicBetas = type === "anthropic" || profileID === "bedrock.mantle.anthropic.messages.v1";
  const matchingCredentials = credentials.filter((credential) => credential.type === type && (!selectedSurface || credential.access_surface === selectedSurface));
  const [credentialID, setCredentialID] = useState(
    current?.credential_id ??
      credentials.find((credential) =>
        credential.type === initialType &&
        (initialType !== "bedrock" ||
          credential.access_surface === findProfile(catalog, "bedrock", initialProfile)?.access_surface),
      )?.id ??
      "",
  );
  const [errors, setErrors] = useState<Record<string, string>>({});
  // A credential is encrypted against the endpoint it was saved for, so editing
  // the base URL afterwards silently invalidates the pairing. The server refuses
  // the save; without this the operator only learns that after a round-trip, and
  // from a message that names neither URL.
  const selectedCredential = matchingCredentials.find((credential) => credential.id === credentialID);
  const credentialBoundURL = selectedCredential ? displayBoundBaseURL(selectedCredential.bound_base_url) : "";
  const baseURLOrigin = urlOrigin(baseURL);
  const credentialBaseURLMismatch = credentialBoundURL && baseURLOrigin && credentialBoundURL !== baseURLOrigin
    ? t("providers.validationCredentialBaseURL")
    : "";
  const queryClient = useQueryClient();
  // One key per open form: a retry after a lost response reaches the same
  // record instead of creating a second one, while a deliberate second create
  // opens the form again and gets a new key.
  const idempotencyKey = useRef(crypto.randomUUID());
  const mutation = useMutation({
    mutationFn: () => {
      const value = {
      name, type, base_url: baseURL,
      ...(type === "bedrock" ? {
        profile_id: profileID,
        access_surface: findProfile(catalog, "bedrock", profileID)?.access_surface,
        credential_scheme: findProfile(catalog, "bedrock", profileID)?.credential_scheme,
        ...(selectedSurface === "bedrock-mantle"
          ? { bedrock_project_id: normalizeBedrockProjectID(bedrockProjectID) }
          : {}),
      } : {}),
      // One flat set. A connection can span more than one profile — an OpenAI key
      // serves both the chat endpoints and the media ones — and sorting the
      // ticked capabilities into a binding per profile is the server's job:
      // doing it here is what made this form's idea of the matrix a second
      // authority over what a connection may be.
      ...(type === "azure_openai" ? { api_version: apiVersion } : {}),
      // Token limits are left out deliberately, and zeroed rather than passed
      // through. They belong to the profile that declares one — only Titan Embed
      // does — and the connection's stored summary reports the loosest of them,
      // so echoing what was read back would hand one profile's bound to every
      // other one on the connection. The model's own limits are declared on the
      // Deployment.
      credential_id: credentialID,
      capabilities: { ...capabilities, max_context_tokens: 0, max_output_tokens: 0 },
      max_concurrency: maxConcurrency, enabled,
      ...(supportsAnthropicBetas ? { allowed_anthropic_betas: parseBetaTokens(anthropicBetas) } : {}),
      };
      return current
        ? api.updateProvider(current.id, value, current.revision)
        : api.createProvider(value, idempotencyKey.current);
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["providers"] });
      notify({ tone: "success", title: t(current ? "providers.notifyUpdated" : "providers.notifyCreated"), description: name });
      onClose();
    },
  });
  const dirty = useDirty({ name, type, profileID, baseURL, apiVersion, bedrockProjectID, anthropicBetas, maxConcurrency, enabled, capabilities, credentialID });
  // The save button sits in a sticky footer while the form scrolls behind it,
  // so a rejection renders into the part of the modal the operator is not
  // looking at: the click appears to do nothing and they click again. Bring the
  // failure into view and move focus onto it, so the reason is announced rather
  // than merely present.
  const submitError = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!mutation.isError) return;
    requestAnimationFrame(() => {
      submitError.current?.scrollIntoView?.({ block: "center" });
      submitError.current?.focus();
    });
  }, [mutation.isError, mutation.error]);
  // A save the form itself refuses has the same problem, and the field that
  // failed already says why. Take the operator to that field rather than adding
  // a second, redundant sentence at the foot of the form.
  //
  // Keyed on the submit that produced the errors rather than on the errors
  // themselves: clearing one field's error is a keystroke in that field, and
  // re-running on it moved focus to the next still-invalid control mid-word, so
  // the rest of what was being typed landed somewhere else.
  const formElement = useRef<HTMLFormElement>(null);
  const [refusedSubmits, setRefusedSubmits] = useState(0);
  useEffect(() => {
    if (refusedSubmits === 0) return;
    requestAnimationFrame(() => {
      const invalid = formElement.current?.querySelector<HTMLElement>("[aria-invalid='true']") ?? submitError.current;
      invalid?.scrollIntoView?.({ block: "center" });
      invalid?.focus();
    });
  }, [refusedSubmits]);
  return (
    <Modal wide title={current ? t("providers.editProvider") : t("providers.createProvider")} dirty={dirty} onClose={onClose}>
      {credentials.length === 0 ? (
        <div className="notice warning">
          <strong>{t("providers.credentialRequired")}</strong>
          <span>{t("providers.credentialRequiredDescription")}</span>
        </div>
      ) : (
        <form className="provider-form" ref={formElement}
          onSubmit={(event) => {
            event.preventDefault();
            const nextErrors = validateProvider({
              name, credentialID, bedrockProjectID,
              mantle: selectedSurface === "bedrock-mantle",
              anyCapability: anyCapabilityEnabled(catalog, capabilities),
              unservable,
              anthropicBetas: supportsAnthropicBetas ? anthropicBetas : "",
            }, t);
            if (!nextErrors.credentialID && credentialBaseURLMismatch) nextErrors.credentialID = credentialBaseURLMismatch;
            setErrors(nextErrors);
            if (Object.keys(nextErrors).length) setRefusedSubmits((value) => value + 1);
            else mutation.mutate();
          }}
        >
          <section className="provider-form-section" aria-labelledby="provider-connection-title">
            <header><h3 id="provider-connection-title">{t("providers.connectionSection")}</h3><p>{t("providers.connectionSectionDescription")}</p></header>
            <div className="form-grid">
          <Field label={t("providers.providerName")} error={errors.name}><input autoComplete="off" autoFocus value={name} onChange={(event) => { setName(event.target.value); setErrors((previous) => omitError(previous, "name")); }} /></Field>
          <Field label={t("providers.type")}>
            <select value={type} onChange={(event) => {
              const next = event.target.value as ProviderType;
              setType(next);
              setBaseURL(endpointForType(catalog, next));
              setProfileID(defaultProfileID(catalog, "bedrock"));
              setCredentialID(credentials.find((credential) => credential.type === next
                && (next !== "bedrock" || credential.access_surface === bedrockCredentialProfile(catalog)?.access_surface))?.id ?? "");
              setCapabilities(connectionDefaults(catalog, next, defaultProfileID(catalog, next)));
            }}>
              <ProviderTypeOptions t={t} />
            </select>
          </Field>
          {type === "bedrock" && (
            <Field label={t("providers.capabilityImplementation")} hint={t("providers.bedrockProfileHint")}>
              <select value={profileID} onChange={(event) => {
                const next = event.target.value;
                const profile = findProfile(catalog, "bedrock", next);
                setProfileID(next);
                setBaseURL(profile?.default_base_url ?? "");
                setCredentialID(credentials.find((credential) => credential.type === "bedrock" && credential.access_surface === profile?.access_surface)?.id ?? "");
                setCapabilities(connectionDefaults(catalog, "bedrock", next));
              }}>
                {profilesForType(catalog, "bedrock").map((profile) => (
                  <option value={profile.id} key={profile.id}>{t(`providers.bedrockProfiles.${profile.id}`)}</option>
                ))}
              </select>
            </Field>
          )}
          <Field label={t("providers.baseURL")} hint={credentialBaseURLMismatch ? t("providers.baseURLBoundHint", { credential: credentialBoundURL }) : undefined}>
            <input autoComplete="off" value={baseURL} onChange={(event) => { setBaseURL(event.target.value); setErrors((previous) => omitError(previous, "credentialID")); }} />
          </Field>
          {type === "azure_openai" && (
            <Field label={t("providers.apiVersion")} hint={t("providers.apiVersionHint")}>
              <input autoComplete="off" value={apiVersion} onChange={(event) => setAPIVersion(event.target.value)} required />
            </Field>
          )}
          {supportsAnthropicBetas && (
            <Field label={t("providers.anthropicBetas")} hint={t("providers.anthropicBetasHint")} error={errors.anthropicBetas}>
              <input autoComplete="off" value={anthropicBetas} placeholder={t("providers.anthropicBetasPlaceholder")} onChange={(event) => { setAnthropicBetas(event.target.value); setErrors((previous) => omitError(previous, "anthropicBetas")); }} />
            </Field>
          )}
          {selectedSurface === "bedrock-mantle" && (
            <>
              <Field label={t("providers.bedrockProject")} hint={t("providers.bedrockProjectHint")} error={errors.bedrockProjectID}>
                <input autoComplete="off" value={bedrockProjectID} placeholder={t("providers.bedrockProjectPlaceholder")} onChange={(event) => { setBedrockProjectID(event.target.value); setErrors((previous) => omitError(previous, "bedrockProjectID")); }} />
              </Field>
              {profileID === "bedrock.mantle.anthropic.messages.v1" && (
                <div className="notice warning">
                  <strong>{t("providers.billableProbe")}</strong>
                  <span>{t("providers.billableProbeDescription")}</span>
                </div>
              )}
            </>
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
                  <div className="capability-grid">{configurableCapabilities.map((capability) => { const unavailable = !capabilityCeiling[capability]; const warned = capabilityNeedsOptInWarning(catalog, capability); return <label className={`capability-option ${unavailable ? "unavailable" : ""}`} key={capability}><input type="checkbox" disabled={unavailable && !capabilities[capability]} checked={Boolean(capabilities[capability])} onChange={(event) => setCapabilities(updateCapabilitySelection(catalog, capabilities, capability, event.target.checked))} /><span>{t(`capabilities.${capability}`)}{unavailable && <small>{t("providers.unsupportedByInterface")}</small>}{!unavailable && warned && <small>{t("providers.capabilityEgressTag")}</small>}</span></label>; })}</div>
                  {/* Every other capability decides what Halro will relay. These
                      decide who else gets to make requests, and a checkbox row
                      shows nothing of that — so the consequence is stated where
                      it is accepted, in what it means rather than what it is
                      called. Which capabilities these are comes from the server. */}
                  {warnedCapabilities.length > 0 && (
                    <div className="notice warning">
                      <strong>{t("providers.capabilityEgressWarning")}</strong>
                      <span>{t("providers.capabilityEgressWarningDescription", {
                        capabilities: warnedCapabilities.map((capability) => t(`capabilities.${capability}`)).join(t("common.listSeparator")),
                      })}</span>
                    </div>
                  )}
                </div>
              )}
            </div>
          </section>
          <section className="provider-form-section" aria-labelledby="provider-capacity-title">
            <header><h3 id="provider-capacity-title">{t("providers.capacitySection")}</h3><p>{t("providers.capacitySectionDescription")}</p></header>
            <div className="form-grid">
          <Field label={t("providers.maxConcurrency")} hint={t("providers.maxConcurrencyHint")}>
            <input autoComplete="off"
              type="number"
              min="0"
              value={maxConcurrency}
              onChange={(event) => setMaxConcurrency(Number(event.target.value))}
            />
          </Field>
          <Field label={t("providers.encryptedCredential")} error={errors.credentialID || credentialBaseURLMismatch}>
            {/* The endpoint the credential is sealed to is what decides whether
                it can be used here, so it is in the option rather than a detail
                page the operator would have to leave the form to read. */}
            <select value={credentialID} onChange={(event) => { setCredentialID(event.target.value); setErrors((previous) => omitError(previous, "credentialID")); }}>
              {matchingCredentials.map((credential) => (
                <option value={credential.id} key={credential.id}>{credential.name} · {displayBoundBaseURL(credential.bound_base_url)}</option>
              ))}
            </select>
          </Field>
            </div>
          </section>
          {(mutation.isError || errors.capabilities) && (
            <div ref={submitError} tabIndex={-1} className="form-submit-error">
              {/* The one refusal with no field of its own to carry it. */}
              {errors.capabilities && <p role="alert">{errors.capabilities}</p>}
              {mutation.isError && <ErrorState error={mutation.error} />}
            </div>
          )}
          {/* Whether deployments may use this upstream is the state the save
              commits, so it belongs in the bar that commits it. */}
          <div className="form-actions sticky-form-actions">
            <div className="form-footer-state">
              <label className="form-footer-enable">
                <input type="checkbox" aria-label={t("providers.enable")} checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />
                <span>
                  <strong>{t("providers.enable")} · {enabled ? t("common.enabled") : t("common.disabled")}</strong>
                  <small>{enabled ? t("providers.enableDescription") : t("providers.disabledImpact")}</small>
                </span>
              </label>
            </div>
            <button type="button" className="button ghost" onClick={onClose}>{t("common.cancel")}</button>
            {/* Every refusal reason is reported by the submit path rather than
                by a disabled button, which states nothing about why. */}
            <button className="button primary" disabled={mutation.isPending}>{current ? t("providers.save") : t("providers.createAndLoad")}</button>
          </div>
        </form>
      )}
    </Modal>
  );
}

// The capability matrix used to be repeated here — which capabilities exist,
// what each provider starts with, what may be turned on, which profiles are
// fixed by the build. It is served now (see hooks/useProviderProfiles), so the
// only thing this file decides about capabilities is how to draw them.

// Mirrors domain.NormalizeBedrockProjectID: `default` is AWS's name for the
// account default project, which is what an empty value already means.
function normalizeBedrockProjectID(value: string) {
  const trimmed = value.trim();
  return trimmed === "default" ? "" : trimmed;
}

// Mirrors domain.MaxBedrockProjectIDLength.
const maxBedrockProjectIDLength = 128;

// The same rules the Admin API enforces, applied where the operator can still
// see which field is wrong. The server stays the authority; this only keeps a
// refusal from arriving as a bare 400 after the modal has scrolled away.
function validateProvider(
  value: {
    name: string; credentialID: string; bedrockProjectID: string; mantle: boolean;
    anyCapability: boolean; unservable: string[]; anthropicBetas: string;
  },
  t: ReturnType<typeof useTranslation>["t"],
): Record<string, string> {
  const errors: Record<string, string> = {};
  if (!value.name.trim()) errors.name = t("providers.validationNameRequired");
  if (!value.credentialID) errors.credentialID = t("providers.validationCredentialRequired");
  if (!value.anyCapability) errors.capabilities = t("providers.validationCapabilityRequired");
  // Refusing here rather than on the round trip: the server rejects a capability
  // no profile can serve, but its refusal cannot say which one the operator
  // ticked, and the form can.
  if (value.unservable.length) {
    errors.capabilities = t("providers.validationCapabilityUnservable", {
      capabilities: value.unservable.map((name) => t(`capabilities.${name}`)).join(t("common.listSeparator")),
    });
  }
  if (value.mantle) {
    const projectID = normalizeBedrockProjectID(value.bedrockProjectID);
    if (projectID.length > maxBedrockProjectIDLength) {
      errors.bedrockProjectID = t("providers.validationProjectTooLong", { max: maxBedrockProjectIDLength });
    } else if (projectID.startsWith("wrkspc_")) {
      errors.bedrockProjectID = t("providers.validationProjectWorkspace");
    } else if (projectID !== "" && !/^proj_[A-Za-z0-9]+$/.test(projectID)) {
      errors.bedrockProjectID = t("providers.validationProjectFormat");
    }
  }
  const betas = parseBetaTokens(value.anthropicBetas);
  if (betas.length > maxAnthropicBetaTokens) {
    errors.anthropicBetas = t("providers.validationBetaTooMany", { max: maxAnthropicBetaTokens });
  } else if (betas.some((token) => token.length > maxAnthropicBetaTokenLength)) {
    errors.anthropicBetas = t("providers.validationBetaTooLong", { max: maxAnthropicBetaTokenLength });
  } else if (betas.some((token) => !/^[a-z0-9._-]+$/.test(token))) {
    errors.anthropicBetas = t("providers.validationBetaCharset");
  } else if (new Set(betas).size !== betas.length) {
    errors.anthropicBetas = t("providers.validationBetaDuplicate");
  }
  return errors;
}

// Mirrors domain.MaxAnthropicBetaTokens and MaxAnthropicBetaTokenLength.
const maxAnthropicBetaTokens = 16;
const maxAnthropicBetaTokenLength = 128;

function omitError(errors: Record<string, string>, key: string) {
  if (!(key in errors)) return errors;
  const { [key]: _removed, ...rest } = errors;
  return rest;
}
