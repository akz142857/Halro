import { useQueryClient } from "@tanstack/react-query";
import { useEffect, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { applyAppearance } from "../theme";
import type { AdminPreferences, Appearance, LocalePreference } from "../types";

const APPEARANCE_OPTIONS: Appearance[] = ["light", "dark"];

export function AppearanceForm({ preferences }: { preferences: AdminPreferences }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const [selected, setSelected] = useState<Appearance>(preferences.appearance);
  const [status, setStatus] = useState<"idle" | "saving" | "saved" | "error">("idle");
  const [errorMessage, setErrorMessage] = useState<string>("");
  // Refs hold the last server-confirmed truth so the async save chain never
  // reads a stale closure. Nothing here touches browser storage (PRD §10.2).
  const confirmed = useRef<Appearance>(preferences.appearance);
  const revision = useRef<number>(preferences.revision);
  const locale = useRef<LocalePreference>(preferences.locale);
  const seq = useRef(0); // monotonic request id; only the latest may win
  const saving = useRef(false);
  const queued = useRef<Appearance | null>(null); // last explicit choice while saving
  const failedTarget = useRef<Appearance | null>(null);

  // Re-sync to server truth when the preferences query changes and we are idle.
  useEffect(() => {
    if (saving.current) return;
    confirmed.current = preferences.appearance;
    revision.current = preferences.revision;
    locale.current = preferences.locale;
    setSelected(preferences.appearance);
  }, [preferences.appearance, preferences.revision, preferences.locale]);

  const rollback = async (message: string, target: Appearance) => {
    saving.current = false;
    queued.current = null;
    failedTarget.current = target;
    setSelected(confirmed.current);
    applyAppearance(confirmed.current);
    setErrorMessage(message);
    // The server may have advanced its revision even when it returned an
    // error (for example a compensated Audit failure), or another tab may have
    // won a revision conflict. Re-fetch before enabling Retry.
    await Promise.all([
      queryClient.invalidateQueries({ queryKey: ["preferences"] }),
      queryClient.invalidateQueries({ queryKey: ["session"] }),
    ]);
    setStatus("error");
  };

  const runSave = (target: Appearance) => {
    saving.current = true;
    const id = ++seq.current;
    setStatus("saving");
    void api
      .updatePreferences({ locale: locale.current, appearance: target }, revision.current)
      .then((result) => {
        if (id !== seq.current && queued.current === null) return; // superseded response
        confirmed.current = result.data.appearance;
        revision.current = result.data.revision;
        // A newer explicit choice arrived while this save was in flight: chain it
        // so the final server state matches the admin's last selection.
        if (queued.current !== null && queued.current !== result.data.appearance) {
          const next = queued.current;
          queued.current = null;
          runSave(next);
          return;
        }
        queued.current = null;
        failedTarget.current = null;
        saving.current = false;
        setStatus("saved");
        void queryClient.invalidateQueries({ queryKey: ["preferences"] });
        void queryClient.invalidateQueries({ queryKey: ["session"] });
      })
      .catch((err) => {
        if (id !== seq.current && queued.current === null) return;
        void rollback(err instanceof Error ? err.message : t("settings.appearance.error"), target);
      });
  };

  const retry = () => {
    const target = failedTarget.current;
    if (!target) return;
    setSelected(target);
    applyAppearance(target);
    setErrorMessage("");
    runSave(target);
  };

  const choose = (value: Appearance) => {
    if (value === selected && status !== "error") return;
    setSelected(value);
    applyAppearance(value); // instant preview (PRD §4.5)
    setErrorMessage("");
    if (saving.current) {
      queued.current = value; // last explicit choice wins
    } else {
      runSave(value);
    }
  };

  return (
    <section className="panel settings-card appearance-settings">
      <header className="panel-header">
        <div><p className="eyebrow">{t("settings.personalScope")}</p><h3>{t("settings.appearance.title")}</h3></div>
        <span className="badge">{t("settings.onlyYou")}</span>
      </header>
      <p className="panel-description">{t("settings.appearance.description")}</p>
      <fieldset className="appearance-choices" aria-busy={status === "saving"}>
        <legend className="sr-only">{t("settings.appearance.legend")}</legend>
        {APPEARANCE_OPTIONS.map((value) => (
          <label key={value} className={`appearance-option ${selected === value ? "selected" : ""}`}>
            <input
              type="radio"
              name="appearance"
              value={value}
              checked={selected === value}
              onChange={() => choose(value)}
            />
            <span className={`appearance-preview appearance-preview-${value}`} aria-hidden="true">
              <span className="appearance-preview-bar" />
              <span className="appearance-preview-body" />
            </span>
            <span className="appearance-option-name">
              {t(`settings.appearance.${value}`)}
              {selected === value && <span className="appearance-check" aria-hidden="true">✓</span>}
            </span>
          </label>
        ))}
      </fieldset>
      <div className="appearance-status" aria-live="polite">
        {status === "saving" && <span className="muted">{t("settings.appearance.saving")}</span>}
        {status === "saved" && <div className="notice success" role="status"><strong>{t("settings.appearance.saved")}</strong></div>}
        {status === "error" && (
          <div className="notice error" role="alert">
            <strong>{t("settings.appearance.error")}</strong>
            {errorMessage && <span> — {errorMessage}</span>}
            <button type="button" className="button ghost small" onClick={retry}>{t("settings.appearance.retry")}</button>
          </div>
        )}
      </div>
    </section>
  );
}
