import { useEffect, useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { Link } from "../../navigation";
import type { GatewayKey, Outcome, OutcomeDefinition, Run, WorkUnit } from "../../types";

type StepState = "complete" | "current" | "blocked" | "error";

interface GuideStep {
  key: "project" | "definition" | "keys" | "workUnit" | "run" | "outcome";
  state: StepState;
}

const orchestrationScopes = ["inference", "work_unit:create", "run:create", "run:attach"] as const;

function activeKey(key: GatewayKey, now: number) {
  return key.enabled && (!key.expires_at || Date.parse(key.expires_at) > now);
}

export function governanceGuideSteps({
  projectEnabled,
  definitions,
  keys,
  workUnits,
  runs,
  outcomes,
  keysUnavailable = false,
  now = Date.now(),
}: {
  projectEnabled: boolean;
  definitions: OutcomeDefinition[];
  keys: GatewayKey[];
  workUnits: WorkUnit[];
  runs: Run[];
  outcomes: Outcome[];
  keysUnavailable?: boolean;
  now?: number;
}): GuideStep[] {
  const enabledKeys = keys.filter((key) => activeKey(key, now));
  const hasOrchestrationKey = enabledKeys.some((key) => {
    const scopes = key.scopes ?? ["inference"];
    return orchestrationScopes.every((scope) => scopes.includes(scope));
  });
  const hasOutcomeKey = enabledKeys.some((key) => (key.scopes ?? ["inference"]).includes("outcome:write"));
  const completed = [
    projectEnabled,
    definitions.some((item) => item.enabled),
    hasOrchestrationKey && hasOutcomeKey,
    workUnits.length > 0,
    runs.length > 0,
    outcomes.some((item) => !item.provisional),
  ];
  const current = completed.findIndex((value) => !value);
  const names: GuideStep["key"][] = ["project", "definition", "keys", "workUnit", "run", "outcome"];
  return names.map((key, index) => ({
    key,
    state: completed[index] ? "complete" : index === current ? (key === "keys" && keysUnavailable ? "error" : "current") : "blocked",
  }));
}

function CodeSample({ title, code }: { title: string; code: string }) {
  const { t } = useTranslation();
  const [copyState, setCopyState] = useState<"idle" | "copied" | "failed">("idle");
  const copy = async () => {
    try {
      await navigator.clipboard.writeText(code);
      setCopyState("copied");
    } catch {
      setCopyState("failed");
    }
  };
  return <article className="governance-guide-sample">
    <header><strong>{title}</strong><button type="button" className="button ghost" onClick={() => void copy()}>{t(`runGovernance.firstRun.copy.${copyState}`)}</button></header>
    <pre><code>{code}</code></pre>
  </article>;
}

export function GovernanceFirstRunGuide({
  projectID,
  projectEnabled,
  definitions,
  keys,
  keysUnavailable,
  workUnits,
  runs,
  outcomes,
  onOpenDefinitions,
}: {
  projectID: string;
  projectEnabled: boolean;
  definitions: OutcomeDefinition[];
  keys: GatewayKey[];
  keysUnavailable: boolean;
  workUnits: WorkUnit[];
  runs: Run[];
  outcomes: Outcome[];
  onOpenDefinitions: () => void;
}) {
  const { t } = useTranslation();
  const steps = useMemo(() => governanceGuideSteps({ projectEnabled, definitions, keys, keysUnavailable, workUnits, runs, outcomes }), [definitions, keys, keysUnavailable, outcomes, projectEnabled, runs, workUnits]);
  const completed = steps.filter((step) => step.state === "complete").length;
  const allComplete = completed === steps.length;
  const current = steps.find((step) => step.state === "current" || step.state === "error") ?? steps[steps.length - 1];
  const apiStep = !allComplete && (current.key === "workUnit" || current.key === "run" || current.key === "outcome");
  const [apiOpen, setAPIOpen] = useState(apiStep);
  useEffect(() => { if (apiStep) setAPIOpen(true); }, [apiStep]);
  const progressText = t("runGovernance.firstRun.progress", { done: completed, total: steps.length });
  const selectedDefinition = [...definitions].filter((item) => item.enabled).sort((a, b) => b.version - a.version)[0];
  const definitionID = selectedDefinition?.id ?? "odef_xxx";
  const outcomeValue = selectedDefinition?.success_values[0] ?? "accepted";
  const observedAt = useMemo(() => new Date().toISOString(), []);
  const projectHref = projectID ? `/admin/projects?project_id=${encodeURIComponent(projectID)}` : "/admin/projects";

  const workUnitCode = `curl "$HALRO_GATEWAY_URL/halro/v1/work-units" \\
  -H "Authorization: Bearer $HALRO_ORCHESTRATOR_KEY" \\
  -H "Content-Type: application/json" \\
  -H "Idempotency-Key: first-work-unit-v1" \\
  -d '${JSON.stringify({ outcome_definition_ids: [definitionID] })}'`;
  const runCode = `curl "$HALRO_GATEWAY_URL/halro/v1/runs" \\
  -H "Authorization: Bearer $HALRO_ORCHESTRATOR_KEY" \\
  -H "Content-Type: application/json" \\
  -H "Idempotency-Key: first-run-v1" \\
  -d '{"work_unit_id":"wku_xxx"}'`;
  const inferenceCode = `curl "$HALRO_GATEWAY_URL/v1/chat/completions" \\
  -H "Authorization: Bearer $HALRO_ORCHESTRATOR_KEY" \\
  -H "Content-Type: application/json" \\
  -H "X-Halro-Run-ID: run_xxx" \\
  -d '{"model":"YOUR_MODEL_ALIAS","messages":[{"role":"user","content":"Hello"}]}'`;
  const outcomeCode = `curl "$HALRO_GATEWAY_URL/halro/v1/runs/run_xxx/close" \\
  -H "Authorization: Bearer $HALRO_ORCHESTRATOR_KEY" \\
  -H "Content-Type: application/json" \\
  -H "Idempotency-Key: first-run-close-v1" \\
  -d '{"reason":"completed"}'

curl "$HALRO_GATEWAY_URL/halro/v1/work-units/wku_xxx/close" \\
  -H "Authorization: Bearer $HALRO_ORCHESTRATOR_KEY" \\
  -H "Content-Type: application/json" \\
  -H "Idempotency-Key: first-work-unit-close-v1" \\
  -d '{}'

curl "$HALRO_GATEWAY_URL/halro/v1/work-units/wku_xxx/outcomes" \\
  -H "Authorization: Bearer $HALRO_ACCEPTANCE_KEY" \\
  -H "Content-Type: application/json" \\
  -H "Idempotency-Key: first-outcome-v1" \\
  -d '${JSON.stringify({ definition_id: definitionID, value: outcomeValue, observed_at: observedAt })}'`;

  const action = allComplete ? null : current.key === "project" || current.key === "keys"
    ? <Link className="button primary first-run-action-button" href={projectHref}>{t(`runGovernance.firstRun.steps.${current.key}.action`)}<span className="first-run-action-arrow" aria-hidden="true">→</span></Link>
    : current.key === "definition"
      ? <button type="button" className="button primary first-run-action-button" onClick={onOpenDefinitions}>{t("runGovernance.firstRun.steps.definition.action")}<span className="first-run-action-arrow" aria-hidden="true">→</span></button>
      : <a className="button primary first-run-action-button" href="#governance-first-api">{t("runGovernance.firstRun.openAPI")}<span className="first-run-action-arrow" aria-hidden="true">↓</span></a>;

  return <section className="panel first-run-panel governance-first-run" aria-labelledby="governance-first-run-title">
    <header className="first-run-header">
      <div><p className="eyebrow">{t("runGovernance.firstRun.eyebrow")}</p><h2 id="governance-first-run-title">{t("runGovernance.firstRun.title")}</h2><p className="first-run-description">{t("runGovernance.firstRun.description")}</p></div>
      <div className="first-run-progress-shell"><strong>{progressText}</strong><div className="first-run-progress-track" role="progressbar" aria-label={t("runGovernance.firstRun.progressLabel")} aria-valuemin={0} aria-valuemax={steps.length} aria-valuenow={completed} aria-valuetext={progressText}><span style={{ width: `${completed / steps.length * 100}%` }} /></div></div>
    </header>
    <ol className="first-run-goals">
      {steps.map((step, index) => <li className={`first-run-goal ${step.state}`} aria-current={step.state === "current" || step.state === "error" ? "step" : undefined} key={step.key}>
        <span className="first-run-goal-marker" aria-hidden="true">{step.state === "complete" ? "✓" : index + 1}</span>
        <span className="first-run-goal-copy"><strong>{t(`runGovernance.firstRun.steps.${step.key}.title`)}</strong><small>{t(`runGovernance.firstRun.steps.${step.key}.detail`)}</small></span>
        <span className={`first-run-goal-state ${step.state}`}>{t(`runGovernance.firstRun.states.${step.state}`)}</span>
      </li>)}
    </ol>
    <footer className="first-run-action"><div><span>{t(allComplete ? "runGovernance.firstRun.finished" : "runGovernance.firstRun.next")}</span><strong>{t(allComplete ? "runGovernance.firstRun.finishedTitle" : `runGovernance.firstRun.steps.${current.key}.title`)}</strong><small>{t(allComplete ? "runGovernance.firstRun.finishedDescription" : `runGovernance.firstRun.steps.${current.key}.${current.state === "error" ? "error" : "detail"}`)}</small></div>{action}</footer>
    {projectEnabled && <details className="governance-guide-api" id="governance-first-api" open={apiOpen} onToggle={(event) => setAPIOpen(event.currentTarget.open)}>
      <summary><span><strong>{t("runGovernance.firstRun.apiTitle")}</strong><small>{t("runGovernance.firstRun.apiDescription")}</small></span></summary>
      <div className="governance-guide-api-body"><p className="notice warning">{t("runGovernance.firstRun.apiBoundary")}</p><CodeSample title={t("runGovernance.firstRun.samples.workUnit")} code={workUnitCode} /><CodeSample title={t("runGovernance.firstRun.samples.run")} code={runCode} /><CodeSample title={t("runGovernance.firstRun.samples.inference")} code={inferenceCode} /><CodeSample title={t("runGovernance.firstRun.samples.outcome")} code={outcomeCode} /></div>
    </details>}
  </section>;
}
