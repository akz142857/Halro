import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { api } from "../api";
import type { MasterKeyCustody } from "../types";
import { MasterKeyCustodyPage } from "./MasterKeyCustodyPage";

afterEach(() => vi.restoreAllMocks());

const keySlotsCustody = (overrides: Partial<MasterKeyCustody> = {}): MasterKeyCustody => ({
  mode: "key_slots", local_custody_ready: true, custody_state: "degraded", production_admission: "external_evidence_required", rotation_incomplete: true, lifecycle_operation: "kek_rewrap",
  pending_slots: 0, retiring_slots: 1, recovery_verification_status: "current", recovery_verified_at: "2026-08-04T00:00:00Z", degraded_reasons: ["retiring_slots"],
  slots: [
    { purpose: "primary", state: "active", provider: "aws-kms", verified_at: "2026-08-04T00:00:00Z" },
    { purpose: "recovery", state: "active", provider: "aws-kms", verified_at: "2026-08-04T00:00:00Z" },
  ],
  lifecycle_runbook_url: "/admin/api/v1/master-key/runbooks/lifecycle",
  recovery_runbook_url: "/admin/api/v1/master-key/runbooks/recovery",
  ...overrides,
});

const client = () => new QueryClient({ defaultOptions: { queries: { retry: false } } });

it("renders only the redacted read-only custody contract", async () => {
  vi.spyOn(api, "masterKeyCustody").mockResolvedValue(keySlotsCustody());
  render(<QueryClientProvider client={client()}><MasterKeyCustodyPage /></QueryClientProvider>);
  expect(await screen.findByRole("heading", { name: "Master Key 托管" })).toBeVisible();
  expect(await screen.findAllByText("aws-kms")).toHaveLength(2);
  expect(screen.getByText(/1 个 retiring/)).toBeVisible();
  expect(screen.getByText("最近 Recovery 验证").nextElementSibling).toHaveTextContent("2026");
  expect(screen.getByText(/证据包中核验/)).toBeVisible();
  expect(document.body.textContent).not.toContain("arn:aws:kms");
  const runbook = screen.getByRole("link", { name: "打开生命周期 Runbook（在新窗口打开）" });
  expect(runbook).toHaveAttribute("href", "/admin/api/v1/master-key/runbooks/lifecycle");
  expect(runbook).toHaveAttribute("target", "_blank");
  runbook.focus();
  expect(runbook).toHaveFocus();
  const primaryDetails = screen.getByText("Primary").closest("details");
  expect(primaryDetails).toHaveAttribute("open");
  fireEvent.click(screen.getByText("Primary").closest("summary")!);
  expect(primaryDetails).not.toHaveAttribute("open");
  expect(screen.getByText("KEK rewrap")).toBeVisible();
});

it("renders a truthful file-mode empty state without KMS runbooks", async () => {
  vi.spyOn(api, "masterKeyCustody").mockResolvedValue({
    mode: "file", local_custody_ready: true, custody_state: "healthy", production_admission: "not_applicable",
    rotation_incomplete: false, lifecycle_operation: "none", pending_slots: 0, retiring_slots: 0,
    recovery_verification_status: "not_applicable", degraded_reasons: [], slots: [],
  });
  window.innerWidth = 390;
  window.dispatchEvent(new Event("resize"));
  render(<QueryClientProvider client={client()}><MasterKeyCustodyPage /></QueryClientProvider>);
  expect(await screen.findByText("本地 File Key 已加载")).toBeVisible();
  expect(screen.getByText("File 模式不使用 Key Slot")).toBeVisible();
  expect(screen.getByText("不适用")).toBeVisible();
  expect(screen.getByText("File 模式不适用外部 KMS 生产准入。")).toBeVisible();
  expect(screen.queryByRole("link", { name: /Runbook/ })).not.toBeInTheDocument();
  expect(screen.queryByText("本地 descriptor 就绪")).not.toBeInTheDocument();
});

it("treats an empty key-slots response as a degraded anomaly", async () => {
  vi.spyOn(api, "masterKeyCustody").mockResolvedValue(keySlotsCustody({
    local_custody_ready: false, custody_state: "degraded", rotation_incomplete: false, lifecycle_operation: "none",
    pending_slots: 0, retiring_slots: 0, recovery_verification_status: "missing", recovery_verified_at: undefined,
    degraded_reasons: ["key_slots_missing", "recovery_verification_missing"], slots: [],
  }));
  render(<QueryClientProvider client={client()}><MasterKeyCustodyPage /></QueryClientProvider>);
  expect(await screen.findByRole("alert")).toHaveTextContent("未读取到 Key Slot");
  expect(screen.getByText("从未验证")).toBeVisible();
});

it("supports retry after an initial API error", async () => {
  const spy = vi.spyOn(api, "masterKeyCustody")
    .mockRejectedValueOnce(new Error("offline"))
    .mockResolvedValueOnce(keySlotsCustody());
  render(<QueryClientProvider client={client()}><MasterKeyCustodyPage /></QueryClientProvider>);
  expect(await screen.findByRole("alert")).toBeVisible();
  fireEvent.click(screen.getByRole("button", { name: "重试" }));
  expect(await screen.findByText("本地 descriptor 就绪")).toBeVisible();
  expect(spy).toHaveBeenCalledTimes(2);
});

it("keeps cached data visibly marked stale after a refetch error", async () => {
  const spy = vi.spyOn(api, "masterKeyCustody")
    .mockResolvedValueOnce(keySlotsCustody())
    .mockRejectedValueOnce(new Error("offline"));
  const queryClient = client();
  render(<QueryClientProvider client={queryClient}><MasterKeyCustodyPage /></QueryClientProvider>);
  expect(await screen.findByText("本地 descriptor 就绪")).toBeVisible();
  await queryClient.invalidateQueries({ queryKey: ["master-key-custody"] });
  expect(await screen.findByText("当前展示的是缓存状态。")).toBeVisible();
  expect(screen.getByText(/最后成功更新于/)).toBeVisible();
  expect(spy).toHaveBeenCalledTimes(2);
});

it("renders invalid future Recovery timestamps without crashing", async () => {
  vi.spyOn(api, "masterKeyCustody").mockResolvedValue(keySlotsCustody({
    local_custody_ready: false, recovery_verification_status: "invalid_future", recovery_verified_at: "not-a-date",
    degraded_reasons: ["recovery_verification_invalid_future"],
  }));
  render(<QueryClientProvider client={client()}><MasterKeyCustodyPage /></QueryClientProvider>);
  expect(await screen.findByText("时间无效（位于未来）")).toBeVisible();
  expect(screen.getByText("未知")).toBeVisible();
  expect(screen.getByRole("status")).toHaveTextContent("Recovery 验证时间位于未来");
});
