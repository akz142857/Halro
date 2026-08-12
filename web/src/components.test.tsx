import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { ApiError } from "./api";
import { ConfirmButton, ErrorState, InlineTestControl } from "./components";

describe("ErrorState", () => {
  it("turns an ambiguous capability interface response into an actionable localized instruction", () => {
    render(<ErrorState error={new ApiError(400, "capability interface cannot be determined for this model; select one explicitly", "ambiguous_capability_binding")} />);
    expect(screen.getByText("该模型可通过多个能力接口调用。请选择实际接口后再确认模型。")).toBeVisible();
    expect(screen.queryByText(/capability interface cannot be determined/)).not.toBeInTheDocument();
  });

  it("explains a Gateway Key replay without exposing the server's English sentinel", () => {
    render(<ErrorState error={new ApiError(
      409,
      "this request already created gateway key key_1",
      "gateway_key_idempotency_replay",
      "",
      { id: "key_1", project_id: "project_1" },
    )} />);
    expect(screen.getByText(/密钥明文无法再次显示/)).toBeVisible();
    expect(screen.queryByText(/this request already created/)).not.toBeInTheDocument();
    expect(screen.getByRole("link", { name: "查看已创建记录" })).toHaveAttribute(
      "href",
      "/admin/projects?project_id=project_1#gateway-key-key_1",
    );
  });

  it.each([
    ["provider_idempotency_replay", "provider_1", "/admin/providers#provider-provider_1"],
    ["deployment_idempotency_replay", "deployment_1", "/admin/deployments#deployment-deployment_1"],
    ["route_idempotency_replay", "route_1", "/admin/routes#route-route_1"],
    ["project_idempotency_replay", "project_1", "/admin/projects?project_id=project_1"],
  ])("links %s to its created record", (code, id, href) => {
    render(<ErrorState error={new ApiError(409, "already created", code, "", { id })} />);
    expect(screen.getByRole("link", { name: "查看已创建记录" })).toHaveAttribute("href", href);
    expect(screen.queryByText("数据已被其他操作修改，请刷新后重试。")).not.toBeInTheDocument();
  });
});

describe("InlineTestControl", () => {
  it("keeps one stable live region through every test state", () => {
    const onTest = vi.fn();
    const { rerender } = render(<InlineTestControl state="idle" onTest={onTest} />);
    const button = screen.getByRole("button", { name: "测试" });
    expect(button).toHaveClass("secondary");
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
