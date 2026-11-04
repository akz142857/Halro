#!/usr/bin/env node
// Isolated Advisor acceptance. Real-shaped synthetic Admin API only; no provider,
// real Chrome profile or Docker access. No extra npm dependency is installed.
import assert from 'node:assert/strict';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { installConsoleFixture } from './console-acceptance-fixtures.mjs';
import { zhCN } from '../src/i18n/locales/zh-CN.ts';
import { enUS } from '../src/i18n/locales/en-US.ts';

export const ruleIDs = ['attempt_header_timeout_reachable', 'retry_fits_route_budget', 'attempt_budget_reaches_fanout', 'suspended_scopes', 'unclassified_refusals'];
const term = (name, value) => ({ name, value: String(value) });
const finding = (rule, status, evidence, comparison, consequence) => ({ rule, status, evidence, comparison, consequence });
// Values and comparisons follow internal/advisor/advisor.go, including ceiling
// division for fan-out and the distinction between total/per-target attempts.
export function knownFindings(warn = false) {
  const header = warn ? '3m0s' : '30s'; const route = '2m0s'; const total = warn ? 3 : 8; const perTarget = 2; const reachable = Math.ceil(total / perTarget);
  return [
    finding(ruleIDs[0], warn ? 'warn' : 'ok', [term('gateway.attempt_response_header_timeout', header), term('gateway.route_total_timeout', route)], warn ? '3m0s >= 2m0s' : '30s < 2m0s', warn ? 'The request deadline fires before the per-attempt deadline; shorten the per-attempt deadline.' : 'A hanging attempt is cut at 30s, leaving 1m30s for another candidate.'),
    finding(ruleIDs[1], warn ? 'warn' : 'ok', [term('gateway.attempt_response_header_timeout', header), term('retry.max_attempts_per_target', perTarget), term('gateway.route_total_timeout', route), term('attempts_that_fit_the_request_budget', warn ? 0 : 4)], warn ? '3m0s × 2 = 6m0s > 2m0s' : '30s × 2 = 1m0s <= 2m0s', warn ? 'Retries exceed the request deadline; reduce retries or per-attempt timeout.' : 'Configured per-target attempts fit inside the request budget.'),
    finding(ruleIDs[2], warn ? 'warn' : 'ok', [term('gateway.max_total_attempts', total), term('retry.max_attempts_per_target', perTarget), term('candidates_the_budget_reaches', reachable), term('widest_fan_out', 4), term('widest_fan_out_public_model', 'synthetic-chat')], `ceil(${total} / ${perTarget}) = ${reachable} ${warn ? '<' : '>='} 4`, warn ? 'The attempt budget reaches only two of four candidates; increase the total attempt budget.' : 'The attempt budget reaches all four candidates.'),
    finding(ruleIDs[3], warn ? 'warn' : 'ok', warn ? [term('suspended_scopes', 2), term('credential:synthetic_credential', 'invalid_credential 401 (indefinite)'), term('deployment:synthetic_deployment', 'unclassified'), term('gateway.attempt_response_header_timeout', header)] : [term('suspended_scopes', 0)], warn ? '2 scopes suspended, 1 of them indefinitely' : '0 scopes suspended', warn ? 'Suspended scopes are skipped; fix the credential or explicitly clear indefinite suspension.' : 'Every configured candidate is admissible.'),
    finding(ruleIDs[4], warn ? 'warn' : 'ok', [term('refusals_observed', 12), term('refusals_unclassified', warn ? 3 : 0), ...(warn ? [term('unclassified_provider_status:503', 2), term('unclassified_provider_status:none', 1)] : [])], warn ? '3 of 12 refusals unclassified' : '0 of 12 refusals unclassified', warn ? 'Three refusals are unclassified; inspect quota and credential failures.' : 'All observed upstream refusals have classified reasons.'),
  ];
}
const unknown = [
  finding(ruleIDs[2], 'unknown', [term('gateway.max_total_attempts', 8), term('retry.max_attempts_per_target', 2), term('candidates_the_budget_reaches', 4)], 'ceil(8 / 2) = 4, widest fan-out unread', 'Route topology was not available.'),
  finding(ruleIDs[3], 'unknown', [], 'admission gate not read', 'The live admission gate was not available.'),
  finding(ruleIDs[4], 'unknown', [], 'refusal classification not read', 'The live classification counter was not available.'),
];
const newRule = finding('synthetic_new_upstream_rule', 'warn', [term('synthetic.new_rule_revision', '1')], 'synthetic revision 1 < required revision 2', 'Synthetic upstream rule: renew the synthetic lease before another request.');
export const scenarios = [
  { name: 'all-ok', findings: knownFindings() },
  { name: 'all-warn', findings: knownFindings(true) },
  { name: 'warn-unknown', findings: [knownFindings()[1], unknown[0], knownFindings(true)[0], knownFindings()[4], unknown[1]] },
  { name: 'unknown-only', findings: unknown },
  { name: 'empty', findings: [] },
  { name: 'new-rule', findings: [...knownFindings(), newRule], fallback: newRule },
  { name: 'missing-evidence', findings: knownFindings(true).map(({ evidence: _evidence, ...rest }) => rest) },
  { name: 'long-key', findings: [finding(newRule.rule, 'warn', [term(`gateway.synthetic_option.${'long_identifier_'.repeat(18)}`, 'synthetic-value-'.repeat(30)), term(`deployment:${'synthetic_scope_'.repeat(16)}`, 'unclassified')], `synthetic comparison ${'long-comparison-'.repeat(40)}`, 'Synthetic upstream rule: inspect this long synthetic identifier and renew its lease.')] },
];

