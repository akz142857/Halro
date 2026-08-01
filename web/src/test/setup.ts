import "@testing-library/jest-dom/vitest";
import { cleanup } from "@testing-library/react";
import { afterEach, beforeEach } from "vitest";
import i18n from "../i18n";

afterEach(cleanup);

beforeEach(async () => {
  await i18n.changeLanguage("zh-CN");
  document.documentElement.lang = "zh-CN";
  document.documentElement.dir = "ltr";
});
