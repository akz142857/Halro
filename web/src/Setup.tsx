import { zodResolver } from "@hookform/resolvers/zod";
import { useState } from "react";
import { useForm } from "react-hook-form";
import { z } from "zod";
import { api, ApiError } from "./api";
import { Field } from "./components";
import type { Session } from "./types";

const schema = z.object({
  username: z.string().trim().min(1, "请输入用户名").max(128),
  password: z.string()
    .refine((value) => new TextEncoder().encode(value).length >= 12, "密码至少需要 12 字节")
    .refine((value) => new TextEncoder().encode(value).length <= 1024, "密码不能超过 1024 字节"),
  confirmation: z.string().min(1, "请再次输入密码")
    .refine((value) => new TextEncoder().encode(value).length <= 1024, "密码不能超过 1024 字节"),
  setupToken: z.string().max(128),
}).refine((value) => value.password === value.confirmation, {
  path: ["confirmation"],
  message: "两次输入的密码不一致",
});
type SetupValue = z.infer<typeof schema>;

export function Setup({
  tokenRequired,
  onSuccess,
  onAlreadyComplete,
}: {
  tokenRequired: boolean;
  onSuccess: (session: Session) => void;
  onAlreadyComplete: () => void;
}) {
  const [serverError, setServerError] = useState("");
  const {
    register,
    handleSubmit,
    formState: { errors, isSubmitting },
    setError,
    reset,
  } = useForm<SetupValue>({
    resolver: zodResolver(schema),
    defaultValues: { username: "admin", password: "", confirmation: "", setupToken: "" },
  });
  const submit = handleSubmit(async (value) => {
    if (tokenRequired && !value.setupToken.trim()) {
      setError("setupToken", { message: "请输入启动终端显示的一次性 Setup Token" });
      return;
    }
    setServerError("");
    try {
      const session = await api.setupAdmin(
        value.username.trim(),
        value.password,
        value.confirmation,
        value.setupToken.trim(),
      );
      reset();
      onSuccess(session);
    } catch (error) {
      if (error instanceof ApiError && error.status === 409) {
        reset();
        onAlreadyComplete();
        return;
      }
      setServerError(error instanceof ApiError ? error.message : "初始化服务暂时不可用");
    }
  });
  return (
    <main className="login-page setup-page">
      <section className="login-story" aria-label="初始化说明">
        <div className="brand login-brand">
          <span className="brand-mark">H</span>
          <span><strong>HEIMDALL</strong><small>FIRST-RUN SETUP</small></span>
        </div>
        <div>
          <p className="eyebrow">WELCOME TO THE GATE</p>
          <h1>创建这台网关的<br />第一位管理员。</h1>
          <p>密码只会以 Argon2id 哈希保存在本机。完成后，首次初始化入口将永久关闭。</p>
        </div>
        <ul className="trust-list">
          <li><span>01</span>本机加密存储与独占数据锁</li>
          <li><span>02</span>一次性并发安全初始化</li>
          <li><span>03</span>自动创建安全管理会话</li>
        </ul>
      </section>
      <section className="login-panel">
        <form onSubmit={submit} autoComplete="off">
          <p className="eyebrow">INSTANCE INITIALIZATION</p>
          <h2>设置管理员账户</h2>
          <p>这是唯一一次无需登录即可创建管理员的操作。</p>
          {serverError && <div className="notice error" role="alert">{serverError}</div>}
          <Field label="管理员用户名" error={errors.username?.message}>
            <input autoFocus autoComplete="username" {...register("username")} />
          </Field>
          <Field label="管理员密码" hint="至少 12 字节" error={errors.password?.message}>
            <input type="password" autoComplete="new-password" {...register("password")} />
          </Field>
          <Field label="确认密码" error={errors.confirmation?.message}>
            <input type="password" autoComplete="new-password" {...register("confirmation")} />
          </Field>
          {tokenRequired && (
            <Field
              label="Setup Token"
              hint="从启动 Heimdall 的终端复制"
              error={errors.setupToken?.message}
            >
              <input type="password" autoComplete="off" spellCheck={false} {...register("setupToken")} />
            </Field>
          )}
          <button className="button primary wide" disabled={isSubmitting}>
            {isSubmitting ? "正在安全初始化…" : "创建管理员并进入控制台"}
          </button>
          <small className="login-note">受 Origin、限速、Argon2id 与可信审计链保护</small>
        </form>
      </section>
    </main>
  );
}
