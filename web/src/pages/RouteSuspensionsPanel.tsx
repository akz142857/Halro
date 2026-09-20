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

// The panel renders in every state, including the two where it has nothing to
// show. An absent panel and an empty one answer differently: "nothing is being
// refused" is the answer an operator came for, and a read that failed must not
// be able to impersonate it.
export type SuspensionReadState = "loading" | "ready" | "unavailable";

export function RouteSuspensionsPanel({
  suspensions, state, credentials, providers, deployments,
}: {
  suspensions: RouteSuspension[];
  state: SuspensionReadState;
  credentials: Credential[];
  providers: Provider[];
  deployments: Deployment[];
}) {
  const { t } = useTranslation();
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
          <thead>
            <tr>
              <th>{t("providers.suspensions.subject")}</th>
              <th>{t("providers.suspensions.cause")}</th>
              <th>{t("providers.suspensions.recovery")}</th>
              <th>{t("providers.suspensions.observed")}</th>
            </tr>
          </thead>
          <tbody>
            {/* The row renders even with nothing in it. An absent table cannot
                be told apart from a panel that failed to load, and "nothing is
                being refused" is the answer an operator came here for. */}
            {suspensions.length === 0 && (
              <tr><td>—</td><td>—</td><td>—</td><td>—</td></tr>
            )}
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
                        ? t("providers.suspensions.untilTime", { time: new Date(suspension.until).toLocaleString() })
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
                  <td>{new Date(suspension.observed_at).toLocaleString()}</td>
                </tr>
              );
            })}
          </tbody>
        </table>
      </div>
      {suspensions.length === 0 && (
        <p className="suspension-empty">
          {state === "unavailable"
            ? t("providers.suspensions.unavailable")
            : state === "loading"
              ? t("providers.suspensions.loading")
              : t("providers.suspensions.empty")}
        </p>
      )}
    </section>
  );
}
