import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState, type FormEvent, type KeyboardEvent } from "react";
import { api, ApiError } from "../api";
import {
  ConfirmButton,
  EmptyState,
  ErrorState,
  Field,
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
import type { AccessSurface, Credential, CredentialScheme, Deployment, Provider, ProviderCapabilities, ProviderEgressCatalog, ProviderEgressProxy, ProviderProfilesCatalog, ProviderType } from "../types";
import {
  anyCapabilityEnabled,
  booleanCapabilityNames,
  capabilityNeedsOptInWarning,
  combinableProfiles,
  connectionCeiling,
  connectionChoices,
  connectionDefaults,
  credentialIdentities,
  defaultProfileID,
  endpointHost,
  findOffering,
  findProfile,
  offeringDocumentation,
  offeringsForType,
  profilesForType,
  regionForEndpoint,
  unservableCapabilities,
  updateCapabilitySelection,
  useProviderProfiles,
  type ConnectionChoice,
  type CredentialIdentity,
} from "../hooks/useProviderProfiles";
import { useTranslation } from "react-i18next";
import { useIsReadOnly } from "../session";
import { hasOnboardingCreateIntent, OnboardingContextBanner } from "../OnboardingContext";

const providerTypes: ProviderType[] = [
  "openai", "anthropic", "azure_openai", "deepseek", "gemini", "bedrock", "minimax", "kimi", "bigmodel", "openai_compatible",
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

function validProviderEndpoint(value: string) {
  try {
    const parsed = new URL(value.trim());
    return (parsed.protocol === "http:" || parsed.protocol === "https:")
      && Boolean(parsed.hostname)
      && !parsed.username
      && !parsed.password;
  } catch {
    return false;
  }
}

function regionFromEndpointTemplate(template: string | undefined, endpoint: string) {
  if (!template?.includes("{region}")) return undefined;
  const [prefix, suffix] = template.split("{region}");
  if (!endpoint.startsWith(prefix) || !endpoint.endsWith(suffix)) return "";
  return endpoint.slice(prefix.length, endpoint.length - suffix.length);
}

function endpointFromRegion(template: string, region: string) {
  return template.replace("{region}", region);
}

function validProviderRegion(region: string) {
  return /^[a-z0-9]+(?:-[a-z0-9]+)+$/.test(region);
}

function validProxyEndpoint(value: string) {
  try {
    const parsed = new URL(value.trim());
    return (parsed.protocol === "http:" || parsed.protocol === "https:")
      && Boolean(parsed.hostname) && Boolean(parsed.port)
      && !parsed.username && !parsed.password
      && parsed.pathname === "/" && !parsed.search && !parsed.hash;
  } catch {
    return false;
  }
}

type ProviderView = "providers" | "credentials" | "proxies";

function fixedRegionEndpointStatus(
  catalog: ProviderProfilesCatalog,
  type: ProviderType,
  offeringID: string,
  regionID: string,
  baseURL: string,
): "match" | "cross_region" | "unknown" {
  const host = endpointHost(baseURL);
  if (!host) return "unknown";
  const published = profilesForType(catalog, type).filter((profile) =>
    profile.offering_id === offeringID && endpointHost(profile.default_base_url) === host);
  if (published.some((profile) => profile.region_id === regionID)) return "match";
  return published.length ? "cross_region" : "unknown";
}

function usagePolicyDocument(
  offering: ReturnType<typeof findOffering>,
  regionID: string,
) {
  const exact = offeringDocumentation(offering, regionID);
  if (exact) return exact;
  if (!regionID && offering?.region_scope === "by_endpoint" && offering.documentation.length === 1) {
    return offering.documentation[0];
  }
  return undefined;
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

// The label for one product identity or connection choice.
//
// Built from the served identifiers and translated by them, so adding a product
// upstream adds a row here without this file being edited. The product name is
// dropped where the type has only one — it would repeat the type — and the
// region is dropped where the product has none.
function productLabel(
  t: ReturnType<typeof useTranslation>["t"],
  catalog: ProviderProfilesCatalog,
  type: ProviderType,
  offeringID: string,
  regionID: string,
): string {
  const parts: string[] = [];
  if (offeringsForType(catalog, type).length > 1) {
    parts.push(t(`providers.offerings.${offeringID}`, { defaultValue: offeringID }));
  }
  if (regionID) parts.push(t(`providers.regions.${regionID}`, { defaultValue: regionID }));
  return parts.join(" · ");
}

// The same label where an empty one would leave a blank option. Two identities
// of one product with no region between them is not a shape any platform has
// today; if one arrives, the operator sees the identifiers rather than a control
// with nothing in it.
function productOptionLabel(
  t: ReturnType<typeof useTranslation>["t"],
  catalog: ProviderProfilesCatalog,
  type: ProviderType,
  offeringID: string,
  regionID: string,
  fallback: string,
): string {
  return productLabel(t, catalog, type, offeringID, regionID) || fallback;
}

// Keep the required action visible without letting the full terms dominate the
// form. Native details/summary supplies keyboard and screen-reader disclosure
// behaviour; the expanded body holds the regional source and acknowledgement.
function SubscriptionUsageDisclosure({
  acknowledged,
  documentationURL,
  identityUnverified = false,
  attentionKey = 0,
  disabled = false,
  onAcknowledgedChange,
}: {
  acknowledged: boolean;
  documentationURL?: string;
  identityUnverified?: boolean;
  attentionKey?: number;
  disabled?: boolean;
  onAcknowledgedChange: (acknowledged: boolean) => void;
}) {
  const { t } = useTranslation();
  const detailsRef = useRef<HTMLDetailsElement>(null);
  const checkboxRef = useRef<HTMLInputElement>(null);
  useEffect(() => {
    if (!attentionKey) return;
    if (detailsRef.current) detailsRef.current.open = true;
    if (disabled) return;
    requestAnimationFrame(() => checkboxRef.current?.focus());
  }, [attentionKey, disabled]);
  return (
    <details ref={detailsRef} className={`subscription-usage-disclosure${acknowledged ? " confirmed" : ""}`}>
      <summary>
        <span className="subscription-usage-icon" aria-hidden="true">!</span>
        <span className="subscription-usage-summary-copy">
          <strong>{t("providers.usageWarningTitle")}</strong>
          <small>{t(acknowledged
            ? "providers.usageWarningConfirmedHint"
            : identityUnverified
              ? "providers.usageIdentityUnverifiedCollapsedHint"
              : "providers.usageWarningCollapsedHint")}</small>
        </span>
        <span className="subscription-usage-state">
          {t(acknowledged ? "providers.usageWarningConfirmed" : "providers.usageWarningNeedsConfirmation")}
        </span>
        <span className="subscription-usage-chevron" aria-hidden="true">›</span>
      </summary>
      <div className="subscription-usage-body" role="note">
        <p>{t("providers.usageWarningDescription")}</p>
        {identityUnverified && <p className="warning-text">{t("providers.usageIdentityUnverified")}</p>}
        {documentationURL && (
          <a href={documentationURL} target="_blank" rel="noreferrer">
            {t("providers.usageWarningDocumentation")} <span aria-hidden="true">↗</span>
          </a>
        )}
        <label className="subscription-usage-acknowledgement">
          <input
            ref={checkboxRef}
            type="checkbox"
            required
            disabled={disabled}
            checked={acknowledged}
            onChange={(event) => onAcknowledgedChange(event.target.checked)}
            onInvalid={(event) => {
              const input = event.currentTarget;
              if (detailsRef.current) detailsRef.current.open = true;
              requestAnimationFrame(() => input.focus());
            }}
          />
          <span>{t("providers.usageWarningAcknowledge")}</span>
        </label>
      </div>
    </details>
  );
}

export function ProvidersPage() {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const [activeView, setActiveView] = useState<ProviderView>(() => providerViewFromURL());
  const [focusedCredentialID, setFocusedCredentialID] = useState("");
  const [focusedProviderCredentialID, setFocusedProviderCredentialID] = useState("");
  const createFromOnboarding = hasOnboardingCreateIntent();
  const [credentialDialog, setCredentialDialog] = useState(() => !readOnly && createFromOnboarding && providerViewFromURL() === "credentials");
  const [providerDialog, setProviderDialog] = useState(() => !readOnly && createFromOnboarding && providerViewFromURL() === "providers");
  const [editingProvider, setEditingProvider] = useState<Provider>();
  const [proxyDialog, setProxyDialog] = useState(false);
  const [editingProxy, setEditingProxy] = useState<ProviderEgressProxy>();
  const [providerQuery, setProviderQuery] = useState("");
  const [providerStatus, setProviderStatus] = useState<"all" | "enabled" | "disabled">("all");
  const [credentialQuery, setCredentialQuery] = useState("");
  const credentials = useQuery({ queryKey: ["credentials"], queryFn: api.credentials });
  const providers = useQuery({ queryKey: ["providers"], queryFn: api.providers });
  const deployments = useQuery({ queryKey: ["deployments"], queryFn: api.deployments });
  const egress = useQuery({ queryKey: ["provider-egress-proxies"], queryFn: api.providerEgressProxies });
  // What this build can serve. The forms cannot decide what to offer without it,
  // so they wait for it; the listing below does not, and stays readable either
  // way.
  const catalog = useProviderProfiles();
  const pending = credentials.isPending || providers.isPending || deployments.isPending || egress.isPending || catalog.isPending;
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
  const selectView = (view: ProviderView) => {
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
    const views: ProviderView[] = ["providers", "credentials", "proxies"];
    const current = views.indexOf(activeView);
    const next = event.key === "Home" ? views[0]
      : event.key === "End" ? views[views.length - 1]
        : views[(current + (event.key === "ArrowLeft" ? -1 : 1) + views.length) % views.length];
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
            : activeView === "credentials"
              ? <button className="button primary" disabled={readOnly || catalog.isError} title={catalog.isError ? t("providers.matrixUnavailable") : undefined} onClick={() => setCredentialDialog(true)}>{t("providers.addCredential")}</button>
              : <button className="button primary" disabled={readOnly} onClick={() => setProxyDialog(true)}>{t("providers.addProxy")}</button>
        }
      />
      <OnboardingContextBanner />
      {pending && <Loading />}
      {/* The forms need the matrix and there is no offline copy of it, so a
          failed fetch is the difference between editing connections and not.
          That makes a retry part of the message rather than something the
          operator has to reload the page to reach — a session that expired
          mid-visit comes back on one click. */}
      {(credentials.isError || providers.isError || deployments.isError || egress.isError || catalog.isError) && (
        <ErrorState
          error={credentials.error || providers.error || deployments.error || egress.error || catalog.error}
          action={
            <button
              className="button ghost"
              disabled={credentials.isFetching || providers.isFetching || deployments.isFetching || egress.isFetching || catalog.isFetching}
              onClick={() => {
                if (credentials.isError) credentials.refetch();
                if (providers.isError) providers.refetch();
                if (deployments.isError) deployments.refetch();
                if (egress.isError) egress.refetch();
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
            <button id="proxies-tab" role="tab" tabIndex={activeView === "proxies" ? 0 : -1} aria-selected={activeView === "proxies"} aria-controls="proxies-panel" onKeyDown={handleTabKey} onClick={() => selectView("proxies")}>{t("providers.egressProxies")} <span>{egress.data?.items.length ?? 0}</span></button>
          </div>
          {activeView === "providers" && <section id="providers-panel" role="tabpanel" aria-labelledby="providers-tab" className="panel provider-resource-panel">
            {!canCreateProvider && (
			  <div className="dependency-notice"><div><strong>{t("providers.credentialRequired")}</strong><span>{t("providers.providerDependencyHint")}</span></div><button className="button secondary" disabled={readOnly} title={readOnly ? t("navigation.readOnlyAction") : undefined} onClick={() => { selectView("credentials"); setCredentialDialog(true); }}>{t("providers.openCredentialVault")}</button></div>
            )}
            {providerItems.length === 0 && canCreateProvider && (
              <EmptyState title={t("providers.noProviders")}>{t("providers.noProvidersDescription")}</EmptyState>
            )}
            {!!providerItems.length && <ResourceToolbar query={providerQuery} onQueryChange={setProviderQuery} queryPlaceholder={t("providers.searchProviders")} count={t("providers.resultCount", { visible: filteredProviders.length, total: providerItems.length })} status={providerStatus} onStatusChange={setProviderStatus} />}
            {!!providerItems.length && !filteredProviders.length && <EmptyState title={t("providers.noMatches")}>{t("providers.noMatchesDescription")}</EmptyState>}
            {filteredProviders.map((provider) => (
              <ProviderRow
                provider={provider}
                credential={credentialItems.find((credential) => credential.id === provider.credential_id)}
                catalog={catalog.data}
                egress={egress.data}
				probeDeploymentID={providerProbeDeploymentID(provider, deployments.data?.items ?? [])}
                highlighted={Boolean(focusedProviderCredentialID && provider.credential_id === focusedProviderCredentialID)}
                key={provider.id}
                onCredentialClick={() => { setFocusedCredentialID(provider.credential_id); selectView("credentials"); }}
                onEdit={() => setEditingProvider(provider)}
              />
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
          {activeView === "proxies" && egress.isSuccess && <section id="proxies-panel" role="tabpanel" aria-labelledby="proxies-tab" className="panel provider-resource-panel">
            {egress.data.items.length === 0 && <EmptyState title={t("providers.noProxies")}>{t("providers.noProxiesDescription")}</EmptyState>}
            {egress.data.items.map((proxy) => (
              <ProviderEgressProxyRow key={proxy.id} proxy={proxy} providers={providerItems} onEdit={() => setEditingProxy(proxy)} />
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
      {providerDialog && credentials.isSuccess && deployments.isSuccess && egress.isSuccess && catalog.isSuccess && (
        <ProviderForm
          credentials={credentials.data?.items ?? []}
          catalog={catalog.data}
          egress={egress.data}
          enabledDeploymentCount={0}
          onClose={() => setProviderDialog(false)}
        />
      )}
      {editingProvider && deployments.isSuccess && egress.isSuccess && catalog.isSuccess && (
        <ProviderForm
          current={editingProvider}
          credentials={credentials.data?.items ?? []}
          catalog={catalog.data}
          egress={egress.data}
          enabledDeploymentCount={(deployments.data?.items ?? []).filter((deployment) => deployment.provider_id === editingProvider.id && deployment.enabled).length}
          onClose={() => setEditingProvider(undefined)}
        />
      )}
      {proxyDialog && <ProviderEgressProxyForm onClose={() => setProxyDialog(false)} />}
      {editingProxy && <ProviderEgressProxyForm current={editingProxy} onClose={() => setEditingProxy(undefined)} />}
    </div>
  );
}

function providerViewFromURL(): ProviderView {
  const view = new URLSearchParams(window.location.search).get("view");
  return view === "credentials" || view === "proxies" ? view : "providers";
}

function providerProbeDeploymentID(provider: Provider, deployments: Deployment[]) {
  const bindings = provider.bindings?.filter((binding) => binding.enabled) ?? [];
  if (bindings.length > 1) return undefined;
  const binding = bindings[0];
  return deployments.find((deployment) => deployment.provider_id === provider.id
    && !deployment.enabled
    && (!binding || deployment.binding_id === binding.id || !deployment.binding_id && deployment.profile_id === binding.profile_id))?.id;
}

function ProviderEgressProxyRow({ proxy, providers, onEdit }: { proxy: ProviderEgressProxy; providers: Provider[]; onEdit: () => void }) {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const queryClient = useQueryClient();
  const { notify } = useNotify();
  const useCount = providers.filter((provider) => provider.egress_proxy_id === proxy.id).length;
  const deletion = useMutation({
    mutationFn: (reauth: ReauthValues) => api.deleteProviderEgressProxy(proxy.id, proxy.revision, reauth),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["provider-egress-proxies"] });
      queryClient.invalidateQueries({ queryKey: ["providers"] });
      notify({ tone: "success", title: t("providers.notifyProxyDeleted"), description: proxy.name });
    },
  });
  return (
    <article className="provider-egress-proxy-row">
      <div className="resource-identity proxy-identity"><span><StatusDot ok /><strong>{proxy.name}</strong></span><small>HTTP CONNECT</small></div>
      <div className="resource-fact proxy-endpoint"><small>{t("providers.proxyEndpoint")}</small><code title={proxy.endpoint}>{proxy.endpoint}</code></div>
      <div className="resource-fact proxy-authentication"><small>{t("providers.proxyAuthentication")}</small><strong>{proxy.authenticated ? t("providers.proxyBasicAuth") : t("providers.proxyNoAuth")}</strong></div>
      <div className="resource-fact proxy-usage"><small>{t("providers.usage")}</small><strong>{t("providers.proxyUsage", { count: useCount })}</strong></div>
      <div className="row-actions proxy-actions">
        <button className="button ghost" disabled={readOnly} onClick={onEdit}>{t("common.edit")}</button>
        <ConfirmButton
          className="button ghost"
          label={t("common.delete")}
          confirmLabel={t("providers.deleteProxy", { name: proxy.name })}
          disabled={readOnly || useCount > 0 || deletion.isPending}
          requireStepUp
          onConfirm={(reauth) => deletion.mutateAsync(reauth)}
        />
      </div>
      {deletion.isError && <div className="proxy-feedback"><ErrorState error={deletion.error} /></div>}
    </article>
  );
}

function ProviderEgressProxyForm({ current, onClose }: { current?: ProviderEgressProxy; onClose: () => void }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const { notify } = useNotify();
  const stepUp = useStepUpPrompt();
  const idempotencyKey = useRef(`provider-egress-${crypto.randomUUID()}`);
  const [name, setName] = useState(current?.name ?? "");
  const [endpoint, setEndpoint] = useState(current?.endpoint ?? "https://");
  const [allowPrivate, setAllowPrivate] = useState(current?.allow_private_endpoint ?? false);
  const [allowLoopback, setAllowLoopback] = useState(current?.allow_loopback_endpoint ?? false);
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [clearAuth, setClearAuth] = useState(false);
  const [allowCleartextAuth, setAllowCleartextAuth] = useState(current?.allow_cleartext_basic_auth ?? false);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const authWillExist = !clearAuth && Boolean(current?.authenticated || username || password);
  const mutation = useMutation({
    mutationFn: () => {
      const value = {
        name,
        kind: "http_connect",
        endpoint,
        allow_private_endpoint: allowPrivate,
        allow_loopback_endpoint: allowLoopback,
        allow_cleartext_basic_auth: authWillExist && allowCleartextAuth,
        ...(username || password ? { username, password } : {}),
        ...(clearAuth ? { clear_basic_auth: true } : {}),
      };
      return current
        ? api.updateProviderEgressProxy(current.id, value, current.revision, stepUp.values)
        : api.createProviderEgressProxy(value, idempotencyKey.current, stepUp.values);
    },
    onMutate: stepUp.begin,
    onError: (error) => {
      if (stepUp.absorb(error)) return;
      setPassword("");
    },
    onSuccess: (saved) => {
      setPassword("");
      queryClient.invalidateQueries({ queryKey: ["provider-egress-proxies"] });
      // A runtime-changing proxy edit invalidates the connection-test evidence
      // returned with every bound Provider. Refresh both resources together so
      // the row cannot keep showing a pre-change test as current.
      queryClient.invalidateQueries({ queryKey: ["providers"] });
      notify({
        tone: saved.activation_pending ? "warning" : "success",
        title: t(saved.activation_pending ? "providers.notifyProxySavedPending" : current ? "providers.notifyProxyUpdated" : "providers.notifyProxyCreated"),
        description: name,
      });
      onClose();
    },
  });
  const dirty = useDirty({ name, endpoint, allowPrivate, allowLoopback, username, password, clearAuth, allowCleartextAuth });
  return (
    <Modal title={t(current ? "providers.editProxy" : "providers.createProxy")} dirty={dirty} closeDisabled={mutation.isPending} onClose={onClose}>
      <form className="provider-credential-form provider-egress-proxy-form" onSubmit={(event) => {
        event.preventDefault();
        const nextErrors: Record<string, string> = {};
        if (!name.trim()) nextErrors.name = t("providers.validationProxyNameRequired");
        if (!validProxyEndpoint(endpoint)) nextErrors.endpoint = t("providers.validationProxyEndpoint");
        if (Boolean(username) !== Boolean(password)) nextErrors.authentication = t("providers.validationProxyAuthPair");
        else if (username.includes(":") || username.length > 1024 || password.length > 4096) nextErrors.authentication = t("providers.validationProxyAuthFormat");
        if (endpoint.trim().startsWith("http://") && authWillExist && !allowCleartextAuth) nextErrors.authentication = t("providers.validationProxyCleartextAuth");
        setErrors(nextErrors);
        if (!Object.keys(nextErrors).length && (!stepUp.asked || stepUp.values.currentPassword)) mutation.mutate();
      }} autoComplete="off">
        <div className="provider-credential-form-body provider-egress-proxy-form-body">
          <section className="proxy-form-section" aria-labelledby="proxy-connection-title">
            <header><h3 id="proxy-connection-title">{t("providers.proxyConnectionTitle")}</h3><p>{t("providers.proxyConnectionDescription")}</p></header>
            <Field label={t("providers.proxyName")} error={errors.name}><input autoFocus value={name} onChange={(event) => { setName(event.target.value); setErrors((previous) => omitError(previous, "name")); }} /></Field>
            <Field label={t("providers.proxyEndpointInput")} hint={`${t("providers.proxyEndpointInputHint")} ${t("providers.proxyEndpointHint")}`} error={errors.endpoint}><input inputMode="url" value={endpoint} onChange={(event) => { setEndpoint(event.target.value); setErrors((previous) => omitError(previous, "endpoint")); }} /></Field>
          </section>

          <section className={`proxy-form-section proxy-boundary-section${allowPrivate || allowLoopback ? " expanded" : ""}`} aria-labelledby="proxy-boundary-title">
            <header>
              <div><h3 id="proxy-boundary-title">{t("providers.proxyBoundarySectionTitle")}</h3><p id="proxy-boundary-description">{t("providers.proxyBoundarySectionDescription")}</p></div>
              <span className="proxy-boundary-state" aria-live="polite">{allowPrivate || allowLoopback
                ? t("providers.proxyBoundaryExceptions", { count: Number(allowPrivate) + Number(allowLoopback) })
                : t("providers.proxyBoundaryProtected")}</span>
            </header>
            <fieldset className="proxy-boundary-options" aria-describedby="proxy-boundary-description">
              <legend className="visually-hidden">{t("providers.proxyBoundarySectionTitle")}</legend>
              <label className={`proxy-boundary-option${allowPrivate ? " selected" : ""}`}>
                <input type="checkbox" checked={allowPrivate} onChange={(event) => setAllowPrivate(event.target.checked)} />
                <span className="proxy-boundary-option-copy"><strong>{t("providers.proxyPrivateTitle")}</strong><small>{t("providers.proxyPrivateDescription")}</small></span>
                <span className="proxy-boundary-option-state">{t(allowPrivate ? "providers.proxyExceptionAllowed" : "providers.proxyExceptionBlocked")}</span>
              </label>
              <label className={`proxy-boundary-option${allowLoopback ? " selected" : ""}`}>
                <input type="checkbox" checked={allowLoopback} onChange={(event) => setAllowLoopback(event.target.checked)} />
                <span className="proxy-boundary-option-copy"><strong>{t("providers.proxyLoopbackTitle")}</strong><small>{t("providers.proxyLoopbackDescription")}</small></span>
                <span className="proxy-boundary-option-state">{t(allowLoopback ? "providers.proxyExceptionAllowed" : "providers.proxyExceptionBlocked")}</span>
              </label>
            </fieldset>
            {allowPrivate || allowLoopback ? <div className="proxy-boundary-warning" role="status">
              <span className="proxy-boundary-warning-icon" aria-hidden="true">!</span>
              <span><strong>{t("providers.proxyBoundaryTitle")}</strong><small>{t("providers.proxyBoundaryDescription")}</small></span>
            </div> : <p className="proxy-boundary-default"><span aria-hidden="true">✓</span>{t("providers.proxyBoundaryDefault")}</p>}
          </section>

          <section className="proxy-form-section proxy-auth-section" aria-labelledby="proxy-auth-title">
            <header><h3 id="proxy-auth-title">{t("providers.proxyAuthenticationTitle")}</h3><p>{t("providers.proxyAuthenticationDescription")}</p></header>
            <div className="proxy-auth-grid">
              <Field label={t("providers.proxyUsername")}><input value={username} disabled={clearAuth} autoComplete="username" onChange={(event) => { setUsername(event.target.value); setErrors((previous) => omitError(previous, "authentication")); }} /></Field>
              <Field label={t("providers.proxyPassword")} hint={current?.authenticated ? t("providers.proxyPasswordPreserveHint") : undefined} error={errors.authentication}><input type="password" value={password} disabled={clearAuth} autoComplete="new-password" onChange={(event) => { setPassword(event.target.value); setErrors((previous) => omitError(previous, "authentication")); }} /></Field>
            </div>
            {current?.authenticated && <label className="form-inline-check"><input type="checkbox" checked={clearAuth} onChange={(event) => { setClearAuth(event.target.checked); if (event.target.checked) { setUsername(""); setPassword(""); } }} /><span>{t("providers.proxyClearAuth")}</span></label>}
            {endpoint.trim().startsWith("http://") && authWillExist && <label className="form-inline-check warning-text"><input type="checkbox" checked={allowCleartextAuth} onChange={(event) => { setAllowCleartextAuth(event.target.checked); setErrors((previous) => omitError(previous, "authentication")); }} /><span>{t("providers.proxyAllowCleartextAuth")}</span></label>}
          </section>
          {mutation.isError && !stepUp.probing && <div className="proxy-form-feedback"><ErrorState error={mutation.error} /></div>}
          {stepUp.asked && <div className="proxy-form-feedback"><ReauthFields values={stepUp.values} onChange={stepUp.setValues} description={t("auth.stepUpSecurityControl")} /></div>}
        </div>
        <div className="form-actions sticky-form-actions">
          <button type="button" className="button ghost" disabled={mutation.isPending} onClick={onClose}>{t("common.cancel")}</button>
          <button className="button primary" disabled={mutation.isPending || (stepUp.asked && !stepUp.values.currentPassword)}>{t(current ? "providers.saveProxy" : "providers.createProxyAndLoad")}</button>
        </div>
      </form>
    </Modal>
  );
}

function ProviderRow({ provider, credential, catalog, egress, probeDeploymentID, highlighted, onCredentialClick, onEdit }: { provider: Provider; credential?: Credential; catalog?: ProviderProfilesCatalog; egress?: ProviderEgressCatalog; probeDeploymentID?: string; highlighted: boolean; onCredentialClick: () => void; onEdit: () => void }) {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const [expanded, setExpanded] = useState(false);
  const queryClient = useQueryClient();
  const { notify } = useNotify();
  const profile = catalog ? findProfile(catalog, provider.type, provider.profile_id) : undefined;
  const withdrawn = Boolean(catalog && !profile);
  const editable = Boolean(catalog && profile);
  const selectedEgress = egress?.items.find((proxy) => proxy.id === provider.egress_proxy_id);
  const egressMissing = Boolean(provider.egress_proxy_id && !selectedEgress);
  const product = profile
    ? productLabel(
        t,
        catalog!,
        provider.type,
        profile.offering_id,
        profile.region_id || regionForEndpoint(findOffering(catalog!, provider.type, profile.offering_id), provider.base_url) || "",
      )
    : "";
  const testMutation = useMutation({
    mutationFn: () => probeDeploymentID
      ? api.testProvider(provider.id, undefined, probeDeploymentID)
      : api.testProvider(provider.id),
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
  const persistedTestIsCurrent = provider.last_test_current ?? (
    provider.last_test_revision === provider.revision && !provider.egress_proxy_id
  );
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
  const liveTestFailure = testMutation.error instanceof ApiError
    ? testMutation.error.payload as { proxy_stage?: string; proxy_status?: number } | undefined
    : undefined;
  const testProxyStage = liveTestFailure?.proxy_stage || (persistedTestIsCurrent ? provider.last_test_proxy_stage : undefined);
  const testProxyStatus = liveTestFailure?.proxy_status || (persistedTestIsCurrent ? provider.last_test_proxy_status : undefined);
  const testLatency = testMutation.data?.latency_ms ?? provider.last_test_latency_millis;
  const healthyTargets = testMutation.data?.healthy_targets ?? provider.last_test_healthy_targets;
  const totalTargets = testMutation.data?.total_targets ?? provider.last_test_total_targets;
  const testVerdict = testState === "success"
    ? testLatency === undefined ? t("testControl.successPlain") : `${t("testControl.successPlain")} ${testLatency}ms`
    : t(`testControl.${testState}`);
  const conditionTone = withdrawn
    ? "danger"
    : !provider.enabled
      ? "muted"
      : testState === "failure"
        ? "danger"
        : testState === "success"
          ? "good"
          : "warning";
  const conditionLabel = withdrawn
    ? t("providers.productWithdrawn")
    : provider.enabled
      ? `${t("providers.enabled")}${t("common.dotSeparator")}${testVerdict}`
      : t("providers.off");
  return (
    <>
      <article id={`provider-${provider.id}`} className={`provider-row ${highlighted ? "resource-highlight" : ""}`}>
        <span className="provider-icon provider-compact-icon">{provider.type === "openai" ? "OA" : "AI"}</span>
        <div className="resource-identity provider-compact-identity"><strong>{provider.name}</strong><small>{product || t(`providers.types.${provider.type}`)}</small></div>
        <div className="resource-fact provider-fact-endpoint"><small>{t("providers.endpoint")}</small><strong>{provider.base_url}</strong></div>
        <div className="resource-fact provider-fact-trust">
          <small>{t("providers.connectionPolicy")}</small>
          <div className="provider-trust-value">
            {credential ? <button className="resource-link" onClick={onCredentialClick}>{credential.name}</button> : <span>{t("providers.missingCredential")}</span>}
            <span className="provider-trust-separator" aria-hidden="true">·</span>
            <span className={`provider-egress-summary ${egressMissing ? "warning-text" : ""}`}>{provider.egress_proxy_id ? selectedEgress?.name ?? t("providers.egressMissing") : t("providers.egressDirect")}</span>
          </div>
        </div>
        <div className="resource-row-state provider-compact-status"><small>{t("common.status")}</small><span className="provider-condition" data-tone={conditionTone} role="status" aria-live="polite" aria-atomic="true"><span aria-hidden="true" />{conditionLabel}</span></div>
        <div className="row-actions provider-compact-actions">
          <button className="button secondary provider-test-action" disabled={readOnly || !provider.enabled || !editable || testMutation.isPending} title={readOnly ? t("navigation.readOnlyAction") : withdrawn ? t("providers.productWithdrawnAction") : totalTargets ? t("providers.testSummary", { healthy: healthyTargets ?? 0, total: totalTargets, latency: testLatency ?? 0 }) : undefined} onClick={() => testMutation.mutate()}>{t("common.test")}</button>
          <button className="button ghost provider-expand" aria-expanded={expanded} aria-controls={`provider-details-${provider.id}`} onClick={() => setExpanded((value) => !value)}>{expanded ? t("providers.collapseDetails") : t("providers.expandDetails")}</button>
          <OverflowMenu label={t("providers.moreActionsFor", { name: provider.name })}>
            {/* Editing opens a form built from the served matrix. Without it the
                click would set state and render nothing, so the reason is on the
                button — the same treatment the create and rotate buttons get. */}
            <button className="button ghost" aria-label={t("providers.actionFor", { action: t("common.edit"), name: provider.name })} disabled={readOnly || !editable} title={withdrawn ? t("providers.productWithdrawnAction") : !catalog ? t("providers.matrixUnavailable") : undefined} onClick={onEdit}>{t("common.edit")}</button>
            {provider.enabled ? <ConfirmButton className="button ghost" label={t("common.disable")} ariaLabel={t("providers.actionFor", { action: t("common.disable"), name: provider.name })} title={withdrawn ? t("providers.productWithdrawnAction") : t("providers.disableTitle")} confirmLabel={t("providers.disableConfirm", { name: provider.name })} disabled={stateMutation.isPending || !editable} onConfirm={() => stateMutation.mutateAsync()} /> : <button className="button ghost" aria-label={t("providers.actionFor", { action: t("common.enable"), name: provider.name })} title={readOnly ? t("navigation.readOnlyAction") : withdrawn ? t("providers.productWithdrawnAction") : undefined} disabled={readOnly || stateMutation.isPending || !editable} onClick={() => stateMutation.mutate()}>{t("common.enable")}</button>}
            <ConfirmButton label={t("common.delete")} ariaLabel={t("providers.actionFor", { action: t("common.delete"), name: provider.name })} confirmLabel={t("providers.deleteProvider", { name: provider.name })} disabled={deleteMutation.isPending} requireStepUp onConfirm={(reauth) => deleteMutation.mutateAsync(reauth)} />
          </OverflowMenu>
        </div>
        {/* The reason belongs in the row that failed, not behind an expander:
            the operator is looking at the button they just pressed. */}
        {testState === "failure" && testFailureReason && (
          <p className="row-test-failure" role="status">
            {testFailureReason}
            {testProxyStage && ` · ${t("testControl.proxyDiagnostic", { stage: testProxyStage, status: testProxyStatus ? ` · HTTP ${testProxyStatus}` : "" })}`}
          </p>
        )}
        {expanded && <div id={`provider-details-${provider.id}`} className="provider-row-content provider-expanded-content">
          <div className="provider-facts">
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
              <div><dt>{t("providers.egressPath")}</dt><dd>{provider.egress_proxy_id ? selectedEgress?.name ?? provider.egress_proxy_id : t("providers.egressDirect")}</dd></div>
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
	const readOnly = useIsReadOnly();
  const identityAvailable = Boolean(catalog && credentialIdentities(catalog, credential.type).some((identity) =>
    identity.accessSurface === credential.access_surface && identity.credentialScheme === credential.scheme));
  const withdrawn = Boolean(catalog && !identityAvailable);
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
          {withdrawn && <small className="warning-text">{t("providers.productWithdrawn")}</small>}
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
		  <button className="button ghost" disabled={readOnly || !identityAvailable} title={readOnly ? t("navigation.readOnlyAction") : withdrawn ? t("providers.productWithdrawnAction") : !catalog ? t("providers.matrixUnavailable") : undefined} onClick={() => setRotating(true)}>{t("providers.rotate")}</button>
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
            {/* The product first, in the words an operator bought it in; the
                surface below it is the identifier that decides behaviour, and
                the two are one fact seen from two sides. */}
            <div><dt>{t("providers.product")}</dt><dd>{[
              t(`providers.offerings.${credential.offering_id}`, { defaultValue: credential.offering_id }),
              credential.region_id ? t(`providers.regions.${credential.region_id}`, { defaultValue: credential.region_id }) : "",
            ].filter(Boolean).join(" · ")}</dd></div>
            <div><dt>{t("providers.normalizedBoundURL")}</dt><dd><code>{credential.bound_base_url}</code></dd></div>
            <div><dt>{t("providers.surface")}</dt><dd><code>{credential.access_surface}</code></dd></div>
            <div><dt>{t("providers.scheme")}</dt><dd><code>{credential.scheme}</code></dd></div>
            <div><dt>{t("providers.credentialID")}</dt><dd><code>{credential.id}</code></dd></div>
            <div><dt>{t("providers.credentialExpiry")}</dt><dd>{credential.expires_at ? dateTime(credential.expires_at, "dateTimeYear") : t("providers.credentialNeverExpires")}</dd></div>
          </dl>
        </section>}
      </article>
      {remove.isError && !isStepUpPrompt(remove.error) && <ErrorState error={remove.error} />}
      {rotating && catalog && identityAvailable && <CredentialForm current={credential} catalog={catalog} onClose={() => setRotating(false)} />}
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
  // Which upstream product this key is for. A credential stores it as an access
  // surface and a scheme, and the server refuses to guess where a type sells
  // more than one — which is what let every BigModel key be sealed to the
  // mainland surface no matter which host it was bound to. A rotation keeps
  // whatever the credential was sealed to and does not ask again: changing the
  // product is a new credential, because every connection built on this one
  // takes its endpoint and its capability set from that surface.
  const initialIdentities = credentialIdentities(catalog, current?.type ?? "openai");
  const [identity, setIdentity] = useState<CredentialIdentity | undefined>(
    current
      ? initialIdentities.find((candidate) => candidate.accessSurface === current.access_surface)
      : initialIdentities[0],
  );
  const [baseURL, setBaseURL] = useState(
    current ? displayBoundBaseURL(current.bound_base_url) : initialIdentities[0]?.defaultBaseURL ?? "",
  );
  const [secret, setSecret] = useState("");
  const [usageWarningAcknowledged, setUsageWarningAcknowledged] = useState(false);
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
  const [policyRefreshPending, setPolicyRefreshPending] = useState(false);
  const [policyRefreshFailed, setPolicyRefreshFailed] = useState(false);
  const [policyAttention, setPolicyAttention] = useState(0);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const formElement = useRef<HTMLFormElement>(null);
  const [refusedSubmits, setRefusedSubmits] = useState(0);
  const submissionPending = useRef(false);
  const mutation = useMutation({
    mutationFn: () => {
      // Sent on creation only. A rotation leaves the pair out so the server keeps
      // whatever the credential was sealed to — including a surface this build no
      // longer offers, which must be deleted rather than silently re-pointed at
      // the one that is.
      const product = !current && identity ? identity : undefined;
      const value = {
        name,
        type,
        base_url: baseURL,
        ...(product
          ? { access_surface: product.accessSurface, scheme: product.credentialScheme }
          : {}),
        ...(usageWarningRequired && usageDocument
          ? { acknowledged_policy_revision: usageDocument.policy_revision }
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
    onError: (error) => {
      if (stepUp.absorb(error)) return;
      if (error instanceof ApiError && error.code === "usage_policy_revision_mismatch") {
        setUsageWarningAcknowledged(false);
        setPolicyAttention((value) => value + 1);
        setPolicyRefreshPending(true);
        setPolicyRefreshFailed(false);
        void api.providerProfiles()
          .then((latest) => {
            queryClient.setQueryData(["provider-profiles"], latest);
            mutation.reset();
          })
          .catch(() => setPolicyRefreshFailed(true))
          .finally(() => setPolicyRefreshPending(false));
        return;
      }
      setSecret("");
    },
    onSuccess: () => {
      setSecret("");
      queryClient.invalidateQueries({ queryKey: ["credentials"] });
      notify({ tone: "success", title: t(current ? "providers.notifyCredentialRotated" : "providers.notifyCredentialSaved"), description: name });
      onClose();
    },
    onSettled: () => { submissionPending.current = false; },
  });
  // Same reason as the provider form below: the rejection must reach the
  // operator who clicked, not sit in a scrolled-away part of the modal.
  const submitError = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!mutation.isError || stepUp.probing || (mutation.error instanceof ApiError && mutation.error.code === "usage_policy_revision_mismatch")) return;
    requestAnimationFrame(() => {
      submitError.current?.scrollIntoView?.({ block: "center" });
      submitError.current?.focus();
    });
  }, [mutation.isError, mutation.error, stepUp.probing]);
  useEffect(() => {
    if (!refusedSubmits) return;
    requestAnimationFrame(() => {
      const invalid = formElement.current?.querySelector<HTMLElement>("[aria-invalid='true']");
      invalid?.scrollIntoView?.({ block: "center" });
      invalid?.focus();
    });
  }, [refusedSubmits]);
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (submissionPending.current || mutation.isPending) return;
    const nextErrors: Record<string, string> = {};
    if (!name.trim()) nextErrors.name = t("providers.validationCredentialNameRequired");
    if (identity?.baseURLTemplate?.includes("{region}") && !validProviderRegion(regionFromEndpointTemplate(identity.baseURLTemplate, baseURL) ?? "")) {
      nextErrors.region = t("providers.validationProviderRegion");
    }
    if (!baseURL.trim()) nextErrors.baseURL = t("providers.validationBaseURLRequired");
    else if (!validProviderEndpoint(baseURL)) nextErrors.baseURL = t("providers.validationBaseURLInvalid");
    else if (fixedRegionMismatch) nextErrors.baseURL = t("providers.validationFixedRegionMismatch");
    if (!current && !secret) nextErrors.secret = t("providers.validationSecretRequired");
    setErrors(nextErrors);
    if (Object.keys(nextErrors).length) {
      setRefusedSubmits((value) => value + 1);
      return;
    }
    if ((!usageWarningRequired || usageWarningAcknowledged)
      && !policyRefreshPending && !policyRefreshFailed
      && (!stepUp.asked || stepUp.values.currentPassword)) {
      submissionPending.current = true;
      mutation.mutate();
    }
  };
  // Everything below is read from the served matrix. A type that sells one
  // product asks nothing; one that sells two cannot have it guessed, and one
  // whose regions live in the endpoint gets the endpoint written for it.
  const typeIdentities = credentialIdentities(catalog, type);
  const offering = findOffering(catalog, type, identity?.offeringID ?? "");
  const detectedRegion = regionForEndpoint(offering, baseURL);
  const usageDocument = usagePolicyDocument(offering, identity?.regionID || detectedRegion || "");
  const usageWarningRequired = Boolean(offering?.requires_usage_warning && usageDocument)
    && (!current || current.usage_policy_current !== true);
  const fixedRegionStatus = offering?.region_scope === "fixed" && identity
    ? fixedRegionEndpointStatus(catalog, type, identity.offeringID, identity.regionID, baseURL)
    : "match";
  const fixedRegionUnverified = fixedRegionStatus === "unknown";
  const fixedRegionMismatch = fixedRegionStatus === "cross_region";
  const configuredRegion = regionFromEndpointTemplate(identity?.baseURLTemplate, baseURL);
  // The control names what it actually chooses: a product where the type sells
  // more than one, and otherwise the region, which is what the identities of a
  // single-product type differ by.
  const identityLabel = offeringsForType(catalog, type).length > 1
    ? t("providers.product")
    : t("providers.region");
  const dirty = useDirty({ name, type, baseURL, secret, expiresAt, usageWarningAcknowledged });
  return (
    <Modal title={current ? t("providers.rotateCredential") : t("providers.saveCredential")} dirty={dirty} closeDisabled={mutation.isPending} onClose={onClose}>
      {/* Like the other modal forms: the form drops the modal's margin so the
          footer can stick to both edges, and the body carries the padding. */}
      <form ref={formElement} className="provider-credential-form" onSubmit={submit} autoComplete="off">
        <div className="provider-credential-form-body">
        <Field label={t("providers.credentialName")} error={errors.name}><input autoComplete="off" autoFocus value={name} onChange={(event) => { setName(event.target.value); setErrors((previous) => omitError(previous, "name")); }} /></Field>
        <Field label={t("providers.providerType")}>
          <select value={type} disabled={Boolean(current)} onChange={(event) => {
            const next = event.target.value as ProviderType;
            const first = credentialIdentities(catalog, next)[0];
            setType(next);
            setIdentity(first);
            setBaseURL(first?.defaultBaseURL ?? "");
            setUsageWarningAcknowledged(false);
          }}>
            <ProviderTypeOptions t={t} />
          </select>
        </Field>
        {/* Which product this key belongs to. Shown only where there is more
            than one, because a question with one answer is not asked — and
            never on a rotation, where the answer is already sealed into the
            credential and changing it is a new credential rather than a new
            secret. */}
        {!current && typeIdentities.length > 1 && (
          <Field label={identityLabel} hint={t("providers.productHint")}>
            <select value={identity?.accessSurface ?? ""} onChange={(event) => {
              const next = typeIdentities.find((candidate) => candidate.accessSurface === event.target.value);
              setIdentity(next);
              // The endpoint belongs to the product, so it is rewritten rather
              // than carried over: a mainland key left pointing at the global
              // host is the exact pairing this form exists to stop.
              setBaseURL(next?.defaultBaseURL ?? "");
              setUsageWarningAcknowledged(false);
            }}>
              {typeIdentities.map((candidate) => (
                <option key={candidate.accessSurface} value={candidate.accessSurface}>
                  {productOptionLabel(t, catalog, type, candidate.offeringID, candidate.regionID, candidate.credentialScheme)}
                </option>
              ))}
            </select>
          </Field>
        )}
        {current && identity && productLabel(t, catalog, type, identity.offeringID, identity.regionID) && (
          <Field label={identityLabel} hint={t("providers.productLockedHint")}>
            <output className="badge">{productLabel(t, catalog, type, identity.offeringID, identity.regionID)}</output>
          </Field>
        )}
        {usageWarningRequired && (
          <SubscriptionUsageDisclosure
            acknowledged={usageWarningAcknowledged}
            documentationURL={usageDocument?.url}
            identityUnverified={usageDocument?.product_identity_assurance === "operator_declared_unverified"}
            attentionKey={policyAttention}
            disabled={policyRefreshPending || policyRefreshFailed}
            onAcknowledgedChange={setUsageWarningAcknowledged}
          />
        )}
        {policyRefreshFailed && <p className="field-hint warning-text">{t("providers.usagePolicyRefreshFailed")}</p>}
        {/* One product, several account hosts, keys that are not interchangeable
            between them. The region is the endpoint here, so choosing it writes
            the endpoint field; an address the upstream does not publish is
            unknown rather than wrong, and saves either way. */}
        {offering?.region_scope === "by_endpoint" && (
          <Field label={t("providers.region")} hint={t("providers.regionByEndpointHint")}>
            <select value={detectedRegion ?? ""} onChange={(event) => {
              const host = offering.region_hosts.find((entry) => entry.region === event.target.value)?.host;
              if (host) setBaseURL(`https://${host}`);
            }}>
              {detectedRegion === undefined && <option value="">{t("providers.regionUnknown")}</option>}
              {offering.region_hosts.map((entry) => (
                <option key={entry.region} value={entry.region}>
                  {t(`providers.regions.${entry.region}`, { defaultValue: entry.region })} · {entry.host}
                </option>
              ))}
            </select>
          </Field>
        )}
        {identity?.baseURLTemplate?.includes("{region}") && (
          <Field label={t("providers.providerRegion")} hint={current ? t("providers.providerRegionLockedHint") : t("providers.providerRegionHint")} error={errors.region}>
            <input
              autoComplete="off"
              value={configuredRegion ?? ""}
              disabled={Boolean(current)}
              placeholder="us-east-1"
              onChange={(event) => {
                setBaseURL(endpointFromRegion(identity.baseURLTemplate!, event.target.value.trim().toLocaleLowerCase()));
                setErrors((previous) => omitError(omitError(previous, "region"), "baseURL"));
              }}
            />
          </Field>
        )}
        <Field label={t("providers.boundURL")} hint={t("providers.boundURLHint")} error={fixedRegionMismatch ? t("providers.validationFixedRegionMismatch") : errors.baseURL}>
          <input autoComplete="off" inputMode="url" value={baseURL} onChange={(event) => { setBaseURL(event.target.value); setErrors((previous) => omitError(previous, "baseURL")); }} />
        </Field>
        {fixedRegionUnverified && <p className="field-hint warning-text">{t("providers.fixedRegionUnverified")}</p>}
        {/* What kind of material this is belongs to the credential scheme, not to
            the provider name: the same scheme on a new platform needs the same
            sentence, and the fallback keeps a new scheme readable rather than
            blank. */}
        <Field
          label={current ? t("providers.newSecret") : t("providers.providerSecret")}
          hint={current
            ? t("providers.secretConfigured")
            : t(`providers.schemeHints.${identity?.credentialScheme ?? ""}`, { defaultValue: t("providers.secretHint") })}
          error={errors.secret}
        >
          <input
            type="password"
            autoComplete="new-password"
            value={secret}
            onChange={(event) => { setSecret(event.target.value); setErrors((previous) => omitError(previous, "secret")); }}
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
          <button type="button" className="button ghost" disabled={mutation.isPending} onClick={onClose}>{t("common.cancel")}</button>
          <button className="button primary" disabled={mutation.isPending || policyRefreshPending || policyRefreshFailed || fixedRegionMismatch || (stepUp.asked && !stepUp.values.currentPassword)}>
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
  egress,
  enabledDeploymentCount,
  onClose,
}: {
  current?: Provider;
  credentials: Credential[];
  catalog: ProviderProfilesCatalog;
  egress: ProviderEgressCatalog;
  enabledDeploymentCount: number;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const { notify } = useNotify();
  const initialType = current?.type ?? "openai";
  const [name, setName] = useState(current?.name ?? "");
  const [type, setType] = useState<ProviderType>(initialType);
  // Which implementation this connection is anchored on. There is something to
  // choose in exactly two situations, and the served matrix says which: a type
  // that sells more than one product (BigModel's two regions), and a group whose
  // routes are partitioned (Bedrock Mantle, where each model answers on exactly
  // one route). Everywhere else the group's profiles ride one connection
  // together and this control is not rendered.
  const initialProfile = current?.profile_id
    && connectionChoices(catalog, initialType).some((choice) => choice.profileID === current.profile_id)
    ? current.profile_id
    : connectionChoices(catalog, initialType)[0]?.profileID ?? defaultProfileID(catalog, initialType);
  const [profileID, setProfileID] = useState(initialProfile);
  const [baseURL, setBaseURL] = useState(current?.base_url ?? endpointForType(catalog, initialType));
  const [apiVersion, setAPIVersion] = useState(current?.api_version ?? "");
  const [bedrockProjectID, setBedrockProjectID] = useState(current?.bedrock_project_id ?? "");
  const [anthropicBetas, setAnthropicBetas] = useState((current?.allowed_anthropic_betas ?? []).join(", "));
  const [maxConcurrency, setMaxConcurrency] = useState(current?.max_concurrency ?? 0);
  const [enabled, setEnabled] = useState(current?.enabled ?? true);
  const [egressProxyID, setEgressProxyID] = useState(current?.egress_proxy_id ?? "");
  const [usageWarningAcknowledged, setUsageWarningAcknowledged] = useState(false);
  const [capabilities, setCapabilities] = useState<ProviderCapabilities>(
    current?.capabilities ?? connectionDefaults(catalog, initialType, defaultProfileID(catalog, initialType)),
  );
  // Both come from the server. The ceiling is what an operator may turn on, the
  // defaults are what a new connection starts with, and they are different
  // questions — provider_executed_tools sits above the defaults and inside the
  // ceiling, because the profile supports it and enabling it accepts upstream
  // egress Halro never sees.
  const choices = connectionChoices(catalog, type);
  const selectedChoice: ConnectionChoice | undefined =
    choices.find((choice) => choice.profileID === profileID) ?? choices[0];
  const anchorProfile = selectedChoice?.profileID ?? defaultProfileID(catalog, type);
  const capabilityCeiling = connectionCeiling(catalog, type, anchorProfile);
  const capabilityNames = booleanCapabilityNames(catalog);
  // A profile whose set is fixed by the build offers no checkboxes to widen.
  const fixedCapabilities = combinableProfiles(catalog, type, anchorProfile).every((profile) => profile.immutable);
  const visibleCapabilities = capabilityNames.filter((capability) => capabilities[capability]);
  const configurableCapabilities = capabilityNames.filter((capability) => capabilityCeiling[capability] || capabilities[capability]);
  const selectedSurface = selectedChoice?.accessSurface;
  const selectedOffering = findOffering(catalog, type, selectedChoice?.offeringID ?? "");
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
  // those would be kept and never sent. Which profiles those are is the
  // server's answer now — it used to be a pair of identifiers written here, and
  // the same rule in two places is a rule that drifts.
  const supportsAnthropicBetas = Boolean(findProfile(catalog, type, anchorProfile)?.sends_anthropic_betas);
  const matchingCredentials = credentials.filter((credential) => credential.type === type && (!selectedSurface || credential.access_surface === selectedSurface));
  const [credentialID, setCredentialID] = useState(
    current?.credential_id ??
      credentials.find((credential) =>
        credential.type === initialType &&
        credential.access_surface === findProfile(catalog, initialType, initialProfile)?.access_surface,
      )?.id ??
      "",
  );
  const [errors, setErrors] = useState<Record<string, string>>({});
  const stepUp = useStepUpPrompt();
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
  const selectedRegion = selectedChoice?.regionID
    || regionForEndpoint(selectedOffering, baseURL)
    || selectedCredential?.region_id
    || "";
  const usageDocument = usagePolicyDocument(selectedOffering, selectedRegion);
  const currentAcknowledgement = current?.usage_policy_acknowledgement;
  const usagePolicyCurrent = Boolean(currentAcknowledgement && usageDocument
    && currentAcknowledgement.offering_id === selectedChoice?.offeringID
    && currentAcknowledgement.access_surface === selectedSurface
    && currentAcknowledgement.account_region_id === selectedRegion
    && currentAcknowledgement.policy_revision === usageDocument.policy_revision
    && currentAcknowledgement.product_identity_assurance === (usageDocument.product_identity_assurance ?? "mechanically_verified"));
  const usageWarningRequired = Boolean(selectedOffering?.requires_usage_warning && usageDocument)
    && (!current || !usagePolicyCurrent);
  const fixedRegionStatus = selectedOffering?.region_scope === "fixed" && selectedChoice
    ? fixedRegionEndpointStatus(catalog, type, selectedChoice.offeringID, selectedChoice.regionID, baseURL)
    : "match";
  const fixedRegionUnverified = fixedRegionStatus === "unknown";
  const fixedRegionMismatch = fixedRegionStatus === "cross_region";
  const queryClient = useQueryClient();
  const [policyRefreshPending, setPolicyRefreshPending] = useState(false);
  const [policyRefreshFailed, setPolicyRefreshFailed] = useState(false);
  const [policyAttention, setPolicyAttention] = useState(0);
  // One key per open form: a retry after a lost response reaches the same
  // record instead of creating a second one, while a deliberate second create
  // opens the form again and gets a new key.
  const idempotencyKey = useRef(crypto.randomUUID());
  const submissionPending = useRef(false);
  const mutation = useMutation({
    mutationFn: () => {
      const value = {
      name, type, base_url: baseURL,
      // Sent where the operator actually chose, and only there. Naming the
      // implementation asserts that the enabled capabilities land on it, which
      // is a claim a form has no business making when the matrix offered one
      // option and it picked it: the server assigns capabilities across the
      // group and the anchor follows.
      ...(choices.length > 1 && selectedChoice ? {
        profile_id: selectedChoice.profileID,
        access_surface: selectedChoice.accessSurface,
        credential_scheme: selectedChoice.credentialScheme,
      } : {}),
      ...(usageWarningRequired && usageDocument
        ? { acknowledged_policy_revision: usageDocument.policy_revision }
        : {}),
      ...(selectedSurface === "bedrock-mantle"
        ? { bedrock_project_id: normalizeBedrockProjectID(bedrockProjectID) }
        : {}),
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
      egress_proxy_id: egressProxyID,
      ...(supportsAnthropicBetas ? { allowed_anthropic_betas: parseBetaTokens(anthropicBetas) } : {}),
      };
      return current
        ? api.updateProvider(current.id, value, current.revision, stepUp.values)
        : api.createProvider(value, idempotencyKey.current, stepUp.values);
    },
    onMutate: stepUp.begin,
    onError: (error) => {
      if (stepUp.absorb(error)) return;
      if (error instanceof ApiError && error.code === "usage_policy_revision_mismatch") {
        setUsageWarningAcknowledged(false);
        setPolicyAttention((value) => value + 1);
        setPolicyRefreshPending(true);
        setPolicyRefreshFailed(false);
        void api.providerProfiles()
          .then((latest) => {
            queryClient.setQueryData(["provider-profiles"], latest);
            mutation.reset();
          })
          .catch(() => setPolicyRefreshFailed(true))
          .finally(() => setPolicyRefreshPending(false));
      }
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["providers"] });
      notify({ tone: "success", title: t(current ? "providers.notifyUpdated" : "providers.notifyCreated"), description: name });
      onClose();
    },
    onSettled: () => { submissionPending.current = false; },
  });
  const dirty = useDirty({ name, type, profileID, baseURL, apiVersion, bedrockProjectID, anthropicBetas, maxConcurrency, enabled, egressProxyID, capabilities, credentialID, usageWarningAcknowledged });
  // The save button sits in a sticky footer while the form scrolls behind it,
  // so a rejection renders into the part of the modal the operator is not
  // looking at: the click appears to do nothing and they click again. Bring the
  // failure into view and move focus onto it, so the reason is announced rather
  // than merely present.
  const submitError = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!mutation.isError || (mutation.error instanceof ApiError && mutation.error.code === "usage_policy_revision_mismatch")) return;
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
    <Modal wide title={current ? t("providers.editProvider") : t("providers.createProvider")} dirty={dirty} closeDisabled={mutation.isPending} onClose={onClose}>
      {credentials.length === 0 ? (
        <div className="notice warning">
          <strong>{t("providers.credentialRequired")}</strong>
          <span>{t("providers.credentialRequiredDescription")}</span>
        </div>
      ) : (
        <form className="provider-form" ref={formElement}
          onSubmit={(event) => {
            event.preventDefault();
            if (submissionPending.current || mutation.isPending) return;
            const nextErrors = validateProvider({
              name, credentialID, bedrockProjectID,
              mantle: selectedSurface === "bedrock-mantle",
              anyCapability: anyCapabilityEnabled(catalog, capabilities),
              unservable,
              anthropicBetas: supportsAnthropicBetas ? anthropicBetas : "",
            }, t);
            if (!nextErrors.credentialID && credentialBaseURLMismatch) nextErrors.credentialID = credentialBaseURLMismatch;
            if (fixedRegionMismatch) nextErrors.baseURL = t("providers.validationFixedRegionMismatch");
            setErrors(nextErrors);
            if (Object.keys(nextErrors).length) setRefusedSubmits((value) => value + 1);
            else if (!policyRefreshPending && !policyRefreshFailed) {
              submissionPending.current = true;
              mutation.mutate();
            }
          }}
        >
          <section className="provider-form-section" aria-labelledby="provider-connection-title">
            <header><h3 id="provider-connection-title">{t("providers.connectionSection")}</h3><p>{t("providers.connectionSectionDescription")}</p></header>
            <div className="form-grid">
          <Field label={t("providers.providerName")} error={errors.name}><input autoComplete="off" autoFocus value={name} onChange={(event) => { setName(event.target.value); setErrors((previous) => omitError(previous, "name")); }} /></Field>
          <Field label={t("providers.type")}>
            <select value={type} onChange={(event) => {
              const next = event.target.value as ProviderType;
              const first = connectionChoices(catalog, next)[0];
              setType(next);
              setBaseURL(first?.defaultBaseURL ?? endpointForType(catalog, next));
              setProfileID(first?.profileID ?? defaultProfileID(catalog, next));
              setCredentialID(credentials.find((credential) => credential.type === next
                && credential.access_surface === first?.accessSurface)?.id ?? "");
              setCapabilities(connectionDefaults(catalog, next, first?.profileID ?? defaultProfileID(catalog, next)));
              setUsageWarningAcknowledged(false);
              setErrors({});
              mutation.reset();
            }}>
              <ProviderTypeOptions t={t} />
            </select>
          </Field>
          {choices.length > 1 && (
            <Field label={t("providers.capabilityImplementation")} hint={t("providers.implementationHint")}>
              <select value={anchorProfile} onChange={(event) => {
                const next = choices.find((choice) => choice.profileID === event.target.value);
                if (!next) return;
                setProfileID(next.profileID);
                setBaseURL(next.defaultBaseURL);
                setCredentialID(credentials.find((credential) =>
                  credential.type === type && credential.access_surface === next.accessSurface)?.id ?? "");
                setCapabilities(connectionDefaults(catalog, type, next.profileID));
                setUsageWarningAcknowledged(false);
                setErrors({});
                mutation.reset();
              }}>
                {choices.map((choice) => (
                  <option value={choice.profileID} key={choice.profileID}>
                    {t(`providers.profiles.${choice.profileID}`, {
                      defaultValue: productLabel(t, catalog, type, choice.offeringID, choice.regionID) || choice.profileID,
                    })}
                  </option>
                ))}
              </select>
            </Field>
          )}
          {usageWarningRequired && (
            <SubscriptionUsageDisclosure
              acknowledged={usageWarningAcknowledged}
              documentationURL={usageDocument?.url}
              identityUnverified={usageDocument?.product_identity_assurance === "operator_declared_unverified"}
              attentionKey={policyAttention}
              disabled={policyRefreshPending || policyRefreshFailed}
              onAcknowledgedChange={setUsageWarningAcknowledged}
            />
          )}
          {policyRefreshFailed && <p className="field-hint warning-text">{t("providers.usagePolicyRefreshFailed")}</p>}
          {/* A connection's endpoint follows its credential, which is already
              sealed to one. Where a product splits by account host — Kimi and
              MiniMax — the two addresses differ by a couple of letters and the
              wrong one fails as an authentication error, so the credential's
              own bound URL is what the field is checked against rather than a
              per-provider sentence. */}
          <Field label={t("providers.baseURL")} error={fixedRegionMismatch ? t("providers.validationFixedRegionMismatch") : errors.baseURL} hint={
            credentialBoundURL ? t("providers.baseURLBoundHint", { credential: credentialBoundURL }) : undefined
          }>
            <input autoComplete="off" value={baseURL} onChange={(event) => {
              setBaseURL(event.target.value);
              setErrors((previous) => omitError(omitError(previous, "credentialID"), "baseURL"));
            }} />
          </Field>
          {fixedRegionUnverified && <p className="field-hint warning-text">{t("providers.fixedRegionUnverified")}</p>}
          <Field label={t("providers.egressPath")} hint={t("providers.egressHint")}>
            <select
              value={egressProxyID}
              disabled={Boolean(current && enabledDeploymentCount > 0)}
              onChange={(event) => setEgressProxyID(event.target.value)}
            >
              <option value="">{t("providers.egressDirect")}</option>
              {current?.egress_proxy_id && !egress.items.some((proxy) => proxy.id === current.egress_proxy_id) && (
                <option value={current.egress_proxy_id}>{t("providers.egressMissing")} · {current.egress_proxy_id}</option>
              )}
              {egress.items.map((proxy) => (
                <option value={proxy.id} key={proxy.id}>{proxy.name} · {proxy.endpoint_host}:{proxy.endpoint_port}</option>
              ))}
            </select>
          </Field>
          {current && enabledDeploymentCount > 0 && <p className="field-hint warning-text">{t("providers.egressLocked", { count: enabledDeploymentCount })}</p>}
          <div className="notice warning">
            <strong>{t("providers.egressTrustTitle")}</strong>
            <span>{t("providers.egressTrustDescription")}</span>
          </div>
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
            <select value={credentialID} onChange={(event) => { setCredentialID(event.target.value); setUsageWarningAcknowledged(false); setErrors((previous) => omitError(previous, "credentialID")); }}>
              {matchingCredentials.map((credential) => (
                <option value={credential.id} key={credential.id}>{credential.name} · {displayBoundBaseURL(credential.bound_base_url)}</option>
              ))}
            </select>
          </Field>
            </div>
          </section>
          {((mutation.isError && !stepUp.probing) || errors.capabilities) && (
            <div ref={submitError} tabIndex={-1} className="form-submit-error">
              {/* The one refusal with no field of its own to carry it. */}
              {errors.capabilities && <p role="alert">{errors.capabilities}</p>}
              {mutation.isError && !stepUp.probing && <ErrorState error={mutation.error} />}
            </div>
          )}
          {stepUp.asked && <ReauthFields values={stepUp.values} onChange={stepUp.setValues} description={t("auth.stepUpSecurityControl")} />}
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
            <button type="button" className="button ghost" disabled={mutation.isPending} onClick={onClose}>{t("common.cancel")}</button>
            {/* Every refusal reason is reported by the submit path rather than
                by a disabled button, which states nothing about why. */}
            <button className="button primary" disabled={mutation.isPending || policyRefreshPending || policyRefreshFailed || fixedRegionMismatch || (stepUp.asked && !stepUp.values.currentPassword)}>{current ? t("providers.save") : t("providers.createAndLoad")}</button>
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
