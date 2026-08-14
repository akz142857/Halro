export interface Page<T> {
  items: T[];
  next_cursor: string;
}

/**
 * Which day a per-day figure covers, decided by the server.
 *
 * The accounting time zone lives in the server's configuration and sets when a
 * daily budget resets, so the browser cannot derive these bounds — it must
 * render against them.
 */
export interface TimeContext {
  accounting_timezone: string;
  timezone_version: number;
  period_id: string;
  period_start: string;
  period_end: string;
  /** A scheduled change that has not taken effect. Absent when none is due. */
  pending_timezone?: string;
  pending_effective_at?: string;
  generated_at: string;
}

/**
 * The governed accounting timezone: the setting that decides when a day ends,
 * and so when a daily budget resets and which day a call is billed to.
 *
 * A change never applies immediately. It is scheduled for the end of the period
 * in progress, because moving the boundary of a day that is already
 * accumulating would redefine what budgets already enforced against it meant.
 */
export interface AccountingSettings {
  timezone: string;
  timezone_version: number;
  pending_timezone?: string;
  pending_effective_at?: string;
  current_period: { period_id: string; period_start: string; period_end: string };
  /** What config.yaml says, and whether it is still what takes effect. */
  config_file_timezone: string;
  config_file_in_effect: boolean;
  tzdata?: { source: string; version: string; fingerprint: string };
  /**
   * What a proposed change would do, returned only when asked for with
   * `preview_timezone`. The switch mints a period in the new zone that can
   * begin before the switch itself, and that period is a fresh balance — so the
   * daily budget starts over `next_reset_in_hours` after the change, which can
   * be as little as an hour.
   */
  switch_preview?: {
    timezone: string;
    effective_at: string;
    first_period: { period_id: string; period_start: string; period_end: string };
    next_reset_at: string;
    next_reset_in_hours: number;
  };
  updated_at: string;
  revision: number;
}

// Two roles, no third. read_only is GET-only server-side; the console reads
// this only to stop offering what the server would refuse, never to decide
// anything on its own.
export type AdminRole = "administrator" | "read_only";

export interface AdminUser {
  username: string;
  role: AdminRole;
}

export interface Session {
  username: string;
  role: AdminRole;
  locale: LocalePreference;
  appearance: Appearance;
  csrf_token: string;
  absolute_expires_at: string;
  idle_expires_at: string;
  mfa_setup_required?: boolean;
}

export interface MFAChallenge { mfa_required: true; challenge_token: string; expires_at: string }
export interface MFAAuthenticator { id: string; name: string; type: "totp"; created_at: string; last_used_at?: string; revision: number }
export interface MFAStatus { enabled: boolean; policy: "optional" | "required"; authenticators: MFAAuthenticator[]; recovery_codes_remaining?: number }
export interface MFAEnrollment { id: string; name: string; secret: string; otpauth_uri: string; expires_at: string; revision: number }

export interface SetupStatus {
  instance_initialized: boolean;
  setup_required: boolean;
  token_required: boolean;
}

export interface MasterKeyCustody {
  mode: "file" | "key_slots";
  local_custody_ready: boolean;
  custody_state: "healthy" | "degraded";
  production_admission: "not_applicable" | "external_evidence_required";
  rotation_incomplete: boolean;
  lifecycle_operation: "none" | "kek_rewrap" | "dek_rotate";
  pending_slots: number;
  retiring_slots: number;
  recovery_verification_status: "not_applicable" | "missing" | "current" | "expired" | "invalid_future";
  recovery_verified_at?: string;
  degraded_reasons: string[];
  slots: Array<{
    purpose: "primary" | "recovery";
    state: "pending" | "active" | "retiring" | "revoked";
    provider: string;
    verified_at?: string;
  }>;
  lifecycle_runbook_url?: string;
  recovery_runbook_url?: string;
}

