import { zodResolver } from "@hookform/resolvers/zod";
import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useMemo, useState } from "react";
import { useForm } from "react-hook-form";
import { z } from "zod";
import { api } from "../api";
import {
  ConfirmButton,
  EmptyState,
  ErrorState,
  Field,
  Loading,
  Modal,
  PageHeader,
  StatusDot,
} from "../components";
import { compactNumber, dateTime, money } from "../format";
import type { CreatedGatewayKey, GatewayKey, Project } from "../types";
import { useTranslation } from "react-i18next";
import type { TFunction } from "i18next";
import { Link } from "../navigation";

const projectSchema = (t: TFunction) => z.object({
  name: z.string().trim().min(1, t("projects.nameRequired")).max(128),
  routes: z.array(z.string()).min(1, t("projects.routeRequired")),
  rpm: z.coerce.number().int().min(0),
  tpm: z.coerce.number().int().min(0),
  concurrency: z.coerce.number().int().min(0),
  budget: z.coerce.number().min(0),
  cidrs: z.string(),
  tokenGuardPolicyID: z.string(),
  redactionPolicyID: z.string(),
  enabled: z.boolean(),
});
type ProjectInput = z.input<ReturnType<typeof projectSchema>>;
type ProjectValue = z.output<ReturnType<typeof projectSchema>>;

export function ProjectsPage() {
  const { t } = useTranslation();
  const [selected, setSelected] = useState<string>("");
  const [creating, setCreating] = useState(false);
  const projects = useQuery({ queryKey: ["projects"], queryFn: api.projects });
  const items = projects.data?.items ?? [];
  const selectedProject = items.find((item) => item.id === selected) ?? items[0];
  const selectedID = selectedProject?.id ?? "";
  return (
    <>
      <PageHeader
        eyebrow={t("projects.eyebrow")}
        title={t("projects.title")}
        description={t("projects.description")}
        action={<button className="button primary" onClick={() => setCreating(true)}>{t("projects.create")}</button>}
      />
      {projects.isPending && <Loading />}
      {projects.isError && <ErrorState error={projects.error} />}
      {projects.isSuccess && items.length === 0 && (
        <EmptyState
          title={t("projects.emptyTitle")}
          action={<button className="button primary" onClick={() => setCreating(true)}>{t("projects.first")}</button>}
        >
          {t("projects.emptyDescription")}
        </EmptyState>
      )}
      {items.length > 0 && (
        <div className="split-view">
          <section className="resource-list" aria-label={t("projects.list")}>
            {items.map((project) => (
              <button
                key={project.id}
                className={selectedID === project.id ? "selected" : ""}
                onClick={() => setSelected(project.id)}
              >
                <span className="resource-title">
                  <StatusDot ok={project.enabled} />
                  <strong>{project.name}</strong>
                </span>
                <span className="resource-meta">
                  {compactNumber(project.rpm)} RPM · {money(project.daily_budget_micros_usd)}{t("projects.perDay")}
                </span>
                <code>{project.id}</code>
              </button>
            ))}
          </section>
          <ProjectDetail project={selectedProject!} />
        </div>
      )}
      {creating && <ProjectForm onClose={() => setCreating(false)} />}
    </>
  );
}

function ProjectDetail({ project }: { project: Project }) {
  const { t } = useTranslation();
  const [keyDialog, setKeyDialog] = useState(false);
  const [editing, setEditing] = useState(false);
  const [unblockResult, setUnblockResult] = useState("");
  const keys = useQuery({
    queryKey: ["project-keys", project.id],
    queryFn: () => api.keys(project.id),
  });
  const unblock = useMutation({
    mutationFn: () => api.unblockProject(project.id),
    onSuccess: (value) => setUnblockResult(t("projects.unblocked", { count: value.subjects })),
  });
  const queryClient = useQueryClient();
  const remove = useMutation({
    mutationFn: () => api.deleteProject(project.id, `"${project.revision}"`),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["projects"] }),
  });
  return (
    <section className="detail-panel">
      <header className="detail-title">
        <div>
          <p className="eyebrow">{t("projects.policy")}</p>
          <h2>{project.name}</h2>
          <code>{project.id}</code>
        </div>
        <div className="row-actions">
          <button className="button ghost" disabled={unblock.isPending} onClick={() => unblock.mutate()}>{t("projects.unblock")}</button>
          <button className="button ghost" onClick={() => setEditing(true)}>{t("common.edit")}</button>
          <ConfirmButton
            label={t("common.delete")}
            confirmLabel={t("projects.deleteConfirm", { name: project.name })}
            disabled={remove.isPending}
            onConfirm={() => remove.mutate()}
          />
          <span className={`badge ${project.enabled ? "good" : ""}`}>
            {project.enabled ? t("common.enabled") : t("common.disabled")}
          </span>
        </div>
      </header>
      <div className="policy-grid">
        <Policy label={t("projects.allowedModels")} value={project.allowed_routes.join(", ") || t("common.none")} />
        <Policy label={t("projects.rateLimit")} value={`${compactNumber(project.rpm)} RPM / ${compactNumber(project.tpm)} TPM`} />
        <Policy label={t("projects.concurrency")} value={String(project.max_concurrency || t("common.unlimited"))} />
        <Policy label={t("projects.dailyBudget")} value={project.daily_budget_micros_usd ? money(project.daily_budget_micros_usd) : t("common.unlimited")} />
        <Policy label={t("projects.tokenGuardPolicy")} value={project.token_guard_policy_id || t("projects.notAttached")} />
      </div>
      {unblockResult && <div className="notice success"><strong>{unblockResult}</strong></div>}
      {unblock.isError && <ErrorState error={unblock.error} />}
      {remove.isError && <ErrorState error={remove.error} />}
      <header className="section-header">
        <div><p className="eyebrow">{t("projects.credentials")}</p><h3>{t("projects.gatewayKeys")}</h3></div>
        <button className="button secondary" onClick={() => setKeyDialog(true)}>{t("projects.createKey")}</button>
      </header>
      {keys.isPending && <Loading label={t("projects.loadingKeys")} />}
      {keys.isError && <ErrorState error={keys.error} />}
      {keys.data?.items.length === 0 && <p className="quiet-row">{t("projects.noKeys")}</p>}
      {keys.data?.items.map((key) => <KeyRow project={project} value={key} key={key.id} />)}
      {keyDialog && <CreateKey project={project} onClose={() => setKeyDialog(false)} />}
      {editing && <ProjectForm current={project} onClose={() => setEditing(false)} />}
    </section>
  );
}

