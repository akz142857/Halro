import { useQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { DisclosureSection, Field, Modal } from "../components";
import { buildProjectIntegration, validGatewayBaseURL, type IntegrationEndpoint } from "../integration";
import type { Project } from "../types";
import "./ProjectAIIntegration.css";

type Feedback = "" | "pending" | "success" | "failure";

export function ProjectAIIntegration({ project, onClose }: { project: Project; onClose: () => void }) {
  const { t, i18n } = useTranslation();
  // This read stays available when execution in the developer workbench is off.
  // Never derive a Gateway address from the browser's Admin origin.
  const config = useQuery({ queryKey: ["developer-config"], queryFn: api.developerConfig });
  const [address, setAddress] = useState("");
  const [addressEdited, setAddressEdited] = useState(false);
  const baseURL = addressEdited ? address : config.data?.gateway_base_url ?? "";
  const models = Array.from(new Set((project.allowed_models ?? []).filter((value) => typeof value === "string" && value.trim())));
  const [model, setModel] = useState(models[0] ?? "");
  const selectedModel = models.includes(model) ? model : models[0] ?? "";
  const [endpoint, setEndpoint] = useState<IntegrationEndpoint>("responses");
  const locale = i18n.resolvedLanguage === "en-US" ? "en-US" : "zh-CN";
  const validURL = validGatewayBaseURL(baseURL);
  const generated = useMemo(() => {
    if (!project.enabled || !selectedModel || !validURL) return null;
    try {
      return buildProjectIntegration({ project, baseURL, endpoint, model: selectedModel, locale });
    } catch {
      return null;
    }
  }, [project, baseURL, endpoint, selectedModel, locale, validURL]);
  const [copyFeedback, setCopyFeedback] = useState<Feedback>("");
  const [downloadFeedback, setDownloadFeedback] = useState<Feedback>("");
  const currentArtifact = useRef(generated);
  currentArtifact.current = generated;
  useEffect(() => {
    setCopyFeedback("");
    setDownloadFeedback("");
  }, [generated?.instructions, generated?.skill]);
  useEffect(() => {
    currentArtifact.current = generated;
    return () => { currentArtifact.current = null; };
  }, [generated]);

  const copy = async () => {
    if (!generated || copyFeedback === "pending") return;
    const text = generated.instructions;
    setCopyFeedback("pending");
    try {
      await navigator.clipboard.writeText(text);
      if (currentArtifact.current?.instructions === text) setCopyFeedback("success");
    } catch {
      if (currentArtifact.current?.instructions === text) setCopyFeedback("failure");
    }
  };
  const download = () => {
    if (!generated) return;
    let objectURL: string | undefined;
    let link: HTMLAnchorElement | undefined;
    let started = false;
    try {
      objectURL = URL.createObjectURL(new Blob([generated.skill], { type: "text/markdown;charset=utf-8" }));
      link = document.createElement("a");
      link.href = objectURL;
      link.download = "SKILL.md";
      document.body.appendChild(link);
      link.click();
      started = true;
      setDownloadFeedback("success");
    } catch {
      setDownloadFeedback("failure");
    } finally {
      link?.remove();
      if (objectURL) {
        const url = objectURL;
        // Let the browser consume the download URL before releasing it. The
        // scheduled release still runs if this drawer closes immediately.
        if (started) setTimeout(() => URL.revokeObjectURL(url), 0);
        else URL.revokeObjectURL(url);
      }
    }
  };
  const blocked = !project.enabled ? t("aiIntegration.projectDisabled") : !models.length ? t("aiIntegration.noModels") : "";

  return <Modal drawer title={t("aiIntegration.title")} onClose={onClose}>
    <div className="project-ai-integration">
      <p className="project-ai-project">{project.name}</p>
      {blocked && <p className="notice warning" role="status">{blocked}</p>}
      <div className="project-ai-config">
        <div className="project-ai-address">
          <Field label={t("aiIntegration.gatewayURL")}
            hint={config.isPending && !addressEdited ? t("aiIntegration.loadingAddress") : config.isError && !addressEdited ? t("aiIntegration.addressUnavailable") : undefined}
            error={baseURL && !validURL ? t("aiIntegration.invalidAddress") : undefined}>
            <input type="url" autoComplete="off" spellCheck={false} value={baseURL} onChange={(event) => { setAddressEdited(true); setAddress(event.target.value); }} />
          </Field>
        </div>
        <Field label={t("aiIntegration.model")}>
          <select value={selectedModel} disabled={!models.length} onChange={(event) => setModel(event.target.value)}>
            {!models.length && <option value="">{t("aiIntegration.noModelsOption")}</option>}
            {models.map((alias) => <option value={alias} key={alias}>{alias}</option>)}
          </select>
        </Field>
        <Field label={t("aiIntegration.endpoint")}>
          <select value={endpoint} onChange={(event) => setEndpoint(event.target.value as IntegrationEndpoint)}>
            <option value="responses">Responses</option>
            <option value="chat">Chat Completions</option>
            <option value="embeddings">Embeddings</option>
          </select>
        </Field>
      </div>
      <p className="project-ai-key-note">{t("aiIntegration.keyBoundary")}</p>
      <div className="project-ai-actions">
        <div>
          <button type="button" className="button primary" disabled={!generated || copyFeedback === "pending"} onClick={() => void copy()}>{t("aiIntegration.copy")}</button>
          <span className={`project-ai-feedback ${copyFeedback === "failure" ? "error" : ""}`} role={copyFeedback === "failure" ? "alert" : "status"}>
            {copyFeedback === "pending" ? t("aiIntegration.copying") : copyFeedback === "success" ? t("aiIntegration.copied") : copyFeedback === "failure" ? t("aiIntegration.copyFailed") : ""}
          </span>
        </div>
        <div>
          <button type="button" className="button secondary" disabled={!generated} onClick={download}>{t("aiIntegration.download")}</button>
          <span className={`project-ai-feedback ${downloadFeedback === "failure" ? "error" : ""}`} role={downloadFeedback === "failure" ? "alert" : "status"}>
            {downloadFeedback === "success" ? t("aiIntegration.downloadStarted") : downloadFeedback === "failure" ? t("aiIntegration.downloadFailed") : ""}
          </span>
        </div>
      </div>
      {generated ? <DisclosureSection title={t("aiIntegration.preview")}><pre className="project-ai-instructions" tabIndex={0} role="region" aria-label={t("aiIntegration.preview")}>{generated.instructions}</pre></DisclosureSection>
        : !blocked && <p className="project-ai-empty">{t(baseURL && validURL && selectedModel ? "aiIntegration.invalidConfiguration" : "aiIntegration.completeFields")}</p>}
    </div>
  </Modal>;
}
