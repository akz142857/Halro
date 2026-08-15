import { cloneElement, Component, isValidElement, useEffect, useId, useRef, useState, type ReactElement, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { ApiError } from "./api";
import { useTranslation } from "react-i18next";
import { useIsReadOnly, useSession } from "./session";
import { errorDetail, localizedError } from "./i18n/errors";

export function PageHeader({
  eyebrow,
  title,
  description,
  action,
}: {
  eyebrow: string;
  title: string;
  description: string;
  action?: ReactNode;
}) {
  return (
    <header className="page-header">
      <div>
        <p className="eyebrow">{eyebrow}</p>
        <h1>{title}</h1>
        <p className="page-description">{description}</p>
      </div>
      {action && <div className="page-action">{action}</div>}
    </header>
  );
}

// The dot carries state through colour alone, which neither a screen reader nor a
// colour-blind reader can resolve. Callers pass `label` to add the text equivalent.
export function StatusDot({ ok = true, label }: { ok?: boolean; label?: string }) {
  return (
    <>
      <span className={`status-dot ${ok ? "ok" : "bad"}`} aria-hidden="true" />
      {label && <span className="sr-only">{label}</span>}
    </>
  );
}

export type InlineTestState = "idle" | "running" | "success" | "failure" | "stale";

export function InlineTestControl({ state, latency, onTest, disabled = false, title }: { state: InlineTestState; latency?: number; onTest: () => void; disabled?: boolean; title?: string }) {
  const { t } = useTranslation();
  const statusID = useId();
  // Without a measured latency the interpolated label would render "{{latency}}ms".
  const status = state === "success"
    ? latency === undefined ? t("testControl.successPlain") : t("testControl.success", { latency })
    : t(`testControl.${state}`);
  return (
    <div className="inline-test-control" aria-busy={state === "running"} title={title}>
      <button className="button secondary" disabled={disabled || state === "running"} aria-describedby={statusID} onClick={onTest}>{t("common.test")}</button>
      <span id={statusID} className={`inline-test-result ${state}`} role="status" aria-live="polite" aria-atomic="true"><span aria-hidden="true" />{status}</span>
    </div>
  );
}

// What a failed connection test answered, as far as it can be shown. A red
// "failed" with nothing beside it sent the operator to logs that had nothing
// either, so the class is turned into a sentence and the upstream's own status,
// code and message follow it when the provider supplied them.
export function useTestFailureReason(error: unknown, persistedErrorClass?: string) {
  const { t } = useTranslation();
  const payload = error instanceof ApiError
    ? error.payload as { error_class?: string; provider_status?: number; provider_code?: string; error_detail?: string; error?: string; code?: string } | undefined
    : undefined;
  // The class this response carried, kept apart from the one the store
  // remembers: a stale class from an older test must not describe a refusal
  // this request produced.
  const responseClass = payload?.error_class || "";
  const errorClass = responseClass || persistedErrorClass || "";
  // A refusal Halro made before probing answers with a plain `error` message and
  // never reaches the provider, so it is the whole explanation rather than a
  // detail beside one — and it was previously dropped, leaving the operator with
  // a class and no sentence. When that refusal carries a code, the class is the
  // better explanation: "provider binding adapter is unavailable" names the
  // symptom in English and leaves the reader nowhere to go, while the class says
  // which record to open. Codes with no wording yet fall back to the message.
  const refusal = payload?.code
    ? t(`testControl.refusals.${payload.code}`, { defaultValue: "" })
    : "";
  const detail = refusal || payload?.error_detail || payload?.error || "";
  if (!errorClass && !detail) return "";
  // Halro's own refusal, not the upstream's: either it classified the request as
  // bad before sending it, or it answered with a message and no class at all.
  // Saying "the upstream rejected this probe" there sends the operator to audit
  // a key and a network that were never involved.
  const local = !payload?.provider_status && (responseClass === "bad_request" || (!responseClass && !!payload?.error));
  const reasonKey = local ? "bad_request_local" : errorClass || "unknown";
  const parts = [t(`testControl.reasons.${reasonKey}`, { defaultValue: t("testControl.reasons.unknown") })];
  if (payload?.provider_status) parts.push(`HTTP ${payload.provider_status}`);
  if (payload?.provider_code) parts.push(payload.provider_code);
  if (detail) parts.push(detail);
  return parts.join(" · ");
}

export function EmptyState({
  title,
  children,
  action,
}: {
  title: string;
  children: ReactNode;
  action?: ReactNode;
}) {
  return (
    <div className="empty-state">
      <div className="empty-mark" aria-hidden="true">H</div>
      <h2>{title}</h2>
      <p>{children}</p>
      {action}
    </div>
  );
}

export function ErrorState({ error, className = "", action }: { error: unknown; className?: string; action?: ReactNode }) {
  const { t } = useTranslation();
  const message = error instanceof ApiError ? localizedError(t, error) : t("common.dataUnavailable");
  const detail = errorDetail(error);
  const replayAction = idempotencyReplayAction(error);
  const renderedAction = action ?? (replayAction
    ? <a className="button ghost" href={replayAction}>{t("errors.viewIdempotencyReplay")}</a>
    : undefined);
  const content = <>
    <strong>{t("common.requestFailed")}</strong>
    <span>{message}</span>
    {detail && <small className="notice-detail">{detail}</small>}
  </>;
  return (
    <div className={`notice error${renderedAction ? " has-action" : ""}${className ? ` ${className}` : ""}`} role="alert">
      {renderedAction ? <div className="notice-copy">{content}</div> : content}
      {renderedAction && <div className="notice-action">{renderedAction}</div>}
    </div>
  );
}

function idempotencyReplayAction(error: unknown) {
  if (!(error instanceof ApiError) || !error.code.endsWith("_idempotency_replay")) return "";
  const payload = error.payload as { id?: unknown; project_id?: unknown } | undefined;
  const id = typeof payload?.id === "string" ? payload.id : "";
  if (!id) return "";
  const anchor = encodeURIComponent(id);
  switch (error.code) {
    case "provider_idempotency_replay": return `/admin/providers#provider-${anchor}`;
    case "deployment_idempotency_replay": return `/admin/deployments#deployment-${anchor}`;
    case "route_idempotency_replay": return `/admin/routes#route-${anchor}`;
    case "project_idempotency_replay": return `/admin/projects?project_id=${encodeURIComponent(id)}`;
    case "gateway_key_idempotency_replay": {
      const projectID = typeof payload?.project_id === "string" ? payload.project_id : "";
      return projectID
        ? `/admin/projects?project_id=${encodeURIComponent(projectID)}#gateway-key-${anchor}`
        : "/admin/projects";
    }
    default: return "";
  }
}

interface CrashLabels {
  title: string;
  description: string;
  details: string;
  retry: string;
  reload: string;
}

class CrashBoundary extends Component<{ labels: CrashLabels; children: ReactNode }, { error: Error | null }> {
  state: { error: Error | null } = { error: null };

  static getDerivedStateFromError(error: Error) {
    return { error };
  }

  render() {
    const { error } = this.state;
    if (!error) return this.props.children;
    const { labels } = this.props;
    return (
      <section className="crash-panel" role="alert">
        <strong>{labels.title}</strong>
        <p>{labels.description}</p>
        <details>
          <summary>{labels.details}</summary>
          <pre>{error.stack || `${error.name}: ${error.message}`}</pre>
        </details>
        <div className="row-actions">
          <button className="button ghost" onClick={() => this.setState({ error: null })}>{labels.retry}</button>
          <button className="button primary" onClick={() => window.location.reload()}>{labels.reload}</button>
        </div>
      </section>
    );
  }
}

// A render error used to unmount the whole console and leave a blank page. Keep the
// surrounding chrome alive and show what threw, so the failure stays reportable.
export function ErrorBoundary({ children }: { children: ReactNode }) {
  const { t } = useTranslation();
  return (
    <CrashBoundary
      labels={{
        title: t("errors.crashTitle"),
        description: t("errors.crashDescription"),
        details: t("errors.crashDetails"),
        retry: t("errors.crashRetry"),
        reload: t("errors.crashReload"),
      }}
    >
      {children}
    </CrashBoundary>
  );
}

export function Loading({ label }: { label?: string }) {
  const { t } = useTranslation();
  return (
    <div className="loading" role="status">
      <span className="loading-bar" />
      <span>{label || t("common.loading")}</span>
    </div>
  );
}

// Compares the live field values against whatever they were on first render. A form then
// declares dirtiness by listing its fields once, rather than restating every default a
// second time — two copies of the same knowledge is exactly what drifts apart.
export function useDirty(values: Record<string, unknown>): boolean {
  const initial = useRef(values);
  return Object.keys(values).some((key) => values[key] !== initial.current[key]);
}

export function Modal({
  title,
  children,
  onClose,
  dangerous = false,
  closeDisabled = false,
  wide = false,
  describedBy,
  dirty = false,
}: {
  title: string;
  children: ReactNode;
  onClose: () => void;
  dangerous?: boolean;
  closeDisabled?: boolean;
  wide?: boolean;
  describedBy?: string;
  dirty?: boolean;
}) {
  const { t } = useTranslation();
  const titleID = useId();
  const dialog = useRef<HTMLElement>(null);
  const onCloseRef = useRef(onClose);
  const closeDisabledRef = useRef(closeDisabled);
  // Escape, the backdrop and × all mean "close". A modal that declares itself dirty
  // turns each of them into a question first, so half-filled forms are never
  // discarded without a word. The children stay mounted behind the prompt: cancelling
  // has to give the operator back every field exactly as they left it.
  const [confirmingDiscard, setConfirmingDiscard] = useState(false);
  const confirmingRef = useRef(false);
  const requestClose = () => {
    if (closeDisabled) return;
    if (confirmingDiscard) return setConfirmingDiscard(false);
    if (dirty) return setConfirmingDiscard(true);
    onClose();
  };
  const requestCloseRef = useRef(requestClose);
  useEffect(() => { onCloseRef.current = onClose; }, [onClose]);
  useEffect(() => { closeDisabledRef.current = closeDisabled; }, [closeDisabled]);
  useEffect(() => { requestCloseRef.current = requestClose; });
  useEffect(() => { confirmingRef.current = confirmingDiscard; }, [confirmingDiscard]);
  const initialRender = useRef(true);
  useEffect(() => {
    if (initialRender.current) { initialRender.current = false; return; }
    const container = dialog.current;
    if (!container) return;
    // Opening the prompt moves focus onto it; cancelling puts it back in the form.
    const selector = confirmingDiscard ? "[data-discard-initial]" : "[data-modal-initial]";
    container.querySelector<HTMLElement>(selector)?.focus();
  }, [confirmingDiscard]);
  useEffect(() => {
    const previouslyFocused = document.activeElement as HTMLElement | null;
    const container = dialog.current;
    // React applies autoFocus during commit, before this effect. Taking the focus back
    // to the container would strand the caret outside the field the form asked for.
    const initial = container?.querySelector<HTMLElement>("[data-modal-initial]");
    if (initial) initial.focus();
    else if (container && !container.contains(document.activeElement)) container.focus();
    const onKeyDown = (event: KeyboardEvent) => {
      // Escape means "cancel", never "confirm", so a dangerous dialog honours it too.
      // Only a modal that must not be dismissed at all (closeDisabled) ignores it.
      if (event.key === "Escape" && !closeDisabledRef.current) requestCloseRef.current();
      if (event.key !== "Tab" || !container) return;
      // While the discard prompt is up the form behind it is inert, so the trap
      // narrows to the prompt rather than tabbing through fields nobody can see.
      const scope = (confirmingRef.current && container.querySelector<HTMLElement>(".discard-prompt")) || container;
      const focusable = Array.from(scope.querySelectorAll<HTMLElement>('button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled), a[href], [tabindex]:not([tabindex="-1"])')).filter((element) => !element.hasAttribute("hidden"));
      if (!focusable.length) { event.preventDefault(); container.focus(); return; }
      const first = focusable[0];
      const last = focusable[focusable.length - 1];
      if (event.shiftKey && document.activeElement === first) { event.preventDefault(); last.focus(); }
      else if (!event.shiftKey && document.activeElement === last) { event.preventDefault(); first.focus(); }
    };
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("keydown", onKeyDown);
      previouslyFocused?.focus();
    };
  }, []);
  return createPortal(
    <div className="modal-backdrop" role="presentation" onMouseDown={() => { if (!dangerous && !closeDisabled) requestClose(); }}>
      <section
        className={`modal ${dangerous ? "dangerous" : ""} ${wide ? "wide" : ""} ${confirmingDiscard ? "discarding" : ""}`}
        role={dangerous ? "alertdialog" : "dialog"}
        aria-modal="true"
        aria-labelledby={titleID}
        aria-describedby={describedBy}
        tabIndex={-1}
        ref={dialog}
        onMouseDown={(event) => event.stopPropagation()}
        // A form's own Cancel button lives inside children, below this component, so it
        // cannot reach the dirty guard through props. Marking it data-modal-close routes
        // it through the same question Escape and the backdrop ask.
        onClick={(event) => { if ((event.target as HTMLElement).closest?.("[data-modal-close]")) requestClose(); }}
      >
        <header>
          <h2 id={titleID}>{title}</h2>
          <button className="icon-button" disabled={closeDisabled} onClick={requestClose} aria-label={t("common.close")}>×</button>
        </header>
        {confirmingDiscard && (
          <div className="confirmation-dialog discard-prompt" role="alert">
            <p>{t("common.discardChangesPrompt")}</p>
            <div className="form-actions">
              <button type="button" className="button ghost" data-discard-initial onClick={() => setConfirmingDiscard(false)}>{t("common.keepEditing")}</button>
              <button type="button" className="button danger" onClick={() => { setConfirmingDiscard(false); onClose(); }}>{t("common.discardChanges")}</button>
            </div>
          </div>
        )}
        {children}
      </section>
    </div>,
    document.body,
  );
}

