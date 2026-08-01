import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Layout } from "../Layout";
import i18n, { applyPreference } from ".";
import { enUS } from "./locales/en-US";
import { zhCN } from "./locales/zh-CN";

describe("admin internationalization", () => {
  afterEach(async () => {
    vi.restoreAllMocks();
    await i18n.changeLanguage("zh-CN");
  });

  it("keeps Chinese and English resource keys in exact parity", () => {
    expect(flattenKeys(zhCN)).toEqual(flattenKeys(enUS));
  });

  it("renders exactly one navigation language and switches without a reload", async () => {
    window.history.replaceState({}, "", "/admin");
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <Layout username="admin"><h1>content</h1></Layout>
      </QueryClientProvider>,
    );
    expect(screen.getByRole("link", { name: "总览" })).toBeVisible();
    expect(screen.getByRole("button", { name: "退出登录" })).toBeVisible();
    expect(screen.queryByRole("link", { name: "Overview" })).not.toBeInTheDocument();

    await i18n.changeLanguage("en-US");
    await waitFor(() => expect(screen.getByRole("link", { name: "Overview" })).toBeVisible());
    expect(screen.getByRole("button", { name: "Sign out" })).toBeVisible();
    expect(screen.queryByRole("link", { name: "总览" })).not.toBeInTheDocument();
    expect(document.documentElement).toHaveAttribute("lang", "en-US");
  });

  it("does not persist locale selection in browser storage", async () => {
    const local = vi.spyOn(Storage.prototype, "setItem");
    const remove = vi.spyOn(Storage.prototype, "removeItem");

    await applyPreference("en-US", "zh-CN");

    expect(local).not.toHaveBeenCalled();
    expect(remove).not.toHaveBeenCalled();
  });
});

function flattenKeys(value: object, prefix = ""): string[] {
  return Object.entries(value).flatMap(([key, child]) => {
    const path = prefix ? `${prefix}.${key}` : key;
    return typeof child === "object" && child !== null ? flattenKeys(child, path) : [path];
  }).sort();
}
