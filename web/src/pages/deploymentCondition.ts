import type { Deployment } from "../types";

/**
 * What a deployment card says about itself, in one line.
 *
 * A card used to carry five independent signals — a dot beside the name, the
 * enabled word, a drift chip, a probe chip, a price cell and a test verdict —
 * each rendered where it happened to fit. Four of them can be true at once,
 * two of them used opposite colour vocabularies, and none of them was the
 * answer to the only question a list screen is scanned for: is this one all
 * right, and if not, what stops it.
 *
 * So the conditions are ranked here instead, by how soon each one stops the
 * deployment carrying traffic, and the card shows the highest with a count of
 * what it outranks. The ladder is a pure function of the record and the
 * client-side test state: no formatting, no translation, no i18n keys resolved
 * — the caller owns the words, this owns the priority.
 */
export type ConditionSeverity = "blocked" | "warn" | "running" | "ok" | "quiet";

/** What the card can do about the condition it is showing. */
export type ConditionAction = "drawer" | "price" | "restorePricing" | "retryPrice";

export interface DeploymentCondition {
  severity: ConditionSeverity;
  /** i18n key for the line's text. */
  key: string;
  /** Interpolation values for `key`, when it takes any. */
  params?: Record<string, string | number>;
  /** The instant the condition was observed, when the record carries one. */
  observedAt?: string;
  /** Conditions ranked below this one that are also true right now. */
  suppressed: number;
  action?: ConditionAction;
}

export type DeploymentTestState = "idle" | "running" | "success" | "failure" | "stale";

export interface ConditionInputs {
  deployment: Deployment;
  testState: DeploymentTestState;
  /** False while the price read is still in flight — nothing is missing yet. */
  priceMissing: boolean;
  /** The price read itself failed, so price readiness is unknown. */
  priceUnknown: boolean;
}

/**
 * Ranked worst first. Everything above `success` is a reason an operator has
 * something to do; `success` and `idle` are the resting states and never count
 * towards the suppressed total, because a card cannot be "also fine".
 */
function ladder({ deployment, testState, priceMissing, priceUnknown }: ConditionInputs): DeploymentCondition[] {
  const review = deployment.capability_review;
  const probe = deployment.probe;
  const found: DeploymentCondition[] = [];
  // Drift: the router drops the deployment from its candidates whatever the
  // enabled flag says, and the only way back is a capability review.
  if (review && review.state === "drifted") {
    found.push({ severity: "blocked", key: "deployments.capabilitiesUnsupported", suppressed: 0, action: "drawer" });
  }
  // The probe is what actually decides routing. The manual test below it is an
  // operator-triggered result that can be days old, so it never outranks this.
  if (probe?.state === "unhealthy") {
    found.push({ severity: "blocked", key: "deployments.probeUnhealthyShort", observedAt: probe.observed_at, suppressed: 0, action: "drawer" });
  }
  // A quarantined price fails price selection, and a request whose price
  // cannot be selected is refused — enabled or not.
  if (deployment.pricing_quarantined) {
    found.push({ severity: "blocked", key: "deployments.pricingQuarantinedShort", suppressed: 0, action: "restorePricing" });
  }
  if (testState === "failure") {
    found.push({ severity: "blocked", key: "testControl.failure", observedAt: deployment.last_tested_at, suppressed: 0 });
  }
  // Not knowing whether a price exists is not the same as knowing one is
  // missing: the first is the console's own failure and offers a retry, the
  // second is the operator's and offers the price form.
  if (priceUnknown) {
    found.push({ severity: "warn", key: "deployments.priceUnavailable", suppressed: 0, action: "retryPrice" });
  } else if (priceMissing) {
    found.push({ severity: "warn", key: "deployments.priceNotConfiguredShort", suppressed: 0, action: "price" });
  }
  if (testState === "stale") {
    found.push({ severity: "warn", key: "testControl.stale", observedAt: deployment.last_tested_at, suppressed: 0 });
  }
  // An opportunity, not a blocker: the deployment keeps serving what it already
  // declared until someone reviews what the catalogue now offers.
  if (review && review.state === "review_available") {
    found.push({ severity: "quiet", key: "deployments.capabilitiesToReview", suppressed: 0, action: "drawer" });
  }
  return found;
}

/**
 * The one line the card shows, and how much it outranks.
 *
 * A test in flight wins outright: it is the operator's own click resolving, and
 * a verdict that arrives under a line about something else reads as a failure
 * to respond.
 */
export function deploymentCondition(inputs: ConditionInputs): DeploymentCondition {
  const { deployment, testState } = inputs;
  const found = ladder(inputs);
  if (testState === "running") {
    return { severity: "running", key: "testControl.running", suppressed: found.length };
  }
  const [top, ...rest] = found;
  if (top) return { ...top, suppressed: rest.length };
  if (testState === "success") {
    return deployment.last_test_latency_millis === undefined
      ? { severity: "ok", key: "testControl.successPlain", observedAt: deployment.last_tested_at, suppressed: 0 }
      : {
        severity: "ok",
        key: "testControl.success",
        params: { latency: deployment.last_test_latency_millis },
        observedAt: deployment.last_tested_at,
        suppressed: 0,
      };
  }
  return { severity: "quiet", key: "testControl.idle", suppressed: 0 };
}