export interface Bucket {
  hour: string;
  requests: number;
  request_errors: number;
  request_latency_samples: number;
  request_latency_p50_millis: number;
  request_latency_p95_millis: number;
  attempts: number;
  input_tokens: number;
  output_tokens: number;
  estimated_input_tokens?: number;
  estimated_output_tokens?: number;
  cost_micros_usd: number;
  estimated_cost_micros_usd?: number;
  unknown_attempts: number;
  errors: number;
  latency_millis: number;
}

export interface UsageBreakdown {
  key: string;
  calls: number;
  input_tokens: number;
  output_tokens: number;
  cost_micros_usd: number;
  estimated_cost_micros_usd?: number;
  errors: number;
}

export interface UsageAnomaly {
  request_id: string;
  attempt_id: string;
  completed_at: string;
  project_id: string;
  deployment_id?: string;
  provider_id?: string;
  requested_model?: string;
  provider_model?: string;
  status: string;
  error_class?: string;
  http_status?: number;
  retry_count: number;
  fallback_count: number;
}

export interface GovernancePressureItem {
  scope: "project" | "provider" | "deployment";
  id: string;
  name: string;
  current: number;
  limit: number;
  utilization: number;
  committed_micros_usd?: number;
  reserved_micros_usd?: number;
}

export interface Dashboard {
  first_value_reached: boolean;
  usage: {
    today: Bucket;
    hourly: Bucket[];
    active_requests: number;
    watermark_sequence: number;
    breakdowns: Record<"project" | "provider" | "requested_model" | "provider_model", Record<"calls" | "cost" | "errors", UsageBreakdown[]>>;
    recent_anomalies: UsageAnomaly[];
  };
  governance: {
    policy_rejections: {
      rpm: number;
      tpm: number;
      project_concurrency: number;
      provider_concurrency: number;
      deployment_concurrency: number;
      budget: number;
      token_guard: number;
      total: number;
    };
    budget: { at_risk: number; items: GovernancePressureItem[] };
    capacity: { at_risk: number; items: GovernancePressureItem[] };
    pricing?: { quarantined: number; unknown: number };
  };
  resource_labels: Record<string, string>;
  accounting_status: number;
  runtime: {
    accepting_traffic: boolean;
    draining: boolean;
    activation: ActivationStatus;
  };
  wal: WALStats;
  alerts: AlertStats;
  time_context: TimeContext;
}

export type OnboardingState = "configuring" | "ready_to_verify" | "verify_failed" | "first_value_reached";
export type OnboardingGoalState = "complete" | "current" | "blocked" | "error";

export interface OnboardingGoal {
  key: "connect_provider" | "publish_model" | "grant_access" | "verify_request";
  state: OnboardingGoalState;
  detail_code: string;
  action_href: string;
}

export interface OnboardingVerification {
  outcome: string;
  request_id: string;
  http_status?: number;
  error_class?: string;
  requested_model?: string;
  completed_at: string;
}

export interface OnboardingReadiness {
  version: number;
  state: OnboardingState;
  completed_goals: number;
  total_goals: number;
  goals: OnboardingGoal[];
  last_verification?: OnboardingVerification;
  evaluated_at: string;
}

export interface WALStats {
  batches: number;
  records: number;
  errors: number;
  syncs: number;
  sync_seconds: number;
  queue_depth: number;
  queue_capacity: number;
}

export interface AlertStats {
  Accepted: number;
  Delivered: number;
  Failed: number;
  Dropped: number;
  Queued: number;
  QueueCapacity: number;
  Endpoints: number;
  UnhealthyEndpoints: number;
  UnknownEndpoints: number;
  LastDeliveredAt?: string;
  LastFailedAt?: string;
}

export interface Project {
  id: string;
  name: string;
  enabled: boolean;
  allowed_models: string[];
  rpm: number;
  tpm: number;
  max_concurrency: number;
  daily_budget_micros_usd: number;
  max_input_tokens: number;
  max_output_tokens: number;
  max_request_bytes: number;
  max_stream_duration: number;
  allowed_cidrs: string[] | null;
  redaction_policy_id: string;
  token_guard_policy_id: string;
  revision: number;
  created_at: string;
  updated_at: string;
}

