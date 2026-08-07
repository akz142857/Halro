import { useInfiniteQuery, useMutation, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState, type FormEvent } from "react";
import { api } from "../api";
import {
  ConfirmButton,
  EmptyState,
  ErrorState,
  Field,
  InlineTestControl,
  Loading,
  LoadMore,
  Modal,
  PageHeader,
  StatusDot,
  useDirty,
  type InlineTestState,
  type ReauthValues,
} from "../components";
import { useInstantFormatter } from "../format";
import type { AlertWebhook } from "../types";
import { useTranslation } from "react-i18next";
import { useIsReadOnly } from "../session";

const PAGE_SIZE = "100";

function pageQuery(cursor: string, limit = PAGE_SIZE) {
  return `?${new URLSearchParams({ limit, ...(cursor ? { cursor } : {}) })}`;
}

// A failed action is not a neutral fact on this page: a rejected or errored event is the
// reason someone opened the audit trail in the first place.
function outcomeTone(outcome: string) {
  if (outcome === "success") return "success";
  if (outcome === "failure" || outcome === "provider_error") return "danger";
  return "warning";
}

export function OperationsPage() {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const dateTime = useInstantFormatter();
  const [editing, setEditing] = useState<AlertWebhook | "new" | null>(null);
  const queryClient = useQueryClient();
  const audit = useInfiniteQuery({
    queryKey: ["audit"], initialPageParam: "",
    queryFn: ({ pageParam }) => api.audit(pageQuery(pageParam)),
    getNextPageParam: (page) => page.next_cursor || undefined,
  });
  const auditRecords = audit.data?.pages.flatMap((page) => page.items) ?? [];
  // The server truncates an unpaged list at 50. A webhook past that point cannot be
  // edited, disabled or deleted from anywhere in the console.
  const alerts = useInfiniteQuery({
    queryKey: ["alerts", "paged"], initialPageParam: "",
    queryFn: ({ pageParam }) => api.alertsPage(pageQuery(pageParam)),
    getNextPageParam: (page) => page.next_cursor || undefined,
  });
  const webhooks = useMemo(
    () => alerts.data?.pages.flatMap((page) => page.items) ?? [],
    [alerts.data?.pages],
  );
  // Everything on this page is itself an audited action, so the trail below has to reflect
  // what the operator just did without a manual reload.
  const refreshAll = () => {
    queryClient.invalidateQueries({ queryKey: ["alerts"] });
    queryClient.invalidateQueries({ queryKey: ["audit"] });
  };
  return (
    <>
      <PageHeader
        eyebrow={t("operations.eyebrow")}
        title={t("operations.title")}
        description={t("operations.description")}
        action={<button className="button primary" disabled={readOnly} onClick={() => setEditing("new")}>{t("operations.addWebhook")}</button>}
      />
      <section className="panel operations-alerts">
        <header className="panel-header operations-panel-header">
          <div><p className="eyebrow">{t("operations.outbound")}</p><h2>{t("operations.webhooks")}</h2></div>
          <span className="count">{alerts.hasNextPage ? `${webhooks.length}+` : webhooks.length}</span>
        </header>
        {alerts.isPending && <Loading />}
        {alerts.isError && <ErrorState error={alerts.error} />}
        {alerts.isSuccess && webhooks.length === 0 && (
          <EmptyState title={t("operations.emptyTitle")}>{t("operations.emptyDescription")}</EmptyState>
        )}
        {webhooks.map((webhook) => (
          <AlertRow key={webhook.id} webhook={webhook} onEdit={() => setEditing(webhook)} onMutated={refreshAll} />
        ))}
        {alerts.hasNextPage && (
          <LoadMore
            label={alerts.isFetchingNextPage ? t("common.loadingMore") : t("common.loadMore")}
            busy={alerts.isFetchingNextPage}
            onLoad={alerts.fetchNextPage}
          />
        )}
      </section>
      <section className="panel audit-panel">
        <header className="panel-header operations-panel-header">
          <div><p className="eyebrow">{t("operations.recentAudit")}</p><h2>{t("operations.auditTrail")}</h2></div>
          {/* Every replay re-verifies the HMAC chain server-side and fails the request on a
              break, so the query's own state is the honest signal. A hardcoded green pill
              would keep claiming the chain is intact while the panel below shows an error. */}
          <span className={`health-pill${audit.isSuccess ? "" : " warning"}`}>
            <StatusDot
              ok={audit.isSuccess}
              label={audit.isSuccess ? t("operations.chainActive") : t("operations.chainUnverified")}
            />
            {audit.isSuccess ? t("operations.chainActive") : t("operations.chainUnverified")}
          </span>
        </header>
        {audit.isPending && <Loading />}
        {audit.isError && <ErrorState error={audit.error} />}
        {audit.isSuccess && auditRecords.length === 0 && (
          <EmptyState title={t("operations.auditEmptyTitle")}>{t("operations.auditEmptyDescription")}</EmptyState>
        )}
        {auditRecords.length > 0 && (
          <div className="timeline" role="list" aria-label={t("operations.auditTrail")}>
            {auditRecords.map((record) => (
              <article key={record.sequence} role="listitem">
                <span className="timeline-sequence">#{record.sequence}</span>
                <span className={`timeline-mark ${outcomeTone(record.outcome)}`} />
                <div>
                  <strong>{record.action}</strong>
                  <p>{record.actor_id || record.actor_type} → {record.target_type} {record.target_id}</p>
                  <small>
                    {dateTime(record.occurred_at)} ·{" "}
                    <span className={`audit-outcome ${outcomeTone(record.outcome)}`}>{record.outcome}</span>
                    {record.reason_code ? ` · ${record.reason_code}` : ""}
                  </small>
                </div>
              </article>
            ))}
          </div>
        )}
        {audit.hasNextPage && (
          <LoadMore
            label={audit.isFetchingNextPage ? t("common.loadingMore") : t("common.loadMore")}
            busy={audit.isFetchingNextPage}
            onLoad={audit.fetchNextPage}
          />
        )}
      </section>
      {editing && (
        <AlertForm
          current={editing === "new" ? undefined : editing}
          onSaved={refreshAll}
          onClose={() => setEditing(null)}
        />
      )}
    </>
  );
}

