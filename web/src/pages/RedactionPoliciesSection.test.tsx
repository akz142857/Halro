import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import { RedactionPoliciesSection } from "./RedactionPoliciesSection";

describe("RedactionPoliciesSection form safety", () => {
  afterEach(() => vi.restoreAllMocks());

  it("creates policies disabled and converts enabled rules when detect-only streaming is selected", async () => {
    const create = vi.spyOn(api, "createRedactionPolicy").mockResolvedValue({} as never);
    renderSection();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建脱敏策略" }));
    const dialog = screen.getByRole("dialog", { name: "创建脱敏策略" });
    expect(within(dialog).getByRole("checkbox", { name: /启用此策略/ })).not.toBeChecked();
    expect(within(dialog).getByText("启用此策略 · 禁用")).toBeInTheDocument();

    fireEvent.change(within(dialog).getByLabelText("策略名称"), { target: { value: "Safe stream" } });
    fireEvent.change(within(dialog).getByLabelText("流式策略"), { target: { value: "detect_only_stream" } });
    expect(within(dialog).getByRole("status")).toHaveTextContent("启用规则 → 仅检测");
    expect(within(dialog).getByLabelText("动作")).toHaveValue("detect_only");
    expect(within(dialog).getByRole("option", { name: "掩码" })).toBeDisabled();

    fireEvent.click(within(dialog).getByRole("button", { name: "编译并保存" }));
    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    expect(create).toHaveBeenCalledWith(expect.objectContaining({
      name: "Safe stream",
      enabled: false,
      mode: "detect_only_stream",
      rules: [expect.objectContaining({ enabled: true, action: "detect_only" })],
    }));
  });

  it("blocks invalid names, scopes, priorities, regexes, and dictionaries before the API call", async () => {
    const create = vi.spyOn(api, "createRedactionPolicy").mockResolvedValue({} as never);
    renderSection();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建脱敏策略" }));
    const dialog = screen.getByRole("dialog", { name: "创建脱敏策略" });

    fireEvent.change(within(dialog).getByLabelText("策略名称"), { target: { value: "Policy" } });
    fireEvent.change(within(dialog).getByLabelText("名称"), { target: { value: " " } });
    fireEvent.change(within(dialog).getByLabelText("优先级"), { target: { value: "-1" } });
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "入站" }));
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "出站" }));
    fireEvent.change(within(dialog).getByLabelText("类型"), { target: { value: "regex" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "编译并保存" }));

    expect(create).not.toHaveBeenCalled();
    expect(within(dialog).getByRole("alert")).toHaveTextContent("Rule name is required");

    fireEvent.change(within(dialog).getByLabelText("名称"), { target: { value: "Custom" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "编译并保存" }));
    expect(within(dialog).getByRole("alert")).toHaveTextContent("Select at least one scope");

    fireEvent.click(within(dialog).getByRole("checkbox", { name: "入站" }));
    fireEvent.change(within(dialog).getByLabelText("优先级"), { target: { value: "" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "编译并保存" }));
    expect(within(dialog).getByRole("alert")).toHaveTextContent("Priority must be a non-negative integer");

    fireEvent.change(within(dialog).getByLabelText("优先级"), { target: { value: "10" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "编译并保存" }));
    expect(within(dialog).getByRole("alert")).toHaveTextContent("Regular expression is required");

    fireEvent.change(within(dialog).getByLabelText("类型"), { target: { value: "dictionary" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "编译并保存" }));
    expect(within(dialog).getByRole("alert")).toHaveTextContent("At least one dictionary item is required");
    expect(create).not.toHaveBeenCalled();
  });

  it("disables cancel and modal close while save is pending", async () => {
    vi.spyOn(api, "createRedactionPolicy").mockImplementation(() => new Promise(() => {}));
    renderSection();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建脱敏策略" }));
    const dialog = screen.getByRole("dialog", { name: "创建脱敏策略" });
    fireEvent.change(within(dialog).getByLabelText("策略名称"), { target: { value: "Pending" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "编译并保存" }));

    await waitFor(() => expect(within(dialog).getByRole("button", { name: "取消" })).toBeDisabled());
    expect(within(dialog).getByRole("button", { name: "关闭" })).toBeDisabled();
  });
});

function renderSection() {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <RedactionPoliciesSection policies={[]} />
    </QueryClientProvider>,
  );
}
