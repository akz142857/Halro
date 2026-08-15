import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ConfirmButton } from "./components";
import type { Session } from "./types";

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
    const onConfirm = vi.fn().mockRejectedValue(new Error("re-authentication required"));
    renderConfirm(
      <ConfirmButton
        label="删除"
        confirmLabel="删除模型路由？"
        requireStepUp
        onConfirm={onConfirm}
      />,
    );
    fireEvent.click(screen.getByRole("button", { name: "删除" }));
    fireEvent.change(screen.getByLabelText(/^当前密码/), { target: { value: "a passphrase" } });
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
});
