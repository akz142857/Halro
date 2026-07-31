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

const projectSchema = z.object({
  name: z.string().trim().min(1, "请输入项目名称").max(128),
  routes: z.string().trim().min(1, "至少允许一个模型别名"),
  rpm: z.coerce.number().int().min(0),
  tpm: z.coerce.number().int().min(0),
  concurrency: z.coerce.number().int().min(0),
  budget: z.coerce.number().min(0),
  cidrs: z.string(),
  tokenGuardPolicyID: z.string(),
  redactionPolicyID: z.string(),
  enabled: z.boolean(),
});
type ProjectValue = z.infer<typeof projectSchema>;

export function ProjectsPage() {
  const [selected, setSelected] = useState<string>("");
  const [creating, setCreating] = useState(false);
  const projects = useQuery({ queryKey: ["projects"], queryFn: api.projects });
  const items = projects.data?.items ?? [];
  const selectedProject = items.find((item) => item.id === selected) ?? items[0];
  const selectedID = selectedProject?.id ?? "";
  return (
    <>
      <PageHeader
        eyebrow="ACCESS BOUNDARIES"
        title="Projects & Keys"
        description="把预算、模型权限和调用速率绑定到业务边界，而不是散落的 Provider Key。"
        action={<button className="button primary" onClick={() => setCreating(true)}>＋ 新建 Project</button>}
      />
      {projects.isPending && <Loading />}
      {projects.isError && <ErrorState error={projects.error} />}
      {projects.isSuccess && items.length === 0 && (
        <EmptyState
          title="还没有 Project"
          action={<button className="button primary" onClick={() => setCreating(true)}>创建第一个 Project</button>}
        >
          Project 是预算、限流、模型权限和内部 Key 的最小安全边界。
        </EmptyState>
      )}
      {items.length > 0 && (
        <div className="split-view">
          <section className="resource-list" aria-label="Project 列表">
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
                  {compactNumber(project.rpm)} RPM · {money(project.daily_budget_micros_usd)}/day
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
  const [keyDialog, setKeyDialog] = useState(false);
  const [editing, setEditing] = useState(false);
  const [unblockResult, setUnblockResult] = useState("");
  const keys = useQuery({
    queryKey: ["project-keys", project.id],
    queryFn: () => api.keys(project.id),
  });
  const unblock = useMutation({
    mutationFn: () => api.unblockProject(project.id),
    onSuccess: (value) => setUnblockResult(`已解除 ${value.subjects} 个异常状态`),
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
          <p className="eyebrow">PROJECT POLICY</p>
          <h2>{project.name}</h2>
          <code>{project.id}</code>
        </div>
        <div className="row-actions">
          <button className="button ghost" disabled={unblock.isPending} onClick={() => unblock.mutate()}>解除 Token Guard</button>
          <button className="button ghost" onClick={() => setEditing(true)}>编辑</button>
          <ConfirmButton
            label="删除"
            confirmLabel={`删除 Project “${project.name}”？其 Gateway Key 将立即失效。`}
            disabled={remove.isPending}
            onConfirm={() => remove.mutate()}
          />
          <span className={`badge ${project.enabled ? "good" : ""}`}>
            {project.enabled ? "ENABLED" : "DISABLED"}
          </span>
        </div>
      </header>
      <div className="policy-grid">
        <Policy label="Allowed models" value={project.allowed_routes.join(", ") || "None"} />
        <Policy label="Rate limit" value={`${compactNumber(project.rpm)} RPM / ${compactNumber(project.tpm)} TPM`} />
        <Policy label="Concurrency" value={String(project.max_concurrency || "Unlimited")} />
        <Policy label="Daily budget" value={project.daily_budget_micros_usd ? money(project.daily_budget_micros_usd) : "Unlimited"} />
        <Policy label="Token Guard" value={project.token_guard_policy_id || "Not attached"} />
      </div>
      {unblockResult && <div className="notice success"><strong>{unblockResult}</strong></div>}
      {unblock.isError && <ErrorState error={unblock.error} />}
      {remove.isError && <ErrorState error={remove.error} />}
      <header className="section-header">
        <div><p className="eyebrow">INTERNAL CREDENTIALS</p><h3>Gateway Keys</h3></div>
        <button className="button secondary" onClick={() => setKeyDialog(true)}>＋ 创建 Key</button>
      </header>
      {keys.isPending && <Loading label="正在读取 Key" />}
      {keys.isError && <ErrorState error={keys.error} />}
      {keys.data?.items.length === 0 && <p className="quiet-row">这个 Project 还没有可用 Key。</p>}
      {keys.data?.items.map((key) => <KeyRow project={project} value={key} key={key.id} />)}
      {keyDialog && <CreateKey project={project} onClose={() => setKeyDialog(false)} />}
      {editing && <ProjectForm current={project} onClose={() => setEditing(false)} />}
    </section>
  );
}

function KeyRow({ project, value }: { project: Project; value: GatewayKey }) {
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
        <small>创建 {dateTime(value.created_at)}</small>
        <small>最近使用 {dateTime(value.last_used_at)}</small>
      </div>
      <div className="row-actions">
        <button className="button ghost" disabled={mutation.isPending} onClick={() => mutation.mutate()}>
          {value.enabled ? "禁用" : "启用"}
        </button>
        <ConfirmButton
          label="删除"
          confirmLabel={`确认永久停用 Key “${value.name}”？此操作不能恢复。`}
          disabled={remove.isPending}
          onConfirm={() => remove.mutate()}
        />
      </div>
    </div>
  );
}

function ProjectForm({ current, onClose }: { current?: Project; onClose: () => void }) {
  const queryClient = useQueryClient();
  const policies = useQuery({
    queryKey: ["token-guard-policies"],
    queryFn: api.tokenGuardPolicies,
  });
  const redactionPolicies = useQuery({
    queryKey: ["redaction-policies"],
    queryFn: api.redactionPolicies,
  });
  const {
    register,
    handleSubmit,
    formState: { errors },
  } = useForm<ProjectValue>({
    resolver: zodResolver(projectSchema),
    defaultValues: {
      name: current?.name ?? "",
      rpm: current?.rpm ?? 60,
      tpm: current?.tpm ?? 100_000,
      concurrency: current?.max_concurrency ?? 8,
      budget: (current?.daily_budget_micros_usd ?? 50_000_000) / 1_000_000,
      routes: current?.allowed_routes.join(", ") ?? "chat",
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
      allowed_routes: splitValues(value.routes),
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
    <Modal title={current ? "编辑 Project" : "创建 Project"} onClose={onClose}>
      <form className="form-grid" onSubmit={handleSubmit((value) => mutation.mutate(value))}>
        <Field label="名称" error={errors.name?.message}><input autoFocus {...register("name")} /></Field>
        <Field label="允许的模型别名" hint="逗号或换行分隔" error={errors.routes?.message}>
          <input {...register("routes")} />
        </Field>
        <Field label="RPM"><input type="number" {...register("rpm")} /></Field>
        <Field label="TPM"><input type="number" {...register("tpm")} /></Field>
        <Field label="最大并发"><input type="number" {...register("concurrency")} /></Field>
        <Field label="每日预算（USD）"><input type="number" step="0.01" {...register("budget")} /></Field>
        <label className="check-row"><input type="checkbox" {...register("enabled")} />启用 Project</label>
        <Field label="Token Guard Policy">
          <select {...register("tokenGuardPolicyID")}>
            <option value="">不绑定</option>
            {policies.data?.items.filter((policy) => policy.enabled).map((policy) => (
              <option value={policy.id} key={policy.id}>{policy.name} · {policy.action}</option>
            ))}
          </select>
        </Field>
        <Field label="脱敏 Policy">
          <select {...register("redactionPolicyID")}>
            <option value="">不绑定</option>
            {redactionPolicies.data?.items.filter((policy) => policy.enabled).map((policy) => (
              <option value={policy.id} key={policy.id}>{policy.name} · {policy.mode}</option>
            ))}
          </select>
        </Field>
        <Field label="允许 CIDR" hint="留空表示不限制；逗号或换行分隔">
          <textarea rows={3} {...register("cidrs")} />
        </Field>
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions">
          <button type="button" className="button ghost" onClick={onClose}>取消</button>
          <button className="button primary" disabled={mutation.isPending}>{current ? "保存并热加载" : "创建 Project"}</button>
        </div>
      </form>
    </Modal>
  );
}

function CreateKey({ project, onClose }: { project: Project; onClose: () => void }) {
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
      <Modal title="保存这个 Gateway Key" onClose={safelyClose}>
        <div className="one-time-secret">
          <div className="notice warning">
            <strong>只显示这一次</strong>
            <span>离开后 Heimdall 无法恢复明文。不要将它保存到浏览器或聊天记录。</span>
          </div>
          <code className="secret-value">{created.key}</code>
          <button
            className="button secondary wide"
            onClick={async () => {
              await navigator.clipboard.writeText(created.key);
              setCopied(true);
            }}
          >
            {copied ? "已复制到剪贴板" : "复制 Key"}
          </button>
          <label className="check-row">
            <input type="checkbox" checked={acknowledged} onChange={(event) => setAcknowledged(event.target.checked)} />
            <span>我已将 Key 保存到安全的 Secret Manager</span>
          </label>
          <button className="button primary wide" disabled={!acknowledged} onClick={safelyClose}>
            完成并清除明文
          </button>
        </div>
      </Modal>
    );
  }
  return (
    <Modal title="创建 Gateway Key" onClose={onClose}>
      <form
        onSubmit={(event) => {
          event.preventDefault();
          if (name.trim()) mutation.mutate();
        }}
      >
        <Field label="Key 名称" hint="使用工作负载或服务名称，便于单独撤销">
          <input autoFocus value={name} onChange={(event) => setName(event.target.value)} />
        </Field>
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions">
          <button type="button" className="button ghost" onClick={onClose}>取消</button>
          <button className="button primary" disabled={!name.trim() || mutation.isPending}>生成 Key</button>
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
