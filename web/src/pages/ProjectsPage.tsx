import { zodResolver } from "@hookform/resolvers/zod";
import { useInfiniteQuery, useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useMemo, useRef, useState } from "react";
import { useForm } from "react-hook-form";
import { z } from "zod";
import { api } from "../api";
import {
  ConfirmButton,
  EmptyState,
  ErrorState,
  Field,
  Loading,
  LoadMore,
  Modal,
  PageHeader,
  ReauthFields,
  ResourceToolbar,
  StatusDot,
  type ResourceStatusFilter,
  type ReauthValues,
} from "../components";
import { compactNumber, money, useInstantFormatter } from "../format";
import { useAccountingTimeZone, zonedInputToISO } from "../timezone";
import type { CreatedGatewayKey, GatewayKey, Project } from "../types";
import { useTranslation } from "react-i18next";
import { useNotify } from "../notifications";
import { useIsReadOnly } from "../session";
import type { TFunction } from "i18next";
import { Link } from "../navigation";
import { hasOnboardingCreateIntent, OnboardingContextBanner } from "../OnboardingContext";

const PAGE_SIZE = "50";

// Mirrors domain.MaxProjectNameLength so the browser and the store agree on the limit.
const MAX_PROJECT_NAME = 128;

const projectSchema = (t: TFunction) => z.object({
  name: z.string().trim().min(1, t("projects.nameRequired")).max(MAX_PROJECT_NAME, t("projects.nameTooLong")),
  // Deliberately stricter than the Admin API, which accepts a project with no
  // model aliases (it just cannot call anything). The console guides the chain,
  // so it requires at least one; the API stays the authority for scripts.
  routes: z.array(z.string()).min(1, t("projects.routeRequired")),
  rpm: z.coerce.number().int().min(0),
  tpm: z.coerce.number().int().min(0),
  concurrency: z.coerce.number().int().min(0),
  budget: z.coerce.number().min(0),
  cidrs: z.string().refine(
    (value) => splitValues(value).every(isCIDR),
    { message: t("projects.cidrInvalid") },
  ),
  tokenGuardPolicyID: z.string(),
  redactionPolicyID: z.string(),
  enabled: z.boolean(),
});
type ProjectInput = z.input<ReturnType<typeof projectSchema>>;
type ProjectValue = z.output<ReturnType<typeof projectSchema>>;

function pageQuery(cursor: string) {
  return `?${new URLSearchParams({ limit: PAGE_SIZE, ...(cursor ? { cursor } : {}) })}`;
}

function projectUpdateBody(project: Project, enabled: boolean) {
  return {
    name: project.name,
    enabled,
    allowed_models: project.allowed_models ?? [],
    rpm: project.rpm,
    tpm: project.tpm,
    max_concurrency: project.max_concurrency,
    daily_budget_micros_usd: project.daily_budget_micros_usd,
    max_input_tokens: project.max_input_tokens,
    max_output_tokens: project.max_output_tokens,
    max_request_bytes: project.max_request_bytes,
    max_stream_duration_seconds: Math.round(project.max_stream_duration / 1_000_000_000),
    allowed_cidrs: project.allowed_cidrs ?? [],
    redaction_policy_id: project.redaction_policy_id,
    token_guard_policy_id: project.token_guard_policy_id,
  };
}

