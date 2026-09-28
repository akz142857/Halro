import { readFileSync } from "node:fs";
import { resolve } from "node:path";
import { JSDOM } from "jsdom";
import { describe, expect, it } from "vitest";

const root = resolve(process.cwd(), "../cmd/halro-ha-health/ui");
const html = readFileSync(resolve(root, "index.html"), "utf8");
const app = readFileSync(resolve(root, "app.js"), "utf8");
const signal = { level: "healthy", reason: "sample" };
const changedAt = "1970-01-01T00:02:10Z";
const previousAt = "1970-01-01T00:01:40Z";
const changes = [
  { instance: "halro-0", kind: "role", from: "replica", to: "primary", previous_seen: previousAt, first_seen: changedAt },
  { instance: "halro-0", kind: "maintenance", from: "normal", to: "maintenance", previous_seen: previousAt, first_seen: changedAt },
  { instance: "halro-0", peer: "halro-1", kind: "peer_session", from: "connected", to: "disconnected", previous_seen: previousAt, first_seen: changedAt },
];
const history = {
  series: [{ metric: { __name__: "halro_replication_index", instance: "halro-0", kind: "durable" }, values: [[100, "10"], [130, "12"]] }],
  sampled_events: changes,
};

function boot(runbookBase = "") {
  const dom = new JSDOM(html.replace("__HALRO_RUNBOOK_BASE__", runbookBase), { url: "http://127.0.0.1:19105/", runScripts: "outside-only" });
  for (const dialog of dom.window.document.querySelectorAll("dialog")) {
    dialog.showModal = () => { dialog.open = true; };
    dialog.close = () => { dialog.open = false; };
  }
  const payloads = {
    "/api/health": { overall: signal, client: signal, confirmation: signal, safety: signal, catchup: signal,
      environment: "test", cluster: "ha", observed_at: new Date().toISOString(), expected_members: 2, members: [], member_statuses: [],
      confirmation_evidence: { status: "observed", source: "Prometheus required wait", instance: "halro-0", increase_5m: 1, sampled_at: changedAt, epoch_sampled_at: changedAt, evaluated_at: changedAt } },
    "/api/history": history, "/api/alerts": [], "/api/latency": [], "/api/impact": { observed: false },
    "/api/client-final": { status: "not_configured" },
    "/api/event-archive": { status: "not_configured" },
    "/api/durable-transitions": { status: "not_configured" },
  };
  const calls = [];
  dom.window.fetch = async url => {
    const path = String(url).split("?")[0];
    calls.push(path);
    return { ok: payloads[path] !== null, json: async () => payloads[path] };
  };
  let interval, expiryTimer;
  dom.window.setInterval = callback => { interval = callback; return 0; };
  dom.window.setTimeout = (callback, delay) => { expiryTimer = { callback, delay }; return 1; };
  dom.window.clearTimeout = () => { expiryTimer = undefined; };
  dom.window.eval(app);
  return { dom, payloads, calls, document: dom.window.document, tick: () => interval?.(),
    expire: () => expiryTimer?.callback(), expiryDelay: () => expiryTimer?.delay };
}

const settled = () => new Promise(resolve => setTimeout(resolve, 20));

