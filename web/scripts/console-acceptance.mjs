#!/usr/bin/env node
// Console acceptance against a locally served source/production build.
// Usage:
// PLAYWRIGHT_MODULE=/absolute/path/to/playwright/index.mjs \
// BASE_URL=http://127.0.0.1:5274/admin/ OUTPUT_DIR=/private/tmp/halro-console-acceptance \
// node web/scripts/console-acceptance.mjs [--baseline] [--quick]
// --states checks read-only, empty, required-MFA and simulated API-error states.
// SCENARIOS/WIDTHS/LOCALES/APPEARANCES select a focused matrix. SCREENSHOTS=all
// captures every selected case. Node 22.18+ is required for native TS data imports.
// No live Admin API or provider is contacted. See the fail-closed router in
// console-acceptance-fixtures.mjs. This is UI evidence, not backend acceptance.
import { mkdir, writeFile } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { installConsoleFixture, fixtureResponse, NOW } from './console-acceptance-fixtures.mjs';
import { zhCN } from '../src/i18n/locales/zh-CN.ts';
import { enUS } from '../src/i18n/locales/en-US.ts';

const baseline = process.argv.includes('--baseline');
const quick = process.argv.includes('--quick');
const statesOnly = process.argv.includes('--states');
const baseURL = process.env.BASE_URL ?? `http://127.0.0.1:${baseline ? 5273 : 5274}/admin/`;
const outputDir = resolve(process.env.OUTPUT_DIR ?? `/private/tmp/halro-console-simplification/acceptance/${baseline ? 'baseline' : 'updated'}`);
const playwrightModule = process.env.PLAYWRIGHT_MODULE;
if (!playwrightModule) throw new Error('Set PLAYWRIGHT_MODULE to an installed Playwright index.mjs; this script installs no dependencies.');
const { chromium } = await import(pathToFileURL(resolve(playwrightModule)).href);
const words = (locale) => locale === 'zh-CN' ? zhCN : enUS;
const button = (page, name) => page.getByRole('button', { name, exact: true });
const openDeveloperCode = async (page, t) => button(page, t.developer.integrationCode).click();
const openDeveloperConnection = async (page, t) => button(page, t.developer.connectionSettings).click();
const longPrompt = 'Synthetic acceptance input with long multilingual content · 这是用于验证控件布局的合成长文本。\n'.repeat(100);
function partialDeveloperResponse(state, method, path, query, body) {
  if (method === 'POST' && path === '/developer/execute/responses') return { status: 200, headers: { 'X-Request-ID': 'req_acceptance' }, data: { id: 'response_acceptance', object: 'response', output: [{ type: 'message', content: [{ type: 'output_text', text: 'Synthetic browser acceptance response.' }] }], usage: { input_tokens: 1000, output_tokens: 250 } } };
  if (method === 'GET' && path === '/usage/requests/req_acceptance') {
    const response = fixtureResponse(state, method, path, query, body);
    response.data.summary.unknown_attempts = 1;
    response.data.attempts[0].tokens_estimated = true;
    response.data.attempts[0].cost_estimated = true;
    return response;
  }
  return undefined;
}

