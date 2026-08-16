import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterEach, beforeEach, vi } from "vitest";
import i18n, { initI18n, loadLocale } from "../i18n";
import { api } from "../api";
import { providerProfilesFixture } from "./fixtures";
import type { ProviderProfilesCatalog } from "../types";

afterEach(cleanup);

// Production loads one locale and fetches the other only when the operator
// switches. Tests assert against both, so both are resident here. Each call is
// idempotent, so this costs one import for the whole run.
beforeEach(async () => {
  await initI18n();
  await loadLocale("zh-CN");
  await loadLocale("en-US");
  await i18n.changeLanguage("zh-CN");
  document.documentElement.lang = "zh-CN";
  document.documentElement.dir = "ltr";
});

// Every screen that offers a connection form reads the provider matrix, and it
// is compile-time data on the server rather than anything a test sets up. Stub
// it once here so each test states only what it is actually about; a test that
// cares what happens when it fails overrides this.
beforeEach(() => {
  vi.spyOn(api, "providerProfiles").mockResolvedValue(
    providerProfilesFixture as unknown as ProviderProfilesCatalog,
  );
});
