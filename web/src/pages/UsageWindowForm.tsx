import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNotify } from "../notifications";
import { api } from "../api";
import { ErrorState, Field, Modal } from "../components";
import { useInstantFormatter } from "../format";
import { useIsReadOnly } from "../session";
import type { UsageSettings } from "../types";

/**
 * How far back the console pages.
 *
 * Lengthening this is free and takes effect on the next export tick.
 * Shortening it is destructive: the attempt log and the failed-request list are
 * served from an in-memory aggregate, and what falls outside the window is
 * trimmed out of it. The confirmation says so in those terms — the records are
 * not deleted, they stay in the archive, but nothing on these screens can reach
 * them again.
 */
export function UsageWindowForm({ settings }: { settings: UsageSettings }) {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const formatInstant = useInstantFormatter();
  const queryClient = useQueryClient();
  const [days, setDays] = useState(settings.console_window_days);
  const [confirming, setConfirming] = useState(false);
  useEffect(() => setDays(settings.console_window_days), [settings.console_window_days]);
  const { notify } = useNotify();
  const save = useMutation({
    mutationFn: (acknowledge: boolean) => api.updateUsageSettings(days, acknowledge, settings.revision),
    onSuccess: () => {
      setConfirming(false);
      notify({ tone: "success", title: t("settings.notifyUsageWindowSaved"), description: String(days) });
      return queryClient.invalidateQueries({ queryKey: ["usage-settings"] });
    },
  });
  // Presets the archive cannot back are dropped rather than shown disabled: an
  // option that exists only to be refused is a worse answer than not offering
  // it. The value in force is always present, even when it is not a preset.
  const choices = Array.from(
    new Set([...settings.presets.filter((preset) => preset <= settings.max_days), settings.console_window_days]),
  ).sort((left, right) => left - right);
  const shrinking = days < settings.console_window_days;
  const changed = days !== settings.console_window_days;
  return (
    <section className="panel settings-card">
      <header className="panel-header">
        <div><p className="eyebrow">{t("settings.usageWindowEyebrow")}</p><h3>{t("settings.usageWindowTitle")}</h3></div>
      </header>
      <dl className="settings-facts">
        <div>
          <dt>{t("settings.usageWindowCurrent")}</dt>
          <dd>{t("settings.usageWindowDays", { count: settings.console_window_days })}</dd>
        </div>
        <div>
          <dt>{t("settings.usageWindowArchive")}</dt>
          <dd>
            {t("settings.usageWindowDays", { count: settings.max_days })}
            <small>{t("settings.usageWindowArchiveHint")}</small>
          </dd>
        </div>
        <div>
          <dt>{t("settings.usageWindowUpdated")}</dt>
          <dd>{formatInstant(settings.updated_at, "full")}</dd>
        </div>
      </dl>
      {/* config.yaml seeds this once; after that the stored setting decides.
          Saying so prevents an operator editing the file and assuming it took. */}
      {!settings.config_file_in_effect && (
        <div className="notice warning" role="status">
          <strong>{t("settings.usageWindowConfigIgnoredTitle")}</strong>
          <span>{t("settings.usageWindowConfigIgnored", { days: settings.config_file_days })}</span>
        </div>
      )}
      <form
        className="settings-form"
        aria-busy={save.isPending}
        onSubmit={(event) => {
          event.preventDefault();
          if (shrinking) {
            setConfirming(true);
            return;
          }
          save.mutate(false);
        }}
      >
        <Field label={t("settings.usageWindowLabel")} hint={t("settings.usageWindowHint")}>
          <select
            value={days}
            disabled={readOnly || save.isPending}
            onChange={(event) => { save.reset(); setDays(Number(event.target.value)); }}
          >
            {choices.map((choice) => (
              <option key={choice} value={choice}>{t("settings.usageWindowDays", { count: choice })}</option>
            ))}
          </select>
        </Field>
        {save.isError && <ErrorState error={save.error} />}
        <div className="form-actions">
          <button className="button primary" disabled={readOnly || save.isPending || !changed}>
            {save.isPending ? t("settings.saving") : t("settings.usageWindowSave")}
          </button>
        </div>
      </form>
      {confirming && (
        <Modal title={t("settings.usageWindowConfirmTitle")} onClose={() => setConfirming(false)}>
          {/* The modal only pads a child it recognises; bare content runs to
              the edges. .confirmation-dialog is that child. */}
          <div className="confirmation-dialog">
            <p>{t("settings.usageWindowConfirmLead", { from: settings.console_window_days, to: days })}</p>
            <ul className="settings-consequences">
              <li>{t("settings.usageWindowConfirmTrim")}</li>
              <li>{t("settings.usageWindowConfirmArchive", { days: settings.max_days })}</li>
              <li>{t("settings.usageWindowConfirmIrreversible")}</li>
            </ul>
            <div className="form-actions">
              <button className="button" onClick={() => setConfirming(false)}>{t("common.cancel")}</button>
              <button
                className="button primary"
                disabled={readOnly || save.isPending}
                onClick={() => save.mutate(true)}
              >
                {save.isPending ? t("settings.saving") : t("settings.usageWindowConfirmAction")}
              </button>
            </div>
          </div>
        </Modal>
      )}
    </section>
  );
}