export function ConfirmButton({
  label,
  confirmLabel,
  title,
  className = "button danger",
  onConfirm,
  disabled,
  disabledReason,
  requireStepUp = false,
}: {
  label: string;
  confirmLabel: string;
  title?: string;
  className?: string;
  // Returning a promise keeps the dialog open until the action actually
  // succeeds. Closing on click meant a rejected step-up left no dialog, no
  // typed credentials, and — on pages that do not render the mutation error —
  // no sign anything had happened at all.
  onConfirm: (reauth: ReauthValues) => void | Promise<unknown>;
  disabled?: boolean;
  disabledReason?: string;
  // Actions the server step-up gates ask for the credentials here, in the same
  // dialog that states the consequence — an operator confirms and proves who
  // they are in one step rather than being sent to a second prompt.
  requireStepUp?: boolean;
}) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const consequenceID = useId();
  const reasonID = useId();
  // Every destructive action in the console routes through this button, which
  // makes it the one place a read-only session has to be honoured rather than
  // twenty. The server refuses these calls regardless; this only stops offering
  // them.
  const readOnly = useIsReadOnly();
  const unavailable = disabled || readOnly;
  const [reauth, setReauth] = useState<ReauthValues>({ currentPassword: "", totpCode: "" });
  const [pending, setPending] = useState(false);
  const [failure, setFailure] = useState<unknown>(null);
  // Cleared on the way out rather than left in state: the password must not
  // survive a dialog the operator closed.
  const close = () => { setReauth({ currentPassword: "", totpCode: "" }); setFailure(null); setPending(false); setOpen(false); };
  const submit = async () => {
    setFailure(null);
    setPending(true);
    try {
      await onConfirm(reauth);
      close();
    } catch (error) {
      // The password stays typed; only the code is dropped, because a TOTP
      // step is consumed once and the next attempt needs a fresh one. Leaving
      // the spent code in the field invites retrying it and reading the second
      // refusal as a wrong password.
      setReauth((values) => ({ ...values, totpCode: "" }));
      setFailure(error);
      setPending(false);
    }
  };
  const reason = readOnly ? t("navigation.readOnlyAction") : disabledReason;
  // A disabled button carries no tooltip in some browsers and is skipped by screen
  // reader tab order, so the reason is also stated in the accessibility tree.
  const blocked = Boolean(unavailable && reason);
  return (
    <>
      <button className={className} disabled={unavailable} title={blocked ? reason : undefined} aria-describedby={blocked ? reasonID : undefined} onClick={() => setOpen(true)}>{label}</button>
      {blocked && <span id={reasonID} className="sr-only">{reason}</span>}
      {open && (
        <Modal
          dangerous
          title={title || t("common.confirmAction")}
          describedBy={consequenceID}
          dirty={Boolean(reauth.currentPassword)}
          onClose={close}
        >
          <form className="confirmation-dialog" onSubmit={(event) => { event.preventDefault(); void submit(); }}>
            <p id={consequenceID}>{confirmLabel}</p>
            {requireStepUp && <ReauthFields values={reauth} onChange={setReauth} description={t("auth.stepUpDestructive")} />}
            {Boolean(failure) && <ErrorState error={failure} />}
            {Boolean(failure) && requireStepUp && <p className="form-note">{t("auth.stepUpRetryNeedsNewCode")}</p>}
            <div className="form-actions">
              <button type="button" className="button ghost" data-modal-initial disabled={pending} onClick={close}>{t("common.cancel")}</button>
              <button
                type="button"
                className="button danger"
                disabled={pending || (requireStepUp && !reauth.currentPassword)}
                onClick={submit}
              >{pending ? t("common.working") : label}</button>
            </div>
          </form>
        </Modal>
      )}
    </>
  );
}

