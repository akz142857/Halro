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
export interface UsageSettings {
  /** How far back the attempt log and the failed-request list can page. */
  console_window_days: number;
  /** What the console offers; any value between min and max is accepted. */
  presets: number[];
  min_days: number;
  /** The archive's retention — the console must not promise more history. */
  max_days: number;
  /** What config.yaml says, and whether it is still what takes effect. */
  config_file_days: number;
  config_file_in_effect: boolean;
  updated_at: string;
  revision: number;
}

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

// SummaryMetrics is one accounting period's totals as the summary endpoint
// reports them. The request-level columns are absent — not zero — on a
// dimension with no request identity: a provider row cannot claim a share of a
// request that may have spanned several providers.
export interface SummaryMetrics {
  requests?: number;
  request_errors?: number;
  request_latency_samples?: number;
  request_latency_p95_millis?: number;
  request_latency_over_max?: number;
  attempts: number;
  errors: number;
  input_tokens: number;
  output_tokens: number;
  estimated_input_tokens: number;
  estimated_output_tokens: number;
  provider_cached_input_tokens: number;
  provider_cache_write_input_tokens: number;
  provider_reasoning_tokens: number;
  cost_micros_usd: number;
  estimated_cost_micros_usd: number;
  unknown_attempts: number;
  latency_millis: number;
  attempt_latency_samples: number;
  attempt_latency_p95_millis: number;
  attempt_latency_over_max?: number;
  latency_approximate: boolean;
}

export interface SummaryBucket extends SummaryMetrics {
  period: string;
  // The absolute interval the label covers. Drill-down links are built from
  // these instants rather than from the label, because the same local date
  // under two generations of the accounting timezone is two different windows.
  start: string;
  end: string;
}

export interface SummaryGroup extends SummaryMetrics {
  key: string;
}

export interface UsageSummary {
  granularity: "day" | "month" | "year";
  start: string;
  end: string;
  totals: SummaryMetrics;
  buckets: SummaryBucket[];
  group_by?: string;
  groups?: SummaryGroup[];
  groups_truncated?: boolean;
  groups_other_count?: number;
  sort?: string;
  order?: string;
  filter?: { dimension: string; value: string };
  timezone_changes: Array<{ period_id: string; from_version: number; to_version: number }>;
  resource_labels?: Record<string, string>;
  watermark_sequence: number;
  time_context?: TimeContext;
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
      /** Refused because no target behind the public model could serve what was
       * asked — a configured route, a good key, and a request naming something
       * the deployments behind it do not declare. */
      route_capability: number;
      rpm: number;
      tpm: number;
      project_concurrency: number;
      provider_concurrency: number;
      deployment_concurrency: number;
      budget: number;
      run_budget: number;
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
  deferred_responses: boolean;
  max_deferred_queue: number;
  allowed_cidrs: string[] | null;
  redaction_policy_id: string;
  token_guard_policy_id: string;
  run_governance?: RunGovernanceConfig;
  revision: number;
  created_at: string;
  updated_at: string;
}

export interface RunGovernanceConfig {
  enabled: boolean;
  default_run_budget_micros_usd: number;
  max_run_budget_micros_usd: number;
  default_run_ttl_seconds: number;
  max_run_ttl_seconds: number;
  max_active_runs: number;
  max_open_work_units: number;
}

export type GatewayScope = "inference" | "work_unit:create" | "run:create" | "run:attach" | "governance:read" | "outcome:write";

export interface GatewayKey {
  id: string;
  project_id: string;
  name: string;
  enabled: boolean;
  scopes?: GatewayScope[];
  expires_at?: string;
  created_at: string;
  revision: number;
}

export interface WorkUnit {
  id: string;
  project_id: string;
  status: "open" | "closed";
  created_by_key_id: string;
  created_at: string;
  closed_at?: string;
  period_id: string;
  period_timezone_version: number;
  run_count?: number;
  committed_micros_usd?: number;
  reserved_micros_usd?: number;
  unknown_attempts?: number;
	 outcome_definitions?: OutcomeDefinitionRef[];
}

