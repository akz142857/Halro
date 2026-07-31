import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { api } from "../api";
import {
  EmptyState,
  ErrorState,
  Field,
  Loading,
  Modal,
  PageHeader,
  StatusDot,
  ConfirmButton,
} from "../components";
import type { Credential, Provider, ProviderCapabilities, ProviderType } from "../types";

function defaultBaseURL(type: ProviderType) {
  if (type === "gemini") return "https://generativelanguage.googleapis.com";
  if (type === "deepseek") return "https://api.deepseek.com";
  if (type === "bedrock") return "https://bedrock-runtime.us-east-1.amazonaws.com";
  return "https://api.openai.com";
}

export function ProvidersPage() {
  const [credentialDialog, setCredentialDialog] = useState(false);
  const [providerDialog, setProviderDialog] = useState(false);
  const [editingProvider, setEditingProvider] = useState<Provider>();
  const credentials = useQuery({ queryKey: ["credentials"], queryFn: api.credentials });
  const providers = useQuery({ queryKey: ["providers"], queryFn: api.providers });
  const pending = credentials.isPending || providers.isPending;
  return (
    <>
      <PageHeader
        eyebrow="UPSTREAM TRUST"
        title="Credentials & Providers"
        description="Provider Secret 加密留存在本机 Vault；运行时只按绑定的 audience 解密。"
        action={
          <div className="button-group">
            <button className="button secondary" onClick={() => setCredentialDialog(true)}>＋ 凭据</button>
            <button className="button primary" onClick={() => setProviderDialog(true)}>＋ Provider</button>
          </div>
        }
      />
      {pending && <Loading />}
      {(credentials.isError || providers.isError) && (
        <ErrorState error={credentials.error || providers.error} />
      )}
      {!pending && (
        <div className="provider-grid">
          <section className="panel">
            <header className="panel-header">
              <div><p className="eyebrow">ENCRYPTED VAULT</p><h2>Credentials</h2></div>
              <span className="count">{credentials.data?.items.length ?? 0}</span>
            </header>
            {credentials.data?.items.length === 0 && (
              <EmptyState title="没有 Provider 凭据">先保存加密凭据，再创建 Provider 实例。</EmptyState>
            )}
            {credentials.data?.items.map((credential) => (
              <CredentialRow key={credential.id} credential={credential} />
            ))}
          </section>
          <section className="panel">
            <header className="panel-header">
              <div><p className="eyebrow">ACTIVE UPSTREAMS</p><h2>Providers</h2></div>
              <span className="count">{providers.data?.items.length ?? 0}</span>
            </header>
            {providers.data?.items.length === 0 && (
              <EmptyState title="没有 Provider">创建一个上游连接，Deployment 才能选择它。</EmptyState>
            )}
            {providers.data?.items.map((provider) => (
              <ProviderRow provider={provider} key={provider.id} onEdit={() => setEditingProvider(provider)} />
            ))}
          </section>
        </div>
      )}
      {credentialDialog && <CredentialForm onClose={() => setCredentialDialog(false)} />}
      {providerDialog && (
        <ProviderForm
          credentials={credentials.data?.items ?? []}
          onClose={() => setProviderDialog(false)}
        />
      )}
      {editingProvider && (
        <ProviderForm
          current={editingProvider}
          credentials={credentials.data?.items ?? []}
          onClose={() => setEditingProvider(undefined)}
        />
      )}
    </>
  );
}