function ruleTitle(words, rule) { return words.advisor.rules[rule] ?? rule; }
function displayedEvidence(words, evidence) {
  if (!['gateway.attempt_response_header_timeout', 'gateway.route_total_timeout'].includes(evidence.name)) return evidence.value;
  // These are the three deliberate fixture durations, not a second production
  // duration parser. The UI humanizes durations; the original remains in math.
  const display = { '30s': ['seconds', 30], '2m0s': ['minutes', 2], '3m0s': ['minutes', 3] }[evidence.value];
  return display ? words.advisor.duration[display[0]].replace('{{count}}', display[1]) : evidence.value;
}
function rowFor(panel, words, rule) {
  const byData = panel.locator(`.advisor-finding[data-rule="${rule}"]`);
  // The concrete choice is made in inspect; stable data-rule takes precedence.
  return { byData, fallback: panel.locator('.advisor-finding').filter({ hasText: ruleTitle(words, rule) }) };
}
async function detailsFor(row) { return await row.evaluate((element) => element.tagName === 'DETAILS') ? row : row.locator('details').first(); }
async function assertNoDocumentOverflow(page) { assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth + 1), 'Document horizontally overflows'); }

async function inspect(page, scenario, words, width) {
  const panel = page.locator('.advisor-findings'); await panel.waitFor();
  assert.equal(await panel.evaluate((element) => element.tagName), 'SECTION', 'The entire diagnostic panel is still an outer disclosure');
  const rows = panel.locator('.advisor-finding'); assert.equal(await rows.count(), scenario.findings.length, 'A rule disappeared or duplicated');
  const expectedTone = scenario.findings.some((item) => item.status === 'warn') ? 'warn' : scenario.findings.some((item) => item.status === 'unknown') ? 'unknown' : scenario.findings.length ? 'ok' : 'empty';
  const overall = panel.locator('.advisor-overall-status'); assert.ok((await overall.getAttribute('class')).split(/\s+/).includes(expectedTone), `Overall status should be ${expectedTone}`);
  const order = await rows.evaluateAll((elements) => elements.map((element) => ({ rule: element.dataset.rule, status: ['warn', 'unknown', 'ok'].find((status) => element.classList.contains(status)), text: element.innerText })));
  const ranks = order.map((row) => ({ warn: 0, unknown: 1, ok: 2 })[row.status]);
  assert.ok(ranks.every((rank, index) => index === 0 || ranks[index - 1] <= rank), 'Warnings do not precede unknown/normal findings');
  if (scenario.name === 'unknown-only' || scenario.name === 'empty') {
    assert.equal(await panel.locator('.status-dot.ok,.advisor-status.ok,.advisor-summary.ok').count(), 0, 'Unknown/empty state is colored as a pass');
    if (words.advisor.summaryClear) assert.equal(await panel.getByText(words.advisor.summaryClear, { exact: true }).count(), 0, 'Unknown/empty state claims everything passed');
    const tone = await overall.evaluate((element) => { const probe = document.createElement('span'); probe.style.color = 'var(--color-status-success-text)'; element.append(probe); const successColor = getComputedStyle(probe).color; probe.remove(); return { color: getComputedStyle(element).color, successColor }; });
    assert.notEqual(tone.color, tone.successColor, 'Unknown/empty overall status uses success color');
  }
  const initial = [];
  for (const item of scenario.findings) {
    const choices = rowFor(panel, words, item.rule); const row = await choices.byData.count() ? choices.byData : choices.fallback;
    assert.equal(await row.count(), 1, `Missing row ${item.rule}`);
    const details = await detailsFor(row); assert.equal(await details.count(), 1, `No native details for ${item.rule}`);
    const summary = details.locator(':scope > summary');
    const isOpen = await details.evaluate((element) => element.open);
    assert.equal(isOpen, false, `Raw evidence is expanded by default for ${item.rule}`);
    assert.equal(await row.locator('.advisor-evidence:visible,.advisor-comparison:visible').count(), 0, `Technical evidence is permanently visible for ${item.rule}`);
    const geometry = await row.evaluate((element) => ({ height: Math.round(element.getBoundingClientRect().height), text: element.innerText }));
    if (item.status === 'ok') {
      assert.ok(geometry.height <= (width < 600 ? 88 : 64), `Normal row is not compact (${geometry.height}px)`);
      assert.equal(await row.locator('.advisor-consequence:visible').count(), 0, 'Normal explanation is permanently visible');
    }
    if (item.status === 'warn') {
      const impact = row.locator('.advisor-consequence,.advisor-impact').filter({ visible: true });
      assert.ok(await impact.count() > 0, `Warning impact is not visible for ${item.rule}`);
      assert.ok((await impact.first().innerText()).trim().length > 0, 'Warning impact is empty');
      const actionWords = words === zhCN ? /缩短|减少|增加|提高|核对|排查|凭据|解除|检查|调低|降低|renew/i : /shorten|reduce|raise|increase|check|inspect|credential|clear|renew/i;
      assert.ok(actionWords.test(await impact.first().innerText()), `Warning next step is not visible for ${item.rule}`);
    }
    if (item.status === 'unknown') {
      const color = await row.locator('.advisor-state-icon').evaluate((element) => { const probe = document.createElement('span'); probe.style.color = 'var(--color-status-success-text)'; element.append(probe); const success = getComputedStyle(probe).color; probe.remove(); return { actual: getComputedStyle(element).color, success }; });
      assert.notEqual(color.actual, color.success, `Unknown rule ${item.rule} has a success-colored icon`);
    }
    const summaryHeight = await summary.evaluate((element) => element.getBoundingClientRect().height);
    assert.ok(summaryHeight >= 44, `Disclosure target is smaller than 44px for ${item.rule}`);
    initial.push({ rule: item.rule, status: item.status, ...geometry, summaryHeight });
  }
  if (scenario.name === 'all-ok') assert.ok(await panel.evaluate((element) => element.getBoundingClientRect().height <= (innerWidth < 600 ? 560 : 432)), 'All-normal diagnostics remain excessively tall');
  const snapshot = await panel.ariaSnapshot();
  return { panel, initial, order, snapshot, expectedTone };
}

