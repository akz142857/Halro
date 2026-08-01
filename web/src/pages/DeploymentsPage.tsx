import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState, type FormEvent } from "react";
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
import { money } from "../format";
import type { Deployment, Provider, ProviderCapabilities } from "../types";
import { useTranslation } from "react-i18next";

export function DeploymentsPage() {
  const { t } = useTranslation();
  const [editing, setEditing] = useState<Deployment | null | "new">(null);
  const deployments = useQuery({ queryKey: ["deployments"], queryFn: api.deployments });
  const providers = useQuery({ queryKey: ["providers"], queryFn: api.providers });
  const providerNames = useMemo(
    () => new Map(providers.data?.items.map((provider) => [provider.id, provider.name]) ?? []),
    [providers.data],
  );
  return (
    <>
      <PageHeader
        eyebrow={t("deployments.eyebrow")}
        title={t("deployments.title")}
        description={t("deployments.description")}
        action={<button className="button primary" onClick={() => setEditing("new")}>{t("deployments.create")}</button>}
      />
      {(deployments.isPending || providers.isPending) && <Loading />}
      {(deployments.isError || providers.isError) && <ErrorState error={deployments.error || providers.error} />}
      {deployments.data?.items.length === 0 && (
        <EmptyState title={t("deployments.emptyTitle")}>{t("deployments.emptyDescription")}</EmptyState>
      )}
      {!!deployments.data?.items.length && (
        <section className="deployment-grid" aria-label={t("deployments.list")}>
          {deployments.data.items.map((deployment) => (
            <DeploymentCard
              key={deployment.id}
              deployment={deployment}
              providerName={providerNames.get(deployment.provider_id) || deployment.provider_id}
              onEdit={() => setEditing(deployment)}
            />
          ))}
        </section>
      )}
      {editing && (
        <DeploymentForm
          current={editing === "new" ? undefined : editing}
          providers={providers.data?.items ?? []}
          onClose={() => setEditing(null)}
        />
      )}
    </>
  );
}

function DeploymentCard({
  deployment,
  providerName,
  onEdit,
}: {
  deployment: Deployment;
  providerName: string;
  onEdit: () => void;
}) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [probe, setProbe] = useState("");
  const test = useMutation({
    mutationFn: () => api.testDeployment(deployment.id),
    onSuccess: (result) => setProbe(t("deployments.healthy", { latency: result.latency_ms })),
    onError: () => setProbe(t("deployments.unhealthy")),
  });
  const remove = useMutation({
    mutationFn: () => api.deleteDeployment(deployment.id, deployment.revision),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["deployments"] }),
  });
  const capabilities = Object.entries(deployment.capabilities)
    .filter(([, enabled]) => typeof enabled === "boolean" && enabled)
    .map(([name]) => name);
  return (
    <article className="deployment-card">
      <header>
        <span><StatusDot ok={deployment.enabled} /><strong>{deployment.name}</strong></span>
        <span className="badge">P{deployment.priority} · W{deployment.weight}</span>
      </header>
      <div className="deployment-model">
        <small>{providerName}</small>
        <strong>{deployment.provider_model}</strong>
        <small>{deployment.access_surface}</small>
        <code>{deployment.profile_id}</code>
        <code>{deployment.id}</code>
      </div>
      <dl>
        <div><dt>{t("deployments.inputPrice")}</dt><dd>{money(deployment.input_micros_per_million)}</dd></div>
        <div><dt>{t("deployments.outputPrice")}</dt><dd>{money(deployment.output_micros_per_million)}</dd></div>
        <div><dt>{t("deployments.concurrency")}</dt><dd>{deployment.max_concurrency || t("common.unlimited")}</dd></div>
        <div><dt>{t("deployments.context")}</dt><dd>{deployment.capabilities.max_context_tokens || t("deployments.providerDefault")}</dd></div>
        <div><dt>{t("deployments.maxOutput")}</dt><dd>{deployment.capabilities.max_output_tokens || t("deployments.providerDefault")}</dd></div>
      </dl>
      <div className="capability-list" aria-label={t("deployments.capabilities")}>
        {capabilities.length ? capabilities.map((capability) => <span className="badge" key={capability}>{t(`capabilities.${capability}`)}</span>) : <span className="badge">{t("deployments.none")}</span>}
      </div>
      <small>{t("deployments.evidence")}: {evidenceSummary(deployment.capability_evidence)}</small>
      <footer>
        <button
          className={`badge ${test.isSuccess ? "good" : test.isError ? "warning" : ""}`}
          disabled={!deployment.enabled || test.isPending}
          onClick={() => test.mutate()}
        >
          {test.isPending ? t("deployments.testing") : probe || (deployment.enabled ? t("deployments.test") : t("deployments.off"))}
        </button>
        <div className="row-actions">
          <button className="button ghost" onClick={onEdit}>{t("common.edit")}</button>
          <ConfirmButton
            label={t("common.delete")}
            confirmLabel={t("deployments.deleteConfirm", { name: deployment.name })}
            onConfirm={() => remove.mutate()}
            disabled={remove.isPending}
          />
        </div>
      </footer>
      {(test.isError || remove.isError) && <ErrorState error={test.error || remove.error} />}
    </article>
  );
}