export interface OutcomeDefinitionRef { id: string; version: number }
export interface OutcomeDefinition {
	id: string; project_id: string; name: string; version: number; data_type: "BOOLEAN" | "CATEGORICAL";
	allowed_values: string[]; success_values: string[]; unit?: string; description?: string; enabled: boolean;
	created_at: string; created_by: string; revision: number;
}
export interface Outcome {
	id: string; project_id: string; work_unit_id: string; definition_id: string; definition_version: number; value: string;
	reporter_key_id: string; evidence_sha256?: string; evidence_ref?: string; observed_at: string; ingested_at: string;
	supersedes_outcome_id?: string; revision: number; governance_sequence: number; provisional: boolean;
}
export interface GovernanceSummary {
	basis: "work_unit_cohort"; cohort_start: string; cohort_end: string; definition_id: string; definition_version: number;
	generated_at: string; eligible_units: number; matured_units: number; evaluated_units: number; successful_units: number;
	outcome_coverage: number | null; success_rate: number | null; known_cost_micros_usd: number; in_progress_cost_micros_usd: number;
	estimated_cost_micros_usd: number; unknown_attempts: number; cost_completeness: "complete" | "partial"; cost_per_success_micros_usd: number | null;
}

export interface Run {
  id: string;
  project_id: string;
  work_unit_id: string;
  budget_micros_usd: number;
  committed_micros_usd: number;
  reserved_micros_usd: number;
  remaining_micros_usd: number;
  budget_state: "available" | "fully_reserved" | "depleted";
  unknown_attempts: number;
  status: "active" | "closed" | "expired";
  created_by_key_id: string;
  created_at: string;
  expires_at: string;
  closed_at?: string;
  close_reason?: string;
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
  | "bedrock-mantle"
  | "minimax-api"
  | "kimi-api";

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
  | "minimax"
  | "kimi"
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
  /** Whether the target will retrieve an image the request only names, as
   * opposed to reading one the request carries. Bedrock does the second only. */
  fetched_image: boolean;
  /** The schema-less JSON mode, which only promises the answer parses. Separate
   * from structured_outputs because no provider serves them as one thing:
   * Anthropic has the schema and not this, DeepSeek has this and not the schema. */
  json_object: boolean;
  /** A schema the upstream enforces — OpenAI json_schema, Anthropic
   * output_config.format. */
  structured_outputs: boolean;
  developer_role: boolean;
  reasoning: boolean;
  stream_usage: boolean;
  // Tools the upstream runs itself. Off by default on every profile: enabling it
  // accepts that this connection originates network calls Halro never sees.
  provider_executed_tools: boolean;
  max_context_tokens: number;
  max_output_tokens: number;
}

/** One profile as the server describes it, from GET /provider-profiles.
 *
 * `ceiling` is what an operator may turn on; `defaults` is what a new connection
 * starts with. `default_base_url` arrives already resolved for this deployment —
 * the region is substituted server-side, so this is a value to put straight into
 * the form. */
export interface ProviderProfileDescriptor {
  id: string;
  access_surface: AccessSurface;
  credential_scheme: CredentialScheme;
  default_base_url: string;
  immutable: boolean;
  defaults: ProviderCapabilities;
  ceiling: ProviderCapabilities;
  /** What a whole connection anchored on this profile may turn on, and what it
   * starts with. One connection can span profiles — an OpenAI key serves the
   * chat endpoints and the media ones — and which profile serves what is the
   * server's answer, so these arrive resolved rather than as something to
   * recompute from the two sets above. */
  connection_ceiling: ProviderCapabilities;
  connection_defaults: ProviderCapabilities;
  /** The other profiles a connection anchored here carries. */
  combines_with: string[];
  /** The half of the capability model routing applies and nothing used to show.
   * A capability tick says what this interface can do; a constraint says which
   * member of a request it still cannot carry, and the Gateway refuses on it
   * before any provider call. */
  request_constraints: ProfileRequestConstraint[];
}

/** One endpoint's worth of members a profile has declared it cannot carry, in
 * that endpoint's own spelling of the field. */
export interface ProfileRequestConstraint {
  endpoint_id: string;
  path: string;
  unsupported_request_fields: string[];
  declared_transforms?: string[];
}

export interface ProviderTypeDescriptor {
  type: ProviderType;
  default_profile_id: string;
  profiles: ProviderProfileDescriptor[];
}

/** What this build can serve. Everything a connection form needs to decide what
 * to offer comes from here rather than from constants in this bundle: the two
 * used to be kept in step by hand, and both directions of drift were bad — a
 * console wider than the server offers capabilities whose save is refused
 * without saying which, a console narrower hides capabilities that work. */
