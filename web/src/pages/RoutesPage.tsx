import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useRef, useState, type FormEvent } from "react";
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
  useTestFailureReason,
  type ReauthValues,
} from "../components";
import type { InlineTestState } from "../components";
import type { Deployment, Provider, Route } from "../types";
import { useTranslation } from "react-i18next";
import { useNotify } from "../notifications";
import { useIsReadOnly } from "../session";
import { Link } from "../navigation";
import { hasOnboardingCreateIntent, OnboardingContextBanner } from "../OnboardingContext";

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
  const { notify } = useNotify();
  const remove = useMutation({
    mutationFn: ({ route, reauth }: { route: Route; reauth: ReauthValues }) => api.deleteRoute(route.id, route.revision, reauth),
    onSuccess: (_result, variables) => {
      queryClient.invalidateQueries({ queryKey: ["routes"] });
      notify({ tone: "success", title: t("routes.notifyDeleted"), description: variables.route.public_model });
    },
  });
  // Switching a route off is the same write the edit form makes, so it goes
  // through the same full-body update rather than a second, partial one.
  const setEnabled = useMutation({
    mutationFn: (route: Route) => api.updateRoute(route.id, routeUpdateBody(route, !route.enabled), route.revision),
    onSuccess: (_result, route) => {
      queryClient.invalidateQueries({ queryKey: ["routes"] });
      notify({ tone: "success", title: t(route.enabled ? "routes.notifyDisabled" : "routes.notifyEnabled"), description: route.public_model });
    },
    // No onError: this mutation renders an ErrorState in place, which carries the
    // reason. A second copy in the notification column says less and, on the
    // confirm-gated path, appears above a modal whose Tab trap cannot reach it.
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
              <tr>
                <th scope="col">{t("routes.publicModel")}</th>
                <th scope="col">{t("routes.deployment")}</th>
                <th scope="col">{t("routes.provider")}</th>
                <th scope="col">{t("routes.upstreamModel")}</th>
                <th scope="col">{t("routes.strategy")}</th>
                <th scope="col">{t("routes.priority")}</th>
                <th scope="col">{t("routes.status")}</th>
                <th scope="col" />
              </tr>
            </thead>
            <tbody>
              {routes.data.items.map((route) => {
                const deployment = deploymentByID.get(route.deployment_id);
                const providerID = deployment?.provider_id || "";
                const siblings = siblingEnabledCount.get(route.public_model) ?? 0;
                return (
                  <tr id={`route-${route.id}`} key={route.id}>
                    <td>
                      <div className="model-cell">
                        <StatusDot ok={route.enabled} />
                        <strong>{route.public_model}</strong>
                        <code>{route.id}</code>
                      </div>
                    </td>
                    <td>{deployment?.name || route.deployment_id}</td>
                    <td>{providerNames.get(providerID) || providerID}</td>
                    <td>{deployment?.provider_model}</td>
                    <td><span className="badge">{(route.strategy || "ordered") === "round_robin" ? t("routes.roundRobin") : t("routes.ordered")}</span></td>
                    <td>{route.priority}</td>
                    <td>
                      <span className={`resource-state ${route.enabled ? "enabled" : ""}`}>
                        {route.enabled ? t("common.enabled") : t("common.disabled")}
                      </span>
                    </td>
                    <td>
                      <div className="row-actions route-row-actions">
                        <RouteTestAction route={route} />
                        <div className="row-actions route-management-actions">
                          <button className="button ghost" disabled={readOnly} onClick={() => setEditing(route)}>{t("common.edit")}</button>
                          {route.enabled ? (
                            <ConfirmButton
                              className="button ghost"
                              label={t("common.disable")}
                              title={t("routes.disableTitle")}
                              confirmLabel={siblings > 1
                                ? t("routes.disableConfirm", { name: route.public_model, count: siblings - 1 })
                                : t("routes.disableConfirmLast", { name: route.public_model })}
                              disabled={setEnabled.isPending}
                              onConfirm={() => setEnabled.mutateAsync(route)}
                            />
                          ) : (
                            <button className="button ghost" disabled={readOnly || setEnabled.isPending} onClick={() => setEnabled.mutate(route)}>{t("common.enable")}</button>
                          )}
                          <ConfirmButton
                            label={t("common.delete")}
                            confirmLabel={t("routes.deleteConfirm", { name: route.public_model })}
                            requireStepUp
                            onConfirm={(reauth) => remove.mutateAsync({ route, reauth })}
                            disabled={remove.isPending}
                          />
                        </div>
                      </div>
                    </td>
                  </tr>
                );
              })}
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
  const failureReason = useTestFailureReason(test.error, persistedTestIsCurrent ? route.last_test_error_class : undefined);
  return (
    // The control and its reason stack, so a wrapped sentence pushes the rest of
    // the action row down rather than sitting between the buttons.
    <div className="inline-test-cell">
      <InlineTestControl state={state} latency={test.data?.latency_ms ?? route.last_test_latency_millis} disabled={!route.enabled} onTest={() => test.mutate()} />
      {state === "failure" && failureReason && (
        <p className="row-test-failure" role="status">{failureReason}</p>
      )}
    </div>
  );
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
  const { notify } = useNotify();
  // One key per open form: a retry after a lost response reaches the same
  // record instead of creating a second one, while a deliberate second create
  // opens the form again and gets a new key.
  const idempotencyKey = useRef(crypto.randomUUID());
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
        : api.createRoute(value, idempotencyKey.current);
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["routes"] });
      notify({ tone: "success", title: t(current ? "routes.notifyUpdated" : "routes.notifyCreated"), description: publicModel });
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
