import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState, type KeyboardEvent } from "react";
import { useTranslation } from "react-i18next";
import { useNotify } from "../notifications";
import { useIsReadOnly } from "../session";
import { OnboardingContextBanner } from "../OnboardingContext";
import { api } from "../api";
import { ConfirmButton, EmptyState, ErrorState, Field, Loading, PageHeader, type ReauthValues } from "../components";
import { navigate } from "../navigation";

type Endpoint = "responses" | "chat" | "embeddings";
type Language = "curl" | "javascript" | "python" | "go" | "java";
type RequestMode = "form" | "json";
type ResponseView = "body" | "headers";
type ExecutionOutcome = "idle" | "running" | "completed" | "httpError" | "cancelled" | "failed" | "truncated";

interface ExecutionState {
  outcome: ExecutionOutcome;
  status?: number;
  statusText?: string;
  headers: string;
  body: string;
  requestID: string;
  usageAvailable?: boolean;
  adminRejected?: boolean;
  streaming?: boolean;
  latency?: number;
  error?: string;
}

/** An image the request carries. An inline image travels as a data URL built in this
 * page; a remote one is only a URL — the console never fetches it, the provider does. */
interface ImageInput {
  id: string;
  name: string;
  url: string;
  inline: boolean;
  mediaType: string;
  bytes: number;
}

const languages: Language[] = ["curl", "javascript", "python", "go", "java"];
const maxResponseBytes = 1 << 20;
// Re-rendering the whole body on every chunk is O(n²) over a long stream; sample instead.
const streamRenderIntervalMillis = 100;
const copyStatusTimeoutMillis = 4000;
// Debug keys expire on their own so a forgotten one cannot stay usable.
const debugKeyLifetimeMillis = 24 * 60 * 60 * 1000;
const emptyExecution: ExecutionState = { outcome: "idle", headers: "", body: "", requestID: "" };