function KeyRow({ project, value }: { project: Project; value: GatewayKey }) {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const mutation = useMutation({
    mutationFn: () => api.updateKey(
      project.id,
      value.id,
      { name: value.name, enabled: !value.enabled, ...(value.expires_at ? { expires_at: value.expires_at } : {}) },
      value.revision,
    ),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["project-keys", project.id] }),
  });
  const remove = useMutation({
    mutationFn: () => api.deleteKey(project.id, value.id, value.revision),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["project-keys", project.id] }),
  });
  return (
    <div className="key-row">
      <div>
        <span><StatusDot ok={value.enabled} /><strong>{value.name}</strong></span>
        <code>{value.id}</code>
      </div>
      <div className="key-dates">
        <small>{t("projects.created", { date: dateTime(value.created_at) })}</small>
        <small>{t("projects.lastUsed", { date: dateTime(value.last_used_at) })}</small>
      </div>
      <div className="row-actions">
        {value.enabled ? <ConfirmButton className="button ghost" label={t("common.disable")} title={t("projects.keyDisableTitle")} confirmLabel={t("projects.keyDisableConfirm", { name: value.name })} disabled={mutation.isPending} onConfirm={() => mutation.mutate()} /> : <button className="button ghost" disabled={mutation.isPending} onClick={() => mutation.mutate()}>{t("common.enable")}</button>}
        <ConfirmButton
          label={t("common.delete")}
          confirmLabel={t("projects.keyDeleteConfirm", { name: value.name })}
          disabled={remove.isPending}
          onConfirm={() => remove.mutate()}
        />
      </div>
    </div>
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
    queryKey: ["routes"],
    queryFn: api.routes,
  });
  const routeOptions = useMemo(() => {
    const options = new Map<string, { enabled: boolean; configured: boolean }>();
    current?.allowed_routes.forEach((value) => options.set(value, { enabled: false, configured: false }));
    availableRoutes.data?.items.forEach((route) => {
      const existing = options.get(route.public_model);
      if (!route.enabled && !existing) return;
      options.set(route.public_model, { enabled: Boolean(existing?.enabled || route.enabled), configured: true });
    });
    return Array.from(options, ([value, state]) => ({ value, ...state })).sort((a, b) => a.value.localeCompare(b.value));
  }, [availableRoutes.data?.items, current?.allowed_routes]);
  const {
    register,
    handleSubmit,
    formState: { errors },
  } = useForm<ProjectInput, unknown, ProjectValue>({
    resolver: zodResolver(schema),
    defaultValues: {
      name: current?.name ?? "",
      rpm: current?.rpm ?? 60,
      tpm: current?.tpm ?? 100_000,
      concurrency: current?.max_concurrency ?? 8,
      budget: (current?.daily_budget_micros_usd ?? 50_000_000) / 1_000_000,
      routes: current?.allowed_routes ?? [],
      cidrs: (current?.allowed_cidrs ?? []).join(", "),
      tokenGuardPolicyID: current?.token_guard_policy_id ?? "",
      redactionPolicyID: current?.redaction_policy_id ?? "",
      enabled: current?.enabled ?? true,
    },
  });
  const mutation = useMutation({
    mutationFn: (value: ProjectValue) => {
      const body = {
      name: value.name,
      enabled: value.enabled,
      allowed_routes: value.routes,
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
        : api.createProject(body);
    },
    onSuccess: () => {
      queryClient.invalidateQueries({ queryKey: ["projects"] });
      onClose();
    },
  });
  return (
    <Modal wide title={current ? t("projects.edit") : t("projects.createTitle")} onClose={onClose}>
      <form className="project-form" onSubmit={handleSubmit((value) => mutation.mutate(value))}>
        <section className="project-form-section" aria-labelledby="project-basics-title">
          <header><h3 id="project-basics-title">{t("projects.basicInfo")}</h3><p>{t("projects.basicInfoDescription")}</p></header>
          <div className="form-grid">
            <Field label={t("projects.name")} error={errors.name?.message}><input autoFocus {...register("name")} /></Field>
            <fieldset className="model-picker" aria-describedby="project-model-help">
              <legend>{t("projects.aliases")}</legend>
              <p id="project-model-help">{t("projects.aliasesHint")}</p>
              {availableRoutes.isPending ? <Loading label={t("projects.loadingModels")} /> : routeOptions.length ? <div className="model-option-grid">{routeOptions.map((route) => <label className="model-option" key={route.value}><input type="checkbox" value={route.value} {...register("routes")} /><span><strong>{route.value}</strong>{(!route.configured || !route.enabled) && <small>{t("projects.unavailableModel")}</small>}</span></label>)}</div> : <div className="notice warning"><strong>{t("projects.noConfiguredModels")}</strong><span>{t("projects.noConfiguredModelsDescription")}</span><Link className="notice-link" href="/admin/routes">{t("projects.openRoutes")}</Link></div>}
              {errors.routes?.message && <small className="field-error" role="alert">{errors.routes.message}</small>}
            </fieldset>
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
          <label className="project-enable-row"><span><strong>{t("projects.enable")}</strong><small>{t("projects.enableDescription")}</small></span><input type="checkbox" {...register("enabled")} /></label>
          <div className="form-grid">
            <Field label={t("projects.tokenGuardPolicy")}><select {...register("tokenGuardPolicyID")}><option value="">{t("projects.noBinding")}</option>{policies.data?.items.filter((policy) => policy.enabled).map((policy) => <option value={policy.id} key={policy.id}>{policy.name} · {policy.action === "temporary_block" ? t("policies.temporaryBlock") : policy.action === "alert" ? t("policies.alert") : t("policies.observe")}</option>)}</select></Field>
            <Field label={t("projects.redactionPolicy")}><select {...register("redactionPolicyID")}><option value="">{t("projects.noBinding")}</option>{redactionPolicies.data?.items.filter((policy) => policy.enabled).map((policy) => <option value={policy.id} key={policy.id}>{policy.name} · {policy.mode === "strict" ? t("redaction.strictBadge") : policy.mode === "bounded_stream" ? t("redaction.boundedBadge") : t("redaction.detectStreamBadge")}</option>)}</select></Field>
            <Field label={t("projects.allowedCIDR")} hint={t("projects.cidrHint")}><textarea rows={3} placeholder={t("projects.cidrPlaceholder")} {...register("cidrs")} /></Field>
          </div>
        </section>
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions project-form-actions">
          <button type="button" className="button ghost" onClick={onClose}>{t("common.cancel")}</button>
          <button className="button primary" disabled={mutation.isPending}>{current ? t("projects.save") : t("projects.createSubmit")}</button>
        </div>
      </form>
    </Modal>
  );
}