export function ProjectsPage() {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const [selected, setSelected] = useState<string>(() => new URLSearchParams(window.location.search).get("project_id") ?? "");
  const [creating, setCreating] = useState(() => !readOnly && hasOnboardingCreateIntent());
  const [query, setQuery] = useState("");
  const [status, setStatus] = useState<ResourceStatusFilter>("all");
  // The server truncates an unpaged list at 50. A silently missing project is a project
  // whose budget and keys nobody can reach, so every page is followed to the end.
  const projects = useInfiniteQuery({
    queryKey: ["projects", "paged"],
    initialPageParam: "",
    queryFn: ({ pageParam }) => api.projectsPage(pageQuery(pageParam)),
    getNextPageParam: (page) => page.next_cursor || undefined,
  });
  const items = useMemo(
    () => projects.data?.pages.flatMap((page) => page.items) ?? [],
    [projects.data?.pages],
  );
  const filtering = Boolean(query.trim()) || status !== "all";
  // /projects accepts only cursor and limit — any other parameter is a 400 — so filtering
  // happens here. A filter over a partial list would hide matches, so pull the rest first.
  const { hasNextPage, isFetchingNextPage, fetchNextPage } = projects;
  useEffect(() => {
    if (filtering && hasNextPage && !isFetchingNextPage) fetchNextPage();
  }, [filtering, hasNextPage, isFetchingNextPage, fetchNextPage]);
  const visible = useMemo(() => {
    const needle = query.trim().toLowerCase();
    return items.filter((project) =>
      (!needle || project.name.toLowerCase().includes(needle) || project.id.toLowerCase().includes(needle)) &&
      (status === "all" || (status === "enabled") === project.enabled));
  }, [items, query, status]);
  const selectedProject = visible.find((item) => item.id === selected) ?? visible[0];
  const selectedID = selectedProject?.id ?? "";
  return (
    <>
      <PageHeader
        eyebrow={t("projects.eyebrow")}
        title={t("projects.title")}
        description={t("projects.description")}
        action={<button className="button primary" disabled={readOnly} onClick={() => setCreating(true)}>{t("projects.create")}</button>}
      />
      <OnboardingContextBanner />
      {projects.isPending && <Loading />}
      {projects.isError && <ErrorState error={projects.error} />}
      {projects.isSuccess && items.length === 0 && (
        <EmptyState
          title={t("projects.emptyTitle")}
          action={<button className="button primary" disabled={readOnly} onClick={() => setCreating(true)}>{t("projects.first")}</button>}
        >
          {t("projects.emptyDescription")}
        </EmptyState>
      )}
      {items.length > 0 && (
        <div className="split-view">
          <div className="resource-column">
            <ResourceToolbar
              query={query}
              onQueryChange={setQuery}
              queryPlaceholder={t("projects.searchProjects")}
              count={hasNextPage
                ? t("common.resultCount", { visible: visible.length, total: items.length })
                : t("projects.allLoaded", { total: items.length })}
              status={status}
              onStatusChange={setStatus}
            />
            <section className="resource-list" aria-label={t("projects.list")}>
              {visible.map((project) => (
                <button
                  key={project.id}
                  className={selectedID === project.id ? "selected" : ""}
                  aria-current={selectedID === project.id ? "true" : undefined}
                  onClick={() => setSelected(project.id)}
                >
                  <span className="resource-title">
                    <StatusDot ok={project.enabled} label={project.enabled ? t("common.enabled") : t("common.disabled")} />
                    <strong>{project.name}</strong>
                  </span>
                  <span className="resource-meta">
                    {compactNumber(project.rpm)} RPM · {money(project.daily_budget_micros_usd)}{t("projects.perDay")}
                  </span>
                  <code>{project.id}</code>
                </button>
              ))}
              {!visible.length && <p className="quiet-row">{t("common.noMatches")}</p>}
              {hasNextPage && (
                <LoadMore
                  label={isFetchingNextPage ? t("common.loadingMore") : t("common.loadMore")}
                  busy={isFetchingNextPage}
                  onLoad={fetchNextPage}
                />
              )}
            </section>
          </div>
          {selectedProject && <ProjectDetail key={selectedID} project={selectedProject} />}
        </div>
      )}
      {creating && <ProjectForm onClose={() => setCreating(false)} />}
    </>
  );
}

