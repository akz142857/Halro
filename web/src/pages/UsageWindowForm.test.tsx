import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import type { UsageSettings } from "../types";
import { UsageWindowForm } from "./UsageWindowForm";

function renderWithClient(node: React.ReactElement) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}>{node}</QueryClientProvider>);
}

function settings(overrides: Partial<UsageSettings> = {}): UsageSettings {
  return {
    console_window_days: 90,
    presets: [30, 60, 90, 180],
    min_days: 7,
    config_file_days: 90,
    config_file_in_effect: true,
    updated_at: "2026-09-01T00:00:00Z",
    revision: 3,
    ...overrides,
  };
}

describe("UsageWindowForm", () => {
  afterEach(() => vi.restoreAllMocks());

  // Neither direction discards anything — every attempt stays in the history
  // index — so neither asks for a confirmation.
  it("saves a shorter window without a confirmation", async () => {
    const save = vi.spyOn(api, "updateUsageSettings").mockResolvedValue({
      data: settings({ console_window_days: 30, config_file_in_effect: false, revision: 4 }),
      etag: '"4"',
    });
    renderWithClient(<UsageWindowForm settings={settings()} />);

    fireEvent.change(screen.getByLabelText(/窗口长度/), { target: { value: "30" } });
    fireEvent.click(screen.getByRole("button", { name: "保存窗口" }));

    await waitFor(() => expect(save).toHaveBeenCalledWith(30, 3));
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
  });

  it("saves a longer window", async () => {
    const save = vi.spyOn(api, "updateUsageSettings").mockResolvedValue({
      data: settings({ console_window_days: 90, revision: 4 }), etag: '"4"',
    });
    renderWithClient(<UsageWindowForm settings={settings({ console_window_days: 30 })} />);

    fireEvent.change(screen.getByLabelText(/窗口长度/), { target: { value: "90" } });
    fireEvent.click(screen.getByRole("button", { name: "保存窗口" }));

    await waitFor(() => expect(save).toHaveBeenCalledWith(90, 3));
  });

  it("offers every preset and the value in force", () => {
    renderWithClient(<UsageWindowForm settings={settings({ console_window_days: 45 })} />);
    const options = screen.getAllByRole("option").map((option) => option.textContent);
    expect(options).toEqual(["30 天", "45 天", "60 天", "90 天", "180 天"]);
  });

  // A refused save has to be visible in the form.
  it("shows why a save was refused", async () => {
    vi.spyOn(api, "updateUsageSettings").mockRejectedValue(new Error("revision conflict"));
    renderWithClient(<UsageWindowForm settings={settings()} />);

    fireEvent.change(screen.getByLabelText(/窗口长度/), { target: { value: "30" } });
    fireEvent.click(screen.getByRole("button", { name: "保存窗口" }));

    expect(await screen.findByRole("alert")).toBeInTheDocument();
  });

  // config.yaml seeds this once. Without the notice, an operator edits the file,
  // restarts, and reads the unchanged screen as a bug.
  it("says so when the configuration file is no longer what decides", () => {
    renderWithClient(<UsageWindowForm settings={settings({ console_window_days: 30, config_file_in_effect: false })} />);
    expect(screen.getByText("配置文件已不再生效")).toBeInTheDocument();
    expect(screen.getByText(/config.yaml 写的是 90 天/)).toBeInTheDocument();
  });
});
