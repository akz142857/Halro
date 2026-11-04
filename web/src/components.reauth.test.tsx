import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "./api";
import { ConfirmButton, ReauthFields, type ReauthValues } from "./components";
import { stepUpRequired } from "./test/fixtures";
import type { MFAStatus, Session } from "./types";

function mfaStatus(enabled: boolean, policy: MFAStatus["policy"] = "optional"): MFAStatus {
  return {
    enabled,
    policy,
    required: policy !== "optional",
    authenticators: enabled ? [{ id: "synthetic-authenticator", name: "Test authenticator", type: "totp", created_at: "2026-11-04T00:00:00Z", revision: 1 }] : [],
    recovery_codes_remaining: enabled ? 8 : 0,
  };
}

function ControlledFields({ initial }: { initial: ReauthValues }) {
  const [values, setValues] = useState(initial);
  return <form>
    <ReauthFields values={values} onChange={setValues} description="确认本次操作" />
    <output data-testid="submitted-code">{values.totpCode}</output>
  </form>;
}

function createClient() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  client.setQueryData(["session"], {
    username: "test-admin", role: "administrator", locale: "zh-CN", appearance: "dark", csrf_token: "synthetic-csrf",
    absolute_expires_at: "2027-01-01T00:00:00Z", idle_expires_at: "2027-01-01T00:00:00Z",
  } satisfies Session);
  return client;
}

function renderFields(initial: ReauthValues = { currentPassword: "", totpCode: "" }, cachedStatus?: MFAStatus) {
  const client = createClient();
  if (cachedStatus) client.setQueryData(["mfa"], cachedStatus);
  const view = render(<QueryClientProvider client={client}><ControlledFields initial={initial} /></QueryClientProvider>);
  return { client, ...view };
}

function renderConfirmation(onConfirm: (values: ReauthValues) => Promise<void>) {
  const client = createClient();
  render(<QueryClientProvider client={client}><ConfirmButton label="删除" confirmLabel="此操作无法恢复" requireStepUp onConfirm={onConfirm} /></QueryClientProvider>);
  fireEvent.click(screen.getByRole("button", { name: "删除" }));
  const confirm = within(screen.getByRole("alertdialog")).getByRole("button", { name: "删除" });
  fireEvent.click(confirm);
  return { client, confirm };
}

