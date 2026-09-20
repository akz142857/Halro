// What the admission gate is refusing to route to right now.
//
// This is the only surface that names *which* credential an upstream refused.
// The caller-facing 503 names nothing and the metrics carry enumerations only,
// both deliberately — so an operator answering HalroCredentialUnusable had to
// curl the endpoint. It lives on the Providers page because that is where the
// remedy is: re-saving the credential advances its revision, which is what
// clears an indefinite suspension.
//
// A credential is the interesting scope, because one backs several deployments
// and takes all of them out at once. A row-per-deployment view would show five
// unexplained failures where there is one cause, which is why this reads the
// gate's own scopes rather than the deployment list.
import { useTranslation } from "react-i18next";
import { useInstantFormatter } from "../format";
import type { Credential, Deployment, Provider, RouteSuspension } from "../types";

// suspensionReasonLabel turns a provider.FailureReason into a sentence.
//
// Two of these conditions already had wording: a refused credential and an
// upstream rate limit are what `testControl.reasons.authentication` and
// `.rate_limit` describe, and a connection test that hit the same wall says it
// that way. Reusing them is the point — a second vocabulary for one condition
// is what that table was written to prevent. The rest are distinctions the
// error class cannot make (an exhausted quota is not a rate limit), so they get
// copy of their own in the same voice.
const reusedProbeWording: Record<string, string> = {
  invalid_credential: "authentication",
  rate_limited: "rate_limit",
};

export function suspensionReasonLabel(
  t: (key: string, values?: Record<string, unknown>) => string,
  reason: string,
): string {
  const reused = reusedProbeWording[reason];
  if (reused) return t(`testControl.reasons.${reused}`);
  return t(`providers.suspensions.reasons.${reason}`, {
    defaultValue: t("providers.suspensions.reasons.unclassified"),
  });
}

// What the operator is looking at. An identifier is not an answer to "which
// credential", so each key is resolved against the lists this page already
// loads; an id with no match renders as itself rather than as nothing, because
// a deployment deleted since the refusal is still the honest answer.
function scopeSubject(
  suspension: RouteSuspension,
  credentials: Credential[],
  providers: Provider[],
  deployments: Deployment[],
): { name: string; detail?: string } {
  const key = suspension.scope_key;
  switch (suspension.scope_kind) {
    case "credential":
      return { name: credentials.find((item) => item.id === key)?.name ?? key };
    case "provider":
      return { name: providers.find((item) => item.id === key)?.name ?? key };
    case "deployment":
      return { name: deployments.find((item) => item.id === key)?.name ?? key };
    case "credential_model": {
      // The server joined the two halves with "/" for a reader. A model
      // identifier may contain one too, so only the first separator is the
      // join — splitting on the last would move part of the model into the
      // credential id and resolve neither.
      const cut = key.indexOf("/");
      const credentialID = cut === -1 ? key : key.slice(0, cut);
      const model = cut === -1 ? "" : key.slice(cut + 1);
      const name = credentials.find((item) => item.id === credentialID)?.name ?? credentialID;
      return { name, detail: model };
    }
    default:
      return { name: key };
  }
}

// The panel is an exception list, so it renders only when there is an
// exception: a band of em dashes above every tab on every visit is the page
// telling an operator nothing, in the space where the connections are. What a
// panel may not do is disappear because the read failed — an absent panel would
// then read as "nothing is being refused", which is the one thing an unreadable
// gate cannot promise. That state keeps a line of its own.
export type SuspensionReadState = "loading" | "ready" | "unavailable";

// Which tab carries a marker for a refusal. A credential-scoped refusal is the
// credential vault's to answer, whichever connections it happens to back, and
// the model half of a credential_model scope narrows the same credential. A
// deployment-scoped one has no tab here at all — deployments are another page —
// so it is counted nowhere and stays visible only in the panel itself.
export function refusalCountsByTab(suspensions: RouteSuspension[]): {
  providers: number;
  credentials: number;
} {
  let providers = 0;
  let credentials = 0;
  for (const suspension of suspensions) {
    if (suspension.scope_kind === "provider") providers += 1;
    else if (suspension.scope_kind === "credential" || suspension.scope_kind === "credential_model") credentials += 1;
  }
  return { providers, credentials };
}

// The mark a tab wears while something under it is refused. The number beside
// it is how many, not which — the panel above says which, and two numbers on
// one tab need telling apart, so this one is the coloured one and carries its
// own sentence for a reader who cannot see the colour.
export function TabRefusalMark({ count }: { count: number }) {
  const { t } = useTranslation();
  if (count <= 0) return null;
  return (
    <span className="tab-refusal-mark">
      ●{count}
      <span className="visually-hidden">{t("providers.suspensions.tabMark", { count })}</span>
    </span>
  );
}

