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
  PageHeader,
  StatusDot,
} from "../components";
import { compactNumber, money } from "../format";
import type { TokenGuardPolicy, TokenGuardPreview } from "../types";
import { RedactionPoliciesSection } from "./RedactionPoliciesSection";

export function PoliciesPage() {
  const [editing, setEditing] = useState<TokenGuardPolicy | "new" | null>(null);
  const [previewing, setPreviewing] = useState<TokenGuardPolicy | null>(null);
  const policies = useQuery({
    queryKey: ["token-guard-policies"],
    queryFn: api.tokenGuardPolicies,
  });
  const queryClient = useQueryClient();
  const remove = useMutation({
    mutationFn: (policy: TokenGuardPolicy) =>
      api.deleteTokenGuardPolicy(policy.id, policy.revision),
    onSuccess: () =>
      queryClient.invalidateQueries({ queryKey: ["token-guard-policies"] }),
  });
  return (
    <>
      <PageHeader
        eyebrow="ANOMALY CONTAINMENT"
        title="Token Guard Policies"
        description="固定阈值负责处置；可选 EWMA 相对基线只检测和告警，绝不会自动封禁。"
        action={
          <button className="button primary" onClick={() => setEditing("new")}>
            ＋ 新建 Policy
          </button>
        }
      />
      {policies.isPending && <Loading />}
      {policies.isError && <ErrorState error={policies.error} />}
      {policies.data?.items.length === 0 && (
        <EmptyState
          title="还没有 Token Guard Policy"
          action={<button className="button primary" onClick={() => setEditing("new")}>创建安全基线</button>}
        >
          建议从 detect/alert 开始观察，再对明确的硬上限启用 temporary block。
        </EmptyState>
      )}
      {!!policies.data?.items.length && (
        <div className="policy-card-grid">
          {policies.data.items.map((policy) => (
            <article className="policy-card" key={policy.id}>
              <header>
                <span><StatusDot ok={policy.enabled} /><strong>{policy.name}</strong></span>
                <span className={`badge ${policy.action === "temporary_block" ? "warning" : ""}`}>
                  {policy.action}
                </span>
              </header>
              <div className="thresholds">
                <Threshold label="Request" value={policy.request_tokens ? `${compactNumber(policy.request_tokens)} tokens` : "off"} />
                <Threshold label="Per minute" value={policy.tokens_per_minute ? `${compactNumber(policy.tokens_per_minute)} tokens` : "off"} />
                <Threshold label="Cost/min" value={policy.cost_micros_per_minute ? money(policy.cost_micros_per_minute) : "off"} />
                <Threshold label="Concurrency" value={policy.concurrency ? String(policy.concurrency) : "off"} />
                <Threshold label="Error rate" value={policy.error_rate ? `${Math.round(policy.error_rate * 100)}%` : "off"} />
                <Threshold label="Unique IP/min" value={policy.unique_ips_per_minute ? String(policy.unique_ips_per_minute) : "off"} />
                <Threshold label="EWMA baseline" value={policy.ewma_enabled ? `${policy.ewma_multiplier}× · detect only` : "off"} />
              </div>
              <footer>
                <code>{policy.id}</code>
                <div className="row-actions">
                  <button className="button ghost" onClick={() => setPreviewing(policy)}>模拟</button>
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
      {editing && <PolicyForm current={editing === "new" ? undefined : editing} onClose={() => setEditing(null)} />}
      {previewing && <PolicyPreview policy={previewing} onClose={() => setPreviewing(null)} />}
      <RedactionPoliciesSection />
    </>
  );
}

function PolicyForm({
  current,
  onClose,
}: {
  current?: TokenGuardPolicy;
  onClose: () => void;
}) {
  const [name, setName] = useState(current?.name ?? "");
  const [action, setAction] = useState<TokenGuardPolicy["action"]>(current?.action ?? "alert");
  const [enabled, setEnabled] = useState(current?.enabled ?? true);
  const [requestTokens, setRequestTokens] = useState(current?.request_tokens ?? 32_000);
  const [tokensPerMinute, setTokensPerMinute] = useState(current?.tokens_per_minute ?? 200_000);
  const [costPerMinute, setCostPerMinute] = useState((current?.cost_micros_per_minute ?? 5_000_000) / 1_000_000);
  const [concurrency, setConcurrency] = useState(current?.concurrency ?? 20);
  const [errorRate, setErrorRate] = useState((current?.error_rate ?? 0.2) * 100);
  const [minimumSamples, setMinimumSamples] = useState(current?.minimum_samples ?? 10);
  const [uniqueIPs, setUniqueIPs] = useState(current?.unique_ips_per_minute ?? 10);
  const [violations, setViolations] = useState(current?.violations_before_block ?? 2);
  const [blockTTL, setBlockTTL] = useState(current?.block_ttl_seconds ?? 300);
  const [cooldown, setCooldown] = useState(current?.cooldown_seconds ?? 60);
  const [ewmaEnabled, setEWMAEnabled] = useState(current?.ewma_enabled ?? false);
  const [ewmaAlpha, setEWMAAlpha] = useState(current?.ewma_alpha || 0.2);
  const [ewmaMultiplier, setEWMAMultiplier] = useState(current?.ewma_multiplier || 3);
  const [ewmaMinimumSamples, setEWMAMinimumSamples] = useState(current?.ewma_minimum_samples || 100);
  const [ewmaWarmup, setEWMAWarmup] = useState(current?.ewma_warmup_seconds || 3600);
  const [ewmaWindow, setEWMAWindow] = useState(current?.ewma_evaluation_window_seconds || 60);
  const [ewmaCooldown, setEWMACooldown] = useState(current?.ewma_cooldown_seconds || 300);
  const [ewmaRPMFloor, setEWMARPMFloor] = useState(current?.ewma_absolute_rpm || 60);
  const [ewmaTPMFloor, setEWMATPMFloor] = useState(current?.ewma_absolute_tpm || 50_000);
  const [ewmaTokensFloor, setEWMATokensFloor] = useState(current?.ewma_absolute_tokens_per_request || 4_000);
  const [ewmaCostFloor, setEWMACostFloor] = useState((current?.ewma_absolute_cost_micros_per_minute || 1_000_000) / 1_000_000);
  const queryClient = useQueryClient();
  const body = {
    name, enabled, action,
    request_tokens: requestTokens,
    tokens_per_minute: tokensPerMinute,
    cost_micros_per_minute: Math.round(costPerMinute * 1_000_000),
    error_rate: errorRate / 100,
    minimum_samples: minimumSamples,
    concurrency,
    unique_ips_per_minute: uniqueIPs,
    violations_before_block: action === "temporary_block" ? Math.max(violations, 2) : violations,
    block_ttl_seconds: blockTTL,
    cooldown_seconds: cooldown,
    ewma_enabled: ewmaEnabled,
    ewma_alpha: ewmaAlpha,
    ewma_multiplier: ewmaMultiplier,
    ewma_minimum_samples: ewmaMinimumSamples,
    ewma_warmup_seconds: ewmaWarmup,
    ewma_evaluation_window_seconds: ewmaWindow,
    ewma_cooldown_seconds: ewmaCooldown,
    ewma_absolute_rpm: ewmaRPMFloor,
    ewma_absolute_tpm: ewmaTPMFloor,
    ewma_absolute_tokens_per_request: ewmaTokensFloor,
    ewma_absolute_cost_micros_per_minute: Math.round(ewmaCostFloor * 1_000_000),
  };
  const mutation = useMutation({
    mutationFn: () => current
      ? api.updateTokenGuardPolicy(current.id, body, current.revision)
      : api.createTokenGuardPolicy(body),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["token-guard-policies"] });
      onClose();
    },
  });
  return (
    <Modal title={current ? "编辑 Token Guard Policy" : "创建 Token Guard Policy"} onClose={onClose}>
      <form className="form-grid" onSubmit={(event) => {
        event.preventDefault();
        if (name.trim()) mutation.mutate();
      }}>
        <Field label="Policy 名称"><input autoFocus value={name} onChange={(event) => setName(event.target.value)} /></Field>
        <Field label="处置动作">
          <select value={action} onChange={(event) => setAction(event.target.value as typeof action)}>
            <option value="observe">Observe only</option>
            <option value="alert">Alert</option>
            <option value="temporary_block">Temporary block</option>
          </select>
        </Field>
        <Field label="单请求 Token 上限"><NumberInput value={requestTokens} set={setRequestTokens} /></Field>
        <Field label="每分钟 Token 上限"><NumberInput value={tokensPerMinute} set={setTokensPerMinute} /></Field>
        <Field label="每分钟成本上限（USD）"><NumberInput value={costPerMinute} set={setCostPerMinute} step=".01" /></Field>
        <Field label="并发上限"><NumberInput value={concurrency} set={setConcurrency} /></Field>
        <Field label="错误率阈值（%）"><NumberInput value={errorRate} set={setErrorRate} step=".1" /></Field>
        <Field label="最少样本"><NumberInput value={minimumSamples} set={setMinimumSamples} /></Field>
        <Field label="每分钟唯一来源 IP"><NumberInput value={uniqueIPs} set={setUniqueIPs} /></Field>
        {action === "temporary_block" && (
          <>
            <Field label="触发次数"><NumberInput value={violations} set={setViolations} /></Field>
            <Field label="封禁时长（秒）"><NumberInput value={blockTTL} set={setBlockTTL} /></Field>
            <Field label="Cooldown（秒）"><NumberInput value={cooldown} set={setCooldown} /></Field>
          </>
        )}
        <label className="check-row">
          <input type="checkbox" checked={ewmaEnabled} onChange={(event) => setEWMAEnabled(event.target.checked)} />
          <span>启用实验性 EWMA 相对基线（detect-only）</span>
        </label>
        {ewmaEnabled && (
          <>
            <p className="form-hint">EWMA 命中只产生告警；硬阈值仍优先，且只有硬阈值允许 temporary block。</p>
            <Field label="EWMA Alpha"><NumberInput value={ewmaAlpha} set={setEWMAAlpha} step=".05" /></Field>
            <Field label="相对基线倍数"><NumberInput value={ewmaMultiplier} set={setEWMAMultiplier} step=".1" /></Field>
            <Field label="基线最少样本"><NumberInput value={ewmaMinimumSamples} set={setEWMAMinimumSamples} /></Field>
            <Field label="Warmup（秒）"><NumberInput value={ewmaWarmup} set={setEWMAWarmup} /></Field>
            <Field label="评估窗口（秒，10 秒倍数）"><NumberInput value={ewmaWindow} set={setEWMAWindow} /></Field>
            <Field label="告警 Cooldown（秒）"><NumberInput value={ewmaCooldown} set={setEWMACooldown} /></Field>
            <Field label="RPM 绝对下限"><NumberInput value={ewmaRPMFloor} set={setEWMARPMFloor} /></Field>
            <Field label="TPM 绝对下限"><NumberInput value={ewmaTPMFloor} set={setEWMATPMFloor} /></Field>
            <Field label="平均 Token/request 下限"><NumberInput value={ewmaTokensFloor} set={setEWMATokensFloor} step=".1" /></Field>
            <Field label="成本速率下限（USD/min）"><NumberInput value={ewmaCostFloor} set={setEWMACostFloor} step=".01" /></Field>
          </>
        )}
        <label className="check-row">
          <input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />
          <span>启用此 Policy</span>
        </label>
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions">
          <button type="button" className="button ghost" onClick={onClose}>取消</button>
          <button className="button primary" disabled={mutation.isPending}>保存 Policy</button>
        </div>
      </form>
    </Modal>
  );
}

function PolicyPreview({ policy, onClose }: { policy: TokenGuardPolicy; onClose: () => void }) {
  const [tokens, setTokens] = useState(policy.request_tokens || 1_000);
  const [windowTokens, setWindowTokens] = useState(0);
  const [concurrency, setConcurrency] = useState(1);
  const [result, setResult] = useState<TokenGuardPreview | null>(null);
  const mutation = useMutation({
    mutationFn: () => api.previewTokenGuardPolicy(policy.id, {
      estimated_tokens: tokens,
      estimated_cost_micros_usd: 0,
      concurrency,
      has_new_source: true,
      window: {
        requests: 10, tokens: windowTokens, cost_micros_usd: 0,
        errors: 0, unique_ips: 1,
      },
    }),
    onSuccess: setResult,
  });
  return (
    <Modal title={`模拟 · ${policy.name}`} onClose={onClose}>
      <form onSubmit={(event) => { event.preventDefault(); mutation.mutate(); }}>
        <Field label="本次估算 Token"><NumberInput value={tokens} set={setTokens} /></Field>
        <Field label="窗口内已有 Token"><NumberInput value={windowTokens} set={setWindowTokens} /></Field>
        <Field label="当前并发"><NumberInput value={concurrency} set={setConcurrency} /></Field>
        {result && (
          <div className={`preview-result ${result.violated ? "violated" : ""}`}>
            <StatusDot ok={!result.violated} />
            <div>
              <strong>{result.violated ? `命中：${result.reason}` : "未命中阈值"}</strong>
              <small>动作：{result.action}</small>
            </div>
          </div>
        )}
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions">
          <button type="button" className="button ghost" onClick={onClose}>关闭</button>
          <button className="button primary" disabled={mutation.isPending}>运行模拟</button>
        </div>
      </form>
    </Modal>
  );
}

function NumberInput({
  value,
  set,
  step = "1",
}: {
  value: number;
  set: (value: number) => void;
  step?: string;
}) {
  return <input type="number" min="0" step={step} value={value} onChange={(event) => set(Number(event.target.value))} />;
}

function Threshold({ label, value }: { label: string; value: string }) {
  return <div><small>{label}</small><strong>{value}</strong></div>;
}