function AlertRow({
  webhook,
  onEdit,
  onMutated,
}: {
  webhook: AlertWebhook;
  onEdit: () => void;
  onMutated: () => void;
}) {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  // Per row, not per page: a page-wide mutation greys out every other row's buttons and
  // leaves one shared "delivered" notice that never says which endpoint it belongs to.
  const test = useMutation({ mutationFn: () => api.testAlert(webhook.id), onSuccess: onMutated, onError: onMutated });
  const remove = useMutation({ mutationFn: (reauth: ReauthValues) => api.deleteAlert(webhook.id, webhook.revision, reauth), onSuccess: onMutated });
  const toggle = useMutation({
    mutationFn: () => api.updateAlert(
      webhook.id,
      { name: webhook.name, url: webhook.url, header_name: webhook.header_name ?? "", enabled: !webhook.enabled },
      webhook.revision,
    ),
    onSuccess: onMutated,
  });
  const testState: InlineTestState = test.isPending
    ? "running"
    : test.isSuccess ? "success" : test.isError ? "failure" : "idle";
  return (
    <>
      <div className="alert-row">
        <span className="provider-icon">WH</span>
        <div>
          <span>
            <StatusDot ok={webhook.enabled} label={webhook.enabled ? t("common.enabled") : t("common.disabled")} />
            <strong>{webhook.name}</strong>
          </span>
          <small>{webhook.url}</small>
          <code>{webhook.id}</code>
        </div>
        <div className="alert-secret">
          <small>{t("operations.secretHeader")}</small>
          {/* Same typography as the rest of the row; colour alone separates the two states.
              Carrying no secret is a configuration fact, not a fault, so it stays neutral. */}
          <strong className={webhook.secret_configured ? "configured" : ""}>
            {webhook.secret_configured ? webhook.header_name || t("operations.configured") : t("operations.none")}
          </strong>
        </div>
        <div className="row-actions">
          <InlineTestControl
            state={testState}
            latency={test.data?.latency_ms}
            disabled={!webhook.enabled || test.isPending}
            title={webhook.enabled ? undefined : t("operations.testDisabledHint")}
            onTest={() => test.mutate()}
          />
          {webhook.enabled ? (
            <ConfirmButton
              className="button ghost"
              label={t("common.disable")}
              title={t("operations.disableTitle")}
              confirmLabel={t("operations.disableConfirm", { name: webhook.name })}
              disabled={toggle.isPending}
              onConfirm={() => toggle.mutateAsync()}
            />
          ) : (
            <button className="button ghost" disabled={toggle.isPending} onClick={() => toggle.mutate()}>
              {toggle.isPending ? t("common.working") : t("common.enable")}
            </button>
          )}
          <button className="button ghost" disabled={readOnly} onClick={onEdit}>{t("common.edit")}</button>
          <ConfirmButton
            label={t("common.delete")}
            confirmLabel={t("operations.deleteConfirm", { name: webhook.name })}
            disabled={remove.isPending}
            requireStepUp
            onConfirm={(reauth) => remove.mutateAsync(reauth)}
          />
        </div>
        {/* Several chat platforms answer 200 and reject the payload in the body. Showing the
            endpoint's own reply is the only way an operator can tell "delivered" from
            "accepted the request and threw it away". It spans the row's own grid so it can
            only ever align with the row it belongs to. */}
        {test.isSuccess && test.data.response && (
          <div className="alert-reply" role="status">
            <small>{t("operations.endpointReply", { status: test.data.status_code })}</small>
            <code>{test.data.response}</code>
          </div>
        )}
      </div>
      {/* A revocation or a disable that failed silently leaves the operator believing an
          endpoint stopped receiving events when it has not. */}
      {remove.isError && <ErrorState error={remove.error} />}
      {toggle.isError && <ErrorState error={toggle.error} />}
      {test.isError && <ErrorState error={test.error} />}
    </>
  );
}

