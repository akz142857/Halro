import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { ApiError, api } from "../api";
import { AdminUsersSection } from "./AdminUsersSection";
import { PoliciesPage } from "./PoliciesPage";
import { ProjectsPage } from "./ProjectsPage";
import { RedactionPoliciesSection } from "./RedactionPoliciesSection";
import { RoutesPage } from "./RoutesPage";
import { DeploymentsPage } from "./DeploymentsPage";
import type { AdminRole, Deployment, DeploymentPriceVersion, Project, Provider, RedactionPolicy, Route, Session, TokenGuardPolicy } from "../types";

// A read-only session that is offered a control the server will refuse learns
// nothing from the 403. §7.3 also requires a disabled control to say why it is
// disabled, so every assertion below checks the reason alongside the state.
const readOnlyReason = "只读账户无法执行此操作。";

function testProject(overrides: Partial<Project> = {}): Project {
  return {
    id: "project_a", name: "Alpha", enabled: true, revision: 1, allowed_models: ["chat"],
    rpm: 60, tpm: 1000, max_concurrency: 8, daily_budget_micros_usd: 0,
    max_input_tokens: 0, max_output_tokens: 0, max_request_bytes: 0, max_stream_duration: 0,
    allowed_cidrs: [], redaction_policy_id: "", token_guard_policy_id: "",
    created_at: "", updated_at: "", ...overrides,
  } as Project;
}

function testTokenGuardPolicy(overrides: Partial<TokenGuardPolicy> = {}): TokenGuardPolicy {
  return {
    id: "tgp_a", name: "Guard", enabled: true, action: "alert", revision: 1,
    request_tokens: 0, tokens_per_minute: 0, cost_micros_per_minute: 0, error_rate: 0,
    minimum_samples: 0, concurrency: 0, unique_ips_per_minute: 0, violations_before_block: 2,
    block_ttl_seconds: 300, cooldown_seconds: 60, ewma_enabled: false, ewma_alpha: 0.2,
    ewma_multiplier: 3, ewma_minimum_samples: 100, ewma_warmup_seconds: 3600,
    ewma_evaluation_window_seconds: 60, ewma_cooldown_seconds: 300, ewma_absolute_rpm: 60,
    ewma_absolute_tpm: 50000, ewma_absolute_tokens_per_request: 4000,
    ewma_absolute_cost_micros_per_minute: 1000000, bound_projects: 1,
    ...overrides,
  } as TokenGuardPolicy;
}

