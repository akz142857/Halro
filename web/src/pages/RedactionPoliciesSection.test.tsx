import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { fireEvent, render, screen, waitFor, within } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { api } from "../api";
import { stepUpRequired } from "../test/fixtures";
import type { RedactionPolicy } from "../types";
import { RedactionPoliciesSection } from "./RedactionPoliciesSection";

describe("RedactionPoliciesSection form safety", () => {
  afterEach(() => vi.restoreAllMocks());

  it("creates policies disabled and converts enabled rules when detect-only streaming is selected", async () => {
    const create = vi.spyOn(api, "createRedactionPolicy").mockResolvedValue({} as never);
    renderSection();

    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建脱敏策略" }));
    const dialog = screen.getByRole("dialog", { name: "创建脱敏策略" });
    const actions = dialog.querySelector<HTMLElement>(".redaction-form-actions")!;
    expect(within(actions).getByRole("checkbox", { name: "启用此策略" })).not.toBeChecked();
    expect(within(dialog).getByText("启用此策略 · 已禁用")).toBeInTheDocument();

    fireEvent.change(within(dialog).getByLabelText("策略名称"), { target: { value: "Safe stream" } });
    fireEvent.click(within(dialog).getByRole("radio", { name: /仅检测/ }));
    expect(within(dialog).getByRole("radio", { name: /仅检测/ })).toBeChecked();
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
    fireEvent.change(within(dialog).getByLabelText(/^优先级/), { target: { value: "-1" } });
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "入站" }));
    fireEvent.click(within(dialog).getByRole("checkbox", { name: "出站" }));
    fireEvent.change(within(dialog).getByLabelText("类型"), { target: { value: "regex" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "编译并保存" }));

    expect(create).not.toHaveBeenCalled();
    expect(within(dialog).getByText("请输入规则名称")).toBeInTheDocument();

    fireEvent.change(within(dialog).getByLabelText(/^名称/), { target: { value: "Custom" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "编译并保存" }));
    expect(within(dialog).getByText("至少选择一个作用方向")).toBeInTheDocument();

    fireEvent.click(within(dialog).getByRole("checkbox", { name: "入站" }));
    fireEvent.change(within(dialog).getByLabelText(/^优先级/), { target: { value: "" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "编译并保存" }));
    expect(within(dialog).getByText("请输入 0–10000 之间的整数")).toBeInTheDocument();

    fireEvent.change(within(dialog).getByLabelText(/^优先级/), { target: { value: "10" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "编译并保存" }));
    expect(within(dialog).getByText("请输入 RE2 正则表达式")).toBeInTheDocument();

    fireEvent.change(within(dialog).getByLabelText("类型"), { target: { value: "dictionary" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "编译并保存" }));
    expect(within(dialog).getByText("至少输入一个字典项")).toBeInTheDocument();
    expect(create).not.toHaveBeenCalled();
  });

  it("adds and manages many rules through a single selected editor", async () => {
    renderSection();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建脱敏策略" }));
    const dialog = screen.getByRole("dialog", { name: "创建脱敏策略" });

    fireEvent.change(within(dialog).getByLabelText("新规则类型"), { target: { value: "regex" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "＋ 添加规则" }));
    fireEvent.change(within(dialog).getByLabelText("新规则类型"), { target: { value: "dictionary" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "＋ 添加规则" }));

    expect(within(dialog).getByText("共 3 条 · 3 条启用")).toBeInTheDocument();
    expect(within(dialog).getAllByLabelText("名称")).toHaveLength(1);
    expect(within(dialog).getByLabelText("名称")).toHaveValue("自定义字典");

    fireEvent.click(within(dialog).getByRole("button", { name: "复制" }));
    expect(within(dialog).getByText("共 4 条 · 4 条启用")).toBeInTheDocument();
    expect(within(dialog).getByLabelText("名称")).toHaveValue("自定义字典 副本");

    const removeRule = within(dialog).getByRole("button", { name: "移除" });
    expect(removeRule).toHaveClass("danger-text");
    fireEvent.click(removeRule);
    expect(within(dialog).getByText("已移除“自定义字典 副本”")).toBeInTheDocument();
    fireEvent.click(within(dialog).getByRole("button", { name: "撤销" }));
    expect(within(dialog).getByText("共 4 条 · 4 条启用")).toBeInTheDocument();
  });

  it("asks before discarding a changed policy draft", async () => {
    renderSection();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建脱敏策略" }));
    const dialog = screen.getByRole("dialog", { name: "创建脱敏策略" });
    fireEvent.change(within(dialog).getByLabelText("策略名称"), { target: { value: "Changed" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "关闭" }));
    expect(within(dialog).getByRole("alert")).toHaveTextContent("关闭会丢弃此处已填写的内容");
  });

  it("edits an existing policy with binding impact and preserves rule ids", async () => {
    // The window is closed here, so the first save is refused and the second
    // carries what the operator typed.
    const update = vi.spyOn(api, "updateRedactionPolicy")
      .mockRejectedValueOnce(stepUpRequired())
      .mockResolvedValue({} as never);
    const policy: RedactionPolicy = {
      id: "rdp_existing", name: "Production", enabled: true, mode: "strict", revision: 7, bound_projects: 2,
      rules: [{
        id: "rrl_existing", name: "Email", kind: "builtin", builtin: "email", scopes: ["inbound"],
        action: "mask", enabled: true, priority: 20, computed_max_match_bytes: 128,
      }],
    };
    renderSection([policy]);

    fireEvent.click(await screen.findByRole("button", { name: "编辑" }));
    const dialog = screen.getByRole("dialog", { name: "编辑脱敏策略" });
    expect(within(dialog).getByText("已绑定 2 个项目")).toBeInTheDocument();
    expect(within(dialog).getByLabelText("名称")).toHaveValue("Email");
    fireEvent.change(within(dialog).getByLabelText(/^优先级/), { target: { value: "30" } });
    // Editing an existing policy can take enforcement away, so the server asks
    // who is doing it — but only once the re-authentication window has closed,
    // which is what the first save finds out.
    fireEvent.click(within(dialog).getByRole("button", { name: "编译并保存" }));
    fireEvent.change(await within(dialog).findByLabelText(/^当前密码/), { target: { value: "a passphrase" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "编译并保存" }));

    await waitFor(() => expect(update).toHaveBeenCalledTimes(2));
    expect(update).toHaveBeenLastCalledWith("rdp_existing", expect.objectContaining({
      rules: [expect.objectContaining({ id: "rrl_existing", priority: 30 })],
    }), 7, expect.objectContaining({ currentPassword: "a passphrase" }));
  });

  it("enables and disables policies directly from the policy list", async () => {
    // Each toggle is refused once first: this fixture has no open window, so
    // both go through the console's ask-then-send path.
    const update = vi.spyOn(api, "updateRedactionPolicy")
      .mockRejectedValueOnce(stepUpRequired())
      .mockResolvedValueOnce({} as never)
      .mockRejectedValueOnce(stepUpRequired())
      .mockResolvedValue({} as never);
    const rule = {
      id: "rrl_existing", name: "Email", kind: "builtin" as const, builtin: "email", scopes: ["inbound" as const],
      action: "mask" as const, enabled: true, priority: 20, computed_max_match_bytes: 128,
    };
    renderSection([
      { id: "rdp_off", name: "Off", enabled: false, mode: "strict", revision: 2, rules: [rule] },
      { id: "rdp_on", name: "On", enabled: true, mode: "strict", revision: 4, rules: [rule] },
    ]);

    fireEvent.click(await screen.findByRole("button", { name: "启用" }));
    const enableDialog = await screen.findByRole("alertdialog");
    fireEvent.click(within(enableDialog).getByRole("button", { name: "启用" }));
    fireEvent.change(await within(enableDialog).findByLabelText(/^当前密码/), { target: { value: "a passphrase" } });
    fireEvent.click(within(enableDialog).getByRole("button", { name: "启用" }));
    await waitFor(() => expect(update).toHaveBeenCalledWith("rdp_off", expect.objectContaining({
      enabled: true,
      rules: [expect.not.objectContaining({ computed_max_match_bytes: expect.anything() })],
    }), 2, expect.objectContaining({ currentPassword: "a passphrase" })));

    // Switching a redaction policy off removes DLP from every Project bound to it,
    // so it asks first — the same weight the delete action carries.
    fireEvent.click(screen.getByRole("button", { name: "禁用" }));
    const dialog = await screen.findByRole("alertdialog");
    expect(within(dialog).getByText(/未脱敏内容直连服务商/)).toBeVisible();
    fireEvent.click(within(dialog).getByRole("button", { name: "禁用" }));
    fireEvent.change(await within(dialog).findByLabelText(/^当前密码/), { target: { value: "a passphrase" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "禁用" }));
    await waitFor(() => expect(update).toHaveBeenCalledWith("rdp_on", expect.objectContaining({ enabled: false }), 4, expect.objectContaining({ currentPassword: "a passphrase" })));
  });

  // The controlled textarea normalised on every keystroke: Enter produced an empty
  // trailing entry that was filtered away, so a second term could never be typed,
  // and a space inside a term was trimmed before the next character arrived.
  it("accepts dictionary entries typed line by line, spaces and all", async () => {
    const create = vi.spyOn(api, "createRedactionPolicy").mockResolvedValue({} as never);
    renderSection();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建脱敏策略" }));
    const dialog = screen.getByRole("dialog", { name: "创建脱敏策略" });

    fireEvent.change(within(dialog).getByLabelText("策略名称"), { target: { value: "Dictionary" } });
    fireEvent.change(within(dialog).getByLabelText("新规则类型"), { target: { value: "dictionary" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "＋ 添加规则" }));

    const items = within(dialog).getByLabelText("字典（每行一项）");
    fireEvent.change(items, { target: { value: "客户 名单" } });
    expect(items).toHaveValue("客户 名单");
    fireEvent.change(items, { target: { value: "客户 名单\n" } });
    expect(items).toHaveValue("客户 名单\n");
    fireEvent.change(items, { target: { value: "客户 名单\n内部 代号" } });
    expect(items).toHaveValue("客户 名单\n内部 代号");

    fireEvent.click(within(dialog).getByRole("button", { name: "编译并保存" }));
    await waitFor(() => expect(create).toHaveBeenCalledOnce());
    // Normalisation happens once, at the boundary.
    expect(create).toHaveBeenCalledWith(expect.objectContaining({
      rules: [expect.anything(), expect.objectContaining({ dictionary: ["客户 名单", "内部 代号"] })],
    }));
  });

  it("does not flag a rule the operator has not filled in yet", async () => {
    renderSection();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建脱敏策略" }));
    const dialog = screen.getByRole("dialog", { name: "创建脱敏策略" });

    fireEvent.change(within(dialog).getByLabelText("新规则类型"), { target: { value: "regex" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "＋ 添加规则" }));

    expect(within(dialog).queryByText("1 个问题")).not.toBeInTheDocument();
    expect(within(dialog).getByLabelText("正则表达式")).not.toHaveAttribute("aria-invalid");

    // Leaving the rule editor is where "not filled in yet" becomes "left unfilled".
    fireEvent.focusOut(within(dialog).getByLabelText("正则表达式"));
    expect(within(dialog).getByText("1 个问题")).toBeVisible();
    expect(within(dialog).getByLabelText(/^正则表达式/)).toHaveAttribute("aria-invalid", "true");
  });

  it("puts the operator on the control that refused the submit", async () => {
    renderSection();
    fireEvent.click(await screen.findByRole("button", { name: "＋ 新建脱敏策略" }));
    const dialog = screen.getByRole("dialog", { name: "创建脱敏策略" });

    fireEvent.click(within(dialog).getByRole("button", { name: "编译并保存" }));
    expect(within(dialog).getByLabelText(/^策略名称/)).toHaveFocus();

    fireEvent.change(within(dialog).getByLabelText(/^策略名称/), { target: { value: "Policy" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "移除" }));
    fireEvent.click(within(dialog).getByRole("button", { name: "编译并保存" }));
    expect(within(dialog).getByRole("button", { name: "＋ 添加规则" })).toHaveFocus();
  });

  it("keeps the rule list still while a rule is being edited", async () => {
    const rule = {
      kind: "builtin" as const, builtin: "email", scopes: ["inbound" as const],
      action: "mask" as const, enabled: true, computed_max_match_bytes: 128,
    };
    renderSection([{
      id: "rdp_order", name: "Ordered", enabled: true, mode: "strict", revision: 1,
      rules: [
        { ...rule, id: "rrl_low", name: "Low", priority: 10 },
        { ...rule, id: "rrl_high", name: "High", priority: 20 },
      ],
    }]);

    fireEvent.click(await screen.findByRole("button", { name: "编辑" }));
    const dialog = screen.getByRole("dialog", { name: "编辑脱敏策略" });
    const order = () => Array.from(dialog.querySelectorAll(".redaction-rule-summary-title strong"), (node) => node.textContent);
    expect(order()).toEqual(["High", "Low"]);

    fireEvent.click(within(dialog).getByRole("button", { name: /^Low/ }));
    fireEvent.change(within(dialog).getByLabelText(/^优先级/), { target: { value: "100" } });
    // The row the operator is editing must not jump out from under the pointer.
    expect(order()).toEqual(["High", "Low"]);

    fireEvent.click(within(dialog).getByRole("button", { name: "按执行顺序重排" }));
    expect(order()).toEqual(["Low", "High"]);
  });

  it("shows the configured rule name instead of its built-in category in test results", async () => {
    const policy: RedactionPolicy = {
      id: "rdp_phone", name: "Phone masking", enabled: true, mode: "strict", revision: 1,
      rules: [{
        id: "rrl_phone", name: "客户手机号", kind: "builtin", builtin: "china_phone", scopes: ["inbound"],
        action: "mask", enabled: true, priority: 10, computed_max_match_bytes: 32,
      }],
    };
    vi.spyOn(api, "testRedactionPolicy").mockResolvedValue({
      match_count: 1,
      matches: [{ rule_id: "rrl_phone", category: "china_phone", action: "mask", field: "" }],
    });
    renderSection([policy]);

    fireEvent.click(await screen.findByRole("button", { name: "测试" }));
    const dialog = screen.getByRole("dialog", { name: "安全测试 · Phone masking" });
    fireEvent.change(within(dialog).getByRole("textbox"), { target: { value: "13800138000" } });
    fireEvent.click(within(dialog).getByRole("button", { name: "运行测试" }));

    expect(await within(dialog).findByText("客户手机号 · 掩码")).toBeVisible();
    expect(within(dialog).queryByText("china_phone · 掩码")).not.toBeInTheDocument();
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

function renderSection(policies: RedactionPolicy[] = []) {
  const client = new QueryClient({ defaultOptions: { queries: { retry: false }, mutations: { retry: false } } });
  return render(
    <QueryClientProvider client={client}>
      <RedactionPoliciesSection policies={policies} />
    </QueryClientProvider>,
  );
}