export interface ProviderProfilesCatalog {
  /** The capability keys. Not a display order: how they are arranged and what
   * they are called in a given language stay with this bundle. */
  capability_names: string[];
  /** What each capability needs alongside it, as the server enforces it, so the
   * form can present the rule instead of discovering it on refusal. Direct
   * dependencies: stream usage names streaming, streaming names chat. */
  capability_dependencies: Record<string, string[]>;
  /** Capabilities a form has to say something about before they are ticked.
   * provider_executed_tools is one: enabling it accepts upstream egress that
   * never passes through Halro's transport. The wording is this bundle's; which
   * capabilities need it is not. */
  capability_opt_in_warnings: string[];
  /** Halro's capability vocabulary rendered as the input/output view a model
   * catalogue uses. Served rather than derived in the browser: the mapping is
   * not obvious (transcriptions is an operation whose input is audio, speech is
   * text-to-audio and so has no input row of its own), and a second copy here
   * would be a second thing to keep true. Capabilities within a row are any-of. */
  capability_modalities: { direction: "input" | "output"; modality: string; capabilities: string[] }[];
  /** Capabilities that describe the protocol rather than the data, listed rather
   * than left over — so a modality view can say "these are not missing, they are
   * not modalities" instead of silently omitting them. */
  non_modal_capabilities: string[];
  provider_types: ProviderTypeDescriptor[];
}

/** One profile a saved connection serves through. This is reported, never sent:
 * a form submits one flat capability set and the server decides which profile
 * carries each part of it. */
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
export type ResolutionState = "resolved" | "unknown" | "conflicting" | "no_variant" | "covered_elsewhere";
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
  metadata_source: "none" | "provider_metadata" | "model_catalog";
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
  /** Interfaces the catalogue lists this model under, when none of them is bound here. */
  covered_by_profiles?: string[];
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
  /** The catalogue was never fetched or the copy expired; reading never dials. */
  not_cached?: boolean;
  fetched_at: string;
  expires_at: string;
  cached: boolean;
}

export type CapabilityProbeStatus = "supported" | "unsupported" | "inconclusive" | "unavailable" | "unauthorized" | "not_probed" | "canceled" | "assertion_failed";

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
  capabilities: Record<string, { status: CapabilityProbeStatus; evidence?: CapabilityEvidence; error_class?: string; provider_status?: number; provider_code?: string; probe_kind: string }>;
  /** What the catalogue claimed when this run was asked to verify it. Absent for
   * a model the catalogue does not cover, where there is nothing to measure
   * against — a probe verdict is then the only claim. */
  baseline_capabilities?: ProviderCapabilities;
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
  /** What the active probe last said. `not_probed` is its own state: a
   * deployment stays eligible for routing until a probe has actually failed. */
  probe?: DeploymentProbe;
  /** Capabilities the operator switched off, kept apart from the ones nothing
   * ever established. */
  operator_disabled?: string[];
}

export interface DeploymentProbe {
  state: "healthy" | "unhealthy" | "not_probed";
  observed_at?: string;
  /** Classified only — never the upstream's sentence about the request. */
  error_class?: string;
}

export type DeploymentTargetKind =
  | "model_id"
  | "azure_deployment"
  | "bedrock_foundation_model"
  | "bedrock_inference_profile"
  | "bedrock_provisioned_throughput"
  | "custom_endpoint_model";

/** Why the live registry is not routing on a route the store has enabled. The
 * reason is a class, never the underlying error. */
export interface RouteWithholding {
  kind: "reference" | "capability_drift";
  reason: string;
}

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
  /** Present only when the route is enabled and the live registry refused it.
   * `enabled` alone cannot say this: it is what the operator asked for, not
   * what the gateway is doing. */
  withheld?: RouteWithholding;
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
  work_unit_id?: string;
  run_id?: string;
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
  started_at: string;
  completed_at: string;
  status: string;
  error_class?: string;
  // The upstream's own status. Absent when the failure never got one — a
  // refused dial, a response that would not decode — which is a different
  // answer from "the upstream returned nothing", so it is left undefined
  // rather than shown as 0.
  http_status?: number;
  // What a support ticket to the upstream is built out of, and where along the
  // request the failure happened. Absent on attempts recorded before these were
  // kept, which is a different answer from "the upstream named none" — the
  // console says which, rather than filling either with a placeholder.
  provider_code?: string;
  provider_request_id?: string;
  failure_phase?: string;
  latency_millis: number;
  // Which rung of the retry/fallback chain this attempt is: retry_count counts
  // re-tries against the same target, fallback_count counts targets already
  // given up on. Both are on every attempt, including successful ones — that
  // is how a fallback that worked can be told apart from a first try.
  retry_count: number;
  fallback_count: number;
}

