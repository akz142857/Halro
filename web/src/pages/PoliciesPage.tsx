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
import { useTranslation } from "react-i18next";

export function PoliciesPage() {
  const { t } = useTranslation();
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
        eyebrow={t("policies.eyebrow")}
        title={t("policies.title")}
        description={t("policies.description")}
        action={
          <button className="button primary" onClick={() => setEditing("new")}>
            {t("policies.create")}
          </button>
        }
      />
      {policies.isPending && <Loading />}
      {policies.isError && <ErrorState error={policies.error} />}
      {policies.data?.items.length === 0 && (
        <EmptyState
          title={t("policies.emptyTitle")}
          action={<button className="button primary" onClick={() => setEditing("new")}>{t("policies.baseline")}</button>}
        >
          {t("policies.emptyDescription")}
        </EmptyState>
      )}
      {!!policies.data?.items.length && (
        <div className="policy-card-grid">
          {policies.data.items.map((policy) => (
            <article className="policy-card" key={policy.id}>
              <header>
                <span><StatusDot ok={policy.enabled} /><strong>{policy.name}</strong></span>
                <span className={`badge ${policy.action === "temporary_block" ? "warning" : ""}`}>
                  {policyActionLabel(t, policy.action)}
                </span>
              </header>
              <div className="thresholds">
                <Threshold label={t("policies.request")} value={policy.request_tokens ? t("policies.tokenCount", { count: compactNumber(policy.request_tokens) }) : t("policies.off")} />
                <Threshold label={t("policies.perMinute")} value={policy.tokens_per_minute ? t("policies.tokenCount", { count: compactNumber(policy.tokens_per_minute) }) : t("policies.off")} />
                <Threshold label={t("policies.costMinute")} value={policy.cost_micros_per_minute ? money(policy.cost_micros_per_minute) : t("policies.off")} />
                <Threshold label={t("policies.concurrency")} value={policy.concurrency ? String(policy.concurrency) : t("policies.off")} />
                <Threshold label={t("policies.errorRate")} value={policy.error_rate ? `${Math.round(policy.error_rate * 100)}%` : t("policies.off")} />
                <Threshold label={t("policies.uniqueIP")} value={policy.unique_ips_per_minute ? String(policy.unique_ips_per_minute) : t("policies.off")} />
                <Threshold label={t("policies.ewma")} value={policy.ewma_enabled ? `${policy.ewma_multiplier}× · ${t("policies.detectOnly")}` : t("policies.off")} />
              </div>
              <footer>
                <code>{policy.id}</code>
                <div className="row-actions">
                  <button className="button ghost" onClick={() => setPreviewing(policy)}>{t("policies.simulate")}</button>
                  <button className="button ghost" onClick={() => setEditing(policy)}>{t("common.edit")}</button>
                  <ConfirmButton
                    label={t("common.delete")}
                    confirmLabel={t("policies.deleteConfirm", { name: policy.name })}
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
  const { t } = useTranslation();
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
    <Modal title={current ? t("policies.edit") : t("policies.createTitle")} onClose={onClose}>
      <form className="form-grid" onSubmit={(event) => {
        event.preventDefault();
        if (name.trim()) mutation.mutate();
      }}>
        <Field label={t("policies.name")}><input autoFocus value={name} onChange={(event) => setName(event.target.value)} /></Field>
        <Field label={t("policies.action")}>
          <select value={action} onChange={(event) => setAction(event.target.value as typeof action)}>
            <option value="observe">{t("policies.observe")}</option>
            <option value="alert">{t("policies.alert")}</option>
            <option value="temporary_block">{t("policies.temporaryBlock")}</option>
          </select>
        </Field>
        <Field label={t("policies.requestLimit")}><NumberInput value={requestTokens} set={setRequestTokens} /></Field>
        <Field label={t("policies.minuteLimit")}><NumberInput value={tokensPerMinute} set={setTokensPerMinute} /></Field>
        <Field label={t("policies.costLimit")}><NumberInput value={costPerMinute} set={setCostPerMinute} step=".01" /></Field>
        <Field label={t("policies.concurrencyLimit")}><NumberInput value={concurrency} set={setConcurrency} /></Field>
        <Field label={t("policies.errorThreshold")}><NumberInput value={errorRate} set={setErrorRate} step=".1" /></Field>
        <Field label={t("policies.minimumSamples")}><NumberInput value={minimumSamples} set={setMinimumSamples} /></Field>
        <Field label={t("policies.uniqueSources")}><NumberInput value={uniqueIPs} set={setUniqueIPs} /></Field>
        {action === "temporary_block" && (
          <>
            <Field label={t("policies.violations")}><NumberInput value={violations} set={setViolations} /></Field>
            <Field label={t("policies.blockTTL")}><NumberInput value={blockTTL} set={setBlockTTL} /></Field>
            <Field label={t("policies.cooldown")}><NumberInput value={cooldown} set={setCooldown} /></Field>
          </>
        )}
        <label className="check-row">
          <input type="checkbox" checked={ewmaEnabled} onChange={(event) => setEWMAEnabled(event.target.checked)} />
          <span>{t("policies.enableEWMA")}</span>
        </label>
        {ewmaEnabled && (
          <>
            <p className="form-hint">{t("policies.ewmaHint")}</p>
            <Field label={t("policies.ewmaAlpha")}><NumberInput value={ewmaAlpha} set={setEWMAAlpha} step=".05" /></Field>
            <Field label={t("policies.multiplier")}><NumberInput value={ewmaMultiplier} set={setEWMAMultiplier} step=".1" /></Field>
            <Field label={t("policies.baselineSamples")}><NumberInput value={ewmaMinimumSamples} set={setEWMAMinimumSamples} /></Field>
            <Field label={t("policies.warmup")}><NumberInput value={ewmaWarmup} set={setEWMAWarmup} /></Field>
            <Field label={t("policies.window")}><NumberInput value={ewmaWindow} set={setEWMAWindow} /></Field>
            <Field label={t("policies.alertCooldown")}><NumberInput value={ewmaCooldown} set={setEWMACooldown} /></Field>
            <Field label={t("policies.rpmFloor")}><NumberInput value={ewmaRPMFloor} set={setEWMARPMFloor} /></Field>
            <Field label={t("policies.tpmFloor")}><NumberInput value={ewmaTPMFloor} set={setEWMATPMFloor} /></Field>
            <Field label={t("policies.tokenFloor")}><NumberInput value={ewmaTokensFloor} set={setEWMATokensFloor} step=".1" /></Field>
            <Field label={t("policies.costFloor")}><NumberInput value={ewmaCostFloor} set={setEWMACostFloor} step=".01" /></Field>
          </>
        )}
        <label className="check-row">
          <input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />
          <span>{t("policies.enable")}</span>
        </label>
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions">
          <button type="button" className="button ghost" onClick={onClose}>{t("common.cancel")}</button>
          <button className="button primary" disabled={mutation.isPending}>{t("policies.save")}</button>
        </div>
      </form>
    </Modal>
  );
}

function PolicyPreview({ policy, onClose }: { policy: TokenGuardPolicy; onClose: () => void }) {
  const { t } = useTranslation();
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
    <Modal title={t("policies.previewTitle", { name: policy.name })} onClose={onClose}>
      <form onSubmit={(event) => { event.preventDefault(); mutation.mutate(); }}>
        <Field label={t("policies.estimatedTokens")}><NumberInput value={tokens} set={setTokens} /></Field>
        <Field label={t("policies.windowTokens")}><NumberInput value={windowTokens} set={setWindowTokens} /></Field>
        <Field label={t("policies.currentConcurrency")}><NumberInput value={concurrency} set={setConcurrency} /></Field>
        {result && (
          <div className={`preview-result ${result.violated ? "violated" : ""}`}>
            <StatusDot ok={!result.violated} />
            <div>
              <strong>{result.violated ? t("policies.hit", { reason: result.reason }) : t("policies.noHit")}</strong>
              <small>{t("policies.resultAction", { action: policyActionLabel(t, result.action) })}</small>
            </div>
          </div>
        )}
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions">
          <button type="button" className="button ghost" onClick={onClose}>{t("common.close")}</button>
          <button className="button primary" disabled={mutation.isPending}>{t("policies.run")}</button>
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

function policyActionLabel(t: ReturnType<typeof useTranslation>["t"], action: string) {
  if (action === "temporary_block") return t("policies.temporaryBlock");
  if (action === "alert") return t("policies.alert");
  return t("policies.observe");
}
