import { useQuery } from "@tanstack/react-query";
import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { EmptyState, ErrorState, Field, Loading, PageHeader } from "../components";

type Endpoint = "responses" | "chat" | "embeddings";
type Language = "curl" | "javascript" | "python" | "go";

const languages: Language[] = ["curl", "javascript", "python", "go"];

export function DeveloperPage() {
  const { t } = useTranslation();
  const projects = useQuery({ queryKey: ["projects"], queryFn: api.projects });
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
  const [gatewayURL, setGatewayURL] = useState(() => typeof window === "undefined" ? "http://127.0.0.1:8080" : window.location.origin);
  const [language, setLanguage] = useState<Language>("curl");
  const [copyStatus, setCopyStatus] = useState("");

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

  const body = useMemo(() => requestBody(endpoint, model, input, stream), [endpoint, input, model, stream]);
  const path = endpoint === "chat" ? "/v1/chat/completions" : `/v1/${endpoint}`;
  const code = useMemo(() => codeExample(language, gatewayURL, path, body), [body, gatewayURL, language, path]);
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(code);
      setCopyStatus(t("developer.copied"));
    } catch {
      setCopyStatus(t("developer.copyFailed"));
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
                <select value={endpoint} onChange={(event) => setEndpoint(event.target.value as Endpoint)}>
                  <option value="responses">{t("developer.endpointResponses")}</option>
                  <option value="chat">{t("developer.endpointChat")}</option>
                  <option value="embeddings">{t("developer.endpointEmbeddings")}</option>
                </select>
              </Field>
              <Field label={t("developer.gatewayURL")} hint={t("developer.gatewayURLHint")}>
                <input type="url" value={gatewayURL} onChange={(event) => setGatewayURL(event.target.value)} spellCheck={false} />
              </Field>
              <Field label={t("developer.input")}>
                <textarea rows={6} value={input} placeholder={t("developer.inputPlaceholder")} onChange={(event) => setInput(event.target.value)} />
              </Field>
              <label className={`developer-stream-toggle ${endpoint === "embeddings" ? "disabled" : ""}`}>
                <input type="checkbox" checked={stream} disabled={endpoint === "embeddings"} onChange={(event) => setStream(event.target.checked)} />
                <span><strong>{t("developer.streaming")}</strong><small>SSE</small></span>
              </label>
            </div>
            <div className="developer-request-summary" aria-label={t("developer.requestDetails")}>
              <div><small>{t("developer.method")}</small><strong>POST</strong></div>
              <div><small>Endpoint</small><code>{path}</code></div>
              <div><small>{t("developer.auth")}</small><strong>{t("developer.authValue")}</strong></div>
            </div>
            <button className="button primary developer-send" disabled>{t("developer.sendPending")}</button>
          </section>

          <div className="developer-output-column">
            <section className="developer-code-panel" aria-labelledby="developer-code-heading">
              <header className="developer-panel-header compact">
                <div><p className="eyebrow">02 / CODE</p><h2 id="developer-code-heading">{t("developer.integrationCode")}</h2></div>
                <button className="button ghost" onClick={copy}>{t("developer.copyCode")}</button>
              </header>
              <div className="developer-code-tabs" role="tablist" aria-label={t("developer.codeLanguage")}>
                {languages.map((item) => (
                  <button role="tab" aria-selected={language === item} onClick={() => { setLanguage(item); setCopyStatus(""); }} key={item}>{languageLabel(item)}</button>
                ))}
              </div>
              <pre className="developer-code"><code>{code}</code></pre>
              <footer><span>{t("developer.secretBoundary")}</span><span role="status" aria-live="polite">{copyStatus}</span></footer>
            </section>

            <section className="developer-response-panel" aria-labelledby="developer-response-heading">
              <header className="developer-panel-header compact">
                <div><p className="eyebrow">03 / RESPONSE</p><h2 id="developer-response-heading">{t("developer.response")}</h2></div>
                <p>{t("developer.responseDescription")}</p>
              </header>
              <div className="developer-response-empty">
                <span aria-hidden="true">{"{ }"}</span>
                <div><strong>{t("developer.awaitingResponse")}</strong><p>{t("developer.awaitingResponseDescription")}</p></div>
              </div>
            </section>
          </div>
        </div>
      )}
    </section>
  );
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

function codeExample(language: Language, baseURL: string, path: string, body: object) {
  const url = `${baseURL.replace(/\/$/, "")}${path}`;
  const json = JSON.stringify(body, null, 2);
  if (language === "javascript") return `const response = await fetch(${JSON.stringify(url)}, {\n  method: "POST",\n  headers: {\n    "Authorization": \`Bearer \${process.env.HEIMDALL_API_KEY}\`,\n    "Content-Type": "application/json",\n  },\n  body: JSON.stringify(${json.replace(/\n/g, "\n  ")}),\n});\n\nconsole.log(await response.json());`;
  if (language === "python") return `import os\nimport requests\n\nresponse = requests.post(\n    ${JSON.stringify(url)},\n    headers={\n        "Authorization": f"Bearer {os.environ['HEIMDALL_API_KEY']}",\n        "Content-Type": "application/json",\n    },\n    json=${json.replace(/true/g, "True").replace(/false/g, "False")},\n)\nresponse.raise_for_status()\nprint(response.json())`;
  if (language === "go") return `payload := []byte(${JSON.stringify(JSON.stringify(body))})\nreq, _ := http.NewRequest(http.MethodPost, ${JSON.stringify(url)}, bytes.NewReader(payload))\nreq.Header.Set("Authorization", "Bearer "+os.Getenv("HEIMDALL_API_KEY"))\nreq.Header.Set("Content-Type", "application/json")\n\nresp, err := http.DefaultClient.Do(req)`;
  return `curl ${JSON.stringify(url)} \\\n  -H "Authorization: Bearer $HEIMDALL_API_KEY" \\\n  -H "Content-Type: application/json" \\\n  -d '${json}'`;
}
