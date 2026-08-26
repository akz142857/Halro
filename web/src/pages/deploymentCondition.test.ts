import { describe, expect, it } from "vitest";
import { deploymentCondition, deploymentNeedsAttention, recordedTestState } from "./deploymentCondition";
import type { CapabilityReview, Deployment, DeploymentProbe } from "../types";

function record(overrides: Partial<Deployment> = {}): Deployment {
  return {
    id: "dep_1", name: "Deployment", provider_id: "p", provider_model: "gpt", access_surface: "openai-api",
    profile_id: "openai.chat-embeddings.v1", region: "", capabilities: {} as Deployment["capabilities"],
    capability_evidence: {}, input_micros_per_million: 0, output_micros_per_million: 0, fixed_request_micros_usd: 0,
    max_concurrency: 0, enabled: true, revision: 1, created_at: "", updated_at: "",
    capability_review: { state: "current" } as CapabilityReview,
    ...overrides,
  } as Deployment;
}

// A card with a route pointing at it and nothing else to say. The two neutral
// rungs are exercised on their own below.
const quiet = { priceMissing: false, priceUnknown: false, activeRouteCount: 1 } as const;
const drifted = { state: "drifted" } as CapabilityReview;
const reviewable = { state: "review_available" } as CapabilityReview;
const failedProbe = { state: "unhealthy", observed_at: "2026-08-25T02:00:00Z", error_class: "connect" } as DeploymentProbe;

describe("the deployment condition ladder", () => {
  it("reports the resting verdict with what the record measured", () => {
    const condition = deploymentCondition({
      deployment: record({ last_test_latency_millis: 1554, last_tested_at: "2026-08-25T02:00:00Z" }),
      testState: "success",
      ...quiet,
    });
    expect(condition).toMatchObject({
      severity: "ok", key: "testControl.success", params: { latency: 1554 },
      observedAt: "2026-08-25T02:00:00Z", suppressed: 0,
    });
  });

  // A verdict that says "passed" for a deployment the router has already
  // dropped is the console asserting something false. Ranking is the whole
  // point of the ladder, so each rung is checked against the one below it.
  it("lets what stops traffic outrank what merely reports on it", () => {
    const probeOverTest = deploymentCondition({
      deployment: record({ probe: failedProbe, last_test_latency_millis: 1554 }),
      testState: "success",
      ...quiet,
    });
    expect(probeOverTest).toMatchObject({ severity: "blocked", key: "deployments.probeUnhealthyShort", suppressed: 0 });

    const driftOverProbe = deploymentCondition({
      deployment: record({ capability_review: drifted, probe: failedProbe }),
      testState: "success",
      ...quiet,
    });
    expect(driftOverProbe).toMatchObject({ key: "deployments.capabilitiesUnsupported", suppressed: 1 });

    const quarantineOverFailure = deploymentCondition({
      deployment: record({ pricing_quarantined: true }),
      testState: "failure",
      ...quiet,
    });
    expect(quarantineOverFailure).toMatchObject({ key: "deployments.pricingQuarantinedShort", suppressed: 1 });
  });

  it("counts what it outranks rather than dropping it", () => {
    const condition = deploymentCondition({
      deployment: record({ capability_review: drifted, probe: failedProbe, pricing_quarantined: true }),
      testState: "stale",
      priceMissing: true,
      priceUnknown: false,
      activeRouteCount: 1,
    });
    // Drift shown; probe, quarantine, missing price and the stale test counted.
    expect(condition).toMatchObject({ key: "deployments.capabilitiesUnsupported", suppressed: 4 });
  });

  // A resting state is not a condition: a card cannot be "also fine", so the
  // count must never include the verdict it replaced.
  it("counts nothing when the deployment is merely untested", () => {
    expect(deploymentCondition({ deployment: record(), testState: "idle", ...quiet }))
      .toMatchObject({ severity: "quiet", key: "testControl.idle", suppressed: 0 });
  });

  // The operator's own click resolving beats anything standing: a verdict that
  // arrives under a line about something else reads as no answer at all.
  it("shows a test in flight above everything, and still counts the rest", () => {
    const condition = deploymentCondition({
      deployment: record({ capability_review: drifted }),
      testState: "running",
      priceMissing: true,
      priceUnknown: false,
      activeRouteCount: 1,
    });
    expect(condition).toMatchObject({ severity: "running", key: "testControl.running", suppressed: 2 });
  });

  // Two different failures with two different remedies. Reporting "no price"
  // when the read itself failed sends the operator to create a version that may
  // already exist.
  it("separates a missing price from a price it could not read", () => {
    expect(deploymentCondition({ ...quiet, deployment: record(), testState: "success", priceUnknown: true }))
      .toMatchObject({ severity: "warn", key: "deployments.priceUnavailable", action: "retryPrice" });
    // Knowing a price is missing is a blocker: the gateway refuses every
    // request under the default cost-governance policy. Not knowing is not.
    expect(deploymentCondition({ ...quiet, deployment: record(), testState: "success", priceMissing: true }))
      .toMatchObject({ severity: "blocked", key: "deployments.priceNotConfiguredShort", action: "price" });
  });

  // The ladder claimed to rank by how soon each condition stops traffic and did
  // not hold it. These two pairs are the places it disagreed with the gateway.
  it("ranks by what actually stops traffic, not by what reports on it", () => {
    // No price is refused on every request under the default policy; a failed
    // manual test is read by nothing in the router.
    expect(deploymentCondition({
      ...quiet, deployment: record(), testState: "failure", priceMissing: true,
    })).toMatchObject({ severity: "blocked", key: "deployments.priceNotConfiguredShort", suppressed: 1 });
    expect(deploymentCondition({ ...quiet, deployment: record(), testState: "failure" }))
      .toMatchObject({ severity: "warn", key: "testControl.failure", suppressed: 0 });
  });

  // Both stop traffic as completely as anything above them, and neither was on
  // the ladder in any position. Neither is a fault: the operator chose them, so
  // they are reported without the vocabulary of failure.
  it("names the two states the operator chose without calling them faults", () => {
    expect(deploymentCondition({ ...quiet, deployment: record({ enabled: false }), testState: "success" }))
      .toMatchObject({ severity: "neutral", key: "common.disabled", suppressed: 0 });
    expect(deploymentCondition({ ...quiet, deployment: record(), testState: "success", activeRouteCount: 0 }))
      .toMatchObject({ severity: "neutral", key: "deployments.noActiveRoutes", action: "drawer", suppressed: 0 });
    // And they stay below anything that is wrong.
    expect(deploymentCondition({
      ...quiet, deployment: record({ enabled: false, probe: failedProbe }), testState: "success", activeRouteCount: 0,
    })).toMatchObject({ severity: "blocked", key: "deployments.probeUnhealthyShort", suppressed: 2 });
  });

  // An opportunity blocks nothing, so it must not outrank a stale verdict or
  // read as a fault when it is the only thing to say.
  it("ranks a reviewable catalogue below a stale test and reports it quietly", () => {
    expect(deploymentCondition({ deployment: record({ capability_review: reviewable }), testState: "stale", ...quiet }))
      .toMatchObject({ key: "testControl.stale", suppressed: 1 });
    expect(deploymentCondition({ deployment: record({ capability_review: reviewable }), testState: "success", ...quiet }))
      .toMatchObject({ severity: "quiet", key: "deployments.capabilitiesToReview", action: "drawer", suppressed: 0 });
  });
});