const scenarios = [
  { name: 'dashboard', path: '/admin/' },
  { name: 'providers', path: '/admin/providers' },
  { name: 'credentials', path: '/admin/providers?view=credentials' },
  { name: 'proxies', path: '/admin/providers?view=proxies' },
  { name: 'deployments', path: '/admin/deployments' },
  { name: 'routes', path: '/admin/routes' },
  { name: 'projects', path: '/admin/projects' },
  { name: 'developer', path: '/admin/developer' },
  { name: 'developer-json', path: '/admin/developer', dialog: false, open: async (page, t) => page.getByRole('tab', { name: t.developer.jsonMode, exact: true }).click() },
  { name: 'developer-images', path: '/admin/developer', dialog: false, newOnly: true, open: async (page, t) => {
    await page.locator('.developer-image-field > summary').click();
    await page.locator('#developer-image-url').fill('https://images.example.test/synthetic.png');
    await button(page, t.developer.addImage).click();
    if (!(await page.locator('.developer-image-field > summary').innerText()).includes(t.developer.remoteImageFetch)) throw new Error('Remote image risk is missing from the disclosure summary');
  } },
  { name: 'developer-code', path: '/admin/developer', newOnly: true, open: openDeveloperCode },
  { name: 'developer-connection', path: '/admin/developer', newOnly: true, open: openDeveloperConnection },
  { name: 'developer-connection-long', path: '/admin/developer', newOnly: true, options: { longContent: true }, open: openDeveloperConnection },
  { name: 'developer-long', path: '/admin/developer', dialog: false, newOnly: true, options: { longContent: true }, open: async (page, t) => page.getByRole('textbox', { name: t.developer.input, exact: true }).fill(longPrompt) },
  { name: 'developer-code-long', path: '/admin/developer', newOnly: true, options: { longContent: true }, open: async (page, t) => { await page.getByRole('textbox', { name: t.developer.input, exact: true }).fill(longPrompt); await openDeveloperCode(page, t); } },
  { name: 'developer-partial', path: '/admin/developer', dialog: false, newOnly: true, options: { respond: partialDeveloperResponse }, open: async (page, t) => {
    await page.locator('#developer-gateway-key').fill('halro_synthetic_acceptance_not_a_real_key');
    await button(page, t.developer.sendRequest).click();
    await page.locator('.developer-response-meta').waitFor();
    await page.getByText(t.developer.costEstimated, { exact: true }).waitFor();
    if (!(await page.locator('.developer-response-meta').innerText()).includes('1')) throw new Error('Unknown-cost attempt count was hidden');
  } },
  { name: 'policies', path: '/admin/policies' },
  { name: 'usage', path: '/admin/usage' },
  { name: 'usage-attempts', path: '/admin/usage?tab=attempts' },
  { name: 'usage-failures', path: '/admin/usage?tab=failures' },
  { name: 'run-governance', path: '/admin/run-governance' },
  { name: 'operations', path: '/admin/operations' },
  { name: 'cluster', path: '/admin/cluster' },
  ...['general', 'security', 'accounts', 'instance', 'config', 'custody', 'diagnostics'].map((pane) => ({ name: `settings-${pane}`, path: `/admin/settings/${pane}` })),
  { name: 'login', path: '/admin/login', options: { auth: 'login' } },
  { name: 'setup', path: '/admin/', options: { auth: 'setup' } },
  { name: 'not-found', path: '/admin/not-a-page' },
  { name: 'first-run', path: '/admin/', options: { firstRun: true } },
];

const dialogs = [
  { name: 'create-provider', path: '/admin/providers', open: async (page, t) => button(page, t.providers.addProvider).click() },
  { name: 'create-credential', path: '/admin/providers?view=credentials', open: async (page, t) => button(page, t.providers.addCredential).click() },
  { name: 'create-proxy', path: '/admin/providers?view=proxies', open: async (page, t) => button(page, t.providers.addProxy).click() },
  { name: 'create-deployment', path: '/admin/deployments', open: async (page, t) => button(page, t.deployments.create).click() },
  { name: 'create-route', path: '/admin/routes', open: async (page, t) => button(page, t.routes.create).click() },
  { name: 'create-project', path: '/admin/projects', open: async (page, t) => button(page, t.projects.create).click() },
  { name: 'create-project-advanced', path: '/admin/projects', open: async (page, t) => { await button(page, t.projects.create).click(); for (const section of await page.getByRole('dialog').locator('.disclosure-section').all()) { if (!(await section.evaluate((element) => element.open))) await section.locator(':scope > summary').click(); } } },
  { name: 'create-policy', path: '/admin/policies', open: async (page, t) => button(page, t.policies.create).click() },
  { name: 'create-webhook', path: '/admin/operations', open: async (page, t) => button(page, t.operations.addWebhook).click() },
];

