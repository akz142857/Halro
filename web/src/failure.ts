// How a failure is explained to a reader.
//
// The gateway sends its own identifiers — `error_class` is a member of
// provider.ErrorClass, `outcome` is what the ledger recorded — and they travel
// that way on purpose: the record is the accounting authority's, and its wire
// form must not depend on who is reading it. Turning them into a sentence is
// this file's business, exactly as it is for audit actions and capability
// names.
//
// Every lookup falls back to the raw identifier. A class an adapter starts
// producing before this table hears about it degrades to the English token the
// console printed before the table existed, never to a broken key.

// The classes the gateway can attach to an attempt. `provider.ErrorClass` is
// the source; `client_disconnected_or_timed_out` is not a member of it and is
// listed anyway, because attempt logs written before it was normalized still
// carry it and history is not rewritten.
export const errorClasses = [
  "authentication",
  "rate_limit",
  "timeout",
  "connect",
  "provider_5xx",
  "bad_request",
  "malformed_response",
  "canceled",
  "client_disconnected_or_timed_out",
  "unknown",
] as const;

export type ErrorClass = (typeof errorClasses)[number];

type Translate = (key: string, values?: Record<string, unknown>) => string;

// errorClassLabel names the class in the reader's language, or hands back the
// identifier the server sent.
export function errorClassLabel(t: Translate, errorClass: string | undefined): string {
  if (!errorClass) return "";
  return t(`usage.errorClasses.${errorClass}`, { defaultValue: errorClass });
}

// errorClassAdvice is what to go and check. It is empty for a class with no
// entry, which the caller renders as nothing rather than as a blank line
// promising help it does not have.
export function errorClassAdvice(t: Translate, errorClass: string | undefined): string {
  if (!errorClass) return "";
  return t(`usage.errorAdvice.${errorClass}`, { defaultValue: "" });
}

// A failed attempt's headline.
//
// The class first, because that is what the ledger classified. Then the
// attempt's own terminal state — a redaction refusal or an unserved capability
// has no upstream class and is not an upstream failure, and naming it is more
// use than the word "error". The upstream status is a fallback only when it is
// a refusal: a local failure settles with HTTP 200, and showing that beside a
// red dot said the upstream had answered fine and still called it an error.
export function attemptFailureLabel(
  t: Translate,
  attempt: { status?: string; error_class?: string; http_status?: number },
): string {
  if (attempt.error_class) return errorClassLabel(t, attempt.error_class);
  if (attempt.status) {
    const key = `usage.outcomes.${attempt.status}`;
    const outcome = t(key, { defaultValue: "" });
    if (outcome) return outcome;
  }
  if (attempt.http_status && attempt.http_status >= 400) return `HTTP ${attempt.http_status}`;
  return t("usage.error");
}

// upstreamStatus is the status worth showing beside the headline: an actual
// refusal, not the 200 a locally-refused attempt settles with.
export function upstreamStatus(status?: number): number | undefined {
  return status && status >= 400 ? status : undefined;
}

// Whether a failure record predates the fields that carry the upstream's own
// identifiers.
//
// The distinction matters and cannot be read off the identifiers themselves: an
// empty provider_code means "the upstream named none" on a record written after
// they were kept, and "nobody asked" on one written before — and showing the
// same blank for both tells an operator to stop looking for a code that exists
// upstream and is simply not here.
//
// Both halves of the predicate are load-bearing. A classified upstream failure
// written by a build that keeps these fields always carries a phase, so a
// record with a class and no phase is an older one. Requiring the class is what
// keeps the notice off the failures that never had an upstream to name — a
// redaction refusal, a target that could not serve the shape — which carry no
// class either and were being told, wrongly, that they were written before the
// fields existed.
export function predatesProviderIdentifiers(
  failure: { error_class?: string; failure_phase?: string } | undefined,
): boolean {
  return Boolean(failure?.error_class) && !failure?.failure_phase;
}
