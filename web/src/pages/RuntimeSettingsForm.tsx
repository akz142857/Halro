import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { ErrorState, Field } from "../components";
import { useIsReadOnly } from "../session";

export function RuntimeSettingsForm({ settings }: { settings: { health_probe_interval_seconds: number; revision: number; updated_at?: string } }) {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const [interval, setInterval] = useState(settings.health_probe_interval_seconds);
  const queryClient = useQueryClient();
  useEffect(() => setInterval(settings.health_probe_interval_seconds), [settings.health_probe_interval_seconds]);
  const mutation = useMutation({
    mutationFn: () => api.updateSettings({ health_probe_interval_seconds: interval }, settings.revision),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["settings"] }),
  });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    mutation.mutate();
  };
  return (
    <section className="panel settings-card runtime-settings">
      <header className="panel-header">
        <div><p className="eyebrow">{t("settings.runtimeEyebrow")}</p><h3>{t("settings.runtimeTitle")}</h3></div>
        <span className="badge">{t("settings.hotApplied")}</span>
      </header>
      <form className="settings-form runtime-form" aria-busy={mutation.isPending} onSubmit={submit}>
        <div className="runtime-editable">
          <Field label={t("settings.probeInterval")} hint={t("settings.probeHint")}>
            <input type="number" min="10" max="3600" required value={interval} onChange={(event) => { mutation.reset(); setInterval(Number(event.target.value)); }} />
          </Field>
          {mutation.isError && <ErrorState error={mutation.error} />}
          {mutation.isSuccess && <div className="notice success" role="status"><strong>{t("settings.runtimeSaved")}</strong></div>}
          <div className="form-actions"><button className="button primary" disabled={readOnly || mutation.isPending || interval === settings.health_probe_interval_seconds}>{mutation.isPending ? t("settings.saving") : t("settings.saveRuntime")}</button></div>
        </div>
        <aside className="runtime-startup-boundary" aria-labelledby="runtime-startup-title">
          <span className="runtime-boundary-label">{t("settings.startupBoundary")}</span>
          <div><strong id="runtime-startup-title">{t("settings.startupLocked")}</strong><span>{t("settings.startupDescription")}</span></div>
        </aside>
      </form>
    </section>
  );
}