const mainScenarios = scenarios.filter((scenario) => ['dashboard', 'providers', 'deployments', 'routes', 'projects', 'developer', 'policies', 'usage', 'run-governance', 'operations', 'cluster', 'settings-general'].includes(scenario.name));
const stateScenarios = [
  ...mainScenarios.map((scenario) => ({ ...scenario, name: `readonly-${scenario.name}`, options: { role: 'read_only' } })),
  ...mainScenarios.filter((scenario) => !['dashboard', 'cluster', 'settings-general'].includes(scenario.name)).map((scenario) => ({ ...scenario, name: `empty-${scenario.name}`, options: { empty: true } })),
  { name: 'mfa-required', path: '/admin/settings', options: { mfaSetupRequired: true } },
  { name: 'providers-read-error', path: '/admin/providers', expectedError: true, options: { respond: (_state, method, path) => method === 'GET' && path === '/providers' ? { status: 503, data: { code: 'acceptance_unavailable', error: 'Synthetic provider listing unavailable' } } : undefined } },
  { name: 'settings-read-error', path: '/admin/settings/instance', expectedError: true, options: { respond: (_state, method, path) => method === 'GET' && path === '/settings' ? { status: 503, data: { code: 'acceptance_unavailable', error: 'Synthetic settings unavailable' } } : undefined } },
];

const viewports = [
  { name: 'desktop', width: 1440, height: 900 },
  { name: 'narrow', width: 390, height: 844 },
];
const representatives = new Set(['dashboard', 'providers', 'create-provider', 'create-project', 'usage', 'settings-instance', 'login', 'setup']);
const requested = process.env.SCENARIOS?.split(',').filter(Boolean);
const languages = process.env.LOCALES?.split(',') ?? (quick ? ['zh-CN'] : ['zh-CN', 'en-US']);
const appearances = process.env.APPEARANCES?.split(',') ?? (quick ? ['dark'] : ['dark', 'light']);
const widths = process.env.WIDTHS?.split(',').map(Number);
const primaryViewports = widths ? widths.map((width) => ({ name: `w${width}`, width, height: width < 821 ? 844 : 900 })) : (quick ? [viewports[0]] : viewports);
const results = [];

await mkdir(outputDir, { recursive: true });
const browser = await chromium.launch({ headless: true, ...(process.env.CHROMIUM_EXECUTABLE ? { executablePath: process.env.CHROMIUM_EXECUTABLE } : {}), args: ['--disable-background-networking', '--disable-component-update', '--disable-sync', '--metrics-recording-only', '--no-first-run'] });
const browserVersion = browser.version();

