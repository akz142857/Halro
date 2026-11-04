#!/usr/bin/env node
// Re-authentication and feedback-spacing acceptance. Every API response is
// synthetic; the shared router blocks external origins and undeclared APIs.
// PLAYWRIGHT_MODULE=/absolute/playwright/index.mjs CHROMIUM_EXECUTABLE=/absolute/chromium \
// BASE_URL=http://127.0.0.1:5274/admin/ node web/scripts/console-reauth-acceptance.mjs
import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { installConsoleFixture, fixtureResponse } from './console-acceptance-fixtures.mjs';
import { zhCN } from '../src/i18n/locales/zh-CN.ts';
import { enUS } from '../src/i18n/locales/en-US.ts';

if (!process.env.PLAYWRIGHT_MODULE) throw new Error('PLAYWRIGHT_MODULE must name an installed Playwright index.mjs');
const { chromium } = await import(pathToFileURL(resolve(process.env.PLAYWRIGHT_MODULE)).href);
const baseURL = process.env.BASE_URL ?? 'http://127.0.0.1:5274/admin/';
const outputDir = resolve(process.env.OUTPUT_DIR ?? '/private/tmp/halro-reauth-acceptance-20261104');
const browser = await chromium.launch({ headless: true, ...(process.env.CHROMIUM_EXECUTABLE ? { executablePath: process.env.CHROMIUM_EXECUTABLE } : {}), args: ['--disable-background-networking', '--disable-component-update', '--disable-sync'] });
const results = [];
await mkdir(outputDir, { recursive: true });
const words = (locale) => locale === 'zh-CN' ? zhCN : enUS;
const response = (data, status = 200) => ({ data, status });
const rejectPassword = () => response({ code: 'recent_reauth_required', error: 'invalid current password' }, 401);
const getDialog = (page) => page.getByRole('dialog').last();
const passwordField = (scope) => scope.locator('input[autocomplete="current-password"],input[type="password"]').first();
const codeField = (scope) => scope.locator('input[autocomplete="one-time-code"]');
const errors = (scope) => scope.locator('.notice.error:visible');
const button = (scope, name) => scope.getByRole('button', { name, exact: true });

async function settleUI(page) {
  await page.waitForFunction(() => !document.querySelector('.boot,.loading'));
  await page.evaluate(async () => {
    await Promise.all(document.getAnimations().filter((animation) => animation.effect?.getTiming().iterations !== Infinity).map((animation) => animation.finished.catch(() => {})));
  });
}
async function measureNotice(notice) {
  return notice.evaluate((element) => {
    const box = element.getBoundingClientRect();
    const parent = element.parentElement;
    const style = getComputedStyle(element);
    const parentStyle = getComputedStyle(parent);
    const shown = (node) => node && node.getClientRects().length > 0 && getComputedStyle(node).position !== 'absolute';
    let previous = element.previousElementSibling;
    while (previous && !shown(previous)) previous = previous.previousElementSibling;
    let next = element.nextElementSibling;
    while (next && !shown(next)) next = next.nextElementSibling;
    return { text: element.textContent.trim(), marginTop: parseFloat(style.marginTop), marginBottom: parseFloat(style.marginBottom), previousClass: previous?.className ?? null, previousMarginBottom: previous ? parseFloat(getComputedStyle(previous).marginBottom) : null, gapAbove: previous ? Math.round((box.top - previous.getBoundingClientRect().bottom) * 100) / 100 : null, gapBelow: next ? Math.round((next.getBoundingClientRect().top - box.bottom) * 100) / 100 : null, parentClass: parent.className, parentDisplay: parentStyle.display, parentRowGap: parseFloat(parentStyle.rowGap) || 0, width: Math.round(box.width), height: Math.round(box.height) };
  });
}
async function check(name, locale, appearance, options, run) {
  if (process.env.CASES && !process.env.CASES.split(',').some((filter) => name.includes(filter))) return;
  const fullName = `${name}-${locale}-${appearance}`;
  const context = await browser.newContext({ viewport: { width: 1440, height: 1000 }, locale, timezoneId: 'Asia/Singapore', reducedMotion: 'reduce' });
  const page = await context.newPage();
  page.setDefaultTimeout(8000);
  const pageErrors = [];
  page.on('pageerror', (error) => pageErrors.push(error.message));
  const state = await installConsoleFixture(page, { baseURL, locale, appearance, ...options });
  try {
    const details = await run({ page, state, t: words(locale), fullName });
    assert.deepEqual(state.unknownRoutes, [], 'Undeclared API request');
    assert.deepEqual(state.blockedRequests, [], 'Unexpected external request');
    assert.deepEqual(pageErrors, [], 'Page error');
    assert.equal(await page.locator('.crash-panel').count(), 0, 'ErrorBoundary rendered');
    results.push({ name: fullName, passed: true, ...details });
    process.stdout.write(`PASS ${fullName}\n`);
  } catch (error) {
    const screenshot = join(outputDir, `${fullName}-failure.png`);
    await page.screenshot({ path: screenshot, fullPage: true }).catch(() => {});
    results.push({ name: fullName, passed: false, error: error.stack, screenshot, unknownRoutes: state.unknownRoutes, pageErrors });
    process.stdout.write(`FAIL ${fullName}: ${error.message.split('\n')[0]}\n`);
  } finally { await context.close(); }
  await writeFile(join(outputDir, 'report.json'), JSON.stringify({ baseURL, total: results.length, passed: results.filter((item) => item.passed).length, failed: results.filter((item) => !item.passed).length, results }, null, 2));
}

