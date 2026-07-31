import { useQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "../api";
import { ErrorState, Loading, PageHeader, StatusDot } from "../components";
import { compactNumber, dateTime, money } from "../format";

export function UsagePage() {
  const [status, setStatus] = useState("");
  const [model, setModel] = useState("");
  const queryString = new URLSearchParams({
    limit: "100",
    ...(status ? { status } : {}),
    ...(model ? { model } : {}),
  }).toString();
  const usage = useQuery({
    queryKey: ["usage", status, model],
    queryFn: () => api.usage(`?${queryString}`),
  });
  return (
    <>
      <PageHeader
        eyebrow="DURABLE ACCOUNTING"
        title="Usage"
        description="每次 Provider attempt 的 Token、成本、延迟与终态。筛选不会执行任意 SQL。"
      />
      <div className="filter-bar">
        <label><span>模型</span><input value={model} onChange={(event) => setModel(event.target.value)} placeholder="chat" /></label>
        <label>
          <span>状态</span>
          <select value={status} onChange={(event) => setStatus(event.target.value)}>
            <option value="">全部</option>
            <option value="success">Success</option>
            <option value="error">Error</option>
          </select>
        </label>
        <span className="filter-count">{usage.data?.items.length ?? 0} records</span>
      </div>
      {usage.isPending && <Loading />}
      {usage.isError && <ErrorState error={usage.error} />}
      {usage.data && (
        <div className="table-shell">
          <table>
            <thead><tr><th>REQUEST</th><th>MODEL</th><th>TOKENS</th><th>COST</th><th>LATENCY</th><th>STATUS</th><th>TIME</th></tr></thead>
            <tbody>
              {usage.data.items.map((attempt) => (
                <tr key={attempt.event_id}>
                  <td><code>{attempt.request_id}</code><small>attempt {attempt.attempt}</small></td>
                  <td><strong>{attempt.requested_model || "—"}</strong><small>{attempt.provider_model}</small></td>
                  <td>{attempt.tokens_estimated ? "EST. " : ""}{compactNumber(attempt.provider_input_tokens + attempt.provider_output_tokens)}<small>{compactNumber(attempt.provider_input_tokens)} in / {compactNumber(attempt.provider_output_tokens)} out · {attempt.tokens_estimated ? "conservative upper bound" : "provider reported"}</small></td>
                  <td>{money(attempt.cost_micros_usd)}</td>
                  <td>{attempt.latency_millis} ms</td>
                  <td><span className="inline-status"><StatusDot ok={attempt.status === "success"} />{attempt.status}</span></td>
                  <td>{dateTime(attempt.completed_at)}</td>
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
    </>
  );
}
