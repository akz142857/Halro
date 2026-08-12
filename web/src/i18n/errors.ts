import type { TFunction } from "i18next";
import { ApiError } from "../api";

// A credential is sealed against one provider type and one base URL, and the
// server sends back both sides of whichever comparison failed. Rendering those
// values into the translated sentence is the difference between "the request is
// invalid" and a sentence naming the two values the operator has to reconcile.
const credentialMismatches: Record<string, { message: string; credential: string; provider: string; name?: string }> = {
  credential_base_url_mismatch: {
    message: "errors.credentialBaseURLMismatch",
    credential: "credential_base_url",
    provider: "provider_base_url",
  },
  credential_type_mismatch: {
    message: "errors.credentialTypeMismatch",
    credential: "credential_provider_type",
    provider: "provider_type",
  },
  credential_surface_mismatch: {
    message: "errors.credentialSurfaceMismatch",
    credential: "credential_access_surface",
    provider: "provider_access_surface",
  },
  // The mirror image, hit from the credential form: rotating the credential to
  // a different endpoint would strand a provider that still uses it there.
  credential_endpoint_in_use: {
    message: "errors.credentialEndpointInUse",
    credential: "credential_base_url",
    provider: "provider_base_url",
    name: "provider_name",
  },
  // Same rotation, other axis: the provider still uses this credential as a
  // different provider type. Reporting that as an endpoint conflict would print
  // the same URL twice and send the operator to fix a base URL that is correct.
  credential_type_in_use: {
    message: "errors.credentialTypeInUse",
    credential: "credential_provider_type",
    provider: "provider_provider_type",
    name: "provider_name",
  },
};

// Both values or neither: a half-filled sentence would leave a raw
// `{{provider}}` on screen, which is worse than the generic fallback.
function credentialMismatchValues(error: ApiError) {
  const mismatch = credentialMismatches[error.code];
  if (!mismatch) return undefined;
  const payload = error.payload as Record<string, unknown> | undefined;
  const credential = payload?.[mismatch.credential];
  const provider = payload?.[mismatch.provider];
  if (typeof credential !== "string" || typeof provider !== "string" || !credential || !provider) return undefined;
  const name = mismatch.name ? payload?.[mismatch.name] : "";
  if (mismatch.name && (typeof name !== "string" || !name)) return undefined;
  return { message: mismatch.message, credential, provider, name: name as string };
}

export function localizedError(t: TFunction, error: unknown) {
  if (!(error instanceof ApiError)) return t("errors.network");
  const mismatch = credentialMismatchValues(error);
  if (mismatch) {
    return t(mismatch.message, { credential: mismatch.credential, provider: mismatch.provider, name: mismatch.name });
  }
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
  // The translated sentence already names both values the server compared, so
  // the English original underneath it says the same thing twice — and says it
  // in the wrong language. It stays visible when the values did not arrive and
  // the message fell back to the generic one.
  if (credentialMismatchValues(error)) return "";
  // A forwarded upstream reply is the whole point of the message; show it at any status.
  if (error.detail) return error.detail;
  const detailed = error.status === 400 || error.status === 409 || error.status === 422;
  if (!detailed) return "";
  const message = error.message.trim();
  return message.startsWith("request failed") ? "" : message;
}
