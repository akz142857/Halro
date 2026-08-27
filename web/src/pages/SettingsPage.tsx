import { useMutation, useQuery } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNotify } from "../notifications";
import { api } from "../api";
import { ErrorState, Loading, PageHeader, StatusDot } from "../components";
import { useInstantFormatter } from "../format";
import { navigate, usePathname } from "../navigation";
import { useIsReadOnly, useSession } from "../session";
import type { ModelCatalogInfo, ReloadStatus, SystemConfigEntry, WritePathSummary } from "../types";
import { AdminUsersSection } from "./AdminUsersSection";
import { AccountingTimezoneForm } from "./AccountingTimezoneForm";
import { AppearanceForm } from "./AppearanceForm";
import { InstanceLanguageForm, PersonalLanguageForm } from "./LanguageSettingsForm";
import { MFASettings } from "./MFASettings";
import { PasswordChangeForm } from "./PasswordChangeForm";
import { RuntimeSettingsForm } from "./RuntimeSettingsForm";
// One list, in the order the nav renders them. The routing used to repeat the
// names in a chain of comparisons and the nav in its own array, so a pane could
// be reachable by URL and absent from the menu, or the reverse.
const SETTINGS_PANES = ["general", "security", "accounts", "instance", "config", "diagnostics"] as const;
type SettingsPane = (typeof SETTINGS_PANES)[number];

function paneFromPath(path: string): SettingsPane {
  const pane = path.split("/")[3];
  return SETTINGS_PANES.find((candidate) => candidate === pane) ?? "general";
}