// What a failed call carried, when the operator has switched capture on. This
// is the only payload in the console that holds material a caller wrote, which
// is why fetching it is an audited action on the server and why nothing here is
// cached or persisted in the browser.
export interface FailurePayload {
  request_id: string;
  project_id: string;
  outcome: string;
  captured_at: string;
  // The operation as it went upstream — already through the project's redaction
  // policy, because capture happens after that policy has run.
  request?: unknown;
  request_truncated?: boolean;
  // The upstream's own answer, or the answer Halro could not put on the
  // caller's wire. Absent when the failure produced nothing to record.
  response?: unknown;
  response_truncated?: boolean;
}

// One failed request, as the failed-request list serves it. It is not a failed
// attempt: a request that failed one target and succeeded on the next is not
// here at all, and a request refused before any upstream call is, with no
// last_failure to show for it.
export interface RequestFailure {
  request_id: string;
  project_id: string;
  key_id?: string;
  requested_model?: string;
  // The ledger's own terminal state. Which of these read as a policy rejection
  // is the console's judgement, not the record's.
  outcome: string;
  sequence: number;
  accepted_at: string;
  completed_at: string;
  attempts: number;
  fallbacks: number;
  // Absent when nothing upstream failed — a budget refusal, an open circuit, a
  // target at its concurrency limit. Absent, not blank: an empty provider
  // context would report that an upstream did not answer a request that never
  // asked one.
  last_failure?: {
    attempt_id: string;
    attempt: number;
    error_class?: string;
    provider_status?: number;
    provider_id?: string;
    deployment_id?: string;
    provider_model?: string;
    provider_code?: string;
    provider_request_id?: string;
    failure_phase?: string;
    completed_at: string;
  };
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
  cached_input_micros_per_million?: number;
  output_micros_per_million?: number;
  fixed_request_micros_usd?: number;
  // Present only when the price version bills by time of day. The four terms
  // above are already this rung's rates; this says which rung, so a settled
  // attempt can be explained without re-reading the rule table.
  schedule_tier?: PriceScheduleTier;
  effective_from?: string;
  source_type?: string;
  source_assurance?: string;
  source_content_sha256?: string;
  source_reference?: string;
  source_without_archive?: boolean;
}

export interface PriceScheduleTier {
  timezone: string;
  source: "base" | "window" | "zone_unavailable";
  start_minute?: number;
  end_minute?: number;
  local_minute?: number;
}

export interface PriceWindow {
  start_minute: number;
  end_minute: number;
  input_micros_per_million: number;
  cached_input_micros_per_million: number;
  output_micros_per_million: number;
  fixed_request_micros_usd: number;
}

// Windows are disjoint and sorted, and they need not cover the day: an instant
// no window covers is billed at the version's own four terms.
export interface PriceSchedule {
  timezone: string;
  windows: PriceWindow[];
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
  cached_input_micros_per_million: number;
  output_micros_per_million: number;
  fixed_request_micros_usd: number;
  schedule?: PriceSchedule;
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
	cached_input_micros_per_million: number;
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
	governance?: { ready: boolean; sequence: number; offset: number };
  time_context: TimeContext;
  activation?: ActivationStatus;
  reload?: ReloadStatus;
  tzdata?: { source: string; path?: string; version: string; fingerprint: string; zones: string[] };
}

// What SIGHUP last did, and what is actually being served. A configuration that
// can change without a restart makes the file on disk an unreliable answer to
// "what is in force", and this is the reliable one.
export interface ReloadStatus {
  items: Array<{
    item: "tls" | "metrics_tls" | "log_level" | "log_file";
    // False where this deployment has nothing of the kind — no TLS, no log
    // file. Without it, "never reloaded" and "not configured" look identical,
    // and only one of them is worth investigating.
    applies: boolean;
    successes: number;
    failures: number;
    last_success: string | null;
  }>;
  certificates: Array<{
    scope: "serving" | "metrics";
    name: string;
    not_after: string;
    // The leading bytes of the certificate's SHA-256, matching what
    // `openssl x509 -fingerprint` reads back from outside.
    fingerprint: string;
  }>;
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
  wal_events_per_second: number;
  wal_max_events_per_second: number;
  wal_max_batch_size: number;
  project_lock_wait_seconds: number;
  project_lock_held_seconds: number;
  project_events_per_second: number;
  requests_per_second: number;
  bound_by: string;
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
