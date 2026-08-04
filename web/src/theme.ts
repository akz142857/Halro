import { useEffect, useState } from "react";

/**
 * Appearance (Light / Dark) controller.
 *
 * The theme is applied by writing `data-appearance` on the root element; the
 * design system (web/src/design-system/) does the rest via CSS. There is no
 * `System`/`Auto` mode in this release, and — per the PRD's no-browser-
 * persistence boundary — the appearance is NEVER written to localStorage,
 * sessionStorage, IndexedDB, cookies or the URL. Memory (React state + the
 * server preference) is the only source of truth.
 */
export type Appearance = "light" | "dark";

export const DEFAULT_APPEARANCE: Appearance = "dark";

/** Maps any missing/unknown value to the default Dark theme (PRD §5.4). */
export function normalizeAppearance(value: unknown): Appearance {
  return value === "light" ? "light" : "dark";
}

/** Applies the given (already-normalized) appearance to the document root. */
export function applyAppearance(appearance: Appearance): void {
  if (typeof document === "undefined") return;
  document.documentElement.setAttribute("data-appearance", appearance);
}

/** Restores the unauthenticated default (Dark) — used on logout / unmount. */
export function resetAppearance(): void {
  applyAppearance(DEFAULT_APPEARANCE);
}

/** Current appearance read from the document root. */
export function currentAppearance(): Appearance {
  if (typeof document === "undefined") return DEFAULT_APPEARANCE;
  return normalizeAppearance(document.documentElement.getAttribute("data-appearance"));
}

/**
 * Subscribes to appearance changes on the document root. Needed by canvas-drawn
 * surfaces (charts) that can't inherit CSS tokens and must re-read them on
 * theme switch. React hook is imported lazily to keep this module DOM-only.
 */
export function useAppearance(): Appearance {
  const [appearance, setAppearance] = useState<Appearance>(currentAppearance);
  useEffect(() => {
    const target = document.documentElement;
    const observer = new MutationObserver(() => setAppearance(currentAppearance()));
    observer.observe(target, { attributes: true, attributeFilter: ["data-appearance"] });
    setAppearance(currentAppearance());
    return () => observer.disconnect();
  }, []);
  return appearance;
}
