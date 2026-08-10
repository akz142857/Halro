import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState, type FormEvent, type KeyboardEvent } from "react";
import { ApiError, api } from "../api";
import {
  ConfirmButton,
  EmptyState,
  ErrorState,
  Field,
  InlineTestControl,
  Loading,
  Modal,
  OverflowMenu,
  PageHeader,
  StatusDot,
  useDirty,
  type ReauthValues,
} from "../components";
import { money, useInstantFormatter } from "../format";
import type { CapabilityPreflight, CapabilityReview, Deployment, DeploymentPriceVersion, DeploymentTargetKind, DeploymentVariant, ModelCapabilityDetection, Provider, ProviderBinding, ProviderCapabilities, ResolvedInvocationTarget } from "../types";
import { useTranslation } from "react-i18next";
import { useIsReadOnly } from "../session";
import { Link } from "../navigation";
import { hasOnboardingCreateIntent, OnboardingContextBanner } from "../OnboardingContext";

export function DeploymentsPage() {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const [editing, setEditing] = useState<Deployment | null | "new">(() => !readOnly && hasOnboardingCreateIntent() ? "new" : null);
  const [replacement, setReplacement] = useState<Deployment>();
  const [query, setQuery] = useState(() => new URLSearchParams(window.location.search).get("q") ?? "");
  const [status, setStatus] = useState<"all" | "enabled" | "disabled" | "attention">("all");
  const deployments = useQuery({ queryKey: ["deployments"], queryFn: api.deployments });
  const providers = useQuery({ queryKey: ["providers"], queryFn: api.providers });
  const routes = useQuery({ queryKey: ["routes"], queryFn: api.routes });
  const providerNames = useMemo(
    () => new Map(providers.data?.items.map((provider) => [provider.id, provider.name]) ?? []),
    [providers.data],
  );
  const activeRouteCounts = useMemo(() => {
    const counts = new Map<string, number>();
    for (const route of routes.data?.items ?? []) {
      if (route.enabled && route.deployment_id) counts.set(route.deployment_id, (counts.get(route.deployment_id) ?? 0) + 1);
    }
    return counts;
  }, [routes.data]);
  const filteredDeployments = useMemo(() => {
    const normalizedQuery = query.trim().toLocaleLowerCase();
    return (deployments.data?.items ?? []).filter((deployment) => {
      const matchesQuery = !normalizedQuery || [
        deployment.id,
        deployment.name,
        deployment.provider_model,
        providerNames.get(deployment.provider_id) || deployment.provider_id,
        deployment.region,
      ].some((value) => value?.toLocaleLowerCase().includes(normalizedQuery));
      const testIsCurrent = deployment.last_test_status === "healthy" && deployment.last_test_revision === deployment.revision;
      const matchesStatus = status === "all"
        || (status === "enabled" && deployment.enabled)
        || (status === "disabled" && !deployment.enabled)
        || (status === "attention" && (!testIsCurrent || deployment.last_test_status === "unhealthy"));
      return matchesQuery && matchesStatus;
    });
  }, [deployments.data?.items, providerNames, query, status]);
  return (
    <>
      <PageHeader
        eyebrow={t("deployments.eyebrow")}
        title={t("deployments.title")}
        description={t("deployments.description")}
        action={<button className="button primary" disabled={readOnly} title={readOnly ? t("navigation.readOnlyAction") : undefined} onClick={() => { setReplacement(undefined); setEditing("new"); }}>{t("deployments.create")}</button>}
      />
      <OnboardingContextBanner />
      {(deployments.isPending || providers.isPending || routes.isPending) && <Loading />}
      {(deployments.isError || providers.isError || routes.isError) && <ErrorState error={deployments.error || providers.error || routes.error} />}
      {deployments.data?.items.length === 0 && (
        <EmptyState title={t("deployments.emptyTitle")}>{t("deployments.emptyDescription")}</EmptyState>
      )}
      {!!deployments.data?.items.length && (
        <section className="deployment-list" aria-label={t("deployments.list")}>
          <div className="deployment-toolbar" role="search" aria-label={t("deployments.filters")}>
            <label className="deployment-search">
              <span>{t("deployments.search")}</span>
              <input value={query} onChange={(event) => setQuery(event.target.value)} placeholder={t("deployments.searchPlaceholder")} />
            </label>
            <label>
              <span>{t("deployments.statusFilter")}</span>
              <select value={status} onChange={(event) => setStatus(event.target.value as typeof status)}>
                <option value="all">{t("deployments.filterAll")}</option>
                <option value="enabled">{t("common.enabled")}</option>
                <option value="disabled">{t("common.disabled")}</option>
                <option value="attention">{t("deployments.needsAttention")}</option>
              </select>
            </label>
            <span className="deployment-result-count" role="status">{t("deployments.resultCount", { visible: filteredDeployments.length, total: deployments.data.items.length })}</span>
          </div>
          {filteredDeployments.length ? (
            filteredDeployments.map((deployment) => (
              <DeploymentRow
                key={deployment.id}
                deployment={deployment}
                providerName={providerNames.get(deployment.provider_id) || deployment.provider_id}
                activeRouteCount={activeRouteCounts.get(deployment.id) ?? 0}
                onEdit={() => setEditing(deployment)}
                onReplace={() => { setReplacement(deployment); setEditing("new"); }}
              />
            ))
          ) : <EmptyState title={t("deployments.noMatches")}>{t("deployments.noMatchesDescription")}</EmptyState>}
        </section>
      )}
      {editing && providers.isSuccess && (
        <DeploymentForm
          current={editing === "new" ? undefined : editing}
          template={editing === "new" ? replacement : undefined}
          providers={providers.data?.items ?? []}
          onClose={() => { setEditing(null); setReplacement(undefined); }}
        />
      )}
    </>
  );
}

