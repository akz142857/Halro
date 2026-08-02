import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import type { TokenGuardPolicy } from "../types";
import { PoliciesPage } from "./PoliciesPage";

const policy = {
  id: "tgp_test", name: "Production guard", enabled: true, action: "alert",
  request_tokens: 32000, tokens_per_minute: 200000, cost_micros_per_minute: 5000000,
  error_rate: 0.2, minimum_samples: 10, concurrency: 20, unique_ips_per_minute: 10,
  violations_before_block: 2, block_ttl_seconds: 300, cooldown_seconds: 60,
  ewma_enabled: false, ewma_alpha: 0.2, ewma_multiplier: 3, ewma_minimum_samples: 100,
  ewma_warmup_seconds: 3600, ewma_evaluation_window_seconds: 60,
  ewma_cooldown_seconds: 300, ewma_absolute_rpm: 60, ewma_absolute_tpm: 50000,
  ewma_absolute_tokens_per_request: 4000, ewma_absolute_cost_micros_per_minute: 1000000,
  revision: 1,
} as TokenGuardPolicy;

describe("token guard policy workflow", () => {
  beforeEach(() => {
    vi.spyOn(api, "tokenGuardPolicies").mockResolvedValue({ items: [], next_cursor: "" });
    vi.spyOn(api, "redactionPolicies").mockResolvedValue({ items: [], next_cursor: "" });
  });

  afterEach(() => vi.restoreAllMocks());

  it("creates policies disabled and reports domain validation at the fields", async () => {
    const create = vi.spyOn(api, "createTokenGuardPolicy").mockResolvedValue({} as never);
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建安全策略" }));

    expect(screen.getByLabelText("启用此策略")).not.toBeChecked();
    fireEvent.change(screen.getByLabelText("错误率阈值（%）"), { target: { value: "101" } });
    fireEvent.click(screen.getByRole("button", { name: "保存策略" }));

    expect(await screen.findByText("Required")).toBeVisible();
    expect(screen.getByText("Must be between 0 and 100")).toBeVisible();
    expect(screen.getByRole("textbox", { name: /^策略名称/ })).toHaveAttribute("aria-invalid", "true");
    expect(create).not.toHaveBeenCalled();
  });

  it("passes every relevant window signal to the simulator", async () => {
    vi.mocked(api.tokenGuardPolicies).mockResolvedValue({ items: [policy], next_cursor: "" });
    const preview = vi.spyOn(api, "previewTokenGuardPolicy").mockResolvedValue({ violated: false, reason: "", action: "observe" });
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "模拟" }));

    fireEvent.change(screen.getByLabelText("本次估算成本（USD）"), { target: { value: "1.25" } });
    fireEvent.change(screen.getByLabelText("窗口内已有成本（USD）"), { target: { value: "2.5" } });
    fireEvent.change(screen.getByLabelText("窗口内请求数"), { target: { value: "12" } });
    fireEvent.change(screen.getByLabelText("窗口内错误数"), { target: { value: "3" } });
    fireEvent.change(screen.getByLabelText("窗口内唯一 IP 数"), { target: { value: "7" } });
    fireEvent.click(screen.getByLabelText("本次请求来自新来源（唯一 IP +1）"));
    fireEvent.click(screen.getByRole("button", { name: "运行模拟" }));

    await waitFor(() => expect(preview).toHaveBeenCalledWith("tgp_test", expect.objectContaining({
      estimated_cost_micros_usd: 1250000,
      has_new_source: false,
      window: expect.objectContaining({ requests: 12, errors: 3, unique_ips: 7, cost_micros_usd: 2500000 }),
    })));
  });

  it("locks every close path while a save is pending", async () => {
    let finish!: (value: never) => void;
    vi.spyOn(api, "createTokenGuardPolicy").mockReturnValue(new Promise((resolve) => { finish = resolve; }));
    renderPage();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建安全策略" }));
    fireEvent.change(screen.getByLabelText("策略名称"), { target: { value: "Pending guard" } });
    fireEvent.click(screen.getByRole("button", { name: "保存策略" }));

    await waitFor(() => expect(screen.getByRole("button", { name: "取消" })).toBeDisabled());
    expect(screen.getByRole("button", { name: "关闭" })).toBeDisabled();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(screen.getByRole("heading", { name: "创建令牌防护策略" })).toBeVisible();
    finish({} as never);
  });

  it("exposes the enabled state as readable card text", async () => {
    vi.mocked(api.tokenGuardPolicies).mockResolvedValue({ items: [policy], next_cursor: "" });
    renderPage();
    expect(await screen.findByText("启用", { selector: ".policy-card header small" })).toBeVisible();
  });
});

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(<QueryClientProvider client={client}><PoliciesPage /></QueryClientProvider>);
}
