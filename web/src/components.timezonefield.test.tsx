import { fireEvent, render, screen, within } from "@testing-library/react";
import { useState } from "react";
import { afterAll, beforeAll, describe, expect, it, vi } from "vitest";

// Stubbed before the module graph loads because the picker reads the zone list
// once and caches it. A fixed list also keeps the assertions readable: the real
// ICU database is 400-odd names and grows with every tzdata release.
vi.hoisted(() => {
  Object.defineProperty(Intl, "supportedValuesOf", {
    configurable: true,
    writable: true,
    value: () => ["America/New_York", "Asia/Shanghai", "Europe/Berlin", "UTC"],
  });
});

const { TimeZoneField } = await import("./components");

function Harness({ initial = "", onPick }: { initial?: string; onPick?: (value: string) => void }) {
  const [value, setValue] = useState(initial);
  return <TimeZoneField
    label="服务商时区"
    value={value}
    onChange={(next) => { setValue(next); onPick?.(next); }}
  />;
}

const zoneField = () => screen.getByLabelText("服务商时区");

// The offset beside each name is the one in force *now*, so a real clock would
// make the expected values flip at every DST transition. Pinned to a January
// instant, where the northern zones are all on standard time.
beforeAll(() => {
  vi.useFakeTimers({ toFake: ["Date"] });
  vi.setSystemTime(new Date("2026-01-15T12:00:00Z"));
});
afterAll(() => vi.useRealTimers());

