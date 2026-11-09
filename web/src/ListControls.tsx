import type { ReactNode } from "react";
import { useTranslation } from "react-i18next";
import { navigate, useNavigationLocation } from "./navigation";

// Query updates read the current URL so consecutive updates never overwrite
// one another. Typing replaces history; choosing a view creates a back target.
export function useListQuery() {
  useNavigationLocation();
  const params = new URLSearchParams(window.location.search);
  const update = (values: Record<string, string>, replace = true) => {
    const url = new URL(window.location.href);
    for (const [key, value] of Object.entries(values)) {
      if (value) url.searchParams.set(key, value);
      else url.searchParams.delete(key);
    }
    navigate(`${url.pathname}${url.search}${url.hash}`, { replace });
  };
  return { params, update };
}

export type ListView = { key: string; label: string; icon?: "all" | "error" | "active" | "clock" | "paused" | "check"; count?: number };

const paths = {
  all: "M4 4h6v6H4zM14 4h6v6h-6zM4 14h6v6H4zM14 14h6v6h-6z",
  error: "M12 3 2 21h20L12 3ZM12 9v5M12 17v1",
  active: "M2 12h5l3-8 4 16 3-8h5",
  clock: "M12 8v4l3 2M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0Z",
  paused: "M8 5v14M16 5v14",
  check: "m7 12 3 3 7-7M21 12a9 9 0 1 1-18 0 9 9 0 0 1 18 0Z",
};

export function ListViews({ label, value, items, onChange, trailing }: {
  label: string; value: string; items: ListView[]; onChange: (value: string) => void; trailing?: ReactNode;
}) {
  return <div className="list-views-bar">
    <div className="list-views" role="group" aria-label={label}>
      {items.map((item) => <button type="button" key={item.key} className="list-view" aria-pressed={value === item.key} onClick={() => onChange(item.key)}>
        <svg viewBox="0 0 24 24" aria-hidden="true"><path d={paths[item.icon ?? "all"]} /></svg>
        <span>{item.label}</span>{item.count !== undefined && <span className="list-view-count">{item.count}</span>}
      </button>)}
    </div>
    {trailing && <div className="list-views-trailing">{trailing}</div>}
  </div>;
}

export function ListRefresh({ busy, onRefresh }: { busy: boolean; onRefresh: () => void }) {
  const { t } = useTranslation();
  return <button type="button" className="list-refresh" aria-label={t("listControls.refreshList")} disabled={busy} onClick={onRefresh}>
    <svg viewBox="0 0 24 24" aria-hidden="true"><path d="M20 8a8 8 0 1 0 0 8M20 3v5h-5" /></svg>
    {t(busy ? "listControls.refreshing" : "listControls.refresh")}
  </button>;
}

export function FilterChips({ items, onClear }: { items: { key: string; label: string; clear: () => void }[]; onClear: () => void }) {
  const { t } = useTranslation();
  if (!items.length) return null;
  return <div className="list-filter-chips" aria-label={t("usage.appliedFilters")}>
    {items.map((item) => <button type="button" className="filter-chip" key={item.key} onClick={item.clear}>{item.label}<span aria-hidden="true">×</span></button>)}
    <button type="button" className="list-clear" onClick={onClear}>{t("listControls.clear")}</button>
  </div>;
}