// A real button, so keyboard and assistive-tech users can page without scrolling, plus an
// observer that pages automatically once it scrolls into view.
export function LoadMore({ label, busy, onLoad }: { label: string; busy: boolean; onLoad: () => void }) {
  const trigger = useRef<HTMLButtonElement>(null);
  const onLoadRef = useRef(onLoad);
  useEffect(() => { onLoadRef.current = onLoad; }, [onLoad]);
  useEffect(() => {
    const element = trigger.current;
    // Absent in jsdom and older browsers; the button alone still pages the list.
    if (!element || typeof IntersectionObserver === "undefined") return;
    const observer = new IntersectionObserver(
      (entries) => { if (entries.some((entry) => entry.isIntersecting)) onLoadRef.current(); },
      { rootMargin: "200px" },
    );
    observer.observe(element);
    return () => observer.disconnect();
  }, []);
  return (
    <button ref={trigger} className="load-more" disabled={busy} onClick={() => onLoadRef.current()}>
      {label}
    </button>
  );
}

export type ResourceStatusFilter = "all" | "enabled" | "disabled";

// Shared by every list that can outgrow one screen. Kept here rather than per page so a
// filter bar means the same thing wherever the operator meets one.
export function ResourceToolbar({
  query,
  onQueryChange,
  queryPlaceholder,
  count,
  status,
  onStatusChange,
}: {
  query: string;
  onQueryChange: (value: string) => void;
  queryPlaceholder: string;
  count: string;
  status?: ResourceStatusFilter;
  onStatusChange?: (value: ResourceStatusFilter) => void;
}) {
  const { t } = useTranslation();
  return (
    <div className="resource-toolbar" role="search" aria-label={t("common.filters")}>
      <label className="resource-search"><span>{t("common.search")}</span><input type="search" autoComplete="off" value={query} onChange={(event) => onQueryChange(event.target.value)} placeholder={queryPlaceholder} /></label>
      {status !== undefined && onStatusChange && <label><span>{t("common.status")}</span><select value={status} onChange={(event) => onStatusChange(event.target.value as ResourceStatusFilter)}><option value="all">{t("common.allStatuses")}</option><option value="enabled">{t("common.enabled")}</option><option value="disabled">{t("common.disabled")}</option></select></label>}
      <span className="resource-result-count" role="status">{count}</span>
    </div>
  );
}