function evidenceSummary(evidence: Record<string, string>) {
  const values = [...new Set(Object.values(evidence).filter((value) => value !== "unsupported"))];
  return values.length ? values.join(" / ") : "—";
}

function DeploymentForm({
  current,
  providers,
  onClose,
}: {
  current?: Deployment;
  providers: Provider[];
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const enabledProviders = providers.filter((provider) => provider.enabled || provider.id === current?.provider_id);
  const [name, setName] = useState(current?.name ?? "");
  const [providerID, setProviderID] = useState(current?.provider_id ?? enabledProviders[0]?.id ?? "");
  const [providerModel, setProviderModel] = useState(current?.provider_model ?? "");
  const initialProvider = enabledProviders.find((provider) => provider.id === (current?.provider_id ?? enabledProviders[0]?.id));
  const [capabilities, setCapabilities] = useState<ProviderCapabilities>(current?.capabilities ?? initialProvider?.capabilities ?? emptyCapabilities());
  const [inputPrice, setInputPrice] = useState((current?.input_micros_per_million ?? 0) / 1_000_000);
  const [outputPrice, setOutputPrice] = useState((current?.output_micros_per_million ?? 0) / 1_000_000);
  const [fixedRequestPrice, setFixedRequestPrice] = useState((current?.fixed_request_micros_usd ?? 0) / 1_000_000);
  const [region, setRegion] = useState(current?.region ?? "");
  const [maxConcurrency, setMaxConcurrency] = useState(current?.max_concurrency ?? 0);
  const [priority, setPriority] = useState(current?.priority ?? 10);
  const [weight, setWeight] = useState(current?.weight ?? 1);
  const [enabled, setEnabled] = useState(current?.enabled ?? true);
  const queryClient = useQueryClient();
  const value = () => ({
    name: name.trim(),
    provider_id: providerID,
    provider_model: providerModel.trim(),
    capabilities,
    input_micros_per_million: Math.round(inputPrice * 1_000_000),
    output_micros_per_million: Math.round(outputPrice * 1_000_000),
    fixed_request_micros_usd: Math.round(fixedRequestPrice * 1_000_000),
    region: region.trim(),
    max_concurrency: maxConcurrency,
    priority,
    weight,
    enabled,
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
    if (name.trim() && providerID && providerModel.trim()) mutation.mutate();
  };
  return (
    <Modal title={current ? t("deployments.edit") : t("deployments.createTitle")} onClose={onClose}>
      {enabledProviders.length === 0 ? (
        <div className="notice warning"><strong>{t("deployments.providerRequired")}</strong><span>{t("deployments.providerRequiredDescription")}</span></div>
      ) : (
        <form className="form-grid" onSubmit={submit}>
          <Field label={t("deployments.name")}><input autoFocus required value={name} onChange={(event) => setName(event.target.value)} /></Field>
          <Field label={t("deployments.provider")}>
            <select required value={providerID} onChange={(event) => {
              const next = event.target.value;
              setProviderID(next);
              const provider = enabledProviders.find((item) => item.id === next);
              if (provider) setCapabilities(provider.capabilities);
            }}>
              {enabledProviders.map((provider) => <option key={provider.id} value={provider.id}>{provider.name}</option>)}
            </select>
          </Field>
          <Field label={t("deployments.upstreamModel")}><input required value={providerModel} onChange={(event) => setProviderModel(event.target.value)} /></Field>
          <Field label={t("deployments.region")} hint={t("deployments.regionHint")}><input value={region} onChange={(event) => setRegion(event.target.value)} /></Field>
          <fieldset className="form-grid">
            <legend>{t("deployments.capabilitySubset")}</legend>
            {(["chat", "streaming", "embeddings", "moderations", "images", "transcriptions", "speech", "files", "batches", "rerank", "async_generate", "tools", "vision", "json_mode", "developer_role", "reasoning", "stream_usage"] as const).map((name) => (
              <label className="check-row" key={name}>
                <input
                  type="checkbox"
                  checked={capabilities[name]}
                  onChange={(event) => setCapabilities({ ...capabilities, [name]: event.target.checked })}
                />
                <span>{t(`capabilities.${name}`)}</span>
              </label>
            ))}
            <Field label={t("deployments.maxContext")} hint={t("deployments.maxContextHint")}>
              <input min="0" type="number" value={capabilities.max_context_tokens} onChange={(event) => setCapabilities({ ...capabilities, max_context_tokens: Number(event.target.value) })} />
            </Field>
            <Field label={t("deployments.maxOutputTokens")} hint={t("deployments.maxOutputHint")}>
              <input min="0" type="number" value={capabilities.max_output_tokens} onChange={(event) => setCapabilities({ ...capabilities, max_output_tokens: Number(event.target.value) })} />
            </Field>
          </fieldset>
          <Field label={t("deployments.concurrencyLimit")} hint={t("deployments.concurrencyHint")}><input min="0" type="number" value={maxConcurrency} onChange={(event) => setMaxConcurrency(Number(event.target.value))} /></Field>
          <Field label={t("deployments.inputUSD")}><input min="0" type="number" step="0.000001" value={inputPrice} onChange={(event) => setInputPrice(Number(event.target.value))} /></Field>
          <Field label={t("deployments.outputUSD")}><input min="0" type="number" step="0.000001" value={outputPrice} onChange={(event) => setOutputPrice(Number(event.target.value))} /></Field>
          <Field label={t("deployments.fixedRequestUSD")} hint={t("deployments.fixedRequestHint")}><input min="0" type="number" step="0.000001" value={fixedRequestPrice} onChange={(event) => setFixedRequestPrice(Number(event.target.value))} /></Field>
          <Field label={t("deployments.priority")}><input type="number" value={priority} onChange={(event) => setPriority(Number(event.target.value))} /></Field>
          <Field label={t("deployments.weight")}><input min="1" type="number" value={weight} onChange={(event) => setWeight(Number(event.target.value))} /></Field>
          <label className="check-row"><input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />{t("deployments.enable")}</label>
          {mutation.isError && <ErrorState error={mutation.error} />}
          <div className="form-actions">
            <button type="button" className="button ghost" onClick={onClose}>{t("common.cancel")}</button>
            <button className="button primary" disabled={mutation.isPending}>{current ? t("deployments.save") : t("deployments.createAndLoad")}</button>
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
