import { useQuery } from "@tanstack/react-query";
import { api } from "../api";
import type {
  ProfileRequestConstraint,
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
  vision: false, fetched_image: false, json_mode: false, developer_role: false, reasoning: false,
  stream_usage: false, provider_executed_tools: false,
  max_context_tokens: 0, max_output_tokens: 0,
};

/** What the profiles in play have declared they cannot carry.
 *
 * Keyed by the profiles a deployment could actually run on, because that is the
 * question being asked at the moment a capability is ticked: this interface can
 * see an image, and still cannot fetch one. Deduplicated by endpoint so two
 * bindings on the same profile do not state the same rule twice. */
export function profileRequestConstraints(
  catalog: ProviderProfilesCatalog,
  profileIDs: readonly string[],
): (ProfileRequestConstraint & { profile_id: string })[] {
  const wanted = new Set(profileIDs);
  const seen = new Set<string>();
  const constraints: (ProfileRequestConstraint & { profile_id: string })[] = [];
  for (const type of catalog.provider_types) {
    for (const profile of type.profiles) {
      if (!wanted.has(profile.id)) continue;
      for (const constraint of profile.request_constraints ?? []) {
        const key = `${profile.id}\u0000${constraint.endpoint_id}`;
        if (seen.has(key)) continue;
        seen.add(key);
        constraints.push({ ...constraint, profile_id: profile.id });
      }
    }
  }
  return constraints;
}

/** What the profiles in play could serve, whether or not the connection has
 * turned it on.
 *
 * This is the difference between "this interface cannot do it" and "this
 * connection has not enabled it yet". Only the second is something an operator
 * can act on, and a form that shows neither leaves a capability unreachable. */
export function interfaceCeiling(
  catalog: ProviderProfilesCatalog,
  profileIDs: readonly string[],
): ProviderCapabilities {
  const wanted = new Set(profileIDs);
  const ceiling = { ...emptyCapabilities };
  for (const type of catalog.provider_types) {
    for (const profile of type.profiles) {
      if (!wanted.has(profile.id)) continue;
      for (const name of booleanCapabilityNames(catalog)) {
        if (profile.ceiling[name]) ceiling[name] = true;
      }
    }
  }
  return ceiling;
}

/** Capability keys, excluding the two numeric limits, which are not checkboxes. */
/** The capability keys that are a yes or a no.
 *
 * ProviderCapabilities mixes them with the two numeric bounds, and the mix is
 * what made a cast necessary to write into it by name. Naming the boolean half
 * as a type moves that from a cast the compiler cannot check to a fact it can:
 * a numeric key reaching a boolean write is a compile error rather than a `true`
 * silently landing in max_context_tokens. */
export type BooleanCapabilityName = {
  [K in keyof ProviderCapabilities]: ProviderCapabilities[K] extends boolean ? K : never;
}[keyof ProviderCapabilities];

export function booleanCapabilityNames(catalog: ProviderProfilesCatalog): BooleanCapabilityName[] {
  return catalog.capability_names.filter(
    (name) => name !== "max_context_tokens" && name !== "max_output_tokens",
  ) as BooleanCapabilityName[];
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

/** Profiles a connection anchored on this one carries.
 *
 * Which profiles go together is the server's rule — every binding has to match
 * the connection's credential — and it says so per profile, so this reads the
 * answer instead of re-deriving it from surfaces and schemes. */
export function combinableProfiles(
  catalog: ProviderProfilesCatalog,
  type: ProviderType,
  profileID: string,
): ProviderProfileDescriptor[] {
  const anchor = findProfile(catalog, type, profileID);
  if (!anchor) return [];
  const peers = anchor.combines_with
    .map((id) => findProfile(catalog, type, id))
    .filter((profile): profile is ProviderProfileDescriptor => Boolean(profile));
  return [anchor, ...peers];
}

/** What an operator may turn on for this connection.
 *
 * Served, not computed. It is not simply the union of the profiles' ceilings: a
 * capability that several of them could serve has no unambiguous home in a flat
 * set, so the server refuses it — and this set is exactly what the server will
 * accept, which is the only property a form needs. */
export function connectionCeiling(
  catalog: ProviderProfilesCatalog,
  type: ProviderType,
  profileID: string,
): ProviderCapabilities {
  return findProfile(catalog, type, profileID)?.connection_ceiling ?? emptyCapabilities;
}

/** What a new connection anchored here starts with. */
export function connectionDefaults(
  catalog: ProviderProfilesCatalog,
  type: ProviderType,
  profileID: string,
): ProviderCapabilities {
  return findProfile(catalog, type, profileID)?.connection_defaults ?? emptyCapabilities;
}

/** Applies the server's capability dependencies so the form cannot offer a
 * combination the save will refuse.
 *
 * The dependencies arrive direct rather than flattened — stream usage names
 * streaming, streaming names chat — so both directions are walked to a fixed
 * point: turning one on turns on everything it stands on, and turning one off
 * takes down everything standing on it. Flattening was the earlier shape and it
 * lost the middle of the chain: stream usage could be ticked with chat and no
 * streaming, which the deployment then refused. */
export function updateCapabilitySelection(
  catalog: ProviderProfilesCatalog,
  current: ProviderCapabilities,
  capability: keyof ProviderCapabilities,
  enabled: boolean,
): ProviderCapabilities {
  const next = { ...current, [capability]: enabled };
  const dependencies = catalog.capability_dependencies;
  if (enabled) {
    for (let changed = true; changed;) {
      changed = false;
      for (const [name, needs] of Object.entries(dependencies)) {
        if (!next[name as keyof ProviderCapabilities]) continue;
        for (const need of needs) {
          if (next[need as keyof ProviderCapabilities]) continue;
          next[need as keyof ProviderCapabilities] = true as never;
          changed = true;
        }
      }
    }
    return next;
  }
  for (let changed = true; changed;) {
    changed = false;
    for (const [name, needs] of Object.entries(dependencies)) {
      if (!next[name as keyof ProviderCapabilities]) continue;
      if (needs.every((need) => next[need as keyof ProviderCapabilities])) continue;
      next[name as keyof ProviderCapabilities] = false as never;
      changed = true;
    }
  }
  return next;
}

/** What the form has ticked that this connection cannot serve.
 *
 * The form submits one flat set and the server decides which profile serves
 * each capability, so there is nothing to split here. What is still worth doing
 * locally is naming a capability the connection cannot carry before the save
 * goes out: the server refuses it too, but the form can point at the checkbox.
 *
 * Nothing is filtered on the way out. A form that quietly dropped an enabled
 * capability would save a connection that does less than what was ticked. */
export function unservableCapabilities(
  catalog: ProviderProfilesCatalog,
  type: ProviderType,
  profileID: string,
  capabilities: ProviderCapabilities,
): (keyof ProviderCapabilities)[] {
  const ceiling = connectionCeiling(catalog, type, profileID);
  return booleanCapabilityNames(catalog).filter((name) => capabilities[name] && !ceiling[name]);
}

/** Whether anything at all is ticked, which the server requires. */
export function anyCapabilityEnabled(
  catalog: ProviderProfilesCatalog,
  capabilities: ProviderCapabilities,
): boolean {
  return booleanCapabilityNames(catalog).some((name) => capabilities[name]);
}

/** Capabilities whose consequence a checkbox does not show, as the server names
 * them. What to say about one is the console's business; which ones need saying
 * is not. */
export function capabilityNeedsOptInWarning(
  catalog: ProviderProfilesCatalog,
  name: keyof ProviderCapabilities,
): boolean {
  return catalog.capability_opt_in_warnings.includes(name as string);
}
