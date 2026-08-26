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
export type ConditionSeverity = "blocked" | "warn" | "neutral" | "running" | "ok" | "quiet";

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
  /** Enabled routes pointing at this deployment. Zero means nothing can reach it. */
  activeRouteCount: number;
}

/**
 * Ranked worst first, by what the router and the gateway actually do — which is
 * not what this list used to say. It claimed that order and did not hold it:
 * a deployment with no effective price is refused on every request under the
 * default cost-governance policy, and sat at `warn`; a failed manual test does
 * not enter any routing decision at all, and sat at `blocked` above it. Two
 * conditions that stop traffic outright — disabled, and no enabled route
 * pointing here — were not on the list in any position.
 *
 * `neutral` is for the two the operator chose. They stop traffic as completely
 * as anything above them, so they belong on the ladder; colouring them as
 * faults would report a deliberate state as a problem.
 *
 * Everything above `neutral` is a reason an operator has something to do;
 * `success` and `idle` are the resting states and never count towards the
 * suppressed total, because a card cannot be "also fine".
 */
function ladder({ deployment, testState, priceMissing, priceUnknown, activeRouteCount }: ConditionInputs): DeploymentCondition[] {
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
  // No effective price version is refused by the gateway on every request
  // unless cost governance is explicitly waived, and the default configuration
  // does not waive it. Knowing a price is missing is therefore a blocker;
  // failing to find out is not the same thing, and stays a warning with a retry.
  if (!priceUnknown && priceMissing) {
    found.push({ severity: "blocked", key: "deployments.priceNotConfiguredShort", suppressed: 0, action: "price" });
  }
  if (priceUnknown) {
    found.push({ severity: "warn", key: "deployments.priceUnavailable", suppressed: 0, action: "retryPrice" });
  }
  // A manual test is a report, not a gate: nothing in the router reads
  // `last_test_status`. It ranks with the other things worth acting on rather
  // than with the things that stop traffic.
  if (testState === "failure") {
    found.push({ severity: "warn", key: "testControl.failure", observedAt: deployment.last_tested_at, suppressed: 0 });
  }
  if (testState === "stale") {
    found.push({ severity: "warn", key: "testControl.stale", observedAt: deployment.last_tested_at, suppressed: 0 });
  }
  // The two the operator chose. Zero traffic either way, and neither is a fault.
  if (!deployment.enabled) {
    found.push({ severity: "neutral", key: "common.disabled", suppressed: 0 });
  }
  if (activeRouteCount === 0) {
    found.push({ severity: "neutral", key: "deployments.noActiveRoutes", suppressed: 0, action: "drawer" });
  }
  // An opportunity, not a blocker: the deployment keeps serving what it already
  // declared until someone reviews what the catalogue now offers.
  if (review && review.state === "review_available") {
    found.push({ severity: "quiet", key: "deployments.capabilitiesToReview", suppressed: 0, action: "drawer" });
  }
  return found;
}

/**
 * The test verdict a stored record supports on its own.
 *
 * The card refines this with a test the operator started in this session — one
 * still in flight, or one that failed before it reached the store, which leaves
 * `last_test_status` describing an older run. The list has neither, and needs
 * the same vocabulary to rank a record it is only filtering.
 */
export function recordedTestState(deployment: Deployment): DeploymentTestState {
  const current = deployment.last_test_revision === deployment.revision;
  if (current && deployment.last_test_status === "healthy") return "success";
  if (current && deployment.last_test_status === "unhealthy") return "failure";
  return deployment.last_test_status === "healthy" ? "stale" : "idle";
}

/**
 * Whether a deployment is one the operator has something to do about.
 *
 * This is what the list's "needs attention" filter means, and it reads the same
 * ladder the card reads, because the two answering differently is how the
 * filter came to exclude every condition the card ranks highest: it was written
 * as "no passing test on the current revision", which admits a deployment that
 * was merely never tested and excludes one that is drifted, failing its probe
 * and quarantined at once.
 *
 * The two price conditions are the exception, and deliberately: price is read
 * per deployment, by the card, and the list has no such read. A deployment
 * whose only problem is a missing price is therefore not caught here — the card
 * still says so, and the filter says less than the card rather than guessing.
 */
export function deploymentNeedsAttention(deployment: Deployment): boolean {
  return ladder({
    deployment,
    testState: recordedTestState(deployment),
    priceMissing: false,
    priceUnknown: false,
    // Zero would put every disabled deployment on the list; the filter asks
    // what is wrong, and having no route pointing at a deployment is a state
    // rather than a fault. The card still says so.
    activeRouteCount: 1,
  }).some((condition) => condition.severity === "blocked" || condition.severity === "warn");
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
