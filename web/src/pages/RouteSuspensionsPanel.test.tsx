import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import i18n from "../i18n";
import type { Credential, Deployment, Provider, RouteSuspension } from "../types";
import { RouteSuspensionsPanel, TabRefusalMark, refusalCountsByTab } from "./RouteSuspensionsPanel";

const credential = {
  id: "credential_openai", name: "OpenAI production", type: "openai",
  access_surface: "openai-api", offering_id: "openai.api-platform", region_id: "",
  scheme: "bearer.static", bound_base_url: "https://api.openai.com:443",
  secret_configured: true, key_version: 1, revision: 3,
} as Credential;

const provider = { id: "provider_main", name: "OpenAI 主连接" } as Provider;
const deployment = { id: "deployment_mini", name: "gpt-4o-mini 部署" } as Deployment;

function suspension(overrides: Partial<RouteSuspension> = {}): RouteSuspension {
  return {
    scope_kind: "credential", scope_key: "credential_openai",
    reason: "invalid_credential", observed_at: "2026-09-20T10:00:00Z",
    indefinite: true, ...overrides,
  };
}

function renderPanel(items: RouteSuspension[], state: "loading" | "ready" | "unavailable" = "ready") {
  return render(
    <RouteSuspensionsPanel
      suspensions={items}
      state={state}
      credentials={[credential]}
      providers={[provider]}
      deployments={[deployment]}
    />,
  );
}

describe("route suspensions panel", () => {
  it("names the credential rather than printing its identifier", () => {
    renderPanel([suspension()]);
    expect(screen.getByText("OpenAI production")).toBeVisible();
    expect(screen.queryByText("credential_openai")).not.toBeInTheDocument();
  });

  // The whole reason this reads the gate's scopes instead of the deployment
  // list: one credential backs several deployments and takes all of them out at
  // once, and a row per deployment would show several unexplained failures
  // where there is one cause.
  it("resolves every scope kind against the lists the page already has", () => {
    renderPanel([
      suspension(),
      suspension({ scope_kind: "provider", scope_key: "provider_main", reason: "rate_limited", indefinite: false }),
      suspension({ scope_kind: "deployment", scope_key: "deployment_mini", reason: "unclassified", indefinite: false }),
    ]);
    expect(screen.getByText("OpenAI production")).toBeVisible();
    expect(screen.getByText("OpenAI 主连接")).toBeVisible();
    expect(screen.getByText("gpt-4o-mini 部署")).toBeVisible();
  });

  // A model identifier may contain a slash, so only the first one is the join
  // the server made. Splitting on the last would move part of the model name
  // into the credential id and resolve neither half.
  it("splits a credential-and-model key on its first separator only", () => {
    renderPanel([suspension({
      scope_kind: "credential_model",
      scope_key: "credential_openai/publisher/model-v1",
      reason: "subscription_quota_exhausted", indefinite: false,
    })]);
    expect(screen.getByText("OpenAI production")).toBeVisible();
    expect(screen.getByText(/publisher\/model-v1/)).toBeVisible();
  });

  // "Replace the credential" and "back at a time" are different instructions,
  // and the operator acts on one and waits on the other.
  it("separates a suspension the operator must end from one a clock ends", () => {
    const { rerender } = renderPanel([suspension()]);
    expect(screen.getByText(i18n.t("providers.suspensions.untilReplaced"))).toBeVisible();

    rerender(
      <RouteSuspensionsPanel
        suspensions={[suspension({ indefinite: false, until: "2026-09-20T14:05:00Z" })]}
        state="ready" credentials={[credential]} providers={[provider]} deployments={[deployment]}
      />,
    );
    expect(screen.queryByText(i18n.t("providers.suspensions.untilReplaced"))).not.toBeInTheDocument();
    // Asserted on the recovery cell rather than on the page: the observed-at
    // column carries a timestamp too, and a bare year matches both.
    const cells = within(screen.getByRole("table")).getAllByRole("cell");
    expect(cells[2]).toHaveTextContent(
      i18n.t("providers.suspensions.untilTime", { time: new Date("2026-09-20T14:05:00Z").toLocaleString() }),
    );
  });

  // The question an operator asks after they have already replaced the key.
  it("records the credential revision the refusal was seen against", () => {
    renderPanel([suspension({ credential_revision: 2 })]);
    expect(screen.getByText(i18n.t("providers.suspensions.observedRevision", { revision: 2 }))).toBeVisible();
  });

  // Reusing the connection test's sentence is the point: a second vocabulary
  // for one condition is what that table was written to prevent.
  it("says a refused credential the way a failed connection test says it", () => {
    renderPanel([suspension()]);
    expect(screen.getByText(new RegExp(i18n.t("testControl.reasons.authentication")))).toBeVisible();
  });

  it("puts the upstream status and code beside the reason", () => {
    renderPanel([suspension({ provider_status: 401, provider_code: "invalid_api_key" })]);
    expect(screen.getByText(/HTTP 401 · invalid_api_key/)).toBeVisible();
  });

  // An exception list with no exception is not a table of em dashes above the
  // page; it is nothing at all.
  it("renders nothing at all while nothing is suspended", () => {
    const { container } = renderPanel([]);
    expect(container).toBeEmptyDOMElement();
  });

  // The one state that still needs a line: an absent panel would otherwise be
  // read as the answer the gate could not give.
  it("does not let an unreadable gate disappear like an answer", () => {
    renderPanel([], "unavailable");
    expect(screen.getByText(i18n.t("providers.suspensions.unavailable"))).toBeVisible();
    expect(screen.queryByRole("table")).not.toBeInTheDocument();
  });

  // Still loading is not an answer either, but it is also not a warning: the
  // rows arrive on their own a moment later.
  it("shows nothing while the gate is still being read", () => {
    const { container } = renderPanel([], "loading");
    expect(container).toBeEmptyDOMElement();
  });
});

describe("refusal counts per tab", () => {
  // A credential backs several connections, and the model half of a
  // credential_model scope narrows that same credential — both are the vault's
  // to answer. A deployment has no tab on this page and is counted nowhere.
  it("attributes each scope to the tab that can act on it", () => {
    expect(refusalCountsByTab([
      suspension(),
      suspension({ scope_kind: "credential_model", scope_key: "credential_openai/gpt-4o" }),
      suspension({ scope_kind: "provider", scope_key: "provider_main" }),
      suspension({ scope_kind: "deployment", scope_key: "deployment_mini" }),
    ])).toEqual({ providers: 1, credentials: 2 });
  });
});

describe("tab refusal mark", () => {
  it("says in words what the dot says in colour", () => {
    render(<TabRefusalMark count={2} />);
    expect(screen.getByText("●2", { exact: false })).toBeVisible();
    expect(screen.getByText(i18n.t("providers.suspensions.tabMark", { count: 2 }))).toBeInTheDocument();
  });

  it("is absent rather than zero when nothing under the tab is refused", () => {
    const { container } = render(<TabRefusalMark count={0} />);
    expect(container).toBeEmptyDOMElement();
  });
});
