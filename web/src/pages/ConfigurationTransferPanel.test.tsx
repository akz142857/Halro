import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { ApiError, api } from "../api";
import type { ConfigurationImportPreview } from "../types";
import { SettingsPage } from "./SettingsPage";

afterEach(() => {
  vi.restoreAllMocks();
  window.history.replaceState(null, "", "/");
});

it("shows how to replace an empty bundle without showing a zero-count preview", async () => {
  vi.spyOn(api, "previewConfigurationImport").mockResolvedValue({
    counts: { providers: 0, deployments: 0, routes: 0, redaction_policies: 0, token_guard_policies: 0, projects: 0, gateway_keys: 0 },
    providers: [], deployments_to_enable: [], routes_to_enable: [],
    issues: [{ resource: "bundle", code: "record_count", message: "configuration is empty" }],
  });
  window.history.replaceState(null, "", "/admin/settings/transfer");
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["session"], { username: "admin", role: "administrator" });
  render(<QueryClientProvider client={client}><SettingsPage /></QueryClientProvider>);
  const input = await screen.findByLabelText("配置文件");
  const chooseFile = vi.spyOn(input, "click");
  fireEvent.click(screen.getByRole("button", { name: "选择文件…" }));
  expect(chooseFile).toHaveBeenCalledOnce();
  const file = new File(['{"version":1}'], "empty.json");
  Object.defineProperty(file, "text", { value: () => Promise.resolve('{"version":1}') });
  fireEvent.change(input, { target: { files: [file] } });

  expect(await screen.findByRole("alert")).toHaveTextContent("请从有配置的源实例重新下载。");
  expect(screen.queryByRole("region", { name: "配置摘要" })).not.toBeInTheDocument();
  expect(screen.queryByRole("button", { name: "检查导入" })).not.toBeInTheDocument();
  expect(screen.getByRole("button", { name: "更换文件…" })).toHaveAccessibleDescription("empty.json");
});

it("requires a fresh preview after binding edits and offers newly issued keys once", async () => {
  const unbound: ConfigurationImportPreview = {
    counts: { providers: 1, deployments: 1, routes: 1, redaction_policies: 0, token_guard_policies: 0, projects: 1, gateway_keys: 1 },
    providers: [{
      id: "prv_source", name: "OpenAI test", type: "openai", access_surface: "openai-api",
      credential_scheme: "bearer.static", base_url: "https://api.openai.com", requires_egress_proxy: false,
    }],
    issues: [{ resource: "providers", id: "prv_source", code: "target_credential_required", message: "select a credential" }],
    deployments_to_enable: [{ id: "dep_chat", name: "Chat deployment" }],
    routes_to_enable: [{ id: "rte_chat", name: "chat", target: "Chat deployment" }],
    operator_declarations: [{ id: "dep_chat", name: "Chat deployment", model: "chat-test", capabilities: ["chat"] }],
  };
  const preview = vi.spyOn(api, "previewConfigurationImport")
    .mockResolvedValueOnce(unbound)
    .mockResolvedValueOnce({ ...unbound, issues: [], plan_id: "first-plan" })
    .mockResolvedValueOnce({ ...unbound, issues: [], plan_id: "reviewed-plan" });
  vi.spyOn(api, "credentials").mockResolvedValue({ items: [{
    id: "crd_target", name: "Test credential", type: "openai", access_surface: "openai-api",
    scheme: "bearer.static", secret_configured: true, bound_base_url: "https://target.example",
    key_version: 1, revision: 1, offering_id: "", region_id: "",
  }], next_cursor: "" });
  vi.spyOn(api, "providerEgressProxies").mockResolvedValue({ items: [], runtime_id: "" });
  vi.spyOn(api, "mfaStatus").mockResolvedValue({ enabled: false, required: false, policy: "optional", authenticators: [] });
  const apply = vi.spyOn(api, "applyConfigurationImport")
    .mockRejectedValueOnce(new ApiError(401, "step up", "recent_reauth_required"))
    .mockResolvedValueOnce({
      status: "imported", operation_id: "aud_fixture",
      counts: unbound.counts, deployments_to_enable: unbound.deployments_to_enable, routes_to_enable: unbound.routes_to_enable,
      gateway_keys: [{ id: "key_1", name: "Service", project_id: "prj_1", key: "gw_target-canary" }],
      activation_stale: false,
    });

  window.history.replaceState(null, "", "/admin/settings/transfer");
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["session"], { username: "admin", role: "administrator" });
  render(<QueryClientProvider client={client}><SettingsPage /></QueryClientProvider>);
  const source = '{"version":1}';
  const file = new File([source], "halro-configuration.json");
  Object.defineProperty(file, "text", { value: () => Promise.resolve(source) });
  fireEvent.change(await screen.findByLabelText("配置文件"), { target: { files: [file] } });
  await screen.findByRole("option", { name: /Test credential/ });
  expect(screen.queryByRole("button", { name: "导入配置" })).not.toBeInTheDocument();

  fireEvent.change(screen.getByLabelText("目标凭据"), { target: { value: "crd_target" } });
  fireEvent.click(screen.getByRole("button", { name: "检查导入" }));
  await screen.findByRole("button", { name: "导入配置" });
  fireEvent.change(screen.getByLabelText("目标端点"), { target: { value: "https://target.example" } });
  expect(screen.queryByRole("button", { name: "导入配置" })).not.toBeInTheDocument();
  fireEvent.click(screen.getByRole("button", { name: "检查导入" }));
  const importButton = await screen.findByRole("button", { name: "导入配置" });
  expect(importButton).toBeDisabled();
  fireEvent.click(screen.getByRole("checkbox", { name: "我确认上述模型能力声明。" }));
  fireEvent.click(importButton);

  const dialog = screen.getByRole("alertdialog", { name: "导入配置" });
  const confirm = within(dialog).getByRole("button", { name: "导入配置" });
  fireEvent.click(confirm);
  fireEvent.change(await within(dialog).findByLabelText(/^当前密码/), { target: { value: "local test password" } });
  await waitFor(() => expect(confirm).toBeEnabled());
  fireEvent.click(confirm);
  expect(await screen.findByText("配置已导入。")).toBeVisible();
  expect(preview).toHaveBeenCalledTimes(3);
  expect(apply).toHaveBeenLastCalledWith(source, {
    prv_source: { credential_id: "crd_target", base_url: "https://target.example", acknowledged_policy_revision: undefined },
  }, "reviewed-plan", { currentPassword: "local test password", totpCode: "" }, true);
  expect(screen.getByRole("link", { name: "chat → Chat deployment" })).toHaveAttribute("href", "/admin/routes#route-rte_chat");
  expect(screen.getByRole("button", { name: "下载新密钥" })).toBeVisible();
  const dismiss = screen.getByRole("button", { name: "关闭密钥下载" });
  expect(dismiss).toBeDisabled();
  const leave = new Event("beforeunload", { cancelable: true });
  window.dispatchEvent(leave);
  expect(leave.defaultPrevented).toBe(true);
  fireEvent.click(screen.getByRole("checkbox", { name: "我已安全保存新密钥。" }));
  fireEvent.click(dismiss);
  expect(screen.queryByRole("button", { name: "下载新密钥" })).not.toBeInTheDocument();
});

