import { useQuery } from "@tanstack/react-query";
import { api } from "./api";
import type { Session } from "./types";

// App already holds this query open, so every consumer reads the same cache
// entry rather than issuing its own request. The key and staleTime have to
// match App's for that to hold.
export function useSession(): Session | undefined {
  return useQuery({ queryKey: ["session"], queryFn: api.session, staleTime: 60_000 }).data;
}

// The console asks this only to avoid offering an action the server will
// refuse. Authorization itself is decided server-side on every request — a
// false here has never permitted anything.
export function useIsReadOnly(): boolean {
  return useSession()?.role === "read_only";
}

// Authenticated surfaces share the account's actual enrollment status with
// Settings. MFA policy alone does not mean an authenticator has been enrolled.
export function useMFAStatus(enabled = true) {
  return useQuery({ queryKey: ["mfa"], queryFn: api.mfaStatus, enabled, retry: false });
}
