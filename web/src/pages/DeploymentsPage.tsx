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
import { isoToZonedInput, useAccountingTimeZone, zonedInputToISO } from "../timezone";
import type { CapabilityPreflight, CapabilityReview, Deployment, DeploymentPriceVersion, DeploymentTargetKind, DeploymentVariant, ModelCapabilityDetection, PriceSchedule, Provider, ProviderBinding, ProviderCapabilities, ProviderProfilesCatalog, ResolvedInvocationTarget } from "../types";
import { updateCapabilitySelection, useProviderProfiles } from "../hooks/useProviderProfiles";
import { useTranslation } from "react-i18next";
import { useNotify } from "../notifications";
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
            filteredDeployments.map((deployment, index) => (
              <DeploymentRow
                key={deployment.id}
                deployment={deployment}
                listIndex={index}
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
  listIndex,
  providerName,
  activeRouteCount,
  onEdit,
  onReplace,
}: {
  deployment: Deployment;
  listIndex: number;
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
  // The collapsed price column is worth having, but a fifty-row page must not
  // turn into fifty simultaneous price reads — each one runs a lifecycle
  // derivation server-side. Rows load in bounded batches instead, and an
  // expanded row jumps its queue because the operator is looking at it.
  const priceSlotReady = useDeferredSlot(expanded ? 0 : priceFetchDelay(listIndex));
  const prices = useQuery({
    queryKey: ["deployment-prices", deployment.id],
    queryFn: () => api.deploymentPrices(deployment.id),
    enabled: priceSlotReady,
    refetchInterval: (query) => scheduledPriceRefreshInterval(query.state.data?.items),
  });
  const priceItems = prices.data?.items ?? [];
  const activePrice = priceItems.find((price) => price.status === "active");
  const scheduledPrices = priceItems.filter((price) => price.status === "scheduled");
  // The server refuses any version that is not strictly later than every
  // non-cancelled one, so a scheduled version makes "immediately" unreachable
  // until it is cancelled. The form has to know that before it offers the
  // choice, or the operator is sent down a path that always ends in 409.
  const blockingPrice = scheduledPrices.reduce<DeploymentPriceVersion | undefined>(
    (latest, price) => (!latest || price.effective_from > latest.effective_from ? price : latest),
    undefined,
  );
  const { notify } = useNotify();
  const cancelPrice = useMutation({ mutationFn: (price: DeploymentPriceVersion) => api.cancelDeploymentPrice(deployment.id, price.id, price.revision), onSuccess: () => queryClient.invalidateQueries({ queryKey: ["deployment-prices", deployment.id] }) });
  const test = useMutation({
    mutationFn: () => api.testDeployment(deployment.id),
    onSettled: () => queryClient.invalidateQueries({ queryKey: ["deployments"] }),
  });
  const remove = useMutation({
    mutationFn: (reauth: ReauthValues) => api.deleteDeployment(deployment.id, deployment.revision, reauth),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["deployments"] });
      notify({ tone: "success", title: t("deployments.notifyDeleted"), description: deployment.name });
    },
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
      enabled: !deployment.enabled,
    }, deployment.revision),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["deployments"] });
      notify({ tone: "success", title: t(deployment.enabled ? "deployments.notifyDisabled" : "deployments.notifyEnabled"), description: deployment.name });
    },
  });
  // The "no effective price" banner is the failed enable attempt's error, and it
  // outlives the condition it describes: setting a price refreshes the price
  // column to "configured" while the dead mutation keeps rendering its blocker,
  // so the row contradicts itself until the page is reloaded. Drop the error
  // once the deployment actually has an active price. A price that is only
  // scheduled leaves the banner up, which is correct — enabling would still be
  // refused.
  const priceBlockedEnable = state.error instanceof ApiError && state.error.code === "deployment_price_unavailable";
  const resetState = state.reset;
  useEffect(() => {
    if (priceBlockedEnable && activePrice) resetState();
  }, [priceBlockedEnable, activePrice, resetState]);
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
    <article id={`deployment-${deployment.id}`} className="deployment-row">
      <div className="deployment-row-main">
        <div className="resource-identity">
          <span><StatusDot ok={deployment.enabled} /><strong>{deployment.name}</strong></span>
          <small>{providerName}</small>
        </div>
        <div className="resource-fact deployment-fact-target">
          <small>{t("deployments.upstreamTarget")}</small>
          <code>{deployment.provider_model}</code>
        </div>
        <div className="resource-fact deployment-fact-concurrency">
          <small>{t("deployments.concurrency")}</small>
          <strong>{deployment.max_concurrency || t("deployments.unlimited")}</strong>
        </div>
        <div className="resource-fact deployment-fact-routes">
          <small>{t("deployments.routeDependency")}</small>
          {activeRouteCount
            ? <Link className="resource-link" href="/admin/routes">{t("deployments.activeRoutesCompact", { count: activeRouteCount })} →</Link>
            : <strong>{t("deployments.noActiveRoutes")}</strong>}
        </div>
        <div className="resource-fact deployment-fact-price">
          <small>{t("deployments.priceSetting")}</small>
          {prices.isPending ? (
            <span className="deployment-price-value">{t("common.loading")}</span>
          ) : activePrice ? (
            <button
              className="deployment-price-link configured"
              type="button"
              aria-label={t("deployments.viewDeploymentPrice", { name: deployment.name })}
              onClick={() => setExpanded(true)}
            >{t("deployments.priceConfigured")}</button>
          ) : prices.isError ? (
            // Without a way back this cell was a dead end: it neither told an
            // assistive reader that something failed nor offered the one action
            // that can fix it.
            <span className="deployment-price-value" role="alert">
              <span>{t("deployments.priceUnavailable")}</span>
              <button className="button ghost" type="button" disabled={prices.isFetching} onClick={() => prices.refetch()}>{t("common.retry")}</button>
            </span>
          ) : (
            <button
              className="deployment-price-link missing"
              type="button"
              disabled={readOnly}
              title={readOnly ? t("navigation.readOnlyAction") : t("deployments.priceRequired")}
              aria-label={t("deployments.setDeploymentPrice", { name: deployment.name })}
              onClick={() => setPricing(true)}
            >{t("deployments.priceNotConfigured")}</button>
          )}
        </div>
        <div className="resource-row-state">
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
          {/* Shown even when it equals the input rate: that is itself the fact
              worth reading, since it means cached prompt tokens are not priced
              separately on this deployment. */}
          {(deployment.capabilities.chat || deployment.capabilities.embeddings) && <DeploymentFact label={t("deployments.cachedInputPrice")} value={activePrice ? money(activePrice.cached_input_micros_per_million) : t("deployments.notConfigured")} meta={t("deployments.perMillionTokens")} unset={!activePrice} />}
          {(deployment.capabilities.chat || deployment.capabilities.embeddings) && <DeploymentFact label={t("deployments.outputPrice")} value={activePrice ? money(activePrice.output_micros_per_million) : t("deployments.notConfigured")} meta={t("deployments.perMillionTokens")} unset={!activePrice} />}
          {activePrice && <DeploymentFact label={t("deployments.fixedPrice")} value={money(activePrice.fixed_request_micros_usd)} meta={t("deployments.perRequest")} />}
          {/* With a schedule the three rates above are only what applies
              outside the windows, so leaving them to stand alone would read as
              the whole price. */}
          {activePrice?.schedule && <DeploymentFact
            label={t("deployments.priceSchedule")}
            value={t("deployments.scheduleWindowCount", { count: activePrice.schedule.windows.length })}
            meta={`${activePrice.schedule.timezone} · ${activePrice.schedule.windows.map((window) => `${minuteToClock(window.start_minute)}–${minuteToClock(window.end_minute)}`).join(" ")}`}
          />}
          <DeploymentFact label={t("deployments.concurrency")} value={deployment.max_concurrency || t("deployments.unlimited")} meta={t("deployments.deploymentScope")} />
          {deployment.region && <DeploymentFact label={t("deployments.region")} value={deployment.region} meta={t("deployments.deploymentScope")} />}
          <DeploymentFact label={t("deployments.context")} value={deployment.capabilities.max_context_tokens || t("deployments.upstreamApplies")} meta={deployment.capabilities.max_context_tokens ? t("deployments.tokens") : t("deployments.undeclared")} />
          <DeploymentFact label={t("deployments.maxOutput")} value={deployment.capabilities.max_output_tokens || t("deployments.upstreamApplies")} meta={deployment.capabilities.max_output_tokens ? t("deployments.tokens") : t("deployments.undeclared")} />
        </dl>
        <CapabilityReviewNotice review={review} />
        {/* Every write in this panel is gated the way the compact row's price
            action already is; §7.3 also requires the disabled control to say
            why, so each one carries the reason. */}
        {deployment.pricing_quarantined && <div className="notice warning deployment-pricing-warning"><strong>{t("deployments.pricingQuarantined")}</strong><span>{deployment.pricing_quarantine_reason}</span><button className="button ghost" disabled={readOnly} title={readOnly ? t("navigation.readOnlyAction") : undefined} onClick={() => setConfirmingRestore(true)}>{t("deployments.confirmRestoredPricing")}</button></div>}
        {prices.isError && <ErrorState
          className="deployment-card-error"
          error={prices.error}
          action={<button className="button secondary" type="button" disabled={prices.isFetching} onClick={() => prices.refetch()}>{t("common.retry")}</button>}
        />}
        <div className="deployment-pricing-grid single">
          <section className="deployment-pricing-panel">
            <header>
              <div>
                <strong>{t("deployments.priceTimeline")}</strong>
                <small>{activePrice
                  ? t("deployments.priceSourceSummary", { type: activePrice.source.type, assurance: activePrice.source.assurance, reference: activePrice.source.reference || "—" })
                  : prices.isError ? t("deployments.priceUnavailable") : prices.isPending ? t("common.loading") : t("deployments.noPriceVersions")}</small>
              </div>
              <button className="button secondary deployment-pricing-action" disabled={readOnly} title={readOnly ? t("navigation.readOnlyAction") : undefined} onClick={() => setPricing(true)}>{activePrice ? t("deployments.adjustPrice") : t("deployments.setPrice")}</button>
            </header>
            {!!scheduledPrices.length && <div className="deployment-pricing-list">
              {scheduledPrices.map((price) => <div key={price.id}>
                <span><code>v{price.version}</code><small>{dateTime(price.effective_from)} · {price.billing_mode}{price.schedule ? ` · ${t("deployments.scheduleWindowCount", { count: price.schedule.windows.length })}` : ""}</small></span>
                <button className="button ghost" disabled={readOnly || cancelPrice.isPending} title={readOnly ? t("navigation.readOnlyAction") : undefined} onClick={() => cancelPrice.mutate(price)}>{t("common.cancel")}</button>
              </div>)}
            </div>}
          </section>
        </div>
        <div className="deployment-capability-summary deployment-capability-strip">
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
      {(remove.isError || state.isError) && <ErrorState
        className="deployment-card-error"
        error={remove.error || state.error}
        action={state.error instanceof ApiError && state.error.code === "deployment_price_unavailable"
          ? <button className="button secondary" type="button" onClick={() => setPricing(true)}>{t("deployments.setPrice")}</button>
          : undefined}
      />}
      {pricing && <PriceVersionForm deployment={deployment} current={activePrice} blocking={blockingPrice} onClose={() => setPricing(false)} />}
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

