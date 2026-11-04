#!/usr/bin/env node
// Chromium keyboard + accessibility-tree smoke. This does not run VoiceOver.
// Uses the same explicit synthetic API router as console-acceptance.mjs.
import { mkdir, writeFile } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { installConsoleFixture } from './console-acceptance-fixtures.mjs';
import { zhCN } from '../src/i18n/locales/zh-CN.ts';
import { enUS } from '../src/i18n/locales/en-US.ts';

const baseURL = process.env.BASE_URL ?? 'http://127.0.0.1:5274/admin/';
const outputDir = resolve(process.env.OUTPUT_DIR ?? '/private/tmp/halro-console-simplification/acceptance/accessibility');
if (!process.env.PLAYWRIGHT_MODULE) throw new Error('PLAYWRIGHT_MODULE must name an installed Playwright index.mjs');
const { chromium } = await import(pathToFileURL(resolve(process.env.PLAYWRIGHT_MODULE)).href);
const browser = await chromium.launch({ headless: true, ...(process.env.CHROMIUM_EXECUTABLE ? { executablePath: process.env.CHROMIUM_EXECUTABLE } : {}), args: ['--disable-background-networking', '--disable-component-update', '--disable-sync'] });
const browserVersion = browser.version();
const results = [];
await mkdir(outputDir, { recursive: true });
const assert = (value, message) => { if (!value) throw new Error(message); };

async function exerciseModal(page, triggerName, expectedInput) {
  const trigger = page.getByRole('button', { name: triggerName, exact: true });
  await trigger.click();
  const dialog = page.getByRole('dialog');
  await dialog.waitFor();
  const nameInput = dialog.getByLabel(expectedInput).first();
  await page.waitForFunction((element) => element === document.activeElement, await nameInput.elementHandle(), { timeout: 2000 });
  const focusableCount = await dialog.locator('input:visible:not([disabled]), select:visible:not([disabled]), textarea:visible:not([disabled]), button:visible:not([disabled]), summary:visible').count();
  for (let index = 0; index < focusableCount + 2; index++) {
    await page.keyboard.press('Tab');
    assert(await dialog.evaluate((element) => element.contains(document.activeElement)), `Tab escaped the dialog at step ${index}`);
  }
  for (let index = 0; index < focusableCount + 2; index++) {
    await page.keyboard.press('Shift+Tab');
    assert(await dialog.evaluate((element) => element.contains(document.activeElement)), `Shift+Tab escaped the dialog at step ${index}`);
  }
  const disclosure = dialog.locator('.disclosure-section > summary').first();
  if (await disclosure.count()) {
    await disclosure.focus();
    await disclosure.press('Enter');
    const bodyID = await disclosure.getAttribute('aria-controls');
    await page.locator(`[id="${bodyID}"]`).waitFor({ state: 'visible' });
    await page.waitForFunction((element) => element.getAttribute('aria-expanded') === 'true', await disclosure.elementHandle(), { timeout: 2000 });
  }
  assert(await dialog.getByLabel(expectedInput).count() > 0, 'The essential name field lost its associated label');
  const snapshot = await dialog.ariaSnapshot();
  await page.keyboard.press('Escape');
  await dialog.waitFor({ state: 'hidden' });
  assert(await trigger.evaluate((element) => element === document.activeElement), 'Closing the dialog did not restore focus to its trigger');
  return snapshot;
}

