import { afterEach, describe, expect, it, vi } from "vitest";
import { createElement } from "react";
import { render } from "@testing-library/react";
import { navigate, setNavigationBlocked, usePathname } from "./navigation";

function PathObserver() {
  return createElement("span", null, usePathname());
}

describe("guarded navigation", () => {
  afterEach(() => {
    setNavigationBlocked(false);
    vi.restoreAllMocks();
  });

  it("explains one-time recovery code risk and allows the user to stay", () => {
    window.history.replaceState({}, "", "/admin/settings");
    const confirm = vi.spyOn(window, "confirm").mockReturnValue(false);
    setNavigationBlocked(true, "Save the one-time recovery codes before leaving.");
    navigate("/admin");
    expect(confirm).toHaveBeenCalledWith("Save the one-time recovery codes before leaving.");
    expect(window.location.pathname).toBe("/admin/settings");
  });

  it("continues navigation after explicit confirmation", () => {
    window.history.replaceState({}, "", "/admin/settings");
    vi.spyOn(window, "confirm").mockReturnValue(true);
    setNavigationBlocked(true, "Save first");
    navigate("/admin");
    expect(window.location.pathname).toBe("/admin");
  });

  // Destinations that carry their subject in the query — a request ID, a project —
  // are different destinations even on the same path, and clearing a filter is a
  // navigation back to the plain list rather than a no-op.
  it("treats the query as part of the destination", () => {
    window.history.replaceState({}, "", "/admin/usage?project_id=project_a");
    navigate("/admin/usage?project_id=project_b");
    expect(window.location.search).toBe("?project_id=project_b");
    navigate("/admin/usage");
    expect(window.location.search).toBe("");
    expect(window.location.pathname).toBe("/admin/usage");
  });

  it("keeps the guarded page when browser history navigation is cancelled", () => {
    window.history.replaceState({}, "", "/admin/settings");
    render(createElement(PathObserver));
    vi.spyOn(window, "confirm").mockReturnValue(false);
    setNavigationBlocked(true, "Save first");
    window.history.pushState({}, "", "/admin");
    window.dispatchEvent(new PopStateEvent("popstate"));
    expect(window.location.pathname).toBe("/admin/settings");
  });
});
