// Synthetic Admin API contracts for console-only browser acceptance.
// Shapes follow src/types.ts and the directly affected Vitest fixtures. No key
// or endpoint here is a real provider credential. Enumeration uses the Go-
// generated provider-profiles golden, never a handwritten replacement.
import { readFile } from 'node:fs/promises';

export const NOW = '2026-11-04T12:00:00Z';
const CREATED = '2026-11-01T10:00:00Z';
const pageOf = (items) => ({ items, next_cursor: '' });
const clone = (value) => structuredClone(value);
const profiles = JSON.parse(await readFile(new URL('../src/test/provider-profiles.golden.json', import.meta.url), 'utf8'));
const emptyCapabilities = Object.fromEntries(profiles.capability_names.map((name) => [name, false]));
const capabilities = { ...emptyCapabilities, chat: true, streaming: true, tools: true, vision: true, json_object: true, structured_outputs: true, developer_role: true, reasoning: true, stream_usage: true, max_context_tokens: 128000, max_output_tokens: 16384 };
const timeContext = { accounting_timezone: 'Asia/Singapore', timezone_version: 1, period_id: '2026-11-04', period_start: '2026-11-03T16:00:00Z', period_end: '2026-11-04T16:00:00Z', generated_at: NOW };
const writePath = { wal_sync_seconds: 0.001, wal_batch_size: 3, wal_events_per_second: 120, wal_max_events_per_second: 500, wal_max_batch_size: 12, project_lock_wait_seconds: 0, project_lock_held_seconds: 0.001, project_events_per_second: 120, requests_per_second: 24, bound_by: '', metadata_batch_size: 1, metadata_write_seconds: 0.001 };
const metrics = { requests: 120, request_errors: 1, attempts: 122, errors: 2, input_tokens: 24000, output_tokens: 8000, estimated_input_tokens: 0, estimated_output_tokens: 0, provider_cached_input_tokens: 0, provider_cache_write_input_tokens: 0, provider_reasoning_tokens: 0, cost_micros_usd: 4500000, estimated_cost_micros_usd: 0, unknown_attempts: 0, latency_millis: 120000, request_latency_samples: 120, request_latency_p50_millis: 750, request_latency_p95_millis: 1800, attempt_latency_samples: 122, attempt_latency_p95_millis: 1600, latency_approximate: true };