async function inspect(page) {
  return page.evaluate(() => {
    const shown = (element) => Boolean(element.getClientRects().length) && getComputedStyle(element).visibility !== 'hidden' && !element.closest('.sr-only,.visually-hidden,[hidden]');
    const inViewport = (element) => {
      if (!shown(element)) return false;
      const rect = element.getBoundingClientRect();
      let clip = { left: Math.max(0, rect.left), top: Math.max(0, rect.top), right: Math.min(innerWidth, rect.right), bottom: Math.min(innerHeight, rect.bottom) };
      for (let parent = element.parentElement; parent; parent = parent.parentElement) {
        const style = getComputedStyle(parent);
        const boundary = parent.getBoundingClientRect();
        if (/(auto|scroll|hidden|clip)/.test(style.overflowX)) { clip.left = Math.max(clip.left, boundary.left); clip.right = Math.min(clip.right, boundary.right); }
        if (/(auto|scroll|hidden|clip)/.test(style.overflowY)) { clip.top = Math.max(clip.top, boundary.top); clip.bottom = Math.min(clip.bottom, boundary.bottom); }
      }
      return clip.right > clip.left && clip.bottom > clip.top;
    };
    const bounds = (element) => {
      if (!element || !shown(element)) return null;
      const rect = element.getBoundingClientRect();
      return { left: Math.round(rect.left + scrollX), right: Math.round(rect.right + scrollX), top: Math.round(rect.top + scrollY), bottom: Math.round(rect.bottom + scrollY), width: Math.round(rect.width), height: Math.round(rect.height), inViewport: inViewport(element), fullyInViewport: rect.top >= 0 && rect.bottom <= innerHeight && rect.left >= 0 && rect.right <= innerWidth };
    };
    const width = document.documentElement.clientWidth;
    const all = [...document.querySelectorAll('body *')];
    const visible = all.filter(shown);
    const text = document.querySelector('#main-content')?.innerText ?? document.body.innerText;
    const hints = [...document.querySelectorAll('p, small, .field-hint, .capability-advanced-note')].filter(shown);
    const taskRoot = document.querySelector('[role="dialog"]') ?? document.querySelector('.developer-page') ?? document.querySelector('#main-content') ?? document.body;
    const taskHints = [...taskRoot.querySelectorAll('p,small,.field-hint,.capability-advanced-note,.developer-code-footnote > span')].filter((element) => shown(element) && !element.closest('.developer-response-meta,.developer-history-meta'));
    const viewportTaskHints = taskHints.filter(inViewport);
    const developer = document.querySelector('.developer-page');
    const visibleSelects = developer ? [...developer.querySelectorAll('select')].filter(shown) : [];
    const endpointSelect = visibleSelects.find((element) => [...element.options].some((option) => option.value === 'responses'));
    const modelSelect = visibleSelects.find((element) => element !== endpointSelect && ![...element.options].some((option) => option.value.startsWith('project_')));
    const prompt = developer?.querySelector('textarea');
    const key = developer?.querySelector('#developer-gateway-key');
    const send = developer?.querySelector('.developer-send');
    return {
      title: document.querySelector('h1')?.innerText ?? '',
      language: document.documentElement.lang,
      appearance: document.documentElement.dataset.appearance,
      viewport: { width: innerWidth, height: innerHeight, devicePixelRatio },
      documentWidth: document.documentElement.scrollWidth,
      documentHeight: document.documentElement.scrollHeight,
      overflow: document.documentElement.scrollWidth > width + 1,
      overflowElements: document.documentElement.scrollWidth > width + 1 ? visible.filter((element) => element.getBoundingClientRect().right > width + 1).slice(-15).map((element) => ({ tag: element.tagName, class: String(element.className), text: element.innerText?.slice(0, 100), right: Math.round(element.getBoundingClientRect().right) })) : [],
      crash: Boolean(document.querySelector('.crash-panel')),
      errors: [...document.querySelectorAll('.notice.error')].filter(shown).map((element) => element.innerText),
      headings: [...document.querySelectorAll('h1,h2,h3')].filter(shown).map((element) => element.innerText),
      visibleTextLength: text.length,
      helperText: hints.map((element) => element.innerText.trim()).filter(Boolean),
      helperTextLength: hints.reduce((count, element) => count + element.innerText.trim().length, 0),
      taskHelperText: taskHints.map((element) => element.innerText.trim()).filter(Boolean),
      taskHelperTextLength: taskHints.reduce((count, element) => count + element.innerText.trim().length, 0),
      viewportTaskHelperText: viewportTaskHints.map((element) => element.innerText.trim()).filter(Boolean),
      viewportTaskHelperTextLength: viewportTaskHints.reduce((count, element) => count + element.innerText.trim().length, 0),
      developerFlow: developer ? { model: bounds(modelSelect), endpoint: bounds(endpointSelect), key: bounds(key), input: bounds(prompt), send: bounds(send), allPrimaryControlsInViewport: [modelSelect, endpointSelect, key, prompt, send].every((element) => bounds(element)?.fullyInViewport), idleMetadataVisible: Boolean(developer.querySelector('.developer-response-meta') && shown(developer.querySelector('.developer-response-meta'))), inlineCodeVisible: Boolean(developer.querySelector('.developer-code-panel') && shown(developer.querySelector('.developer-code-panel'))) } : undefined,
      dialog: document.querySelector('[role="dialog"]')?.getAttribute('aria-labelledby') ?? null,
      dialogBounds: bounds(document.querySelector('[role="dialog"]')),
      dialogHorizontalOverflow: (() => { const dialog = document.querySelector('[role="dialog"]'); return dialog ? dialog.scrollWidth > dialog.clientWidth + 1 : false; })(),
      controls: [...document.querySelectorAll('input,select,textarea,button,summary')].filter(shown).length,
    };
  });
}

