export interface Page<T> {
  items: T[];
  next_cursor: string;
}

export interface Session {
  username: string;
  locale: LocalePreference;
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

export interface Bucket {
  hour: string;
  requests: number;
  attempts: number;
  input_tokens: number;
  output_tokens: number;
  estimated_input_tokens?: number;
  estimated_output_tokens?: number;
  cost_micros_usd: number;
  errors: number;
  latency_millis: number;
}

export interface Dashboard {
  usage: {
    today: Bucket;
    hourly: Bucket[];
    active_requests: number;
    watermark_sequence: number;
  };
  accounting_status: number;
  wal: Record<string, number>;
  alerts: Record<string, number>;
}

export interface Project {
  id: string;
  name: string;
  enabled: boolean;
  allowed_routes: string[];
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
  last_used_at?: string;
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

export type CapabilityEvidence = "verified" | "declared" | "legacy" | "unsupported";
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
  max_context_tokens: number;
  max_output_tokens: number;
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
  allowed_hosts: string[];
  capabilities: ProviderCapabilities;
  capability_evidence: CapabilityEvidenceSet;
  max_concurrency: number;
  enabled: boolean;
  revision: number;
  created_at: string;
  updated_at: string;
}

export interface Deployment {
  id: string;
  name: string;
  provider_id: string;
  provider_model: string;
  access_surface: AccessSurface;
  profile_id: string;
  region: string;
  capabilities: ProviderCapabilities;
  capability_evidence: CapabilityEvidenceSet;
  input_micros_per_million: number;
  output_micros_per_million: number;
  fixed_request_micros_usd: number;
  max_concurrency: number;
  priority: number;
  weight: number;
  enabled: boolean;
  revision: number;
  created_at: string;
  updated_at: string;
}

export interface Route {
  id: string;
  public_model: string;
  deployment_id: string;
  provider_id?: string;
  provider_model?: string;
  input_micros_per_million?: number;
  output_micros_per_million?: number;
  priority: number;
  strategy: "ordered" | "round_robin" | "";
  enabled: boolean;
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
  cost_micros_usd: number;
  cost_estimated: boolean;
  tokens_estimated: boolean;
  completed_at: string;
  status: string;
  error_class?: string;
  latency_millis: number;
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
  audit: Record<string, number | string>;
  alerts: Record<string, number>;
  usage_watermark: Record<string, number>;
}

export interface RuntimeSettings {
  health_probe_interval_seconds: number;
  updated_at?: string;
  revision: number;
}

export type SupportedLocale = "zh-CN" | "en-US";
export type LocalePreference = SupportedLocale | "system";

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
  revision: number;
}
