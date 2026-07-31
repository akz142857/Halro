import { zodResolver } from "@hookform/resolvers/zod";
import { useState } from "react";
import { useForm } from "react-hook-form";
import { z } from "zod";
import { api, ApiError } from "./api";
import { Field } from "./components";

const schema = z.object({
  username: z.string().min(1, "请输入用户名").max(128),
  password: z.string().min(1, "请输入密码").max(1024),
});
type LoginValue = z.infer<typeof schema>;

export function Login({ onSuccess }: { onSuccess: () => void }) {
  const [serverError, setServerError] = useState("");
  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
    reset,
  } = useForm<LoginValue>({ resolver: zodResolver(schema) });
  const submit = handleSubmit(async (value) => {
    setServerError("");
    try {
      await api.login(value.username, value.password);
      reset();
      onSuccess();
    } catch (error) {
      setServerError(error instanceof ApiError ? error.message : "登录服务暂时不可用");
    }
  });
  return (
    <main className="login-page">
      <section className="login-story" aria-label="产品介绍">
        <div className="brand login-brand">
          <span className="brand-mark">H</span>
          <span><strong>HEIMDALL</strong><small>LLM CONTROL PLANE</small></span>
        </div>
        <div>
          <p className="eyebrow">THE GATE STAYS LOCAL</p>
          <h1>把模型密钥留在<br />你的边界之内。</h1>
          <p>统一路由、预算、异常 Token 防护与脱敏。一个二进制，不依赖外部数据库。</p>
        </div>
        <ul className="trust-list">
          <li><span>01</span>AES-256-GCM Provider Vault</li>
          <li><span>02</span>Hash-only Internal Gateway Keys</li>
          <li><span>03</span>Durable Usage & Budget Ledger</li>
        </ul>
      </section>
      <section className="login-panel">
        <form onSubmit={submit} autoComplete="on">
          <p className="eyebrow">AUTHORIZED OPERATORS ONLY</p>
          <h2>进入控制台</h2>
          <p>使用本机管理员凭据。会话不会离开这台网关。</p>
          {serverError && <div className="notice error" role="alert">{serverError}</div>}
          <Field label="用户名" error={errors.username?.message}>
            <input autoFocus autoComplete="username" {...register("username")} />
          </Field>
          <Field label="密码" error={errors.password?.message}>
            <input type="password" autoComplete="current-password" {...register("password")} />
          </Field>
          <button className="button primary wide" disabled={isSubmitting}>
            {isSubmitting ? "正在验证…" : "安全登录"}
          </button>
          <small className="login-note">受 Origin、CSRF、限速与 Argon2id 保护</small>
        </form>
      </section>
    </main>
  );
}