async function runCase(context, scenario, locale, appearance, viewport, extra = {}) {
  const page = await context.newPage();
  await page.setViewportSize({ width: viewport.width, height: viewport.height });
  const errors = [];
  page.on('pageerror', (error) => errors.push(error.message));
  const state = await installConsoleFixture(page, { baseURL, locale, appearance, ...scenario.options });
  // Fixed Date does not pause timers, requests, focus management or animations.
  await page.addInitScript((instant) => {
    const RealDate = Date;
    globalThis.Date = class extends RealDate {
      constructor(...args) { super(...(args.length ? args : [instant])); }
      static now() { return new RealDate(instant).getTime(); }
    };
  }, NOW);
  const name = `${scenario.name}-${locale}-${appearance}-${viewport.name}${extra.zoomEquivalent ? `-zoom${extra.zoomEquivalent}` : ''}`;
  try {
    await page.goto(new URL(scenario.path, baseURL).href, { waitUntil: 'networkidle', timeout: 20000 });
    await page.waitForFunction(() => [...document.querySelectorAll('h1,h2')].some((heading) => heading.getClientRects().length > 0), undefined, { timeout: 10000 });
    await page.waitForFunction(() => !document.querySelector('.boot, .loading'), { timeout: 10000 });
    if (scenario.open) {
      await scenario.open(page, words(locale));
      if (scenario.dialog !== false) {
        const dialog = page.getByRole('dialog');
        await dialog.waitFor({ timeout: 5000 });
        await dialog.evaluate(async (element) => {
          await Promise.all(element.getAnimations({ subtree: true }).filter((animation) => animation.effect?.getTiming().iterations !== Infinity).map((animation) => animation.finished.catch(() => {})));
        });
      }
    }
    if (!baseline && scenario.name.startsWith('developer') && scenario.name !== 'developer-partial') {
      if (await page.locator('.developer-response-meta:visible').count()) throw new Error('Idle response still shows empty metadata');
      if (await page.locator('.developer-page .developer-code-panel:visible').count()) throw new Error('Integration code remains in the primary request task');
      if (await page.locator('.developer-config-panel select:visible').count() !== 2) throw new Error('Primary task should expose only model and API selectors');
    }
    await page.evaluate(() => document.fonts.ready);
    const observation = await inspect(page);
    const authenticated = !['login', 'setup'].includes(scenario.options?.auth);
    const failures = [
      ...(observation.crash ? ['ErrorBoundary rendered'] : []),
      ...(!scenario.expectedError ? observation.errors.map((error) => `Error notice: ${error}`) : []),
      ...(scenario.expectedError && !observation.errors.length ? ['Expected API error was not rendered'] : []),
      ...errors.map((error) => `Page error: ${error}`),
      ...state.unknownRoutes.map((route) => `Missing fixture: ${route.method} ${route.pathname}`),
      ...state.blockedRequests.map((request) => `Blocked unexpected network: ${request.method} ${request.url}`),
      ...(observation.overflow ? [`Horizontal page overflow: ${observation.documentWidth}/${viewport.width}`] : []),
      ...(scenario.name.startsWith('developer') && observation.dialogHorizontalOverflow ? ['Developer overlay has horizontal overflow outside its code scroller'] : []),
      ...(observation.language !== locale ? [`Locale mismatch: ${observation.language}/${locale}`] : []),
      ...(observation.appearance !== (authenticated ? appearance : 'dark') ? [`Appearance mismatch: ${observation.appearance}`] : []),
    ];
    let screenshot;
    if (process.env.SCREENSHOTS === 'all' || failures.length || (locale === 'zh-CN' && appearance === 'dark' && viewport.width === 1440) || representatives.has(scenario.name)) {
      screenshot = join(outputDir, `${name}.png`);
      await page.screenshot({ path: screenshot, fullPage: true, animations: 'disabled' });
    }
    results.push({ name, scenario: scenario.name, path: scenario.path, locale, appearance, requestedViewport: viewport, ...extra, ...observation, screenshot, failures, apiRequests: state.requests });
    process.stdout.write(`${failures.length ? 'FAIL' : 'PASS'} ${name}${failures.length ? ` ${failures.join('; ')}` : ''}\n`);
  } catch (error) {
    const screenshot = join(outputDir, `${name}-failure.png`);
    await page.screenshot({ path: screenshot, fullPage: true }).catch(() => {});
    results.push({ name, scenario: scenario.name, path: scenario.path, locale, appearance, requestedViewport: viewport, screenshot, failures: [error.message, ...errors], apiRequests: state.requests, unknownRoutes: state.unknownRoutes });
    process.stdout.write(`FAIL ${name}: ${error.message.split('\n')[0]}\n`);
  } finally { await page.close(); }
}

