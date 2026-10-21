import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen } from "@testing-library/react";
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
    expect(await screen.findByRole("heading", { name: "当前为单实例模式" })).toBeVisible();
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
    expect(await screen.findByText("production-a")).toBeVisible();
    expect(screen.getByText("halro-0")).toBeVisible();
    expect(screen.getByText("本地持久索引").nextElementSibling).toHaveTextContent("13");
    expect(screen.getByText("已确认索引").nextElementSibling).toHaveTextContent("12");
    expect(screen.getByText("已应用索引").nextElementSibling).toHaveTextContent("11");
    expect(screen.getByText("待确认帧数").nextElementSibling).toHaveTextContent("1");
    expect(screen.getByText("待应用帧数").nextElementSibling).toHaveTextContent("1");
    expect(screen.getByText("halro-1").closest("div")).toHaveTextContent("已连接");
    expect(screen.getByText("halro-2").closest("div")).toHaveTextContent("未连接");
    expect(screen.getByText(/连接已建立不代表对端健康或已追平/)).toBeVisible();
  });

  it("shows a failed refresh instead of silently keeping an old cluster view", async () => {
    vi.spyOn(api, "clusterStatus").mockRejectedValue(new Error("cluster status unavailable"));
    renderPage();
    expect(await screen.findByRole("alert")).toBeVisible();
    expect(screen.queryByText("本地持久索引")).not.toBeInTheDocument();
  });
});
