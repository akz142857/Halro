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
