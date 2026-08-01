import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
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

export function RedactionPoliciesSection() {
  const { t } = useTranslation();
  const [editing, setEditing] = useState<RedactionPolicy | "new" | null>(null);
  const [testing, setTesting] = useState<RedactionPolicy | null>(null);
  const policies = useQuery({
    queryKey: ["redaction-policies"],
    queryFn: api.redactionPolicies,
  });
  const queryClient = useQueryClient();
  const remove = useMutation({
    mutationFn: (policy: RedactionPolicy) =>
      api.deleteRedactionPolicy(policy.id, policy.revision),
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: ["redaction-policies"] }),
  });
  return (
    <section className="policy-section">
      <div className="section-heading">
        <div>
          <p className="eyebrow">{t("redaction.eyebrow")}</p>
          <h2>{t("redaction.title")}</h2>
          <p>{t("redaction.description")}</p>
        </div>
        <button className="button primary" onClick={() => setEditing("new")}>{t("redaction.create")}</button>
      </div>
      {policies.isPending && <Loading />}
      {policies.isError && <ErrorState error={policies.error} />}
      {policies.data?.items.length === 0 && (
        <EmptyState
          title={t("redaction.emptyTitle")}
          action={<button className="button primary" onClick={() => setEditing("new")}>{t("redaction.baseline")}</button>}
        >
          {t("redaction.emptyDescription")}
        </EmptyState>
      )}
      {!!policies.data?.items.length && (
        <div className="policy-card-grid">
          {policies.data.items.map((policy) => (
            <article className="policy-card redaction-card" key={policy.id}>
              <header>
                <span><StatusDot ok={policy.enabled} /><strong>{policy.name}</strong></span>
                <span className="badge">{policy.mode === "strict" ? t("redaction.strictBadge") : policy.mode === "bounded_stream" ? t("redaction.boundedBadge") : t("redaction.detectStreamBadge")}</span>
              </header>
              <div className="redaction-rule-list">
                {policy.rules.map((rule) => (
                  <div key={rule.id}>
                    <span><strong>{rule.name}</strong><small>{rule.kind === "builtin" ? rule.builtin : t(`redaction.${rule.kind}Badge`)}</small></span>
                    <code>{rule.scopes.map((scope) => t(`redaction.${scope}`)).join(" + ")}</code>
                    <span className={`badge ${rule.action === "reject" ? "warning" : ""}`}>{t(`redaction.${rule.action === "detect_only" ? "detect" : rule.action}`)}</span>
                    <small>{rule.computed_max_match_bytes > 0 ? t("redaction.byteLimit", { count: rule.computed_max_match_bytes }) : t("redaction.unbounded")}</small>
                  </div>
                ))}
              </div>
              <footer>
                <code>{policy.id}</code>
                <div className="row-actions">
                  <button className="button ghost" onClick={() => setTesting(policy)}>{t("common.test")}</button>
                  <button className="button ghost" onClick={() => setEditing(policy)}>{t("common.edit")}</button>
                  <ConfirmButton
                    label={t("common.delete")}
                    confirmLabel={t("redaction.deleteConfirm", { name: policy.name })}
                    disabled={remove.isPending}
                    onConfirm={() => remove.mutate(policy)}
                  />
                </div>
              </footer>
              {remove.isError && <ErrorState error={remove.error} />}
            </article>
          ))}
        </div>
      )}
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
  const [enabled, setEnabled] = useState(current?.enabled ?? true);
  const [mode, setMode] = useState<RedactionPolicy["mode"]>(current?.mode ?? "strict");
  const [rules, setRules] = useState<EditableRule[]>(
    current?.rules.map(({ computed_max_match_bytes: _width, ...rule }) => rule) ?? [blankRule()],
  );
  const queryClient = useQueryClient();
  const updateRule = (index: number, value: Partial<EditableRule>) =>
    setRules((currentRules) =>
      currentRules.map((rule, currentIndex) => currentIndex === index ? { ...rule, ...value } : rule),
    );
  const body = { name, enabled, mode, rules };
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
    <Modal title={current ? t("redaction.edit") : t("redaction.createTitle")} onClose={onClose}>
      <form className="redaction-form" onSubmit={(event: FormEvent) => {
        event.preventDefault();
        if (name.trim() && rules.length) mutation.mutate();
      }}>
        <div className="form-grid">
          <Field label={t("redaction.name")}><input autoFocus value={name} onChange={(event) => setName(event.target.value)} /></Field>
          <Field label={t("redaction.streaming")}>
            <select value={mode} onChange={(event) => setMode(event.target.value as typeof mode)}>
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
        <div className="rule-editor-list">
          {rules.map((rule, index) => (
            <section className="rule-editor" key={rule.id || `new-${index}`}>
              <header>
                <strong>{t("redaction.rule", { count: index + 1 })}</strong>
                {rules.length > 1 && <button type="button" className="button ghost" onClick={() => setRules(rules.filter((_, item) => item !== index))}>{t("redaction.remove")}</button>}
              </header>
              <div className="form-grid">
                <Field label={t("redaction.ruleName")}><input value={rule.name} onChange={(event) => updateRule(index, { name: event.target.value })} /></Field>
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
                    <option value="mask">{t("redaction.mask")}</option>
                    <option value="replace">{t("redaction.replace")}</option>
                    <option value="reject">{t("redaction.reject")}</option>
                  </select>
                </Field>
                {rule.action === "replace" && <Field label={t("redaction.replacement")}><input value={rule.replacement ?? ""} onChange={(event) => updateRule(index, { replacement: event.target.value })} /></Field>}
                <Field label={t("redaction.priority")}><input type="number" value={rule.priority} onChange={(event) => updateRule(index, { priority: Number(event.target.value) })} /></Field>
                <div className="scope-checks">
                  {(["inbound", "outbound"] as const).map((scope) => (
                    <label className="check-row" key={scope}>
                      <input type="checkbox" checked={rule.scopes.includes(scope)} onChange={(event) => updateRule(index, {
                        scopes: event.target.checked
                          ? [...rule.scopes, scope]
                          : rule.scopes.filter((item) => item !== scope),
                      })} />
                      <span>{t(`redaction.${scope}`)}</span>
                    </label>
                  ))}
                  <label className="check-row">
                    <input type="checkbox" checked={rule.enabled} onChange={(event) => updateRule(index, { enabled: event.target.checked })} />
                    <span>{t("redaction.enableRule")}</span>
                  </label>
                </div>
              </div>
            </section>
          ))}
        </div>
        <button type="button" className="button ghost" onClick={() => setRules([...rules, blankRule()])}>{t("redaction.addRule")}</button>
        <label className="check-row policy-enabled">
          <input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />
          <span>{t("redaction.enable")}</span>
        </label>
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions">
          <button type="button" className="button ghost" onClick={onClose}>{t("common.cancel")}</button>
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
