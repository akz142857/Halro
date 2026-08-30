import { fireEvent, render, screen } from "@testing-library/react";
import { useState } from "react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Combobox } from "./components";

// The menu is positioned by measurement, and jsdom has no layout: every box is
// zero. So the field's box and the viewport are stubbed, which is the only way
// to assert which side the menu opens on — and that side was wrong in a way
// nothing here could see, because no test read the style it computes.
function openAt({ top, height, viewport }: { top: number; height: number; viewport: number }) {
  vi.spyOn(window, "innerHeight", "get").mockReturnValue(viewport);
  vi.spyOn(Element.prototype, "getBoundingClientRect").mockReturnValue({
    top, bottom: top + height, left: 0, right: 200, width: 200, height,
    x: 0, y: top, toJSON: () => ({}),
  } as DOMRect);
}

function Harness({ options, emptyText }: { options: string[]; emptyText?: string }) {
  const [value, setValue] = useState("");
  return <Combobox
    label="公共模型别名"
    value={value}
    onChange={setValue}
    options={options.map((option) => ({ value: option }))}
    listLabel="已有的公共模型别名"
    emptyText={emptyText}
  />;
}

const menu = () => screen.getByRole("listbox");

describe("Combobox menu placement", () => {
  afterEach(() => vi.restoreAllMocks());

  // The flip threshold was a flat 176px, sized for the timezone list. A menu of
  // one row would rather open into 150px of space than flip — flipping put it
  // on top of the field and covered the label above it, which is exactly what
  // the alias field did on a real screen.
  it("opens a short menu downwards even when a long one would not fit", () => {
    openAt({ top: 300, height: 40, viewport: 500 });
    render(<Harness options={[]} emptyText="没有匹配的已有别名，保存后会新建一个。" />);

    fireEvent.focus(screen.getByRole("combobox"));

    // 500 - 340 - 12 = 148 below: less than the 176 a full list wants, and more
    // than the one row this menu is about to draw.
    expect(menu().style.top).toBe("344px");
    expect(menu().style.bottom).toBe("");
  });

  // And the flip still happens when the menu genuinely cannot fit, or the field
  // near the foot of the window would open a list off the bottom of the screen.
  it("flips a long menu that has no room beneath the field", () => {
    openAt({ top: 400, height: 40, viewport: 500 });
    render(<Harness options={["alpha", "beta", "gamma", "delta", "epsilon", "zeta"]} />);

    fireEvent.focus(screen.getByRole("combobox"));

    // 500 - 440 - 12 = 48 below against 388 above, and six rows want more.
    expect(menu().style.bottom).toBe("104px");
    expect(menu().style.top).toBe("");
  });

  // Typing filters the list, so the side it opens on has to be reconsidered:
  // the menu that needed to flip stops needing to once one option is left.
  it("reconsiders the side when the query shortens the list", () => {
    openAt({ top: 400, height: 40, viewport: 500 });
    render(<Harness options={["alpha", "beta", "gamma", "delta", "epsilon", "zeta"]} />);
    fireEvent.focus(screen.getByRole("combobox"));
    expect(menu().style.bottom).toBe("104px");

    fireEvent.change(screen.getByRole("combobox"), { target: { value: "alp" } });

    expect(menu().style.top).toBe("444px");
    expect(menu().style.bottom).toBe("");
  });
});
