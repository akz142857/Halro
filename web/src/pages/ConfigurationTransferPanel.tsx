import { useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { ApiError, api, type Reauth } from "../api";
import { ConfirmButton, ErrorState } from "../components";
import { localizedError } from "../i18n/errors";
import { setNavigationBlocked } from "../navigation";
import type {
  ConfigurationBindings, ConfigurationImportPreview, ConfigurationImportResult, ConfigurationImportSelection,
  ConfigurationNotice, ConfigurationObject, ConfigurationProviderReview, ConfigurationProviderTarget, ConfigurationResource, Credential, ProviderEgressProxy,
} from "../types";

const fileLimit = 2 * 1024 * 1024;
const emptyTarget: ConfigurationProviderTarget = { credential_id: "" };
const defaultSelection: ConfigurationImportSelection = { mode: "all", include_gateway_keys: false };
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
  const [selection, setSelection] = useState(defaultSelection);
  const [scopeDirty, setScopeDirty] = useState(false);
  const [showBindingIssues, setShowBindingIssues] = useState(false);
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
  const fileRejected = preview?.issues.some(({ code }) => code === "record_count" || code === "unsupported_version" || code === "reexport_required");
  const emptyBundle = preview && resources.every((resource) => preview.counts[resource] === 0);
  const targetBlocked = preview?.issues.some(({ code }) => code === "target_not_empty");
  const hasSelection = selection.mode === "all" || selection.project_ids.length > 0 || selection.route_aliases.length > 0;
  const showBindings = hasProviders && !scopeDirty && !targetBlocked;
  const canApply = Boolean(preview?.plan_id) && !scopeDirty && !targetBlocked;
  const issues = targetBlocked ? preview?.issues.filter(({ resource }) => resource === "target") ?? [] :
    scopeDirty ? [] : preview?.issues.filter(({ code }) => showBindingIssues || code !== "target_credential_required") ?? [];
  const credentials = useQuery({ queryKey: ["credentials"], queryFn: api.credentials, enabled: showBindings, refetchOnWindowFocus: true });
  const proxies = useQuery({ queryKey: ["provider-egress-proxies"], queryFn: api.providerEgressProxies, enabled: showBindings, refetchOnWindowFocus: true });

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
    setSelection(defaultSelection);
    setScopeDirty(false);
    setShowBindingIssues(false);
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
      setPreview(await api.previewConfigurationImport(content, {}, defaultSelection));
    } catch (error) {
      if (error instanceof ApiError && (error.status === 413 || error.code === "invalid_request")) {
        setFileError(error.status === 413 ? "fileSize" : "fileInvalid");
        setBundle("");
      } else {
        setImportError(error);
      }
    } finally {
      setBusy(false);
    }
  }

  function updateTarget(id: string, change: Partial<ConfigurationProviderTarget>) {
    setAcceptDeclarations(false);
    setTargets((previous) => new Map(previous).set(id, { ...(previous.get(id) ?? emptyTarget), ...change }));
    setPreview((previous) => previous && { ...previous, plan_id: undefined });
  }

  function updateSelection(value: ConfigurationImportSelection) {
    setSelection(value.mode === "selected" && value.project_ids.length === 0 ? { ...value, include_gateway_keys: false } : value);
    setScopeDirty(true);
    setAcceptDeclarations(false);
    setImportError(null);
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
      setPreview(await api.previewConfigurationImport(bundle, bindings(), selection));
      setScopeDirty(false);
      setShowBindingIssues(true);
    } catch (error) {
      setImportError(error);
    } finally {
      setBusy(false);
    }
  }

  async function applyImport(reauth: Reauth) {
    if (!preview?.plan_id || !canApply) return;
    setBusy(true);
    try {
      const imported = await api.applyConfigurationImport(bundle, bindings(), preview.plan_id, reauth, acceptDeclarations, selection);
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
      if (!current?.issues.length) setImportError(error);
      setShowBindingIssues(true);
    } finally {
      setBusy(false);
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
          {!targetBlocked && preview.selection_options && <ConfigurationSelectionFields
            options={preview.selection_options} selection={selection} disabled={busy} onChange={updateSelection} />}
          {!fileRejected && !scopeDirty && <ConfigurationCounts counts={preview.counts} pending={targetBlocked} />}
          {targetBlocked && <p className="configuration-transfer-note">{t("configurationTransfer.targetBlockedHelp")}</p>}
          {issues.length > 0 && <div className={`notice ${fileRejected || targetBlocked ? "error" : "warning"} configuration-list`} role={fileRejected || targetBlocked ? "alert" : "status"}>
            <strong>{t("configurationTransfer.issues")}</strong>
            <ul>{issues.map((issue, index) => <li key={index}>
              {issue.id ? <>{preview.providers.find((provider) => provider.id === issue.id)?.name ?? issue.id}{" — "}</> : null}
              {issue.fields ? localizedError(t, new ApiError(400, issue.message, issue.code, "", issue.fields)) :
                issue.code === "record_count" && emptyBundle ? t("configurationTransfer.emptyFile") :
                  t(`configurationTransfer.issueCodes.${issue.code}`, { defaultValue: issue.message })}
            </li>)}</ul>
          </div>}
          {showBindings && <>
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
              credentials={credentials.data?.items ?? []} credentialsReady={credentials.isSuccess} proxies={proxies.data?.items ?? []}
              disabled={busy} onChange={(change) => updateTarget(provider.id, change)} />)}
          </>}
          {!scopeDirty && <ConfigurationNotices notices={preview.notices ?? []} />}
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
          <button type="button" className={`button${canApply || targetBlocked ? "" : " primary"}`} disabled={busy || !hasSelection} onClick={() => void reviewImport()}>
            {busy ? t("common.working") : t(targetBlocked ? "configurationTransfer.recheckTarget" : "configurationTransfer.preview")}
          </button>
          {preview && canApply && <ConfirmButton className="button primary" label={t("configurationTransfer.apply")}
            title={t("configurationTransfer.apply")} confirmLabel={t(preview.counts.gateway_keys > 0 ? "configurationTransfer.confirm" : "configurationTransfer.confirmWithoutKeys")}
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

function ConfigurationSelectionFields({ options, selection, disabled, onChange }: {
  options: NonNullable<ConfigurationImportPreview["selection_options"]>;
  selection: ConfigurationImportSelection;
  disabled: boolean;
  onChange: (selection: ConfigurationImportSelection) => void;
}) {
  const { t } = useTranslation();
  const canSelect = options.projects.length > 0 || options.route_aliases.length > 0;
  const toggle = (values: string[], value: string, checked: boolean) => checked ? [...values, value] : values.filter((entry) => entry !== value);
  return <fieldset className="configuration-binding" disabled={disabled}>
    <legend>{t("configurationTransfer.selectionTitle")}</legend>
    {canSelect && <>
      <div className="configuration-scope-modes">
        <label className="configuration-acknowledgement"><input type="radio" name="configuration-scope" checked={selection.mode === "all"}
          onChange={() => onChange({ mode: "all", include_gateway_keys: selection.include_gateway_keys })} />
          <span>{t("configurationTransfer.allConfiguration")}</span></label>
        <label className="configuration-acknowledgement"><input type="radio" name="configuration-scope" checked={selection.mode === "selected"}
          onChange={() => onChange({ mode: "selected", project_ids: [], route_aliases: [], include_gateway_keys: false })} />
          <span>{t("configurationTransfer.selectedConfiguration")}</span></label>
      </div>
      {selection.mode === "selected" && <>
        <p className="configuration-transfer-description">{t("configurationTransfer.selectionHelp")}</p>
        <div className="configuration-binding-fields configuration-scope-choices">
          {options.projects.length > 0 && <fieldset className="configuration-scope-options">
            <legend>{t("configurationTransfer.resources.projects")}</legend>
            {options.projects.map((project) => <label className="configuration-acknowledgement" key={project.id}>
              <input type="checkbox" checked={selection.project_ids.includes(project.id)} onChange={(event) =>
                onChange({ ...selection, project_ids: toggle(selection.project_ids, project.id, event.target.checked) })} />
              <span>{project.name} <small className="configuration-transfer-note">{project.id}</small></span>
            </label>)}
          </fieldset>}
          {options.route_aliases.length > 0 && <fieldset className="configuration-scope-options">
            <legend>{t("configurationTransfer.modelAliases")}</legend>
            {options.route_aliases.map((alias) => <label className="configuration-acknowledgement" key={alias}>
              <input type="checkbox" checked={selection.route_aliases.includes(alias)} onChange={(event) =>
                onChange({ ...selection, route_aliases: toggle(selection.route_aliases, alias, event.target.checked) })} />
              <span>{alias}</span>
            </label>)}
          </fieldset>}
        </div>
      </>}
    </>}
    {options.gateway_keys > 0 && <label className="configuration-acknowledgement">
      <input type="checkbox" checked={selection.include_gateway_keys} disabled={selection.mode === "selected" && !selection.project_ids.length} onChange={(event) =>
        onChange({ ...selection, include_gateway_keys: event.target.checked })} />
      <span>{t("configurationTransfer.issueGatewayKeys")}</span>
    </label>}
  </fieldset>;
}

function ConfigurationCounts({ counts, pending = false }: { counts: ConfigurationImportPreview["counts"]; pending?: boolean }) {
  const { t } = useTranslation();
  const populated = resources.filter((resource) => counts[resource] > 0);
  if (!populated.length) return null;
  const title = t(pending ? "configurationTransfer.fileSummary" : "configurationTransfer.configurationSummary");
  return <section className="configuration-count-summary" aria-label={title}>
    <h4>{title}</h4>
    <dl className="configuration-counts">{populated.map((resource) => <div key={resource}>
      <dt>{t(`configurationTransfer.resources.${resource}`)}</dt><dd>{counts[resource]}</dd>
    </div>)}</dl>
  </section>;
}

function configurationEndpointOrigin(value: string): string | undefined {
  try {
    const url = new URL(value);
    if (url.protocol !== "https:" && url.protocol !== "http:") return;
    url.hostname = url.hostname.replace(/\.$/, "");
    return url.origin;
  } catch {
    return undefined;
  }
}

function ConfigurationProviderBinding({ provider, target, credentials, credentialsReady, proxies, disabled, onChange }: {
  provider: ConfigurationProviderReview;
  target: ConfigurationProviderTarget;
  credentials: Credential[];
  credentialsReady: boolean;
  proxies: ProviderEgressProxy[];
  disabled: boolean;
  onChange: (change: Partial<ConfigurationProviderTarget>) => void;
}) {
  const { t } = useTranslation();
  const endpoint = target.base_url ?? provider.base_url;
  const origin = configurationEndpointOrigin(endpoint);
  const sameType = credentials.filter((credential) => credential.secret_configured && credential.type === provider.type &&
    credential.access_surface === provider.access_surface && credential.scheme === provider.credential_scheme);
  const compatible = origin ? sameType.filter((credential) => configurationEndpointOrigin(credential.bound_base_url) === origin) : [];
  const selectedCredential = sameType.find((credential) => credential.id === target.credential_id);
  const policyIsCurrent = endpoint.trim() === provider.base_url;
  return <fieldset className="configuration-binding" disabled={disabled}>
    <legend>{provider.name}</legend>
    <div className="configuration-binding-fields">
      <label className="field configuration-endpoint"><span>{t("configurationTransfer.endpoint")}</span>
        <input type="url" value={endpoint} onChange={(event) => {
          const next = event.target.value;
          onChange({ base_url: next, acknowledged_policy_revision: undefined,
            ...(selectedCredential && configurationEndpointOrigin(next) !== configurationEndpointOrigin(selectedCredential.bound_base_url) ? { credential_id: "" } : {}),
          });
        }} />
      </label>
      <label className="field"><span>{t("configurationTransfer.credential")}</span>
        <select value={compatible.some((credential) => credential.id === target.credential_id) ? target.credential_id : ""}
          disabled={compatible.length === 0} onChange={(event) => onChange({ credential_id: event.target.value })}>
          <option value="">{t("configurationTransfer.selectCredential")}</option>
          {compatible.map((credential) => <option key={credential.id} value={credential.id}>
            {credential.name} ({credential.bound_base_url})
          </option>)}
        </select>
        {credentialsReady && compatible.length === 0 && <small>{t(origin ? "configurationTransfer.noMatchingCredential" : "configurationTransfer.enterEndpointFirst")}</small>}
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
    {policyIsCurrent && provider.policy_revision && <label className="configuration-acknowledgement">
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
