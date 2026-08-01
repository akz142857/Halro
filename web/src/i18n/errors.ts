import type { TFunction } from "i18next";
import { ApiError } from "../api";

export function localizedError(t: TFunction, error: unknown) {
  if (!(error instanceof ApiError)) return t("errors.network");
  if (error.status === 400 || error.status === 422) return t("errors.badRequest");
  if (error.status === 401) return t("errors.authentication");
  if (error.status === 403) return t("errors.forbidden");
  if (error.status === 404) return t("errors.notFound");
  if (error.status === 409 || error.status === 412 || error.status === 428) return t("errors.conflict");
  if (error.status === 429) return t("errors.rateLimited");
  return t("errors.server");
}
