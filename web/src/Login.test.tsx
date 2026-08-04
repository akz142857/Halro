import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { api, ApiError } from "./api";
import { Login } from "./Login";

describe("Login", () => {
  it("does not submit empty credentials and clears the form after success", async () => {
    const login = vi.spyOn(api, "login").mockResolvedValue({
      username: "admin",
      locale: "system",
      appearance: "dark",
      csrf_token: "csrf",
      absolute_expires_at: "2026-01-01T00:00:00Z",
      idle_expires_at: "2026-01-01T00:00:00Z",
    });
    const success = vi.fn();
    render(<Login onSuccess={success} />);

    fireEvent.click(screen.getByRole("button", { name: "安全登录" }));
    expect(await screen.findByText("请输入用户名")).toBeVisible();
    expect(login).not.toHaveBeenCalled();

    fireEvent.change(screen.getByRole("textbox", { name: /用户名/ }), { target: { value: "admin" } });
    fireEvent.change(screen.getByLabelText(/密码/), { target: { value: "strong password" } });
    fireEvent.click(screen.getByRole("button", { name: "安全登录" }));

    await waitFor(() => expect(success).toHaveBeenCalledOnce());
    expect(login).toHaveBeenCalledWith("admin", "strong password");
    expect(screen.getByLabelText(/密码/)).toHaveValue("");
  });

  it("clears credentials for the MFA step and only returns after challenge cancellation succeeds", async () => {
    vi.spyOn(api, "login").mockResolvedValue({ mfa_required: true, challenge_token: "challenge", expires_at: "2026-01-01T00:05:00Z" });
    const cancel = vi.spyOn(api, "cancelMFAChallenge").mockRejectedValueOnce(new ApiError(503, "unavailable")).mockResolvedValueOnce({ status: "cancelled" });
    render(<Login onSuccess={vi.fn()} />);
    fireEvent.change(screen.getByRole("textbox", { name: /用户名/ }), { target: { value: "admin" } });
    fireEvent.change(screen.getByLabelText(/密码/), { target: { value: "secret password" } });
    fireEvent.click(screen.getByRole("button", { name: "安全登录" }));
    expect(await screen.findByRole("heading", { name: "验证你的身份" })).toBeVisible();
    expect(screen.queryByDisplayValue("secret password")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "返回密码登录" }));
    expect(await screen.findByRole("alert")).toBeVisible();
    expect(screen.getByRole("heading", { name: "验证你的身份" })).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "返回密码登录" }));
    expect(await screen.findByRole("heading", { name: "进入控制台" })).toBeVisible();
    expect(cancel).toHaveBeenCalledTimes(2);
  });

  it("guards duplicate MFA submissions while verification is pending", async () => {
    vi.spyOn(api, "login").mockResolvedValue({ mfa_required: true, challenge_token: "challenge", expires_at: "2026-01-01T00:05:00Z" });
    let resolve!: (value: Awaited<ReturnType<typeof api.completeMFA>>) => void;
    const complete = vi.spyOn(api, "completeMFA").mockImplementation(() => new Promise((done) => { resolve = done; }));
    render(<Login onSuccess={vi.fn()} />);
    fireEvent.change(screen.getByRole("textbox", { name: /用户名/ }), { target: { value: "admin" } });
    fireEvent.change(screen.getByLabelText(/密码/), { target: { value: "password" } });
    fireEvent.click(screen.getByRole("button", { name: "安全登录" }));
    fireEvent.change(await screen.findByLabelText("身份验证器验证码"), { target: { value: "123456" } });
    const verify = screen.getByRole("button", { name: "验证" });
    fireEvent.click(verify); fireEvent.click(verify);
    expect(complete).toHaveBeenCalledOnce();
    expect(await screen.findByRole("button", { name: "正在验证…" })).toBeDisabled();
    resolve({ username: "admin", locale: "system", appearance: "dark", csrf_token: "csrf", absolute_expires_at: "x", idle_expires_at: "x" });
  });
});
