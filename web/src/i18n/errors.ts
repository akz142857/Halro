import type { TFunction } from "i18next";
import { ApiError } from "../api";

export function localizedError(t: TFunction, error: unknown) {
  if (!(error instanceof ApiError)) return t("errors.network");
  if (error.code === "deployment_price_unavailable") {
    return t("errors.deploymentPriceUnavailable");
  }
  if (error.status === 400 || error.status === 422) return t("errors.badRequest");
  if (error.status === 401) return t("errors.authentication");
  if (error.status === 403) return t("errors.forbidden");
  if (error.status === 404) return t("errors.notFound");
  if (error.status === 409 || error.status === 412 || error.status === 428) return t("errors.conflict");
  if (error.status === 429) return t("errors.rateLimited");
  return t("errors.server");
}

// Validation and conflict responses name the field that failed. The localized headline
// alone leaves the operator guessing between name, models, CIDR and policy bindings, so
// the server's reason is surfaced verbatim underneath it.
export function errorDetail(error: unknown) {
  if (!(error instanceof ApiError)) return "";
  // A forwarded upstream reply is the whole point of the message; show it at any status.
  if (error.detail) return error.detail;
  const detailed = error.status === 400 || error.status === 409 || error.status === 422;
  if (!detailed) return "";
  const message = error.message.trim();
  return message.startsWith("request failed") ? "" : message;
}
