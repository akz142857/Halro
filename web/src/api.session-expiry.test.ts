import { afterEach, describe, expect, it, vi } from "vitest";
import { api, clearSensitiveClientState, subscribeSessionExpiry } from "./api";

const session = { username: "admin", role: "administrator", locale: "system", appearance: "dark", csrf_token: "new-csrf", absolute_expires_at: "x", idle_expires_at: "x" };
const response = (body: unknown, status = 200) => new Response(JSON.stringify(body), { status, headers: { "Content-Type": "application/json" } });

describe("Admin session expiry boundary", () => {
  afterEach(() => { clearSensitiveClientState(); vi.unstubAllGlobals(); });

  it("coalesces concurrent failures, pauses protected requests and resumes with fresh CSRF after login", async () => {
    const expired = vi.fn();
    const unsubscribe = subscribeSessionExpiry(expired);
    const fetch = vi.fn().mockImplementation(async () => response({ error: "admin authentication required" }, 401));
    vi.stubGlobal("fetch", fetch);
    try {
      await Promise.allSettled([api.alerts(), api.audit()]);
      expect(expired).toHaveBeenCalledOnce();
      const count = fetch.mock.calls.length;
      await expect(api.createProject({}, "synthetic-idempotency")).rejects.toMatchObject({ status: 401 });
      expect(fetch).toHaveBeenCalledTimes(count);
      fetch.mockResolvedValueOnce(response(session)).mockResolvedValueOnce(response({}));
      await api.login("admin", "password");
      await api.createProject({}, "synthetic-idempotency");
      expect(new Headers(fetch.mock.calls.at(-1)?.[1]?.headers).get("X-CSRF-Token")).toBe("new-csrf");
      expect(expired).toHaveBeenCalledOnce();
    } finally { unsubscribe(); }
  });

  it.each([
    [401, { error: "invalid credentials" }],
    [401, { error: "invalid current password" }],
    [401, { error: "recent re-authentication required", code: "recent_reauth_required" }],
    [401, { error: "invalid code", code: "mfa_invalid_code" }],
    [403, { error: "read only", code: "read_only_role" }],
    [503, { error: "unavailable" }],
  ])("does not expire an Admin session for %s %j", async (status, body) => {
    const expired = vi.fn();
    const unsubscribe = subscribeSessionExpiry(expired);
    vi.stubGlobal("fetch", vi.fn(async () => response(body, status)));
    try { await expect(api.alerts()).rejects.toMatchObject({ status }); expect(expired).not.toHaveBeenCalled(); }
    finally { unsubscribe(); }
  });

  it("ignores a delayed 401 from a previous session after a successful sign-in", async () => {
    let finish!: (response: Response) => void;
    const expired = vi.fn();
    const unsubscribe = subscribeSessionExpiry(expired);
    vi.stubGlobal("fetch", vi.fn().mockImplementationOnce(() => new Promise<Response>((resolve) => { finish = resolve; })).mockResolvedValueOnce(response(session)));
    try {
      const oldRead = api.alerts();
      await api.login("admin", "password");
      finish(response({ error: "admin authentication required" }, 401));
      await expect(oldRead).rejects.toMatchObject({ status: 401 });
      expect(expired).not.toHaveBeenCalled();
    } finally { unsubscribe(); }
  });

  it("does not interrupt an explicit sign-out with a recovery screen", async () => {
    const expired = vi.fn();
    const unsubscribe = subscribeSessionExpiry(expired);
    vi.stubGlobal("fetch", vi.fn(async () => response({ error: "admin authentication required" }, 401)));
    try { await expect(api.logout()).rejects.toMatchObject({ status: 401 }); expect(expired).not.toHaveBeenCalled(); }
    finally { unsubscribe(); }
  });

  it("does not replace new CSRF with a delayed session snapshot", async () => {
    let finish!: (response: Response) => void;
    const fetch = vi.fn().mockImplementationOnce(() => new Promise<Response>((resolve) => { finish = resolve; }))
      .mockResolvedValueOnce(response(session)).mockResolvedValueOnce(response({}));
    vi.stubGlobal("fetch", fetch);
    const staleSnapshot = api.session();
    await api.login("admin", "password");
    finish(response({ ...session, csrf_token: "old-csrf" }));
    await staleSnapshot;
    await api.createProject({}, "synthetic-idempotency");
    expect(new Headers(fetch.mock.calls.at(-1)?.[1]?.headers).get("X-CSRF-Token")).toBe("new-csrf");
  });

  it.each([false, true])("distinguishes forwarded Gateway rejection from Admin expiry (admin=%s) without consuming the response", async (admin) => {
    const expired = vi.fn();
    const unsubscribe = subscribeSessionExpiry(expired);
    const body = admin ? { error: "admin authentication required" } : { error: { code: "invalid_api_key", message: "Invalid gateway key" } };
    vi.stubGlobal("fetch", vi.fn(async () => response(body, 401)));
    try {
      const result = await api.developerExecute("chat", "synthetic-key", {}, false, new AbortController().signal);
      expect(await result.json()).toEqual(body);
      expect(expired).toHaveBeenCalledTimes(admin ? 1 : 0);
    } finally { unsubscribe(); }
  });
});
