import { zodResolver } from "@hookform/resolvers/zod";
import { useMemo, useState } from "react";
import { useTranslation } from "react-i18next";
import { useForm } from "react-hook-form";
import { z } from "zod";
import { api, ApiError } from "./api";
import { Field } from "./components";
import type { Session } from "./types";
import { LanguageSelect } from "./LanguageSelect";
import { localizedError } from "./i18n/errors";
import { MAX_PASSWORD_BYTES, MIN_PASSWORD_CHARACTERS, passwordByteCount, passwordCharacterCount } from "./password";

type SetupValue = { username: string; password: string; confirmation: string; setupToken: string };

export function Setup({
  tokenRequired,
  onSuccess,
  onAlreadyComplete,
}: {
  tokenRequired: boolean;
  onSuccess: (session: Session) => void;
  onAlreadyComplete: () => void;
}) {
  const { t } = useTranslation();
  const schema = useMemo(() => z.object({
    username: z.string().trim().min(1, t("auth.usernameRequired")).max(128),
    password: z.string()
      .refine((value) => passwordCharacterCount(value) >= MIN_PASSWORD_CHARACTERS, t("auth.passwordMin"))
      .refine((value) => passwordByteCount(value) <= MAX_PASSWORD_BYTES, t("auth.passwordMax")),
    confirmation: z.string().min(1, t("auth.confirmRequired"))
      .refine((value) => passwordByteCount(value) <= MAX_PASSWORD_BYTES, t("auth.passwordMax")),
    setupToken: z.string().max(128),
  }).refine((value) => value.password === value.confirmation, {
    path: ["confirmation"], message: t("auth.passwordMismatch"),
  }), [t]);
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
      setError("setupToken", { message: t("auth.tokenRequired") });
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
      setServerError(error instanceof ApiError ? localizedError(t, error) : t("auth.serviceUnavailable"));
    }
  });
  return (
    <main className="login-page setup-page">
      <section className="login-story" aria-label={t("auth.setupIntro")}>
        <div className="brand login-brand">
          <span className="brand-mark">H</span>
          <span><strong>HEIMDALL</strong><small>{t("auth.firstRunSubtitle")}</small></span>
        </div>
        <div>
          <p className="eyebrow">{t("auth.setupEyebrow")}</p>
          <h1>{t("auth.setupTitle")}</h1>
          <p>{t("auth.setupDescription")}</p>
        </div>
        <ul className="trust-list">
          <li><span>01</span>{t("auth.setupTrust1")}</li>
          <li><span>02</span>{t("auth.setupTrust2")}</li>
          <li><span>03</span>{t("auth.setupTrust3")}</li>
        </ul>
      </section>
      <section className="login-panel">
        <LanguageSelect compact />
        <form onSubmit={submit} autoComplete="off">
          <p className="eyebrow">{t("auth.setupEyebrow")}</p>
          <h2>{t("auth.setupHeading")}</h2>
          <p>{t("auth.setupOnlyOnce")}</p>
          {serverError && <div className="notice error" role="alert">{serverError}</div>}
          <Field label={t("auth.username")} error={errors.username?.message}>
            <input autoFocus autoComplete="username" {...register("username")} />
          </Field>
          <Field label={t("auth.password")} hint={t("auth.passwordHint")} error={errors.password?.message}>
            <input type="password" autoComplete="new-password" {...register("password")} />
          </Field>
          <Field label={t("auth.confirmPassword")} error={errors.confirmation?.message}>
            <input type="password" autoComplete="new-password" {...register("confirmation")} />
          </Field>
          {tokenRequired && (
            <Field
              label={t("auth.setupToken")}
              hint={t("auth.setupTokenHint")}
              error={errors.setupToken?.message}
            >
              <input type="password" autoComplete="off" spellCheck={false} {...register("setupToken")} />
            </Field>
          )}
          <button className="button primary wide" disabled={isSubmitting}>
            {isSubmitting ? t("auth.settingUp") : t("auth.setupSubmit")}
          </button>
          <small className="login-note">{t("auth.setupSecurity")}</small>
        </form>
      </section>
    </main>
  );
}
