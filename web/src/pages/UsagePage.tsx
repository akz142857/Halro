import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useRef, useState, type FormEvent } from "react";
import { api } from "../api";
import { ErrorState, Field, Loading, Modal, PageHeader, StatusDot } from "../components";
import { compactNumber, dateTime, money } from "../format";
import { useTranslation } from "react-i18next";
import type { UsageAttempt } from "../types";

export function UsagePage() {
  const { t } = useTranslation();
  const [status, setStatus] = useState("");
  const [model, setModel] = useState("");
  const [requestID, setRequestID] = useState(() => new URLSearchParams(window.location.search).get("request_id") ?? "");
  const [adjusting, setAdjusting] = useState<UsageAttempt | null>(null);
  const usage = useInfiniteQuery({
    queryKey: ["usage", status, model, requestID],
    initialPageParam: "",
    queryFn: ({ pageParam }) => api.usage(`?${new URLSearchParams({
      limit: "100", ...(status ? { status } : {}), ...(model ? { model } : {}), ...(requestID ? { request_id: requestID } : {}),
      ...(pageParam ? { cursor: pageParam } : {}),
    })}`),
    getNextPageParam: (page) => page.next_cursor || undefined,
  });
  const attempts = usage.data?.pages.flatMap((page) => page.items) ?? [];
  return (
    <>
      <PageHeader
        eyebrow={t("usage.eyebrow")}
        title={t("usage.title")}
        description={t("usage.description")}
      />
      <div className="filter-bar">
        <label><span>Request ID</span><input value={requestID} onChange={(event) => setRequestID(event.target.value)} placeholder="req_…" /></label>
        <label><span>{t("usage.model")}</span><input value={model} onChange={(event) => setModel(event.target.value)} placeholder="chat" /></label>
        <label>
          <span>{t("usage.status")}</span>
          <select value={status} onChange={(event) => setStatus(event.target.value)}>
            <option value="">{t("usage.all")}</option>
            <option value="success">{t("usage.success")}</option>
            <option value="error">{t("usage.error")}</option>
          </select>
        </label>
        <span className="filter-count">{t("usage.records", { count: attempts.length })}</span>
      </div>
      {usage.isPending && <Loading />}
      {usage.isError && <ErrorState error={usage.error} />}
      {usage.data && (
        <div className="table-shell">
          <table>
            <thead><tr><th>{t("usage.request")}</th><th>{t("usage.model")}</th><th>{t("usage.tokens")}</th><th>{t("usage.cost")}</th><th>{t("usage.latency")}</th><th>{t("usage.status")}</th><th>{t("usage.time")}</th></tr></thead>
            <tbody>
              {attempts.map((attempt) => (
                <tr key={attempt.event_id}>
                  <td><code>{attempt.request_id}</code><small>{t("usage.attempt", { count: attempt.attempt })}</small></td>
                  <td><strong>{attempt.requested_model || "—"}</strong><small>{attempt.provider_model}</small></td>
                  <td>{attempt.tokens_estimated ? t("usage.estimated") : ""}{compactNumber(attempt.provider_input_tokens + attempt.provider_output_tokens)}<small>{t("usage.inputOutput", { input: compactNumber(attempt.provider_input_tokens), output: compactNumber(attempt.provider_output_tokens) })} · {attempt.tokens_estimated ? t("usage.conservative") : t("usage.reported")}</small></td>
                  <td>
                    <strong>{attempt.final_cost_micros_usd == null ? t("usage.unknownCost") : money(attempt.final_cost_micros_usd)}</strong>
                    {!!attempt.tags?.length && <small>{attempt.tags.map((tag) => <span className="badge" key={tag}>{tag}</span>)}</small>}
                    <details><summary>{t("usage.costEvidence")}</summary><small>
                      {t("usage.originalAdjustmentFinal", { original: attempt.original_cost_micros_usd == null ? "—" : money(attempt.original_cost_micros_usd), adjustment: money(attempt.adjustment_delta_micros_usd), final: attempt.final_cost_micros_usd == null ? "—" : money(attempt.final_cost_micros_usd) })}<br />
                      {attempt.price_snapshot?.price_version_id ? `${attempt.price_snapshot.price_version_id} · v${attempt.price_snapshot.price_version}` : attempt.price_evidence_status}<br />
                      {attempt.input_cost_micros_usd == null ? "" : t("usage.formulaComponents", { input: money(attempt.input_cost_micros_usd), output: money(attempt.output_cost_micros_usd ?? 0), fixed: money(attempt.fixed_cost_micros_usd ?? 0) })}
                    </small></details>
                    {attempt.final_cost_micros_usd != null && <button className="button ghost" onClick={() => setAdjusting(attempt)}>{t("usage.adjustCost")}</button>}
                  </td>
                  <td>{attempt.latency_millis} ms</td>
                  <td><span className="inline-status"><StatusDot ok={attempt.status === "success"} />{attempt.status === "success" ? t("usage.success") : t("usage.error")}</span></td>
                  <td>{dateTime(attempt.completed_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {usage.hasNextPage && <button className="button ghost load-more" disabled={usage.isFetchingNextPage} onClick={() => usage.fetchNextPage()}>
            {usage.isFetchingNextPage ? t("common.loading") : t("common.loadMore")}
          </button>}
        </div>
      )}
      {adjusting && <CostAdjustmentModal attempt={adjusting} onClose={() => setAdjusting(null)} />}
    </>
  );
}

function CostAdjustmentModal({ attempt, onClose }: { attempt: UsageAttempt; onClose: () => void }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [delta, setDelta] = useState(0);
  const [reason, setReason] = useState("");
  const [evidence, setEvidence] = useState("");
  const [password, setPassword] = useState("");
  const [totp, setTOTP] = useState("");
  const idempotencyKey = useRef(crypto.randomUUID());
  const base = () => ({ mode: "explicit_delta", explicit_delta_micros_usd: delta, expected_sequence: attempt.adjustment_sequence,
    expected_net_cost_micros_usd: attempt.final_cost_micros_usd, reason_code: "invoice_difference", reason, evidence_digest: evidence });
  const preview = useMutation({ mutationFn: () => api.previewCostAdjustment(attempt.attempt_id, base()) });
  const create = useMutation({ mutationFn: () => api.createCostAdjustment(attempt.attempt_id, { ...base(), confirm: true, current_password: password, totp_code: totp }, idempotencyKey.current),
    onSuccess: () => { queryClient.invalidateQueries({ queryKey: ["usage"] }); onClose(); } });
  const submit = (event: FormEvent) => { event.preventDefault(); create.mutate(); };
  return <Modal title={t("usage.adjustCost")} onClose={onClose}>
    <form onSubmit={submit}>
      <p>{t("usage.adjustmentImmutableWarning")}</p>
      <Field label={t("usage.deltaMicros")}><input type="number" required value={delta} onChange={(event) => setDelta(Number(event.target.value))} /></Field>
      <Field label={t("usage.reason")}><textarea required maxLength={1024} value={reason} onChange={(event) => setReason(event.target.value)} /></Field>
      <Field label={t("usage.evidenceDigest")}><input required pattern="sha256:[a-fA-F0-9]{64}" value={evidence} onChange={(event) => setEvidence(event.target.value)} placeholder="sha256:…" /></Field>
      <button type="button" className="button ghost" disabled={preview.isPending || !reason || !evidence} onClick={() => preview.mutate()}>{t("usage.previewAdjustment")}</button>
      {preview.data && <div className="notice warning"><strong>{money(preview.data.event.net_cost_before_micros_usd)} → {money(preview.data.event.net_cost_after_micros_usd)}</strong><span>{t("usage.adjustmentPeriods", { service: preview.data.event.service_period_id, posted: preview.data.event.posted_period_id })}</span></div>}
      <Field label={t("usage.currentPassword")}><input type="password" required value={password} onChange={(event) => setPassword(event.target.value)} /></Field>
      <Field label={t("usage.totpOptional")}><input inputMode="numeric" value={totp} onChange={(event) => setTOTP(event.target.value)} /></Field>
      {(preview.isError || create.isError) && <ErrorState error={preview.error || create.error} />}
      <button className="button primary" disabled={!preview.data || create.isPending}>{t("usage.confirmAdjustment")}</button>
    </form>
  </Modal>;
}
