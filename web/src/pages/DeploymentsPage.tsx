import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useId, useMemo, useRef, useState, type FormEvent, type KeyboardEvent } from "react";
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
  TimeZoneField,
  useDirty,
  useTestFailureReason,
  type ReauthValues,
} from "../components";
import { exactNumber, formatAge, money, useInstantFormatter } from "../format";
import { isoToZonedInput, useAccountingTimeZone, zonedInputToISO } from "../timezone";
import type { CapabilityPreflight, CapabilityReview, Deployment, DeploymentPriceVersion, DeploymentTargetKind, DeploymentVariant, ModelCapabilityDetection, PriceSchedule, Provider, ProviderBinding, ProviderCapabilities, ProviderProfilesCatalog, ResolutionState, ResolvedInvocationTarget } from "../types";
import { interfaceCeiling, updateCapabilitySelection, useProviderProfiles } from "../hooks/useProviderProfiles";
import { ModalityMarks } from "../ModalityMarks";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { useNotify } from "../notifications";
import { useIsReadOnly } from "../session";
import { Link } from "../navigation";
import { hasOnboardingCreateIntent, OnboardingContextBanner } from "../OnboardingContext";
import { deploymentCondition, deploymentNeedsAttention, recordedTestState } from "./deploymentCondition";
import { localizedError } from "../i18n/errors";

export function DeploymentsPage() {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const [editing, setEditing] = useState<Deployment | null | "new">(() => !readOnly && hasOnboardingCreateIntent() ? "new" : null);
  const [replacement, setReplacement] = useState<Deployment>();
  const [query, setQuery] = useState(() => new URLSearchParams(window.location.search).get("q") ?? "");
  const [status, setStatus] = useState<"all" | "enabled" | "disabled" | "attention">("all");
  // What the grid's single live region is currently saying. Cards publish here
  // rather than each owning a region of its own.
  const [announcement, setAnnouncement] = useState("");
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
      const matchesStatus = status === "all"
        || (status === "enabled" && deployment.enabled)
        || (status === "disabled" && !deployment.enabled)
        || (status === "attention" && deploymentNeedsAttention(deployment));
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
              <input autoComplete="off" value={query} onChange={(event) => setQuery(event.target.value)} placeholder={t("deployments.searchPlaceholder")} />
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
            <div className="resource-card-grid deployment-grid">
              {filteredDeployments.map((deployment, index) => (
                <DeploymentRow
                  key={deployment.id}
                  deployment={deployment}
                  listIndex={index}
                  providerName={providerNames.get(deployment.provider_id) || deployment.provider_id}
                  activeRouteCount={activeRouteCounts.get(deployment.id) ?? 0}
                  onEdit={() => setEditing(deployment)}
                  onReplace={() => { setReplacement(deployment); setEditing("new"); }}
                  onAnnounce={setAnnouncement}
                />
              ))}
            </div>
          ) : <EmptyState title={t("deployments.noMatches")}>{t("deployments.noMatchesDescription")}</EmptyState>}
          {/* One live region for the whole grid, naming the deployment it is
              speaking about. A region per card is fifty registered regions that
              announce a bare "失败" with no way to tell which card produced
              it — and announce again whenever a filter keystroke remounts
              them. */}
          <p className="sr-only" role="status" aria-live="polite">{announcement}</p>
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

// Token counts on a card are read at a glance, so they are abbreviated the way
// every model catalogue abbreviates them. The exact figure stays available in
// the details drawer, which is where an operator goes to check one.
function tokenCount(tokens: number) {
  if (tokens >= 1_000_000) return `${Math.round(tokens / 100_000) / 10}M`;
  if (tokens >= 1_000) return `${Math.round(tokens / 1_000)}K`;
  return String(tokens);
}

/*
 * Enable and disable, as one button in the card's action bar.
 *
 * The button is named for what a click does, not for the state it leaves — the
 * state is the word in the card's head, which reports and does not act. One
 * button rather than two, because only one of the pair is ever available and a
 * greyed-out twin beside it says nothing the label does not.
 *
 * Neither direction confirms. Disabling is refused outright while any enabled
 * route still points at the deployment (see routeBlocked), so what remains
 * reachable here is a deployment nothing routes through, and one click undoes
 * one click. Enabling wants a test of the current revision, and a blocked
 * button states why in the accessibility tree as well as its tooltip, because
 * a disabled button carries no tooltip in some browsers and is skipped in tab
 * order.
 */
