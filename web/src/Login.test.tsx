import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { api } from "./api";
import { Login } from "./Login";

describe("Login", () => {
  it("does not submit empty credentials and clears the form after success", async () => {
    const login = vi.spyOn(api, "login").mockResolvedValue({
      username: "admin",
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
});
