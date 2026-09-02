import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { EmptyState, ErrorState, Loading, LoadMore, StatusDot } from "../components";
import { errorClassAdvice, errorClassLabel, predatesProviderIdentifiers } from "../failure";
import { useInstantFormatter, type InstantStyle } from "../format";
import { Link } from "../navigation";
import { accountingTimeZone, isoToZonedInput, useAccountingTimeZone, zonedInputToISO } from "../timezone";
import type { RequestFailure } from "../types";

// The terminal states that mean a policy did its job rather than that something
// broke. They are failed requests — they count toward the summary card and they
// belong in this list — but there is no upstream to blame, no error class to
// read, and no attempt chain to expand, because none of that ever happened.
//
// Kept as a set rather than derived from "has no last_failure": a provider
// failure whose attempt record has aged out of the aggregate would otherwise be
// relabelled a policy rejection, which is a different accusation.
const policyOutcomes = new Set([
  "rejected", "token_guard_rejected", "unsupported_feature", "policy_rejected",
]);

export function UsageFailuresPanel() {
  const { t } = useTranslation();
  const dateTime = useInstantFormatter();
  const timeZone = useAccountingTimeZone();
  const projects = useQuery({ queryKey: ["projects"], queryFn: api.projects });
  const deployments = useQuery({ queryKey: ["deployments"], queryFn: api.deployments });
  const parameter = (name: string) => new URLSearchParams(window.location.search).get(name) ?? "";
  // The filter this list is most often opened with: a caller reports an ID from
  // a failed call and wants to know what happened to it. It leads the bar for
  // that reason — the other filters narrow a population, this one answers a
  // question that already has an answer.
  const [requestID, setRequestID] = useState(() => parameter("request_id"));
  const [projectID, setProjectID] = useState(() => parameter("project_id"));
  const [deploymentID, setDeploymentID] = useState(() => parameter("deployment_id"));
  const [start, setStart] = useState(() => isoToZonedInput(parameter("start") || undefined, accountingTimeZone()));
  const [end, setEnd] = useState(() => isoToZonedInput(parameter("end") || undefined, accountingTimeZone()));

  const failures = useInfiniteQuery({
    queryKey: ["usage-failures", requestID, projectID, deploymentID, start, end, timeZone],
    initialPageParam: "",
    queryFn: ({ pageParam }) => api.usageFailures(`?${new URLSearchParams({
      limit: "100",
      ...(requestID ? { request_id: requestID } : {}),
      ...(projectID ? { project_id: projectID } : {}),
      ...(deploymentID ? { deployment_id: deploymentID } : {}),
      ...(start ? { start: zonedInputToISO(start, timeZone) } : {}),
      ...(end ? { end: zonedInputToISO(end, timeZone) } : {}),
      ...(pageParam ? { cursor: pageParam } : {}),
    })}`),
    getNextPageParam: (page) => page.next_cursor || undefined,
  });
  const rows = failures.data?.pages.flatMap((page) => page.items) ?? [];
  const projectNames = Object.fromEntries((projects.data?.items ?? []).map((project) => [project.id, project.name]));
  const deploymentNames = Object.fromEntries((deployments.data?.items ?? []).map((item) => [item.id, item.name]));

  return (
    <>
      <div className="filter-bar">
        <label><span>{t("usage.requestID")}</span><input autoComplete="off" value={requestID} onChange={(event) => setRequestID(event.target.value)} placeholder="req_…" /></label>
        <label>
          <span>{t("usage.project")}</span>
          <select value={projectID} onChange={(event) => setProjectID(event.target.value)}>
            <option value="">{t("usage.all")}</option>
            {(projects.data?.items ?? []).map((project) => <option key={project.id} value={project.id}>{project.name || project.id}</option>)}
          </select>
        </label>
        <label>
          <span>{t("usage.deployment")}</span>
          <select value={deploymentID} onChange={(event) => setDeploymentID(event.target.value)}>
            <option value="">{t("usage.all")}</option>
            {(deployments.data?.items ?? []).map((item) => <option key={item.id} value={item.id}>{item.name || item.id}</option>)}
            {deploymentID && !deploymentNames[deploymentID] && <option value={deploymentID}>{deploymentID}</option>}
          </select>
        </label>
        <label><span>{t("usage.start")}</span><input autoComplete="off" type="datetime-local" value={start} onChange={(event) => setStart(event.target.value)} /></label>
        <label><span>{t("usage.end")}</span><input autoComplete="off" type="datetime-local" value={end} onChange={(event) => setEnd(event.target.value)} /></label>
        <span className="filter-count">{t("usage.failures.records", { count: rows.length })}</span>
      </div>
      {failures.isPending && <Loading />}
      {failures.isError && <ErrorState error={failures.error} />}
      {failures.data && rows.length === 0 && (
        <EmptyState title={t("usage.failures.emptyTitle")}>{t("usage.failures.emptyDescription")}</EmptyState>
      )}
      {failures.data && rows.length > 0 && (
        <div className="table-shell">
          <table className="usage-table">
            <colgroup>
              <col style={{ width: "20%" }} /><col style={{ width: "14%" }} /><col style={{ width: "24%" }} />
              <col style={{ width: "16%" }} /><col style={{ width: "12%" }} /><col style={{ width: "14%" }} />
            </colgroup>
            <thead>
              <tr>
                <th>{t("usage.request")}</th>
                <th>{t("usage.project")}</th>
                <th>{t("usage.failures.cause")}</th>
                <th>{t("usage.deployment")}</th>
                <th>{t("usage.failures.attempts")}</th>
                <th>{t("usage.time")}</th>
              </tr>
            </thead>
            <tbody>
              {rows.map((failure) => (
                <FailureRow
                  key={failure.request_id}
                  failure={failure}
                  projectName={projectNames[failure.project_id]}
                  deploymentName={failure.last_failure?.deployment_id ? deploymentNames[failure.last_failure.deployment_id] : undefined}
                  formatInstant={dateTime}
                />
              ))}
            </tbody>
          </table>
          {failures.hasNextPage && (
            <LoadMore label={t("common.loadMore")} busy={failures.isFetchingNextPage} onLoad={() => failures.fetchNextPage()} />
          )}
        </div>
      )}
    </>
  );
}