async function keyboardEvidence(page, panel, scenario, words) {
  const checks = [];
  const summaries = panel.locator('.advisor-check > summary'); assert.equal(await summaries.count(), scenario.findings.length, 'Each rule needs a native keyboard disclosure');
  if (scenario.findings.length) {
    await summaries.first().focus();
    for (let index = 1; index < scenario.findings.length; index++) { await page.keyboard.press('Tab'); assert.equal(await summaries.nth(index).evaluate((element) => element === document.activeElement), true, 'Tab does not reach each diagnostic rule in visual order'); }
  }
  for (const item of scenario.findings) {
    const choices = rowFor(panel, words, item.rule); const row = await choices.byData.count() ? choices.byData : choices.fallback; const details = await detailsFor(row); const summary = details.locator(':scope > summary');
    await summary.focus(); await page.keyboard.press('Enter'); assert.equal(await details.evaluate((element) => element.open), true, `Enter failed for ${item.rule}`);
    if (item.comparison) assert.ok((await row.innerText()).includes(item.comparison), `Raw comparison disappeared for ${item.rule}`);
    const evidenceRows = row.locator('.advisor-evidence > div'); assert.equal(await evidenceRows.count(), (item.evidence ?? []).length, `An evidence term disappeared for ${item.rule}`);
    assert.ok((await evidenceRows.evaluateAll((elements) => elements.map((element) => [...element.children].map((child) => child.tagName).join(',')))).every((tags) => tags === 'DT,DD'), 'Evidence loses definition-list semantics');
    if (!item.evidence?.length) assert.equal(await row.getByText(words.advisor.noEvidence, { exact: true }).isVisible(), true, 'Missing evidence is not identified');
    for (const [index, evidence] of (item.evidence ?? []).entries()) {
      assert.equal((await evidenceRows.nth(index).locator('dd').innerText()).trim(), displayedEvidence(words, evidence), `Evidence value disappeared: ${evidence.name}`);
      if (evidence.name.includes('.')) assert.ok((await row.innerText()).includes(evidence.name), `Configuration key disappeared: ${evidence.name}`);
    }
    if (scenario.fallback?.rule === item.rule) assert.ok((await row.innerText()).includes(scenario.fallback.consequence), 'New upstream rule lost the server fallback');
    assert.equal(await summary.evaluate((element) => element === document.activeElement), true, 'Opening evidence lost focus');
    await assertNoDocumentOverflow(page);
    let bodyLayout;
    if (scenario.name === 'long-key') {
      const body = row.locator('.advisor-check-body');
      bodyLayout = await body.evaluate((element) => {
        const metric = (node) => { const style = getComputedStyle(node); const box = node.getBoundingClientRect(); return { selector: `${node.tagName.toLowerCase()}${node.className ? `.${String(node.className).split(/\s+/).join('.')}` : ''}`, scrollWidth: node.scrollWidth, clientWidth: node.clientWidth, whiteSpace: style.whiteSpace, overflowWrap: style.overflowWrap, rect: { left: Math.round(box.left), right: Math.round(box.right), width: Math.round(box.width) } }; };
        return { ...metric(element), overflowElements: [...element.querySelectorAll('*')].filter((node) => node.clientWidth > 0 && node.scrollWidth > node.clientWidth + 1).map(metric).slice(0, 8) };
      });
      assert.ok(bodyLayout.scrollWidth <= bodyLayout.clientWidth + 1, `Long technical identifiers overflow the evidence body: ${JSON.stringify(bodyLayout)}`);
    }
    await page.keyboard.press('Space'); assert.equal(await details.evaluate((element) => element.open), false, `Space failed for ${item.rule}`);
    assert.equal(await summary.evaluate((element) => element === document.activeElement), true, 'Closing evidence lost focus');
    checks.push({ rule: item.rule, enterOpens: true, spaceCloses: true, focusRetained: true, bodyLayout });
  }
  return { tabReachesAllSummaries: true, rows: checks };
}