function DeploymentRow({
  deployment,
  providerName,
  activeRouteCount,
  onEdit,
  onReplace,
}: {
  deployment: Deployment;
  providerName: string;
  activeRouteCount: number;
  onEdit: () => void;
  onReplace: () => void;
}) {
  const { t } = useTranslation();
  const dateTime = useInstantFormatter();
  const readOnly = useIsReadOnly();
  const [expanded, setExpanded] = useState(false);
  const [pricing, setPricing] = useState(false);
	const [confirmingRestore, setConfirmingRestore] = useState(false);
  const queryClient = useQueryClient();
  const prices = useQuery({ queryKey: ["deployment-prices", deployment.id], queryFn: () => api.deploymentPrices(deployment.id), enabled: expanded });
  const priceItems = prices.data?.items ?? [];
  const activePrice = priceItems.find((price) => price.status === "active");
  const scheduledPrices = priceItems.filter((price) => price.status === "scheduled");
  const cancelPrice = useMutation({ mutationFn: (price: DeploymentPriceVersion) => api.cancelDeploymentPrice(deployment.id, price.id, price.revision), onSuccess: () => queryClient.invalidateQueries({ queryKey: ["deployment-prices", deployment.id] }) });
  const test = useMutation({
    mutationFn: () => api.testDeployment(deployment.id),
    onSettled: () => queryClient.invalidateQueries({ queryKey: ["deployments"] }),
  });
  const remove = useMutation({
    mutationFn: (reauth: ReauthValues) => api.deleteDeployment(deployment.id, deployment.revision, reauth),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["deployments"] }),
  });
  const state = useMutation({
    mutationFn: () => api.updateDeployment(deployment.id, {
      name: deployment.name,
      provider_id: deployment.provider_id,
      ...(deployment.binding_id ? { binding_id: deployment.binding_id } : {}),
      provider_model: deployment.provider_model,
      ...(deployment.target_kind ? { target_kind: deployment.target_kind } : {}),
      capabilities: deployment.capabilities,
      region: deployment.region,
      max_concurrency: deployment.max_concurrency,
      priority: deployment.priority,
      weight: deployment.weight,
      enabled: !deployment.enabled,
    }, deployment.revision),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["deployments"] }),
  });
  const capabilities = Object.entries(deployment.capabilities)
    .filter(([, enabled]) => typeof enabled === "boolean" && enabled)
    .map(([name]) => name);
  const testIsCurrent = deployment.last_test_status === "healthy" && deployment.last_test_revision === deployment.revision;
  const testFailed = deployment.last_test_status === "unhealthy" && deployment.last_test_revision === deployment.revision;
  const testState = test.isPending
    ? "running"
    : testFailed
      ? "failure"
      : testIsCurrent
        ? "success"
        : deployment.last_test_status === "healthy"
        ? "stale"
        : "idle";
  const evidence = evidenceSummary(deployment.capability_evidence).map((value) => t(`deployments.evidenceValues.${value}`));
  const review = deployment.capability_review;
  const routeBlocked = activeRouteCount > 0;
  return (
    <article className="deployment-row">
      <div className="deployment-row-main">
        <div className="deployment-compact-identity">
          <span><StatusDot ok={deployment.enabled} /><strong>{deployment.name}</strong></span>
          <small>{providerName}</small>
        </div>
        <div className="deployment-compact-fact deployment-fact-target">
          <small>{t("deployments.upstreamTarget")}</small>
          <code>{deployment.provider_model}</code>
        </div>
        <div className="deployment-compact-fact deployment-fact-concurrency">
          <small>{t("deployments.concurrency")}</small>
          <strong>{deployment.max_concurrency || t("deployments.unlimited")}</strong>
        </div>
        <div className="deployment-compact-fact deployment-fact-routes">
          <small>{t("deployments.routeDependency")}</small>
          {activeRouteCount
            ? <Link className="resource-link" href="/admin/routes">{t("deployments.activeRoutesCompact", { count: activeRouteCount })} →</Link>
            : <strong>{t("deployments.noActiveRoutes")}</strong>}
        </div>
        <div className="deployment-compact-status">
          <small>{t("deployments.status")}</small>
          <span className={`resource-state ${deployment.enabled ? "enabled" : ""}`}>{deployment.enabled ? t("common.enabled") : t("common.disabled")}</span>
          {/* A drifted deployment is not routing whatever the enabled flag says,
              so the state that decides that has to be visible in the row. */}
          {review && review.state !== "current" && (
            <small className="capability-review-state" data-state={review.state}>
              {review.state === "drifted" ? t("deployments.capabilitiesUnsupported") : t("deployments.capabilitiesToReview")}
            </small>
          )}
        </div>
        <div className="row-actions deployment-compact-actions">
          {/* Test, enable and create-replacement are writes. ConfirmButton
              gates itself; these three did not, so a read-only session was
              shown three controls that would 403 on click. §7.3 also requires a
              disabled control to say why, so each carries the reason. */}
          <InlineTestControl state={testState} latency={deployment.last_test_latency_millis} onTest={() => test.mutate()} disabled={readOnly} title={readOnly ? t("navigation.readOnlyAction") : undefined} />
          <button className="button ghost" disabled={readOnly} title={readOnly ? t("navigation.readOnlyAction") : undefined} onClick={onEdit}>{t("common.edit")}</button>
          <button className="button ghost deployment-expand" aria-expanded={expanded} aria-controls={`deployment-details-${deployment.id}`} onClick={() => setExpanded((value) => !value)}>
            <span>{expanded ? t("deployments.collapseDetails") : t("deployments.expandDetails")}</span>
            {/* Reserves the width of the other label so toggling never resizes the row. */}
            <span aria-hidden="true">{expanded ? t("deployments.expandDetails") : t("deployments.collapseDetails")}</span>
          </button>
          {/* The sizer keeps enable and disable rows equally wide so columns line up across the list. */}
          <span className="deployment-state-toggle">
            {deployment.enabled ? (
              <ConfirmButton className="button ghost" label={t("common.disable")} title={t("deployments.disableTitle")} confirmLabel={t("deployments.disableConfirm", { name: deployment.name })} disabled={state.isPending || routeBlocked} disabledReason={routeBlocked ? t("deployments.routeBlocked") : undefined} onConfirm={() => state.mutateAsync()} />
            ) : (
              <button className="button ghost" title={readOnly ? t("navigation.readOnlyAction") : !testIsCurrent ? t("deployments.testRequired") : undefined} disabled={readOnly || state.isPending || !testIsCurrent} onClick={() => state.mutate()}>{t("common.enable")}</button>
            )}
            <span className="button ghost deployment-state-sizer" aria-hidden="true">{deployment.enabled ? t("common.enable") : t("common.disable")}</span>
          </span>
          <OverflowMenu label={t("deployments.moreActions")}>
            <button className="button ghost" disabled={readOnly} title={readOnly ? t("navigation.readOnlyAction") : undefined} onClick={onReplace}>{t("deployments.createReplacement")}</button>
            <ConfirmButton label={t("common.delete")} confirmLabel={t("deployments.deleteConfirm", { name: deployment.name })} requireStepUp onConfirm={(reauth) => remove.mutateAsync(reauth)} disabled={remove.isPending || routeBlocked} disabledReason={routeBlocked ? t("deployments.routeBlocked") : undefined} />
          </OverflowMenu>
        </div>
      </div>
      {expanded && <div id={`deployment-details-${deployment.id}`} className="deployment-details">
        <dl className="deployment-facts">
          <DeploymentFact label={t("deployments.priceStatus")} value={activePrice ? activePrice.billing_mode === "free" ? t("deployments.freePrice") : t("deployments.versionedPrice") : prices.isPending ? t("common.loading") : t("deployments.unknownPrice")} meta={activePrice ? `v${activePrice.version} · ${dateTime(activePrice.effective_from)}` : t("deployments.priceRequired")} unset={!activePrice} />
          {(deployment.capabilities.chat || deployment.capabilities.embeddings) && <DeploymentFact label={t("deployments.inputPrice")} value={activePrice ? money(activePrice.input_micros_per_million) : t("deployments.notConfigured")} meta={t("deployments.perMillionTokens")} unset={!activePrice} />}
          {(deployment.capabilities.chat || deployment.capabilities.embeddings) && <DeploymentFact label={t("deployments.outputPrice")} value={activePrice ? money(activePrice.output_micros_per_million) : t("deployments.notConfigured")} meta={t("deployments.perMillionTokens")} unset={!activePrice} />}
          {activePrice && <DeploymentFact label={t("deployments.fixedPrice")} value={money(activePrice.fixed_request_micros_usd)} meta={t("deployments.perRequest")} />}
          <DeploymentFact label={t("deployments.concurrency")} value={deployment.max_concurrency || t("deployments.unlimited")} meta={t("deployments.deploymentScope")} />
          {deployment.region && <DeploymentFact label={t("deployments.region")} value={deployment.region} meta={t("deployments.deploymentScope")} />}
          <DeploymentFact label={t("deployments.context")} value={deployment.capabilities.max_context_tokens || t("deployments.upstreamApplies")} meta={deployment.capabilities.max_context_tokens ? t("deployments.tokens") : t("deployments.undeclared")} />
          <DeploymentFact label={t("deployments.maxOutput")} value={deployment.capabilities.max_output_tokens || t("deployments.upstreamApplies")} meta={deployment.capabilities.max_output_tokens ? t("deployments.tokens") : t("deployments.undeclared")} />
        </dl>
        <CapabilityReviewNotice review={review} />
        {deployment.pricing_quarantined && <div className="notice warning deployment-pricing-warning"><strong>{t("deployments.pricingQuarantined")}</strong><span>{deployment.pricing_quarantine_reason}</span><button className="button ghost" onClick={() => setConfirmingRestore(true)}>{t("deployments.confirmRestoredPricing")}</button></div>}
        <div className="deployment-pricing-grid single">
          <section className="deployment-pricing-panel">
            <header>
              <div>
                <strong>{t("deployments.priceTimeline")}</strong>
                <small>{activePrice
                  ? t("deployments.priceSourceSummary", { type: activePrice.source.type, assurance: activePrice.source.assurance, reference: activePrice.source.reference || "—" })
                  : prices.isPending ? t("common.loading") : t("deployments.noPriceVersions")}</small>
              </div>
              <button className="button secondary deployment-pricing-action" onClick={() => setPricing(true)}>{activePrice ? t("deployments.adjustPrice") : t("deployments.setPrice")}</button>
            </header>
            {!!scheduledPrices.length && <div className="deployment-pricing-list">
              {scheduledPrices.map((price) => <div key={price.id}>
                <span><code>v{price.version}</code><small>{dateTime(price.effective_from)} · {price.billing_mode}</small></span>
                <button className="button ghost" disabled={cancelPrice.isPending} onClick={() => cancelPrice.mutate(price)}>{t("common.cancel")}</button>
              </div>)}
            </div>}
          </section>
        </div>
        <div className="deployment-capability-summary">
          <strong>{t("deployments.capabilityCount", { count: capabilities.length })}</strong>
          <div className="capability-list" aria-label={t("deployments.capabilities")}>
            {capabilities.slice(0, 5).map((capability) => <span className="badge" key={capability}>{t(`capabilities.${capability}`)}</span>)}
            {capabilities.length > 5 && <span className="badge">+{capabilities.length - 5}</span>}
          </div>
        </div>
        <details className="technical-details deployment-technical-details">
          <summary>{t("deployments.technicalDetails")}</summary>
          <dl>
            <div><dt>{t("deployments.accessSurface")}</dt><dd><code>{deployment.access_surface}</code></dd></div>
            <div><dt>{t("deployments.profile")}</dt><dd><code>{deployment.profile_id}</code></dd></div>
            <div><dt>{t("deployments.deploymentID")}</dt><dd><code>{deployment.id}</code></dd></div>
            {deployment.binding_id && <div><dt>{t("deployments.bindingID")}</dt><dd><code>{deployment.binding_id}</code></dd></div>}
            <div><dt>{t("deployments.evidence")}</dt><dd>{evidence.length ? evidence.join(" / ") : "—"}</dd></div>
          </dl>
        </details>
      </div>}
      {(remove.isError || state.isError) && <ErrorState className="deployment-card-error" error={remove.error || state.error} />}
      {pricing && <PriceVersionForm deployment={deployment} current={activePrice} onClose={() => setPricing(false)} />}
	  {confirmingRestore && <RestorePricingConfirm deployment={deployment} onClose={() => setConfirmingRestore(false)} />}
    </article>
  );
}

// The saved capability answer and what supports it now can diverge without the
// deployment being touched, so the console states which it is rather than
// leaving the operator to infer it from a deployment that stopped serving.
function CapabilityReviewNotice({ review }: { review: CapabilityReview | undefined }) {
  const { t } = useTranslation();
  if (!review || review.state === "current") return null;
  const drifted = review.state === "drifted";
  const unavailable = review.state === "catalog_unavailable";
  const names = (values: string[] | undefined) =>
    (values ?? []).map((name) => t(`capabilities.${name}`)).join(t("common.listSeparator"));
  return (
    <div className={`notice ${drifted ? "warning" : ""} deployment-capability-review`}>
      <strong>{drifted ? t("deployments.capabilitiesUnsupported") : unavailable ? t("deployments.catalogUnavailableTitle") : t("deployments.capabilitiesToReview")}</strong>
      <span>{t(`deployments.reviewReasons.${review.reason ?? "catalog_revision_advanced"}`)}</span>
      <dl className="deployment-review-facts">
        <div>
          <dt>{t("deployments.capabilitySource")}</dt>
          <dd>{t(`deployments.capabilitySources.${review.source}`, { defaultValue: review.source })}</dd>
        </div>
        <div>
          <dt>{t("deployments.noLongerSupported")}</dt>
          <dd>{names(review.no_longer_supported) || "—"}</dd>
        </div>
        <div>
          <dt>{t("deployments.availableForReview")}</dt>
          <dd>{names(review.available_for_review) || "—"}</dd>
        </div>
        <div>
          <dt>{t("deployments.switchedOff")}</dt>
          <dd>{names(review.operator_disabled) || "—"}</dd>
        </div>
      </dl>
      <span className="deployment-review-consequence">
        {drifted ? t("deployments.driftedConsequence") : unavailable ? t("deployments.catalogUnavailableConsequence") : t("deployments.reviewAvailableConsequence")}
      </span>
    </div>
  );
}