function FailureRow({ failure, projectName, deploymentName, formatInstant }: {
  failure: RequestFailure;
  projectName?: string;
  deploymentName?: string;
  formatInstant: (instant: string, style?: InstantStyle) => string;
}) {
  const { t } = useTranslation();
  const policy = policyOutcomes.has(failure.outcome);
  const last = failure.last_failure;
  return (
    <tr>
      <td>
        {/* The Request ID goes to the attempt list filtered to this request,
            which is where its whole chain already lives. Building a second
            renderer for the chain here would leave two screens to keep in
            agreement about one record. */}
        <Link className="resource-link" href={`/admin/usage?tab=attempts&request_id=${encodeURIComponent(failure.request_id)}`}>
          <code>{failure.request_id}</code>
        </Link>
        <small>{failure.requested_model || "—"}</small>
      </td>
      <td>
        <Link className="resource-link" href={`/admin/projects?project_id=${encodeURIComponent(failure.project_id)}`}>
          {projectName || failure.project_id}
        </Link>
      </td>
      <td>
        <span className="inline-status">
          <StatusDot ok={false} label={t("usage.error")} />
          {/* A policy rejection is named for what it is. Showing an upstream
              error class here — or an empty one — would send the operator to
              audit a provider that was never called. */}
          {policy
            ? t(`usage.outcomes.${failure.outcome}`, { defaultValue: t("usage.failures.policyRejected") })
            : errorClassLabel(t, last?.error_class) || t("usage.error")}
        </span>
        {!policy && last?.provider_status ? <small>{t("usage.httpStatus", { status: last.provider_status })}</small> : null}
        <details className="failure-detail">
          <summary>{t("usage.attemptDetails")}</summary>
          <small>
            {policy
              ? t("usage.failures.policyRejectedDetail")
              : errorClassAdvice(t, last?.error_class) || t("usage.failures.noAdvice")}
            {last && <><br />{t("usage.failures.decidedBy", { attempt: last.attempt })}</>}
            {last && !policy && <ProviderIdentifiers failure={last} />}
          </small>
          {!policy && <CapturedPayload requestID={failure.request_id} />}
        </details>
      </td>
      <td>
        {last?.deployment_id ? (
          <>
            <Link className="resource-link" href={`/admin/deployments?q=${encodeURIComponent(last.deployment_id)}`}>
              {deploymentName || last.deployment_id}
            </Link>
            {last.provider_model && <small>{last.provider_model}</small>}
          </>
        ) : (
          // Not a dash for tidiness: this request chose no deployment, and an
          // aligned blank is the honest rendering of that.
          <span className="muted">{t("usage.failures.noTarget")}</span>
        )}
      </td>
      <td>
        {t("usage.failures.attemptCount", { count: failure.attempts })}
        {failure.fallbacks > 0 && <small>{t("usage.failures.fallbackCount", { count: failure.fallbacks })}</small>}
      </td>
      <td>{formatInstant(failure.completed_at, "dateTimeYear")}</td>
    </tr>
  );
}

