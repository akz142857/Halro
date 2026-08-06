import type {
  AuditRecord,
  AlertWebhook,
  CreatedGatewayKey,
  Credential,
  Dashboard,
  Deployment,
  GatewayKey,
  Page,
  Project,
  Provider,
  ProviderModelCatalog,
  RedactionPolicy,
  RedactionTestResult,
  Route,
  RuntimeSettings,
  UIBootstrap,
  InstanceUISettings,
  AccountingSettings,
  AdminPreferences,
  LocalePreference,
  Appearance,
  SupportedLocale,
  Session,
  MFAChallenge,
  MFAStatus,
  MFAEnrollment,
	MasterKeyCustody,
  SetupStatus,
  SystemStatus,
  TokenGuardPolicy,
  TokenGuardPreview,
  UsageAttempt,
  DeploymentPriceVersion,
  DeploymentPriceProposal,
  CostAdjustmentPreview,
} from "./types";

const API_ROOT = "/admin/api/v1";
let csrfToken = "";

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
    public readonly code = "",
    // What the far side actually answered, when a handler forwards it. A webhook endpoint
    // that rejects a payload explains itself in its body, not in our error string.
    public readonly detail = "",
  ) {
    super(message);
  }
}

export interface ApiResult<T> {
  data: T;
  etag: string;
}

async function request<T>(
  path: string,
  init: RequestInit = {},
  etag = "",
): Promise<ApiResult<T>> {
  const headers = new Headers(init.headers);
  headers.set("Accept", "application/json");
  if (init.body) headers.set("Content-Type", "application/json");
  if (csrfToken && init.method && init.method !== "GET") {
    headers.set("X-CSRF-Token", csrfToken);
  }
  if (etag) headers.set("If-Match", etag);
  const response = await fetch(`${API_ROOT}${path}`, {
    ...init,
    headers,
    credentials: "same-origin",
    cache: "no-store",
  });
  const text = await response.text();
  let payload: unknown = undefined;
  if (text) {
    try {
      payload = JSON.parse(text);
    } catch {
      throw new ApiError(response.status, "server returned an invalid response", "invalid_response");
    }
  }
  if (!response.ok) {
    const error = payload as { error?: string; code?: string; response?: string } | undefined;
    throw new ApiError(
      response.status,
      error?.error || `request failed (${response.status})`,
      error?.code,
      error?.response,
    );
  }
  return { data: payload as T, etag: response.headers.get("ETag") || "" };
}

function json(method: string, value?: unknown): RequestInit {
  return { method, body: value === undefined ? undefined : JSON.stringify(value) };
}

