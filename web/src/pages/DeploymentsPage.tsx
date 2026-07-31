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

export function DeploymentsPage() {
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
        eyebrow="MODEL DEPLOYMENTS"
        title="Deployments"
        description="把 Provider 连接实例化为可路由的模型目标，在这里独立维护模型名称、能力、价格与并发上限。"
        action={<button className="button primary" onClick={() => setEditing("new")}>＋ 新建 Deployment</button>}
      />
      {(deployments.isPending || providers.isPending) && <Loading />}
      {(deployments.isError || providers.isError) && <ErrorState error={deployments.error || providers.error} />}
      {deployments.data?.items.length === 0 && (
        <EmptyState title="还没有模型 Deployment">先创建 Provider，再为具体上游模型建立 Deployment。</EmptyState>
      )}
      {!!deployments.data?.items.length && (
        <section className="deployment-grid" aria-label="模型部署列表">
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
  const queryClient = useQueryClient();
  const [probe, setProbe] = useState("");
  const test = useMutation({
    mutationFn: () => api.testDeployment(deployment.id),
    onSuccess: (result) => setProbe(`HEALTHY · ${result.latency_ms}ms`),
    onError: () => setProbe("UNHEALTHY"),
  });
  const remove = useMutation({
    mutationFn: () => api.deleteDeployment(deployment.id, deployment.revision),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["deployments"] }),
  });
  const capabilities = Object.entries(deployment.capabilities)
    .filter(([, enabled]) => typeof enabled === "boolean" && enabled)
    .map(([name]) => name.replace("json_mode", "json"));
  return (
    <article className="deployment-card">
      <header>
        <span><StatusDot ok={deployment.enabled} /><strong>{deployment.name}</strong></span>
        <span className="badge">P{deployment.priority} · W{deployment.weight}</span>
      </header>
      <div className="deployment-model">
        <small>{providerName}</small>
        <strong>{deployment.provider_model}</strong>
        <code>{deployment.id}</code>
      </div>
      <dl>
        <div><dt>Input / 1M</dt><dd>{money(deployment.input_micros_per_million)}</dd></div>
        <div><dt>Output / 1M</dt><dd>{money(deployment.output_micros_per_million)}</dd></div>
        <div><dt>Concurrency</dt><dd>{deployment.max_concurrency || "Unlimited"}</dd></div>
        <div><dt>Context</dt><dd>{deployment.capabilities.max_context_tokens || "Provider default"}</dd></div>
        <div><dt>Max output</dt><dd>{deployment.capabilities.max_output_tokens || "Provider default"}</dd></div>
      </dl>
      <div className="capability-list" aria-label="能力">
        {capabilities.length ? capabilities.map((capability) => <span className="badge" key={capability}>{capability}</span>) : <span className="badge">none</span>}
      </div>
      <footer>
        <button
          className={`badge ${probe.startsWith("HEALTHY") ? "good" : probe === "UNHEALTHY" ? "warning" : ""}`}
          disabled={!deployment.enabled || test.isPending}
          onClick={() => test.mutate()}
        >
          {test.isPending ? "TESTING" : probe || (deployment.enabled ? "TEST" : "OFF")}
        </button>
        <div className="row-actions">
          <button className="button ghost" onClick={onEdit}>编辑</button>
          <ConfirmButton
            label="删除"
            confirmLabel={`删除 Deployment “${deployment.name}”？`}
            onConfirm={() => remove.mutate()}
            disabled={remove.isPending}
          />
        </div>
      </footer>
      {(test.isError || remove.isError) && <ErrorState error={test.error || remove.error} />}
    </article>
  );
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
  const enabledProviders = providers.filter((provider) => provider.enabled || provider.id === current?.provider_id);
  const [name, setName] = useState(current?.name ?? "");
  const [providerID, setProviderID] = useState(current?.provider_id ?? enabledProviders[0]?.id ?? "");
  const [providerModel, setProviderModel] = useState(current?.provider_model ?? "");
  const initialProvider = enabledProviders.find((provider) => provider.id === (current?.provider_id ?? enabledProviders[0]?.id));
  const [capabilities, setCapabilities] = useState<ProviderCapabilities>(current?.capabilities ?? initialProvider?.capabilities ?? emptyCapabilities());
  const [inputPrice, setInputPrice] = useState((current?.input_micros_per_million ?? 0) / 1_000_000);
  const [outputPrice, setOutputPrice] = useState((current?.output_micros_per_million ?? 0) / 1_000_000);
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
    <Modal title={current ? "编辑 Deployment" : "创建 Deployment"} onClose={onClose}>
      {enabledProviders.length === 0 ? (
        <div className="notice warning"><strong>需要可用 Provider</strong><span>先在 Providers 页面创建并启用一个上游连接。</span></div>
      ) : (
        <form className="form-grid" onSubmit={submit}>
          <Field label="Deployment 名称"><input autoFocus required value={name} onChange={(event) => setName(event.target.value)} /></Field>
          <Field label="Provider">
            <select required value={providerID} onChange={(event) => {
              const next = event.target.value;
              setProviderID(next);
              const provider = enabledProviders.find((item) => item.id === next);
              if (provider) setCapabilities(provider.capabilities);
            }}>
              {enabledProviders.map((provider) => <option key={provider.id} value={provider.id}>{provider.name}</option>)}
            </select>
          </Field>
          <Field label="上游模型名称"><input required value={providerModel} onChange={(event) => setProviderModel(event.target.value)} /></Field>
          <fieldset className="form-grid">
            <legend>模型能力（只能是 Provider 能力的子集）</legend>
            {(["chat", "streaming", "embeddings", "tools", "vision", "json_mode", "developer_role", "reasoning", "stream_usage"] as const).map((name) => (
              <label className="check-row" key={name}>
                <input
                  type="checkbox"
                  checked={capabilities[name]}
                  onChange={(event) => setCapabilities({ ...capabilities, [name]: event.target.checked })}
                />
                <span>{name.replace("json_mode", "JSON mode").replaceAll("_", " ")}</span>
              </label>
            ))}
            <Field label="最大 Context Tokens" hint="0 表示沿用 Provider 未声明的限制">
              <input min="0" type="number" value={capabilities.max_context_tokens} onChange={(event) => setCapabilities({ ...capabilities, max_context_tokens: Number(event.target.value) })} />
            </Field>
            <Field label="最大 Output Tokens" hint="不得超过 Context 或 Provider 限制">
              <input min="0" type="number" value={capabilities.max_output_tokens} onChange={(event) => setCapabilities({ ...capabilities, max_output_tokens: Number(event.target.value) })} />
            </Field>
          </fieldset>
          <Field label="并发上限" hint="0 表示不在 Deployment 层限制"><input min="0" type="number" value={maxConcurrency} onChange={(event) => setMaxConcurrency(Number(event.target.value))} /></Field>
          <Field label="Input USD / 1M tokens"><input min="0" type="number" step="0.000001" value={inputPrice} onChange={(event) => setInputPrice(Number(event.target.value))} /></Field>
          <Field label="Output USD / 1M tokens"><input min="0" type="number" step="0.000001" value={outputPrice} onChange={(event) => setOutputPrice(Number(event.target.value))} /></Field>
          <Field label="默认优先级"><input type="number" value={priority} onChange={(event) => setPriority(Number(event.target.value))} /></Field>
          <Field label="权重"><input min="1" type="number" value={weight} onChange={(event) => setWeight(Number(event.target.value))} /></Field>
          <label className="check-row"><input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />启用 Deployment</label>
          {mutation.isError && <ErrorState error={mutation.error} />}
          <div className="form-actions">
            <button type="button" className="button ghost" onClick={onClose}>取消</button>
            <button className="button primary" disabled={mutation.isPending}>{current ? "保存并热加载" : "创建并热加载"}</button>
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
