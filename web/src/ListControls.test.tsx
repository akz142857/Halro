import { act, fireEvent, render, screen } from "@testing-library/react";
import { beforeEach, describe, expect, it } from "vitest";
import { ListViews, useListQuery } from "./ListControls";
import { navigate, setNavigationBlocked } from "./navigation";

function QueryExample() {
  const { params, update } = useListQuery();
  return <><input aria-label="Search" value={params.get("q") ?? ""} onChange={(event) => update({ q: event.target.value })} />
    <ListViews label="Views" value={params.get("status") || "all"} items={[{ key: "all", label: "All" }, { key: "error", label: "Failures" }]} onChange={(key) => update({ status: key === "all" ? "" : key }, false)} />
    <button onClick={() => update({ q: "", status: "" })}>Clear</button></>;
}
describe("shareable list conditions", () => {
  beforeEach(() => { setNavigationBlocked(false); window.history.replaceState({}, "", "/admin/usage?tab=attempts&project_id=p1"); });
  it("combines filters, preserves page context, and restores query-only navigation", () => {
    render(<QueryExample />);
    fireEvent.change(screen.getByRole("textbox"), { target: { value: "req_1" } });
    fireEvent.click(screen.getByRole("button", { name: "Failures" }));
    expect(new URLSearchParams(window.location.search).get("q")).toBe("req_1");
    expect(new URLSearchParams(window.location.search).get("project_id")).toBe("p1");
    expect(screen.getByRole("button", { name: "Failures" })).toHaveAttribute("aria-pressed", "true");
    act(() => { window.history.replaceState({}, "", "/admin/usage?tab=attempts&q=earlier"); window.dispatchEvent(new PopStateEvent("popstate")); });
    expect(screen.getByRole("textbox")).toHaveValue("earlier");
    expect(screen.getByRole("button", { name: "All" })).toHaveAttribute("aria-pressed", "true");
    act(() => navigate("/admin/usage?tab=attempts&q=linked&status=error"));
    expect(screen.getByRole("textbox")).toHaveValue("linked");
    fireEvent.click(screen.getByRole("button", { name: "Clear" }));
    expect(window.location.search).toBe("?tab=attempts");
  });
});
