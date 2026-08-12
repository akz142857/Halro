import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, type InputHTMLAttributes, type KeyboardEvent } from "react";
import { api } from "../api";
import {
  ConfirmButton,
  EmptyState,
  ErrorState,
  Field,
  Loading,
  Modal,
  PageHeader,
  ReauthFields,
  StatusDot,
  useDirty,
  type ReauthValues,
} from "../components";
import { compactNumber, money } from "../format";
import type { TokenGuardPolicy, TokenGuardPreview } from "../types";
import { RedactionPoliciesSection } from "./RedactionPoliciesSection";
import { useTranslation } from "react-i18next";
import { useIsReadOnly } from "../session";

export function PoliciesPage() {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const [tab, setTab] = useState<PolicyTab>(policyTabFromURL);
  const [search, setSearch] = useState("");
  const [status, setStatus] = useState("");
  const [action, setAction] = useState("");
  const [editing, setEditing] = useState<TokenGuardPolicy | "new" | null>(null);
  const [previewing, setPreviewing] = useState<TokenGuardPolicy | null>(null);
  // Paged keys stay distinct from the plain list queries other pages run under the same
  // prefix: sharing a key would hand one observer the other's data shape. The shared
  // prefix keeps invalidateQueries(["token-guard-policies"]) refreshing both.
  const policies = useInfiniteQuery({
    queryKey: ["token-guard-policies", "paged"],
    initialPageParam: "",
    queryFn: ({ pageParam }) => api.tokenGuardPoliciesPage(`?${new URLSearchParams({ limit: "50", ...(pageParam ? { cursor: pageParam } : {}) })}`),
    getNextPageParam: (page) => page.next_cursor || undefined,
  });
  const redactionPolicies = useInfiniteQuery({
    queryKey: ["redaction-policies", "paged"],
    initialPageParam: "",
    queryFn: ({ pageParam }) => api.redactionPoliciesPage(`?${new URLSearchParams({ limit: "50", ...(pageParam ? { cursor: pageParam } : {}) })}`),
    getNextPageParam: (page) => page.next_cursor || undefined,
  });
  const tokenItems = policies.data?.pages.flatMap((page) => page.items) ?? [];
  const redactionItems = redactionPolicies.data?.pages.flatMap((page) => page.items) ?? [];
  const normalizedSearch = search.trim().toLowerCase();
  const visible = tokenItems.filter((policy) =>
    (!normalizedSearch || policy.name.toLowerCase().includes(normalizedSearch) || policy.id.toLowerCase().includes(normalizedSearch)) &&
    (!status || (status === "enabled") === policy.enabled) &&
    (!action || policy.action === action),
  );
  const queryClient = useQueryClient();
  const remove = useMutation({
    mutationFn: ({ policy, reauth }: { policy: TokenGuardPolicy; reauth: ReauthValues }) =>
      api.deleteTokenGuardPolicy(policy.id, policy.revision, reauth),
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: ["token-guard-policies"] }),
  });
  const toggleStatus = useMutation({
    mutationFn: ({ policy, reauth }: { policy: TokenGuardPolicy; reauth: ReauthValues }) =>
      api.updateTokenGuardPolicy(policy.id, tokenGuardPolicyBody(policy, !policy.enabled), policy.revision, reauth),
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: ["token-guard-policies"] }),
  });
  useEffect(() => {
    const syncTab = () => setTab(policyTabFromURL());
    window.addEventListener("popstate", syncTab);
    return () => window.removeEventListener("popstate", syncTab);
  }, []);
  const selectTab = (nextTab: PolicyTab) => {
    if (nextTab === tab) return;
    setTab(nextTab);
    const url = new URL(window.location.href);
    url.searchParams.set("tab", nextTab);
    window.history.pushState({}, "", `${url.pathname}${url.search}${url.hash}`);
  };
  // APG tab behaviour: the tab strip is one stop in the tab order and the arrow
  // keys move between tabs. Without it a keyboard operator who reaches the strip
  // can only ever activate the tab they landed on.
  const onTabKeys = (event: KeyboardEvent<HTMLDivElement>) => {
    const index = policyTabs.indexOf(tab);
    const next = event.key === "ArrowRight" ? (index + 1) % policyTabs.length
      : event.key === "ArrowLeft" ? (index + policyTabs.length - 1) % policyTabs.length
        : event.key === "Home" ? 0
          : event.key === "End" ? policyTabs.length - 1
            : -1;
    if (next < 0) return;
    event.preventDefault();
    selectTab(policyTabs[next]);
    document.getElementById(policyTabID(policyTabs[next]))?.focus();
  };
  return (
    <>
      <PageHeader
        eyebrow={t("policyManagement.eyebrow")}
        title={t("policyManagement.title")}
        description={t("policyManagement.description")}
      />
      <div className="provider-tabs-shell policy-tabs-shell">
        <div className="provider-tabs policy-tabs" role="tablist" aria-label={t("policyManagement.views")} onKeyDown={onTabKeys}>
          <button role="tab" id={policyTabID("token")} aria-controls={policyPanelID("token")} aria-selected={tab === "token"} tabIndex={tab === "token" ? 0 : -1} onClick={() => selectTab("token")}>{t("policies.title")} <span>{tokenItems.length}{policies.hasNextPage ? "+" : ""}</span></button>
          <button role="tab" id={policyTabID("redaction")} aria-controls={policyPanelID("redaction")} aria-selected={tab === "redaction"} tabIndex={tab === "redaction" ? 0 : -1} onClick={() => selectTab("redaction")}>{t("redaction.title")} <span>{redactionItems.length}{redactionPolicies.hasNextPage ? "+" : ""}</span></button>
        </div>
      {tab === "token" && <section className="policy-management-panel" role="tabpanel" id={policyPanelID("token")} aria-labelledby={policyTabID("token")}>
        <div className="filter-bar policy-filter-bar">
          <label><span>{t("policyManagement.search")}</span><input value={search} onChange={(event) => setSearch(event.target.value)} placeholder={t("policyManagement.searchPlaceholder")} /></label>
          <label><span>{t("policyManagement.status")}</span><select value={status} onChange={(event) => setStatus(event.target.value)}><option value="">{t("policyManagement.all")}</option><option value="enabled">{t("common.enabled")}</option><option value="disabled">{t("common.disabled")}</option></select></label>
          <label><span>{t("policies.action")}</span><select value={action} onChange={(event) => setAction(event.target.value)}><option value="">{t("policyManagement.all")}</option><option value="observe">{t("policies.observe")}</option><option value="alert">{t("policies.alert")}</option><option value="temporary_block">{t("policies.temporaryBlock")}</option></select></label>
          <button className="button primary" disabled={readOnly} onClick={() => setEditing("new")}>{t("policies.create")}</button>
        </div>
        {policies.isPending && <Loading />}
        {policies.isError && <ErrorState error={policies.error} />}
        {!policies.isPending && tokenItems.length === 0 && <EmptyState title={t("policies.emptyTitle")} action={<button className="button primary" disabled={readOnly} onClick={() => setEditing("new")}>{t("policies.baseline")}</button>}>{t("policies.emptyDescription")}</EmptyState>}
        {!!tokenItems.length && <div className="table-shell policy-table-shell"><table className="policy-table"><thead><tr><th>{t("policyManagement.policy")}</th><th>{t("policyManagement.status")}</th><th>{t("policies.action")}</th><th>{t("policyManagement.summary")}</th><th>{t("policyManagement.bindings")}</th><th>{t("policyManagement.actions")}</th></tr></thead><tbody>{visible.map((policy) => {
          // One row's mutation must not disable every other row's controls, and a
          // failure has to name the row it came from — a notice under the table
          // says only that "something" failed.
          const rowBusy = toggleStatus.isPending && toggleStatus.variables?.policy.id === policy.id;
          const rowFailed = toggleStatus.isError && toggleStatus.variables?.policy.id === policy.id;
          return <tr key={policy.id}>
          <td><strong>{policy.name}</strong><code>{policy.id}</code></td>
          <td><span className="inline-status"><StatusDot ok={policy.enabled} />{policy.enabled ? t("common.enabled") : t("common.disabled")}</span></td>
          <td><span className={`badge ${policy.action === "temporary_block" ? "warning" : ""}`}>{policyActionLabel(t, policy.action)}</span></td>
          <td><strong>{compactNumber(policy.request_tokens)} / {compactNumber(policy.tokens_per_minute)} TPM</strong><small>{money(policy.cost_micros_per_minute)} · {policy.concurrency} {t("policies.concurrency")} · {policy.ewma_enabled ? `${policy.ewma_multiplier}× EWMA` : t("policies.ewmaOff")}</small></td>
          <td>{t("policyManagement.projectCount", { count: policy.bound_projects ?? 0 })}</td>
          {/* Switching a policy off stops enforcement for every Project bound to
              it, which is the same class of consequence as deleting it — so it
              asks first and states the blast radius. */}
          <td><div className="row-actions policy-row-actions">{policy.enabled
            ? <ConfirmButton className="button secondary policy-status-toggle" label={t("policies.disableAction")} title={t("policies.disableTitle")} confirmLabel={t("policies.disableConfirm", { name: policy.name, count: policy.bound_projects ?? 0 })} disabled={rowBusy} requireStepUp onConfirm={(reauth) => toggleStatus.mutateAsync({ policy, reauth })} />
            : <ConfirmButton className="button secondary policy-status-toggle" label={t("policies.enableAction")} confirmLabel={t("policies.enableConfirm", { name: policy.name })} disabled={rowBusy} requireStepUp onConfirm={(reauth) => toggleStatus.mutateAsync({ policy, reauth })} />}<button className="button ghost" onClick={() => setPreviewing(policy)}>{t("policies.simulate")}</button><button className="button ghost" disabled={readOnly || rowBusy} title={readOnly ? t("navigation.readOnlyAction") : undefined} onClick={() => setEditing(policy)}>{t("common.edit")}</button><ConfirmButton label={t("common.delete")} confirmLabel={t("policies.deleteConfirm", { name: policy.name })} disabled={remove.isPending || rowBusy} requireStepUp onConfirm={(reauth) => remove.mutateAsync({ policy, reauth })} /></div>{rowFailed && <ErrorState error={toggleStatus.error} />}</td>
        </tr>;
        })}</tbody></table>{remove.isError && <ErrorState error={remove.error} />}</div>}
        {policies.hasNextPage && <button className="button ghost policy-load-more" disabled={policies.isFetchingNextPage} onClick={() => policies.fetchNextPage()}>{policies.isFetchingNextPage ? t("common.loading") : t("common.loadMore")}</button>}
      </section>}
      {tab === "redaction" && <RedactionPoliciesSection panelID={policyPanelID("redaction")} labelledBy={policyTabID("redaction")} policies={redactionItems} isPending={redactionPolicies.isPending} error={redactionPolicies.isError ? redactionPolicies.error : undefined} hasNextPage={redactionPolicies.hasNextPage} isFetchingNextPage={redactionPolicies.isFetchingNextPage} onLoadMore={() => redactionPolicies.fetchNextPage()} />}
      </div>
      {editing && <PolicyForm current={editing === "new" ? undefined : editing} onClose={() => setEditing(null)} />}
      {previewing && <PolicyPreview policy={previewing} onClose={() => setPreviewing(null)} />}
    </>
  );
}

