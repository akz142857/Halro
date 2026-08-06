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
