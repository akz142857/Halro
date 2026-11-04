#!/usr/bin/env node
// Project-scoped exports, tested in an isolated browser with real download bytes.
// Clipboard writes are captured inside this page; no OS/user clipboard is read.
// All Admin API calls are synthetic; undeclared APIs and external origins fail.
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdir, readFile, writeFile } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { installConsoleFixture } from './console-acceptance-fixtures.mjs';
import { zhCN } from '../src/i18n/locales/zh-CN.ts';
import { enUS } from '../src/i18n/locales/en-US.ts';

const scenarios = ['scope-switch', 'invalid-url', 'no-address', 'no-models', 'project-disabled', 'workbench-disabled', 'read-only', 'long-content'];
const gatewayURL = 'https://gateway.example.test/proxy';
const paths = { responses: '/v1/responses', chat: '/v1/chat/completions', embeddings: '/v1/embeddings' };
const hash = (text) => createHash('sha256').update(text).digest('hex');
const button = (scope, name) => scope.getByRole('button', { name, exact: true });
const parsedFences = (text) => {
  const lines = text.split('\n'); const blocks = [];
  for (let index = 0; index < lines.length; index++) {
    const opening = /^(`{3,})([a-z]+)$/.exec(lines[index]); if (!opening) continue;
    const content = []; let end = index + 1; for (; end < lines.length && lines[end] !== opening[1]; end++) content.push(lines[end]);
    assert.ok(end < lines.length, 'Unclosed Markdown fence'); blocks.push({ language: opening[2], content: content.join('\n') }); index = end;
  }
  return blocks;
};
function parseSkill(text, copied) {
  const match = /^---\n([\s\S]*?)\n---\n\n([\s\S]*)$/.exec(text); assert.ok(match, 'Download is not a frontmatter-bearing SKILL.md');
  const name = /^name: ([a-z0-9-]+)$/m.exec(match[1])?.[1]; assert.ok(name && name.length <= 64, 'Invalid skill name');
  const description = /^description: (.+)$/m.exec(match[1])?.[1]; assert.ok(description && typeof JSON.parse(description) === 'string' && JSON.parse(description).length > 0, 'Missing skill description');
  assert.equal(match[2], copied, 'Download body differs from Copy instructions'); assert.ok(copied.startsWith('# '), 'Instructions have no Markdown heading');
  return { name, description: JSON.parse(description), bodyBytes: Buffer.byteLength(match[2]) };
}
function assertContent(text, project, model, endpoint, base, forbidden) {
  const blocks = parsedFences(text); const snapshot = JSON.parse(blocks.find((block) => block.language === 'json')?.content ?? 'null'); assert.ok(snapshot, 'Project snapshot is missing');
  const normalized = base.trim().replace(/\/+$/, ''); const apiRoot = normalized.endsWith('/v1') ? normalized : normalized + '/v1'; const expectedURL = apiRoot + paths[endpoint].slice(3);
  assert.deepEqual(snapshot.project, { id: project.id, name: project.name, revision: project.revision }); assert.deepEqual(snapshot.authorized_public_aliases, project.allowed_models);
  assert.equal(snapshot.model, model); assert.equal(snapshot.endpoint, endpoint); assert.equal(snapshot.gateway_base_url, normalized); assert.equal(snapshot.api_base_url, apiRoot); assert.equal(snapshot.request_url, expectedURL); assert.equal(snapshot.method, 'POST');
  assert.equal(snapshot.response_data, { responses: 'output[].content[type=output_text].text', chat: 'choices[].message.content', embeddings: 'data[].embedding' }[endpoint]); assert.equal(snapshot.request_id_header, 'X-Request-ID');
  assert.deepEqual(snapshot.authentication, { environment_variable: 'HALRO_API_KEY', required_scope: 'inference', project_id_is_not_a_credential: true });
  assert.deepEqual(snapshot.limits, { rpm: project.rpm, tpm: project.tpm, max_concurrency: project.max_concurrency, daily_budget_micros_usd: project.daily_budget_micros_usd });
  const sample = blocks.find((block) => block.language === 'bash')?.content ?? ''; assert.ok(sample.includes(expectedURL)); assert.ok(sample.includes('Authorization: Bearer $HALRO_API_KEY')); assert.ok(!sample.includes('/v1/v1/'));
  for (const value of forbidden) if (value) assert.ok(!text.includes(value), `Unrelated project/credential/secret data leaked: ${value}`);
  assert.ok(!/undefined|NaN|\[object Object\]/.test(text)); return snapshot;
}
async function noOverflow(page) { assert.ok(await page.evaluate(() => document.documentElement.scrollWidth <= document.documentElement.clientWidth + 1), 'Document horizontally overflows'); }
async function openEntry(page, words) {
  const trigger = button(page, words.aiIntegration.title); await trigger.click(); const dialog = page.getByRole('dialog', { name: words.aiIntegration.title, exact: true }); await dialog.waitFor();
  await page.waitForFunction((element) => element.contains(document.activeElement), await dialog.elementHandle()); return { dialog, trigger };
}
async function waitExport(dialog, words) { const copy = button(dialog, words.aiIntegration.copy); await dialog.page().waitForFunction((element) => !element.disabled, await copy.elementHandle()); }
async function exportRoundTrip({ page, dialog, words, project, model, endpoint, base, forbidden, outputDir, name }) {
  await waitExport(dialog, words);
  const details = dialog.locator('.disclosure-section'); if (!(await details.evaluate((element) => element.open))) await details.locator(':scope > summary').click();
  const instructions = await dialog.locator('.project-ai-instructions').textContent(); const snapshot = assertContent(instructions, project, model, endpoint, base, forbidden);
  const count = await page.evaluate(() => window.__aiClipboard.length); await button(dialog, words.aiIntegration.copy).click(); await dialog.getByText(words.aiIntegration.copied, { exact: true }).waitFor();
  const copied = await page.evaluate(() => window.__aiClipboard.at(-1)); assert.equal(await page.evaluate(() => window.__aiClipboard.length), count + 1); assert.equal(copied, instructions, 'Copy differs from the visible preview');
  const downloading = page.waitForEvent('download'); await button(dialog, words.aiIntegration.download).click(); const download = await downloading; assert.equal(download.suggestedFilename(), 'SKILL.md'); assert.equal(await download.failure(), null);
  const downloadedPath = join(outputDir, `${name}-${project.id}-${endpoint}-SKILL.md`); await download.saveAs(downloadedPath); const skill = await readFile(downloadedPath, 'utf8'); const frontmatter = parseSkill(skill, copied);
  assertContent(skill, project, model, endpoint, base, forbidden); await dialog.getByText(words.aiIntegration.downloadStarted, { exact: true }).waitFor(); await noOverflow(page);
  return { project: project.id, model, endpoint, snapshot, frontmatter, instructionsSHA256: hash(instructions), skillSHA256: hash(skill), downloadedPath, samePreviewCopyAndSkillBody: true };
}
async function blocked(dialog, words, reason) {
  assert.equal(await button(dialog, words.aiIntegration.copy).isDisabled(), true); assert.equal(await button(dialog, words.aiIntegration.download).isDisabled(), true); assert.equal(await dialog.locator('.project-ai-instructions').count(), 0);
  if (reason) assert.equal(await dialog.getByText(reason, { exact: true }).isVisible(), true);
}

assert.ok(process.env.PLAYWRIGHT_MODULE, 'PLAYWRIGHT_MODULE is required');
const { chromium } = await import(pathToFileURL(resolve(process.env.PLAYWRIGHT_MODULE)).href);
const baseURL = process.env.BASE_URL ?? 'http://127.0.0.1:5275/admin/'; assert.ok(['localhost', '127.0.0.1', '[::1]'].includes(new URL(baseURL).hostname));
const outputDir = resolve(process.env.OUTPUT_DIR ?? '/private/tmp/halro-project-ai-access-20261104/ai'); await mkdir(outputDir, { recursive: true });
const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_EXECUTABLE, args: ['--disable-background-networking', '--disable-component-update', '--disable-sync'] }); const results = [];
try {
  for (const locale of process.env.LOCALES?.split(',') ?? ['zh-CN', 'en-US']) for (const appearance of process.env.APPEARANCES?.split(',') ?? ['dark', 'light']) for (const width of process.env.WIDTHS?.split(',').map(Number) ?? [1440, 390]) for (const scenario of scenarios) {
    if (process.env.CASES && !process.env.CASES.split(',').includes(scenario)) continue;
    const name = `${scenario}-${locale}-${appearance}-${width}`; const context = await browser.newContext({ locale, reducedMotion: 'reduce', acceptDownloads: true, viewport: { width, height: 1000 } }); const page = await context.newPage(); page.setDefaultTimeout(8000);
    await page.addInitScript(() => { window.__aiClipboard = []; Object.defineProperty(navigator, 'clipboard', { configurable: true, value: { writeText: async (text) => { window.__aiClipboard.push(String(text)); } } }); });
    const errors = []; page.on('pageerror', (error) => errors.push(error.message)); const words = locale === 'zh-CN' ? zhCN : enUS;
    const state = await installConsoleFixture(page, { baseURL, locale, appearance, role: scenario === 'read-only' ? 'read_only' : 'administrator', respond: (_state, method, path) => method === 'GET' && path === '/developer/config' ? { status: 200, data: { gateway_base_url: scenario === 'no-address' ? '' : gatewayURL, enabled: scenario !== 'workbench-disabled', max_request_bytes: 1048576 } } : undefined });
    const primary = { ...state.projects[0], name: 'Synthetic Alpha', allowed_models: ['alpha-chat', 'alpha-embedding'] }; const secondary = { ...primary, id: 'project_beta', name: 'Synthetic Beta', revision: 7, allowed_models: ['beta-private-alias'] };
    if (scenario === 'project-disabled') primary.enabled = false;
    if (scenario === 'no-models') primary.allowed_models = [];
    if (scenario === 'long-content') { primary.name = 'Synthetic long project name '.repeat(8); primary.allowed_models = ['synthetic-long-alias-'.repeat(10)]; }
    state.projects = [primary, secondary]; const exported = []; const forbidden = [secondary.id, secondary.name, ...secondary.allowed_models, 'halro_synthetic_acceptance_not_a_real_key', state.session.csrf_token, state.credentials[0].id];
    try {
      await page.goto(new URL('projects', baseURL).href, { waitUntil: 'networkidle' }); await page.locator('.detail-title').waitFor();
      if (scenario === 'read-only') assert.equal(await button(page, words.projects.createKey).first().isDisabled(), true);
      const opened = await openEntry(page, words); const { dialog, trigger } = opened;
      assert.equal(await dialog.locator('input[type="password"]').count(), 0, 'AI exports must not request a plaintext secret');
      const address = dialog.locator('input[type="url"]'); const model = dialog.locator('select').first(); const endpoint = dialog.locator('select').nth(1);
      await dialog.getByText(words.aiIntegration.loadingAddress, { exact: true }).waitFor({ state: 'hidden' });
      assert.deepEqual(await model.locator('option').evaluateAll((options) => options.map((option) => option.value)), primary.allowed_models.length ? primary.allowed_models : ['']);
      const defaultScreenshot = join(outputDir, `${name}-default.png`); await dialog.screenshot({ path: defaultScreenshot });
      if (scenario === 'project-disabled' || scenario === 'no-models') await blocked(dialog, words, scenario === 'project-disabled' ? words.aiIntegration.projectDisabled : words.aiIntegration.noModels);
      else {
        let activeBase = gatewayURL;
        if (scenario === 'no-address') { assert.equal(await address.inputValue(), ''); await blocked(dialog, words); assert.ok(!(await dialog.innerText()).includes(new URL(baseURL).origin), 'Admin origin was guessed as Gateway URL'); activeBase = 'https://gateway.example.test/manual/v1/'; await address.fill(activeBase); }
        if (scenario === 'invalid-url') {
          for (const invalid of ['javascript:alert(1)', 'https://user:password@gateway.example.test', 'https://gateway.example.test?token=synthetic', 'https://gateway.example.test#fragment', 'not-a-url', '']) { await address.fill(invalid); await blocked(dialog, words); }
          activeBase = 'https://gateway.example.test/proxy/v1/'; await address.fill(activeBase);
        }
        await waitExport(dialog, words); assert.equal(await dialog.locator('.disclosure-section').evaluate((element) => element.open), false, 'Long instructions are expanded by default');
        const endpoints = scenario === 'scope-switch' ? ['responses', 'chat', 'embeddings'] : ['responses'];
        for (const selected of endpoints) { await endpoint.selectOption(selected); exported.push(await exportRoundTrip({ page, dialog, words, project: primary, model: primary.allowed_models[0], endpoint: selected, base: activeBase, forbidden, outputDir, name })); }
      }
      const screenshot = join(outputDir, `${name}.png`); await page.screenshot({ path: screenshot, fullPage: true }); await noOverflow(page); const ariaSnapshot = await dialog.ariaSnapshot();
      await page.keyboard.press('Escape'); await dialog.waitFor({ state: 'hidden' }); assert.equal(await trigger.evaluate((element) => element === document.activeElement), true, 'Closing AI entry lost focus');
      if (scenario === 'scope-switch') {
        await page.locator('.resource-list button').filter({ hasText: secondary.id }).click(); await page.locator('.detail-title code').filter({ hasText: secondary.id }).waitFor(); const second = await openEntry(page, words);
        exported.push(await exportRoundTrip({ page, dialog: second.dialog, words, project: secondary, model: secondary.allowed_models[0], endpoint: 'responses', base: gatewayURL, forbidden: [primary.id, primary.name, ...primary.allowed_models, 'halro_synthetic_acceptance_not_a_real_key', state.session.csrf_token], outputDir, name })); await page.keyboard.press('Escape');
      }
      assert.deepEqual(errors, []); assert.deepEqual(state.unknownRoutes, []); assert.deepEqual(state.blockedRequests, []); assert.deepEqual(state.writes, []); assert.ok(state.requests.every((request) => request.method === 'GET'), 'Export issued a mutation or execution request'); assert.equal(await page.locator('.crash-panel').count(), 0);
      results.push({ name, passed: true, exported, defaultScreenshot, screenshot, ariaSnapshot, noMutationsOrExecutionRequests: true }); process.stdout.write(`PASS ${name}\n`);
    } catch (error) { const screenshot = join(outputDir, `${name}-failure.png`); await page.screenshot({ path: screenshot, fullPage: true }).catch(() => {}); results.push({ name, passed: false, error: error.stack, screenshot, unknownRoutes: state.unknownRoutes, blockedRequests: state.blockedRequests, pageErrors: errors }); process.stdout.write(`FAIL ${name}: ${error.message.split('\n')[0]}\n`); }
    finally { await context.close(); }
    await writeFile(join(outputDir, 'report.json'), JSON.stringify({ baseURL, syntheticOnly: true, note: 'Clipboard writes are captured in the isolated page; actual SKILL.md download bytes are parsed. No real provider/Chrome/Docker access; all undeclared APIs and external origins rejected.', total: results.length, passed: results.filter((result) => result.passed).length, failed: results.filter((result) => !result.passed).length, results }, null, 2));
  }
} finally { await browser.close(); }
process.exitCode = results.some((result) => !result.passed) ? 1 : 0;
