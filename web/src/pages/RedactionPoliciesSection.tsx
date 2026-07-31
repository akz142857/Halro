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
          <p className="eyebrow">DATA LOSS PREVENTION</p>
          <h2>Redaction Policies</h2>
          <p>在请求进入 Provider 前和响应返回内部调用方前，对结构化内容执行检测、掩码、替换或拒绝。</p>
        </div>
        <button className="button primary" onClick={() => setEditing("new")}>＋ 新建脱敏 Policy</button>
      </div>
      {policies.isPending && <Loading />}
      {policies.isError && <ErrorState error={policies.error} />}
      {policies.data?.items.length === 0 && (
        <EmptyState
          title="还没有脱敏 Policy"
          action={<button className="button primary" onClick={() => setEditing("new")}>创建脱敏基线</button>}
        >
          strict 模式会禁用流式响应，是当前最稳妥的生产默认值。
        </EmptyState>
      )}
      {!!policies.data?.items.length && (
        <div className="policy-card-grid">
          {policies.data.items.map((policy) => (
            <article className="policy-card redaction-card" key={policy.id}>
              <header>
                <span><StatusDot ok={policy.enabled} /><strong>{policy.name}</strong></span>
                <span className="badge">{policy.mode}</span>
              </header>
              <div className="redaction-rule-list">
                {policy.rules.map((rule) => (
                  <div key={rule.id}>
                    <span><strong>{rule.name}</strong><small>{rule.kind === "builtin" ? rule.builtin : rule.kind}</small></span>
                    <code>{rule.scopes.join(" + ")}</code>
                    <span className={`badge ${rule.action === "reject" ? "warning" : ""}`}>{rule.action}</span>
                    <small>{rule.computed_max_match_bytes > 0 ? `≤ ${rule.computed_max_match_bytes} bytes` : "unbounded"}</small>
                  </div>
                ))}
              </div>
              <footer>
                <code>{policy.id}</code>
                <div className="row-actions">
                  <button className="button ghost" onClick={() => setTesting(policy)}>测试</button>
                  <button className="button ghost" onClick={() => setEditing(policy)}>编辑</button>
                  <ConfirmButton
                    label="删除"
                    confirmLabel={`删除 Policy “${policy.name}”？被 Project 引用时服务端会拒绝。`}
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
    <Modal title={current ? "编辑脱敏 Policy" : "创建脱敏 Policy"} onClose={onClose}>
      <form className="redaction-form" onSubmit={(event: FormEvent) => {
        event.preventDefault();
        if (name.trim() && rules.length) mutation.mutate();
      }}>
        <div className="form-grid">
          <Field label="Policy 名称"><input autoFocus value={name} onChange={(event) => setName(event.target.value)} /></Field>
          <Field label="流式策略">
            <select value={mode} onChange={(event) => setMode(event.target.value as typeof mode)}>
              <option value="strict">Strict · 禁止流式</option>
              <option value="bounded_stream">Bounded · 有界规则（执行规则当前仍保守拒绝流式）</option>
              <option value="detect_only_stream">Detect only · 流式仅检测</option>
            </select>
          </Field>
        </div>
        <div className="notice warning">
          <strong>安全边界</strong>
          <span>Provider/Gateway Key 等密钥检测始终开启，不受此 Policy 开关影响。</span>
        </div>
        <div className="rule-editor-list">
          {rules.map((rule, index) => (
            <section className="rule-editor" key={rule.id || `new-${index}`}>
              <header>
                <strong>规则 {index + 1}</strong>
                {rules.length > 1 && <button type="button" className="button ghost" onClick={() => setRules(rules.filter((_, item) => item !== index))}>移除</button>}
              </header>
              <div className="form-grid">
                <Field label="名称"><input value={rule.name} onChange={(event) => updateRule(index, { name: event.target.value })} /></Field>
                <Field label="类型">
                  <select value={rule.kind} onChange={(event) => updateRule(index, {
                    kind: event.target.value as EditableRule["kind"],
                    builtin: event.target.value === "builtin" ? "china_phone" : "",
                  })}>
                    <option value="builtin">内置规则</option>
                    <option value="regex">RE2 正则</option>
                    <option value="dictionary">字典</option>
                  </select>
                </Field>
                {rule.kind === "builtin" && (
                  <Field label="内置类别">
                    <select value={rule.builtin} onChange={(event) => updateRule(index, { builtin: event.target.value })}>
                      {builtins.map((item) => <option key={item} value={item}>{item}</option>)}
                    </select>
                  </Field>
                )}
                {rule.kind === "regex" && <Field label="正则表达式"><input value={rule.pattern ?? ""} onChange={(event) => updateRule(index, { pattern: event.target.value })} /></Field>}
                {rule.kind === "dictionary" && (
                  <Field label="字典（每行一项）">
                    <textarea rows={3} value={(rule.dictionary ?? []).join("\n")} onChange={(event) => updateRule(index, { dictionary: event.target.value.split("\n").map((value) => value.trim()).filter(Boolean) })} />
                  </Field>
                )}
                <Field label="动作">
                  <select value={rule.action} onChange={(event) => updateRule(index, { action: event.target.value as EditableRule["action"] })}>
                    <option value="detect_only">Detect only</option>
                    <option value="mask">Mask</option>
                    <option value="replace">Replace</option>
                    <option value="reject">Reject</option>
                  </select>
                </Field>
                {rule.action === "replace" && <Field label="替换文本"><input value={rule.replacement ?? ""} onChange={(event) => updateRule(index, { replacement: event.target.value })} /></Field>}
                <Field label="优先级"><input type="number" value={rule.priority} onChange={(event) => updateRule(index, { priority: Number(event.target.value) })} /></Field>
                <div className="scope-checks">
                  {(["inbound", "outbound"] as const).map((scope) => (
                    <label className="check-row" key={scope}>
                      <input type="checkbox" checked={rule.scopes.includes(scope)} onChange={(event) => updateRule(index, {
                        scopes: event.target.checked
                          ? [...rule.scopes, scope]
                          : rule.scopes.filter((item) => item !== scope),
                      })} />
                      <span>{scope}</span>
                    </label>
                  ))}
                  <label className="check-row">
                    <input type="checkbox" checked={rule.enabled} onChange={(event) => updateRule(index, { enabled: event.target.checked })} />
                    <span>启用规则</span>
                  </label>
                </div>
              </div>
            </section>
          ))}
        </div>
        <button type="button" className="button ghost" onClick={() => setRules([...rules, blankRule()])}>＋ 添加规则</button>
        <label className="check-row policy-enabled">
          <input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />
          <span>启用此 Policy</span>
        </label>
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions">
          <button type="button" className="button ghost" onClick={onClose}>取消</button>
          <button className="button primary" disabled={mutation.isPending}>编译并保存</button>
        </div>
      </form>
    </Modal>
  );
}

function RedactionPolicyTest({ policy, onClose }: { policy: RedactionPolicy; onClose: () => void }) {
  const [input, setInput] = useState("");
  const [scope, setScope] = useState<"inbound" | "outbound">("inbound");
  const [result, setResult] = useState<RedactionTestResult | null>(null);
  const mutation = useMutation({
    mutationFn: () => api.testRedactionPolicy(policy.id, { input, scope }),
    onSuccess: setResult,
  });
  return (
    <Modal title={`安全测试 · ${policy.name}`} onClose={onClose}>
      <form onSubmit={(event) => { event.preventDefault(); mutation.mutate(); }}>
        <Field label="方向">
          <select value={scope} onChange={(event) => setScope(event.target.value as typeof scope)}>
            <option value="inbound">Inbound</option>
            <option value="outbound">Outbound</option>
          </select>
        </Field>
        <Field label="测试内容" hint="服务端只返回规则元数据，不回显原文。">
          <textarea rows={6} value={input} onChange={(event) => setInput(event.target.value)} />
        </Field>
        {result && (
          <div className={`preview-result ${result.match_count ? "violated" : ""}`}>
            <StatusDot ok={!result.match_count} />
            <div>
              <strong>{result.match_count ? `命中 ${result.match_count} 条规则` : "未命中规则"}</strong>
              <small>{result.matches.map((match) => `${match.category} · ${match.action}`).join(" / ") || "输入不会出现在结果中"}</small>
            </div>
          </div>
        )}
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions">
          <button type="button" className="button ghost" onClick={onClose}>关闭</button>
          <button className="button primary" disabled={mutation.isPending || !input}>运行测试</button>
        </div>
      </form>
    </Modal>
  );
}