const cases = [
  { name: 'provider', path: '/admin/providers', run: (page, t) => exerciseModal(page, t.providers.addProvider, new RegExp(t.providers.providerName)) },
  { name: 'project', path: '/admin/projects', run: (page, t) => exerciseModal(page, t.projects.create, new RegExp(`^${t.projects.name}`)) },
  { name: 'developer', path: '/admin/developer', run: async (page, t) => {
    const formTab = page.getByRole('tab', { name: t.developer.formMode, exact: true });
    await formTab.focus();
    await page.keyboard.press('ArrowRight');
    const jsonTab = page.getByRole('tab', { name: t.developer.jsonMode, exact: true });
    assert(await jsonTab.getAttribute('aria-selected') === 'true', 'Arrow key did not select the JSON request tab');
    assert(await jsonTab.evaluate((element) => element === document.activeElement), 'Arrow-key tab navigation lost focus');
    await page.keyboard.press('ArrowLeft');
    const imageDisclosure = page.locator('.developer-image-field > summary');
    await imageDisclosure.focus();
    await imageDisclosure.press('Enter');
    await page.locator('#developer-image-url').waitFor({ state: 'visible' });
    await page.waitForFunction((element) => element.getAttribute('aria-expanded') === 'true', await imageDisclosure.elementHandle(), { timeout: 2000 });
    return page.locator('#main-content').ariaSnapshot();
  } },
  { name: 'developer-overlays', path: '/admin/developer', run: async (page, t) => {
    const prompt = page.getByRole('textbox', { name: t.developer.input, exact: true });
    await prompt.fill('Synthetic keyboard state must survive opening and closing secondary surfaces.');
    await page.locator('#developer-gateway-key').fill('halro_synthetic_acceptance_not_a_real_key');
    const snapshots = [];
    for (const title of [t.developer.integrationCode, t.developer.connectionSettings]) {
      const trigger = page.getByRole('button', { name: title, exact: true });
      await trigger.focus();
      await page.keyboard.press('Enter');
      const dialog = page.getByRole('dialog', { name: title, exact: true });
      await dialog.waitFor();
      await page.waitForFunction((element) => element.contains(document.activeElement), await dialog.elementHandle());
      if (title === t.developer.integrationCode) {
        assert(!(await dialog.locator('.developer-code').innerText()).includes('halro_synthetic_acceptance_not_a_real_key'), 'The integration example exposes the entered Gateway Key');
        const activeTab = dialog.getByRole('tab', { selected: true });
        await activeTab.focus();
        await page.keyboard.press('ArrowRight');
        await page.waitForFunction((element) => element.querySelector('[role="tab"][aria-selected="true"]') === document.activeElement, await dialog.elementHandle(), { timeout: 2000 });
      }
      const count = await dialog.locator('input:visible:not([disabled]),select:visible:not([disabled]),button:visible:not([disabled]),summary:visible').count();
      for (let index = 0; index < count + 3; index++) {
        await page.keyboard.press('Tab');
        assert(await dialog.evaluate((element) => element.contains(document.activeElement)), `Tab escaped ${title} at step ${index}`);
      }
      for (let index = 0; index < count + 3; index++) {
        await page.keyboard.press('Shift+Tab');
        assert(await dialog.evaluate((element) => element.contains(document.activeElement)), `Shift+Tab escaped ${title} at step ${index}`);
      }
      snapshots.push(await dialog.ariaSnapshot());
      await page.keyboard.press('Escape');
      await dialog.waitFor({ state: 'hidden' });
      assert(await trigger.evaluate((element) => element === document.activeElement), `Closing ${title} lost the trigger focus`);
      assert(await prompt.inputValue() === 'Synthetic keyboard state must survive opening and closing secondary surfaces.', 'Secondary surface discarded the request input');
      assert(await page.locator('#developer-gateway-key').inputValue() === 'halro_synthetic_acceptance_not_a_real_key', 'Secondary surface discarded the in-memory Gateway Key');
    }
    return snapshots.join('\n\n');
  } },
  { name: 'developer-invalid-code', path: '/admin/developer', run: async (page, t) => {
    await page.getByRole('tab', { name: t.developer.jsonMode, exact: true }).click();
    await page.locator('.developer-json-editor').fill('{');
    const trigger = page.getByRole('button', { name: t.developer.integrationCode, exact: true });
    await trigger.click();
    const dialog = page.getByRole('dialog', { name: t.developer.integrationCode, exact: true });
    await dialog.waitFor();
    assert(await dialog.getByRole('button', { name: t.developer.copyCode, exact: true }).isDisabled(), 'Invalid JSON code can still be copied');
    await dialog.getByRole('tab', { selected: true }).focus();
    await page.keyboard.press('ArrowRight');
    await page.waitForFunction((element) => element.querySelector('[role="tab"][aria-selected="true"]') === document.activeElement, await dialog.elementHandle(), { timeout: 2000 });
    for (let index = 0; index < 10; index++) {
      await page.keyboard.press('Tab');
      assert(await dialog.evaluate((element) => element.contains(document.activeElement)), `Tab escaped invalid-JSON code drawer at step ${index}`);
    }
    const snapshot = await dialog.ariaSnapshot();
    await page.keyboard.press('Escape');
    await dialog.waitFor({ state: 'hidden' });
    assert(await trigger.evaluate((element) => element === document.activeElement), 'Invalid code drawer lost return focus');
    assert(await page.locator('.developer-json-editor').inputValue() === '{', 'Code drawer discarded invalid JSON while closing');
    return snapshot;
  } },
  { name: 'login', path: '/admin/login', options: { auth: 'login' }, run: async (page) => {
    const username = page.locator('input[autocomplete="username"]');
    await username.focus();
    await page.keyboard.press('Tab');
    assert(await page.locator('input[type="password"]').evaluate((element) => element === document.activeElement), 'Login keyboard order did not reach password after username');
    assert(await username.evaluate((element) => Boolean(element.labels?.length)), 'Login username has no associated label');
    return page.locator('body').ariaSnapshot();
  } },
  { name: 'readonly-routes', path: '/admin/routes', options: { role: 'read_only' }, run: async (page, t) => {
    const snapshot = await page.locator('#main-content').ariaSnapshot();
    assert(snapshot.includes(t.navigation.readOnlyNotice), 'Read-only context is missing from the accessibility tree');
    assert(await page.getByRole('button', { name: t.routes.create, exact: true }).isDisabled(), 'Read-only route creation is available');
    return snapshot;
  } },
];
const selectedCases = process.env.CASES ? cases.filter((test) => process.env.CASES.split(',').includes(test.name)) : cases;