// What the narrowing would do to live routing, listed per route so the operator
// confirms against the actual consequence rather than a warning about one.
function CapabilityImpactNotice({ impact }: { impact: CapabilityPreflight }) {
  const { t } = useTranslation();
  const removed = impact.removed_capabilities.map((name) => t(`capabilities.${name}`)).join(t("common.listSeparator"));
  return (
    <div className={`notice ${impact.blocking ? "warning" : ""} deployment-capability-impact`}>
      <strong>{impact.blocking ? t("deployments.routesLoseTheirOnlyCandidate") : t("deployments.routesAffected")}</strong>
      <span>{t("deployments.impactSummary", { capabilities: removed, count: impact.affected_routes.length })}</span>
      <ul className="deployment-impact-list">
        {impact.affected_routes.map((route) => (
          <li key={`${route.route_id}-${route.capability}`} data-sole={route.sole_candidate}>
            <code>{route.public_model}</code>
            <span>{t(`capabilities.${route.capability}`)}</span>
            <small>{route.sole_candidate ? t("deployments.soleCandidate") : t("deployments.otherCandidateRemains")}</small>
          </li>
        ))}
      </ul>
    </div>
  );
}

function RestorePricingConfirm({ deployment, onClose }: { deployment: Deployment; onClose: () => void }) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const [password, setPassword] = useState("");
	const [totp, setTotp] = useState("");
	const mutation = useMutation({ mutationFn: () => api.confirmRestoredDeploymentPricing(deployment.id, { current_password: password, totp_code: totp }), onSuccess: () => { queryClient.invalidateQueries({ queryKey: ["deployments"] }); queryClient.invalidateQueries({ queryKey: ["deployment-prices", deployment.id] }); onClose(); } });
	return <Modal title={t("deployments.confirmRestoredPricing")} onClose={onClose}><form onSubmit={(event) => { event.preventDefault(); mutation.mutate(); }}>
		<p>{t("deployments.restorePricingWarning")}</p>
		<Field label={t("usage.currentPassword")}><input type="password" required value={password} onChange={(event) => setPassword(event.target.value)} /></Field>
		<Field label={t("usage.totpOptional")}><input inputMode="numeric" value={totp} onChange={(event) => setTotp(event.target.value)} /></Field>
		{mutation.isError && <ErrorState error={mutation.error} />}<button className="button primary" disabled={mutation.isPending}>{t("deployments.confirmRestoredPricing")}</button>
	</form></Modal>;
}

function DeploymentFact({ label, value, meta, unset = false }: { label: string; value: string | number; meta: string; unset?: boolean }) {
  return (
    <div>
      <dt>{label}</dt>
      <dd className={unset ? "unset" : undefined}><span>{value}</span><small>{meta}</small></dd>
    </div>
  );
}

function PriceVersionForm({ deployment, current, onClose }: { deployment: Deployment; current?: DeploymentPriceVersion; onClose: () => void }) {
  const { t } = useTranslation();
  const dateTime = useInstantFormatter();
  const queryClient = useQueryClient();
  const [step, setStep] = useState<"details" | "confirm">("details");
  const [mode, setMode] = useState<"metered" | "free">(current?.billing_mode ?? "metered");
  const [input, setInput] = useState(current ? priceInputValue(current.input_micros_per_million) : "0");
  const [output, setOutput] = useState(current ? priceInputValue(current.output_micros_per_million) : "0");
  const [fixed, setFixed] = useState(current ? priceInputValue(current.fixed_request_micros_usd) : "0");
  const [effectiveMode, setEffectiveMode] = useState<"now" | "scheduled">("now");
  const [effective, setEffective] = useState(localDateTimeValue(new Date(Date.now() + 3_600_000)));
  const [confirmedEffective, setConfirmedEffective] = useState("");
  const [sourceKind, setSourceKind] = useState("temporary_estimate");
  const [sourceNote, setSourceNote] = useState("");
  const [password, setPassword] = useState("");
  const [totp, setTotp] = useState("");
  const idempotencyKey = useRef(crypto.randomUUID());
  const confirmSummary = useRef<HTMLElement>(null);
  const validPrice = mode === "free" || ([input, output, fixed].every(validUSD) && [input, output, fixed].some((value) => Number(value) > 0));
  const scheduledTimestamp = Date.parse(effective);
  const validEffective = effectiveMode === "now" || (Number.isFinite(scheduledTimestamp) && scheduledTimestamp > Date.now());
  const sourceNeedsNote = sourceKind === "official_public_price" || sourceKind === "contract_price";
  const validSource = !sourceNeedsNote || sourceNote.trim() !== "";
  const validDetails = validPrice && validEffective && validSource;
  const exampleCost = mode === "free" ? 0 : Number(input) / 1000 + Number(output) / 2000 + Number(fixed);
  const effectiveLabel = effectiveMode === "now" ? t("deployments.effectiveNow") : validEffective ? dateTime(new Date(scheduledTimestamp).toISOString()) : "—";
  const mutation = useMutation({
    mutationFn: () => api.createDeploymentPrice(deployment.id, {
      billing_mode: mode, currency: "USD",
      input_usd_per_million: mode === "free" ? "0" : input,
      output_usd_per_million: mode === "free" ? "0" : output,
      fixed_request_usd: mode === "free" ? "0" : fixed,
      effective_from: confirmedEffective,
      source: { type: "manual", reference: sourceKind, note: sourceNote.trim(), asserted_without_archive: true },
      current_password: password, totp_code: totp,
    }, idempotencyKey.current),
    onSuccess: () => { queryClient.invalidateQueries({ queryKey: ["deployment-prices", deployment.id] }); onClose(); },
  });
  const ambiguousResult = mutation.isError && (!(mutation.error instanceof ApiError) || mutation.error.status >= 500);
  useEffect(() => {
    if (step === "confirm") requestAnimationFrame(() => confirmSummary.current?.focus());
  }, [step]);
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (step === "details") {
      if (validDetails) {
        setConfirmedEffective(effectiveMode === "now" ? new Date(Date.now() + 60_000).toISOString() : new Date(scheduledTimestamp).toISOString());
        setStep("confirm");
      }
      return;
    }
    mutation.mutate();
  };
  return <Modal title={current ? t("deployments.adjustPrice") : t("deployments.setPrice")} onClose={onClose}>
    <form className="price-version-form" onSubmit={submit}>
      <ol className={`price-form-steps ${step === "confirm" ? "on-confirm" : ""}`} aria-label={t("deployments.priceSteps")}>
        <li className={step === "details" ? "active" : "complete"} aria-current={step === "details" ? "step" : undefined}>
          <span className="price-step-number" aria-hidden="true">1</span>
          <span className="price-step-label">{t("deployments.priceStepDetails")}</span>
        </li>
        <li className={step === "confirm" ? "active" : ""} aria-current={step === "confirm" ? "step" : undefined}>
          <span className="price-step-number" aria-hidden="true">2</span>
          <span className="price-step-label">{t("deployments.priceStepConfirm")}</span>
        </li>
      </ol>
      {step === "details" ? <>
        <fieldset className="price-mode-options">
          <legend>{t("deployments.billingMode")}</legend>
          <label className={mode === "metered" ? "price-mode-option selected" : "price-mode-option"}>
            <input type="radio" name="price-mode" checked={mode === "metered"} onChange={() => setMode("metered")} />
            <span><strong>{t("deployments.meteredLabel")}</strong><small>{t("deployments.meteredDescription")}</small></span>
          </label>
          <label className={mode === "free" ? "price-mode-option selected" : "price-mode-option"}>
            <input type="radio" name="price-mode" checked={mode === "free"} onChange={() => setMode("free")} />
            <span><strong>{t("deployments.freeLabel")}</strong><small>{t("deployments.freeDescription")}</small></span>
          </label>
        </fieldset>
        {mode === "metered" && <>
          <div className="price-form-grid">
            <Field label={t("deployments.inputUSD")}><input inputMode="decimal" required value={input} onChange={(event) => setInput(event.target.value)} /></Field>
            <Field label={t("deployments.outputUSD")}><input inputMode="decimal" required value={output} onChange={(event) => setOutput(event.target.value)} /></Field>
          </div>
          <details className="price-advanced">
            <summary>{t("deployments.advancedPricing")}</summary>
            <Field label={t("deployments.fixedRequestUSD")}><input inputMode="decimal" required value={fixed} onChange={(event) => setFixed(event.target.value)} /></Field>
          </details>
        </>}
        <div className="price-form-grid">
          <Field label={t("deployments.effectiveMode")}><select value={effectiveMode} onChange={(event) => setEffectiveMode(event.target.value as "now" | "scheduled")}><option value="now">{t("deployments.effectiveNow")}</option><option value="scheduled">{t("deployments.effectiveScheduled")}</option></select></Field>
          <Field label={t("deployments.priceSourceKind")}><select value={sourceKind} onChange={(event) => setSourceKind(event.target.value)}><option value="official_public_price">{t("deployments.sourceKinds.officialPublicPrice")}</option><option value="contract_price">{t("deployments.sourceKinds.contractPrice")}</option><option value="internal_cost">{t("deployments.sourceKinds.internalCost")}</option><option value="temporary_estimate">{t("deployments.sourceKinds.temporaryEstimate")}</option></select></Field>
        </div>
        {effectiveMode === "scheduled" && <Field label={t("deployments.effectiveFrom")}><input type="datetime-local" required value={effective} onChange={(event) => setEffective(event.target.value)} /></Field>}
        {!validEffective && <p className="field-hint error">{t("deployments.invalidEffectiveTime")}</p>}
        <Field label={t("deployments.sourceNote")}><textarea value={sourceNote} onChange={(event) => setSourceNote(event.target.value)} placeholder={t("deployments.sourceNotePlaceholder")} /></Field>
        {!validSource && <p className="field-hint error">{t("deployments.sourceEvidenceRequired")}</p>}
        {!validPrice && <p className="field-hint error">{t("deployments.invalidPrice")}</p>}
        <div className="form-actions"><button className="button primary" disabled={!validDetails}>{t("deployments.nextReview")}</button></div>
      </> : <>
        <section className="price-confirm-summary" ref={confirmSummary} tabIndex={-1} aria-live="polite">
          <header><strong>{t("deployments.priceSummary")}</strong><small>{deployment.name} · {deployment.provider_model}</small></header>
          <dl>
            <div><dt>{t("deployments.billingMode")}</dt><dd>{mode === "free" ? t("deployments.freeLabel") : t("deployments.meteredLabel")}</dd></div>
            {mode === "metered" && <><div><dt>{t("deployments.inputUSD")}</dt><dd>${input}</dd></div><div><dt>{t("deployments.outputUSD")}</dt><dd>${output}</dd></div><div><dt>{t("deployments.fixedRequestUSD")}</dt><dd>${fixed}</dd></div></>}
            <div><dt>{t("deployments.effectiveFrom")}</dt><dd>{effectiveLabel}</dd></div>
            <div><dt>{t("deployments.priceSourceKind")}</dt><dd>{t(`deployments.sourceKinds.${sourceKind === "official_public_price" ? "officialPublicPrice" : sourceKind === "contract_price" ? "contractPrice" : sourceKind === "internal_cost" ? "internalCost" : "temporaryEstimate"}`)}</dd></div>
            <div><dt>{t("deployments.sourceAssurance")}</dt><dd>{t("deployments.assertedSource")}</dd></div>
          </dl>
          <p>{t("deployments.priceExample", { cost: exampleCost.toFixed(6) })}</p>
        </section>
        <div className="notice warning"><strong>{t("deployments.priceWillTakeEffect")}</strong><span>{t("deployments.immutablePriceWarning")}</span></div>
        <Field label={t("usage.currentPassword")}><input type="password" autoComplete="current-password" required value={password} onChange={(event) => setPassword(event.target.value)} /></Field>
        <Field label={t("usage.totpOptional")}><input inputMode="numeric" autoComplete="one-time-code" value={totp} onChange={(event) => setTotp(event.target.value)} /></Field>
        {mutation.isError && <ErrorState error={mutation.error} />}
        {ambiguousResult && <p className="field-hint error">{t("deployments.ambiguousPriceRetry")}</p>}
        <div className="form-actions">{!ambiguousResult && <button type="button" className="button ghost" onClick={() => { idempotencyKey.current = crypto.randomUUID(); setStep("details"); }}>{t("deployments.backToPrice")}</button>}<button className="button primary" disabled={mutation.isPending}>{ambiguousResult ? t("deployments.retryExactPrice") : t("deployments.confirmPrice")}</button></div>
      </>}
    </form>
  </Modal>;
}