function securityResponse(enabled) { return { enabled, policy: 'optional', required: false, authenticators: enabled ? [{ id: 'authenticator_synthetic', name: 'Synthetic authenticator', type: 'totp', created_at: '2026-11-01T00:00:00Z', revision: 1 }] : [] }; }
function stepUpResponder(enabled) {
  return (state, method, path, query, body) => {
    if (method === 'GET' && path === '/security/mfa') return response(securityResponse(enabled));
    if (method === 'POST' && /\/projects\/[^/]+\/keys$/.test(path)) {
      (state.authAttempts ??= []).push({ hasPassword: Boolean(body?.current_password), hasCode: Boolean(body?.totp_code) });
      if (!body?.current_password || body.current_password === 'wrong-synthetic-password') return rejectPassword();
      if (enabled && body.totp_code !== '12345678') return rejectPassword();
      return fixtureResponse(state, method, path, query, body);
    }
    if (method === 'POST' && /\/deployments\/[^/]+\/prices(?:\/restore-confirm)?$/.test(path)) return rejectPassword();
    return undefined;
  };
}

async function developerStepUp({ page, state, t, fullName }, enabled) {
  let releaseMFA;
  const gate = new Promise((resolveGate) => { releaseMFA = resolveGate; });
  const mfaURL = new URL('/admin/api/v1/security/mfa', baseURL).href;
  await page.route(mfaURL, async (route) => {
    state.requests.push({ method: 'GET', pathname: '/security/mfa', query: '' });
    await gate;
    await route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(securityResponse(enabled)) });
  });
  await page.goto(new URL('/admin/developer', baseURL).href, { waitUntil: 'networkidle' });
  await settleUI(page);
  await button(page, t.developer.createDebugKey).click();
  const dialog = page.getByRole('alertdialog');
  await dialog.waitFor();
  const confirm = button(dialog, t.developer.createDebugKey);
  await confirm.click();
  const password = passwordField(dialog);
  await password.waitFor();
  assert.equal(await password.isEditable(), true, 'Password cannot be edited while MFA status loads');
  await password.fill('wrong-synthetic-password');
  assert.equal(await confirm.isDisabled(), true, 'Step-up can submit before actual enrollment is known');
  releaseMFA();
  await page.waitForFunction(() => !document.querySelector('.confirmation-dialog .form-note')?.textContent.includes('加载'));
  if (enabled) {
    await codeField(dialog).waitFor();
    assert.equal(await codeField(dialog).evaluate((element) => element.required), true, 'Enrolled authenticator code is not required');
    assert.equal(await confirm.isDisabled(), true, 'Enrolled MFA can submit with no code');
    await codeField(dialog).fill('12345678');
  } else {
    await page.waitForFunction(() => !document.querySelector('.confirmation-dialog [role="status"]'));
    assert.equal(await codeField(dialog).count(), 0, 'Unenrolled account was asked for an authenticator code');
  }
  await page.waitForFunction((element) => !element.disabled, await confirm.elementHandle());
  await confirm.click();
  await errors(dialog).waitFor();
  assert.equal(await errors(page).count(), 1, 'A real step-up failure is duplicated outside the confirmation');
  assert.equal(await page.locator('.developer-key-field .notice.error').count(), 0, 'Developer retains the failed child mutation error');
  assert.equal(await password.inputValue(), 'wrong-synthetic-password', 'Wrong-password failure discarded the password');
  assert.equal(await password.isEditable(), true, 'Password cannot be corrected after a real failure');
  if (enabled) {
    assert.equal(await codeField(dialog).inputValue(), '', 'A consumed MFA code was retained for retry');
    assert.equal(await confirm.isDisabled(), true, 'Retry does not require a fresh enrolled-factor code');
  }
  const spacing = await measureNotice(errors(dialog));
  const screenshot = join(outputDir, `${fullName}-wrong-password.png`);
  await page.screenshot({ path: screenshot, fullPage: true });
  const markup = await errors(dialog).evaluate((element) => element.outerHTML);
  await password.fill('correct-synthetic-password');
  if (enabled) await codeField(dialog).fill('12345678');
  await confirm.click();
  await dialog.waitFor({ state: 'hidden' });
  assert.equal(await page.locator('#developer-gateway-key').inputValue(), 'halro_synthetic_acceptance_not_a_real_key');
  assert.equal(await errors(page).count(), 0, 'Successful step-up leaves an old error on the workbench');
  return { mfaEnabled: enabled, passwordEditable: true, wrongPasswordErrorCount: 1, statusLoadingBlocked: true, codeRequired: enabled, authAttempts: state.authAttempts, spacing, screenshot, markup };
}