export interface GatewayKey {
  id: string;
  project_id: string;
  name: string;
  enabled: boolean;
  expires_at?: string;
  created_at: string;
  revision: number;
}

export interface CreatedGatewayKey {
  key: string;
  metadata: GatewayKey;
}

export interface Credential {
  id: string;
  name: string;
  type: ProviderType;
  access_surface: AccessSurface;
  scheme: CredentialScheme;
  bound_base_url: string;
  secret_configured: boolean;
  key_version: number;
  expires_at?: string;
  revision: number;
}

export type AccessSurface =
  | "openai-api"
  | "azure-openai"
  | "deepseek-api"
  | "openai-compatible"
  | "gemini-generate-content"
  | "anthropic-api"
  | "bedrock-runtime"
  | "bedrock-agent-runtime"
  | "bedrock-mantle";

export type CredentialScheme =
  | "bearer.static"
  | "anthropic.x-api-key"
  | "azure.api-key"
  | "google.api-key"
  | "aws.sigv4.explicit-session"
  | "aws.bedrock.api-key";

export type CapabilityEvidence = "verified" | "declared" | "unsupported";
export type CapabilityEvidenceSet = Record<string, CapabilityEvidence>;

export type ProviderType =
  | "openai"
  | "azure_openai"
  | "anthropic"
  | "deepseek"
  | "gemini"
  | "bedrock"
  | "openai_compatible";

export interface ProviderCapabilities {
  chat: boolean;
  streaming: boolean;
  embeddings: boolean;
  moderations: boolean;
  images: boolean;
  transcriptions: boolean;
  speech: boolean;
  files: boolean;
  batches: boolean;
  rerank: boolean;
  async_generate: boolean;
  tools: boolean;
  vision: boolean;
  json_mode: boolean;
  developer_role: boolean;
  reasoning: boolean;
  stream_usage: boolean;
  // Tools the upstream runs itself. Off by default on every profile: enabling it
  // accepts that this connection originates network calls Halro never sees.
  provider_executed_tools: boolean;
  max_context_tokens: number;
  max_output_tokens: number;
}

export interface ProviderBinding {
  id?: string;
  profile_id: string;
  enabled: boolean;
  capabilities: ProviderCapabilities;
}

export interface Provider {
  id: string;
  name: string;
  type: ProviderType;
  access_surface: AccessSurface;
  profile_id: string;
  credential_scheme: CredentialScheme;
  base_url: string;
  api_version?: string;
  credential_id: string;
  /** Empty or absent means the account's default Bedrock project. */
  bedrock_project_id?: string;
  allowed_anthropic_betas?: string[];
  allowed_hosts: string[];
  capabilities: ProviderCapabilities;
  bindings?: ProviderBinding[];
  capability_evidence: CapabilityEvidenceSet;
  max_concurrency: number;
  enabled: boolean;
  last_test_status?: "healthy" | "unhealthy";
  last_tested_at?: string;
  last_test_latency_millis?: number;
  last_test_error_class?: string;
  last_test_revision?: number;
  last_test_healthy_targets?: number;
  last_test_total_targets?: number;
  revision: number;
  created_at: string;
  updated_at: string;
}

export type ModelCapabilityStatus = "known" | "partial" | "unknown" | "conflicting";

export type ModelCapabilitySource =
  | "builtin_catalog"
  | "provider_metadata"
  | "signed_catalog"
  | "verified_probe"
  | "operator_declared"
  | "unsupported";

export type AvailabilityState = "available" | "unverified" | "unavailable";
export type TargetLifecycle = "active" | "deprecated" | "unknown";
export type ResolutionState = "resolved" | "unknown" | "conflicting" | "no_variant";
export type ClaimStatus = "supported" | "unsupported" | "unknown" | "conflicting";