export function createFixtureState(options = {}) {
  const { locale = 'zh-CN', appearance = 'dark', role = 'administrator', auth = 'authenticated', empty = false, firstRun = false, mfaSetupRequired = false } = options;
  const session = { username: 'acceptance-admin', role, locale, appearance, csrf_token: 'synthetic-csrf-not-a-secret', absolute_expires_at: '2027-01-01T00:00:00Z', idle_expires_at: '2027-01-01T00:00:00Z', mfa_setup_required: mfaSetupRequired };
  const credential = { id: 'credential_openai', name: 'OpenAI production', type: 'openai', access_surface: 'openai-api', offering_id: 'openai.api-platform', region_id: '', scheme: 'bearer.static', bound_base_url: 'https://api.openai.com:443', secret_configured: true, key_version: 1, revision: 1 };
  const provider = { id: 'provider_openai', name: 'OpenAI production', type: 'openai', base_url: 'https://api.openai.com', access_surface: 'openai-api', profile_id: 'openai.chat-embeddings.v1', credential_scheme: 'bearer.static', credential_id: credential.id, allowed_hosts: ['api.openai.com'], capabilities, capability_evidence: {}, max_concurrency: 8, enabled: true, revision: 1, created_at: CREATED, updated_at: CREATED, bindings: [{ id: 'b-chat', profile_id: 'openai.chat-embeddings.v1', enabled: true, capabilities }], last_test_status: 'healthy', last_test_revision: 1, last_test_current: true, last_test_runtime_id: 'runtime_acceptance', last_test_latency_ms: 42, last_test_at: NOW };
  const deployment = { id: 'deployment_gpt', name: 'Production chat', provider_id: provider.id, binding_id: 'b-chat', provider_model: 'gpt-chat', target_kind: 'model_id', access_surface: 'openai-api', profile_id: 'openai.chat-embeddings.v1', capabilities, capability_evidence: Object.fromEntries(Object.keys(capabilities).filter((key) => capabilities[key] === true).map((key) => [key, 'declared'])), region: '', max_concurrency: 8, priority: 0, weight: 1, enabled: true, revision: 1, created_at: CREATED, updated_at: CREATED };
  const route = { id: 'route_chat', public_model: 'chat', deployment_id: deployment.id, priority: 10, strategy: 'ordered', enabled: true, revision: 1, created_at: CREATED, updated_at: CREATED };
  const project = { id: 'project_acceptance', name: 'Production application', enabled: true, allowed_models: ['chat'], rpm: 60, tpm: 100000, max_concurrency: 8, daily_budget_micros_usd: 10000000, max_input_tokens: 0, max_output_tokens: 0, max_request_bytes: 0, max_stream_duration: 0, deferred_responses: false, max_deferred_queue: 0, allowed_cidrs: [], redaction_policy_id: '', token_guard_policy_id: '', run_governance: { enabled: true, default_run_budget_micros_usd: 1000000, max_run_budget_micros_usd: 5000000, default_run_ttl_seconds: 3600, max_run_ttl_seconds: 86400, max_active_runs: 10, max_open_work_units: 100 }, revision: 1, created_at: CREATED, updated_at: CREATED };
  const key = { id: 'key_acceptance', project_id: project.id, name: 'Production gateway access', enabled: true, scopes: ['inference', 'discovery', 'work_unit:create', 'run:create', 'run:attach', 'outcome:write'], created_at: CREATED, revision: 1 };
  const definition = { id: 'odef_acceptance', project_id: project.id, name: 'Task accepted', version: 1, data_type: 'CATEGORICAL', allowed_values: ['accepted', 'rejected'], success_values: ['accepted'], enabled: true, created_at: CREATED, created_by: 'acceptance-admin', revision: 1 };
  const workUnit = { id: 'wku_acceptance', project_id: project.id, status: 'closed', created_by_key_id: key.id, created_at: CREATED, closed_at: NOW, period_id: '2026-11-04', period_timezone_version: 1, run_count: 1, committed_micros_usd: 125000, reserved_micros_usd: 0, unknown_attempts: 0, outcome_definitions: [{ id: definition.id, version: 1 }] };
  const run = { id: 'run_acceptance', project_id: project.id, work_unit_id: workUnit.id, budget_micros_usd: 1000000, committed_micros_usd: 125000, reserved_micros_usd: 0, remaining_micros_usd: 875000, budget_state: 'available', unknown_attempts: 0, status: 'closed', created_by_key_id: key.id, created_at: CREATED, expires_at: NOW, closed_at: NOW, close_reason: 'completed' };
  const outcome = { id: 'out_acceptance', project_id: project.id, work_unit_id: workUnit.id, definition_id: definition.id, definition_version: 1, value: 'accepted', reporter_key_id: key.id, observed_at: NOW, ingested_at: NOW, revision: 1, governance_sequence: 10, provisional: false };
  const attempt = { event_id: 'event_acceptance', request_id: 'req_acceptance', attempt_id: 'att_acceptance', sequence: 10, attempt: 1, project_id: project.id, work_unit_id: workUnit.id, run_id: run.id, key_id: key.id, route_id: route.id, deployment_id: deployment.id, provider_id: provider.id, offering_id: 'openai.api-platform', profile_id: 'openai.chat-embeddings.v1', requested_model: 'chat', provider_model: 'gpt-chat', provider_input_tokens: 1000, provider_output_tokens: 250, cost_micros_usd: 125000, price_evidence_status: 'versioned', cost_value_status: 'known', lease_mode: 'metered', input_cost_micros_usd: 100000, output_cost_micros_usd: 25000, fixed_cost_micros_usd: 0, cost_estimated: false, tokens_estimated: false, token_usage_source: 'provider_reported', started_at: '2026-11-04T11:59:59Z', completed_at: NOW, status: 'success', latency_millis: 1000, retry_count: 0, fallback_count: 0 };
  const failure = { request_id: 'req_failed', project_id: project.id, requested_model: 'chat', outcome: 'provider_error', sequence: 11, accepted_at: CREATED, completed_at: NOW, attempts: 1, fallbacks: 0, last_failure: { attempt_id: 'att_failed', attempt: 1, error_class: 'timeout', provider_status: 504, provider_id: provider.id, deployment_id: deployment.id, provider_model: 'gpt-chat', provider_code: 'timeout', provider_request_id: 'synthetic-upstream-id', failure_phase: 'provider', completed_at: NOW } };
  const policy = { id: 'tgp_acceptance', name: 'Production guard', enabled: true, action: 'alert', request_tokens: 32000, tokens_per_minute: 200000, cost_micros_per_minute: 5000000, error_rate: 0.2, minimum_samples: 10, concurrency: 20, unique_ips_per_minute: 10, violations_before_block: 2, block_ttl_seconds: 300, cooldown_seconds: 60, ewma_enabled: false, ewma_alpha: 0.2, ewma_multiplier: 3, ewma_minimum_samples: 100, ewma_warmup_seconds: 3600, ewma_evaluation_window_seconds: 60, ewma_cooldown_seconds: 300, ewma_absolute_rpm: 60, ewma_absolute_tpm: 50000, ewma_absolute_tokens_per_request: 4000, ewma_absolute_cost_micros_per_minute: 1000000, revision: 1, bound_projects: 0 };
  const prices = [{ id: 'price_acceptance', deployment_id: deployment.id, version: 1, billing_mode: 'metered', formula_version: 'usd_tokens_v1', input_micros_per_million: 1000000, output_micros_per_million: 2000000, cached_input_micros_per_million: 100000, fixed_request_micros_usd: 0, currency: 'USD', status: 'active', effective_from: CREATED, source: { type: 'manual', assurance: 'asserted', reference: 'temporary_estimate' }, revision: 1 }];
  const proxy = { id: 'proxy_acceptance', name: 'Office proxy', kind: 'http_connect', endpoint_host: 'proxy.example.test', endpoint_port: 443, endpoint_scheme: 'https', endpoint: 'https://proxy.example.test:443', authenticated: false, allow_private_endpoint: false, allow_loopback_endpoint: false, allow_cleartext_basic_auth: false, revision: 1 };
  const system = { build: { version: '0.11.0', commit: 'synthetic-acceptance', date: CREATED }, accounting_status: 0, draining: false, wal: { batches: 100, records: 500, errors: 0, queue_depth: 0, queue_capacity: 1024 }, write_path: writePath, audit: { records: 12, last_hash: 'synthetic-hash' }, alerts: { Accepted: 1, Delivered: 1, Failed: 0, Dropped: 0, Queued: 0 }, usage_watermark: { sequence: 10 }, governance: { ready: true, sequence: 10, offset: 1000 }, time_context: timeContext, activation: { stale: false, generation: 1, domains: ['topology', 'auth', 'redaction', 'token_guard'].map((domain) => ({ domain, stale: false })) }, reload: { items: [], certificates: [] }, tzdata: { source: 'embedded', version: '2026a', fingerprint: 'synthetic', zones: ['Asia/Singapore', 'UTC', 'Europe/Berlin'] } };
  const dashboard = { first_value_reached: !firstRun, usage: { today: { ...metrics, hour: NOW }, hourly: Array.from({ length: 8 }, (_, index) => ({ ...metrics, hour: `2026-11-04T0${index}:00:00Z` })), active_requests: 0, watermark_sequence: 10, breakdowns: Object.fromEntries(['project', 'provider', 'requested_model', 'provider_model'].map((dimension) => [dimension, { calls: [], cost: [], errors: [] }])), recent_anomalies: [] }, governance: { policy_rejections: { route_capability: 0, rpm: 0, tpm: 0, project_concurrency: 0, provider_concurrency: 0, deployment_concurrency: 0, budget: 0, run_budget: 0, token_guard: 0, total: 0 }, budget: { at_risk: 0, items: [] }, capacity: { at_risk: 0, items: [] }, pricing: { quarantined: 0, unknown: 0 } }, resource_labels: { [project.id]: project.name }, accounting_status: 0, runtime: { accepting_traffic: true, draining: false, activation: system.activation }, time_context: timeContext, alerts: { Accepted: 1, Delivered: 1, Failed: 0, Dropped: 0, Queued: 0, QueueCapacity: 16, Endpoints: 1, UnhealthyEndpoints: 0, UnknownEndpoints: 0 }, wal: { ...system.wal, syncs: 100, sync_seconds: 0.08 } };
  const state = { options: { locale, appearance, role, auth, empty, firstRun, mfaSetupRequired }, session, system, dashboard, credentials: [credential], providers: [provider], deployments: [deployment], routes: [route], projects: [project], keys: [key], definitions: [definition], workUnits: [workUnit], runs: [run], outcomes: [outcome], attempts: [attempt], failures: [failure], policies: [policy], redactions: [{ id: 'rp_acceptance', name: 'PII redaction', enabled: false, mode: 'strict', rules: [], revision: 1, bound_projects: 0 }], alerts: [{ id: 'whk_acceptance', name: 'Security operations', url: 'https://hooks.example.test/halro', header_name: 'authorization', secret_configured: true, enabled: true, revision: 1 }], proxies: [proxy], prices, audit: [{ sequence: 12, event_id: 'aud_acceptance', occurred_at: NOW, actor_type: 'admin_user', actor_id: 'acceptance-admin', action: 'alert_webhook.create', target_type: 'alert_webhook', target_id: 'whk_acceptance', outcome: 'success' }], writes: [] };
  if (empty) for (const name of ['credentials', 'providers', 'deployments', 'routes', 'projects', 'keys', 'definitions', 'workUnits', 'runs', 'outcomes', 'attempts', 'failures', 'policies', 'redactions', 'alerts', 'audit']) state[name] = [];
  if (options.longContent && !empty) {
    const alias = 'production-chat-with-an-unusually-long-public-model-alias-'.repeat(3);
    state.projects[0].name = 'Production application with an unusually long multilingual name · 生产应用与跨区域业务访问边界 '.repeat(3);
    state.providers[0].name = 'OpenAI production connection with a long administrative identifier '.repeat(3);
    state.deployments[0].name = 'Production chat deployment with long display metadata '.repeat(3);
    state.projects[0].allowed_models = [alias];
    state.routes[0].public_model = alias;
    state.attempts[0].requested_model = alias;
  }
  return state;
}

