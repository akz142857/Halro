import {
  createColumnHelper,
  flexRender,
  getCoreRowModel,
  useReactTable,
} from "@tanstack/react-table";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState, type FormEvent } from "react";
import { api } from "../api";
import {
  ConfirmButton,
  EmptyState,
  ErrorState,
  Field,
  InlineTestControl,
  Loading,
  Modal,
  PageHeader,
  StatusDot,
  useDirty,
  type ReauthValues,
} from "../components";
import type { InlineTestState } from "../components";
import type { Deployment, Provider, Route } from "../types";
import { useTranslation } from "react-i18next";
import { useIsReadOnly } from "../session";
import { Link } from "../navigation";

const column = createColumnHelper<Route>();

export function RoutesPage() {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const [creating, setCreating] = useState(false);
  const [editing, setEditing] = useState<Route>();
  const routes = useQuery({ queryKey: ["routes"], queryFn: api.routes });
  const deployments = useQuery({ queryKey: ["deployments"], queryFn: api.deployments });
  const providers = useQuery({ queryKey: ["providers"], queryFn: api.providers });
  const queryClient = useQueryClient();
  const remove = useMutation({
    mutationFn: ({ route, reauth }: { route: Route; reauth: ReauthValues }) => api.deleteRoute(route.id, route.revision, reauth),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["routes"] }),
  });
  const deploymentByID = useMemo(
    () => new Map(deployments.data?.items.map((item) => [item.id, item]) ?? []),
    [deployments.data],
  );
  const providerNames = useMemo(
    () => new Map(providers.data?.items.map((item) => [item.id, item.name]) ?? []),
    [providers.data],
  );
  const columns = useMemo(() => [
    column.accessor("public_model", {
      header: t("routes.publicModel"),
      cell: ({ row, getValue }) => (
        <div className="model-cell">
          <StatusDot ok={row.original.enabled} />
          <strong>{getValue()}</strong>
          <code>{row.original.id}</code>
        </div>
      ),
    }),
    column.accessor("deployment_id", {
      header: t("routes.deployment"),
      cell: ({ getValue }) => {
        const deployment = deploymentByID.get(getValue());
        return deployment?.name || getValue() || t("routes.legacy");
      },
    }),
    column.display({
      id: "provider",
      header: t("routes.provider"),
      cell: ({ row }) => {
        const deployment = deploymentByID.get(row.original.deployment_id);
        const providerID = deployment?.provider_id || row.original.provider_id || "";
        return providerNames.get(providerID) || providerID;
      },
    }),
    column.display({
      id: "provider_model",
      header: t("routes.upstreamModel"),
      cell: ({ row }) => deploymentByID.get(row.original.deployment_id)?.provider_model || row.original.provider_model,
    }),
    column.accessor("strategy", {
      header: t("routes.strategy"),
      cell: ({ getValue }) => <span className="badge">{(getValue() || "ordered") === "round_robin" ? t("routes.roundRobin") : t("routes.ordered")}</span>,
    }),
    column.accessor("priority", { header: t("routes.priority") }),
    column.display({
      id: "actions",
      header: "",
      cell: ({ row }) => (
        <div className="row-actions route-row-actions">
          <RouteTestAction route={row.original} />
          <div className="row-actions route-management-actions">
            <button className="button ghost" disabled={readOnly} onClick={() => setEditing(row.original)}>{t("common.edit")}</button>
            <ConfirmButton
              label={t("common.delete")}
              confirmLabel={t("routes.deleteConfirm", { name: row.original.public_model })}
              requireStepUp
              onConfirm={(reauth) => remove.mutate({ route: row.original, reauth })}
              disabled={remove.isPending}
            />
          </div>
        </div>
      ),
    }),
  ], [deploymentByID, providerNames, remove, t]);
  const table = useReactTable({
    data: routes.data?.items ?? [],
    columns,
    getCoreRowModel: getCoreRowModel(),
  });
  const pending = routes.isPending || deployments.isPending || providers.isPending;
  const error = routes.error || deployments.error || providers.error;
  return (
    <>
      <PageHeader
        eyebrow={t("routes.eyebrow")}
        title={t("routes.title")}
        description={t("routes.description")}
        action={<button className="button primary" disabled={readOnly} onClick={() => setCreating(true)}>{t("routes.create")}</button>}
      />
      {pending && <Loading />}
      {error && <ErrorState error={error} />}
      {routes.data?.items.length === 0 && (
        <EmptyState title={t("routes.emptyTitle")}>{t("routes.emptyDescription")}</EmptyState>
      )}
      {!!routes.data?.items.length && (
        <div className="table-shell">
          <table className="route-table">
            <caption className="visually-hidden">{t("routes.list")}</caption>
            <thead>
              {table.getHeaderGroups().map((group) => (
                <tr key={group.id}>
                  {group.headers.map((header) => (
                    <th scope="col" key={header.id}>{flexRender(header.column.columnDef.header, header.getContext())}</th>
                  ))}
                </tr>
              ))}
            </thead>
            <tbody>
              {table.getRowModel().rows.map((row) => (
                <tr key={row.id}>
                  {row.getVisibleCells().map((cell) => (
                    <td key={cell.id}>{flexRender(cell.column.columnDef.cell, cell.getContext())}</td>
                  ))}
                </tr>
              ))}
            </tbody>
          </table>
        </div>
      )}
      {creating && (
        <RouteForm deployments={deployments.data?.items ?? []} onClose={() => setCreating(false)} />
      )}
      {editing && (
        <RouteForm current={editing} deployments={deployments.data?.items ?? []} onClose={() => setEditing(undefined)} />
      )}
    </>
  );
}