function PriceVersionForm({ deployment, current, blocking, onClose }: { deployment: Deployment; current?: DeploymentPriceVersion; blocking?: DeploymentPriceVersion; onClose: () => void }) {
  const { t } = useTranslation();
  const dateTime = useInstantFormatter();
  const queryClient = useQueryClient();
  const [step, setStep] = useState<"details" | "confirm">("details");
  const [mode, setMode] = useState<"metered" | "free">(current?.billing_mode ?? "metered");
  const [input, setInput] = useState(current ? priceInputValue(current.input_micros_per_million) : "0");
  // Null means the operator has not touched the cache-read rate, and it then
  // tracks the input rate: that is what a cached token cost before the term
  // existed, and what migration 30 wrote onto every price that predates it.
  // Defaulting the field to zero instead would give cached prompts away to
  // anyone who filled in the other two rates and moved on.
  const [cachedInput, setCachedInput] = useState<string | null>(current ? priceInputValue(current.cached_input_micros_per_million) : null);
  const cachedInputValue = cachedInput ?? input;
  const [output, setOutput] = useState(current ? priceInputValue(current.output_micros_per_million) : "0");
  const [fixed, setFixed] = useState(current ? priceInputValue(current.fixed_request_micros_usd) : "0");
  // A schedule is off unless the provider actually charges by time of day. The
  // four rates above are then what applies outside every window, which is why
  // the windows are edited underneath them rather than replacing them.
  const [schedule, setSchedule] = useState<ScheduleDraft | null>(current?.schedule ? scheduleToDraft(current.schedule) : null);
  const scheduleProblem = schedule ? scheduleDraftProblem(schedule) : undefined;
  // A scheduled version already occupies the end of the timeline, so the only
  // reachable choice is a later scheduled time — the form opens on it rather
  // than on an "immediately" the server is bound to refuse.
  const blockingFrom = blocking ? Date.parse(blocking.effective_from) : Number.NaN;
  const [effectiveMode, setEffectiveMode] = useState<"now" | "scheduled">(blocking ? "scheduled" : "now");
  // datetime-local carries no zone. This field is read and pre-filled in the
  // accounting zone, which is the zone the confirmation line beside it and the
  // price timeline behind it are rendered in — the browser's own wall clock put
  // the two an offset apart with nothing on screen to say so.
  const timeZone = useAccountingTimeZone();
  const [effective, setEffective] = useState(
    isoToZonedInput(new Date(Math.max(Date.now() + 3_600_000, (blockingFrom || 0) + 60_000)), timeZone),
  );
  const [confirmedEffective, setConfirmedEffective] = useState("");
  const [sourceKind, setSourceKind] = useState("temporary_estimate");
  const [sourceNote, setSourceNote] = useState("");
  const [password, setPassword] = useState("");
  const [totp, setTotp] = useState("");
  const idempotencyKey = useRef(crypto.randomUUID());
  const confirmSummary = useRef<HTMLElement>(null);
  const submitError = useRef<HTMLDivElement>(null);
  const validPrice = mode === "free" || ([input, cachedInputValue, output, fixed].every(validUSD) && [input, cachedInputValue, output, fixed].some((value) => Number(value) > 0));
  const scheduledTimestamp = Date.parse(zonedInputToISO(effective, timeZone));
  const validEffective = effectiveMode === "now"
    ? !blocking
    : Number.isFinite(scheduledTimestamp) && scheduledTimestamp > Date.now() && (!Number.isFinite(blockingFrom) || scheduledTimestamp > blockingFrom);
  const sourceNeedsNote = sourceKind === "official_public_price" || sourceKind === "contract_price";
  const validSource = !sourceNeedsNote || sourceNote.trim() !== "";
  const validDetails = validPrice && validEffective && validSource && !scheduleProblem;
  // The worked example prices a prompt with no cache hit, which is what a first
  // request costs; the cache-read rate only ever lowers it from here.
  const exampleCost = mode === "free" ? 0 : Number(input) / 1000 + Number(output) / 2000 + Number(fixed);
  const effectiveLabel = effectiveMode === "now" ? t("deployments.effectiveNow") : validEffective ? dateTime(new Date(scheduledTimestamp).toISOString()) : "—";
  // Someone adjusting a live price is changing what every request costs from
  // the moment they confirm. The version being replaced belongs on the same
  // screen as its replacement, or the change is invisible at the only point
  // where it can still be stopped.
  const previousPrice = current
    ? {
        input: priceInputValue(current.input_micros_per_million),
        cachedInput: priceInputValue(current.cached_input_micros_per_million),
        output: priceInputValue(current.output_micros_per_million),
        fixed: priceInputValue(current.fixed_request_micros_usd),
      }
    : undefined;
  const priceCell = (previous: string | undefined, next: string) =>
    previous !== undefined && previous !== next ? t("deployments.priceChange", { from: previous, to: next }) : `$${next}`;
  const mutation = useMutation({
    mutationFn: () => api.createDeploymentPrice(deployment.id, {
      billing_mode: mode, currency: "USD",
      input_usd_per_million: mode === "free" ? "0" : input,
      cached_input_usd_per_million: mode === "free" ? "0" : cachedInputValue,
      output_usd_per_million: mode === "free" ? "0" : output,
      fixed_request_usd: mode === "free" ? "0" : fixed,
      ...(mode === "metered" && schedule ? { schedule: draftToScheduleRequest(schedule) } : {}),
      ...(effectiveMode === "now"
        ? { effective_immediately: true }
        : { effective_from: confirmedEffective }),
      source: { type: "manual", reference: sourceKind, note: sourceNote.trim(), asserted_without_archive: true },
      current_password: password, totp_code: totp,
    }, idempotencyKey.current),
    onSuccess: () => { queryClient.invalidateQueries({ queryKey: ["deployment-prices", deployment.id] }); onClose(); },
  });
  const ambiguousResult = mutation.isError && (!(mutation.error instanceof ApiError) || mutation.error.status >= 500);
  useEffect(() => {
    if (step === "confirm") requestAnimationFrame(() => confirmSummary.current?.focus());
  }, [step]);
  // The submit button sits in a sticky footer, so a rejection renders into the
  // scrolled-away part of the modal: the operator sees the click do nothing and
  // clicks again. Bring the failure to them, and move focus onto it so the
  // reason is announced instead of merely present.
  useEffect(() => {
    if (!mutation.isError) return;
    requestAnimationFrame(() => {
      submitError.current?.scrollIntoView?.({ block: "center" });
      submitError.current?.focus();
    });
  }, [mutation.isError, mutation.error]);
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (step === "details") {
      if (validDetails) {
        setConfirmedEffective(effectiveMode === "now" ? "" : new Date(scheduledTimestamp).toISOString());
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
          {/* Every provider that reports a cache-read tier bills it well below
              the input rate, so this field is asked for beside the other two
              rather than hidden: left at the input rate it over-charges cached
              prompts, and left at zero it gives them away. */}
          <div className="price-form-grid">
            <Field label={t("deployments.cachedInputUSD")} hint={t("deployments.cachedInputHint")}><input inputMode="decimal" required value={cachedInputValue} onChange={(event) => setCachedInput(event.target.value)} /></Field>
          </div>
          {/* Some providers publish peak and off-peak rates. Without this the
              operator can only enter one number, and which way the accounting
              is wrong is decided by which number they happen to pick. */}
          <PriceScheduleFields schedule={schedule} onChange={setSchedule} problem={scheduleProblem} baseRates={{ input, cachedInput: cachedInputValue, output, fixed }} />
          <details className="price-advanced">
            <summary>{t("deployments.advancedPricing")}</summary>
            <Field label={t("deployments.fixedRequestUSD")}><input inputMode="decimal" required value={fixed} onChange={(event) => setFixed(event.target.value)} /></Field>
          </details>
        </>}
        <div className="price-form-grid">
          <Field label={t("deployments.effectiveMode")}><select value={effectiveMode} onChange={(event) => setEffectiveMode(event.target.value as "now" | "scheduled")}><option value="now" disabled={!!blocking}>{t("deployments.effectiveNow")}</option><option value="scheduled">{t("deployments.effectiveScheduled")}</option></select></Field>
          <Field label={t("deployments.priceSourceKind")}><select value={sourceKind} onChange={(event) => setSourceKind(event.target.value)}><option value="official_public_price">{t("deployments.sourceKinds.officialPublicPrice")}</option><option value="contract_price">{t("deployments.sourceKinds.contractPrice")}</option><option value="internal_cost">{t("deployments.sourceKinds.internalCost")}</option><option value="temporary_estimate">{t("deployments.sourceKinds.temporaryEstimate")}</option></select></Field>
        </div>
        {/* Naming the version that occupies the end of the timeline is the whole
            point: without it the operator only learns that "immediately" is
            unavailable, not which scheduled version to cancel to get it back. */}
        {blocking && <p className="field-hint">{t("deployments.scheduledBlocksImmediate", { version: blocking.version, effective: dateTime(blocking.effective_from) })}</p>}
        {effectiveMode === "scheduled" && <Field label={t("deployments.effectiveFrom")}><input type="datetime-local" required value={effective} onChange={(event) => setEffective(event.target.value)} /></Field>}
        {!validEffective && <p className="field-hint error">{blocking ? t("deployments.effectiveAfterScheduled", { version: blocking.version, effective: dateTime(blocking.effective_from) }) : t("deployments.invalidEffectiveTime")}</p>}
        <Field label={t("deployments.sourceNote")}><textarea value={sourceNote} onChange={(event) => setSourceNote(event.target.value)} placeholder={t("deployments.sourceNotePlaceholder")} /></Field>
        {!validSource && <p className="field-hint error">{t("deployments.sourceEvidenceRequired")}</p>}
        {!validPrice && <p className="field-hint error">{t("deployments.invalidPrice")}</p>}
        <div className="form-actions"><button className="button primary" disabled={!validDetails}>{t("deployments.nextReview")}</button></div>
      </> : <>
        <section className="price-confirm-summary" ref={confirmSummary} tabIndex={-1} aria-live="polite">
          <header><strong>{t("deployments.priceSummary")}</strong><small>{deployment.name} · {deployment.provider_model}</small></header>
          {current && <p>{t("deployments.currentPriceVersion", { version: current.version })}</p>}
          <dl>
            <div><dt>{t("deployments.billingMode")}</dt><dd>{mode === "free" ? t("deployments.freeLabel") : t("deployments.meteredLabel")}</dd></div>
            {mode === "metered" && <><div><dt>{t("deployments.inputUSD")}</dt><dd>{priceCell(previousPrice?.input, input)}</dd></div><div><dt>{t("deployments.cachedInputUSD")}</dt><dd>{priceCell(previousPrice?.cachedInput, cachedInputValue)}</dd></div><div><dt>{t("deployments.outputUSD")}</dt><dd>{priceCell(previousPrice?.output, output)}</dd></div><div><dt>{t("deployments.fixedRequestUSD")}</dt><dd>{priceCell(previousPrice?.fixed, fixed)}</dd></div></>}
            {mode === "metered" && schedule && <div>
              <dt>{t("deployments.priceSchedule")}</dt>
              <dd>
                <span>{t("deployments.scheduleZoneSummary", { timezone: schedule.timezone })}</span>
                <ul className="price-schedule-summary">
                  {schedule.windows.map((window, index) => <li key={index}>
                    {t("deployments.scheduleWindowSummary", { start: window.start, end: window.end, input: window.input, output: window.output })}
                  </li>)}
                  <li>{t("deployments.scheduleBaseSummary", { input, output })}</li>
                </ul>
              </dd>
            </div>}
            <div><dt>{t("deployments.effectiveFrom")}</dt><dd>{effectiveLabel}</dd></div>
            <div><dt>{t("deployments.priceSourceKind")}</dt><dd>{t(`deployments.sourceKinds.${sourceKind === "official_public_price" ? "officialPublicPrice" : sourceKind === "contract_price" ? "contractPrice" : sourceKind === "internal_cost" ? "internalCost" : "temporaryEstimate"}`)}</dd></div>
            <div><dt>{t("deployments.sourceAssurance")}</dt><dd>{t("deployments.assertedSource")}</dd></div>
          </dl>
          <p>{t("deployments.priceExample", { cost: exampleCost.toFixed(6) })}</p>
        </section>
        {/* A version that is already effective when it is written can never
            satisfy the cancellation rule (cancelled_at < effective_from), so
            "immediately" also means "only a later version can undo this". */}
        <div className="notice warning"><strong>{t("deployments.priceWillTakeEffect")}</strong><span>{t("deployments.immutablePriceWarning")}</span>{effectiveMode === "now" && <span>{t("deployments.immediateNotCancellable")}</span>}</div>
        <Field label={t("usage.currentPassword")}><input type="password" autoComplete="current-password" required value={password} onChange={(event) => setPassword(event.target.value)} /></Field>
        <Field label={t("usage.totpOptional")}><input inputMode="numeric" autoComplete="one-time-code" value={totp} onChange={(event) => setTotp(event.target.value)} /></Field>
        {mutation.isError && <div ref={submitError} tabIndex={-1} className="price-submit-error"><ErrorState error={mutation.error} /></div>}
        {ambiguousResult && <p className="field-hint error">{t("deployments.ambiguousPriceRetry")}</p>}
        <div className="form-actions">{!ambiguousResult && <button type="button" className="button ghost" onClick={() => { idempotencyKey.current = crypto.randomUUID(); setStep("details"); }}>{t("deployments.backToPrice")}</button>}<button className="button primary" disabled={mutation.isPending}>{ambiguousResult ? t("deployments.retryExactPrice") : t("deployments.confirmPrice")}</button></div>
      </>}
    </form>
  </Modal>;
}

