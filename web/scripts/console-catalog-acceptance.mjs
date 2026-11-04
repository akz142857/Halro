#!/usr/bin/env node
// Isolated ModelCatalogCard layout acceptance; no real backend/provider traffic.
import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import { resolve, join } from 'node:path';
import { pathToFileURL } from 'node:url';
import { installConsoleFixture, fixtureResponse } from './console-acceptance-fixtures.mjs';
assert.ok(process.env.PLAYWRIGHT_MODULE, 'PLAYWRIGHT_MODULE is required');
const { chromium } = await import(pathToFileURL(resolve(process.env.PLAYWRIGHT_MODULE)).href);
const baseURL = process.env.BASE_URL ?? 'http://127.0.0.1:5274/admin/';
assert.ok(['localhost', '127.0.0.1', '[::1]'].includes(new URL(baseURL).hostname));
const outputDir = resolve(process.env.OUTPUT_DIR ?? '/private/tmp/halro-project-ai-access-20261104/catalog'); await mkdir(outputDir, { recursive: true });
const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_EXECUTABLE, args: ['--disable-background-networking', '--disable-component-update', '--disable-sync'] }); const results = [];
try {
  for (const locale of process.env.LOCALES?.split(',') ?? ['zh-CN', 'en-US']) for (const appearance of process.env.APPEARANCES?.split(',') ?? ['dark', 'light']) for (const width of process.env.WIDTHS?.split(',').map(Number) ?? [1440, 390]) for (const stateName of process.env.STATES?.split(',') ?? ['disabled', 'current', 'degraded']) {
    const name = `${stateName}-${locale}-${appearance}-${width}`; const context = await browser.newContext({ locale, reducedMotion: 'reduce', viewport: { width, height: 1000 } }); const page = await context.newPage(); page.setDefaultTimeout(8000); const errors = []; page.on('pageerror', (error) => errors.push(error.message));
    const state = await installConsoleFixture(page, { baseURL, locale, appearance, respond: (fixture, method, path, query, body) => {
      if (method !== 'GET' || path !== '/model-catalog') return undefined;
      const result = fixtureResponse(fixture, method, path, query, body); Object.assign(result.data.status, { state: stateName, enabled: stateName !== 'disabled', source: stateName === 'disabled' ? 'bundled' : 'signed_catalog', revision: `sha256:${'synthetic-model-catalog-'.repeat(4)}`, sequence: 4, error_class: stateName === 'degraded' ? 'fetch_failed' : '' }); return result;
    } });
    try {
      await page.goto(new URL('settings/config', baseURL).href, { waitUntil: 'networkidle' }); const card = page.locator('.model-catalog-card'); await card.waitFor(); await page.evaluate(() => document.fonts.ready);
      const metrics = await card.evaluate((element) => {
        const header = element.querySelector(':scope > header'); const title = header.querySelector('h3'); const list = element.querySelector(':scope > dl'); const first = list.firstElementChild; const label = first.querySelector('dt'); const css = getComputedStyle; const rect = (node) => node.getBoundingClientRect(); const number = (value) => parseFloat(value) || 0;
        return { cardPaddingTop: number(css(element).paddingTop), cardPaddingLeft: number(css(element).paddingLeft), titleMarginTop: number(css(title).marginTop), titleMarginBottom: number(css(title).marginBottom), headerHeight: rect(header).height, headerTopBorder: number(css(header).borderTopWidth), headerBottomBorder: number(css(header).borderBottomWidth), firstRowTopBorder: number(css(first).borderTopWidth), sourceLabelGap: rect(label).top - rect(header).bottom, titleInset: rect(title).left - rect(element).left, sourceInset: rect(label).left - rect(element).left, documentWidth: document.documentElement.scrollWidth, viewportWidth: innerWidth };
      });
      if (!process.env.BASELINE) { assert.equal(metrics.cardPaddingTop, 0); assert.equal(metrics.cardPaddingLeft, 0); assert.equal(metrics.titleMarginTop, 0); assert.equal(metrics.titleMarginBottom, 0); assert.equal(metrics.headerTopBorder, 0); assert.equal(metrics.firstRowTopBorder, 0); assert.equal(metrics.sourceLabelGap, 12); assert.equal(metrics.titleInset, 17); assert.equal(metrics.sourceInset, 17); assert.ok(metrics.headerHeight <= (width < 600 ? 112 : 80), 'Header remains oversized'); }
      assert.ok(metrics.documentWidth <= metrics.viewportWidth + 1); assert.deepEqual(errors, []); assert.deepEqual(state.unknownRoutes, []); assert.deepEqual(state.blockedRequests, []);
      const screenshot = join(outputDir, `${name}.png`); await card.screenshot({ path: screenshot }); results.push({ name, passed: true, metrics, screenshot }); process.stdout.write(`PASS ${name}\n`);
    } catch (error) { const screenshot = join(outputDir, `${name}-failure.png`); await page.screenshot({ path: screenshot, fullPage: true }).catch(() => {}); results.push({ name, passed: false, error: error.stack, screenshot }); process.stdout.write(`FAIL ${name}: ${error.message.split('\n')[0]}\n`); }
    finally { await context.close(); }
    await writeFile(join(outputDir, 'report.json'), JSON.stringify({ baseURL, syntheticOnly: true, baseline: Boolean(process.env.BASELINE), total: results.length, passed: results.filter((result) => result.passed).length, failed: results.filter((result) => !result.passed).length, results }, null, 2));
  }
} finally { await browser.close(); }
process.exitCode = results.some((result) => !result.passed) ? 1 : 0;
