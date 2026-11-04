// What Halro's own numbers say about each other.
//
// Most of what an operator has to be told after a bad afternoon is arithmetic
// this process already holds — a request that failed at 60021ms under a 1m0s
// attempt deadline is its own timeout firing, a four-candidate fan-out behind a
// budget that reaches two is two routes that are never called. Both are one
// subtraction and one division, and until this panel neither was said anywhere
// by anything.
//
// It sits in Diagnostics because it is the console's half of `halro doctor`:
// the same rules, against a running process that can also answer what the
// admission gate is holding and how its refusals classified.
//
// Two rules the panel keeps to:
//
// Every rule keeps its row, including the quiet ones. A panel that shows only
// problems cannot distinguish "checked and fine" from "never ran", and the
// second is the state worth knowing about — which is why `unknown` is its own
// status rather than being rendered as a pass.
//
// The conclusion leads, in plain words and with the numbers in it. The first
// version led with the arithmetic ("1m0s < 3m30s") and named its inputs by
// config key, which was checkable and unreadable: an operator had to already
// know what attempt_response_header_timeout bounds to learn anything from it.
// The arithmetic and the keys are still on the row — the keys are what
// config.yaml is searched for — but as the footnote, not the headline.
import { useId } from "react";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import type { AdvisorFinding } from "../types";

// The server sends its sentences in English because the CLI prints them to a
// terminal with no translation to look up. The console renders its own copy
// keyed on the rule, and falls back to what the server said — so a rule added
// upstream reads as itself here rather than as a blank, exactly the way the
// audit trail treats an action it has no copy for.
function ruleTitle(t: TFunction, finding: AdvisorFinding) {
  return t(`advisor.rules.${finding.rule}`, { defaultValue: finding.rule });
}

// The numbers a sentence quotes, pulled from the evidence by name. A rule's
// sentence is written per rule and status here, so it can only quote what the
// server reported; a missing term reads as a dash rather than as "undefined".
function sentenceValues(t: TFunction, finding: AdvisorFinding) {
  const evidence = new Map((finding.evidence ?? []).map((term) => [term.name, term.value]));
  const value = (name: string) => evidence.get(name) ?? "—";
  const duration = (name: string) => humanDuration(t, value(name));
  return {
    header: duration("gateway.attempt_response_header_timeout"),
    route: duration("gateway.route_total_timeout"),
    total: value("gateway.max_total_attempts"),
    perTarget: value("retry.max_attempts_per_target"),
    reachable: value("candidates_the_budget_reaches"),
    widest: value("widest_fan_out"),
    model: value("widest_fan_out_public_model"),
    suspended: value("suspended_scopes"),
    observed: value("refusals_observed"),
    unclassified: value("refusals_unclassified"),
  };
}

// Some statuses cover sub-cases one sentence cannot hold: "the widest alias has
// one candidate" is not "the budget reaches all four", and "no refusal yet" is
// not "every refusal understood". The variant key is tried first and the
// status's own sentence is the fallback.
//
// A suspension held without a reason is the one warning with advice of its
// own: the availability policy counted failures this instance's own deadline
// may have produced, so that deadline is the first thing to check. The server
// marks the case by adding the deadline to the evidence, and only then.
function sentenceVariant(finding: AdvisorFinding, values: ReturnType<typeof sentenceValues>) {
  const hasEvidence = (name: string) => (finding.evidence ?? []).some((term) => term.name === name);
  if (finding.rule === "suspended_scopes" && finding.status === "warn" && hasEvidence("gateway.attempt_response_header_timeout")) return "warnUnclassified";
  if (finding.status !== "ok") return undefined;
  if (finding.rule === "attempt_budget_reaches_fanout" && values.widest === "0") return "okEmpty";
  if (finding.rule === "attempt_budget_reaches_fanout" && values.widest === "1") return "okSingle";
  if (finding.rule === "unclassified_refusals" && values.observed === "0") return "okNone";
  return undefined;
}

function ruleConsequence(t: TFunction, finding: AdvisorFinding) {
  const values = sentenceValues(t, finding);
  const variant = sentenceVariant(finding, values);
  const fallback = t(`advisor.consequences.${finding.rule}.${finding.status}`, { ...values, defaultValue: finding.consequence });
  return variant ? t(`advisor.consequences.${finding.rule}.${variant}`, { ...values, defaultValue: fallback }) : fallback;
}

// Go prints durations as "3m30s"; the sentence says "3 分钟 30 秒". Anything
// that does not parse is shown as the server wrote it. Callers apply it only to
// values they know are durations: an alias may well be called "5m".
function humanDuration(t: TFunction, raw: string) {
  const match = /^(?:(\d+)h)?(?:(\d+)m)?(?:(\d+(?:\.\d+)?)s)?$/.exec(raw);
  if (!raw || !match || match[0] === "") return raw;
  const [, hours, minutes, seconds] = match;
  const parts = [
    hours && Number(hours) > 0 ? t("advisor.duration.hours", { count: Number(hours) }) : "",
    minutes && Number(minutes) > 0 ? t("advisor.duration.minutes", { count: Number(minutes) }) : "",
    seconds && Number(seconds) > 0 ? t("advisor.duration.seconds", { count: Number(seconds) }) : "",
  ].filter(Boolean);
  return parts.length ? parts.join(" ") : t("advisor.duration.seconds", { count: 0 });
}

