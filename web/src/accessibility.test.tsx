import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { Layout } from "./Layout";
import { Modal } from "./components";

describe("admin accessibility baseline", () => {
  it("provides landmarks, a skip link, and current navigation context", () => {
    window.history.replaceState({}, "", "/admin/deployments");
    const client = new QueryClient();
    render(
      <QueryClientProvider client={client}>
        <Layout username="admin"><h1>Deployments</h1></Layout>
      </QueryClientProvider>,
    );
    expect(screen.getByRole("link", { name: "跳到主要内容" })).toHaveAttribute("href", "#main-content");
    expect(screen.getByRole("navigation", { name: "主导航" })).toBeVisible();
    expect(screen.getByRole("main")).toHaveAttribute("id", "main-content");
    expect(screen.getByRole("link", { name: /Deployments/ })).toHaveAttribute("aria-current", "page");
  });

  it("labels dialogs, moves focus, and supports Escape", () => {
    const close = vi.fn();
    render(<Modal title="创建 Deployment" onClose={close}><button>保存</button></Modal>);
    const dialog = screen.getByRole("dialog", { name: "创建 Deployment" });
    expect(dialog).toHaveFocus();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(close).toHaveBeenCalledOnce();
  });
});
