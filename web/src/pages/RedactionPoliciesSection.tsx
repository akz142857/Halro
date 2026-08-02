import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { api } from "../api";
import {
  ConfirmButton,
  EmptyState,
  ErrorState,
  Field,
  Loading,
  Modal,
  StatusDot,
} from "../components";
import type {
  RedactionPolicy,
  RedactionRule,
  RedactionTestResult,
} from "../types";
import { useTranslation } from "react-i18next";

const builtins = [
  "china_phone", "email", "china_id", "bank_card_candidate",
  "gateway_key", "openai_key", "anthropic_key", "google_key",
  "aws_access_key", "bearer_token", "private_key",
];

type EditableRule = Omit<RedactionRule, "computed_max_match_bytes"> & {
  computed_max_match_bytes?: number;
};

function blankRule(): EditableRule {
  return {
    id: "", name: "Phone", kind: "builtin", builtin: "china_phone",
    scopes: ["inbound", "outbound"], action: "mask", enabled: true, priority: 10,
  };
}

function ruleProblem(rule: EditableRule): string | null {
  if (!rule.name.trim()) return "Rule name is required";
  if (rule.scopes.length === 0) return "Select at least one scope";
  if (!Number.isInteger(rule.priority) || rule.priority < 0) return "Priority must be a non-negative integer";
  if (rule.kind === "regex" && !rule.pattern?.trim()) return "Regular expression is required";
  if (rule.kind === "dictionary" && !rule.dictionary?.some((item) => item.trim())) return "At least one dictionary item is required";
  return null;
}

