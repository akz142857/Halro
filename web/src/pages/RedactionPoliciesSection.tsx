import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useRef, useState, type FormEvent } from "react";
import { api } from "../api";
import {
  ConfirmButton,
  EmptyState,
  ErrorState,
  Field,
  Loading,
  Modal,
  ReauthFields,
  StatusDot,
  type ReauthValues,
} from "../components";
import type {
  RedactionPolicy,
  RedactionRule,
  RedactionTestResult,
} from "../types";
import { useTranslation } from "react-i18next";
import { useNotify } from "../notifications";
import { useIsReadOnly } from "../session";

const builtins = [
  "china_phone", "email", "china_id", "bank_card_candidate",
  "gateway_key", "openai_key", "anthropic_key", "google_key",
  "aws_access_key", "bearer_token", "private_key",
];

type EditableRule = Omit<RedactionRule, "computed_max_match_bytes"> & {
  computed_max_match_bytes?: number;
  draftKey: string;
};

let draftRuleSequence = 0;

function nextDraftRuleKey() {
  draftRuleSequence += 1;
  return `draft-rule-${draftRuleSequence}`;
}

function blankRule(kind: EditableRule["kind"] = "builtin", name = "Phone"): EditableRule {
  return {
    id: "", name, kind, builtin: kind === "builtin" ? "china_phone" : undefined,
    pattern: kind === "regex" ? "" : undefined,
    dictionary: kind === "dictionary" ? [] : undefined,
    scopes: ["inbound", "outbound"], action: "mask", enabled: true, priority: 10,
    draftKey: nextDraftRuleKey(),
  };
}

type RuleProblem = "name" | "scope" | "priority" | "expression" | "dictionary" | "replacement";

function ruleProblems(rule: EditableRule): RuleProblem[] {
  const problems: RuleProblem[] = [];
  if (!rule.name.trim()) problems.push("name");
  if (rule.scopes.length === 0) problems.push("scope");
  if (!Number.isInteger(rule.priority) || rule.priority < 0 || rule.priority > 10_000) problems.push("priority");
  if (rule.kind === "regex" && !rule.pattern?.trim()) problems.push("expression");
  if (rule.kind === "dictionary" && !rule.dictionary?.some((item) => item.trim())) problems.push("dictionary");
  if (rule.action === "replace" && !rule.replacement?.trim()) problems.push("replacement");
  return problems;
}

// Dictionary entries are normalised here, at the boundary, and nowhere else. Doing
// it on every keystroke made the field unusable: Enter produced an empty trailing
// entry that was filtered away, so a second term could never be typed, and a space
// inside a term was trimmed off before it could be followed by another character.
function serializeRules(rules: EditableRule[]): Array<Omit<EditableRule, "draftKey">> {
  return rules.map(({ draftKey: _draftKey, computed_max_match_bytes: _width, ...rule }) => ({
    ...rule,
    dictionary: rule.dictionary?.map((item) => item.trim()).filter(Boolean),
  }));
}

function compareRuleExecution(left: EditableRule, right: EditableRule): number {
  if (left.enabled !== right.enabled) return left.enabled ? -1 : 1;
  if (left.action === "reject" && right.action !== "reject") return -1;
  if (right.action === "reject" && left.action !== "reject") return 1;
  return right.priority - left.priority;
}

function policyUpdateBody(policy: RedactionPolicy, enabled: boolean) {
  return {
    name: policy.name,
    enabled,
    mode: policy.mode,
    rules: policy.rules.map(({ computed_max_match_bytes: _width, ...rule }) => rule),
  };
}

