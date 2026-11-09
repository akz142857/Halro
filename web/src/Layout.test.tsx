import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it } from "vitest";
import { Layout } from "./Layout";
import { setNavigationBlocked } from "./navigation";
import { adoptTimeContext, resetAccountingTimeZone } from "./timezone";
import { timeContext } from "./test/fixtures";

function renderLayout() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <Layout username="admin">
        <div>content</div>
      </Layout>
    </QueryClientProvider>,
  );
}

describe("Layout", () => {
  afterEach(() => { resetAccountingTimeZone(); setNavigationBlocked(false); });

  // Every figure in the console is measured against the accounting zone, so it
  // belongs somewhere always visible rather than on the one page that happens
  // to report a daily total.
  it("names the accounting time zone in the header", () => {
    adoptTimeContext(timeContext({ accounting_timezone: "Asia/Shanghai" }));
    renderLayout();
    expect(screen.getByText("Asia/Shanghai")).toBeInTheDocument();
    expect(document.querySelector(".topline")).toHaveTextContent(/时区[:：]\s*Asia\/Shanghai/);
    expect(screen.queryByText(/本地控制 \| 无云端依赖/)).not.toBeInTheDocument();
  });

  it("follows the zone the server reports rather than a fixed default", () => {
    adoptTimeContext(timeContext({ accounting_timezone: "America/New_York" }));
    renderLayout();
    expect(screen.getByText("America/New_York")).toBeInTheDocument();
    expect(screen.queryByText("UTC")).not.toBeInTheDocument();
  });

  it("exposes the compact navigation as a labelled disclosure", () => {
    renderLayout();
    const toggle = screen.getByRole("button", { name: /打开菜单/ });
    expect(toggle).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(toggle);
    expect(screen.getByRole("button", { name: /关闭菜单/ })).toHaveAttribute("aria-expanded", "true");
    expect(screen.getByRole("navigation", { name: "主导航" })).toHaveAttribute("id", "primary-navigation");
  });

  it("uses the console dialog rather than a native confirm for guarded navigation", () => {
    window.history.replaceState({}, "", "/admin/settings/security");
    renderLayout();
    setNavigationBlocked(true, "Save the one-time recovery codes before leaving.");
    fireEvent.click(screen.getByRole("link", { name: /运行总览/ }));
    expect(screen.getByRole("alertdialog", { name: "离开当前页面？" })).toHaveTextContent("Save the one-time recovery codes before leaving.");
    fireEvent.click(screen.getByRole("button", { name: "留在此页" }));
    expect(window.location.pathname).toBe("/admin/settings/security");
  });

  it("reveals account actions on demand and restores focus when Escape dismisses them", () => {
    renderLayout();
    const trigger = screen.getByRole("button", { name: /admin\s*本地管理员/ });
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    expect(screen.queryByRole("button", { name: "退出登录" })).not.toBeInTheDocument();
    fireEvent.click(trigger);
    expect(screen.getByRole("region", { name: "账户操作" })).toBeInTheDocument();
    screen.getByRole("button", { name: "退出登录" }).focus();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(trigger).toHaveFocus();
    expect(trigger).toHaveAttribute("aria-expanded", "false");
    fireEvent.click(trigger);
    fireEvent.pointerDown(screen.getByText("content"));
    expect(screen.queryByRole("region", { name: "账户操作" })).not.toBeInTheDocument();
    fireEvent.click(trigger);
    fireEvent.focusIn(screen.getByText("content"));
    expect(trigger).toHaveAttribute("aria-expanded", "false");
  });

  it("closes the account disclosure after navigating to settings", () => {
    window.history.replaceState({}, "", "/admin/usage");
    renderLayout();
    fireEvent.click(screen.getByRole("button", { name: /admin\s*本地管理员/ }));
    fireEvent.click(document.querySelector(".account-panel a")!);
    expect(window.location.pathname).toBe("/admin/settings/general");
    expect(screen.queryByRole("region", { name: "账户操作" })).not.toBeInTheDocument();
  });

  it("preserves unsaved-change protection when signing out from account actions", () => {
    renderLayout();
    setNavigationBlocked(true, "Unsaved changes");
    fireEvent.click(screen.getByRole("button", { name: /admin\s*本地管理员/ }));
    fireEvent.click(screen.getByRole("button", { name: "退出登录" }));
    expect(screen.getByRole("alertdialog", { name: "离开当前页面？" })).toHaveTextContent("Unsaved changes");
    fireEvent.click(screen.getByRole("button", { name: "留在此页" }));
    expect(screen.queryByRole("alertdialog")).not.toBeInTheDocument();
  });
});