export function DeveloperPage() {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const projects = useQuery({ queryKey: ["projects"], queryFn: api.projects });
  const developerConfig = useQuery({ queryKey: ["developer-config"], queryFn: api.developerConfig });
  const availableProjects = useMemo(
    () => (projects.data?.items ?? []).filter((project) => project.enabled && (project.allowed_models ?? []).length > 0),
    [projects.data?.items],
  );
  const [projectID, setProjectID] = useState("");
  const selectedProject = availableProjects.find((project) => project.id === projectID) ?? availableProjects[0];
  const [model, setModel] = useState("");
  const [endpoint, setEndpoint] = useState<Endpoint>("responses");
  const [input, setInput] = useState(() => t("developer.defaultInput"));
  const [images, setImages] = useState<ImageInput[]>([]);
  const [imageURL, setImageURL] = useState("");
  const [imageProblem, setImageProblem] = useState("");
  // Debugging defaults to a standard response: it is the simpler thing to read back.
  const [stream, setStream] = useState(false);
  const [requestMode, setRequestMode] = useState<RequestMode>("form");
  const [gatewayKey, setGatewayKey] = useState("");
  const [showGatewayKey, setShowGatewayKey] = useState(false);
  const [createdKeyName, setCreatedKeyName] = useState("");
  const [gatewayURL, setGatewayURL] = useState("");
  const [language, setLanguage] = useState<Language>("curl");
  const [copyStatus, setCopyStatus] = useState("");
  const [responseView, setResponseView] = useState<ResponseView>("body");
  const [codeExpanded, setCodeExpanded] = useState(false);
  const [execution, setExecution] = useState<ExecutionState>(emptyExecution);
  const executionController = useRef<AbortController | null>(null);

  useEffect(() => {
    if (selectedProject && selectedProject.id !== projectID) setProjectID(selectedProject.id);
  }, [projectID, selectedProject]);
  useEffect(() => {
    const routes = selectedProject?.allowed_models ?? [];
    if (!routes.includes(model)) setModel(routes[0] ?? "");
  }, [model, selectedProject]);
  // Seed the URL once. Reacting to gatewayURL would refill the field the moment the
  // user clears it to type a different one.
  const gatewayURLSeeded = useRef(false);
  useEffect(() => {
    if (gatewayURLSeeded.current || !developerConfig.data?.gateway_base_url) return;
    gatewayURLSeeded.current = true;
    setGatewayURL(developerConfig.data.gateway_base_url);
  }, [developerConfig.data?.gateway_base_url]);
  useEffect(() => () => executionController.current?.abort(), []);

  // Embeddings cannot stream, but the preference survives so switching back restores it.
  const streamRequested = stream && endpoint !== "embeddings";
  // Embeddings has no multimodal input, so images stay out of its body — and survive the
  // detour, the way the streaming preference does.
  const formImages = useMemo(() => endpoint === "embeddings" ? [] : images, [endpoint, images]);
  const formBody = useMemo(() => requestBody(endpoint, model, input, streamRequested, formImages), [endpoint, formImages, input, model, streamRequested]);
  const [rawJSON, setRawJSON] = useState(() => JSON.stringify(requestBody("responses", "", t("developer.defaultInput"), false, []), null, 2));
  const parsedJSON = useMemo(() => parseJSON(rawJSON), [rawJSON]);
  const body = requestMode === "json" ? parsedJSON.value : formBody;
  const isStreaming = endpoint !== "embeddings" && body?.stream === true;
  const path = endpoint === "chat" ? "/v1/chat/completions" : `/v1/${endpoint}`;
  const gatewayURLValid = validGatewayBaseURL(gatewayURL);
  // A local file is megabytes of base64: pasted into an integration sample it drowns the
  // request it is meant to explain, so the sample names the file where the real request
  // carries its bytes.
  const sampleBody = useMemo(
    () => requestMode === "json" ? body : requestBody(endpoint, model, input, streamRequested, formImages.map(sampleImage)),
    [body, endpoint, formImages, input, model, requestMode, streamRequested],
  );
  const code = useMemo(() => sampleBody && gatewayURLValid ? codeExample(language, gatewayURL, path, sampleBody) : "", [gatewayURL, gatewayURLValid, language, path, sampleBody]);
  const running = execution.outcome === "running";
  const responseStreaming = execution.outcome === "idle" ? isStreaming : execution.streaming === true;
  // The Gateway URL only feeds the code sample; the real call always enters this Runtime,
  // so an unusable URL must not block sending.
  const missingRequirements = [
    !body ? t("developer.rawJSON") : "",
    !model ? t("developer.publicModel") : "",
    !gatewayKey.trim() ? t("developer.gatewayKey") : "",
  ].filter(Boolean);
  // An inline image is base64, so the body outgrows the Gateway's limit long before the
  // file looks large. Measure what will actually be sent rather than spend a round trip
  // on a 413.
  const requestBytes = useMemo(() => body ? byteLength(JSON.stringify(body)) : 0, [body]);
  const requestLimit = developerConfig.data?.max_request_bytes ?? 0;
  const oversized = requestLimit > 0 && requestBytes > requestLimit;
  const canSend = missingRequirements.length === 0 && !oversized;
  const executionLabel = execution.outcome === "running" ? t("developer.requestRunning") :
    execution.outcome === "completed" ? t("developer.requestCompleted") :
      execution.outcome === "httpError" ? t("developer.requestHTTPError", { status: execution.status ?? "" }) :
        execution.outcome === "cancelled" ? t("developer.requestCancelled") :
          execution.outcome === "truncated" ? t("developer.responseTruncated") :
            execution.outcome === "failed" ? t("developer.requestFailed") : "";
  const executionHint = execution.outcome !== "httpError" ? "" :
    execution.adminRejected ? t("developer.hintSessionExpired") :
      execution.status === 401 || execution.status === 403 ? t("developer.hintUnauthorized") :
        execution.status === 429 ? t("developer.hintRateLimited") : "";
  const copy = async () => {
    if (!code) return;
    try {
      await navigator.clipboard.writeText(code);
      setCopyStatus(t("developer.copied"));
    } catch {
      setCopyStatus(t("developer.copyFailed"));
    }
  };
  useEffect(() => {
    if (!copyStatus) return;
    const timer = setTimeout(() => setCopyStatus(""), copyStatusTimeoutMillis);
    return () => clearTimeout(timer);
  }, [copyStatus]);
  // Picking a language is only meaningful if the sample is on screen, so reveal it.
  const selectLanguage = (next: Language) => {
    setLanguage(next);
    setCopyStatus("");
    setCodeExpanded(true);
  };
  const selectRequestMode = (mode: RequestMode) => {
    if (mode === "json" && requestMode !== "json") setRawJSON(JSON.stringify(formBody, null, 2));
    setRequestMode(mode);
  };
  const selectEndpoint = (next: Endpoint) => {
    setEndpoint(next);
    if (requestMode === "json") {
      setRawJSON(JSON.stringify(requestBody(next, model, input, stream && next !== "embeddings", next === "embeddings" ? [] : images), null, 2));
    }
  };
  const queryClient = useQueryClient();
  const { notify } = useNotify();
  // The console never holds an existing key's plaintext — it is stored as a SHA-256 hash.
  // Creating one is the only moment the secret exists, so fill it in straight from there.
  const createDebugKey = useMutation({
    // A debug key is a real Gateway Key: shown once, valid after this session
    // ends, and billable. It asks for the same proof any other minting does.
    mutationFn: (reauth: ReauthValues) => api.createKey(
      selectedProject!.id,
      t("developer.debugKeyName", { time: debugKeyTimestamp() }),
      crypto.randomUUID(),
      reauth,
      new Date(Date.now() + debugKeyLifetimeMillis).toISOString(),
    ),
    onSuccess: (created) => {
      setGatewayKey(created.data.key);
      setShowGatewayKey(false);
      setCreatedKeyName(created.data.metadata.name);
      queryClient.invalidateQueries({ queryKey: ["project-keys", selectedProject?.id] });
      // The key itself stays in the field it was written into; the column only
      // reports that a billable key now exists.
      notify({ tone: "success", title: t("developer.notifyKeyCreated"), description: created.data.metadata.name });
    },
  });
  const addImageURL = () => {
    const image = imageFromURL(imageURL.trim());
    if (!image) {
      setImageProblem(t("developer.imageURLInvalid"));
      return;
    }
    if (requestLimit > 0 && image.bytes > requestLimit) {
      setImageProblem(t("developer.imageTooLarge", { name: image.name, limit: formatBytes(requestLimit) }));
      return;
    }
    setImageProblem("");
    setImages((current) => [...current, image]);
    setImageURL("");
  };
  const addImageFiles = async (files: FileList | null) => {
    const accepted: ImageInput[] = [];
    let problem = "";
    for (const file of Array.from(files ?? [])) {
      if (!file.type.startsWith("image/")) {
        problem = t("developer.imageFileType");
        continue;
      }
      // Refuse before reading: a file too large to send is also large enough to
      // stall the page while it is turned into base64.
      if (requestLimit > 0 && base64Bytes(file.size) > requestLimit) {
        problem = t("developer.imageTooLarge", { name: file.name, limit: formatBytes(requestLimit) });
        continue;
      }
      try {
        const url = await readDataURL(file);
        accepted.push({ id: crypto.randomUUID(), name: file.name, url, inline: true, mediaType: file.type, bytes: byteLength(url) });
      } catch {
        problem = t("developer.imageReadFailed");
      }
    }
    setImageProblem(problem);
    if (accepted.length) setImages((current) => [...current, ...accepted]);
  };
  const removeImage = (id: string) => setImages((current) => current.filter((image) => image.id !== id));
  const cancelExecution = () => executionController.current?.abort();
  const execute = async () => {
    if (!body || !canSend || running) return;
    const controller = new AbortController();
    executionController.current = controller;
    const startedAt = performance.now();
    let correlatedRequestID = "";
    setExecution({ ...emptyExecution, outcome: "running", streaming: isStreaming });
    setResponseView("body");
    try {
      const response = await api.developerExecute(executionEndpoint(endpoint), gatewayKey.trim(), body, isStreaming, controller.signal);
      const headers = responseHeaders(response.headers);
      const requestID = response.headers.get("X-Request-ID") || response.headers.get("request-id") || "";
      correlatedRequestID = requestID;
      setExecution((current) => ({ ...current, status: response.status, statusText: response.statusText, headers, requestID }));
      if (!response.body) throw new Error(t("developer.missingResponseBody"));
      const reader = response.body.getReader();
      const decoder = new TextDecoder();
      let received = "";
      let receivedBytes = 0;
      let truncated = false;
      let renderedAt = 0;
      while (true) {
        const { value, done } = await reader.read();
        if (done) break;
        receivedBytes += value.byteLength;
        received += decoder.decode(value, { stream: true });
        if (receivedBytes > maxResponseBytes) {
          truncated = true;
          await reader.cancel();
        }
        const now = performance.now();
        if (isStreaming && (truncated || now - renderedAt >= streamRenderIntervalMillis)) {
          renderedAt = now;
          setExecution((current) => ({ ...current, body: received }));
        }
        if (truncated) break;
      }
      received += decoder.decode();
      setExecution((current) => ({
        ...current,
        // A 4xx/5xx body still reads cleanly off the wire; only response.ok separates
        // "the gateway answered" from "the gateway accepted".
        outcome: truncated ? "truncated" : response.ok ? "completed" : "httpError",
        body: isStreaming ? received : formatResponseBody(received),
        latency: Math.round(performance.now() - startedAt),
        // The admin layer and the Gateway both answer 401 here; only the error envelope
        // shape says whether the session lapsed or the Gateway Key was refused.
        adminRejected: !response.ok && adminEnvelopeError(received),
      }));
    } catch (error) {
      const cancelled = controller.signal.aborted;
      setExecution((current) => ({
        ...current,
        outcome: cancelled ? "cancelled" : "failed",
        latency: Math.round(performance.now() - startedAt),
        error: cancelled ? t("developer.requestCancelled") : error instanceof Error ? error.message : t("developer.requestFailed"),
      }));
    } finally {
      if (executionController.current === controller) executionController.current = null;
      // A cancelled or unmounted execution must not keep probing after the user moved on.
      if (correlatedRequestID && !controller.signal.aborted) {
        try {
          await api.usageRequest(correlatedRequestID);
          setExecution((current) => current.requestID === correlatedRequestID ? { ...current, usageAvailable: true } : current);
        } catch {
          // Authentication and request-validation failures legitimately have no Usage record.
        }
      }
    }
  };

  return (
    <section className="developer-page">
      <PageHeader
        eyebrow={t("developer.eyebrow")}
        title={t("developer.title")}
        description={t("developer.description")}
        action={<span className="badge developer-preview-badge">{t("developer.previewBadge")}</span>}
      />
      <OnboardingContextBanner />
      {developerConfig.data?.enabled === false && (
        <EmptyState title={t("developer.disabledTitle")}>{t("developer.disabledDescription")}</EmptyState>
      )}
      {projects.isPending && <Loading label={t("developer.loading")} />}
      {projects.isError && <ErrorState error={projects.error} />}
      {projects.isSuccess && availableProjects.length === 0 && developerConfig.data?.enabled !== false && (
        <EmptyState
          title={t("developer.noProjects")}
          action={<button className="button primary" onClick={() => navigate("/admin/projects")}>{t("developer.goToProjects")}</button>}
        >{t("developer.noProjectsDescription")}</EmptyState>
      )}
      {selectedProject && developerConfig.data?.enabled !== false && (
        <div className="developer-workbench">
          <section className="developer-config-panel" aria-labelledby="developer-request-setup">
            <header className="developer-panel-header">
              <div><p className="eyebrow">{t("developer.stepRequest")}</p><h2 id="developer-request-setup">{t("developer.requestSetup")}</h2></div>
            </header>
            <div className="developer-mode-tabs" role="tablist" aria-label={t("developer.requestMode")}>
              <button id="developer-request-tab-form" type="button" role="tab" tabIndex={requestMode === "form" ? 0 : -1} aria-selected={requestMode === "form"} aria-controls={requestMode === "form" ? "developer-request-panel-form" : undefined} onKeyDown={(event) => moveTab(event, ["form", "json"], requestMode, selectRequestMode, "developer-request-tab")} onClick={() => selectRequestMode("form")}>{t("developer.formMode")}</button>
              <button id="developer-request-tab-json" type="button" role="tab" tabIndex={requestMode === "json" ? 0 : -1} aria-selected={requestMode === "json"} aria-controls={requestMode === "json" ? "developer-request-panel-json" : undefined} onKeyDown={(event) => moveTab(event, ["form", "json"], requestMode, selectRequestMode, "developer-request-tab")} onClick={() => selectRequestMode("json")}>{t("developer.jsonMode")}</button>
            </div>
            <div id={`developer-request-panel-${requestMode}`} role="tabpanel" aria-labelledby={`developer-request-tab-${requestMode}`}>
            <div className="developer-config-fields">
              <Field label={t("developer.project")} hint={t("developer.projectFilterHint")}>
                <select value={selectedProject.id} onChange={(event) => setProjectID(event.target.value)}>
                  {availableProjects.map((project) => <option value={project.id} key={project.id}>{project.name}</option>)}
                </select>
              </Field>
              {/* In JSON mode the body below is the source of truth; leaving the picker live
                  would show one model while sending — and billing — another. */}
              <Field label={t("developer.publicModel")} hint={requestMode === "json" ? t("developer.jsonModeOverride") : undefined}>
                <select value={model} disabled={requestMode === "json"} onChange={(event) => setModel(event.target.value)}>
                  {(selectedProject.allowed_models ?? []).map((route) => <option value={route} key={route}>{route}</option>)}
                </select>
              </Field>
              <Field label={t("developer.endpoint")}>
                <select value={endpoint} onChange={(event) => selectEndpoint(event.target.value as Endpoint)}>
                  <option value="responses">{t("developer.endpointResponses")}</option>
                  <option value="chat">{t("developer.endpointChat")}</option>
                  <option value="embeddings">{t("developer.endpointEmbeddings")}</option>
                </select>
              </Field>
              <Field
                label={t("developer.gatewayURL")}
                hint={developerConfig.isError ? t("developer.gatewayURLUnavailable") : t("developer.gatewayURLHint")}
                error={gatewayURL && !gatewayURLValid ? t("developer.invalidGatewayURL") : undefined}
              >
                <input type="url" value={gatewayURL} onChange={(event) => setGatewayURL(event.target.value)} spellCheck={false} />
              </Field>
              <div className="field developer-key-field">
                <label htmlFor="developer-gateway-key">{t("developer.gatewayKey")}</label>
                <div className="developer-secret-input">
                  <input
                    id="developer-gateway-key"
                    type={showGatewayKey ? "text" : "password"}
                    value={gatewayKey}
                    required
                    aria-required="true"
                    aria-describedby="developer-gateway-key-hint"
                    autoComplete="new-password"
                    placeholder={t("developer.gatewayKeyPlaceholder")}
                    onChange={(event) => setGatewayKey(event.target.value)}
                  />
                  <button type="button" className="button ghost" aria-pressed={showGatewayKey} aria-controls="developer-gateway-key" onClick={() => setShowGatewayKey((value) => !value)}>
                    {showGatewayKey ? t("developer.hideKey") : t("developer.showKey")}
                  </button>
                  <ConfirmButton
                    className="button secondary developer-create-key"
                    label={t("developer.createDebugKey")}
                    confirmLabel={t("auth.stepUpMintKey")}
                    disabled={readOnly || createDebugKey.isPending}
                    requireStepUp
                    onConfirm={(reauth) => createDebugKey.mutateAsync(reauth)}
                  />
                </div>
                <small id="developer-gateway-key-hint">{t("developer.gatewayKeyHint")}</small>
                {createDebugKey.isError && <ErrorState error={createDebugKey.error} />}
                {createdKeyName && (
                  <p className="developer-created-key" role="status">
                    {t("developer.debugKeyCreated", { name: createdKeyName })}{" "}
                    <button type="button" className="resource-link inline" onClick={() => navigate("/admin/projects")}>
                      {t("developer.manageKeys")}
                    </button>
                  </p>
                )}
              </div>
              {requestMode === "form" ? (
                <>
                  <Field label={t("developer.input")}>
                    <textarea rows={6} value={input} placeholder={t("developer.inputPlaceholder")} onChange={(event) => setInput(event.target.value)} />
                  </Field>
                  {endpoint !== "embeddings" && (
                    <div className="field developer-image-field">
                      <label htmlFor="developer-image-url">{t("developer.images")}</label>
                      <div className="developer-image-add">
                        <input
                          id="developer-image-url"
                          type="url"
                          value={imageURL}
                          spellCheck={false}
                          placeholder={t("developer.imageURLPlaceholder")}
                          aria-describedby="developer-image-hint"
                          onChange={(event) => setImageURL(event.target.value)}
                          onKeyDown={(event) => {
                            if (event.key !== "Enter") return;
                            event.preventDefault();
                            addImageURL();
                          }}
                        />
                        <button type="button" className="button secondary" disabled={!imageURL.trim()} onClick={addImageURL}>{t("developer.addImage")}</button>
                        {/* The visible control is a label bound to the file input, which
                            stays in the accessibility tree behind it: a button that
                            forwarded the click would leave the input unnamed. */}
                        <label className="button ghost developer-image-file" htmlFor="developer-image-file">{t("developer.chooseImageFile")}</label>
                        <input
                          id="developer-image-file"
                          className="visually-hidden"
                          type="file"
                          accept="image/*"
                          multiple
                          onChange={(event) => {
                            void addImageFiles(event.target.files);
                            // Clearing it lets the same file be picked again after a removal.
                            event.target.value = "";
                          }}
                        />
                      </div>
                      {imageProblem && <small className="developer-image-problem" role="alert">{imageProblem}</small>}
                      {images.length > 0 && (
                        <ul className="developer-image-list" aria-label={t("developer.imageList")}>
                          {images.map((image) => (
                            <li key={image.id}>
                              {/* Only a data URL is previewed: the console's CSP allows no
                                  remote image, and fetching one here would not prove the
                                  provider can reach it either. */}
                              {image.inline
                                ? <img src={image.url} alt="" />
                                : <span className="developer-image-remote" aria-hidden="true">URL</span>}
                              <div>
                                <strong title={image.inline ? image.name : image.url}>{image.name}</strong>
                                <small>{image.inline ? formatBytes(image.bytes) : t("developer.imageRemote")}</small>
                              </div>
                              <button type="button" className="button ghost" onClick={() => removeImage(image.id)}>
                                {t("developer.removeImage")}
                              </button>
                            </li>
                          ))}
                        </ul>
                      )}
                      <small id="developer-image-hint">{t("developer.imagesHint")}</small>
                    </div>
                  )}
                  <div className="developer-response-mode">
                    <span>{t("developer.responseMode")}</span>
                    <div role="group" aria-label={t("developer.responseMode")}>
                      <button type="button" aria-pressed={!streamRequested} className={!streamRequested ? "selected" : ""} onClick={() => setStream(false)}>{t("developer.standardResponse")}</button>
                      <button type="button" aria-pressed={streamRequested} className={streamRequested ? "selected" : ""} disabled={endpoint === "embeddings"} onClick={() => setStream(true)}>{t("developer.sseResponse")}</button>
                    </div>
                    {endpoint === "embeddings" && <small>{t("developer.embeddingsNoStream")}</small>}
                  </div>
                </>
              ) : (
                <Field label={t("developer.rawJSON")} hint={t("developer.rawJSONHint")} error={parsedJSON.error ? t("developer.invalidJSON") : undefined}>
                  <textarea className="developer-json-editor" rows={14} value={rawJSON} spellCheck={false} onChange={(event) => setRawJSON(event.target.value)} />
                </Field>
              )}
            </div>
            <div className="developer-request-summary" role="group" aria-label={t("developer.requestDetails")}>
              <div><small>{t("developer.method")}</small><strong>POST</strong></div>
              <div><small>{t("developer.path")}</small><code>{path}</code></div>
              <div><small>{t("developer.auth")}</small><strong>{t("developer.authValue")}</strong></div>
              <div><small>{t("developer.bodySize")}</small><strong>{requestLimit > 0 ? `${formatBytes(requestBytes)} / ${formatBytes(requestLimit)}` : formatBytes(requestBytes)}</strong></div>
            </div>
            <div className="developer-send-bar">
              <p className="developer-send-note">{t("developer.costReminder")}</p>
              {/* Send and cancel are separate controls: a double click on one toggling button
                  used to abort a request the gateway had already begun billing. */}
              <div className="developer-send-actions">
                <button type="button" className="button primary developer-send" disabled={readOnly || !canSend || running} aria-busy={running} onClick={execute}>
                  {t("developer.sendRequest")}
                </button>
                {running && (
                  <button type="button" className="button ghost developer-cancel" onClick={cancelExecution}>
                    {t("developer.cancelRequest")}
                  </button>
                )}
              </div>
              <p className="developer-send-missing" role="status">
                {running ? "" :
                  missingRequirements.length ? t("developer.missingRequirements", { fields: missingRequirements.join("、") }) :
                    oversized ? t("developer.requestTooLarge", { size: formatBytes(requestBytes), limit: formatBytes(requestLimit) }) : ""}
              </p>
            </div>
            </div>
          </section>

          <div className="developer-output-column">
            <section className="developer-code-panel" aria-labelledby="developer-code-heading">
              <header className="developer-panel-header compact">
                <div><p className="eyebrow">{t("developer.stepCode")}</p><h2 id="developer-code-heading">{t("developer.integrationCode")}</h2></div>
                <button className="button ghost" disabled={!code} onClick={copy}>{t("developer.copyCode")}</button>
              </header>
              {/* The toggle sits beside the tablist, not inside it: a tablist may only contain tabs. */}
              <div className="developer-code-tabbar">
                <div className="developer-code-tabs" role="tablist" aria-label={t("developer.codeLanguage")}>
                  {languages.map((item) => (
                    <button id={`developer-code-tab-${item}`} type="button" role="tab" tabIndex={language === item ? 0 : -1} aria-selected={language === item} aria-controls={language === item ? `developer-code-panel-${item}` : undefined} onKeyDown={(event) => moveTab(event, languages, language, selectLanguage, "developer-code-tab")} onClick={() => selectLanguage(item)} key={item}>{languageLabel(item)}</button>
                  ))}
                </div>
                <button
                  type="button"
                  className="button ghost developer-code-toggle"
                  aria-expanded={codeExpanded}
                  aria-controls={`developer-code-panel-${language}`}
                  onClick={() => setCodeExpanded((value) => !value)}
                >
                  <span>{codeExpanded ? t("developer.collapseCode") : t("developer.expandCode")}</span>
                  {/* Reserves the other label's width so toggling never resizes the bar. */}
                  <span aria-hidden="true">{codeExpanded ? t("developer.expandCode") : t("developer.collapseCode")}</span>
                </button>
              </div>
              <div id={`developer-code-panel-${language}`} role="tabpanel" aria-labelledby={`developer-code-tab-${language}`}>
                {codeExpanded && <pre className="developer-code"><code>{code || t("developer.codeUnavailable")}</code></pre>}
                <footer className="developer-code-footnote"><span>{t("developer.secretBoundary")}</span><span role="status" aria-live="polite">{copyStatus}</span></footer>
              </div>
            </section>

            <section className="developer-response-panel" aria-labelledby="developer-response-heading">
              <header className="developer-panel-header compact">
                <div><p className="eyebrow">{t("developer.stepResponse")}</p><h2 id="developer-response-heading">{t("developer.response")}</h2></div>
                <button className="button ghost" disabled={!execution.usageAvailable} onClick={() => navigate(`/admin/usage?request_id=${encodeURIComponent(execution.requestID)}`)}>{t("developer.openUsage")}</button>
              </header>
              <div className="developer-response-meta" role="group" aria-label={t("developer.responseMetadata")}>
                <div><small>{t("developer.httpStatus")}</small><strong>{execution.status ? `${execution.status} ${execution.statusText || ""}`.trim() : "—"}</strong></div>
                <div><small>{t("developer.requestID")}</small><code>{execution.requestID || "—"}</code></div>
                <div><small>{t("developer.latency")}</small><strong>{execution.latency == null ? running ? "…" : "—" : `${execution.latency} ms`}</strong></div>
                <div><small>{t("developer.delivery")}</small><strong>{responseStreaming ? "SSE" : t("developer.standardResponse")}</strong></div>
              </div>
              <div className="developer-response-tabs" role="tablist" aria-label={t("developer.responseViews")}>
                <button id="developer-response-tab-body" type="button" role="tab" tabIndex={responseView === "body" ? 0 : -1} aria-selected={responseView === "body"} aria-controls={responseView === "body" ? "developer-response-panel-body" : undefined} onKeyDown={(event) => moveTab(event, ["body", "headers"], responseView, setResponseView, "developer-response-tab")} onClick={() => setResponseView("body")}>{t("developer.responseBody")}</button>
                <button id="developer-response-tab-headers" type="button" role="tab" tabIndex={responseView === "headers" ? 0 : -1} aria-selected={responseView === "headers"} aria-controls={responseView === "headers" ? "developer-response-panel-headers" : undefined} onKeyDown={(event) => moveTab(event, ["body", "headers"], responseView, setResponseView, "developer-response-tab")} onClick={() => setResponseView("headers")}>{t("developer.responseHeaders")}</button>
              </div>
              <div id={`developer-response-panel-${responseView}`} role="tabpanel" aria-labelledby={`developer-response-tab-${responseView}`}>
                {/* The live region is mounted up front and empty while idle: a region inserted
                    together with its text is not announced by most screen readers. */}
                <div className={`developer-execution-status ${execution.outcome}`} role="status" aria-live="polite" aria-atomic="true">
                  {execution.outcome === "idle" ? "" : `${executionLabel}${execution.error && execution.error !== executionLabel ? ` · ${execution.error}` : ""}${executionHint ? ` · ${executionHint}` : ""}`}
                </div>
                {execution.outcome === "idle" ? <div className="developer-response-empty" data-view={responseView}>
                  <span aria-hidden="true">{responseView === "body" ? "{ }" : "H"}</span>
                  <div><strong>{t("developer.awaitingResponse")}</strong><p>{responseView === "body" ? t("developer.awaitingBody") : t("developer.awaitingHeaders")}</p></div>
                </div> : <div className="developer-response-result">
                  <pre tabIndex={0} role="region" aria-label={responseView === "body" ? t("developer.responseBody") : t("developer.responseHeaders")}><code>{responseView === "body" ? execution.body || (execution.outcome === "failed" ? execution.error : "") || t("developer.emptyResponseBody") : execution.headers || t("developer.awaitingHeaders")}</code></pre>
                </div>}
              </div>
              <footer className="developer-response-footnote">{t("developer.responseDescription")}</footer>
            </section>
          </div>
        </div>
      )}
    </section>
  );
}

