// Browser task acceptance against isolated synthetic API contracts only.
// Run a local frontend at BASE_URL and (optionally) the old frontend at
// BASELINE_URL; HALRO_PLAYWRIGHT_MODULE may point to an installed Playwright.
import assert from 'node:assert/strict';
import { mkdir, writeFile } from 'node:fs/promises';
import { resolve } from 'node:path';
import { installConsoleFixture, fixtureResponse, NOW } from './console-acceptance-fixtures.mjs';

const { chromium } = await import(process.env.HALRO_PLAYWRIGHT_MODULE || 'playwright');
const baseURL = process.env.BASE_URL || 'http://127.0.0.1:5274/admin/';
const baselineURL = process.env.BASELINE_URL || 'http://127.0.0.1:5273/admin/';
const output = resolve(process.env.HALRO_ACCEPTANCE_OUTPUT || '/tmp/halro-console-workflows');
await mkdir(output, { recursive: true });
const browser = await chromium.launch({ headless: true, executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH });
const report = { generatedAt: new Date().toISOString(), baseURL, baselineURL, syntheticOnly: true, checks: [], measurements: {} };
let serial = 0;
const contexts = [];
const pageErrors = [];
let activePage;
async function fresh(url = baseURL, options = {}) {
  const context = await browser.newContext({ viewport: { width: 1440, height: 900 }, reducedMotion: 'reduce' });
  contexts.push(context);
  const page = await context.newPage();
  activePage = page;
  page.setDefaultTimeout(10000);
  page.on('pageerror', (error) => pageErrors.push({ url: page.url(), message: error.message }));
  const state = await installConsoleFixture(page, { baseURL: url, ...options });
  return { context, page, state, url };
}
async function visit(env, path) {
  await env.page.goto(new URL(`/admin/${path}`, env.url).href);
  await env.page.locator('.page-header h1').waitFor();
}
async function disclosure(page, title, open = true) {
  const element = page.locator('details').filter({ has: page.locator(':scope > summary .disclosure-title', { hasText: title }) }).first();
  const isOpen = await element.evaluate((el) => el.open);
  if (isOpen !== open) await element.locator(':scope > summary').click();
  assert.equal(await element.evaluate((el) => el.open), open, `${title} disclosure state`);
  return element;
}
async function check(name, fn) {
  if (process.env.HALRO_WORKFLOW_CASE && !name.includes(process.env.HALRO_WORKFLOW_CASE)) return;
  const item = { name, status: 'running' }; report.checks.push(item);
  try { await fn(item); item.status = 'passed'; console.log(`PASS ${name}`); }
  catch (error) { item.status = 'failed'; item.error = error.stack; console.error(`FAIL ${name}: ${error.message}`); if (activePage && !activePage.isClosed()) { await activePage.screenshot({ path: `${output}/failed-${name}.png` }); await writeFile(`${output}/failed-${name}.txt`, await activePage.locator('body').textContent()); } }
  await writeFile(`${output}/report.json`, JSON.stringify(report, null, 2));
}
const ok = (data, status = 200, headers = {}) => ({ data, status, headers: { ETag: '"1"', ...headers } });
function workflowResponse(state, method, path, query, body) {
  if (method !== 'GET' && state.options.role === 'read_only') return undefined;
  if (method === 'GET' && /^\/deployments\/[^/]+\/prices$/.test(path)) return ok({ items: state.prices.filter((price) => price.deployment_id === path.split('/')[2]), next_cursor: '' });
  if (method === 'GET' && /^\/providers\/[^/]+\/invocation-targets$/.test(path)) {
    const response = fixtureResponse(state, method, path, query, body);
    response.data.items.push({ ...response.data.items[0], target_id: 'synthetic-unknown', display_name: 'Unknown synthetic model', canonical_model_ref: '', variants: [], resolution_state: 'unknown', resolution_revision: 'sha256:unknown' });
    return response;
  }
  if (method === 'POST' && path.includes('/synthetic-unknown/resolution')) return ok({ target_id: 'synthetic-unknown', target_kind: 'model_id', display_name: 'Unknown synthetic model', canonical_model_ref: '', lifecycle: 'active', metadata: {}, metadata_source: 'none', availability: 'available', fetched_at: NOW, variants: [], resolution_state: 'unknown', resolution_revision: 'sha256:unknown' });
  if (method === 'POST' && path === '/deployments') {
    const response = fixtureResponse(state, method, path, query, body);
    const provider = state.providers.find((item) => item.id === body.provider_id);
    Object.assign(response.data, { access_surface: provider.access_surface, profile_id: provider.profile_id, priority: 0, weight: 1, capability_evidence: Object.fromEntries(Object.entries(body.capabilities).filter(([, value]) => value === true).map(([key]) => [key, 'declared'])) });
    return response;
  }
  if (method === 'POST' && /^\/deployments\/[^/]+\/prices$/.test(path)) {
    const value = { ...body, id: `price_workflow_${++serial}`, deployment_id: path.split('/')[2], version: 1, status: 'active', effective_from: '2026-01-01T00:00:00Z', created_at: NOW, revision: 1 };
    state.prices.push(value); state.writes.push({ method, pathname: path, body, id: value.id });
    return ok(value, 201);
  }
  if (method === 'POST' && /^\/deployments\/[^/]+\/test$/.test(path)) {
    const value = state.deployments.find((item) => item.id === path.split('/')[2]);
    Object.assign(value, { last_test_status: 'healthy', last_test_revision: value.revision, last_test_current: true, last_tested_at: NOW, last_test_latency_ms: 12 });
    state.writes.push({ method, pathname: path, syntheticProbe: true });
    return ok({ status: 'healthy', latency_ms: 12, tested_at: NOW, revision: value.revision });
  }
  if (method === 'PUT' && /^\/deployments\/[^/]+$/.test(path)) {
    const value = state.deployments.find((item) => item.id === path.split('/')[2]);
    if (body.enabled) {
      assert.equal(value.last_test_current, true, 'test required before enable');
      assert.ok(state.prices.some((price) => price.deployment_id === value.id), 'price required before enable');
    }
    Object.assign(value, body, { revision: value.revision + 1 });
    state.writes.push({ method, pathname: path, body, id: value.id });
    return ok(value);
  }
  if (method === 'POST' && /^\/projects\/[^/]+\/keys$/.test(path)) {
    const metadata = { id: `key_workflow_${++serial}`, name: body.name, project_id: path.split('/')[2], enabled: true, scopes: body.scopes, created_at: NOW, revision: 1 };
    state.keys.push(metadata); state.writes.push({ method, pathname: path, id: metadata.id });
    return ok({ key: 'halro_synthetic_acceptance_not_a_real_key', metadata }, 201);
  }
  if (method === 'POST' && path.startsWith('/developer/execute/')) {
    assert.ok(state.keys.length, 'synthetic Gateway Key must exist');
    assert.ok(state.projects.some((project) => project.allowed_models.includes(body.model)), 'request model is authorized by a synthetic project');
    state.writes.push({ method, pathname: path, body, syntheticExecution: true });
    return ok({ id: 'response_synthetic_workflow', object: 'response', output: [{ type: 'message', content: [{ type: 'output_text', text: 'Synthetic acceptance response.' }] }], usage: { input_tokens: 3, output_tokens: 4 } }, 200, { 'X-Request-ID': 'req_acceptance' });
  }
  return undefined;
}

