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

// A capability set the connection cannot carry. The server names the
// capabilities by key, which is how it has to travel — the reader's language is
// this bundle's business — so the sentence is assembled from the same
// `capabilities.*` copy the checkboxes use rather than echoing
// `provider_executed_tools` at an operator.
const capabilityRefusals: Record<string, string> = {
  capabilities_unservable: "errors.capabilitiesUnservable",
  capabilities_ambiguous: "errors.capabilitiesAmbiguous",
  capabilities_limit_unavailable: "errors.capabilitiesLimitUnavailable",
  capabilities_limit_too_large: "errors.capabilitiesLimitTooLarge",
};

function capabilityRefusal(t: TFunction, error: ApiError) {
  const message = capabilityRefusals[error.code];
  if (!message) return "";
  const payload = error.payload as Record<string, unknown> | undefined;
  const names = typeof payload?.capabilities === "string" ? payload.capabilities.split(",").filter(Boolean) : [];
  if (!names.length) return "";
  const listed = names.map((name) => t(`capabilities.${name}`)).join(t("common.listSeparator"));
  return t(message, { capabilities: listed });
}

export function localizedError(t: TFunction, error: unknown) {
  if (!(error instanceof ApiError)) return t("errors.network");
  const mismatch = credentialMismatchValues(error);
  if (mismatch) {
    return t(mismatch.message, { credential: mismatch.credential, provider: mismatch.provider, name: mismatch.name });
  }
  const capabilities = capabilityRefusal(t, error);
  if (capabilities) return capabilities;
  const codeMessages: Record<string, string> = {
    run_governance_active_runs: "errors.runGovernanceActiveRuns",
    governance_unavailable: "errors.governanceUnavailable",
    run_governance_unavailable: "errors.runGovernanceUnavailable",
    governance_summary_unavailable: "errors.governanceSummaryUnavailable",
    governance_export_inconsistent: "errors.governanceExportInconsistent",
    governance_summary_overflow: "errors.governanceSummaryOverflow",
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
    // A 409 whose generic headline sends the operator to refresh, which cannot
    // help: nothing changed underneath them, and the edit stays refused until
    // the routes are dealt with.
    capability_expansion_requires_revalidation: "errors.capabilityExpansionRequiresRevalidation",
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
    // The refusals the Admin API states itself. Each one arrives as a code with
    // the server's English sentence beside it; without the code the console had
    // nothing to translate and printed that sentence to every reader, in every
    // language. Adding a refusal upstream without a code here is not a break —
    // it falls back to the generic headline plus the server's reason, which is
    // what every one of these did before.
    invalid_request: "errors.badRequest",
    invalid_console_window: "errors.invalidConsoleWindow",
    console_window_exceeds_retention: "errors.consoleWindowExceedsRetention",
    console_window_trim_unacknowledged: "errors.consoleWindowTrimUnacknowledged",
    alert_id_required: "errors.alertIDRequired",
    deployment_provider_unavailable: "errors.deploymentProviderUnavailable",
    deployment_provider_adapter_unavailable: "errors.deploymentProviderAdapterUnavailable",
    provider_test_unsupported: "errors.providerTestUnsupported",
    provider_disabled: "errors.providerDisabled",
    invocation_target_invalid: "errors.invocationTargetInvalid",
    provider_credential_unavailable: "errors.providerCredentialUnavailable",
    capability_detection_target_invalid: "errors.capabilityDetectionTargetInvalid",
    provider_type_locked_by_deployments: "errors.providerTypeLockedByDeployments",
    provider_binding_unavailable: "errors.providerBindingUnavailable",
    route_disabled: "errors.routeDisabled",
    route_deployment_unavailable: "errors.routeDeploymentUnavailable",
    route_provider_unavailable: "errors.routeProviderUnavailable",
    route_provider_adapter_unavailable: "errors.routeProviderAdapterUnavailable",
    preview_values_negative: "errors.previewValuesNegative",
    admin_role_invalid: "errors.adminRoleInvalid",
    idempotency_key_required: "errors.idempotencyKeyRequired",
    invalid_preferences: "errors.invalidPreferences",
    invalid_locale_preference: "errors.invalidLocalePreference",
    invalid_appearance_preference: "errors.invalidAppearancePreference",
    mfa_setup_required: "errors.mfaSetupRequired",
    reauth_rate_limited: "errors.reauthRateLimited",
    recent_reauth_required: "errors.recentReauthRequired",
    model_capability_changed: "errors.modelCapabilityChanged",
    model_capabilities_unknown: "errors.modelCapabilitiesUnknown",
    model_capabilities_exceed_catalog: "errors.modelCapabilitiesExceedCatalog",
    model_not_served_by_profile: "errors.modelNotServedByProfile",
    operation_bindings_unavailable: "errors.operationBindingsUnavailable",
    resolution_changed: "errors.resolutionChanged",
    // The console already words this one on every disabled control; a second
    // sentence for the server enforcing the same rule would be the same fact
    // maintained twice.
    read_only_role: "navigation.readOnlyAction",
    cannot_delete_self: "adminUsers.cannotDeleteSelf",
    last_administrator: "adminUsers.cannotDeleteLastAdministrator",
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
    "invalid_request",
    "alert_id_required",
    "deployment_provider_unavailable",
    "deployment_provider_adapter_unavailable",
    "provider_test_unsupported",
    "provider_disabled",
    "invocation_target_invalid",
    "provider_credential_unavailable",
    "capability_detection_target_invalid",
    "provider_type_locked_by_deployments",
    "provider_binding_unavailable",
    "route_disabled",
    "route_deployment_unavailable",
    "route_provider_unavailable",
    "route_provider_adapter_unavailable",
    "preview_values_negative",
    "admin_role_invalid",
    "idempotency_key_required",
    "invalid_preferences",
    "invalid_locale_preference",
    "invalid_appearance_preference",
    "mfa_setup_required",
    "reauth_rate_limited",
    "recent_reauth_required",
    "model_capability_changed",
    "model_capabilities_unknown",
    "model_capabilities_exceed_catalog",
    "operation_bindings_unavailable",
    "resolution_changed",
    "read_only_role",
    "cannot_delete_self",
    "last_administrator",
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
  // Same reasoning: the translated sentence already names the capabilities, and
  // the server's original names them in implementation spelling.
  if (capabilityRefusals[error.code]) return "";
  // A forwarded upstream reply is the whole point of the message; show it at any status.
  if (error.detail) return error.detail;
  const detailed = error.status === 400 || error.status === 409 || error.status === 422;
  if (!detailed) return "";
  const message = error.message.trim();
  return message.startsWith("request failed") ? "" : message;
}
