import { useMutation, useQueryClient } from "@tanstack/react-query";
import { useState, type FormEvent } from "react";
import { useTranslation } from "react-i18next";
import { MIN_PASSWORD_CHARACTERS, passwordCharacterCount } from "../password";
import { api } from "../api";
import { ErrorState, Field } from "../components";

export function PasswordChangeForm() {
  const { t } = useTranslation();
  const [editing, setEditing] = useState(false);
  const [currentPassword, setCurrentPassword] = useState("");
  const [newPassword, setNewPassword] = useState("");
  const [confirmation, setConfirmation] = useState("");
  const [validationError, setValidationError] = useState("");
  const queryClient = useQueryClient();
  const mutation = useMutation({
    mutationFn: () => api.changePassword(currentPassword, newPassword),
    onSuccess: () => {
      setCurrentPassword("");
      setNewPassword("");
      setConfirmation("");
      setEditing(false);
      void queryClient.invalidateQueries({ queryKey: ["session"] });
    },
  });
  const submit = (event: FormEvent) => {
    event.preventDefault();
    setValidationError("");
    if (passwordCharacterCount(newPassword) < MIN_PASSWORD_CHARACTERS) {
      setValidationError(t("settings.passwordTooShort"));
      return;
    }
    if (newPassword !== confirmation) {
      setValidationError(t("settings.passwordMismatch"));
      return;
    }
    mutation.mutate();
  };
  return (
    <section className="panel settings-card password-settings">
      <header className="panel-header">
        <div><p className="eyebrow">{t("navigation.localAdmin")}</p><h3>{t("settings.changePassword")}</h3><p>{t("settings.otherSessionsEnd")}</p></div>
        {!editing && <button type="button" className="button" onClick={() => setEditing(true)}>{t("settings.changePassword")}</button>}
      </header>
      {mutation.isSuccess && <div className="notice success" role="status"><strong>{t("settings.passwordChanged")}</strong></div>}
      {editing && <form className="settings-form credential-form action-panel" aria-busy={mutation.isPending} onSubmit={submit} autoComplete="off">
        <Field label={t("settings.currentPassword")}><input type="password" autoComplete="current-password" required value={currentPassword} onChange={(event) => { mutation.reset(); setCurrentPassword(event.target.value); }} /></Field>
        <Field label={t("settings.newPassword")} hint={t("settings.newPasswordHint")}><input type="password" autoComplete="new-password" required value={newPassword} onChange={(event) => { mutation.reset(); setNewPassword(event.target.value); }} /></Field>
        <Field label={t("settings.confirmNewPassword")}><input type="password" autoComplete="new-password" required value={confirmation} onChange={(event) => { mutation.reset(); setConfirmation(event.target.value); }} /></Field>
        {validationError && <div className="notice warning" role="alert"><strong>{validationError}</strong></div>}
        {mutation.isError && <ErrorState error={mutation.error} />}
        <div className="form-actions"><button type="button" className="button ghost" disabled={mutation.isPending} onClick={() => { setEditing(false); setValidationError(""); }}>{t("common.cancel")}</button><button className="button primary" disabled={mutation.isPending || !currentPassword || !newPassword || !confirmation}>{mutation.isPending ? t("settings.saving") : t("settings.changePassword")}</button></div>
      </form>}
    </section>
  );
}
