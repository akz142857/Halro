import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { ApiError, api, type Reauth } from "../api";
import { ConfirmButton, ErrorState, Field, Loading } from "../components";
import { Link } from "../navigation";
import type { AIConfigurationPreview, AIConfigurationResult } from "../types";

export function AIConfigurationPanel() {
  const { t, i18n } = useTranslation();
  const client = useQueryClient();
  const deployments = useQuery({ queryKey: ["deployments"], queryFn: api.deployments });
  const projects = useQuery({ queryKey: ["projects"], queryFn: api.projects });
  const [deploymentID, setDeploymentID] = useState("");
  const [projectID, setProjectID] = useState("");
  const [plan, setPlan] = useState("");
  const [reading, setReading] = useState(false);
  const [fileError, setFileError] = useState(false);
  const [preview, setPreview] = useState<AIConfigurationPreview>();
  const [result, setResult] = useState<AIConfigurationResult>();
  const [conflict, setConflict] = useState<ApiError>();
  const [applying, setApplying] = useState(false);
  const availableDeployments = (deployments.data?.items ?? []).filter((item) => item.enabled);
  const availableProjects = (projects.data?.items ?? []).filter((item) => item.enabled);

  const download = useMutation({
    mutationFn: async () => {
      const content = await api.exportConfigurationSkill(deploymentID, projectID, i18n.resolvedLanguage ?? i18n.language);
      const url = URL.createObjectURL(new Blob([content], { type: "text/markdown;charset=utf-8" }));
      const link = document.createElement("a");
      link.href = url;
      link.download = "SKILL.md";
      document.body.appendChild(link);
      try {
        link.click();
      } finally {
        link.remove();
        window.setTimeout(() => URL.revokeObjectURL(url), 1_000);
      }
    },
  });
  const review = useMutation({
    mutationFn: () => api.previewAIConfiguration(plan),
    onMutate: () => { setPreview(undefined); setConflict(undefined); },
    onSuccess: (next) => {
      setPreview(next);
      setResult(next.result);
    },
  });

  async function selectPlan(file?: File) {
    setPlan("");
    setPreview(undefined);
    setResult(undefined);
    setConflict(undefined);
    setFileError(false);
    review.reset();
    if (!file) return;
    setReading(true);
    try {
      // Leave space for the preview ID and reauthentication envelope on apply.
      if (file.size > 6 * 1024) throw new Error("plan is too large");
      const content = await file.text();
      const value: unknown = JSON.parse(content);
      if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("expected a JSON object");
      setPlan(content);
    } catch {
      setFileError(true);
    } finally {
      setReading(false);
    }
  }

  async function apply(reauth: Reauth) {
    if (!preview?.preview_id) return;
    setApplying(true);
    try {
      setResult(await api.applyAIConfiguration(plan, preview.preview_id, reauth));
      setPreview(undefined);
      void client.invalidateQueries({ queryKey: ["routes"] });
      void client.invalidateQueries({ queryKey: ["projects"] });
      void client.invalidateQueries({ queryKey: ["audit"] });
    } catch (error) {
      if (!(error instanceof ApiError) || error.status !== 409) throw error;
      setConflict(error);
      setPreview(undefined);
    } finally {
      setApplying(false);
    }
  }

  const exportLoading = deployments.isPending || projects.isPending;
  const exportError = deployments.error ?? projects.error ?? download.error;
  const canDownload = !download.isPending && availableDeployments.some((item) => item.id === deploymentID) &&
    availableProjects.some((item) => item.id === projectID);
  const busy = reading || review.isPending || applying;
  return <>
    <section className="configuration-transfer-card" aria-labelledby="ai-skill-title">
      <h3 id="ai-skill-title">{t("aiConfiguration.exportTitle")}</h3>
      <p className="configuration-transfer-description">{t("aiConfiguration.exportDescription")}</p>
      {exportLoading && <Loading />}
      {exportError && <ErrorState error={exportError} />}
      {!exportLoading && <div className="form-grid">
        <Field label={t("aiConfiguration.deployment")}>
          <select value={deploymentID} onChange={(event) => { setDeploymentID(event.target.value); download.reset(); }} disabled={download.isPending}>
            <option value="">{t("aiConfiguration.chooseDeployment")}</option>
            {availableDeployments.map((item) => <option key={item.id} value={item.id}>{item.name} · {item.id}</option>)}
          </select>
        </Field>
        <Field label={t("aiConfiguration.project")}>
          <select value={projectID} onChange={(event) => { setProjectID(event.target.value); download.reset(); }} disabled={download.isPending}>
            <option value="">{t("aiConfiguration.chooseProject")}</option>
            {availableProjects.map((item) => <option key={item.id} value={item.id}>{item.name} · {item.id}</option>)}
          </select>
        </Field>
      </div>}
      <div className="form-actions configuration-transfer-actions">
        <button className="button" type="button" disabled={!canDownload} onClick={() => download.mutate()}>
          {download.isPending ? t("common.working") : t("aiIntegration.download")}
        </button>
      </div>
    </section>

    <section className="configuration-transfer-card" aria-labelledby="ai-plan-title">
      <h3 id="ai-plan-title">{t("aiConfiguration.planTitle")}</h3>
      <p className="configuration-transfer-description">{t("aiConfiguration.planDescription")}</p>
      <Field label={t("aiConfiguration.file")}>
        <input type="file" accept=".json,application/json" disabled={busy} onChange={(event) => void selectPlan(event.currentTarget.files?.[0])} />
      </Field>
      {fileError && <p className="notice error" role="alert">{t("aiConfiguration.invalidPlan")}</p>}
      {(conflict || review.error) && <ErrorState error={conflict ?? review.error} />}
      {!result && <div className="form-actions configuration-transfer-actions">
        <button className="button" type="button" disabled={!plan || busy} onClick={() => review.mutate()}>
          {review.isPending ? t("common.working") : t("aiConfiguration.preview")}
        </button>
      </div>}
      {preview?.preview_id && <div className="configuration-detail configuration-list">
        <h4>{t("aiConfiguration.diffTitle")}</h4>
        <ul>
          <li>{t("aiConfiguration.routeChange", { alias: preview.public_model, deployment: preview.deployment.name, id: preview.deployment.id })}</li>
          <li>{t("aiConfiguration.projectChange", { alias: preview.public_model, project: preview.project.name, id: preview.project.id })}</li>
        </ul>
        <ConfirmButton label={t("aiConfiguration.apply")} className="button primary" requireStepUp disabled={busy}
          confirmLabel={t("aiConfiguration.confirm", { alias: preview.public_model, project: preview.project.name, id: preview.project.id })}
          onConfirm={apply} />
      </div>}
      {result && <div className={`notice ${result.verification === "verified" ? "success" : "warning"} configuration-list`} role="status">
        <h4>{t(`aiConfiguration.result.${result.status}`)}</h4>
        <p>{t(`aiConfiguration.verification.${result.verification}`)}</p>
        <p>{t("aiConfiguration.operation", { id: result.operation_id })}</p>
        <div className="form-actions configuration-transfer-actions">
          <Link className="text-link" href={`/admin/routes#route-${encodeURIComponent(result.route_id)}`}>{t("aiConfiguration.viewRoute")}</Link>
          <Link className="text-link" href={`/admin/operations?q=${encodeURIComponent(result.operation_id)}`}>{t("aiConfiguration.viewAudit")}</Link>
        </div>
        {result.verification !== "verified" && <div className="form-actions configuration-transfer-actions">
          <button className="button" type="button" disabled={busy} onClick={() => review.mutate()}>
            {review.isPending ? t("common.working") : t("aiConfiguration.recheck")}
          </button>
        </div>}
      </div>}
    </section>
  </>;
}
