import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState } from "react";
import { useTranslation } from "react-i18next";
import { useNotify } from "../notifications";
import { api } from "../api";
import { ErrorState, Modal, TimeZoneField } from "../components";
import { useInstantFormatter } from "../format";
import { useIsReadOnly } from "../session";
import type { AccountingSettings } from "../types";

/**
 * The accounting timezone: where a day ends for daily budgets and for every
 * "today" figure in the console.
 *
 * Deliberately verbose about consequences. A change here moves money between
 * days, so the confirmation states the exact instant it takes effect, that the
 * period in progress is untouched, and that nothing already recorded is
 * recomputed — an administrator should not have to infer any of that.
 */
export function AccountingTimezoneForm({ settings }: { settings: AccountingSettings }) {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const formatInstant = useInstantFormatter();
  const queryClient = useQueryClient();
  const [timezone, setTimezone] = useState(settings.pending_timezone || settings.timezone);
  const [confirming, setConfirming] = useState(false);
  // Fetched only once the administrator opens the confirmation, so the warning
  // describes the zone actually about to be committed.
  const preview = useQuery({
    queryKey: ["accounting-preview", timezone.trim()],
    queryFn: () => api.previewAccountingTimezone(timezone.trim()),
    enabled: confirming && timezone.trim() !== "" && timezone.trim() !== settings.timezone,
  });
  useEffect(() => setTimezone(settings.pending_timezone || settings.timezone), [settings.pending_timezone, settings.timezone]);
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["accounting-settings"] });
  const { notify } = useNotify();
  const schedule = useMutation({
    mutationFn: () => api.scheduleAccountingTimezone(timezone.trim(), settings.revision),
    onSuccess: () => {
      setConfirming(false);
      notify({ tone: "success", title: t("settings.notifyTimezoneScheduled"), description: timezone.trim() });
      return refresh();
    },
  });
  const cancel = useMutation({
    mutationFn: () => api.cancelAccountingTimezoneChange(settings.revision),
    onSuccess: () => {
      notify({ tone: "success", title: t("settings.notifyTimezoneCancelled") });
      return refresh();
    },
  });
  const changed = timezone.trim() !== "" && timezone.trim() !== settings.timezone;
  const busy = schedule.isPending || cancel.isPending;
  return (
    <section className="panel settings-card">
      <header className="panel-header">
        <div><p className="eyebrow">{t("settings.accountingEyebrow")}</p><h3>{t("settings.accountingTitle")}</h3></div>

      </header>
      <dl className="settings-facts">
        <div><dt>{t("settings.accountingCurrentZone")}</dt><dd><code>{settings.timezone}</code></dd></div>
        <div>
          <dt>{t("settings.accountingVersionLabel")}</dt>
          <dd>
            {settings.timezone_version}
            <small>{t("settings.accountingVersionHint")}</small>
          </dd>
        </div>
        <div>
          <dt>{t("settings.accountingCurrentPeriod")}</dt>
          <dd>{t("settings.accountingPeriodRange", {
            start: formatInstant(settings.current_period.period_start, "full"),
            end: formatInstant(settings.current_period.period_end, "full"),
          })}</dd>
        </div>
        {settings.tzdata && (
          <div>
            <dt>{t("settings.accountingRules")}</dt>
            <dd>{t("settings.accountingRulesValue", { source: settings.tzdata.source, version: settings.tzdata.version })}<br /><code>{settings.tzdata.fingerprint}</code></dd>
          </div>
        )}
      </dl>
      {/* config.yaml seeds the zone once; after that the stored setting decides.
          Saying so prevents an operator editing the file and assuming it took. */}
      {!settings.config_file_in_effect && (
        <div className="notice warning" role="status">
          <strong>{t("settings.accountingConfigIgnoredTitle")}</strong>
          <span>{t("settings.accountingConfigIgnored", { timezone: settings.config_file_timezone })}</span>
        </div>
      )}
      {settings.pending_timezone && settings.pending_effective_at && (
        <div className="notice" role="status">
          <strong>{t("settings.accountingPendingTitle", { timezone: settings.pending_timezone })}</strong>
          <span>{t("settings.accountingPendingDetail", { at: formatInstant(settings.pending_effective_at, "full") })}</span>
          <button className="button ghost" disabled={busy} onClick={() => cancel.mutate()}>{t("settings.accountingCancelPending")}</button>
        </div>
      )}
      <form className="settings-form" aria-busy={busy} onSubmit={(event) => { event.preventDefault(); setConfirming(true); }}>
        <TimeZoneField
          required
          label={t("settings.accountingZoneLabel")}
          hint={t("settings.accountingZoneHint")}
          value={timezone}
          onChange={(value) => { schedule.reset(); setTimezone(value); }}
        />
        {schedule.isError && <ErrorState error={schedule.error} />}
        {cancel.isError && <ErrorState error={cancel.error} />}
        <div className="form-actions">
          <button className="button primary" disabled={readOnly || busy || !changed}>{t("settings.accountingSchedule")}</button>
        </div>
      </form>
      {confirming && (
        <Modal title={t("settings.accountingConfirmTitle")} onClose={() => setConfirming(false)}>
          {/* The modal only pads a child it recognises; bare content runs to
              the edges. .confirmation-dialog is that child. */}
          <div className="confirmation-dialog">
          <p>{t("settings.accountingConfirmLead", { from: settings.timezone, to: timezone.trim() })}</p>
          <ul className="settings-consequences">
            <li>{t("settings.accountingConfirmEffective", {
              at: formatInstant(settings.current_period.period_end, "full"),
              utc: settings.current_period.period_end,
            })}</li>
            <li>{t("settings.accountingConfirmInProgress")}</li>
            <li>{t("settings.accountingConfirmNoRecompute")}</li>
          </ul>
          {/* The switch starts a fresh balance, so the daily budget resets again
              this soon afterwards — often well under a day. Nobody works that
              out from the zone names alone. */}
          {preview.data?.data.switch_preview && (
            <div className="notice warning" role="status">
              <strong>{t("settings.accountingResetWindowTitle", {
                hours: Math.round(preview.data.data.switch_preview.next_reset_in_hours),
              })}</strong>
              <span>{t("settings.accountingResetWindowDetail", {
                at: formatInstant(preview.data.data.switch_preview.next_reset_at, "full"),
                utc: preview.data.data.switch_preview.next_reset_at,
              })}</span>
            </div>
          )}
          {/* The reset-window warning above is the one consequence nobody can
              derive from the zone names, and it arrives asynchronously. Leaving
              the button live while it is in flight let a confirmation land
              before the warning rendered, and a failed preview looked exactly
              like "no extra risk" — the operator signing without the sentence
              the dialog exists to show them. */}
          {preview.isError && (
            <div className="notice error" role="alert">
              <strong>{t("settings.accountingPreviewUnavailable")}</strong>
              <span>{t("settings.accountingPreviewUnavailableDetail")}</span>
            </div>
          )}
          <div className="form-actions">
            <button className="button" onClick={() => setConfirming(false)}>{t("common.cancel")}</button>
            <button
              className="button primary"
              disabled={readOnly || busy || preview.isFetching || preview.isError}
              onClick={() => schedule.mutate()}
            >
              {preview.isFetching ? t("common.loading") : schedule.isPending ? t("settings.saving") : t("settings.accountingConfirmAction")}
            </button>
          </div>
          </div>
        </Modal>
      )}
    </section>
  );
}
