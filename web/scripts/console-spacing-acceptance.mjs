#!/usr/bin/env node
// Shared feedback-layout audit against synthetic Admin API contracts only.
// External origins and undeclared APIs are rejected by installConsoleFixture.
import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { installConsoleFixture, fixtureResponse } from './console-acceptance-fixtures.mjs';
import { zhCN } from '../src/i18n/locales/zh-CN.ts';
import { enUS } from '../src/i18n/locales/en-US.ts';

assert.ok(process.env.PLAYWRIGHT_MODULE, 'PLAYWRIGHT_MODULE must name an installed module');
const { chromium } = await import(pathToFileURL(resolve(process.env.PLAYWRIGHT_MODULE)).href);
const baseURL = process.env.BASE_URL ?? 'http://127.0.0.1:5274/admin/';
const outputDir = resolve(process.env.OUTPUT_DIR ?? '/private/tmp/halro-system-polish-20261104');
const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_EXECUTABLE, args: ['--disable-background-networking', '--disable-component-update', '--disable-sync'] });
await mkdir(outputDir, { recursive: true });
const results = [];
const click = (page, name) => page.getByRole('button', { name, exact: true }).click();
const advisorOptions = { respond: (_state, method, path) => method === 'GET' && path === '/advisor-findings' ? { status: 200, data: { items: ['warn', 'ok', 'unknown'].map((status, index) => ({ rule: ['attempt_header_timeout_reachable', 'retry_fits_route_budget', 'suspended_scopes'][index], status, consequence: 'Synthetic server evidence', comparison: ['3m0s >= 2m0s', '1m0s × 1 = 1m0s <= 2m0s', 'admission gate not read'][index], evidence: index === 2 ? [] : [{ name: 'gateway.attempt_response_header_timeout', value: index === 0 ? '3m0s' : '1m0s' }, { name: 'gateway.route_total_timeout', value: '2m0s' }] })) } } : undefined };
const scenarios = [
  ...[['dashboard', ''], ['providers', 'providers'], ['credentials', 'providers?view=credentials'], ['proxies', 'providers?view=proxies'], ['deployments', 'deployments'], ['routes', 'routes'], ['projects', 'projects'], ['developer', 'developer'], ['policies', 'policies'], ['usage', 'usage'], ['usage-attempts', 'usage?tab=attempts'], ['usage-failures', 'usage?tab=failures'], ['run-governance', 'run-governance'], ['operations', 'operations'], ['cluster', 'cluster']].map(([name, path]) => ({ name, path })),
  ...['general', 'security', 'accounts', 'instance', 'config', 'custody', 'diagnostics'].map((pane) => ({ name: `settings-${pane}`, path: `settings/${pane}` })),
  ...[['provider', 'providers', 'providers', 'addProvider'], ['credential', 'providers?view=credentials', 'providers', 'addCredential'], ['proxy', 'providers?view=proxies', 'providers', 'addProxy'], ['deployment', 'deployments', 'deployments', 'create'], ['route', 'routes', 'routes', 'create'], ['project', 'projects', 'projects', 'create'], ['policy', 'policies', 'policies', 'create'], ['webhook', 'operations', 'operations', 'addWebhook']].map(([name, path, namespace, key]) => ({ name: `create-${name}`, path, open: (page, t) => click(page, t[namespace][key]) })),
  { name: 'readonly-usage', path: 'usage', options: { role: 'read_only' } },
  { name: 'usage-timezone-notice', path: 'usage', options: { respond: (state, method, path, query, body) => {
    if (method !== 'GET' || path !== '/usage/summary') return undefined;
    const result = fixtureResponse(state, method, path, query, body); result.data.timezone_changes = [{ period_id: '2026-11-03', timezone: 'UTC', timezone_version: 1 }]; return result;
  } } },
  { name: 'mfa-required', path: 'settings/security', options: { mfaSetupRequired: true } },
  { name: 'provider-error', path: 'providers', options: { respond: (_state, method, path) => method === 'GET' && path === '/providers' ? { status: 503, data: { code: 'synthetic_failure', error: 'Synthetic resource unavailable' } } : undefined } },
  { name: 'route-submit-error', path: 'routes', options: { respond: (_state, method, path) => method === 'POST' && path === '/routes' ? { status: 400, data: { code: 'invalid_request', error: 'Synthetic route save failed' } } : undefined }, open: async (page, t) => {
    await click(page, t.routes.create); await page.locator('.route-form-body input[role="combobox"]').fill('synthetic-new-alias'); await click(page.getByRole('dialog'), t.routes.createAndLoad); await page.locator('.route-form-body .notice.error').waitFor();
  } },
  { name: 'webhook-validation-error', path: 'operations', open: async (page, t) => { await click(page, t.operations.addWebhook); await click(page.getByRole('dialog'), t.operations.saveWebhook); await page.getByRole('dialog').locator('.notice.error').waitFor(); } },
  { name: 'price-details', path: 'deployments', open: async (page, t) => { await page.locator('button[aria-haspopup="dialog"]').first().click(); await click(page.getByRole('dialog').last(), t.deployments.adjustPrice); } },
  { name: 'login', path: 'login', options: { auth: 'login' } },
  { name: 'advisor-collapsed', path: 'settings/diagnostics', options: advisorOptions },
  { name: 'advisor-keyboard', path: 'settings/diagnostics', options: advisorOptions, open: async (page, _t, name) => {
    const details = page.locator('.advisor-evidence-details').first(); const summary = details.locator(':scope > summary');
    assert.equal(await details.evaluate((element) => element.open), false, 'Evidence is expanded by default');
    assert.equal(await details.locator('.advisor-evidence').isVisible(), false);
    const collapsedScreenshot = join(outputDir, `${name}-collapsed.png`); await page.screenshot({ path: collapsedScreenshot, fullPage: true });
    await summary.focus(); await page.keyboard.press('Enter');
    assert.equal(await details.evaluate((element) => element.open), true); assert.equal(await details.locator('.advisor-evidence').isVisible(), true);
    const measurements = await details.evaluate((element) => {
      const summaryBox = element.querySelector('summary').getBoundingClientRect(); const evidenceBox = element.querySelector('.advisor-evidence').getBoundingClientRect(); const comparisonBox = element.querySelector('.advisor-comparison').getBoundingClientRect();
      return { summaryHeight: summaryBox.height, summaryDisplay: getComputedStyle(element.querySelector('summary')).display, comparisonGap: Math.round(comparisonBox.top - evidenceBox.bottom), focused: document.activeElement === element.querySelector('summary') };
    });
    assert.ok(measurements.summaryHeight >= 44); assert.equal(measurements.summaryDisplay, 'list-item'); assert.equal(measurements.comparisonGap, 12); assert.equal(measurements.focused, true);
    await page.keyboard.press('Space'); assert.equal(await details.evaluate((element) => element.open), false); await page.keyboard.press('Enter');
    return { keyboard: 'Enter opens; Space closes; focus retained; Enter reopens', measurements, collapsedScreenshot };
  } },
  { name: 'contracts', path: 'routes', open: (page, t) => click(page, t.routes.create), contract: true },
  ...[['dashboard', '', '/dashboard'], ['deployments', 'deployments', '/deployments'], ['routes', 'routes', '/routes'], ['projects', 'projects', '/projects'], ['developer', 'developer', '/projects'], ['policies', 'policies', '/token-guard-policies'], ['usage', 'usage', '/usage/summary'], ['run-governance', 'run-governance', '/projects'], ['operations', 'operations', '/alerts'], ['cluster', 'cluster', '/cluster/status'], ['settings-security', 'settings/security', '/security/mfa'], ['settings-accounts', 'settings/accounts', '/admin-users'], ['settings-instance', 'settings/instance', '/settings'], ['settings-config', 'settings/config', '/system/config'], ['settings-custody', 'settings/custody', '/master-key/custody'], ['settings-diagnostics', 'settings/diagnostics', '/system/status']].map(([name, path, api]) => ({ name: `read-error-${name}`, path, expectedError: true, options: { respond: (_state, method, requestPath) => method === 'GET' && requestPath === api ? { status: 503, data: { code: 'synthetic_failure', error: 'Synthetic resource unavailable' } } : undefined } })),
];