function debugKeyTimestamp() {
  const now = new Date();
  const pad = (value: number) => String(value).padStart(2, "0");
  return `${now.getFullYear()}-${pad(now.getMonth() + 1)}-${pad(now.getDate())} ${pad(now.getHours())}:${pad(now.getMinutes())}:${pad(now.getSeconds())}`;
}

function executionEndpoint(endpoint: Endpoint) {
  return endpoint === "chat" ? "chat-completions" : endpoint;
}

function responseHeaders(headers: Headers) {
  const rows: string[] = [];
  const adminEnvelopeHeaders = new Set(["content-security-policy", "permissions-policy", "referrer-policy", "x-content-type-options"]);
  headers.forEach((value, name) => {
    if (!adminEnvelopeHeaders.has(name.toLowerCase())) rows.push(`${name}: ${value}`);
  });
  return rows.sort((left, right) => left.localeCompare(right)).join("\n");
}

// Admin errors are {"error": "..."} while the Gateway answers with OpenAI's
// {"error": {"message": ...}} envelope.
function adminEnvelopeError(raw: string) {
  try {
    const parsed: unknown = JSON.parse(raw);
    return !!parsed && typeof parsed === "object" && typeof (parsed as { error?: unknown }).error === "string";
  } catch {
    return false;
  }
}