type PolicyTab = "token" | "redaction";

const policyTabs: PolicyTab[] = ["token", "redaction"];

function policyTabID(tab: PolicyTab) {
  return `policy-tab-${tab}`;
}

function policyPanelID(tab: PolicyTab) {
  return `policy-panel-${tab}`;
}

function policyTabFromURL(): PolicyTab {
  return new URLSearchParams(window.location.search).get("tab") === "redaction" ? "redaction" : "token";
}

function tokenGuardPolicyBody(policy: TokenGuardPolicy, enabled: boolean) {
  return {
    name: policy.name,
    enabled,
    action: policy.action,
    request_tokens: policy.request_tokens,
    tokens_per_minute: policy.tokens_per_minute,
    cost_micros_per_minute: policy.cost_micros_per_minute,
    error_rate: policy.error_rate,
    minimum_samples: policy.minimum_samples,
    concurrency: policy.concurrency,
    unique_ips_per_minute: policy.unique_ips_per_minute,
    violations_before_block: policy.violations_before_block,
    block_ttl_seconds: policy.block_ttl_seconds,
    cooldown_seconds: policy.cooldown_seconds,
    ewma_enabled: policy.ewma_enabled,
    ewma_alpha: policy.ewma_alpha,
    ewma_multiplier: policy.ewma_multiplier,
    ewma_minimum_samples: policy.ewma_minimum_samples,
    ewma_warmup_seconds: policy.ewma_warmup_seconds,
    ewma_evaluation_window_seconds: policy.ewma_evaluation_window_seconds,
    ewma_cooldown_seconds: policy.ewma_cooldown_seconds,
    ewma_absolute_rpm: policy.ewma_absolute_rpm,
    ewma_absolute_tpm: policy.ewma_absolute_tpm,
    ewma_absolute_tokens_per_request: policy.ewma_absolute_tokens_per_request,
    ewma_absolute_cost_micros_per_minute: policy.ewma_absolute_cost_micros_per_minute,
  };
}

