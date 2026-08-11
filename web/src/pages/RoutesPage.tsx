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
import { hasOnboardingCreateIntent, OnboardingContextBanner } from "../OnboardingContext";

const column = createColumnHelper<Route>();

// The route update is a full replacement, so a state toggle has to resend
// everything the route already holds — sending only `enabled` would blank the
// deployment and strategy it routes on.
function routeUpdateBody(route: Route, enabled: boolean) {
  return {
    public_model: route.public_model,
    deployment_id: route.deployment_id,
    priority: route.priority,
    strategy: route.strategy,
    enabled,
  };
}

export function RoutesPage() {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const [creating, setCreating] = useState(() => !readOnly && hasOnboardingCreateIntent());
  const [editing, setEditing] = useState<Route>();
  const routes = useQuery({ queryKey: ["routes"], queryFn: api.routes });
  const deployments = useQuery({ queryKey: ["deployments"], queryFn: api.deployments });
  const providers = useQuery({ queryKey: ["providers"], queryFn: api.providers });
  const queryClient = useQueryClient();
  const remove = useMutation({
    mutationFn: ({ route, reauth }: { route: Route; reauth: ReauthValues }) => api.deleteRoute(route.id, route.revision, reauth),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["routes"] }),
  });
  // Switching a route off is the same write the edit form makes, so it goes
  // through the same full-body update rather than a second, partial one.
  const setEnabled = useMutation({
    mutationFn: (route: Route) => api.updateRoute(route.id, routeUpdateBody(route, !route.enabled), route.revision),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["routes"] }),
  });
  // Whether an alias keeps answering after this route is switched off depends
  // on what else still serves it, so the confirmation has to count.
  const siblingEnabledCount = useMemo(() => {
    const counts = new Map<string, number>();
    routes.data?.items.forEach((route) => {
      if (route.enabled) counts.set(route.public_model, (counts.get(route.public_model) ?? 0) + 1);
    });
    return counts;
  }, [routes.data]);
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
        return deployment?.name || getValue();
      },
    }),
    column.display({
      id: "provider",
      header: t("routes.provider"),
      cell: ({ row }) => {
        const deployment = deploymentByID.get(row.original.deployment_id);
        const providerID = deployment?.provider_id || "";
        return providerNames.get(providerID) || providerID;
      },
    }),
    column.display({
      id: "provider_model",
      header: t("routes.upstreamModel"),
      cell: ({ row }) => deploymentByID.get(row.original.deployment_id)?.provider_model,
    }),
    column.accessor("strategy", {
      header: t("routes.strategy"),
      cell: ({ getValue }) => <span className="badge">{(getValue() || "ordered") === "round_robin" ? t("routes.roundRobin") : t("routes.ordered")}</span>,
    }),
    column.accessor("priority", { header: t("routes.priority") }),
    column.accessor("enabled", {
      header: t("routes.status"),
      cell: ({ getValue }) => (
        <span className={`resource-state ${getValue() ? "enabled" : ""}`}>
          {getValue() ? t("common.enabled") : t("common.disabled")}
        </span>
      ),
    }),
    column.display({
      id: "actions",
      header: "",
      cell: ({ row }) => (
        <div className="row-actions route-row-actions">
          <RouteTestAction route={row.original} />
          <div className="row-actions route-management-actions">
            <button className="button ghost" disabled={readOnly} onClick={() => setEditing(row.original)}>{t("common.edit")}</button>
            {row.original.enabled ? (
              <ConfirmButton
                className="button ghost"
                label={t("common.disable")}
                title={t("routes.disableTitle")}
                confirmLabel={(siblingEnabledCount.get(row.original.public_model) ?? 0) > 1
                  ? t("routes.disableConfirm", { name: row.original.public_model, count: (siblingEnabledCount.get(row.original.public_model) ?? 1) - 1 })
                  : t("routes.disableConfirmLast", { name: row.original.public_model })}
                disabled={setEnabled.isPending}
                onConfirm={() => setEnabled.mutateAsync(row.original)}
              />
            ) : (
              <button className="button ghost" disabled={readOnly || setEnabled.isPending} onClick={() => setEnabled.mutate(row.original)}>{t("common.enable")}</button>
            )}
            <ConfirmButton
              label={t("common.delete")}
              confirmLabel={t("routes.deleteConfirm", { name: row.original.public_model })}
              requireStepUp
              onConfirm={(reauth) => remove.mutateAsync({ route: row.original, reauth })}
              disabled={remove.isPending}
            />
          </div>
        </div>
      ),
    }),
  ], [deploymentByID, providerNames, readOnly, remove, setEnabled, siblingEnabledCount, t]);
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
      <OnboardingContextBanner />
      {pending && <Loading />}
      {error && <ErrorState error={error} />}
      {/* Every other resource page surfaces its delete failure here. This one
          did not, so a refused deletion left the route in the list with nothing
          to explain why. */}
      {remove.isError && <ErrorState error={remove.error} />}
      {setEnabled.isError && <ErrorState error={setEnabled.error} />}
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
        <form className="route-form" onSubmit={submit}>
          <div className="route-form-body form-grid">
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
          {mutation.isError && <ErrorState error={mutation.error} />}
          </div>
          {/* Whether this alias will answer requests is the state the save
              commits, so it belongs in the bar that commits it. */}
          <div className="form-actions sticky-form-actions">
            <div className="form-footer-state">
              <label className="form-footer-enable">
                <input type="checkbox" aria-label={t("routes.enable")} checked={routeEnabled} onChange={(event) => setRouteEnabled(event.target.checked)} />
                <span>
                  <strong>{t("routes.enable")} · {routeEnabled ? t("common.enabled") : t("common.disabled")}</strong>
                  <small>{routeEnabled ? t("routes.enabledImpact", { alias: publicModel.trim() || t("routes.publicAlias") }) : t("routes.disabledImpact")}</small>
                </span>
              </label>
            </div>
            <button type="button" className="button ghost" onClick={onClose}>{t("common.cancel")}</button>
            <button className="button primary" disabled={mutation.isPending}>{current ? t("routes.save") : t("routes.createAndLoad")}</button>
          </div>
        </form>
      )}
    </Modal>
  );
}
