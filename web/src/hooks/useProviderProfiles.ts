import { useQuery } from "@tanstack/react-query";
import { api } from "../api";
import type {
  ProviderCapabilities,
  ProviderOfferingDescriptor,
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
  vision: false, fetched_image: false, json_object: false, structured_outputs: false,
  developer_role: false, reasoning: false,
  stream_usage: false, provider_executed_tools: false,
  max_context_tokens: 0, max_output_tokens: 0,
};

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

/** The upstream products of one provider type.
 *
 * A product is what an operator bought — a metered API, a Coding Plan — and it
 * is the first thing a credential form has to establish, because it decides the
 * endpoint, the key, and which capabilities the connection can carry. Served,
 * never inferred here: deciding it in the browser from a provider name is how
 * the console came to seal every BigModel key to the mainland surface. */
export function offeringsForType(catalog: ProviderProfilesCatalog, type: ProviderType): ProviderOfferingDescriptor[] {
  return catalog.provider_types.find((entry) => entry.type === type)?.offerings ?? [];
}

export function findOffering(
  catalog: ProviderProfilesCatalog,
  type: ProviderType,
  offeringID: string,
): ProviderOfferingDescriptor | undefined {
  return offeringsForType(catalog, type).find((offering) => offering.id === offeringID);
}

/** One product identity a credential of this type can be created for.
 *
 * A credential stores an access surface and a scheme, and that pair is what says
 * which product it belongs to. Several profiles usually share one pair — an
 * OpenAI key reaches chat and media, a Kimi key reaches three wire shapes — so
 * these are the distinct pairs rather than the profiles. Mirrors
 * domain.CredentialIdentities, which is what the Admin write path resolves
 * against; where there is more than one, the server refuses to guess. */
export interface CredentialIdentity {
  accessSurface: string;
  credentialScheme: string;
  offeringID: string;
  regionID: string;
  /** The profile a credential on this identity resolves to, and the endpoint it
   * prefills. */
  primaryProfileID: string;
  defaultBaseURL: string;
}

export function credentialIdentities(catalog: ProviderProfilesCatalog, type: ProviderType): CredentialIdentity[] {
  const identities: CredentialIdentity[] = [];
  for (const profile of profilesForType(catalog, type)) {
    if (identities.some((identity) =>
      identity.accessSurface === profile.access_surface && identity.credentialScheme === profile.credential_scheme)) {
      continue;
    }
    identities.push({
      accessSurface: profile.access_surface,
      credentialScheme: profile.credential_scheme,
      offeringID: profile.offering_id,
      regionID: profile.region_id,
      primaryProfileID: profile.id,
      defaultBaseURL: profile.default_base_url,
    });
  }
  return identities;
}

/** One choice a connection form offers for "which implementation".
 *
 * Two different things produce a choice, and both are properties of the served
 * matrix rather than of a provider name:
 *
 *  - more than one credential identity, which is BigModel's two regional
 *    products: different surfaces, different capability sets, different keys;
 *  - a route-partitioned group, which is Bedrock Mantle: one credential, one
 *    surface, and models that each answer on exactly one of its routes.
 *
 * Where neither holds, the group's profiles ride one connection together and
 * there is nothing to ask — which is every other provider. */
export interface ConnectionChoice {
  profileID: string;
  accessSurface: string;
  credentialScheme: string;
  offeringID: string;
  regionID: string;
  defaultBaseURL: string;
}

export function connectionChoices(catalog: ProviderProfilesCatalog, type: ProviderType): ConnectionChoice[] {
  const choices: ConnectionChoice[] = [];
  const groupsSeen = new Set<string>();
  for (const profile of profilesForType(catalog, type)) {
    const group = `${profile.access_surface}\u0000${profile.credential_scheme}`;
    if (groupsSeen.has(group) && !profile.route_partitioned) continue;
    groupsSeen.add(group);
    choices.push({
      profileID: profile.id,
      accessSurface: profile.access_surface,
      credentialScheme: profile.credential_scheme,
      offeringID: profile.offering_id,
      regionID: profile.region_id,
      defaultBaseURL: profile.default_base_url,
    });
  }
  return choices;
}

/** The host of an endpoint, lowercased and without its port.
 *
 * Mirrors domain.endpointHost: the server compares the same normalised form, so
 * a value this recognises is a value it recognises. */
export function endpointHost(value: string): string {
  try {
    return new URL(value.trim()).hostname.toLocaleLowerCase();
  } catch {
    return "";
  }
}

/** Which account region an endpoint names, for a product whose region is read
 * from the endpoint.
 *
 * Undefined is an ordinary answer, not a failure: an operator may front any
 * upstream with a proxy, and the host list is recognition rather than an
 * allowlist. The console says "unknown" and saves anyway, which is what the
 * server does too. */
export function regionForEndpoint(offering: ProviderOfferingDescriptor | undefined, value: string): string | undefined {
  if (!offering || offering.region_scope !== "by_endpoint") return undefined;
  const host = endpointHost(value);
  return offering.region_hosts.find((entry) => entry.host === host)?.region;
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