const defaultAdaptiveSettings = {
  alpha: 0.2,
  multiplier: 3,
  minimumSamples: 100,
  warmupSeconds: 3600,
  evaluationWindowSeconds: 60,
  cooldownSeconds: 300,
  absoluteRPM: 60,
  absoluteTPM: 50_000,
  absoluteTokensPerRequest: 4_000,
  absoluteCostUSDPerMinute: 1,
} as const;

type AdaptiveSettings = { -readonly [K in keyof typeof defaultAdaptiveSettings]: number };

function adaptiveSettingsOf(policy: TokenGuardPolicy): AdaptiveSettings {
  return {
    alpha: policy.ewma_alpha,
    multiplier: policy.ewma_multiplier,
    minimumSamples: policy.ewma_minimum_samples,
    warmupSeconds: policy.ewma_warmup_seconds,
    evaluationWindowSeconds: policy.ewma_evaluation_window_seconds,
    cooldownSeconds: policy.ewma_cooldown_seconds,
    absoluteRPM: policy.ewma_absolute_rpm,
    absoluteTPM: policy.ewma_absolute_tpm,
    absoluteTokensPerRequest: policy.ewma_absolute_tokens_per_request,
    absoluteCostUSDPerMinute: policy.ewma_absolute_cost_micros_per_minute / 1_000_000,
  };
}

function customizedAdaptiveSettings(values: AdaptiveSettings) {
  return (Object.keys(defaultAdaptiveSettings) as Array<keyof AdaptiveSettings>)
    .some((key) => values[key] !== defaultAdaptiveSettings[key]);
}

// The adaptive block opens by itself whenever the saved policy cannot be described
// by the recommended defaults — including a policy with adaptive detection turned
// off, whose switch would otherwise be unreachable and whose values would be
// judged by a validator for fields nobody can see.
function opensAdaptiveSettings(policy?: TokenGuardPolicy) {
  if (!policy) return false;
  return !policy.ewma_enabled || customizedAdaptiveSettings(adaptiveSettingsOf(policy));
}