async function main() {
  assert.ok(process.env.PLAYWRIGHT_MODULE, 'PLAYWRIGHT_MODULE must name an installed module');
  const { chromium } = await import(pathToFileURL(resolve(process.env.PLAYWRIGHT_MODULE)).href);
  const baseURL = process.env.BASE_URL ?? 'http://127.0.0.1:5275/admin/'; const outputDir = resolve(process.env.OUTPUT_DIR ?? '/private/tmp/halro-advisor-redesign-20261104'); await mkdir(outputDir, { recursive: true });
  assert.ok(['127.0.0.1', 'localhost', '[::1]'].includes(new URL(baseURL).hostname), 'This acceptance script only runs against localhost');
  const caseNames = process.env.CASE_NAMES_FILE ? new Set(JSON.parse(await readFile(process.env.CASE_NAMES_FILE, 'utf8'))) : undefined;
  const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_EXECUTABLE, args: ['--disable-background-networking', '--disable-component-update', '--disable-sync'] }); const results = [];
  try {
    for (const locale of process.env.LOCALES?.split(',') ?? ['zh-CN', 'en-US']) for (const appearance of process.env.APPEARANCES?.split(',') ?? ['dark', 'light']) for (const width of process.env.WIDTHS?.split(',').map(Number) ?? [1440, 390]) for (const scenario of scenarios) {
      if (process.env.CASES && !process.env.CASES.split(',').includes(scenario.name)) continue;
      const name = `${scenario.name}-${locale}-${appearance}-${width}`; if (caseNames && !caseNames.has(name)) continue;
      const context = await browser.newContext({ viewport: { width, height: 1000 }, locale, reducedMotion: 'reduce' }); const page = await context.newPage(); page.setDefaultTimeout(8000); const pageErrors = []; page.on('pageerror', (error) => pageErrors.push(error.message));
      const state = await installConsoleFixture(page, { baseURL, locale, appearance, respond: (_state, method, path) => method === 'GET' && path === '/advisor-findings' ? { status: 200, data: { items: structuredClone(scenario.findings) } } : undefined });
      try {
        await page.goto(new URL('settings/diagnostics', baseURL).href, { waitUntil: 'networkidle' }); await page.waitForFunction(() => !document.querySelector('.boot,.loading')); await page.evaluate(() => document.fonts.ready);
        const words = locale === 'zh-CN' ? zhCN : enUS; const inspected = await inspect(page, scenario, words, width); await assertNoDocumentOverflow(page);
        const collapsedScreenshot = join(outputDir, `${name}-default.png`); await inspected.panel.screenshot({ path: collapsedScreenshot });
        const keyboard = await keyboardEvidence(page, inspected.panel, scenario, words);
        let expandedScreenshot = null;
        if (scenario.findings.length) { for (const item of scenario.findings) { const choices = rowFor(inspected.panel, words, item.rule); const row = await choices.byData.count() ? choices.byData : choices.fallback; const details = await detailsFor(row); await details.locator(':scope > summary').click(); } await assertNoDocumentOverflow(page); expandedScreenshot = join(outputDir, `${name}-expanded.png`); await inspected.panel.screenshot({ path: expandedScreenshot }); }
        assert.deepEqual(pageErrors, []); assert.deepEqual(state.unknownRoutes, []); assert.deepEqual(state.blockedRequests, []); assert.equal(await page.locator('.crash-panel').count(), 0);
        const text = await inspected.panel.innerText(); assert.ok(!/undefined|NaN|\[object Object\]/.test(text), 'Missing evidence renders an invalid value');
        results.push({ name, passed: true, expectedTone: inspected.expectedTone, initial: inspected.initial, order: inspected.order, keyboard, ariaSnapshot: inspected.snapshot, collapsedScreenshot, expandedScreenshot }); process.stdout.write(`PASS ${name}\n`);
      } catch (error) { const screenshot = join(outputDir, `${name}-failure.png`); await page.screenshot({ path: screenshot, fullPage: true }).catch(() => {}); results.push({ name, passed: false, error: error.stack, screenshot, unknownRoutes: state.unknownRoutes, pageErrors }); process.stdout.write(`FAIL ${name}: ${error.message.split('\n')[0]}\n`); }
      finally { await context.close(); }
      await writeFile(join(outputDir, 'report.json'), JSON.stringify({ baseURL, syntheticOnly: true, note: 'Five real rule identifiers/evidence shapes follow internal/advisor/advisor.go. New-rule scenario intentionally validates forward compatibility. Keyboard/ARIA checks are not actual VoiceOver testing.', total: results.length, passed: results.filter((result) => result.passed).length, failed: results.filter((result) => !result.passed).length, results }, null, 2));
    }
  } finally { await browser.close(); }
  process.exitCode = results.some((result) => !result.passed) ? 1 : 0;
}
if (process.argv[1] && pathToFileURL(resolve(process.argv[1])).href === import.meta.url) await main();