// An evidence name is a configuration key (dotted, and what an operator
// searches config.yaml for), a derived number this panel named, or a composite
// a rule built from a scope or a status code ("deployment:dep_1",
// "unclassified_provider_status:503"). Each gets a readable label; the config
// key is kept beside its label rather than replaced by it.
// A configuration key whose value is a Go duration. Only these are rewritten
// for reading; every other value is shown exactly as the server sent it.
function isDurationKey(name: string) {
  return name.includes(".") && name.endsWith("_timeout");
}

function termLabel(t: TFunction, name: string): { label: string; configKey?: string } {
  const separator = name.indexOf(":");
  if (separator > 0) {
    const kind = name.slice(0, separator);
    const subject = name.slice(separator + 1);
    if (kind === "unclassified_provider_status") {
      return { label: subject === "none" ? t("advisor.unclassifiedNoStatus") : t("advisor.unclassifiedStatus", { status: subject }) };
    }
    return { label: `${t(`advisor.scopeKinds.${kind}`, { defaultValue: kind })} · ${subject}` };
  }
  if (name.includes(".")) {
    const label = t(`advisor.config.${name}`, { defaultValue: name });
    return label === name ? { label } : { label, configKey: name };
  }
  return { label: t(`advisor.terms.${name}`, { defaultValue: name.replaceAll("_", " ") }) };
}

function ruleBrief(t: TFunction, finding: AdvisorFinding) {
  const values = sentenceValues(t, finding);
  const variant = sentenceVariant(finding, values) ?? finding.status;
  return t(`advisor.briefs.${finding.rule}.${variant}`, { ...values, defaultValue: finding.consequence });
}

function StateIcon({ status }: { status: AdvisorFinding["status"] | "empty" }) {
  return <span className={`advisor-state-icon ${status}`} aria-hidden="true">{status === "ok" ? "✓" : status === "warn" ? "!" : status === "unknown" ? "?" : "—"}</span>;
}

export function AdvisorFindingsPanel({ findings }: { findings: AdvisorFinding[] }) {
  const { t } = useTranslation();
  const titleID = useId();
  const warnings = findings.filter((finding) => finding.status === "warn").length;
  const unknown = findings.filter((finding) => finding.status === "unknown").length;
  const overall = !findings.length ? "empty" : warnings ? "warn" : unknown ? "unknown" : "ok";
  const summary = overall === "empty" ? t("advisor.summaryEmpty")
    : overall === "warn" ? t("advisor.summaryWarnings", { count: warnings })
    : overall === "unknown" ? t("advisor.summaryUnknown", { count: unknown })
    : t("advisor.summaryClear");
  // Present attention first without changing the server's verdict or order
  // within a status. A local check passing is not a provider-health promise.
  const priority = { warn: 0, unknown: 1, ok: 2 };
  const ordered = [...findings].sort((left, right) => priority[left.status] - priority[right.status]);
  return (
    <section className="panel advisor-findings" aria-labelledby={titleID}>
      <header className="advisor-panel-header">
        <div><h3 id={titleID}>{t("advisor.title")}</h3><span className="advisor-check-count">{t("advisor.checkCount", { count: findings.length })}</span></div>
        <div className="advisor-overview">
          <span className={`advisor-overall-status ${overall}`}><StateIcon status={overall} />{summary}</span>
          {warnings > 0 && unknown > 0 && <span className="advisor-unchecked-count">{t("advisor.summaryUnknown", { count: unknown })}</span>}
        </div>
      </header>
      {!findings.length ? <p className="advisor-empty">{t("advisor.emptyDescription")}</p> : <ul className="advisor-finding-list">
        {ordered.map((finding) => {
          const consequence = ruleConsequence(t, finding);
          const brief = finding.status === "ok" ? ruleBrief(t, finding) : consequence;
          return <li key={finding.rule} data-rule={finding.rule} className={`advisor-finding ${finding.status}`}>
            <details className="advisor-check advisor-evidence-details">
              <summary className="advisor-check-summary">
                <StateIcon status={finding.status} />
                <span className="advisor-finding-copy">
                  <strong>{ruleTitle(t, finding)}</strong>
                  <span className={finding.status === "ok" ? "advisor-brief" : "advisor-consequence"}>{brief}</span>
                </span>
                <span className={`advisor-status ${finding.status}`}>{t(`advisor.statuses.${finding.status}`)}</span>
                <svg className="advisor-check-chevron" viewBox="0 0 16 16" aria-hidden="true"><path d="m6 4 4 4-4 4" /></svg>
              </summary>
              <div className="advisor-check-body">
                {finding.status === "ok" && consequence !== brief && <p className="advisor-explanation">{consequence}</p>}
                <h4>{t("advisor.evidenceLabel")}</h4>
                {finding.evidence?.length ? <dl className="advisor-evidence" aria-label={t("advisor.evidenceLabel")}>
                  {finding.evidence.map((term) => {
                    const { label, configKey } = termLabel(t, term.name);
                    return <div key={term.name}>
                      <dt>{label}{configKey && <code className="advisor-config-key">{configKey}</code>}</dt>
                      <dd className="advisor-value">{isDurationKey(term.name) ? humanDuration(t, term.value) : term.value}</dd>
                    </div>;
                  })}
                </dl> : <p className="advisor-no-evidence">{t("advisor.noEvidence")}</p>}
                {finding.comparison && <p className="advisor-comparison"><span>{t("advisor.comparisonLabel")}</span><code>{finding.comparison}</code></p>}
              </div>
            </details>
          </li>;
        })}
      </ul>}
    </section>
  );
}