export function OverflowMenu({ label, children }: { label: string; children: ReactNode }) {
  const details = useRef<HTMLDetailsElement>(null);
  useEffect(() => {
    const close = (restoreFocus = false) => {
      const menu = details.current;
      if (!menu?.open) return;
      menu.open = false;
      if (restoreFocus) menu.querySelector<HTMLElement>("summary")?.focus();
    };
    const onPointerDown = (event: PointerEvent) => {
      if (!details.current?.contains(event.target as Node)) close();
    };
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") close(true);
    };
    document.addEventListener("pointerdown", onPointerDown);
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("pointerdown", onPointerDown);
      document.removeEventListener("keydown", onKeyDown);
    };
  }, []);
  return (
    <details className="row-overflow" ref={details}>
      <summary aria-label={label}>•••</summary>
      <div className="row-overflow-menu">{children}</div>
    </details>
  );
}

export function Field({
  label,
  hint,
  error,
  children,
}: {
  label: string;
  hint?: string;
  error?: string;
  children: ReactNode;
}) {
  const inputID = useId();
  const descriptionID = useId();
  const childProps = isValidElement(children) ? children.props as Record<string, unknown> : {};
  const controlID = typeof childProps.id === "string" ? childProps.id : inputID;
  const existingDescription = typeof childProps["aria-describedby"] === "string" ? childProps["aria-describedby"] : "";
  const describedBy = [existingDescription, hint || error ? descriptionID : ""].filter(Boolean).join(" ") || undefined;
  const control = isValidElement(children)
    ? cloneElement(children as ReactElement<Record<string, unknown>>, {
        id: controlID,
        "aria-describedby": describedBy,
        "aria-invalid": error ? true : undefined,
      })
    : children;
  return (
    <label className="field" htmlFor={controlID}>
      <span>{label}</span>
      {control}
      {hint && !error && <small id={descriptionID}>{hint}</small>}
      {error && <small id={descriptionID} className="field-error">{error}</small>}
    </label>
  );
}