export function SettingsPage({ mfaSetupRequired = false }: { mfaSetupRequired?: boolean }) {
  const { t } = useTranslation();
  const session = useSession();
  const path = usePathname();
  const pane = mfaSetupRequired ? "security" : paneFromPath(path);
  const status = useQuery({
    queryKey: ["system-status"],
    queryFn: api.systemStatus,
    refetchInterval: 15_000,
    enabled: !mfaSetupRequired && pane === "diagnostics",
  });
  // The effective config.yaml gets its own pane rather than riding along with
  // the diagnostics, whose question is whether the instance is well right now.
  // This one answers what the instance is running with, which is a subject
  // large enough to be its own place and not a card at the bottom of another.
  const config = useQuery({
    queryKey: ["system-config"],
    queryFn: api.systemConfig,
    enabled: !mfaSetupRequired && pane === "config",
  });
  const modelCatalog = useQuery({
    queryKey: ["model-catalog"], queryFn: api.modelCatalog,
    enabled: !mfaSetupRequired && pane === "config",
    refetchInterval: 30_000,
  });
  const settings = useQuery({ queryKey: ["settings"], queryFn: api.settings, enabled: !mfaSetupRequired && pane === "instance" });
  const accounting = useQuery({ queryKey: ["accounting-settings"], queryFn: api.accountingSettings, enabled: !mfaSetupRequired && pane === "instance" });
  const uiSettings = useQuery({ queryKey: ["ui-settings"], queryFn: api.uiSettings, enabled: !mfaSetupRequired && (pane === "general" || pane === "instance") });
  const preferences = useQuery({ queryKey: ["preferences"], queryFn: api.preferences, enabled: !mfaSetupRequired && (pane === "general" || pane === "instance") });
  const queries = pane === "diagnostics" ? [status] : pane === "config" ? [config, modelCatalog] : pane === "instance" ? [settings, uiSettings, preferences, accounting] : pane === "general" ? [uiSettings, preferences] : [];
  const pending = queries.some((query) => query.isPending);
  const error = queries.find((query) => query.error)?.error;
  const accountingLabels = [t("settings.healthy"), t("settings.degraded"), t("settings.unavailable"), t("settings.recoveryRequired")];
  const metricLabels: Record<string, string> = {
    batches: t("settings.batches"),
    records: t("settings.records"),
    errors: t("settings.errors"),
    queuedepth: t("settings.queueDepth"),
    queuecapacity: t("settings.queueCapacity"),
    lasthash: t("settings.lastHash"),
    bytes: t("settings.bytes"),
  };
  return (
    <>
      <PageHeader
        eyebrow={t("settings.eyebrow")}
        title={mfaSetupRequired?t("settings.mfaRequiredTitle"):t("settings.title")}
        description={mfaSetupRequired?t("settings.mfaRequiredDescription"):t("settings.description")}
      />
      {mfaSetupRequired ? <div className="settings-pane"><MFASettings /></div> : (
        <div className="settings-shell">
          <nav className="settings-nav" aria-label={t("settings.sectionNavigation")}>
            {SETTINGS_PANES.map((item) => (
              <a key={item} href={`/admin/settings/${item}`} className={pane === item ? "active" : ""} aria-current={pane === item ? "page" : undefined} onClick={(event) => { event.preventDefault(); navigate(`/admin/settings/${item}`); }}>{t(`settings.panes.${item}`)}</a>
            ))}
          </nav>
          <div className="settings-pane">
            {pending && <Loading />}
            {error && <ErrorState error={error} />}
            {!pending && !error && pane === "general" && uiSettings.data && preferences.data && <section aria-labelledby="general-title"><SettingsGroupHeader title={t("settings.panes.general")} description={t("settings.generalDescription")} id="general-title" /><AppearanceForm preferences={preferences.data.data} /><PersonalLanguageForm ui={uiSettings.data.data} preferences={preferences.data.data} /></section>}
            {pane === "security" && <section aria-labelledby="security-title"><SettingsGroupHeader title={t("settings.panes.security")} description={t("settings.securityDescription")} id="security-title" /><PasswordChangeForm username={session?.username} /><MFASettings username={session?.username} /></section>}
            {pane === "accounts" && <section aria-labelledby="accounts-title"><SettingsGroupHeader title={t("settings.panes.accounts")} description={t("settings.accountsDescription")} id="accounts-title" /><AdminUsersSection /></section>}
            {!pending && !error && pane === "instance" && uiSettings.data && preferences.data && settings.data && <section aria-labelledby="instance-title"><SettingsGroupHeader title={t("settings.panes.instance")} description={t("settings.instanceDescription")} id="instance-title" /><InstanceLanguageForm ui={uiSettings.data.data} preferences={preferences.data.data} />{accounting.data && <AccountingTimezoneForm settings={accounting.data.data} />}<RuntimeSettingsForm settings={settings.data.data} /></section>}
            {!pending && !error && pane === "config" && config.data && modelCatalog.data && <section aria-labelledby="config-title"><SettingsGroupHeader title={t("settings.panes.config")} description={t("settings.configPreviewDescription")} id="config-title" /><ModelCatalogCard info={modelCatalog.data} onRefresh={() => modelCatalog.refetch()} /><ConfigPreviewCard yaml={config.data.yaml} entries={config.data.entries} /></section>}
            {!pending && !error && pane === "diagnostics" && status.data && <DiagnosticsPane status={status.data} accountingLabels={accountingLabels} metricLabels={metricLabels} />}
          </div>
        </div>
      )}
    </>
  );
}