await check('provider-before-after-measurement', async (item) => {
  const measures = [];
  for (const [version, url] of [['before', baselineURL], ['after', baseURL]]) {
    const env = await fresh(url);
    await visit(env, 'providers');
    await env.page.evaluate(() => { globalThis.__acceptanceClicks = 0; document.addEventListener('click', () => { globalThis.__acceptanceClicks += 1; }); });
    await env.page.getByRole('button', { name: '＋ 服务商', exact: true }).click();
    const form = env.page.locator('.provider-form'); await form.waitFor();
    const measurement = await form.evaluate((element) => {
      const visible = (el) => el.checkVisibility({ checkVisibilityCSS: true }) && el.getBoundingClientRect().width > 0;
      const normalize = (text) => text.replace(/\s+/g, ' ').trim();
      const help = Array.from(element.querySelectorAll('small, header > p, .capability-advanced-note, .field-hint')).filter((el) => !el.closest('.notice,.capability-option'));
      const texts = [...new Set(help.filter(visible).map((el) => normalize(el.textContent)).filter(Boolean))];
      const risks = Array.from(element.querySelectorAll('.notice')).filter(visible).map((el) => normalize(el.textContent));
      const fields = Array.from(element.querySelectorAll('input:not([type=checkbox]),select')).filter(visible).map((el) => { const box = el.getBoundingClientRect(); return { label: el.closest('.field')?.querySelector(':scope > span')?.textContent, top: box.top, bottom: box.bottom, value: el.value }; });
      return { help: texts, helpCharacters: texts.reduce((sum, text) => sum + Array.from(text.replace(/\s/g, '')).length, 0), risks, fields, allText: normalize(element.textContent), viewport: { width: innerWidth, height: innerHeight } };
    });
    await env.page.screenshot({ path: `${output}/provider-${version}.png` });
    await env.page.getByLabel('服务商名称', { exact: true }).fill('Measured synthetic provider');
    await env.page.getByRole('button', { name: /^创建并(?:热加载|应用)$/ }).click();
    await env.page.getByRole('dialog').waitFor({ state: 'detached' });
    measurement.createClicks = await env.page.evaluate(() => globalThis.__acceptanceClicks);
    measurement.version = version; measures.push(measurement);
    assert.equal(env.state.blockedRequests.length, 0); assert.equal(env.state.unknownRoutes.length, 0);
    await env.context.close();
  }
  const [before, after] = measures;
  const deleted = before.help.filter((text) => !after.allText.includes(text));
  const folded = before.help.filter((text) => after.allText.includes(text) && !after.help.includes(text));
  report.measurements = { before, after, deleted, folded, helperCharacterReduction: before.helpCharacters ? 1 - after.helpCharacters / before.helpCharacters : 0, requiredFieldsInFirstViewport: after.fields.every((field) => field.bottom <= after.viewport.height), ordinaryCreateClicks: { before: before.createClicks, after: after.createClicks, definition: 'Open create dialog, submit after entering a name; existing compatible credential defaults are retained. Typing and navigation are excluded.' } };
  item.helpCharacters = { before: before.helpCharacters, after: after.helpCharacters };
  assert.ok(before.helpCharacters > after.helpCharacters, 'visible helper copy reduced');
  assert.ok(report.measurements.requiredFieldsInFirstViewport, 'required ordinary provider fields fit viewport');
});