function validUSD(value: string) {
  return /^(?:0|[1-9]\d*)(?:\.\d{1,6})?$/.test(value) && Number.isFinite(Number(value));
}

function priceInputValue(micros: number) {
  return String(micros / 1_000_000);
}

export function localDateTimeValue(date: Date) {
  return new Date(date.getTime() - date.getTimezoneOffset() * 60_000).toISOString().slice(0, 16);
}

function evidenceSummary(evidence: Record<string, string>) {
  const values = [...new Set(Object.values(evidence).filter((value) => value !== "unsupported"))];
  return values;
}

type SelectableBinding = ProviderBinding & { id: string };

const deploymentCapabilityNames = ["chat", "streaming", "embeddings", "moderations", "images", "transcriptions", "speech", "files", "batches", "rerank", "async_generate", "tools", "vision", "json_mode", "developer_role", "reasoning", "stream_usage"] as const;
type DeploymentCapabilityName = typeof deploymentCapabilityNames[number];

const deploymentCapabilityGroups = [
  { id: "operations", capabilities: ["chat", "embeddings", "moderations", "images", "transcriptions", "speech", "rerank", "async_generate"] },
  { id: "modalities", capabilities: ["vision"] },
  { id: "protocol", capabilities: ["streaming", "tools", "json_mode", "developer_role", "reasoning", "stream_usage"] },
  { id: "managed", capabilities: ["files", "batches"] },
] as const;

function providerBindings(provider?: Provider): SelectableBinding[] {
  if (!provider) return [];
  const bindings = (provider.bindings ?? [])
    .filter((binding) => binding.enabled)
    .map((binding) => ({ ...binding, id: binding.id || binding.profile_id }));
  if (bindings.length) return bindings;
  return [{ id: "", profile_id: provider.profile_id, enabled: true, capabilities: provider.capabilities }];
}

function bindingLabel(binding: SelectableBinding, t: ReturnType<typeof useTranslation>["t"]) {
  const capabilities = Object.entries(binding.capabilities)
    .filter(([, enabled]) => enabled === true)
    .slice(0, 3)
    .map(([name]) => t(`capabilities.${name}`));
  return `${capabilities.join(" · ")} — ${binding.profile_id}`;
}