export function RedactionPoliciesSection({
  policies,
  isPending = false,
  error,
  hasNextPage = false,
  isFetchingNextPage = false,
  onLoadMore,
}: {
  policies: RedactionPolicy[];
  isPending?: boolean;
  error?: unknown;
  hasNextPage?: boolean;
  isFetchingNextPage?: boolean;
  onLoadMore?: () => void;
}) {
  const { t } = useTranslation();
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
  const remove = useMutation({
    mutationFn: (policy: RedactionPolicy) =>
      api.deleteRedactionPolicy(policy.id, policy.revision),
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: ["redaction-policies"] }),
  });
  return (
    <section className="policy-management-panel" role="tabpanel" aria-label={t("redaction.title")}>
      <div className="filter-bar policy-filter-bar">
        <label><span>{t("policyManagement.search")}</span><input value={search} onChange={(event) => setSearch(event.target.value)} placeholder={t("policyManagement.searchPlaceholder")} /></label>
        <label><span>{t("policyManagement.status")}</span><select value={status} onChange={(event) => setStatus(event.target.value)}><option value="">{t("policyManagement.all")}</option><option value="enabled">{t("common.enabled")}</option><option value="disabled">{t("common.disabled")}</option></select></label>
        <label><span>{t("policyManagement.mode")}</span><select value={mode} onChange={(event) => setMode(event.target.value)}><option value="">{t("policyManagement.all")}</option><option value="strict">{t("redaction.strictBadge")}</option><option value="bounded_stream">{t("redaction.boundedBadge")}</option><option value="detect_only_stream">{t("redaction.detectStreamBadge")}</option></select></label>
        <span className="filter-count">{t("policyManagement.showing", { visible: visible.length, loaded: policies.length })}</span>
        <button className="button primary" onClick={() => setEditing("new")}>{t("redaction.create")}</button>
      </div>
      {isPending && <Loading />}
      {error !== undefined && <ErrorState error={error} />}
      {!isPending && policies.length === 0 && (
        <EmptyState
          title={t("redaction.emptyTitle")}
          action={<button className="button primary" onClick={() => setEditing("new")}>{t("redaction.baseline")}</button>}
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
            return <tr key={policy.id}>
              <td><strong>{policy.name}</strong><code>{policy.id}</code></td>
              <td><span className="inline-status"><StatusDot ok={policy.enabled} />{policy.enabled ? t("common.enabled") : t("common.disabled")}</span></td>
              <td><span className="badge">{policy.mode === "strict" ? t("redaction.strictBadge") : policy.mode === "bounded_stream" ? t("redaction.boundedBadge") : t("redaction.detectStreamBadge")}</span></td>
              <td><strong>{t("redaction.rules", { count: policy.rules.length })}</strong><small>{t("policyManagement.enabledRules", { count: enabledRules.length })} · {t(`redaction.${strongest === "detect_only" ? "detect" : strongest}`)}</small></td>
              <td>{t("policyManagement.projectCount", { count: policy.bound_projects ?? 0 })}</td>
              <td><div className="row-actions policy-row-actions"><button className="button ghost" onClick={() => setTesting(policy)}>{t("common.test")}</button><button className="button ghost" onClick={() => setEditing(policy)}>{t("common.edit")}</button><ConfirmButton label={t("common.delete")} confirmLabel={t("redaction.deleteConfirm", { name: policy.name })} disabled={remove.isPending} onConfirm={() => remove.mutate(policy)} /></div></td>
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
  const [rules, setRules] = useState<EditableRule[]>(
    current?.rules.map(({ computed_max_match_bytes: _width, ...rule }) => rule) ?? [blankRule()],
  );
  const [submitted, setSubmitted] = useState(false);
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
  const body = { name, enabled, mode, rules };
  const problems = rules.map(ruleProblem);
  const formValid = !!name.trim() && rules.length > 0 && problems.every((problem) => problem === null);
  const changeMode = (nextMode: RedactionPolicy["mode"]) => {
    setMode(nextMode);
    if (nextMode === "detect_only_stream") {
      setRules((currentRules) => currentRules.map((rule) =>
        rule.enabled && rule.action !== "detect_only" ? { ...rule, action: "detect_only", replacement: undefined } : rule,
      ));
    }
  };
  const mutation = useMutation({
    mutationFn: () => current
      ? api.updateRedactionPolicy(current.id, body, current.revision)
      : api.createRedactionPolicy(body),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["redaction-policies"] });
      onClose();
    },
  });
  return (
    <Modal title={current ? t("redaction.edit") : t("redaction.createTitle")} onClose={onClose} closeDisabled={mutation.isPending}>
      <form className="redaction-form" noValidate onSubmit={(event: FormEvent) => {
        event.preventDefault();
        setSubmitted(true);
        if (formValid) mutation.mutate();
      }}>
        <div className="form-grid">
          <Field label={t("redaction.name")}><input autoFocus aria-invalid={submitted && !name.trim()} required value={name} onChange={(event) => setName(event.target.value)} /></Field>
          <Field label={t("redaction.streaming")}>
            <select value={mode} onChange={(event) => changeMode(event.target.value as typeof mode)}>
              <option value="strict">{t("redaction.strict")}</option>
              <option value="bounded_stream">{t("redaction.bounded")}</option>
              <option value="detect_only_stream">{t("redaction.detectStream")}</option>
            </select>
          </Field>
        </div>
        <div className="notice warning">
          <strong>{t("redaction.boundary")}</strong>
          <span>{t("redaction.boundaryDescription")}</span>
        </div>
        {mode === "detect_only_stream" && (
          <div className="notice warning" role="status">
            <strong>{t("redaction.detectStreamBadge")}</strong>
            <span>{t("redaction.enableRule")} → {t("redaction.detect")}</span>
          </div>
        )}
        <div className="rule-editor-list">
          {rules.map((rule, index) => (
            <section className="rule-editor" key={rule.id || `new-${index}`}>
              <header>
                <strong>{t("redaction.rule", { count: index + 1 })}</strong>
                {rules.length > 1 && <button type="button" className="button ghost" onClick={() => setRules(rules.filter((_, item) => item !== index))}>{t("redaction.remove")}</button>}
              </header>
              <div className="form-grid">
                <Field label={t("redaction.ruleName")}><input required aria-invalid={submitted && !rule.name.trim()} value={rule.name} onChange={(event) => updateRule(index, { name: event.target.value })} /></Field>
                <Field label={t("redaction.type")}>
                  <select value={rule.kind} onChange={(event) => updateRule(index, {
                    kind: event.target.value as EditableRule["kind"],
                    builtin: event.target.value === "builtin" ? "china_phone" : "",
                  })}>
                    <option value="builtin">{t("redaction.builtin")}</option>
                    <option value="regex">{t("redaction.regex")}</option>
                    <option value="dictionary">{t("redaction.dictionary")}</option>
                  </select>
                </Field>
                {rule.kind === "builtin" && (
                  <Field label={t("redaction.category")}>
                    <select value={rule.builtin} onChange={(event) => updateRule(index, { builtin: event.target.value })}>
                      {builtins.map((item) => <option key={item} value={item}>{item}</option>)}
                    </select>
                  </Field>
                )}
                {rule.kind === "regex" && <Field label={t("redaction.expression")}><input value={rule.pattern ?? ""} onChange={(event) => updateRule(index, { pattern: event.target.value })} /></Field>}
                {rule.kind === "dictionary" && (
                  <Field label={t("redaction.dictionaryItems")}>
                    <textarea rows={3} value={(rule.dictionary ?? []).join("\n")} onChange={(event) => updateRule(index, { dictionary: event.target.value.split("\n").map((value) => value.trim()).filter(Boolean) })} />
                  </Field>
                )}
                <Field label={t("redaction.action")}>
                  <select value={rule.action} onChange={(event) => updateRule(index, { action: event.target.value as EditableRule["action"] })}>
                    <option value="detect_only">{t("redaction.detect")}</option>
                    <option value="mask" disabled={mode === "detect_only_stream"}>{t("redaction.mask")}</option>
                    <option value="replace" disabled={mode === "detect_only_stream"}>{t("redaction.replace")}</option>
                    <option value="reject" disabled={mode === "detect_only_stream"}>{t("redaction.reject")}</option>
                  </select>
                </Field>
                {rule.action === "replace" && <Field label={t("redaction.replacement")}><input value={rule.replacement ?? ""} onChange={(event) => updateRule(index, { replacement: event.target.value })} /></Field>}
                <Field label={t("redaction.priority")}><input min="0" step="1" type="number" aria-invalid={submitted && (!Number.isInteger(rule.priority) || rule.priority < 0)} value={Number.isNaN(rule.priority) ? "" : rule.priority} onChange={(event) => updateRule(index, { priority: event.target.value === "" ? Number.NaN : Number(event.target.value) })} /></Field>
                <div className="scope-checks">
                  {(["inbound", "outbound"] as const).map((scope) => (
                    <label className="check-row" key={scope}>
                      <input type="checkbox" checked={rule.scopes.includes(scope)} onChange={(event) => toggleScope(index, scope, event.target.checked)} />
                      <span>{t(`redaction.${scope}`)}</span>
                    </label>
                  ))}
                  <label className="check-row">
                    <input type="checkbox" checked={rule.enabled} onChange={(event) => updateRule(index, { enabled: event.target.checked })} />
                    <span>{t("redaction.enableRule")}</span>
                  </label>
                </div>
                {submitted && problems[index] && <p role="alert">{problems[index]}</p>}
              </div>
            </section>
          ))}
        </div>
        <button type="button" className="button ghost" onClick={() => setRules([...rules, blankRule()])}>{t("redaction.addRule")}</button>
        <label className="check-row policy-enabled">
          <input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />
          <span>{t("redaction.enable")} · {enabled ? t("common.enabled") : t("common.disabled")}</span>
        </label>
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions">
          <button type="button" className="button ghost" disabled={mutation.isPending} onClick={onClose}>{t("common.cancel")}</button>
          <button className="button primary" disabled={mutation.isPending}>{t("redaction.compile")}</button>
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
          <textarea rows={6} value={input} onChange={(event) => setInput(event.target.value)} />
        </Field>
        {result && (
          <div className={`preview-result ${result.match_count ? "violated" : ""}`}>
            <StatusDot ok={!result.match_count} />
            <div>
              <strong>{result.match_count ? t("redaction.matches", { count: result.match_count }) : t("redaction.noMatches")}</strong>
              <small>{result.matches.map((match) => `${match.category} · ${t(`redaction.${match.action === "detect_only" ? "detect" : match.action}`)}`).join(" / ") || t("redaction.noEcho")}</small>
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