// The draft keeps the operator's own strings — "09:00", "0.27" — so a half-typed
// window is representable. It becomes minutes and micro-USD only on submit.
export interface ScheduleWindowDraft {
  start: string;
  end: string;
  input: string;
  cachedInput: string;
  output: string;
  fixed: string;
}

export interface ScheduleDraft {
  timezone: string;
  windows: ScheduleWindowDraft[];
}

export function minuteToClock(minute: number) {
  return `${String(Math.floor(minute / 60)).padStart(2, "0")}:${String(minute % 60).padStart(2, "0")}`;
}

// Returns null for anything that is not a whole HH:MM on the clock. "24:00" is
// accepted as the exclusive end of the last window, which is how a span that
// runs to midnight is written without claiming the next day's 00:00.
export function clockToMinute(value: string): number | null {
  const match = /^(\d{2}):(\d{2})$/.exec(value.trim());
  if (!match) return null;
  const [hour, minute] = [Number(match[1]), Number(match[2])];
  if (minute > 59) return null;
  const total = hour * 60 + minute;
  return total <= 24 * 60 ? total : null;
}

function scheduleToDraft(schedule: PriceSchedule): ScheduleDraft {
  return {
    timezone: schedule.timezone,
    windows: schedule.windows.map((window) => ({
      start: minuteToClock(window.start_minute),
      end: minuteToClock(window.end_minute),
      input: priceInputValue(window.input_micros_per_million),
      cachedInput: priceInputValue(window.cached_input_micros_per_million),
      output: priceInputValue(window.output_micros_per_million),
      fixed: priceInputValue(window.fixed_request_micros_usd),
    })),
  };
}