try {
  const selectedScenarios = (statesOnly ? stateScenarios : [...scenarios, ...dialogs]).filter((scenario) => (!baseline || !scenario.newOnly) && (!requested || requested.includes(scenario.name)));
  for (const locale of languages) for (const appearance of appearances) {
    const context = await browser.newContext({ locale, timezoneId: 'Asia/Singapore', reducedMotion: 'reduce' });
    for (const viewport of primaryViewports) for (const scenario of selectedScenarios) await runCase(context, scenario, locale, appearance, viewport);
    if (!quick && !widths && !requested && !statesOnly) {
      for (const width of [1280, 820, 320]) for (const scenario of [...scenarios, ...dialogs].filter((item) => representatives.has(item.name))) await runCase(context, scenario, locale, appearance, { name: `w${width}`, width, height: width < 821 ? 844 : 800 });
      // CSS viewport + output scale reproduces browser zoom's layout space;
      // this records reflow equivalence, not an OS browser-zoom setting.
      for (const zoom of [200, 400]) for (const name of ['create-provider', 'settings-instance', 'developer']) {
        const scenario = [...scenarios, ...dialogs].find((item) => item.name === name);
        await runCase(context, scenario, locale, appearance, { name: 'zoom-equivalent', width: 1440 / (zoom / 100), height: 900 / (zoom / 100) }, { zoomEquivalent: zoom });
      }
    }
    await context.close();
  }
} finally {
  await browser.close();
  const report = { baseline, baseURL, browserVersion, nodeVersion: process.version, generatedAt: new Date().toISOString(), fixtureTime: NOW, mode: statesOnly ? 'states' : quick ? 'quick' : 'matrix', note: 'Isolated synthetic Admin API; no real provider requests. Unauthenticated screens intentionally use dark. Zoom cases test equivalent reflow viewport, not OS/browser zoom.', total: results.length, passed: results.filter((result) => !result.failures.length).length, failed: results.filter((result) => result.failures.length).length, results };
  await writeFile(join(outputDir, 'report.json'), JSON.stringify(report, null, 2));
  process.stdout.write(`Report: ${join(outputDir, 'report.json')} (${report.passed}/${report.total} passed)\n`);
  if (report.failed) process.exitCode = 1;
}
