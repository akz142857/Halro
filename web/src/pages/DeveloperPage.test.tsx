import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { api } from "../api";
import type { Project } from "../types";
import { DeveloperPage } from "./DeveloperPage";

const project: Project = {
  id: "project_1",
  name: "Checkout service",
  enabled: true,
  allowed_routes: ["support-chat", "text-embedding"],
  rpm: 60,
  tpm: 100000,
  max_concurrency: 8,
  daily_budget_micros_usd: 50000000,
  max_input_tokens: 0,
  max_output_tokens: 0,
  max_request_bytes: 0,
  max_stream_duration: 0,
  allowed_cidrs: null,
  redaction_policy_id: "",
  token_guard_policy_id: "",
  revision: 1,
  created_at: "2026-08-05T00:00:00Z",
  updated_at: "2026-08-05T00:00:00Z",
};

describe("DeveloperPage", () => {
  it("builds integration examples from project public routes without sending a request", async () => {
    vi.spyOn(api, "projects").mockResolvedValue({ items: [project], next_cursor: "" });
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(<QueryClientProvider client={client}><DeveloperPage /></QueryClientProvider>);

    expect(await screen.findByRole("heading", { name: "开发者工作台" })).toBeVisible();
    expect(await screen.findByRole("option", { name: "support-chat" })).toBeVisible();
    expect(screen.getByText("/v1/responses")).toBeVisible();
    expect(screen.getAllByText(/HEIMDALL_API_KEY/)).toHaveLength(2);
    expect(screen.getByRole("button", { name: "发送请求（待接入）" })).toBeDisabled();
    expect(screen.getByText("等待真实请求")).toBeVisible();

    fireEvent.change(screen.getByLabelText("API 协议"), { target: { value: "chat" } });
    expect(screen.getByText("/v1/chat/completions")).toBeVisible();
    expect(screen.getByText(/messages/)).toBeVisible();
  });
});