describe("independent HA health page", () => {
  it("shows isolated historical samples when gaps break every line segment", async () => {
    const { dom, payloads, document } = boot();
    try {
      payloads["/api/history"] = { series: [{ metric: { __name__: "halro_replication_index", instance: "halro-0", kind: "durable" },
        values: [[100, "10"], [160, "11"], [220, "12"]] }], sampled_events: [] };
      document.querySelector("#view").value = "history";
      document.querySelector("#view").dispatchEvent(new dom.window.Event("change"));
      await settled();
      expect(document.querySelectorAll("#chart circle")).toHaveLength(3);
      expect(document.querySelectorAll("#chart polyline")).toHaveLength(0);
      expect(document.querySelector("#chart circle title").textContent).toContain("halro-0 durable");
      expect(document.querySelectorAll("#chart text").length).toBeGreaterThanOrEqual(4);
    } finally {
      dom.window.close();
    }
  });

  it("shows durable transition coverage without treating a legacy baseline as full history", async () => {
    const { dom, payloads, document } = boot();
    try {
      payloads["/api/durable-transitions"] = { status: "partial", members: [
        { node_id: "halro-0", status: "caught_up", incarnation: "inc-1", baseline_kind: "legacy_baseline", stored_sequence: 4, observed_head: 4, events_retained: 4, observed_at: new Date().toISOString(), recent_events: [
          { sequence: 4, at: new Date().toISOString(), kind: "promote", from_role: "replica", to_role: "primary", from_term: 7, to_term: 8, from_promised_term: 8, to_promised_term: 8 },
        ] },
        { node_id: "halro-1", status: "unavailable", failure: "journal_changed_requires_reconciliation", stored_sequence: 2, observed_head: 2, events_retained: 2 },
      ] };
      document.querySelector("#view").value = "history";
      document.querySelector("#view").dispatchEvent(new dom.window.Event("change"));
      await settled();
      expect(document.querySelector("#durable-archive-summary").textContent).toContain("不完整");
      expect(document.querySelector("#durable-archive-summary").textContent).toContain("本地证据");
      expect(document.querySelector("#durable-archive").textContent).toContain("传统迁移基线（此前未知）");
      expect(document.querySelector("#durable-archive").textContent).toContain("来源不可用");
      expect(document.querySelector("#durable-archive").textContent).toContain("旧链待核验交接");
      expect(document.querySelector("#durable-archive").textContent).toContain("保留采集器文件和旧成员数据");
      expect(document.querySelector("#durable-events").textContent).toContain("replica / 7 / 8");
      expect(document.querySelector("#durable-events").textContent).toContain("primary / 8 / 8");
    } finally {
      dom.window.close();
    }
  });

  it("shows external client-final results only when the coverage gate reports observed", async () => {
    const { dom, payloads, document, tick } = boot();
    try {
      await settled();
      expect(document.querySelector("#client-final").textContent).toContain("未接入");
      payloads["/api/client-final"] = { status: "unobserved", reason: "coverage_incomplete" };
      tick();
      await settled();
      expect(document.querySelector("#client-final").textContent).toContain("未观测");
      expect(document.querySelector("#client-final").textContent).toContain("采样连续性不完整");
      payloads["/api/client-final"] = { status: "unobserved", reason: "empty_window" };
      tick();
      await settled();
      expect(document.querySelector("#client-final").textContent).toContain("窗口内无最终逻辑操作");
      payloads["/api/client-final"] = { status: "observed", acceptance_record: "ha-client-final-acceptance", counts: { write: { success: 2, timeout: 1 } } };
      tick();
      await settled();
      expect(document.querySelector("#client-final").textContent).toContain("成功 2.0");
      expect(document.querySelector("#client-final").textContent).toContain("超时 1.0");
      expect(document.querySelector("#client-final").textContent).toContain("ha-client-final-acceptance");
    } finally {
      dom.window.close();
    }
  });

  it("names missing member HA signals in node details", async () => {
    const { dom, payloads, document } = boot();
    try {
      const sampled_at = new Date().toISOString();
      payloads["/api/health"].observed_at = sampled_at;
      payloads["/api/health"].members = [
        { instance: "halro-0", sampled_at, up_sampled_at: sampled_at, newest_sampled_at: sampled_at, up: true, role: "primary", maintenance: false,
          startup_ready: true, replication_unavailable: false,
          incompatible_reasons: { schema: false, key_challenge: false }, peers: {} },
        { instance: "halro-1", sampled_at, up_sampled_at: sampled_at, newest_sampled_at: sampled_at, up: true, role: "replica" },
      ];
      await settled();
      document.querySelector("#nodes .node-button").click();
      const details = document.querySelector("#detail-list").textContent;
      expect(details).toContain("HA 指标缺报");
      expect(details).toContain("不兼容原因 spki");
      expect(details).toContain("Peer halro-1");
      expect(details).not.toContain("维护；");
    } finally {
      dom.window.close();
    }
  });

  it("shows a previous-scrape metric as missing rather than fresh", async () => {
    const { dom, payloads, document } = boot();
    try {
      const up_sampled_at = new Date().toISOString();
      payloads["/api/health"].observed_at = up_sampled_at;
      payloads["/api/health"].members = [{ instance: "halro-0", up: true, role: "primary",
        sampled_at: new Date(Date.now() - 15000).toISOString(), up_sampled_at, newest_sampled_at: up_sampled_at }];
      await settled();
      expect(document.querySelector("#nodes tr").cells[1].textContent).toBe("本轮指标缺报");
      expect(document.querySelector("#topology-note").textContent).toContain("没有可据以绘制连接的 Primary");
      document.querySelector("#nodes .node-button").click();
      expect(document.querySelector("#detail-list").textContent).toContain("本轮 up 观测");
      expect(document.querySelector("#detail-list").textContent).toContain("最早指标观测");
    } finally {
      dom.window.close();
    }
  });

  it.each([-1, 1])("uses the health service clock for member freshness with a %i-hour browser skew", async hours => {
    const { dom, payloads, document } = boot();
    try {
      const sampled_at = new Date().toISOString();
      payloads["/api/health"].observed_at = sampled_at;
      payloads["/api/health"].members = [{ instance: "halro-0", up: true, role: "primary",
        sampled_at, up_sampled_at: sampled_at, newest_sampled_at: sampled_at }];
      const browserNow = dom.window.Date.now();
      dom.window.Date.now = () => browserNow + hours * 3600000;
      await settled();
      expect(document.querySelector("#nodes tr").cells[1].textContent).toBe("新鲜");
      expect(document.querySelector("#scope").textContent).toContain("采集 1/2");
    } finally {
      dom.window.close();
    }
  });

  it("keeps current cards visible while switching to sampled history and card anchors", async () => {
    const { dom, calls, document } = boot();
    try {
      await settled();
      expect(document.querySelector("#runbook-path").hidden).toBe(false);
      expect(document.querySelector("#updated").textContent).toContain("服务端观测");
      expect(document.querySelector(".runbook-context").hidden).toBe(true);
      expect(document.querySelector("#nodes-title").closest("section").hidden).toBe(false);
      expect(document.querySelector("#confirmation-evidence").textContent).toContain("估算增量 1.00");
      expect(document.querySelector("#confirmation-evidence").textContent).toContain("最旧的抓取");
      expect(document.querySelector("#confirmation-evidence").textContent).toContain("稳定任期规则抓取");
      expect(document.querySelector("#confirmation-evidence").textContent).toContain("窗口求值");
      expect(document.querySelector("#confirmation").getAttribute("href")).toBe("#confirmation-evidence-title");
      expect(document.querySelector("#chart-title").closest("section").hidden).toBe(true);
      expect(calls).not.toContain("/api/history");

      document.querySelector("#view").value = "history";
      document.querySelector("#view").dispatchEvent(new dom.window.Event("change"));
      await settled();
      expect(document.querySelector("#chart-title").closest("section").hidden).toBe(false);
      expect(document.querySelectorAll("#chart line[stroke-dasharray]")).toHaveLength(1);
      expect(document.querySelector("#chart title").textContent).toContain("Peer 会话");
      expect(document.querySelectorAll("#sampled-events tr")).toHaveLength(3);
      expect(document.querySelector("#overall .reason").textContent).toBe("sample");

      document.querySelector("#overall").click();
      expect(document.querySelector("#view").value).toBe("current");
      expect(document.querySelector("#alerts-title").closest("section").hidden).toBe(false);
    } finally {
      dom.window.close();
    }
  });

  it("does not erase current health on history failure or history on current health failure", async () => {
    const { dom, payloads, document } = boot();
    try {
      await settled();
      payloads["/api/alerts"] = null;
      document.querySelector("#refresh").click();
      await settled();
      expect(document.querySelector("#overall .reason").textContent).toBe("sample");
      expect(document.querySelector("#alerts").textContent).toContain("查询失败");

      document.querySelector("#view").value = "history";
      document.querySelector("#view").dispatchEvent(new dom.window.Event("change"));
      await settled();
      payloads["/api/history"] = null;
      document.querySelector("#refresh").click();
      await settled();
      expect(document.querySelector("#chart text").textContent).toBe("历史查询失败");
      expect(document.querySelector("#overall .reason").textContent).toBe("sample");

      payloads["/api/history"] = history;
      payloads["/api/health"] = null;
      document.querySelector("#refresh").click();
      await settled();
      expect(document.querySelector("#chart title").textContent).toContain("Peer 会话");
      expect(document.querySelector("#error").hidden).toBe(false);
    } finally {
      dom.window.close();
    }
  });

  it("refreshes current cards while a long history request is pending and limits range polling", async () => {
    const { dom, payloads, calls, document, tick } = boot();
    try {
      await settled();
      let resolveHistory;
      payloads["/api/history"] = new Promise(resolve => { resolveHistory = resolve; });
      payloads["/api/health"] = { ...payloads["/api/health"], overall: { level: "healthy", reason: "fresh" } };
      document.querySelector("#view").value = "history";
      document.querySelector("#view").dispatchEvent(new dom.window.Event("change"));
      await settled();
      expect(document.querySelector("#overall .reason").textContent).toBe("fresh");
      tick();
      await settled();
      expect(calls.filter(path => path === "/api/history")).toHaveLength(1);
      resolveHistory(history);
      await settled();
      expect(document.querySelector("#chart title").textContent).toContain("Peer 会话");
      const historyCalls = calls.filter(path => path === "/api/history").length;
      tick();
      await settled();
      expect(calls.filter(path => path === "/api/history")).toHaveLength(historyCalls);
    } finally {
      dom.window.close();
    }
  });

  it("keeps history polling on a monotonic schedule across browser clock changes", async () => {
    const { dom, calls, document, tick } = boot();
    try {
      await settled();
      document.querySelector("#view").value = "history";
      document.querySelector("#view").dispatchEvent(new dom.window.Event("change"));
      await settled();
      expect(calls.filter(path => path === "/api/history")).toHaveLength(1);

      const wallNow = dom.window.Date.now();
      dom.window.Date.now = () => wallNow + 3600000;
      tick();
      await settled();
      expect(calls.filter(path => path === "/api/history")).toHaveLength(1);

      const monoNow = dom.window.performance.now();
      Object.defineProperty(dom.window.performance, "now", { configurable: true, value: () => monoNow + 61000 });
      dom.window.Date.now = () => wallNow - 3600000;
      tick();
      await settled();
      expect(calls.filter(path => path === "/api/history")).toHaveLength(2);
    } finally {
      dom.window.close();
    }
  });

  it("expires a previously healthy card while the next health query is still pending", async () => {
    const { dom, payloads, document, tick, expire, expiryDelay } = boot();
    try {
      const sampled_at = new Date().toISOString();
      payloads["/api/health"].observed_at = sampled_at;
      payloads["/api/health"].members = [{ instance: "halro-0", up: true, role: "primary", incarnation: "inc-a",
        sampled_at, up_sampled_at: sampled_at, newest_sampled_at: sampled_at, peers: { "halro-1": true } }];
      payloads["/api/health"].member_statuses = [{ node_id: "halro-0", health_live: true, health_ready: true }];
      await settled();
      expect(document.querySelector("#overall .status").textContent).toBe("健康");
      expect(document.querySelector("#scope").textContent).toContain("采集 1/2");
      expect(document.querySelector("#nodes tr").cells[1].textContent).toBe("新鲜");
      expect(expiryDelay()).toBeGreaterThan(0);
      expect(expiryDelay()).toBeLessThanOrEqual(30001);
      payloads["/api/health"] = new Promise(() => {});
      tick();
      const now = dom.window.performance.now();
      Object.defineProperty(dom.window.performance, "now", { configurable: true, value: () => now + 31000 });
      expire();
      expect(document.querySelector("#overall .status").textContent).toBe("未知");
      expect(document.querySelector("#updated").textContent).toContain("已过期");
      expect(document.querySelector("#scope").textContent).toContain("采集覆盖未知");
      expect(document.querySelector("#scope").textContent).not.toContain("采集 1/2");
      expect(document.querySelector("#nodes tr").cells[1].textContent).toBe("缺失/陈旧");
      expect(document.querySelector("#nodes tr").cells[2].textContent).toBe("状态陈旧");
      expect(document.querySelector("#nodes tr").cells[3].textContent).toBe("未知");
      expect(document.querySelector("#nodes tr").textContent).toContain("未知（指标陈旧）");
      expect(document.querySelector("#topology").textContent).toContain("认证连接：当前未观测");
      expect(document.querySelector("#topology-note").textContent).toContain("没有可据以绘制连接的 Primary");
      expect(document.querySelector("#confirmation-evidence").textContent).toContain("确认依据未知");
    } finally {
      dom.window.close();
    }
  });

  it("expires health when a throttled browser tab regains focus", async () => {
    const { dom, document } = boot();
    try {
      await settled();
      expect(document.querySelector("#overall .status").textContent).toBe("健康");
      const now = dom.window.performance.now();
      Object.defineProperty(dom.window.performance, "now", { configurable: true, value: () => now + 31000 });
      dom.window.dispatchEvent(new dom.window.Event("focus"));
      expect(document.querySelector("#overall .status").textContent).toBe("未知");
      expect(document.querySelector("#scope").textContent).toContain("采集覆盖未知");
    } finally {
      dom.window.close();
    }
  });

  it("rejects a healthy response that arrives after its request age limit", async () => {
    const { dom, payloads, document } = boot();
    try {
      await settled();
      const health = payloads["/api/health"];
      let resolveHealth;
      payloads["/api/health"] = new Promise(resolve => { resolveHealth = resolve; });
      document.querySelector("#refresh").click();
      await settled();
      const now = dom.window.performance.now();
      Object.defineProperty(dom.window.performance, "now", { configurable: true, value: () => now + 31000 });
      resolveHealth(health);
      await settled();
      expect(document.querySelector("#overall .status").textContent).toBe("未知");
      expect(document.querySelector("#overall .reason").textContent).toBe("本次查询过期");
    } finally {
      dom.window.close();
    }
  });

  it("rejects a healthy response without a valid server observation time", async () => {
    const { dom, payloads, document } = boot();
    try {
      await settled();
      payloads["/api/health"] = { ...payloads["/api/health"], observed_at: null };
      document.querySelector("#refresh").click();
      await settled();
      expect(document.querySelector("#overall .status").textContent).toBe("未知");
      expect(document.querySelector("#overall .reason").textContent).toBe("观测时间无效");
    } finally {
      dom.window.close();
    }
  });

  it("does not let an older history range overwrite a newer result", async () => {
    const { dom, payloads, document } = boot();
    try {
      await settled();
      let resolveOld;
      payloads["/api/history"] = new Promise(resolve => { resolveOld = resolve; });
      document.querySelector("#view").value = "history";
      document.querySelector("#view").dispatchEvent(new dom.window.Event("change"));
      await settled();
      payloads["/api/history"] = { ...history, sampled_events: [] };
      document.querySelector("#refresh").click();
      await settled();
      expect(document.querySelector("#sampled-events").textContent).toContain("无可判定");
      resolveOld(history);
      await settled();
      expect(document.querySelector("#sampled-events").textContent).toContain("无可判定");
    } finally {
      dom.window.close();
    }
  });

  it("leaves a gap where a frame index cannot be represented exactly", async () => {
    const { dom, payloads, document } = boot();
    try {
      await settled();
      payloads["/api/history"] = {
        series: [{ metric: { __name__: "halro_replication_index", instance: "halro-0", kind: "durable" },
          values: [[100, "10"], [130, "9007199254740992"], [160, "12"]] }],
        sampled_events: [],
      };
      document.querySelector("#view").value = "history";
      document.querySelector("#view").dispatchEvent(new dom.window.Event("change"));
      await settled();
      expect(document.querySelectorAll("#chart circle")).toHaveLength(2);
      expect(document.querySelectorAll("#chart polyline")).toHaveLength(0);
      expect(document.querySelector("#chart").textContent).not.toContain("9007199254740992");
    } finally {
      dom.window.close();
    }
  });

  it("shows member-local live and ready observations without treating absent probes as healthy", async () => {
    const { dom, payloads, document } = boot();
    try {
      await settled();
      const sampled_at = new Date().toISOString();
      const member = { instance: "halro-0", up: true, sampled_at, up_sampled_at: sampled_at, newest_sampled_at: sampled_at, role: "primary",
        durable: 14, confirmed: 13, applied: 12 };
      payloads["/api/health"] = { ...payloads["/api/health"], observed_at: sampled_at, members: [member],
        member_statuses: [{ node_id: "halro-0", health_live: true, health_ready: false }] };
      document.querySelector("#refresh").click();
      await settled();
      let cells = document.querySelector("#nodes tr").cells;
      expect(cells[3].textContent).toBe("是");
      expect(cells[4].textContent).toBe("否");
      const localApplyGap = [...document.querySelectorAll('section[aria-labelledby="nodes-title"] th')]
        .findIndex(header => header.textContent === "确认−应用");
      expect(cells[localApplyGap].textContent).toBe("1");

      payloads["/api/health"] = { ...payloads["/api/health"],
        member_statuses: [{ node_id: "halro-0", health_probe_error: "ready_transport", health_live: true }] };
      document.querySelector("#refresh").click();
      await settled();
      cells = document.querySelector("#nodes tr").cells;
      expect(cells[2].textContent).toContain("探针异常");
      expect(cells[4].textContent).toBe("未知");

      payloads["/api/health"] = { ...payloads["/api/health"],
        member_statuses: [{ node_id: "halro-0", error: "authentication", health_live: true, health_ready: false }] };
      document.querySelector("#refresh").click();
      await settled();
      cells = document.querySelector("#nodes tr").cells;
      expect(cells[2].textContent).toContain("authentication");
      expect(cells[3].textContent).toBe("是");
      expect(cells[4].textContent).toBe("否");
    } finally {
      dom.window.close();
    }
  });

  it("links only scoped runbook paths when a controlled documentation root is configured", async () => {
    const { dom, payloads, document } = boot("https://docs.internal/");
    try {
      await settled();
      expect(document.querySelector("#runbook-path").hidden).toBe(true);
      expect(document.querySelector('[aria-labelledby="confirmation-evidence-title"] .runbook-context').href)
        .toBe("https://docs.internal/docs/runbooks/ha-operations.md");
      payloads["/api/alerts"] = [
        { name: "HalroNoPrimary", runbook: "/docs/observability/operations-runbook.md#halronoprimary" },
        { name: "Unsafe", runbook: "javascript:alert(1)" },
      ];
      document.querySelector("#refresh").click();
      await settled();
      expect(document.querySelector("#alerts tr:first-child a").href)
        .toBe("https://docs.internal/docs/observability/operations-runbook.md#halronoprimary");
      expect(document.querySelector("#alerts tr:last-child a")).toBeNull();
    } finally {
      dom.window.close();
    }
  });

  it("shows alert rule evidence and member sample provenance without treating missing rules as healthy", async () => {
    const { dom, payloads, document } = boot();
    try {
      const sampled_at = new Date().toISOString();
      payloads["/api/health"].observed_at = sampled_at;
      payloads["/api/health"].members = [{ instance: "halro-0", up: true, sampled_at, up_sampled_at: sampled_at, newest_sampled_at: sampled_at, role: "primary", incarnation: "inc-a", term: 4, durable: 12, confirmed: 11, applied: 10 }];
      payloads["/api/alerts"] = [{ name: "HalroNoPrimary", instance: "halro-0", state: "firing", severity: "critical", started: changedAt, value: "1e+00", summary: "missing" }];
      payloads["/api/alert-rule"] = { status: "available", query: "sum(halro_cluster_role) == 0", for_seconds: 120, health: "ok", last_evaluation: changedAt };
      document.querySelector("#refresh").click();
      await settled();
      document.querySelector("#alerts button").click();
      await settled();
      const detail = document.querySelector("#alert-detail");
      expect(detail.open).toBe(true);
      expect(detail.textContent).toContain("sum(halro_cluster_role) == 0");
      expect(detail.textContent).toContain("120 秒");
      expect(detail.textContent).toContain("durable/confirmed/applied 12/11/10");
      expect(detail.textContent).toContain("不是同一时刻的原始样本");

      payloads["/api/alert-rule"] = null;
      document.querySelector("#alerts button").click();
      await settled();
      expect(detail.textContent).toContain("规则查询失败");
      expect(detail.textContent).not.toContain("sum(halro_cluster_role) == 0");
    } finally {
      dom.window.close();
    }
  });

  it("labels fresh and unverifiable extra member evidence separately", async () => {
    const { dom, payloads, document } = boot();
    try {
      payloads["/api/health"].unexpected_members = [
        { instance: "rogue-live", observed: true, sampled_at: changedAt },
        { instance: "rogue-old", observed: false, sampled_at: previousAt },
        { instance: "", identity_missing: true, observed: false, sampled_at: changedAt },
      ];
      document.querySelector("#refresh").click();
      await settled();
      expect(document.querySelector("#scope").textContent).toContain("异常来源 3（未证实 2）");
      expect(document.querySelector("#unexpected-members").textContent).toContain("rogue-live 近期已观测");
      expect(document.querySelector("#unexpected-members").textContent).toContain("rogue-old 未证实/陈旧");
      expect(document.querySelector("#unexpected-members").textContent).toContain("缺少 instance 标签 身份不可核对");
      payloads["/api/health"] = null;
      document.querySelector("#refresh").click();
      await settled();
      expect(document.querySelector("#unexpected-members").hidden).toBe(true);
    } finally {
      dom.window.close();
    }
  });
});
