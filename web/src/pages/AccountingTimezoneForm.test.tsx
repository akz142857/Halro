import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import type { AccountingSettings } from "../types";
import { AccountingTimezoneForm } from "./AccountingTimezoneForm";

function renderWithClient(node: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}>{node}</QueryClientProvider>);
}

function settings(overrides: Partial<AccountingSettings> = {}): AccountingSettings {
  return {
    timezone: "Asia/Shanghai",
    timezone_version: 3,
    current_period: {
      period_id: "2026-08-06",
      period_start: "2026-08-05T16:00:00Z",
      period_end: "2026-08-06T16:00:00Z",
    },
    config_file_timezone: "Asia/Shanghai",
    config_file_in_effect: true,
    tzdata: { source: "embedded", version: "2026a", fingerprint: "sha256:abc" },
    updated_at: "2026-08-01T00:00:00Z",
    revision: 4,
    ...overrides,
  };
}

describe("AccountingTimezoneForm", () => {
  afterEach(() => vi.restoreAllMocks());

  // A change here moves money between days, so the confirmation must state the
  // consequences rather than leaving an administrator to infer them.
  it("states when the change applies and what it does not touch before scheduling", async () => {
    const schedule = vi.spyOn(api, "scheduleAccountingTimezone").mockResolvedValue({
      data: settings({ pending_timezone: "Europe/Berlin", pending_effective_at: "2026-08-06T16:00:00Z", revision: 5 }),
      etag: '"5"',
    });
    // The dialog now waits for this before it will let anything be confirmed,
    // so a test that does not answer it is testing a dialog the operator would
    // never be allowed to submit either.
    vi.spyOn(api, "previewAccountingTimezone").mockResolvedValue({
      data: settings(), etag: '"4"',
    } as never);
    renderWithClient(<AccountingTimezoneForm settings={settings()} />);

    fireEvent.change(screen.getByLabelText(/目标时区/), { target: { value: "Europe/Berlin" } });
    fireEvent.click(screen.getByRole("button", { name: "安排变更" }));

    expect(await screen.findByText(/进行中的周期不受影响/)).toBeInTheDocument();
    expect(screen.getByText(/历史数据不会重算/)).toBeInTheDocument();
    expect(screen.getByText(/即当前周期结束时/)).toBeInTheDocument();
    // Nothing is sent until the consequences have been acknowledged.
    expect(schedule).not.toHaveBeenCalled();

    // Confirmation waits for the preview: the reset-window warning is the one
    // consequence nobody can derive from the zone names, and confirming before
    // it arrives is signing without having been shown it.
    const confirm = await screen.findByRole("button", { name: "确认并安排" });
    fireEvent.click(confirm);
    await waitFor(() => expect(schedule).toHaveBeenCalledWith("Europe/Berlin", 4));
  });

  // The switch restarts the daily budget hours later, which is the one
  // consequence an administrator cannot infer from the zone names.
  it("warns that the daily budget resets again shortly after the switch", async () => {
    vi.spyOn(api, "scheduleAccountingTimezone").mockResolvedValue({ data: settings(), etag: '"5"' });
    vi.spyOn(api, "previewAccountingTimezone").mockResolvedValue({
      data: settings({
        switch_preview: {
          timezone: "UTC",
          effective_at: "2026-08-06T16:00:00Z",
          first_period: { period_id: "2026-08-06", period_start: "2026-08-06T00:00:00Z", period_end: "2026-08-07T00:00:00Z" },
          next_reset_at: "2026-08-07T00:00:00Z",
          next_reset_in_hours: 8,
        },
      }),
      etag: '"4"',
    });
    renderWithClient(<AccountingTimezoneForm settings={settings()} />);

    fireEvent.change(screen.getByLabelText(/目标时区/), { target: { value: "UTC" } });
    fireEvent.click(screen.getByRole("button", { name: "安排变更" }));

    expect(await screen.findByText(/切换后约 8 小时，日预算会再次重置/)).toBeInTheDocument();
    expect(screen.getByText(/可用额度接近翻倍/)).toBeInTheDocument();
  });

  it("surfaces a scheduled change and lets it be cancelled", async () => {
    const cancel = vi.spyOn(api, "cancelAccountingTimezoneChange").mockResolvedValue({
      data: settings({ revision: 6 }), etag: '"6"',
    });
    renderWithClient(
      <AccountingTimezoneForm
        settings={settings({ pending_timezone: "Europe/Berlin", pending_effective_at: "2026-08-06T16:00:00Z" })}
      />,
    );

    expect(screen.getByText(/已安排切换到 Europe\/Berlin/)).toBeInTheDocument();
    fireEvent.click(screen.getByRole("button", { name: "撤销该变更" }));
    await waitFor(() => expect(cancel).toHaveBeenCalledWith(4));
  });

  // config.yaml seeds the zone once. Without this warning an operator can edit
  // the file, restart, and believe a boundary moved when it did not.
  it("warns when the configuration file no longer decides the zone", () => {
    renderWithClient(
      <AccountingTimezoneForm settings={settings({ config_file_timezone: "UTC", config_file_in_effect: false })} />,
    );
    expect(screen.getByText("配置文件已不再生效")).toBeInTheDocument();
    expect(screen.getByText(/usage.timezone 为 UTC/)).toBeInTheDocument();
  });

  it("cannot schedule the zone already in force", () => {
    renderWithClient(<AccountingTimezoneForm settings={settings()} />);
    expect(screen.getByRole("button", { name: "安排变更" })).toBeDisabled();
  });

  // Two nodes disagreeing on the rules place the same instant in different
  // periods, so the fingerprint has to be readable from the console.
  it("shows which time zone rules the node resolved against", () => {
    renderWithClient(<AccountingTimezoneForm settings={settings()} />);
    expect(screen.getByText("sha256:abc")).toBeInTheDocument();
    expect(screen.getByText(/embedded · 2026a/)).toBeInTheDocument();
  });
});