function ProjectDetail({ project }: { project: Project }) {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const [keyDialog, setKeyDialog] = useState(false);
  const [editing, setEditing] = useState(false);
  const keys = useInfiniteQuery({
    queryKey: ["project-keys", project.id, "paged"],
    initialPageParam: "",
    queryFn: ({ pageParam }) => api.keysPage(project.id, pageQuery(pageParam)),
    getNextPageParam: (page) => page.next_cursor || undefined,
  });
  const keyItems = keys.data?.pages.flatMap((page) => page.items) ?? [];
  const { notify } = useNotify();
  const unblock = useMutation({
    mutationFn: (reauth: ReauthValues) => api.unblockProject(project.id, reauth),
    onSuccess: (value) => notify({ tone: "success", title: t("projects.unblocked", { count: value.subjects }), description: project.name }),
  });
  const queryClient = useQueryClient();
  const remove = useMutation({
    mutationFn: (reauth: ReauthValues) => api.deleteProject(project.id, `"${project.revision}"`, reauth),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["projects"] });
      notify({ tone: "success", title: t("projects.notifyDeleted"), description: project.name });
    },
  });
  const toggleStatus = useMutation({
    mutationFn: () => api.updateProject(project.id, projectUpdateBody(project, !project.enabled), `"${project.revision}"`),
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["projects"] });
      notify({ tone: "success", title: t(project.enabled ? "projects.notifyDisabled" : "projects.notifyEnabled"), description: project.name });
    },
  });
  return (
    <section className="detail-panel">
      <header className="detail-title">
        <div>
          <p className="eyebrow">{t("projects.policy")}</p>
          <h2>
            {project.name}
            <span className={`badge ${project.enabled ? "good" : "muted"}`}>
              {project.enabled ? t("common.enabled") : t("common.disabled")}
            </span>
          </h2>
          <code>{project.id}</code>
        </div>
        <div className="row-actions">
          {/* Lifting the block switches off a control that is currently in
              force, so it asks for the same proof a deletion does. */}
          <ConfirmButton
            className="button ghost"
            label={t("projects.unblock")}
            confirmLabel={t("projects.unblockConfirm", { name: project.name })}
            disabled={unblock.isPending}
            requireStepUp
            onConfirm={(reauth) => unblock.mutateAsync(reauth)}
          />
          {/* Disabling a project revokes every gateway key under it at once, which
              is strictly larger than disabling one key — and that already asks.
              The count comes from the keys actually loaded, so a list still being
              paged says "at least". */}
          {project.enabled
            ? <ConfirmButton
              className="button secondary"
              label={t("common.disable")}
              title={t("projects.disableTitle")}
              confirmLabel={t(keys.hasNextPage ? "projects.disableConfirmMoreKeys" : "projects.disableConfirm", { name: project.name, count: keyItems.length })}
              disabled={toggleStatus.isPending}
              onConfirm={() => toggleStatus.mutateAsync()}
            />
            : <button className="button secondary" disabled={readOnly || toggleStatus.isPending} title={readOnly ? t("navigation.readOnlyAction") : undefined} onClick={() => toggleStatus.mutate()}>{toggleStatus.isPending ? t("common.working") : t("common.enable")}</button>}
          <button className="button ghost" disabled={readOnly} title={readOnly ? t("navigation.readOnlyAction") : undefined} onClick={() => setEditing(true)}>{t("common.edit")}</button>
          <ConfirmButton
            label={t("common.delete")}
            confirmLabel={t("projects.deleteConfirm", { name: project.name })}
            disabled={remove.isPending}
            requireStepUp
            onConfirm={(reauth) => remove.mutateAsync(reauth)}
          />
        </div>
      </header>
      {/* Quotas are read rarely and changed through the edit form anyway. Gateway keys are
          what an operator comes here for, so the policy block starts folded away. */}
      <details className="policy-details">
        <summary>
          <svg className="policy-details-chevron" viewBox="0 0 16 16" aria-hidden="true"><path d="M6 3.5 10.5 8 6 12.5" /></svg>
          <span className="policy-details-label">{t("projects.policyDetails")}</span>
          <small>{[
            (project.allowed_models ?? []).length
              ? t("projects.modelCount", { count: (project.allowed_models ?? []).length })
              : t("common.none"),
            `${compactNumber(project.rpm)} RPM`,
            project.daily_budget_micros_usd ? money(project.daily_budget_micros_usd) : t("common.unlimited"),
          ].join(" · ")}</small>
          {/* Looks and behaves like the expand control the provider and deployment rows
              use. Not a real button — nesting one inside <summary> is invalid, and the
              summary already exposes the expanded state to assistive tech. */}
          <span className="button ghost policy-details-toggle" aria-hidden="true">
            <span className="on-closed">{t("common.expandDetails")}</span>
            <span className="on-open">{t("common.collapseDetails")}</span>
          </span>
        </summary>
        <div className="policy-grid">
          <Policy label={t("projects.allowedModels")} value={(project.allowed_models ?? []).join(", ") || t("common.none")} />
          <Policy label={t("projects.rateLimit")} value={`${compactNumber(project.rpm)} RPM / ${compactNumber(project.tpm)} TPM`} />
          <Policy label={t("projects.concurrency")} value={String(project.max_concurrency || t("common.unlimited"))} />
          <Policy label={t("projects.dailyBudget")} value={project.daily_budget_micros_usd ? money(project.daily_budget_micros_usd) : t("common.unlimited")} />
          <Policy label={t("projects.tokenGuardPolicy")} value={project.token_guard_policy_id || t("projects.notAttached")} />
        </div>
      </details>
      {unblock.isError && <ErrorState error={unblock.error} />}
      {toggleStatus.isError && <ErrorState error={toggleStatus.error} />}
      {remove.isError && <ErrorState error={remove.error} />}
      <header className="section-header">
        <div><p className="eyebrow">{t("projects.credentials")}</p><h3>{t("projects.gatewayKeys")}</h3></div>
        <button className="button secondary" disabled={readOnly} onClick={() => setKeyDialog(true)}>{t("projects.createKey")}</button>
      </header>
      {keys.isPending && <Loading label={t("projects.loadingKeys")} />}
      {keys.isError && <ErrorState error={keys.error} />}
      {keys.isSuccess && keyItems.length === 0 && (
        <EmptyState
          title={t("projects.noKeysTitle")}
          action={<button className="button secondary" disabled={readOnly} onClick={() => setKeyDialog(true)}>{t("projects.createKey")}</button>}
        >
          {t("projects.noKeys")}
        </EmptyState>
      )}
      {keyItems.map((key) => <KeyRow project={project} value={key} key={key.id} />)}
      {keys.hasNextPage && (
        <LoadMore
          label={keys.isFetchingNextPage ? t("common.loadingMore") : t("common.loadMore")}
          busy={keys.isFetchingNextPage}
          onLoad={keys.fetchNextPage}
        />
      )}
      {keyDialog && <CreateKey project={project} onClose={() => setKeyDialog(false)} />}
      {editing && <ProjectForm current={project} onClose={() => setEditing(false)} />}
    </section>
  );
}

