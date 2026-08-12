import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterEach, beforeEach } from "vitest";
import i18n, { initI18n, loadLocale } from "../i18n";

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
