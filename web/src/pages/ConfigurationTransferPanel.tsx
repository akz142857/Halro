import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { api, type Reauth } from "../api";
import { ConfirmButton, ErrorState } from "../components";
import type {
  ConfigurationBindings, ConfigurationImportPreview, ConfigurationImportResult,
  ConfigurationObject, ConfigurationProviderReview, ConfigurationProviderTarget, ConfigurationResource, Credential, ProviderEgressProxy,
} from "../types";

const fileLimit = 2 * 1024 * 1024;
const emptyTarget: ConfigurationProviderTarget = { credential_id: "" };
const resources: ConfigurationResource[] = [
  "providers", "deployments", "routes", "redaction_policies", "token_guard_policies", "projects", "gateway_keys",
];

function downloadJSON(name: string, content: string) {
  const url = URL.createObjectURL(new Blob([content], { type: "application/json;charset=utf-8" }));
  const link = document.createElement("a");
  link.download = name;
  link.href = url;
  link.click();
  // Let the browser begin consuming the Blob before releasing it.
  window.setTimeout(() => URL.revokeObjectURL(url), 1_000);
}

export function ConfigurationTransferPanel() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [exporting, setExporting] = useState(false);
  const [exportError, setExportError] = useState<unknown>(null);
  const [busy, setBusy] = useState(false);
  const [fileError, setFileError] = useState<"fileSize" | "fileInvalid" | null>(null);
  const [importError, setImportError] = useState<unknown>(null);
  const [bundle, setBundle] = useState("");
  const [targets, setTargets] = useState(new Map<string, ConfigurationProviderTarget>());
  const [preview, setPreview] = useState<ConfigurationImportPreview>();
  const [result, setResult] = useState<ConfigurationImportResult>();
  const hasProviders = Boolean(preview?.providers.length);
  const credentials = useQuery({ queryKey: ["credentials"], queryFn: api.credentials, enabled: hasProviders, refetchOnWindowFocus: true });
  const proxies = useQuery({ queryKey: ["provider-egress-proxies"], queryFn: api.providerEgressProxies, enabled: hasProviders, refetchOnWindowFocus: true });

  async function exportBundle() {
    setExportError(null);
    setExporting(true);
    try {
      downloadJSON("halro-configuration.json", await api.exportConfiguration());
    } catch (error) {
      setExportError(error);
    } finally {
      setExporting(false);
    }
  }

  async function selectFile(file: File | undefined) {
    setBundle("");
    setTargets(new Map());
    setPreview(undefined);
    setFileError(null);
    setImportError(null);
    if (!file) return;
    if (file.size > fileLimit) {
      setFileError("fileSize");
      return;
    }
    setBusy(true);
    try {
      const content = await file.text();
      try {
        const value: unknown = JSON.parse(content);
        if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("expected a JSON object");
      } catch {
        setFileError("fileInvalid");
        return;
      }
      setBundle(content);
      setPreview(await api.previewConfigurationImport(content, {}));
    } catch (error) {
      setImportError(error);
    } finally {
      setBusy(false);
    }
  }

  function updateTarget(id: string, change: Partial<ConfigurationProviderTarget>) {
    setTargets((previous) => new Map(previous).set(id, { ...(previous.get(id) ?? emptyTarget), ...change }));
    setPreview((previous) => previous && { ...previous, plan_id: undefined });
  }

  function bindings(): ConfigurationBindings {
    return Object.fromEntries(targets);
  }

  async function reviewImport() {
    setBusy(true);
    setImportError(null);
    setPreview((previous) => previous && { ...previous, plan_id: undefined });
    try {
      setPreview(await api.previewConfigurationImport(bundle, bindings()));
    } catch (error) {
      setImportError(error);
    } finally {
      setBusy(false);
    }
  }

  async function applyImport(reauth: Reauth) {
    if (!preview?.plan_id) return;
    const imported = await api.applyConfigurationImport(bundle, bindings(), preview.plan_id, reauth);
    setResult(imported);
    setBundle("");
    setPreview(undefined);
    setTargets(new Map());
    void queryClient.invalidateQueries();
  }

  return <>
    <section className="panel configuration-transfer-card" aria-labelledby="configuration-export-title">
      <h3 id="configuration-export-title">{t("configurationTransfer.exportTitle")}</h3>
      <p>{t("configurationTransfer.exportDescription")}</p>
      <div className="form-actions">
        <button className="button secondary" type="button" disabled={exporting} onClick={() => void exportBundle()}>
          {exporting ? t("common.working") : t("configurationTransfer.downloadBundle")}
        </button>
      </div>
      {Boolean(exportError) && <ErrorState error={exportError} />}
    </section>

    <section className="panel configuration-transfer-card" aria-labelledby="configuration-import-title">
      <h3 id="configuration-import-title">{t("configurationTransfer.importTitle")}</h3>
      {!result && <>
        <p>{t("configurationTransfer.importDescription")}</p>
        <div className="field">
          <label htmlFor="configuration-file">{t("configurationTransfer.file")}</label>
          <input id="configuration-file" type="file" accept=".json,application/json" disabled={busy}
            onChange={(event) => void selectFile(event.currentTarget.files?.[0])} />
        </div>
        {fileError && <p className="notice error" role="alert">{t(`configurationTransfer.${fileError}`)}</p>}
        {Boolean(importError) && <ErrorState error={importError} />}
        {preview && <>
          <ConfigurationCounts counts={preview.counts} />
          {hasProviders && <>
            <p>{t("configurationTransfer.bindingHelp")} <a className="text-link" href="/admin/providers?view=credentials" target="_blank" rel="noreferrer">{t("configurationTransfer.manageCredentials")}</a></p>
            {(credentials.error || proxies.error) && <ErrorState error={credentials.error || proxies.error} />}
            {preview.providers.map((provider) => <ConfigurationProviderBinding key={provider.id}
              provider={provider} target={targets.get(provider.id) ?? emptyTarget}
              credentials={credentials.data?.items ?? []} proxies={proxies.data?.items ?? []}
              disabled={busy} onChange={(change) => updateTarget(provider.id, change)} />)}
          </>}
          {preview.issues.length > 0 && <div className="notice warning configuration-list" role="status">
            <strong>{t("configurationTransfer.issues")}</strong>
            <ul>{preview.issues.map((issue, index) => <li key={index}>
              {t(`configurationTransfer.resources.${issue.resource}`)}{issue.id ? ` (${preview.providers.find((provider) => provider.id === issue.id)?.name ?? issue.id})` : ""}{" — "}
              {t(`configurationTransfer.issueCodes.${issue.code}`, { defaultValue: issue.message })}
            </li>)}</ul>
          </div>}
          <ConfigurationActivationSteps deployments={preview.deployments_to_enable} routes={preview.routes_to_enable} />
        </>}
        {bundle && <div className="form-actions">
          <button type="button" className="button secondary" disabled={busy} onClick={() => void reviewImport()}>
            {busy ? t("common.working") : t("configurationTransfer.preview")}
          </button>
          {preview?.plan_id && <ConfirmButton className="button primary" label={t("configurationTransfer.apply")}
            title={t("configurationTransfer.apply")} confirmLabel={t("configurationTransfer.confirm")}
            disabled={busy} requireStepUp onConfirm={applyImport} />}
        </div>}
      </>}
      {result && <>
        <p className="notice success" role="status">{t("configurationTransfer.imported")}</p>
        {result.activation_stale && <p className="notice warning" role="alert">{t("settings.activationStaleDescription")}</p>}
        <ConfigurationCounts counts={result.counts} />
        <ConfigurationActivationSteps deployments={result.deployments_to_enable} routes={result.routes_to_enable} imported />
        {result.gateway_keys.length > 0 && <div className="field">
          <strong>{t("configurationTransfer.newKeys")}</strong>
          <p>{t("configurationTransfer.keyHelp")}</p>
          <div className="form-actions">
            <button type="button" className="button primary" onClick={() =>
              downloadJSON("halro-new-gateway-keys.json", JSON.stringify(result.gateway_keys, null, 2))}>
              {t("configurationTransfer.downloadKeys")}
            </button>
            <button type="button" className="button ghost" onClick={() => {
              setResult({ ...result, gateway_keys: [] });
            }}>{t("configurationTransfer.dismissKeys")}</button>
          </div>
        </div>}
      </>}
    </section>
  </>;
}