function formatResponseBody(value: string) {
  try {
    return JSON.stringify(JSON.parse(value), null, 2);
  } catch {
    return value;
  }
}

function parseJSON(value: string): { value?: Record<string, unknown>; error: boolean } {
  try {
    const parsed: unknown = JSON.parse(value);
    return parsed && typeof parsed === "object" && !Array.isArray(parsed)
      ? { value: parsed as Record<string, unknown>, error: false }
      : { error: true };
  } catch {
    return { error: true };
  }
}

function requestBody(endpoint: Endpoint, model: string, input: string, stream: boolean, images: readonly ImageInput[]) {
  if (endpoint === "embeddings") return { model, input };
  if (endpoint === "chat") {
    const content = images.length
      ? [{ type: "text", text: input }, ...images.map((image) => ({ type: "image_url", image_url: { url: image.url } }))]
      : input;
    return { model, messages: [{ role: "user", content }], stream };
  }
  if (!images.length) return { model, input, stream };
  // Responses carries the image on the message item, and its content parts are decoded
  // strictly: image_url is the URL itself, not an object.
  const content = [
    { type: "input_text", text: input },
    ...images.map((image) => ({ type: "input_image", image_url: image.url })),
  ];
  return { model, input: [{ type: "message", role: "user", content }], stream };
}