export interface InvocationTargetScopeKey {
  provider_id: string;
  target_kind: DeploymentTargetKind;
  target_id: string;
  binding_id: string;
  profile_id: string;
  location?: string;
}

export interface CapabilityClaim {
  capability_id: string;
  status: ClaimStatus;
  evidence: CapabilityEvidence;
  source: Exclude<ModelCapabilitySource, "unsupported">;
  scope: InvocationTargetScopeKey;
  observed_at: string;
  expires_at?: string;
  revision: string;
}

export interface InvocationTargetDescriptor {
  target_id: string;
  target_kind: DeploymentTargetKind;
  display_name: string;
  owned_by?: string;
  canonical_model_ref?: string;
  region?: string;
  lifecycle: TargetLifecycle;
  metadata: {
    input_modalities?: string[];
    output_modalities?: string[];
    supported_operations?: string[];
    inference_types?: string[];
    max_context_tokens?: number;
    max_output_tokens?: number;
  };
  metadata_source: "none" | "provider_metadata";
  availability: AvailabilityState;
  fetched_at: string;
}

export interface DeploymentVariant {
  id: string;
  binding_id: string;
  profile_id: string;
  target: InvocationTargetDescriptor;
  capabilities: ProviderCapabilities;
  capability_claims: CapabilityClaim[];
  resolution_state: ResolutionState;
  revision: string;
}

export interface ResolvedInvocationTarget extends InvocationTargetDescriptor {
  variants: DeploymentVariant[];
  resolution_state: ResolutionState;
  resolution_revision: string;
}

export interface InvocationTargetDiscoveryCapabilities {
  target_kinds: DeploymentTargetKind[];
  can_enumerate: boolean;
  can_describe: boolean;
  can_verify: boolean;
  requires_management_identity: boolean;
  requires_canonical_model_mapping: boolean;
  /** Configured ceiling on possibly billable calls one detection may spend. */
  max_verification_calls: number;
}

/** A binding whose catalog could not be read. Its models are absent from the
 * list; that absence never means a capability is unsupported. */
export interface DegradedBinding {
  binding_id: string;
  profile_id: string;
  error_class: string;
}

export interface InvocationTargetCatalog {
  items: ResolvedInvocationTarget[];
  canonical_models?: ResolvedInvocationTarget[];
  discovery: InvocationTargetDiscoveryCapabilities;
  catalog_revision: string;
  provider_revision: number;
  degraded_bindings?: DegradedBinding[];
  fetched_at: string;
  expires_at: string;
  cached: boolean;
}

export type CapabilityProbeStatus = "supported" | "unsupported" | "inconclusive" | "unavailable" | "unauthorized" | "not_probed" | "canceled";

export interface DetectionBindingCandidate {
  binding_id: string;
  profile_id: string;
  access_surface: string;
  model_revision: string;
  /** Capabilities this interface can establish by probing at all. */
  verifiable?: string[];
  capability?: string;
  probe_kind?: string;
  status: CapabilityProbeStatus;
  evidence?: CapabilityEvidence;
  error_class?: string;
  answered: boolean;
}

export interface ModelCapabilityDetection {
  id: string;
  status: "queued" | "running" | "completed" | "failed" | "canceled" | "interrupted" | "ambiguous";
  source: "builtin_catalog" | "verified_probe";
  provider_id: string;
  provider_model: string;
  /** The interfaces identification considered, and what each one answered. */
  binding_candidates: DetectionBindingCandidate[];
  /** Empty until identification resolves one; "ambiguous" leaves it empty. */
  binding_id?: string;
  profile_id?: string;
  provider_calls: number;
  max_provider_calls: number;
  started_at?: string;
  completed_at?: string;
  expires_at?: string;
  cancel_requested_at?: string;
  capabilities: Record<string, { status: CapabilityProbeStatus; evidence?: CapabilityEvidence; error_class?: string; probe_kind: string }>;
  recommended_capabilities: ProviderCapabilities;
  selection_revision?: string;
  revision: number;
}