function DeploymentStateToggle({ name, enabled, pending, blockedReason, onToggle }: {
  name: string;
  enabled: boolean;
  pending: boolean;
  blockedReason?: string;
  onToggle: () => void;
}) {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const reasonID = useId();
  const reason = readOnly ? t("navigation.readOnlyAction") : blockedReason;
  const blocked = Boolean(reason);
  return (
    <>
      <button
        type="button"
        className="button ghost"
        disabled={blocked || pending}
        title={blocked ? reason : undefined}
        aria-describedby={blocked ? reasonID : undefined}
        aria-label={t(enabled ? "deployments.disableDeployment" : "deployments.enableDeployment", { name })}
        onClick={onToggle}
      >{enabled ? t("common.disable") : t("common.enable")}</button>
      {blocked && <span id={reasonID} className="sr-only">{reason}</span>}
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
  onAnnounce,
}: {
  deployment: Deployment;
  listIndex: number;
  providerName: string;
  activeRouteCount: number;
  onEdit: () => void;
  onReplace: () => void;
  /** Publishes into the page's single live region — see the card's test effect. */
  onAnnounce: (message: string) => void;
}) {
  const { t } = useTranslation();
  const dateTime = useInstantFormatter();
  const readOnly = useIsReadOnly();
  const nameID = useId();
  const conditionID = useId();
  const [details, setDetails] = useState(false);
  const [pricing, setPricing] = useState(false);
	const [confirmingRestore, setConfirmingRestore] = useState(false);
  const queryClient = useQueryClient();
  // The card's price fact is worth having, but a fifty-card page must not turn
  // into fifty simultaneous price reads — each one runs a lifecycle derivation
  // server-side. Cards load in bounded batches instead, and the card whose
  // details are open jumps the queue because the operator is looking at it.
  const priceSlotReady = useDeferredSlot(details ? 0 : priceFetchDelay(listIndex));
  // Cached with staleTime Infinity and shared across every card, so asking per
  // card costs one fetch for the page rather than one per deployment.
  const profiles = useProviderProfiles();
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
  const capabilities = Object.entries(deployment.capabilities)
    .filter(([, enabled]) => typeof enabled === "boolean" && enabled)
    .map(([name]) => name);
  const recordedTest = recordedTestState(deployment);
  const testIsCurrent = recordedTest === "success";
  const testFailed = recordedTest === "failure";
  const testState = test.isPending
    ? "running"
    // A test that never reached the store — a 409 on a revision that moved, a
    // transport failure — leaves `last_test_status` describing an older run, so
    // reading only the record would report the previous verdict for a request
    // that just failed.
    : test.isError
      ? "failure"
      : recordedTest;
  const testFailureReason = useTestFailureReason(test.error, testFailed ? deployment.last_test_error_class : undefined);
  const probe = deployment.probe;
  const probeReason = useTestFailureReason(undefined, probe?.state === "unhealthy" ? probe.error_class : undefined);
  const review = deployment.capability_review;
  const routeBlocked = activeRouteCount > 0;
  const testVerdict = testState === "success" && deployment.last_test_latency_millis !== undefined
    ? t("testControl.success", { latency: deployment.last_test_latency_millis })
    : testState === "success" ? t("testControl.successPlain") : t(`testControl.${testState}`);
  // The card says a price is missing. While the read is still in flight nothing
  // is missing yet, and saying so then would put a condition on every card for
  // as long as the batch takes and take it back again.
  const priceNeedsAttention = !prices.isPending && !activePrice;
  // The card's one line about itself: the worst thing true of this deployment,
  // and how much it outranks. See deploymentCondition.ts for the ladder.
  const condition = deploymentCondition({
    deployment, testState, priceMissing: priceNeedsAttention, priceUnknown: prices.isError, activeRouteCount,
  });
  const conditionAge = formatAge(condition.observedAt);
  // A classified reason is a sentence, and the line is one clipped row: the
  // reason is still on the card, starting where the operator is already
  // reading, and the tooltip carries the part the width cut off. The drawer
  // keeps the whole of it either way.
  const conditionDetail = condition.key === "testControl.failure"
    ? testFailureReason
    : condition.key === "deployments.probeUnhealthyShort"
      ? probeReason
      : condition.key === "deployments.pricingQuarantinedShort"
        ? deployment.pricing_quarantine_reason
        : undefined;
  const conditionText = [t(condition.key, condition.params ?? {}), conditionDetail, conditionAge]
    .filter(Boolean).join(t("common.dotSeparator"));
  // A read-only session reads the condition and acts on none of it, so the line
  // stays text rather than offering a control the server would refuse.
  const conditionAct = readOnly
    ? undefined
    : condition.action === "price"
      ? () => setPricing(true)
      : condition.action === "restorePricing"
        ? () => setConfirmingRestore(true)
        : condition.action === "retryPrice"
          ? () => { void prices.refetch(); }
          : condition.action === "drawer"
            ? () => setDetails(true)
            : undefined;
  // One line of machine facts, in the order an operator checks them. It clips
  // rather than wraps: a band that can become two lines is what made every card
  // in a row a different height.
  const tokenSpec = [
    deployment.capabilities.max_context_tokens ? tokenCount(deployment.capabilities.max_context_tokens) : "",
    deployment.capabilities.max_output_tokens ? tokenCount(deployment.capabilities.max_output_tokens) : "",
  ].filter(Boolean).join("/");
  // A verdict that arrives while nothing announces it is a click with no
  // answer. The page owns one live region for the whole grid — fifty cards each
  // owning one announced anonymously, and announced again on every filter
  // keystroke that remounted them.
  const settledTest = useRef(testState);
  useEffect(() => {
    if (settledTest.current === "running" && testState !== "running") {
      onAnnounce(t("deployments.testAnnouncement", { name: deployment.name, verdict: conditionText }));
    }
    settledTest.current = testState;
  }, [testState, conditionText, deployment.name, onAnnounce, t]);
  // Enable, disable and delete report through the notification channel their
  // successes already use. A failed mutation is a transient answer to a click,
  // not a property of the deployment, and rendering it inside the card put a
  // block below the action bar that broke the one alignment the grid keeps.
  const stateError = state.error;
  const removeError = remove.error;
  useEffect(() => {
    if (stateError) notify({ tone: "error", title: t("common.requestFailed"), description: localizedError(t, stateError) });
  }, [stateError, notify, t]);
  useEffect(() => {
    if (removeError) notify({ tone: "error", title: t("common.requestFailed"), description: localizedError(t, removeError) });
  }, [removeError, notify, t]);
  // Newest first, and cancelled versions stay out: they never took effect and
  // never will, so they are not part of what this deployment charged.
  const evidenceOf = (capability: string) => deployment.capability_evidence[capability] ?? "unrecorded";
  const evidenceKinds = new Set(capabilities.map(evidenceOf));
  const uniformEvidence = evidenceKinds.size === 1 ? [...evidenceKinds][0] : undefined;
  const priceTimeline = priceItems
    .filter((price) => price.status !== "cancelled")
    .slice()
    .sort((first, second) => second.effective_from.localeCompare(first.effective_from));
  return (
    <article id={`deployment-${deployment.id}`} className="resource-card" aria-labelledby={nameID}>
      {/* Slot 1 — who this is. Two lines, always, so slot 2 starts at the same
          height on every card in the row. */}
      <div className="resource-card-head">
        <div className="resource-identity">
          {/* A heading, so a screen reader can walk the grid card by card, and
              the article's accessible name. Thirty nameless articles were
              thirty stops that announced nothing. */}
          <h3 id={nameID}>{deployment.name}</h3>
          {/* Provider and upstream model are one identity, not two lines: which
              connection, and which model on it. The label that names the mono
              string is real text — aria-label on a <code> has no role to attach
              to and browsers drop it. */}
          <small>
            {providerName}
            {" · "}
            <span className="sr-only">{t("deployments.upstreamTarget")}{t("common.labelSeparator")}</span>
            <code title={deployment.provider_model}>{deployment.provider_model}</code>
          </small>
        </div>
        {/* The actions that are neither daily nor undoable. The state word used
            to sit here too; it reads better at the foot beside the control that
            changes it — see the action bar. */}
        <div className="resource-row-state deployment-card-state">
          <OverflowMenu label={t("deployments.moreActionsFor", { name: deployment.name })}>
            <button className="button ghost" disabled={readOnly} title={readOnly ? t("navigation.readOnlyAction") : undefined} onClick={onReplace}>{t("deployments.createReplacement")}</button>
            <ConfirmButton label={t("common.delete")} confirmLabel={t("deployments.deleteConfirm", { name: deployment.name })} requireStepUp onConfirm={(reauth) => remove.mutateAsync(reauth)} disabled={remove.isPending || routeBlocked} disabledReason={routeBlocked ? t("deployments.routeBlocked") : undefined} />
          </OverflowMenu>
        </div>
      </div>
      {/* Slot 2 — the worst thing true of this deployment, and the control that
          changes the answer. One line whatever is wrong, so the card's verdict
          is always at the same height and the dot is the only coloured mark on
          the card. What it outranks is counted, never dropped. */}
      <div className="deployment-condition" data-severity={condition.severity}>
        <span className="deployment-condition-dot" aria-hidden="true" />
        {conditionAct ? (
          <button type="button" className="deployment-condition-text" id={conditionID} title={conditionText} onClick={conditionAct}>{conditionText}</button>
        ) : (
          <span className="deployment-condition-text" id={conditionID} title={conditionText}>{conditionText}</span>
        )}
        {condition.suppressed > 0 && (
          <button
            type="button"
            className="deployment-condition-more"
            aria-label={t("deployments.moreConditions", { count: condition.suppressed, name: deployment.name })}
            onClick={() => setDetails(true)}
          >+{condition.suppressed}</button>
        )}
        <button
          type="button"
          className="button ghost deployment-condition-test"
          // The button reports its own last verdict, not the card's condition:
          // a probe failure can outrank a manual test that passed, and tinting
          // the control red for something it did not measure would say the test
          // failed when it did not.
          data-test-state={testState}
          disabled={readOnly || testState === "running"}
          title={readOnly ? t("navigation.readOnlyAction") : undefined}
          aria-describedby={conditionID}
          aria-label={t("deployments.testDeployment", { name: deployment.name })}
          onClick={() => test.mutate()}
        >{t("common.test")}</button>
      </div>
      {/* Slot 3 — what this model is, on one clipped line. The exact figures are
          in the drawer; concurrency is last because it is the first thing a
          narrow card should give up. */}
      <p className="deployment-spec">
        <ModalityMarks catalog={profiles.data} capabilities={deployment.capabilities} evidence={deployment.capability_evidence} />
        <span>{t("deployments.capabilityCount", { count: capabilities.length })}</span>
        {tokenSpec && <span>{tokenSpec}</span>}
        {deployment.max_concurrency > 0 && <span>{t("deployments.concurrencyCompact", { count: deployment.max_concurrency })}</span>}
      </p>
      {/* Slot 4 — what an operator does to a deployment they are working on.
          ConfirmButton gates itself; these do not, so a read-only session was
          shown controls that would 403 on click. §7.3 also requires a disabled
          control to say why, so each carries the reason. */}
      <div className="resource-card-actions row-actions deployment-compact-actions">
        <button className="button ghost" disabled={readOnly} title={readOnly ? t("navigation.readOnlyAction") : undefined} aria-label={t("deployments.editDeployment", { name: deployment.name })} onClick={onEdit}>{t("common.edit")}</button>
        {/* A dialog, not a disclosure: the label never changes and the card
            never resizes, so aria-expanded would describe a state the card no
            longer has. */}
        <button className="button ghost" aria-haspopup="dialog" aria-label={t("deployments.viewDeploymentDetails", { name: deployment.name })} onClick={() => setDetails(true)}>{t("deployments.expandDetails")}</button>
        <DeploymentStateToggle
          name={deployment.name}
          enabled={deployment.enabled}
          pending={state.isPending}
          blockedReason={deployment.enabled
            ? (routeBlocked ? t("deployments.routeBlocked") : undefined)
            : (!testIsCurrent ? t("deployments.testRequired") : undefined)}
          onToggle={() => state.mutate()}
        />
        {/* No state word here any more. The conclusion line above owns where the
            deployment stands, and being disabled is now one of the things it
            says — a second copy in the action bar put "已禁用" on the same card
            twice. The button still says what a click does. */}
      </div>
      {details && <Modal drawer title={t("deployments.detailsTitle", { name: deployment.name })} onClose={() => setDetails(false)}>
        <div className="detail-drawer">
          <section className="detail-section">
            <div className="detail-section-head">
              <h3>{t("deployments.sectionBilling")}</h3>
              <button className="button secondary" disabled={readOnly} title={readOnly ? t("navigation.readOnlyAction") : undefined} onClick={() => setPricing(true)}>{activePrice ? t("deployments.adjustPrice") : t("deployments.setPrice")}</button>
            </div>
            {deployment.pricing_quarantined && <div className="notice warning"><strong>{t("deployments.pricingQuarantined")}</strong><span>{deployment.pricing_quarantine_reason}</span><button className="button ghost" disabled={readOnly} title={readOnly ? t("navigation.readOnlyAction") : undefined} onClick={() => setConfirmingRestore(true)}>{t("deployments.confirmRestoredPricing")}</button></div>}
            {prices.isError && <ErrorState
              error={prices.error}
              action={<button className="button secondary" type="button" disabled={prices.isFetching} onClick={() => prices.refetch()}>{t("common.retry")}</button>}
            />}
            {/* A free price needs no rate grid at all — the version row in the
                timeline below states it once, with the date and the source that
                fixed it. Repeating it here made the same fact appear three times
                on one screen, counting the status strip. */}
            {(activePrice?.billing_mode === "metered" || !activePrice) && <div className="detail-facts">
              {activePrice?.billing_mode === "metered" && (deployment.capabilities.chat || deployment.capabilities.embeddings) && <>
                <DetailFact label={t("deployments.inputPrice")} value={money(activePrice.input_micros_per_million)} meta={t("deployments.perMillionTokens")} />
                {/* Shown even when it equals the input rate: that is itself the
                    fact worth reading, since it means cached prompt tokens are
                    not priced separately on this deployment. */}
                <DetailFact label={t("deployments.cachedInputPrice")} value={money(activePrice.cached_input_micros_per_million)} meta={t("deployments.perMillionTokens")} />
                <DetailFact label={t("deployments.outputPrice")} value={money(activePrice.output_micros_per_million)} meta={t("deployments.perMillionTokens")} />
              </>}
              {activePrice?.billing_mode === "metered" && <DetailFact label={t("deployments.fixedPrice")} value={money(activePrice.fixed_request_micros_usd)} meta={t("deployments.perRequest")} />}
              {/* With a schedule the rates above are only what applies outside
                  the windows, so leaving them to stand alone would read as the
                  whole price. */}
              {activePrice?.schedule && <DetailFact
                label={t("deployments.priceSchedule")}
                value={t("deployments.scheduleWindowCount", { count: activePrice.schedule.windows.length })}
                meta={`${activePrice.schedule.timezone} · ${activePrice.schedule.windows.map((window) => `${minuteToClock(window.start_minute)}–${minuteToClock(window.end_minute)}`).join(" ")}`}
              />}
              {!activePrice && <DetailFact
                label={t("deployments.priceStatus")}
                value={prices.isPending ? t("common.loadingShort") : prices.isError ? t("deployments.priceUnavailable") : t("deployments.priceNotConfigured")}
                meta={t("deployments.priceRequired")}
                unset
              />}
            </div>}
            {/* The panel used to promise a timeline and render only the versions
                that had not taken effect yet, so the one in force and everything
                it replaced were invisible. The read already returns them. */}
            <div className="detail-subhead"><strong>{t("deployments.priceTimeline")}</strong></div>
            {priceTimeline.length ? (
              <ol className="detail-timeline">
                {priceTimeline.map((price) => (
                  <li key={price.id} data-status={price.status}>
                    <code>v{price.version}</code>
                    <div>
                      <strong>{t(`deployments.priceVersionStatus.${price.status}`)} · {t(price.billing_mode === "free" ? "deployments.freeLabel" : "deployments.meteredLabel")}</strong>
                      <small>{dateTime(price.effective_from)} · {priceSourceWords(price.source, t)}{price.schedule ? ` · ${t("deployments.scheduleWindowCount", { count: price.schedule.windows.length })}` : ""}</small>
                    </div>
                    {price.status === "scheduled" && <button
                      className="button ghost"
                      // Every scheduled row carries this button, so the visible
                      // word cannot be the whole accessible name.
                      aria-label={t("deployments.cancelPriceVersion", { version: price.version })}
                      disabled={readOnly || cancelPrice.isPending}
                      title={readOnly ? t("navigation.readOnlyAction") : undefined}
                      onClick={() => cancelPrice.mutate(price)}
                    >{t("common.cancel")}</button>}
                  </li>
                ))}
              </ol>
            ) : <p className="detail-empty">{prices.isPending ? t("common.loadingShort") : prices.isError ? t("deployments.priceUnavailable") : t("deployments.noPriceVersions")}</p>}
          </section>

          <section className="detail-section">
            <div className="detail-section-head">
              <h3>{t("deployments.capabilities")}</h3>
              <small>{[
                t("deployments.capabilityCount", { count: capabilities.length }),
                uniformEvidence ? t("deployments.uniformEvidence", { evidence: t(`deployments.evidenceValues.${uniformEvidence}`) }) : "",
              ].filter(Boolean).join(" · ")}</small>
            </div>
            {/* Every capability, with what establishes it. The card truncates
                because a tile has a width; the drawer is the one place that can
                list them all, so truncating here answered nothing.
                The evidence column only appears when there is something to
                compare — eight rows all reading "已声明" is one fact printed
                eight times, and it says it in the section head instead. */}
            {capabilities.length ? (
              <ul className="detail-capability-list" aria-label={t("deployments.capabilities")}>
                {capabilities.map((capability) => (
                  <li key={capability}>
                    <span>{t(`capabilities.${capability}`)}</span>
                    {!uniformEvidence && <small data-evidence={evidenceOf(capability)}>
                      {t(`deployments.evidenceValues.${evidenceOf(capability)}`)}
                    </small>}
                  </li>
                ))}
              </ul>
            ) : <p className="detail-empty">{t("deployments.noCapabilities")}</p>}
            <CapabilityReviewNotice review={review} readOnly={readOnly} onEdit={() => { setDetails(false); onEdit(); }} />
          </section>

          <section className="detail-section">
            <div className="detail-section-head"><h3>{t("deployments.sectionRuntime")}</h3></div>
            <div className="detail-facts">
              <DetailFact label={t("deployments.status")} value={deployment.enabled ? t("common.enabled") : t("common.disabled")} unset={!deployment.enabled} />
              {/* The state that decides routing today. A manual test can be days
                  old; this is what the router acted on last. */}
              <DetailFact
                label={t("deployments.probe")}
                value={t(`deployments.probeStates.${probe?.state ?? "not_probed"}`)}
                meta={probe?.state === "unhealthy"
                  ? [t("deployments.probeUnhealthyMeta"), probeReason].filter(Boolean).join(" · ")
                  : probe?.state === "healthy"
                    ? probe.observed_at ? dateTime(probe.observed_at) : undefined
                    : t("deployments.probeNotProbedMeta")}
                unset={probe?.state === "unhealthy"}
              />
              <DetailFact
                label={t("deployments.lastTest")}
                value={testVerdict}
                meta={testState === "failure" && testFailureReason ? testFailureReason : deployment.last_tested_at ? dateTime(deployment.last_tested_at) : undefined}
                unset={testState === "failure"}
              />
              <DetailFact
                label={t("deployments.routeDependency")}
                value={activeRouteCount ? t("deployments.activeRoutesEnabled", { count: activeRouteCount }) : t("deployments.noActiveRoutes")}
              />
              <DetailFact label={t("deployments.concurrency")} value={deployment.max_concurrency || t("deployments.unlimited")} />
              {deployment.region && <DetailFact label={t("deployments.region")} value={deployment.region} />}
              <DetailFact label={t("deployments.context")} value={deployment.capabilities.max_context_tokens ? exactNumber(deployment.capabilities.max_context_tokens) : t("deployments.upstreamApplies")} meta={deployment.capabilities.max_context_tokens ? t("deployments.tokens") : t("deployments.undeclared")} />
              <DetailFact label={t("deployments.maxOutput")} value={deployment.capabilities.max_output_tokens ? exactNumber(deployment.capabilities.max_output_tokens) : t("deployments.upstreamApplies")} meta={deployment.capabilities.max_output_tokens ? t("deployments.tokens") : t("deployments.undeclared")} />
            </div>
          </section>

          {/* Identifiers were behind a disclosure, which put one extra click in
              front of the one thing an operator opens the drawer for while
              reading a log line. Last is far enough. */}
          <section className="detail-section">
            <div className="detail-section-head"><h3>{t("deployments.sectionConnection")}</h3></div>
            <div className="detail-facts">
              <DetailFact label={t("deployments.provider")} value={providerName} href={`/admin/providers#provider-${deployment.provider_id}`} />
              <DetailFact label={t("deployments.upstreamTarget")} value={deployment.provider_model} mono />
              {/* The question this answers is which API shape a client has to
                  speak, so it is named and worded that way rather than by the
                  internal surface id. */}
              <DetailFact label={t("deployments.compatibleInterface")} value={t(`deployments.accessSurfaces.${deployment.access_surface}`, { defaultValue: deployment.access_surface })} />
              {/* The revision number is Halro's optimistic-concurrency counter,
                  the same kind of internal identifier as the profile and binding
                  ids. What an operator reads it for is "has anyone changed this,
                  and when" — which is the timestamp, not the counter. */}
              {deployment.updated_at && <DetailFact label={t("deployments.lastUpdated")} value={dateTime(deployment.updated_at)} />}
            </div>
          </section>

        </div>
      </Modal>}
      {pricing && <PriceVersionForm deployment={deployment} current={activePrice} blocking={blockingPrice} onClose={() => setPricing(false)} />}
	  {confirmingRestore && <RestorePricingConfirm deployment={deployment} onClose={() => setConfirmingRestore(false)} />}
    </article>
  );
}

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