function ModelCatalogCard({ info, onRefresh }: { info: ModelCatalogInfo; onRefresh: () => Promise<unknown> }) {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const formatInstant = useInstantFormatter();
  const { notify } = useNotify();
  const refresh = useMutation({
    mutationFn: api.refreshModelCatalog,
    onSettled: () => onRefresh(),
    onSuccess: () => notify({ tone: "success", title: t("settings.modelCatalogRefreshComplete") }),
  });
  const status = info.status;
  const degraded = status.state === "degraded" || status.state === "catalog_unavailable";
  return <section className={`panel system-card model-catalog-card${degraded ? " warning" : ""}`} aria-labelledby="model-catalog-title">
    <header className="config-overview-header">
      <div><h3 id="model-catalog-title">{t("settings.modelCatalogTitle")}</h3><p>{t("settings.modelCatalogDescription")}</p></div>
      <span className="config-overview-state" role="status" aria-live="polite"><StatusDot ok={!degraded} />{t(`settings.modelCatalogStates.${status.state}`)}</span>
    </header>
    <dl>
      <div><dt>{t("settings.modelCatalogSource")}</dt><dd>{t(`deployments.capabilitySources.${status.source}`, { defaultValue: status.source })}</dd></div>
      <div><dt>{t("settings.modelCatalogRevision")}</dt><dd><code>{status.revision}</code></dd></div>
      <div><dt>{t("settings.modelCatalogSequence")}</dt><dd>{status.sequence ?? "—"}</dd></div>
      <div><dt>{t("settings.modelCatalogLastSuccess")}</dt><dd>{formatInstant(status.last_success_at, "full")}</dd></div>
      <div><dt>{t("settings.modelCatalogTrustRoots")}</dt><dd>{info.trust_root_count}</dd></div>
      {status.error_class && <div><dt>{t("settings.modelCatalogError")}</dt><dd>{t(`settings.modelCatalogErrors.${status.error_class}`, { defaultValue: t("settings.modelCatalogErrors.unknown") })}</dd></div>}
    </dl>
    <div className="form-actions"><button type="button" className="button ghost" disabled={!status.enabled || readOnly || refresh.isPending} aria-describedby={readOnly ? "model-catalog-read-only" : undefined} onClick={() => refresh.mutate()}>{refresh.isPending ? t("common.loading") : t("settings.refreshModelCatalog")}</button></div>
    {readOnly && <span id="model-catalog-read-only" className="sr-only">{t("settings.modelCatalogReadOnly")}</span>}
    {!status.enabled && <p className="muted">{t("settings.modelCatalogDisabledHint")}</p>}
    {refresh.error && <ErrorState error={refresh.error} />}
  </section>;
}

function DiagnosticsPane({ status, accountingLabels, metricLabels }: { status: Awaited<ReturnType<typeof api.systemStatus>>; accountingLabels: string[]; metricLabels: Record<string, string> }) {
  const { t } = useTranslation();
  const formatInstant = useInstantFormatter();
  const activation = status.activation;
  // Every domain keeps its row, healthy or not. A panel that drops the rows it
  // has nothing to report about answers "is redaction current?" with silence,
  // which reads the same as "there is no such thing as redaction here".
  const activationDomains = activation?.domains ?? [];
  return <section aria-labelledby="diagnostics-title">
    <SettingsGroupHeader title={t("settings.panes.diagnostics")} description={t("settings.systemDescription")} id="diagnostics-title" />
    {(status.accounting_status !== 0 || status.draining || activation?.stale) && <SettingsOverview status={status} />}
    {activation?.stale && <div className="notice warning" role="alert">
      <strong>{t("settings.activationStale")}</strong>
      <span>{t("settings.activationStaleDescription")}</span>
    </div>}
    <div className="settings-grid">
          <details id="system" className="panel system-card diagnostic-details" open>
            <summary><span>{t("settings.build")}</span><strong>Halro {status.build.version || "development"}</strong></summary>
            <dl>
              <div><dt>{t("settings.commit")}</dt><dd><code>{status.build.commit || t("common.local")}</code></dd></div>
              <div><dt>{t("settings.buildDate")}</dt><dd>{status.build.date || "—"}</dd></div>
              <div><dt>{t("settings.draining")}</dt><dd>{status.draining ? t("common.yes") : t("common.no")}</dd></div>
            </dl>
          </details>
          <details className="panel system-card diagnostic-details" open>
            <summary><span>{t("settings.durability")}</span><strong><StatusDot ok={status.accounting_status === 0} />{accountingLabels[status.accounting_status] || t("common.unknown")}</strong></summary>
            <dl>
              {Object.entries(status.wal).map(([key, value]) => (
                <div key={key}><dt>{metricLabels[key.replaceAll("_", "").toLowerCase()] || key.replaceAll("_", " ")}</dt><dd>{value}</dd></div>
              ))}
            </dl>
          </details>
          <WritePathCard summary={status.write_path} batches={Number(status.wal?.batches ?? 0)} />
          {activation && <details id="activation" className="panel system-card diagnostic-details" open>
            <summary><span>{t("settings.activationTitle")}</span><strong><StatusDot ok={!activation.stale} />{t(activation.stale ? "settings.activationStale" : "settings.activationCurrent")}</strong></summary>
            <p>{t(activation.stale ? "settings.activationStaleDescription" : "settings.activationCurrentDescription")}</p>
            <dl>
              <div><dt>{t("settings.activationGeneration")}</dt><dd>{activation.generation}</dd></div>
              {activation.stale_since && <div><dt>{t("settings.activationSince")}</dt><dd>{formatInstant(activation.stale_since, "full")}</dd></div>}
              {activationDomains.map((domain) => <div key={domain.domain}>
                <dt>{t(`settings.activationDomainNames.${domain.domain}`)}</dt>
                <dd>{domain.stale ? (domain.reason || t("common.unknown")) : t("settings.activationDomainCurrent")}</dd>
              </div>)}
            </dl>
            {activation.stale && <p>{t("settings.activationRecovery")}</p>}
          </details>}
          {status.reload && <ReloadCard reload={status.reload} />}
          <details className="panel system-card diagnostic-details" open>
            <summary><span>{t("settings.auditHead")}</span><strong>{t("settings.chainCheckpoint")}</strong></summary>
            <dl>
              {Object.entries(status.audit).map(([key, value]) => (
                <div key={key}><dt>{metricLabels[key.replaceAll("_", "").toLowerCase()] || key.replaceAll("_", " ")}</dt><dd>{String(value)}</dd></div>
              ))}
            </dl>
          </details>
    </div>
  </section>;
}