function ConfigurationCounts({ counts }: { counts: ConfigurationImportPreview["counts"] }) {
  const { t } = useTranslation();
  return <dl className="configuration-counts">{resources.map((resource) => <div key={resource}>
    <dt>{t(`configurationTransfer.resources.${resource}`)}</dt><dd>{counts[resource]}</dd>
  </div>)}</dl>;
}

function ConfigurationProviderBinding({ provider, target, credentials, proxies, disabled, onChange }: {
  provider: ConfigurationProviderReview;
  target: ConfigurationProviderTarget;
  credentials: Credential[];
  proxies: ProviderEgressProxy[];
  disabled: boolean;
  onChange: (change: Partial<ConfigurationProviderTarget>) => void;
}) {
  const { t } = useTranslation();
  const compatible = credentials.filter((credential) => credential.secret_configured && credential.type === provider.type &&
    credential.access_surface === provider.access_surface && credential.scheme === provider.credential_scheme);
  return <fieldset className="configuration-binding" disabled={disabled}>
    <legend>{provider.name}</legend>
    <label className="field"><span>{t("configurationTransfer.endpoint")}</span>
      <input type="url" value={target.base_url ?? provider.base_url} onChange={(event) =>
        onChange({ base_url: event.target.value, acknowledged_policy_revision: undefined })} />
    </label>
    <label className="field"><span>{t("configurationTransfer.credential")}</span>
      <select value={target.credential_id} onChange={(event) => onChange({ credential_id: event.target.value })}>
        <option value="">{t("configurationTransfer.selectCredential")}</option>
        {compatible.map((credential) => <option key={credential.id} value={credential.id}>
          {credential.name} ({credential.bound_base_url})
        </option>)}
      </select>
    </label>
    <label className="field"><span>{t("configurationTransfer.proxy")}</span>
      <select value={target.egress_proxy_id ?? ""} onChange={(event) => onChange({ egress_proxy_id: event.target.value })}>
        <option value="">{t(provider.requires_egress_proxy ? "configurationTransfer.selectProxy" : "configurationTransfer.direct")}</option>
        {proxies.map((proxy) => <option key={proxy.id} value={proxy.id}>{proxy.name}</option>)}
      </select>
    </label>
    {provider.access_surface === "bedrock-mantle" && <label className="field"><span>{t("configurationTransfer.bedrockProject")}</span>
      <input value={target.bedrock_project_id ?? provider.bedrock_project_id ?? ""} onChange={(event) =>
        onChange({ bedrock_project_id: event.target.value })} />
    </label>}
    {provider.policy_revision && <label className="configuration-acknowledgement">
      <input type="checkbox" checked={target.acknowledged_policy_revision === provider.policy_revision} onChange={(event) =>
        onChange({ acknowledged_policy_revision: event.target.checked ? provider.policy_revision : undefined })} />
      <span>{t("configurationTransfer.policyAcknowledgement")}{" "}
        {provider.policy_url && <a className="text-link" href={provider.policy_url} target="_blank" rel="noreferrer">{t("configurationTransfer.policyLink")}</a>}
      </span>
    </label>}
  </fieldset>;
}

function ConfigurationActivationSteps({ deployments, routes, imported = false }: {
  deployments: ConfigurationObject[]; routes: ConfigurationObject[]; imported?: boolean;
}) {
  const { t } = useTranslation();
  if (deployments.length === 0 && routes.length === 0) return null;
  const labels = (items: ConfigurationObject[], page: string, kind: string) =>
    items.map((item, index) => {
      const label = item.target ? `${item.name} → ${item.target}` : item.name;
      return <span key={item.id}>{index > 0 && ", "}
        {imported ? <a className="text-link" href={`/admin/${page}#${kind}-${encodeURIComponent(item.id)}`} target="_blank" rel="noreferrer">{label}</a> : label}
      </span>;
    });
  return <div className="notice warning configuration-list">
    <strong>{t("configurationTransfer.activationTitle")}</strong>
    <ol>
      {deployments.length > 0 && <li>{t("configurationTransfer.activateDeployments")}{" "}
        {labels(deployments, "deployments", "deployment")}</li>}
      {routes.length > 0 && <li>{t("configurationTransfer.activateRoutes")}{" "}
        {labels(routes, "routes", "route")}</li>}
    </ol>
  </div>;
}
