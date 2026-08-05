import { fireEvent, render, screen } from "@testing-library/react";
import { afterEach, beforeEach, describe, expect, it, vi } from "vitest";
import { ErrorBoundary } from "./components";

function Boom({ crash }: { crash: boolean }): React.ReactElement {
  if (crash) throw new Error("render exploded");
  return <p>page content</p>;
}

describe("ErrorBoundary", () => {
  beforeEach(() => vi.spyOn(console, "error").mockImplementation(() => {}));
  afterEach(() => vi.restoreAllMocks());

  it("shows the failure and its stack instead of unmounting the console", () => {
    render(<ErrorBoundary><Boom crash /></ErrorBoundary>);

    expect(screen.getByRole("alert")).toHaveTextContent("这个页面出错了");
    expect(screen.getByRole("button", { name: "重新加载页面" })).toBeVisible();
    // The stack ships collapsed so the panel stays readable, but it must be there to report.
    expect(screen.getByText(/render exploded/)).toBeInTheDocument();
    fireEvent.click(screen.getByText("错误详情"));
    expect(screen.getByText(/render exploded/)).toBeVisible();
  });

  it("renders children again after a retry", () => {
    const { rerender } = render(<ErrorBoundary><Boom crash /></ErrorBoundary>);
    expect(screen.getByRole("alert")).toBeVisible();

    rerender(<ErrorBoundary><Boom crash={false} /></ErrorBoundary>);
    fireEvent.click(screen.getByRole("button", { name: "重试渲染" }));

    expect(screen.getByText("page content")).toBeVisible();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });

  it("passes children through when nothing throws", () => {
    render(<ErrorBoundary><Boom crash={false} /></ErrorBoundary>);
    expect(screen.getByText("page content")).toBeVisible();
    expect(screen.queryByRole("alert")).not.toBeInTheDocument();
  });
});
