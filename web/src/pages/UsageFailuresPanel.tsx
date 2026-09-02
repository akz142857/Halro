import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { EmptyState, ErrorState, Loading, LoadMore, StatusDot } from "../components";
import { FailureDetailDrawer, providerIdentifierFacts } from "./FailureDetailDrawer";
import { errorClassAdvice, errorClassLabel } from "../failure";
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
            {/* Attempts and time are fixed-shape values — "1 次尝试", a
                timestamp — so they take what they need and give the rest to
                the two columns that carry variable-length text. */}
            <colgroup>
              <col style={{ width: "20%" }} /><col style={{ width: "12%" }} /><col style={{ width: "26%" }} />
              <col style={{ width: "18%" }} /><col style={{ width: "8%" }} /><col style={{ width: "10%" }} />
              <col style={{ width: "6%" }} />
            </colgroup>
            <thead>
              <tr>
                <th>{t("usage.request")}</th>
                <th>{t("usage.project")}</th>
                <th>{t("usage.failures.cause")}</th>
                <th>{t("usage.deployment")}</th>
                <th>{t("usage.failures.attempts")}</th>
                <th>{t("usage.time")}</th>
                {/* The action column carries no heading, like the summary
                    table's. Its button names itself. */}
                <th />
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
  const [open, setOpen] = useState(false);
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
      <td>
        {/* Its own column. Sharing the cause cell put a control immediately
            after a status word with nothing between them — "错误失败详情" read
            as one phrase — and made the row's one action the hardest thing on
            it to find. */}
        <button type="button" className="resource-link failure-detail-open" onClick={() => setOpen(true)}>
          {t("usage.attemptDetails")}
        </button>
        {/* Rendered inside a cell rather than beside the row: the dialog
            portals to the document body and leaves nothing here, and a
            component placed directly under <tr> would be invalid markup the
            day it stops portalling. */}
        {open && (
          <FailureDetailDrawerFor
            failure={failure}
            projectName={projectName}
            deploymentName={deploymentName}
            formatInstant={formatInstant}
            onClose={() => setOpen(false)}
          />
        )}
      </td>
    </tr>
  );
}

// The facts of one failed request, as the shared drawer renders them. Kept as a
// builder rather than a component so the failed-request row and the attempt row
// assemble their own list and share everything else.
function FailureDetailDrawerFor({ failure, projectName, deploymentName, formatInstant, onClose }: {
  failure: RequestFailure;
  projectName?: string;
  deploymentName?: string;
  formatInstant: (instant: string, style?: InstantStyle) => string;
  onClose: () => void;
}) {
  const { t } = useTranslation();
  const policy = policyOutcomes.has(failure.outcome);
  const last = failure.last_failure;
  const identifiers = policy
    ? { facts: [], unrecorded: false }
    : providerIdentifierFacts(t, last);
  const cause = policy
    ? t(`usage.outcomes.${failure.outcome}`, { defaultValue: t("usage.failures.policyRejected") })
    : errorClassLabel(t, last?.error_class) || t("usage.error");
  return (
    <FailureDetailDrawer
      title={t("usage.failures.dialogTitle")}
      onClose={onClose}
      advice={policy
        ? t("usage.failures.policyRejectedDetail")
        : errorClassAdvice(t, last?.error_class) || t("usage.failures.noAdvice")}
      identifiersUnrecorded={identifiers.unrecorded}
      // A policy refusal never reached an upstream, so there is nothing to show
      // and no reason to offer an audited read.
      requestID={policy ? undefined : failure.request_id}
      links={
        <Link href={`/admin/usage?tab=attempts&request_id=${encodeURIComponent(failure.request_id)}`}>
          {t("usage.failures.viewAttemptChain")} →
        </Link>
      }
      facts={[
        { label: t("usage.failures.cause"), value: cause, emphasis: true },
        { label: t("usage.time"), value: formatInstant(failure.completed_at, "full") },
        { label: t("usage.requestID"), value: failure.request_id, code: true },
        { label: t("usage.project"), value: projectName || failure.project_id },
        { label: t("usage.model"), value: failure.requested_model },
        {
          label: t("usage.deployment"),
          value: last?.deployment_id ? deploymentName || last.deployment_id : t("usage.failures.noTarget"),
        },
        { label: t("usage.actualModel"), value: last?.provider_model },
        {
          label: t("usage.status"),
          value: last?.provider_status ? t("usage.httpStatus", { status: last.provider_status }) : undefined,
        },
        ...identifiers.facts,
        { label: t("usage.failures.attempts"), value: t("usage.failures.attemptCount", { count: failure.attempts }) },
        { label: t("usage.failures.fallbacks"), value: failure.fallbacks > 0 ? String(failure.fallbacks) : undefined },
        {
          label: t("usage.failures.decidedByLabel"),
          value: last ? t("usage.failures.decidedBy", { attempt: last.attempt }) : undefined,
        },
      ]}
    />
  );
}
