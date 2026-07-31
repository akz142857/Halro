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
import { dateTime } from "../format";
import type { AlertWebhook } from "../types";

export function OperationsPage() {
  const [editing, setEditing] = useState<AlertWebhook | "new" | null>(null);
  const audit = useQuery({ queryKey: ["audit"], queryFn: api.audit });
  const alerts = useQuery({ queryKey: ["alerts"], queryFn: api.alerts });
  const queryClient = useQueryClient();
  const remove = useMutation({
    mutationFn: (webhook: AlertWebhook) => api.deleteAlert(webhook.id, webhook.revision),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["alerts"] }),
  });
  const test = useMutation({ mutationFn: (id: string) => api.testAlert(id) });
  return (
    <>
      <PageHeader
        eyebrow="SECURITY OPERATIONS"
        title="Alerts & Audit"
        description="异常状态推送到受控 Webhook；所有配置、测试和管理动作进入 HMAC 链式审计日志。"
        action={<button className="button primary" onClick={() => setEditing("new")}>＋ Alert Webhook</button>}
      />
      <section className="panel operations-alerts">
        <header className="panel-header">
          <div><p className="eyebrow">OUTBOUND SIGNALS</p><h2>Alert Webhooks</h2></div>
          <span className="count">{alerts.data?.items.length ?? 0}</span>
        </header>
        {alerts.isPending && <Loading />}
        {alerts.isError && <ErrorState error={alerts.error} />}
        {alerts.data?.items.length === 0 && (
          <EmptyState title="没有 Alert Webhook">Token Guard 事件仍会在本机统计，但不会发送到外部系统。</EmptyState>
        )}
        {alerts.data?.items.map((webhook) => (
          <div className="alert-row" key={webhook.id}>
            <span className="provider-icon">WH</span>
            <div>
              <span><StatusDot ok={webhook.enabled} /><strong>{webhook.name}</strong></span>
              <small>{webhook.url}</small>
              <code>{webhook.id}</code>
            </div>
            <div className="alert-secret">
              <small>SECRET HEADER</small>
              <strong>{webhook.secret_configured ? webhook.header_name || "configured" : "none"}</strong>
            </div>
            <div className="row-actions">
              <button className="button ghost" disabled={!webhook.enabled || test.isPending} onClick={() => test.mutate(webhook.id)}>
                测试
              </button>
              <button className="button ghost" onClick={() => setEditing(webhook)}>编辑</button>
              <ConfirmButton
                label="删除"
                confirmLabel={`删除 Alert Webhook “${webhook.name}”？`}
                disabled={remove.isPending}
                onConfirm={() => remove.mutate(webhook)}
              />
            </div>
          </div>
        ))}
        {test.isSuccess && <div className="notice success"><strong>测试事件已送达</strong></div>}
        {test.isError && <ErrorState error={test.error} />}
      </section>
      <section className="panel audit-panel">
        <header className="panel-header">
          <div><p className="eyebrow">RECENT AUDIT EVENTS</p><h2>不可静默修改的操作轨迹</h2></div>
          <span className="health-pill"><StatusDot />CHAIN ACTIVE</span>
        </header>
        {audit.isPending && <Loading />}
        {audit.isError && <ErrorState error={audit.error} />}
        {audit.data && (
          <div className="timeline">
            {audit.data.items.map((record) => (
              <article key={record.sequence}>
                <span className="timeline-sequence">#{record.sequence}</span>
                <span className="timeline-mark" />
                <div>
                  <strong>{record.action}</strong>
                  <p>{record.actor_id || record.actor_type} → {record.target_type} {record.target_id}</p>
                  <small>{dateTime(record.occurred_at)} · {record.outcome}{record.reason_code ? ` · ${record.reason_code}` : ""}</small>
                </div>
              </article>
            ))}
          </div>
        )}
      </section>
      {editing && (
        <AlertForm current={editing === "new" ? undefined : editing} onClose={() => setEditing(null)} />
      )}
    </>
  );
}

function AlertForm({
  current,
  onClose,
}: {
  current?: AlertWebhook;
  onClose: () => void;
}) {
  const [name, setName] = useState(current?.name ?? "");
  const [url, setURL] = useState(current?.url ?? "");
  const [header, setHeader] = useState(current?.header_name || "authorization");
  const [secret, setSecret] = useState("");
  const [removeSecret, setRemoveSecret] = useState(false);
  const [enabled, setEnabled] = useState(current?.enabled ?? false);
  const queryClient = useQueryClient();
  const body = {
    name, url, enabled,
    header_name: removeSecret ? "" : header,
    ...(removeSecret ? { secret: "" } : secret ? { secret } : {}),
  };
  const mutation = useMutation({
    mutationFn: () => current
      ? api.updateAlert(current.id, body, current.revision)
      : api.createAlert(body),
    onSettled: () => setSecret(""),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["alerts"] });
      onClose();
    },
  });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (name && url && (current || !header || secret || removeSecret)) mutation.mutate();
  };
  return (
    <Modal title={current ? "编辑 Alert Webhook" : "创建 Alert Webhook"} onClose={onClose}>
      <form onSubmit={submit} autoComplete="off">
        <Field label="名称"><input autoFocus value={name} onChange={(event) => setName(event.target.value)} /></Field>
        <Field label="HTTPS Webhook URL" hint="运行时禁用代理和跳转，并校验所有 DNS 结果">
          <input inputMode="url" value={url} onChange={(event) => setURL(event.target.value)} placeholder="https://hooks.example.com/heimdall" />
        </Field>
        <Field label="Secret Header">
          <select value={header} disabled={removeSecret} onChange={(event) => setHeader(event.target.value)}>
            <option value="authorization">Authorization</option>
            <option value="x-webhook-token">X-Webhook-Token</option>
          </select>
        </Field>
        <Field
          label={current?.secret_configured ? "新 Secret（留空保持不变）" : "Webhook Secret（可选）"}
          hint="Secret 绑定 Webhook URL 和 Header audience，永不回显"
        >
          <input
            type="password"
            autoComplete="new-password"
            disabled={removeSecret}
            value={secret}
            onChange={(event) => setSecret(event.target.value)}
          />
        </Field>
        {current?.secret_configured && (
          <label className="check-row danger-check">
            <input type="checkbox" checked={removeSecret} onChange={(event) => setRemoveSecret(event.target.checked)} />
            <span>明确移除已配置的 Secret</span>
          </label>
        )}
        <label className="check-row">
          <input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />
          <span>保存后立即启用</span>
        </label>
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions">
          <button type="button" className="button ghost" onClick={onClose}>取消</button>
          <button className="button primary" disabled={mutation.isPending}>保存 Webhook</button>
        </div>
      </form>
    </Modal>
  );
}