/** The integration sample stands in for the base64 rather than reprinting it. */
function sampleImage(image: ImageInput): ImageInput {
  return image.inline ? { ...image, url: `data:${image.mediaType};base64,<BASE64_OF_${image.name}>` } : image;
}

function imageFromURL(value: string): ImageInput | null {
  const inline = /^data:(image\/[a-z0-9.+-]+);base64,[A-Za-z0-9+/]+={0,2}$/i.exec(value);
  if (inline) {
    const mediaType = inline[1].toLowerCase();
    return { id: crypto.randomUUID(), name: `image.${mediaType.slice("image/".length)}`, url: value, inline: true, mediaType, bytes: byteLength(value) };
  }
  try {
    const parsed = new URL(value);
    // Credentials in the URL would be handed to the provider verbatim and echoed back in
    // the code sample, so they are refused rather than carried.
    if ((parsed.protocol !== "http:" && parsed.protocol !== "https:") || parsed.username || parsed.password) return null;
    const name = parsed.pathname.split("/").filter(Boolean).pop();
    return { id: crypto.randomUUID(), name: name || parsed.host, url: value, inline: false, mediaType: "", bytes: byteLength(value) };
  } catch {
    return null;
  }
}

function readDataURL(file: File) {
  return new Promise<string>((resolve, reject) => {
    const reader = new FileReader();
    reader.onerror = () => reject(new Error("image file is unreadable"));
    reader.onload = () => typeof reader.result === "string" ? resolve(reader.result) : reject(new Error("image file is unreadable"));
    reader.readAsDataURL(file);
  });
}

