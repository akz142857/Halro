import { zodResolver } from "@hookform/resolvers/zod";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useForm } from "react-hook-form";
import { z } from "zod";
import { api, ApiError } from "./api";
import { Field } from "./components";
import { LanguageSelect } from "./LanguageSelect";
import { localizedError } from "./i18n/errors";

type LoginValue = { username: string; password: string };

export function Login({ onSuccess }: { onSuccess: () => void }) {
  const { t } = useTranslation();
  const schema = useMemo(() => z.object({
    username: z.string().min(1, t("auth.usernameRequired")).max(128),
    password: z.string().min(1, t("auth.passwordRequired")).max(1024),
  }), [t]);
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
      setServerError(error instanceof ApiError ? localizedError(t, error) : t("auth.loginUnavailable"));
    }
  });
  return (
    <main className="login-page">
      <section className="login-story" aria-label={t("auth.productIntro")}>
        <div className="brand login-brand">
          <span className="brand-mark">H</span>
          <span><strong>HEIMDALL</strong><small>{t("navigation.productSubtitle")}</small></span>
        </div>
        <div>
          <p className="eyebrow">{t("auth.loginStoryEyebrow")}</p>
          <h1>{t("auth.loginStoryTitle")}</h1>
          <p>{t("auth.loginStoryDescription")}</p>
        </div>
        <ul className="trust-list">
          <li><span>01</span>{t("auth.trustVault")}</li>
          <li><span>02</span>{t("auth.trustKeys")}</li>
          <li><span>03</span>{t("auth.trustLedger")}</li>
        </ul>
      </section>
      <section className="login-panel">
        <LanguageSelect compact />
        <form onSubmit={submit} autoComplete="on">
          <p className="eyebrow">{t("auth.loginEyebrow")}</p>
          <h2>{t("auth.loginHeading")}</h2>
          <p>{t("auth.loginPrompt")}</p>
          {serverError && <div className="notice error" role="alert">{serverError}</div>}
          <Field label={t("auth.username")} error={errors.username?.message}>
            <input autoFocus autoComplete="username" {...register("username")} />
          </Field>
          <Field label={t("auth.password")} error={errors.password?.message}>
            <input type="password" autoComplete="current-password" {...register("password")} />
          </Field>
          <button className="button primary wide" disabled={isSubmitting}>
            {isSubmitting ? t("auth.loggingIn") : t("auth.login")}
          </button>
          <small className="login-note">{t("auth.loginSecurity")}</small>
        </form>
      </section>
    </main>
  );
}
