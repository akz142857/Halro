import { cloneElement, Component, isValidElement, useEffect, useId, useRef, useState, type ReactElement, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { ApiError } from "./api";
import { useTranslation } from "react-i18next";
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
      <button className="button ghost" disabled={disabled || state === "running"} aria-describedby={statusID} onClick={onTest}>{t("common.test")}</button>
      <span id={statusID} className={`inline-test-result ${state}`} role="status" aria-live="polite" aria-atomic="true"><span aria-hidden="true" />{status}</span>
    </div>
  );
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

export function ErrorState({ error, className = "" }: { error: unknown; className?: string }) {
  const { t } = useTranslation();
  const message = error instanceof ApiError ? localizedError(t, error) : t("common.dataUnavailable");
  const detail = errorDetail(error);
  return (
    <div className={`notice error${className ? ` ${className}` : ""}`} role="alert">
      <strong>{t("common.requestFailed")}</strong>
      <span>{message}</span>
      {detail && <small className="notice-detail">{detail}</small>}
    </div>
  );
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

export function Modal({
  title,
  children,
  onClose,
  dangerous = false,
  closeDisabled = false,
  wide = false,
  describedBy,
}: {
  title: string;
  children: ReactNode;
  onClose: () => void;
  dangerous?: boolean;
  closeDisabled?: boolean;
  wide?: boolean;
  describedBy?: string;
}) {
  const { t } = useTranslation();
  const titleID = useId();
  const dialog = useRef<HTMLElement>(null);
  const onCloseRef = useRef(onClose);
  const closeDisabledRef = useRef(closeDisabled);
  useEffect(() => { onCloseRef.current = onClose; }, [onClose]);
  useEffect(() => { closeDisabledRef.current = closeDisabled; }, [closeDisabled]);
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
      if (event.key === "Escape" && !closeDisabledRef.current) onCloseRef.current();
      if (event.key !== "Tab" || !container) return;
      const focusable = Array.from(container.querySelectorAll<HTMLElement>('button:not(:disabled), input:not(:disabled), select:not(:disabled), textarea:not(:disabled), a[href], [tabindex]:not([tabindex="-1"])')).filter((element) => !element.hasAttribute("hidden"));
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
    <div className="modal-backdrop" role="presentation" onMouseDown={() => { if (!dangerous && !closeDisabled) onClose(); }}>
      <section
        className={`modal ${dangerous ? "dangerous" : ""} ${wide ? "wide" : ""}`}
        role={dangerous ? "alertdialog" : "dialog"}
        aria-modal="true"
        aria-labelledby={titleID}
        aria-describedby={describedBy}
        tabIndex={-1}
        ref={dialog}
        onMouseDown={(event) => event.stopPropagation()}
      >
        <header>
          <h2 id={titleID}>{title}</h2>
          <button className="icon-button" disabled={closeDisabled} onClick={onClose} aria-label={t("common.close")}>×</button>
        </header>
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
}: {
  label: string;
  confirmLabel: string;
  title?: string;
  className?: string;
  onConfirm: () => void;
  disabled?: boolean;
}) {
  const { t } = useTranslation();
  const [open, setOpen] = useState(false);
  const consequenceID = useId();
  return (
    <>
      <button className={className} disabled={disabled} onClick={() => setOpen(true)}>{label}</button>
      {open && (
        <Modal dangerous title={title || t("common.confirmAction")} describedBy={consequenceID} onClose={() => setOpen(false)}>
          <div className="confirmation-dialog">
            <p id={consequenceID}>{confirmLabel}</p>
            <div className="form-actions">
              <button type="button" className="button ghost" data-modal-initial onClick={() => setOpen(false)}>{t("common.cancel")}</button>
              <button type="button" className="button danger" onClick={() => { setOpen(false); onConfirm(); }}>{label}</button>
            </div>
          </div>
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
      <label className="resource-search"><span>{t("common.search")}</span><input type="search" value={query} onChange={(event) => onQueryChange(event.target.value)} placeholder={queryPlaceholder} /></label>
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