function PolicyForm({
  current,
  onClose,
}: {
  current?: TokenGuardPolicy;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const [name, setName] = useState(current?.name ?? "");
  const [action, setAction] = useState<TokenGuardPolicy["action"]>(current?.action ?? "alert");
  const [enabled, setEnabled] = useState(current?.enabled ?? false);
  const [errors, setErrors] = useState<Record<string, string>>({});
  const [requestTokens, setRequestTokens] = useState(current?.request_tokens ?? 32_000);
  const [tokensPerMinute, setTokensPerMinute] = useState(current?.tokens_per_minute ?? 200_000);
  const [costPerMinute, setCostPerMinute] = useState((current?.cost_micros_per_minute ?? 5_000_000) / 1_000_000);
  const [concurrency, setConcurrency] = useState(current?.concurrency ?? 20);
  const [errorRate, setErrorRate] = useState((current?.error_rate ?? 0.2) * 100);
  const [minimumSamples, setMinimumSamples] = useState(current?.minimum_samples ?? 10);
  const [uniqueIPs, setUniqueIPs] = useState(current?.unique_ips_per_minute ?? 10);
  const [violations, setViolations] = useState(current?.violations_before_block ?? 2);
  const [blockTTL, setBlockTTL] = useState(current?.block_ttl_seconds ?? 300);
  const [cooldown, setCooldown] = useState(current?.cooldown_seconds ?? 60);
  const [advancedSettings, setAdvancedSettings] = useState(() => opensAdaptiveSettings(current));
  // Read from the policy rather than hardcoded: saving an edit used to switch
  // adaptive detection on for a policy that had it off, while the list kept
  // rendering "EWMA off".
  const [ewmaEnabled, setEWMAEnabled] = useState(current?.ewma_enabled ?? true);
  const [ewmaAlpha, setEWMAAlpha] = useState(current?.ewma_alpha ?? defaultAdaptiveSettings.alpha);
  const [ewmaMultiplier, setEWMAMultiplier] = useState(current?.ewma_multiplier ?? defaultAdaptiveSettings.multiplier);
  const [ewmaMinimumSamples, setEWMAMinimumSamples] = useState(current?.ewma_minimum_samples ?? defaultAdaptiveSettings.minimumSamples);
  const [ewmaWarmup, setEWMAWarmup] = useState(current?.ewma_warmup_seconds ?? defaultAdaptiveSettings.warmupSeconds);
  const [ewmaWindow, setEWMAWindow] = useState(current?.ewma_evaluation_window_seconds ?? defaultAdaptiveSettings.evaluationWindowSeconds);
  const [ewmaCooldown, setEWMACooldown] = useState(current?.ewma_cooldown_seconds ?? defaultAdaptiveSettings.cooldownSeconds);
  const [ewmaRPMFloor, setEWMARPMFloor] = useState(current?.ewma_absolute_rpm ?? defaultAdaptiveSettings.absoluteRPM);
  const [ewmaTPMFloor, setEWMATPMFloor] = useState(current?.ewma_absolute_tpm ?? defaultAdaptiveSettings.absoluteTPM);
  const [ewmaTokensFloor, setEWMATokensFloor] = useState(current?.ewma_absolute_tokens_per_request ?? defaultAdaptiveSettings.absoluteTokensPerRequest);
  const [ewmaCostFloor, setEWMACostFloor] = useState((current?.ewma_absolute_cost_micros_per_minute ?? defaultAdaptiveSettings.absoluteCostUSDPerMinute * 1_000_000) / 1_000_000);
  const adaptiveValues: AdaptiveSettings = {
    alpha: ewmaAlpha,
    multiplier: ewmaMultiplier,
    minimumSamples: ewmaMinimumSamples,
    warmupSeconds: ewmaWarmup,
    evaluationWindowSeconds: ewmaWindow,
    cooldownSeconds: ewmaCooldown,
    absoluteRPM: ewmaRPMFloor,
    absoluteTPM: ewmaTPMFloor,
    absoluteTokensPerRequest: ewmaTokensFloor,
    absoluteCostUSDPerMinute: ewmaCostFloor,
  };
  // Escape, the backdrop and Cancel all discard about thirty fields, so this form
  // declares dirtiness the same way the redaction form does.
  const dirty = useDirty({
    name, action, enabled, requestTokens, tokensPerMinute, costPerMinute, concurrency,
    errorRate, minimumSamples, uniqueIPs, violations, blockTTL, cooldown, ewmaEnabled,
    ewmaAlpha, ewmaMultiplier, ewmaMinimumSamples, ewmaWarmup, ewmaWindow, ewmaCooldown,
    ewmaRPMFloor, ewmaTPMFloor, ewmaTokensFloor, ewmaCostFloor,
  });
  const queryClient = useQueryClient();
  const body = {
    name, enabled, action,
    request_tokens: requestTokens,
    tokens_per_minute: tokensPerMinute,
    cost_micros_per_minute: Math.round(costPerMinute * 1_000_000),
    error_rate: errorRate / 100,
    minimum_samples: minimumSamples,
    concurrency,
    unique_ips_per_minute: uniqueIPs,
    violations_before_block: action === "temporary_block" ? Math.max(violations, 2) : violations,
    block_ttl_seconds: blockTTL,
    cooldown_seconds: cooldown,
    ewma_enabled: ewmaEnabled,
    ewma_alpha: ewmaAlpha,
    ewma_multiplier: ewmaMultiplier,
    ewma_minimum_samples: ewmaMinimumSamples,
    ewma_warmup_seconds: ewmaWarmup,
    ewma_evaluation_window_seconds: ewmaWindow,
    ewma_cooldown_seconds: ewmaCooldown,
    ewma_absolute_rpm: ewmaRPMFloor,
    ewma_absolute_tpm: ewmaTPMFloor,
    ewma_absolute_tokens_per_request: ewmaTokensFloor,
    ewma_absolute_cost_micros_per_minute: Math.round(ewmaCostFloor * 1_000_000),
  };
  // Editing an existing policy can raise every limit to unlimited or switch it
  // off, so the server asks who is doing it; creating one cannot.
  const [reauth, setReauth] = useState<ReauthValues>({ currentPassword: "", totpCode: "" });
  const mutation = useMutation({
    mutationFn: () => current
      ? api.updateTokenGuardPolicy(current.id, body, current.revision, reauth)
      : api.createTokenGuardPolicy(body),
    onSettled: () => setReauth((values) => ({ ...values, totpCode: "" })),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["token-guard-policies"] });
      onClose();
    },
  });
  return (
    <Modal wide title={current ? t("policies.edit") : t("policies.createTitle")} onClose={onClose} closeDisabled={mutation.isPending} dirty={dirty}>
      <form className="policy-form" aria-busy={mutation.isPending} onSubmit={(event) => {
        event.preventDefault();
        const nextErrors = validatePolicy({
          name, requestTokens, tokensPerMinute, costPerMinute, concurrency, errorRate,
          minimumSamples, uniqueIPs, action, violations, blockTTL, cooldown, ewmaEnabled,
          ewmaAlpha, ewmaMultiplier, ewmaMinimumSamples, ewmaWarmup, ewmaWindow,
          ewmaCooldown, ewmaRPMFloor, ewmaTPMFloor, ewmaTokensFloor, ewmaCostFloor,
        }, t);
        setErrors(nextErrors);
        // A reported error whose field is inside a collapsed block leaves the
        // operator counting problems they cannot reach, so the block opens.
        if (Object.keys(nextErrors).some((key) => key.startsWith("ewma"))) setAdvancedSettings(true);
        if (!Object.keys(nextErrors).length) mutation.mutate();
      }}>
        {!!Object.keys(errors).length && <div className="policy-error-summary" role="alert">{t("policies.errorSummary", { count: Object.keys(errors).length })}</div>}
        <section className="policy-form-section policy-basics-section">
          <header><div><h3>{t("policies.identitySection")}</h3><p>{t("policies.identityDescription")}</p></div></header>
          <div className="policy-basics-name">
            <Field label={t("policies.name")} error={errors.name}><input autoFocus data-modal-initial value={name} onChange={(event) => setName(event.target.value)} /></Field>
          </div>
          {/* A group rather than a fieldset: the legend box interrupts the section
              border it sits on, and the radios already have a name binding them
              together. Same shape the project form's model picker uses. */}
          <div className="policy-action-section" role="group" aria-labelledby="policy-action-heading">
            <h3 id="policy-action-heading">{t("policies.actionSection")}</h3>
            <p>{t("policies.actionDescription")}</p>
            <div className="policy-action-cards">
              {(["observe", "alert", "temporary_block"] as const).map((value) => <label className={`policy-action-card ${action === value ? "selected" : ""}`} key={value}><input type="radio" name="policy-action" value={value} checked={action === value} onChange={() => setAction(value)} /><span><strong>{policyActionLabel(t, value)}</strong><small>{t(`policies.${value}Description`)}</small></span></label>)}
            </div>
          </div>
        </section>
        <section className="policy-form-section">
          <header><div><h3>{t("policies.fixedLimitsSection")}</h3><p>{t("policies.zeroDisables")}</p></div></header>
          <h4>{t("policies.trafficLimits")}</h4><div className="policy-section-grid">
            <Field label={t("policies.requestLimit")} hint={t("policies.tokensUnit")} error={errors.requestTokens}><NumberInput value={requestTokens} set={setRequestTokens} /></Field>
            <Field label={t("policies.minuteLimit")} hint={t("policies.tpmUnit")} error={errors.tokensPerMinute}><NumberInput value={tokensPerMinute} set={setTokensPerMinute} /></Field>
          </div>
          <h4>{t("policies.capacityLimits")}</h4><div className="policy-section-grid">
            <Field label={t("policies.costLimit")} hint="USD/min" error={errors.costPerMinute}><NumberInput value={costPerMinute} set={setCostPerMinute} step=".01" /></Field>
            <Field label={t("policies.concurrencyLimit")} error={errors.concurrency}><NumberInput value={concurrency} set={setConcurrency} /></Field>
          </div>
          <h4>{t("policies.qualityLimits")}</h4><div className="policy-section-grid">
            <Field label={t("policies.errorThreshold")} hint="%" error={errors.errorRate}><NumberInput value={errorRate} set={setErrorRate} step=".1" /></Field>
            <Field label={t("policies.minimumSamples")} hint={t("policies.minimumSamplesHint")} error={errors.minimumSamples}><NumberInput value={minimumSamples} set={setMinimumSamples} /></Field>
            <Field label={t("policies.uniqueSources")} hint={t("policies.perMinuteUnit")} error={errors.uniqueIPs}><NumberInput value={uniqueIPs} set={setUniqueIPs} /></Field>
          </div>
        </section>
        {action === "temporary_block" && (
          <section className="policy-form-section policy-block-section">
            <header><div><h3>{t("policies.blockSection")}</h3><p>{t("policies.blockCondition", { violations, samples: minimumSamples, ttl: blockTTL })}</p></div><span className="badge warning">{t("policies.notImmediate")}</span></header>
            <div className="notice warning">{t("policies.blockWarning")}</div>
            <div className="policy-section-grid">
              <Field label={t("policies.violations")} error={errors.violations}><NumberInput value={violations} set={setViolations} /></Field>
              <Field label={t("policies.blockTTL")} hint={t("policies.secondsUnit")} error={errors.blockTTL}><NumberInput value={blockTTL} set={setBlockTTL} /></Field>
              <Field label={t("policies.cooldown")} hint={t("policies.cooldownMeaning")} error={errors.cooldown}><NumberInput value={cooldown} set={setCooldown} /></Field>
            </div>
          </section>
        )}
        <section className="policy-form-section policy-ewma-section">
          {/* Purely a disclosure. Clearing it used to reset ten tuned values with
              no warning and no undo, which reads as folding a panel away. */}
          <header><div><h3>{t("policies.ewmaSection")}</h3><p>{t("policies.ewmaSectionDescription")}</p></div><label className="policy-switch"><input type="checkbox" aria-label={t("policies.advancedSettings")} aria-expanded={advancedSettings} checked={advancedSettings} onChange={(event) => setAdvancedSettings(event.target.checked)} /><span>{t("policies.advancedSettings")}</span></label></header>
          <div className="notice"> <strong>{t("policies.detectOnlyBadge")}</strong>{t("policies.ewmaHint")}</div>
        {advancedSettings && (
          <div className="policy-ewma-fields">
            <label className="check-row">
              <input type="checkbox" aria-label={t("policies.ewmaEnable")} checked={ewmaEnabled} onChange={(event) => setEWMAEnabled(event.target.checked)} />
              <span><strong>{t("policies.ewmaEnable")} · {ewmaEnabled ? t("common.enabled") : t("common.disabled")}</strong><small>{t("policies.ewmaEnableHint")}</small></span>
            </label>
            <h4>{t("policies.baselineModel")}</h4><div className="policy-section-grid"><Field label={t("policies.ewmaAlpha")} error={errors.ewmaAlpha}><NumberInput value={ewmaAlpha} set={setEWMAAlpha} step=".05" /></Field><Field label={t("policies.multiplier")} error={errors.ewmaMultiplier}><NumberInput value={ewmaMultiplier} set={setEWMAMultiplier} step=".1" /></Field><Field label={t("policies.baselineSamples")} error={errors.ewmaMinimumSamples}><NumberInput value={ewmaMinimumSamples} set={setEWMAMinimumSamples} /></Field></div>
            <h4>{t("policies.timeControls")}</h4><div className="policy-section-grid"><Field label={t("policies.warmup")} error={errors.ewmaWarmup}><NumberInput value={ewmaWarmup} set={setEWMAWarmup} /></Field><Field label={t("policies.window")} error={errors.ewmaWindow}><NumberInput value={ewmaWindow} set={setEWMAWindow} /></Field><Field label={t("policies.alertCooldown")} error={errors.ewmaCooldown}><NumberInput value={ewmaCooldown} set={setEWMACooldown} /></Field></div>
            <h4>{t("policies.absoluteFloors")}</h4><div className="policy-section-grid"><Field label={t("policies.rpmFloor")} error={errors.ewmaFloors}><NumberInput value={ewmaRPMFloor} set={setEWMARPMFloor} /></Field><Field label={t("policies.tpmFloor")} error={errors.ewmaFloors}><NumberInput value={ewmaTPMFloor} set={setEWMATPMFloor} /></Field><Field label={t("policies.tokenFloor")} error={errors.ewmaFloors}><NumberInput value={ewmaTokensFloor} set={setEWMATokensFloor} step=".1" /></Field><Field label={t("policies.costFloor")} error={errors.ewmaFloors}><NumberInput value={ewmaCostFloor} set={setEWMACostFloor} step=".01" /></Field></div>
          </div>
        )}
        </section>
        {mutation.isError && <ErrorState error={mutation.error} />}
        {current && <ReauthFields values={reauth} onChange={setReauth} description={t("auth.stepUpSecurityControl")} />}
        <div className="form-actions sticky-form-actions">
          <div className="form-footer-state">
            <label className="form-footer-enable">
              <input type="checkbox" aria-label={t("policies.enable")} checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />
              <span><strong>{t("policies.enable")} · {enabled ? t("common.enabled") : t("common.disabled")}</strong><small>{current?.bound_projects ? t("policies.boundImpact", { count: current.bound_projects }) : t("policies.unboundImpact")}</small></span>
            </label>
            <div className="form-footer-summary"><strong>{policyActionLabel(t, action)}</strong><small>{!ewmaEnabled
              ? t("policies.ewmaOffSummary")
              : customizedAdaptiveSettings(adaptiveValues)
                ? t("policies.ewmaSummary", { multiplier: ewmaMultiplier, window: ewmaWindow })
                : t("policies.ewmaDefaultSummary")}</small></div>
          </div>
          <button type="button" className="button ghost" disabled={mutation.isPending} data-modal-close>{t("common.cancel")}</button>
          <button className="button primary" disabled={mutation.isPending || (Boolean(current) && !reauth.currentPassword)}>{mutation.isPending ? t("common.saving") : enabled && current ? t("policies.saveAndApply") : t("policies.save")}</button>
        </div>
      </form>
    </Modal>
  );
}