export function RedactionPoliciesSection({
  policies,
  isPending = false,
  error,
  hasNextPage = false,
  isFetchingNextPage = false,
  onLoadMore,
  panelID,
  labelledBy,
}: {
  policies: RedactionPolicy[];
  isPending?: boolean;
  error?: unknown;
  hasNextPage?: boolean;
  isFetchingNextPage?: boolean;
  onLoadMore?: () => void;
  // Supplied when the section is one panel of the policies tab strip, so the
  // panel and its tab point at each other.
  panelID?: string;
  labelledBy?: string;
}) {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const [editing, setEditing] = useState<RedactionPolicy | "new" | null>(null);
  const [testing, setTesting] = useState<RedactionPolicy | null>(null);
  const [search, setSearch] = useState("");
  const [status, setStatus] = useState("");
  const [mode, setMode] = useState("");
  const normalizedSearch = search.trim().toLowerCase();
  const visible = policies.filter((policy) =>
    (!normalizedSearch || policy.name.toLowerCase().includes(normalizedSearch) || policy.id.toLowerCase().includes(normalizedSearch)) &&
    (!status || (status === "enabled") === policy.enabled) &&
    (!mode || policy.mode === mode),
  );
  const queryClient = useQueryClient();
  const { notify } = useNotify();
  const remove = useMutation({
    mutationFn: ({ policy, reauth }: { policy: RedactionPolicy; reauth: ReauthValues }) =>
      api.deleteRedactionPolicy(policy.id, policy.revision, reauth),
    onSuccess: (_result, variables) => {
      queryClient.invalidateQueries({ queryKey: ["redaction-policies"] });
      notify({ tone: "success", title: t("redaction.notifyDeleted"), description: variables.policy.name });
    },
  });
  const toggle = useMutation({
    mutationFn: ({ policy, reauth }: { policy: RedactionPolicy; reauth: ReauthValues }) =>
      api.updateRedactionPolicy(policy.id, policyUpdateBody(policy, !policy.enabled), policy.revision, reauth),
    onSuccess: (_result, variables) => {
      queryClient.invalidateQueries({ queryKey: ["redaction-policies"] });
      notify({ tone: "success", title: t(variables.policy.enabled ? "redaction.notifyDisabled" : "redaction.notifyEnabled"), description: variables.policy.name });
    },
  });
  return (
    <section className="policy-management-panel" role="tabpanel" id={panelID} aria-labelledby={labelledBy} aria-label={labelledBy ? undefined : t("redaction.title")}>
      <div className="filter-bar policy-filter-bar">
        <label><span>{t("policyManagement.search")}</span><input autoComplete="off" value={search} onChange={(event) => setSearch(event.target.value)} placeholder={t("policyManagement.searchPlaceholder")} /></label>
        <label><span>{t("policyManagement.status")}</span><select value={status} onChange={(event) => setStatus(event.target.value)}><option value="">{t("policyManagement.all")}</option><option value="enabled">{t("common.enabled")}</option><option value="disabled">{t("common.disabled")}</option></select></label>
        <label><span>{t("policyManagement.mode")}</span><select value={mode} onChange={(event) => setMode(event.target.value)}><option value="">{t("policyManagement.all")}</option><option value="strict">{t("redaction.strictBadge")}</option><option value="bounded_stream">{t("redaction.boundedBadge")}</option><option value="detect_only_stream">{t("redaction.detectStreamBadge")}</option></select></label>
        <button className="button primary" disabled={readOnly} onClick={() => setEditing("new")}>{t("redaction.create")}</button>
      </div>
      {isPending && <Loading />}
      {error !== undefined && <ErrorState error={error} />}
      {!isPending && policies.length === 0 && (
        <EmptyState
          title={t("redaction.emptyTitle")}
          action={<button className="button primary" disabled={readOnly} onClick={() => setEditing("new")}>{t("redaction.baseline")}</button>}
        >
          {t("redaction.emptyDescription")}
        </EmptyState>
      )}
      {!!policies.length && (
        <div className="table-shell policy-table-shell">
          <table className="policy-table"><thead><tr><th>{t("policyManagement.policy")}</th><th>{t("policyManagement.status")}</th><th>{t("policyManagement.mode")}</th><th>{t("policyManagement.summary")}</th><th>{t("policyManagement.bindings")}</th><th>{t("policyManagement.actions")}</th></tr></thead>
          <tbody>{visible.map((policy) => {
            const enabledRules = policy.rules.filter((rule) => rule.enabled);
            const strongest = enabledRules.some((rule) => rule.action === "reject") ? "reject" : enabledRules.some((rule) => rule.action === "replace") ? "replace" : enabledRules.some((rule) => rule.action === "mask") ? "mask" : "detect_only";
            const rowBusy = toggle.isPending && toggle.variables?.policy.id === policy.id;
            return <tr key={policy.id}>
              <td><strong>{policy.name}</strong><code>{policy.id}</code></td>
              <td><span className="inline-status"><StatusDot ok={policy.enabled} />{policy.enabled ? t("common.enabled") : t("common.disabled")}</span></td>
              <td><span className="badge">{policy.mode === "strict" ? t("redaction.strictBadge") : policy.mode === "bounded_stream" ? t("redaction.boundedBadge") : t("redaction.detectStreamBadge")}</span></td>
              <td><strong>{t("redaction.rules", { count: policy.rules.length })}</strong><small>{t("policyManagement.enabledRules", { count: enabledRules.length })} · {t(`redaction.${strongest === "detect_only" ? "detect" : strongest}`)}</small></td>
              <td>{t("policyManagement.projectCount", { count: policy.bound_projects ?? 0 })}</td>
              {/* Switching a redaction policy off takes DLP away from every Project
                  bound to it, so it asks first and names the blast radius. */}
              <td><div className="row-actions policy-row-actions"><button className="button ghost" onClick={() => setTesting(policy)}>{t("common.test")}</button>{policy.enabled
                ? <ConfirmButton className="button ghost" label={t("common.disable")} title={t("redaction.disableTitle")} confirmLabel={t("redaction.disableConfirm", { name: policy.name, count: policy.bound_projects ?? 0 })} disabled={rowBusy} requireStepUp onConfirm={(reauth) => toggle.mutateAsync({ policy, reauth })} />
                : <ConfirmButton className="button secondary" label={t("common.enable")} confirmLabel={t("redaction.enableConfirm", { name: policy.name })} disabled={rowBusy} requireStepUp onConfirm={(reauth) => toggle.mutateAsync({ policy, reauth })} />}<button className="button ghost" disabled={readOnly || rowBusy} title={readOnly ? t("navigation.readOnlyAction") : undefined} onClick={() => setEditing(policy)}>{t("common.edit")}</button><ConfirmButton label={t("common.delete")} confirmLabel={t("redaction.deleteConfirm", { name: policy.name })} disabled={remove.isPending || rowBusy} requireStepUp onConfirm={(reauth) => remove.mutateAsync({ policy, reauth })} /></div>{toggle.isError && toggle.variables?.policy.id === policy.id && <ErrorState error={toggle.error} />}</td>
            </tr>;
          })}</tbody></table>
          {remove.isError && <ErrorState error={remove.error} />}
        </div>
      )}
      {hasNextPage && <button className="button ghost policy-load-more" disabled={isFetchingNextPage} onClick={onLoadMore}>{isFetchingNextPage ? t("common.loading") : t("common.loadMore")}</button>}
      {editing && (
        <RedactionPolicyForm
          current={editing === "new" ? undefined : editing}
          onClose={() => setEditing(null)}
        />
      )}
      {testing && <RedactionPolicyTest policy={testing} onClose={() => setTesting(null)} />}
    </section>
  );
}