function base64Bytes(size: number) {
  return Math.ceil(size / 3) * 4;
}

function byteLength(value: string) {
  return new TextEncoder().encode(value).length;
}

function formatBytes(value: number) {
  if (value < 1024) return `${value} B`;
  if (value < 1024 * 1024) return `${(value / 1024).toFixed(1)} KB`;
  return `${(value / 1024 / 1024).toFixed(1)} MB`;
}

function languageLabel(language: Language) {
  if (language === "javascript") return "JavaScript";
  if (language === "python") return "Python";
  if (language === "go") return "Go";
  if (language === "java") return "Java";
  return "curl";
}

function moveTab<T extends string>(event: KeyboardEvent<HTMLButtonElement>, items: readonly T[], current: T, select: (value: T) => void, idPrefix: string) {
  if (!["ArrowLeft", "ArrowRight", "Home", "End"].includes(event.key)) return;
  event.preventDefault();
  const currentIndex = items.indexOf(current);
  const nextIndex = event.key === "Home" ? 0 : event.key === "End" ? items.length - 1 :
    event.key === "ArrowRight" ? (currentIndex + 1) % items.length : (currentIndex - 1 + items.length) % items.length;
  const next = items[nextIndex];
  select(next);
  requestAnimationFrame(() => document.getElementById(idPrefix + "-" + next)?.focus());
}

