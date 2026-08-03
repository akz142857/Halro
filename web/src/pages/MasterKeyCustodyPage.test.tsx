import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
import { afterEach, expect, it, vi } from "vitest";
import { api } from "../api";
import { MasterKeyCustodyPage } from "./MasterKeyCustodyPage";

afterEach(() => vi.restoreAllMocks());

it("renders only the redacted read-only custody contract", async () => {
  vi.spyOn(api, "masterKeyCustody").mockResolvedValue({
    mode: "key_slots", production_ready: true, rotation_incomplete: true,
    pending_slots: 0, retiring_slots: 1, recovery_verified_at: "2026-08-04T00:00:00Z",
    slots: [
      { purpose: "primary", state: "active", provider: "aws-kms", verified_at: "2026-08-04T00:00:00Z" },
      { purpose: "recovery", state: "active", provider: "aws-kms", verified_at: "2026-08-04T00:00:00Z" },
    ],
    lifecycle_runbook: "docs/runbooks/m11-kms-key-lifecycle.md",
    recovery_runbook: "docs/runbooks/m11-kms-disaster-recovery.md",
  });
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  render(<QueryClientProvider client={client}><MasterKeyCustodyPage /></QueryClientProvider>);
  expect(await screen.findByRole("heading", { name: "Master Key 托管" })).toBeVisible();
  expect(await screen.findAllByText("aws-kms")).toHaveLength(2);
  expect(screen.getByText(/1 个 retiring/)).toBeVisible();
  expect(document.body.textContent).not.toContain("arn:aws:kms");
  expect(screen.queryByRole("button")).not.toBeInTheDocument();
});