function DeploymentForm({
  current,
  template,
  providers,
  onClose,
}: {
  current?: Deployment;
  template?: Deployment;
  providers: Provider[];
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const dateTime = useInstantFormatter();
  const source = current ?? template;
  const enabledProviders = providers.filter((provider) => provider.enabled || provider.id === source?.provider_id);
  const [name, setName] = useState(current?.name ?? (template ? `${template.name} v2` : ""));
  const [providerID, setProviderID] = useState(source?.provider_id ?? enabledProviders[0]?.id ?? "");
  const [providerModel, setProviderModel] = useState(source?.provider_model ?? "");
  const [bindingID, setBindingID] = useState(source?.binding_id ?? "");
  // A new deployment starts with nothing enabled. Prefilling from the provider
  // ceiling was the habit that let a deployment claim capabilities its model
  // does not have; capabilities now arrive from the catalog or from a
  // deliberate declaration.
  const [capabilities, setCapabilities] = useState<ProviderCapabilities>(source?.capabilities ?? emptyCapabilities());
  const [selectedTarget, setSelectedTarget] = useState<ResolvedInvocationTarget | null>(null);
  const [selectedVariant, setSelectedVariant] = useState<DeploymentVariant | null>(null);
  const [resolutionRequiresConfirmation, setResolutionRequiresConfirmation] = useState(false);
  const [canonicalModelRef, setCanonicalModelRef] = useState("");
  const [capabilityDetection, setCapabilityDetection] = useState<ModelCapabilityDetection | null>(null);
  const [manualDeclaration, setManualDeclaration] = useState(false);
  const [selectionRevision, setSelectionRevision] = useState(() => crypto.randomUUID());
  const detectionIdempotencyKey = useRef(crypto.randomUUID());
  const appliedDetectionRevision = useRef(0);
  const [region, setRegion] = useState(source?.region ?? "");
  const [maxConcurrency, setMaxConcurrency] = useState(source?.max_concurrency ?? 0);
  const [targetKind, setTargetKind] = useState<DeploymentTargetKind>(source?.target_kind ?? "model_id");
  const queryClient = useQueryClient();
  const selectedProvider = enabledProviders.find((item) => item.id === providerID);
  const selectableBindings = providerBindings(selectedProvider);
  const pinnedBinding = selectableBindings.find((item) => item.id === bindingID);
  const detectionBindingID = pinnedBinding?.id ?? "";
  const identityLocked = Boolean(current);
  const targetCatalogKey = ["provider-invocation-targets", providerID] as const;
  const targetCatalog = useQuery({
    queryKey: targetCatalogKey,
    queryFn: () => api.invocationTargets(providerID),
    enabled: Boolean(providerID && !identityLocked),
    staleTime: 5 * 60 * 1000,
    retry: false,
  });
  const refreshTargetCatalog = useMutation({
    mutationFn: () => api.invocationTargets(providerID, "", true),
    onSuccess: (catalog) => queryClient.setQueryData(targetCatalogKey, catalog),
  });
  const availableTargetKinds = targetCatalog.data?.discovery?.target_kinds ?? (source?.target_kind ? [source.target_kind] : [targetKind]);
  useEffect(() => {
    if (identityLocked || !targetCatalog.data?.discovery?.target_kinds?.length) return;
    if (!targetCatalog.data.discovery.target_kinds.includes(targetKind)) {
      setTargetKind(targetCatalog.data.discovery.target_kinds[0]);
    }
  }, [identityLocked, targetCatalog.data, targetKind]);
  const listedTarget = targetCatalog.data?.items?.find((item) => item.target_id === providerModel.trim() && item.target_kind === targetKind) ?? null;
  const targetResolution = useQuery({
    queryKey: ["invocation-target-resolution", providerID, providerModel.trim(), targetKind, canonicalModelRef, region],
    queryFn: () => api.resolveInvocationTarget(providerID, providerModel.trim(), { targetKind, canonicalModelRef, region: region.trim() }),
    enabled: Boolean(!identityLocked && providerID && providerModel.trim() && (!listedTarget || canonicalModelRef)),
    retry: false,
  });
  const resolvedTarget = selectedTarget?.target_id === providerModel.trim() && selectedTarget.target_kind === targetKind && !canonicalModelRef
    ? selectedTarget
    : listedTarget && !canonicalModelRef
      ? listedTarget
      : targetResolution.data ?? null;
  const declaredModel = !current && providerModel.trim() !== "" && resolvedTarget?.resolution_state === "unknown";
  const noVariant = !current && providerModel.trim() !== "" && resolvedTarget?.resolution_state === "no_variant";
  const detectionBindingRequired = declaredModel && selectableBindings.length > 1;
  useEffect(() => {
    if (current || manualDeclaration || capabilityDetection) return;
    const variants = resolvedTarget?.variants ?? [];
    if (variants.length === 1 && !resolutionRequiresConfirmation) {
      setSelectedVariant(variants[0]);
      setBindingID(variants[0].binding_id);
      setCapabilities({ ...variants[0].capabilities });
    } else if (!variants.some((variant) => variant.revision === selectedVariant?.revision)) {
      setSelectedVariant(null);
      setBindingID("");
      setCapabilities(emptyCapabilities());
    }
  }, [capabilityDetection, current, manualDeclaration, resolutionRequiresConfirmation, resolvedTarget, selectedVariant?.revision]);
  const detectionQuery = useQuery({
    queryKey: ["model-capability-detection", capabilityDetection?.id],
    queryFn: () => api.modelCapabilityDetection(capabilityDetection!.id),
    enabled: Boolean(capabilityDetection?.id && !["completed", "failed", "canceled", "interrupted"].includes(capabilityDetection.status)),
    refetchInterval: 750,
    retry: false,
  });
  const detection = detectionQuery.data ?? capabilityDetection;
  const detectionNeedsProviderRepair = Boolean(
    detection?.status === "completed"
    && Object.values(detection.capabilities).some((result) => result.status === "unauthorized" || result.status === "unavailable"),
  );
  useEffect(() => {
    if (!detection || detection.revision <= appliedDetectionRevision.current) return;
    const belongsToAcceptedRequest = detection.id === capabilityDetection?.id
      && capabilityDetection.selection_revision === selectionRevision;
    if (detection.selection_revision && detection.selection_revision !== selectionRevision && !belongsToAcceptedRequest) return;
    setCapabilityDetection(detection);
    if (detection.status === "completed") {
      setCapabilities({ ...detection.recommended_capabilities });
      setBindingID(detection.binding_id);
      setManualDeclaration(false);
    }
    appliedDetectionRevision.current = detection.revision;
  }, [detection, selectionRevision]);
  const resetDetection = () => {
    setCapabilityDetection(null);
    appliedDetectionRevision.current = 0;
    detectionIdempotencyKey.current = crypto.randomUUID();
    setSelectionRevision(crypto.randomUUID());
    setManualDeclaration(false);
    setResolutionRequiresConfirmation(false);
  };
  const detectCapabilities = useMutation({
    mutationFn: (requestedSelectionRevision: string) => api.createModelCapabilityDetection(providerID, {
      provider_model: providerModel.trim(), target_kind: targetKind, region: region.trim(),
      ...(detectionBindingID ? { binding_id: detectionBindingID } : {}), risk_tier: "safe_automatic",
      selection_revision: requestedSelectionRevision,
    }, detectionIdempotencyKey.current),
    onSuccess: (result, requestedSelectionRevision) => {
      if (requestedSelectionRevision !== selectionRevision) return;
      // A provider/model result may be reused from the server cache and carry
      // the selection token of the client that originally created it. The
      // mutation variables are the correlation boundary for this click; keep
      // that boundary locally while preserving the shared detection identity.
      setCapabilityDetection({ ...result, selection_revision: requestedSelectionRevision });
    },
  });
  const cancelDetection = useMutation({
    mutationFn: () => api.cancelModelCapabilityDetection(detection!.id, detection!.revision),
    onSuccess: setCapabilityDetection,
  });
  const value = () => ({
    name: name.trim(),
    provider_id: providerID,
    ...(bindingID ? { binding_id: bindingID } : {}),
    provider_model: providerModel.trim(),
    ...(canonicalModelRef ? { capability_model: canonicalModelRef } : {}),
    target_kind: targetKind,
    capabilities,
    ...(declaredModel && manualDeclaration ? { mode: "operator_declared" } : {}),
    ...(declaredModel && detection?.status === "completed" ? {
      capability_detection_id: detection.id,
      capability_detection_revision: detection.revision,
    } : {}),
    ...(!current && !manualDeclaration && !detection && selectedVariant ? { resolution_revision: selectedVariant.revision } : {}),
    region: region.trim(),
    max_concurrency: maxConcurrency,
    priority: current?.priority ?? 0,
    weight: current?.weight ?? 1,
    enabled: current?.enabled ?? false,
  });
  const mutation = useMutation({
    mutationFn: () => current
      ? api.updateDeployment(current.id, value(), current.revision)
      : api.createDeployment(value()),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["deployments"] });
      onClose();
    },
    onError: (error) => {
      if (!(error instanceof ApiError) || error.code !== "resolution_changed") return;
      const latest = (error.payload as { resolution?: ResolvedInvocationTarget } | undefined)?.resolution;
      if (!latest) return;
      queryClient.setQueryData(
        ["invocation-target-resolution", providerID, providerModel.trim(), targetKind, canonicalModelRef, region],
        latest,
      );
      setSelectedTarget(latest);
      setSelectedVariant(null);
      setResolutionRequiresConfirmation(true);
      setBindingID("");
      setCapabilities(emptyCapabilities());
      queryClient.invalidateQueries({ queryKey: targetCatalogKey });
    },
  });
  // Turning a capability off is allowed in place, and that is exactly why it
  // needs asking about first: the router drops candidates that lack a required
  // capability, so a public model can lose its last one and start rejecting
  // requests that used to work. The server answers which routes those are.
  const narrowing = Boolean(current && deploymentCapabilityNames.some((name) => current.capabilities[name] && !capabilities[name]));
  // Turning a capability on makes the deployment claim something no test has
  // exercised, so the server drops it out of routing until it is retested and
  // re-enabled. Saying so here means that is a decision, not a surprise.
  const widening = Boolean(current && deploymentCapabilityNames.some((name) => !current.capabilities[name] && capabilities[name]));
  const preflight = useMutation({
    mutationFn: () => api.preflightDeploymentCapabilities(current!.id, capabilities),
    onSuccess: (result) => {
      // Nothing would be stranded, so there is nothing to confirm.
      if (!result.affected_routes.length) mutation.mutate();
    },
  });
  const impact = preflight.data;
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (!formValid) return;
    if (narrowing && !impact) {
      preflight.mutate();
      return;
    }
    mutation.mutate();
  };
  // Editing the capabilities again invalidates the answer that was given about
  // them; a stale confirmation must not carry over to a different change.
  const changeCapabilities = (next: ProviderCapabilities) => {
    if (preflight.data || preflight.isError) preflight.reset();
    setCapabilities(next);
  };
  const catalogCeiling = current
    ? catalogEditCeiling(current)
    : detection?.status === "completed"
      ? detection.recommended_capabilities
      : selectedVariant
      ? selectedVariant.capabilities
      : null;
  const bindingCeiling = pinnedBinding?.capabilities ?? unionCapabilities(selectableBindings);
  const capabilityCeiling = catalogCeiling
    ? pinnedBinding ? intersectCapabilityCeilings(catalogCeiling, bindingCeiling) : catalogCeiling
    : bindingCeiling;
  const configurableCapabilityNames = deploymentCapabilityNames.filter((name) => capabilityCeiling[name] || capabilities[name]);
  const targetLabel = t(`deployments.targetLabels.${targetKind}`);
  const modelCatalogSupported = Boolean(!identityLocked && targetCatalog.data?.discovery.can_enumerate);
  const capabilityModelSupported = Boolean(!identityLocked && targetCatalog.data?.discovery.requires_canonical_model_mapping);
  const modelPickerRef = useRef<HTMLDivElement>(null);
  const modelOptionsRef = useRef<HTMLDivElement>(null);
  const [modelPickerOpen, setModelPickerOpen] = useState(false);
  const [activeModelIndex, setActiveModelIndex] = useState(-1);
  const visibleModels = useMemo(() => {
    const query = providerModel.trim().toLocaleLowerCase();
    return (targetCatalog.data?.items ?? [])
      .filter((target) => target.target_kind === targetKind && (!query || target.display_name.toLocaleLowerCase().includes(query)));
  }, [targetCatalog.data?.items, providerModel, targetKind]);
  useEffect(() => {
    if (!modelPickerOpen) return;
    const closeOnOutsideInteraction = (event: PointerEvent) => {
      if (!modelPickerRef.current?.contains(event.target as Node)) {
        setModelPickerOpen(false);
        setActiveModelIndex(-1);
      }
    };
    document.addEventListener("pointerdown", closeOnOutsideInteraction);
    return () => document.removeEventListener("pointerdown", closeOnOutsideInteraction);
  }, [modelPickerOpen]);
  useEffect(() => {
    if (activeModelIndex < 0) return;
    const activeOption = modelOptionsRef.current?.querySelector<HTMLElement>(`[data-model-index="${activeModelIndex}"]`);
    activeOption?.scrollIntoView?.({ block: "nearest" });
  }, [activeModelIndex]);
  const chooseModel = (target: ResolvedInvocationTarget) => {
    resetDetection();
    setProviderModel(target.target_id);
    setTargetKind(target.target_kind);
    setSelectedTarget(target);
    setSelectedVariant(null);
    setCanonicalModelRef("");
    setCapabilities(emptyCapabilities());
    setBindingID("");
    setModelPickerOpen(false);
    setActiveModelIndex(-1);
  };
  const handleModelPickerKeyDown = (event: KeyboardEvent<HTMLInputElement>) => {
    if (!modelCatalogSupported) return;
    if (event.key === "ArrowDown") {
      event.preventDefault();
      event.stopPropagation();
      setModelPickerOpen(true);
      setActiveModelIndex((currentIndex) => Math.min(currentIndex + 1, visibleModels.length - 1));
    } else if (event.key === "ArrowUp") {
      event.preventDefault();
      event.stopPropagation();
      setModelPickerOpen(true);
      setActiveModelIndex((currentIndex) => currentIndex <= 0 ? Math.max(visibleModels.length - 1, 0) : currentIndex - 1);
    } else if (event.key === "Enter" && modelPickerOpen && activeModelIndex >= 0 && visibleModels[activeModelIndex]) {
      event.preventDefault();
      chooseModel(visibleModels[activeModelIndex]);
    } else if (event.key === "Escape" && modelPickerOpen) {
      event.preventDefault();
      event.stopPropagation();
      setModelPickerOpen(false);
      setActiveModelIndex(-1);
    } else if (event.key === "Tab") {
      setModelPickerOpen(false);
      setActiveModelIndex(-1);
    }
  };
  const selectedCapabilityNames = deploymentCapabilityNames.filter((name) => capabilities[name]);
  const modelResolutionMessage = declaredModel
    ? t("deployments.modelDeclarationRequired")
    : canonicalModelRef
      ? t("deployments.capabilityModelApplied")
      : resolvedTarget?.resolution_state === "resolved"
        ? t("deployments.modelResolvedCapabilities")
        : "";
  const regional = selectedProvider?.access_surface === "bedrock-runtime" || selectedProvider?.access_surface === "bedrock-agent-runtime";
  const fixedPriced = capabilities.moderations || capabilities.images || capabilities.transcriptions || capabilities.speech || capabilities.files || capabilities.batches || capabilities.rerank || capabilities.async_generate;
  const anyOperation = capabilities.chat || capabilities.embeddings || fixedPriced;
  const numericValues = [maxConcurrency, capabilities.max_context_tokens, capabilities.max_output_tokens];
  const tokenLimitsValid = capabilities.max_context_tokens === 0 || capabilities.max_output_tokens <= capabilities.max_context_tokens;
  const resolutionReady = Boolean(current || selectedVariant || detection?.status === "completed" && anyOperation || manualDeclaration && bindingID);
  const formValid = Boolean(name.trim() && providerID && providerModel.trim() && resolutionReady && anyOperation && numericValues.every((value) => Number.isFinite(value) && value >= 0) && tokenLimitsValid);
  const dirty = useDirty({ name, providerID, providerModel, canonicalModelRef, bindingID, capabilities, region, maxConcurrency, targetKind });
  return (
    <Modal wide title={current ? t("deployments.edit") : template ? t("deployments.createReplacementTitle") : t("deployments.createTitle")} dirty={dirty} onClose={onClose}>
      {enabledProviders.length === 0 ? (
        <div className="notice warning"><strong>{t("deployments.providerRequired")}</strong><span>{t("deployments.providerRequiredDescription")}</span><Link className="notice-link" href="/admin/providers">{t("deployments.openProviders")}</Link></div>
      ) : (
        <form className="deployment-form" onSubmit={submit}>
          <section className="deployment-form-section" aria-labelledby="deployment-target-heading">
            <header><h3 id="deployment-target-heading">{t("deployments.targetSection")}</h3><p>{t("deployments.targetSectionDescription")}</p></header>
            <div className="form-grid deployment-target-grid">
          <Field label={t("deployments.name")}><input autoFocus required value={name} onChange={(event) => setName(event.target.value)} /></Field>
          <Field label={t("deployments.provider")}>
            <select required disabled={identityLocked} value={providerID} onChange={(event) => {
              const next = event.target.value;
              resetDetection();
              setProviderID(next);
              if (enabledProviders.some((item) => item.id === next)) {
                setBindingID("");
                setCapabilities(emptyCapabilities());
                setSelectedTarget(null);
                setSelectedVariant(null);
                setCanonicalModelRef("");
                setProviderModel("");
                setRegion("");
              }
            }}>
              {enabledProviders.map((provider) => <option key={provider.id} value={provider.id}>{provider.name}</option>)}
            </select>
          </Field>
          {availableTargetKinds.length > 1 && <Field label={t("deployments.targetKind")} hint={t("deployments.targetKindHint")}>
            <select disabled={identityLocked} value={targetKind} onChange={(event) => { resetDetection(); setTargetKind(event.target.value as DeploymentTargetKind); setProviderModel(""); setSelectedTarget(null); setSelectedVariant(null); setCanonicalModelRef(""); setCapabilities(emptyCapabilities()); }}>
              {availableTargetKinds.map((kind) => <option value={kind} key={kind}>{t(`deployments.targetKinds.${kind}`)}</option>)}
            </select>
          </Field>}
          <div className="deployment-model-picker deployment-model-field" ref={modelPickerRef}>
            <div className="field">
              <label className="deployment-model-label" htmlFor="deployment-provider-model-id">{targetLabel}</label>
              <div className={`deployment-model-input-shell ${modelPickerOpen ? "open" : ""}`}>
              <input
                id="deployment-provider-model-id"
                required
                disabled={identityLocked}
                role={modelCatalogSupported ? "combobox" : undefined}
                aria-autocomplete={modelCatalogSupported ? "list" : undefined}
                aria-expanded={modelCatalogSupported ? modelPickerOpen : undefined}
                aria-controls={modelCatalogSupported ? "deployment-provider-model-options" : undefined}
                aria-activedescendant={modelPickerOpen && activeModelIndex >= 0 ? `deployment-provider-model-option-${activeModelIndex}` : undefined}
                value={providerModel}
                onFocus={() => { if (modelCatalogSupported) setModelPickerOpen(true); }}
                onClick={() => { if (modelCatalogSupported) setModelPickerOpen(true); }}
                onChange={(event) => {
                  resetDetection();
                  setProviderModel(event.target.value);
                  setSelectedTarget(null);
                  setSelectedVariant(null);
                  setBindingID("");
                  setCapabilities(emptyCapabilities());
                  setModelPickerOpen(true);
                  setActiveModelIndex(-1);
                }}
                onKeyDown={handleModelPickerKeyDown}
              />
                {modelCatalogSupported && <span className="deployment-model-input-icon" aria-hidden="true" />}
                {modelCatalogSupported && modelPickerOpen && (
                  <div className="deployment-model-options" id="deployment-provider-model-options" ref={modelOptionsRef} role="listbox" aria-label={t("deployments.modelCatalogLabel")}>
                    {visibleModels.length ? visibleModels.map((model, index) => (
                      <button
                        className={index === activeModelIndex ? "active" : ""}
                        id={`deployment-provider-model-option-${index}`}
                        data-model-index={index}
                        key={`${model.target_kind}:${model.target_id}`}
                        role="option"
                        aria-selected={providerModel === model.target_id}
                        type="button"
                        onMouseDown={(event) => event.preventDefault()}
                        onMouseEnter={() => setActiveModelIndex(index)}
                        onClick={() => chooseModel(model)}
                      >
                        <strong>{model.display_name}</strong>
                      </button>
                    )) : (
                      <div className="deployment-model-empty">{targetCatalog.isPending ? t("deployments.modelCatalogLoading") : t("deployments.noModelMatches")}</div>
                    )}
                  </div>
                )}
              </div>
              <small>{t(`deployments.targetHints.${targetKind}`)}</small>
            </div>
            {!identityLocked && providerModel.trim() !== "" && modelResolutionMessage && (
              <div className={`deployment-model-declaration ${declaredModel ? "declared" : "catalogued"}`} role="status">
                {modelResolutionMessage}
              </div>
            )}
            {modelCatalogSupported && targetCatalog.data?.degraded_bindings?.length ? (
              <div className="deployment-model-degraded" role="status">
                {t("deployments.modelCatalogPartial", { count: targetCatalog.data.degraded_bindings.length })}
              </div>
            ) : null}
            {modelCatalogSupported && (
              <div className="deployment-model-catalog-status" role="status">
                <span>
                  {targetCatalog.isPending
                    ? t("deployments.modelCatalogLoading")
                    : targetCatalog.isError || refreshTargetCatalog.isError
                      ? t("deployments.modelCatalogUnavailable")
                      : <>{t("deployments.modelCatalogCountPrefix")} <strong className="deployment-model-count">{targetCatalog.data?.items?.length ?? 0}</strong> {t("deployments.modelCatalogCountSuffix")}</>}
                </span>
                <button
                  className="button ghost deployment-model-refresh"
                  type="button"
                  disabled={targetCatalog.isPending || refreshTargetCatalog.isPending}
                  onClick={() => refreshTargetCatalog.mutate()}
                >
                  {refreshTargetCatalog.isPending ? t("deployments.refreshingModels") : t("deployments.refreshModels")}
                </button>
              </div>
            )}
          </div>
          {capabilityModelSupported && <Field label={t("deployments.capabilityModel")} hint={t("deployments.capabilityModelHint")}>
            <select
              value={canonicalModelRef}
              onChange={(event) => {
                resetDetection();
                const selectedModelID = event.target.value;
                setCanonicalModelRef(selectedModelID);
                setSelectedVariant(null);
                setBindingID("");
                setCapabilities(emptyCapabilities());
              }}
            >
              <option value="">{targetCatalog.isPending ? t("deployments.capabilityModelLoading") : t("deployments.capabilityModelUnknown")}</option>
              {(targetCatalog.data?.canonical_models ?? []).map((model) => (
                <option key={model.resolution_revision} value={model.canonical_model_ref}>{model.display_name}</option>
              ))}
            </select>
          </Field>}
          {regional && <Field label={t("deployments.region")} hint={t("deployments.regionHint")}><input disabled={identityLocked} value={region} placeholder={t("deployments.regionAutomatic")} onChange={(event) => { resetDetection(); setRegion(event.target.value); setSelectedTarget(null); setSelectedVariant(null); setBindingID(""); setCapabilities(emptyCapabilities()); }} /></Field>}
          {identityLocked && <div className="notice"><strong>{t("deployments.targetLocked")}</strong><span>{t("deployments.targetLockedDescription")}</span></div>}
            </div>
          </section>
          <section className="deployment-form-section" aria-labelledby="deployment-capabilities-heading">
            <header><h3 id="deployment-capabilities-heading">{t("deployments.capabilitySection")}</h3><p>{t("deployments.capabilitySectionDescription")}</p></header>
            {!providerModel.trim() && <div className="deployment-capability-empty">
              <strong>{t("deployments.selectModelFirst")}</strong>
              <span>{t("deployments.selectModelFirstDescription")}</span>
            </div>}
            {!current && targetResolution.isError && <div className="notice warning">
              <strong>{t("deployments.resolutionUnavailableTitle")}</strong>
              <span>{t("deployments.resolutionUnavailableDescription")}</span>
              <div className="form-actions">
                <button type="button" className="button ghost" disabled={targetResolution.isFetching} onClick={() => targetResolution.refetch()}>{t("deployments.retryResolution")}</button>
                <Link className="notice-link" href="/admin/providers">{t("deployments.openProviders")}</Link>
              </div>
            </div>}
            {!current && resolvedTarget && ((resolvedTarget.variants?.length ?? 0) > 1 || resolutionRequiresConfirmation) && !manualDeclaration && !detection && <fieldset className="deployment-variant-picker">
              <legend>{t("deployments.variantChoiceTitle", { count: resolvedTarget.variants.length })}</legend>
              <p>{t("deployments.variantChoiceDescription")}</p>
              {resolvedTarget.variants.map((variant, index) => <label key={variant.revision}>
                <input
                  type="radio"
                  name="deployment-variant"
                  checked={selectedVariant?.revision === variant.revision}
                  onChange={() => {
                    setSelectedVariant(variant);
                    setResolutionRequiresConfirmation(false);
                    setBindingID(variant.binding_id);
                    setCapabilities({ ...variant.capabilities });
                  }}
                />
                <span>{variantLabel(variant, resolvedTarget.variants, index, t)}</span>
              </label>)}
              <details><summary>{t("deployments.technicalDetails")}</summary><code>{resolvedTarget.variants.map((variant) => variant.profile_id).join(" · ")}</code></details>
            </fieldset>}
            {!current && selectedVariant && !manualDeclaration && !detection && <CapabilitySummary capabilities={capabilities} sources={claimSources(selectedVariant)} />}
            {!current && selectedVariant && !manualDeclaration && !detection && <details className="capability-disclosure capability-advanced">
              <summary><span>{t("deployments.capabilityNarrowing")}</span><strong>{t("providers.selectedCapabilities", { count: selectedCapabilityNames.length })}</strong></summary>
              <p className="capability-advanced-note">{t("deployments.inheritedCapabilitiesHint")}</p>
              <CapabilitySubsetEditor capabilities={capabilities} ceiling={selectedVariant.capabilities} onChange={changeCapabilities} />
            </details>}
            {!current && detection?.status === "completed" && anyOperation && <CapabilitySummary capabilities={detection.recommended_capabilities} sources={[detection.source]} />}
            {!current && noVariant && <div className="notice warning"><strong>{t("deployments.noVariantTitle")}</strong><span>{t("deployments.noVariantDescription")}</span><Link className="notice-link" href="/admin/providers">{t("deployments.openProviders")}</Link></div>}
            {!current && resolvedTarget?.resolution_state === "conflicting" && <div className="notice warning"><strong>{t("deployments.resolutionConflictTitle")}</strong><span>{t("deployments.resolutionConflictDescription")}</span></div>}
            {!current && declaredModel && !manualDeclaration && (
              <div className="capability-detection-panel" aria-live="polite">
                {!detection && <>
                  <div className="notice warning"><strong>{t("deployments.detectionUnknownTitle")}</strong><span>{t("deployments.detectionCostBoundary", { count: 8 })}</span></div>
                  {detectionBindingRequired && <Field label={t("deployments.binding")} hint={t("deployments.detectionBindingHint")}>
                    <select value={bindingID} onChange={(event) => {
                      resetDetection();
                      detectCapabilities.reset();
                      setBindingID(event.target.value);
                      setCapabilities(emptyCapabilities());
                    }}>
                      <option value="">{t("deployments.interfaceRequired")}</option>
                      {selectableBindings.map((binding) => <option value={binding.id} key={binding.id}>{bindingLabel(binding, t)}</option>)}
                    </select>
                  </Field>}
                  <div className="form-actions">
                    <button type="button" className="button primary" disabled={!providerModel.trim() || !targetCatalog.data?.discovery.can_verify || detectionBindingRequired && !detectionBindingID || detectCapabilities.isPending} onClick={() => detectCapabilities.mutate(selectionRevision)}>{detectCapabilities.isPending ? t("common.working") : t("deployments.confirmAndDetect")}</button>
                    <button type="button" className="button ghost" onClick={() => { setManualDeclaration(true); setCapabilities(emptyCapabilities()); }}>{t("deployments.advancedManualDeclaration")}</button>
                  </div>
                </>}
                {detection && (detection.status === "queued" || detection.status === "running") && <>
                  <div className="notice"><strong>{t("deployments.detectingCapabilities", { completed: Object.values(detection.capabilities).filter((result) => result.status !== "not_probed").length, total: detection.max_provider_calls })}</strong><span>{t("deployments.detectionCancelCost")}</span></div>
                  <button type="button" className="button ghost" disabled={cancelDetection.isPending} onClick={() => cancelDetection.mutate()}>{t("deployments.cancelDetection")}</button>
                </>}
                {detection?.status === "completed" && anyOperation && <div className="notice success"><strong>{t("deployments.detectionCompleted", { count: deploymentCapabilityNames.filter((name) => detection.recommended_capabilities[name]).length })}</strong><span>{detection.source === "builtin_catalog" ? t("deployments.detectionCatalogEvidence") : t("deployments.detectionVerifiedEvidence")}</span></div>}
                {detection?.status === "completed" && !anyOperation && detectionNeedsProviderRepair && <div className="notice warning"><strong>{t("deployments.detectionProviderRepairTitle")}</strong><span>{t("deployments.detectionProviderRepairDescription")}</span><Link className="notice-link" href="/admin/providers">{t("deployments.openProviders")}</Link></div>}
                {detection?.status === "completed" && !anyOperation && !detectionNeedsProviderRepair && <div className="notice warning"><strong>{t("deployments.detectionInconclusiveTitle")}</strong><span>{t("deployments.detectionInconclusiveDescription")}</span><div className="form-actions"><button type="button" className="button ghost" onClick={() => { resetDetection(); setManualDeclaration(true); }}>{t("deployments.advancedManualDeclaration")}</button></div></div>}
                {detection && ["failed", "canceled", "interrupted"].includes(detection.status) && <div className="notice warning"><strong>{t(`deployments.detectionStatus.${detection.status}`)}</strong><span>{t("deployments.detectionRetryOrManual")}</span><div className="form-actions"><button type="button" className="button ghost" onClick={() => { resetDetection(); setManualDeclaration(true); }}>{t("deployments.advancedManualDeclaration")}</button></div></div>}
                {detection && Object.keys(detection.capabilities).length > 0 && <details className="detection-details">
                  <summary>
                    <span>{t("deployments.detectionResults")}</span>
                    <strong>{t("deployments.detectionResultCount", {
                      supported: Object.values(detection.capabilities).filter((result) => result.status === "supported").length,
                      total: Object.keys(detection.capabilities).length,
                    })}</strong>
                  </summary>
                  <div className="detection-result-groups">
                    {deploymentCapabilityGroups.map((group) => {
                      const names = group.capabilities.filter((name) => detection.capabilities[name]);
                      if (!names.length) return null;
                      const supported = names.filter((name) => detection.capabilities[name]?.status === "supported").length;
                      return <section className="detection-result-group" aria-labelledby={`detection-group-${group.id}`} key={group.id}>
                        <header>
                          <div><strong id={`detection-group-${group.id}`}>{t(`deployments.capabilityGroups.${group.id}.title`)}</strong><small>{t(`deployments.capabilityGroups.${group.id}.description`)}</small></div>
                          <span>{t("deployments.capabilityGroupSupported", { supported, total: names.length })}</span>
                        </header>
                        <div className="detection-results" data-count={names.length} role="list" aria-label={t(`deployments.capabilityGroups.${group.id}.title`)}>
                          {names.map((name) => {
                            const result = detection.capabilities[name];
                            return <div role="listitem" key={name}>
                              <span>{t(`capabilities.${name}`)}</span>
                              <strong className={`detection-result ${result.status}`}>{t(`deployments.detectionProbeStatus.${result.status}`)}</strong>
                            </div>;
                          })}
                        </div>
                      </section>;
                    })}
                  </div>
                </details>}
                {detection?.status === "completed" && detection.expires_at && <small className="technical detection-freshness">{t("deployments.detectionFreshUntil", { date: dateTime(detection.expires_at) })}</small>}
                {(detectCapabilities.isError || detectionQuery.isError || cancelDetection.isError) && <ErrorState error={detectCapabilities.error || detectionQuery.error || cancelDetection.error} />}
              </div>
            )}
            {providerModel.trim() !== "" && (current || manualDeclaration) && <div className="capability-disclosure capability-advanced">
              <header>
                <span>{t("providers.advancedCapabilities")}</span>
                <div className="deployment-capability-header-actions">
                  <strong>{t("providers.selectedCapabilities", { count: selectedCapabilityNames.length })}</strong>
                  {declaredModel && manualDeclaration && <button type="button" className="button ghost deployment-capability-back" onClick={() => setManualDeclaration(false)}>{t("deployments.backToDetectionOptions")}</button>}
                </div>
              </header>
              <fieldset className="deployment-capability-groups" id="deployment-capability-editor">
                <legend className="visually-hidden">{t("deployments.capabilitySubset")}</legend>
                {deploymentCapabilityGroups.map((group) => {
                  const names = group.capabilities.filter((name) => configurableCapabilityNames.includes(name));
                  if (!names.length) return null;
                  const selected = names.filter((name) => capabilities[name]).length;
                  return <section className="deployment-capability-group" aria-labelledby={`capability-group-${group.id}`} key={group.id}>
                    <header>
                      <div><strong id={`capability-group-${group.id}`}>{t(`deployments.capabilityGroups.${group.id}.title`)}</strong><small>{t(`deployments.capabilityGroups.${group.id}.description`)}</small></div>
                      <span>{t("deployments.capabilityGroupSelected", { selected, total: names.length })}</span>
                    </header>
                    <div className="deployment-capabilities capability-grid" data-count={names.length}>
                      {names.map((name) => {
                        const unavailable = !capabilityCeiling[name];
                        return <label className={`capability-option ${unavailable ? "unavailable" : ""}`} key={name}>
                          <input
                            type="checkbox"
                            disabled={unavailable && !capabilities[name]}
                            checked={capabilities[name]}
                            onChange={(event) => changeCapabilities(updateDeploymentCapability(capabilities, name, event.target.checked))}
                          />
                          <span>{t(`capabilities.${name}`)}{unavailable && <small>{t("providers.unsupportedByInterface")}</small>}</span>
                        </label>;
                      })}
                    </div>
                  </section>;
                })}
              </fieldset>
            </div>}
            {providerModel.trim() !== "" && !anyOperation && (manualDeclaration || !declaredModel || detection?.status === "completed") && <p className="deployment-operation-required" role="alert">{t("deployments.operationRequired")}</p>}
            {providerModel.trim() !== "" && manualDeclaration && (
              <details className="capability-disclosure capability-advanced">
                <summary>
                  <span>{t("deployments.interfaceAdvanced")}</span>
                  <strong>{pinnedBinding ? bindingLabel(pinnedBinding, t) : t("deployments.interfaceAutomatic")}</strong>
                </summary>
                <div className="deployment-interface-control">
                  <p>{t("deployments.interfaceAdvancedHint")}</p>
                  <select aria-label={t("deployments.binding")} disabled={identityLocked} value={bindingID} onChange={(event) => {
                      const next = event.target.value;
                      const nextBinding = selectableBindings.find((binding) => binding.id === next);
                      resetDetection();
                      setManualDeclaration(true);
                      setBindingID(next);
                      setCapabilities(nextBinding ? { ...nextBinding.capabilities } : emptyCapabilities());
                  }}>
                    <option value="">{t("deployments.interfaceRequired")}</option>
                    {selectableBindings.map((binding) => <option value={binding.id} key={binding.id}>{bindingLabel(binding, t)}</option>)}
                  </select>
                  <small>{t("deployments.bindingHint")}</small>
                </div>
              </details>
            )}
          </section>
          <section className="deployment-form-section deployment-limit-section" aria-labelledby="deployment-limits-heading">
            <header><h3 id="deployment-limits-heading">{t("deployments.limitSection")}</h3><p>{t("deployments.limitSectionDescription")}</p></header>
            <div className="deployment-limit-grid">
              <Field label={t("deployments.maxContext")} hint={t("deployments.maxContextHint")}>
                <input min="0" type="number" value={capabilities.max_context_tokens} onChange={(event) => setCapabilities({ ...capabilities, max_context_tokens: Number(event.target.value) })} />
              </Field>
              <Field label={t("deployments.maxOutputTokens")} hint={t("deployments.maxOutputHint")}>
                <input min="0" type="number" value={capabilities.max_output_tokens} onChange={(event) => setCapabilities({ ...capabilities, max_output_tokens: Number(event.target.value) })} />
              </Field>
              <Field label={t("deployments.concurrencyLimit")} hint={t("deployments.concurrencyHint")}><input min="0" type="number" value={maxConcurrency} onChange={(event) => setMaxConcurrency(Number(event.target.value))} /></Field>
            </div>
            {!tokenLimitsValid && <div className="notice warning"><span>{t("deployments.tokenLimitInvalid")}</span></div>}
          </section>
          {widening && <div className="notice warning deployment-capability-expansion">
            <strong>{t("deployments.expansionNeedsRevalidation")}</strong>
            <span>{current?.enabled ? t("deployments.expansionWhileRouted") : t("deployments.expansionSavedDisabled")}</span>
          </div>}
          {impact && <CapabilityImpactNotice impact={impact} />}
          {mutation.isError && mutation.error instanceof ApiError && mutation.error.code === "resolution_changed"
            ? <div className="notice warning"><strong>{t("deployments.resolutionChangedTitle")}</strong><span>{t("deployments.resolutionChangedDescription")}</span></div>
            : (mutation.isError || preflight.isError) && <ErrorState error={mutation.error || preflight.error} />}
          <div className="form-actions deployment-form-actions">
            <div className="deployment-save-summary">
              <strong>{current?.enabled ? t("deployments.updateLiveWarning") : t("deployments.savedDisabled")}</strong>
              <small>{current?.enabled ? t("deployments.updateLiveDescription") : t("deployments.savedDisabledDescription")}</small>
            </div>
            <button type="button" className="button ghost" onClick={onClose}>{t("common.cancel")}</button>
            <button className={`button ${impact?.blocking ? "danger" : "primary"}`} disabled={mutation.isPending || preflight.isPending || !formValid}>
              {preflight.isPending
                ? t("deployments.checkingRouteImpact")
                : impact
                  ? t("deployments.saveDespiteImpact")
                  : narrowing
                    ? t("deployments.checkRouteImpact")
                    : current ? t("deployments.save") : template ? t("deployments.saveReplacement") : t("deployments.saveDisabled")}
            </button>
          </div>
        </form>
      )}
    </Modal>
  );
}