function RedactionPolicyForm({
  current,
  onClose,
}: {
  current?: RedactionPolicy;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const [name, setName] = useState(current?.name ?? "");
  const [enabled, setEnabled] = useState(current?.enabled ?? false);
  const [mode, setMode] = useState<RedactionPolicy["mode"]>(current?.mode ?? "strict");
  const [initialDraft] = useState(() => {
    const initialRules: EditableRule[] = current?.rules.map(({ computed_max_match_bytes: _width, ...rule }) => ({
      ...rule,
      draftKey: rule.id || nextDraftRuleKey(),
    })) ?? [blankRule("builtin", t("redaction.defaultBuiltinName"))];
    return { rules: initialRules, selectedRuleKey: [...initialRules].sort(compareRuleExecution)[0]?.draftKey ?? "" };
  });
  const [rules, setRules] = useState<EditableRule[]>(initialDraft.rules);
  const [selectedRuleKey, setSelectedRuleKey] = useState(initialDraft.selectedRuleKey);
  const [ruleSearch, setRuleSearch] = useState("");
  const [ruleStatus, setRuleStatus] = useState<"all" | "enabled" | "disabled" | "errors">("all");
  const [addKind, setAddKind] = useState<EditableRule["kind"]>("builtin");
  const [removedRule, setRemovedRule] = useState<{ rule: EditableRule; index: number } | null>(null);
  const [submitted, setSubmitted] = useState(false);
  // A rule the operator just created has not been filled in yet. Reporting "1
  // problem" and colouring its fields red before a single keystroke reads as a
  // failure the operator caused; the errors are held back until they leave the
  // rule or submit the form.
  const [untouchedRules, setUntouchedRules] = useState<string[]>([]);
  // The browser list is ordered by execution, but resolving that order on every
  // keystroke moved the row out from under the pointer while its priority or its
  // enabled box was being changed. The order is settled on mount and whenever the
  // operator asks for it.
  const [executionOrder, setExecutionOrder] = useState<string[]>(
    () => [...initialDraft.rules].sort(compareRuleExecution).map((rule) => rule.draftKey),
  );
  const nameField = useRef<HTMLInputElement>(null);
  const addRuleButton = useRef<HTMLButtonElement>(null);
  const initialSnapshot = useRef(JSON.stringify({
    name: current?.name ?? "",
    enabled: current?.enabled ?? false,
    mode: current?.mode ?? "strict",
    rules: serializeRules(initialDraft.rules),
  }));
  const queryClient = useQueryClient();
  const updateRule = (index: number, value: Partial<EditableRule>) =>
    setRules((currentRules) =>
      currentRules.map((rule, currentIndex) => currentIndex === index ? { ...rule, ...value } : rule),
    );
  const toggleScope = (index: number, scope: "inbound" | "outbound", checked: boolean) =>
    setRules((currentRules) => currentRules.map((rule, currentIndex) => {
      if (currentIndex !== index) return rule;
      return {
        ...rule,
        scopes: checked
          ? Array.from(new Set([...rule.scopes, scope]))
          : rule.scopes.filter((item) => item !== scope),
      };
    }));
  const serializedRules = serializeRules(rules);
  const body = { name, enabled, mode, rules: serializedRules };
  const actualProblems = rules.map(ruleProblems);
  // What the form enforces stays the full set; what it shows is the reported set.
  const problems = rules.map((rule, index) => untouchedRules.includes(rule.draftKey) ? [] : actualProblems[index]);
  const problemCount = problems.reduce((count, currentProblems) => count + currentProblems.length, 0);
  const actualProblemCount = actualProblems.reduce((count, currentProblems) => count + currentProblems.length, 0);
  const formValid = !!name.trim() && rules.length > 0 && actualProblemCount === 0;
  const dirty = JSON.stringify(body) !== initialSnapshot.current;
  const selectedIndex = rules.findIndex((rule) => rule.draftKey === selectedRuleKey);
  const selectedRule = selectedIndex >= 0 ? rules[selectedIndex] : undefined;
  const selectedProblems = selectedIndex >= 0 ? problems[selectedIndex] : [];
  const normalizedSearch = ruleSearch.trim().toLowerCase();
  const executionRank = (draftKey: string) => {
    const rank = executionOrder.indexOf(draftKey);
    return rank < 0 ? executionOrder.length : rank;
  };
  const visibleRules = rules
    .map((rule, index) => ({ rule, index, problems: problems[index] }))
    .filter(({ rule, problems: currentProblems }) =>
      (!normalizedSearch || rule.name.toLowerCase().includes(normalizedSearch) || rule.builtin?.toLowerCase().includes(normalizedSearch)) &&
      (ruleStatus === "all" ||
        (ruleStatus === "enabled" && rule.enabled) ||
        (ruleStatus === "disabled" && !rule.enabled) ||
        (ruleStatus === "errors" && currentProblems.length > 0)),
    )
    .sort((left, right) => executionRank(left.rule.draftKey) - executionRank(right.rule.draftKey) || left.index - right.index);
  const resortRules = () =>
    setExecutionOrder([...rules].sort(compareRuleExecution).map((rule) => rule.draftKey));
  const revealRule = (draftKey: string) =>
    setUntouchedRules((keys) => keys.filter((key) => key !== draftKey));
  const changeMode = (nextMode: RedactionPolicy["mode"]) => {
    setMode(nextMode);
    if (nextMode === "detect_only_stream") {
      setRules((currentRules) => currentRules.map((rule) =>
        rule.enabled && rule.action !== "detect_only" ? { ...rule, action: "detect_only", replacement: undefined } : rule,
      ));
    }
  };
  const addRule = () => {
    if (rules.length >= 128) return;
    const defaultName = addKind === "builtin"
      ? t("redaction.defaultBuiltinName")
      : addKind === "regex"
        ? t("redaction.defaultRegexName")
        : t("redaction.defaultDictionaryName");
    const nextRule = blankRule(addKind, defaultName);
    setRules((currentRules) => [...currentRules, nextRule]);
    setSelectedRuleKey(nextRule.draftKey);
    setUntouchedRules((keys) => [...keys, nextRule.draftKey]);
    setRuleSearch("");
    setRuleStatus("all");
    setRemovedRule(null);
  };
  const removeRule = (index: number) => {
    const rule = rules[index];
    if (!rule) return;
    const remaining = rules.filter((_, currentIndex) => currentIndex !== index);
    setRules(remaining);
    setRemovedRule({ rule, index });
    if (rule.draftKey === selectedRuleKey) {
      setSelectedRuleKey(remaining[Math.min(index, remaining.length - 1)]?.draftKey ?? "");
    }
  };
  const undoRemove = () => {
    if (!removedRule) return;
    setRules((currentRules) => {
      const nextRules = [...currentRules];
      nextRules.splice(Math.min(removedRule.index, nextRules.length), 0, removedRule.rule);
      return nextRules;
    });
    setSelectedRuleKey(removedRule.rule.draftKey);
    setRemovedRule(null);
  };
  const duplicateRule = (index: number) => {
    if (rules.length >= 128) return;
    const source = rules[index];
    if (!source) return;
    const copy = { ...source, id: "", draftKey: nextDraftRuleKey(), name: t("redaction.copyName", { name: source.name }) };
    setRules((currentRules) => [...currentRules.slice(0, index + 1), copy, ...currentRules.slice(index + 1)]);
    setSelectedRuleKey(copy.draftKey);
    setRemovedRule(null);
  };
  // Every reason a submit can be refused has to put the operator in front of the
  // control that refuses it. A missing policy name and an empty rule list used to
  // produce no visible reaction at all.
  const selectFirstProblem = () => {
    if (!name.trim()) return nameField.current?.focus();
    if (!rules.length) return addRuleButton.current?.focus();
    const problemIndex = actualProblems.findIndex((currentProblems) => currentProblems.length > 0);
    if (problemIndex >= 0) {
      setRuleSearch("");
      setRuleStatus("all");
      setSelectedRuleKey(rules[problemIndex].draftKey);
      revealRule(rules[problemIndex].draftKey);
    }
  };
  // Editing an existing policy can take enforcement away, so the server asks
  // who is doing it; creating one cannot, and does not ask.
  const [reauth, setReauth] = useState<ReauthValues>({ currentPassword: "", totpCode: "" });
  const { notify } = useNotify();
  const mutation = useMutation({
    mutationFn: () => current
      ? api.updateRedactionPolicy(current.id, body, current.revision, reauth)
      : api.createRedactionPolicy(body),
    onSettled: () => setReauth((values) => ({ ...values, totpCode: "" })),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["redaction-policies"] });
      notify({ tone: "success", title: t(current ? "redaction.notifyUpdated" : "redaction.notifyCreated"), description: name });
      onClose();
    },
  });
  return (
    <Modal title={current ? t("redaction.edit") : t("redaction.createTitle")} onClose={onClose} closeDisabled={mutation.isPending} dirty={dirty}>
      <form className="redaction-form" noValidate onSubmit={(event: FormEvent) => {
        event.preventDefault();
        setSubmitted(true);
        setUntouchedRules([]);
        if (formValid) mutation.mutate();
        else selectFirstProblem();
      }}>
        <div className="redaction-form-scroll">
        <section className="redaction-policy-basics">
          <Field label={t("redaction.name")} error={submitted && !name.trim() ? t("redaction.policyNameRequired") : undefined}><input autoComplete="off" ref={nameField} data-modal-initial required value={name} onChange={(event) => setName(event.target.value)} /></Field>
          <fieldset className="redaction-streaming-options">
            <legend>{t("redaction.streaming")}</legend>
            <div className="policy-action-cards redaction-streaming-cards">
              {(["strict", "bounded_stream", "detect_only_stream"] as const).map((value) => (
                <label className={`policy-action-card ${mode === value ? "selected" : ""}`} key={value}>
                  <input type="radio" name="redaction-streaming" value={value} checked={mode === value} onChange={() => changeMode(value)} />
                  <span>
                    <strong>{value === "strict" ? t("redaction.strictBadge") : value === "bounded_stream" ? t("redaction.boundedBadge") : t("redaction.detectStreamBadge")}</strong>
                    <small>{value === "strict" ? t("redaction.strict") : value === "bounded_stream" ? t("redaction.bounded") : t("redaction.detectStream")}</small>
                  </span>
                </label>
              ))}
            </div>
          </fieldset>
        </section>
        <div className="redaction-boundary notice warning">
          <strong>{t("redaction.boundary")}</strong>
          <span>{t("redaction.boundaryDescription")}</span>
        </div>
        <section className="redaction-rules-workspace">
          <header className="redaction-rules-toolbar">
            <div>
              <strong>{t("redaction.manageRules")}</strong>
              <span>{t("redaction.ruleCounts", { total: rules.length, enabled: rules.filter((rule) => rule.enabled).length })}</span>
            </div>
            <div className="redaction-add-rule">
              <select aria-label={t("redaction.newRuleType")} value={addKind} onChange={(event) => setAddKind(event.target.value as EditableRule["kind"])}>
                <option value="builtin">{t("redaction.builtin")}</option>
                <option value="regex">{t("redaction.regex")}</option>
                <option value="dictionary">{t("redaction.dictionary")}</option>
              </select>
              <button ref={addRuleButton} type="button" className="button secondary" disabled={rules.length >= 128} onClick={addRule}>{t("redaction.addRule")}</button>
            </div>
          </header>
          <div className="redaction-rules-layout">
            <aside className="redaction-rule-browser">
              <div className="redaction-rule-filters">
                <input autoComplete="off" aria-label={t("redaction.searchRules")} placeholder={t("redaction.searchRulesPlaceholder")} value={ruleSearch} onChange={(event) => setRuleSearch(event.target.value)} />
                <select aria-label={t("redaction.ruleStatus")} value={ruleStatus} onChange={(event) => setRuleStatus(event.target.value as typeof ruleStatus)}>
                  <option value="all">{t("redaction.allRules")}</option>
                  <option value="enabled">{t("common.enabled")}</option>
                  <option value="disabled">{t("common.disabled")}</option>
                  <option value="errors">{t("redaction.onlyErrors")}</option>
                </select>
              </div>
              <div className="redaction-rule-order-note">
                <span>{t("redaction.executionOrder")}</span>
                <button type="button" className="button ghost small" onClick={resortRules}>{t("redaction.resortRules")}</button>
              </div>
              <div className="redaction-rule-summaries">
                {visibleRules.map(({ rule, index, problems: currentProblems }) => (
                  <article className={`redaction-rule-summary ${selectedRuleKey === rule.draftKey ? "selected" : ""} ${!rule.enabled ? "disabled" : ""}`} key={rule.draftKey}>
                    <button type="button" className="redaction-rule-select" aria-pressed={selectedRuleKey === rule.draftKey} onClick={() => setSelectedRuleKey(rule.draftKey)}>
                      <span className="redaction-rule-summary-title">
                        <strong>{rule.name || t("redaction.unnamedRule")}</strong>
                        {currentProblems.length > 0 && <span className="redaction-rule-error-count">{t("redaction.problemCount", { count: currentProblems.length })}</span>}
                      </span>
                      <span className="redaction-rule-summary-info">
                        <span className="redaction-rule-summary-meta">
                          {t(`redaction.${rule.kind}`)} · {rule.scopes.map((scope) => t(`redaction.${scope}`)).join("/") || t("redaction.noScope")}
                        </span>
                        <span className="redaction-rule-summary-tags">
                          <span className="badge">{t(`redaction.${rule.action === "detect_only" ? "detect" : rule.action}`)}</span>
                          <span>{t("redaction.priorityValue", { value: rule.priority })}</span>
                        </span>
                      </span>
                    </button>
                    <label className="redaction-rule-toggle" title={rule.enabled ? t("common.enabled") : t("common.disabled")}>
                      <input aria-label={t("redaction.toggleRule", { name: rule.name })} type="checkbox" checked={rule.enabled} onChange={(event) => updateRule(index, { enabled: event.target.checked })} />
                    </label>
                  </article>
                ))}
                {visibleRules.length === 0 && <div className="redaction-rule-empty">{t("redaction.noRuleMatches")}</div>}
              </div>
            </aside>
            <div
              className="redaction-rule-detail"
              // Leaving the rule editor is the point at which "not filled in yet"
              // becomes "left unfilled", so that is when its problems are reported.
              onBlur={(event) => {
                if (!selectedRule) return;
                const next = event.relatedTarget as Node | null;
                if (next && event.currentTarget.contains(next)) return;
                revealRule(selectedRule.draftKey);
              }}
            >
              {selectedRule ? <>
                <header>
                  <div>
                    <span>{t("redaction.editingRule")}</span>
                    <strong>{selectedRule.name || t("redaction.unnamedRule")}</strong>
                  </div>
                  <div className="row-actions">
                    <button type="button" className="button ghost" disabled={rules.length >= 128} onClick={() => duplicateRule(selectedIndex)}>{t("redaction.duplicate")}</button>
                    <button type="button" className="button danger-text" onClick={() => removeRule(selectedIndex)}>{t("redaction.remove")}</button>
                  </div>
                </header>
                <div className="redaction-rule-fields form-grid">
                <Field label={t("redaction.ruleName")} error={selectedProblems.includes("name") ? t("redaction.ruleProblems.name") : undefined}><input autoComplete="off" required value={selectedRule.name} onChange={(event) => updateRule(selectedIndex, { name: event.target.value })} /></Field>
                <Field label={t("redaction.type")}>
                  <select value={selectedRule.kind} onChange={(event) => {
                    const kind = event.target.value as EditableRule["kind"];
                    updateRule(selectedIndex, {
                      kind,
                      builtin: kind === "builtin" ? "china_phone" : undefined,
                      pattern: kind === "regex" ? "" : undefined,
                      dictionary: kind === "dictionary" ? [] : undefined,
                    });
                  }}>
                    <option value="builtin">{t("redaction.builtin")}</option>
                    <option value="regex">{t("redaction.regex")}</option>
                    <option value="dictionary">{t("redaction.dictionary")}</option>
                  </select>
                </Field>
                {selectedRule.kind === "builtin" && (
                  <Field label={t("redaction.category")}>
                    <select value={selectedRule.builtin} onChange={(event) => updateRule(selectedIndex, { builtin: event.target.value })}>
                      {builtins.map((item) => <option key={item} value={item}>{item}</option>)}
                    </select>
                  </Field>
                )}
                {selectedRule.kind === "regex" && <Field label={t("redaction.expression")} error={selectedProblems.includes("expression") ? t("redaction.ruleProblems.expression") : undefined}><input autoComplete="off" value={selectedRule.pattern ?? ""} onChange={(event) => updateRule(selectedIndex, { pattern: event.target.value })} /></Field>}
                {selectedRule.kind === "dictionary" && (
                  <Field label={t("redaction.dictionaryItems")} error={selectedProblems.includes("dictionary") ? t("redaction.ruleProblems.dictionary") : undefined}>
                    <textarea autoComplete="off" rows={5} value={(selectedRule.dictionary ?? []).join("\n")} onChange={(event) => updateRule(selectedIndex, { dictionary: event.target.value.split("\n") })} />
                  </Field>
                )}
                <Field label={t("redaction.action")}>
                  <select value={selectedRule.action} onChange={(event) => updateRule(selectedIndex, { action: event.target.value as EditableRule["action"] })}>
                    <option value="detect_only">{t("redaction.detect")}</option>
                    <option value="mask" disabled={mode === "detect_only_stream"}>{t("redaction.mask")}</option>
                    <option value="replace" disabled={mode === "detect_only_stream"}>{t("redaction.replace")}</option>
                    <option value="reject" disabled={mode === "detect_only_stream"}>{t("redaction.reject")}</option>
                  </select>
                </Field>
                {selectedRule.action === "replace" && <Field label={t("redaction.replacement")} error={selectedProblems.includes("replacement") ? t("redaction.ruleProblems.replacement") : undefined}><input autoComplete="off" maxLength={256} value={selectedRule.replacement ?? ""} onChange={(event) => updateRule(selectedIndex, { replacement: event.target.value })} /></Field>}
                <Field label={t("redaction.priority")} hint={t("redaction.priorityHint")} error={selectedProblems.includes("priority") ? t("redaction.ruleProblems.priority") : undefined}><input autoComplete="off" min="0" max="10000" step="1" type="number" value={Number.isNaN(selectedRule.priority) ? "" : selectedRule.priority} onChange={(event) => updateRule(selectedIndex, { priority: event.target.value === "" ? Number.NaN : Number(event.target.value) })} /></Field>
                <fieldset className="scope-checks">
                  <legend>{t("redaction.scope")}</legend>
                  {(["inbound", "outbound"] as const).map((scope) => (
                    <label className="check-row" key={scope}>
                      <input type="checkbox" checked={selectedRule.scopes.includes(scope)} onChange={(event) => toggleScope(selectedIndex, scope, event.target.checked)} />
                      <span>{t(`redaction.${scope}`)}</span>
                    </label>
                  ))}
                  {selectedProblems.includes("scope") && <small className="field-error">{t("redaction.ruleProblems.scope")}</small>}
                </fieldset>
                {/* Outside the direction fieldset: inside it, a screen reader
                    announced "Enable rule, Direction group", which states the
                    opposite of what the box does. */}
                <label className="check-row">
                  <input type="checkbox" checked={selectedRule.enabled} onChange={(event) => updateRule(selectedIndex, { enabled: event.target.checked })} />
                  <span>{t("redaction.enableRule")}</span>
                </label>
                </div>
              </> : <div className="redaction-no-selection">{rules.length ? t("redaction.selectRule") : t("redaction.addFirstRule")}</div>}
            </div>
          </div>
        </section>
        {removedRule && <div className="redaction-undo" role="status"><span>{t("redaction.ruleRemoved", { name: removedRule.rule.name })}</span><button type="button" className="button ghost small" onClick={undoRemove}>{t("redaction.undo")}</button></div>}
        {mutation.isError && <ErrorState error={mutation.error} />}
        </div>
        {current && <ReauthFields values={reauth} onChange={setReauth} description={t("auth.stepUpSecurityControl")} />}
        <div className="redaction-form-actions form-actions">
          <div className="redaction-footer-state">
            <label className="redaction-footer-enable">
              <input type="checkbox" aria-label={t("redaction.enable")} checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />
              <span><strong>{t("redaction.enable")} · {enabled ? t("common.enabled") : t("common.disabled")}</strong>{current && <small>{t("redaction.boundProjects", { count: current.bound_projects ?? 0 })}</small>}</span>
            </label>
            <div className={`redaction-save-summary ${problemCount || rules.length === 0 || (submitted && !name.trim()) ? "invalid" : ""}`} aria-live="polite">
              {problemCount > 0
                ? <button type="button" onClick={selectFirstProblem}>{t("redaction.problemsSummary", { count: problemCount })}</button>
                : rules.length === 0
                  ? t("redaction.ruleRequired")
                  : dirty ? t("redaction.unsavedChanges") : t("redaction.noChanges")}
            </div>
          </div>
          <button type="button" className="button ghost" disabled={mutation.isPending} data-modal-close>{t("common.cancel")}</button>
          <button className="button primary" disabled={mutation.isPending || (Boolean(current) && !reauth.currentPassword)}>{t("redaction.compile")}</button>
        </div>
      </form>
    </Modal>
  );
}