function validGatewayBaseURL(value: string) {
  try {
    const parsed = new URL(value);
    return (parsed.protocol === "http:" || parsed.protocol === "https:") && !parsed.username && !parsed.password && !parsed.search && !parsed.hash;
  } catch {
    return false;
  }
}

// Java text blocks keep the JSON readable without escaping every quote. The closing
// delimiter has to sit on its own line, and any embedded backslash still needs escaping.
function javaTextBlock(json: string) {
  return `"""\n${json.replace(/\\/g, "\\\\").replace(/"""/g, '\\"\\"\\"')}\n"""`;
}

function shellQuote(value: string) {
  return "'" + value.replace(/'/g, "'\"'\"'") + "'";
}

function curlExample(url: string, json: string, streaming: boolean) {
  return [
    `curl${streaming ? " -N" : ""} ${shellQuote(url)}`,
    "  -H \"Authorization: Bearer $HALRO_API_KEY\"",
    "  -H \"Content-Type: application/json\"",
    `  --data-binary ${shellQuote(json)}`,
  ].join(" \\\n");
}

function codeExample(language: Language, baseURL: string, path: string, body: object) {
  const url = `${baseURL.replace(/\/$/, "")}${path}`;
  const json = JSON.stringify(body, null, 2);
  const streaming = "stream" in body && body.stream === true;
  if (language === "curl") return curlExample(url, json, streaming);
  if (language === "javascript" && streaming) return `const response = await fetch(${JSON.stringify(url)}, {\n  method: "POST",\n  headers: {\n    "Authorization": \`Bearer \${process.env.HALRO_API_KEY}\`,\n    "Content-Type": "application/json",\n  },\n  body: JSON.stringify(${json.replace(/\n/g, "\n  ")}),\n});\n\nconst reader = response.body.getReader();\nconst decoder = new TextDecoder();\nwhile (true) {\n  const { value, done } = await reader.read();\n  if (done) break;\n  console.log(decoder.decode(value, { stream: true }));\n}`;
  if (language === "python" && streaming) return `import json\nimport os\nimport requests\n\npayload = json.loads(${JSON.stringify(JSON.stringify(body))})\nresponse = requests.post(\n    ${JSON.stringify(url)},\n    headers={\n        "Authorization": f"Bearer {os.environ['HALRO_API_KEY']}",\n        "Content-Type": "application/json",\n    },\n    json=payload,\n    stream=True,\n)\nresponse.raise_for_status()\nfor line in response.iter_lines():\n    if line:\n        print(line.decode("utf-8"))`;
  if (language === "go" && streaming) return `payload := []byte(${JSON.stringify(JSON.stringify(body))})\nreq, _ := http.NewRequest(http.MethodPost, ${JSON.stringify(url)}, bytes.NewReader(payload))\nreq.Header.Set("Authorization", "Bearer "+os.Getenv("HALRO_API_KEY"))\nreq.Header.Set("Content-Type", "application/json")\n\nresp, err := http.DefaultClient.Do(req)\nif err != nil { log.Fatal(err) }\ndefer resp.Body.Close()\n\nscanner := bufio.NewScanner(resp.Body)\nfor scanner.Scan() {\n    fmt.Println(scanner.Text())\n}`;
  if (language === "java" && streaming) return `String payload = ${javaTextBlock(json)};\n\nHttpRequest request = HttpRequest.newBuilder()\n    .uri(URI.create(${JSON.stringify(url)}))\n    .header("Authorization", "Bearer " + System.getenv("HALRO_API_KEY"))\n    .header("Content-Type", "application/json")\n    .POST(HttpRequest.BodyPublishers.ofString(payload))\n    .build();\n\nHttpResponse<Stream<String>> response = HttpClient.newHttpClient()\n    .send(request, HttpResponse.BodyHandlers.ofLines());\nresponse.body().forEach(System.out::println);`;
  if (language === "javascript") return `const response = await fetch(${JSON.stringify(url)}, {\n  method: "POST",\n  headers: {\n    "Authorization": \`Bearer \${process.env.HALRO_API_KEY}\`,\n    "Content-Type": "application/json",\n  },\n  body: JSON.stringify(${json.replace(/\n/g, "\n  ")}),\n});\n\nconsole.log(await response.json());`;
  if (language === "python") return `import json\nimport os\nimport requests\n\npayload = json.loads(${JSON.stringify(JSON.stringify(body))})\nresponse = requests.post(\n    ${JSON.stringify(url)},\n    headers={\n        "Authorization": f"Bearer {os.environ['HALRO_API_KEY']}",\n        "Content-Type": "application/json",\n    },\n    json=payload,\n)\nresponse.raise_for_status()\nprint(response.json())`;
  if (language === "go") return `payload := []byte(${JSON.stringify(JSON.stringify(body))})\nreq, _ := http.NewRequest(http.MethodPost, ${JSON.stringify(url)}, bytes.NewReader(payload))\nreq.Header.Set("Authorization", "Bearer "+os.Getenv("HALRO_API_KEY"))\nreq.Header.Set("Content-Type", "application/json")\n\nresp, err := http.DefaultClient.Do(req)`;
  if (language === "java") return `String payload = ${javaTextBlock(json)};\n\nHttpRequest request = HttpRequest.newBuilder()\n    .uri(URI.create(${JSON.stringify(url)}))\n    .header("Authorization", "Bearer " + System.getenv("HALRO_API_KEY"))\n    .header("Content-Type", "application/json")\n    .POST(HttpRequest.BodyPublishers.ofString(payload))\n    .build();\n\nHttpResponse<String> response = HttpClient.newHttpClient()\n    .send(request, HttpResponse.BodyHandlers.ofString());\nSystem.out.println(response.body());`;
  throw new Error("unsupported code language");
}