async function priceConfirmation({ page, state, t, fullName }, enabled, restored) {
  if (restored) { state.deployments[0].pricing_quarantined = true; state.deployments[0].pricing_quarantine_reason = 'Synthetic restored pricing requires confirmation'; }
  await page.goto(new URL('/admin/deployments', baseURL).href, { waitUntil: 'networkidle' });
  await settleUI(page);
  await page.locator('button[aria-haspopup="dialog"]').first().click();
  const details = getDialog(page);
  await details.waitFor();
  if (restored) await button(details, t.deployments.confirmRestoredPricing).click();
  else {
    await button(details, t.deployments.adjustPrice).click();
    await button(getDialog(page), t.deployments.nextReview).click();
  }
  const dialog = getDialog(page);
  const password = passwordField(dialog);
  await password.waitFor();
  assert.equal(await password.isEditable(), true);
  await password.fill('wrong-synthetic-password');
  if (enabled) {
    await codeField(dialog).waitFor();
    assert.equal(await codeField(dialog).evaluate((element) => element.required), true);
    await codeField(dialog).fill('12345678');
  } else {
    await page.waitForFunction(() => !document.querySelector('[role="dialog"]:last-of-type .form-note'));
    assert.equal(await codeField(dialog).count(), 0, 'Pricing asks an unenrolled account for a code');
  }
  await button(dialog, restored ? t.deployments.confirmRestoredPricing : t.deployments.confirmPrice).click();
  await errors(dialog).waitFor();
  assert.equal(await errors(dialog).count(), 1);
  const spacing = await measureNotice(errors(dialog));
  const screenshot = join(outputDir, `${fullName}.png`);
  await page.screenshot({ path: screenshot, fullPage: true });
  return { restored, mfaEnabled: enabled, codeRequired: enabled, spacing, screenshot };
}