function PolicyPreview({ policy, onClose }: { policy: TokenGuardPolicy; onClose: () => void }) {
  const { t } = useTranslation();
  const [tokens, setTokens] = useState(policy.request_tokens || 1_000);
  const [estimatedCostUSD, setEstimatedCostUSD] = useState(0);
  const [windowTokens, setWindowTokens] = useState(0);
  const [concurrency, setConcurrency] = useState(1);
  const [costUSD, setCostUSD] = useState(0);
  const [requests, setRequests] = useState(10);
  const [errors, setErrors] = useState(0);
  const [uniqueIPs, setUniqueIPs] = useState(1);
  const [hasNewSource, setHasNewSource] = useState(true);
  const [result, setResult] = useState<TokenGuardPreview | null>(null);
  const mutation = useMutation({
    mutationFn: () => api.previewTokenGuardPolicy(policy.id, {
      estimated_tokens: tokens,
      estimated_cost_micros_usd: Math.round(estimatedCostUSD * 1_000_000),
      concurrency,
      has_new_source: hasNewSource,
      window: {
        requests, tokens: windowTokens, cost_micros_usd: Math.round(costUSD * 1_000_000),
        errors, unique_ips: uniqueIPs,
      },
    }),
    onSuccess: setResult,
  });
  const reasonSummary = result?.violated
    ? tokenGuardReasonSummary(t, result.reason ?? "", {
      tokens,
      estimatedCostMicrosUSD: Math.round(estimatedCostUSD * 1_000_000),
      windowTokens,
      windowCostMicrosUSD: Math.round(costUSD * 1_000_000),
      concurrency,
      requests,
      errors,
    }, policy)
    : null;
  return (
    <Modal wide title={t("policies.previewTitle", { name: policy.name })} onClose={onClose} closeDisabled={mutation.isPending}>
      <form className="policy-preview-form" onSubmit={(event) => { event.preventDefault(); mutation.mutate(); }}>
        <p className="policy-preview-context">{t("policies.previewWindowHint")}</p>
        <div className="policy-preview-grid">
          <Field label={t("policies.estimatedTokens")}><NumberInput value={tokens} set={setTokens} /></Field>
          <Field label={t("policies.estimatedCost")}><NumberInput value={estimatedCostUSD} set={setEstimatedCostUSD} step=".01" /></Field>
          <Field label={t("policies.windowTokens")}><NumberInput value={windowTokens} set={setWindowTokens} /></Field>
          <Field label={t("policies.windowCost")}><NumberInput value={costUSD} set={setCostUSD} step=".01" /></Field>
          <Field label={t("policies.windowRequests")}><NumberInput value={requests} set={setRequests} /></Field>
          <Field label={t("policies.windowErrors")}><NumberInput value={errors} set={setErrors} /></Field>
          <Field label={t("policies.currentConcurrency")}><NumberInput value={concurrency} set={setConcurrency} /></Field>
          <Field label={t("policies.windowUniqueIPs")}><NumberInput value={uniqueIPs} set={setUniqueIPs} /></Field>
        </div>
        <label className="check-row"><input type="checkbox" checked={hasNewSource} onChange={(event) => setHasNewSource(event.target.checked)} /><span>{t("policies.newSource")}</span></label>
        {result && (
          <div className={`preview-result ${result.violated ? "violated" : ""}`}>
            <StatusDot ok={!result.violated} />
            <div>
              <strong title={result.reason}>{reasonSummary?.title ?? t("policies.noHit")}</strong>
              {reasonSummary?.detail && <small>{reasonSummary.detail}</small>}
              <small>{result.violated
                ? t("policies.resultAction", { action: policyPreviewActionLabel(t, result.action) })
                : t("policies.noHitDetail")}</small>
            </div>
          </div>
        )}
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions">
          <button type="button" className="button ghost" disabled={mutation.isPending} onClick={onClose}>{t("common.close")}</button>
          <button className="button primary" disabled={mutation.isPending}>{t("policies.run")}</button>
        </div>
      </form>
    </Modal>
  );
}

