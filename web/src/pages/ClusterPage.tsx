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
  return <div className="cluster-page">
    <PageHeader
      title={t("cluster.title")}
      action={<div className="cluster-refresh">
        <button type="button" className="button secondary" onClick={() => void status.refetch()} disabled={status.isFetching} aria-busy={status.isFetching}>
          <svg className="cluster-refresh-icon" viewBox="0 0 24 24" aria-hidden="true"><path d="M20 7v5h-5M4 17v-5h5M6.1 7a7 7 0 0 1 11.5-1L20 9M4 15l2.4 3A7 7 0 0 0 17.9 17" /></svg>
          {t("cluster.refresh")}
        </button>
        <span className="cluster-refresh-meta">{t("cluster.autoRefresh")}</span>
      </div>}
    />
    {status.isPending && <Loading />}
    {status.isError && <ErrorState error={status.error} />}
    {status.data?.mode === "standalone" && !status.isError && <section className="panel cluster-mode" aria-labelledby="cluster-standalone-title">
      <h2 id="cluster-standalone-title">{t("cluster.standaloneTitle")}</h2>
      <p>{t("cluster.standaloneDescription")}</p>
    </section>}
    {status.data?.mode === "ha" && !status.isError && <HAStatus status={status.data} />}
  </div>;
}

function HAStatus({ status }: { status: HAMemberStatus }) {
  const { t } = useTranslation();
  const confirmationLag = status.durable_index - status.confirmed_index;
  const applyLag = status.confirmed_index - status.applied_index;
  const disconnected = status.peers.filter((peer) => !peer.connected).length;
  return <section className="panel cluster-status" aria-label={t("cluster.haTitle")}>
      <details className="cluster-details" open={!status.startup_ready}>
        <summary>
          <h2>{t("cluster.memberTitle")}</h2>
          <span className="cluster-detail-summary"><span>{status.node_id}</span><span>{t(`cluster.roles.${status.role}`)}</span><span className={status.startup_ready ? "" : "cluster-attention"}>{t("cluster.startupReady")} · {t(status.startup_ready ? "cluster.ready" : "cluster.notReady")}</span></span>
          <span className="cluster-chevron" aria-hidden="true">›</span>
        </summary>
        <dl>
          <div><dt>{t("cluster.clusterID")}</dt><dd><code>{status.cluster_id}</code></dd></div>
          <div><dt>{t("cluster.incarnation")}</dt><dd><code>{status.incarnation}</code></dd></div>
          <div><dt>{t("cluster.nodeID")}</dt><dd><code>{status.node_id}</code></dd></div>
          <div><dt>{t("cluster.term")}</dt><dd>{status.term}</dd></div>
          <div><dt>{t("cluster.promisedTerm")}</dt><dd>{status.promised_term}</dd></div>
          <div><dt>{t("cluster.startupReady")}</dt><dd>{t(status.startup_ready ? "cluster.ready" : "cluster.notReady")}</dd></div>
        </dl>
      </details>
      <details className="cluster-details" open={confirmationLag > 0 || applyLag > 0}>
        <summary>
          <h2>{t("cluster.progressTitle")}</h2>
          <span className={`cluster-detail-summary${confirmationLag > 0 || applyLag > 0 ? " cluster-attention" : ""}`}>{t("cluster.progressSummary", { unconfirmed: confirmationLag, unapplied: applyLag })}</span>
          <span className="cluster-chevron" aria-hidden="true">›</span>
        </summary>
        <dl>
          <div><dt>{t("cluster.durableIndex")}</dt><dd>{status.durable_index}</dd></div>
          <div><dt>{t("cluster.confirmedIndex")}</dt><dd>{status.confirmed_index}</dd></div>
          <div><dt>{t("cluster.appliedIndex")}</dt><dd>{status.applied_index}</dd></div>
          <div><dt>{t("cluster.confirmationLag")}</dt><dd>{confirmationLag}</dd></div>
          <div><dt>{t("cluster.applyLag")}</dt><dd>{applyLag}</dd></div>
          <div><dt>{t("cluster.metadataProjection")}</dt><dd>{status.projection.metadata_epoch} / {status.projection.metadata_sequence}</dd></div>
        </dl>
      </details>
      <details className="cluster-details" open={disconnected > 0}>
        <summary>
          <h2>{t("cluster.peersTitle")}</h2>
          <span className={`cluster-detail-summary${disconnected > 0 ? " cluster-attention" : ""}`}>{t("cluster.peersSummary", { connected: status.peers.length - disconnected, total: status.peers.length, disconnected })}</span>
          <span className="cluster-chevron" aria-hidden="true">›</span>
        </summary>
        <p className="cluster-caption">{t("cluster.peersDescription")}</p>
        {!status.peers.length && <p className="cluster-empty">{t("cluster.noPeers")}</p>}
        <dl>
          {status.peers.map((peer) => <div key={peer.node_id}>
            <dt><code>{peer.node_id}</code></dt>
            <dd className="cluster-peer-state"><StatusDot ok={peer.connected} />{t(peer.connected ? "cluster.connected" : "cluster.disconnected")}</dd>
          </div>)}
        </dl>
      </details>
    <p className="cluster-scope">{t("cluster.localOnly")}</p>
  </section>;
}