describe("TimeZoneField", () => {
  it("offers every zone the engine reports, each with its current offset", () => {
    render(<Harness />);
    fireEvent.focus(zoneField());
    const list = screen.getByRole("listbox", { name: "时区列表" });
    expect(within(list).getAllByRole("option").map((option) => option.textContent)).toEqual([
      "America/New_YorkUTC-05:00",
      "Asia/ShanghaiUTC+08:00",
      "Europe/BerlinUTC+01:00",
      "UTCUTC+00:00",
    ]);
    expect(within(list).getByText("4 个时区")).toBeVisible();
  });

  // The reason a picker beats the old free text box: nobody types the
  // underscore, and before this the spelling was only checked on submit.
  it("finds a zone typed the way the city is actually spelled", () => {
    render(<Harness />);
    fireEvent.change(zoneField(), { target: { value: "new york" } });
    const options = within(screen.getByRole("listbox", { name: "时区列表" })).getAllByRole("option");
    expect(options).toHaveLength(1);
    expect(options[0]).toHaveTextContent("America/New_York");
  });

  // Zone names are ASCII and the host locale is not the console's: folding them
  // in it maps the "I" of Indian/Maldives to a dotless "ı" on a Turkish
  // machine, and a typed "indian" stops matching. Pinned by asserting the
  // locale-sensitive fold is never the one used, which is the only way to hold
  // it without a Turkish test runner.
  it("case-folds zone names independently of the host locale", () => {
    const localeFold = vi.spyOn(String.prototype, "toLocaleLowerCase");
    render(<Harness />);
    fireEvent.change(zoneField(), { target: { value: "BERLIN" } });
    const options = within(screen.getByRole("listbox", { name: "时区列表" })).getAllByRole("option");
    expect(options).toHaveLength(1);
    expect(options[0]).toHaveTextContent("Europe/Berlin");
    expect(localeFold).not.toHaveBeenCalled();
    localeFold.mockRestore();
  });

  // The list is driven by the arrow keys, so leaving 400-odd options tabbable
  // would bury the next control — inside the price modal, the Save button —
  // behind one Tab press each.
  it("keeps its options out of the tab order", () => {
    render(<Harness />);
    fireEvent.focus(zoneField());
    for (const option of screen.getAllByRole("option")) {
      expect(option).toHaveAttribute("tabindex", "-1");
    }
  });

  // Name and offset are separate inline elements, so without this a screen
  // reader announces them run together as "Asia/ShanghaiUTC+08:00".
  it("announces the offset as a separate word from the name", () => {
    render(<Harness />);
    fireEvent.focus(zoneField());
    expect(screen.getByRole("option", { name: "Asia/Shanghai UTC+08:00" })).toBeVisible();
  });

  it("writes the canonical IANA name when an option is chosen", () => {
    const onPick = vi.fn();
    render(<Harness onPick={onPick} />);
    fireEvent.change(zoneField(), { target: { value: "shang" } });
    fireEvent.click(screen.getByRole("option", { name: /Asia\/Shanghai/ }));
    expect(onPick).toHaveBeenCalledWith("Asia/Shanghai");
    expect(zoneField()).toHaveValue("Asia/Shanghai");
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
  });

  it("selects with the keyboard alone", () => {
    render(<Harness />);
    const input = zoneField();
    fireEvent.keyDown(input, { key: "ArrowDown" });
    fireEvent.keyDown(input, { key: "ArrowDown" });
    fireEvent.keyDown(input, { key: "Enter" });
    expect(input).toHaveValue("Asia/Shanghai");
  });

  it("closes on Escape without discarding what was typed", () => {
    render(<Harness />);
    const input = zoneField();
    fireEvent.change(input, { target: { value: "Asia/Sh" } });
    fireEvent.keyDown(input, { key: "Escape" });
    expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    expect(input).toHaveValue("Asia/Sh");
  });

  // The list is browser tzdata and the validator is the server's, so a name
  // missing here is a spelling warning, not a refusal — the field stays
  // submittable and the server decides.
  it("warns about an unrecognised name without blocking it", () => {
    render(<Harness initial="Asia/Shangai" />);
    expect(screen.getByText("此浏览器不认识该时区名，请检查拼写。最终以服务端校验为准。")).toBeVisible();
    expect(zoneField()).not.toBeRequired();
    expect(zoneField()).toHaveValue("Asia/Shangai");
  });

  it("accepts an alias the canonical list omits but the engine understands", () => {
    render(<Harness initial="Asia/Calcutta" />);
    expect(screen.queryByText(/此浏览器不认识该时区名/)).not.toBeInTheDocument();
  });

  // Every ancestor that could hold the menu also clips it: the price modal
  // scrolls its own body and the settings card ends above the foot of the list,
  // so an absolutely positioned menu was cut off in both.
  describe("placement", () => {
    const stubRect = (top: number, height = 40) => vi.spyOn(Element.prototype, "getBoundingClientRect")
      .mockReturnValue({ top, bottom: top + height, left: 24, right: 424, width: 400, height, x: 24, y: top, toJSON: () => ({}) } as DOMRect);

    it("escapes the clipping ancestor and hangs off the field's box on screen", () => {
      stubRect(100);
      const { container } = render(<Harness />);
      fireEvent.focus(zoneField());

      const list = screen.getByRole("listbox", { name: "时区列表" });
      expect(container.contains(list)).toBe(false);
      expect(document.body.contains(list)).toBe(true);
      expect(list.style.top).toBe("144px");
      expect(list.style.left).toBe("24px");
      expect(list.style.maxHeight).toBe("288px");
      // Flush with the field it belongs to, however wide the form makes that.
      expect(list.style.width).toBe("400px");
    });

    it("flips above the field when the room below it has run out", () => {
      // 768 tall in jsdom, so a field at 700 leaves 28px underneath.
      stubRect(700);
      render(<Harness />);
      fireEvent.focus(zoneField());

      const list = screen.getByRole("listbox", { name: "时区列表" });
      expect(list.style.top).toBe("");
      expect(list.style.bottom).toBe("72px");
      // Shortened to the room it flipped into rather than overflowing the top.
      expect(list.style.maxHeight).toBe("288px");
    });

    // Not only when the room below is gone: 156px underneath would render a
    // menu two rows tall, which is worse than the 548px above it.
    it("flips for room that is merely too little, not just for none", () => {
      stubRect(560);
      render(<Harness />);
      fireEvent.focus(zoneField());

      const list = screen.getByRole("listbox", { name: "时区列表" });
      expect(list.style.bottom).toBe("212px");
      expect(list.style.maxHeight).toBe("288px");
    });

    // Neither side has the full height, so it takes the better one and cuts the
    // menu to fit instead of running off the screen.
    it("shortens itself when neither side has room for the whole list", () => {
      stubRect(150, 500);
      render(<Harness />);
      fireEvent.focus(zoneField());

      const list = screen.getByRole("listbox", { name: "时区列表" });
      expect(list.style.bottom).toBe("622px");
      expect(list.style.maxHeight).toBe("138px");
    });

    it("closes when a click lands outside both the field and the menu", () => {
      stubRect(100);
      render(<Harness />);
      fireEvent.focus(zoneField());
      expect(screen.getByRole("listbox", { name: "时区列表" })).toBeVisible();

      fireEvent.pointerDown(document.body);
      expect(screen.queryByRole("listbox")).not.toBeInTheDocument();
    });

    it("keeps the menu open for a pointer landing inside it", () => {
      stubRect(100);
      render(<Harness />);
      fireEvent.focus(zoneField());
      const list = screen.getByRole("listbox", { name: "时区列表" });

      fireEvent.pointerDown(list);
      expect(screen.getByRole("listbox", { name: "时区列表" })).toBeVisible();
    });
  });

  it("reports a validation error from the caller ahead of its own hint", () => {
    render(<TimeZoneField label="服务商时区" hint="填 IANA 名称" error="请填写服务商时区" value="" onChange={() => {}} />);
    expect(screen.getByText("请填写服务商时区")).toBeVisible();
    expect(screen.queryByText("填 IANA 名称")).not.toBeInTheDocument();
    expect(zoneField()).toHaveAttribute("aria-invalid", "true");
  });
});