function CreateKey({ project, onClose }: { project: Project; onClose: () => void }) {
  const { t } = useTranslation();
  const [name, setName] = useState("");
  const [created, setCreated] = useState<CreatedGatewayKey | null>(null);
  const [acknowledged, setAcknowledged] = useState(false);
  const [copied, setCopied] = useState(false);
  const queryClient = useQueryClient();
  const mutation = useMutation({
    mutationFn: () => api.createKey(project.id, name),
    onSuccess: (result) => {
      setCreated(result.data);
      queryClient.invalidateQueries({ queryKey: ["project-keys", project.id] });
    },
  });
  const safelyClose = () => {
    if (created && !acknowledged) return;
    setCreated(null);
    onClose();
  };
  if (created) {
    return (
      <Modal title={t("projects.saveKey")} onClose={safelyClose}>
        <div className="one-time-secret">
          <div className="notice warning">
            <strong>{t("projects.oneTime")}</strong>
            <span>{t("projects.oneTimeDescription")}</span>
          </div>
          <code className="secret-value">{created.key}</code>
          <button
            className="button secondary wide"
            onClick={async () => {
              await navigator.clipboard.writeText(created.key);
              setCopied(true);
            }}
          >
            {copied ? t("common.copied") : t("projects.copyKey")}
          </button>
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
    <Modal title={t("projects.createKeyTitle")} onClose={onClose}>
      <form
        onSubmit={(event) => {
          event.preventDefault();
          if (name.trim()) mutation.mutate();
        }}
      >
        <Field label={t("projects.keyName")} hint={t("projects.keyNameHint")}>
          <input autoFocus value={name} onChange={(event) => setName(event.target.value)} />
        </Field>
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions">
          <button type="button" className="button ghost" onClick={onClose}>{t("common.cancel")}</button>
          <button className="button primary" disabled={!name.trim() || mutation.isPending}>{t("projects.generateKey")}</button>
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