function ProviderRow({ provider, onEdit }: { provider: Provider; onEdit: () => void }) {
  const [result, setResult] = useState("");
  const queryClient = useQueryClient();
  const testMutation = useMutation({
    mutationFn: () => api.testProvider(provider.id),
    onSuccess: (value) => setResult(`HEALTHY · ${value.latency_ms}ms`),
    onError: () => setResult("UNHEALTHY"),
  });
  const deleteMutation = useMutation({
    mutationFn: () => api.deleteProvider(provider.id, provider.revision),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["providers"] }),
  });
  return (
    <>
      <div className="provider-row">
        <span className="provider-icon">{provider.type === "openai" ? "OA" : "AI"}</span>
        <div>
          <span><StatusDot ok={provider.enabled} /><strong>{provider.name}</strong></span>
          <small>{provider.base_url}</small>
          <small>并发：{provider.max_concurrency || "Unlimited"}</small>
          <code>{provider.id}</code>
        </div>
        <div className="button-group">
          <button className="button ghost" onClick={onEdit}>编辑</button>
          <button
            className={`badge ${result.startsWith("HEALTHY") ? "good" : ""}`}
            disabled={!provider.enabled || testMutation.isPending}
            onClick={() => testMutation.mutate()}
          >
            {testMutation.isPending ? "TESTING" : result || (provider.enabled ? "TEST" : "OFF")}
          </button>
          <ConfirmButton
            label="删除"
            confirmLabel={`删除 Provider “${provider.name}”？仍被 Deployment 引用时操作会被拒绝。`}
            disabled={deleteMutation.isPending}
            onConfirm={() => deleteMutation.mutate()}
          />
        </div>
      </div>
      {deleteMutation.isError && <ErrorState error={deleteMutation.error} />}
      {testMutation.isError && <ErrorState error={testMutation.error} />}
    </>
  );
}

function CredentialRow({ credential }: { credential: Credential }) {
  const [rotating, setRotating] = useState(false);
  const queryClient = useQueryClient();
  const remove = useMutation({
    mutationFn: () => api.deleteCredential(credential.id, credential.revision),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["credentials"] }),
  });
  return (
    <>
      <div className="credential-row">
        <span className="vault-lock" aria-hidden="true">◆</span>
        <div>
          <strong>{credential.name}</strong>
          <small>{credential.type} · key generation {credential.key_version}</small>
          <code>{credential.id}</code>
        </div>
        <div className="row-actions">
          <button className="button ghost" onClick={() => setRotating(true)}>轮换</button>
          <ConfirmButton
            label="删除"
            confirmLabel={`删除凭据 “${credential.name}”？仍被 Provider 引用时操作会被拒绝。`}
            disabled={remove.isPending}
            onConfirm={() => remove.mutate()}
          />
        </div>
      </div>
      {remove.isError && <ErrorState error={remove.error} />}
      {rotating && <CredentialForm current={credential} onClose={() => setRotating(false)} />}
    </>
  );
}