function KeyRow({ project, value }: { project: Project; value: GatewayKey }) {
  const { t } = useTranslation();
  const readOnly = useIsReadOnly();
  const dateTime = useInstantFormatter();
  const queryClient = useQueryClient();
  const refresh = () => queryClient.invalidateQueries({ queryKey: ["project-keys", project.id] });
  const { notify } = useNotify();
  const mutation = useMutation({
    mutationFn: () => api.updateKey(
      project.id,
      value.id,
      { name: value.name, enabled: !value.enabled, ...(value.expires_at ? { expires_at: value.expires_at } : {}) },
      value.revision,
    ),
    onSuccess: () => {
      refresh();
      notify({ tone: "success", title: t(value.enabled ? "projects.notifyKeyDisabled" : "projects.notifyKeyEnabled"), description: value.name });
    },
    // No onError: this mutation renders an ErrorState in place, which carries the
    // reason. A second copy in the notification column says less and, on the
    // confirm-gated path, appears above a modal whose Tab trap cannot reach it.
  });
  const remove = useMutation({
    mutationFn: (reauth: ReauthValues) => api.deleteKey(project.id, value.id, value.revision, reauth),
    onSuccess: () => {
      refresh();
      notify({ tone: "success", title: t("projects.notifyKeyDeleted"), description: value.name });
    },
  });
  const expired = Boolean(value.expires_at && new Date(value.expires_at).getTime() <= Date.now());
  const live = value.enabled && !expired;
  return (
    <>
      <div id={`gateway-key-${value.id}`} className="key-row">
        <div>
          <span>
            <StatusDot ok={live} label={live ? t("common.enabled") : expired ? t("projects.expired") : t("common.disabled")} />
            <strong>{value.name}</strong>
          </span>
          <code>{value.id}</code>
        </div>
        <div className="key-dates">
          <small>{t("projects.created", { date: dateTime(value.created_at) })}</small>
          <small className={expired ? "key-expiry expired" : "key-expiry"}>
            {value.expires_at
              ? t(expired ? "projects.expiredAt" : "projects.expiresAt", { date: dateTime(value.expires_at) })
              : t("projects.neverExpires")}
          </small>
        </div>
        <div className="row-actions">
          {value.enabled ? <ConfirmButton className="button ghost" label={t("common.disable")} title={t("projects.keyDisableTitle")} confirmLabel={t("projects.keyDisableConfirm", { name: value.name })} disabled={mutation.isPending} onConfirm={() => mutation.mutateAsync()} /> : <button className="button ghost" disabled={readOnly || mutation.isPending} onClick={() => mutation.mutate()}>{mutation.isPending ? t("common.working") : t("common.enable")}</button>}
          <ConfirmButton
            label={t("common.delete")}
            confirmLabel={t("projects.keyDeleteConfirm", { name: value.name })}
            disabled={remove.isPending}
            requireStepUp
            onConfirm={(reauth) => remove.mutateAsync(reauth)}
          />
        </div>
      </div>
      {/* A revocation that silently failed leaves a live credential the operator believes
          is gone, so both key mutations report their errors in place. */}
      {mutation.isError && <ErrorState error={mutation.error} />}
      {remove.isError && <ErrorState error={remove.error} />}
    </>
  );
}