export function RouteSuspensionsPanel({
  suspensions, state, credentials, providers, deployments, onRetry, retrying,
}: {
  suspensions: RouteSuspension[];
  state: SuspensionReadState;
  credentials: Credential[];
  providers: Provider[];
  deployments: Deployment[];
  onRetry?: () => void;
  retrying?: boolean;
}) {
  const { t } = useTranslation();
  // Every instant in the console is rendered in the server's accounting zone,
  // and this panel was reading the browser's. The two timestamps here are the
  // ones an operator carries to another screen — "first seen" is matched
  // against a request on Usage, "until" against a window someone else is
  // watching — so an hour of disagreement here is worse than elsewhere.
  const dateTime = useInstantFormatter();
  if (suspensions.length === 0) {
    // Read and answered: there is nothing to show, so nothing is shown.
    // Still reading: nothing yet either, and a table of dashes that turns into
    // rows a moment later is worse than the rows arriving on their own.
    if (state !== "unavailable") return null;
    // The read is the only one on this page that is deliberately not folded
    // into the page-level error state — the connection list must not wait on
    // it — which also means nothing else offers to try it again.
    return (
      <div className={`notice warning route-suspensions-unavailable${onRetry ? " has-action" : ""}`}>
        <div className="notice-copy"><span>{t("providers.suspensions.unavailable")}</span></div>
        {onRetry && (
          <div className="notice-action">
            <button className="button ghost" disabled={retrying} onClick={onRetry}>{t("common.retry")}</button>
          </div>
        )}
      </div>
    );
  }
  return (
    <section className="panel route-suspensions-panel" aria-labelledby="route-suspensions-heading">
      <div className="panel-header">
        <div>
          <h2 id="route-suspensions-heading">{t("providers.suspensions.title")}</h2>
          <p>{t("providers.suspensions.description")}</p>
        </div>
      </div>
      <div className="table-shell usage-table-shell">
        <table>
          {/* Without fixed widths four columns divide a wide viewport evenly,
              and the two that carry a sentence get the same room as a
              timestamp. The cause column is the one that runs long — a reason,
              a status and an upstream code joined together — so it takes the
              share the fixed-shape columns do not need. */}
          <colgroup>
            <col style={{ width: "26%" }} /><col style={{ width: "34%" }} />
            <col style={{ width: "24%" }} /><col style={{ width: "16%" }} />
          </colgroup>
          <thead>
            <tr>
              <th>{t("providers.suspensions.subject")}</th>
              <th>{t("providers.suspensions.cause")}</th>
              <th>{t("providers.suspensions.recovery")}</th>
              <th>{t("providers.suspensions.observed")}</th>
            </tr>
          </thead>
          <tbody>
            {suspensions.map((suspension) => {
              const subject = scopeSubject(suspension, credentials, providers, deployments);
              const cause = [
                suspensionReasonLabel(t, suspension.reason),
                suspension.provider_status ? `HTTP ${suspension.provider_status}` : "",
                suspension.provider_code || "",
              ].filter(Boolean).join(" · ");
              return (
                <tr key={`${suspension.scope_kind}:${suspension.scope_key}`}>
                  <td>
                    <strong>{subject.name}</strong>
                    <span className="suspension-detail">
                      {t(`providers.suspensions.scopes.${suspension.scope_kind}`)}
                      {subject.detail ? ` · ${subject.detail}` : ""}
                    </span>
                  </td>
                  <td>{cause}</td>
                  <td>
                    {/* "Replace the credential" and "back at 14:05" are
                        different instructions, so they are never rendered as
                        the same sentence with a different time in it. */}
                    {suspension.indefinite
                      ? <strong>{t("providers.suspensions.untilReplaced")}</strong>
                      : suspension.until
                        ? t("providers.suspensions.untilTime", { time: dateTime(suspension.until) })
                        : t("providers.suspensions.untilNextAttempt")}
                    {/* The revision answers the question an operator asks after
                        they have already replaced the key: this is the one the
                        refusal was seen against, so a higher one means the
                        suspension is not about the secret they just saved. */}
                    {suspension.credential_revision ? (
                      <span className="suspension-detail">
                        {t("providers.suspensions.observedRevision", { revision: suspension.credential_revision })}
                      </span>
                    ) : null}
                  </td>
                  <td>{dateTime(suspension.observed_at)}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
    </section>
  );
}