function targetCatalog(state) {
  const provider = state.providers[0];
  const target = { target_id: 'gpt-chat', target_kind: 'model_id', display_name: 'GPT Chat', canonical_model_ref: 'gpt-chat', lifecycle: 'active', metadata: {}, metadata_source: 'none', availability: 'available', fetched_at: NOW };
  const variant = { id: 'b-chat', binding_id: 'b-chat', profile_id: 'openai.chat-embeddings.v1', target, capabilities, capability_claims: Object.keys(capabilities).filter((key) => capabilities[key] === true).map((key) => ({ capability_id: key, status: 'supported', evidence: 'declared', source: 'builtin_catalog', scope: { provider_id: provider?.id ?? 'provider_openai', target_kind: 'model_id', target_id: target.target_id, binding_id: 'b-chat', profile_id: 'openai.chat-embeddings.v1' }, observed_at: NOW, revision: `sha256:synthetic-${key}` })), resolution_state: 'resolved', revision: 'sha256:synthetic-variant' };
  return { items: [{ ...target, variants: [variant], resolution_state: 'resolved', resolution_revision: 'sha256:synthetic-resolution' }], discovery: { target_kinds: ['model_id'], can_enumerate: true, can_describe: true, can_verify: true, requires_management_identity: false, requires_canonical_model_mapping: false, max_verification_calls: 10 }, catalog_revision: 'sha256:synthetic-catalog', provider_revision: 1, fetched_at: NOW, expires_at: '2026-11-04T13:00:00Z', cached: true };
}

