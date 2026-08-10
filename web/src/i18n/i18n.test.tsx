import { readFileSync, readdirSync, statSync } from "node:fs";
import { join } from "node:path";
import { QueryClient, QueryClientProvider } from "@tanstack/react-query";
import { render, screen, waitFor } from "@testing-library/react";
import { afterEach, describe, expect, it, vi } from "vitest";
import { Layout } from "../Layout";
import i18n, { applyPreference } from ".";
import { enUS } from "./locales/en-US";
import { zhCN } from "./locales/zh-CN";

describe("admin internationalization", () => {
  afterEach(async () => {
    vi.restoreAllMocks();
    await i18n.changeLanguage("zh-CN");
  });

  it("keeps Chinese and English resource keys in exact parity", () => {
    expect(flattenKeys(zhCN)).toEqual(flattenKeys(enUS));
  });

  // Half-width commas inside Chinese copy render noticeably wrong next to the
  // full-width punctuation used everywhere else, and it is the kind of thing that
  // survives review because the string still reads correctly in a diff. Caught
  // once already, in the write-path card, from a screenshot rather than a test.
  it("uses full-width punctuation in Chinese copy", () => {
    const offenders: string[] = [];
    const walk = (node: unknown, path: string) => {
      if (typeof node === "string") {
        if (/[\u4e00-\u9fa5][,;]|[,;][\u4e00-\u9fa5]/.test(node)) offenders.push(`${path}: ${node}`);
        return;
      }
      if (node && typeof node === "object") {
        for (const [key, value] of Object.entries(node)) walk(value, path ? `${path}.${key}` : key);
      }
    };
    walk(zhCN, "");
    expect(offenders).toEqual([]);
  });

  it("keeps navigation labels aligned with page titles", () => {
    expect([
      zhCN.navigation.overview, zhCN.navigation.providers, zhCN.navigation.deployments,
      zhCN.navigation.routes, zhCN.navigation.policies, zhCN.navigation.projects,
      zhCN.navigation.developer, zhCN.navigation.usage, zhCN.navigation.operations, zhCN.navigation.masterKey,
      zhCN.navigation.settings,
    ]).toEqual([
      zhCN.dashboard.title, zhCN.providers.title, zhCN.deployments.title,
      zhCN.routes.title, zhCN.policyManagement.title, zhCN.projects.title,
      zhCN.developer.title, zhCN.usage.title, zhCN.operations.title, zhCN.custody.title,
      zhCN.settings.title,
    ]);
    expect([
      enUS.navigation.overview, enUS.navigation.providers, enUS.navigation.deployments,
      enUS.navigation.routes, enUS.navigation.policies, enUS.navigation.projects,
      enUS.navigation.developer, enUS.navigation.usage, enUS.navigation.operations, enUS.navigation.masterKey,
      enUS.navigation.settings,
    ]).toEqual([
      enUS.dashboard.title, enUS.providers.title, enUS.deployments.title,
      enUS.routes.title, enUS.policyManagement.title, enUS.projects.title,
      enUS.developer.title, enUS.usage.title, enUS.operations.title, enUS.custody.title,
      enUS.settings.title,
    ]);
  });

  it("renders exactly one navigation language and switches without a reload", async () => {
    window.history.replaceState({}, "", "/admin");
    const client = new QueryClient({ defaultOptions: { queries: { retry: false } } });
    render(
      <QueryClientProvider client={client}>
        <Layout username="admin"><h1>content</h1></Layout>
      </QueryClientProvider>,
    );
    expect(screen.getByRole("link", { name: "运行总览" })).toBeVisible();
    expect(screen.getByRole("button", { name: "退出登录" })).toBeVisible();
    expect(screen.queryByRole("link", { name: "Overview" })).not.toBeInTheDocument();

    await i18n.changeLanguage("en-US");
    await waitFor(() => expect(screen.getByRole("link", { name: "Overview" })).toBeVisible());
    expect(screen.getByRole("button", { name: "Sign out" })).toBeVisible();
    expect(screen.queryByRole("link", { name: "运行总览" })).not.toBeInTheDocument();
    expect(document.documentElement).toHaveAttribute("lang", "en-US");
  });

  // Copy that no screen reads is not neutral: it looks like a decision that was
  // made, it gets translated and reviewed, and the next person wires the wrong
  // one of two near-identical keys. This catches the leftover at the point it is
  // created rather than at the next audit.
  //
  // SKIPPED ON PURPOSE: the ledger below is the debt that exists today, and the
  // policies/projects/redaction sections are being edited in parallel. Re-run
  // this with `it` once those land, delete what the run reports, and keep the
  // ledger only for keys with a stated reason to exist unreferenced.
  it("defines no locale key that nothing in web/src reads", () => {
    expect(unreferencedKeys()).toEqual(KNOWN_UNREFERENCED);
  });

  it("does not persist locale selection in browser storage", async () => {
    const local = vi.spyOn(Storage.prototype, "setItem");
    const remove = vi.spyOn(Storage.prototype, "removeItem");

    await applyPreference("en-US", "zh-CN");

    expect(local).not.toHaveBeenCalled();
    expect(remove).not.toHaveBeenCalled();
  });
});