/** Whether a deployment's saved capability snapshot still describes something
 * that is supported now. Always derived by the server from a live comparison —
 * it is never stored on the deployment, because both sides of the comparison
 * move without the record being rewritten. */
export type CapabilityReviewState = "current" | "review_available" | "drifted" | "catalog_unavailable";

export interface CapabilityReview {
  state: CapabilityReviewState;
  /** What the deployment saved. */
  source: ModelCapabilitySource | string;
  status: ModelCapabilityStatus | string;
  model_revision: string;
  /** What the catalog says now. `catalog_covered` distinguishes "the catalog no
   * longer covers this model" from "it covers it and establishes nothing". */
  catalog_covered: boolean;
  catalog_source?: ModelCapabilitySource | string;
  catalog_status?: ModelCapabilityStatus | string;
  catalog_model_revision?: string;
  /** Established now but not in use. Offered, never enabled. Excludes anything
   * the operator switched off — that is reported separately, not re-offered. */
  available_for_review?: string[];
  /** Established now and switched off on purpose. Shown so the decision stays
   * visible and reversible, never as something new to adopt. */
  operator_disabled?: string[];
  /** Claimed by the snapshot but no longer established by profile or catalog. */
  no_longer_supported?: string[];
  reason?: CapabilityReviewReason | string;
}

export type CapabilityReviewReason =
  | "profile_narrowed"
  | "catalog_no_longer_covers_model"
  | "catalog_establishes_less"
  | "catalog_revision_advanced"
  | "catalog_now_covers_model"
  | "catalog_disagrees_with_declaration"
  | "catalog_unavailable";

export interface RouteCapabilityImpact {
  route_id: string;
  public_model: string;
  capability: string;
  /** No other enabled route on this public model is served by a deployment that
   * still has the capability, so requests needing it start being rejected. */
  sole_candidate: boolean;
}

export interface CapabilityPreflight {
  removed_capabilities: string[];
  added_capabilities: string[];
  affected_routes: RouteCapabilityImpact[];
  blocking: boolean;
}

export interface Deployment {
  id: string;
  name: string;
  provider_id: string;
  provider_model: string;
  target_kind?: DeploymentTargetKind;
  access_surface: AccessSurface;
  profile_id: string;
  binding_id?: string;
  region: string;
  capabilities: ProviderCapabilities;
  capability_evidence: CapabilityEvidenceSet;
  input_micros_per_million: number;
  output_micros_per_million: number;
  fixed_request_micros_usd: number;
  max_concurrency: number;
  enabled: boolean;
  last_test_status?: "healthy" | "unhealthy";
  last_tested_at?: string;
  last_test_latency_millis?: number;
  last_test_error_class?: string;
  last_test_revision?: number;
  revision: number;
  created_at: string;
  updated_at: string;
	pricing_quarantined?: boolean;
	pricing_quarantine_reason?: string;
  capability_review: CapabilityReview;
  /** Capabilities the operator switched off, kept apart from the ones nothing
   * ever established. */
  operator_disabled?: string[];
}

export type DeploymentTargetKind =
  | "model_id"
  | "azure_deployment"
  | "bedrock_foundation_model"
  | "bedrock_inference_profile"
  | "bedrock_provisioned_throughput"
  | "custom_endpoint_model";

export interface Route {
  id: string;
  public_model: string;
  /** A route names a deployment and nothing else about the upstream; provider,
   * model and price all come from it. */
  deployment_id: string;
  priority: number;
  strategy: "ordered" | "round_robin" | "";
  enabled: boolean;
  last_test_status?: "healthy" | "unhealthy";
  last_tested_at?: string;
  last_test_latency_millis?: number;
  last_test_error_class?: string;
  last_test_revision?: number;
  revision: number;
  created_at: string;
  updated_at: string;
}

