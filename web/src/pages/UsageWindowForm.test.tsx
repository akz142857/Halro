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
    max_days: 90,
    config_file_days: 90,
    config_file_in_effect: true,
    updated_at: "2026-09-01T00:00:00Z",
    revision: 3,
    ...overrides,
  };
}

describe("UsageWindowForm", () => {
  afterEach(() => vi.restoreAllMocks());

  // Shortening the window trims the attempt log on the next export tick. The
  // confirmation is the whole point of the form: the number alone gives an
  // operator no reason to expect anything to disappear.
  it("confirms before shortening, and says what is lost and what is not", async () => {
    const save = vi.spyOn(api, "updateUsageSettings").mockResolvedValue({
      data: settings({ console_window_days: 30, config_file_in_effect: false, revision: 4 }),
      etag: '"4"',
    });
    renderWithClient(<UsageWindowForm settings={settings()} />);

    fireEvent.change(screen.getByLabelText(/窗口长度/), { target: { value: "30" } });
    fireEvent.click(screen.getByRole("button", { name: "保存窗口" }));

    expect(await screen.findByText("缩短窗口会丢弃调用历史")).toBeInTheDocument();
    expect(screen.getByText(/Parquet 归档仍保留 90 天/)).toBeInTheDocument();
    expect(save).not.toHaveBeenCalled();

    fireEvent.click(screen.getByRole("button", { name: "缩短窗口" }));
    await waitFor(() => expect(save).toHaveBeenCalledWith(30, true, 3));
  });

  // Lengthening discards nothing, so asking would be a dialog that teaches an
  // operator to click through dialogs.
  it("saves a longer window without a confirmation", async () => {
    const save = vi.spyOn(api, "updateUsageSettings").mockResolvedValue({
      data: settings({ console_window_days: 90, revision: 4 }), etag: '"4"',
    });
    renderWithClient(<UsageWindowForm settings={settings({ console_window_days: 30 })} />);

    fireEvent.change(screen.getByLabelText(/窗口长度/), { target: { value: "90" } });
    fireEvent.click(screen.getByRole("button", { name: "保存窗口" }));

    await waitFor(() => expect(save).toHaveBeenCalledWith(90, false, 3));
    expect(screen.queryByText("缩短窗口会丢弃调用历史")).not.toBeInTheDocument();
  });

  // A preset the archive cannot back is not offered. An option that exists only
  // to be refused by the server is a worse answer than not showing it.
  it("offers no window longer than the archive keeps", () => {
    renderWithClient(<UsageWindowForm settings={settings({ console_window_days: 30, max_days: 60 })} />);
    const options = screen.getAllByRole("option").map((option) => option.textContent);
    expect(options).toEqual(["30 天", "60 天"]);
  });

  // config.yaml seeds this once. Without the notice, an operator edits the file,
  // restarts, and reads the unchanged screen as a bug.
  it("says so when the configuration file is no longer what decides", () => {
    renderWithClient(<UsageWindowForm settings={settings({ console_window_days: 30, config_file_in_effect: false })} />);
    expect(screen.getByText("配置文件已不再生效")).toBeInTheDocument();
    expect(screen.getByText(/config.yaml 写的是 90 天/)).toBeInTheDocument();
  });
});
