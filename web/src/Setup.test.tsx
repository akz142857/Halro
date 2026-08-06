import { cleanup, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api, ApiError } from "./api";
import { Setup } from "./Setup";

describe("first-run setup", () => {
  afterEach(() => {
    cleanup();
    vi.restoreAllMocks();
  });
  it("validates confirmation and creates the first administrator", async () => {
    const session = {
      username: "admin",
      locale: "system" as const,
      appearance: "dark" as const,
      role: "administrator" as const,
      csrf_token: "csrf",
      absolute_expires_at: "2026-08-02T00:00:00Z",
      idle_expires_at: "2026-08-01T01:00:00Z",
    };
    const setup = vi.spyOn(api, "setupAdmin").mockResolvedValue(session);
    const complete = vi.fn();
    render(<Setup tokenRequired={false} onSuccess={complete} onAlreadyComplete={vi.fn()} />);

    fireEvent.change(screen.getByLabelText(/^管理员密码/), { target: { value: "a strong local password" } });
    fireEvent.change(screen.getByLabelText(/^确认密码/), { target: { value: "different password" } });
    fireEvent.click(screen.getByRole("button", { name: /创建管理员/ }));
    expect(await screen.findByText("两次输入的密码不一致")).toBeInTheDocument();
    expect(setup).not.toHaveBeenCalled();

    fireEvent.change(screen.getByLabelText(/^确认密码/), { target: { value: "a strong local password" } });
    fireEvent.click(screen.getByRole("button", { name: /创建管理员/ }));
    await waitFor(() => expect(setup).toHaveBeenCalledWith(
      "admin", "a strong local password", "a strong local password", "",
    ));
    expect(complete).toHaveBeenCalledWith(session);
  });

  it("counts Unicode characters instead of UTF-8 bytes", async () => {
    const setup = vi.spyOn(api, "setupAdmin").mockResolvedValue({
      username: "admin",
      locale: "system",
      appearance: "dark",
      role: "administrator" as const,
      csrf_token: "csrf",
      absolute_expires_at: "2026-08-02T00:00:00Z",
      idle_expires_at: "2026-08-01T01:00:00Z",
    });
    render(<Setup tokenRequired={false} onSuccess={vi.fn()} onAlreadyComplete={vi.fn()} />);

    fireEvent.change(screen.getByLabelText(/^管理员密码/), { target: { value: "密密密密密密密" } });
    fireEvent.change(screen.getByLabelText(/^确认密码/), { target: { value: "密密密密密密密" } });
    fireEvent.click(screen.getByRole("button", { name: /创建管理员/ }));
    expect(await screen.findByText("密码至少需要 8 个字符")).toBeInTheDocument();
    expect(setup).not.toHaveBeenCalled();

    fireEvent.change(screen.getByLabelText(/^管理员密码/), { target: { value: "密密密密密密密密" } });
    fireEvent.change(screen.getByLabelText(/^确认密码/), { target: { value: "密密密密密密密密" } });
    fireEvent.click(screen.getByRole("button", { name: /创建管理员/ }));
    await waitFor(() => expect(setup).toHaveBeenCalledWith("admin", "密密密密密密密密", "密密密密密密密密", ""));
  });

  it("requires the transient token when the admin listener is public", async () => {
    const setup = vi.spyOn(api, "setupAdmin");
    render(<Setup tokenRequired onSuccess={vi.fn()} onAlreadyComplete={vi.fn()} />);
    fireEvent.change(screen.getByLabelText(/^管理员密码/), { target: { value: "a strong local password" } });
    fireEvent.change(screen.getByLabelText(/^确认密码/), { target: { value: "a strong local password" } });
    fireEvent.click(screen.getByRole("button", { name: /创建管理员/ }));
    expect(await screen.findByText(/请输入启动终端显示/)).toBeInTheDocument();
    expect(setup).not.toHaveBeenCalled();
  });

  it("moves to login when another request completed setup first", async () => {
    vi.spyOn(api, "setupAdmin").mockRejectedValue(new ApiError(409, "already complete"));
    const alreadyComplete = vi.fn();
    render(<Setup tokenRequired={false} onSuccess={vi.fn()} onAlreadyComplete={alreadyComplete} />);
    fireEvent.change(screen.getByLabelText(/^管理员密码/), { target: { value: "a strong local password" } });
    fireEvent.change(screen.getByLabelText(/^确认密码/), { target: { value: "a strong local password" } });
    fireEvent.click(screen.getByRole("button", { name: /创建管理员/ }));
    await waitFor(() => expect(alreadyComplete).toHaveBeenCalledOnce());
  });
});