export interface UsageAttempt {
  event_id: string;
  request_id: string;
  attempt_id: string;
  sequence: number;
  attempt: number;
  project_id: string;
  key_id?: string;
  route_id?: string;
  deployment_id?: string;
  provider_id?: string;
  requested_model?: string;
  provider_model?: string;
  provider_input_tokens: number;
  provider_output_tokens: number;
  cost_micros_usd: number | null;
  price_evidence_status: "versioned" | "legacy_unversioned" | "unknown";
  cost_value_status: "known" | "unknown";
  lease_mode?: "metered" | "free" | "unknown_allowed";
  price_snapshot?: PriceSnapshot;
  input_cost_micros_usd: number | null;
  output_cost_micros_usd: number | null;
  fixed_cost_micros_usd: number | null;
  tags?: string[];
  token_usage_source?: "provider_reported" | "gateway_estimated" | "none";
  cost_estimated: boolean;
  tokens_estimated: boolean;
  completed_at: string;
  status: string;
  error_class?: string;
  latency_millis: number;
}

export interface PriceSnapshot {
  pricing_selected_at: string;
  price_evidence_status: "versioned" | "unknown";
  cost_value_status: "known" | "unknown";
  price_version_id?: string;
  price_version?: number;
  billing_mode?: "metered" | "free";
  currency?: string;
  formula_version?: string;
  input_micros_per_million?: number;
  output_micros_per_million?: number;
  fixed_request_micros_usd?: number;
  effective_from?: string;
  source_type?: string;
  source_assurance?: string;
  source_content_sha256?: string;
  source_reference?: string;
  source_without_archive?: boolean;
}

export interface DeploymentPriceVersion {
  id: string;
  deployment_id: string;
  version: number;
  revision: number;
  billing_mode: "metered" | "free";
  currency: "USD";
  formula_version: string;
  input_micros_per_million: number;
  output_micros_per_million: number;
  fixed_request_micros_usd: number;
  effective_from: string;
  source: { type: string; assurance: string; content_sha256?: string; reference?: string; uri?: string };
  status: "active" | "scheduled" | "superseded" | "cancelled";
}

export interface DeploymentPriceProposal {
	id: string;
	deployment_id: string;
	provider_id: string;
	provider_model: string;
	region?: string;
	tier?: string;
	billing_mode: "metered" | "free";
	currency: "USD";
	input_micros_per_million: number;
	output_micros_per_million: number;
	fixed_request_micros_usd: number;
	source: { type: string; assurance: string; content_sha256: string; reference?: string; uri?: string };
	fetched_at: string;
	warnings?: string[];
	match: "exact" | "likely" | "ambiguous";
	expires_at: string;
	digest: string;
	status: "pending" | "adopted" | "rejected";
	adopted_price_version_id?: string;
	revision: number;
}

export interface AuditRecord {
  sequence: number;
  occurred_at: string;
  actor_type: string;
  actor_id: string;
  action: string;
  target_type: string;
  target_id: string;
  outcome: string;
  reason_code?: string;
}

export interface TokenGuardPolicy {
  id: string;
  name: string;
  enabled: boolean;
  action: "observe" | "alert" | "temporary_block";
  request_tokens: number;
  tokens_per_minute: number;
  cost_micros_per_minute: number;
  error_rate: number;
  minimum_samples: number;
  concurrency: number;
  unique_ips_per_minute: number;
  violations_before_block: number;
  block_ttl_seconds: number;
  cooldown_seconds: number;
  ewma_enabled: boolean;
  ewma_alpha: number;
  ewma_multiplier: number;
  ewma_minimum_samples: number;
  ewma_warmup_seconds: number;
  ewma_evaluation_window_seconds: number;
  ewma_cooldown_seconds: number;
  ewma_absolute_rpm: number;
  ewma_absolute_tpm: number;
  ewma_absolute_tokens_per_request: number;
  ewma_absolute_cost_micros_per_minute: number;
  revision: number;
  bound_projects?: number;
}

export interface TokenGuardPreview {
  violated: boolean;
  reason?: string;
  action: string;
}

