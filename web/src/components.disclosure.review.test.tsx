import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import { DisclosureSection, Field, Modal } from "./components";

describe("disclosure error focus order", () => {
  it("keeps focus on the first invalid field when separate sections fail together", async () => {
    const form = (error?: string) => <form>
      <DisclosureSection title="Capacity"><Field label="First" error={error}><input /></Field></DisclosureSection>
      <DisclosureSection title="Network"><Field label="Second" error={error}><input /></Field></DisclosureSection>
    </form>;
    const { rerender } = render(form());
    rerender(form("Invalid"));
    await waitFor(() => expect(screen.getByLabelText(/^First/)).toHaveFocus());
  });

  it("keeps the first native invalid field focused when both sections fail validation", async () => {
    render(<form>
      <DisclosureSection title="Capacity"><Field label="First"><input required /></Field></DisclosureSection>
      <DisclosureSection title="Network"><Field label="Second"><input required /></Field></DisclosureSection>
    </form>);
    fireEvent.invalid(screen.getByLabelText("First"));
    fireEvent.invalid(screen.getByLabelText("Second"));
    await waitFor(() => expect(screen.getByLabelText("First")).toHaveFocus());
  });
});

describe("modal roving tab controls", () => {
  it("wraps after the selected tab when later language tabs and copy are not tab stops", () => {
    render(<Modal drawer title="Code" onClose={() => {}}><button disabled>Copy</button><div role="tablist" aria-label="Language"><button role="tab" aria-selected={false} tabIndex={-1}>curl</button><button role="tab" aria-selected tabIndex={0}>JavaScript</button><button role="tab" aria-selected={false} tabIndex={-1}>Java</button></div></Modal>);
    const selected = screen.getByRole("tab", { name: "JavaScript" });
    selected.focus();
    fireEvent.keyDown(document, { key: "Tab" });
    const close = screen.getByRole("button", { name: "关闭" });
    expect(close).toHaveFocus();
    fireEvent.keyDown(document, { key: "Tab", shiftKey: true });
    expect(selected).toHaveFocus();
  });
});