// What SIGHUP last replaced, and which certificate is actually being served.
// Once material can change without a restart, the file on disk stops being an
// answer to "what is in force" — the process is, and this is where it says so.
function ReloadCard({ reload }: { reload: ReloadStatus }) {
  const { t } = useTranslation();
  const formatInstant = useInstantFormatter();
  const now = Date.now();
  const expiries = reload.certificates.map((certificate) => ({
    ...certificate,
    daysLeft: Math.floor((new Date(certificate.not_after).getTime() - now) / 86_400_000),
  }));
  const failing = reload.items.some((item) => item.failures > 0);
  const expiring = expiries.some((certificate) => certificate.daysLeft < 30);
  return <details id="reload" className="panel system-card diagnostic-details" open>
    <summary>
      <span>{t("settings.reloadTitle")}</span>
      <strong><StatusDot ok={!failing && !expiring} />{t(failing ? "settings.reloadFailing" : expiring ? "settings.reloadExpiringSoon" : "settings.reloadHealthy")}</strong>
    </summary>
    <p>{t("settings.reloadDescription")}</p>
    <dl>
      {expiries.map((certificate) => <div key={`${certificate.scope}:${certificate.name}`}>
        <dt>{t(`settings.reloadScopes.${certificate.scope}`)} · {certificate.name}</dt>
        <dd>
          {formatInstant(certificate.not_after, "full")}
          {certificate.daysLeft < 30 && <span className="badge warning">{certificate.daysLeft <= 0
            ? t("settings.reloadExpired")
            : t("settings.reloadDaysLeft", { count: certificate.daysLeft })}</span>}
          {" "}<code>{certificate.fingerprint}</code>
        </dd>
      </div>)}
      {/* Every item keeps its row. An item this deployment does not have is
          said so explicitly, because a missing row and a never-reloaded one
          read the same and only one of them is a question. */}
      {reload.items.map((item) => <div key={item.item}>
        <dt>{t(`settings.reloadItems.${item.item}`)}</dt>
        <dd>
          {!item.applies
            ? t("settings.reloadNotConfigured")
            : item.last_success
              ? formatInstant(item.last_success, "full")
              : t("settings.reloadNever")}
          {item.failures > 0 && <span className="badge warning">{t("settings.reloadFailureCount", { count: item.failures })}</span>}
        </dd>
      </div>)}
    </dl>
  </details>;
}

