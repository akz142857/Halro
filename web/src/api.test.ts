import { afterEach, describe, expect, it, vi } from "vitest";
import { LIST_PAGE_CEILING, api, clearSensitiveClientState } from "./api";

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
    await api.createKey("prj_1", "service", "idem-canary", { currentPassword: "password-canary", totpCode: "" });

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

  it("follows every page of a listing rather than returning the first", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/routes?limit=100")) {
        return response({ items: [{ id: "rt_1", public_model: "chat" }], next_cursor: "rt_1" });
      }
      if (url.endsWith("/routes?limit=100&cursor=rt_1")) {
        return response({ items: [{ id: "rt_2", public_model: "embedding" }], next_cursor: "" });
      }
      return response({ error: "unexpected request" }, 500);
    });
    vi.stubGlobal("fetch", fetchMock);

    await expect(api.routes()).resolves.toEqual({
      items: [{ id: "rt_1", public_model: "chat" }, { id: "rt_2", public_model: "embedding" }],
      next_cursor: "",
    });
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  // Every list the console derives a fact from goes through the same follower.
  // Deployments is checked by name because it is the one that reports its own
  // length back to the operator as a total.
  it("follows every deployment page too", async () => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith("/deployments?limit=100")) {
        return response({ items: [{ id: "dep_1" }], next_cursor: "dep_1" });
      }
      if (url.endsWith("/deployments?limit=100&cursor=dep_1")) {
        return response({ items: [{ id: "dep_2" }], next_cursor: "" });
      }
      return response({ error: "unexpected request" }, 500);
    });
    vi.stubGlobal("fetch", fetchMock);

    await expect(api.deployments()).resolves.toEqual({
      items: [{ id: "dep_1" }, { id: "dep_2" }],
      next_cursor: "",
    });
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it.each([
    ["projects", () => api.projects()],
    ["credentials", () => api.credentials()],
    ["token-guard-policies", () => api.tokenGuardPolicies()],
    ["redaction-policies", () => api.redactionPolicies()],
  ])("follows every %s page used by finite selectors", async (resource, list) => {
    const fetchMock = vi.fn(async (input: RequestInfo | URL) => {
      const url = String(input);
      if (url.endsWith(`/${resource}?limit=100`)) {
        return response({ items: [{ id: "first" }], next_cursor: "first" });
      }
      if (url.endsWith(`/${resource}?limit=100&cursor=first`)) {
        return response({ items: [{ id: "second" }], next_cursor: "" });
      }
      return response({ error: "unexpected request" }, 500);
    });
    vi.stubGlobal("fetch", fetchMock);

    await expect(list()).resolves.toEqual({ items: [{ id: "first" }, { id: "second" }], next_cursor: "" });
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  // Callers turn these lists into a project's model authorization set and into
  // the count of enabled routes that disables a deployment's delete button, so
  // a truncated answer must never be handed back looking complete. A cursor
  // that repeats used to loop forever.
  it("fails closed instead of looping when the route cursor stops advancing", async () => {
    const fetchMock = vi.fn(async () =>
      response({ items: [{ id: "rt_1", public_model: "chat" }], next_cursor: "stuck" }));
    vi.stubGlobal("fetch", fetchMock);

    await expect(api.routes()).rejects.toMatchObject({ code: "listing_incomplete" });
    expect(fetchMock).toHaveBeenCalledTimes(2);
  });

  it("fails closed instead of paging past the listing ceiling", async () => {
    let page = 0;
    const fetchMock = vi.fn(async () => {
      page += 1;
      return response({ items: [{ id: `rt_${page}`, public_model: "chat" }], next_cursor: `rt_${page}` });
    });
    vi.stubGlobal("fetch", fetchMock);

    await expect(api.routes()).rejects.toMatchObject({ code: "listing_incomplete" });
    expect(fetchMock).toHaveBeenCalledTimes(LIST_PAGE_CEILING);
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
