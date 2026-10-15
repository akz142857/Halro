import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNotify } from "../notifications";
import { api } from "../api";
import { ErrorState, Field } from "../components";
import { useInstantFormatter } from "../format";
import { useIsReadOnly } from "../session";
import type { UsageSettings } from "../types";

/**
 * How many days the attempt log and the failed-request list show by default.
 *
 * Changing it in either direction discards nothing: every attempt stays in the
 * usage history index, and a list can always be asked for an older range.
 */
export function UsageWindowForm({ settings }: { settings: UsageSettings }) {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const formatInstant = useInstantFormatter();
  const queryClient = useQueryClient();
  const [days, setDays] = useState(settings.console_window_days);
  useEffect(() => setDays(settings.console_window_days), [settings.console_window_days]);
  const { notify } = useNotify();
  const save = useMutation({
    mutationFn: () => api.updateUsageSettings(days, settings.revision),
    onSuccess: () => {
      notify({ tone: "success", title: t("settings.notifyUsageWindowSaved"), description: String(days) });
      return queryClient.invalidateQueries({ queryKey: ["usage-settings"] });
    },
  });
  // The value in force is always offered, even when it is not a preset.
  const choices = Array.from(new Set([...settings.presets, settings.console_window_days])).sort(
    (left, right) => left - right,
  );
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
          save.mutate();
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
    </section>
  );
}
