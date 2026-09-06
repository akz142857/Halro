import { FormEvent, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { EmptyState, ErrorState, Loading, Modal, SegmentedTabs } from "../../components";
import { money, useInstantFormatter } from "../../format";
import type { GovernanceSummary, Outcome, OutcomeDefinition, WorkUnit } from "../../types";
import { CopyableID, GovernanceBadge } from "./GovernancePrimitives";
import { inDateRange, type ResultsView } from "./governance-state";

function valueList(value: string) {
  return value.split(",").map((item) => item.trim()).filter(Boolean);
}

function watermark(value: { generation?: number; sequence: number; offset: number } | undefined) {
  if (!value) return "—";
  return `${value.generation == null ? "" : `g${value.generation} · `}#${value.sequence} · ${value.offset} B`;
}

function summaryReason(summary: GovernanceSummary, metric: "coverage" | "success" | "cost") {
  if (metric === "coverage" && summary.outcome_coverage == null) return summary.outcome_reason || "no_eligible_units";
  if (metric === "success" && summary.success_rate == null) return summary.outcome_reason || "no_evaluated_units";
  if (metric === "cost" && summary.cost_per_success_micros_usd == null) return summary.cost_per_success_reason || "no_successful_units";
  return "complete";
}

interface DefinitionForm {
  source: OutcomeDefinition | null;
  name: string;
  type: "BOOLEAN" | "CATEGORICAL";
  allowed: string[];
  success: string[];
  enabled: boolean;
}

const emptyForm: DefinitionForm = { source: null, name: "", type: "CATEGORICAL", allowed: ["accepted", "rejected"], success: ["accepted"], enabled: true };

function ValueTokens({ label, values, disabled = false, initial = false, onChange }: { label: string; values: string[]; disabled?: boolean; initial?: boolean; onChange: (values: string[]) => void }) {
  const { t } = useTranslation();
  const [draft, setDraft] = useState("");
  function add() {
    const additions = valueList(draft);
    if (!additions.length) return;
    onChange([...new Set([...values, ...additions])]);
    setDraft("");
  }
  return <fieldset className="governance-token-field" disabled={disabled}><legend>{label}</legend><div className="governance-token-editor">{values.map((value) => <span key={value}>{value}<button type="button" aria-label={t("runGovernance.removeValue", { value })} onClick={() => onChange(values.filter((item) => item !== value))}>×</button></span>)}</div><div className="governance-token-add"><input {...(initial ? { "data-modal-initial": true } : {})} aria-label={t("runGovernance.addValueLabel", { label })} value={draft} onChange={(event) => setDraft(event.target.value)} onKeyDown={(event) => { if (event.key === "Enter") { event.preventDefault(); add(); } }} /><button type="button" className="button ghost" onClick={add} disabled={!draft.trim()}>{t("runGovernance.addValue")}</button></div></fieldset>;
}

export function OutcomeWorkspace({
  definitions,
  outcomes,
  workUnits,
  definitionID,
  cohortStart,
  cohortEnd,
  summary,
  summaryPending,
  summaryError,
  mutationError,
  mutationPending,
  onDefinitionChange,
  onCohortStartChange,
  onCohortEndChange,
  onSaveDefinition,
  onToggleDefinition,
}: {
  definitions: OutcomeDefinition[];
  outcomes: Outcome[];
  workUnits: WorkUnit[];
  definitionID: string;
  cohortStart: string;
  cohortEnd: string;
  summary?: GovernanceSummary;
  summaryPending: boolean;
  summaryError: unknown;
  mutationError: unknown;
  mutationPending: boolean;
  onDefinitionChange: (id: string) => void;
  onCohortStartChange: (value: string) => void;
  onCohortEndChange: (value: string) => void;
  onSaveDefinition: (source: OutcomeDefinition | null, body: { name: string; data_type: "BOOLEAN" | "CATEGORICAL"; allowed_values: string[]; success_values: string[]; enabled: boolean }) => Promise<void>;
  onToggleDefinition: (item: OutcomeDefinition) => Promise<void>;
}) {
  const { t } = useTranslation();
  const dateTime = useInstantFormatter();
  const [view, setView] = useState<ResultsView>("analytics");
  const [form, setForm] = useState<DefinitionForm | null>(null);
  const [pendingToggle, setPendingToggle] = useState<OutcomeDefinition | null>(null);
  const selectedDefinition = definitions.find((item) => `${item.id}:${item.version}` === definitionID);
  const latestDefinitions = useMemo(() => {
    const latest = new Map<string, OutcomeDefinition>();
    for (const item of definitions) if (!latest.has(item.id) || latest.get(item.id)!.version < item.version) latest.set(item.id, item);
    return [...latest.values()].sort((a, b) => a.name.localeCompare(b.name));
  }, [definitions]);
  const filteredOutcomes = outcomes.filter((item) => {
    if (selectedDefinition && (item.definition_id !== selectedDefinition.id || item.definition_version !== selectedDefinition.version)) return false;
    const unit = workUnits.find((candidate) => candidate.id === item.work_unit_id);
    return Boolean(unit && inDateRange(unit.period_id, cohortStart, cohortEnd));
  });
  const unlinkedOutcomes = outcomes.filter((item) => (!selectedDefinition || (item.definition_id === selectedDefinition.id && item.definition_version === selectedDefinition.version)) && !workUnits.some((unit) => unit.id === item.work_unit_id)).length;
  const outcomeState = summary?.outcome_completeness ?? (summary && summary.eligible_units === 0 ? "unknown" : summary && summary.evaluated_units < summary.matured_units ? "partial" : "complete");
  const overallState = !summary ? "unknown" : outcomeState === "unknown" || summary.cost_completeness === "unknown" ? "unknown" : outcomeState === "partial" || summary.cost_completeness === "partial" ? "partial" : "complete";

  function openForm(item?: OutcomeDefinition) {
    setForm(item ? { source: item, name: item.name, type: item.data_type, allowed: item.allowed_values.length ? item.allowed_values : ["false", "true"], success: item.success_values, enabled: item.enabled } : { ...emptyForm, allowed: [...emptyForm.allowed], success: [...emptyForm.success] });
  }

  async function submit(event: FormEvent) {
    event.preventDefault();
    if (!form) return;
    try {
      await onSaveDefinition(form.source, { name: form.name, data_type: form.type, allowed_values: form.type === "BOOLEAN" ? [] : form.allowed, success_values: form.success, enabled: form.enabled });
      setForm(null);
    } catch {
      // The mutation error is rendered inside the still-open form.
    }
  }

  return <section className="governance-workspace" id="run-governance-panel" role="tabpanel" aria-labelledby="run-governance-tab-results">
    <div className="governance-subnav"><SegmentedTabs id="run-governance-results" label={t("runGovernance.resultsSections")} value={view} items={[{ key: "analytics", label: t("runGovernance.analytics") }, { key: "definitions", label: t("runGovernance.definitionManagement") }]} onChange={setView} /></div>
    {view === "analytics" && <div id="run-governance-results-panel" role="tabpanel" aria-labelledby="run-governance-results-tab-analytics">
      <div className="governance-toolbar governance-analytics-filter"><label><span>{t("runGovernance.definitionVersion")}</span><select value={definitionID} onChange={(event) => onDefinitionChange(event.target.value)}><option value="">{t("runGovernance.chooseDefinition")}</option>{definitions.map((item) => <option key={`${item.id}:${item.version}`} value={`${item.id}:${item.version}`}>{item.name} · v{item.version}</option>)}</select></label><label><span>{t("runGovernance.cohortStart")}</span><input type="date" value={cohortStart} onChange={(event) => onCohortStartChange(event.target.value)} /></label><label><span>{t("runGovernance.cohortEnd")}</span><input type="date" value={cohortEnd} onChange={(event) => onCohortEndChange(event.target.value)} /></label></div>
      {!selectedDefinition && <EmptyState title={t("runGovernance.chooseDefinition")}>{t("runGovernance.chooseDefinitionDescription")}</EmptyState>}
      {summaryPending && selectedDefinition && <Loading />}{Boolean(summaryError) && <ErrorState error={summaryError} />}
      {unlinkedOutcomes > 0 && <div className="notice warning" role="status">{t("runGovernance.unlinkedOutcomes", { count: unlinkedOutcomes })}</div>}
      {summary && <>
        <div className={`governance-completeness ${overallState}`} role="status"><div><GovernanceBadge tone={overallState === "complete" ? "good" : "warning"}>{t(`runGovernance.summaryState.${overallState}`)}</GovernanceBadge><strong>{t(`runGovernance.summaryReason.${summary.outcome_reason || (outcomeState === "complete" ? "complete" : "missing_outcomes")}`)}</strong></div>{summary.unknown_attempts > 0 && <p>{t("runGovernance.unknownCostExplanation", { count: summary.unknown_attempts })}</p>}{summary.in_progress_cost_micros_usd > 0 && <p>{t("runGovernance.inProgressExplanation", { amount: money(summary.in_progress_cost_micros_usd) })}</p>}</div>
        <div className="governance-primary-metrics"><article><span>{t("runGovernance.coverage")}</span><strong>{summary.outcome_coverage == null ? "—" : `${(summary.outcome_coverage * 100).toFixed(1)}%`}</strong><small>{summary.outcome_coverage == null ? t(`runGovernance.summaryReason.${summaryReason(summary, "coverage")}`) : `${summary.evaluated_units}/${summary.eligible_units}`}</small></article><article><span>{t("runGovernance.successRate")}</span><strong>{summary.success_rate == null ? "—" : `${(summary.success_rate * 100).toFixed(1)}%`}</strong><small>{summary.success_rate == null ? t(`runGovernance.summaryReason.${summaryReason(summary, "success")}`) : `${summary.successful_units}/${summary.evaluated_units}`}</small></article><article><span>{t("runGovernance.costPerSuccess")}</span><strong>{summary.cost_per_success_micros_usd == null ? "—" : money(summary.cost_per_success_micros_usd)}</strong><small>{summary.cost_per_success_reason ? t(`runGovernance.summaryReason.${summary.cost_per_success_reason}`) : t(`runGovernance.${summary.cost_completeness}`)}</small></article></div>
        <div className="governance-evidence-grid"><article><span>{t("runGovernance.knownCost")}</span><strong>{money(summary.known_cost_micros_usd)}</strong><small>{t(`runGovernance.${summary.cost_completeness}`)}</small></article><article><span>{t("runGovernance.estimatedCost")}</span><strong>{money(summary.estimated_cost_micros_usd)}</strong><small>{t("runGovernance.estimatedSubset")}</small></article><article><span>{t("runGovernance.unknownCost")}</span><strong>{summary.unknown_attempts}</strong><small>{t("runGovernance.unknownAttemptUnit")}</small></article></div>
        <section className="governance-funnel"><header><p className="eyebrow">{t("runGovernance.cohortEvidence")}</p><h2>{t("runGovernance.cohortFunnel")}</h2></header><ol><li><span>{t("runGovernance.eligible")}</span><strong>{summary.eligible_units}</strong></li><li><span>{t("runGovernance.mature")}</span><strong>{summary.matured_units}</strong></li><li><span>{t("runGovernance.evaluated")}</span><strong>{summary.evaluated_units}</strong></li><li><span>{t("runGovernance.successful")}</span><strong>{summary.successful_units}</strong></li></ol></section>
        <details className="governance-audit"><summary>{t("runGovernance.dataAndAuditDetails")}</summary><div className="governance-audit-body"><p>{t("runGovernance.generatedAt", { date: dateTime(summary.generated_at, "full") })}</p><div className="governance-audit-watermarks"><code>{t("runGovernance.accountingWatermark", { value: watermark(summary.accounting_watermark) })}</code><code>{t("runGovernance.governanceWatermark", { value: watermark(summary.governance_watermark) })}</code></div></div></details>
        <section className="governance-section"><header className="governance-section-header"><div><p className="eyebrow">{t("runGovernance.journalEyebrow")}</p><h2>{t("runGovernance.filteredOutcomes")}</h2></div><span className="governance-count">{filteredOutcomes.length}</span></header>{filteredOutcomes.length === 0 ? <p className="governance-muted">{t("runGovernance.noOutcomesDescription")}</p> : <div className="governance-outcomes">{filteredOutcomes.map((item) => <article key={item.id}><div><GovernanceBadge tone={item.provisional ? "warning" : "neutral"}>{item.provisional ? t("runGovernance.outcomeProvisional") : t("runGovernance.outcomeFinal")}</GovernanceBadge><strong>{item.value}</strong></div><CopyableID value={item.work_unit_id} label={t("runGovernance.workUnit")} /><span>{item.definition_id} · v{item.definition_version}</span><span>#{item.revision}</span><time dateTime={item.observed_at}>{dateTime(item.observed_at, "full")}</time></article>)}</div>}</section>
      </>}
    </div>}
    {view === "definitions" && <div id="run-governance-results-panel" role="tabpanel" aria-labelledby="run-governance-results-tab-definitions"><section className="governance-section"><header className="governance-section-header"><div><p className="eyebrow">{t("runGovernance.definitionsEyebrow")}</p><h2>{t("runGovernance.outcomeDefinitions")}</h2><p>{t("runGovernance.outcomeDefinitionsDescription")}</p></div><button type="button" className="button primary" onClick={() => openForm()}>{t("runGovernance.createDefinition")}</button></header>{latestDefinitions.length === 0 ? <EmptyState title={t("runGovernance.noDefinitions")}>{t("runGovernance.noDefinitionsDescription")}</EmptyState> : <div className="governance-definitions">{latestDefinitions.map((item) => {
      const history = definitions.filter((candidate) => candidate.id === item.id && candidate.version !== item.version).sort((a, b) => b.version - a.version);
      return <article key={item.id}><div className="governance-definition-main"><div><strong>{item.name}</strong><CopyableID value={item.id} label={t("runGovernance.definition")} /><span>v{item.version} · {t(`runGovernance.${item.data_type.toLowerCase()}`)}</span></div><div className="governance-chips" aria-label={t("runGovernance.successValues")}>{item.success_values.map((value) => <span key={value}>{value}</span>)}</div><GovernanceBadge tone={item.enabled ? "good" : "neutral"}>{item.enabled ? t("common.enabled") : t("common.disabled")}</GovernanceBadge><div className="form-actions"><button type="button" className="button ghost" onClick={() => openForm(item)}>{t("runGovernance.newVersion")}</button><button type="button" className="button ghost" onClick={() => setPendingToggle(item)}>{item.enabled ? t("common.disable") : t("common.enable")}</button></div></div>{history.length > 0 && <details><summary>{t("runGovernance.versionHistory", { count: history.length })}</summary><ol>{history.map((version) => <li key={version.version}><span>v{version.version}</span><span>{version.success_values.join(", ")}</span><GovernanceBadge tone={version.enabled ? "good" : "neutral"}>{version.enabled ? t("common.enabled") : t("common.disabled")}</GovernanceBadge><time dateTime={version.created_at}>{dateTime(version.created_at, "full")}</time></li>)}</ol></details>}</article>;
    })}</div>}{Boolean(mutationError) && <ErrorState error={mutationError} />}</section></div>}
    {form && <Modal title={form.source ? t("runGovernance.newVersionTitle") : t("runGovernance.createDefinition")} onClose={() => setForm(null)} wide>
      <form className="governance-definition-form" onSubmit={submit}><p className="notice warning">{t("runGovernance.versionEffectHint")}</p><label><span>{t("runGovernance.definitionName")}</span><input {...(!form.source ? { "data-modal-initial": true } : {})} value={form.name} pattern="[a-z][a-z0-9_]{0,63}" disabled={Boolean(form.source)} onChange={(event) => setForm({ ...form, name: event.target.value })} required /></label><label><span>{t("runGovernance.type")}</span><select {...(form.source ? { "data-modal-initial": true } : {})} value={form.type} onChange={(event) => { const type = event.target.value as DefinitionForm["type"]; setForm({ ...form, type, allowed: type === "BOOLEAN" ? ["false", "true"] : ["accepted", "rejected"], success: type === "BOOLEAN" ? ["true"] : ["accepted"] }); }}><option value="CATEGORICAL">{t("runGovernance.categorical")}</option><option value="BOOLEAN">{t("runGovernance.boolean")}</option></select></label><ValueTokens label={t("runGovernance.values")} values={form.allowed} disabled={form.type === "BOOLEAN"} onChange={(allowed) => setForm({ ...form, allowed })} /><ValueTokens label={t("runGovernance.successValues")} values={form.success} onChange={(success) => setForm({ ...form, success })} /><label className="check-row"><input type="checkbox" checked={form.enabled} onChange={(event) => setForm({ ...form, enabled: event.target.checked })} /><span>{t("runGovernance.definitionEnabled")}</span></label>{Boolean(mutationError) && <ErrorState error={mutationError} />}<div className="form-actions"><button type="button" className="button ghost" data-modal-close>{t("common.cancel")}</button><button className="button primary" disabled={mutationPending || !form.success.length || (form.type === "CATEGORICAL" && !form.allowed.length)}>{t(form.source ? "runGovernance.saveNewVersion" : "runGovernance.createDefinition")}</button></div></form>
    </Modal>}
    {pendingToggle && <Modal dangerous title={pendingToggle.enabled ? t("runGovernance.disableDefinitionTitle") : t("runGovernance.enableDefinitionTitle")} onClose={() => setPendingToggle(null)} closeDisabled={mutationPending}><div className="confirmation-dialog"><p>{t("runGovernance.toggleDefinitionDescription", { name: pendingToggle.name, action: pendingToggle.enabled ? t("common.disable") : t("common.enable") })}</p>{Boolean(mutationError) && <ErrorState error={mutationError} />}<div className="form-actions"><button type="button" className="button ghost" onClick={() => setPendingToggle(null)}>{t("common.cancel")}</button><button type="button" className={pendingToggle.enabled ? "button danger" : "button primary"} disabled={mutationPending} onClick={async () => { try { await onToggleDefinition(pendingToggle); setPendingToggle(null); } catch { /* Keep the confirmation open and render the mutation error. */ } }}>{pendingToggle.enabled ? t("common.disable") : t("common.enable")}</button></div></div></Modal>}
  </section>;
}
