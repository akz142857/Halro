import { act, fireEvent, render, screen } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { NotificationProvider, useNotify, type NotificationInput } from "./notifications";

function Emitter({ input, label }: { input: NotificationInput; label: string }) {
  const { notify } = useNotify();
  return <button onClick={() => notify(input)}>{label}</button>;
}

describe("system notifications", () => {
  afterEach(() => {
    vi.useRealTimers();
  });

  it("dismisses a success by itself and keeps a failure until it is closed", () => {
    vi.useFakeTimers();
    render(
      <NotificationProvider>
        <Emitter label="save" input={{ tone: "success", title: "服务商已保存", description: "OpenAI" }} />
        <Emitter label="fail" input={{ tone: "error", title: "服务商状态未能更新" }} />
      </NotificationProvider>,
    );

    fireEvent.click(screen.getByRole("button", { name: "save" }));
    fireEvent.click(screen.getByRole("button", { name: "fail" }));
    expect(screen.getByText("服务商已保存")).toBeVisible();
    expect(screen.getByText("OpenAI")).toBeVisible();

    act(() => { vi.advanceTimersByTime(6500); });
    expect(screen.queryByText("服务商已保存")).not.toBeInTheDocument();
    // An error that removed itself would be the same defect as an error
    // rendered below the fold: gone before it was read.
    expect(screen.getByText("服务商状态未能更新")).toBeVisible();

    fireEvent.click(screen.getByRole("button", { name: "关闭通知" }));
    expect(screen.queryByText("服务商状态未能更新")).not.toBeInTheDocument();
  });

  it("counts a repeated message instead of stacking copies of it", () => {
    vi.useFakeTimers();
    render(
      <NotificationProvider>
        <Emitter label="save" input={{ tone: "success", title: "路由已保存" }} />
      </NotificationProvider>,
    );

    fireEvent.click(screen.getByRole("button", { name: "save" }));
    act(() => { vi.advanceTimersByTime(4000); });
    fireEvent.click(screen.getByRole("button", { name: "save" }));

    expect(screen.getAllByText("路由已保存")).toHaveLength(1);
    expect(screen.getByText("×2")).toBeVisible();
    // The repeat restarted the countdown, so the original 6s deadline passing
    // must not take the refreshed notification with it.
    act(() => { vi.advanceTimersByTime(3000); });
    expect(screen.getByText("路由已保存")).toBeVisible();
    act(() => { vi.advanceTimersByTime(3500); });
    expect(screen.queryByText("路由已保存")).not.toBeInTheDocument();
  });

  it("separates the interrupting region from the polite one", () => {
    render(
      <NotificationProvider>
        <Emitter label="save" input={{ tone: "success", title: "凭据已加密保存" }} />
        <Emitter label="fail" input={{ tone: "error", title: "服务商状态未能更新" }} />
      </NotificationProvider>,
    );

    fireEvent.click(screen.getByRole("button", { name: "save" }));
    fireEvent.click(screen.getByRole("button", { name: "fail" }));
    expect(screen.getByText("服务商状态未能更新").closest("[aria-live]")).toHaveAttribute("aria-live", "assertive");
    expect(screen.getByText("凭据已加密保存").closest("[aria-live]")).toHaveAttribute("aria-live", "polite");
  });

  it("holds a notification open while the pointer is on the column", () => {
    vi.useFakeTimers();
    render(
      <NotificationProvider>
        <Emitter label="save" input={{ tone: "success", title: "模型部署已保存" }} />
      </NotificationProvider>,
    );

    fireEvent.click(screen.getByRole("button", { name: "save" }));
    const region = screen.getByLabelText("系统通知");
    fireEvent.mouseEnter(region);
    act(() => { vi.advanceTimersByTime(10_000); });
    expect(screen.getByText("模型部署已保存")).toBeVisible();

    fireEvent.mouseLeave(region);
    act(() => { vi.advanceTimersByTime(6500); });
    expect(screen.queryByText("模型部署已保存")).not.toBeInTheDocument();
  });

  it("keeps the newest notifications when more arrive than the column holds", () => {
    render(
      <NotificationProvider>
        <Emitter label="one" input={{ tone: "success", title: "第一条" }} />
        <Emitter label="two" input={{ tone: "success", title: "第二条" }} />
        <Emitter label="three" input={{ tone: "success", title: "第三条" }} />
        <Emitter label="four" input={{ tone: "success", title: "第四条" }} />
        <Emitter label="five" input={{ tone: "success", title: "第五条" }} />
      </NotificationProvider>,
    );

    for (const label of ["one", "two", "three", "four", "five"]) {
      fireEvent.click(screen.getByRole("button", { name: label }));
    }
    expect(screen.queryByText("第一条")).not.toBeInTheDocument();
    expect(screen.getByText("第五条")).toBeVisible();
    expect(document.querySelectorAll(".notification-card")).toHaveLength(4);
  });

  // An error is the one notification nothing dismisses on the operator's behalf,
  // and on several pages it is the only place a rejected toggle is reported.
  // Evicting it to make room for a confirmation loses the failure entirely.
  it("evicts a self-dismissing notification before an unread failure", () => {
    render(
      <NotificationProvider>
        <Emitter label="fail" input={{ tone: "error", title: "路由状态未能更新" }} />
        <Emitter label="one" input={{ tone: "success", title: "第一条" }} />
        <Emitter label="two" input={{ tone: "success", title: "第二条" }} />
        <Emitter label="three" input={{ tone: "success", title: "第三条" }} />
        <Emitter label="four" input={{ tone: "success", title: "第四条" }} />
      </NotificationProvider>,
    );

    for (const label of ["fail", "one", "two", "three", "four"]) {
      fireEvent.click(screen.getByRole("button", { name: label }));
    }
    expect(screen.getByText("路由状态未能更新")).toBeVisible();
    expect(screen.queryByText("第一条")).not.toBeInTheDocument();
    expect(document.querySelectorAll(".notification-card")).toHaveLength(4);
  });

  // Dismissing from the keyboard removes the focused button, and a node that
  // leaves the DOM fires no blur: the pause that focus started outlived it and
  // every later notification stopped auto-dismissing.
  it("resumes auto-dismissal after a failure is closed from the keyboard", () => {
    vi.useFakeTimers();
    render(
      <NotificationProvider>
        <Emitter label="fail" input={{ tone: "error", title: "路由状态未能更新" }} />
        <Emitter label="save" input={{ tone: "success", title: "模型部署已保存" }} />
      </NotificationProvider>,
    );

    fireEvent.click(screen.getByRole("button", { name: "fail" }));
    const dismiss = screen.getAllByRole("button", { name: "关闭通知" })[0];
    dismiss.focus();
    fireEvent.focus(dismiss);
    fireEvent.click(dismiss);
    expect(screen.queryByText("路由状态未能更新")).not.toBeInTheDocument();

    fireEvent.click(screen.getByRole("button", { name: "save" }));
    act(() => { vi.advanceTimersByTime(6500); });
    expect(screen.queryByText("模型部署已保存")).not.toBeInTheDocument();
  });
});
