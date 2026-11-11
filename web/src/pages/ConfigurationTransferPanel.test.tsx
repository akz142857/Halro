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
  fireEvent.click(await screen.findByRole("button", { name: "导入配置" }));

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
  }, "reviewed-plan", { currentPassword: "local test password", totpCode: "" });
  expect(screen.getByRole("link", { name: "chat → Chat deployment" })).toHaveAttribute("href", "/admin/routes#route-rte_chat");
  expect(screen.getByRole("button", { name: "下载新密钥" })).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "关闭密钥下载" }));
  expect(screen.queryByRole("button", { name: "下载新密钥" })).not.toBeInTheDocument();
});