export interface RedactionRule {
  id: string;
  name: string;
  kind: "builtin" | "regex" | "dictionary";
  builtin?: string;
  pattern?: string;
  dictionary?: string[];
  scopes: ("inbound" | "outbound")[];
  action: "detect_only" | "mask" | "replace" | "reject";
  replacement?: string;
  enabled: boolean;
  priority: number;
  computed_max_match_bytes: number;
}

export interface RedactionPolicy {
  id: string;
  name: string;
  enabled: boolean;
  mode: "strict" | "bounded_stream" | "detect_only_stream";
  rules: RedactionRule[];
  revision: number;
  bound_projects?: number;
}

export interface RedactionTestResult {
  match_count: number;
  matches: Array<{
    rule_id: string;
    category: string;
    action: string;
    field: string;
  }>;
}

export interface AlertWebhook {
  id: string;
  name: string;
  url: string;
  header_name?: string;
  secret_configured: boolean;
  enabled: boolean;
  revision: number;
}

export interface SystemStatus {
  build: { version: string; commit: string; date: string };
  accounting_status: number;
  draining: boolean;
  wal: Record<string, number>;
  write_path: WritePathSummary;
  audit: Record<string, number | string>;
  alerts: Record<string, number>;
  usage_watermark: Record<string, number>;
  time_context: TimeContext;
  activation?: ActivationStatus;
  tzdata?: { source: string; path?: string; version: string; fingerprint: string; zones: string[] };
}

export type ActivationDomain = "topology" | "auth" | "redaction" | "token_guard";

export interface ActivationStatus {
  stale: boolean;
  stale_since?: string;
  reason?: string;
  generation: number;
  domains: Array<{
    domain: ActivationDomain;
    stale: boolean;
    stale_since?: string;
    reason?: string;
  }>;
}

// The durable write path reduced to the means that explain this instance's
// throughput ceilings, so the console can answer "what is it doing right now"
// without an operator standing up Prometheus first.
export interface WritePathSummary {
  wal_sync_seconds: number;
  wal_batch_size: number;
  project_lock_wait_seconds: number;
  project_lock_held_seconds: number;
  project_events_per_second: number;
  project_requests_per_second: number;
  metadata_batch_size: number;
  metadata_write_seconds: number;
}

export interface SystemConfig {
  yaml: string;
  entries: SystemConfigEntry[];
  time_context: TimeContext;
}

export interface SystemConfigEntry {
  path: string;
  title_zh: string;
  title_en: string;
  description_zh: string;
  description_en: string;
  value: string;
  kind: "boolean" | "collection" | "number" | "text";
}

export interface ModelCatalogStatus {
  enabled: boolean;
  state: "disabled" | "current" | "degraded" | "catalog_unavailable";
  source: ModelCapabilitySource | string;
  revision: string;
  sequence?: number;
  pinned_revision?: string;
  last_attempt_at?: string;
  last_success_at?: string;
  degraded_since?: string;
  error_class?: string;
  expires_at?: string;
  using_expired_last_known_good?: boolean;
}

export interface ModelCatalogInfo {
  status: ModelCatalogStatus;
  bundled_revision: string;
  effective_revision: string;
  schema: { min_readable: number; max_readable: number };
  capability_dictionary_version: number;
  trust_root_count: number;
}

export interface RuntimeSettings {
  health_probe_interval_seconds: number;
  updated_at?: string;
  revision: number;
}

export type SupportedLocale = "zh-CN" | "en-US";
export type LocalePreference = SupportedLocale | "system";
export type Appearance = "light" | "dark";

export interface UIBootstrap {
  default_locale: SupportedLocale;
  supported_locales: SupportedLocale[];
}

export interface InstanceUISettings {
  default_locale: SupportedLocale;
  updated_at?: string;
  revision: number;
}

export interface AdminPreferences {
  locale: LocalePreference;
  appearance: Appearance;
  revision: number;
}