export const api = {
  uiBootstrap: () => request<UIBootstrap>("/ui/bootstrap").then((result) => result.data),
  setupStatus: () =>
    request<SetupStatus>("/setup/status").then((value) => value.data),
  async setupAdmin(
    username: string,
    password: string,
    passwordConfirmation: string,
    setupToken: string,
  ) {
    const result = await request<Session>(
      "/setup/admin",
      json("POST", {
        username,
        password,
        password_confirmation: passwordConfirmation,
        setup_token: setupToken,
      }),
    );
    csrfToken = result.data.csrf_token;
    return result.data;
  },
  async login(username: string, password: string) {
    const result = await request<Session | MFAChallenge>(
      "/session/login",
      json("POST", { username, password }),
    );
	if ("csrf_token" in result.data) csrfToken = result.data.csrf_token;
    return result.data;
  },
  async completeMFA(challengeToken: string, code: string) {
    const result = await request<Session>("/session/mfa/totp", json("POST", { challenge_token: challengeToken, code })); csrfToken = result.data.csrf_token; return result.data;
  },
  async completeMFARecovery(challengeToken: string, recoveryCode: string) {
    const result = await request<Session>("/session/mfa/recovery-code", json("POST", { challenge_token: challengeToken, recovery_code: recoveryCode })); csrfToken = result.data.csrf_token; return result.data;
  },
  cancelMFAChallenge: (challengeToken:string) => request<{status:string}>("/session/mfa/challenge",json("DELETE",{challenge_token:challengeToken})).then((v)=>v.data),
  mfaStatus: () => request<MFAStatus>("/security/mfa").then((v) => v.data),
  createMFAAuthenticator: (name: string, currentPassword: string, code: string) => request<MFAEnrollment>("/security/mfa/authenticators", json("POST", { name, current_password: currentPassword, code })).then((v) => v.data),
  confirmMFAAuthenticator: (id: string, code: string) => request<{status:string; recovery_codes?:string[]}>(`/security/mfa/authenticators/${encodeURIComponent(id)}/confirm`, json("POST", {code})).then((v)=>v.data),
  cancelPendingMFAAuthenticator: (id:string) => request<{status:string}>(`/security/mfa/authenticators/${encodeURIComponent(id)}/pending`,json("DELETE")).then((v)=>v.data),
  renameMFAAuthenticator: (id:string,name:string,revision:number) => request<{status:string}>(`/security/mfa/authenticators/${encodeURIComponent(id)}`,json("PATCH",{name}),`\"${revision}\"`).then((v)=>v.data),
  deleteMFAAuthenticator: (id: string, currentPassword: string, code: string) => request<{status:string}>(`/security/mfa/authenticators/${encodeURIComponent(id)}`, json("DELETE", {current_password:currentPassword,code})).then((v)=>v.data),
  regenerateMFARecoveryCodes: (currentPassword:string,code:string) => request<{recovery_codes:string[]}>("/security/mfa/recovery-codes/regenerate",json("POST",{current_password:currentPassword,code})).then((v)=>v.data),
  disableMFA: (currentPassword:string,code:string) => request<{status:string}>("/security/mfa",json("DELETE",{current_password:currentPassword,code})).then((v)=>v.data),
  async session() {
    const result = await request<Session>("/session");
    csrfToken = result.data.csrf_token;
    return result.data;
  },
  async logout() {
    await request<{ status: string }>("/session/logout", json("POST"));
    csrfToken = "";
  },
  async changePassword(currentPassword: string, newPassword: string) {
    const result = await request<Session>(
      "/session/password",
      json("POST", { current_password: currentPassword, new_password: newPassword }),
    );
    csrfToken = result.data.csrf_token;
    return result.data;
  },
  dashboard: () => request<Dashboard>("/dashboard").then((value) => value.data),
	masterKeyCustody: () => request<MasterKeyCustody>("/master-key/custody").then((value) => value.data),
  systemStatus: () =>
    request<SystemStatus>("/system/status").then((value) => value.data),
  settings: () => request<RuntimeSettings>("/settings"),
  updateSettings: (value: unknown, revision: number) =>
    request<RuntimeSettings>("/settings", json("PUT", value), `"${revision}"`),
  uiSettings: () => request<InstanceUISettings>("/settings/ui"),
  updateUISettings: (defaultLocale: SupportedLocale, revision: number) =>
    request<InstanceUISettings>("/settings/ui", json("PUT", { default_locale: defaultLocale }), `"${revision}"`),
  accountingSettings: () => request<AccountingSettings>("/settings/accounting"),
  // The reset window after a switch is a server judgement: the client must
  // never derive a period boundary of its own.
  previewAccountingTimezone: (timezone: string) =>
    request<AccountingSettings>(`/settings/accounting?preview_timezone=${encodeURIComponent(timezone)}`),
  // Scheduling a timezone change, not applying one: the server pins it to the
  // end of the period in progress so a day already being billed never moves.
  scheduleAccountingTimezone: (timezone: string, revision: number) =>
    request<AccountingSettings>("/settings/accounting", json("PUT", { timezone }), `"${revision}"`),
  cancelAccountingTimezoneChange: (revision: number) =>
    request<AccountingSettings>("/settings/accounting/pending", { method: "DELETE" }, `"${revision}"`),
  preferences: () => request<AdminPreferences>("/preferences"),
  // The complete writable preference resource must be sent so updating one
  // field never clears another (PRD §4.4). Callers pass their current confirmed
  // values for the fields they are not changing.
  updatePreferences: (preferences: { locale: LocalePreference; appearance: Appearance }, revision: number) =>
    request<AdminPreferences>("/preferences", json("PUT", preferences), `"${revision}"`),
  projects: () => request<Page<Project>>("/projects").then((value) => value.data),
  projectsPage: (query = "") =>
    request<Page<Project>>(`/projects${query}`).then((value) => value.data),
  project: (id: string) => request<Project>(`/projects/${encodeURIComponent(id)}`),
  createProject: (value: unknown) =>
    request<Project>("/projects", json("POST", value)),
  updateProject: (id: string, value: unknown, etag: string) =>
    request<Project>(`/projects/${encodeURIComponent(id)}`, json("PUT", value), etag),
  deleteProject: (id: string, etag: string) =>
    request<void>(`/projects/${encodeURIComponent(id)}`, json("DELETE"), etag),
  unblockProject: (id: string) =>
    request<{ status: "unblocked"; subjects: number }>(
      `/projects/${encodeURIComponent(id)}/unblock`,
      json("POST"),
    ).then((value) => value.data),
  keys: (projectID: string) =>
    request<Page<GatewayKey>>(`/projects/${encodeURIComponent(projectID)}/keys`)
      .then((value) => value.data),
  keysPage: (projectID: string, query = "") =>
    request<Page<GatewayKey>>(`/projects/${encodeURIComponent(projectID)}/keys${query}`)
      .then((value) => value.data),
  // The plaintext is returned once and never again, so a retried create must carry the
  // same idempotency key: the server then refuses to mint a second unaccounted credential.
  createKey: (projectID: string, name: string, idempotencyKey: string, expiresAt?: string) =>
    request<CreatedGatewayKey>(
      `/projects/${encodeURIComponent(projectID)}/keys`,
      {
        ...json("POST", { name, ...(expiresAt ? { expires_at: expiresAt } : {}) }),
        headers: { "Idempotency-Key": idempotencyKey },
      },
    ),
  updateKey: (
    projectID: string,
    keyID: string,
    value: { name: string; enabled: boolean; expires_at?: string },
    revision: number,
  ) =>
    request<GatewayKey>(
      `/projects/${encodeURIComponent(projectID)}/keys/${encodeURIComponent(keyID)}`,
      json("PUT", value),
      `"${revision}"`,
    ),
  deleteKey: (projectID: string, keyID: string, revision: number) =>
    request<void>(
      `/projects/${encodeURIComponent(projectID)}/keys/${encodeURIComponent(keyID)}`,
      json("DELETE"),
      `"${revision}"`,
    ),
  credentials: () =>
    request<Page<Credential>>("/credentials").then((value) => value.data),
  createCredential: (value: unknown) =>
    request<Credential>("/credentials", json("POST", value)),
  rotateCredential: (id: string, value: unknown, revision: number) =>
    request<Credential>(
      `/credentials/${encodeURIComponent(id)}`,
      json("PUT", value),
      `"${revision}"`,
    ),
  deleteCredential: (id: string, revision: number) =>
    request<void>(
      `/credentials/${encodeURIComponent(id)}`,
      json("DELETE"),
      `"${revision}"`,
    ),
  providers: () => request<Page<Provider>>("/providers").then((value) => value.data),
  providerModels: (id: string, bindingID = "", refresh = false) => {
    const query = new URLSearchParams();
    if (bindingID) query.set("binding_id", bindingID);
    if (refresh) query.set("refresh", "true");
    const suffix = query.size ? `?${query.toString()}` : "";
    return request<ProviderModelCatalog>(`/providers/${encodeURIComponent(id)}/models${suffix}`).then((value) => value.data);
  },
  createProvider: (value: unknown) =>
    request<Provider>("/providers", json("POST", value)),
  updateProvider: (id: string, value: unknown, revision: number) =>
    request<Provider>(
      `/providers/${encodeURIComponent(id)}`,
      json("PUT", value),
      `"${revision}"`,
    ),
  testProvider: (id: string, bindingID?: string) =>
    request<{ status: "healthy"; latency_ms: number; tested_at: string; revision: number; healthy_targets: number; total_targets: number }>(
      `/providers/${encodeURIComponent(id)}/test${bindingID ? `?binding_id=${encodeURIComponent(bindingID)}` : ""}`,
      json("POST"),
    ).then((value) => value.data),
  deleteProvider: (id: string, revision: number) =>
    request<void>(
      `/providers/${encodeURIComponent(id)}`,
      json("DELETE"),
      `"${revision}"`,
    ),
  deployments: () =>
    request<Page<Deployment>>("/deployments").then((value) => value.data),
  developerConfig: () =>
    request<{ gateway_base_url: string; enabled?: boolean }>("/developer/config").then((value) => value.data),
  developerExecute: (endpoint: string, gatewayKey: string, value: unknown, streaming: boolean, signal: AbortSignal) => {
    const headers = new Headers({
      "Accept": streaming ? "text/event-stream" : "application/json",
      "Authorization": `Bearer ${gatewayKey}`,
      "Content-Type": "application/json",
    });
    if (csrfToken) headers.set("X-CSRF-Token", csrfToken);
    return fetch(`${API_ROOT}/developer/execute/${encodeURIComponent(endpoint)}`, {
      method: "POST",
      body: JSON.stringify(value),
      headers,
      credentials: "same-origin",
      cache: "no-store",
      signal,
    });
  },
  createDeployment: (value: unknown) =>
    request<Deployment>("/deployments", json("POST", value)),
  updateDeployment: (id: string, value: unknown, revision: number) =>
    request<Deployment>(
      `/deployments/${encodeURIComponent(id)}`,
      json("PUT", value),
      `"${revision}"`,
    ),
  deleteDeployment: (id: string, revision: number) =>
    request<void>(
      `/deployments/${encodeURIComponent(id)}`,
      json("DELETE"),
      `"${revision}"`,
    ),
  testDeployment: (id: string) =>
    request<{ status: "healthy"; latency_ms: number; tested_at: string; revision: number }>(
      `/deployments/${encodeURIComponent(id)}/test`,
      json("POST"),
    ).then((value) => value.data),
  deploymentPrices: (id: string) => request<Page<DeploymentPriceVersion>>(`/deployments/${encodeURIComponent(id)}/prices`).then((value) => value.data),
  createDeploymentPrice: (id: string, value: unknown, idempotencyKey: string) =>
    request<DeploymentPriceVersion>(`/deployments/${encodeURIComponent(id)}/prices`, { ...json("POST", value), headers: { "Idempotency-Key": idempotencyKey } }).then((result) => result.data),
  cancelDeploymentPrice: (deploymentID: string, priceID: string, revision: number) =>
    request<DeploymentPriceVersion>(`/deployments/${encodeURIComponent(deploymentID)}/prices/${encodeURIComponent(priceID)}/cancel`, json("POST"), `"${revision}"`).then((result) => result.data),
	confirmRestoredDeploymentPricing: (deploymentID: string, value: unknown) => request<{ deployment_id: string; pricing_quarantine: false }>(`/deployments/${encodeURIComponent(deploymentID)}/prices/restore-confirm`, json("POST", value)).then((result) => result.data),
  deploymentPriceProposals: (id: string) => request<Page<DeploymentPriceProposal>>(`/deployments/${encodeURIComponent(id)}/price-proposals`).then((value) => value.data),
  createDeploymentPriceProposal: (id: string, value: unknown, idempotencyKey: string) =>
    request<DeploymentPriceProposal>(`/deployments/${encodeURIComponent(id)}/price-proposals`, { ...json("POST", value), headers: { "Idempotency-Key": idempotencyKey } }).then((result) => result.data),
  adoptDeploymentPriceProposal: (deploymentID: string, proposalID: string, revision: number, value: unknown) =>
    request<{ proposal: DeploymentPriceProposal; price_version: DeploymentPriceVersion }>(`/deployments/${encodeURIComponent(deploymentID)}/price-proposals/${encodeURIComponent(proposalID)}/adopt`, json("POST", value), `"${revision}"`).then((result) => result.data),
  rejectDeploymentPriceProposal: (deploymentID: string, proposalID: string, revision: number) =>
    request<DeploymentPriceProposal>(`/deployments/${encodeURIComponent(deploymentID)}/price-proposals/${encodeURIComponent(proposalID)}/reject`, json("POST"), `"${revision}"`).then((result) => result.data),
  routes: () => request<Page<Route>>("/routes").then((value) => value.data),
  createRoute: (value: unknown) => request<Route>("/routes", json("POST", value)),
  updateRoute: (id: string, value: unknown, revision: number) =>
    request<Route>(
      `/routes/${encodeURIComponent(id)}`,
      json("PUT", value),
      `"${revision}"`,
    ),
  deleteRoute: (id: string, revision: number) =>
    request<void>(`/routes/${encodeURIComponent(id)}`, json("DELETE"), `"${revision}"`),
  testRoute: (id: string) =>
    request<{ status: "healthy"; latency_ms: number; tested_at: string; revision: number }>(
      `/routes/${encodeURIComponent(id)}/test`,
      json("POST"),
    ).then((value) => value.data),
  usage: (query = "") =>
    request<Page<UsageAttempt>>(`/usage${query}`).then((value) => value.data),
  usageRequest: (requestID: string) =>
    request<unknown>(`/usage/requests/${encodeURIComponent(requestID)}`).then((value) => value.data),
  previewCostAdjustment: (attemptID: string, value: unknown) => request<CostAdjustmentPreview>(`/usage/attempts/${encodeURIComponent(attemptID)}/cost-adjustments/preview`, json("POST", value)).then((result) => result.data),
  createCostAdjustment: (attemptID: string, value: unknown, idempotencyKey: string) => request<{ adjustment: unknown; budget_overage_micros_usd?: number }>(`/usage/attempts/${encodeURIComponent(attemptID)}/cost-adjustments`, { ...json("POST", value), headers: { "Idempotency-Key": idempotencyKey } }).then((result) => result.data),
  audit: (query = "") => request<Page<AuditRecord>>(`/audit${query}`).then((value) => value.data),
  tokenGuardPolicies: () =>
    request<Page<TokenGuardPolicy>>("/token-guard-policies").then((value) => value.data),
  tokenGuardPoliciesPage: (query = "") =>
    request<Page<TokenGuardPolicy>>(`/token-guard-policies${query}`).then((value) => value.data),
  createTokenGuardPolicy: (value: unknown) =>
    request<TokenGuardPolicy>("/token-guard-policies", json("POST", value)),
  updateTokenGuardPolicy: (id: string, value: unknown, revision: number) =>
    request<TokenGuardPolicy>(
      `/token-guard-policies/${encodeURIComponent(id)}`,
      json("PUT", value),
      `"${revision}"`,
    ),
  deleteTokenGuardPolicy: (id: string, revision: number) =>
    request<void>(
      `/token-guard-policies/${encodeURIComponent(id)}`,
      json("DELETE"),
      `"${revision}"`,
    ),
  previewTokenGuardPolicy: (id: string, value: unknown) =>
    request<TokenGuardPreview>(
      `/token-guard-policies/${encodeURIComponent(id)}/test`,
      json("POST", value),
    ).then((result) => result.data),
  redactionPolicies: () =>
    request<Page<RedactionPolicy>>("/redaction-policies").then((value) => value.data),
  redactionPoliciesPage: (query = "") =>
    request<Page<RedactionPolicy>>(`/redaction-policies${query}`).then((value) => value.data),
  createRedactionPolicy: (value: unknown) =>
    request<RedactionPolicy>("/redaction-policies", json("POST", value)),
  updateRedactionPolicy: (id: string, value: unknown, revision: number) =>
    request<RedactionPolicy>(
      `/redaction-policies/${encodeURIComponent(id)}`,
      json("PUT", value),
      `"${revision}"`,
    ),
  deleteRedactionPolicy: (id: string, revision: number) =>
    request<void>(
      `/redaction-policies/${encodeURIComponent(id)}`,
      json("DELETE"),
      `"${revision}"`,
    ),
  testRedactionPolicy: (id: string, value: unknown) =>
    request<RedactionTestResult>(
      `/redaction-policies/${encodeURIComponent(id)}/test`,
      json("POST", value),
    ).then((result) => result.data),
  alerts: () => request<Page<AlertWebhook>>("/alerts").then((value) => value.data),
  alertsPage: (query = "") =>
    request<Page<AlertWebhook>>(`/alerts${query}`).then((value) => value.data),
  createAlert: (value: unknown) =>
    request<AlertWebhook>("/alerts", json("POST", value)),
  updateAlert: (id: string, value: unknown, revision: number) =>
    request<AlertWebhook>(
      `/alerts/${encodeURIComponent(id)}`,
      json("PUT", value),
      `"${revision}"`,
    ),
  deleteAlert: (id: string, revision: number) =>
    request<void>(`/alerts/${encodeURIComponent(id)}`, json("DELETE"), `"${revision}"`),
  testAlert: (id: string) =>
    request<{ status: string; latency_ms: number; status_code: number; response: string }>(
      `/alerts/${encodeURIComponent(id)}/test`,
      json("POST"),
    ).then((result) => result.data),
};

export function clearSensitiveClientState() {
  csrfToken = "";
}