function variantCapabilityLabel(variant: DeploymentVariant, t: ReturnType<typeof useTranslation>["t"]): string {
  const operations = ["chat", "embeddings", "moderations", "images", "transcriptions", "speech", "files", "batches", "rerank", "async_generate"] as const;
  const labels = operations.filter((name) => variant.capabilities[name]).map((name) => t(`capabilities.${name}`));
  return labels.join(" · ") || variant.profile_id;
}

function variantLabel(variant: DeploymentVariant, variants: DeploymentVariant[], index: number, t: ReturnType<typeof useTranslation>["t"]): string {
  const label = variantCapabilityLabel(variant, t);
  const duplicate = variants.filter((candidate) => variantCapabilityLabel(candidate, t) === label).length > 1;
  return duplicate ? t("deployments.variantLabelWithInterface", { capabilities: label, index: index + 1 }) : label;
}

type CapabilityClaimSource = DeploymentVariant["capability_claims"][number]["source"] | ModelCapabilityDetection["source"];

function claimSources(variant: DeploymentVariant): CapabilityClaimSource[] {
  return [...new Set(variant.capability_claims.filter((claim) => claim.status === "supported").map((claim) => claim.source))];
}

function CapabilitySummary({ capabilities, sources }: { capabilities: ProviderCapabilities; sources: CapabilityClaimSource[] }) {
  const { t } = useTranslation();
  const names = deploymentCapabilityNames.filter((name) => capabilities[name]);
  const separator = t("deployments.capabilityListSeparator");
  const sourceSummary = sources.map((source) => t(`deployments.capabilityEvidenceSources.${source}`)).join(separator);
  return <div className="deployment-capability-summary" aria-live="polite">
    <strong>{t("deployments.capabilityIdentified")}</strong>
    <span>{t("deployments.capabilityAvailableFor", { capabilities: names.map((name) => t(`capabilities.${name}`)).join(separator) })}</span>
    {sourceSummary && <small>{t("deployments.capabilityEvidenceSummary", { source: sourceSummary })}</small>}
  </div>;
}

