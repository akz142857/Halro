import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeAll, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import type { AccountingSettings } from "../types";
import { AccountingTimezoneForm } from "./AccountingTimezoneForm";

// styles.css carries unscoped `dl`, `dt`, `dd` and `dl div` rules written for
// the two-column diagnostics cards. They apply to every definition list on the
// page, so a new one has to answer each property they set — the accounting
// panel shipped with values right-aligned in 10px monospace under stray
// separators because four of them went unanswered.
beforeAll(() => {
  const style = document.createElement("style");
  style.textContent = readFileSync(resolve(process.cwd(), "src/styles.css"), "utf8");
  document.head.appendChild(style);
});

function renderWithClient(node: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}>{node}</QueryClientProvider>);
}

function settings(overrides: Partial<AccountingSettings> = {}): AccountingSettings {
  return {
    timezone: "UTC",
    timezone_version: 1,
    current_period: {
      period_id: "2026-08-06",
      period_start: "2026-08-06T00:00:00Z",
      period_end: "2026-08-07T00:00:00Z",
    },
    config_file_timezone: "UTC",
    config_file_in_effect: true,
    tzdata: { source: "system", version: "2026b", fingerprint: "sha256:a19daa" },
    updated_at: "2026-08-01T00:00:00Z",
    revision: 1,
    ...overrides,
  };
}

describe("accounting panel layout", () => {
  afterEach(() => vi.restoreAllMocks());

  it("reads its own facts left to right, not right-aligned in monospace", () => {
    renderWithClient(<AccountingTimezoneForm settings={settings()} />);
    const value = document.querySelector(".settings-facts dd");
    expect(value).not.toBeNull();
    const computed = getComputedStyle(value!);
    expect(computed.textAlign).toBe("left");
    expect(computed.font).not.toMatch(/mono/);
  });

  it("does not draw a separator above the first fact", () => {
    renderWithClient(<AccountingTimezoneForm settings={settings()} />);
    const rows = document.querySelectorAll<HTMLElement>(".settings-facts > div");
    expect(rows.length).toBeGreaterThan(1);
    expect(getComputedStyle(rows[0]).borderTopWidth).toBe("0px");
  });

  // The modal pads a child it recognises and nothing else, so bare content runs
  // to the edges of the dialog.
  it("puts the confirmation body in the wrapper the modal actually pads", async () => {
    vi.spyOn(api, "previewAccountingTimezone").mockResolvedValue({ data: settings(), etag: '"1"' });
    renderWithClient(<AccountingTimezoneForm settings={settings()} />);

    fireEvent.change(screen.getByLabelText(/目标时区/), { target: { value: "Asia/Shanghai" } });
    fireEvent.click(screen.getByRole("button", { name: "安排变更" }));

    const lead = await screen.findByText(/请确认以下三点/);
    expect(lead.closest(".confirmation-dialog")).not.toBeNull();
    const actions = document.querySelector(".modal .form-actions");
    expect(actions?.closest(".confirmation-dialog")).not.toBeNull();
  });
});