function flattenKeys(value: object, prefix = ""): string[] {
  return Object.entries(value).flatMap(([key, child]) => {
    const path = prefix ? `${prefix}.${key}` : key;
    return typeof child === "object" && child !== null ? flattenKeys(child, path) : [path];
  }).sort();
}

// Every key i18next can still resolve counts as referenced: a literal
// `t("a.b")`, a template prefix such as `t(`capabilities.${name}`)`, and the
// plural variants i18next derives from a base key.
function unreferencedKeys(): string[] {
  const sourceRoot = join(process.cwd(), "src");
  const files: string[] = [];
  const walk = (dir: string) => {
    for (const entry of readdirSync(dir)) {
      const child = join(dir, entry);
      if (statSync(child).isDirectory()) {
        if (!child.includes(join("i18n", "locales"))) walk(child);
      } else if (/\.tsx?$/.test(child) && !child.includes(join("i18n", "locales")) && !/\.test\.tsx?$/.test(child)) {
        // Test files are excluded on purpose: a key that only a test mentions
        // still ships to nobody, and this file's own ledger would otherwise
        // count every entry as a reference to itself.
        files.push(child);
      }
    }
  };
  walk(sourceRoot);
  const source = files.map((file) => readFileSync(file, "utf8")).join("\n");
  const dynamicPrefixes = [...source.matchAll(/`([A-Za-z0-9_.]*?)\$\{/g)].map((match) => match[1]).filter(Boolean);
  const pluralSuffix = /_(zero|one|two|few|many|other)$/;
  const referenced = (path: string) =>
    source.includes(path) || dynamicPrefixes.some((prefix) => path.startsWith(prefix));
  return flattenKeys(zhCN)
    .filter((path) => !referenced(path) && !referenced(path.replace(pluralSuffix, "")))
    .sort();
}

// The keys that are defined and read by nothing. Shrinking this list is the
// point; adding to it needs a reason written next to the entry.
const KNOWN_UNREFERENCED = [
    "auth.loginDescription",
    "auth.loginTitle",
    "common.create",
    "common.noMatchesDescription",
    "common.update",
    "dashboard.accountingPeriod",
    "dashboard.trendLegend",
    "dashboard.trendTitle",
    "deployments.adoptProposal",
    "deployments.adoptProposalWarning",
    "deployments.canonicalTarget",
    "deployments.capacityCostSection",
    "deployments.capacityCostSectionDescription",
    "deployments.createAndLoad",
    "deployments.createPriceVersion",
    "deployments.customizeCapabilities",
    "deployments.detectionCatalogEvidence",
    "deployments.detectionVerifiedEvidence",
    "deployments.fixedRequestHint",
    "deployments.healthy",
    "deployments.hideCapabilityEditor",
    "deployments.importProposal",
    "deployments.importedProposalWarning",
    "deployments.lastTest",
    "deployments.manualTarget",
    "deployments.modelStatus.conflicting",
    "deployments.modelStatus.known",
    "deployments.modelStatus.partial",
    "deployments.modelStatus.unknown",
    "deployments.newPriceVersion",
    "deployments.noPendingProposals",
    "deployments.none",
    "deployments.notTested",
    "deployments.notValidated",
    "deployments.off",
    "deployments.priceProposals",
    "deployments.priority",
    "deployments.proposalExpires",
    "deployments.proposalMatch",
    "deployments.proposalSafety",
    "deployments.providerDefault",
    "deployments.rejectProposal",
    "deployments.rejectProposalConfirm",
    "deployments.retryDetection",
    "deployments.reviewProposal",
    "deployments.saveProposal",
    "deployments.selectionSpansInterfaces",
    "deployments.selectionSpansInterfacesDescription",
    "deployments.sourceDigest",
    "deployments.sourceReference",
    "deployments.sourceURL",
    "deployments.testing",
    "deployments.tokenLimitSection",
    "deployments.tokenLimitSectionDescription",
    "deployments.unhealthy",
    "deployments.unsupportedByInterface",
    "deployments.upstreamModel",
    "deployments.upstreamModelHint",
    "deployments.validationExpired",
    "deployments.validationFailed",
    "deployments.validationPassed",
    "deployments.weight",
    "developer.creatingDebugKey",
    "operations.discardChanges",
    "operations.testDelivered",
    "providers.capabilityLimit",
    "providers.concurrency",
    "providers.healthy",
    "providers.healthyStatus",
    "providers.notTested",
    "providers.openAIProfiles.chat",
    "providers.openAIProfiles.media",
    "providers.runtimeControls",
    "providers.testConnection",
    "providers.testing",
    "providers.unhealthy",
    "routes.healthy",
    "routes.testing",
    "routes.unhealthy",
    "settings.acceptingTraffic",
    "settings.accountDescription",
    "settings.accountSection",
    "settings.advancedDetails",
    "settings.configFacts.admin_listen",
    "settings.configFacts.capability_fresh_ttl",
    "settings.configFacts.capability_global_concurrency",
    "settings.configFacts.capability_max_provider_calls",
    "settings.configFacts.capability_provider_concurrency",
    "settings.configFacts.capability_refresh_cooldown",
    "settings.configFacts.capability_retention",
    "settings.configFacts.capability_total_timeout",
    "settings.configFacts.data_dir",
    "settings.configFacts.gateway_listen",
    "settings.configFacts.master_key_mode",
    "settings.configFacts.metadata_file",
    "settings.configFacts.metrics_enabled",
    "settings.configFacts.metrics_listen",
    "settings.configFacts.metrics_require_auth",
    "settings.configFacts.metrics_tls_enabled",
    "settings.configFacts.private_provider_endpoints",
    "settings.configFacts.private_webhooks",
    "settings.configFacts.tls_cert_file",
    "settings.configFacts.tls_enabled",
    "settings.configFacts.tls_key_file",
    "settings.configFacts.trust_proxy_headers",
    "settings.configPreviewTitle",
    "settings.configSectionDescriptions.capability_detection",
    "settings.configSectionDescriptions.metrics",
    "settings.configSectionDescriptions.network",
    "settings.configSectionDescriptions.security",
    "settings.configSectionDescriptions.storage",
    "settings.configSectionDescriptions.transport",
    "settings.configSectionMarks.capability_detection",
    "settings.configSectionMarks.metrics",
    "settings.configSectionMarks.network",
    "settings.configSectionMarks.security",
    "settings.configSectionMarks.storage",
    "settings.configSectionMarks.transport",
    "settings.configSections.capability_detection",
    "settings.configSections.metrics",
    "settings.configSections.network",
    "settings.configSections.security",
    "settings.configSections.storage",
    "settings.configSections.transport",
    "settings.instanceSection",
    "settings.languageDescription",
    "settings.languageEyebrow",
    "settings.languageSaved",
    "settings.languageTitle",
    "settings.masterKeyModes.file",
    "settings.masterKeyModes.key_slots",
    "settings.preferencesSection",
    "settings.readOnlyScope",
    "settings.runtimeSection",
    "settings.saveLanguage",
    "settings.savePreference",
    "settings.securityScope",
    "settings.securitySection",
    "settings.sessionRotation",
    "settings.systemSection",
    "settings.version",
];