function AlertForm({
  current,
  onSaved,
  onClose,
}: {
  current?: AlertWebhook;
  onSaved: () => void;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const [name, setName] = useState(current?.name ?? "");
  const [url, setURL] = useState(current?.url ?? "");
  const [header, setHeader] = useState(current?.header_name || "authorization");
  const [secret, setSecret] = useState("");
  const [removeSecret, setRemoveSecret] = useState(false);
  const [enabled, setEnabled] = useState(current?.enabled ?? false);
  const [problem, setProblem] = useState("");
  const body = {
    name, url, enabled,
    header_name: removeSecret ? "" : header,
    ...(removeSecret ? { secret: "" } : secret ? { secret } : {}),
  };
  const mutation = useMutation({
    mutationFn: () => current
      ? api.updateAlert(current.id, body, current.revision)
      : api.createAlert(body),
    // Cleared only on success. Wiping it on failure turns the operator's next click from
    // "replace the secret" into "keep the old one" without saying so.
    onSuccess: () => {
      setSecret("");
      onSaved();
      onClose();
    },
  });
  // Re-pointing a webhook that already holds a secret means re-entering it: the server
  // refuses to rebind a secret the operator has never seen to a new destination.
  const destinationChanged = Boolean(
    current?.secret_configured && !removeSecret &&
    (current.url.toLowerCase() !== url.toLowerCase() || (current.header_name ?? "") !== header),
  );
  const dirty = useDirty({ name, url, header, enabled, secret, removeSecret });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (mutation.isPending) return;
    if (!name.trim()) return setProblem(t("operations.nameRequired"));
    if (!/^https:\/\/\S+$/i.test(url.trim())) return setProblem(t("operations.urlInvalid"));
    if (destinationChanged && !secret) return setProblem(t("operations.secretRequiredForNewDestination"));
    setProblem("");
    mutation.mutate();
  };
  return (
    <Modal
      title={current ? t("operations.editWebhook") : t("operations.createWebhook")}
      closeDisabled={mutation.isPending}
      dirty={dirty}
      onClose={onClose}
    >
      <form onSubmit={submit} autoComplete="off">
        <Field label={t("operations.name")}><input data-modal-initial value={name} onChange={(event) => setName(event.target.value)} /></Field>
        <Field label={t("operations.url")} hint={t("operations.urlHint")}>
          <input type="url" inputMode="url" value={url} onChange={(event) => setURL(event.target.value)} placeholder="https://hooks.example.com/heimdall" />
        </Field>
        <Field label={t("operations.header")}>
          <select value={header} disabled={removeSecret} onChange={(event) => setHeader(event.target.value)}>
            <option value="authorization">Authorization</option>
            <option value="x-webhook-token">X-Webhook-Token</option>
          </select>
        </Field>
        <Field
          label={current?.secret_configured ? t("operations.newSecret") : t("operations.optionalSecret")}
          hint={destinationChanged ? t("operations.secretRequiredForNewDestination") : t("operations.secretHint")}
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
        {problem && <div className="notice error" role="alert"><strong>{problem}</strong></div>}
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions">
          <button type="button" className="button ghost" disabled={mutation.isPending} data-modal-close>{t("common.cancel")}</button>
          <button className="button primary" disabled={mutation.isPending}>
            {mutation.isPending ? t("common.working") : t("operations.saveWebhook")}
          </button>
        </div>
      </form>
    </Modal>
  );
}