export function fixtureResponse(state, method, pathname, query, body) {
  const { locale, appearance, role, auth, firstRun } = state.options;
  const ok = (data, status = 200) => ({ status, data, headers: { ETag: '"1"' } });
  const error = (status, code, message) => ({ status, data: { code, error: message } });
  const endpoints = {
    '/ui/bootstrap': { default_locale: locale, supported_locales: ['zh-CN', 'en-US'] },
    '/setup/status': { setup_required: auth === 'setup', token_required: true },
    '/session': state.session,
    '/system/status': state.system,
    '/dashboard': state.dashboard,
    '/provider-profiles': profiles,
    '/provider-egress-proxies': { runtime_id: 'runtime_acceptance', items: state.proxies },
    '/route-suspensions': { items: [], runtime_id: 'runtime_acceptance' },
    '/advisor-findings': { items: [] },
    '/cluster/status': { mode: 'ha', cluster_id: 'synthetic-cluster', incarnation: 'inc_acceptance', node_id: 'halro-0', role: 'primary', term: 7, promised_term: 7, durable_index: 13, confirmed_index: 12, applied_index: 11, startup_ready: true, projection: { index: 11, metadata_epoch: 2, metadata_sequence: 9 }, peers: [{ node_id: 'halro-1', connected: true }, { node_id: 'halro-2', connected: false }] },
    '/preferences': { locale, appearance, revision: 1 },
    '/settings': { health_probe_interval_seconds: 30, revision: 1, updated_at: CREATED },
    '/settings/ui': { default_locale: locale, revision: 1, updated_at: CREATED },
    '/settings/usage': { console_window_days: 30, presets: [7, 30, 90], min_days: 1, config_file_days: 30, config_file_in_effect: true, revision: 1, updated_at: CREATED },
    '/settings/accounting': { timezone: 'Asia/Singapore', timezone_version: 1, current_period: timeContext, config_file_timezone: 'Asia/Singapore', config_file_in_effect: true, tzdata: { source: 'embedded', version: '2026a', fingerprint: 'synthetic' }, revision: 1, updated_at: CREATED },
    '/system/config': { yaml: 'server:\n  gateway_listen: 127.0.0.1:8080\n  admin_listen: 127.0.0.1:8081\n', entries: [{ path: 'server.gateway_listen', title_zh: 'Gateway 监听地址', title_en: 'Gateway listen address', description_zh: 'Gateway 接收模型请求的监听地址。', description_en: 'Listener address for model requests.', value: '127.0.0.1:8080', kind: 'text' }, { path: 'tls.enabled', title_zh: 'Gateway TLS', title_en: 'Gateway TLS', description_zh: '是否启用 TLS。', description_en: 'Enables TLS.', value: 'false', kind: 'boolean' }], time_context: timeContext },
    '/model-catalog': { status: { enabled: false, state: 'disabled', source: 'bundled', revision: 'synthetic-catalog', sequence: 0, pinned_revision: '', last_attempt_at: '', last_success_at: '', degraded_since: '', error_class: '', using_expired_last_known_good: false }, bundled_revision: 'synthetic-catalog', effective_revision: 'synthetic-catalog', schema: { min_readable: 1, max_readable: 1 }, capability_dictionary_version: 1, trust_root_count: 0 },
    '/security/mfa': { enabled: false, policy: state.options.mfaSetupRequired ? 'required' : 'optional', required: state.options.mfaSetupRequired, authenticators: [], recovery_codes_remaining: 0 },
    '/admin-users': { users: [{ username: 'acceptance-admin', role: 'administrator' }, { username: 'acceptance-auditor', role: 'read_only' }] },
    '/master-key/custody': { mode: 'file', local_custody_ready: true, custody_state: 'healthy', production_admission: 'not_applicable', rotation_incomplete: false, lifecycle_operation: 'none', pending_slots: 0, retiring_slots: 0, recovery_verification_status: 'not_applicable', degraded_reasons: [], slots: [] },
    '/developer/config': { gateway_base_url: 'http://127.0.0.1:8080', enabled: true, max_request_bytes: 1048576 },
    '/usage/failures': pageOf(state.failures),
    '/usage': pageOf(state.attempts),
    '/audit': pageOf(state.audit),
    '/run-governance/work-units': pageOf(state.workUnits),
    '/run-governance/runs': pageOf(state.runs),
    '/governance/outcomes': pageOf(state.outcomes),
    '/governance/summary': { basis: 'work_unit_cohort', cohort_start: '2026-11-01', cohort_end: '2026-11-04', definition_id: 'odef_acceptance', definition_version: 1, generated_at: NOW, accounting_watermark: { generation: 1, sequence: 10, offset: 1000 }, governance_watermark: { sequence: 10, offset: 1000 }, eligible_units: 1, matured_units: 1, evaluated_units: 1, successful_units: 1, outcome_coverage: 1, success_rate: 1, known_cost_micros_usd: 125000, in_progress_cost_micros_usd: 0, estimated_cost_micros_usd: 0, unknown_attempts: 0, outcome_completeness: 'complete', cost_completeness: 'complete', cost_per_success_micros_usd: 125000 },
    '/onboarding/readiness': { version: 1, state: 'configuring', completed_goals: 1, total_goals: 4, evaluated_at: NOW, goals: [{ key: 'connect_provider', state: 'complete', detail_code: 'provider_ready', action_href: '/admin/providers' }, { key: 'publish_model', state: 'current', detail_code: 'deployment_missing', action_href: '/admin/deployments?intent=create&onboarding=first-request' }, { key: 'grant_access', state: 'blocked', detail_code: 'model_not_verified', action_href: '/admin/projects' }, { key: 'verify_request', state: 'blocked', detail_code: 'request_ready', action_href: '/admin/developer?onboarding=first-request' }] },
  };
  const collections = { '/credentials': 'credentials', '/providers': 'providers', '/deployments': 'deployments', '/routes': 'routes', '/projects': 'projects', '/token-guard-policies': 'policies', '/redaction-policies': 'redactions', '/alerts': 'alerts' };
  if (pathname === '/session' && auth === 'login') return error(401, 'session_required', 'Sign in required');
  if (method === 'GET' && Object.hasOwn(endpoints, pathname)) return ok(clone(endpoints[pathname]));
  if (method === 'GET' && pathname === '/usage/summary') return ok({ granularity: query.get('granularity') ?? 'day', start: '2026-11-01', end: '2026-11-04', totals: metrics, buckets: [{ ...metrics, period: '2026-11-04', start: timeContext.period_start, end: timeContext.period_end }], group_by: query.get('group_by') ?? '', groups: [{ ...metrics, key: 'project_acceptance' }], resource_labels: { project_acceptance: 'Production application' }, timezone_changes: [], watermark_sequence: 10, time_context: timeContext });
  if (method === 'GET' && collections[pathname]) return ok(pageOf(clone(state[collections[pathname]])));
  if (method === 'GET' && /^\/projects\/[^/]+\/keys$/.test(pathname)) return ok(pageOf(clone(state.keys.filter((key) => key.project_id === pathname.split('/')[2]))));
  if (method === 'GET' && /^\/projects\/[^/]+\/outcome-definitions$/.test(pathname)) return ok(pageOf(clone(state.definitions)));
  if (method === 'GET' && /^\/projects\/[^/]+$/.test(pathname)) return state.projects.some((item) => item.id === pathname.split('/')[2]) ? ok(clone(state.projects.find((item) => item.id === pathname.split('/')[2]))) : error(404, 'not_found', 'Synthetic project not found');
  if (method === 'GET' && /^\/deployments\/[^/]+\/prices$/.test(pathname)) return ok(pageOf(clone(state.prices)));
  if (method === 'GET' && /^\/deployments\/[^/]+\/price-proposals$/.test(pathname)) return ok(pageOf([]));
  if (method === 'GET' && /^\/providers\/[^/]+\/invocation-targets$/.test(pathname)) return ok(targetCatalog(state));
  if (method === 'POST' && /^\/providers\/[^/]+\/invocation-targets\/[^/]+\/resolution$/.test(pathname)) return ok(targetCatalog(state).items[0]);
  if (method === 'GET' && /^\/usage\/requests\/[^/]+$/.test(pathname)) return ok({ summary: { request_id: 'req_acceptance', project_id: 'project_acceptance', requested_model: 'chat', attempts: 1, input_tokens: 1000, output_tokens: 250, cost_micros_usd: 125000, unknown_attempts: 0, fallbacks: 0, outcome: 'success', accepted_at: CREATED, completed_at: NOW }, attempts: state.attempts });
  if (method !== 'GET' && role === 'read_only') return error(403, 'read_only', 'This synthetic account is read only');
  if (method === 'POST' && collections[pathname]) {
    const name = collections[pathname];
    const prefix = { credentials: 'credential', providers: 'provider', deployments: 'deployment', routes: 'route', projects: 'project', policies: 'tgp', redactions: 'rp', alerts: 'whk' }[name];
    const created = { ...clone(state[name][0] ?? {}), ...body, id: `${prefix}_created_${state.writes.length + 1}`, revision: 1, created_at: NOW, updated_at: NOW };
    for (const key of Object.keys(created)) if (key.startsWith('last_test')) delete created[key];
    if (name === 'providers') { created.bindings = [{ id: 'b-chat', profile_id: body.profile_id, enabled: true, capabilities: body.capabilities }]; created.capability_evidence = {}; }
    state[name].push(created);
    state.writes.push({ method, pathname, body: clone(body), id: created.id });
    return ok(created, 201);
  }
  if (method === 'POST' && /^\/projects\/[^/]+\/keys$/.test(pathname)) {
    const key = { id: `key_created_${state.writes.length + 1}`, project_id: pathname.split('/')[2], name: body.name, enabled: true, scopes: body.scopes ?? ['inference'], revision: 1, created_at: NOW };
    state.keys.push(key); state.writes.push({ method, pathname, id: key.id });
    return ok({ metadata: key, key: 'halro_synthetic_acceptance_not_a_real_key' }, 201);
  }
  return undefined;
}