async function collect(page) {
  return page.evaluate(() => {
    const visible = (element) => element.getClientRects().length > 0 && getComputedStyle(element).visibility !== 'hidden' && !element.closest('[hidden],.sr-only');
    const number = (value) => parseFloat(value) || 0;
    const rounded = (value) => Math.round(value * 100) / 100;
    const inspect = (element) => {
      const style = getComputedStyle(element); const parent = element.parentElement; const parentStyle = getComputedStyle(parent); const box = element.getBoundingClientRect();
      let previous = element.previousElementSibling; while (previous && !visible(previous)) previous = previous.previousElementSibling;
      let next = element.nextElementSibling; while (next && !visible(next)) next = next.nextElementSibling;
      return { className: element.className, parentClass: parent.className, parentDisplay: parentStyle.display, rowGap: number(parentStyle.rowGap), marginTop: number(style.marginTop), marginBottom: number(style.marginBottom), gapAbove: previous ? rounded(box.top - previous.getBoundingClientRect().bottom) : null, gapBelow: next ? rounded(next.getBoundingClientRect().top - box.bottom) : null, text: element.textContent.trim().slice(0, 150) };
    };
    const feedback = [...document.querySelectorAll('.notice,.field-hint,.form-note,.field > small')].filter(visible).map(inspect);
    const fields = [...document.querySelectorAll('.form-grid > .field,.price-form-grid > .field')].filter(visible).map(inspect);
    const diagnostics = [...document.querySelectorAll('.system-card.diagnostic-details')].filter(visible).map((element) => ({ className: element.className, paddingTop: number(getComputedStyle(element).paddingTop), paddingLeft: number(getComputedStyle(element).paddingLeft), height: rounded(element.getBoundingClientRect().height) }));
    const diagnosticParagraphs = [...document.querySelectorAll('.diagnostic-details > p:not(.notice)')].filter(visible).map((element) => {
      const style = getComputedStyle(element); const header = element.parentElement.querySelector(':scope > summary'); const headerStyle = getComputedStyle(header); const parentLeft = element.parentElement.getBoundingClientRect().left;
      return { className: element.className, paddingLeft: number(style.paddingLeft), textInset: rounded(element.getBoundingClientRect().left + number(style.paddingLeft) - parentLeft), headerTextInset: rounded(header.getBoundingClientRect().left + number(headerStyle.paddingLeft) - parentLeft) };
    });
    const priceGroups = [...document.querySelectorAll('.price-version-form > .price-form-grid')].filter(visible).map(inspect);
    return { documentWidth: document.documentElement.scrollWidth, viewportWidth: innerWidth, feedback, fields, diagnostics, diagnosticParagraphs, priceGroups };
  });
}

