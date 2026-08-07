import { useInfiniteQuery, useQuery } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import { api } from "../api";
import { ErrorState, Loading, PageHeader, StatusDot } from "../components";
import { compactNumber, money, useInstantFormatter } from "../format";
import { Link } from "../navigation";
import { useTranslation } from "react-i18next";
import type { UsageAttempt } from "../types";

export function UsagePage() {
  const { t } = useTranslation();
  const dateTime = useInstantFormatter();
  // Every project ever billed can show up in Usage history, including a
  // disabled or since-deleted one, so this list is unfiltered — narrowing it
  // to "currently enabled" would make an old project's calls unfindable.
  const projects = useQuery({ queryKey: ["projects"], queryFn: api.projects });
  const projectNames = useMemo(
    () => Object.fromEntries((projects.data?.items ?? []).map((project) => [project.id, project.name])),
    [projects.data?.items],
  );
  const [status, setStatus] = useState("");
  const [model, setModel] = useState("");
  const [requestID, setRequestID] = useState(() => new URLSearchParams(window.location.search).get("request_id") ?? "");
  const [projectID, setProjectID] = useState(() => new URLSearchParams(window.location.search).get("project_id") ?? "");
  const [start, setStart] = useState("");
  const [end, setEnd] = useState("");
  const usage = useInfiniteQuery({
    queryKey: ["usage", status, model, requestID, projectID, start, end],
    initialPageParam: "",
    queryFn: ({ pageParam }) => api.usage(`?${new URLSearchParams({
      limit: "100", ...(status ? { status } : {}), ...(model ? { model } : {}), ...(requestID ? { request_id: requestID } : {}),
      ...(projectID ? { project_id: projectID } : {}),
      ...(start ? { start: new Date(start).toISOString() } : {}),
      ...(end ? { end: new Date(end).toISOString() } : {}),
      ...(pageParam ? { cursor: pageParam } : {}),
    })}`),
    getNextPageParam: (page) => page.next_cursor || undefined,
  });
  const attempts = usage.data?.pages.flatMap((page) => page.items) ?? [];
  return (
    <>
      <PageHeader
        eyebrow={t("usage.eyebrow")}
        title={t("usage.title")}
        description={t("usage.description")}
      />
      <div className="filter-bar">
        <label><span>Request ID</span><input value={requestID} onChange={(event) => setRequestID(event.target.value)} placeholder="req_…" /></label>
        <label>
          <span>{t("usage.project")}</span>
          <select value={projectID} onChange={(event) => setProjectID(event.target.value)}>
            <option value="">{t("usage.all")}</option>
            {(projects.data?.items ?? []).map((project) => <option key={project.id} value={project.id}>{project.name || project.id}</option>)}
          </select>
        </label>
        <label><span>{t("usage.model")}</span><input value={model} onChange={(event) => setModel(event.target.value)} placeholder="chat" /></label>
        <label>
          <span>{t("usage.status")}</span>
          <select value={status} onChange={(event) => setStatus(event.target.value)}>
            <option value="">{t("usage.all")}</option>
            <option value="success">{t("usage.success")}</option>
            <option value="error">{t("usage.error")}</option>
          </select>
        </label>
        <label><span>{t("usage.start")}</span><input type="datetime-local" value={start} onChange={(event) => setStart(event.target.value)} /></label>
        <label><span>{t("usage.end")}</span><input type="datetime-local" value={end} onChange={(event) => setEnd(event.target.value)} /></label>
        <span className="filter-count">{t("usage.records", { count: attempts.length })}</span>
      </div>
      {usage.isPending && <Loading />}
      {usage.isError && <ErrorState error={usage.error} />}
      {usage.data && (
        <div className="table-shell">
          <table className="usage-table">
            <colgroup>
              <col style={{ width: "16%" }} /><col style={{ width: "10%" }} /><col style={{ width: "11%" }} /><col style={{ width: "14%" }} />
              <col style={{ width: "21%" }} /><col style={{ width: "8%" }} /><col style={{ width: "7%" }} /><col style={{ width: "13%" }} />
            </colgroup>
            <thead><tr><th>{t("usage.request")}</th><th>{t("usage.project")}</th><th>{t("usage.model")}</th><th>{t("usage.tokens")}</th><th>{t("usage.cost")}</th><th>{t("usage.latency")}</th><th>{t("usage.status")}</th><th>{t("usage.time")}</th></tr></thead>
            <tbody>
              {attempts.map((attempt) => (
                <tr key={attempt.event_id}>
                  <td><code>{attempt.request_id}</code><small>{t("usage.attempt", { count: attempt.attempt })}</small></td>
                  <td>
                    <Link className="resource-link" href={`/admin/projects?project_id=${encodeURIComponent(attempt.project_id)}`}>{projectNames[attempt.project_id] || attempt.project_id}</Link>
                    {projectNames[attempt.project_id] && <small><code>{attempt.project_id}</code></small>}
                  </td>
                  <td>
                    <Link className="resource-link" href={`/admin/deployments?q=${encodeURIComponent(attempt.deployment_id || attempt.provider_model || "")}`}>{attempt.requested_model || "—"}</Link>
                    <small>{attempt.provider_model}</small>
                  </td>
                  <td>{attempt.tokens_estimated ? t("usage.estimated") : ""}{compactNumber(attempt.provider_input_tokens + attempt.provider_output_tokens)}<small>{t("usage.inputOutput", { input: compactNumber(attempt.provider_input_tokens), output: compactNumber(attempt.provider_output_tokens) })} · {attempt.tokens_estimated ? t("usage.conservative") : t("usage.reported")}</small></td>
                  <td><CostCell attempt={attempt} /></td>
                  <td>{attempt.latency_millis} ms</td>
                  <td><span className="inline-status"><StatusDot ok={attempt.status === "success"} />{attempt.status === "success" ? t("usage.success") : t("usage.error")}</span></td>
                  <td>{dateTime(attempt.completed_at, "dateTimeYear")}</td>
                </tr>
              ))}
            </tbody>
          </table>
          {usage.hasNextPage && <button className="button ghost load-more" disabled={usage.isFetchingNextPage} onClick={() => usage.fetchNextPage()}>
            {usage.isFetchingNextPage ? t("common.loading") : t("common.loadMore")}
          </button>}
        </div>
      )}
    </>
  );
}

// The pricing evidence disclosure shows how a settled attempt's cost was
// reached: the price snapshot it billed against and the input/output/fixed
// components that summed to it.
function CostCell({ attempt }: { attempt: UsageAttempt }) {
  const { t } = useTranslation();
  return (
    <>
      <strong>{attempt.cost_micros_usd == null ? t("usage.unknownCost") : money(attempt.cost_micros_usd)}</strong>
      {!!attempt.tags?.length && <small>{attempt.tags.map((tag) => <span className="badge" key={tag}>{tag}</span>)}</small>}
      <details className="cost-evidence">
        <summary>{t("usage.costEvidence")}</summary>
        <small>
          {attempt.price_snapshot?.price_version_id ? `${attempt.price_snapshot.price_version_id} · v${attempt.price_snapshot.price_version}` : attempt.price_evidence_status}<br />
          {attempt.input_cost_micros_usd == null ? "" : t("usage.formulaComponents", { input: money(attempt.input_cost_micros_usd), output: money(attempt.output_cost_micros_usd ?? 0), fixed: money(attempt.fixed_cost_micros_usd ?? 0) })}
        </small>
      </details>
    </>
  );
}
