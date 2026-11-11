import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { ApiError, api, type Reauth } from "../api";
import { ConfirmButton, ErrorState } from "../components";
import { setNavigationBlocked } from "../navigation";
import type {
  ConfigurationBindings, ConfigurationImportPreview, ConfigurationImportResult,
  ConfigurationNotice, ConfigurationObject, ConfigurationProviderReview, ConfigurationProviderTarget, ConfigurationResource, Credential, ProviderEgressProxy,
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
  const fileInput = useRef<HTMLInputElement>(null);
  const [fileName, setFileName] = useState("");
  const [exporting, setExporting] = useState(false);
  const [exportError, setExportError] = useState<unknown>(null);
  const [exportOmissions, setExportOmissions] = useState<ConfigurationNotice[]>([]);
  const [busy, setBusy] = useState(false);
  const [fileError, setFileError] = useState<"fileSize" | "fileInvalid" | null>(null);
  const [importError, setImportError] = useState<unknown>(null);
  const [bundle, setBundle] = useState("");
  const [targets, setTargets] = useState(new Map<string, ConfigurationProviderTarget>());
  const [preview, setPreview] = useState<ConfigurationImportPreview>();
  const [result, setResult] = useState<ConfigurationImportResult>();
  const [acceptDeclarations, setAcceptDeclarations] = useState(false);
  const [keysSaved, setKeysSaved] = useState(false);
  const unsavedKeys = result?.status === "imported" && result.gateway_keys.length > 0 && !keysSaved;

  useEffect(() => {
    if (!unsavedKeys) return;
    setNavigationBlocked(true, t("configurationTransfer.keysLeaveWarning"));
    const beforeUnload = (event: BeforeUnloadEvent) => { event.preventDefault(); event.returnValue = ""; };
    window.addEventListener("beforeunload", beforeUnload);
    return () => {
      window.removeEventListener("beforeunload", beforeUnload);
      setNavigationBlocked(false);
    };
  }, [unsavedKeys, t]);
  const hasProviders = Boolean(preview?.providers.length);
  const fileRejected = preview?.issues.some(({ code }) => code === "record_count" || code === "unsupported_version");
  const emptyBundle = preview && resources.every((resource) => preview.counts[resource] === 0);
  const canApply = Boolean(preview?.plan_id);
  const credentials = useQuery({ queryKey: ["credentials"], queryFn: api.credentials, enabled: hasProviders, refetchOnWindowFocus: true });
  const proxies = useQuery({ queryKey: ["provider-egress-proxies"], queryFn: api.providerEgressProxies, enabled: hasProviders, refetchOnWindowFocus: true });

  async function exportBundle() {
    setExportError(null);
    setExportOmissions([]);
    setExporting(true);
    try {
      const contents = await api.exportConfiguration();
      const bundle = JSON.parse(contents) as { omissions?: ConfigurationNotice[] };
      downloadJSON("halro-configuration.json", contents);
      setExportOmissions(bundle.omissions ?? []);
    } catch (error) {
      setExportError(error);
    } finally {
      setExporting(false);
    }
  }

  async function selectFile(file: File | undefined) {
    if (!file) return;
    setFileName(file.name);
    setBundle("");
    setTargets(new Map());
    setPreview(undefined);
    setAcceptDeclarations(false);
    setFileError(null);
    setImportError(null);
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
    setAcceptDeclarations(false);
    setTargets((previous) => new Map(previous).set(id, { ...(previous.get(id) ?? emptyTarget), ...change }));
    setPreview((previous) => previous && { ...previous, plan_id: undefined });
  }

  function bindings(): ConfigurationBindings {
    return Object.fromEntries(targets);
  }

  async function reviewImport() {
    setAcceptDeclarations(false);
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
    try {
      const imported = await api.applyConfigurationImport(bundle, bindings(), preview.plan_id, reauth, acceptDeclarations);
      setResult(imported);
      setKeysSaved(false);
      setBundle("");
      setPreview(undefined);
      setTargets(new Map());
      void queryClient.invalidateQueries();
    } catch (error) {
      if (!(error instanceof ApiError) || error.status !== 409 ||
          !["configuration_preview_required", "configuration_target_not_empty"].includes(error.code)) throw error;
      const current = (error.payload as { preview?: ConfigurationImportPreview } | undefined)?.preview;
      setPreview({ ...(current ?? preview), plan_id: undefined });
      setAcceptDeclarations(false);
      setImportError(error);
    }
  }

  return <>
    <section className="configuration-transfer-card" aria-labelledby="configuration-export-title">
      <div className="configuration-transfer-row">
        <div>
          <h3 id="configuration-export-title">{t("configurationTransfer.exportTitle")}</h3>
          <p className="configuration-transfer-description">{t("configurationTransfer.exportDescription")}</p>
        </div>
        <button className="button" type="button" disabled={exporting} onClick={() => void exportBundle()}>
          {exporting ? t("common.working") : t("configurationTransfer.downloadBundle")}
        </button>
      </div>
      <p className="configuration-transfer-note">{t("configurationTransfer.exportSensitiveRules")}</p>
      {Boolean(exportError) && <ErrorState error={exportError} />}
      <ConfigurationNotices notices={exportOmissions} />
    </section>

    <section className="configuration-transfer-card" aria-labelledby="configuration-import-title">
      <h3 id="configuration-import-title">{t("configurationTransfer.importTitle")}</h3>
      {!result && <>
        <p className="configuration-transfer-description">{t("configurationTransfer.importDescription")}</p>
        <div className="configuration-transfer-row configuration-file">
          <div>
            <label htmlFor="configuration-file">{t("configurationTransfer.file")}</label>
            <p className="configuration-transfer-note" id="configuration-file-detail" aria-live="polite">
              {fileName || t("configurationTransfer.fileFormat")}
            </p>
          </div>
          <input ref={fileInput} id="configuration-file" type="file" accept=".json,application/json" hidden disabled={busy}
            onChange={(event) => {
              const file = event.currentTarget.files?.[0];
              event.currentTarget.value = "";
              void selectFile(file);
            }} />
          <button className={`button${fileName ? "" : " primary"}`} type="button" disabled={busy}
            aria-describedby="configuration-file-detail" onClick={() => fileInput.current?.click()}>
            {busy && !preview ? t("common.working") : t(fileName ? "configurationTransfer.replaceFile" : "configurationTransfer.chooseFile")}
          </button>
        </div>
        {fileError && <p className="notice error configuration-list" role="alert">{t(`configurationTransfer.${fileError}`)}</p>}
        {Boolean(importError) && <ErrorState error={importError} />}
        {preview && <>
          {!fileRejected && <ConfigurationCounts counts={preview.counts} />}
          {preview.issues.length > 0 && <div className={`notice ${fileRejected ? "error" : "warning"} configuration-list`} role={fileRejected ? "alert" : "status"}>
            <strong>{t("configurationTransfer.issues")}</strong>
            <ul>{preview.issues.map((issue, index) => <li key={index}>
              {issue.id ? <>{preview.providers.find((provider) => provider.id === issue.id)?.name ?? issue.id}{" — "}</> : null}
              {issue.code === "record_count" && emptyBundle ? t("configurationTransfer.emptyFile") :
                t(`configurationTransfer.issueCodes.${issue.code}`, { defaultValue: issue.message })}
            </li>)}</ul>
          </div>}
          {hasProviders && <>
            <div className="configuration-transfer-row configuration-section-header">
              <div>
                <h4>{t("configurationTransfer.targetBindings")}</h4>
                <p className="configuration-transfer-description">{t("configurationTransfer.bindingHelp")}</p>
              </div>
              <a className="text-link" href="/admin/providers?view=credentials" target="_blank" rel="noreferrer">{t("configurationTransfer.manageCredentials")}</a>
            </div>
            {(credentials.error || proxies.error) && <ErrorState error={credentials.error || proxies.error} />}
            {preview.providers.map((provider) => <ConfigurationProviderBinding key={provider.id}
              provider={provider} target={targets.get(provider.id) ?? emptyTarget}
              credentials={credentials.data?.items ?? []} proxies={proxies.data?.items ?? []}
              disabled={busy} onChange={(change) => updateTarget(provider.id, change)} />)}
          </>}
          <ConfigurationNotices notices={preview.notices ?? []} />
          {canApply && Boolean(preview.region_changes?.length) && <div className="notice info configuration-list">
            <strong>{t("configurationTransfer.regionChanges")}</strong>
            <ul>{preview.region_changes?.map((change) => <li key={change.id}>{change.name} ({change.source_region || "—"} → {change.target_region})</li>)}</ul>
          </div>}
          {canApply && Boolean(preview.operator_declarations?.length) && <div className="configuration-detail configuration-list">
            <strong>{t("configurationTransfer.operatorDeclarations")}</strong>
            <ul>{preview.operator_declarations?.map((declaration) => <li key={declaration.id}>
              {declaration.name} ({declaration.model}) — {declaration.capabilities.map((name) => t(`capabilities.${name}`, { defaultValue: name })).join(t("common.listSeparator"))}
            </li>)}</ul>
            <label className="check-row">
              <input type="checkbox" checked={acceptDeclarations} onChange={(event) => setAcceptDeclarations(event.target.checked)} />
              <span>{t("configurationTransfer.acceptDeclarations")}</span>
            </label>
          </div>}
          {canApply && <ConfigurationActivationSteps deployments={preview.deployments_to_enable} routes={preview.routes_to_enable} />}
        </>}
        {bundle && !fileRejected && <div className="form-actions configuration-transfer-actions">
          <button type="button" className={`button${canApply ? "" : " primary"}`} disabled={busy} onClick={() => void reviewImport()}>
            {busy ? t("common.working") : t("configurationTransfer.preview")}
          </button>
          {preview?.plan_id && <ConfirmButton className="button primary" label={t("configurationTransfer.apply")}
            title={t("configurationTransfer.apply")} confirmLabel={t("configurationTransfer.confirm")}
            disabled={busy || (Boolean(preview.operator_declarations?.length) && !acceptDeclarations)} requireStepUp onConfirm={applyImport} />}
        </div>}
      </>}
      {result?.status === "confirmation_unavailable" && <>
        <div className="notice warning configuration-list" role="alert">
          <strong>{t("configurationTransfer.confirmationUnavailable")}</strong>
          <p>{t("configurationTransfer.confirmationRecovery")}</p>
          <p>{t("configurationTransfer.operationId")} <code>{result.operation_id}</code></p>
          {result.keys_to_reissue.length > 0 && <>
            <p>{t("configurationTransfer.reissueKeys")}</p>
            <ul>{result.keys_to_reissue.map((key) => <li key={key.id}>
              <a className="text-link" href={`/admin/projects?project_id=${encodeURIComponent(key.project_id)}`} target="_blank" rel="noreferrer">{key.name}</a>{" "}
              <code>{key.id}</code>
            </li>)}</ul>
          </>}
        </div>
        <ConfigurationCounts counts={result.counts} />
      </>}
      {result?.status === "imported" && <>
        <p className="notice success configuration-list" role="status">{t("configurationTransfer.imported")}</p>
        {result.activation_stale && <p className="notice warning configuration-list" role="alert">{t("settings.activationStaleDescription")}</p>}
        <ConfigurationCounts counts={result.counts} />
        <ConfigurationActivationSteps deployments={result.deployments_to_enable} routes={result.routes_to_enable} imported />
        {result.gateway_keys.length > 0 && <div className="configuration-detail">
          <h4>{t("configurationTransfer.newKeys")}</h4>
          <p className="configuration-transfer-description">{t("configurationTransfer.keyHelp")}</p>
          <label className="check-row">
            <input type="checkbox" checked={keysSaved} onChange={(event) => setKeysSaved(event.target.checked)} />
            <span>{t("configurationTransfer.keysSaved")}</span>
          </label>
          <div className="form-actions configuration-transfer-actions">
            <button type="button" className="button primary" onClick={() =>
              downloadJSON("halro-new-gateway-keys.json", JSON.stringify(result.gateway_keys, null, 2))}>
              {t("configurationTransfer.downloadKeys")}
            </button>
            <button type="button" className="button ghost" disabled={!keysSaved} onClick={() =>
              setResult({ ...result, gateway_keys: [] })
            }>{t("configurationTransfer.dismissKeys")}</button>
          </div>
        </div>}
      </>}
    </section>
  </>;
}

