import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { api } from "../api";
import { MasterKeyCustodyPage } from "./MasterKeyCustodyPage";

afterEach(() => vi.restoreAllMocks());

it("renders only the redacted read-only custody contract", async () => {
  vi.spyOn(api, "masterKeyCustody").mockResolvedValue({
    mode: "key_slots", descriptor_ready: true, custody_state: "degraded", production_admission: "external_evidence_required", rotation_incomplete: true, lifecycle_operation: "kek_rewrap",
    pending_slots: 0, retiring_slots: 1, recovery_verified_at: "2026-08-04T00:00:00Z",
    recovery_verification_expired: false, degraded_reasons: ["retiring_slots"],
    slots: [
      { purpose: "primary", state: "active", provider: "aws-kms", verified_at: "2026-08-04T00:00:00Z" },
      { purpose: "recovery", state: "active", provider: "aws-kms", verified_at: "2026-08-04T00:00:00Z" },
    ],
    lifecycle_runbook_url: "https://github.com/akz142857/Heimdall/blob/abc/docs/runbooks/m11-kms-key-lifecycle.md",
    recovery_runbook_url: "https://github.com/akz142857/Heimdall/blob/abc/docs/runbooks/m11-kms-disaster-recovery.md",
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><MasterKeyCustodyPage /></QueryClientProvider>);
  expect(await screen.findByRole("heading", { name: "Master Key 托管" })).toBeVisible();
  expect(await screen.findAllByText("aws-kms")).toHaveLength(2);
  expect(screen.getByText(/1 个 retiring/)).toBeVisible();
  expect(document.body.textContent).not.toContain("arn:aws:kms");
  const runbook = screen.getByRole("link", { name: "打开生命周期 Runbook" });
  expect(runbook).toHaveAttribute("href", expect.stringContaining("/blob/abc/"));
  runbook.focus();
  expect(runbook).toHaveFocus();
  const primaryDetails = screen.getByText("Primary").closest("details");
  expect(primaryDetails).toHaveAttribute("open");
  fireEvent.click(screen.getByText("Primary").closest("summary")!);
  expect(primaryDetails).not.toHaveAttribute("open");
  expect(screen.getByText("KEK rewrap")).toBeVisible();
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
});

it("renders file mode with empty slots without claiming KMS production admission", async () => {
  vi.spyOn(api, "masterKeyCustody").mockResolvedValue({
    mode: "file", descriptor_ready: true, custody_state: "healthy", production_admission: "not_applicable",
    rotation_incomplete: false, lifecycle_operation: "none", pending_slots: 0, retiring_slots: 0,
    recovery_verification_expired: false, degraded_reasons: [], slots: [],
    lifecycle_runbook_url: "https://example.test/lifecycle", recovery_runbook_url: "https://example.test/recovery",
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><MasterKeyCustodyPage /></QueryClientProvider>);
  expect(await screen.findByText("File 模式不适用外部 KMS 生产准入。")).toBeVisible();
  expect(screen.queryByText("满足生产条件")).not.toBeInTheDocument();
});

it("isolates API errors and invalid verification dates", async () => {
  const spy = vi.spyOn(api, "masterKeyCustody").mockRejectedValueOnce(new Error("offline"));
  let client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  const first = render(<QueryClientProvider client={client}><MasterKeyCustodyPage /></QueryClientProvider>);
  expect(await screen.findByRole("alert")).toBeVisible();
  first.unmount();

  spy.mockResolvedValueOnce({
    mode: "key_slots", descriptor_ready: false, custody_state: "degraded", production_admission: "external_evidence_required",
    rotation_incomplete: false, lifecycle_operation: "none", pending_slots: 0, retiring_slots: 0,
    recovery_verification_expired: true, degraded_reasons: ["recovery_verification_expired"],
    slots: [{ purpose: "recovery", state: "active", provider: "aws-kms", verified_at: "not-a-date" }],
    lifecycle_runbook_url: "https://example.test/lifecycle", recovery_runbook_url: "https://example.test/recovery",
  });
  client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><MasterKeyCustodyPage /></QueryClientProvider>);
  expect(await screen.findByText("未知")).toBeVisible();
  expect(screen.getByRole("status")).toHaveTextContent("Recovery 验证已超过 90 天");
});