function draftToScheduleRequest(draft: ScheduleDraft) {
  // Sorted here as well as validated: the server stores the table in order and
  // hashes it into every price snapshot, so the order the rows happen to be on
  // screen must not decide the digest.
  const windows = [...draft.windows].sort((left, right) => (clockToMinute(left.start) ?? 0) - (clockToMinute(right.start) ?? 0));
  return {
    timezone: draft.timezone.trim(),
    windows: windows.map((window) => ({
      start: window.start.trim(), end: window.end.trim(),
      input_usd_per_million: window.input, cached_input_usd_per_million: window.cachedInput,
      output_usd_per_million: window.output, fixed_request_usd: window.fixed,
    })),
  };
}

// Mirrors the server's rule table check so the operator is told which row is
// wrong while they can still see it, rather than being handed one rejected
// submission. The server stays the authority; this is not a substitute for it.
export function scheduleDraftProblem(draft: ScheduleDraft): "timezone" | "windows" | "time" | "rate" | "overlap" | undefined {
  if (!draft.timezone.trim()) return "timezone";
  if (!draft.windows.length) return "windows";
  const bounds: Array<[number, number]> = [];
  for (const window of draft.windows) {
    const start = clockToMinute(window.start);
    const end = clockToMinute(window.end);
    if (start === null || end === null || start >= end) return "time";
    if (![window.input, window.cachedInput, window.output, window.fixed].every(validUSD)) return "rate";
    if (![window.input, window.cachedInput, window.output, window.fixed].some((value) => Number(value) > 0)) return "rate";
    bounds.push([start, end]);
  }
  bounds.sort((left, right) => left[0] - right[0]);
  for (let index = 1; index < bounds.length; index += 1) {
    if (bounds[index][0] < bounds[index - 1][1]) return "overlap";
  }
  return undefined;
}

function PriceScheduleFields({ schedule, onChange, problem, baseRates }: {
  schedule: ScheduleDraft | null;
  onChange: (value: ScheduleDraft | null) => void;
  problem?: string;
  baseRates: { input: string; cachedInput: string; output: string; fixed: string };
}) {
  const { t } = useTranslation();
  const update = (index: number, patch: Partial<ScheduleWindowDraft>) => {
    if (!schedule) return;
    onChange({ ...schedule, windows: schedule.windows.map((window, position) => (position === index ? { ...window, ...patch } : window)) });
  };
  return <section className="price-schedule">
    <label className="price-schedule-toggle">
      <input
        type="checkbox"
        checked={!!schedule}
        onChange={(event) => onChange(event.target.checked
          // The zone starts empty and the form blocks until it is filled. It is
          // the provider's zone, and neither this browser's nor the instance's
          // accounting zone is a defensible guess at it — a wrong default here
          // silently shifts every peak hour.
          //
          // A new window copies the rates already on screen, so the operator
          // edits real numbers rather than four empty boxes that would read as
          // free until filled.
          ? { timezone: "", windows: [{ start: "09:00", end: "12:00", ...baseRates }] }
          : null)}
      />
      <span><strong>{t("deployments.priceSchedule")}</strong><small>{t("deployments.priceScheduleHint")}</small></span>
    </label>
    {schedule && <>
      <Field label={t("deployments.scheduleTimezone")} hint={t("deployments.scheduleTimezoneHint")}>
        <input value={schedule.timezone} onChange={(event) => onChange({ ...schedule, timezone: event.target.value })} placeholder="Asia/Shanghai" />
      </Field>
      <table className="price-schedule-table">
        <thead><tr>
          <th scope="col">{t("deployments.scheduleStart")}</th>
          <th scope="col">{t("deployments.scheduleEnd")}</th>
          <th scope="col">{t("deployments.inputUSD")}</th>
          <th scope="col">{t("deployments.cachedInputUSD")}</th>
          <th scope="col">{t("deployments.outputUSD")}</th>
          <th scope="col">{t("deployments.fixedRequestUSD")}</th>
          <th scope="col"><span className="visually-hidden">{t("deployments.scheduleWindowActions")}</span></th>
        </tr></thead>
        <tbody>
          {schedule.windows.map((window, index) => <tr key={index}>
            <td><input value={window.start} onChange={(event) => update(index, { start: event.target.value })} placeholder="09:00" aria-label={t("deployments.scheduleStart")} /></td>
            <td><input value={window.end} onChange={(event) => update(index, { end: event.target.value })} placeholder="12:00" aria-label={t("deployments.scheduleEnd")} /></td>
            <td><input inputMode="decimal" value={window.input} onChange={(event) => update(index, { input: event.target.value })} aria-label={t("deployments.inputUSD")} /></td>
            <td><input inputMode="decimal" value={window.cachedInput} onChange={(event) => update(index, { cachedInput: event.target.value })} aria-label={t("deployments.cachedInputUSD")} /></td>
            <td><input inputMode="decimal" value={window.output} onChange={(event) => update(index, { output: event.target.value })} aria-label={t("deployments.outputUSD")} /></td>
            <td><input inputMode="decimal" value={window.fixed} onChange={(event) => update(index, { fixed: event.target.value })} aria-label={t("deployments.fixedRequestUSD")} /></td>
            <td><button type="button" className="button ghost" onClick={() => onChange({ ...schedule, windows: schedule.windows.filter((_, position) => position !== index) })}>{t("deployments.removeScheduleWindow")}</button></td>
          </tr>)}
        </tbody>
      </table>
      <div className="form-actions">
        <button type="button" className="button secondary" onClick={() => onChange({ ...schedule, windows: [...schedule.windows, { start: "14:00", end: "18:00", ...baseRates }] })}>
          {t("deployments.addScheduleWindow")}
        </button>
      </div>
      {/* Saying which rate covers the rest of the day is the difference between
          a table the operator can reason about and one where the uncovered
          hours are an unmarked hole. */}
      <p className="field-hint">{t("deployments.scheduleBaseHint", { input: baseRates.input, output: baseRates.output })}</p>
      {problem && <p className="field-hint error">{t(`deployments.scheduleProblem.${problem}`)}</p>}
    </>}
  </section>;
}

