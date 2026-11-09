#!/usr/bin/env node
// Local visual preview using synthetic records. No live Admin API or provider.
// npm run preview:console -- --port 5274
import { createServer } from 'vite';
import react from '@vitejs/plugin-react';
import { fileURLToPath } from 'node:url';
import { createFixtureState, fixtureResponse } from './console-acceptance-fixtures.mjs';

const portIndex = process.argv.indexOf('--port');
const port = portIndex >= 0 ? Number(process.argv[portIndex + 1]) : 5274;
const state = createFixtureState({ locale: 'zh-CN', appearance: 'light' });
const now = Date.now();
const at = (minutes) => new Date(now - minutes * 60000).toISOString();
state.session.username = 'preview-admin';
state.projects[0].name = '客户服务助手';
state.deployments = ['主对话部署', '推理任务部署', '文档分析部署', '备用对话部署'].map((name, i) => ({ ...state.deployments[0], id: `deployment_preview_${i}`, name, provider_model: ['chat-primary', 'reasoning', 'document-chat', 'chat-backup'][i], enabled: i !== 3 }));
state.routes[0].deployment_id = state.deployments[0].id;
state.attempts = Array.from({ length: 18 }, (_, i) => ({ ...state.attempts[0], event_id: `event_preview_${i}`, request_id: `req_support_${String(1042 - i).padStart(4, '0')}`, deployment_id: state.deployments[i % 4].id, requested_model: i % 3 === 0 ? 'support-reasoning' : 'support-chat', provider_model: state.deployments[i % 4].provider_model, completed_at: at(i * 7), started_at: at(i * 7 + 0.1), latency_millis: 280 + i * 63, provider_input_tokens: 1200 + i * 87, provider_output_tokens: 320 + i * 21, status: i % 5 === 0 ? 'error' : 'success', error_class: i % 5 === 0 ? 'timeout' : '', http_status: i % 5 === 0 ? 504 : 200 }));
state.workUnits = Array.from({ length: 6 }, (_, i) => ({ ...state.workUnits[0], id: `wku_support_${120 + i}`, status: i < 3 ? 'open' : 'closed', created_at: at(180 + i * 20), closed_at: i < 3 ? undefined : at(i * 20) }));
state.runs = state.workUnits.map((unit, i) => ({ ...state.runs[0], id: `run_support_${i}`, work_unit_id: unit.id, status: i < 3 ? 'active' : 'closed', budget_state: i === 1 ? 'depleted' : 'available', created_at: unit.created_at, closed_at: unit.closed_at }));
state.outcomes = state.workUnits.filter((_, i) => i > 3).map((unit, i) => ({ ...state.outcomes[0], id: `out_support_${i}`, work_unit_id: unit.id, observed_at: unit.closed_at, ingested_at: unit.closed_at }));
state.audit = Array.from({ length: 12 }, (_, i) => ({ ...state.audit[0], sequence: 120 - i, event_id: `audit_preview_${i}`, occurred_at: at(i * 13), actor_id: i % 2 === 0 ? 'preview-admin' : 'ops-reviewer', action: ['route.update', 'deployment.create', 'alert_webhook.create'][i % 3], target_type: ['route', 'deployment', 'alert_webhook'][i % 3], target_id: ['route_support', 'deployment_preview_0', 'whk_acceptance'][i % 3], outcome: i % 4 === 0 ? 'failure' : 'success' }));
const server = await createServer({
  configFile: false,
  root: fileURLToPath(new URL('..', import.meta.url)),
  base: '/admin/',
  plugins: [react(), {
    name: 'synthetic-console-preview',
    transformIndexHtml(html) {
      return html.replace('<body>', '<body><div style="position:fixed;bottom:8px;right:12px;z-index:9999;background:#183b35;color:white;border:1px solid #739e92;border-radius:6px;padding:5px 10px;font:12px system-ui;pointer-events:none">本地预览 · 示例数据</div>');
    },
    configureServer(vite) {
      vite.middlewares.use((req, res, next) => {
        const url = new URL(req.url, 'http://127.0.0.1');
        if (!url.pathname.startsWith('/admin/api/')) return next();
        const pathname = url.pathname.slice('/admin/api/v1'.length);
        let body = '';
        req.on('data', (chunk) => { body += chunk; });
        req.on('end', () => {
          let data;
          try { data = body ? JSON.parse(body) : undefined; } catch { res.writeHead(400); res.end(); return; }
          let result;
          if (req.method === 'PUT' && pathname === '/preferences') {
            const appearance = data?.appearance === 'dark' ? 'dark' : 'light';
            const locale = data?.locale === 'en-US' ? 'en-US' : 'zh-CN';
            Object.assign(state.options, { appearance, locale }); Object.assign(state.session, { appearance, locale });
            result = { data: { appearance, locale, revision: 1 } };
          } else if (req.method !== 'GET') {
            result = { status: 403, data: { error: '此预览仅供查看，未连接真实服务。', code: 'preview_read_only' } };
          } else if (pathname === '/usage') {
            const rows = state.attempts.filter((item) => ['status', 'project_id', 'deployment_id', 'provider_id', 'provider_model', 'request_id', 'run_id'].every((key) => !url.searchParams.get(key) || item[key] === url.searchParams.get(key)) && (!url.searchParams.get('model') || item.requested_model === url.searchParams.get('model')) && (!url.searchParams.get('start') || Date.parse(item.completed_at) >= Date.parse(url.searchParams.get('start'))) && (!url.searchParams.get('end') || Date.parse(item.completed_at) <= Date.parse(url.searchParams.get('end'))));
            result = { data: { items: rows, next_cursor: '' } };
          } else result = fixtureResponse(state, req.method, pathname, url.searchParams, data);
          result ??= { status: 501, data: { error: 'Preview fixture unavailable', code: 'preview_missing' } };
          res.writeHead(result.status ?? 200, { 'Content-Type': 'application/json', 'Cache-Control': 'no-store', ...result.headers });
          res.end(JSON.stringify(result.data));
        });
      });
    },
  }],
  server: { host: '127.0.0.1', port, strictPort: true },
});
await server.listen();
console.log(`Synthetic Halro preview: http://127.0.0.1:${port}/admin/usage?tab=attempts`);
