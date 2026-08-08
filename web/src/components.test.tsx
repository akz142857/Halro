import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ConfirmButton, InlineTestControl } from "./components";

describe("InlineTestControl", () => {
  it("keeps one stable live region through every test state", () => {
    const onTest = vi.fn();
    const { rerender } = render(<InlineTestControl state="idle" onTest={onTest} />);
    const button = screen.getByRole("button", { name: "测试" });
    const status = screen.getByRole("status");
    expect(status).toHaveTextContent("尚未测试");
    fireEvent.click(button);
    expect(onTest).toHaveBeenCalledOnce();

    rerender(<InlineTestControl state="running" onTest={onTest} />);
    expect(screen.getByRole("button", { name: "测试" })).toBe(button);
    expect(button).toBeDisabled();
    expect(button.closest(".inline-test-control")).toHaveAttribute("aria-busy", "true");
    expect(screen.getByRole("status")).toBe(status);
    expect(status).toHaveTextContent("测试中…");

    rerender(<InlineTestControl state="success" latency={42} onTest={onTest} />);
    expect(screen.getByRole("button", { name: "测试" })).toBe(button);
    expect(screen.getByRole("status")).toBe(status);
    expect(status).toHaveTextContent("通过 · 42ms");
  });
});

// A step-up dialog asks for a password. If that password field has no username
// beside it, and no form around it, the browser widens its search for the
// matching username field to the whole document and fills the first text input
// it finds — on a list page that is the filter box, which silently filtered the
// list down to "no matches" after every key deletion. Both halves are asserted
// structurally, because the behaviour itself belongs to the browser and cannot
// be reproduced in jsdom.
describe("step-up confirmation dialog", () => {
  const renderDialog = () => {
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <ConfirmButton label="删除" confirmLabel="确认删除?" requireStepUp onConfirm={() => {}} />
      </QueryClientProvider>,
    );
    fireEvent.click(screen.getByRole("button", { name: "删除" }));
  };

  it("keeps the browser's credential matching inside the dialog", () => {
    renderDialog();
    const password = document.querySelector('input[type="password"]');
    expect(password).not.toBeNull();

    // Scoped: the credential fields live in their own form, so the heuristic
    // cannot reach a filter box elsewhere on the page.
    const form = password!.closest("form");
    expect(form).not.toBeNull();

    // Targeted: and inside that form there is a username for it to pair with.
    const username = form!.querySelector('input[autocomplete="username"]');
    expect(username).not.toBeNull();
    expect(username).toHaveAttribute("aria-hidden", "true");
    expect(username).toHaveAttribute("tabindex", "-1");
  });
});
