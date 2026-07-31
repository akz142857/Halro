import { useEffect, useId, useRef, type ReactNode } from "react";
import { ApiError } from "./api";

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
  const message = error instanceof ApiError ? error.message : "数据暂时不可用";
  return (
    <div className="notice error" role="alert">
      <strong>无法完成请求</strong>
      <span>{message}</span>
    </div>
  );
}

export function Loading({ label = "正在读取网关状态" }: { label?: string }) {
  return (
    <div className="loading" role="status">
      <span className="loading-bar" />
      <span>{label}</span>
    </div>
  );
}

export function Modal({
  title,
  children,
  onClose,
  dangerous = false,
}: {
  title: string;
  children: ReactNode;
  onClose: () => void;
  dangerous?: boolean;
}) {
  const titleID = useId();
  const dialog = useRef<HTMLElement>(null);
  useEffect(() => {
    const previouslyFocused = document.activeElement as HTMLElement | null;
    dialog.current?.focus();
    const onKeyDown = (event: KeyboardEvent) => {
      if (event.key === "Escape") onClose();
    };
    document.addEventListener("keydown", onKeyDown);
    return () => {
      document.removeEventListener("keydown", onKeyDown);
      previouslyFocused?.focus();
    };
  }, [onClose]);
  return (
    <div className="modal-backdrop" role="presentation" onMouseDown={onClose}>
      <section
        className={`modal ${dangerous ? "dangerous" : ""}`}
        role="dialog"
        aria-modal="true"
        aria-labelledby={titleID}
        tabIndex={-1}
        ref={dialog}
        onMouseDown={(event) => event.stopPropagation()}
      >
        <header>
          <h2 id={titleID}>{title}</h2>
          <button className="icon-button" onClick={onClose} aria-label="关闭">×</button>
        </header>
        {children}
      </section>
    </div>
  );
}

export function ConfirmButton({
  label,
  confirmLabel,
  onConfirm,
  disabled,
}: {
  label: string;
  confirmLabel: string;
  onConfirm: () => void;
  disabled?: boolean;
}) {
  return (
    <button
      className="button danger"
      disabled={disabled}
      onClick={() => {
        if (window.confirm(confirmLabel)) onConfirm();
      }}
    >
      {label}
    </button>
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
  return (
    <label className="field">
      <span>{label}</span>
      {children}
      {hint && !error && <small>{hint}</small>}
      {error && <small className="field-error">{error}</small>}
    </label>
  );
}
