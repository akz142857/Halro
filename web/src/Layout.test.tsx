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
    expect(screen.getByText(/本地控制 \| 无云端依赖/)).toBeInTheDocument();
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
});