function testRedactionPolicy(overrides: Partial<RedactionPolicy> = {}): RedactionPolicy {
  return {
    id: "rdp_a", name: "Redact", enabled: true, mode: "strict", revision: 1, bound_projects: 1,
    rules: [{
      id: "rrl_a", name: "Email", kind: "builtin", builtin: "email", scopes: ["inbound"],
      action: "mask", enabled: true, priority: 10, computed_max_match_bytes: 128,
    }],
    ...overrides,
  } as RedactionPolicy;
}

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

  // The page reads api.projectsPage; mocking api.projects left the detail panel
  // unrendered, so the project-level write controls were never under test at all.
  it("offers no write action a read-only session cannot complete", async () => {
    vi.spyOn(api, "projectsPage").mockResolvedValue({ items: [testProject()], next_cursor: "" });
    vi.spyOn(api, "keysPage").mockResolvedValue({ items: [], next_cursor: "" } as never);
    renderAs("read_only", <ProjectsPage />);

    const create = await screen.findByRole("button", { name: "＋ 新建项目" });
    expect(create).toBeDisabled();

    // Disabling a project revokes every gateway key under it, which is exactly the
    // kind of control a read-only session must not be offered.
    const disable = await screen.findByRole("button", { name: "禁用" });
    expect(disable).toBeDisabled();
    expect(disable).toHaveAttribute("title", readOnlyReason);
    expect(screen.getByRole("button", { name: "编辑" })).toBeDisabled();
    expect(screen.getByRole("button", { name: "编辑" })).toHaveAttribute("title", readOnlyReason);
  });

  it("leaves the same action available to an administrator", async () => {
    vi.spyOn(api, "projectsPage").mockResolvedValue({ items: [testProject()], next_cursor: "" });
    vi.spyOn(api, "keysPage").mockResolvedValue({ items: [], next_cursor: "" } as never);
    renderAs("administrator", <ProjectsPage />);

    expect(await screen.findByRole("button", { name: "＋ 新建项目" })).toBeEnabled();
    expect(await screen.findByRole("button", { name: "禁用" })).toBeEnabled();
  });

  // The three status switches added with the policy console carried a `readOnly ||`
  // guard and no test: deleting all three of them at once left the suite green.
  it("offers no token guard status switch to a read-only session", async () => {
    vi.spyOn(api, "tokenGuardPoliciesPage").mockResolvedValue({
      items: [testTokenGuardPolicy(), testTokenGuardPolicy({ id: "tgp_b", name: "Off guard", enabled: false })],
      next_cursor: "",
    });
    vi.spyOn(api, "redactionPoliciesPage").mockResolvedValue({ items: [], next_cursor: "" });
    renderAs("read_only", <PoliciesPage />);

    await screen.findByText("Off guard");
    for (const name of ["禁用", "启用", "编辑"]) {
      for (const control of screen.getAllByRole("button", { name })) {
        expect(control).toBeDisabled();
        expect(control).toHaveAttribute("title", readOnlyReason);
      }
    }
    // Reading has to stay unimpeded.
    expect(screen.getAllByRole("button", { name: "模拟" })[0]).toBeEnabled();
  });

  it("offers no redaction policy status switch to a read-only session", async () => {
    renderAs("read_only", <RedactionPoliciesSection policies={[
      testRedactionPolicy(),
      testRedactionPolicy({ id: "rdp_b", name: "Off redaction", enabled: false }),
    ]} />);

    await screen.findByText("Off redaction");
    for (const name of ["禁用", "启用", "编辑"]) {
      for (const control of screen.getAllByRole("button", { name })) {
        expect(control).toBeDisabled();
        expect(control).toHaveAttribute("title", readOnlyReason);
      }
    }
    expect(screen.getAllByRole("button", { name: "测试" })[0]).toBeEnabled();
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

    fireEvent.change(screen.getByLabelText(/^当前密码/), { target: { value: "correct horse battery staple" } });
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
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [], next_cursor: "" });

    renderAs("read_only", <DeploymentsPage />);

    // The test is healthy and current, so nothing but the role should be
    // holding these back.
    for (const name of ["测试", "为 GPT 设置价格", "启用", "编辑", "＋ 新建模型部署"]) {
      expect(await screen.findByRole("button", { name })).toBeDisabled();
    }
    expect(screen.getByText("价格设置")).toBeVisible();
    expect(screen.getByText("未设置")).toBeVisible();
    fireEvent.click(screen.getByLabelText("更多操作"));
    expect(screen.getByRole("button", { name: "创建替代" })).toBeDisabled();

    // A disabled control has to say why it is disabled.
    expect(screen.getByRole("button", { name: "编辑" })).toHaveAttribute(
      "title", "只读账户无法执行此操作。");
  });

  it("opens price setup directly from a collapsed deployment without a price", async () => {
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
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [], next_cursor: "" });
    const createPrice = vi.spyOn(api, "createDeploymentPrice").mockResolvedValue({} as DeploymentPriceVersion);

    renderAs("administrator", <DeploymentsPage />);

    expect(await screen.findByText("未设置")).toBeVisible();
    expect(screen.queryByText("价格版本")).not.toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "为 GPT 设置价格" }));
    const dialog = await screen.findByRole("dialog", { name: "设置价格" });
    fireEvent.click(within(dialog).getByRole("radio", { name: /^免费/ }));
    fireEvent.click(within(dialog).getByRole("button", { name: "下一步：核对" }));
    fireEvent.change(within(dialog).getByLabelText(/^当前密码/), { target: { value: "correct horse battery staple" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "确认并创建价格版本" }));
    await waitFor(() => expect(createPrice).toHaveBeenCalledOnce());
    expect(createPrice).toHaveBeenCalledWith(deployment.id, expect.objectContaining({
      billing_mode: "free",
      effective_immediately: true,
    }), expect.any(String));
    expect(createPrice.mock.calls[0][1]).not.toHaveProperty("effective_from");
  });

  it("says nothing about a price that is set, and reads it out in the drawer", async () => {
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
    const activePrice = {
      id: "price_a", deployment_id: deployment.id, version: 1, revision: 1,
      billing_mode: "metered", currency: "USD", formula_version: "v1",
      input_micros_per_million: 1_000_000, output_micros_per_million: 2_000_000,
      fixed_request_micros_usd: 0, effective_from: "2026-08-10T00:00:00Z",
      source: { type: "manual", assurance: "asserted" }, status: "active",
    } as DeploymentPriceVersion;
    vi.spyOn(api, "providers").mockResolvedValue({ items: [provider], next_cursor: "" });
    vi.spyOn(api, "deployments").mockResolvedValue({ items: [deployment], next_cursor: "" });
    vi.spyOn(api, "routes").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [activePrice], next_cursor: "" });

    renderAs("administrator", <DeploymentsPage />);

    // A configured price needs nothing from the operator, so the card does not
    // carry it at all — the cell exists to say a price is missing.
    const open = await screen.findByRole("button", { name: "查看详情" });
    expect(screen.queryByText("价格设置")).not.toBeInTheDocument();
    expect(screen.queryByText("价格版本")).not.toBeInTheDocument();

    fireEvent.click(open);

    expect(await screen.findByText("价格版本")).toBeVisible();
    expect(screen.getByRole("dialog", { name: "GPT 详情" })).toBeVisible();
  });

  it("offers price setup inside the enable error when no effective price exists", async () => {
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
    vi.spyOn(api, "deploymentPrices").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "updateDeployment").mockRejectedValue(new ApiError(
      409,
      "deployment requires an effective versioned price before enable",
      "deployment_price_unavailable",
    ));

    renderAs("administrator", <DeploymentsPage />);
    fireEvent.click(await screen.findByRole("button", { name: "启用" }));

    const error = await screen.findByRole("alert");
    expect(within(error).getByText("该部署没有已生效的价格版本。设置价格后再启用；如模型免费，请明确选择“免费”。")).toBeVisible();
    expect(within(error).queryByText(/deployment requires an effective/)).not.toBeInTheDocument();
    fireEvent.click(within(error).getByRole("button", { name: "设置价格" }));
    expect(await screen.findByRole("dialog", { name: "设置价格" })).toBeVisible();
  });
});
