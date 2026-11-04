#!/usr/bin/env node
// Focused ClusterPage acceptance. All Admin API calls are synthetic and strict;
// no real cluster, provider, Chrome profile, login flow or Docker API is touched.
import assert from 'node:assert/strict';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { installConsoleFixture } from './console-acceptance-fixtures.mjs';
import { zhCN } from '../src/i18n/locales/zh-CN.ts';
import { enUS } from '../src/i18n/locales/en-US.ts';

const synced = { mode: 'ha', cluster_id: 'synthetic-cluster', incarnation: 'inc_synthetic', node_id: 'halro-0', role: 'primary', term: 9, promised_term: 9, durable_index: 17, confirmed_index: 17, applied_index: 17, startup_ready: true, projection: { index: 17, metadata_epoch: 3, metadata_sequence: 23 }, peers: [{ node_id: 'halro-1', connected: true }, { node_id: 'halro-2', connected: true }] };
const attention = { ...synced, node_id: `Node-MixedCase-${'replica-zone-'.repeat(12)}`, role: 'awaiting_decision', startup_ready: false, durable_index: 17, confirmed_index: 12, applied_index: 8, projection: { index: 8, metadata_epoch: 3, metadata_sequence: 19 }, peers: [{ node_id: `Peer-MixedCase-${'connected-zone-'.repeat(10)}`, connected: true }, { node_id: `Peer-MixedCase-${'disconnected-zone-'.repeat(10)}`, connected: false }] };
const scenarios = [
  { name: 'standalone', data: { mode: 'standalone' } },
  { name: 'ha-synced', data: synced, timing: true },
  { name: 'ha-attention', data: attention },
  { name: 'empty-peers', data: { ...synced, peers: [] } },
  { name: 'initial-error-recovery', data: synced, initialError: true },
  { name: 'refetch-error-recovery', data: synced, refetchError: true },
];
const interpolate = (text, values) => text.replace(/{{(\w+)}}/g, (_match, name) => String(values[name]));
const delay = (milliseconds) => new Promise((done) => setTimeout(done, milliseconds));
async function eventually(predicate, message, timeout = 8000) { const end = Date.now() + timeout; while (Date.now() < end) { if (await predicate()) return; await delay(10); } assert.fail(message); }
function observeClusterTimers() {
  window.__clusterFetches ??= []; window.__clusterIntervals ??= [];
  if (!window.fetch.__clusterAudit) { const original = window.fetch; const wrapped = function(...args) { const value = args[0]; const url = typeof value === 'string' ? value : value instanceof URL ? value.href : value.url; if (new URL(url, location.href).pathname === '/admin/api/v1/cluster/status') window.__clusterFetches.push(Date.now()); return Reflect.apply(original, this, args); }; wrapped.__clusterAudit = true; window.fetch = wrapped; }
  if (!window.setInterval.__clusterAudit) { const original = window.setInterval; const wrapped = function(...args) { window.__clusterIntervals.push(Number(args[1])); return Reflect.apply(original, this, args); }; wrapped.__clusterAudit = true; window.setInterval = wrapped; }
}
const refreshButton = (page, words) => page.getByRole('button', { name: words.cluster.refresh, exact: true });
async function noOverflow(page) { assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth + 1), 'Cluster document horizontally overflows'); }
async function clickRefreshByKeyboard(page, words) { const button = refreshButton(page, words); await button.focus(); await page.keyboard.press('Enter'); return button; }
async function inspect(page, words, data) {
  const root = page.locator('.cluster-page'); await root.waitFor(); const metadata = root.locator('.cluster-refresh-meta'); await metadata.waitFor();
  assert.equal(await metadata.innerText(), words.cluster.autoRefresh); assert.equal(await root.locator('.count').count(), 0, 'Polling metadata still uses a count badge');
  const meta = await metadata.evaluate((element) => { const css = getComputedStyle(element); return { tag: element.tagName, fontSize: parseFloat(css.fontSize), fontWeight: parseFloat(css.fontWeight), fontFamily: css.fontFamily, color: css.color, background: css.backgroundColor, border: parseFloat(css.borderTopWidth) + parseFloat(css.borderBottomWidth) }; });
  assert.ok(meta.fontSize <= 12 && meta.fontWeight <= 500); assert.ok(!meta.fontFamily.includes('monospace')); assert.equal(meta.border, 0); assert.ok(meta.background === 'rgba(0, 0, 0, 0)' || meta.background === 'transparent');
  assert.equal(await metadata.evaluate((element) => Boolean(element.closest('.notice,h1,h2,[role="alert"]'))), false, 'Routine polling is presented as a headline or alert');
  const button = refreshButton(page, words); assert.equal(await button.isEnabled(), true); assert.equal(await button.getAttribute('aria-busy'), 'false');
  const refreshWidth = await button.evaluate((element) => element.getBoundingClientRect().width); const headerWidth = await root.locator('.page-header').evaluate((element) => element.getBoundingClientRect().width);
  if (await page.evaluate(() => innerWidth === 390)) assert.ok(refreshWidth < headerWidth * 0.8, 'Narrow refresh button still fills the whole row');
  if (data.mode === 'standalone') { assert.equal(await root.getByRole('heading', { name: words.cluster.standaloneTitle, exact: true }).isVisible(), true); assert.equal(await root.locator('.cluster-status,.cluster-details,.cluster-peer-state').count(), 0); }
  else {
    const details = root.locator('.cluster-details'); assert.equal(await details.count(), 3); assert.equal(await root.locator('.cluster-scope').innerText(), words.cluster.localOnly); assert.equal(await root.locator('.cluster-scope').isVisible(), true);
    const lag = data.durable_index - data.confirmed_index; const unapplied = data.confirmed_index - data.applied_index; const disconnected = data.peers.filter((peer) => !peer.connected).length;
    assert.deepEqual(await details.evaluateAll((elements) => elements.map((element) => element.open)), [!data.startup_ready, lag > 0 || unapplied > 0, disconnected > 0], 'Important state does not control default disclosures');
    assert.ok((await details.nth(0).locator(':scope > summary').innerText()).includes(data.node_id));
    assert.ok((await details.nth(1).locator(':scope > summary').innerText()).includes(interpolate(words.cluster.progressSummary, { unconfirmed: lag, unapplied })));
    assert.ok((await details.nth(2).locator(':scope > summary').innerText()).includes(interpolate(words.cluster.peersSummary, { connected: data.peers.length - disconnected, total: data.peers.length, disconnected })));
    if (!data.startup_ready) assert.equal(await details.nth(0).locator('.cluster-attention').isVisible(), true);
    if (lag || unapplied) assert.equal(await details.nth(1).locator('.cluster-attention').isVisible(), true);
    if (disconnected) assert.equal(await details.nth(2).locator('.cluster-attention').isVisible(), true);
  }
  await noOverflow(page); return { metadata: meta, refreshWidth, headerWidth, localScopeVisible: data.mode === 'ha', defaultOpen: await root.locator('.cluster-details').evaluateAll((elements) => elements.map((element) => element.open)) };
}
async function disclosures(page, words, data) {
  if (data.mode !== 'ha') return [];
  const root = page.locator('.cluster-page'); const checks = []; const details = root.locator('.cluster-details');
  for (let index = 0; index < 3; index++) {
    const item = details.nth(index); const summary = item.locator(':scope > summary'); const wasOpen = await item.evaluate((element) => element.open); await summary.focus(); await page.keyboard.press('Enter'); assert.equal(await item.evaluate((element) => element.open), !wasOpen); await page.keyboard.press('Space'); assert.equal(await item.evaluate((element) => element.open), wasOpen); assert.equal(await summary.evaluate((element) => element === document.activeElement), true);
    if (!wasOpen) await summary.click();
    assert.ok(await summary.evaluate((element) => element.getBoundingClientRect().height >= 44)); await noOverflow(page); checks.push({ index, enterAndSpace: true, focusRetained: true });
  }
  const values = { [words.cluster.durableIndex]: data.durable_index, [words.cluster.confirmedIndex]: data.confirmed_index, [words.cluster.appliedIndex]: data.applied_index, [words.cluster.confirmationLag]: data.durable_index - data.confirmed_index, [words.cluster.applyLag]: data.confirmed_index - data.applied_index };
  for (const [label, value] of Object.entries(values)) { const term = root.locator('dt').filter({ hasText: label }); assert.equal(await term.count(), 1); assert.equal((await term.locator('..').locator('dd').innerText()).trim(), String(value)); }
  assert.equal(await root.locator('.cluster-caption').innerText(), words.cluster.peersDescription); assert.equal(await root.locator('.cluster-caption').isVisible(), true, 'Authenticated-session boundary disappeared');
  assert.equal(await root.locator('.cluster-peer-state').count(), data.peers.length);
  assert.ok((await root.locator('.cluster-details dt,.cluster-details dt code').evaluateAll((elements) => elements.map((element) => getComputedStyle(element).textTransform))).every((value) => value === 'none'), 'Cluster identifiers inherit capitalization');
  assert.ok(await root.locator('.cluster-status').evaluate((element) => element.scrollWidth <= element.clientWidth + 1), 'Long cluster identifiers overflow or clip inside the panel');
  for (const peer of data.peers) { const row = root.locator('.cluster-details dl > div').filter({ hasText: peer.node_id }); assert.equal(await row.count(), 1); assert.ok((await row.innerText()).includes(peer.connected ? words.cluster.connected : words.cluster.disconnected)); }
  if (!data.peers.length) assert.equal(await root.getByText(words.cluster.noPeers, { exact: true }).isVisible(), true);
  return checks;
}
async function pollingAndManual(page, words, control) {
  await page.evaluate(observeClusterTimers);
  const refresh = refreshButton(page, words); await page.locator('#main-content').focus(); await page.keyboard.press('Tab'); assert.equal(await refresh.evaluate((element) => element === document.activeElement), true, 'Refresh is not reachable by Tab from main content');
  const before = control.count; control.holdNext = true; await page.keyboard.press('Enter'); await eventually(() => control.active === 1, 'Manual refresh did not start');
  // The intercepted GET starts before React's query subscription has painted
  // its pending state. Observe the DOM state rather than racing that notify.
  await eventually(() => refresh.isDisabled(), 'Pending refresh was not disabled'); assert.equal(await refresh.getAttribute('aria-busy'), 'true');
  await page.keyboard.press('Enter'); await page.keyboard.press('Space'); await refresh.evaluate((element) => element.click()); await delay(30); assert.equal(control.count, before + 1, 'Manual refresh is reentrant');
  control.release(); await eventually(async () => !(await refresh.isDisabled()), 'Refresh did not settle'); assert.equal(control.maxActive, 1);
  const pause = await page.evaluate(() => Date.now() + 250); await page.clock.pauseAt(new Date(pause)); const beforePolling = control.count;
  for (let cycle = 0; cycle < 2; cycle++) { const previous = control.count; await page.clock.runFor(10_100); await eventually(() => control.count === previous + 1, '10-second automatic polling stopped or duplicated'); await page.clock.runFor(100); await eventually(async () => !(await refresh.isDisabled()), 'Automatic refresh did not settle'); }
  const observed = await page.evaluate(() => ({ fetches: window.__clusterFetches, intervals: window.__clusterIntervals })); assert.ok(observed.intervals.includes(10_000), 'No 10-second refetch interval was registered');
  const auto = observed.fetches.slice(-2); assert.ok(auto[1] - auto[0] >= 9_900 && auto[1] - auto[0] <= 10_500, `Unexpected automatic polling interval: ${auto[1] - auto[0]}ms`);
  return { keyboardReachable: true, pendingDisabled: true, nonReentrant: true, maximumConcurrentRequests: control.maxActive, manualRequests: 1, automaticRequests: control.count - beforePolling, virtualAutoRequestTimes: auto, intervalMs: auto[1] - auto[0], registeredIntervalMs: 10_000, note: 'Playwright browser clock advances timers; callbacks issue actual synthetic GETs.' };
}

