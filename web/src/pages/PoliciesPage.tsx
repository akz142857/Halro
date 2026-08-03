import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent, type InputHTMLAttributes } from "react";
import { api } from "../api";
import {
  ConfirmButton,
  EmptyState,
  ErrorState,
  Field,
  Loading,
  Modal,
  PageHeader,
  StatusDot,
} from "../components";
import { compactNumber, money } from "../format";
import type { TokenGuardPolicy, TokenGuardPreview } from "../types";
import { RedactionPoliciesSection } from "./RedactionPoliciesSection";
import { useTranslation } from "react-i18next";

export function PoliciesPage() {
  const { t } = useTranslation();
  const [tab, setTab] = useState<"token" | "redaction">("token");
  const [search, setSearch] = useState("");
  const [status, setStatus] = useState("");
  const [action, setAction] = useState("");
  const [editing, setEditing] = useState<TokenGuardPolicy | "new" | null>(null);
  const [previewing, setPreviewing] = useState<TokenGuardPolicy | null>(null);
  const policies = useInfiniteQuery({
    queryKey: ["token-guard-policies"],
    initialPageParam: "",
    queryFn: ({ pageParam }) => api.tokenGuardPoliciesPage(`?${new URLSearchParams({ limit: "50", ...(pageParam ? { cursor: pageParam } : {}) })}`),
    getNextPageParam: (page) => page.next_cursor || undefined,
  });
  const redactionPolicies = useInfiniteQuery({
    queryKey: ["redaction-policies"],
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
    mutationFn: (policy: TokenGuardPolicy) =>
      api.deleteTokenGuardPolicy(policy.id, policy.revision),
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: ["token-guard-policies"] }),
  });
  return (
    <>
      <PageHeader
        eyebrow={t("policyManagement.eyebrow")}
        title={t("policyManagement.title")}
        description={t("policyManagement.description")}
      />
      <div className="provider-tabs-shell policy-tabs-shell">
        <div className="provider-tabs policy-tabs" role="tablist" aria-label={t("policyManagement.views")}>
          <button role="tab" aria-selected={tab === "token"} onClick={() => setTab("token")}>{t("policies.title")} <span>{tokenItems.length}{policies.hasNextPage ? "+" : ""}</span></button>
          <button role="tab" aria-selected={tab === "redaction"} onClick={() => setTab("redaction")}>{t("redaction.title")} <span>{redactionItems.length}{redactionPolicies.hasNextPage ? "+" : ""}</span></button>
        </div>
      {tab === "token" && <section className="policy-management-panel" role="tabpanel" aria-label={t("policies.title")}>
        <div className="filter-bar policy-filter-bar">
          <label><span>{t("policyManagement.search")}</span><input value={search} onChange={(event) => setSearch(event.target.value)} placeholder={t("policyManagement.searchPlaceholder")} /></label>
          <label><span>{t("policyManagement.status")}</span><select value={status} onChange={(event) => setStatus(event.target.value)}><option value="">{t("policyManagement.all")}</option><option value="enabled">{t("common.enabled")}</option><option value="disabled">{t("common.disabled")}</option></select></label>
          <label><span>{t("policies.action")}</span><select value={action} onChange={(event) => setAction(event.target.value)}><option value="">{t("policyManagement.all")}</option><option value="observe">{t("policies.observe")}</option><option value="alert">{t("policies.alert")}</option><option value="temporary_block">{t("policies.temporaryBlock")}</option></select></label>
          <button className="button primary" onClick={() => setEditing("new")}>{t("policies.create")}</button>
        </div>
        {policies.isPending && <Loading />}
        {policies.isError && <ErrorState error={policies.error} />}
        {!policies.isPending && tokenItems.length === 0 && <EmptyState title={t("policies.emptyTitle")} action={<button className="button primary" onClick={() => setEditing("new")}>{t("policies.baseline")}</button>}>{t("policies.emptyDescription")}</EmptyState>}
        {!!tokenItems.length && <div className="table-shell policy-table-shell"><table className="policy-table"><thead><tr><th>{t("policyManagement.policy")}</th><th>{t("policyManagement.status")}</th><th>{t("policies.action")}</th><th>{t("policyManagement.summary")}</th><th>{t("policyManagement.bindings")}</th><th>{t("policyManagement.actions")}</th></tr></thead><tbody>{visible.map((policy) => <tr key={policy.id}>
          <td><strong>{policy.name}</strong><code>{policy.id}</code></td>
          <td><span className="inline-status"><StatusDot ok={policy.enabled} />{policy.enabled ? t("common.enabled") : t("common.disabled")}</span></td>
          <td><span className={`badge ${policy.action === "temporary_block" ? "warning" : ""}`}>{policyActionLabel(t, policy.action)}</span></td>
          <td><strong>{compactNumber(policy.request_tokens)} / {compactNumber(policy.tokens_per_minute)} TPM</strong><small>{money(policy.cost_micros_per_minute)} · {policy.concurrency} {t("policies.concurrency")} · {policy.ewma_enabled ? `${policy.ewma_multiplier}× EWMA` : t("policies.ewmaOff")}</small></td>
          <td>{t("policyManagement.projectCount", { count: policy.bound_projects ?? 0 })}</td>
          <td><div className="row-actions policy-row-actions"><button className="button ghost" onClick={() => setPreviewing(policy)}>{t("policies.simulate")}</button><button className="button ghost" onClick={() => setEditing(policy)}>{t("common.edit")}</button><ConfirmButton label={t("common.delete")} confirmLabel={t("policies.deleteConfirm", { name: policy.name })} disabled={remove.isPending} onConfirm={() => remove.mutate(policy)} /></div></td>
        </tr>)}</tbody></table>{remove.isError && <ErrorState error={remove.error} />}</div>}
        {policies.hasNextPage && <button className="button ghost policy-load-more" disabled={policies.isFetchingNextPage} onClick={() => policies.fetchNextPage()}>{policies.isFetchingNextPage ? t("common.loading") : t("common.loadMore")}</button>}
      </section>}
      {tab === "redaction" && <RedactionPoliciesSection policies={redactionItems} isPending={redactionPolicies.isPending} error={redactionPolicies.isError ? redactionPolicies.error : undefined} hasNextPage={redactionPolicies.hasNextPage} isFetchingNextPage={redactionPolicies.isFetchingNextPage} onLoadMore={() => redactionPolicies.fetchNextPage()} />}
      </div>
      {editing && <PolicyForm current={editing === "new" ? undefined : editing} onClose={() => setEditing(null)} />}
      {previewing && <PolicyPreview policy={previewing} onClose={() => setPreviewing(null)} />}
    </>
  );
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
  const [ewmaEnabled, setEWMAEnabled] = useState(current?.ewma_enabled ?? false);
  const [ewmaAlpha, setEWMAAlpha] = useState(current?.ewma_alpha ?? 0.2);
  const [ewmaMultiplier, setEWMAMultiplier] = useState(current?.ewma_multiplier ?? 3);
  const [ewmaMinimumSamples, setEWMAMinimumSamples] = useState(current?.ewma_minimum_samples ?? 100);
  const [ewmaWarmup, setEWMAWarmup] = useState(current?.ewma_warmup_seconds ?? 3600);
  const [ewmaWindow, setEWMAWindow] = useState(current?.ewma_evaluation_window_seconds ?? 60);
  const [ewmaCooldown, setEWMACooldown] = useState(current?.ewma_cooldown_seconds ?? 300);
  const [ewmaRPMFloor, setEWMARPMFloor] = useState(current?.ewma_absolute_rpm ?? 60);
  const [ewmaTPMFloor, setEWMATPMFloor] = useState(current?.ewma_absolute_tpm ?? 50_000);
  const [ewmaTokensFloor, setEWMATokensFloor] = useState(current?.ewma_absolute_tokens_per_request ?? 4_000);
  const [ewmaCostFloor, setEWMACostFloor] = useState((current?.ewma_absolute_cost_micros_per_minute ?? 1_000_000) / 1_000_000);
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
  const mutation = useMutation({
    mutationFn: () => current
      ? api.updateTokenGuardPolicy(current.id, body, current.revision)
      : api.createTokenGuardPolicy(body),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["token-guard-policies"] });
      onClose();
    },
  });
  return (
    <Modal wide title={current ? t("policies.edit") : t("policies.createTitle")} onClose={onClose} closeDisabled={mutation.isPending}>
      <form className="policy-form" aria-busy={mutation.isPending} onSubmit={(event) => {
        event.preventDefault();
        const nextErrors = validatePolicy({
          name, requestTokens, tokensPerMinute, costPerMinute, concurrency, errorRate,
          minimumSamples, uniqueIPs, action, violations, blockTTL, cooldown, ewmaEnabled,
          ewmaAlpha, ewmaMultiplier, ewmaMinimumSamples, ewmaWarmup, ewmaWindow,
          ewmaCooldown, ewmaRPMFloor, ewmaTPMFloor, ewmaTokensFloor, ewmaCostFloor,
        }, t);
        setErrors(nextErrors);
        if (!Object.keys(nextErrors).length) mutation.mutate();
      }}>
        {!!Object.keys(errors).length && <div className="policy-error-summary" role="alert">{t("policies.errorSummary", { count: Object.keys(errors).length })}</div>}
        <section className="policy-form-section">
          <header><div><h3>{t("policies.identitySection")}</h3><p>{t("policies.identityDescription")}</p></div><span className={`badge ${enabled ? "" : "warning"}`}>{enabled ? t("common.enabled") : t("common.disabled")}</span></header>
          <div className="policy-section-grid">
            <Field label={t("policies.name")} error={errors.name}><input autoFocus data-modal-initial value={name} onChange={(event) => setName(event.target.value)} /></Field>
            <div className="policy-status-field"><span>{t("policies.statusField")}</span><label className="policy-status-control"><input type="checkbox" aria-label={t("policies.enable")} checked={enabled} onChange={(event) => setEnabled(event.target.checked)} /><span><strong>{t("policies.enable")}</strong><small>{current?.bound_projects ? t("policies.boundImpact", { count: current.bound_projects }) : t("policies.unboundImpact")}</small></span></label></div>
          </div>
        </section>
        <fieldset className="policy-form-section policy-action-section">
          <legend>{t("policies.actionSection")}</legend>
          <p>{t("policies.actionDescription")}</p>
          <div className="policy-action-cards">
            {(["observe", "alert", "temporary_block"] as const).map((value) => <label className={`policy-action-card ${action === value ? "selected" : ""}`} key={value}><input type="radio" name="policy-action" value={value} checked={action === value} onChange={() => setAction(value)} /><span><strong>{policyActionLabel(t, value)}</strong><small>{t(`policies.${value}Description`)}</small></span></label>)}
          </div>
        </fieldset>
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
          <header><div><h3>{t("policies.ewmaSection")}</h3><p>{t("policies.ewmaSectionDescription")}</p></div><label className="policy-switch"><input type="checkbox" aria-label={t("policies.enableEWMA")} checked={ewmaEnabled} onChange={(event) => setEWMAEnabled(event.target.checked)} /><span>{ewmaEnabled ? t("common.enabled") : t("common.disabled")}</span></label></header>
          <div className="notice"> <strong>{t("policies.detectOnlyBadge")}</strong>{t("policies.ewmaHint")}</div>
        {ewmaEnabled && (
          <div className="policy-ewma-fields">
            <h4>{t("policies.baselineModel")}</h4><div className="policy-section-grid"><Field label={t("policies.ewmaAlpha")} error={errors.ewmaAlpha}><NumberInput value={ewmaAlpha} set={setEWMAAlpha} step=".05" /></Field><Field label={t("policies.multiplier")} error={errors.ewmaMultiplier}><NumberInput value={ewmaMultiplier} set={setEWMAMultiplier} step=".1" /></Field><Field label={t("policies.baselineSamples")} error={errors.ewmaMinimumSamples}><NumberInput value={ewmaMinimumSamples} set={setEWMAMinimumSamples} /></Field></div>
            <h4>{t("policies.timeControls")}</h4><div className="policy-section-grid"><Field label={t("policies.warmup")} error={errors.ewmaWarmup}><NumberInput value={ewmaWarmup} set={setEWMAWarmup} /></Field><Field label={t("policies.window")} error={errors.ewmaWindow}><NumberInput value={ewmaWindow} set={setEWMAWindow} /></Field><Field label={t("policies.alertCooldown")} error={errors.ewmaCooldown}><NumberInput value={ewmaCooldown} set={setEWMACooldown} /></Field></div>
            <h4>{t("policies.absoluteFloors")}</h4><div className="policy-section-grid"><Field label={t("policies.rpmFloor")} error={errors.ewmaFloors}><NumberInput value={ewmaRPMFloor} set={setEWMARPMFloor} /></Field><Field label={t("policies.tpmFloor")} error={errors.ewmaFloors}><NumberInput value={ewmaTPMFloor} set={setEWMATPMFloor} /></Field><Field label={t("policies.tokenFloor")} error={errors.ewmaFloors}><NumberInput value={ewmaTokensFloor} set={setEWMATokensFloor} step=".1" /></Field><Field label={t("policies.costFloor")} error={errors.ewmaFloors}><NumberInput value={ewmaCostFloor} set={setEWMACostFloor} step=".01" /></Field></div>
          </div>
        )}
        </section>
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions policy-form-actions">
          <div className="policy-save-summary"><strong>{enabled ? t("common.enabled") : t("common.disabled")} · {policyActionLabel(t, action)}</strong><small>{ewmaEnabled ? t("policies.ewmaSummary", { multiplier: ewmaMultiplier, window: ewmaWindow }) : t("policies.ewmaDisabledSummary")}</small></div>
          <button type="button" className="button ghost" disabled={mutation.isPending} onClick={onClose}>{t("common.cancel")}</button>
          <button className="button primary" disabled={mutation.isPending}>{mutation.isPending ? t("common.saving") : enabled && current ? t("policies.saveAndApply") : t("policies.save")}</button>
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
  return (
    <Modal title={t("policies.previewTitle", { name: policy.name })} onClose={onClose} closeDisabled={mutation.isPending}>
      <form onSubmit={(event) => { event.preventDefault(); mutation.mutate(); }}>
        <Field label={t("policies.estimatedTokens")}><NumberInput value={tokens} set={setTokens} /></Field>
        <Field label={t("policies.estimatedCost")}><NumberInput value={estimatedCostUSD} set={setEstimatedCostUSD} step=".01" /></Field>
        <Field label={t("policies.windowTokens")}><NumberInput value={windowTokens} set={setWindowTokens} /></Field>
        <Field label={t("policies.currentConcurrency")}><NumberInput value={concurrency} set={setConcurrency} /></Field>
        <Field label={t("policies.windowCost")}><NumberInput value={costUSD} set={setCostUSD} step=".01" /></Field>
        <Field label={t("policies.windowRequests")}><NumberInput value={requests} set={setRequests} /></Field>
        <Field label={t("policies.windowErrors")}><NumberInput value={errors} set={setErrors} /></Field>
        <Field label={t("policies.windowUniqueIPs")}><NumberInput value={uniqueIPs} set={setUniqueIPs} /></Field>
        <label className="check-row"><input type="checkbox" checked={hasNewSource} onChange={(event) => setHasNewSource(event.target.checked)} /><span>{t("policies.newSource")}</span></label>
        {result && (
          <div className={`preview-result ${result.violated ? "violated" : ""}`}>
            <StatusDot ok={!result.violated} />
            <div>
              <strong>{result.violated ? t("policies.hit", { reason: result.reason }) : t("policies.noHit")}</strong>
              <small>{t("policies.resultAction", { action: policyActionLabel(t, result.action) })}</small>
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
