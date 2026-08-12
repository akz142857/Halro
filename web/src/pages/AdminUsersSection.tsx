import { useMutation, useQuery, useQueryClient } from "@tanstack/react-query";
import { useState } from "react";
import { useTranslation } from "react-i18next";
import { useNotify } from "../notifications";
import { api } from "../api";
import { ErrorState, Field, Loading, Modal, ReauthFields } from "../components";
import { MIN_PASSWORD_CHARACTERS, passwordCharacterCount } from "../password";
import { useIsReadOnly, useSession } from "../session";
import type { AdminRole } from "../types";

const emptyReauth = { currentPassword: "", totpCode: "" };

export function AdminUsersSection() {
  const { t } = useTranslation();
  const queryClient = useQueryClient();
  const session = useSession();
  const readOnly = useIsReadOnly();
  const [creating, setCreating] = useState(false);
  const [deleting, setDeleting] = useState("");
  const users = useQuery({ queryKey: ["admin-users"], queryFn: api.listAdminUsers });

  // The server refuses to remove the last administrator; showing that here
  // means an operator learns it before spending a password and a TOTP code on
  // a request that cannot succeed.
  const administrators = (users.data || []).filter((user) => user.role === "administrator").length;

  return <section className="panel settings-card" aria-labelledby="admin-users-title">
    <header className="panel-header">
      <div>
        <p className="eyebrow">{t("adminUsers.eyebrow")}</p>
        <h3 id="admin-users-title">{t("adminUsers.title")}</h3>
        <p>{t("adminUsers.description")}</p>
      </div>
      {/* Disabled with a stated reason, not hidden. Everywhere else in the
          console a read-only operator sees the control and is told why it is
          unavailable; hiding it here meant they could not tell that account
          management exists, let alone who to ask. */}
      <button
        type="button"
        className="button primary"
        disabled={readOnly}
        title={readOnly ? t("navigation.readOnlyAction") : undefined}
        onClick={() => setCreating(true)}
      >{t("adminUsers.create")}</button>
    </header>
    {users.isPending && <Loading />}
    {users.isError && <ErrorState error={users.error} />}
    {users.data && <ul className="admin-user-list">
      {users.data.map((user) => {
        const isSelf = user.username === session?.username;
        const lastAdministrator = user.role === "administrator" && administrators <= 1;
        return <li key={user.username} className="admin-user-row">
          <div className="admin-user-identity">
            <strong>{user.username}</strong>
            <span className="badge">{t(`adminUsers.roles.${user.role}`)}</span>
            {isSelf && <span className="badge">{t("adminUsers.you")}</span>}
          </div>
          <button
            type="button"
            className="button ghost"
            disabled={readOnly || isSelf || lastAdministrator}
            title={readOnly ? t("navigation.readOnlyAction") : isSelf ? t("adminUsers.cannotDeleteSelf") : lastAdministrator ? t("adminUsers.cannotDeleteLastAdministrator") : undefined}
            onClick={() => setDeleting(user.username)}
          >{t("common.delete")}</button>
        </li>;
      })}
    </ul>}
    {creating && <CreateAdminUserModal
      onClose={() => setCreating(false)}
      onCreated={() => { setCreating(false); void queryClient.invalidateQueries({ queryKey: ["admin-users"] }); }}
    />}
    {deleting && <DeleteAdminUserModal
      username={deleting}
      onClose={() => setDeleting("")}
      onDeleted={() => { setDeleting(""); void queryClient.invalidateQueries({ queryKey: ["admin-users"] }); }}
    />}
  </section>;
}

function CreateAdminUserModal({ onClose, onCreated }: { onClose: () => void; onCreated: () => void }) {
  const { t } = useTranslation();
  const [username, setUsername] = useState("");
  const [password, setPassword] = useState("");
  const [role, setRole] = useState<AdminRole>("read_only");
  const [reauth, setReauth] = useState(emptyReauth);
  const { notify } = useNotify();
  const create = useMutation({
    mutationFn: () => api.createAdminUser(username.trim(), password, role, reauth.currentPassword, reauth.totpCode),
    onSuccess: () => {
      onCreated();
      notify({ tone: "success", title: t("adminUsers.notifyCreated"), description: username.trim() });
    },
  });
  const tooShort = passwordCharacterCount(password) < MIN_PASSWORD_CHARACTERS;
  return <Modal title={t("adminUsers.create")} onClose={onClose} dirty={Boolean(username || password || reauth.currentPassword)}>
    <form className="settings-form credential-form" onSubmit={(event) => { event.preventDefault(); create.mutate(); }}>
      <Field label={t("adminUsers.username")}>
        <input autoFocus required value={username} onChange={(event) => setUsername(event.target.value)} />
      </Field>
      <Field label={t("adminUsers.password")} hint={t("auth.passwordHint", { count: MIN_PASSWORD_CHARACTERS })}>
        <input required type="password" autoComplete="new-password" value={password} onChange={(event) => setPassword(event.target.value)} />
      </Field>
      {/* Defaulting to read_only means a slip of attention grants the smaller
          capability, not the larger one. */}
      <Field label={t("adminUsers.role")} hint={t(`adminUsers.roleHints.${role}`)}>
        <select value={role} onChange={(event) => setRole(event.target.value as AdminRole)}>
          <option value="read_only">{t("adminUsers.roles.read_only")}</option>
          <option value="administrator">{t("adminUsers.roles.administrator")}</option>
        </select>
      </Field>
      <ReauthFields values={reauth} onChange={setReauth} description={t("adminUsers.stepUpCreate")} />
      {create.isError && <ErrorState error={create.error} />}
      <div className="form-actions">
        <button className="button primary" disabled={create.isPending || tooShort || !username.trim() || !reauth.currentPassword}>
          {create.isPending ? t("common.working") : t("adminUsers.createSubmit")}
        </button>
        <button type="button" className="button ghost" disabled={create.isPending} onClick={onClose}>{t("common.cancel")}</button>
      </div>
    </form>
  </Modal>;
}

function DeleteAdminUserModal({ username, onClose, onDeleted }: { username: string; onClose: () => void; onDeleted: () => void }) {
  const { t } = useTranslation();
  const [reauth, setReauth] = useState(emptyReauth);
  const { notify } = useNotify();
  const remove = useMutation({
    mutationFn: () => api.deleteAdminUser(username, reauth.currentPassword, reauth.totpCode),
    onSuccess: () => {
      onDeleted();
      notify({ tone: "success", title: t("adminUsers.notifyDeleted"), description: username });
    },
  });
  return <Modal dangerous title={t("adminUsers.deleteTitle", { username })} onClose={onClose} dirty={Boolean(reauth.currentPassword)}>
    <form className="settings-form credential-form danger-panel" onSubmit={(event) => { event.preventDefault(); remove.mutate(); }}>
      <p>{t("adminUsers.deleteWarning", { username })}</p>
      <ReauthFields values={reauth} onChange={setReauth} description={t("adminUsers.stepUpDelete")} />
      {remove.isError && <ErrorState error={remove.error} />}
      <div className="form-actions">
        <button className="button danger" disabled={remove.isPending || !reauth.currentPassword}>
          {remove.isPending ? t("common.working") : t("common.delete")}
        </button>
        <button type="button" className="button ghost" disabled={remove.isPending} onClick={onClose}>{t("common.cancel")}</button>
      </div>
    </form>
  </Modal>;
}
