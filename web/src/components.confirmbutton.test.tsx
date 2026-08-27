import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ApiError } from "./api";
import { ConfirmButton } from "./components";
import type { Session } from "./types";

// The refusal the server sends when the re-authentication window is not open.
// It is the same 401 a wrong password gets; only the console's own record of
// whether it has sent anything yet tells the two apart.
function asksForStepUp() {
  return new ApiError(401, "recent re-authentication required", "recent_reauth_required");
}

// ConfirmButton reads the session to decide whether a read-only operator should
// be offered the action at all, so it needs the same cache entry App holds open.
function renderConfirm(element: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  client.setQueryData(["session"], {
    username: "admin", role: "administrator", locale: "system", appearance: "dark",
    csrf_token: "csrf", absolute_expires_at: "2026-08-08T00:00:00Z",
    idle_expires_at: "2026-08-07T01:00:00Z",
  } satisfies Session);
  return render(<QueryClientProvider client={client}>{element}</QueryClientProvider>);
}

// The dialog used to close on click and only then call onConfirm, so a refused
// action left no dialog, no typed credentials, and — on a page that did not
// render the mutation error — nothing at all to say it had failed. The operator
// read that as a misclick and did it again.
describe("ConfirmButton", () => {
  it("keeps the dialog open and states the reason when the action is refused", async () => {
    const onConfirm = vi.fn()
      .mockRejectedValueOnce(asksForStepUp())
      .mockRejectedValue(new Error("re-authentication required"));
    renderConfirm(
      <ConfirmButton
        label="删除"
        confirmLabel="删除模型路由？"
        requireStepUp
        onConfirm={onConfirm}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "删除" }));
    // The dialog opens stating the consequence and nothing else: whether the
    // operator still has to prove who they are is the server's answer, so it is
    // asked for first and the fields appear only when the answer comes back.
    expect(screen.queryByLabelText(/^当前密码/)).not.toBeInTheDocument();
    fireEvent.click(screen.getAllByRole("button", { name: "删除" }).at(-1)!);
    fireEvent.change(await screen.findByLabelText(/^当前密码/), { target: { value: "a passphrase" } });
    fireEvent.change(screen.getByLabelText(/验证码/), { target: { value: "123456" } });
    fireEvent.click(screen.getAllByRole("button", { name: "删除" }).at(-1)!);

    // The wording is ErrorState's business; what matters here is that the
    // refusal is announced at all rather than vanishing with the dialog.
    await waitFor(() => expect(screen.getByRole("alert")).toBeInTheDocument());
    // Still open, so the operator can correct and retry rather than start over.
    expect(screen.getByText("删除模型路由？")).toBeInTheDocument();
    // The password survives; the code does not, because a TOTP step is spent
    // once and reusing it would be refused again for a different reason.
    expect(screen.getByLabelText(/^当前密码/)).toHaveValue("a passphrase");
    expect(screen.getByLabelText(/验证码/)).toHaveValue("");
    expect(screen.getByText(/验证码已被使用/)).toBeInTheDocument();
  });

  // Inside the re-authentication window the server accepts the first attempt, so
  // the operator confirms the consequence and is done. Asking anyway would be
  // the prompt this window exists to remove.
  it("never asks for credentials while the re-authentication window is open", async () => {
    const onConfirm = vi.fn().mockResolvedValue(undefined);
    renderConfirm(<ConfirmButton label="删除" confirmLabel="删除模型路由？" requireStepUp onConfirm={onConfirm} />);
    fireEvent.click(screen.getByRole("button", { name: "删除" }));
    fireEvent.click(screen.getAllByRole("button", { name: "删除" }).at(-1)!);

    await waitFor(() => expect(screen.queryByText("删除模型路由？")).not.toBeInTheDocument());
    expect(onConfirm).toHaveBeenCalledTimes(1);
    expect(onConfirm).toHaveBeenCalledWith({ currentPassword: "", totpCode: "" });
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  // The first refusal is the console's own question, asked with nothing. Showing
  // it as an error would report a failure the operator did not cause, beside the
  // empty fields it just opened.
  it("reports nothing when the server merely asks for the credentials", async () => {
    const onConfirm = vi.fn().mockRejectedValue(asksForStepUp());
    renderConfirm(<ConfirmButton label="删除" confirmLabel="删除模型路由？" requireStepUp onConfirm={onConfirm} />);
    fireEvent.click(screen.getByRole("button", { name: "删除" }));
    fireEvent.click(screen.getAllByRole("button", { name: "删除" }).at(-1)!);

    expect(await screen.findByLabelText(/^当前密码/)).toBeInTheDocument();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    // Nothing is deleted until the operator has proved who they are.
    expect(screen.getAllByRole("button", { name: "删除" }).at(-1)!).toBeDisabled();
  });

  it("closes only once the action has actually succeeded", async () => {
    let settle: (() => void) | undefined;
    const onConfirm = vi.fn().mockReturnValue(new Promise<void>((resolve) => { settle = resolve; }));
    renderConfirm(<ConfirmButton label="删除" confirmLabel="删除模型路由？" onConfirm={onConfirm} />);
    fireEvent.click(screen.getByRole("button", { name: "删除" }));
    fireEvent.click(screen.getAllByRole("button", { name: "删除" }).at(-1)!);

    // In flight: the dialog stays up and cannot be double-submitted.
    await waitFor(() => expect(screen.getByRole("button", { name: "处理中…" })).toBeDisabled());
    expect(screen.getByText("删除模型路由？")).toBeInTheDocument();

    settle!();
    await waitFor(() => expect(screen.queryByText("删除模型路由？")).not.toBeInTheDocument());
  });

  // Opening a confirmation must not be the action. The trigger used to carry no
  // type, which makes it a submit button, and every ConfirmButton inside a form
  // — the deployment editor is one — saved that form on the way to asking
  // whether the operator meant it. The dialog then confirmed something that had
  // already happened.
  it("opens the dialog without submitting the form it sits in", async () => {
    const onSubmit = vi.fn((event: React.FormEvent) => event.preventDefault());
    const onConfirm = vi.fn();
    renderConfirm(
      <form onSubmit={onSubmit}>
        <ConfirmButton label="实测校验" confirmLabel="这会调用上游。" onConfirm={onConfirm} />
      </form>,
    );
    fireEvent.click(screen.getByRole("button", { name: "实测校验" }));
    expect(await screen.findByRole("alertdialog")).toBeVisible();
    expect(onSubmit).not.toHaveBeenCalled();
    expect(onConfirm).not.toHaveBeenCalled();
  });
});