function CapabilitySubsetEditor({ capabilities, ceiling, onChange }: {
  capabilities: ProviderCapabilities;
  ceiling: ProviderCapabilities;
  onChange: (next: ProviderCapabilities) => void;
}) {
  const { t } = useTranslation();
  return <fieldset className="deployment-capability-groups">
    <legend className="visually-hidden">{t("deployments.capabilitySubset")}</legend>
    {deploymentCapabilityGroups.map((group) => {
      const names = group.capabilities.filter((name) => ceiling[name] || capabilities[name]);
      if (!names.length) return null;
      return <section className="deployment-capability-group" key={group.id}>
        <header><strong>{t(`deployments.capabilityGroups.${group.id}.title`)}</strong></header>
        <div className="deployment-capabilities capability-grid">
          {names.map((name) => <label className="capability-option" key={name}>
            <input
              type="checkbox"
              disabled={!ceiling[name] && !capabilities[name]}
              checked={capabilities[name]}
              onChange={(event) => onChange(updateDeploymentCapability(capabilities, name, event.target.checked))}
            />
            <span>{t(`capabilities.${name}`)}</span>
          </label>)}
        </div>
      </section>;
    })}
  </fieldset>;
}

// Nothing declared. It used to hand back chat and streaming, which made "no
// answer yet" indistinguishable from "this model does chat", and that guess
// then rode along into the saved deployment.
function emptyCapabilities(): ProviderCapabilities {
  return {
    chat: false,
    streaming: false,
    embeddings: false,
    moderations: false,
    images: false,
    transcriptions: false,
    speech: false,
    files: false,
    batches: false,
    rerank: false,
    async_generate: false,
    tools: false,
    vision: false,
    json_mode: false,
    developer_role: false,
    reasoning: false,
    stream_usage: false,
    max_context_tokens: 0,
    max_output_tokens: 0,
  };
}

