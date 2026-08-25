import { describe, expect, it } from "vitest";
import { deploymentCondition } from "./deploymentCondition";
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

const quiet = { priceMissing: false, priceUnknown: false } as const;
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
    });
    expect(condition).toMatchObject({ severity: "running", key: "testControl.running", suppressed: 2 });
  });

  // Two different failures with two different remedies. Reporting "no price"
  // when the read itself failed sends the operator to create a version that may
  // already exist.
  it("separates a missing price from a price it could not read", () => {
    expect(deploymentCondition({ deployment: record(), testState: "success", priceMissing: false, priceUnknown: true }))
      .toMatchObject({ severity: "warn", key: "deployments.priceUnavailable", action: "retryPrice" });
    expect(deploymentCondition({ deployment: record(), testState: "success", priceMissing: true, priceUnknown: false }))
      .toMatchObject({ severity: "warn", key: "deployments.priceNotConfiguredShort", action: "price" });
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