function validUSD(value: string) {
  return /^(?:0|[1-9]\d*)(?:\.\d{1,6})?$/.test(value) && Number.isFinite(Number(value));
}

function priceInputValue(micros: number) {
  return String(micros / 1_000_000);
}

// A scheduled version whose effective time has already passed by this client's
// clock, while the server still calls it scheduled, is the normal case for a
// browser running slightly ahead or a tab that just woke up. Filtering those
// out and returning false stopped polling for good and froze the row on
// "scheduled", so they get a short retry instead.
export const OVERDUE_SCHEDULED_PRICE_REFRESH_MS = 15_000;

export function scheduledPriceRefreshInterval(prices: DeploymentPriceVersion[] | undefined, now = Date.now()): number | false {
  const scheduled = (prices ?? [])
    .filter((price) => price.status === "scheduled")
    .map((price) => Date.parse(price.effective_from))
    .filter((effective) => Number.isFinite(effective));
  if (!scheduled.length) return false;
  const nextEffective = scheduled.filter((effective) => effective > now).sort((left, right) => left - right)[0];
  if (nextEffective === undefined) return OVERDUE_SCHEDULED_PRICE_REFRESH_MS;
  // Wake just after the server-side boundary. Cap long schedules at one day so
  // clock corrections and resumed browser tabs eventually reconcile as well.
  return Math.min(Math.max(nextEffective - now + 250, 1_000), 24 * 60 * 60 * 1_000);
}

// Rows load their price in batches so opening the page costs a bounded number
// of concurrent reads rather than one per deployment.
export const PRICE_FETCH_BATCH_SIZE = 10;
export const PRICE_FETCH_BATCH_DELAY_MS = 300;

export function priceFetchDelay(listIndex: number): number {
  return Math.floor(Math.max(listIndex, 0) / PRICE_FETCH_BATCH_SIZE) * PRICE_FETCH_BATCH_DELAY_MS;
}

function useDeferredSlot(delayMillis: number): boolean {
  const [ready, setReady] = useState(delayMillis <= 0);
  useEffect(() => {
    if (delayMillis <= 0) {
      setReady(true);
      return;
    }
    const timer = setTimeout(() => setReady(true), delayMillis);
    return () => clearTimeout(timer);
  }, [delayMillis]);
  return ready;
}

function evidenceSummary(evidence: Record<string, string>) {
  const values = [...new Set(Object.values(evidence).filter((value) => value !== "unsupported"))];
  return values;
}

type SelectableBinding = ProviderBinding & { id: string };

// How the capabilities are grouped on screen. Which group a capability belongs
// in is a judgement about the operator's question, not something the server can
// answer, so it stays here — but the set has to stay complete, or a capability
// the server offers would have nowhere to be drawn and would silently vanish
// from this form. DeploymentsPage.test.tsx checks it against what the endpoint
// actually serves.
const deploymentCapabilityGroups = [
  { id: "operations", capabilities: ["chat", "embeddings", "moderations", "images", "transcriptions", "speech", "rerank", "async_generate"] },
  { id: "modalities", capabilities: ["vision"] },
  { id: "protocol", capabilities: ["streaming", "tools", "json_mode", "developer_role", "reasoning", "stream_usage", "provider_executed_tools"] },
  { id: "managed", capabilities: ["files", "batches"] },
] as const;

/** Exported for DeploymentsPage.test.tsx, which checks the grouping against what
 * the provider-profiles endpoint actually serves. */
export const deploymentCapabilityGroupsForTest = deploymentCapabilityGroups;

