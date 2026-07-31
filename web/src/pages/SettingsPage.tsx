import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useEffect, useState, type FormEvent } from "react";
import { api } from "../api";
import { ErrorState, Field, Loading, PageHeader, StatusDot } from "../components";

const accountingLabels = ["Healthy", "Degraded", "Unavailable", "Recovery required"];

export function SettingsPage() {
  const status = useQuery({
    queryKey: ["system-status"],
    queryFn: api.systemStatus,
    refetchInterval: 15_000,
  });
  const settings = useQuery({ queryKey: ["settings"], queryFn: api.settings });
  return (
    <>
      <PageHeader
        eyebrow="LOCAL SYSTEM"
        title="Settings & Status"
        description="仅运行时安全参数允许热更新；监听地址、TLS、数据目录和安全边界始终由 YAML 管理并需重启生效。"
      />
      {(status.isPending || settings.isPending) && <Loading />}
      {(status.isError || settings.isError) && <ErrorState error={status.error || settings.error} />}
      {settings.data && <RuntimeSettingsForm settings={settings.data.data} />}
      <PasswordChangeForm />
      {status.data && (
        <div className="settings-grid">
          <section className="panel system-card">
            <p className="eyebrow">BUILD</p>
            <h2>Heimdall {status.data.build.version || "development"}</h2>
            <dl>
              <div><dt>Commit</dt><dd><code>{status.data.build.commit || "local"}</code></dd></div>
              <div><dt>Build date</dt><dd>{status.data.build.date || "—"}</dd></div>
              <div><dt>Draining</dt><dd>{status.data.draining ? "Yes" : "No"}</dd></div>
            </dl>
          </section>
          <section className="panel system-card">
            <p className="eyebrow">DURABILITY</p>
            <h2><StatusDot ok={status.data.accounting_status === 0} />{accountingLabels[status.data.accounting_status] || "Unknown"}</h2>
            <dl>
              {Object.entries(status.data.wal).slice(0, 5).map(([key, value]) => (
                <div key={key}><dt>{key.replaceAll("_", " ")}</dt><dd>{value}</dd></div>
              ))}
            </dl>
          </section>
          <section className="panel system-card">
            <p className="eyebrow">AUDIT HEAD</p>
            <h2>Chain checkpoint</h2>
            <dl>
              {Object.entries(status.data.audit).slice(0, 5).map(([key, value]) => (
                <div key={key}><dt>{key.replaceAll("_", " ")}</dt><dd>{String(value)}</dd></div>
              ))}
            </dl>
          </section>
        </div>
      )}
    </>
  );
}

export function PasswordChangeForm() {
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [validationError, setValidationError] = useState("");
  const queryClient = useQueryClient();
  const mutation = useMutation({
    mutationFn: () => api.changePassword(currentPassword, newPassword),
    onSettled: () => {
      setCurrentPassword("");
      setNewPassword("");
      setConfirmation("");
    },
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["session"] }),
  });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    setValidationError("");
    if (new TextEncoder().encode(newPassword).length < 12) {
      setValidationError("新密码至少需要 12 字节");
      return;
    }
    if (newPassword !== confirmation) {
      setValidationError("两次输入的新密码不一致");
      return;
    }
    mutation.mutate();
  };
  return (
    <section className="panel runtime-settings">
      <header className="panel-header">
        <div><p className="eyebrow">LOCAL ADMIN</p><h2>变更管理员密码</h2></div>
        <span className="badge">SESSION ROTATION</span>
      </header>
      <form onSubmit={submit} autoComplete="off">
        <Field label="当前密码"><input type="password" autoComplete="current-password" required value={currentPassword} onChange={(event) => setCurrentPassword(event.target.value)} /></Field>
        <Field label="新密码" hint="至少 12 字节；成功后全部旧 Session 立即失效"><input type="password" autoComplete="new-password" required value={newPassword} onChange={(event) => setNewPassword(event.target.value)} /></Field>
        <Field label="确认新密码"><input type="password" autoComplete="new-password" required value={confirmation} onChange={(event) => setConfirmation(event.target.value)} /></Field>
        {validationError && <div className="notice warning" role="alert"><strong>{validationError}</strong></div>}
        {mutation.isError && <ErrorState error={mutation.error} />}
        {mutation.isSuccess && <div className="notice success" role="status"><strong>密码已变更，Session 已安全轮换</strong></div>}
        <div className="form-actions"><button className="button primary" disabled={mutation.isPending || !currentPassword || !newPassword || !confirmation}>变更密码</button></div>
      </form>
    </section>
  );
}

function RuntimeSettingsForm({ settings }: { settings: { health_probe_interval_seconds: number; revision: number; updated_at?: string } }) {
  const [interval, setInterval] = useState(settings.health_probe_interval_seconds);
  const queryClient = useQueryClient();
  useEffect(() => setInterval(settings.health_probe_interval_seconds), [settings.health_probe_interval_seconds]);
  const mutation = useMutation({
    mutationFn: () => api.updateSettings({ health_probe_interval_seconds: interval }, settings.revision),
    onSuccess: () => queryClient.invalidateQueries({ queryKey: ["settings"] }),
  });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    mutation.mutate();
  };
  return (
    <section className="panel runtime-settings">
      <header className="panel-header">
        <div><p className="eyebrow">HOT SETTINGS</p><h2>运行时设置</h2></div>
        <span className="badge">REV {settings.revision}</span>
      </header>
      <form onSubmit={submit}>
        <Field label="Deployment 主动探测周期（秒）" hint="10–3600 秒；保存后无需重启，在下一轮计时生效。">
          <input type="number" min="10" max="3600" required value={interval} onChange={(event) => setInterval(Number(event.target.value))} />
        </Field>
        <div className="notice warning"><strong>启动级设置已锁定</strong><span>监听、TLS、存储路径、Provider 私网策略与 Metrics 认证只能修改 config.yaml。</span></div>
        {mutation.isError && <ErrorState error={mutation.error} />}
        {mutation.isSuccess && <div className="notice success" role="status"><strong>已保存并热加载</strong></div>}
        <div className="form-actions"><button className="button primary" disabled={mutation.isPending || interval === settings.health_probe_interval_seconds}>保存运行时设置</button></div>
      </form>
    </section>
  );
}