export interface ReauthValues {
  currentPassword: string;
  totpCode: string;
}

// Step-up re-authentication: the caller resupplies their own password and a
// fresh TOTP code with the request itself, rather than being handed a
// short-lived elevated session. Every destructive Admin action that asks for it
// asks with these same two fields, so the operator learns the shape once.
//
// The TOTP field is optional in the markup because the server only demands a
// code from an account that actually has an authenticator enrolled; requiring
// it here would lock out an instance where MFA is still optional.
export function ReauthFields({
  values,
  onChange,
  description,
}: {
  values: ReauthValues;
  onChange: (values: ReauthValues) => void;
  description?: string;
}) {
  const { t } = useTranslation();
  const username = useSession()?.username ?? "";
  // A fragment, not a wrapper: these are two ordinary fields and they belong to
  // the surrounding form's grid. Nesting them one level deeper takes them out
  // of its gap and lines them up with nothing.
  return (
    <>
      {/* A password field with no username beside it makes the browser look
          further out for one, and it will fill whatever text input it finds —
          on a list page that is the filter box, which then silently filters the
          list to nothing. Naming the account here keeps that search inside this
          form. Hidden from assistive tech and from the tab order: it exists for
          the password manager, and the operator already knows who they are. */}
      <input
        type="text"
        name="username"
        autoComplete="username"
        value={username}
        readOnly
        tabIndex={-1}
        aria-hidden="true"
        className="sr-only"
        onChange={() => {}}
      />
      {/* Why the form is asking sits under the field that answers it, as every
          other hint in the console does. Standing above the pair it read as a
          note about the section before it, and it now describes the password
          input to assistive tech rather than floating unattached. */}
      <Field label={t("auth.currentPassword")} hint={description}>
        <input
          required
          type="password"
          autoComplete="current-password"
          value={values.currentPassword}
          onChange={(event) => onChange({ ...values, currentPassword: event.target.value })}
        />
      </Field>
      {/* The qualifier goes in the hint, where every other field in the console
          puts one. Folding it into the label made this the only label on the
          form carrying its own explanation. */}
      <Field label={t("auth.authenticatorCode")} hint={t("auth.authenticatorCodeWhenEnabled")}>
        <input
          inputMode="numeric"
          pattern="[0-9]*"
          maxLength={8}
          autoComplete="one-time-code"
          value={values.totpCode}
          onChange={(event) => onChange({ ...values, totpCode: event.target.value })}
        />
      </Field>
    </>
  );
}