await check('provider-disclosure-risk-focus-and-keyboard', async (item) => {
  const env = await fresh(baseURL, { respond: workflowResponse }); const { page, state } = env;
  state.credentials.push({ ...state.credentials[0], id: 'credential_anthropic', type: 'anthropic', access_surface: 'anthropic-api', offering_id: 'anthropic.console-api', scheme: 'anthropic.x-api-key', bound_base_url: 'https://api.anthropic.com:443' });
  await visit(env, 'providers');
  await page.getByRole('button', { name: '＋ 服务商', exact: true }).click();
  const close = page.getByRole('dialog').getByRole('button', { name: '关闭', exact: true });
  const commit = page.getByRole('button', { name: '创建并应用', exact: true });
  await close.focus(); await page.keyboard.press('Shift+Tab');
  assert.equal(await commit.evaluate((el) => el === document.activeElement), true, 'Shift+Tab wraps to primary action');
  await page.keyboard.press('Tab');
  assert.equal(await close.evaluate((el) => el === document.activeElement), true, 'Tab wraps to close');
  const capabilityDisclosure = page.locator('details').filter({ has: page.locator('.disclosure-title', { hasText: '开放能力' }) });
  await capabilityDisclosure.locator(':scope > summary').focus();
  await page.keyboard.press('Enter');
  assert.equal(await capabilityDisclosure.evaluate((el) => el.open), true, 'Enter expands native disclosure');
  await page.keyboard.press('Space');
  assert.equal(await capabilityDisclosure.evaluate((el) => el.open), false, 'Space collapses native disclosure');
  await page.getByLabel('服务商名称', { exact: true }).fill('Unsubmitted');
  await page.keyboard.press('Escape');
  await page.getByRole('button', { name: '继续编辑', exact: true }).waitFor();
  await page.getByRole('button', { name: '继续编辑', exact: true }).click();
  await page.getByRole('combobox', { name: 'API 接口', exact: true }).selectOption('openai.responses.v1');
  await page.getByText(/已按所选接口更新地址、凭据和开放能力/).waitFor();
  await page.getByRole('combobox', { name: '类型', exact: true }).selectOption('anthropic');
  await disclosure(page, '开放能力');
  await page.getByRole('checkbox', { name: /服务商侧执行工具/ }).check();
  await disclosure(page, '开放能力', false);
  assert.equal(await page.getByText(/这部分流量不经过 Halro/).isVisible(), true);
  await disclosure(page, '连接选项');
  await page.getByLabel(/Anthropic Beta 允许列表/).fill('INVALID TOKEN');
  await page.getByRole('combobox', { name: '出站路径', exact: true }).selectOption(state.proxies[0].id);
  await disclosure(page, '连接选项', false);
  assert.equal(await page.getByText(/代理可见目标 IP、端口和 TLS 服务器名/).isVisible(), true);
  await page.getByRole('button', { name: '创建并应用', exact: true }).click();
  const beta = page.getByLabel(/Anthropic Beta 允许列表/);
  await beta.waitFor({ state: 'visible' });
  await page.waitForFunction((id) => document.activeElement?.id === id, await beta.getAttribute('id'));
  assert.equal(await beta.evaluate((el) => el === document.activeElement), true, 'invalid hidden field focused');
  // Closing a dirty modal remains cancellable, and focus returns to its trigger.
  await page.keyboard.press('Escape');
  await page.getByRole('button', { name: '放弃填写的内容', exact: true }).click();
  assert.equal(await page.getByRole('dialog').count(), 0);
  await page.waitForFunction(() => document.activeElement?.textContent === '＋ 服务商');
  assert.equal(await page.getByRole('button', { name: '＋ 服务商', exact: true }).evaluate((el) => el === document.activeElement), true);
  assert.equal(state.writes.length, 0, 'invalid form/dirty discard writes nothing');
  assert.equal(state.unknownRoutes.length, 0); assert.equal(state.blockedRequests.length, 0);
  item.requests = state.requests.length;
  await env.context.close();
});