// One list, derived. It used to be written out a second time above these groups,
// which meant a capability could be in one and not the other.
const deploymentCapabilityNames = deploymentCapabilityGroups.flatMap((group) => group.capabilities);
type DeploymentCapabilityName = typeof deploymentCapabilityGroups[number]["capabilities"][number];

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
  const { notify } = useNotify();
  // Which capabilities cannot stand without chat is the server's rule, and this
  // form used to keep its own copy of the list. A capability added to the rule
  // upstream would have left this form offering a combination the save refuses.
  const capabilityCatalog = useProviderProfiles();
  const catalogReady = capabilityCatalog.isSuccess ? capabilityCatalog.data : undefined;
  const enabledProviders = providers.filter((provider) => provider.enabled || provider.id === source?.provider_id);
  const [name, setName] = useState(current?.name ?? (template ? `${template.name} v2` : ""));
  const [providerID, setProviderID] = useState(source?.provider_id ?? enabledProviders[0]?.id ?? "");
  const [providerModel, setProviderModel] = useState(source?.provider_model ?? "");
  const [bindingID, setBindingID] = useState(source?.binding_id ?? "");
  // The interface the operator picked for capability detection, before any
  // variant or detection result has pinned one.
  const [detectionBinding, setDetectionBinding] = useState(source?.binding_id ?? "");
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
  // "Which interface does this model speak" is the same question detection
  // exists to answer, so it is not asked up front. Detection probes every
  // candidate interface and binds the one that answers. The choice comes back
  // only when several answered — and then it arrives with the evidence.
  const answeringCandidates = capabilityDetection?.status === "ambiguous"
    ? (capabilityDetection.binding_candidates ?? []).filter((candidate) => candidate.answered)
    : [];
  const detectionBindingChoiceRequired = answeringCandidates.length > 1;
  const detectionBindingID = pinnedBinding?.id
    ?? (detectionBindingChoiceRequired
      ? answeringCandidates.find((candidate) => candidate.binding_id === detectionBinding)?.binding_id ?? ""
      : selectableBindings.find((item) => item.id === detectionBinding)?.id ?? "");
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
    mutationFn: () => api.refreshInvocationTargets(providerID),
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
    // Only a completed detection has resolved an interface; an ambiguous one
    // deliberately has not, and must not pin the deployment to anything.
    if (detection.status === "completed" && detection.binding_id) {
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
  // Detection spends the operator's Provider credential upstream, so it asks
  // who is spending it. The step-up material travels with the mutation rather
  // than in component state: it must not outlive the click that supplied it.
  const detectCapabilities = useMutation({
    mutationFn: ({ requestedSelectionRevision, reauth }: { requestedSelectionRevision: string; reauth: ReauthValues }) =>
      api.createModelCapabilityDetection(providerID, {
        provider_model: providerModel.trim(), target_kind: targetKind, region: region.trim(),
        ...(detectionBindingID ? { binding_id: detectionBindingID } : {}), risk_tier: "safe_automatic",
        selection_revision: requestedSelectionRevision,
      }, detectionIdempotencyKey.current, reauth),
    onSuccess: (result, { requestedSelectionRevision }) => {
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
  // Resolving an ambiguous detection re-runs it pinned to the chosen interface,
  // which is the same path an explicitly bound detection already takes. The
  // binding it sends is the one this render resolved, so the request carries
  // the operator's pick rather than whatever a later render computes.
  const confirmDetectionBinding = (reauth: ReauthValues) => {
    const nextSelection = crypto.randomUUID();
    setCapabilityDetection(null);
    appliedDetectionRevision.current = 0;
    detectionIdempotencyKey.current = crypto.randomUUID();
    setSelectionRevision(nextSelection);
    setResolutionRequiresConfirmation(false);
    return detectCapabilities.mutateAsync({ requestedSelectionRevision: nextSelection, reauth });
  };
  const value = () => ({
    name: name.trim(),
    provider_id: providerID,
    ...(bindingID ? { binding_id: bindingID } : {}),
    provider_model: providerModel.trim(),
    ...(canonicalModelRef ? { capability_model: canonicalModelRef } : {}),
    target_kind: targetKind,
    capabilities,
    ...(widening || declaredModel && manualDeclaration ? { mode: "operator_declared" } : {}),
    ...(declaredModel && detection?.status === "completed" ? {
      capability_detection_id: detection.id,
      capability_detection_revision: detection.revision,
    } : {}),
    ...(!current && !manualDeclaration && !detection && selectedVariant ? { resolution_revision: selectedVariant.revision } : {}),
    region: region.trim(),
    max_concurrency: maxConcurrency,
    enabled: current?.enabled ?? false,
  });
  // One key per open form: a retry after a lost response reaches the same
  // record instead of creating a second one, while a deliberate second create
  // opens the form again and gets a new key.
  const idempotencyKey = useRef(crypto.randomUUID());
  const mutation = useMutation({
    mutationFn: () => current
      ? api.updateDeployment(current.id, value(), current.revision)
      : api.createDeployment(value(), idempotencyKey.current),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["deployments"] });
      notify({ tone: "success", title: t(current ? "deployments.notifyUpdated" : "deployments.notifyCreated"), description: name });
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
  //
  // It is also the operator's own claim. An edit locks the invocation identity,
  // so detection cannot run and no variant is re-resolved — nothing but the
  // operator can establish a capability the deployment never recorded, and the
  // server refuses the save unless it carries mode=operator_declared. Ticking
  // the box is that claim; the save commits it under a button that says so.
  // Asking for a separate "I declare" click before the save bought nothing the
  // tick had not already said.
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
  const modelCatalogEnumerable = Boolean(!identityLocked && targetCatalog.data?.discovery.can_enumerate);
  const modelCatalogLoading = targetCatalog.isPending || refreshTargetCatalog.isPending;
  // One flag drives every catalog affordance. Splitting the combobox from the
  // refresh button gave a provider that cannot enumerate a dropdown arrow and a
  // refresh control while the catalog was loading or had failed, and both led
  // nowhere. A provider whose discovery says it cannot enumerate gets neither.
  const showModelCatalogControls = Boolean(
    !identityLocked
    && providerID
    && (modelCatalogEnumerable || targetCatalog.isPending || targetCatalog.isError || refreshTargetCatalog.isPending || refreshTargetCatalog.isError),
  );
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
    if (!showModelCatalogControls) return;
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
  const modelResolutionMessage = canonicalModelRef
    ? t("deployments.capabilityModelApplied")
    : "";
  const regional = selectedProvider?.access_surface === "bedrock-runtime" || selectedProvider?.access_surface === "bedrock-agent-runtime";
  const fixedPriced = capabilities.moderations || capabilities.images || capabilities.transcriptions || capabilities.speech || capabilities.files || capabilities.batches || capabilities.rerank || capabilities.async_generate;
  const anyOperation = capabilities.chat || capabilities.embeddings || fixedPriced;
  const numericValues = [maxConcurrency, capabilities.max_context_tokens, capabilities.max_output_tokens];
  const tokenLimitsValid = capabilities.max_context_tokens === 0 || capabilities.max_output_tokens <= capabilities.max_context_tokens;
  const resolutionReady = Boolean(current || selectedVariant || detection?.status === "completed" && anyOperation || manualDeclaration && bindingID);
  const limitsValid = numericValues.every((value) => Number.isFinite(value) && value >= 0) && tokenLimitsValid;
  // A disabled save button beside a blank margin is the same as no button at
  // all: seven separate conditions collapse into one boolean, and the operator
  // is left guessing which of them is unmet. Name each one that still is.
  const saveBlockers: string[] = [];
  if (!name.trim()) saveBlockers.push("name");
  if (!providerID || !providerModel.trim()) {
    // Everything downstream of the model is unanswerable until it exists;
    // listing those too would bury the one step that is actually next.
    saveBlockers.push("model");
  } else {
    if (!resolutionReady) {
      if (manualDeclaration && !bindingID) saveBlockers.push("interface");
      else if (declaredModel && detection?.status !== "completed") saveBlockers.push("detection");
      else saveBlockers.push("resolution");
    }
    if (!anyOperation) saveBlockers.push("operation");
  }
  if (!limitsValid) saveBlockers.push("limits");
  const formValid = saveBlockers.length === 0;
  // Capabilities a probe could not settle. "not probed" is only one of these
  // when the plan meant to reach it — the risk policy leaves everything outside
  // the safe-automatic set unprobed by design, and reporting that as an open
  // question would make every detection look incomplete.
  const unestablishedCapabilities = detection?.status === "completed"
    ? deploymentCapabilityNames.filter((name) => {
      const result = detection.capabilities[name];
      if (!result) return false;
      return result.status === "inconclusive" || result.status === "not_probed" && result.probe_kind !== "risk_policy";
    })
    : [];
  // What the save will record, not what the form happens to display: widening
  // sends mode=operator_declared, so the source shown has to say so too.
  const capabilityEvidenceSource = manualDeclaration || widening
    ? "operator_declared"
    : detection?.status === "completed" ? detection.source : "";
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
                setDetectionBinding("");
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
              <div className="deployment-model-input-row">
                <div className={`deployment-model-input-shell ${modelPickerOpen ? "open" : ""}`}>
                  <input
                    id="deployment-provider-model-id"
                    required
                    disabled={identityLocked}
                    role={showModelCatalogControls ? "combobox" : undefined}
                    aria-autocomplete={showModelCatalogControls ? "list" : undefined}
                    aria-expanded={showModelCatalogControls ? modelPickerOpen : undefined}
                    aria-controls={showModelCatalogControls ? "deployment-provider-model-options" : undefined}
                    aria-activedescendant={modelPickerOpen && activeModelIndex >= 0 ? `deployment-provider-model-option-${activeModelIndex}` : undefined}
                    value={providerModel}
                    onFocus={() => { if (showModelCatalogControls) setModelPickerOpen(true); }}
                    onClick={() => { if (showModelCatalogControls) setModelPickerOpen(true); }}
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
                  {showModelCatalogControls && <span className="deployment-model-input-icon" aria-hidden="true" />}
                  {showModelCatalogControls && modelPickerOpen && (
                    <div className="deployment-model-options" id="deployment-provider-model-options" ref={modelOptionsRef} role="listbox" aria-label={t("deployments.modelCatalogLabel")}>
                      <div className="deployment-model-options-meta" role="presentation">
                        {targetCatalog.isPending
                          ? t("deployments.modelCatalogLoading")
                          : targetCatalog.isError || refreshTargetCatalog.isError
                            ? t("deployments.modelCatalogUnavailable")
                            : <>{t("deployments.modelCatalogCountPrefix")} <strong>{targetCatalog.data?.items?.length ?? 0}</strong> {t("deployments.modelCatalogCountSuffix")}</>}
                      </div>
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
                {showModelCatalogControls && <button
                  className="button secondary deployment-model-refresh"
                  type="button"
                  disabled={modelCatalogLoading}
                  aria-busy={modelCatalogLoading}
                  onClick={() => refreshTargetCatalog.mutate()}
                >
                  {modelCatalogLoading && <span className="deployment-model-refresh-spinner" aria-hidden="true" />}
                  <span>{modelCatalogLoading ? t("deployments.refreshingModels") : t("deployments.refreshModels")}</span>
                </button>}
              </div>
              <small>{t(`deployments.targetHints.${targetKind}`)}</small>
            </div>
            {!identityLocked && providerModel.trim() !== "" && modelResolutionMessage && (
              <div className="deployment-model-declaration" role="status">
                {modelResolutionMessage}
              </div>
            )}
            {modelCatalogEnumerable && targetCatalog.data?.degraded_bindings?.length ? (
              <div className="deployment-model-degraded" role="status">
                {t("deployments.modelCatalogPartial", { count: targetCatalog.data.degraded_bindings.length })}
              </div>
            ) : null}
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
              {catalogReady && <CapabilitySubsetEditor catalog={catalogReady} capabilities={capabilities} ceiling={selectedVariant.capabilities} onChange={changeCapabilities} />}
              {!catalogReady && !capabilityCatalog.isError && <Loading />}
              {capabilityCatalog.isError && <CapabilityMatrixError query={capabilityCatalog} />}
            </details>}
            {!current && noVariant && <div className="notice warning"><strong>{t("deployments.noVariantTitle")}</strong><span>{t("deployments.noVariantDescription")}</span><Link className="notice-link" href="/admin/providers">{t("deployments.openProviders")}</Link></div>}
            {!current && resolvedTarget?.resolution_state === "conflicting" && <div className="notice warning"><strong>{t("deployments.resolutionConflictTitle")}</strong><span>{t("deployments.resolutionConflictDescription")}</span></div>}
            {!current && declaredModel && !manualDeclaration && (
              <div className="capability-detection-panel" aria-live="polite">
                {!detection && <>
                  <div className="capability-onboarding-card">
                    <header>
                      <div>
                        <strong>{t("deployments.detectionUnknownTitle")}</strong>
                        <span>{t("deployments.detectionCostBoundary", { count: targetCatalog.data?.discovery.max_verification_calls ?? 0 })}</span>
                      </div>
                      <span className="capability-onboarding-status">{t("deployments.detectionRequired")}</span>
                    </header>
                    <div className="capability-onboarding-controls">
                      {selectableBindings.length > 1 && <p className="capability-advanced-note">{t("deployments.detectionResolvesInterface")}</p>}
                      <div className="form-actions">
                        <button type="button" className="button ghost" onClick={() => { setManualDeclaration(true); setCapabilities(emptyCapabilities()); }}>{t("deployments.advancedManualDeclaration")}</button>
                        <ConfirmButton className="button primary" label={detectCapabilities.isPending ? t("common.working") : t("deployments.confirmAndDetect")} title={t("deployments.confirmAndDetect")} confirmLabel={t("deployments.detectionSpendConfirm")} disabled={!providerModel.trim() || !targetCatalog.data?.discovery.can_verify || detectCapabilities.isPending} requireStepUp onConfirm={(reauth) => detectCapabilities.mutateAsync({ requestedSelectionRevision: selectionRevision, reauth })} />
                      </div>
                    </div>
                  </div>
                </>}
                {detection && (detection.status === "queued" || detection.status === "running") && <>
                  <div className="notice"><strong>{t("deployments.detectingCapabilities", { completed: Object.values(detection.capabilities).filter((result) => result.status !== "not_probed").length, total: detection.max_provider_calls })}</strong><span>{t("deployments.detectionCancelCost")}</span></div>
                  <button type="button" className="button ghost" disabled={cancelDetection.isPending} onClick={() => cancelDetection.mutate()}>{t("deployments.cancelDetection")}</button>
                </>}
                {detection?.status === "ambiguous" && <div className="capability-onboarding-card">
                  <header>
                    <div>
                      <strong>{t("deployments.detectionAmbiguousTitle")}</strong>
                      <span>{t("deployments.detectionAmbiguousDescription")}</span>
                    </div>
                  </header>
                  <div className="capability-onboarding-controls">
                    <Field label={t("deployments.detectionBindingLabel")} hint={t("deployments.detectionBindingHint")}>
                      <select value={detectionBindingID} onChange={(event) => setDetectionBinding(event.target.value)}>
                        <option value="">{t("deployments.interfaceRequired")}</option>
                        {answeringCandidates.map((candidate) => {
                          const binding = selectableBindings.find((item) => item.id === candidate.binding_id);
                          const label = binding ? bindingLabel(binding, t) : candidate.profile_id;
                          return <option value={candidate.binding_id} key={candidate.binding_id}>
                            {t("deployments.detectionCandidateOption", { interface: label, capability: t(`capabilities.${candidate.capability}`) })}
                          </option>;
                        })}
                      </select>
                    </Field>
                    <div className="form-actions">
                      <button type="button" className="button ghost" onClick={() => { resetDetection(); setManualDeclaration(true); }}>{t("deployments.advancedManualDeclaration")}</button>
                      <ConfirmButton className="button primary" label={detectCapabilities.isPending ? t("common.working") : t("deployments.confirmDetectionBinding")} title={t("deployments.confirmDetectionBinding")} confirmLabel={t("deployments.detectionSpendConfirm")} disabled={!detectionBindingID || detectCapabilities.isPending} requireStepUp onConfirm={confirmDetectionBinding} />
                    </div>
                  </div>
                </div>}
                {detection?.status === "completed" && !anyOperation && detectionNeedsProviderRepair && <div className="notice warning"><strong>{t("deployments.detectionProviderRepairTitle")}</strong><span>{t("deployments.detectionProviderRepairDescription")}</span><Link className="notice-link" href="/admin/providers">{t("deployments.openProviders")}</Link></div>}
                {detection?.status === "completed" && !anyOperation && !detectionNeedsProviderRepair && <div className="notice warning"><strong>{t("deployments.detectionInconclusiveTitle")}</strong><span>{t("deployments.detectionInconclusiveDescription")}</span><div className="form-actions"><button type="button" className="button ghost" onClick={() => { resetDetection(); setManualDeclaration(true); }}>{t("deployments.advancedManualDeclaration")}</button></div></div>}
                {/* A failure is not "the model is broken". Detection can only
                    ask what its plan has probes for, so a model whose real work
                    is generating images or transcribing audio is asked text
                    questions it cannot answer by construction. Name what each
                    interface was asked, what came back, and what it could have
                    established at all, so a rejected credential is not read as
                    a model that refused. */}
                {detection && ["failed", "canceled", "interrupted"].includes(detection.status) && <div className="notice warning">
                  <strong>{t(`deployments.detectionStatus.${detection.status}`)}</strong>
                  <span>{t("deployments.detectionUnverifiableDescription")}</span>
                  {detection.status === "failed" && !!detection.binding_candidates?.length && <ul className="detection-candidate-outcomes">
                    {detection.binding_candidates.map((candidate) => {
                      const binding = selectableBindings.find((item) => item.id === candidate.binding_id);
                      return <li key={candidate.binding_id}>
                        <strong>{binding ? bindingLabel(binding, t) : candidate.profile_id}</strong>
                        <span>{candidate.capability
                          ? t("deployments.detectionCandidateOutcome", {
                            capability: t(`capabilities.${candidate.capability}`),
                            status: t(`deployments.detectionProbeStatus.${candidate.status}`),
                          })
                          : t("deployments.detectionCandidateNotAsked")}</span>
                        <small>{t("deployments.detectionCandidateVerifiable", {
                          capabilities: (candidate.verifiable ?? []).map((name) => t(`capabilities.${name}`)).join("、") || t("deployments.detectionCandidateVerifiableNone"),
                        })}</small>
                      </li>;
                    })}
                  </ul>}
                  <div className="form-actions"><button type="button" className="button ghost" onClick={() => { resetDetection(); setManualDeclaration(true); }}>{t("deployments.advancedManualDeclaration")}</button></div>
                </div>}
                {/* What a model cannot do is the complement of the capability
                    editor below, so listing it twice only adds rows nobody acts
                    on. What a probe failed to establish is not that complement:
                    it is the one outcome the editor cannot express, so it is
                    the only one still named here. */}
                {!!unestablishedCapabilities.length && <div className="notice" role="status">
                  <strong>{t("deployments.detectionUnestablishedTitle", {
                    capabilities: unestablishedCapabilities.map((name) => t(`capabilities.${name}`)).join("、"),
                  })}</strong>
                  <span>{t("deployments.detectionUnestablishedDescription")}</span>
                </div>}
                {detection?.status === "completed" && detection.expires_at && <small className="technical detection-freshness">{t("deployments.detectionFreshUntil", { date: dateTime(detection.expires_at) })}</small>}
                {(detectCapabilities.isError || detectionQuery.isError || cancelDetection.isError) && <ErrorState error={detectCapabilities.error || detectionQuery.error || cancelDetection.error} />}
              </div>
            )}
            {/* The checkboxes enforce the server's chat dependency, which arrives
                with the matrix; without it the form would be guessing at which
                combinations save. */}
            {providerModel.trim() !== "" && (current || manualDeclaration || detection?.status === "completed" && anyOperation) && capabilityCatalog.isError && (
              <CapabilityMatrixError query={capabilityCatalog} />
            )}
            {providerModel.trim() !== "" && catalogReady && (current || manualDeclaration || detection?.status === "completed" && anyOperation) && <div className="capability-disclosure capability-advanced">
              <header>
                <span>{t("deployments.enabledCapabilities")}</span>
                <div className="deployment-capability-header-actions">
                  {/* Evidence tier stays on screen. A verified set and a set an
                      administrator simply asserted must never look alike. */}
                  {capabilityEvidenceSource && <small className="capability-evidence-source">{t(`deployments.capabilityEvidenceSources.${capabilityEvidenceSource}`)}</small>}
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
                      <strong id={`capability-group-${group.id}`}>{t(`deployments.capabilityGroups.${group.id}.title`)}</strong>
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
                            onChange={(event) => changeCapabilities(updateCapabilitySelection(catalogReady, capabilities, name, event.target.checked))}
                          />
                          <span>{t(`capabilities.${name}`)}{unavailable && <small>{t("providers.unsupportedByInterface")}</small>}</span>
                        </label>;
                      })}
                    </div>
                  </section>;
                })}
              </fieldset>
            </div>}
            {widening && <div className="notice warning deployment-capability-declaration" aria-live="polite">
              <strong>{t("deployments.widenDeclarationTitle")}</strong>
              <span>{t("deployments.widenDeclarationDescription")}</span>
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
            <header>
              <div><h3 id="deployment-limits-heading">{t("deployments.limitSection")}</h3><p>{t("deployments.limitSectionDescription")}</p></div>
            </header>
            <div>
              <div className="deployment-limit-grid">
                {/* "0 is automatic" said nothing about who then enforces the
                    limit. Zero means the deployment declares nothing and the
                    upstream ceiling still applies, which is a billing and
                    throttling fact, so each field states it. */}
                <Field label={t("deployments.maxContext")} hint={t("deployments.maxContextHint")}>
                  <input min="0" type="number" value={capabilities.max_context_tokens} onChange={(event) => setCapabilities({ ...capabilities, max_context_tokens: Number(event.target.value) })} />
                </Field>
                <Field label={t("deployments.maxOutputTokens")} hint={t("deployments.maxOutputHint")}>
                  <input min="0" type="number" value={capabilities.max_output_tokens} onChange={(event) => setCapabilities({ ...capabilities, max_output_tokens: Number(event.target.value) })} />
                </Field>
                <Field label={t("deployments.concurrencyLimit")} hint={t("deployments.concurrencyHint")}><input min="0" type="number" value={maxConcurrency} onChange={(event) => setMaxConcurrency(Number(event.target.value))} /></Field>
              </div>
              {!tokenLimitsValid && <div className="notice warning"><span>{t("deployments.tokenLimitInvalid")}</span></div>}
            </div>
          </section>
          {impact && <CapabilityImpactNotice impact={impact} />}
          {mutation.isError && mutation.error instanceof ApiError && mutation.error.code === "resolution_changed"
            ? <div className="notice warning"><strong>{t("deployments.resolutionChangedTitle")}</strong><span>{t("deployments.resolutionChangedDescription")}</span></div>
            : (mutation.isError || preflight.isError) && <ErrorState error={mutation.error || preflight.error} />}
          {/* Saving an existing deployment hot-reloads it; saving a new one
              cannot enable it. Both consequences belong in the bar that commits
              them, beside what is still stopping the save.

              A widening replaces that line rather than adding a second notice
              above the bar: it does not hot-reload into service, it drops the
              deployment to disabled, and while the deployment is routed the
              server refuses the save outright
              (capability_expansion_requires_revalidation). The instruction that
              gets past it — disable the routes first — has nowhere else to
              live, so it belongs on the bar that would otherwise 409. */}
          <div className="form-actions sticky-form-actions deployment-form-actions">
            <div className="form-footer-state">
              <div className="form-footer-summary">
                <strong>{widening ? t("deployments.expansionNeedsRevalidation") : current ? t("deployments.updateLiveWarning") : t("deployments.savedDisabled")}</strong>
                <small>{widening
                  ? current?.enabled ? t("deployments.expansionWhileRouted") : t("deployments.expansionSavedDisabled")
                  : current ? t("deployments.updateLiveDescription") : t("deployments.savedDisabledDescription")}</small>
              </div>
              {!!saveBlockers.length && <div className="form-footer-summary" role="status">
                <strong>{t("deployments.saveBlocked")}</strong>
                <small>{saveBlockers.map((blocker) => t(`deployments.saveBlockers.${blocker}`)).join(" · ")}</small>
              </div>}
            </div>
            <button type="button" className="button ghost" onClick={onClose}>{t("common.cancel")}</button>
            <button className={`button ${impact?.blocking ? "danger" : "primary"}`} disabled={mutation.isPending || preflight.isPending || !formValid}>
              {preflight.isPending
                ? t("deployments.checkingRouteImpact")
                : impact
                  ? t("deployments.saveDespiteImpact")
                  : narrowing
                    ? t("deployments.checkRouteImpact")
                    // The commit is where the declaration is made, so the button
                    // has to name it rather than read as an ordinary save.
                    : widening ? t("deployments.saveWithDeclaration")
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
    <span className="deployment-capability-summary-icon" aria-hidden="true">✓</span>
    <div className="deployment-capability-summary-copy">
      <strong>{t("deployments.capabilityReady", { count: names.length })}</strong>
      {sourceSummary && <small>{sourceSummary}</small>}
    </div>
    <div className="deployment-capability-summary-tags">
      {names.map((name) => <span key={name}>{t(`capabilities.${name}`)}</span>)}
    </div>
  </div>;
}

// The capability matrix failed to load, so the checkboxes cannot be drawn: they
// enforce the server's chat dependency, and a form guessing at that offers
// combinations the save refuses. Failing closed is the point; the retry is what
// keeps it from meaning "reload the page and hope".
function CapabilityMatrixError({ query }: { query: ReturnType<typeof useProviderProfiles> }) {
  const { t } = useTranslation();
  return <ErrorState
    error={query.error}
    action={<button type="button" className="button ghost" disabled={query.isFetching} onClick={() => query.refetch()}>{t("common.retry")}</button>}
  />;
}

function CapabilitySubsetEditor({ catalog, capabilities, ceiling, onChange }: {
  catalog: ProviderProfilesCatalog;
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
              onChange={(event) => onChange(updateCapabilitySelection(catalog, capabilities, name, event.target.checked))}
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
    provider_executed_tools: false,
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

