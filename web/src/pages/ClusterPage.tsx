import { useQuery } from "@tanstack/react-query";
import { useTranslation } from "react-i18next";
import { api } from "../api";
import { ErrorState, Loading, PageHeader, StatusDot } from "../components";
import type { ClusterStatus } from "../types";

type HAMemberStatus = Extract<ClusterStatus, { mode: "ha" }>;

export function ClusterPage() {
  const { t } = useTranslation();
  const status = useQuery({
    queryKey: ["cluster-status"],
    queryFn: api.clusterStatus,
    refetchInterval: 10_000,
  });
  return <>
    <PageHeader
      eyebrow={t("cluster.eyebrow")}
      title={t("cluster.title")}
      description={t("cluster.description")}
      action={<button type="button" className="button" onClick={() => void status.refetch()} disabled={status.isFetching}>{t("cluster.refresh")}</button>}
    />
    {status.isPending && <Loading />}
    {status.isError && <ErrorState error={status.error} />}
    {status.data?.mode === "standalone" && !status.isError && <section className="panel system-card" aria-labelledby="cluster-standalone-title">
      <h2 id="cluster-standalone-title">{t("cluster.standaloneTitle")}</h2>
      <p>{t("cluster.standaloneDescription")}</p>
    </section>}
    {status.data?.mode === "ha" && !status.isError && <HAStatus status={status.data} />}
  </>;
}

function HAStatus({ status }: { status: HAMemberStatus }) {
  const { t } = useTranslation();
  return <>
    <p className="notice">{t("cluster.localOnly")}</p>
    <div className="settings-grid">
      <details className="panel system-card diagnostic-details" open>
        <summary><span>{t("cluster.memberTitle")}</span><strong><StatusDot ok={status.startup_ready} />{t(`cluster.roles.${status.role}`)}</strong></summary>
        <dl>
          <div><dt>{t("cluster.clusterID")}</dt><dd><code>{status.cluster_id}</code></dd></div>
          <div><dt>{t("cluster.incarnation")}</dt><dd><code>{status.incarnation}</code></dd></div>
          <div><dt>{t("cluster.nodeID")}</dt><dd><code>{status.node_id}</code></dd></div>
          <div><dt>{t("cluster.term")}</dt><dd>{status.term}</dd></div>
          <div><dt>{t("cluster.promisedTerm")}</dt><dd>{status.promised_term}</dd></div>
          <div><dt>{t("cluster.startupReady")}</dt><dd>{t(status.startup_ready ? "cluster.ready" : "cluster.notReady")}</dd></div>
        </dl>
      </details>
      <details className="panel system-card diagnostic-details" open>
        <summary><span>{t("cluster.progressTitle")}</span><strong>{t("cluster.localWatermarks")}</strong></summary>
        <dl>
          <div><dt>{t("cluster.durableIndex")}</dt><dd>{status.durable_index}</dd></div>
          <div><dt>{t("cluster.confirmedIndex")}</dt><dd>{status.confirmed_index}</dd></div>
          <div><dt>{t("cluster.appliedIndex")}</dt><dd>{status.applied_index}</dd></div>
          <div><dt>{t("cluster.confirmationLag")}</dt><dd>{status.durable_index - status.confirmed_index}</dd></div>
          <div><dt>{t("cluster.applyLag")}</dt><dd>{status.confirmed_index - status.applied_index}</dd></div>
          <div><dt>{t("cluster.metadataProjection")}</dt><dd>{status.projection.metadata_epoch} / {status.projection.metadata_sequence}</dd></div>
        </dl>
      </details>
      <details className="panel system-card diagnostic-details" open>
        <summary><span>{t("cluster.peersTitle")}</span><strong>{t("cluster.peersCount", { count: status.peers.length })}</strong></summary>
        <p className="muted">{t("cluster.peersDescription")}</p>
        <dl>
          {status.peers.map((peer) => <div key={peer.node_id}>
            <dt><code>{peer.node_id}</code></dt>
            <dd><StatusDot ok={peer.connected} />{t(peer.connected ? "cluster.connected" : "cluster.disconnected")}</dd>
          </div>)}
        </dl>
      </details>
    </div>
  </>;
}
