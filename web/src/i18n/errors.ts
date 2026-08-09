import type { TFunction } from "i18next";
import { ApiError } from "../api";

export function localizedError(t: TFunction, error: unknown) {
  if (!(error instanceof ApiError)) return t("errors.network");
  const codeMessages: Record<string, string> = {
    deployment_price_unavailable: "errors.deploymentPriceUnavailable",
    capability_detection_stale: "errors.capabilityDetectionStale",
    capability_detection_changed: "errors.capabilityDetectionChanged",
    capability_detection_target_mismatch: "errors.capabilityDetectionTargetMismatch",
    capabilities_exceed_detection: "errors.capabilitiesExceedDetection",
    capability_detection_cooldown: "errors.capabilityDetectionCooldown",
    capability_detection_rate_limited: "errors.capabilityDetectionRateLimited",
    no_detectable_binding: "errors.noDetectableBinding",
    idempotency_conflict: "errors.idempotencyConflict",
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
  // A forwarded upstream reply is the whole point of the message; show it at any status.
  if (error.detail) return error.detail;
  const detailed = error.status === 400 || error.status === 409 || error.status === 422;
  if (!detailed) return "";
  const message = error.message.trim();
  return message.startsWith("request failed") ? "" : message;
}