function RouteTestAction({ route }: { route: Route }) {
  const queryClient = useQueryClient();
  const test = useMutation({ mutationFn: () => api.testRoute(route.id), onSettled: () => queryClient.invalidateQueries({ queryKey: ["routes"] }) });
  const persistedTestIsCurrent = route.last_test_revision === route.revision;
  const state: InlineTestState = test.isPending
    ? "running"
    : test.isError
      ? "failure"
      : test.isSuccess || persistedTestIsCurrent && route.last_test_status === "healthy"
        ? "success"
        : persistedTestIsCurrent && route.last_test_status === "unhealthy"
          ? "failure"
          : route.last_test_status
            ? "stale"
            : "idle";
  return <InlineTestControl state={state} latency={test.data?.latency_ms ?? route.last_test_latency_millis} disabled={!route.enabled} onTest={() => test.mutate()} />;
}

function RouteForm({ current, deployments, onClose }: { current?: Route; deployments: Deployment[]; onClose: () => void }) {
  const { t } = useTranslation();
  const enabled = deployments.filter((item) => item.enabled || item.id === current?.deployment_id);
  const [publicModel, setPublicModel] = useState(current?.public_model ?? "chat");
  const [deploymentID, setDeploymentID] = useState(current?.deployment_id ?? enabled[0]?.id ?? "");
  const [priority, setPriority] = useState(current?.priority ?? 10);
  const [strategy, setStrategy] = useState<"ordered" | "round_robin">(current?.strategy || "ordered");
  const [routeEnabled, setRouteEnabled] = useState(current?.enabled ?? true);
  const queryClient = useQueryClient();
  const mutation = useMutation({
    mutationFn: () => {
      const value = {
      public_model: publicModel,
      deployment_id: deploymentID,
      priority,
      strategy,
      enabled: routeEnabled,
      };
      return current
        ? api.updateRoute(current.id, value, current.revision)
        : api.createRoute(value);
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["routes"] });
      onClose();
    },
  });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    if (publicModel.trim() && deploymentID) mutation.mutate();
  };
  const dirty = useDirty({ publicModel, deploymentID, priority, strategy, routeEnabled });
  return (
    <Modal title={current ? t("routes.edit") : t("routes.createTitle")} dirty={dirty} onClose={onClose}>
      {enabled.length === 0 ? (
        <div className="notice warning"><strong>{t("routes.deploymentRequired")}</strong><span>{t("routes.deploymentRequiredDescription")}</span><Link className="notice-link" href="/admin/deployments">{t("routes.openDeployments")}</Link></div>
      ) : (
        <form className="form-grid" onSubmit={submit}>
          <Field label={t("routes.publicAlias")}><input autoFocus required value={publicModel} onChange={(event) => setPublicModel(event.target.value)} /></Field>
          <Field label={t("routes.deployment")}>
            <select required value={deploymentID} onChange={(event) => setDeploymentID(event.target.value)}>
              {enabled.map((deployment) => (
                <option value={deployment.id} key={deployment.id}>{deployment.name} · {deployment.provider_model}</option>
              ))}
            </select>
          </Field>
          <Field label={t("routes.routeStrategy")}>
            <select value={strategy} onChange={(event) => setStrategy(event.target.value as typeof strategy)}>
              <option value="ordered">{t("routes.ordered")}</option>
              <option value="round_robin">{t("routes.roundRobin")}</option>
            </select>
          </Field>
          <Field label={t("routes.priority")}><input type="number" value={priority} onChange={(event) => setPriority(Number(event.target.value))} /></Field>
          <label className="check-row"><input type="checkbox" checked={routeEnabled} onChange={(event) => setRouteEnabled(event.target.checked)} />{t("routes.enable")}</label>
          {mutation.isError && <ErrorState error={mutation.error} />}
          <div className="form-actions">
            <button type="button" className="button ghost" onClick={onClose}>{t("common.cancel")}</button>
            <button className="button primary" disabled={mutation.isPending}>{current ? t("routes.save") : t("routes.createAndLoad")}</button>
          </div>
        </form>
      )}
    </Modal>
  );
}