function ProjectForm({ current, onClose }: { current?: Project; onClose: () => void }) {
  const { t } = useTranslation();
  const schema = useMemo(() => projectSchema(t), [t]);
  const queryClient = useQueryClient();
  const policies = useQuery({
    queryKey: ["token-guard-policies"],
    queryFn: api.tokenGuardPolicies,
  });
  const redactionPolicies = useQuery({
    queryKey: ["redaction-policies"],
    queryFn: api.redactionPolicies,
  });
  const availableRoutes = useQuery({
    queryKey: ["project-route-aliases"],
    queryFn: api.allRoutes,
  });
  const routeOptions = useMemo(() => {
    const retained = new Set(current?.allowed_models ?? []);
    const options = new Map<string, { enabledCount: number; strategies: Set<string> }>();
    retained.forEach((value) => options.set(value, { enabledCount: 0, strategies: new Set() }));
    availableRoutes.data?.forEach((route) => {
      const existing = options.get(route.public_model) ?? { enabledCount: 0, strategies: new Set<string>() };
      // Both readings come from the enabled routes only. Counting the strategy
      // of a disabled route made a retired row with a different strategy hide
      // the label entirely, beside a count that had already ignored that row.
      if (!route.enabled) return;
      existing.enabledCount += 1;
      existing.strategies.add(route.strategy || "ordered");
      options.set(route.public_model, existing);
    });
    return Array.from(options, ([value, state]) => ({ value, ...state }))
      .filter((option) => option.enabledCount > 0 || retained.has(option.value))
      .sort((a, b) => a.value.localeCompare(b.value));
  }, [availableRoutes.data, current?.allowed_models]);
  const {
    register,
    handleSubmit,
    watch,
    formState: { errors, isDirty },
  } = useForm<ProjectInput, unknown, ProjectValue>({
    resolver: zodResolver(schema),
    defaultValues: {
      name: current?.name ?? "",
      rpm: current?.rpm ?? 60,
      tpm: current?.tpm ?? 100_000,
      concurrency: current?.max_concurrency ?? 8,
      budget: (current?.daily_budget_micros_usd ?? 50_000_000) / 1_000_000,
      routes: current?.allowed_models ?? [],
      cidrs: (current?.allowed_cidrs ?? []).join(", "),
      tokenGuardPolicyID: current?.token_guard_policy_id ?? "",
      redactionPolicyID: current?.redaction_policy_id ?? "",
      enabled: current?.enabled ?? true,
    },
  });
  const selectedRouteAliases = watch("routes") ?? [];
  const enabled = watch("enabled");
  const { notify } = useNotify();
  // One key per open form: a retry after a lost response reaches the same
  // record instead of creating a second one, while a deliberate second create
  // opens the form again and gets a new key.
  const idempotencyKey = useRef(crypto.randomUUID());
  const mutation = useMutation({
    mutationFn: (value: ProjectValue) => {
      const body = {
      name: value.name,
      enabled: value.enabled,
      allowed_models: value.routes,
      rpm: value.rpm,
      tpm: value.tpm,
      max_concurrency: value.concurrency,
      daily_budget_micros_usd: Math.round(value.budget * 1_000_000),
      max_input_tokens: current?.max_input_tokens ?? 128_000,
      max_output_tokens: current?.max_output_tokens ?? 16_384,
      max_request_bytes: current?.max_request_bytes ?? 1_048_576,
      max_stream_duration_seconds: current ? Math.round(current.max_stream_duration / 1_000_000_000) : 600,
      allowed_cidrs: splitValues(value.cidrs),
      redaction_policy_id: value.redactionPolicyID,
      token_guard_policy_id: value.tokenGuardPolicyID,
      };
      return current
        ? api.updateProject(current.id, body, `"${current.revision}"`)
        : api.createProject(body, idempotencyKey.current);
    },
    onSuccess: (_result, value) => {
      queryClient.invalidateQueries({ queryKey: ["projects"] });
      notify({ tone: "success", title: t(current ? "projects.notifyUpdated" : "projects.notifyCreated"), description: value.name });
      onClose();
    },
  });
  return (
    // Dismissing the dialog mid-flight would unmount the mutation before its success
    // handler refreshes the list, leaving the console showing stale server state.
    <Modal wide closeDisabled={mutation.isPending} dirty={isDirty} title={current ? t("projects.edit") : t("projects.createTitle")} onClose={onClose}>
      <form
        className="project-form"
        onSubmit={handleSubmit((value) => {
          // Enter inside a field submits natively, bypassing the disabled button.
          if (mutation.isPending) return;
          mutation.mutate(value);
        })}
      >
        <section className="project-form-section" aria-labelledby="project-basics-title">
          <header><h3 id="project-basics-title">{t("projects.basicInfo")}</h3><p>{t("projects.basicInfoDescription")}</p></header>
          <div className="form-grid">
            <Field label={t("projects.name")} error={errors.name?.message}><input data-modal-initial maxLength={MAX_PROJECT_NAME} {...register("name")} /></Field>
            <div className="model-picker" role="group" aria-labelledby="project-model-title" aria-describedby="project-model-help">
              <div className="model-picker-heading">
                <strong id="project-model-title">{t("projects.aliases")}</strong>
                <span className="model-picker-selection-count">{t("projects.selectedAliases", { count: selectedRouteAliases.length })}</span>
              </div>
              <p id="project-model-help">{t("projects.aliasesHint")}</p>
              {availableRoutes.isPending ? <Loading label={t("projects.loadingModels")} /> : routeOptions.length ? <div className="route-alias-picker">
                <div className="model-option-grid">
                  {routeOptions.map((route) => {
                    const strategy = route.strategies.size === 1 ? Array.from(route.strategies)[0] : "";
                    return <label className="model-option" key={route.value}>
                      <input type="checkbox" value={route.value} {...register("routes")} />
                      <span>
                        <strong>{route.value}</strong>
                        <small className={route.enabledCount ? undefined : "unavailable"}>{route.enabledCount ? t("projects.enabledRouteCount", { count: route.enabledCount }) : t("projects.unavailableAlias")}{strategy && ` · ${strategy === "round_robin" ? t("routes.roundRobin") : t("routes.ordered")}`}</small>
                      </span>
                    </label>;
                  })}
                </div>
              </div> : <div className="notice warning"><strong>{t("projects.noConfiguredModels")}</strong><span>{t("projects.noConfiguredModelsDescription")}</span><Link className="notice-link" href="/admin/routes">{t("projects.openRoutes")}</Link></div>}
              {errors.routes?.message && <small className="field-error" role="alert">{errors.routes.message}</small>}
            </div>
          </div>
        </section>
        <section className="project-form-section" aria-labelledby="project-capacity-title">
          <header><h3 id="project-capacity-title">{t("projects.capacityControls")}</h3><p>{t("projects.capacityControlsDescription")}</p></header>
          <div className="form-grid compact-number-grid">
            <Field label={t("projects.rpm")}><input type="number" {...register("rpm")} /></Field>
            <Field label={t("projects.tpm")}><input type="number" {...register("tpm")} /></Field>
            <Field label={t("projects.maxConcurrency")}><input type="number" {...register("concurrency")} /></Field>
            <Field label={t("projects.dailyBudgetUSD")}><input type="number" step="0.01" {...register("budget")} /></Field>
          </div>
        </section>
        <section className="project-form-section" aria-labelledby="project-security-title">
          <header><h3 id="project-security-title">{t("projects.securityControls")}</h3><p>{t("projects.securityControlsDescription")}</p></header>
          <div className="form-grid">
            <Field label={t("projects.tokenGuardPolicy")}><select {...register("tokenGuardPolicyID")}><option value="">{t("projects.noBinding")}</option>{policies.data?.items.filter((policy) => policy.enabled).map((policy) => <option value={policy.id} key={policy.id}>{policy.name} · {policy.action === "temporary_block" ? t("policies.temporaryBlock") : policy.action === "alert" ? t("policies.alert") : t("policies.observe")}</option>)}</select></Field>
            <Field label={t("projects.redactionPolicy")}><select {...register("redactionPolicyID")}><option value="">{t("projects.noBinding")}</option>{redactionPolicies.data?.items.filter((policy) => policy.enabled).map((policy) => <option value={policy.id} key={policy.id}>{policy.name} · {policy.mode === "strict" ? t("redaction.strictBadge") : policy.mode === "bounded_stream" ? t("redaction.boundedBadge") : t("redaction.detectStreamBadge")}</option>)}</select></Field>
            <Field label={t("projects.allowedCIDR")} hint={t("projects.cidrHint")} error={errors.cidrs?.message}><textarea rows={3} placeholder={t("projects.cidrPlaceholder")} {...register("cidrs")} /></Field>
          </div>
        </section>
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions sticky-form-actions">
          <div className="form-footer-state">
            <label className="form-footer-enable">
              <input type="checkbox" {...register("enabled")} />
              <span><strong>{t("projects.enable")} · {enabled ? t("common.enabled") : t("common.disabled")}</strong><small>{t("projects.enableDescription")}</small></span>
            </label>
          </div>
          <button type="button" className="button ghost" disabled={mutation.isPending} onClick={onClose}>{t("common.cancel")}</button>
          <button className="button primary" disabled={mutation.isPending}>{mutation.isPending ? t("common.working") : current ? t("projects.save") : t("projects.createSubmit")}</button>
        </div>
      </form>
    </Modal>
  );
}

