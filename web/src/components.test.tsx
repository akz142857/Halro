import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";
import { InlineTestControl } from "./components";

describe("InlineTestControl", () => {
  it("keeps one stable live region through every test state", () => {
    const onTest = vi.fn();
    const { rerender } = render(<InlineTestControl state="idle" onTest={onTest} />);
    const button = screen.getByRole("button", { name: "测试" });
    const status = screen.getByRole("status");
    expect(status).toHaveTextContent("尚未测试");
    fireEvent.click(button);
    expect(onTest).toHaveBeenCalledOnce();

    rerender(<InlineTestControl state="running" onTest={onTest} />);
    expect(screen.getByRole("button", { name: "测试" })).toBe(button);
    expect(button).toBeDisabled();
    expect(button.closest(".inline-test-control")).toHaveAttribute("aria-busy", "true");
    expect(screen.getByRole("status")).toBe(status);
    expect(status).toHaveTextContent("测试中…");

    rerender(<InlineTestControl state="success" latency={42} onTest={onTest} />);
    expect(screen.getByRole("button", { name: "测试" })).toBe(button);
    expect(screen.getByRole("status")).toBe(status);
    expect(status).toHaveTextContent("通过 · 42ms");
  });
});