function RedactionPolicyTest({ policy, onClose }: { policy: RedactionPolicy; onClose: () => void }) {
  const { t } = useTranslation();
  const [input, setInput] = useState("");
  const [scope, setScope] = useState<"inbound" | "outbound">("inbound");
  const [result, setResult] = useState<RedactionTestResult | null>(null);
  const mutation = useMutation({
    mutationFn: () => api.testRedactionPolicy(policy.id, { input, scope }),
    onSuccess: setResult,
  });
  return (
    <Modal title={t("redaction.testTitle", { name: policy.name })} onClose={onClose}>
      <form onSubmit={(event) => { event.preventDefault(); mutation.mutate(); }}>
        <Field label={t("redaction.direction")}>
          <select value={scope} onChange={(event) => setScope(event.target.value as typeof scope)}>
            <option value="inbound">{t("redaction.inbound")}</option>
            <option value="outbound">{t("redaction.outbound")}</option>
          </select>
        </Field>
        <Field label={t("redaction.content")} hint={t("redaction.contentHint")}>
          <textarea autoComplete="off" rows={6} value={input} onChange={(event) => setInput(event.target.value)} />
        </Field>
        {result && (
          <div className={`preview-result ${result.match_count ? "violated" : ""}`}>
            <StatusDot ok={!result.match_count} />
            <div>
              <strong>{result.match_count ? t("redaction.matches", { count: result.match_count }) : t("redaction.noMatches")}</strong>
              <small>{result.matches.map((match) => `${redactionRuleName(policy, match.rule_id, match.category)} · ${t(`redaction.${match.action === "detect_only" ? "detect" : match.action}`)}`).join(" / ") || t("redaction.noEcho")}</small>
            </div>
          </div>
        )}
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions">
          <button type="button" className="button ghost" onClick={onClose}>{t("common.close")}</button>
          <button className="button primary" disabled={mutation.isPending || !input}>{t("redaction.run")}</button>
        </div>
      </form>
    </Modal>
  );
}

function redactionRuleName(policy: RedactionPolicy, ruleID: string, fallback: string) {
  return policy.rules.find((rule) => rule.id === ruleID)?.name || fallback;
}
