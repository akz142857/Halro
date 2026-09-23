import { render, screen, within } from "@testing-library/react";
import { describe, expect, it } from "vitest";
import i18n from "../i18n";
import type { AdvisorFinding } from "../types";
import { AdvisorFindingsPanel } from "./AdvisorFindingsPanel";

function finding(overrides: Partial<AdvisorFinding> = {}): AdvisorFinding {
  return {
    rule: "attempt_header_timeout_reachable",
    status: "ok",
    comparison: "1m0s < 2m0s",
    consequence: "server wording",
    evidence: [
      { name: "gateway.attempt_response_header_timeout", value: "1m0s" },
      { name: "gateway.route_total_timeout", value: "2m0s" },
    ],
    ...overrides,
  };
}

describe("AdvisorFindingsPanel", () => {
  // The rule the whole surface rests on. A panel that hides its quiet rows
  // cannot tell "checked and fine" from "never ran", and an operator who only
  // ever sees problems has no reason to believe the absence of one.
  it("keeps a row for every rule, including the ones that found nothing", () => {
    render(<AdvisorFindingsPanel findings={[
      finding(),
      finding({ rule: "retry_fits_route_budget", comparison: "1m0s × 1 = 1m0s <= 2m0s" }),
      finding({ rule: "suspended_scopes", status: "unknown", comparison: "admission gate not read", evidence: [] }),
    ]} />);
    expect(screen.getAllByRole("listitem")).toHaveLength(3);
    expect(screen.getByText("1m0s × 1 = 1m0s <= 2m0s")).toBeInTheDocument();
    expect(screen.getByText("admission gate not read")).toBeInTheDocument();
  });

  // Every finding shows the values its comparison read. That is the difference
  // between a finding an operator can check against config.yaml and one they
  // have to take on trust.
  it("renders the evidence a finding was drawn from", () => {
    render(<AdvisorFindingsPanel findings={[finding()]} />);
    const row = screen.getByRole("listitem");
    expect(within(row).getByText("gateway.attempt_response_header_timeout")).toBeInTheDocument();
    expect(within(row).getByText("1m0s")).toBeInTheDocument();
    expect(within(row).getByText("1m0s < 2m0s")).toBeInTheDocument();
  });

  // A configuration key is what an operator greps config.yaml for, so it is
  // never translated. A number this panel named is.
  it("leaves configuration keys alone and names the derived numbers", () => {
    render(<AdvisorFindingsPanel findings={[finding({
      rule: "attempt_budget_reaches_fanout",
      status: "warn",
      comparison: "ceil(3 / 2) = 2 < 3",
      evidence: [
        { name: "gateway.max_total_attempts", value: "3" },
        { name: "candidates_the_budget_reaches", value: "2" },
      ],
    })]} />);
    expect(screen.getByText("gateway.max_total_attempts")).toBeInTheDocument();
    expect(screen.getByText(i18n.t("advisor.terms.candidates_the_budget_reaches"))).toBeInTheDocument();
  });

  // "Nothing to report" and "some rules could not run" are different answers,
  // and the summary is where an operator reads one of them at a glance.
  it("separates a clean instance from one whose rules could not run", () => {
    const { unmount } = render(<AdvisorFindingsPanel findings={[finding()]} />);
    expect(screen.getByText(i18n.t("advisor.summaryClear"))).toBeInTheDocument();
    unmount();

    render(<AdvisorFindingsPanel findings={[
      finding(),
      finding({ rule: "suspended_scopes", status: "unknown", evidence: [] }),
    ]} />);
    expect(screen.getByText(i18n.t("advisor.summaryUnknown", { count: 1 }))).toBeInTheDocument();
    expect(screen.queryByText(i18n.t("advisor.summaryClear"))).not.toBeInTheDocument();
  });

  // A warning outranks an unread rule in the summary: the thing an operator can
  // act on is the thing the header has to say.
  it("leads the summary with what can be acted on", () => {
    render(<AdvisorFindingsPanel findings={[
      finding({ status: "warn" }),
      finding({ rule: "suspended_scopes", status: "unknown", evidence: [] }),
    ]} />);
    expect(screen.getByText(i18n.t("advisor.summaryWarnings", { count: 1 }))).toBeInTheDocument();
  });

  // A rule this bundle has no copy for still has to read as itself. The server
  // ships its own English for exactly this, the same way the audit trail keeps
  // an action it cannot translate.
  it("falls back to the server's wording for a rule it has no copy for", () => {
    render(<AdvisorFindingsPanel findings={[finding({
      rule: "a_rule_added_upstream",
      consequence: "what the server said about it",
    })]} />);
    expect(screen.getByText("a_rule_added_upstream")).toBeInTheDocument();
    expect(screen.getByText("what the server said about it")).toBeInTheDocument();
  });

  // The verdict is carried in a word as well as a colour.
  it("names the verdict in text", () => {
    render(<AdvisorFindingsPanel findings={[finding({ status: "warn" })]} />);
    expect(screen.getByText(i18n.t("advisor.statuses.warn"))).toBeInTheDocument();
  });
});
