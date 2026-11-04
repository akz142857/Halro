import { fireEvent, render, screen, within } from "@testing-library/react";
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
  it("orders warnings, unread checks and normal results stably without mutating the server answer", () => {
    const findings = [
      finding({ rule: "normal_first" }),
      finding({ rule: "warning_first", status: "warn" }),
      finding({ rule: "unread_first", status: "unknown" }),
      finding({ rule: "warning_second", status: "warn" }),
      finding({ rule: "normal_second" }),
      finding({ rule: "unread_second", status: "unknown" }),
    ];
    const originalOrder = findings.map((item) => item.rule);
    render(<AdvisorFindingsPanel findings={findings} />);
    const rendered = screen.getAllByRole("listitem").map((row) => originalOrder.find((rule) => within(row).queryByText(rule)));
    expect(rendered).toEqual(["warning_first", "warning_second", "unread_first", "unread_second", "normal_first", "normal_second"]);
    expect(findings.map((item) => item.rule)).toEqual(originalOrder);
  });

  it("keeps an unread rule's full consequence visible without implying a successful check", () => {
    render(<AdvisorFindingsPanel findings={[finding({
      rule: "unread_rule_added_upstream", status: "unknown", consequence: "The runtime has not provided the required evidence.", evidence: [], comparison: "runtime evidence unread",
    })]} />);
    expect(screen.getByText("The runtime has not provided the required evidence.")).toBeVisible();
    expect(screen.queryByText(i18n.t("advisor.summaryClear"))).not.toBeInTheDocument();
    expect(document.querySelector(".advisor-overall-status")).not.toHaveClass("ok");
  });

  it("treats an empty answer as no diagnostic evidence rather than a green pass", () => {
    render(<AdvisorFindingsPanel findings={[]} />);
    expect(screen.queryAllByRole("listitem")).toHaveLength(0);
    expect(screen.queryByText(i18n.t("advisor.summaryClear"))).not.toBeInTheDocument();
    expect(document.querySelector(".advisor-overall-status")).not.toHaveClass("ok");
    expect(screen.getByText(i18n.t("advisor.title"))).toBeVisible();
  });

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
    expect(within(row.querySelector(".advisor-evidence") as HTMLElement).getByText("单次调用超时")).toBeInTheDocument();
    expect(within(row).getByText("gateway.attempt_response_header_timeout")).toBeInTheDocument();
    expect(within(row).getByText("1 分钟")).toBeInTheDocument();
    expect(within(row).getByText("1m0s < 2m0s")).toBeInTheDocument();
  });

  // The conclusion is the sentence an operator reads first, so it quotes the
  // numbers it is about instead of pointing at them.
  it("keeps the verdict visible and lets operators disclose its parameters and arithmetic", () => {
    render(<AdvisorFindingsPanel findings={[finding({ status: "warn" })]} />);
    const details = document.querySelector(".advisor-evidence-details") as HTMLDetailsElement;
    expect(details.open).toBe(false);
    expect(document.querySelector(".advisor-consequence")).toBeVisible();
    expect(screen.getByText("gateway.attempt_response_header_timeout")).not.toBeVisible();
    fireEvent.click(details.querySelector("summary")!);
    expect(details.open).toBe(true);
    expect(screen.getByText("gateway.attempt_response_header_timeout")).toBeVisible();
    expect(screen.getByText("1m0s < 2m0s")).toBeVisible();
  });

  it("keeps normal checks to a brief and discloses the full numerical explanation", () => {
    render(<AdvisorFindingsPanel findings={[finding()]} />);
    const row = screen.getByRole("listitem");
    const details = row.querySelector("details")!;
    expect(details).not.toHaveAttribute("open");
    expect(within(row.querySelector("summary")!).getByText(i18n.t("advisor.rules.attempt_header_timeout_reachable"))).toBeVisible();
    expect(row.querySelector(".advisor-brief")).toBeVisible();
    const explanation = row.querySelector(".advisor-explanation");
    expect(explanation).not.toBeVisible();
    expect(explanation).toHaveTextContent("1 分钟");
    expect(explanation).toHaveTextContent("2 分钟");
    expect(screen.getByText("gateway.attempt_response_header_timeout")).not.toBeVisible();
    expect(screen.getByText("1m0s < 2m0s")).not.toBeVisible();
    fireEvent.click(details.querySelector("summary")!);
    expect(explanation).toBeVisible();
    expect(screen.getByText("gateway.attempt_response_header_timeout")).toBeVisible();
    expect(screen.getByText("1m0s < 2m0s")).toBeVisible();
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
    expect(document.querySelector(".advisor-explanation")).toHaveTextContent(i18n.t("advisor.consequences.unclassified_refusals.okNone"));
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
    expect(document.querySelector(".advisor-explanation")).toHaveTextContent(i18n.t("advisor.consequences.attempt_budget_reaches_fanout.okSingle"));
  });

  it("does not turn an empty topology into a claim that every alias has one candidate", () => {
    render(<AdvisorFindingsPanel findings={[finding({
      rule: "attempt_budget_reaches_fanout",
      comparison: "ceil(3 / 1) = 3 >= 0",
      evidence: [
        { name: "gateway.max_total_attempts", value: "3" },
        { name: "retry.max_attempts_per_target", value: "1" },
        { name: "candidates_the_budget_reaches", value: "3" },
        { name: "widest_fan_out", value: "0" },
      ],
    })]} />);
    expect(screen.queryByText(/各别名仅有一个备选/)).not.toBeInTheDocument();
    expect(screen.getByText("ceil(3 / 1) = 3 >= 0")).toBeInTheDocument();
  });

  it("does not infer healthy or usable candidates from an empty suspension list", () => {
    render(<AdvisorFindingsPanel findings={[finding({
      rule: "suspended_scopes", comparison: "0 scopes suspended", evidence: [{ name: "suspended_scopes", value: "0" }],
    })]} />);
    expect(screen.queryByText("全部备选均可接收请求。")).not.toBeInTheDocument();
    expect(screen.getByText("0 scopes suspended")).toBeInTheDocument();
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
    expect(screen.getByText(/本实例单次超时（1 分钟）/)).toBeInTheDocument();
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

  // A dl row keeps its term and value paired. The config key belongs inside
  // that pair, rather than becoming an unpaired third child of the row.
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

  it("retains an untranslated configuration key in full without repeating it as its own label", () => {
    const configKey = "gateway.future_regional_provider_attempt_budget_override";
    render(<AdvisorFindingsPanel findings={[finding({
      rule: "new_gateway_rule",
      evidence: [{ name: configKey, value: "17" }],
      comparison: "17 >= 12",
    })]} />);
    const row = screen.getByRole("listitem");
    expect(within(row).getAllByText(configKey)).toHaveLength(1);
    expect(row.textContent?.split(configKey).length).toBe(2);
    expect(within(row).getByText(configKey)).not.toBeVisible();
    fireEvent.click(row.querySelector("summary")!);
    expect(within(row).getByText(configKey)).toBeVisible();
    expect(within(row).getByText(configKey).closest("dt")).toHaveTextContent(configKey);
    expect(within(row).getByText("17", { selector: "dd" })).toBeVisible();
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
    expect(within(screen.getByRole("listitem").querySelector("summary")!).getByText("what the server said about it")).toBeVisible();
  });

  // The verdict is carried in a word as well as a colour.
  it("names the verdict in text", () => {
    render(<AdvisorFindingsPanel findings={[finding({ status: "warn" })]} />);
    expect(screen.getByText(i18n.t("advisor.statuses.warn"))).toBeInTheDocument();
  });
});
