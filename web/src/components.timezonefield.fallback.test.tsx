import { fireEvent, render, screen } from "@testing-library/react";
import { describe, expect, it, vi } from "vitest";

// Its own file: the zone list is read once per module graph, so an engine
// without `supportedValuesOf` can only be staged before the import happens.
vi.hoisted(() => {
  Reflect.deleteProperty(Intl, "supportedValuesOf");
});

const { TimeZoneField } = await import("./components");

describe("TimeZoneField without an enumerable zone database", () => {
  // An engine too old to list zones can still format them, and the server would
  // accept whatever is typed. Degrading to the plain field it replaced keeps
  // that operator working; an empty menu would only look broken.
  it("degrades to a plain text field", () => {
    render(<TimeZoneField label="供应商时区" value="" onChange={() => {}} />);
    const input = screen.getByLabelText("供应商时区");
    fireEvent.focus(input);
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(input).not.toHaveAttribute("role", "combobox");
    expect(input).toHaveAttribute("placeholder", "Asia/Shanghai");
    fireEvent.keyDown(input, { key: "ArrowDown" });
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
  });

  it("still validates what was typed against the engine it does have", () => {
    render(<TimeZoneField label="供应商时区" value="Nowhere/Nowhere" onChange={() => {}} />);
    expect(screen.getByText("此浏览器不认识该时区名，请检查拼写。最终以服务端校验为准。")).toBeVisible();
  });
});
