#!/usr/bin/env node
// Pixel equality for the unchanged login page. Both previews use synthetic APIs.
import assert from 'node:assert/strict';
import { createHash } from 'node:crypto';
import { mkdir, writeFile } from 'node:fs/promises';
import { join, resolve } from 'node:path';
import { pathToFileURL } from 'node:url';
import { installConsoleFixture } from './console-acceptance-fixtures.mjs';
assert.ok(process.env.PLAYWRIGHT_MODULE, 'PLAYWRIGHT_MODULE must name an installed module');
const { chromium } = await import(pathToFileURL(resolve(process.env.PLAYWRIGHT_MODULE)).href);
const browser = await chromium.launch({ headless: true, executablePath: process.env.CHROMIUM_EXECUTABLE, args: ['--disable-background-networking', '--disable-component-update', '--disable-sync'] });
const beforeURL = process.env.BASELINE_URL ?? 'http://127.0.0.1:5276/admin/';
const afterURL = process.env.BASE_URL ?? 'http://127.0.0.1:5275/admin/';
const outputDir = process.env.OUTPUT_DIR ?? '/private/tmp/halro-system-polish-20261104/login-pixels';
await mkdir(outputDir, { recursive: true });
const results = [];
try {
  for (const locale of ['zh-CN', 'en-US']) for (const appearance of ['dark', 'light']) for (const width of [1440, 390]) {
    const name = `${locale}-${appearance}-${width}`; const images = [];
    for (const [version, baseURL] of [['before', beforeURL], ['after', afterURL]]) {
      const context = await browser.newContext({ viewport: { width, height: 1000 }, locale, timezoneId: 'Asia/Singapore', reducedMotion: 'reduce' }); const page = await context.newPage();
      const errors = []; page.on('pageerror', (error) => errors.push(error.message));
      const state = await installConsoleFixture(page, { baseURL, locale, appearance, auth: 'login' });
      await page.goto(new URL('login', baseURL).href, { waitUntil: 'networkidle' }); await page.locator('input[autocomplete="username"]').waitFor(); await page.evaluate(() => document.fonts.ready);
      // Eliminate the time-dependent caret while preserving all rendered pixels.
      await page.locator('h2').first().click();
      const path = join(outputDir, `${name}-${version}.png`); const bytes = await page.screenshot({ path, fullPage: true, animations: 'disabled', caret: 'hide' });
      assert.deepEqual(state.unknownRoutes, []); assert.deepEqual(state.blockedRequests, []); assert.deepEqual(errors, []);
      images.push({ bytes, path, sha256: createHash('sha256').update(bytes).digest('hex') }); await context.close();
    }
    let comparison = { differentPixels: 0, maxChannelDelta: 0, method: 'identical PNG bytes' };
    if (!images[0].bytes.equals(images[1].bytes)) {
      // Decode with Chromium's PNG implementation; no extra package is needed.
      const page = await browser.newPage(); await page.route('**/*', (route) => route.abort('blockedbyclient'));
      comparison = await page.evaluate(async ({ before, after }) => {
        const decode = async (data) => { const image = new Image(); image.src = `data:image/png;base64,${data}`; await image.decode(); const canvas = document.createElement('canvas'); canvas.width = image.naturalWidth; canvas.height = image.naturalHeight; const context = canvas.getContext('2d'); context.drawImage(image, 0, 0); return { width: canvas.width, height: canvas.height, pixels: context.getImageData(0, 0, canvas.width, canvas.height).data }; };
        const left = await decode(before); const right = await decode(after);
        if (left.width !== right.width || left.height !== right.height) return { differentPixels: null, sameDimensions: false, before: [left.width, left.height], after: [right.width, right.height] };
        let differentPixels = 0; let maxChannelDelta = 0;
        for (let index = 0; index < left.pixels.length; index += 4) { let changed = false; for (let channel = 0; channel < 4; channel++) { const delta = Math.abs(left.pixels[index + channel] - right.pixels[index + channel]); changed ||= delta !== 0; maxChannelDelta = Math.max(maxChannelDelta, delta); } if (changed) differentPixels++; }
        return { differentPixels, maxChannelDelta, sameDimensions: true, width: left.width, height: left.height, method: 'decoded RGBA equality' };
      }, { before: images[0].bytes.toString('base64'), after: images[1].bytes.toString('base64') }); await page.close();
    }
    results.push({ name, passed: comparison.differentPixels === 0, before: { path: images[0].path, sha256: images[0].sha256 }, after: { path: images[1].path, sha256: images[1].sha256 }, comparison });
    process.stdout.write(`${results.at(-1).passed ? 'PASS' : 'FAIL'} ${name}: ${comparison.differentPixels} different pixels\n`);
  }
} finally { await browser.close(); }
await writeFile(join(outputDir, 'report.json'), JSON.stringify({ syntheticOnly: true, beforeURL, afterURL, total: results.length, passed: results.filter((result) => result.passed).length, results }, null, 2));
process.exitCode = results.some((result) => !result.passed) ? 1 : 0;