function NumberInput({
  value,
  set,
  step = "1",
  ...inputProps
}: {
  value: number;
  set: (value: number) => void;
  step?: string;
} & Omit<InputHTMLAttributes<HTMLInputElement>, "value" | "onChange" | "type" | "step">) {
  return <input {...inputProps} type="number" min="0" step={step} value={value} onChange={(event) => set(Number(event.target.value))} />;
}

type PolicyValues = {
  name: string;
  action: TokenGuardPolicy["action"];
  requestTokens: number; tokensPerMinute: number; costPerMinute: number;
  concurrency: number; errorRate: number; minimumSamples: number; uniqueIPs: number;
  violations: number; blockTTL: number; cooldown: number; ewmaEnabled: boolean;
  ewmaAlpha: number; ewmaMultiplier: number; ewmaMinimumSamples: number;
  ewmaWarmup: number; ewmaWindow: number; ewmaCooldown: number;
  ewmaRPMFloor: number; ewmaTPMFloor: number; ewmaTokensFloor: number; ewmaCostFloor: number;
};

function validatePolicy(value: PolicyValues, t: ReturnType<typeof useTranslation>["t"]) {
  const errors: Record<string, string> = {};
  if (!value.name.trim()) errors.name = t("policies.validationRequired");
  const wholeNonNegative: Array<[keyof PolicyValues, number]> = [
    ["requestTokens", value.requestTokens], ["tokensPerMinute", value.tokensPerMinute],
    ["concurrency", value.concurrency], ["minimumSamples", value.minimumSamples],
    ["uniqueIPs", value.uniqueIPs],
  ];
  for (const [key, number] of wholeNonNegative) {
    if (!Number.isInteger(number) || number < 0) errors[key] = t("policies.validationNonNegativeInteger");
  }
  if (!Number.isFinite(value.costPerMinute) || value.costPerMinute < 0) errors.costPerMinute = t("policies.validationNonNegative");
  if (!Number.isFinite(value.errorRate) || value.errorRate < 0 || value.errorRate > 100) errors.errorRate = t("policies.validationPercent");
  if (value.action === "temporary_block") {
    if (!Number.isInteger(value.violations) || value.violations < 2) errors.violations = t("policies.validationAtLeastTwo");
    if (!Number.isInteger(value.blockTTL) || value.blockTTL <= 0) errors.blockTTL = t("policies.validationPositiveInteger");
    if (!Number.isInteger(value.cooldown) || value.cooldown <= 0) errors.cooldown = t("policies.validationPositiveInteger");
  }
  if (value.ewmaEnabled) {
    if (!(value.ewmaAlpha > 0 && value.ewmaAlpha <= 1)) errors.ewmaAlpha = t("policies.validationAlpha");
    if (!(value.ewmaMultiplier > 1)) errors.ewmaMultiplier = t("policies.validationMultiplier");
    if (!Number.isInteger(value.ewmaMinimumSamples) || value.ewmaMinimumSamples < 10) errors.ewmaMinimumSamples = t("policies.validationBaselineSamples");
    if (!Number.isInteger(value.ewmaWindow) || value.ewmaWindow < 10 || value.ewmaWindow > 300 || value.ewmaWindow % 10 !== 0) errors.ewmaWindow = t("policies.validationWindow");
    if (!Number.isInteger(value.ewmaWarmup) || value.ewmaWarmup < value.ewmaWindow) errors.ewmaWarmup = t("policies.validationWarmup");
    if (!Number.isInteger(value.ewmaCooldown) || value.ewmaCooldown <= 0) errors.ewmaCooldown = t("policies.validationPositiveInteger");
    const floors = [value.ewmaRPMFloor, value.ewmaTPMFloor, value.ewmaTokensFloor, value.ewmaCostFloor];
    if (floors.some((floor) => !Number.isFinite(floor) || floor < 0) || floors.every((floor) => floor === 0)) errors.ewmaFloors = t("policies.validationFloors");
  }
  return errors;
}

