import { useInfiniteQuery } from "@tanstack/react-query";
import { useState } from "react";
import { api } from "../api";
import { ErrorState, Loading, PageHeader, StatusDot } from "../components";
import { compactNumber, dateTime, money } from "../format";
import { useTranslation } from "react-i18next";

export function UsagePage() {
  const { t } = useTranslation();
  const [status, setStatus] = useState("");
  const [model, setModel] = useState("");
  const usage = useInfiniteQuery({
    queryKey: ["usage", status, model],
    initialPageParam: "",
    queryFn: ({ pageParam }) => api.usage(`?${new URLSearchParams({
      limit: "100", ...(status ? { status } : {}), ...(model ? { model } : {}),
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
        <label><span>{t("usage.model")}</span><input value={model} onChange={(event) => setModel(event.target.value)} placeholder="chat" /></label>
        <label>
          <span>{t("usage.status")}</span>
          <select value={status} onChange={(event) => setStatus(event.target.value)}>
            <option value="">{t("usage.all")}</option>
            <option value="success">{t("usage.success")}</option>
            <option value="error">{t("usage.error")}</option>
          </select>
        </label>
        <span className="filter-count">{t("usage.records", { count: attempts.length })}</span>
      </div>
      {usage.isPending && <Loading />}
      {usage.isError && <ErrorState error={usage.error} />}
      {usage.data && (
        <div className="table-shell">
          <table>
            <thead><tr><th>{t("usage.request")}</th><th>{t("usage.model")}</th><th>{t("usage.tokens")}</th><th>{t("usage.cost")}</th><th>{t("usage.latency")}</th><th>{t("usage.status")}</th><th>{t("usage.time")}</th></tr></thead>
            <tbody>
              {attempts.map((attempt) => (
                <tr key={attempt.event_id}>
                  <td><code>{attempt.request_id}</code><small>{t("usage.attempt", { count: attempt.attempt })}</small></td>
                  <td><strong>{attempt.requested_model || "—"}</strong><small>{attempt.provider_model}</small></td>
                  <td>{attempt.tokens_estimated ? t("usage.estimated") : ""}{compactNumber(attempt.provider_input_tokens + attempt.provider_output_tokens)}<small>{t("usage.inputOutput", { input: compactNumber(attempt.provider_input_tokens), output: compactNumber(attempt.provider_output_tokens) })} · {attempt.tokens_estimated ? t("usage.conservative") : t("usage.reported")}</small></td>
                  <td>{money(attempt.cost_micros_usd)}</td>
                  <td>{attempt.latency_millis} ms</td>
                  <td><span className="inline-status"><StatusDot ok={attempt.status === "success"} />{attempt.status === "success" ? t("usage.success") : t("usage.error")}</span></td>
                  <td>{dateTime(attempt.completed_at)}</td>
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