async function contract(page) {
  // Use the real production Field element, and actual notice class contracts.
  // The isolated host is explicitly a CSS harness, not a business-flow screenshot.
  return page.locator('.route-form-body .field').first().evaluate((field) => {
    const host = document.createElement('section'); host.className = 'main spacing-contract'; host.style.cssText = 'position:relative;padding:24px;background:var(--surface-panel);max-width:680px;margin:auto';
    document.querySelector('.modal-backdrop')?.remove(); document.querySelector('.app')?.setAttribute('hidden', '');
    const rows = [];
    for (const container of ['block', 'form-grid', 'price-form-grid', 'settings-form', 'capability-detection-panel', 'usage-summary', 'disclosure-body', 'table-cell']) {
      for (const variant of ['', 'warning', 'error', 'info', 'success']) {
        const wrapper = document.createElement('div'); wrapper.className = container === 'block' ? '' : container;
        const title = document.createElement('strong'); title.textContent = `${container} / ${variant || 'normal'}`; host.append(title);
        const first = ['block', 'form-grid', 'price-form-grid', 'settings-form', 'disclosure-body', 'table-cell'].includes(container) ? field.cloneNode(true) : document.createElement('div');
        if (!first.textContent) first.textContent = 'Previous content';
        first.style.gridColumn = '1 / -1'; first.querySelectorAll('input').forEach((input) => { input.id = ''; input.value = ''; });
        const notice = document.createElement('div'); notice.className = `notice ${variant}`; notice.textContent = 'Synthetic feedback spacing';
        const after = document.createElement('button'); after.className = 'button secondary'; after.textContent = 'Next action';
        notice.style.gridColumn = '1 / -1'; after.style.gridColumn = '1 / -1';
        if (container === 'table-cell') {
          const table = document.createElement('table'); const body = document.createElement('tbody'); const row = document.createElement('tr'); const cell = document.createElement('td'); cell.append(first, notice, after); row.append(cell); body.append(row); table.append(body); wrapper.append(table);
        } else wrapper.append(first, notice, after);
        host.append(wrapper); document.body.append(host);
        const parentStyle = getComputedStyle(notice.parentElement); const noticeStyle = getComputedStyle(notice); const fieldStyle = getComputedStyle(first);
        const metrics = { container, variant: variant || 'normal', parentDisplay: parentStyle.display, rowGap: parseFloat(parentStyle.rowGap) || 0, fieldMarginBottom: parseFloat(fieldStyle.marginBottom), noticeMarginTop: parseFloat(noticeStyle.marginTop), noticeMarginBottom: parseFloat(noticeStyle.marginBottom), gapAbove: Math.round(notice.getBoundingClientRect().top - first.getBoundingClientRect().bottom), gapBelow: Math.round(after.getBoundingClientRect().top - notice.getBoundingClientRect().bottom) };
        // Measure terminal and followed contexts independently, rather than
        // repeatedly removing/reinserting a sibling of the same live element.
        const terminalParent = notice.parentElement.cloneNode(false); const terminalNotice = notice.cloneNode(true); terminalParent.append(terminalNotice); host.append(terminalParent);
        terminalNotice.getBoundingClientRect(); metrics.lastNoticeMarginBottom = parseFloat(getComputedStyle(terminalNotice).marginBottom); terminalParent.remove(); rows.push(metrics);
      }
    }
    return rows;
  });
}