// The filter and the card have to answer the same question, because an
// operator reaches for "needs attention" during exactly the incident the card
// was ranked for. The definition it replaced — no passing test on the current
// revision — agreed with the card on none of the conditions the card ranks
// highest.
describe("what the list calls needing attention", () => {
  it("catches every condition that stops the deployment carrying traffic", () => {
    expect(deploymentNeedsAttention(record({ capability_review: drifted }))).toBe(true);
    expect(deploymentNeedsAttention(record({ probe: failedProbe }))).toBe(true);
    expect(deploymentNeedsAttention(record({ pricing_quarantined: true }))).toBe(true);
    expect(deploymentNeedsAttention(record({
      last_test_status: "unhealthy", last_test_revision: 1, revision: 1,
    }))).toBe(true);
    // Tested healthy, then edited: the record's verdict describes a revision
    // that is no longer the one deployed.
    expect(deploymentNeedsAttention(record({
      last_test_status: "healthy", last_test_revision: 1, revision: 2,
    }))).toBe(true);
  });

  it("leaves alone a deployment with nothing to act on", () => {
    expect(deploymentNeedsAttention(record({
      last_test_status: "healthy", last_test_revision: 1, revision: 1,
    }))).toBe(false);
    // Never tested is a draft, not a fault: the server refuses to enable it, so
    // it is not serving and not failing. The card calls this resting too, and
    // the previous filter definition put it top of the incident list.
    expect(deploymentNeedsAttention(record())).toBe(false);
    // An opportunity costs nothing to ignore.
    expect(deploymentNeedsAttention(record({ capability_review: reviewable }))).toBe(false);
    // Disabled is a state, not a fault. Putting it here would fill the incident
    // list with every draft on the page.
    expect(deploymentNeedsAttention(record({
      enabled: false, last_test_status: "healthy", last_test_revision: 1, revision: 1,
    }))).toBe(false);
  });

  it("reads the record the way the card does", () => {
    expect(recordedTestState(record())).toBe("idle");
    expect(recordedTestState(record({ last_test_status: "healthy", last_test_revision: 1, revision: 1 }))).toBe("success");
    expect(recordedTestState(record({ last_test_status: "unhealthy", last_test_revision: 1, revision: 1 }))).toBe("failure");
    expect(recordedTestState(record({ last_test_status: "healthy", last_test_revision: 1, revision: 2 }))).toBe("stale");
    // A failure on a revision that has since moved is not a current failure;
    // the card shows it as untested, and so does this.
    expect(recordedTestState(record({ last_test_status: "unhealthy", last_test_revision: 1, revision: 2 }))).toBe("idle");
  });
});
