import { useQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useState, type KeyboardEvent } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { EmptyState, ErrorState, Field, Loading, PageHeader } from "../components";

type Endpoint = "responses" | "chat" | "embeddings";
type Language = "curl" | "javascript" | "python" | "go";
type RequestMode = "form" | "json";
type ResponseView = "body" | "headers";

const languages: Language[] = ["curl", "javascript", "python", "go"];

export function DeveloperPage() {
  const { t } = useTranslation();
  const projects = useQuery({ queryKey: ["projects"], queryFn: api.projects });
  const developerConfig = useQuery({ queryKey: ["developer-config"], queryFn: api.developerConfig });
  const availableProjects = useMemo(
    () => (projects.data?.items ?? []).filter((project) => project.enabled && project.allowed_routes.length > 0),
    [projects.data?.items],
  );
  const [projectID, setProjectID] = useState("");
  const selectedProject = availableProjects.find((project) => project.id === projectID) ?? availableProjects[0];
  const [model, setModel] = useState("");
  const [endpoint, setEndpoint] = useState<Endpoint>("responses");
  const [input, setInput] = useState("Explain how Heimdall routes this request in one sentence.");
  const [stream, setStream] = useState(true);
  const [requestMode, setRequestMode] = useState<RequestMode>("form");
  const [gatewayKey, setGatewayKey] = useState("");
  const [showGatewayKey, setShowGatewayKey] = useState(false);
  const [gatewayURL, setGatewayURL] = useState("");
  const [language, setLanguage] = useState<Language>("curl");
  const [copyStatus, setCopyStatus] = useState("");
  const [responseView, setResponseView] = useState<ResponseView>("body");

  useEffect(() => {
    if (selectedProject && selectedProject.id !== projectID) setProjectID(selectedProject.id);
  }, [projectID, selectedProject]);
  useEffect(() => {
    const routes = selectedProject?.allowed_routes ?? [];
    if (!routes.includes(model)) setModel(routes[0] ?? "");
  }, [model, selectedProject]);
  useEffect(() => {
    if (endpoint === "embeddings" && stream) setStream(false);
  }, [endpoint, stream]);
  useEffect(() => {
    if (!gatewayURL && developerConfig.data?.gateway_base_url) setGatewayURL(developerConfig.data.gateway_base_url);
  }, [developerConfig.data?.gateway_base_url, gatewayURL]);

  const formBody = useMemo(() => requestBody(endpoint, model, input, stream), [endpoint, input, model, stream]);
  const [rawJSON, setRawJSON] = useState(() => JSON.stringify(requestBody("responses", "", "Explain how Heimdall routes this request in one sentence.", true), null, 2));
  const parsedJSON = useMemo(() => parseJSON(rawJSON), [rawJSON]);
  const body = requestMode === "json" ? parsedJSON.value : formBody;
  const isStreaming = endpoint !== "embeddings" && body?.stream === true;
  const path = endpoint === "chat" ? "/v1/chat/completions" : `/v1/${endpoint}`;
  const gatewayURLValid = validGatewayBaseURL(gatewayURL);
  const code = useMemo(() => body && gatewayURLValid ? codeExample(language, gatewayURL, path, body) : "", [body, gatewayURL, gatewayURLValid, language, path]);
  const copy = async () => {
    if (!code) return;
    try {
      await navigator.clipboard.writeText(code);
      setCopyStatus(t("developer.copied"));
    } catch {
      setCopyStatus(t("developer.copyFailed"));
    }
  };
  const selectRequestMode = (mode: RequestMode) => {
    if (mode === "json" && requestMode !== "json") setRawJSON(JSON.stringify(formBody, null, 2));
    setRequestMode(mode);
  };
  const selectEndpoint = (next: Endpoint) => {
    const nextStream = next === "embeddings" ? false : stream;
    setEndpoint(next);
    setStream(nextStream);
    if (requestMode === "json") setRawJSON(JSON.stringify(requestBody(next, model, input, nextStream), null, 2));
  };

  return (
    <section className="developer-page">
      <PageHeader
        eyebrow={t("developer.eyebrow")}
        title={t("developer.title")}
        description={t("developer.description")}
        action={<span className="badge developer-preview-badge">{t("developer.previewBadge")}</span>}
      />
      <div className="notice warning developer-preview-notice" role="status">{t("developer.previewNotice")}</div>
      {projects.isPending && <Loading label={t("developer.loading")} />}
      {projects.isError && <ErrorState error={projects.error} />}
      {projects.isSuccess && availableProjects.length === 0 && (
        <EmptyState title={t("developer.noProjects")}>{t("developer.noProjectsDescription")}</EmptyState>
      )}
      {selectedProject && (
        <div className="developer-workbench">
          <section className="developer-config-panel" aria-labelledby="developer-request-setup">
            <header className="developer-panel-header">
              <div><p className="eyebrow">01 / REQUEST</p><h2 id="developer-request-setup">{t("developer.requestSetup")}</h2></div>
              <p>{t("developer.requestSetupDescription")}</p>
            </header>
            <div className="developer-mode-tabs" role="tablist" aria-label={t("developer.requestMode")}>
              <button id="developer-request-tab-form" type="button" role="tab" tabIndex={requestMode === "form" ? 0 : -1} aria-selected={requestMode === "form"} aria-controls="developer-request-panel-form" onKeyDown={(event) => moveTab(event, ["form", "json"], requestMode, selectRequestMode, "developer-request-tab")} onClick={() => selectRequestMode("form")}>{t("developer.formMode")}</button>
              <button id="developer-request-tab-json" type="button" role="tab" tabIndex={requestMode === "json" ? 0 : -1} aria-selected={requestMode === "json"} aria-controls="developer-request-panel-json" onKeyDown={(event) => moveTab(event, ["form", "json"], requestMode, selectRequestMode, "developer-request-tab")} onClick={() => selectRequestMode("json")}>{t("developer.jsonMode")}</button>
            </div>
            <div id={`developer-request-panel-${requestMode}`} role="tabpanel" aria-labelledby={`developer-request-tab-${requestMode}`}>
            <div className="developer-config-fields">
              <Field label={t("developer.project")}>
                <select value={selectedProject.id} onChange={(event) => setProjectID(event.target.value)}>
                  {availableProjects.map((project) => <option value={project.id} key={project.id}>{project.name}</option>)}
                </select>
              </Field>
              <Field label={t("developer.publicModel")}>
                <select value={model} onChange={(event) => setModel(event.target.value)}>
                  {selectedProject.allowed_routes.map((route) => <option value={route} key={route}>{route}</option>)}
                </select>
              </Field>
              <Field label={t("developer.endpoint")}>
                <select value={endpoint} onChange={(event) => selectEndpoint(event.target.value as Endpoint)}>
                  <option value="responses">{t("developer.endpointResponses")}</option>
                  <option value="chat">{t("developer.endpointChat")}</option>
                  <option value="embeddings">{t("developer.endpointEmbeddings")}</option>
                </select>
              </Field>
              <Field label={t("developer.gatewayURL")} hint={t("developer.gatewayURLHint")} error={gatewayURL && !gatewayURLValid ? t("developer.invalidGatewayURL") : undefined}>
                <input type="url" value={gatewayURL} onChange={(event) => setGatewayURL(event.target.value)} spellCheck={false} />
              </Field>
              <div className="field developer-key-field">
                <label htmlFor="developer-gateway-key">{t("developer.gatewayKey")}</label>
                <div className="developer-secret-input">
                  <input id="developer-gateway-key" type={showGatewayKey ? "text" : "password"} value={gatewayKey} autoComplete="off" placeholder={t("developer.gatewayKeyPlaceholder")} onChange={(event) => setGatewayKey(event.target.value)} />
                  <button type="button" className="button ghost" onClick={() => setShowGatewayKey((value) => !value)}>{showGatewayKey ? t("developer.hideKey") : t("developer.showKey")}</button>
                </div>
                <small>{t("developer.gatewayKeyHint")}</small>
              </div>
              {requestMode === "form" ? (
                <>
                  <Field label={t("developer.input")}>
                    <textarea rows={6} value={input} placeholder={t("developer.inputPlaceholder")} onChange={(event) => setInput(event.target.value)} />
                  </Field>
                  <div className="developer-response-mode">
                    <span>{t("developer.responseMode")}</span>
                    <div role="group" aria-label={t("developer.responseMode")}>
                      <button type="button" className={!stream ? "selected" : ""} onClick={() => setStream(false)}>{t("developer.standardResponse")}</button>
                      <button type="button" className={stream ? "selected" : ""} disabled={endpoint === "embeddings"} onClick={() => setStream(true)}>{t("developer.sseResponse")}</button>
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
            <div className="developer-request-summary" aria-label={t("developer.requestDetails")}>
              <div><small>{t("developer.method")}</small><strong>POST</strong></div>
              <div><small>Endpoint</small><code>{path}</code></div>
              <div><small>{t("developer.auth")}</small><strong>{t("developer.authValue")}</strong></div>
            </div>
            <button className="button primary developer-send" disabled>{t("developer.sendPending")}</button>
            </div>
          </section>

          <div className="developer-output-column">
            <section className="developer-code-panel" aria-labelledby="developer-code-heading">
              <header className="developer-panel-header compact">
                <div><p className="eyebrow">02 / CODE</p><h2 id="developer-code-heading">{t("developer.integrationCode")}</h2></div>
                <button className="button ghost" disabled={!code} onClick={copy}>{t("developer.copyCode")}</button>
              </header>
              <div className="developer-code-tabs" role="tablist" aria-label={t("developer.codeLanguage")}>
                {languages.map((item) => (
                  <button id={`developer-code-tab-${item}`} type="button" role="tab" tabIndex={language === item ? 0 : -1} aria-selected={language === item} aria-controls={`developer-code-panel-${item}`} onKeyDown={(event) => moveTab(event, languages, language, (next) => { setLanguage(next); setCopyStatus(""); }, "developer-code-tab")} onClick={() => { setLanguage(item); setCopyStatus(""); }} key={item}>{languageLabel(item)}</button>
                ))}
              </div>
              <div id={`developer-code-panel-${language}`} role="tabpanel" aria-labelledby={`developer-code-tab-${language}`}>
                <pre className="developer-code"><code>{code || t("developer.codeUnavailable")}</code></pre>
                <footer><span>{t("developer.secretBoundary")}</span><span role="status" aria-live="polite">{copyStatus}</span></footer>
              </div>
            </section>

            <section className="developer-response-panel" aria-labelledby="developer-response-heading">
              <header className="developer-panel-header compact">
                <div><p className="eyebrow">03 / RESPONSE</p><h2 id="developer-response-heading">{t("developer.response")}</h2></div>
                <button className="button ghost" disabled>{t("developer.openUsage")}</button>
              </header>
              <div className="developer-response-meta" aria-label={t("developer.responseMetadata")}>
                <div><small>{t("developer.httpStatus")}</small><strong>—</strong></div>
                <div><small>Request ID</small><code>—</code></div>
                <div><small>{t("developer.latency")}</small><strong>— ms</strong></div>
                <div><small>{t("developer.delivery")}</small><strong>{isStreaming ? "SSE" : t("developer.standardResponse")}</strong></div>
              </div>
              <div className="developer-response-tabs" role="tablist" aria-label={t("developer.responseViews")}>
                <button id="developer-response-tab-body" type="button" role="tab" tabIndex={responseView === "body" ? 0 : -1} aria-selected={responseView === "body"} aria-controls="developer-response-panel-body" onKeyDown={(event) => moveTab(event, ["body", "headers"], responseView, setResponseView, "developer-response-tab")} onClick={() => setResponseView("body")}>{t("developer.responseBody")}</button>
                <button id="developer-response-tab-headers" type="button" role="tab" tabIndex={responseView === "headers" ? 0 : -1} aria-selected={responseView === "headers"} aria-controls="developer-response-panel-headers" onKeyDown={(event) => moveTab(event, ["body", "headers"], responseView, setResponseView, "developer-response-tab")} onClick={() => setResponseView("headers")}>{t("developer.responseHeaders")}</button>
              </div>
              <div id={`developer-response-panel-${responseView}`} role="tabpanel" aria-labelledby={`developer-response-tab-${responseView}`}>
                <div className="developer-response-empty" data-view={responseView}>
                  <span aria-hidden="true">{responseView === "body" ? "{ }" : "H"}</span>
                  <div><strong>{t("developer.awaitingResponse")}</strong><p>{responseView === "body" ? t("developer.awaitingBody") : t("developer.awaitingHeaders")}</p></div>
                </div>
              </div>
              <footer className="developer-response-footnote">{t("developer.responseDescription")}</footer>
            </section>
          </div>
        </div>
      )}
    </section>
  );
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

function requestBody(endpoint: Endpoint, model: string, input: string, stream: boolean) {
  if (endpoint === "chat") return { model, messages: [{ role: "user", content: input }], stream };
  if (endpoint === "embeddings") return { model, input };
  return { model, input, stream };
}

function languageLabel(language: Language) {
  if (language === "javascript") return "JavaScript";
  if (language === "python") return "Python";
  if (language === "go") return "Go";
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

function shellQuote(value: string) {
  return "'" + value.replace(/'/g, "'\"'\"'") + "'";
}

function curlExample(url: string, json: string, streaming: boolean) {
  return [
    `curl${streaming ? " -N" : ""} ${shellQuote(url)}`,
    "  -H \"Authorization: Bearer $HEIMDALL_API_KEY\"",
    "  -H \"Content-Type: application/json\"",
    `  --data-binary ${shellQuote(json)}`,
  ].join(" \\\n");
}

function codeExample(language: Language, baseURL: string, path: string, body: object) {
  const url = `${baseURL.replace(/\/$/, "")}${path}`;
  const json = JSON.stringify(body, null, 2);
  const streaming = "stream" in body && body.stream === true;
  if (language === "curl") return curlExample(url, json, streaming);
  if (language === "javascript" && streaming) return `const response = await fetch(${JSON.stringify(url)}, {\n  method: "POST",\n  headers: {\n    "Authorization": \`Bearer \${process.env.HEIMDALL_API_KEY}\`,\n    "Content-Type": "application/json",\n  },\n  body: JSON.stringify(${json.replace(/\n/g, "\n  ")}),\n});\n\nconst reader = response.body.getReader();\nconst decoder = new TextDecoder();\nwhile (true) {\n  const { value, done } = await reader.read();\n  if (done) break;\n  console.log(decoder.decode(value, { stream: true }));\n}`;
  if (language === "python" && streaming) return `import json\nimport os\nimport requests\n\npayload = json.loads(${JSON.stringify(JSON.stringify(body))})\nresponse = requests.post(\n    ${JSON.stringify(url)},\n    headers={\n        "Authorization": f"Bearer {os.environ['HEIMDALL_API_KEY']}",\n        "Content-Type": "application/json",\n    },\n    json=payload,\n    stream=True,\n)\nresponse.raise_for_status()\nfor line in response.iter_lines():\n    if line:\n        print(line.decode("utf-8"))`;
  if (language === "go" && streaming) return `payload := []byte(${JSON.stringify(JSON.stringify(body))})\nreq, _ := http.NewRequest(http.MethodPost, ${JSON.stringify(url)}, bytes.NewReader(payload))\nreq.Header.Set("Authorization", "Bearer "+os.Getenv("HEIMDALL_API_KEY"))\nreq.Header.Set("Content-Type", "application/json")\n\nresp, err := http.DefaultClient.Do(req)\nif err != nil { log.Fatal(err) }\ndefer resp.Body.Close()\n\nscanner := bufio.NewScanner(resp.Body)\nfor scanner.Scan() {\n    fmt.Println(scanner.Text())\n}`;
  if (language === "javascript") return `const response = await fetch(${JSON.stringify(url)}, {\n  method: "POST",\n  headers: {\n    "Authorization": \`Bearer \${process.env.HEIMDALL_API_KEY}\`,\n    "Content-Type": "application/json",\n  },\n  body: JSON.stringify(${json.replace(/\n/g, "\n  ")}),\n});\n\nconsole.log(await response.json());`;
  if (language === "python") return `import json\nimport os\nimport requests\n\npayload = json.loads(${JSON.stringify(JSON.stringify(body))})\nresponse = requests.post(\n    ${JSON.stringify(url)},\n    headers={\n        "Authorization": f"Bearer {os.environ['HEIMDALL_API_KEY']}",\n        "Content-Type": "application/json",\n    },\n    json=payload,\n)\nresponse.raise_for_status()\nprint(response.json())`;
  if (language === "go") return `payload := []byte(${JSON.stringify(JSON.stringify(body))})\nreq, _ := http.NewRequest(http.MethodPost, ${JSON.stringify(url)}, bytes.NewReader(payload))\nreq.Header.Set("Authorization", "Bearer "+os.Getenv("HEIMDALL_API_KEY"))\nreq.Header.Set("Content-Type", "application/json")\n\nresp, err := http.DefaultClient.Do(req)`;
  throw new Error("unsupported code language");
}
