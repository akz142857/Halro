import { useQuery } from "@tanstack/react-query";
import { api } from "../api";
import type {
  ProviderCapabilities,
  ProviderProfileDescriptor,
  ProviderProfilesCatalog,
  ProviderType,
} from "../types";

/** The provider matrix, fetched once and kept.
 *
 * It is compile-time data on the server plus one config value, so it cannot
 * change while a session is open. There is deliberately no fallback: a form
 * built from a guess is how the console and the server drifted apart in the
 * first place, and a wrong ceiling either hides a capability that works or
 * offers one whose save is refused without saying which. Callers wait, or show
 * the failure. */
export function useProviderProfiles() {
  return useQuery({
    queryKey: ["provider-profiles"],
    queryFn: api.providerProfiles,
    staleTime: Infinity,
    gcTime: Infinity,
  });
}

export const emptyCapabilities: ProviderCapabilities = {
  chat: false, streaming: false, embeddings: false, moderations: false,
  images: false, transcriptions: false, speech: false, files: false,
  batches: false, rerank: false, async_generate: false, tools: false,
  vision: false, json_mode: false, developer_role: false, reasoning: false,
  stream_usage: false, provider_executed_tools: false,
  max_context_tokens: 0, max_output_tokens: 0,
};

/** Capability keys, excluding the two numeric limits, which are not checkboxes. */
export function booleanCapabilityNames(catalog: ProviderProfilesCatalog): (keyof ProviderCapabilities)[] {
  return catalog.capability_names.filter(
    (name) => name !== "max_context_tokens" && name !== "max_output_tokens",
  ) as (keyof ProviderCapabilities)[];
}

export function profilesForType(catalog: ProviderProfilesCatalog, type: ProviderType): ProviderProfileDescriptor[] {
  return catalog.provider_types.find((entry) => entry.type === type)?.profiles ?? [];
}

export function findProfile(
  catalog: ProviderProfilesCatalog,
  type: ProviderType,
  profileID: string,
): ProviderProfileDescriptor | undefined {
  return profilesForType(catalog, type).find((profile) => profile.id === profileID);
}

export function defaultProfileID(catalog: ProviderProfilesCatalog, type: ProviderType): string {
  return catalog.provider_types.find((entry) => entry.type === type)?.default_profile_id ?? "";
}

/** Profiles a connection of this type may combine.
 *
 * The server requires every binding to match the connection's own surface and
 * credential scheme, so only profiles sharing those can sit on one connection.
 * Deriving it from the descriptors keeps this rule in one place — the server's —
 * rather than restating which profiles happen to go together. */
export function combinableProfiles(
  catalog: ProviderProfilesCatalog,
  type: ProviderType,
  profileID: string,
): ProviderProfileDescriptor[] {
  const anchor = findProfile(catalog, type, profileID);
  if (!anchor) return [];
  return profilesForType(catalog, type).filter(
    (profile) =>
      profile.access_surface === anchor.access_surface &&
      profile.credential_scheme === anchor.credential_scheme,
  );
}

function unionOf(profiles: ProviderProfileDescriptor[], key: "ceiling" | "defaults"): ProviderCapabilities {
  const union = { ...emptyCapabilities };
  for (const profile of profiles) {
    for (const name of Object.keys(union) as (keyof ProviderCapabilities)[]) {
      const value = profile[key][name];
      if (typeof value === "number") {
        union[name] = Math.max(union[name] as number, value) as never;
      } else if (value) {
        union[name] = true as never;
      }
    }
  }
  return union;
}

/** What an operator may turn on for this connection: the union over every profile
 * it can combine. For a type with one profile that is just its ceiling. */
export function connectionCeiling(
  catalog: ProviderProfilesCatalog,
  type: ProviderType,
  profileID: string,
): ProviderCapabilities {
  return unionOf(combinableProfiles(catalog, type, profileID), "ceiling");
}

/** What a new connection starts with: the union of the same profiles' defaults. */
export function connectionDefaults(
  catalog: ProviderProfilesCatalog,
  type: ProviderType,
  profileID: string,
): ProviderCapabilities {
  return unionOf(combinableProfiles(catalog, type, profileID), "defaults");
}

/** Applies the server's chat dependency so the form cannot offer a combination
 * the save will refuse. */
export function updateCapabilitySelection(
  catalog: ProviderProfilesCatalog,
  current: ProviderCapabilities,
  capability: keyof ProviderCapabilities,
  enabled: boolean,
): ProviderCapabilities {
  const next = { ...current, [capability]: enabled };
  const dependents = catalog.capability_requires_chat as (keyof ProviderCapabilities)[];
  if (capability === "chat" && !enabled) {
    for (const dependent of dependents) next[dependent] = false as never;
  } else if (enabled && dependents.includes(capability)) {
    next.chat = true;
  }
  return next;
}

/** Splits one capability set across the profiles that will carry it.
 *
 * Each capability goes to the profile whose ceiling admits it. Where the
 * operator has named a profile — Bedrock, whose form asks which implementation
 * to use — there is one candidate and the split is the identity. Where they have
 * not, as with OpenAI, the candidates' ceilings are disjoint, so each capability
 * has exactly one home.
 *
 * The numeric limits follow the profile that carries chat, or the only profile
 * that bounds them; a profile whose ceiling leaves them unbounded and serves no
 * chat has nothing to apply them to.
 *
 * Anything the operator enabled that no candidate can serve is returned in
 * `unservable` rather than dropped. The server refuses such a request, and a
 * form that quietly discarded it would be showing a saved connection that does
 * less than what was ticked. */
export function splitCapabilitiesAcrossProfiles(
  catalog: ProviderProfilesCatalog,
  type: ProviderType,
  profileID: string,
  capabilities: ProviderCapabilities,
): { bindings: { profile_id: string; enabled: boolean; capabilities: ProviderCapabilities }[]; unservable: string[] } {
  const candidates = combinableProfiles(catalog, type, profileID);
  const names = booleanCapabilityNames(catalog);
  const perProfile = new Map<string, ProviderCapabilities>(
    candidates.map((profile) => [profile.id, { ...emptyCapabilities }]),
  );
  const unservable: string[] = [];

  for (const name of names) {
    if (!capabilities[name]) continue;
    const home = candidates.find((profile) => profile.ceiling[name]);
    if (!home) {
      unservable.push(name);
      continue;
    }
    (perProfile.get(home.id) as ProviderCapabilities)[name] = true as never;
  }

  for (const profile of candidates) {
    const assigned = perProfile.get(profile.id) as ProviderCapabilities;
    const bounds = profile.ceiling.max_context_tokens > 0 || profile.ceiling.max_output_tokens > 0;
    if (assigned.chat || bounds) {
      assigned.max_context_tokens = capabilities.max_context_tokens;
      assigned.max_output_tokens = capabilities.max_output_tokens;
    }
  }

  const bindings = candidates
    .map((profile) => ({
      profile_id: profile.id,
      capabilities: perProfile.get(profile.id) as ProviderCapabilities,
    }))
    .map((binding) => ({
      ...binding,
      enabled: booleanCapabilityNames(catalog).some((name) => binding.capabilities[name]),
    }))
    .filter((binding) => binding.enabled);

  return { bindings, unservable };
}
