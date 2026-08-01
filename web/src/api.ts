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
  RedactionPolicy,
  RedactionTestResult,
  Route,
  RuntimeSettings,
  Session,
  SetupStatus,
  SystemStatus,
  TokenGuardPolicy,
  TokenGuardPreview,
  UsageAttempt,
} from "./types";

const API_ROOT = "/admin/api/v1";
let csrfToken = "";

export class ApiError extends Error {
  constructor(
    public readonly status: number,
    message: string,
    public readonly code = "",
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
      throw new ApiError(response.status, "服务端返回了无法解析的响应");
    }
  }
  if (!response.ok) {
    const error = payload as { error?: string; code?: string } | undefined;
    throw new ApiError(
      response.status,
      error?.error || `请求失败（${response.status}）`,
      error?.code,
    );
  }
  return { data: payload as T, etag: response.headers.get("ETag") || "" };
}

function json(method: string, value?: unknown): RequestInit {
  return { method, body: value === undefined ? undefined : JSON.stringify(value) };
}

export const api = {
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
    const result = await request<Session>(
      "/session/login",
      json("POST", { username, password }),
    );
    csrfToken = result.data.csrf_token;
    return result.data;
  },
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
  systemStatus: () =>
    request<SystemStatus>("/system/status").then((value) => value.data),
  settings: () => request<RuntimeSettings>("/settings"),
  updateSettings: (value: unknown, revision: number) =>
    request<RuntimeSettings>("/settings", json("PUT", value), `"${revision}"`),
  projects: () => request<Page<Project>>("/projects").then((value) => value.data),
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
  createKey: (projectID: string, name: string, expiresAt?: string) =>
    request<CreatedGatewayKey>(
      `/projects/${encodeURIComponent(projectID)}/keys`,
      json("POST", { name, ...(expiresAt ? { expires_at: expiresAt } : {}) }),
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
  createProvider: (value: unknown) =>
    request<Provider>("/providers", json("POST", value)),
  updateProvider: (id: string, value: unknown, revision: number) =>
    request<Provider>(
      `/providers/${encodeURIComponent(id)}`,
      json("PUT", value),
      `"${revision}"`,
    ),
  testProvider: (id: string) =>
    request<{ status: "healthy"; latency_ms: number }>(
      `/providers/${encodeURIComponent(id)}/test`,
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
    request<{ status: "healthy"; latency_ms: number }>(
      `/deployments/${encodeURIComponent(id)}/test`,
      json("POST"),
    ).then((value) => value.data),
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
    request<{ status: "healthy"; latency_ms: number }>(
      `/routes/${encodeURIComponent(id)}/test`,
      json("POST"),
    ).then((value) => value.data),
  usage: (query = "") =>
    request<Page<UsageAttempt>>(`/usage${query}`).then((value) => value.data),
  audit: () => request<Page<AuditRecord>>("/audit").then((value) => value.data),
  tokenGuardPolicies: () =>
    request<Page<TokenGuardPolicy>>("/token-guard-policies").then((value) => value.data),
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
    request<{ status: string }>(`/alerts/${encodeURIComponent(id)}/test`, json("POST")),
};

export function clearSensitiveClientState() {
  csrfToken = "";
}