it("shows the current preview after a conflict and explains recovery after a local commit", async () => {
  const preview: ConfigurationImportPreview = {
    counts: { providers: 0, deployments: 0, routes: 0, redaction_policies: 0, token_guard_policies: 0, projects: 1, gateway_keys: 1 },
    providers: [], issues: [], deployments_to_enable: [], routes_to_enable: [], plan_id: "plan-first",
  };
  vi.spyOn(api, "previewConfigurationImport")
    .mockResolvedValueOnce(preview)
    .mockResolvedValueOnce({ ...preview, plan_id: "plan-second" });
  vi.spyOn(api, "applyConfigurationImport")
    .mockRejectedValueOnce(new ApiError(409, "review the current import preview", "configuration_preview_required", "", {
      preview: {
        ...preview, plan_id: undefined,
        issues: [{ resource: "target", code: "target_not_empty", message: "target already configured" }],
      },
    }))
    .mockResolvedValueOnce({
      status: "confirmation_unavailable", operation_id: "aud_local", counts: preview.counts,
      keys_to_reissue: [{ id: "key_fixture", project_id: "project_fixture", name: "Test key" }],
    });

  window.history.replaceState(null, "", "/admin/settings/transfer");
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  client.setQueryData(["session"], { username: "admin", role: "administrator" });
  render(<QueryClientProvider client={client}><SettingsPage /></QueryClientProvider>);
  const source = '{"version":1}';
  const file = new File([source], "halro-configuration.json");
  Object.defineProperty(file, "text", { value: () => Promise.resolve(source) });
  fireEvent.change(await screen.findByLabelText("配置文件"), { target: { files: [file] } });

  async function submitImport() {
    fireEvent.click(await screen.findByRole("button", { name: "导入配置" }));
    fireEvent.click(within(screen.getByRole("alertdialog", { name: "导入配置" })).getByRole("button", { name: "导入配置" }));
  }
  await submitImport();
  await screen.findByText("目标实例已有配置或删除记录，请使用新实例。", { exact: false });
  expect(screen.queryByRole("button", { name: "导入配置" })).not.toBeInTheDocument();

  fireEvent.click(screen.getByRole("button", { name: "检查导入" }));
  await submitImport();
  expect(await screen.findByText("配置已写入主节点，暂时无法确认副本。")).toBeVisible();
  expect(screen.getByText("aud_local")).toBeVisible();
  expect(screen.getByRole("link", { name: "Test key" })).toHaveAttribute("href", "/admin/projects?project_id=project_fixture");
  expect(screen.queryByRole("button", { name: "下载新密钥" })).not.toBeInTheDocument();
});
