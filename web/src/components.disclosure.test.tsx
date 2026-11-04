import { fireEvent, render, screen, waitFor } from "@testing-library/react";
import { StrictMode, useState } from "react";
import { describe, expect, it, vi } from "vitest";
import { DisclosureSection, Field, Modal, PageHeader } from "./components";

describe("concise page headers", () => {
  it("needs only a title and does not leave empty explanatory elements", () => {
    const { container } = render(<PageHeader title="Models" action={<button>Create</button>} />);
    expect(screen.getByRole("heading", { level: 1, name: "Models" })).toBeVisible();
    expect(screen.getByRole("button", { name: "Create" })).toBeVisible();
    expect(container.querySelector(".eyebrow, .page-description")).toBeNull();
  });
});

describe("optional configuration", () => {
  it("keeps configured values mounted while showing a useful collapsed summary", () => {
    const { rerender } = render(<DisclosureSection title="Connection" summary="Proxy · 8 concurrent"><Field label="Limit"><input defaultValue="8" /></Field></DisclosureSection>);
    expect(screen.getByText("Proxy · 8 concurrent")).toBeVisible();
    const input = screen.getByLabelText("Limit");
    expect(input).not.toBeVisible();
    fireEvent.change(input, { target: { value: "12" } });
    rerender(<DisclosureSection title="Connection" summary="Proxy · 12 concurrent"><Field label="Limit"><input defaultValue="8" /></Field></DisclosureSection>);
    expect(input).toHaveValue("12");
    expect(screen.getByText("Proxy · 12 concurrent")).toBeVisible();
  });

  it("reveals nested invalid fields when validation is added after submit", async () => {
    const form = (error?: string) => <DisclosureSection title="Advanced"><DisclosureSection title="Network"><Field label="CIDR" error={error}><input /></Field></DisclosureSection></DisclosureSection>;
    const { rerender } = render(form());
    expect(screen.getByLabelText("CIDR")).not.toBeVisible();
    rerender(form("Enter a valid network"));
    await waitFor(() => expect(screen.getByLabelText(/^CIDR/)).toBeVisible());
    expect(screen.getByText("Enter a valid network")).toBeVisible();
    for (const summary of document.querySelectorAll("summary")) expect(summary).toHaveAttribute("aria-expanded", "true");
  });

  it("reveals a native validation failure and returns focus to the invalid input", async () => {
    render(<DisclosureSection title="Limits"><Field label="Maximum"><input required /></Field></DisclosureSection>);
    const input = screen.getByLabelText("Maximum");
    fireEvent.invalid(input);
    await waitFor(() => expect(input).toBeVisible());
    await waitFor(() => expect(input).toHaveFocus());
  });

  it("reveals unchanged errors again when an operator collapses the section and resubmits", async () => {
    render(<form aria-label="Project" onSubmit={(event) => event.preventDefault()}><DisclosureSection title="Security"><Field label="Network" error="Invalid network"><input /></Field></DisclosureSection><button>Save</button></form>);
    const details = screen.getByText("Security").closest("details")!;
    details.open = false;
    fireEvent(details, new Event("toggle"));
    fireEvent.submit(screen.getByRole("form", { name: "Project" }));
    await waitFor(() => expect(screen.getByLabelText(/^Network/)).toBeVisible());
    await waitFor(() => expect(screen.getByLabelText(/^Network/)).toHaveFocus());
  });

  it("keeps collapsed descendants out of a modal's focus loop", () => {
    const close = vi.fn();
    render(<Modal title="Edit" onClose={close}><DisclosureSection title="More"><Field label="Hidden"><input /></Field></DisclosureSection><button>Save</button></Modal>);
    const save = screen.getByRole("button", { name: "Save" });
    save.focus();
    fireEvent.keyDown(document, { key: "Tab" });
    expect(screen.getByRole("button", { name: "关闭" })).toHaveFocus();
    fireEvent.keyDown(document, { key: "Tab", shiftKey: true });
    expect(save).toHaveFocus();
    fireEvent.keyDown(document, { key: "Escape" });
    expect(close).toHaveBeenCalledOnce();
  });
});

describe("autofocused modal recovery", () => {
  it.each([{ dirty: false, strict: false }, { dirty: true, strict: false }, { dirty: false, strict: true }, { dirty: true, strict: true }])("preserves input and trigger focus (dirty=$dirty, StrictMode=$strict)", ({ dirty, strict }) => {
    function Editor() {
      const [open, setOpen] = useState(false);
      return <><button onClick={() => setOpen(true)}>Create</button>{open && <Modal title="New resource" dirty={dirty} onClose={() => setOpen(false)}><Field label="Name"><input autoFocus /></Field></Modal>}</>;
    }
    render(strict ? <StrictMode><Editor /></StrictMode> : <Editor />);
    const trigger = screen.getByRole("button", { name: "Create" });
    trigger.focus();
    fireEvent.click(trigger);
    expect(screen.getByLabelText("Name")).toHaveFocus();
    fireEvent.keyDown(document, { key: "Escape" });
    if (dirty) {
      fireEvent.click(screen.getByRole("button", { name: "继续编辑" }));
      expect(screen.getByLabelText("Name")).toHaveFocus();
      fireEvent.keyDown(document, { key: "Escape" });
      fireEvent.click(screen.getByRole("button", { name: "放弃填写的内容" }));
    }
    expect(screen.queryByRole("dialog")).not.toBeInTheDocument();
    expect(trigger).toHaveFocus();
  });
});
