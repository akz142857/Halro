import { cloneElement, isValidElement, useEffect, useId, useRef, useState, type ReactElement, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { ApiError } from "./api";
import { useTranslation } from "react-i18next";
import { localizedError } from "./i18n/errors";

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

export function StatusDot({ ok = true }: { ok?: boolean }) {
  return <span className={`status-dot ${ok ? "ok" : "bad"}`} aria-hidden="true" />;
}

export type InlineTestState = "idle" | "running" | "success" | "failure" | "stale";

export function InlineTestControl({ state, latency, onTest, disabled = false, title }: { state: InlineTestState; latency?: number; onTest: () => void; disabled?: boolean; title?: string }) {
  const { t } = useTranslation();
  const statusID = useId();
  const status = state === "success" && latency !== undefined
    ? t("testControl.success", { latency })
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

export function ErrorState({ error }: { error: unknown }) {
  const { t } = useTranslation();
  const message = error instanceof ApiError ? localizedError(t, error) : t("common.dataUnavailable");
  return (
    <div className="notice error" role="alert">
      <strong>{t("common.requestFailed")}</strong>
      <span>{message}</span>
    </div>
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
}: {
  title: string;
  children: ReactNode;
  onClose: () => void;
  dangerous?: boolean;
  closeDisabled?: boolean;
  wide?: boolean;
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
    (container?.querySelector<HTMLElement>("[data-modal-initial]") || container)?.focus();
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape" && !dangerous && !closeDisabledRef.current) onCloseRef.current();
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
  return (
    <>
      <button className={className} disabled={disabled} onClick={() => setOpen(true)}>{label}</button>
      {open && (
        <Modal dangerous title={title || t("common.confirmAction")} onClose={() => setOpen(false)}>
          <div className="confirmation-dialog">
            <p>{confirmLabel}</p>
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
