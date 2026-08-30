import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useRef, useState, type FormEvent } from "react";
import { api } from "../api";
import {
  ConfirmButton,
  EmptyState,
  ErrorState,
  Combobox,
  Field,
  InlineTestControl,
  isStepUpPrompt,
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
  // on what else still serves it, so the confirmation has to count — and a
  // withheld sibling does not serve it. Counting `enabled` alone told the
  // operator "1 enabled route keeps answering" about a route this very page was
  // showing as withheld, and the alias went dark on the next request.
  const siblingServingCount = useMemo(() => {
    const counts = new Map<string, number>();
    routes.data?.items.forEach((route) => {
      if (route.enabled && !route.withheld) counts.set(route.public_model, (counts.get(route.public_model) ?? 0) + 1);
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
  // A failed deployments read cannot be reported as "no usable target": that is
  // a claim about the configuration, and this is a claim about one request.
  const targetStateUnknown = deployments.isError;
  // One alias is one group, because that is what the engine resolves against —
  // `public_model` is the key its target map is built on. The list is stored
  // sorted by route ID, so before this the members of an alias landed wherever
  // their random IDs put them, with another alias in between and nothing
  // saying the two rows were one failover chain.
  const aliasGroups = useMemo(() => {
    const byAlias = new Map<string, Route[]>();
    routes.data?.items.forEach((route) => {
      byAlias.set(route.public_model, [...(byAlias.get(route.public_model) ?? []), route]);
    });
    return Array.from(byAlias, ([alias, items]) => {
      // Priority ascending, route ID as the tie-break: the registry's own
      // ordering (`slices.SortFunc` in provider.go), so row order is the order
      // the engine tries them in rather than the order they were created.
      const ordered = [...items].sort((left, right) =>
        left.priority - right.priority || left.id.localeCompare(right.id));
      const candidates = targetStateUnknown ? [] : ordered.filter((route) => {
        if (!route.enabled || route.withheld) return false;
        const deployment = deploymentByID.get(route.deployment_id);
        return !!deployment?.enabled && deployment.probe?.state !== "unhealthy";
      });
      // The engine reads `targets[0].Strategy` and ignores the rest, so the
      // group states the one in force rather than requiring unanimity — and
      // says so when the others disagree, since editing them changes nothing.
      const strategy = candidates[0]?.strategy || "ordered";
      const mixed = candidates.some((route) => (route.strategy || "ordered") !== strategy);
      return { alias, ordered, candidates, strategy, mixed };
    }).sort((left, right) => left.alias.localeCompare(right.alias));
  }, [routes.data, deploymentByID, targetStateUnknown]);
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
      {remove.isError && !isStepUpPrompt(remove.error) && <ErrorState error={remove.error} />}
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
                <th scope="col">{t("routes.routeColumn")}</th>
                <th scope="col">{t("routes.deployment")}</th>
                <th scope="col">{t("routes.provider")}</th>
                <th scope="col">{t("routes.upstreamModel")}</th>
                <th scope="col">{t("routes.strategy")}</th>
                <th scope="col">{t("routes.priority")}</th>
                <th scope="col">{t("routes.status")}</th>
                <th scope="col" />
              </tr>
            </thead>
            {aliasGroups.map((group) => {
              const summary = targetStateUnknown
                ? t("aliasTargets.unknown")
                : group.candidates.length === 0
                  ? t("aliasTargets.none")
                  : group.candidates.length === 1
                    ? t("aliasTargets.single")
                    : `${t("aliasTargets.count", { count: group.candidates.length })} · ${group.strategy === "round_robin" ? t("routes.roundRobin") : t("routes.ordered")}`;
              // Only ordered failover has a first target. Round robin rotates
              // the starting point on every request, so marking one row primary
              // there would name a precedence that does not exist.
              const primaryID = !targetStateUnknown && group.candidates.length > 1 && group.strategy === "ordered"
                ? group.candidates[0].id
                : "";
              return (
            <tbody key={group.alias}>
              <tr className="route-group-heading">
                <th colSpan={8} scope="colgroup">
                  <strong>{group.alias}</strong>
                  <span>{summary}</span>
                  {group.mixed && <span className="badge warning" title={t("routes.mixedStrategyTitle")}>{t("routes.mixedStrategy")}</span>}
                </th>
              </tr>
              {group.ordered.map((route) => {
                const deployment = deploymentByID.get(route.deployment_id);
                const providerID = deployment?.provider_id || "";
                // What still serves the alias once THIS row is off: the row
                // itself is only subtracted when it was serving, so disabling a
                // withheld route does not report the alias as going dark when a
                // healthy sibling is still answering.
                const serving = siblingServingCount.get(route.public_model) ?? 0;
                const remainingAfterDisable = serving - (route.enabled && !route.withheld ? 1 : 0);
                return (
                  <tr id={`route-${route.id}`} key={route.id}>
                    <td>
                      <div className="model-cell">
                        <StatusDot ok={route.enabled && !route.withheld} />
                        <code>{route.id}</code>
                        {route.id === primaryID && <span className="badge" title={t("routes.primaryTargetTitle")}>{t("routes.primaryTarget")}</span>}
                      </div>
                    </td>
                    <td>{deployment?.name || route.deployment_id}</td>
                    <td>{providerNames.get(providerID) || providerID}</td>
                    <td>{deployment?.provider_model}</td>
                    <td><span className="badge">{(route.strategy || "ordered") === "round_robin" ? t("routes.roundRobin") : t("routes.ordered")}</span></td>
                    <td>{route.priority}</td>
                    <td>
                      {/* Enabled is what the operator asked for; it used to be
                          the only thing this column could say. A route the
                          registry refused is up and serving nothing, and until
                          the server started reporting it, only the log knew. */}
                      {route.enabled && route.withheld ? (
                        <div className="route-withheld-cell">
                          <span className="badge warning">{t("routes.withheld")}</span>
                          {/* The reason is shown rather than hidden in a
                              tooltip: which of them it is decides whether the
                              repair is on the deployment, the connection or
                              this route. */}
                          <small className="resource-state">{t(
                            `routes.withheldReasons.${route.withheld.kind === "capability_drift" ? "capability_drift" : route.withheld.reason}`,
                            { defaultValue: t("routes.withheldReasons.unknown") },
                          )}</small>
                        </div>
                      ) : (
                        <span className={`resource-state ${route.enabled ? "enabled" : ""}`}>
                          {route.enabled ? t("common.enabled") : t("common.disabled")}
                        </span>
                      )}
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
                              confirmLabel={remainingAfterDisable > 0
                                ? t("routes.disableConfirm", { name: route.public_model, count: remainingAfterDisable })
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
              );
            })}
          </table>
        </div>
      )}
      {creating && (
        <RouteForm routes={routes.data?.items ?? []} deployments={deployments.data?.items ?? []} onClose={() => setCreating(false)} />
      )}
      {editing && (
        <RouteForm current={editing} routes={routes.data?.items ?? []} deployments={deployments.data?.items ?? []} onClose={() => setEditing(undefined)} />
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

function RouteForm({ current, routes, deployments, onClose }: { current?: Route; routes: Route[]; deployments: Deployment[]; onClose: () => void }) {
  const { t } = useTranslation();
  const enabled = deployments.filter((item) => item.enabled || item.id === current?.deployment_id);
  // Empty rather than a guessed "chat": the field is a combobox now, and a
  // prefilled value is also a filter — it would hide every alias that does not
  // contain it, which is the opposite of offering what exists.
  const [publicModel, setPublicModel] = useState(current?.public_model ?? "");
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
  const deploymentNames = useMemo(
    () => new Map(deployments.map((item) => [item.id, `${item.name} · ${item.provider_model}`])),
    [deployments],
  );
  // The alias is the grouping key, so typing one that already exists joins that
  // group rather than creating something new. It was a bare text box, which
  // made the two outcomes look identical and a typo silently produce a second
  // alias with one target — no error, and the project that authorized the first
  // one cannot reach it.
  const aliasOptions = useMemo(() => {
    const targets = new Map<string, number>();
    routes.forEach((route) => {
      if (route.enabled) targets.set(route.public_model, (targets.get(route.public_model) ?? 0) + 1);
    });
    return Array.from(new Set(routes.map((route) => route.public_model)))
      .sort((left, right) => left.localeCompare(right))
      .map((alias) => ({
        value: alias,
        // How many enabled routes the alias already has, so picking one is not
        // a blind choice between names that look alike.
        secondary: t("routes.aliasTargetCount", { count: targets.get(alias) ?? 0 }),
      }));
  }, [routes, t]);
  // What the operator is about to join. Facts only: which targets the alias
  // already has, in the order the engine tries them. What joining it *costs*
  // — the Phase 2 endpoints it gives up, the projects whose reach it widens —
  // is C2's other half and waits on D1 and D5.
  const joining = useMemo(() => {
    const alias = publicModel.trim();
    const siblings = routes
      .filter((route) => route.public_model === alias && route.id !== current?.id)
      .sort((left, right) => left.priority - right.priority || left.id.localeCompare(right.id));
    if (siblings.length === 0) return undefined;
    const enabledSiblings = siblings.filter((route) => route.enabled);
    return {
      alias,
      siblings,
      // Both are refusals the Admin API already makes. Saying so here turns a
      // 400 after the fact into something visible before the save.
      duplicateDeployment: routeEnabled
        && enabledSiblings.some((route) => route.deployment_id === deploymentID),
      strategyConflict: routeEnabled
        && enabledSiblings.some((route) => (route.strategy || "ordered") !== strategy),
    };
  }, [routes, publicModel, current?.id, deploymentID, strategy, routeEnabled]);
  return (
    <Modal title={current ? t("routes.edit") : t("routes.createTitle")} dirty={dirty} onClose={onClose}>
      {enabled.length === 0 ? (
        <div className="notice warning"><strong>{t("routes.deploymentRequired")}</strong><span>{t("routes.deploymentRequiredDescription")}</span><Link className="notice-link" href="/admin/deployments">{t("routes.openDeployments")}</Link></div>
      ) : (
        <form className="route-form" onSubmit={submit}>
          <div className="route-form-body form-grid">
          {/* Pick an existing alias to join its group, or type a new name to
              start one. Both outcomes stay reachable — the list is a
              suggestion, not a constraint, because the first route on a new
              alias has nothing to pick from. This is the console's own
              combobox rather than a native <datalist>, whose popup the browser
              draws itself and no stylesheet can reach. */}
          <Combobox
            label={t("routes.publicAlias")}
            value={publicModel}
            onChange={setPublicModel}
            options={aliasOptions}
            listLabel={t("routes.aliasListLabel")}
            emptyText={t("routes.aliasNoMatches")}
            note={t("routes.publicAliasHint")}
            required
            autoFocus
          />
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
          <Field label={t("routes.priority")} hint={t("routes.priorityHint")}><input autoComplete="off" type="number" value={priority} onChange={(event) => setPriority(Number(event.target.value))} /></Field>
          {joining && (
            <section className="route-join" aria-label={t("routes.joiningAliasLabel", { alias: joining.alias })}>
              <p className="route-join-lede">{t("routes.joiningAlias", { alias: joining.alias, count: joining.siblings.length })}</p>
              {/* In the engine's order, so the priority being typed above can be
                  placed against the ones already there rather than guessed. The
                  priority sits in its own column, right-aligned and tabular, so
                  the numbers line up as a column of numbers rather than as the
                  first word of each row. */}
              <ul className="route-join-targets">
                {joining.siblings.map((sibling) => (
                  <li key={sibling.id}>
                    <span className="route-join-priority">{sibling.priority}</span>
                    <span className="route-join-target">{deploymentNames.get(sibling.deployment_id) || sibling.deployment_id}</span>
                    <span className="route-join-meta">
                      {(sibling.strategy || "ordered") === "round_robin" ? t("routes.roundRobin") : t("routes.ordered")}
                      {sibling.enabled ? "" : ` · ${t("common.disabled")}`}
                    </span>
                  </li>
                ))}
              </ul>
              {joining.duplicateDeployment && <p className="route-join-refusal">{t("routes.joinDuplicateDeployment")}</p>}
              {joining.strategyConflict && <p className="route-join-refusal">{t("routes.joinStrategyConflict")}</p>}
            </section>
          )}
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
