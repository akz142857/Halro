import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState, type FormEvent, type KeyboardEvent } from "react";
import { api } from "../api";
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
} from "../components";
import type { InlineTestState } from "../components";
import { dateTime, money } from "../format";
import type { Deployment, DeploymentPriceProposal, DeploymentPriceVersion, DeploymentTargetKind, Provider, ProviderBinding, ProviderCapabilities } from "../types";
import { useTranslation } from "react-i18next";
import { Link } from "../navigation";

export function DeploymentsPage() {
  const { t } = useTranslation();
  const [editing, setEditing] = useState<Deployment | null | "new">(null);
  const [replacement, setReplacement] = useState<Deployment>();
  const [query, setQuery] = useState("");
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
        action={<button className="button primary" onClick={() => { setReplacement(undefined); setEditing("new"); }}>{t("deployments.create")}</button>}
      />
      {(deployments.isPending || providers.isPending || routes.isPending) && <Loading />}
      {(deployments.isError || providers.isError || routes.isError) && <ErrorState error={deployments.error || providers.error || routes.error} />}
      {deployments.data?.items.length === 0 && (
        <EmptyState title={t("deployments.emptyTitle")}>{t("deployments.emptyDescription")}</EmptyState>
      )}
      {!!deployments.data?.items.length && (
        <>
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
            <section className="deployment-grid" aria-label={t("deployments.list")}>
              {filteredDeployments.map((deployment) => (
                <DeploymentCard
                  key={deployment.id}
                  deployment={deployment}
                  providerName={providerNames.get(deployment.provider_id) || deployment.provider_id}
                  activeRouteCount={activeRouteCounts.get(deployment.id) ?? 0}
                  onEdit={() => setEditing(deployment)}
                  onReplace={() => { setReplacement(deployment); setEditing("new"); }}
                />
              ))}
            </section>
          ) : <EmptyState title={t("deployments.noMatches")}>{t("deployments.noMatchesDescription")}</EmptyState>}
        </>
      )}
      {editing && (
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

function DeploymentCard({
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
  const [expanded, setExpanded] = useState(false);
  const [pricing, setPricing] = useState(false);
	const [suggesting, setSuggesting] = useState(false);
	const [reviewing, setReviewing] = useState<DeploymentPriceProposal>();
	const [confirmingRestore, setConfirmingRestore] = useState(false);
  const queryClient = useQueryClient();
  const prices = useQuery({ queryKey: ["deployment-prices", deployment.id], queryFn: () => api.deploymentPrices(deployment.id), enabled: expanded });
	const proposals = useQuery({ queryKey: ["deployment-price-proposals", deployment.id], queryFn: () => api.deploymentPriceProposals(deployment.id), enabled: expanded });
  const activePrice = prices.data?.items.find((price) => price.status === "active");
  const scheduledPrices = prices.data?.items.filter((price) => price.status === "scheduled") ?? [];
  const cancelPrice = useMutation({ mutationFn: (price: DeploymentPriceVersion) => api.cancelDeploymentPrice(deployment.id, price.id, price.revision), onSuccess: () => queryClient.invalidateQueries({ queryKey: ["deployment-prices", deployment.id] }) });
	const rejectProposal = useMutation({ mutationFn: (proposal: DeploymentPriceProposal) => api.rejectDeploymentPriceProposal(deployment.id, proposal.id, proposal.revision), onSuccess: () => queryClient.invalidateQueries({ queryKey: ["deployment-price-proposals", deployment.id] }) });
  const test = useMutation({
    mutationFn: () => api.testDeployment(deployment.id),
    onSettled: () => queryClient.invalidateQueries({ queryKey: ["deployments"] }),
  });
  const remove = useMutation({
    mutationFn: () => api.deleteDeployment(deployment.id, deployment.revision),
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
  const testState: InlineTestState = test.isPending
    ? "running"
    : testFailed
      ? "failure"
      : testIsCurrent
        ? "success"
        : deployment.last_test_status === "healthy"
          ? "stale"
          : "idle";
  const evidence = evidenceSummary(deployment.capability_evidence).map((value) => t(`deployments.evidenceValues.${value}`));
  const routeBlocked = activeRouteCount > 0;
  return (
    <article className="deployment-card">
      <header className="deployment-compact-row">
        <span className="deployment-identity"><StatusDot ok={deployment.enabled} /><span><strong>{deployment.name}</strong><small>{providerName}</small></span></span>
        <span className="deployment-compact-target"><small>{t("deployments.upstreamTarget")}</small><code>{deployment.provider_model}</code></span>
        <span className="deployment-compact-metric"><small>{t("deployments.concurrency")}</small><strong>{deployment.max_concurrency || t("deployments.unlimited")}</strong></span>
        <span className="deployment-compact-metric"><small>{t("deployments.routeDependency")}</small><Link href="/admin/routes">{activeRouteCount ? t("deployments.activeRoutesCompact", { count: activeRouteCount }) : t("deployments.noActiveRoutes")}</Link></span>
        <span className="deployment-status-stack">
          <span className={`resource-state ${deployment.enabled ? "enabled" : ""}`}>{deployment.enabled ? t("common.enabled") : t("common.disabled")}</span>
        </span>
        <button className="button ghost deployment-expand" aria-expanded={expanded} aria-controls={`deployment-details-${deployment.id}`} onClick={() => setExpanded((value) => !value)}>
          {expanded ? t("deployments.collapseDetails") : t("deployments.expandDetails")}
        </button>
      </header>
      {expanded && <div id={`deployment-details-${deployment.id}`} className="deployment-details">
      <div className="deployment-summary">
        <div className="deployment-model">
        <small>{t("deployments.upstreamTarget")}</small>
        <strong>{deployment.provider_model}</strong>
        {deployment.region && <small>{deployment.region}</small>}
        </div>
        <div className="deployment-route-dependency">
          <small>{t("deployments.routeDependency")}</small>
          <Link href="/admin/routes">{activeRouteCount ? t("deployments.activeRoutes", { count: activeRouteCount }) : t("deployments.noActiveRoutes")}</Link>
        </div>
      </div>
      <dl className="deployment-facts">
        <DeploymentFact label={t("deployments.priceStatus")} value={activePrice ? activePrice.billing_mode === "free" ? t("deployments.freePrice") : t("deployments.versionedPrice") : prices.isPending ? t("common.loading") : t("deployments.unknownPrice")} meta={activePrice ? `v${activePrice.version} · ${dateTime(activePrice.effective_from)}` : t("deployments.priceRequired")} unset={!activePrice} />
        {(deployment.capabilities.chat || deployment.capabilities.embeddings) && <DeploymentFact label={t("deployments.inputPrice")} value={activePrice ? money(activePrice.input_micros_per_million) : t("deployments.notConfigured")} meta={t("deployments.perMillionTokens")} unset={!activePrice} />}
        {(deployment.capabilities.chat || deployment.capabilities.embeddings) && <DeploymentFact label={t("deployments.outputPrice")} value={activePrice ? money(activePrice.output_micros_per_million) : t("deployments.notConfigured")} meta={t("deployments.perMillionTokens")} unset={!activePrice} />}
        {activePrice && <DeploymentFact label={t("deployments.fixedPrice")} value={money(activePrice.fixed_request_micros_usd)} meta={t("deployments.perRequest")} />}
        <DeploymentFact label={t("deployments.concurrency")} value={deployment.max_concurrency || t("deployments.unlimited")} meta={t("deployments.deploymentScope")} />
        <DeploymentFact label={t("deployments.context")} value={deployment.capabilities.max_context_tokens || t("deployments.upstreamApplies")} meta={deployment.capabilities.max_context_tokens ? t("deployments.tokens") : t("deployments.undeclared")} />
        <DeploymentFact label={t("deployments.maxOutput")} value={deployment.capabilities.max_output_tokens || t("deployments.upstreamApplies")} meta={deployment.capabilities.max_output_tokens ? t("deployments.tokens") : t("deployments.undeclared")} />
      </dl>
      <section className="technical-details">
        <strong>{t("deployments.priceTimeline")}</strong>
        {activePrice && <small>{t("deployments.priceSourceSummary", { type: activePrice.source.type, assurance: activePrice.source.assurance, reference: activePrice.source.reference || "—" })}</small>}
        {scheduledPrices.map((price) => <div key={price.id}><code>v{price.version}</code> · {dateTime(price.effective_from)} · {price.billing_mode} <button className="button ghost" disabled={cancelPrice.isPending} onClick={() => cancelPrice.mutate(price)}>{t("common.cancel")}</button></div>)}
        <button className="button ghost" onClick={() => setPricing(true)}>{t("deployments.newPriceVersion")}</button>
      </section>
	  {deployment.pricing_quarantined && <div className="notice warning"><strong>{t("deployments.pricingQuarantined")}</strong><span>{deployment.pricing_quarantine_reason}</span><button className="button ghost" onClick={() => setConfirmingRestore(true)}>{t("deployments.confirmRestoredPricing")}</button></div>}
	  <section className="technical-details">
		<strong>{t("deployments.priceProposals")}</strong>
		<p><small>{t("deployments.proposalSafety")}</small></p>
		{proposals.data?.items.filter((proposal) => proposal.status === "pending").map((proposal) => <div key={proposal.id}>
		  <code>{proposal.match}</code> · {proposal.source.assurance} · {proposal.source.reference || proposal.source.uri || "—"} · {t("deployments.proposalExpires", { time: dateTime(proposal.expires_at) })}
		  {proposal.warnings?.map((warning) => <small key={warning}> ⚠ {warning}</small>)}
		  <button className="button ghost" disabled={proposal.match === "ambiguous" || new Date(proposal.expires_at) <= new Date()} onClick={() => setReviewing(proposal)}>{t("deployments.reviewProposal")}</button>
		  <ConfirmButton className="button ghost" label={t("deployments.rejectProposal")} confirmLabel={t("deployments.rejectProposalConfirm")} onConfirm={() => rejectProposal.mutate(proposal)} />
		</div>)}
		{!proposals.isPending && !proposals.data?.items.some((proposal) => proposal.status === "pending") && <small>{t("deployments.noPendingProposals")}</small>}
		<button className="button ghost" onClick={() => setSuggesting(true)}>{t("deployments.importProposal")}</button>
	  </section>
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
      <footer>
        <InlineTestControl state={testState} latency={deployment.last_test_latency_millis} title={deployment.last_tested_at ? t("deployments.lastTest", { time: dateTime(deployment.last_tested_at), latency: deployment.last_test_latency_millis ?? 0 }) : undefined} onTest={() => test.mutate()} />
        <div className="row-actions">
          {deployment.enabled ? (
            <ConfirmButton className="button ghost" label={t("common.disable")} title={t("deployments.disableTitle")} confirmLabel={t("deployments.disableConfirm", { name: deployment.name })} disabled={state.isPending || routeBlocked} onConfirm={() => state.mutate()} />
          ) : (
            <button className="button primary" title={!testIsCurrent ? t("deployments.testRequired") : undefined} disabled={state.isPending || !testIsCurrent} onClick={() => state.mutate()}>{t("common.enable")}</button>
          )}
          <button className="button ghost" onClick={onEdit}>{t("common.edit")}</button>
          <OverflowMenu label={t("deployments.moreActions")}>
            <button className="button ghost" onClick={onReplace}>{t("deployments.createReplacement")}</button>
            <ConfirmButton label={t("common.delete")} confirmLabel={t("deployments.deleteConfirm", { name: deployment.name })} onConfirm={() => remove.mutate()} disabled={remove.isPending || routeBlocked} />
          </OverflowMenu>
        </div>
      </footer>
      {(remove.isError || state.isError) && <ErrorState error={remove.error || state.error} />}
      {pricing && <PriceVersionForm deployment={deployment} current={activePrice} onClose={() => setPricing(false)} />}
	  {suggesting && <PriceProposalForm deployment={deployment} onClose={() => setSuggesting(false)} />}
	  {reviewing && <PriceProposalReview deployment={deployment} proposal={reviewing} onClose={() => setReviewing(undefined)} />}
	  {confirmingRestore && <RestorePricingConfirm deployment={deployment} onClose={() => setConfirmingRestore(false)} />}
    </article>
  );
}

function PriceProposalForm({ deployment, onClose }: { deployment: Deployment; onClose: () => void }) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const [input, setInput] = useState("0");
	const [output, setOutput] = useState("0");
	const [fixed, setFixed] = useState("0");
	const [uri, setURI] = useState("");
	const [reference, setReference] = useState("");
	const [digest, setDigest] = useState("");
	const [match, setMatch] = useState<"exact" | "likely" | "ambiguous">("likely");
	const idempotencyKey = useRef(crypto.randomUUID());
	const mutation = useMutation({
		mutationFn: () => api.createDeploymentPriceProposal(deployment.id, {
			billing_mode: "metered", currency: "USD", input_usd_per_million: input, output_usd_per_million: output,
			fixed_request_usd: fixed, match, expires_at: new Date(Date.now() + 7 * 86400_000).toISOString(),
			warnings: [t("deployments.importedProposalWarning")], source: { type: "official_url", uri, reference, content_sha256: digest, retrieved_at: new Date().toISOString() },
		}, idempotencyKey.current),
		onSuccess: () => { queryClient.invalidateQueries({ queryKey: ["deployment-price-proposals", deployment.id] }); onClose(); },
	});
	return <Modal title={t("deployments.importProposal")} onClose={onClose}><form onSubmit={(event) => { event.preventDefault(); mutation.mutate(); }}>
		<Field label={t("deployments.inputUSD")}><input required value={input} onChange={(event) => setInput(event.target.value)} /></Field>
		<Field label={t("deployments.outputUSD")}><input required value={output} onChange={(event) => setOutput(event.target.value)} /></Field>
		<Field label={t("deployments.fixedRequestUSD")}><input required value={fixed} onChange={(event) => setFixed(event.target.value)} /></Field>
		<Field label={t("deployments.proposalMatch")}><select value={match} onChange={(event) => setMatch(event.target.value as typeof match)}><option value="exact">exact</option><option value="likely">likely</option><option value="ambiguous">ambiguous</option></select></Field>
		<Field label={t("deployments.sourceURL")}><input type="url" required value={uri} onChange={(event) => setURI(event.target.value)} /></Field>
		<Field label={t("deployments.sourceReference")}><input required value={reference} onChange={(event) => setReference(event.target.value)} /></Field>
		<Field label={t("deployments.sourceDigest")}><input required pattern="sha256:[a-fA-F0-9]{64}" value={digest} onChange={(event) => setDigest(event.target.value)} /></Field>
		<p>{t("deployments.proposalSafety")}</p>{mutation.isError && <ErrorState error={mutation.error} />}
		<button className="button primary" disabled={mutation.isPending}>{t("deployments.saveProposal")}</button>
	</form></Modal>;
}

function PriceProposalReview({ deployment, proposal, onClose }: { deployment: Deployment; proposal: DeploymentPriceProposal; onClose: () => void }) {
	const { t } = useTranslation();
	const queryClient = useQueryClient();
	const [effective, setEffective] = useState(new Date(Date.now() + 60_000).toISOString().slice(0, 16));
	const [password, setPassword] = useState("");
	const [totp, setTotp] = useState("");
	const mutation = useMutation({
		mutationFn: () => api.adoptDeploymentPriceProposal(deployment.id, proposal.id, proposal.revision, { effective_from: new Date(effective).toISOString(), confirm: true, current_password: password, totp_code: totp }),
		onSuccess: () => { queryClient.invalidateQueries({ queryKey: ["deployment-price-proposals", deployment.id] }); queryClient.invalidateQueries({ queryKey: ["deployment-prices", deployment.id] }); onClose(); },
	});
	return <Modal title={t("deployments.reviewProposal")} onClose={onClose}><form onSubmit={(event) => { event.preventDefault(); mutation.mutate(); }}>
		<p><strong>{proposal.provider_model}</strong> · {proposal.region || "—"} · {proposal.tier || "—"} · {proposal.match}</p>
		<p>{money(proposal.input_micros_per_million)} / {money(proposal.output_micros_per_million)} · {proposal.source.assurance}</p>
		<p>{dateTime(proposal.fetched_at)} · {proposal.source.type} · {proposal.source.reference || proposal.source.uri || "—"}<br/><code>{proposal.source.content_sha256}</code></p>
		<p><code>{proposal.digest}</code></p>
		<Field label={t("deployments.effectiveFrom")}><input type="datetime-local" required value={effective} onChange={(event) => setEffective(event.target.value)} /></Field>
		<Field label={t("usage.currentPassword")}><input type="password" required value={password} onChange={(event) => setPassword(event.target.value)} /></Field>
		<Field label={t("usage.totpOptional")}><input inputMode="numeric" value={totp} onChange={(event) => setTotp(event.target.value)} /></Field>
		<p>{t("deployments.adoptProposalWarning")}</p>{mutation.isError && <ErrorState error={mutation.error} />}
		<button className="button primary" disabled={mutation.isPending}>{t("deployments.adoptProposal")}</button>
	</form></Modal>;
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
  const queryClient = useQueryClient();
  const [mode, setMode] = useState<"metered" | "free">("metered");
  const [input, setInput] = useState("0");
  const [output, setOutput] = useState("0");
  const [fixed, setFixed] = useState("0");
  const [effective, setEffective] = useState(new Date(Date.now() + 60_000).toISOString().slice(0, 16));
  const [reference, setReference] = useState("");
  const [digest, setDigest] = useState("");
  const [password, setPassword] = useState("");
	const [totp, setTotp] = useState("");
  const idempotencyKey = useRef(crypto.randomUUID());
  const mutation = useMutation({
    mutationFn: () => api.createDeploymentPrice(deployment.id, {
      billing_mode: mode, currency: "USD",
      input_usd_per_million: mode === "free" ? "0" : input,
      output_usd_per_million: mode === "free" ? "0" : output,
      fixed_request_usd: mode === "free" ? "0" : fixed,
      effective_from: new Date(effective).toISOString(),
      source: { type: "manual", content_sha256: digest, reference, asserted_without_archive: true },
		current_password: password, totp_code: totp,
    }, idempotencyKey.current),
    onSuccess: () => { queryClient.invalidateQueries({ queryKey: ["deployment-prices", deployment.id] }); onClose(); },
  });
  const submit = (event: FormEvent) => { event.preventDefault(); mutation.mutate(); };
  return <Modal title={t("deployments.newPriceVersion")} onClose={onClose}>
    <form onSubmit={submit}>
      {current && <p>{t("deployments.currentPriceVersion", { version: current.version })}</p>}
      <Field label={t("deployments.billingMode")}><select value={mode} onChange={(event) => setMode(event.target.value as "metered" | "free")}><option value="metered">metered</option><option value="free">free</option></select></Field>
      {mode === "metered" && <><Field label={t("deployments.inputUSD")}><input required value={input} onChange={(event) => setInput(event.target.value)} /></Field><Field label={t("deployments.outputUSD")}><input required value={output} onChange={(event) => setOutput(event.target.value)} /></Field><Field label={t("deployments.fixedRequestUSD")}><input required value={fixed} onChange={(event) => setFixed(event.target.value)} /></Field></>}
      <Field label={t("deployments.effectiveFrom")}><input type="datetime-local" required value={effective} onChange={(event) => setEffective(event.target.value)} /></Field>
      <Field label={t("deployments.sourceReference")}><input required value={reference} onChange={(event) => setReference(event.target.value)} /></Field>
      <Field label={t("deployments.sourceDigest")}><input required pattern="sha256:[a-fA-F0-9]{64}" value={digest} onChange={(event) => setDigest(event.target.value)} placeholder="sha256:…" /></Field>
		<Field label={t("usage.currentPassword")}><input type="password" required value={password} onChange={(event) => setPassword(event.target.value)} /></Field>
		<Field label={t("usage.totpOptional")}><input inputMode="numeric" value={totp} onChange={(event) => setTotp(event.target.value)} /></Field>
      <p>{t("deployments.immutablePriceWarning")}</p>
      {mutation.isError && <ErrorState error={mutation.error} />}
      <button className="button primary" disabled={mutation.isPending}>{t("deployments.createPriceVersion")}</button>
    </form>
  </Modal>;
}

function evidenceSummary(evidence: Record<string, string>) {
  const values = [...new Set(Object.values(evidence).filter((value) => value !== "unsupported"))];
  return values;
}

type SelectableBinding = ProviderBinding & { id: string };

const deploymentCapabilityNames = ["chat", "streaming", "embeddings", "moderations", "images", "transcriptions", "speech", "files", "batches", "rerank", "async_generate", "tools", "vision", "json_mode", "developer_role", "reasoning", "stream_usage"] as const;

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

function defaultTargetKind(provider?: Provider): DeploymentTargetKind {
  if (provider?.type === "azure_openai") return "azure_deployment";
  if (provider?.type === "openai_compatible") return "custom_endpoint_model";
  if (provider?.type === "bedrock" && provider.access_surface !== "bedrock-mantle") return "bedrock_foundation_model";
  return "model_id";
}

function targetKinds(provider?: Provider, binding?: SelectableBinding): DeploymentTargetKind[] {
  if (provider?.type === "bedrock" && provider.access_surface !== "bedrock-mantle") {
    if (binding?.profile_id !== "bedrock.runtime.converse.text.v1") return ["bedrock_foundation_model"];
    return ["bedrock_foundation_model", "bedrock_inference_profile", "bedrock_provisioned_throughput"];
  }
  return [defaultTargetKind(provider)];
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
  const source = current ?? template;
  const enabledProviders = providers.filter((provider) => provider.enabled || provider.id === source?.provider_id);
  const [name, setName] = useState(current?.name ?? (template ? `${template.name} v2` : ""));
  const [providerID, setProviderID] = useState(source?.provider_id ?? enabledProviders[0]?.id ?? "");
  const [providerModel, setProviderModel] = useState(source?.provider_model ?? "");
  const initialProvider = enabledProviders.find((provider) => provider.id === (source?.provider_id ?? enabledProviders[0]?.id));
  const initialBindings = providerBindings(initialProvider);
  const [bindingID, setBindingID] = useState(source?.binding_id ?? initialBindings[0]?.id ?? "");
  const initialBinding = initialBindings.find((binding) => binding.id === (source?.binding_id ?? initialBindings[0]?.id));
  const [capabilities, setCapabilities] = useState<ProviderCapabilities>(source?.capabilities ?? initialBinding?.capabilities ?? initialProvider?.capabilities ?? emptyCapabilities());
  const [region, setRegion] = useState(source?.region ?? "");
  const [maxConcurrency, setMaxConcurrency] = useState(source?.max_concurrency ?? 0);
  const [targetKind, setTargetKind] = useState<DeploymentTargetKind>(source?.target_kind ?? defaultTargetKind(initialProvider));
  const queryClient = useQueryClient();
  const value = () => ({
    name: name.trim(),
    provider_id: providerID,
    ...(bindingID ? { binding_id: bindingID } : {}),
    provider_model: providerModel.trim(),
    target_kind: targetKind,
    capabilities,
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
  });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (formValid) mutation.mutate();
  };
  const selectedProvider = enabledProviders.find((item) => item.id === providerID);
  const selectableBindings = providerBindings(selectedProvider);
  const selectedBinding = selectableBindings.find((item) => item.id === bindingID) ?? selectableBindings[0];
  const capabilityCeiling = selectedBinding?.capabilities ?? selectedProvider?.capabilities ?? emptyCapabilities();
  const configurableCapabilityNames = deploymentCapabilityNames.filter((name) => capabilityCeiling[name] || capabilities[name]);
  const availableTargetKinds = targetKinds(selectedProvider, selectedBinding);
  const targetLabel = t(`deployments.targetLabels.${targetKind}`);
  const identityLocked = Boolean(current);
  const modelCatalogSupported = Boolean(
    !identityLocked
    && selectedProvider
    && ["openai", "deepseek", "openai_compatible"].includes(selectedProvider.type)
    && (targetKind === "model_id" || targetKind === "custom_endpoint_model"),
  );
  const modelCatalogKey = ["provider-models", providerID, selectedBinding?.id ?? ""] as const;
  const modelCatalog = useQuery({
    queryKey: modelCatalogKey,
    queryFn: () => api.providerModels(providerID, selectedBinding?.id ?? ""),
    enabled: modelCatalogSupported,
    staleTime: 5 * 60 * 1000,
    retry: false,
  });
  const refreshModelCatalog = useMutation({
    mutationFn: () => api.providerModels(providerID, selectedBinding?.id ?? "", true),
    onSuccess: (catalog) => queryClient.setQueryData(modelCatalogKey, catalog),
  });
  const modelPickerRef = useRef<HTMLDivElement>(null);
  const modelOptionsRef = useRef<HTMLDivElement>(null);
  const [modelPickerOpen, setModelPickerOpen] = useState(false);
  const [activeModelIndex, setActiveModelIndex] = useState(-1);
  const visibleModels = useMemo(() => {
    const query = providerModel.trim().toLocaleLowerCase();
    return (modelCatalog.data?.items ?? [])
      .filter((model) => !query || model.id.toLocaleLowerCase().includes(query) || model.owned_by?.toLocaleLowerCase().includes(query));
  }, [modelCatalog.data?.items, providerModel]);
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
  const chooseModel = (modelID: string) => {
    setProviderModel(modelID);
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
      chooseModel(visibleModels[activeModelIndex].id);
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
  const regional = selectedProvider?.access_surface === "bedrock-runtime" || selectedProvider?.access_surface === "bedrock-agent-runtime";
  const fixedPriced = capabilities.moderations || capabilities.images || capabilities.transcriptions || capabilities.speech || capabilities.files || capabilities.batches || capabilities.rerank || capabilities.async_generate;
  const anyOperation = capabilities.chat || capabilities.embeddings || fixedPriced;
  const numericValues = [maxConcurrency, capabilities.max_context_tokens, capabilities.max_output_tokens];
  const tokenLimitsValid = capabilities.max_context_tokens === 0 || capabilities.max_output_tokens <= capabilities.max_context_tokens;
  const formValid = Boolean(name.trim() && providerID && providerModel.trim() && anyOperation && numericValues.every((value) => Number.isFinite(value) && value >= 0) && tokenLimitsValid);
  return (
    <Modal wide title={current ? t("deployments.edit") : template ? t("deployments.createReplacementTitle") : t("deployments.createTitle")} onClose={onClose}>
      {enabledProviders.length === 0 ? (
        <div className="notice warning"><strong>{t("deployments.providerRequired")}</strong><span>{t("deployments.providerRequiredDescription")}</span><Link className="notice-link" href="/admin/providers">{t("deployments.openProviders")}</Link></div>
      ) : (
        <form className="deployment-form" onSubmit={submit}>
          <section className="deployment-form-section">
            <header><h3>{t("deployments.targetSection")}</h3><p>{t("deployments.targetSectionDescription")}</p></header>
            <div className="form-grid deployment-target-grid">
          <Field label={t("deployments.name")}><input autoFocus required value={name} onChange={(event) => setName(event.target.value)} /></Field>
          <Field label={t("deployments.provider")}>
            <select required disabled={identityLocked} value={providerID} onChange={(event) => {
              const next = event.target.value;
              setProviderID(next);
              const provider = enabledProviders.find((item) => item.id === next);
              if (provider) {
                const binding = providerBindings(provider)[0];
                setBindingID(binding?.id ?? "");
                setCapabilities(binding?.capabilities ?? provider.capabilities);
                setTargetKind(defaultTargetKind(provider));
                setProviderModel("");
                setRegion("");
              }
            }}>
              {enabledProviders.map((provider) => <option key={provider.id} value={provider.id}>{provider.name}</option>)}
            </select>
          </Field>
          {(() => {
            const provider = enabledProviders.find((item) => item.id === providerID);
            const bindings = providerBindings(provider);
            return bindings.length > 1 ? (
              <Field label={t("deployments.binding")} hint={t("deployments.bindingHint")}>
                <select disabled={identityLocked} value={bindingID} onChange={(event) => {
                  const next = event.target.value;
                  setBindingID(next);
                  const binding = bindings.find((item) => item.id === next);
                  if (binding) {
                    setCapabilities(binding.capabilities);
                    setProviderModel("");
                    setRegion("");
                  }
                }}>
                  {bindings.map((binding) => <option value={binding.id} key={binding.id}>{bindingLabel(binding, t)}</option>)}
                </select>
              </Field>
            ) : null;
          })()}
          {availableTargetKinds.length > 1 && <Field label={t("deployments.targetKind")} hint={t("deployments.targetKindHint")}>
            <select disabled={identityLocked} value={targetKind} onChange={(event) => { setTargetKind(event.target.value as DeploymentTargetKind); setProviderModel(""); }}>
              {availableTargetKinds.map((kind) => <option value={kind} key={kind}>{t(`deployments.targetKinds.${kind}`)}</option>)}
            </select>
          </Field>}
          <div className="deployment-model-picker" ref={modelPickerRef}>
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
                onChange={(event) => { setProviderModel(event.target.value); setModelPickerOpen(true); setActiveModelIndex(-1); }}
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
                        key={model.id}
                        role="option"
                        aria-selected={providerModel === model.id}
                        type="button"
                        onMouseDown={(event) => event.preventDefault()}
                        onMouseEnter={() => setActiveModelIndex(index)}
                        onClick={() => chooseModel(model.id)}
                      >
                        <strong>{model.id}</strong>
                        {model.owned_by && <small>{model.owned_by}</small>}
                      </button>
                    )) : (
                      <div className="deployment-model-empty">{modelCatalog.isPending ? t("deployments.modelCatalogLoading") : t("deployments.noModelMatches")}</div>
                    )}
                  </div>
                )}
              </div>
              <small>{t(`deployments.targetHints.${targetKind}`)}</small>
            </div>
            {modelCatalogSupported && (
              <div className="deployment-model-catalog-status" role="status">
                <span>
                  {modelCatalog.isPending
                    ? t("deployments.modelCatalogLoading")
                    : modelCatalog.isError || refreshModelCatalog.isError
                      ? t("deployments.modelCatalogUnavailable")
                      : <>{t("deployments.modelCatalogCountPrefix")} <strong className="deployment-model-count">{modelCatalog.data?.items.length ?? 0}</strong> {t("deployments.modelCatalogCountSuffix")}</>}
                </span>
                <button
                  className="button ghost deployment-model-refresh"
                  type="button"
                  disabled={modelCatalog.isPending || refreshModelCatalog.isPending}
                  onClick={() => refreshModelCatalog.mutate()}
                >
                  {refreshModelCatalog.isPending ? t("deployments.refreshingModels") : t("deployments.refreshModels")}
                </button>
              </div>
            )}
          </div>
          {regional && <Field label={t("deployments.region")} hint={t("deployments.regionHint")}><input disabled={identityLocked} value={region} placeholder={t("deployments.regionAutomatic")} onChange={(event) => setRegion(event.target.value)} /></Field>}
          {identityLocked && <div className="notice"><strong>{t("deployments.targetLocked")}</strong><span>{t("deployments.targetLockedDescription")}</span></div>}
            </div>
          </section>
          <section className="deployment-form-section">
            <header><h3>{t("deployments.capabilitySection")}</h3><p>{t("deployments.capabilitySectionDescription")}</p></header>
            <div className="capability-disclosure capability-advanced">
              <header><span>{t("providers.advancedCapabilities")}</span><strong>{t("providers.selectedCapabilities", { count: selectedCapabilityNames.length })}</strong></header>
              <p className="capability-advanced-note">{t("deployments.inheritedCapabilitiesHint")}</p>
              <fieldset className="deployment-capabilities capability-grid" id="deployment-capability-editor">
                <legend className="visually-hidden">{t("deployments.capabilitySubset")}</legend>
                {configurableCapabilityNames.map((name) => {
                  const unavailable = !capabilityCeiling[name];
                  return (
                  <label className={`capability-option ${unavailable ? "unavailable" : ""}`} key={name}>
                    <input
                      type="checkbox"
                      disabled={unavailable && !capabilities[name]}
                      checked={capabilities[name]}
                      onChange={(event) => setCapabilities(updateDeploymentCapability(capabilities, name, event.target.checked))}
                    />
                    <span>{t(`capabilities.${name}`)}{unavailable && <small>{t("providers.unsupportedByInterface")}</small>}</span>
                  </label>
                  );
                })}
              </fieldset>
            </div>
            {!anyOperation && <div className="notice warning"><span>{t("deployments.operationRequired")}</span></div>}
          </section>
          <section className="deployment-form-section deployment-token-section">
            <header><h3>{t("deployments.tokenLimitSection")}</h3><p>{t("deployments.tokenLimitSectionDescription")}</p></header>
            <div className="form-grid deployment-token-grid">
              <Field label={t("deployments.maxContext")} hint={t("deployments.maxContextHint")}>
                <input min="0" type="number" value={capabilities.max_context_tokens} onChange={(event) => setCapabilities({ ...capabilities, max_context_tokens: Number(event.target.value) })} />
              </Field>
              <Field label={t("deployments.maxOutputTokens")} hint={t("deployments.maxOutputHint")}>
                <input min="0" type="number" value={capabilities.max_output_tokens} onChange={(event) => setCapabilities({ ...capabilities, max_output_tokens: Number(event.target.value) })} />
              </Field>
            </div>
            {!tokenLimitsValid && <div className="notice warning"><span>{t("deployments.tokenLimitInvalid")}</span></div>}
          </section>
          <section className="deployment-form-section">
            <header><h3>{t("deployments.capacityCostSection")}</h3><p>{t("deployments.capacityCostSectionDescription")}</p></header>
            <div className="form-grid deployment-capacity-grid">
              <Field label={t("deployments.concurrencyLimit")} hint={t("deployments.concurrencyHint")}><input min="0" type="number" value={maxConcurrency} onChange={(event) => setMaxConcurrency(Number(event.target.value))} /></Field>
            </div>
          </section>
          <div className="deployment-release-note"><strong>{current?.enabled ? t("deployments.updateLiveWarning") : t("deployments.savedDisabled")}</strong><span>{current?.enabled ? t("deployments.updateLiveDescription") : t("deployments.savedDisabledDescription")}</span></div>
          {mutation.isError && <ErrorState error={mutation.error} />}
          <div className="form-actions deployment-form-actions">
            <button type="button" className="button ghost" onClick={onClose}>{t("common.cancel")}</button>
            <button className="button primary" disabled={mutation.isPending || !formValid}>{current ? t("deployments.save") : template ? t("deployments.saveReplacement") : t("deployments.saveDisabled")}</button>
          </div>
        </form>
      )}
    </Modal>
  );
}

function emptyCapabilities(): ProviderCapabilities {
  return {
    chat: true,
    streaming: true,
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