describe("ReauthFields enrollment-aware verification", () => {
  afterEach(() => vi.restoreAllMocks());

  it("asks only for the current password after confirming MFA is not enabled", async () => {
    const status = vi.spyOn(api, "mfaStatus").mockResolvedValue(mfaStatus(false));
    const { client } = renderFields();
    await waitFor(() => expect(client.getQueryState(["mfa"])?.status).toBe("success"));
    expect(status).toHaveBeenCalledOnce();
    expect(screen.getByLabelText(/^当前密码/)).toBeRequired();
    expect(screen.getByLabelText(/^当前密码/)).toHaveAttribute("type", "password");
    expect(screen.getByText("确认本次操作")).toBeVisible();
    expect(screen.queryByLabelText(/^身份验证器验证码/)).not.toBeInTheDocument();
  });

  it("requires the authenticator code for an enrolled account and keeps it controlled", async () => {
    vi.spyOn(api, "mfaStatus").mockResolvedValue(mfaStatus(true));
    renderFields();
    const code = await screen.findByLabelText(/^身份验证器验证码/);
    expect(code).toBeRequired();
    expect(code).toHaveAttribute("autocomplete", "one-time-code");
    fireEvent.change(code, { target: { value: "123456" } });
    expect(code).toHaveValue("123456");
    expect(screen.getByTestId("submitted-code")).toHaveTextContent("123456");
  });

  it.each(["required", "administrators_required"] as const)("does not confuse %s policy with an enrolled authenticator", async (policy) => {
    vi.spyOn(api, "mfaStatus").mockResolvedValue(mfaStatus(false, policy));
    const { client } = renderFields();
    await waitFor(() => expect(client.getQueryState(["mfa"])?.status).toBe("success"));
    expect(screen.queryByLabelText(/^身份验证器验证码/)).not.toBeInTheDocument();
    expect(screen.getByLabelText(/^当前密码/)).toBeEnabled();
  });

  it("waits for confirmed enrollment before showing a code field", async () => {
    let resolveStatus: (status: MFAStatus) => void = () => {};
    vi.spyOn(api, "mfaStatus").mockImplementation(() => new Promise<MFAStatus>((resolve) => { resolveStatus = resolve; }));
    const { client } = renderFields();
    expect(client.getQueryState(["mfa"])?.status).toBe("pending");
    expect(screen.queryByLabelText(/^身份验证器验证码/)).not.toBeInTheDocument();
    fireEvent.change(screen.getByLabelText(/^当前密码/), { target: { value: "synthetic-password" } });
    resolveStatus(mfaStatus(true));
    expect(await screen.findByLabelText(/^身份验证器验证码/)).toBeRequired();
    expect(screen.getByLabelText(/^当前密码/)).toHaveValue("synthetic-password");
  });

  it("does not invent a code field when enrollment status cannot be read", async () => {
    vi.spyOn(api, "mfaStatus").mockRejectedValue(new Error("synthetic status unavailable"));
    const { client } = renderFields();
    await waitFor(() => expect(client.getQueryState(["mfa"])?.status).toBe("error"));
    expect(screen.queryByLabelText(/^身份验证器验证码/)).not.toBeInTheDocument();
    fireEvent.change(screen.getByLabelText(/^当前密码/), { target: { value: "synthetic-password" } });
    expect(screen.getByLabelText(/^当前密码/)).toHaveValue("synthetic-password");
  });

  it("removes the old code from form state when MFA becomes disabled without clearing the password", async () => {
    vi.spyOn(api, "mfaStatus").mockResolvedValue(mfaStatus(true));
    const { client } = renderFields({ currentPassword: "synthetic-password", totpCode: "" }, mfaStatus(true));
    const code = await screen.findByLabelText(/^身份验证器验证码/);
    fireEvent.change(code, { target: { value: "654321" } });
    expect(screen.getByTestId("submitted-code")).toHaveTextContent("654321");
    act(() => { client.setQueryData(["mfa"], mfaStatus(false)); });
    await waitFor(() => expect(screen.queryByLabelText(/^身份验证器验证码/)).not.toBeInTheDocument());
    await waitFor(() => expect(screen.getByTestId("submitted-code")).toBeEmptyDOMElement());
    expect(screen.getByLabelText(/^当前密码/)).toHaveValue("synthetic-password");
  });

  it("blocks a step-up confirmation while enrollment is pending and until an enrolled account supplies its code", async () => {
    let resolveStatus: (status: MFAStatus) => void = () => {};
    vi.spyOn(api, "mfaStatus").mockImplementation(() => new Promise<MFAStatus>((resolve) => { resolveStatus = resolve; }));
    const submit = vi.fn<(values: ReauthValues) => Promise<void>>().mockRejectedValueOnce(stepUpRequired()).mockResolvedValue(undefined);
    const { confirm } = renderConfirmation(submit);
    fireEvent.change(await screen.findByLabelText(/^当前密码/), { target: { value: "synthetic-password" } });
    expect(confirm).toBeDisabled();
    expect(screen.queryByLabelText(/^身份验证器验证码/)).not.toBeInTheDocument();
    resolveStatus(mfaStatus(true));
    const code = await screen.findByLabelText(/^身份验证器验证码/);
    expect(confirm).toBeDisabled();
    fireEvent.change(code, { target: { value: "123456" } });
    expect(confirm).toBeEnabled();
    fireEvent.click(confirm);
    await waitFor(() => expect(submit).toHaveBeenCalledTimes(2));
    expect(submit).toHaveBeenLastCalledWith({ currentPassword: "synthetic-password", totpCode: "123456" });
  });

  it("allows password-only step-up when enrollment is confirmed disabled", async () => {
    vi.spyOn(api, "mfaStatus").mockResolvedValue(mfaStatus(false));
    const submit = vi.fn<(values: ReauthValues) => Promise<void>>().mockRejectedValueOnce(stepUpRequired()).mockResolvedValue(undefined);
    const { confirm } = renderConfirmation(submit);
    fireEvent.change(await screen.findByLabelText(/^当前密码/), { target: { value: "synthetic-password" } });
    await waitFor(() => expect(confirm).toBeEnabled());
    expect(screen.queryByLabelText(/^身份验证器验证码/)).not.toBeInTheDocument();
    fireEvent.click(confirm);
    await waitFor(() => expect(submit).toHaveBeenCalledTimes(2));
    expect(submit).toHaveBeenLastCalledWith({ currentPassword: "synthetic-password", totpCode: "" });
  });

  it("keeps step-up blocked on status failure and offers a retry instead of an invented code field", async () => {
    const status = vi.spyOn(api, "mfaStatus").mockRejectedValueOnce(new Error("synthetic MFA status failure")).mockResolvedValue(mfaStatus(false));
    const submit = vi.fn<(values: ReauthValues) => Promise<void>>().mockRejectedValueOnce(stepUpRequired()).mockResolvedValue(undefined);
    const { client, confirm } = renderConfirmation(submit);
    fireEvent.change(await screen.findByLabelText(/^当前密码/), { target: { value: "synthetic-password" } });
    await waitFor(() => expect(client.getQueryState(["mfa"])?.status).toBe("error"));
    expect(confirm).toBeDisabled();
    expect(screen.queryByLabelText(/^身份验证器验证码/)).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "重试" }));
    await waitFor(() => expect(confirm).toBeEnabled());
    expect(status).toHaveBeenCalledTimes(2);
    expect(submit).toHaveBeenCalledOnce();
  });

  it("blocks confirmation after a background status failure even with cached enrollment and a typed code", async () => {
    const status = vi.spyOn(api, "mfaStatus").mockResolvedValue(mfaStatus(true));
    const submit = vi.fn<(values: ReauthValues) => Promise<void>>().mockRejectedValueOnce(stepUpRequired()).mockResolvedValue(undefined);
    const { client, confirm } = renderConfirmation(submit);
    fireEvent.change(await screen.findByLabelText(/^当前密码/), { target: { value: "synthetic-password" } });
    fireEvent.change(await screen.findByLabelText(/^身份验证器验证码/), { target: { value: "123456" } });
    expect(confirm).toBeEnabled();
    status.mockRejectedValue(new Error("synthetic background status failure"));
    await act(async () => { await client.refetchQueries({ queryKey: ["mfa"] }); });
    expect(client.getQueryData<MFAStatus>(["mfa"])?.enabled).toBe(true);
    await waitFor(() => expect(client.getQueryState(["mfa"])?.status).toBe("error"));
    expect(confirm).toBeDisabled();
    expect(screen.queryByLabelText(/^身份验证器验证码/)).not.toBeInTheDocument();
    fireEvent.click(confirm);
    fireEvent.submit(confirm.closest("form")!);
    expect(submit).toHaveBeenCalledOnce();
    expect(screen.getByRole("alertdialog")).toBeVisible();
  });

  it.each(["pending", "enrolled-without-code"] as const)("prevents form submission from bypassing %s re-authentication", async (state) => {
    const status = vi.spyOn(api, "mfaStatus");
    if (state === "pending") status.mockImplementation(() => new Promise<MFAStatus>(() => {}));
    else status.mockResolvedValue(mfaStatus(true));
    const submit = vi.fn<(values: ReauthValues) => Promise<void>>().mockRejectedValueOnce(stepUpRequired()).mockResolvedValue(undefined);
    const { confirm } = renderConfirmation(submit);
    fireEvent.change(await screen.findByLabelText(/^当前密码/), { target: { value: "synthetic-password" } });
    if (state === "enrolled-without-code") expect(await screen.findByLabelText(/^身份验证器验证码/)).toBeRequired();
    expect(confirm).toBeDisabled();
    fireEvent.submit(confirm.closest("form")!);
    expect(submit).toHaveBeenCalledOnce();
    expect(screen.getByRole("alertdialog")).toBeVisible();
  });
});
