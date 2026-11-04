import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { act, fireEvent, render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import { ClusterPage } from "./ClusterPage";

function renderPage() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
  return render(<QueryClientProvider client={client}><ClusterPage /></QueryClientProvider>);
}

describe("ClusterPage", () => {
  afterEach(() => vi.restoreAllMocks());

  it("names standalone mode without claiming HA is healthy", async () => {
    vi.spyOn(api, "clusterStatus").mockResolvedValue({ mode: "standalone" });
    renderPage();
    expect(await screen.findByRole("heading", { name: "单实例模式" })).toBeVisible();
    expect(screen.queryByText("已连接")).not.toBeInTheDocument();
  });

  it("shows local HA indexes and authenticated sessions without claiming peer progress", async () => {
    vi.spyOn(api, "clusterStatus").mockResolvedValue({
      mode: "ha", cluster_id: "production-a", incarnation: "inc_01", node_id: "halro-0",
      role: "primary", term: 7, promised_term: 7, durable_index: 13, confirmed_index: 12,
      applied_index: 11, startup_ready: true,
      projection: { index: 11, metadata_epoch: 2, metadata_sequence: 9 },
      peers: [{ node_id: "halro-1", connected: true }, { node_id: "halro-2", connected: false }],
    });
    renderPage();
    await screen.findByText("halro-0", { selector: "summary span span" });
    expect(screen.getByText("production-a")).not.toBeVisible();
    fireEvent.click(screen.getByText("当前成员").closest("summary")!);
    expect(await screen.findByText("production-a")).toBeVisible();
    expect(screen.getByText("halro-0", { selector: "dd code" })).toBeVisible();
    expect(screen.getByText("本地持久索引").nextElementSibling).toHaveTextContent("13");
    expect(screen.getByText("已确认索引").nextElementSibling).toHaveTextContent("12");
    expect(screen.getByText("已应用索引").nextElementSibling).toHaveTextContent("11");
    expect(screen.getByText("待确认帧数").nextElementSibling).toHaveTextContent("1");
    expect(screen.getByText("待应用帧数").nextElementSibling).toHaveTextContent("1");
    expect(screen.getByText("halro-1").closest("div")).toHaveTextContent("已连接");
    expect(screen.getByText("halro-2").closest("div")).toHaveTextContent("未连接");
    expect(screen.getByText(/本节点观察值，不代表对端健康或已追平/)).toBeVisible();
  });

  it("keeps synchronized indexes in details while showing the local scope and progress summary", async () => {
    vi.spyOn(api, "clusterStatus").mockResolvedValue({
      mode: "ha", cluster_id: "production-a", incarnation: "inc_01", node_id: "halro-0",
      role: "replica", term: 7, promised_term: 7, durable_index: 13, confirmed_index: 13,
      applied_index: 13, startup_ready: true,
      projection: { index: 13, metadata_epoch: 2, metadata_sequence: 9 },
      peers: [{ node_id: "halro-1", connected: true }],
    });
    renderPage();
    expect(await screen.findByText("待确认 0 · 待应用 0")).toBeVisible();
    expect(screen.getByText("本地持久索引")).not.toBeVisible();
    expect(screen.getByText(/本节点观察值/)).toBeVisible();
    expect(screen.getByText("会话 1/1 · 未连接 0")).toBeVisible();
    fireEvent.click(screen.getByText("复制水位").closest("summary")!);
    expect(screen.getByText("本地持久索引")).toBeVisible();
  });

  it("shows a failed refresh instead of silently keeping an old cluster view", async () => {
    vi.spyOn(api, "clusterStatus").mockRejectedValue(new Error("cluster status unavailable"));
    renderPage();
    expect(await screen.findByRole("alert")).toBeVisible();
    expect(screen.queryByText("本地持久索引")).not.toBeInTheDocument();
  });

  it("blocks overlapping manual refreshes and recovers from a failed refresh", async () => {
    let complete!: (value: { mode: "standalone" }) => void;
    const request = vi.spyOn(api, "clusterStatus").mockResolvedValueOnce({ mode: "standalone" })
      .mockImplementationOnce(() => new Promise((resolve) => { complete = resolve; }))
      .mockRejectedValueOnce(new Error("refresh unavailable"))
      .mockResolvedValueOnce({ mode: "standalone" });
    renderPage();
    await screen.findByRole("heading", { name: "单实例模式" });
    const refresh = screen.getByRole("button", { name: "刷新" });
    fireEvent.click(refresh);
    await waitFor(() => expect(refresh).toBeDisabled());
    expect(refresh).toHaveAttribute("aria-busy", "true");
    fireEvent.click(refresh);
    expect(request).toHaveBeenCalledTimes(2);
    await act(async () => complete({ mode: "standalone" }));
    await waitFor(() => expect(refresh).toBeEnabled());
    fireEvent.click(refresh);
    expect(await screen.findByRole("alert")).toBeVisible();
    expect(screen.queryByRole("heading", { name: "单实例模式" })).not.toBeInTheDocument();
    fireEvent.click(refresh);
    expect(await screen.findByRole("heading", { name: "单实例模式" })).toBeVisible();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
    expect(request).toHaveBeenCalledTimes(4);
  });

  it("continues to poll every ten seconds without a manual refresh", async () => {
    vi.useFakeTimers();
    const request = vi.spyOn(api, "clusterStatus").mockResolvedValue({ mode: "standalone" });
    const view = renderPage();
    try {
      await act(async () => { await vi.advanceTimersByTimeAsync(1); });
      expect(request).toHaveBeenCalledTimes(1);
      await act(async () => { await vi.advanceTimersByTimeAsync(9998); });
      expect(request).toHaveBeenCalledTimes(1);
      await act(async () => { await vi.advanceTimersByTimeAsync(2); });
      expect(request).toHaveBeenCalledTimes(2);
    } finally {
      view.unmount();
      vi.useRealTimers();
    }
  });

  it("expands incomplete startup and explains when no peers are configured", async () => {
    vi.spyOn(api, "clusterStatus").mockResolvedValue({
      mode: "ha", cluster_id: "production-a", incarnation: "inc_01", node_id: "halro-0",
      role: "awaiting_decision", term: 7, promised_term: 7, durable_index: 13, confirmed_index: 13,
      applied_index: 13, startup_ready: false,
      projection: { index: 13, metadata_epoch: 2, metadata_sequence: 9 }, peers: [],
    });
    renderPage();
    expect(await screen.findByText("production-a")).toBeVisible();
    expect(screen.getByText("等待角色判定")).toBeVisible();
    expect(screen.getByText(/启动判定 · 尚未完成/)).toBeVisible();
    fireEvent.click(screen.getByRole("heading", { name: "已配置成员" }).closest("summary")!);
    expect(screen.getByText("未配置其他成员。")).toBeVisible();
    expect(screen.getByText("已认证会话状态，不含对端复制水位。")).toBeVisible();
  });
});