async function settingsGridSpacing({ page, t, fullName }) {
  await page.goto(new URL('/admin/settings/instance', baseURL).href, { waitUntil: 'networkidle' });
  await settleUI(page);
  await page.getByRole('spinbutton', { name: new RegExp(t.settings.probeInterval.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')) }).fill('31');
  await button(page, t.settings.saveRuntime).click();
  const notice = page.locator('.runtime-editable .notice.error');
  await notice.waitFor();
  const spacing = await measureNotice(notice);
  assert.equal(spacing.marginTop, 0, 'Settings grid added a notice margin on top of its gap');
  assert.ok(Math.abs(spacing.gapAbove - spacing.parentRowGap) < 1, 'Settings grid gap doubled');
  const screenshot = join(outputDir, `${fullName}.png`);
  await page.screenshot({ path: screenshot, fullPage: true });
  return { spacing, screenshot };
}

async function loginSpacing({ page, t, fullName }) {
  await page.goto(new URL('/admin/login', baseURL).href, { waitUntil: 'networkidle' });
  await page.locator('input[autocomplete="username"]').fill('synthetic-admin');
  await page.locator('input[type="password"]').fill('wrong-synthetic-password');
  await button(page, t.auth.login).click();
  await errors(page).waitFor();
  const spacing = await measureNotice(errors(page));
  assert.equal(spacing.marginTop, 0, 'Feedback rule changed the separate login layout');
  const screenshot = join(outputDir, `${fullName}.png`);
  await page.screenshot({ path: screenshot, fullPage: true });
  return { spacing, screenshot };
}

async function gridContract({ page, state, t, fullName }) {
  // Capture a real ErrorState markup through a rejected synthetic resource read.
  await page.route(new URL('/admin/api/v1/settings', baseURL).href, (route) => route.fulfill({ status: 503, contentType: 'application/json', body: JSON.stringify({ error: 'Synthetic spacing failure', code: 'synthetic_failure' }) }));
  await page.goto(new URL('/admin/settings/instance', baseURL).href, { waitUntil: 'networkidle' });
  await errors(page).waitFor();
  const measurements = await errors(page).evaluate((notice) => {
    const original = notice.cloneNode(true);
    const host = document.createElement('section');
    host.className = 'main';
    host.style.cssText = 'position:fixed;inset:40px 40px auto auto;width:560px;min-height:0;padding:20px;z-index:99999;background:var(--surface-panel)';
    const modes = [{ name: 'block', className: '' }, { name: 'form-grid', className: 'form-grid' }, { name: 'settings-grid', className: 'settings-form' }];
    const values = [];
    for (const mode of modes) {
      const form = document.createElement('form'); form.className = mode.className; form.style.marginBottom = '24px';
      const sentinel = document.createElement('div'); sentinel.textContent = mode.name; sentinel.style.cssText = 'height:32px;grid-column:1/-1;background:var(--surface-inset)';
      const error = original.cloneNode(true);
      form.append(sentinel, error); host.append(form);
      document.body.append(host);
      const box = error.getBoundingClientRect(); const style = getComputedStyle(error); const parentStyle = getComputedStyle(form);
      values.push({ mode: mode.name, display: parentStyle.display, rowGap: parseFloat(parentStyle.rowGap) || 0, noticeMarginTop: parseFloat(style.marginTop), gapAbove: Math.round(box.top - sentinel.getBoundingClientRect().bottom) });
    }
    return values;
  });
  const screenshot = join(outputDir, `${fullName}.png`);
  await page.screenshot({ path: screenshot, fullPage: true });
  for (const item of measurements.filter((item) => item.display === 'grid')) assert.equal(item.gapAbove, item.rowGap, `${item.mode} adds notice margin to an existing grid gap`);
  return { measurements, screenshot, note: 'CSS contract harness clones actual rendered ErrorState markup into block/form-grid/settings-form contexts; business-flow screenshots are separate.' };
}

try {
  const locales = process.env.LOCALES?.split(',') ?? ['zh-CN', 'en-US'];
  const appearances = process.env.APPEARANCES?.split(',') ?? ['dark', 'light'];
  for (const locale of locales) for (const appearance of appearances) {
    for (const enabled of [false, true]) {
      const mode = enabled ? 'mfa-enabled' : 'mfa-disabled';
      const options = { respond: stepUpResponder(enabled) };
      await check(`developer-stepup-${mode}`, locale, appearance, options, (env) => developerStepUp(env, enabled));
      await check(`price-confirm-${mode}`, locale, appearance, options, (env) => priceConfirmation(env, enabled, false));
      await check(`price-restore-${mode}`, locale, appearance, options, (env) => priceConfirmation(env, enabled, true));
    }
    await check('settings-grid-spacing', locale, appearance, { respond: (_state, method, path) => method === 'PUT' && path === '/settings' ? response({ error: 'Synthetic runtime save failed', code: 'synthetic_failure' }, 400) : undefined }, settingsGridSpacing);
    await check('login-spacing', locale, appearance, { auth: 'login', respond: (_state, method, path) => method === 'POST' && path === '/session/login' ? response({ error: 'Invalid username or password', code: 'invalid_credentials' }, 401) : undefined }, loginSpacing);
    await check('grid-contract-spacing', locale, appearance, { respond: stepUpResponder(false) }, gridContract);
  }
} finally {
  await browser.close();
  const report = { generatedAt: new Date().toISOString(), baseURL, syntheticOnly: true, note: 'Local synthetic Admin API; all other origins and unknown endpoints rejected. No real provider calls, real Chrome or Docker access.', total: results.length, passed: results.filter((item) => item.passed).length, failed: results.filter((item) => !item.passed).length, results };
  await writeFile(join(outputDir, 'report.json'), JSON.stringify(report, null, 2));
  process.stdout.write(`Report: ${join(outputDir, 'report.json')} (${report.passed}/${report.total} passed)\n`);
  if (report.failed) process.exitCode = 1;
}
