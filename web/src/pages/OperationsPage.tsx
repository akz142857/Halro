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
import { useTranslation } from "react-i18next";

export function OperationsPage() {
  const { t } = useTranslation();
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
        eyebrow={t("operations.eyebrow")}
        title={t("operations.title")}
        description={t("operations.description")}
        action={<button className="button primary" onClick={() => setEditing("new")}>{t("operations.addWebhook")}</button>}
      />
      <section className="panel operations-alerts">
        <header className="panel-header">
          <div><p className="eyebrow">{t("operations.outbound")}</p><h2>{t("operations.webhooks")}</h2></div>
          <span className="count">{alerts.data?.items.length ?? 0}</span>
        </header>
        {alerts.isPending && <Loading />}
        {alerts.isError && <ErrorState error={alerts.error} />}
        {alerts.data?.items.length === 0 && (
          <EmptyState title={t("operations.emptyTitle")}>{t("operations.emptyDescription")}</EmptyState>
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
              <small>{t("operations.secretHeader")}</small>
              <strong>{webhook.secret_configured ? webhook.header_name || t("operations.configured") : t("operations.none")}</strong>
            </div>
            <div className="row-actions">
              <button className="button ghost" disabled={!webhook.enabled || test.isPending} onClick={() => test.mutate(webhook.id)}>
                {t("common.test")}
              </button>
              <button className="button ghost" onClick={() => setEditing(webhook)}>{t("common.edit")}</button>
              <ConfirmButton
                label={t("common.delete")}
                confirmLabel={t("operations.deleteConfirm", { name: webhook.name })}
                disabled={remove.isPending}
                onConfirm={() => remove.mutate(webhook)}
              />
            </div>
          </div>
        ))}
        {test.isSuccess && <div className="notice success"><strong>{t("operations.testDelivered")}</strong></div>}
        {test.isError && <ErrorState error={test.error} />}
      </section>
      <section className="panel audit-panel">
        <header className="panel-header">
          <div><p className="eyebrow">{t("operations.recentAudit")}</p><h2>{t("operations.auditTrail")}</h2></div>
          <span className="health-pill"><StatusDot />{t("operations.chainActive")}</span>
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
  const { t } = useTranslation();
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
    <Modal title={current ? t("operations.editWebhook") : t("operations.createWebhook")} onClose={onClose}>
      <form onSubmit={submit} autoComplete="off">
        <Field label={t("operations.name")}><input autoFocus value={name} onChange={(event) => setName(event.target.value)} /></Field>
        <Field label={t("operations.url")} hint={t("operations.urlHint")}>
          <input inputMode="url" value={url} onChange={(event) => setURL(event.target.value)} placeholder="https://hooks.example.com/heimdall" />
        </Field>
        <Field label={t("operations.header")}>
          <select value={header} disabled={removeSecret} onChange={(event) => setHeader(event.target.value)}>
            <option value="authorization">Authorization</option>
            <option value="x-webhook-token">X-Webhook-Token</option>
          </select>
        </Field>
        <Field
          label={current?.secret_configured ? t("operations.newSecret") : t("operations.optionalSecret")}
          hint={t("operations.secretHint")}
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
            <span>{t("operations.removeSecret")}</span>
          </label>
        )}
        <label className="check-row">
          <input type="checkbox" checked={enabled} onChange={(event) => setEnabled(event.target.checked)} />
          <span>{t("operations.enableNow")}</span>
        </label>
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions">
          <button type="button" className="button ghost" onClick={onClose}>{t("common.cancel")}</button>
          <button className="button primary" disabled={mutation.isPending}>{t("operations.saveWebhook")}</button>
        </div>
      </form>
    </Modal>
  );
}