// Every network request passes this guard, including assets. Only the selected
// localhost console origin may reach a server; Admin API responses are always
// intercepted, and undeclared routes fail visibly instead of returning {}.
export async function installConsoleFixture(page, options = {}) {
  const baseURL = options.baseURL ?? process.env.BASE_URL ?? 'http://127.0.0.1:5274/admin/';
  const origin = new URL(baseURL).origin;
  if (!['127.0.0.1', 'localhost', '[::1]'].includes(new URL(baseURL).hostname)) throw new Error('Acceptance only permits a localhost BASE_URL');
  const state = createFixtureState(options);
  state.requests = []; state.unknownRoutes = []; state.blockedRequests = [];
  await page.route('**/*', async (route) => {
    const request = route.request();
    const url = new URL(request.url());
    if (url.origin !== origin) {
      state.blockedRequests.push({ method: request.method(), url: request.url() });
      await route.abort('blockedbyclient'); return;
    }
    if (!url.pathname.startsWith('/admin/api/')) { await route.continue(); return; }
    const prefix = '/admin/api/v1';
    const pathname = url.pathname.slice(prefix.length);
    let body;
    try { body = request.postDataJSON(); } catch { body = undefined; }
    state.requests.push({ method: request.method(), pathname, query: url.search });
    const response = options.respond?.(state, request.method(), pathname, url.searchParams, body)
      ?? fixtureResponse(state, request.method(), pathname, url.searchParams, body);
    if (!response) {
      state.unknownRoutes.push({ method: request.method(), pathname, query: url.search });
      await route.fulfill({ status: 501, contentType: 'application/json', body: JSON.stringify({ error: `Acceptance fixture missing: ${request.method()} ${pathname}`, code: 'acceptance_fixture_missing' }) });
      return;
    }
    await route.fulfill({ status: response.status ?? 200, contentType: response.contentType ?? 'application/json', headers: response.headers ?? {}, body: response.body ?? JSON.stringify(response.data) });
  });
  return state;
}