await check('ordinary-configuration-task-chain', async (item) => {
  const env = await fresh(baseURL, { respond: workflowResponse }); const { page, state } = env;
  for (const collection of ['providers', 'deployments', 'routes', 'projects', 'keys', 'prices', 'attempts']) state[collection] = [];
  await visit(env, 'providers');
  await page.getByRole('button', { name: '＋ 服务商', exact: true }).click();
  await page.getByLabel('服务商名称', { exact: true }).fill('Workflow provider');
  await page.getByRole('button', { name: '创建并应用', exact: true }).click();
  await page.getByRole('dialog').waitFor({ state: 'detached' });
  assert.equal(state.providers.length, 1);
  await visit(env, 'deployments');
  await page.getByRole('button', { name: /新建模型部署/ }).click();
  await page.getByLabel('部署名称', { exact: true }).fill('Workflow deployment');
  await page.locator('#deployment-provider-model-id').click();
  await page.getByRole('option', { name: 'GPT Chat', exact: true }).click();
  await page.getByRole('button', { name: '保存为停用', exact: true }).click();
  await page.getByRole('dialog').waitFor({ state: 'detached' });
  assert.equal(state.deployments[0].enabled, false, 'new deployments begin disabled');
  const row = page.locator('article.resource-card').filter({ hasText: 'Workflow deployment' });
  await row.getByRole('button', { name: '查看详情 — Workflow deployment', exact: true }).click();
  await page.getByRole('button', { name: '设置价格', exact: true }).click();
  await page.getByRole('radio', { name: /^免费/ }).check();
  await page.getByRole('combobox', { name: '价格依据', exact: true }).selectOption('internal_cost');
  await page.getByRole('button', { name: '下一步：核对', exact: true }).click();
  await page.getByLabel(/^当前密码/).fill('synthetic-acceptance-password');
  await page.getByRole('button', { name: '确认并创建价格版本', exact: true }).click();
  await page.getByRole('dialog', { name: '设置价格', exact: true }).waitFor({ state: 'detached' });
  await page.getByRole('dialog', { name: 'Workflow deployment 详情', exact: true }).getByRole('button', { name: '关闭', exact: true }).click();
  await row.getByRole('button', { name: '测试 — Workflow deployment', exact: true }).click();
  await row.getByRole('button', { name: '启用 — Workflow deployment', exact: true }).click();
  await row.getByRole('button', { name: '禁用 — Workflow deployment', exact: true }).waitFor();
  assert.equal(state.deployments[0].enabled, true);
  await visit(env, 'routes');
  await page.getByRole('button', { name: /新建.*路由/ }).click();
  await page.getByRole('combobox', { name: '公共模型别名', exact: true }).fill('workflow-chat');
  await page.getByRole('button', { name: '创建并应用', exact: true }).click();
  await page.getByRole('dialog').waitFor({ state: 'detached' });
  assert.equal(state.routes[0].deployment_id, state.deployments[0].id);
  await visit(env, 'projects');
  await page.getByRole('button', { name: /新建项目/ }).click();
  await page.getByRole('dialog').getByLabel('名称', { exact: true }).fill('Workflow application');
  await page.getByRole('checkbox', { name: /workflow-chat/ }).check();
  await page.getByRole('button', { name: '创建项目', exact: true }).click();
  await page.getByRole('dialog').waitFor({ state: 'detached' });
  assert.deepEqual(state.projects[0].allowed_models, ['workflow-chat']);
  await page.getByRole('button', { name: '＋ 创建密钥', exact: true }).first().click();
  await page.getByLabel(/^密钥名称/).fill('Workflow key');
  await page.getByLabel(/^当前密码/).fill('synthetic-acceptance-password');
  await page.getByRole('button', { name: '生成密钥', exact: true }).click();
  await page.getByRole('dialog', { name: '保存这个网关密钥', exact: true }).waitFor();
  await page.keyboard.press('Escape');
  assert.equal(await page.getByRole('dialog', { name: '保存这个网关密钥', exact: true }).isVisible(), true, 'one-time key cannot be lost before acknowledgement');
  assert.equal(await page.getByRole('button', { name: '完成并清除明文', exact: true }).isDisabled(), true);
  await page.getByRole('checkbox', { name: '我已将密钥保存到安全的密钥管理器', exact: true }).check();
  await page.getByRole('button', { name: '完成并清除明文', exact: true }).click();
  assert.equal(await page.getByText('halro_synthetic_acceptance_not_a_real_key', { exact: true }).count(), 0);
  await visit(env, 'developer');
  await page.getByRole('button', { name: '连接设置', exact: true }).click();
  await page.getByRole('combobox', { name: /^项目/ }).selectOption(state.projects[0].id);
  await page.keyboard.press('Escape');
  assert.equal(await page.getByRole('combobox', { name: '模型', exact: true }).inputValue(), 'workflow-chat');
  await page.getByLabel('Gateway Key', { exact: true }).fill('halro_synthetic_acceptance_not_a_real_key');
  await page.getByRole('button', { name: '发送', exact: true }).click();
  await page.getByText('请求已完成', { exact: true }).waitFor();
  await page.getByText(/Synthetic acceptance response/).waitFor();
  assert.equal(await page.evaluate(() => Object.values(localStorage).some((value) => value.includes('halro_synthetic_acceptance_not_a_real_key')) || Object.values(sessionStorage).some((value) => value.includes('halro_synthetic_acceptance_not_a_real_key'))), false);
  assert.equal(state.writes.filter((entry) => entry.syntheticExecution).length, 1);
  assert.equal(state.unknownRoutes.length, 0, JSON.stringify(state.unknownRoutes)); assert.equal(state.blockedRequests.length, 0);
  item.writes = state.writes.map(({ method, pathname, id, syntheticExecution, syntheticProbe }) => ({ method, pathname, id, syntheticExecution, syntheticProbe }));
  await page.screenshot({ path: `${output}/task-chain-completed.png` });
  await env.context.close();
});