// What the operator may declare when no interface is pinned. The server accepts
// a declaration that any one enabled interface can carry, so the offer is the
// union — narrower would hide a legitimate choice, and the server still refuses
// a selection no single interface covers.
function unionCapabilities(bindings: SelectableBinding[]): ProviderCapabilities {
  return bindings.reduce<ProviderCapabilities>((union, binding) => {
    const merged = { ...union };
    for (const name of deploymentCapabilityNames) merged[name] = union[name] || binding.capabilities[name];
    merged.max_context_tokens = Math.max(union.max_context_tokens, binding.capabilities.max_context_tokens);
    merged.max_output_tokens = Math.max(union.max_output_tokens, binding.capabilities.max_output_tokens);
    return merged;
  }, emptyCapabilities());
}

function intersectCapabilityCeilings(left: ProviderCapabilities, right: ProviderCapabilities): ProviderCapabilities {
  const intersection = { ...left };
  for (const name of deploymentCapabilityNames) intersection[name] = left[name] && right[name];
  intersection.max_context_tokens = intersectLimit(left.max_context_tokens, right.max_context_tokens);
  intersection.max_output_tokens = intersectLimit(left.max_output_tokens, right.max_output_tokens);
  return intersection;
}

function intersectLimit(left: number, right: number): number {
  if (!left) return right;
  if (!right) return left;
  return Math.min(left, right);
}

// The ceiling for an existing deployment, when the catalog is what established
// it. `available_for_review` is what the catalog covers now and the deployment
// has not taken up, so the two together are the current catalog position. A
// deployment the operator declared has no catalog ceiling and falls back to the
// interface.
function catalogEditCeiling(deployment: Deployment): ProviderCapabilities | null {
  const review = deployment.capability_review;
  if (!review?.catalog_covered || review.source === "operator_declared") return null;
  const ceiling = { ...deployment.capabilities };
  for (const name of review.available_for_review ?? []) {
    if ((deploymentCapabilityNames as readonly string[]).includes(name)) {
      ceiling[name as typeof deploymentCapabilityNames[number]] = true;
    }
  }
  return ceiling;
}

function updateDeploymentCapability(current: ProviderCapabilities, capability: keyof ProviderCapabilities, enabled: boolean): ProviderCapabilities {
  const next = { ...current, [capability]: enabled };
  const chatFeatures = ["streaming", "tools", "vision", "json_mode", "developer_role", "reasoning", "stream_usage"] as const;
  if (capability === "chat" && !enabled) {
    for (const feature of chatFeatures) next[feature] = false;
  } else if (capability !== "chat" && chatFeatures.includes(capability as typeof chatFeatures[number]) && enabled) {
    next.chat = true;
  }
  return next;
}