function CredentialForm({
  current,
  onClose,
}: {
  current?: Credential;
  onClose: () => void;
}) {
  const [name, setName] = useState(current?.name ?? "");
  const [type, setType] = useState<ProviderType>(current?.type ?? "openai");
  const [baseURL, setBaseURL] = useState(defaultBaseURL(current?.type ?? "openai"));
  const [secret, setSecret] = useState("");
  const queryClient = useQueryClient();
  const mutation = useMutation({
    mutationFn: () => current
      ? api.rotateCredential(current.id, {
        name,
        type,
        base_url: baseURL,
        ...(secret ? { secret } : {}),
      }, current.revision)
      : api.createCredential({ name, type, base_url: baseURL, secret }),
    onSettled: () => setSecret(""),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["credentials"] });
      onClose();
    },
  });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (name.trim() && baseURL.trim() && (current || secret)) mutation.mutate();
  };
  return (
    <Modal title={current ? "轮换 Provider 凭据" : "保存 Provider 凭据"} onClose={onClose}>
      <form onSubmit={submit} autoComplete="off">
        <Field label="凭据名称"><input autoFocus value={name} onChange={(event) => setName(event.target.value)} /></Field>
        <Field label="Provider 类型">
          <select value={type} onChange={(event) => {
            const next = event.target.value as ProviderType;
            setType(next);
            setBaseURL(defaultBaseURL(next));
          }}>
            <option value="openai">OpenAI</option>
            <option value="azure_openai">Azure OpenAI</option>
            <option value="deepseek">DeepSeek</option>
            <option value="gemini">Gemini (Beta)</option>
            <option value="bedrock">AWS Bedrock (Beta)</option>
            <option value="openai_compatible">OpenAI Compatible</option>
          </select>
        </Field>
        <Field label="绑定的 Base URL" hint="Secret 将与规范化后的 scheme、host、port 和 Provider 类型绑定">
          <input inputMode="url" value={baseURL} onChange={(event) => setBaseURL(event.target.value)} />
        </Field>
        <Field
          label={current ? "新 Secret（留空则只更新元数据）" : type === "bedrock" ? "AWS Credential JSON" : "Provider Secret"}
          hint={current
            ? "已配置的 Secret 永不回显"
            : type === "bedrock"
              ? '字段：access_key_id、secret_access_key、region；session_token 可选。区域必须匹配 Base URL。'
              : "只通过 HTTPS 请求体发送，不写入浏览器存储"}
        >
          <input
            type="password"
            autoComplete="new-password"
            value={secret}
            onChange={(event) => setSecret(event.target.value)}
          />
        </Field>
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions">
          <button type="button" className="button ghost" onClick={onClose}>取消</button>
          <button className="button primary" disabled={mutation.isPending || (!current && !secret)}>
            {current ? "安全轮换" : "加密保存"}
          </button>
        </div>
      </form>
    </Modal>
  );
}

