import { afterEach, describe, expect, it, vi } from "vitest";
import { api, clearSensitiveClientState } from "./api";

describe("typed admin API client", () => {
  afterEach(() => {
    clearSensitiveClientState();
    vi.unstubAllGlobals();
  });

  it("keeps CSRF in memory and sends mutations as same-origin no-store requests", async () => {
    const calls: Array<[RequestInfo | URL, RequestInit | undefined]> = [];
    const fetchMock = vi.fn(async (input: RequestInfo | URL, init?: RequestInit) => {
      calls.push([input, init]);
      if (String(input).endsWith("/session/login")) {
        return response({
          username: "admin",
          locale: "system",
          csrf_token: "csrf-canary",
          absolute_expires_at: "2026-01-01T00:00:00Z",
          idle_expires_at: "2026-01-01T00:00:00Z",
        });
      }
      if (String(input).endsWith("/session/password")) {
        return response({
          username: "admin",
          locale: "system",
          csrf_token: "csrf-rotated-canary",
          absolute_expires_at: "2026-01-01T00:00:00Z",
          idle_expires_at: "2026-01-01T00:00:00Z",
        });
      }
      return response({
        key: "gw_plaintext-canary",
        metadata: {
          id: "key_1",
          project_id: "prj_1",
          name: "service",
          enabled: true,
          created_at: "2026-01-01T00:00:00Z",
          revision: 1,
        },
      }, 201);
    });
    vi.stubGlobal("fetch", fetchMock);

    await api.login("admin", "password-canary");
    await api.changePassword("password-canary", "new-password-canary");
    await api.createKey("prj_1", "service", "idem-canary");

    const passwordMutation = calls[1][1]!;
    expect((passwordMutation.headers as Headers).get("X-CSRF-Token")).toBe("csrf-canary");
    const mutation = calls[2][1]!;
    const headers = mutation.headers as Headers;
    expect(mutation.credentials).toBe("same-origin");
    expect(mutation.cache).toBe("no-store");
    expect(headers.get("X-CSRF-Token")).toBe("csrf-rotated-canary");
    // The per-dialog token must survive alongside the CSRF header the client injects,
    // or a retried create would mint a second key whose plaintext is never shown.
    expect(headers.get("Idempotency-Key")).toBe("idem-canary");
    expect(JSON.stringify(calls)).not.toContain("localStorage");
  });
});

function response(payload: unknown, status = 200) {
  return new Response(JSON.stringify(payload), {
    status,
    headers: { "Content-Type": "application/json" },
  });
}

describe("destructive deletes carry step-up material", () => {
  afterEach(() => {
    clearSensitiveClientState();
    vi.unstubAllGlobals();
  });

  // One case per endpoint rather than one representative: the body is written
  // per method, so a method that forgets it is exactly what this has to catch.
  const deletes: Array<[string, (reauth: { currentPassword: string; totpCode: string }) => Promise<unknown>]> = [
    ["/projects/prj_1", (r) => api.deleteProject("prj_1", '"1"', r)],
    ["/projects/prj_1/keys/key_1", (r) => api.deleteKey("prj_1", "key_1", 1, r)],
    ["/credentials/cred_1", (r) => api.deleteCredential("cred_1", 1, r)],
    ["/providers/prov_1", (r) => api.deleteProvider("prov_1", 1, r)],
    ["/deployments/dep_1", (r) => api.deleteDeployment("dep_1", 1, r)],
    ["/routes/route_1", (r) => api.deleteRoute("route_1", 1, r)],
    ["/token-guard-policies/tg_1", (r) => api.deleteTokenGuardPolicy("tg_1", 1, r)],
    ["/redaction-policies/red_1", (r) => api.deleteRedactionPolicy("red_1", 1, r)],
    ["/alerts/alert_1", (r) => api.deleteAlert("alert_1", 1, r)],
  ];

  it.each(deletes)("sends the operator's credentials when deleting %s", async (path, call) => {
    let sent: RequestInit | undefined;
    vi.stubGlobal("fetch", vi.fn(async (_input: RequestInfo | URL, init?: RequestInit) => {
      sent = init;
      return new Response(null, { status: 204 });
    }));

    await call({ currentPassword: "correct horse battery staple", totpCode: "123456" });

    expect(sent?.method).toBe("DELETE");
    expect(sent?.body).toBeTruthy();
    expect(JSON.parse(String(sent?.body))).toEqual({
      current_password: "correct horse battery staple",
      totp_code: "123456",
    });
    expect(path).toBeTruthy();
  });
});