function CreateKey({ project, onClose }: { project: Project; onClose: () => void }) {
  const { t } = useTranslation();
  const [name, setName] = useState("");
  const [expiresAt, setExpiresAt] = useState("");
  const timeZone = useAccountingTimeZone();
  const [created, setCreated] = useState<CreatedGatewayKey | null>(null);
  const [acknowledged, setAcknowledged] = useState(false);
  const [copied, setCopied] = useState(false);
  const [copyFailed, setCopyFailed] = useState(false);
  const { notify } = useNotify();
  // One key per dialog: a retry after a timeout replays this token, so the server can
  // recognise the second attempt instead of issuing a key the operator never sees.
  const idempotencyKey = useRef(crypto.randomUUID());
  const queryClient = useQueryClient();
  // Minting a key asks for the same proof a deletion does: the plaintext is
  // shown once and the key outlives this session, so a stolen session that
  // could not delete anything could still leave with durable billable access.
  const [reauth, setReauth] = useState<ReauthValues>({ currentPassword: "", totpCode: "" });
  const mutation = useMutation({
    mutationFn: () => api.createKey(
      project.id,
      name,
      idempotencyKey.current,
      reauth,
      // Read in the accounting zone, which is the zone the key row renders the
      // expiry back in; the browser's own wall clock would have made the two
      // disagree by the offset between them.
      zonedInputToISO(expiresAt, timeZone) || undefined,
    ),
    onSuccess: (result) => {
      setCreated(result.data);
      queryClient.invalidateQueries({ queryKey: ["project-keys", project.id] });
      // The plaintext key stays in the dialog that can gate its own close; the
      // column only says a key now exists, and never carries the secret.
      notify({ tone: "success", title: t("projects.notifyKeyCreated"), description: name });
    },
  });
  const safelyClose = () => {
    if (created && !acknowledged) return;
    setCreated(null);
    onClose();
  };
  if (created) {
    return (
      // Closing before the acknowledgement destroys the only copy of the plaintext, so
      // the dialog reports the close control as unavailable rather than ignoring clicks.
      <Modal title={t("projects.saveKey")} closeDisabled={!acknowledged} onClose={safelyClose}>
        <div className="one-time-secret">
          <div className="notice error">
            <strong>{t("projects.oneTime")}</strong>
            <span>{t("projects.oneTimeDescription")}</span>
          </div>
          <code className="secret-value">{created.key}</code>
          <button
            className="button secondary wide"
            onClick={async () => {
              try {
                await navigator.clipboard.writeText(created.key);
                setCopied(true);
                setCopyFailed(false);
              } catch {
                // Clipboard access is denied outside a secure context. Say so instead of
                // leaving a button that silently does nothing.
                setCopyFailed(true);
              }
            }}
          >
            {copied ? t("common.copied") : t("projects.copyKey")}
          </button>
          {copyFailed && <small className="field-error" role="alert">{t("projects.copyFailed")}</small>}
          <label className="check-row">
            <input type="checkbox" checked={acknowledged} onChange={(event) => setAcknowledged(event.target.checked)} />
            <span>{t("projects.keyStored")}</span>
          </label>
          <button className="button primary wide" disabled={!acknowledged} onClick={safelyClose}>
            {t("projects.finish")}
          </button>
        </div>
      </Modal>
    );
  }
  return (
    <Modal title={t("projects.createKeyTitle")} closeDisabled={mutation.isPending} onClose={onClose}>
      <form
        onSubmit={(event) => {
          event.preventDefault();
          // Without this guard, holding Enter mints extra keys whose plaintext is never
          // shown — live credentials nobody knows exist.
          if (mutation.isPending) return;
          if (name.trim()) mutation.mutate();
        }}
      >
        <Field label={t("projects.keyName")} hint={t("projects.keyNameHint")}>
          <input data-modal-initial value={name} onChange={(event) => setName(event.target.value)} />
        </Field>
        <Field label={t("projects.keyExpiry")} hint={t("projects.keyExpiryHint")}>
          <input type="datetime-local" value={expiresAt} onChange={(event) => setExpiresAt(event.target.value)} />
        </Field>
        <ReauthFields values={reauth} onChange={setReauth} description={t("auth.stepUpMintKey")} />
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions">
          <button type="button" className="button ghost" disabled={mutation.isPending} onClick={onClose}>{t("common.cancel")}</button>
          <button className="button primary" disabled={!name.trim() || !reauth.currentPassword || mutation.isPending}>{mutation.isPending ? t("common.working") : t("projects.generateKey")}</button>
        </div>
      </form>
    </Modal>
  );
}

function Policy({ label, value }: { label: string; value: string }) {
  return <div><small>{label}</small><strong>{value}</strong></div>;
}

function splitValues(value: string) {
  return value.split(/[,\n]/).map((item) => item.trim()).filter(Boolean);
}

// The server parses these with netip.ParsePrefix and rejects the whole project on the
// first bad entry. Catch it in the field so the operator sees which value is wrong.
function isCIDR(value: string) {
  const [address, bits, ...rest] = value.split("/");
  if (rest.length || !address) return false;
  const isIPv6 = address.includes(":");
  if (bits !== undefined) {
    if (!/^\d{1,3}$/.test(bits) || Number(bits) > (isIPv6 ? 128 : 32)) return false;
  }
  if (isIPv6) return /^[0-9a-fA-F:]+$/.test(address) && (address.match(/::/g) ?? []).length <= 1;
  const octets = address.split(".");
  return octets.length === 4 && octets.every((octet) => /^\d{1,3}$/.test(octet) && Number(octet) <= 255);
}