function ProviderForm({
  current,
  credentials,
  onClose,
}: {
  current?: Provider;
  credentials: Credential[];
  onClose: () => void;
}) {
  const initialType = current?.type ?? "openai";
  const [name, setName] = useState(current?.name ?? "");
  const [type, setType] = useState<ProviderType>(initialType);
  const [baseURL, setBaseURL] = useState(current?.base_url ?? defaultBaseURL(initialType));
  const [apiVersion, setAPIVersion] = useState(current?.api_version ?? "");
  const [maxConcurrency, setMaxConcurrency] = useState(current?.max_concurrency ?? 0);
  const [enabled, setEnabled] = useState(current?.enabled ?? true);
  const [capabilities, setCapabilities] = useState<ProviderCapabilities>(current?.capabilities ?? defaultProviderCapabilities(initialType));
  const [credentialID, setCredentialID] = useState(current?.credential_id ?? credentials.find((credential) => credential.type === initialType)?.id ?? "");
  const matchingCredentials = credentials.filter((credential) => credential.type === type);
  const queryClient = useQueryClient();
  const mutation = useMutation({
    mutationFn: () => {
      const value = {
      name, type, base_url: baseURL,
      ...(type === "azure_openai" ? { api_version: apiVersion } : {}),
      credential_id: credentialID, capabilities, max_concurrency: maxConcurrency, enabled,
      };
      return current
        ? api.updateProvider(current.id, value, current.revision)
        : api.createProvider(value);
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["providers"] });
      onClose();
    },
  });
  return (
    <Modal title={current ? "编辑 Provider" : "创建 Provider"} onClose={onClose}>
      {credentials.length === 0 ? (
        <div className="notice warning">
          <strong>需要先创建凭据</strong>
          <span>关闭此窗口，使用“＋ 凭据”保存一个 audience-bound Secret。</span>
        </div>
      ) : (
        <form
          onSubmit={(event) => {
            event.preventDefault();
            if (name && credentialID) mutation.mutate();
          }}
        >
          <Field label="Provider 名称"><input autoFocus value={name} onChange={(event) => setName(event.target.value)} /></Field>
          <Field label="类型">
            <select value={type} onChange={(event) => {
              const next = event.target.value as ProviderType;
              setType(next);
              setBaseURL(defaultBaseURL(next));
              setCredentialID(credentials.find((credential) => credential.type === next)?.id ?? "");
              setCapabilities(defaultProviderCapabilities(next));
            }}>
              <option value="openai">OpenAI</option>
              <option value="azure_openai">Azure OpenAI</option>
              <option value="deepseek">DeepSeek</option>
              <option value="gemini">Gemini (Beta)</option>
              <option value="bedrock">AWS Bedrock (Beta)</option>
              <option value="openai_compatible">OpenAI Compatible</option>
            </select>
          </Field>
          <Field label="Base URL"><input value={baseURL} onChange={(event) => setBaseURL(event.target.value)} /></Field>
          {type === "azure_openai" && (
            <Field label="API Version" hint="显式固定 Azure data-plane API 版本；升级时由管理员变更">
              <input value={apiVersion} onChange={(event) => setAPIVersion(event.target.value)} required />
            </Field>
          )}
          <Field label="Provider 最大并发" hint="0 表示不限；用于保护上游账户和部署">
            <input
              type="number"
              min="0"
              value={maxConcurrency}
              onChange={(event) => setMaxConcurrency(Number(event.target.value))}
            />
          </Field>
          <fieldset className="form-grid">
            <legend>Provider 能力上限</legend>
            {capabilityNames.map((capability) => (
              <label className="check-row" key={capability}>
                <input
                  type="checkbox"
                  checked={capabilities[capability]}
                  onChange={(event) => setCapabilities({ ...capabilities, [capability]: event.target.checked })}
                />
                <span>{capability.replace("json_mode", "JSON mode").replaceAll("_", " ")}</span>
              </label>
            ))}
            <Field label="最大 Context Tokens" hint="0 表示 Provider 未声明限制">
              <input min="0" type="number" value={capabilities.max_context_tokens} onChange={(event) => setCapabilities({ ...capabilities, max_context_tokens: Number(event.target.value) })} />
            </Field>
            <Field label="最大 Output Tokens" hint="不得大于 Context Tokens">
              <input min="0" type="number" value={capabilities.max_output_tokens} onChange={(event) => setCapabilities({ ...capabilities, max_output_tokens: Number(event.target.value) })} />
            </Field>
          </fieldset>
          <Field label="加密凭据">
            <select value={credentialID} onChange={(event) => setCredentialID(event.target.value)}>
              {matchingCredentials.map((credential) => (
                <option value={credential.id} key={credential.id}>{credential.name} · {credential.type}</option>
              ))}
            </select>
          </Field>
          <label className="check-row"><input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />启用 Provider</label>
          {mutation.isError && <ErrorState error={mutation.error} />}
          <div className="form-actions">
            <button type="button" className="button ghost" onClick={onClose}>取消</button>
            <button className="button primary" disabled={mutation.isPending || !credentialID}>{current ? "保存并热加载" : "创建并热加载"}</button>
          </div>
        </form>
      )}
    </Modal>
  );
}

const capabilityNames = [
  "chat", "streaming", "embeddings", "tools", "vision", "json_mode",
  "developer_role", "reasoning", "stream_usage",
] as const;

function defaultProviderCapabilities(type: ProviderType): ProviderCapabilities {
  const value: ProviderCapabilities = {
    chat: true, streaming: true, embeddings: false, tools: false, vision: false,
    json_mode: false, developer_role: false, reasoning: false, stream_usage: false,
    max_context_tokens: 0, max_output_tokens: 0,
  };
  if (type === "openai" || type === "azure_openai") {
    return { ...value, embeddings: true, tools: true, vision: true, json_mode: true, developer_role: true, reasoning: true, stream_usage: true };
  }
  if (type === "deepseek") return { ...value, tools: true, json_mode: true, reasoning: true, stream_usage: true };
  if (type === "openai_compatible") return { ...value, embeddings: true };
  if (type === "gemini") return { ...value, embeddings: true, developer_role: true, stream_usage: false };
  if (type === "bedrock") return { ...value, developer_role: true, stream_usage: true };
  return value;
}
