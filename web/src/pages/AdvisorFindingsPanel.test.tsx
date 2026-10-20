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
  // have to take on trust — but the values read as words and durations, with
  // the key kept beside them rather than in their place.
  it("renders the evidence a finding was drawn from", () => {
    render(<AdvisorFindingsPanel findings={[finding()]} />);
    const row = screen.getByRole("listitem");
    expect(within(row).getByText("单次调用超时")).toBeInTheDocument();
    expect(within(row).getByText("gateway.attempt_response_header_timeout")).toBeInTheDocument();
    expect(within(row).getByText("1 分钟")).toBeInTheDocument();
    expect(within(row).getByText("1m0s < 2m0s")).toBeInTheDocument();
  });

  // The conclusion is the sentence an operator reads first, so it quotes the
  // numbers it is about instead of pointing at them.
  it("states the conclusion in plain words with the numbers in it", () => {
    render(<AdvisorFindingsPanel findings={[finding()]} />);
    expect(screen.getByText(/1 分钟 内没有开始响应就会被放弃；整个请求最多 2 分钟/)).toBeInTheDocument();
  });

  // "Nothing refused yet" is not "every refusal understood", and the second
  // sentence quoted with a zero reads as a contradiction.
  it("words a clean refusal counter differently from an empty one", () => {
    render(<AdvisorFindingsPanel findings={[finding({
      rule: "unclassified_refusals",
      comparison: "0 of 0 refusals unclassified",
      evidence: [
        { name: "refusals_observed", value: "0" },
        { name: "refusals_unclassified", value: "0" },
      ],
    })]} />);
    expect(screen.getByText(i18n.t("advisor.consequences.unclassified_refusals.okNone"))).toBeInTheDocument();
  });

  // One sentence per status cannot say both "every alias has one route" and
  // "the budget reaches all four".
  it("words a single-route fan-out differently from a covered one", () => {
    render(<AdvisorFindingsPanel findings={[finding({
      rule: "attempt_budget_reaches_fanout",
      comparison: "ceil(3 / 1) = 3 >= 1",
      evidence: [
        { name: "gateway.max_total_attempts", value: "3" },
        { name: "retry.max_attempts_per_target", value: "1" },
        { name: "candidates_the_budget_reaches", value: "3" },
        { name: "widest_fan_out", value: "1" },
      ],
    })]} />);
    expect(screen.getByText(i18n.t("advisor.consequences.attempt_budget_reaches_fanout.okSingle"))).toBeInTheDocument();
  });

  // A suspension with no reason was counted by the availability policy, which
  // the instance's own deadline feeds. The server says so by adding that
  // deadline to the evidence, and the sentence has to carry the advice.
  it("points an unexplained suspension at the instance's own deadline first", () => {
    render(<AdvisorFindingsPanel findings={[finding({
      rule: "suspended_scopes",
      status: "warn",
      comparison: "1 scopes suspended, 0 of them indefinitely",
      evidence: [
        { name: "suspended_scopes", value: "1" },
        { name: "deployment:dep_1", value: "unclassified" },
        { name: "gateway.attempt_response_header_timeout", value: "1m0s" },
      ],
    })]} />);
    expect(screen.getByText(/本实例自己的单次调用超时（1 分钟）/)).toBeInTheDocument();
    expect(screen.getByText("模型部署 · dep_1")).toBeInTheDocument();
  });

  // Durations are rewritten for reading only where the key says the value is
  // one. Go's sub-second forms are left as written rather than misparsed, and
  // a value that merely looks like a duration — an alias called "5m" — is not
  // a duration at all.
  it("reads durations only where the key says they are durations", () => {
    render(<AdvisorFindingsPanel findings={[finding({
      evidence: [
        { name: "gateway.attempt_response_header_timeout", value: "500ms" },
        { name: "gateway.route_total_timeout", value: "1h0m0s" },
        { name: "widest_fan_out_public_model", value: "5m" },
      ],
    })]} />);
    const row = screen.getByRole("listitem");
    expect(within(row).getByText("500ms")).toBeInTheDocument();
    expect(within(row).getByText("1 小时")).toBeInTheDocument();
    expect(within(row).getByText("5m")).toBeInTheDocument();
  });

  it("reads minutes and seconds together, and zero as zero", () => {
    render(<AdvisorFindingsPanel findings={[finding({
      evidence: [
        { name: "gateway.attempt_response_header_timeout", value: "0s" },
        { name: "gateway.route_total_timeout", value: "3m30s" },
      ],
    })]} />);
    const row = screen.getByRole("listitem");
    expect(within(row).getByText("0 秒")).toBeInTheDocument();
    expect(within(row).getByText("3 分钟 30 秒")).toBeInTheDocument();
  });

  // A dl row may hold only dt and dd; the config key lives inside the dd it
  // describes so a screen reader still pairs each name with its value.
  it("keeps every evidence row to a term and its description", () => {
    render(<AdvisorFindingsPanel findings={[finding()]} />);
    const rows = document.querySelectorAll(".advisor-evidence > div");
    expect(rows.length).toBe(2);
    rows.forEach((row) => {
      expect([...row.children].map((child) => child.tagName)).toEqual(["DT", "DD"]);
    });
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
