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
// The evidence is the content and the sentence is the annotation. A finding an
// operator can check beats one they have to believe, so the two numbers and the
// comparison between them are rendered at full weight and the consequence is
// one line underneath.
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { StatusDot } from "../components";
import type { AdvisorFinding } from "../types";

// The server sends its sentences in English because the CLI prints them to a
// terminal with no translation to look up. The console renders its own copy
// keyed on the rule, and falls back to what the server said — so a rule added
// upstream reads as itself here rather than as a blank, exactly the way the
// audit trail treats an action it has no copy for.
function ruleTitle(t: TFunction, finding: AdvisorFinding) {
  return t(`advisor.rules.${finding.rule}`, { defaultValue: finding.rule });
}

function ruleConsequence(t: TFunction, finding: AdvisorFinding) {
  return t(`advisor.consequences.${finding.rule}.${finding.status}`, { defaultValue: finding.consequence });
}

// An evidence name is either a configuration key, which is what an operator
// searches config.yaml for and is therefore left exactly as it is, or a derived
// number this panel named. Only the second kind has copy; the first falls
// through unchanged, and so do the composite names a rule builds from a scope
// or a status code.
function termName(t: TFunction, name: string) {
  if (name.includes(".")) return name;
  return t(`advisor.terms.${name}`, { defaultValue: name.replaceAll("_", " ") });
}

export function AdvisorFindingsPanel({ findings }: { findings: AdvisorFinding[] }) {
  const { t } = useTranslation();
  const warnings = findings.filter((finding) => finding.status === "warn").length;
  const unknown = findings.filter((finding) => finding.status === "unknown").length;
  // Three summaries, not two. "Nothing to report" and "some rules could not
  // run" are different answers, and collapsing the second into the first is the
  // panel claiming a check it never made.
  const summary = warnings > 0
    ? t("advisor.summaryWarnings", { count: warnings })
    : unknown > 0
      ? t("advisor.summaryUnknown", { count: unknown })
      : t("advisor.summaryClear");
  return (
    <details className="panel system-card diagnostic-details advisor-findings" open>
      <summary>
        <span>{t("advisor.title")}</span>
        {/* The dot repeats what the sentence beside it already says, so it
            carries no label of its own: a screen reader that read both would
            announce the summary twice. */}
        <strong><StatusDot ok={warnings === 0} />{summary}</strong>
      </summary>
      <p className="advisor-caption">{t("advisor.description")}</p>
      <ul className="advisor-finding-list">
        {findings.map((finding) => (
          <li key={finding.rule} className={`advisor-finding ${finding.status}`}>
            <div className="advisor-finding-head">
              <span className={`advisor-status ${finding.status}`}>{t(`advisor.statuses.${finding.status}`)}</span>
              <strong>{ruleTitle(t, finding)}</strong>
            </div>
            {/* The arithmetic, at full weight: it is the part that can be
                checked against config.yaml without trusting this panel. */}
            <code className="advisor-comparison">{finding.comparison}</code>
            <p className="advisor-consequence">{ruleConsequence(t, finding)}</p>
            {finding.evidence && finding.evidence.length > 0 && (
              <dl className="advisor-evidence">
                {finding.evidence.map((term) => (
                  <div key={term.name}>
                    <dt title={term.name}>{termName(t, term.name)}</dt>
                    <dd>{term.value}</dd>
                  </div>
                ))}
              </dl>
            )}
          </li>
        ))}
      </ul>
    </details>
  );
}