// What the deployment claims, against what establishes it now.
//
// The panel used to open with two sentences of explanation, list five facts —
// three of them usually a dash — and close with a third sentence, without ever
// saying what to do. It now states the disagreement as the capability names it
// is about, one line each, and carries the one action that resolves it.
//
// Two things are kept because they change what the reader should do, and only
// in the state where they do: drift says why (a profile that narrowed cannot be
// fixed by editing, a catalog that dropped the model can) and what it costs
// (that deployment is not serving traffic). A review costs nothing and needs
// neither.
function CapabilityReviewNotice({ review, readOnly, onEdit }: { review: CapabilityReview | undefined; readOnly: boolean; onEdit: () => void }) {
  const { t } = useTranslation();
  if (!review || review.state === "current") return null;
  const drifted = review.state === "drifted";
  const unavailable = review.state === "catalog_unavailable";
  const names = (values: string[] | undefined) =>
    (values ?? []).map((name) => t(`capabilities.${name}`)).join(t("common.listSeparator"));
  const offered = review.available_for_review ?? [];
  // Claimed here and not established by the catalog. Under drift that means
  // nothing supports it any more; under review it means the catalog disagrees
  // with a declaration that is still being served — a different fact, and it
  // used to wear the same label and the same warning colour as the first.
  const disputed = review.no_longer_supported ?? [];
  const switchedOff = review.operator_disabled ?? [];
  const title = drifted
    ? t("deployments.capabilitiesUnsupported")
    : unavailable
      ? t("deployments.catalogUnavailableTitle")
      // A title that says "new capabilities" over a panel whose loudest line is
      // a disagreement describes half of what happened.
      : disputed.length ? t("deployments.capabilitiesDisagree") : t("deployments.capabilitiesToReview");
  return (
    <div className={`notice ${drifted ? "warning" : ""} ${unavailable ? "" : "has-action"} deployment-capability-review`}>
      <div className="notice-copy">
        <strong>{title}</strong>
        {drifted && <span>{t(`deployments.reviewReasons.${review.reason ?? "catalog_revision_advanced"}`)}</span>}
        <div className="capability-review-rows">
          {!!offered.length && <div><small>{t("deployments.canEnable")}</small><span>{names(offered)}</span></div>}
          {!!disputed.length && <div>
            <small>{drifted ? t("deployments.noLongerSupported") : t("deployments.catalogDisputes")}</small>
            <span>{names(disputed)}{drifted ? "" : ` · ${t("deployments.stillServing")}`}</span>
          </div>}
          {!!switchedOff.length && <div><small>{t("deployments.switchedOff")}</small><span>{names(switchedOff)}</span></div>}
        </div>
        {drifted && <span>{t("deployments.driftedConsequence")}</span>}
        {unavailable && <span>{t("deployments.catalogUnavailableConsequence")}</span>}
      </div>
      {/* The instruction was "turn a capability on and retest", and following it
          meant closing the drawer and finding the card again. */}
      {!unavailable && <div className="notice-action">
        <button className="button secondary" disabled={readOnly} title={readOnly ? t("navigation.readOnlyAction") : undefined} onClick={onEdit}>{t("deployments.editCapabilities")}</button>
      </div>}
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
		<Field label={t("usage.currentPassword")}><input autoComplete="off" type="password" required value={password} onChange={(event) => setPassword(event.target.value)} /></Field>
		<Field label={t("usage.totpOptional")}><input autoComplete="off" inputMode="numeric" value={totp} onChange={(event) => setTotp(event.target.value)} /></Field>
		{mutation.isError && <ErrorState error={mutation.error} />}<button className="button primary" disabled={mutation.isPending}>{t("deployments.confirmRestoredPricing")}</button>
	</form></Modal>;
}

// One fact in the details drawer: label, value, and an optional line naming the
// unit or the scope the value is in.
//
// It is a `.resource-fact` — the same label/value typography the cards and the
// resource lists use, from resource-list.css. Only two things differ, and both
// follow from the container: a drawer has no column to keep to, so the value
// wraps instead of ellipsizing, and a fact that is missing something the
// deployment needs says so in the warning colour.
function DetailFact({ label, value, meta, unset = false, mono = false, href }: {
  label: string;
  value: string | number;
  meta?: string;
  unset?: boolean;
  mono?: boolean;
  href?: string;
}) {
  return (
    <div className={`resource-fact detail-fact ${unset ? "unset" : ""}`}>
      <small>{label}</small>
      {href
        ? <Link className="resource-link" href={href}>{value} →</Link>
        : mono ? <code>{value}</code> : <strong>{value}</strong>}
      {meta ? <small className="detail-fact-meta">{meta}</small> : null}
    </div>
  );
}

// The version's provenance in words. `manual · asserted · temporary_estimate`
// is three identifiers from three different enums; printed raw it reads as a
// system trace rather than as an answer to "how much do we trust this price".
const PRICE_SOURCE_KINDS: Record<string, string> = {
  official_public_price: "officialPublicPrice",
  contract_price: "contractPrice",
  internal_cost: "internalCost",
  temporary_estimate: "temporaryEstimate",
};

function priceSourceWords(source: DeploymentPriceVersion["source"], t: TFunction): string {
  const reference = (source.reference ?? "").trim();
  const kind = PRICE_SOURCE_KINDS[reference];
  return [
    kind ? t(`deployments.sourceKinds.${kind}`) : reference,
    t(`deployments.priceSourceTypes.${source.type}`, { defaultValue: source.type }),
    t(`deployments.priceAssurances.${source.assurance}`, { defaultValue: source.assurance }),
  ].filter(Boolean).join(" · ");
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
            <Field label={t("deployments.inputUSD")}><input autoComplete="off" inputMode="decimal" required value={input} onChange={(event) => setInput(event.target.value)} /></Field>
            <Field label={t("deployments.outputUSD")}><input autoComplete="off" inputMode="decimal" required value={output} onChange={(event) => setOutput(event.target.value)} /></Field>
          </div>
          {/* Every provider that reports a cache-read tier bills it well below
              the input rate, so this field is asked for beside the other two
              rather than hidden: left at the input rate it over-charges cached
              prompts, and left at zero it gives them away. */}
          {/* Alone in the two-column grid this field left half the row empty and
              wrapped its own three-line explanation into the other half. The row
              spans instead, and the box keeps the width of the two above it. */}
          <div className="price-form-grid price-cached-row">
            <Field label={t("deployments.cachedInputUSD")} hint={t("deployments.cachedInputHint")}><input autoComplete="off" inputMode="decimal" required value={cachedInputValue} onChange={(event) => setCachedInput(event.target.value)} /></Field>
          </div>
          {/* Some providers publish peak and off-peak rates. Without this the
              operator can only enter one number, and which way the accounting
              is wrong is decided by which number they happen to pick. */}
          <PriceScheduleFields schedule={schedule} onChange={setSchedule} problem={scheduleProblem} baseRates={{ input, cachedInput: cachedInputValue, output, fixed }} />
          <details className="price-advanced">
            <summary>{t("deployments.advancedPricing")}</summary>
            <Field label={t("deployments.fixedRequestUSD")}><input autoComplete="off" inputMode="decimal" required value={fixed} onChange={(event) => setFixed(event.target.value)} /></Field>
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
        {effectiveMode === "scheduled" && <Field label={t("deployments.effectiveFrom")}><input autoComplete="off" type="datetime-local" required value={effective} onChange={(event) => setEffective(event.target.value)} /></Field>}
        {!validEffective && <p className="field-hint error">{blocking ? t("deployments.effectiveAfterScheduled", { version: blocking.version, effective: dateTime(blocking.effective_from) }) : t("deployments.invalidEffectiveTime")}</p>}
        <Field label={t("deployments.sourceNote")}><textarea autoComplete="off" value={sourceNote} onChange={(event) => setSourceNote(event.target.value)} placeholder={t("deployments.sourceNotePlaceholder")} /></Field>
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
  return <section className={schedule ? "price-schedule open" : "price-schedule"}>
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
    {schedule && <div className="price-schedule-body">
      {/* A missing zone is the zone field's problem, so it is reported on the
          field. Collected with the window problems at the foot of the card it
          sat a full schedule away from the box it names. */}
      <TimeZoneField
        label={t("deployments.scheduleTimezone")}
        hint={t("deployments.scheduleTimezoneHint")}
        error={problem === "timezone" ? t("deployments.scheduleProblem.timezone") : undefined}
        value={schedule.timezone}
        onChange={(timezone) => onChange({ ...schedule, timezone })}
      />
      {/* Both rules hold for every window, so they are stated once. Repeating
          them per card turned a two-window schedule into four lines of the same
          sentence and buried the rates between them. */}
      <p className="price-schedule-note">{t("deployments.scheduleTimeHint")} {t("deployments.scheduleRateUnit")}</p>
      {/* One window is a card rather than a table row. Six rate columns plus a
          remove button never fit the modal's width, so the table could only
          scroll sideways — which hides the very columns being compared and puts
          the start time out of view while the operator types a rate. */}
      {schedule.windows.map((window, index) => <fieldset className="price-schedule-window" key={index}>
        <legend>{t("deployments.scheduleWindowLabel", { index: index + 1 })}</legend>
        <div className="price-schedule-window-head">
          <div className="price-schedule-range">
            <label><span>{t("deployments.scheduleStart")}</span><input autoComplete="off" value={window.start} onChange={(event) => update(index, { start: event.target.value })} placeholder="09:00" inputMode="numeric" /></label>
            <label><span>{t("deployments.scheduleEnd")}</span><input autoComplete="off" value={window.end} onChange={(event) => update(index, { end: event.target.value })} placeholder="12:00" inputMode="numeric" /></label>
          </div>
          {/* Ghost rendered as borderless grey text floating at the far edge of
              the card, which read as a caption rather than as the destructive
              control it is. */}
          <button type="button" className="button danger-text" onClick={() => onChange({ ...schedule, windows: schedule.windows.filter((_, position) => position !== index) })}>{t("deployments.removeScheduleWindow")}</button>
        </div>
        <div className="price-schedule-rates">
          <label><span>{t("deployments.scheduleRateInput")}</span><input autoComplete="off" inputMode="decimal" value={window.input} onChange={(event) => update(index, { input: event.target.value })} aria-label={t("deployments.inputUSD")} /></label>
          <label><span>{t("deployments.scheduleRateCachedInput")}</span><input autoComplete="off" inputMode="decimal" value={window.cachedInput} onChange={(event) => update(index, { cachedInput: event.target.value })} aria-label={t("deployments.cachedInputUSD")} /></label>
          <label><span>{t("deployments.scheduleRateOutput")}</span><input autoComplete="off" inputMode="decimal" value={window.output} onChange={(event) => update(index, { output: event.target.value })} aria-label={t("deployments.outputUSD")} /></label>
          <label><span>{t("deployments.scheduleRateFixed")}</span><input autoComplete="off" inputMode="decimal" value={window.fixed} onChange={(event) => update(index, { fixed: event.target.value })} aria-label={t("deployments.fixedRequestUSD")} /></label>
        </div>
      </fieldset>)}
      <div className="price-schedule-actions">
        <button type="button" className="button secondary" onClick={() => onChange({ ...schedule, windows: [...schedule.windows, { start: "14:00", end: "18:00", ...baseRates }] })}>
          {t("deployments.addScheduleWindow")}
        </button>
      </div>
      {/* Saying which rate covers the rest of the day is the difference between
          a schedule the operator can reason about and one where the uncovered
          hours are an unmarked hole. */}
      <p className="field-hint">{t("deployments.scheduleBaseHint", { input: baseRates.input, output: baseRates.output })}</p>
      {problem && problem !== "timezone" && <p className="field-hint error">{t(`deployments.scheduleProblem.${problem}`)}</p>}
    </div>}
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

type SelectableBinding = ProviderBinding & { id: string };

// How the capabilities are grouped on screen. Which group a capability belongs
// in is a judgement about the operator's question, not something the server can
// answer, so it stays here — but the set has to stay complete, or a capability
// the server offers would have nowhere to be drawn and would silently vanish
// from this form. DeploymentsPage.test.tsx checks it against what the endpoint
// actually serves.
const deploymentCapabilityGroups = [
  { id: "operations", capabilities: ["chat", "embeddings", "moderations", "images", "transcriptions", "speech", "rerank", "async_generate"] },
  { id: "modalities", capabilities: ["vision", "fetched_image"] },
  { id: "protocol", capabilities: ["streaming", "tools", "json_object", "structured_outputs", "developer_role", "reasoning", "stream_usage", "provider_executed_tools"] },
  { id: "managed", capabilities: ["files", "batches"] },
] as const;

/** Exported for DeploymentsPage.test.tsx, which checks the grouping against what
 * the provider-profiles endpoint actually serves. */
export const deploymentCapabilityGroupsForTest = deploymentCapabilityGroups;

// One list, derived. It used to be written out a second time above these groups,
// which meant a capability could be in one and not the other.
const deploymentCapabilityNames = deploymentCapabilityGroups.flatMap((group) => group.capabilities);

/**
 * The order the model catalogue is banded in, by what the operator does next.
 *
 * Ready first, because that is what most visits are for. The rest are not
 * hidden: an operator arrives with a name read off the upstream's own console,
 * and a list that quietly omits it reads as Halro's catalogue being wrong
 * rather than as this provider not serving that model.
 *
 * The last two are separate bands because their remedies are opposite —
 * a catalogue that disagrees with itself needs a person to look, while a model
 * no binding here can serve needs a different provider or another interface
 * enabled on this one. Collapsing them would send half the operators the wrong
 * way, which is the same mistake the resolution state itself makes today by
 * reporting "not in the catalogue" for a model the catalogue lists under a
 * profile this provider has not bound.
 */
const MODEL_BAND_ORDER: readonly ResolutionState[] = ["resolved", "unknown", "covered_elsewhere", "conflicting", "no_variant"];
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
  const picked = !current && providerModel.trim() !== "";
  // Both states mean the same thing about capabilities: nothing here claims any,
  // so the operator declares them or spends a detection call. They differ in why,
  // and the pointer below says so — but a model the catalogue serves on another
  // interface must not lose the way to declare it. The catalogue can be stale,
  // and the operator may know something Halro does not.
  const declaredModel = picked
    && (resolvedTarget?.resolution_state === "unknown" || resolvedTarget?.resolution_state === "covered_elsewhere");
  const coveredElsewhere = picked && resolvedTarget?.resolution_state === "covered_elsewhere";
  const coveringProfiles = coveredElsewhere ? resolvedTarget?.covered_by_profiles ?? [] : [];
  // Why the capability section is empty, which decides what the operator should
  // do about it. One panel used to answer all three with "not in the catalogue,
  // run a detection" — wrong for the first, since the catalogue does list the
  // model, and a detection cannot answer a routing question anyway; and
  // incomplete for the second, where the interface returns identifiers only and
  // no amount of refreshing will produce more.
  // Where each capability's claim came from. Rendered per row only when the
  // rows disagree: a column reading "built-in catalogue" eight times is not a
  // comparison, and the header already says it once. When they differ, which
  // ones were measured and which were merely listed is the whole question.
  const claimSourceByCapability = useMemo(() => {
    const sources = new Map<string, string>();
    for (const claim of selectedVariant?.capability_claims ?? []) {
      if (claim.status === "supported") sources.set(claim.capability_id, claim.source);
    }
    return sources;
  }, [selectedVariant]);
  const claimSourcesDiffer = new Set(claimSourceByCapability.values()).size > 1;
  const emptyCapabilityReason = coveredElsewhere
    ? "coveredElsewhere"
    : resolvedTarget?.metadata_source === "none"
      ? "noUpstreamMetadata"
      : "notInCatalogue";
  const noVariant = picked && resolvedTarget?.resolution_state === "no_variant";
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
  // What a verification moved. The baseline is what the catalogue claimed when
  // the run was asked for, the recommendation is what the run decided, and the
  // difference is the only reason to have paid for it. It is null for a
  // detection of a model the catalogue does not cover: there was nothing to
  // measure against, so nothing moved — the probes are the whole claim.
  const verification = useMemo(() => {
    const baseline = detection?.baseline_capabilities;
    if (!detection || !baseline) return null;
    const answered = (name: string) => ["supported", "unsupported"].includes(detection.capabilities[name]?.status ?? "");
    return {
      measured: deploymentCapabilityNames.filter(answered),
      gained: deploymentCapabilityNames.filter((name) => detection.recommended_capabilities[name] && !baseline[name]),
      lost: deploymentCapabilityNames.filter((name) => !detection.recommended_capabilities[name] && baseline[name]),
      // Kept disjoint from `lost` on purpose: a claim the probes never answered
      // can still be switched off by the dependency clamp — stream_usage when
      // streaming is refused, vision when chat is — and reporting the same
      // capability as both "refused upstream" and "still on the catalogue's
      // claim" describes two different outcomes for one row.
      carried: deploymentCapabilityNames.filter((name) => baseline[name] && !answered(name) && detection.recommended_capabilities[name]),
    };
  }, [detection]);
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
    mutationFn: ({ requestedSelectionRevision, reauth, verify }: { requestedSelectionRevision: string; reauth: ReauthValues; verify?: boolean }) =>
      api.createModelCapabilityDetection(providerID, {
        provider_model: providerModel.trim(), target_kind: targetKind, region: region.trim(),
        // A verification runs on the interface the catalogue already names, so
        // it says which one rather than paying to identify one that is not in
        // question.
        ...(detectionBindingID || verify && bindingID ? { binding_id: detectionBindingID || bindingID } : {}), risk_tier: "safe_automatic",
        selection_revision: requestedSelectionRevision,
        // The refresh is what declines the answers that already exist — the
        // stored result and the catalogue's review alike — and asks the upstream
        // instead.
        ...(verify ? { force_refresh: true } : {}),
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
  // Verifying a catalogue claim is the same detection, asked about a model the
  // catalogue already answered for. It is deliberately not automatic: the
  // catalogue costs nothing and is right most of the time, and this spends the
  // operator's credential to find out where it is not.
  const verifyCatalogClaims = (reauth: ReauthValues) => {
    const nextSelection = crypto.randomUUID();
    setCapabilityDetection(null);
    appliedDetectionRevision.current = 0;
    detectionIdempotencyKey.current = crypto.randomUUID();
    setSelectionRevision(nextSelection);
    return detectCapabilities.mutateAsync({ requestedSelectionRevision: nextSelection, reauth, verify: true });
  };
  const value = () => ({
    name: name.trim(),
    provider_id: providerID,
    ...(bindingID ? { binding_id: bindingID } : {}),
    provider_model: providerModel.trim(),
    ...(canonicalModelRef ? { capability_model: canonicalModelRef } : {}),
    target_kind: targetKind,
    capabilities,
    ...(widening || declaringBeyondVariant || declaredModel && manualDeclaration ? { mode: "operator_declared" } : {}),
    // Every completed detection pins the write to itself, whether it answered
    // for a model the catalogue does not cover or verified one it does. Pinning
    // only the first left a verification's own findings to be validated against
    // the catalogue claims it was run to check, and anything it established
    // beyond them was refused.
    ...(detection?.status === "completed" ? {
      capability_detection_id: detection.id,
      capability_detection_revision: detection.revision,
    } : {}),
    // Dropped when the operator declared past the variant: the revision pins
    // this write to the claims it was resolved from, and the server refuses a
    // capability set that exceeds them. Sending both would be asking the server
    // to honour a pin and break it in the same request.
    ...(!current && !manualDeclaration && detection?.status !== "completed" && !declaringBeyondVariant && selectedVariant
      ? { resolution_revision: selectedVariant.revision } : {}),
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
  // A capability the operator ticked that the resolved variant does not claim.
  //
  // The narrowing editor used to be bounded by the variant, so these could not
  // be ticked at all: a Mantle model whose catalogue entry says chat and
  // streaming rendered two rows, and vision — which the interface carries and
  // the model has — had nowhere to be turned on. The editor is bounded by the
  // interface now, and this is what it costs: the variant path refuses
  // capabilities beyond the claims it was resolved from, so a tick past them
  // has to be saved as the operator's own declaration instead.
  const declaringBeyondVariant = Boolean(
    !current && selectedVariant && !manualDeclaration && !detection
    && deploymentCapabilityNames.some((name) => !selectedVariant.capabilities[name] && capabilities[name]),
  );
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
  // A capability tick is only half of what routing applies. The other half — the
  // members this interface cannot carry — was computed on the server, refused on
  // by the Gateway, and shown nowhere, so an operator ticked vision and met the
  // rest as a refused request.
  const activeProfileIDs = pinnedBinding
    ? [pinnedBinding.profile_id]
    : selectableBindings.map((binding) => binding.profile_id);
  // What the interface could serve, which is not what this connection has turned
  // on. A capability neither the ceiling nor the deployment carries used to be
  // left out of the form entirely, so a capability added to a profile after the
  // connection was made had nowhere to be enabled: the refusal names it, the
  // operator opens this form, and there is no box. Drawn as unavailable with the
  // reason instead — the connection is where it is turned on, and saying so is
  // the difference between a dead end and a next step.
  const servableCeiling = catalogReady ? interfaceCeiling(catalogReady, activeProfileIDs) : capabilityCeiling;
  const configurableCapabilityNames = deploymentCapabilityNames.filter(
    (name) => capabilityCeiling[name] || capabilities[name] || servableCeiling[name],
  );
  // Three different dead ends used to wear one label. "Enable it on the
  // connection" is the next step only when the connection is what withholds
  // the capability; for one the connection already carries, the box is grey
  // because this identification did not put it in the recommended set, and
  // sending the operator to tick a box they ticked already is a detour with
  // nothing at the end of it. The probe outcome separates the two answers that
  // are not interchangeable either: refused upstream is a finding, unestablished
  // is the absence of one, and only the second is worth re-running.
  const capabilityUnavailableReason = (name: (typeof deploymentCapabilityNames)[number]) => {
    if (!servableCeiling[name]) return "providers.unsupportedByInterface";
    if (!bindingCeiling[name]) return "deployments.enableOnConnection";
    if (detection?.status === "completed" && !current) {
      // Six outcomes with six next steps. The server has always told them
      // apart; the form used to say "could not be verified" to all of them,
      // which is the right sentence for exactly one and an invitation to a
      // pointless re-run for the rest. A capability missing from the map
      // entirely was never reached, same as not_probed.
      const result = detection.capabilities[name];
      // A capability the profile serves and the call budget could not fit. It
      // was not skipped by policy and it is not a model answer — it is a
      // ceiling, and the operator can raise it or verify this one by hand.
      if (result?.status === "not_probed" && result.probe_kind === "probe_budget") {
        return "deployments.detectionProbeBudget";
      }
      switch (result?.status) {
        // Probed, and the upstream refused.
        case "unsupported": return "deployments.detectionRefused";
        // The upstream answered; the answer carried no evidence. Re-running
        // sends the identical request and gets the identical answer.
        case "assertion_failed": return "deployments.detectionAssertionFailed";
        // Not the model's answer at all — the provider, or this credential.
        // Both are fixed somewhere other than this form.
        case "unavailable": return "deployments.detectionUnavailable";
        case "unauthorized": return "deployments.detectionUnauthorized";
        // Refused, and Halro could not read why. This is the one worth
        // identifying again.
        case "inconclusive": return "deployments.detectionUnestablished";
        default: return "deployments.detectionNotProbed";
      }
    }
    return "deployments.notDeclaredByModel";
  };
  const targetLabel = t(`deployments.targetLabels.${targetKind}`);
  const modelCatalogEnumerable = Boolean(!identityLocked && targetCatalog.data?.discovery.can_enumerate);
  const modelCatalogLoading = targetCatalog.isPending || refreshTargetCatalog.isPending;
  // A provider that cannot enumerate can still have something to choose from:
  // Halro's own catalog carries exact identifiers for some profiles, and the
  // server offers them here labelled as offers. Gating the picker on
  // can_enumerate alone hid a populated list behind a blank field.
  const hasCatalogOffers = Boolean(
    !identityLocked && (targetCatalog.data?.items ?? []).some((target) => target.metadata_source === "model_catalog"),
  );
  // One flag drives every catalog affordance. Splitting the combobox from the
  // refresh button gave a provider that cannot enumerate a dropdown arrow and a
  // refresh control while the catalog was loading or had failed, and both led
  // nowhere. A provider whose discovery says it cannot enumerate, and for which
  // Halro knows no models either, still gets neither.
  const showModelCatalogControls = Boolean(
    !identityLocked
    && providerID
    && (modelCatalogEnumerable || hasCatalogOffers || targetCatalog.isPending || targetCatalog.isError || refreshTargetCatalog.isPending || refreshTargetCatalog.isError),
  );
  // Refreshing means asking the provider again, so it belongs wherever a
  // provider listing is in play — including before the first one has arrived,
  // when whether this provider enumerates is not yet known. It does not belong
  // on a list that came from Halro's own catalog, where there is nothing to ask.
  const showModelCatalogRefresh = Boolean(
    showModelCatalogControls
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
  // The listing already knows which of these the provider can actually serve:
  // the catalogue GET resolves every binding and returns a resolution state per
  // target. The options rendered one line each, so a model that is ready to
  // deploy and one this provider cannot serve at all looked identical, and the
  // operator found out by filling in the form.
  //
  // Banded, not filtered: an operator arrives with a name read off the
  // upstream's own console, and a list that silently omits it reads as Halro's
  // catalogue being broken.
  const modelBands = useMemo(() => {
    const bands = new Map<ResolutionState, ResolvedInvocationTarget[]>(
      MODEL_BAND_ORDER.map((state) => [state, []]),
    );
    for (const model of visibleModels) {
      (bands.get(model.resolution_state) ?? bands.get("unknown")!).push(model);
    }
    let offset = 0;
    return MODEL_BAND_ORDER.flatMap((state) => {
      const items = bands.get(state) ?? [];
      if (!items.length) return [];
      const band = { state, items, offset };
      offset += items.length;
      return [band];
    });
  }, [visibleModels]);
  // One flat order, derived from the banded one. Arrow keys walk the list the
  // eye walks; an index into the unbanded array would jump.
  const orderedModels = useMemo(() => modelBands.flatMap((band) => band.items), [modelBands]);
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
      setActiveModelIndex((currentIndex) => Math.min(currentIndex + 1, orderedModels.length - 1));
    } else if (event.key === "ArrowUp") {
      event.preventDefault();
      event.stopPropagation();
      setModelPickerOpen(true);
      setActiveModelIndex((currentIndex) => currentIndex <= 0 ? Math.max(orderedModels.length - 1, 0) : currentIndex - 1);
    } else if (event.key === "Enter" && modelPickerOpen && activeModelIndex >= 0 && orderedModels[activeModelIndex]) {
      event.preventDefault();
      chooseModel(orderedModels[activeModelIndex]);
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
  // The outcome travels with the name. The reason a probe settled nothing is
  // already in this same response object — it was rendered only for a failed
  // detection, so a completed one with an inconclusive capability showed the
  // operator a bare sentence while the answer sat one field away.
  const unestablishedCapabilities = detection?.status === "completed"
    ? deploymentCapabilityNames.flatMap((name) => {
      const result = detection.capabilities[name];
      if (!result) return [];
      // risk_policy rows are capabilities the plan never meant to reach, so they
      // are not outcomes. Everything else the editor cannot express belongs here.
      const unestablished = result.status === "inconclusive" || result.status === "assertion_failed"
        || result.status === "unavailable" || result.status === "unauthorized"
        || (result.status === "not_probed" && result.probe_kind !== "risk_policy");
      if (!unestablished) return [];
      return [{ name, status: result.status, errorClass: result.error_class, providerStatus: result.provider_status, providerCode: result.provider_code }];
    })
    : [];
  // What the upstream refused outright, and which field it named doing it.
  //
  // A refusal is the one probe outcome the console used to state without
  // evidence: "this model does not support it", full stop. That reads as
  // Halro's judgement when it is the upstream's, and an operator whose model
  // card says otherwise has nowhere to look. The identifiers are recorded per
  // probe — they are the half that says which request and which field — so they
  // are shown for the same reason they are shown on every other outcome.
  const refusedCapabilities = detection?.status === "completed"
    ? deploymentCapabilityNames.flatMap((name) => {
      const result = detection.capabilities[name];
      if (result?.status !== "unsupported") return [];
      return [{ name, status: result.status, providerStatus: result.provider_status, providerCode: result.provider_code }];
    })
    : [];
  // What every probe on the resolved interface actually came back with. A
  // failure used to be explained only by what identification asked, which is
  // empty by construction when there is a single interface to identify — the
  // one case where the outcomes below are the only record of why it failed.
  // Same risk_policy filter as above: those rows are capabilities the plan
  // never meant to reach, not outcomes.
  const failedProbeOutcomes = detection?.status === "failed"
    ? deploymentCapabilityNames.flatMap((name) => {
      const result = detection.capabilities?.[name];
      if (!result || result.probe_kind === "risk_policy") return [];
      return [{ name, status: result.status, errorClass: result.error_class }];
    })
    : [];
  // What the save will record, not what the form happens to display: widening
  // sends mode=operator_declared, so the source shown has to say so too.
  const capabilityEvidenceSource = manualDeclaration || widening || declaringBeyondVariant
    ? "operator_declared"
    : detection?.status === "completed" ? detection.source : "";
  const dirty = useDirty({ name, providerID, providerModel, canonicalModelRef, bindingID, capabilities, region, maxConcurrency, targetKind });
  return (
    <Modal wide title={current ? t("deployments.edit") : template ? t("deployments.createReplacementTitle") : t("deployments.createTitle")} dirty={dirty} onClose={onClose}>
      {enabledProviders.length === 0 ? (
        <div className="notice warning"><strong>{t("deployments.providerRequired")}</strong><span>{t("deployments.providerRequiredDescription")}</span><Link className="notice-link" href="/admin/providers">{t("deployments.openProviders")}</Link></div>
      ) : (
        <form className="deployment-form" onSubmit={submit} autoComplete="off">
          <section className="deployment-form-section" aria-labelledby="deployment-target-heading">
            <header><h3 id="deployment-target-heading">{t("deployments.targetSection")}</h3><p>{t("deployments.targetSectionDescription")}</p></header>
            <div className="form-grid deployment-target-grid">
          <Field label={t("deployments.name")}><input autoComplete="off" autoFocus required value={name} onChange={(event) => setName(event.target.value)} /></Field>
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
                    autoComplete="off"
                    spellCheck={false}
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
                    <div className="deployment-model-options" ref={modelOptionsRef}>
                      {/* Outside the listbox on purpose: a listbox owns options
                          and groups, and a count bar sitting among them is
                          counted as neither by the browser and read as an
                          option by some screen readers. */}
                      <div className="deployment-model-options-meta">
                        {targetCatalog.isPending
                          ? t("deployments.modelCatalogLoading")
                          : targetCatalog.isError || refreshTargetCatalog.isError
                            ? t("deployments.modelCatalogUnavailable")
                            // Never fetched is not the same as could not be
                            // fetched: the first is one click away and costs an
                            // upstream call, the second is a fault. Reading the
                            // catalogue never dials, so the operator has to be
                            // the one who decides to spend it.
                            : targetCatalog.data?.not_cached
                              ? t("deployments.modelCatalogNotCached")
                              : <>{t("deployments.modelCatalogCountPrefix")} <strong>{targetCatalog.data?.items?.length ?? 0}</strong> {t("deployments.modelCatalogCountSuffix")}</>}
                      </div>
                      <div id="deployment-provider-model-options" role="listbox" aria-label={t("deployments.modelCatalogLabel")}>
                        {modelBands.map((band) => (
                          // The band name goes on the group, not only on the
                          // heading: the heading is the sighted half, and a
                          // band that exists only visually tells a screen
                          // reader nothing about why these are separated.
                          <div className="deployment-model-band" key={band.state} role="group" aria-label={t(`deployments.modelBands.${band.state}`)}>
                            <div className="deployment-model-band-name" aria-hidden="true">{t(`deployments.modelBands.${band.state}`)}</div>
                            {band.items.map((model, itemIndex) => {
                              const index = band.offset + itemIndex;
                              return (
                                <button
                                  className={`deployment-model-option${index === activeModelIndex ? " active" : ""}`}
                                  id={`deployment-provider-model-option-${index}`}
                                  data-model-index={index}
                                  key={`${model.target_kind}:${model.target_id}`}
                                  role="option"
                                  // The input owns the focus; an option that
                                  // stays tabbable lets a Shift+Tab land inside
                                  // the popup while aria-activedescendant still
                                  // says the input has it.
                                  tabIndex={-1}
                                  aria-selected={providerModel === model.target_id}
                                  aria-setsize={orderedModels.length}
                                  aria-posinset={index + 1}
                                  type="button"
                                  onMouseDown={(event) => event.preventDefault()}
                                  onMouseEnter={() => setActiveModelIndex(index)}
                                  onClick={() => chooseModel(model)}
                                >
                                  <strong>{model.display_name}</strong>
                                  {model.metadata_source === "model_catalog" && (
                                    // An upstream listing a model says this account
                                    // reaches it. A built-in entry says only that
                                    // Halro pre-checked the identifier, so the two
                                    // must not read the same in one list.
                                    <span className="deployment-model-offer">{t("deployments.modelFromCatalog")}</span>
                                  )}
                                </button>
                              );
                            })}
                          </div>
                        ))}
                      </div>
                      {!orderedModels.length && (
                        <div className="deployment-model-empty">{
                          targetCatalog.isPending
                            ? t("deployments.modelCatalogLoading")
                            : targetCatalog.data?.not_cached
                              ? t("deployments.modelCatalogNotCachedHint")
                              : t("deployments.noModelMatches")
                        }</div>
                      )}
                    </div>
                  )}
                </div>
                {showModelCatalogRefresh && <button
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
              <small>{
                hasCatalogOffers && !modelCatalogEnumerable
                  ? t("deployments.modelCatalogOfferHint")
                  : t(`deployments.targetHints.${targetKind}`)
              }</small>
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
          {regional && <Field label={t("deployments.region")} hint={t("deployments.regionHint")}><input autoComplete="off" disabled={identityLocked} value={region} placeholder={t("deployments.regionAutomatic")} onChange={(event) => { resetDetection(); setRegion(event.target.value); setSelectedTarget(null); setSelectedVariant(null); setBindingID(""); setCapabilities(emptyCapabilities()); }} /></Field>}
          {identityLocked && <div className="notice"><strong>{t("deployments.targetLocked")}</strong><span>{t("deployments.targetLockedDescription")}</span></div>}
            </div>
          </section>
          <section className="deployment-form-section deployment-capability-section" aria-labelledby="deployment-capabilities-heading">
            <header className="deployment-capability-section-header">
              <div>
                <h3 id="deployment-capabilities-heading">{t("deployments.capabilitySection")}</h3>
                <p>{t("deployments.capabilitySectionDescription")}</p>
              </div>
              {/* Beside the capabilities it re-decides, rather than in a card of
                  its own: the catalogue answers for free and is right most of the
                  time, so measuring is an action on what is already shown, not a
                  step of the flow. The call ceiling it may spend is stated in the
                  confirmation, before anything is spent. */}
              {!current && selectedVariant && !manualDeclaration && !detection && <ConfirmButton
                className="button ghost"
                label={detectCapabilities.isPending ? t("common.working") : t("deployments.verifyClaims")}
                title={t("deployments.verifyClaims")}
                confirmLabel={t("deployments.verifySpendConfirm", { count: targetCatalog.data?.discovery.max_verification_calls ?? 0 })}
                disabled={!targetCatalog.data?.discovery.can_verify || detectCapabilities.isPending}
                requireStepUp
                onConfirm={verifyCatalogClaims}
              />}
            </header>
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
            {/* The catalogue answers for free and is right most of the time, so
                measuring is offered rather than performed: this spends the
                operator's credential on the model to find out where the review
                and the account disagree. */}
            {!current && selectedVariant && !manualDeclaration && !detection && <details className="capability-disclosure capability-advanced">
              <summary><span>{t("deployments.capabilityNarrowing")}</span><strong>{t("providers.selectedCapabilities", { count: selectedCapabilityNames.length })}</strong></summary>
              <p className="capability-advanced-note">{t("deployments.inheritedCapabilitiesHint")}</p>
              {/* The connection's own ceiling, not the variant's. A capability
                  the profile could serve but this connection has not enabled is
                  turned on where connections are edited, and the manual editor
                  below already says so; a capability the connection carries and
                  the catalogue merely did not record belongs here. */}
              {catalogReady && <CapabilitySubsetEditor catalog={catalogReady} capabilities={capabilities} ceiling={bindingCeiling} onChange={changeCapabilities} />}
              {!catalogReady && !capabilityCatalog.isError && <Loading />}
              {capabilityCatalog.isError && <CapabilityMatrixError query={capabilityCatalog} />}
            </details>}
            {!current && coveredElsewhere && coveringProfiles.length > 0 && <div className="notice">
              <strong>{t("deployments.coveredElsewhereTitle")}</strong>
              <span>{t("deployments.coveredElsewhereDescription", { profiles: coveringProfiles.join(t("common.dotSeparator")) })}</span>
              <Link className="notice-link" href="/admin/providers">{t("deployments.openProviders")}</Link>
            </div>}
            {!current && noVariant && <div className="notice warning"><strong>{t("deployments.noVariantTitle")}</strong><span>{t("deployments.noVariantDescription")}</span><Link className="notice-link" href="/admin/providers">{t("deployments.openProviders")}</Link></div>}
            {!current && resolvedTarget?.resolution_state === "conflicting" && <div className="notice warning"><strong>{t("deployments.resolutionConflictTitle")}</strong><span>{t("deployments.resolutionConflictDescription")}</span></div>}
            {/* A verification runs on a model the catalogue does cover, so the
                panel cannot be gated on the model being undeclared any more —
                that gate hid a running detection's own progress, cancel and
                failure reporting for every model the catalogue knows.

                The region itself is mounted before it has anything to say. A
                live region inserted together with its first content is not
                reliably announced, and on this path the first content is
                "detecting capabilities" — the one line whose whole purpose is to
                be heard without looking. */}
            {!current && !manualDeclaration && (
              <div className="capability-detection-panel" aria-live="polite">
                {(declaredModel || detection) && <>
                {!detection && <>
                  <div className="capability-onboarding-card">
                    <header>
                      <div>
                        <strong>{t(`deployments.emptyCapabilities.${emptyCapabilityReason}.title`)}</strong>
                        <span>{t(`deployments.emptyCapabilities.${emptyCapabilityReason}.description`)}</span>
                        {/* The count is the operator's own configured ceiling,
                            and it is stated before the billable button, not in
                            the confirmation after it. */}
                        {emptyCapabilityReason !== "coveredElsewhere" && <span>{t("deployments.detectionCostBoundary", { count: targetCatalog.data?.discovery.max_verification_calls ?? 0 })}</span>}
                      </div>
                      <span className="capability-onboarding-status">{t(`deployments.emptyCapabilities.${emptyCapabilityReason}.next`)}</span>
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
                      {declaredModel && <button type="button" className="button ghost" onClick={() => { resetDetection(); setManualDeclaration(true); }}>{t("deployments.advancedManualDeclaration")}</button>}
                      <ConfirmButton className="button primary" label={detectCapabilities.isPending ? t("common.working") : t("deployments.confirmDetectionBinding")} title={t("deployments.confirmDetectionBinding")} confirmLabel={t("deployments.detectionSpendConfirm")} disabled={!detectionBindingID || detectCapabilities.isPending} requireStepUp onConfirm={confirmDetectionBinding} />
                    </div>
                  </div>
                </div>}
                {detection?.status === "completed" && !anyOperation && detectionNeedsProviderRepair && <div className="notice warning"><strong>{t("deployments.detectionProviderRepairTitle")}</strong><span>{t("deployments.detectionProviderRepairDescription")}</span><Link className="notice-link" href="/admin/providers">{t("deployments.openProviders")}</Link></div>}
                {/* Manual declaration answers "the catalogue does not cover this
                    model"; it is not an answer to "the verification of a covered
                    model came back empty", where the claims being checked are
                    still there to go back to. Offering it there dropped the
                    operator into a mode with no variant pin, no detection pin
                    and no way out. */}
                {detection?.status === "completed" && !anyOperation && !detectionNeedsProviderRepair && <div className="notice warning"><strong>{t("deployments.detectionInconclusiveTitle")}</strong><span>{t("deployments.detectionInconclusiveDescription")}</span><div className="form-actions">
                  {declaredModel && <button type="button" className="button ghost" onClick={() => { resetDetection(); setManualDeclaration(true); }}>{t("deployments.advancedManualDeclaration")}</button>}
                  {verification && <button type="button" className="button ghost" onClick={resetDetection}>{t("deployments.keepCatalogueClaims")}</button>}
                </div></div>}
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
                  {/* A verification that could not finish says nothing about the
                      model, so the claims it was checking are still the best
                      answer available and going back to them is one click. This
                      is the catalogued model's escape; manual declaration is the
                      uncovered model's, and neither is offered in the other's
                      situation. */}
                  {verification && <div className="form-actions">
                    <button type="button" className="button ghost" onClick={resetDetection}>{t("deployments.keepCatalogueClaims")}</button>
                  </div>}
                  {detection.status === "failed" && !!detection.binding_candidates?.length && <ul className="detection-candidate-outcomes">
                    {detection.binding_candidates.map((candidate) => {
                      const binding = selectableBindings.find((item) => item.id === candidate.binding_id);
                      return <li key={candidate.binding_id}>
                        <strong>{binding ? bindingLabel(binding, t) : candidate.profile_id}</strong>
                        {/* Identification is skipped outright when there is one
                            interface — nothing to tell it apart from — so this
                            candidate carries no probe by design. Reporting that
                            as "no probe was sent" reads as the failure itself. */}
                        <span>{candidate.capability
                          ? t("deployments.detectionCandidateOutcome", {
                            capability: t(`capabilities.${candidate.capability}`),
                            status: t(`deployments.detectionProbeStatus.${candidate.status}`),
                          })
                          : detection.binding_candidates.length === 1
                            ? t("deployments.detectionCandidateSingleInterface")
                            : t("deployments.detectionCandidateNotAsked")}</span>
                        <small>{t("deployments.detectionCandidateVerifiable", {
                          capabilities: (candidate.verifiable ?? []).map((name) => t(`capabilities.${name}`)).join("、") || t("deployments.detectionCandidateVerifiableNone"),
                        })}</small>
                      </li>;
                    })}
                  </ul>}
                  {/* The outcomes themselves. Identification answers which
                      interface, never why the capabilities did not establish,
                      so without this the reason a detection failed was in the
                      record and nowhere on the screen. */}
                  {!!failedProbeOutcomes.length && <>
                    <span>{t("deployments.detectionProbeOutcomesTitle")}</span>
                    <ul className="detection-candidate-outcomes">
                      {failedProbeOutcomes.map((outcome) => <li key={outcome.name}>
                        <strong>{t("deployments.detectionCandidateOutcome", {
                          capability: t(`capabilities.${outcome.name}`),
                          status: t(`deployments.detectionProbeStatus.${outcome.status}`),
                        })}</strong>
                        {!!outcome.errorClass && <span>{t(`testControl.reasons.${outcome.errorClass}`, { defaultValue: t("testControl.reasons.unknown") })}</span>}
                      </li>)}
                    </ul>
                  </>}
                  {detection.status === "failed" && !failedProbeOutcomes.length && <span>{t("deployments.detectionProbeNoneRan")}</span>}
                  {declaredModel && <div className="form-actions"><button type="button" className="button ghost" onClick={() => { resetDetection(); setManualDeclaration(true); }}>{t("deployments.advancedManualDeclaration")}</button></div>}
                </div>}
                {!!refusedCapabilities.length && <div className="notice" role="status">
                  <strong>{t("deployments.detectionRefusedTitle", {
                    capabilities: refusedCapabilities.map((outcome) => t(`capabilities.${outcome.name}`)).join(t("common.listSeparator")),
                  })}</strong>
                  <span>{t("deployments.detectionRefusedDescription")}</span>
                  {refusedCapabilities.some((outcome) => outcome.providerStatus || outcome.providerCode) && <ul className="detection-candidate-outcomes">
                    {refusedCapabilities.map((outcome) => <li key={outcome.name}>
                      <strong>{t(`capabilities.${outcome.name}`)}</strong>
                      {/* Identifiers only — the upstream's own sentence never
                          reaches this cell, here as everywhere else. */}
                      <small className="technical">{t("deployments.detectionProbeUpstream", {
                        status: outcome.providerStatus || t("common.unknown"),
                        code: outcome.providerCode || t("common.none"),
                      })}</small>
                    </li>)}
                  </ul>}
                </div>}
                {/* What a model cannot do is the complement of the capability
                    editor below, so listing it twice only adds rows nobody acts
                    on. What a probe failed to establish is not that complement:
                    it is the one outcome the editor cannot express, so it is
                    the only one still named here. */}
                {!!unestablishedCapabilities.length && <div className="notice" role="status">
                  <strong>{t("deployments.detectionUnestablishedTitle", {
                    capabilities: unestablishedCapabilities.map((outcome) => t(`capabilities.${outcome.name}`)).join(t("common.listSeparator")),
                  })}</strong>
                  <span>{t("deployments.detectionUnestablishedDescription")}</span>
                  {/* Same rows the failed-detection panel already draws, on the
                      completed path that had none. A capability with no recorded
                      error class is one the upstream answered without proving
                      anything, and has nothing to add here. */}
                  {unestablishedCapabilities.some((outcome) => outcome.errorClass) && <ul className="detection-candidate-outcomes">
                    {unestablishedCapabilities.filter((outcome) => outcome.errorClass).map((outcome) => <li key={outcome.name}>
                      <strong>{t("deployments.detectionCandidateOutcome", {
                        capability: t(`capabilities.${outcome.name}`),
                        status: t(`deployments.detectionProbeStatus.${outcome.status}`),
                      })}</strong>
                      <span>{t(`testControl.reasons.${outcome.errorClass}`, { defaultValue: t("testControl.reasons.unknown") })}</span>
                      {/* The class says what kind of failure; these say which
                          request and which field. Identifiers only — the
                          upstream's own sentence never reaches this cell. */}
                      {(outcome.providerStatus || outcome.providerCode) && <small className="technical">{t("deployments.detectionProbeUpstream", {
                        status: outcome.providerStatus || t("common.unknown"),
                        code: outcome.providerCode || t("common.none"),
                      })}</small>}
                    </li>)}
                  </ul>}
                </div>}
                {detection?.status === "completed" && verification && <div className="notice">
                  <strong>{t("deployments.verificationDoneTitle", { count: verification.measured.length })}</strong>
                  {verification.gained.length > 0 && <span>{t("deployments.verificationGained", { capabilities: verification.gained.map((name) => t(`capabilities.${name}`)).join(t("common.dotSeparator")) })}</span>}
                  {verification.lost.length > 0 && <span>{t("deployments.verificationLost", { capabilities: verification.lost.map((name) => t(`capabilities.${name}`)).join(t("common.dotSeparator")) })}</span>}
                  {/* Said out loud, because it is the half a measured set is
                      easiest to misread: what no probe could ask about still
                      stands on the catalogue's claim, and is not a finding. */}
                  {verification.carried.length > 0 && <span>{t("deployments.verificationCarried", { capabilities: verification.carried.map((name) => t(`capabilities.${name}`)).join(t("common.dotSeparator")) })}</span>}
                  {verification.gained.length === 0 && verification.lost.length === 0 && <span>{t("deployments.verificationAgrees")}</span>}
                </div>}
                {detection?.status === "completed" && detection.expires_at && <small className="technical detection-freshness">{t("deployments.detectionFreshUntil", { date: dateTime(detection.expires_at) })}</small>}
                {(detectCapabilities.isError || detectionQuery.isError || cancelDetection.isError) && <ErrorState error={detectCapabilities.error || detectionQuery.error || cancelDetection.error} />}
                </>}
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
                  // The counter reads as "how many of the ones you can turn on",
                  // so its denominator has to exclude the ones this form cannot
                  // turn on at all. Counting them in read as three unticked boxes
                  // when they are three boxes belonging to another screen.
                  const tickable = names.filter((name) => capabilityCeiling[name] || capabilities[name]);
                  const blocked = names.length - tickable.length;
                  const selected = tickable.filter((name) => capabilities[name]).length;
                  return <section className="deployment-capability-group" aria-labelledby={`capability-group-${group.id}`} key={group.id}>
                    <header>
                      <strong id={`capability-group-${group.id}`}>{t(`deployments.capabilityGroups.${group.id}.title`)}</strong>
                      <span>
                        {t("deployments.capabilityGroupSelected", { selected, total: tickable.length })}
                        {blocked > 0 && ` · ${t("deployments.capabilityGroupBlocked", { count: blocked })}`}
                      </span>
                    </header>
                    <div className="deployment-capabilities capability-grid" data-count={names.length}>
                      {names.map((name) => {
                        const unavailable = !capabilityCeiling[name];
                        const reason = capabilityUnavailableReason(name);
                        return <label className={`capability-option ${unavailable ? "unavailable" : ""}`} key={name}>
                          <input
                            type="checkbox"
                            disabled={unavailable && !capabilities[name]}
                            checked={capabilities[name]}
                            onChange={(event) => changeCapabilities(updateCapabilitySelection(catalogReady, capabilities, name, event.target.checked))}
                          />
                          <span>
                            {t(`capabilities.${name}`)}
                            {unavailable && <small>{t(reason)}</small>}
                            {!unavailable && claimSourcesDiffer && claimSourceByCapability.has(name) && (
                              <small className="capability-claim-source">{t(`deployments.capabilitySources.${claimSourceByCapability.get(name)}`)}</small>
                            )}
                          </span>
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
            {/* Not a warning: turning on a capability the catalogue never
                recorded is the supported way to deploy a model Halro has no
                card for. What it changes is who is answering for it, and that
                has to be said before the save rather than discovered in the
                evidence column afterwards. */}
            {declaringBeyondVariant && <div className="notice deployment-capability-declaration" aria-live="polite">
              <strong>{t("deployments.declareBeyondCatalogTitle")}</strong>
              <span>{t("deployments.declareBeyondCatalogDescription")}</span>
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
                  <input autoComplete="off" min="0" type="number" value={capabilities.max_context_tokens} onChange={(event) => setCapabilities({ ...capabilities, max_context_tokens: Number(event.target.value) })} />
                </Field>
                <Field label={t("deployments.maxOutputTokens")} hint={t("deployments.maxOutputHint")}>
                  <input autoComplete="off" min="0" type="number" value={capabilities.max_output_tokens} onChange={(event) => setCapabilities({ ...capabilities, max_output_tokens: Number(event.target.value) })} />
                </Field>
                <Field label={t("deployments.concurrencyLimit")} hint={t("deployments.concurrencyHint")}><input autoComplete="off" min="0" type="number" value={maxConcurrency} onChange={(event) => setMaxConcurrency(Number(event.target.value))} /></Field>
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

/**
 * The capability editor for a model the catalogue resolved.
 *
 * `ceiling` is what the interface can carry and decides which rows exist;
 * `capabilities` is what the deployment claims and decides which are ticked.
 * Nothing is ever pre-ticked from the ceiling, because filling a form from what
 * an interface permits is how deployments came to claim capabilities their
 * model does not have.
 *
 * The two used to be the same value, and the row list was the narrower one. A
 * model whose catalogue entry says chat and streaming rendered two rows, so
 * vision — carried by the interface, present in the model, absent from the
 * entry — could not be turned on here at all. Listing the ceiling keeps a
 * capability that is merely unrecorded available to an operator who knows
 * better.
 */
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
    fetched_image: false,
    json_object: false,
    structured_outputs: false,
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

