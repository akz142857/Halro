import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import { AdminUsersSection } from "./AdminUsersSection";
import { ProjectsPage } from "./ProjectsPage";
import { RoutesPage } from "./RoutesPage";
import { DeploymentsPage } from "./DeploymentsPage";
import type { AdminRole, Deployment, Project, Provider, Route, Session } from "../types";

function session(role: AdminRole): Session {
  return {
    username: "admin",
    role,
    locale: "system",
    appearance: "dark",
    csrf_token: "csrf",
    absolute_expires_at: "2026-08-08T00:00:00Z",
    idle_expires_at: "2026-08-07T01:00:00Z",
  };
}

function renderAs(role: AdminRole, element: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  // Seeded rather than fetched: App holds this query open in the real console,
  // and the read-only decision has to come from the same cache entry.
  client.setQueryData(["session"], session(role));
  return render(<QueryClientProvider client={client}>{element}</QueryClientProvider>);
}

describe("read-only role", () => {
  afterEach(() => vi.restoreAllMocks());

  it("offers no write action a read-only session cannot complete", async () => {
    const project = {
      id: "project_a", name: "Alpha", enabled: true, revision: 1,
      created_at: "", updated_at: "",
    } as Project;
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    renderAs("read_only", <ProjectsPage />);

    const create = await screen.findByRole("button", { name: "＋ 新建项目" });
    expect(create).toBeDisabled();
  });

  it("leaves the same action available to an administrator", async () => {
    const project = {
      id: "project_a", name: "Alpha", enabled: true, revision: 1,
      created_at: "", updated_at: "",
    } as Project;
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    renderAs("administrator", <ProjectsPage />);

    expect(await screen.findByRole("button", { name: "＋ 新建项目" })).toBeEnabled();
  });

  // ConfirmButton backs every destructive action in the console, so honouring
  // the role there is what keeps a new destructive button from shipping
  // ungated by default.
  it("disables destructive row actions through the shared confirm button", async () => {
    const route = {
      id: "route_chat", public_model: "chat", deployment_id: "deployment_gpt",
      priority: 10, strategy: "ordered", enabled: true, revision: 1, created_at: "", updated_at: "",
    } as Route;
    vi.spyOn(api, "routes").mockResolvedValue({ items: [route], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({
      items: [{ id: "deployment_gpt", name: "GPT", provider_id: "provider_openai", provider_model: "gpt-5.1" } as Deployment],
      next_cursor: "",
    });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [{ id: "provider_openai", name: "OpenAI" } as Provider], next_cursor: "" });
    renderAs("read_only", <RoutesPage />);

    const row = (await screen.findByText("chat")).closest("tr");
    expect(row).not.toBeNull();
    expect(within(row!).getByRole("button", { name: "删除" })).toBeDisabled();
    expect(within(row!).getByRole("button", { name: "编辑" })).toBeDisabled();
    // Reading has to stay unimpeded — a read-only console that cannot test a
    // route is not read-only, it is broken.
    expect(within(row!).getByRole("button", { name: "测试" })).toBeEnabled();
  });

  // Disabled with a stated reason, not hidden. Every other page tells a
  // read-only operator why a control is unavailable; hiding these meant they
  // could not tell account management exists, and so could not tell whether to
  // ask someone for it or report it missing.
  it("shows account administration to a read-only session but refuses it", async () => {
    vi.spyOn(api, "listAdminUsers").mockResolvedValue([
      { username: "admin", role: "administrator" },
      { username: "viewer", role: "read_only" },
    ]);
    renderAs("read_only", <AdminUsersSection />);

    expect(await screen.findByText("viewer")).toBeInTheDocument();
    const create = screen.getByRole("button", { name: "新建账户" });
    expect(create).toBeDisabled();
    expect(create).toHaveAttribute("title", "只读账户无法执行此操作。");
    for (const remove of screen.getAllByRole("button", { name: "删除" })) {
      expect(remove).toBeDisabled();
    }
  });
});