// The durable write path. This is the answer to "what is this instance doing
// right now" for an operator who has not stood up Prometheus — the same numbers
// the metrics endpoint exposes, already reduced to the means that matter.
function WritePathCard({ summary, batches }: { summary?: WritePathSummary; batches: number }) {
  const { t } = useTranslation();
  // Optional even though the server always sends it: this is the card an
  // operator opens when the instance is misbehaving, so it must not be the thing
  // that blanks the page.
  const idle = !summary || (summary.wal_sync_seconds === 0 && summary.project_lock_held_seconds === 0);
  // Rows always render. Zero is data — "over 0 barriers" answers the question
  // this card exists for — and hiding the table behind an idle branch is what
  // turned a diagnostics panel into three paragraphs of prose. The CLI never
  // hid it either.
  const rows: [string, string][] = summary ? [
    [t("settings.walSyncSeconds"), formatMillis(summary.wal_sync_seconds)],
    [t("settings.walBatchSize"), formatFactor(summary.wal_batch_size)],
    [t("settings.projectLockWaitSeconds"), formatMillis(summary.project_lock_wait_seconds)],
    [t("settings.projectLockHeldSeconds"), formatMillis(summary.project_lock_held_seconds)],
    [t("settings.projectEventsPerSecond"), formatFactor(summary.project_events_per_second)],
    [t("settings.metadataBatchSize"), formatFactor(summary.metadata_batch_size)],
    [t("settings.metadataWriteSeconds"), formatMillis(summary.metadata_write_seconds)],
  ] : [];
  // The reading that looks like a slow disk and is not. Only stated once there
  // is enough traffic for the batch size to mean anything.
  const notCoalescing = !!summary && batches >= 20 && summary.wal_batch_size > 0 && summary.wal_batch_size < 1.2;
  return (
    <details className="panel system-card diagnostic-details write-path" open>
      <summary>
        <span>{t("settings.writePathTitle")}</span>
        <strong>{idle || !summary?.project_requests_per_second
          ? t("settings.writePathUnknownRate")
          : t("settings.writePathRequestsPerSecond", { rate: formatFactor(summary.project_requests_per_second).trim() })}</strong>
      </summary>
      <p className="write-path-caption">{t("settings.writePathDescription")}</p>
      <dl>{rows.map(([label, value]) => <div key={label}><dt>{label}</dt><dd>{value}</dd></div>)}</dl>
      <p className="write-path-meta">{idle ? t("settings.writePathIdle") : t("settings.writePathWindow")}</p>
      {notCoalescing && <p className="notice warning">{t("settings.writePathNotCoalescing")}</p>}
    </details>
  );
}

// Sub-millisecond fsyncs are normal on NVMe and tens of milliseconds are normal
// on a laptop filesystem, so the unit has to stay readable across three orders
// of magnitude rather than round everything to 0 ms.
function formatMillis(seconds: number) {
  const millis = seconds * 1000;
  if (millis === 0) return "0 ms";
  if (millis < 1) return `${millis.toFixed(3)} ms`;
  if (millis < 10) return `${millis.toFixed(2)} ms`;
  return `${millis.toFixed(1)} ms`;
}

function formatFactor(value: number) {
  if (value === 0) return "—";
  return value < 10 ? value.toFixed(2) : value.toFixed(1);
}

// The disclosure state lives for the session rather than in storage: an
// operator who opened the YAML once is usually still debugging, and reopening
// it on every visit to the pane is the kind of small friction that makes people
// stop using the summary above it.
let yamlDisclosureOpen = false;

