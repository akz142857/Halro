import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Layout } from "./Layout";
import { Modal, OverflowMenu } from "./components";
import { api } from "./api";
import { SettingsPage } from "./pages/SettingsPage";
import { emptyWritePath, timeContext } from "./test/fixtures";

describe("admin accessibility baseline", () => {
  afterEach(() => document.documentElement.removeAttribute("data-appearance"));
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
    expect(Array.from(screen.getByRole("navigation", { name: "主导航" }).querySelectorAll("a")).map((link) => link.textContent)).toEqual([
      "运行总览", "凭据与服务商", "模型部署", "模型路由", "安全策略", "项目与密钥", "开发者工作台", "用量与调用", "告警与审计", "根密钥状态", "设置与状态",
    ]);
    expect(screen.getByRole("main")).toHaveAttribute("id", "main-content");
    expect(screen.getByRole("link", { name: "模型部署" })).toHaveAttribute("aria-current", "page");
  });

  it("labels dialogs, moves focus, and supports Escape", () => {
    const close = vi.fn();
    render(<Modal title="创建 Deployment" onClose={close}><button>保存</button></Modal>);
    const dialog = screen.getByRole("dialog", { name: "创建 Deployment" });
    expect(dialog).toHaveFocus();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(close).toHaveBeenCalledOnce();
  });

  it("asks before Escape, the backdrop or Cancel discards a dirty form, and keeps the fields", () => {
    const close = vi.fn();
    render(
      <Modal title="创建 Deployment" dirty onClose={close}>
        <form><input aria-label="部署名称" defaultValue="填了一半" /><button type="button" data-modal-close>取消</button></form>
      </Modal>,
    );

    fireEvent.keyDown(document, { key: "Escape" });
    expect(close).not.toHaveBeenCalled();
    expect(screen.getByRole("alert")).toHaveTextContent("关闭会丢弃此处已填写的内容");
    fireEvent.click(screen.getByRole("button", { name: "继续编辑" }));
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(screen.getByLabelText("部署名称")).toHaveValue("填了一半");

    fireEvent.mouseDown(document.querySelector(".modal-backdrop")!);
    expect(close).not.toHaveBeenCalled();
    fireEvent.click(screen.getByRole("button", { name: "继续编辑" }));

    fireEvent.click(screen.getByRole("button", { name: "取消" }));
    expect(screen.getByRole("alert")).toBeVisible();
    fireEvent.click(screen.getByRole("button", { name: "放弃填写的内容" }));
    expect(close).toHaveBeenCalledOnce();
  });

  it("closes a modal with nothing filled in without asking", () => {
    const close = vi.fn();
    render(<Modal title="创建 Deployment" onClose={close}><button>保存</button></Modal>);
    fireEvent.keyDown(document, { key: "Escape" });
    expect(close).toHaveBeenCalledOnce();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("resets the personal appearance when the administrator logs out", async () => {
    document.documentElement.setAttribute("data-appearance", "light");
    vi.spyOn(api, "logout").mockResolvedValue(undefined);
    const client = new QueryClient();
    render(
      <QueryClientProvider client={client}>
        <Layout username="admin"><h1>Dashboard</h1></Layout>
      </QueryClientProvider>,
    );
    fireEvent.click(screen.getByRole("button", { name: /退出/ }));
    await waitFor(() => expect(document.documentElement).toHaveAttribute("data-appearance", "dark"));
  });

  it("closes overflow menus on outside interaction and Escape", () => {
    const { container } = render(<OverflowMenu label="更多操作"><button>删除</button></OverflowMenu>);
    const trigger = screen.getByLabelText("更多操作");
    const details = container.querySelector("details")!;

    fireEvent.click(trigger);
    expect(details).toHaveAttribute("open");
    fireEvent.pointerDown(document.body);
    expect(details).not.toHaveAttribute("open");

    fireEvent.click(trigger);
    fireEvent.keyDown(document, { key: "Escape" });
    expect(details).not.toHaveAttribute("open");
    expect(trigger).toHaveFocus();
  });

  it("keeps one main landmark and exposes settings sections and field descriptions", async () => {
    vi.spyOn(api, "systemStatus").mockResolvedValue({ build: { version: "dev", commit: "local", date: "" }, accounting_status: 0, draining: false, wal: {}, write_path: emptyWritePath(), audit: {}, alerts: {}, usage_watermark: {}, time_context: timeContext() });
    vi.spyOn(api, "settings").mockResolvedValue({ data: { health_probe_interval_seconds: 30, revision: 1 }, etag: '"1"' });
    vi.spyOn(api, "uiSettings").mockResolvedValue({ data: { default_locale: "zh-CN", revision: 1 }, etag: '"1"' });
    vi.spyOn(api, "preferences").mockResolvedValue({ data: { locale: "zh-CN", appearance: "dark", revision: 1 }, etag: '"1"' });
    vi.spyOn(api, "mfaStatus").mockResolvedValue({ enabled: false, policy: "optional", authenticators: [] });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><Layout username="admin"><SettingsPage /></Layout></QueryClientProvider>);
    await screen.findByRole("heading", { name: "通用" });
    expect(screen.getAllByRole("main")).toHaveLength(1);
    expect(await screen.findByRole("navigation", { name: "设置分区" })).toBeVisible();
    expect(screen.getByRole("link", { name: "通用" })).toHaveAttribute("aria-current", "page");
    fireEvent.click(screen.getByRole("link", { name: "登录与安全" }));
    expect(await screen.findByRole("link", { name: "登录与安全" })).toHaveAttribute("aria-current", "page");
    fireEvent.click(screen.getByRole("button", { name: /更改登录密码|Change sign-in password/ }));
    await screen.findByRole("heading", { name: /更改登录密码|Change sign-in password/ });
    const nextPassword = document.querySelector<HTMLInputElement>('input[autocomplete="new-password"]');
    expect(nextPassword).not.toBeNull();
    const description = nextPassword!.getAttribute("aria-describedby");
    expect(document.getElementById(description!)).toHaveTextContent(/8/);
  });
});
