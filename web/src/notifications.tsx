import { createContext, useCallback, useContext, useEffect, useMemo, useRef, useState, type ReactNode } from "react";
import { createPortal } from "react-dom";
import { useTranslation } from "react-i18next";

export type NotificationTone = "success" | "error" | "warning" | "info";

export interface NotificationInput {
  tone: NotificationTone;
  title: string;
  description?: string;
  // Milliseconds until the notification removes itself. 0 keeps it until the
  // operator dismisses it, which is the default for errors: a failure that
  // scrolls itself away is the defect this component exists to fix.
  timeout?: number;
}

interface Notification extends NotificationInput {
  id: string;
  // Bumped when the same message arrives again, so a repeated click restarts
  // one notification's timer instead of stacking identical copies.
  repeat: number;
}

interface NotificationAPI {
  notify: (input: NotificationInput) => void;
  dismiss: (id: string) => void;
}

const NotificationContext = createContext<NotificationAPI | undefined>(undefined);

// Beyond this the column covers the page it is reporting on, and the oldest
// message is the one the operator has already read.
const maxVisibleNotifications = 4;

// Overflow evicts the oldest notification that would have left on its own
// anyway. A notification with no timeout is one the operator has to read and
// dismiss — an error nothing else on the page reports — so four later
// confirmations must not be able to carry it off unseen.
function trimToVisible(notifications: Notification[]) {
  if (notifications.length <= maxVisibleNotifications) return notifications;
  const surplus = notifications.length - maxVisibleNotifications;
  const evicted = new Set<string>();
  for (const notification of notifications) {
    if (evicted.size === surplus) break;
    if (effectiveTimeout(notification) > 0) evicted.add(notification.id);
  }
  // Every survivor is undismissable-by-timer: the column is full of failures,
  // and the oldest one has at least been on screen the longest.
  for (const notification of notifications) {
    if (evicted.size === surplus) break;
    evicted.add(notification.id);
  }
  return notifications.filter((notification) => !evicted.has(notification.id));
}

function effectiveTimeout(notification: NotificationInput) {
  return notification.timeout ?? defaultTimeouts[notification.tone];
}

const defaultTimeouts: Record<NotificationTone, number> = {
  success: 6000,
  info: 6000,
  warning: 9000,
  error: 0,
};

export function NotificationProvider({ children }: { children: ReactNode }) {
  const [notifications, setNotifications] = useState<Notification[]>([]);
  const sequence = useRef(0);
  const dismiss = useCallback((id: string) => {
    setNotifications((current) => current.filter((notification) => notification.id !== id));
  }, []);
  const notify = useCallback((input: NotificationInput) => {
    setNotifications((current) => {
      const duplicate = current.find((notification) =>
        notification.tone === input.tone && notification.title === input.title && notification.description === input.description);
      if (duplicate) {
        return current.map((notification) => notification === duplicate
          ? { ...notification, repeat: notification.repeat + 1 }
          : notification);
      }
      sequence.current += 1;
      const next = [...current, { ...input, id: `notification-${sequence.current}`, repeat: 0 }];
      return trimToVisible(next);
    });
  }, []);
  const api = useMemo(() => ({ notify, dismiss }), [notify, dismiss]);
  return (
    <NotificationContext.Provider value={api}>
      {children}
      <NotificationRegion notifications={notifications} onDismiss={dismiss} />
    </NotificationContext.Provider>
  );
}

// Outside a provider the console still has to work — a page rendered in a test
// or in isolation should not crash because it reported a save.
const noopNotifications: NotificationAPI = { notify: () => {}, dismiss: () => {} };

export function useNotify() {
  return useContext(NotificationContext) ?? noopNotifications;
}

function NotificationRegion({ notifications, onDismiss }: { notifications: Notification[]; onDismiss: (id: string) => void }) {
  const { t } = useTranslation();
  // Hover and focus pause the countdown for different reasons and end
  // differently, so they are tracked apart. Dismissing from the keyboard removes
  // the focused button, and a node that leaves the DOM fires no blur — the focus
  // pause it started would never end, and every later notification would stop
  // auto-dismissing. Re-derive it from where focus actually is once the removal
  // has rendered; the hover pause is left alone, since the pointer is still
  // there.
  const [hovered, setHovered] = useState(false);
  const [focused, setFocused] = useState(false);
  const region = useRef<HTMLDivElement>(null);
  useEffect(() => {
    if (!focused) return;
    const element = region.current;
    if (element && !element.contains(document.activeElement)) setFocused(false);
  }, [notifications, focused]);
  const paused = hovered || focused;
  if (typeof document === "undefined") return null;
  // Two regions rather than one per notification: assertive for the failures
  // that interrupt, polite for everything else, so a screen reader is not
  // interrupted by a save confirmation.
  const assertive = notifications.filter((notification) => notification.tone === "error");
  const polite = notifications.filter((notification) => notification.tone !== "error");
  return createPortal(
    <div
      ref={region}
      className="notification-region"
      aria-label={t("notifications.region")}
      onMouseEnter={() => setHovered(true)}
      onMouseLeave={() => setHovered(false)}
      onFocus={() => setFocused(true)}
      onBlur={() => setFocused(false)}
    >
      <div className="notification-stack" aria-live="assertive" aria-relevant="additions text">
        {assertive.map((notification) => (
          <NotificationCard key={notification.id} notification={notification} paused={paused} onDismiss={onDismiss} />
        ))}
      </div>
      <div className="notification-stack" aria-live="polite" aria-relevant="additions text">
        {polite.map((notification) => (
          <NotificationCard key={notification.id} notification={notification} paused={paused} onDismiss={onDismiss} />
        ))}
      </div>
    </div>,
    document.body,
  );
}

function NotificationCard({ notification, paused, onDismiss }: { notification: Notification; paused: boolean; onDismiss: (id: string) => void }) {
  const { t } = useTranslation();
  const timeout = notification.timeout ?? defaultTimeouts[notification.tone];
  useEffect(() => {
    if (timeout <= 0 || paused) return;
    const timer = window.setTimeout(() => onDismiss(notification.id), timeout);
    return () => window.clearTimeout(timer);
    // notification.repeat restarts the countdown when the same message repeats.
  }, [notification.id, notification.repeat, timeout, paused, onDismiss]);
  return (
    <div className={`notification-card ${notification.tone}`} data-tone={notification.tone}>
      <div className="notification-body">
        <strong>{notification.title}{notification.repeat > 0 && <span className="notification-repeat"> ×{notification.repeat + 1}</span>}</strong>
        {notification.description && <span>{notification.description}</span>}
      </div>
      <button type="button" className="notification-dismiss" aria-label={t("notifications.dismiss")} onClick={() => onDismiss(notification.id)}>×</button>
    </div>
  );
}