try {
  for (const locale of ['zh-CN', 'en-US']) for (const appearance of ['dark', 'light']) for (const accessibleMedia of [false, true]) {
    const context = await browser.newContext({ locale, timezoneId: 'Asia/Singapore', viewport: { width: 820, height: 900 }, forcedColors: accessibleMedia ? 'active' : 'none', reducedMotion: accessibleMedia ? 'reduce' : 'no-preference' });
    for (const test of selectedCases) {
      const page = await context.newPage();
      const pageErrors = [];
      page.on('pageerror', (error) => pageErrors.push(error.message));
      const state = await installConsoleFixture(page, { baseURL, locale, appearance, ...test.options });
      const name = `${test.name}-${locale}-${appearance}-${accessibleMedia ? 'forced-colors-reduced-motion' : 'normal'}`;
      try {
        await page.goto(new URL(test.path, baseURL).href, { waitUntil: 'networkidle' });
        await page.waitForFunction(() => !document.querySelector('.loading,.boot'));
        const snapshot = await test.run(page, locale === 'zh-CN' ? zhCN : enUS);
        const media = await page.evaluate(() => ({ forcedColors: matchMedia('(forced-colors: active)').matches, reducedMotion: matchMedia('(prefers-reduced-motion: reduce)').matches, viewportWidth: innerWidth, documentWidth: document.documentElement.scrollWidth, crash: Boolean(document.querySelector('.crash-panel')) }));
        assert(media.forcedColors === accessibleMedia && media.reducedMotion === accessibleMedia, 'Requested accessibility media was not active');
        assert(media.documentWidth <= media.viewportWidth + 1, `Horizontal overflow ${media.documentWidth}/${media.viewportWidth}`);
        assert(!media.crash && !pageErrors.length, `Page crashed: ${pageErrors.join('; ')}`);
        assert(!state.unknownRoutes.length && !state.blockedRequests.length, `Unexpected network: ${JSON.stringify([...state.unknownRoutes, ...state.blockedRequests])}`);
        const snapshotPath = join(outputDir, `${name}.aria.yml`);
        await writeFile(snapshotPath, snapshot);
        if (accessibleMedia) await page.screenshot({ path: join(outputDir, `${name}.png`), fullPage: true });
        results.push({ name, passed: true, media, snapshot: snapshotPath });
        process.stdout.write(`PASS ${name}\n`);
      } catch (error) {
        const screenshot = join(outputDir, `${name}-failure.png`);
        await page.screenshot({ path: screenshot, fullPage: true }).catch(() => {});
        results.push({ name, passed: false, error: error.message, pageErrors, unknownRoutes: state.unknownRoutes, screenshot });
        process.stdout.write(`FAIL ${name}: ${error.message.split('\n')[0]}\n`);
      } finally { await page.close(); }
    }
    await context.close();
  }
} finally {
  await browser.close();
  const report = { generatedAt: new Date().toISOString(), browserVersion, baseURL, note: 'Chromium accessibility tree and real keyboard focus checks. This is not a VoiceOver or other screen-reader session.', total: results.length, passed: results.filter((result) => result.passed).length, failed: results.filter((result) => !result.passed).length, results };
  await writeFile(join(outputDir, 'report.json'), JSON.stringify(report, null, 2));
  process.stdout.write(`Report: ${join(outputDir, 'report.json')} (${report.passed}/${report.total} passed)\n`);
  if (report.failed) process.exitCode = 1;
}