function ConfigPreviewCard({ yaml, entries }: { yaml: string; entries: SystemConfigEntry[] }) {
  const { t, i18n } = useTranslation();
  const [copyStatus, setCopyStatus] = useState("");
  const [yamlOpen, setYamlOpen] = useState(yamlDisclosureOpen);
  const [query, setQuery] = useState("");
  const english = i18n.resolvedLanguage === "en-US";
  const visibleEntries = useMemo(() => {
    const needle = query.trim().toLocaleLowerCase();
    if (!needle) return entries;
    return entries.filter((entry) => [entry.path, entry.title_zh, entry.title_en, entry.description_zh, entry.description_en, entry.value]
      .some((value) => value.toLocaleLowerCase().includes(needle)));
  }, [entries, query]);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(yaml);
      setCopyStatus(t("settings.configPreviewCopied"));
    } catch {
      setCopyStatus(t("settings.configPreviewCopyFailed"));
    }
  };
  return <section className="config-overview" aria-labelledby="effective-config-title">
    <header className="config-overview-header">
      <div>
        <h3 id="effective-config-title">{t("settings.effectiveConfig")}</h3>
        <p>{t("settings.effectiveConfigDescription")}</p>
      </div>
      <span className="config-overview-state">{t("settings.readOnlyRestart")}</span>
    </header>
    <div className="config-list-toolbar">
      <label>
        <span className="sr-only">{t("settings.searchConfig")}</span>
        <input autoComplete="off" type="search" value={query} onChange={(event) => setQuery(event.target.value)} placeholder={t("settings.searchConfigPlaceholder")} />
      </label>
      <span>{t("settings.configResultCount", { visible: visibleEntries.length, total: entries.length })}</span>
    </div>
    <div className="config-list" role="list">
      {visibleEntries.map((entry) => <ConfigEntryRow key={entry.path} entry={entry} english={english} />)}
      {visibleEntries.length === 0 && <p className="config-list-empty">{t("settings.noConfigMatches")}</p>}
    </div>
    <details
      className="config-preview"
      open={yamlOpen}
      onToggle={(event) => {
        const open = (event.currentTarget as HTMLDetailsElement).open;
        yamlDisclosureOpen = open;
        setYamlOpen(open);
      }}
    >
      <summary>{t("settings.effectiveConfigYaml")}</summary>
      <p className="config-preview-description">{t("settings.effectiveConfigYamlDescription")}</p>
      <pre className="config-preview-body" tabIndex={0} aria-label={t("settings.effectiveConfigYaml")}><code>{yaml}</code></pre>
      <div className="form-actions">
        <button type="button" className="button ghost" onClick={() => void copy()}>{t("settings.copyYaml")}</button>
        <span aria-live="polite">{copyStatus}</span>
      </div>
    </details>
  </section>;
}

function ConfigEntryRow({ entry, english }: { entry: SystemConfigEntry; english: boolean }) {
  const { t } = useTranslation();
  const title = (english ? entry.title_en : entry.title_zh) || entry.path;
  const description = english ? entry.description_en : entry.description_zh;
  return <article className="config-entry" role="listitem">
    <div className="config-entry-copy">
      <div>
        <h4>{title}</h4>
        <code>{entry.path}</code>
      </div>
      <p>{description || t("settings.configDescriptionMissing")}</p>
    </div>
    <div className={entry.kind === "boolean" ? `config-entry-value config-value-state ${entry.value === "true" ? "enabled" : "disabled"}` : "config-entry-value"}>
      {formatConfigEntry(entry, t)}
    </div>
  </article>;
}

function formatConfigEntry(entry: SystemConfigEntry, t: (key: string, options?: Record<string, unknown>) => string) {
  if (!entry.value || entry.value === "[]") return t("settings.notConfigured");
  if (entry.kind === "boolean") return t(entry.value === "true" ? "settings.enabled" : "settings.disabled");
  return entry.value;
}

function SettingsGroupHeader({ title, description, id }: { title: string; description: string; id: string }) {
  return <header className="settings-group-header"><h2 id={id}>{title}</h2><p>{description}</p></header>;
}

function SettingsOverview({ status }: { status: Awaited<ReturnType<typeof api.systemStatus>> }) {
  const { t } = useTranslation();
  const healthy = status.accounting_status === 0 && !status.draining && !status.activation?.stale;
  return (
    <section className="settings-overview" aria-labelledby="settings-overview-title">
      <div className="settings-overview-copy">
        <p className="eyebrow">{t("settings.overviewEyebrow")}</p>
        <h2 id="settings-overview-title"><StatusDot ok={healthy} />{healthy ? t("settings.systemReady") : t("settings.systemAttention")}</h2>
        <p>{healthy ? t("settings.systemReadyDescription") : t("settings.systemAttentionDescription")}</p>
      </div>
      <a className="button" href="#system">{t("settings.reviewSystemDetails")}</a>
    </section>
  );
}