describe("admin accounts", () => {
  afterEach(() => vi.restoreAllMocks());

  it("requires step-up credentials before creating an account", async () => {
    vi.spyOn(api, "listAdminUsers").mockResolvedValue([{ username: "admin", role: "administrator" }]);
    const create = vi.spyOn(api, "createAdminUser").mockResolvedValue({ username: "viewer", role: "read_only" });
    renderAs("administrator", <AdminUsersSection />);

    fireEvent.click(await screen.findByRole("button", { name: "新建账户" }));
    fireEvent.change(screen.getByLabelText("用户名"), { target: { value: "viewer" } });
    fireEvent.change(screen.getByLabelText(/初始密码/), { target: { value: "another correct horse" } });

    const submit = screen.getByRole("button", { name: "创建账户" });
    // Without the caller's own password this cannot be submitted at all, so a
    // step-up request is never sent half-filled.
    expect(submit).toBeDisabled();

    fireEvent.change(screen.getByLabelText("当前密码"), { target: { value: "correct horse battery staple" } });
    expect(submit).toBeEnabled();
    fireEvent.click(submit);

    await waitFor(() => expect(create).toHaveBeenCalledWith(
      "viewer", "another correct horse", "read_only", "correct horse battery staple", "",
    ));
  });

  // The server refuses both of these; saying so up front spares an operator a
  // password and a TOTP code spent on a request that cannot succeed.
  it("blocks deleting yourself and the last administrator before any request", async () => {
    vi.spyOn(api, "listAdminUsers").mockResolvedValue([
      { username: "admin", role: "administrator" },
      { username: "viewer", role: "read_only" },
    ]);
    renderAs("administrator", <AdminUsersSection />);

    const rows = await screen.findAllByRole("listitem");
    const self = rows.find((row) => within(row).queryByText("admin"));
    const other = rows.find((row) => within(row).queryByText("viewer"));
    expect(within(self!).getByRole("button", { name: "删除" })).toBeDisabled();
    expect(within(other!).getByRole("button", { name: "删除" })).toBeEnabled();
  });

  it("defaults a new account to the smaller capability", async () => {
    vi.spyOn(api, "listAdminUsers").mockResolvedValue([{ username: "admin", role: "administrator" }]);
    renderAs("administrator", <AdminUsersSection />);

    fireEvent.click(await screen.findByRole("button", { name: "新建账户" }));
    expect(screen.getByLabelText(/权限档位/)).toHaveValue("read_only");
  });
});

describe("destructive step-up", () => {
  afterEach(() => vi.restoreAllMocks());

  // The credentials have to reach the call, not just appear in the dialog: a
  // prompt that collects a password and then sends the old bodiless request
  // looks correct and protects nothing.
  it("sends the operator's own credentials with the delete", async () => {
    const route = {
      id: "route_chat", public_model: "chat", deployment_id: "deployment_gpt",
      priority: 10, strategy: "ordered", enabled: true, revision: 3, created_at: "", updated_at: "",
    } as Route;
    vi.spyOn(api, "routes").mockResolvedValue({ items: [route], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({
      items: [{ id: "deployment_gpt", name: "GPT", provider_id: "provider_openai", provider_model: "gpt-5.1" } as Deployment],
      next_cursor: "",
    });
    vi.spyOn(api, "providers").mockResolvedValue({ items: [{ id: "provider_openai", name: "OpenAI" } as Provider], next_cursor: "" });
    const remove = vi.spyOn(api, "deleteRoute").mockResolvedValue(undefined as never);
    renderAs("administrator", <RoutesPage />);

    const row = (await screen.findByText("chat")).closest("tr");
    fireEvent.click(within(row!).getByRole("button", { name: "删除" }));
    const dialog = await screen.findByRole("alertdialog");

    // Nothing is deleted until the operator has proved who they are.
    expect(within(dialog).getByRole("button", { name: "删除" })).toBeDisabled();

    fireEvent.change(within(dialog).getByLabelText(/当前密码/), { target: { value: "correct horse battery staple" } });
    fireEvent.change(within(dialog).getByLabelText(/身份验证器验证码/), { target: { value: "123456" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "删除" }));

    await waitFor(() => expect(remove).toHaveBeenCalledWith("route_chat", 3, {
      currentPassword: "correct horse battery staple",
      totpCode: "123456",
    }));
  });

  // Deployments had three write controls that were never role-gated — test,
  // enable and create-replacement — so a read-only session was shown buttons
  // that would 403 on click. §7.3 also requires a disabled control to say why.
  it("offers no live write control on the deployments page", async () => {
    const capabilities = {
      chat: true, streaming: true, embeddings: false, moderations: false, images: false,
      transcriptions: false, speech: false, files: false, batches: false, rerank: false,
      async_generate: false, tools: false, vision: false, json_mode: false,
      developer_role: false, reasoning: false, stream_usage: false,
      max_context_tokens: 0, max_output_tokens: 0,
    };
    const provider = {
      id: "provider_openai", name: "OpenAI", type: "openai", enabled: true, capabilities,
      capability_evidence: {}, revision: 1, created_at: "", updated_at: "",
    } as Provider;
    const deployment = {
      id: "deployment_a", name: "GPT", provider_id: provider.id, provider_model: "gpt-5",
      capabilities, capability_evidence: {}, enabled: false, revision: 2,
      last_test_status: "healthy", last_test_revision: 2,
      created_at: "", updated_at: "",
    } as Deployment;
    vi.spyOn(api, "providers").mockResolvedValue({ items: [provider], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [deployment], next_cursor: "" });
    vi.spyOn(api, "routes").mockResolvedValue({ items: [], next_cursor: "" });

    renderAs("read_only", <DeploymentsPage />);

    // The test is healthy and current, so nothing but the role should be
    // holding these back.
    for (const name of ["测试", "启用", "编辑", "＋ 新建模型部署"]) {
      expect(await screen.findByRole("button", { name })).toBeDisabled();
    }
    fireEvent.click(screen.getByLabelText("更多操作"));
    expect(screen.getByRole("button", { name: "创建替代" })).toBeDisabled();

    // A disabled control has to say why it is disabled.
    expect(screen.getByRole("button", { name: "编辑" })).toHaveAttribute(
      "title", "只读账户无法执行此操作。");
  });
});