try {
  for (const locale of process.env.LOCALES?.split(',') ?? ['zh-CN', 'en-US']) for (const appearance of process.env.APPEARANCES?.split(',') ?? ['dark', 'light']) for (const width of process.env.WIDTHS?.split(',').map(Number) ?? [1440, 390]) for (const scenario of scenarios) {
    if (process.env.CASES && !process.env.CASES.split(',').includes(scenario.name)) continue;
    const name = `${scenario.name}-${locale}-${appearance}-${width}`;
    const context = await browser.newContext({ viewport: { width, height: 1000 }, locale, reducedMotion: 'reduce' }); const page = await context.newPage(); page.setDefaultTimeout(8000);
    const pageErrors = []; page.on('pageerror', (error) => pageErrors.push(error.message));
    const state = await installConsoleFixture(page, { baseURL, locale, appearance, ...scenario.options });
    try {
      await page.goto(new URL(scenario.path, baseURL).href, { waitUntil: 'networkidle' });
      await page.waitForFunction(() => !document.querySelector('.boot,.loading'));
      const interaction = scenario.open ? await scenario.open(page, locale === 'zh-CN' ? zhCN : enUS, name) : undefined;
      // Expand existing disclosures with their native controls; no production state changes.
      if (scenario.open && !scenario.contract) for (const section of await page.locator('[role="dialog"] details').all()) if (!(await section.evaluate((element) => element.open))) await section.locator(':scope > summary').click();
      await page.evaluate(async () => { await Promise.all(document.getAnimations().filter((animation) => animation.effect?.getTiming().iterations !== Infinity).map((animation) => animation.finished.catch(() => {}))); });
      if (scenario.expectedError) await page.locator('.notice.error:visible').first().waitFor();
      const metrics = await collect(page);
      const contracts = scenario.contract ? await contract(page) : undefined;
      assert.deepEqual(state.unknownRoutes, [], 'Undeclared API request'); assert.deepEqual(state.blockedRequests, [], 'External request'); assert.deepEqual(pageErrors, [], 'Page error');
      assert.equal(await page.locator('.crash-panel').count(), 0, 'ErrorBoundary');
      if (!scenario.contract) assert.ok(metrics.documentWidth <= width + 1, 'Document overflow');
      if (process.env.ASSERT_SPACING) for (const field of metrics.fields) assert.equal(field.marginBottom, 0, `${field.parentClass} doubles field spacing`);
      if (process.env.ASSERT_SPACING) for (const diagnostic of metrics.diagnostics) { assert.equal(diagnostic.paddingTop, 0, 'Diagnostic card duplicates header/body padding'); assert.equal(diagnostic.paddingLeft, 0, 'Diagnostic card duplicates header/body inset'); }
      if (process.env.ASSERT_SPACING) for (const paragraph of metrics.diagnosticParagraphs) { assert.equal(paragraph.paddingLeft, 16, 'Diagnostic body paragraph loses its inset'); assert.equal(paragraph.textInset, paragraph.headerTextInset, 'Diagnostic paragraph does not align with header text'); }
      if (process.env.ASSERT_SPACING) for (const price of metrics.priceGroups) assert.equal(price.marginBottom, 16, 'Zero-gap price form loses separation between field groups');
      if (process.env.ASSERT_SPACING) for (const feedback of metrics.feedback.filter((item) => item.className.includes('notice') && ['grid', 'flex'].includes(item.parentDisplay) && item.rowGap > 0)) {
        assert.equal(feedback.marginTop, 0, `${feedback.parentClass} adds a feedback top margin to gap`);
        assert.equal(feedback.marginBottom, 0, `${feedback.parentClass} adds a feedback bottom margin to gap`);
      }
      if (process.env.ASSERT_SPACING && contracts) for (const row of contracts.filter((item) => ['form-grid', 'price-form-grid', 'settings-form', 'capability-detection-panel', 'usage-summary'].includes(item.container))) {
        assert.equal(row.noticeMarginTop, 0, `${row.container}/${row.variant} adds top margin to gap`);
        assert.equal(row.noticeMarginBottom, 0, `${row.container}/${row.variant} adds bottom margin to gap`);
        if (['form-grid', 'price-form-grid', 'settings-form'].includes(row.container)) assert.equal(row.fieldMarginBottom, 0, `${row.container} doubles field spacing`);
      }
      if (process.env.ASSERT_SPACING && contracts) for (const row of contracts.filter((item) => ['disclosure-body', 'table-cell'].includes(item.container))) assert.equal(row.lastNoticeMarginBottom, 0, `${row.container} retains terminal notice margin: ${JSON.stringify(row)}`);
      const capture = process.env.SCREENSHOTS === 'all' || scenario.contract || scenario.name.startsWith('advisor-') || ['create-route', 'readonly-usage', 'create-provider', 'login', 'route-submit-error', 'webhook-validation-error'].includes(scenario.name);
      const screenshot = capture ? join(outputDir, `${name}.png`) : null;
      if (screenshot) await page.screenshot({ path: screenshot, fullPage: true });
      results.push({ name, passed: true, metrics, contracts, interaction, screenshot }); process.stdout.write(`PASS ${name}\n`);
    } catch (error) {
      const screenshot = join(outputDir, `${name}-failure.png`); await page.screenshot({ path: screenshot, fullPage: true }).catch(() => {});
      results.push({ name, passed: false, error: error.stack, unknownRoutes: state.unknownRoutes, pageErrors, screenshot }); process.stdout.write(`FAIL ${name}: ${error.message.split('\n')[0]}\n`);
    } finally { await context.close(); }
    await writeFile(join(outputDir, 'spacing.json'), JSON.stringify({ baseURL, syntheticOnly: true, total: results.length, passed: results.filter((row) => row.passed).length, failed: results.filter((row) => !row.passed).length, results }, null, 2));
  }
} finally { await browser.close(); }
process.exitCode = results.some((row) => !row.passed) ? 1 : 0;