function ConfigurationNotices({ notices }: { notices: ConfigurationNotice[] }) {
  const { t } = useTranslation();
  if (!notices.length) return null;
  return <div className="notice warning configuration-list" role="status">
    <strong>{t("configurationTransfer.notices")}</strong>
    <ul>{notices.map((notice, index) => <li key={index}>
      {t(`configurationTransfer.resources.${notice.resource}`)} <code>{notice.id}</code>{" — "}
      {t(`configurationTransfer.noticeCodes.${notice.code}`, { alias: notice.alias, defaultValue: notice.code })}
    </li>)}</ul>
  </div>;
}

function ConfigurationCounts({ counts }: { counts: ConfigurationImportPreview["counts"] }) {
  const { t } = useTranslation();
  const populated = resources.filter((resource) => counts[resource] > 0);
  if (!populated.length) return null;
  return <section className="configuration-count-summary" aria-label={t("configurationTransfer.configurationSummary")}>
    <h4>{t("configurationTransfer.configurationSummary")}</h4>
    <dl className="configuration-counts">{populated.map((resource) => <div key={resource}>
      <dt>{t(`configurationTransfer.resources.${resource}`)}</dt><dd>{counts[resource]}</dd>
    </div>)}</dl>
  </section>;
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
    <div className="configuration-binding-fields">
      <label className="field configuration-endpoint"><span>{t("configurationTransfer.endpoint")}</span>
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
      {provider.access_surface === "bedrock-mantle" && <label className="field configuration-endpoint"><span>{t("configurationTransfer.bedrockProject")}</span>
        <input value={target.bedrock_project_id ?? provider.bedrock_project_id ?? ""} onChange={(event) =>
          onChange({ bedrock_project_id: event.target.value })} />
      </label>}
    </div>
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
  return <div className="configuration-detail configuration-list">
    <strong>{t("configurationTransfer.activationTitle")}</strong>
    <ol>
      {deployments.length > 0 && <li>{t("configurationTransfer.activateDeployments")}{" "}
        {labels(deployments, "deployments", "deployment")}</li>}
      {routes.length > 0 && <li>{t("configurationTransfer.activateRoutes")}{" "}
        {labels(routes, "routes", "route")}</li>}
    </ol>
  </div>;
}