await check('unknown-capability-stays-unknown-and-readonly-is-not-writable', async () => {
  const env = await fresh(baseURL, { respond: workflowResponse });
  await visit(env, 'deployments');
  await env.page.getByRole('button', { name: /新建模型部署/ }).click();
  await env.page.getByLabel('部署名称', { exact: true }).fill('Unknown model');
  await env.page.locator('#deployment-provider-model-id').click();
  await env.page.getByRole('option', { name: 'Unknown synthetic model', exact: true }).click();
  assert.equal(await env.page.getByRole('button', { name: '保存为停用', exact: true }).isDisabled(), true);
  assert.equal(await env.page.locator('.deployment-capability-summary').count(), 0, 'no fabricated supported-capability summary');
  await env.context.close();
  const readonly = await fresh(baseURL, { role: 'read_only', respond: workflowResponse });
  for (const [path, label] of [['providers', '＋ 服务商'], ['deployments', /新建模型部署/], ['projects', /新建项目/]]) {
    await visit(readonly, path);
    assert.equal(await readonly.page.getByRole('button', { name: label, exact: typeof label === 'string' }).isDisabled(), true, `read-only ${path} create`);
  }
  assert.equal(readonly.state.writes.length, 0);
  assert.equal(readonly.state.unknownRoutes.length, 0); assert.equal(readonly.state.blockedRequests.length, 0);
  await readonly.context.close();
});

report.pageErrors = pageErrors;
await Promise.allSettled(contexts.map((context) => context.close()));
await browser.close();
await writeFile(`${output}/report.json`, JSON.stringify(report, null, 2));
console.log(JSON.stringify({ passed: report.checks.filter((item) => item.status === 'passed').length, failed: report.checks.filter((item) => item.status === 'failed').length, output }));
if (report.checks.some((item) => item.status === 'failed') || pageErrors.length) process.exitCode = 1;