function policyActionLabel(t: ReturnType<typeof useTranslation>["t"], action: string) {
  if (action === "temporary_block") return t("policies.temporaryBlock");
  if (action === "alert") return t("policies.alert");
  return t("policies.observe");
}

function policyPreviewActionLabel(t: ReturnType<typeof useTranslation>["t"], action: string) {
  if (action === "temporary_block") return t("policies.previewActions.temporaryBlock");
  if (action === "alert") return t("policies.previewActions.alert");
  return t("policies.previewActions.observe");
}

function tokenGuardReasonSummary(
  t: ReturnType<typeof useTranslation>["t"],
  reason: string,
  input: {
    tokens: number;
    estimatedCostMicrosUSD: number;
    windowTokens: number;
    windowCostMicrosUSD: number;
    concurrency: number;
    requests: number;
    errors: number;
  },
  policy: TokenGuardPolicy,
) {
  switch (reason) {
  case "request_tokens":
    return {
      title: t("policies.previewReasons.requestTokens"),
      detail: t("policies.previewReasonDetails.requestTokens", { actual: input.tokens, limit: policy.request_tokens }),
    };
  case "tokens_per_minute":
    return {
      title: t("policies.previewReasons.tokensPerMinute"),
      detail: t("policies.previewReasonDetails.tokensPerMinute", { actual: input.windowTokens + input.tokens, limit: policy.tokens_per_minute }),
    };
  case "cost_per_minute":
    return {
      title: t("policies.previewReasons.costPerMinute"),
      detail: t("policies.previewReasonDetails.costPerMinute", { actual: money(input.windowCostMicrosUSD + input.estimatedCostMicrosUSD), limit: money(policy.cost_micros_per_minute) }),
    };
  case "concurrency":
    return {
      title: t("policies.previewReasons.concurrency"),
      detail: t("policies.previewReasonDetails.concurrency", { actual: input.concurrency, limit: policy.concurrency }),
    };
  case "unique_ip":
    return {
      title: t("policies.previewReasons.uniqueIP"),
      detail: t("policies.previewReasonDetails.uniqueIP", { limit: policy.unique_ips_per_minute }),
    };
  case "error_rate": {
    const actual = input.requests > 0 ? Math.round(input.errors / input.requests * 10_000) / 100 : 0;
    return {
      title: t("policies.previewReasons.errorRate"),
      detail: t("policies.previewReasonDetails.errorRate", { actual, limit: Math.round(policy.error_rate * 10_000) / 100 }),
    };
  }
  default:
    return {
      title: t("policies.previewReasons.unknown"),
      detail: t("policies.previewReasonDetails.unknown", { reason }),
    };
  }
}
