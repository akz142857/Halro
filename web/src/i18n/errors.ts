import type { TFunction } from "i18next";
import { ApiError } from "../api";

export function localizedError(t: TFunction, error: unknown) {
  if (!(error instanceof ApiError)) return t("errors.network");
  const codeMessages: Record<string, string> = {
    deployment_price_unavailable: "errors.deploymentPriceUnavailable",
    price_effective_from_conflict: "errors.priceEffectiveConflict",
    price_timeline_conflict: "errors.priceTimelineConflict",
    price_effective_from_required: "errors.priceEffectiveRequired",
    route_listing_incomplete: "errors.routeListingIncomplete",
    capability_detection_stale: "errors.capabilityDetectionStale",
    capability_detection_changed: "errors.capabilityDetectionChanged",
    capability_detection_target_mismatch: "errors.capabilityDetectionTargetMismatch",
    capabilities_exceed_detection: "errors.capabilitiesExceedDetection",
    capability_detection_cooldown: "errors.capabilityDetectionCooldown",
    capability_detection_rate_limited: "errors.capabilityDetectionRateLimited",
    ambiguous_capability_binding: "errors.ambiguousCapabilityBinding",
    no_detectable_binding: "errors.noDetectableBinding",
    // The server names the deployment in the detail, so this stays a headline and
    // the detail is deliberately left visible underneath it.
    binding_referenced_by_deployment: "errors.bindingReferencedByDeployment",
    route_referenced_by_project: "errors.routeReferencedByProject",
    bedrock_project_id_invalid: "errors.bedrockProjectIDInvalid",
    idempotency_conflict: "errors.idempotencyConflict",
    provider_idempotency_replay: "errors.providerIdempotencyReplay",
    deployment_idempotency_replay: "errors.deploymentIdempotencyReplay",
    route_idempotency_replay: "errors.routeIdempotencyReplay",
    project_idempotency_replay: "errors.projectIdempotencyReplay",
    gateway_key_idempotency_replay: "errors.gatewayKeyIdempotencyReplay",
  };
  if (error.code && codeMessages[error.code]) return t(codeMessages[error.code]);
  if (error.status === 400 || error.status === 422) return t("errors.badRequest");
  if (error.status === 401) return t("errors.authentication");
  if (error.status === 403) return t("errors.forbidden");
  if (error.status === 404) return t("errors.notFound");
  if (error.status === 409 || error.status === 412 || error.status === 428) return t("errors.conflict");
  if (error.status === 429) return t("errors.rateLimited");
  return t("errors.server");
}

// Validation and conflict responses name the field that failed. The localized headline
// alone leaves the operator guessing between name, models, CIDR and policy bindings, so
// the server's reason is surfaced verbatim underneath it.
export function errorDetail(error: unknown) {
  if (!(error instanceof ApiError)) return "";
  // This is an actionable, localized workflow state rather than a malformed
  // field. Repeating the server's English sentinel under the translated
  // instruction makes the UI look like an internal validation failure.
  const localizedWorkflowCodes = [
    "ambiguous_capability_binding",
    "deployment_price_unavailable",
    "price_effective_from_conflict",
    "price_effective_from_required",
    "provider_idempotency_replay",
    "deployment_idempotency_replay",
    "route_idempotency_replay",
    "project_idempotency_replay",
    "gateway_key_idempotency_replay",
  ];
  if (localizedWorkflowCodes.includes(error.code)) return "";
  // A forwarded upstream reply is the whole point of the message; show it at any status.
  if (error.detail) return error.detail;
  const detailed = error.status === 400 || error.status === 409 || error.status === 422;
  if (!detailed) return "";
  const message = error.message.trim();
  return message.startsWith("request failed") ? "" : message;
}