assert.ok(process.env.PLAYWRIGHT_MODULE, 'PLAYWRIGHT_MODULE is required'); const { chromium } = await import(pathToFileURL(resolve(process.env.PLAYWRIGHT_MODULE)).href);
const baseURL = process.env.BASE_URL ?? 'http://127.0.0.1:5275/admin/'; assert.ok(['localhost', '127.0.0.1', '[::1]'].includes(new URL(baseURL).hostname));
const outputDir = resolve(process.env.OUTPUT_DIR ?? '/private/tmp/halro-cluster-redesign-20261104'); await mkdir(outputDir, { recursive: true }); const selectedNames = process.env.CASE_NAMES_FILE ? new Set(JSON.parse(await readFile(process.env.CASE_NAMES_FILE, 'utf8'))) : undefined;
const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_EXECUTABLE, args: ['--disable-background-networking', '--disable-component-update', '--disable-sync'] }); const results = [];
try {
  for (const locale of process.env.LOCALES?.split(',') ?? ['zh-CN', 'en-US']) for (const appearance of process.env.APPEARANCES?.split(',') ?? ['dark', 'light']) for (const width of process.env.WIDTHS?.split(',').map(Number) ?? [1440, 390]) for (const scenario of scenarios) {
    if (process.env.CASES && !process.env.CASES.split(',').includes(scenario.name)) continue; const name = `${scenario.name}-${locale}-${appearance}-${width}`; if (selectedNames && !selectedNames.has(name)) continue;
    const context = await browser.newContext({ locale, reducedMotion: 'reduce', timezoneId: 'Asia/Singapore', viewport: { width, height: 1000 } }); const page = await context.newPage(); page.setDefaultTimeout(8000); if (scenario.timing) await page.clock.install({ time: new Date('2026-11-04T12:00:00Z') }); await page.addInitScript(observeClusterTimers);
    const errors = []; page.on('pageerror', (error) => errors.push(error.message)); const state = await installConsoleFixture(page, { baseURL, locale, appearance });
    const control = { count: 0, active: 0, maxActive: 0, error: Boolean(scenario.initialError), holdNext: false, release: () => {} }; const words = locale === 'zh-CN' ? zhCN : enUS;
    await page.route(new URL('/admin/api/v1/cluster/status', baseURL).href, async (route) => {
      if (route.request().method() !== 'GET') { state.unknownRoutes.push({ method: route.request().method(), pathname: '/cluster/status' }); await route.abort('blockedbyclient'); return; }
      state.requests.push({ method: 'GET', pathname: '/cluster/status', query: '' }); control.count++; control.active++; control.maxActive = Math.max(control.maxActive, control.active);
      try { if (control.holdNext) { control.holdNext = false; await new Promise((release) => { control.release = release; }); } await route.fulfill({ status: control.error ? 503 : 200, contentType: 'application/json', body: JSON.stringify(control.error ? { code: 'synthetic_cluster_unavailable', error: 'Synthetic cluster status unavailable' } : scenario.data) }); } finally { control.active--; }
    });
    try {
      await page.goto(new URL('cluster', baseURL).href, { waitUntil: 'networkidle' }); const root = page.locator('.cluster-page'); await root.waitFor(); let errorScreenshot;
      if (scenario.initialError || scenario.refetchError) {
        if (scenario.refetchError) { await root.locator('.cluster-status').waitFor(); control.error = true; await clickRefreshByKeyboard(page, words); }
        const alert = root.locator('.notice.error'); await alert.waitFor(); assert.equal(await root.locator('.cluster-status,.cluster-mode').count(), 0, 'Failed data remains presented as current'); errorScreenshot = join(outputDir, `${name}-error.png`); await root.screenshot({ path: errorScreenshot });
        control.error = false; await eventually(async () => !(await refreshButton(page, words).isDisabled()), 'Failed refresh never settled'); await clickRefreshByKeyboard(page, words); await root.locator('.cluster-status').waitFor(); await alert.waitFor({ state: 'hidden' });
      }
      await eventually(async () => !(await refreshButton(page, words).isDisabled()), 'Initial cluster request did not settle'); const initial = await inspect(page, words, scenario.data); const screenshot = join(outputDir, `${name}.png`); await root.screenshot({ path: screenshot });
      const keyboard = await disclosures(page, words, scenario.data); const expandedScreenshot = scenario.data.mode === 'ha' ? join(outputDir, `${name}-expanded.png`) : null; if (expandedScreenshot) await root.screenshot({ path: expandedScreenshot });
      const timing = scenario.timing ? await pollingAndManual(page, words, control) : undefined;
      assert.deepEqual(errors, []); assert.deepEqual(state.unknownRoutes, []); assert.deepEqual(state.blockedRequests, []); assert.deepEqual(state.writes, []); assert.ok(state.requests.every((request) => request.method === 'GET')); assert.equal(await page.locator('.crash-panel').count(), 0); await noOverflow(page);
      results.push({ name, passed: true, initial, keyboard, timing, errorRecovery: Boolean(scenario.initialError || scenario.refetchError), clusterRequests: control.count, screenshot, expandedScreenshot, errorScreenshot }); process.stdout.write(`PASS ${name}\n`);
    } catch (error) { control.release(); const screenshot = join(outputDir, `${name}-failure.png`); await page.screenshot({ path: screenshot, fullPage: true }).catch(() => {}); results.push({ name, passed: false, error: error.stack, screenshot, unknownRoutes: state.unknownRoutes, pageErrors: errors }); process.stdout.write(`FAIL ${name}: ${error.message.split('\n')[0]}\n`); }
    finally { await context.close(); }
    await writeFile(join(outputDir, 'report.json'), JSON.stringify({ baseURL, syntheticOnly: true, note: 'Only ClusterPage. Strict real-shaped synthetic API, no provider/real Chrome/Docker API/login changes. Polling uses Playwright virtual browser time plus observed 10-second interval registration and actual intercepted GETs. Keyboard/ARIA checks are not VoiceOver testing.', total: results.length, passed: results.filter((result) => result.passed).length, failed: results.filter((result) => !result.passed).length, results }, null, 2));
  }
} finally { await browser.close(); }
process.exitCode = results.some((result) => !result.passed) ? 1 : 0;
