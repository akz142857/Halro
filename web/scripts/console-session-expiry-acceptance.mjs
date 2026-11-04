#!/usr/bin/env node
// Local synthetic Admin responses only. Run against Vite or the built bundle.
// PLAYWRIGHT_MODULE=/path/to/playwright/index.mjs BASE_URL=http://127.0.0.1:5274/admin/ node web/scripts/console-session-expiry-acceptance.mjs
import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { installConsoleFixture } from './console-acceptance-fixtures.mjs';
import { zhCN } from '../src/i18n/locales/zh-CN.ts';
import { enUS } from '../src/i18n/locales/en-US.ts';

if (!process.env.PLAYWRIGHT_MODULE) throw new Error('PLAYWRIGHT_MODULE is required');
const { chromium } = await import(pathToFileURL(resolve(process.env.PLAYWRIGHT_MODULE)).href);
const baseURL = process.env.BASE_URL ?? 'http://127.0.0.1:5274/admin/';
const outputDir = resolve(process.env.OUTPUT_DIR ?? '/private/tmp/halro-session-expiry-acceptance');
await mkdir(outputDir, { recursive: true });
const browser = await chromium.launch({ headless: true, ...(process.env.CHROMIUM_EXECUTABLE ? { executablePath: process.env.CHROMIUM_EXECUTABLE } : {}), args: ['--disable-background-networking', '--disable-component-update', '--disable-sync'] });
const results = [];
try {
  for (const locale of ['zh-CN', 'en-US']) for (const width of [1440, 390, 320]) for (const target of ['usage?tab=failures', 'policies', 'run-governance?project_id=project_acceptance', 'operations', 'settings/security']) {
    const name = `${locale}-${width}-${target.replaceAll(/[/?=]/g, '-')}`;
    const t = locale === 'zh-CN' ? zhCN : enUS;
    const context = await browser.newContext({ viewport: { width, height: 900 }, locale });
    const page = await context.newPage();
    const faults = [];
    page.on('pageerror', (error) => faults.push(error.message));
    let expired = false;
    let loginAttempts = 0;
    const mfa = target === 'settings/security';
    const state = await installConsoleFixture(page, { baseURL, locale, appearance: 'light', respond: (state, method, path, _query, body) => {
      if (path === '/session/login') {
        loginAttempts += 1;
        if (body.password === 'wrong-synthetic-password') return { status: 401, data: { error: 'invalid credentials' } };
        if (mfa) return { status: 200, data: { mfa_required: true, challenge_token: 'synthetic-challenge', expires_at: '2027-01-01T00:00:00Z' } };
        expired = false;
        return { status: 200, data: state.session };
      }
      if (path === '/session/mfa/totp') { expired = false; return { status: 200, data: state.session }; }
      if (expired && !['/setup/status', '/ui/bootstrap'].includes(path)) return { status: 401, data: { error: 'admin authentication required' } };
    } });
    try {
      await page.goto(new URL('/admin/cluster', baseURL).href, { waitUntil: 'networkidle' });
      await page.locator('.main').waitFor();
      expired = true;
      // Exercise the app's navigation, retaining the confirmed session cache.
      await page.evaluate((path) => { history.pushState({}, '', `/admin/${path}`); window.dispatchEvent(new Event('halro:navigate')); }, target);
      await page.getByRole('heading', { name: t.auth.sessionExpiredHeading, exact: true }).waitFor();
      assert.equal(await page.locator('.main,.empty-state,.notice.error').count(), 0);
      assert.equal(await page.getByRole('main').count(), 1);
      assert.equal(await page.locator('input[autocomplete="username"]').evaluate((input) => input === document.activeElement), true);
      assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth), true);
      await page.screenshot({ path: join(outputDir, `${name}.png`), fullPage: true });
      const destination = page.url();
      await page.locator('input[autocomplete="username"]').fill('acceptance-admin');
      await page.locator('input[type="password"]').fill('wrong-synthetic-password');
      await page.getByRole('button', { name: t.auth.signInAgain, exact: true }).click();
      await page.getByText(t.errors.invalidCredentials, { exact: true }).waitFor();
      assert.equal(await page.getByRole('alert').count(), 1);
      await page.locator('input[type="password"]').fill('correct-synthetic-password');
      await page.getByRole('button', { name: t.auth.signInAgain, exact: true }).click();
      if (mfa) {
        await page.getByRole('heading', { name: t.auth.mfaHeading, exact: true }).waitFor();
        await page.locator('input[autocomplete="one-time-code"]').fill('123456');
        await page.getByRole('button', { name: t.auth.verify, exact: true }).click();
      }
      await page.locator('.main').waitFor();
      await page.waitForLoadState('networkidle');
      assert.equal(page.url(), destination);
      assert.equal(await page.locator('.session-recovery').count(), 0);
      assert.equal(loginAttempts, 2);
      assert.deepEqual(faults, []);
      assert.deepEqual(state.unknownRoutes, []);
      assert.deepEqual(state.blockedRequests, []);
      results.push({ name, passed: true, mfa });
    } catch (error) { results.push({ name, passed: false, error: error.message }); }
    finally { await context.close(); }
    process.stdout.write(`${results.at(-1).passed ? 'PASS' : 'FAIL'} ${name}${results.at(-1).error ? ': ' + results.at(-1).error : ''}\n`);
  }
} finally {
  await browser.close();
  await writeFile(join(outputDir, 'report.json'), JSON.stringify({ syntheticOnly: true, results }, null, 2));
  if (results.some((result) => !result.passed)) process.exitCode = 1;
}