// The upstream's own identifiers, which are what a support desk asks for and
// what an operator cannot reconstruct from anything else on the page.
//
// A record written before they were kept says so in words. A blank there and a
// blank on a record that simply had none are different answers, and showing one
// rendering for both talks the operator out of chasing an identifier that does
// exist upstream. It lives here rather than beside the attempt table because
// the attempt table already imports this panel; the other direction would be a
// cycle.
export function ProviderIdentifiers({ failure }: {
  failure: { provider_code?: string; provider_request_id?: string; failure_phase?: string };
}) {
  const { t } = useTranslation();
  if (predatesProviderIdentifiers(failure)) {
    return <><br />{t("usage.identifiersNotRecorded")}</>;
  }
  return (
    <>
      {failure.provider_code && <><br />{t("usage.providerCode", { code: failure.provider_code })}</>}
      {failure.provider_request_id && <><br />{t("usage.providerRequestID", { id: failure.provider_request_id })}</>}
    </>
  );
}

// What the failed call carried, fetched only when an operator asks for it.
//
// It is behind a click rather than rendered with the row for three reasons that
// all point the same way: it is the only thing on this page holding material a
// caller wrote, the server audits every read of it, and a row that fetched it
// automatically would file an audit record for every failure an operator merely
// scrolled past. Nothing is cached — leaving a prompt in a query cache is the
// browser-side version of the storage decision this feature was careful about.
function CapturedPayload({ requestID }: { requestID: string }) {
  const { t } = useTranslation();
  const [requested, setRequested] = useState(false);
  const payload = useQuery({
    queryKey: ["usage-failure-payload", requestID],
    queryFn: () => api.usageFailurePayload(requestID),
    enabled: requested,
    gcTime: 0,
    staleTime: 0,
    retry: false,
  });

  if (!requested) {
    return (
      <button type="button" className="button secondary payload-reveal" onClick={() => setRequested(true)}>
        {t("usage.failures.revealPayload")}
      </button>
    );
  }
  if (payload.isPending) return <Loading />;
  // A 404 here is the ordinary case, not a fault: capture may be off, or this
  // failure may predate it, or the record may have aged out of its window.
  if (payload.isError) return <p className="payload-absent">{t("usage.failures.noPayload")}</p>;
  return (
    <div className="payload-view">
      <p className="payload-warning" role="note">{t("usage.failures.payloadWarning")}</p>
      <PayloadSection
        label={t("usage.failures.payloadRequest")}
        value={payload.data.request}
        truncated={payload.data.request_truncated}
      />
      <PayloadSection
        label={t("usage.failures.payloadResponse")}
        value={payload.data.response}
        truncated={payload.data.response_truncated}
      />
    </div>
  );
}

function PayloadSection({ label, value, truncated }: { label: string; value: unknown; truncated?: boolean }) {
  const { t } = useTranslation();
  if (value === undefined || value === null) return null;
  return (
    <>
      <h4>{label}</h4>
      {/* Truncation is stated rather than left to be inferred: a reader who
          diagnoses a malformed body that is only an incomplete one goes looking
          for a bug the upstream does not have. */}
      {truncated && <p className="payload-truncated">{t("usage.failures.payloadTruncated")}</p>}
      <pre className="payload-body">{JSON.stringify(value, null, 2)}</pre>
    </>
  );
}
