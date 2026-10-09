// Offline tests for the Qoder panel's model section.
//
// Same approach as workbuddy/panel.test.js, but kept small: the panel's model
// state is a set of module-local `let`s, so the tests drive the real
// loadModels()/renderModels() path with a stubbed api() instead of string-
// matching the HTML file. Every assertion below runs the shipped code.
//
// Run with: node --test panel.test.js
const test = require("node:test");
const assert = require("node:assert/strict");
const fs = require("node:fs");
const path = require("node:path");
const vm = require("node:vm");

function fakeElement() {
  // querySelector must return an element (toast() writes into the nodes it
  // builds), and it must be stable per selector so a test can read it back.
  const queries = new Map();
  const element = {
    hidden: false,
    className: "",
    textContent: "",
    innerHTML: "",
    value: "",
    disabled: false,
    style: {},
    dataset: {},
    children: [],
    parentNode: null,
    classList: {
      values: new Set(),
      add(value) { this.values.add(value); },
      remove(value) { this.values.delete(value); },
      contains(value) { return this.values.has(value); },
    },
    appendChild(child) {
      child.parentNode = this;
      this.children.push(child);
      return child;
    },
    querySelector(selector) {
      if (!queries.has(selector)) queries.set(selector, fakeElement());
      return queries.get(selector);
    },
    querySelectorAll() { return []; },
    addEventListener() {},
    setAttribute() {},
    removeAttribute() {},
    focus() {},
    closest() { return null; },
    remove() {
      if (!this.parentNode) return;
      const index = this.parentNode.children.indexOf(this);
      if (index >= 0) this.parentNode.children.splice(index, 1);
      this.parentNode = null;
    },
  };
  Object.defineProperty(element, "firstChild", {
    get() { return element.children[0] || null; },
  });
  return element;
}

// loadPanel runs the panel's last <script> block (the app; the first block is
// only the pre-paint theme sync) in a vm with the DOM surface this panel
// actually touches. No key is seeded, so the bottom-of-script bootstrap takes
// the showAuth() branch and issues no network call.
function loadPanel(overrides = {}) {
  const html = fs.readFileSync(path.join(__dirname, "panel.html"), "utf8");
  const scripts = [...html.matchAll(/<script>([\s\S]*?)<\/script>/g)];
  const source = scripts.at(-1)[1];
  const elements = new Map();
  const storage = new Map(overrides.sessionEntries || []);
  const document = {
    documentElement: fakeElement(),
    getElementById(id) {
      if (!elements.has(id)) elements.set(id, fakeElement());
      return elements.get(id);
    },
    createElement() { return fakeElement(); },
    querySelectorAll() { return []; },
    addEventListener() {},
  };
  const context = {
    console,
    document,
    sessionStorage: overrides.sessionStorage || {
      getItem(key) { return storage.has(key) ? storage.get(key) : null; },
      setItem(key, value) { storage.set(key, String(value)); },
      removeItem(key) { storage.delete(key); },
    },
    localStorage: overrides.localStorage || { getItem() { return null; }, setItem() {} },
    location: overrides.location || { href: "http://localhost/panel", search: "", pathname: "/panel", hash: "", host: "localhost" },
    history: overrides.history || { replaceState() {} },
    navigator: { userAgent: "node-test" },
    URL,
    URLSearchParams,
    TextEncoder,
    TextDecoder,
    Uint8Array,
    btoa(value) { return Buffer.from(value, "binary").toString("base64"); },
    atob(value) { return Buffer.from(value, "base64").toString("binary"); },
    requestAnimationFrame(fn) { fn(); },
    setTimeout() { return 1; },
    clearTimeout() {},
    fetch: overrides.fetch || (async () => { throw new Error("unexpected fetch"); }),
  };
  context.window = context;
  context.self = context;
  context.top = context;
  vm.createContext(context);
  vm.runInContext(source, context, { filename: "panel.html" });
  return { context, document, elements, storage };
}

function panelState(panel, expression) {
  return vm.runInContext(expression, panel.context);
}

// A two-region catalog exercising every documented cell shape:
//   CN    — present, 倍率 0.5, 思考 supported with three levels, 200K default tier
//   Intl  — absent (the region has a catalog but not this model → "—")
// and a second row whose CN side is not_loaded (no account in that region).
const CATALOG = {
  source: "dynamic",
  persistent: true,
  models: [
    { id: "auto", name: "Auto", displayName: "Auto", hidden: false, custom: false, coolingAccounts: 2 },
    { id: "qmodel", name: "Q", displayName: "Q", hidden: false, custom: true, coolingAccounts: 0 },
  ],
  region_models: [
    {
      id: "auto",
      name: "Auto",
      cn: {
        status: "present",
        present: true,
        price_factor: 0.5,
        context_length: 200000,
        context_tiers: [
          { label: "200K", tokens: 200000, is_default: true },
          { label: "400K", tokens: 400000 },
        ],
        thinking: { status: "supported", default: "high", can_disable: true, levels: ["low", "medium", "high"] },
      },
      intl: { status: "absent", present: false },
    },
    {
      id: "qmodel",
      name: "Q",
      cn: { status: "not_loaded", present: null },
      intl: {
        status: "present",
        present: true,
        price_factor: 0,
        context_options: [128000],
        thinking: { status: "unknown", levels: [] },
      },
    },
  ],
  region_status: {
    cn: { loaded: true, status: "ok", source: "upstream", fetched_at: "2026-10-09T10:00:00Z" },
    intl: { loaded: false, status: "empty", source: "none" },
  },
};

function loadPanelWithCatalog(catalog = CATALOG) {
  const panel = loadPanel();
  // The stub backend remembers model_order the way the real one does: the
  // catalog it serves is re-ordered by whatever was last PATCHed. Without that,
  // the reload saveModelOrder() performs at the end would silently undo the
  // order under test and hide order bugs.
  const state = { order: null, patches: [] };
  panel.state = state;
  const served = () => {
    const models = state.order
      ? state.order.map(id => catalog.models.find(m => m.id === id)).filter(Boolean)
      : catalog.models;
    return { ...catalog, models };
  };
  panel.context.api = async () => served();
  panel.context.managementAPI = async (p, o) => {
    const body = JSON.parse(o.body);
    state.patches.push([p, body]);
    if (body.model_order) state.order = body.model_order;
    return { ok: true };
  };
  return panel;
}

// ---------------------------------------------------------------- 1. 排序入口

test("model rows no longer render the up/down buttons but moveModel survives", async () => {
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  const html = panel.elements.get("modelList").innerHTML;

  // The ↑ / ↓ buttons are gone; ordering is drag-and-drop plus keyboard on the handle.
  assert.doesNotMatch(html, /aria-label="上移/);
  assert.doesNotMatch(html, /aria-label="下移/);
  assert.doesNotMatch(html, /moveModel\(/, "no inline moveModel() handler may remain");
  assert.doesNotMatch(html, /onclick=/, "event binding must use data-* attributes");
  assert.match(html, /data-model-action="drag"/);
  assert.match(html, /⠿/);

  // moveModel is still the keyboard path's implementation, so deleting the
  // buttons must not delete the function.
  assert.equal(typeof panel.context.moveModel, "function");
  const calls = [];
  panel.context.api = async (p) => { calls.push(p); return CATALOG; };
  panel.context.managementAPI = async (p) => { calls.push(p); return { ok: true }; };
  await panel.context.moveModel("auto", 1);
  assert.ok(!calls.includes("/models/action"), "keyboard ordering must not use the legacy move endpoint");
});

test("model ordering never calls the legacy move endpoint", async () => {
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  const calls = [];
  panel.context.api = async (p) => { calls.push(p); return CATALOG; };
  panel.context.managementAPI = async (p, o) => { calls.push([p, JSON.parse(o.body)]); return { ok: true }; };
  await panel.context.saveModelOrder(["qmodel", "auto"]);
  assert.ok(
    calls.every(entry => !(typeof entry === "string" && entry === "/models/action")),
    "ordering must go through the plugin config PATCH, not /models/action",
  );
});

// --------------------------------------------------------- 2. 分区对照表语义

test("region table renders CN and Intl columns with per-region facts", async () => {
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  const html = panel.elements.get("modelList").innerHTML;

  assert.match(html, /model-table/);
  // Five-column header: 排序 / 模型 / CN / Intl / 操作.
  assert.match(html, /<span>排序<\/span><span>模型<\/span><span>CN<\/span><span>Intl<\/span><span>操作<\/span>/);
  assert.match(html, /倍率 0\.5x/);
  assert.match(html, /思考 默认 high（low\/medium\/high）/);
  assert.doesNotMatch(html, /Global/, "Qoder's regions are cn/intl, not global");
});

test("absent renders an em dash and not_loaded renders 未加载, and they never conflate", async () => {
  const panel = loadPanel();
  const absent = panel.context.regionCell({ intl: { status: "absent", present: false } }, "intl");
  const notLoaded = panel.context.regionCell({ cn: { status: "not_loaded", present: null } }, "cn");
  const missing = panel.context.regionCell({}, "cn");
  assert.equal(absent, "—");
  assert.equal(notLoaded, "未加载");
  assert.equal(missing, "未加载");
  assert.notEqual(absent, notLoaded, "an absent model and an unloaded region are different facts");

  // A region that is loaded but does not contain the model must not fall through
  // to the "present" branch just because status is unrecognised.
  assert.equal(panel.context.regionCell({ cn: { status: "present", present: false } }, "cn"), "—");
  assert.equal(panel.context.regionCell({ cn: { present: true } }, "cn").includes("倍率"), true);
  // Contradictory/partial shapes: present===null means "the region never
  // reported", so it must read 未加载 even when status claims it was loaded —
  // rendering 倍率/上下文 over a null present would invent facts.
  assert.equal(panel.context.regionCell({ cn: { status: "present", present: null } }, "cn"), "未加载");
  assert.equal(panel.context.regionCell({ cn: { present: null, price_factor: 0.5 } }, "cn"), "未加载");
  assert.equal(panel.context.regionCell({ cn: { status: "not_loaded", present: true } }, "cn"), "未加载");
  assert.doesNotMatch(panel.context.regionCell({ cn: { present: null, price_factor: 0.5 } }, "cn"), /倍率/);
});

// --------------------------------------------------------------- 3. 倍率统一

test("price factor renders one unified multiplier string", () => {
  const { context } = loadPanel();
  const credit = factor => {
    const html = context.regionCell({ cn: { status: "present", present: true, price_factor: factor } }, "cn");
    const match = html.match(/倍率 ([^<]*)<\/span>/);
    assert.ok(match, `no 倍率 fact in ${html}`);
    return match[1];
  };
  assert.equal(credit(0.5), "0.5x");
  assert.equal(credit(0.1), "0.1x");
  assert.equal(credit(2.2), "2.2x");
  assert.equal(credit(1), "1x");
  // Free keeps its meaning instead of collapsing to "0x".
  assert.equal(credit(0), "免费 (0x)");
  // No reported value must say so rather than faking a free tier — Number(null)
  // is 0, so a missing factor must be checked before the numeric branch.
  assert.equal(credit(null), "未上报");
  assert.equal(credit(undefined), "未上报");
  assert.equal(credit(""), "未上报");
  assert.equal(credit("n/a"), "未上报");
  assert.equal(credit(NaN), "未上报");
});

// ------------------------------------------------------------------ 4. 思考

test("thinking reports the default level, unknown, unsupported and off_only", () => {
  const { context } = loadPanel();
  const think = thinking => {
    const html = context.regionCell({ cn: { status: "present", present: true, thinking } }, "cn");
    const match = html.match(/思考 ([^<]*)<\/span>/);
    assert.ok(match, `no 思考 fact in ${html}`);
    return match[1];
  };
  const supported = { status: "supported", default: "high", can_disable: true, levels: ["low", "medium", "high", "xhigh", "max"] };
  assert.equal(think(supported), "默认 high（low/medium/high/xhigh/max）");
  // can_disable must not change the wording on its own.
  assert.equal(think({ ...supported, can_disable: false }), "默认 high（low/medium/high/xhigh/max）");
  assert.equal(think({ status: "unknown", levels: [] }), "未上报");
  assert.equal(think({}), "未上报");
  assert.equal(think(undefined), "未上报");
  assert.equal(think({ status: "unsupported", levels: [] }), "不支持");
  assert.equal(think({ status: "off_only", levels: [] }), "仅可关闭");
  assert.equal(think({ status: "off_only", levels: ["high"] }), "仅可关闭（high）");
  // supported without a default still lists the levels it accepts.
  assert.equal(think({ status: "supported", levels: ["low", "high"] }), "low / high");
});

// ------------------------------------------------------- 5. 上下文默认档高亮

test("the current context tier is bolded, for tiers and both legacy key shapes", () => {
  const { context } = loadPanel();
  const line = facts => {
    const html = context.regionCell({ cn: { status: "present", present: true, ...facts } }, "cn");
    const match = html.match(/上下文 ([\s\S]*?)<\/span><\/div>$/);
    assert.ok(match, `no 上下文 fact in ${html}`);
    return match[1];
  };
  // context_tiers is the current key: objects with label/tokens/is_default.
  const tiers = line({
    context_tiers: [
      { label: "200K", tokens: 200000, is_default: true },
      { label: "400K", tokens: 400000 },
      { label: "1M", tokens: 1000000 },
    ],
  });
  assert.equal(
    tiers,
    "<b class=\"ctx-default\" title=\"当前默认档位\">200K</b> / 400K / 1M",
  );
  // context_options is the legacy key; supported_context_lengths the older one.
  const options = line({ context_options: [300000, 600000, 1000000], default_context_length: 600000 });
  assert.equal(options, "300K / <b class=\"ctx-default\" title=\"当前默认档位\">600K</b> / 1M");
  const legacy = line({ supported_context_lengths: [300000, 600000], default_context_length: 300000 });
  assert.equal(legacy, "<b class=\"ctx-default\" title=\"当前默认档位\">300K</b> / 600K");
  // context_tiers wins when both are present.
  assert.equal(line({ context_tiers: [{ label: "128K", tokens: 128000, is_default: true }], context_options: [300000] }), "<b class=\"ctx-default\" title=\"当前默认档位\">128K</b>");
  // Without a default tier the list renders exactly as before — nothing bolded.
  const noDefault = line({ context_options: [300000, 600000] });
  assert.equal(noDefault, "300K / 600K");
  assert.doesNotMatch(noDefault, /ctx-default/);
  assert.doesNotMatch(line({ context_options: [300000], default_context_length: 0 }), /ctx-default/);
  // A default that is not one of the offered tiers must not invent a tier.
  const foreign = line({ context_options: [300000, 600000], default_context_length: 999999 });
  assert.equal(foreign, "300K / 600K");
  assert.doesNotMatch(foreign, /999K|ctx-default/);
  // Single-tier fallback when no option list is reported.
  assert.equal(line({ context_length: 128000 }), "128K");
  assert.equal(line({}), "未上报");
});

// -------------------------------------------------- 6. 操作列 nowrap / 宽度

test("model action column never wraps and keeps a content-sized track", () => {
  const html = fs.readFileSync(path.join(__dirname, "panel.html"), "utf8");
  const rule = selector => {
    const escaped = selector.replace(/[.*+?^${}()|[\]\\]/g, "\\$&");
    // A rule may share a line with its predecessor, so accept `}` or a line
    // break as the left boundary; that also keeps `.ftag` from matching the
    // longer `.model-actions .ftag` selector.
    const match = html.match(new RegExp("(?:^|[}\\n])" + escaped + "\\{([^}]*)\\}"));
    assert.ok(match, `missing CSS rule for ${selector}`);
    return match[1];
  };
  assert.match(rule(".model-actions"), /white-space:nowrap/);
  assert.match(rule(".model-actions"), /flex-wrap:nowrap/);
  // Buttons and badges inside the column inherit the no-wrap guarantee.
  assert.match(rule(".model-actions>*"), /white-space:nowrap/);
  assert.match(rule(".badge"), /white-space:nowrap/);
  assert.match(rule(".ftag"), /white-space:nowrap/);
  // .ctx-default is a descendant of .realm-facts (same shape as WorkBuddy's),
  // so the rule lookup has to spell out the full selector.
  assert.match(rule(".realm-facts .ctx-default"), /font-weight:700/);
  assert.match(rule(".realm-facts .ctx-default"), /color:var\(--acc\)/);

  const tracks = rule(".model-head,.model-row").match(/grid-template-columns:([^;]+);/);
  assert.ok(tracks, "grid-template-columns must be declared for the model table");
  const columns = tracks[1].trim().split(/\s+(?![^()]*\))/);
  assert.equal(columns.length, 5, `expected 5 tracks, got ${columns.join(" | ")}`);
  const last = columns[4];
  assert.doesNotMatch(last, /1fr|fit-content/);
  // 290px is the shared panel layout's floor, kept so Qoder matches WorkBuddy.
  // It is not a guess for Qoder either: the widest action set Qoder can emit
  // (移除/恢复 + 自定义 + 冷却 999 + 已隐藏) measures 235.4px in headless
  // Chrome, so the floor sits comfortably above it. `max-content` lets an
  // unusually wide row grow instead of clipping, while ordinary rows all share
  // one width and stay column-aligned.
  const WIDEST_ACTION_SET_PX = 236;
  const floor = last.match(/^minmax\(\s*(\d+)px\s*,\s*max-content\s*\)$/);
  assert.ok(floor, `last track must be minmax(<px>,max-content), got ${last}`);
  assert.ok(
    Number(floor[1]) >= WIDEST_ACTION_SET_PX,
    `action track floor ${floor[1]}px is below the widest action set (${WIDEST_ACTION_SET_PX}px)`,
  );
  assert.match(columns[1], /^minmax\(\s*\d+px/);
  const minWidth = Number((rule(".model-table").match(/min-width:(\d+)px/) || [])[1] || 0);
  const padding = Number((rule(".model-head,.model-row").match(/padding:\d+px (\d+)px/) || [])[1] || 0);
  const floorSum = columns.reduce((sum, track) => {
    const value = track.match(/(\d+)px/);
    return sum + (value ? Number(value[1]) : 0);
  }, 0);
  assert.ok(minWidth >= floorSum, `.model-table min-width ${minWidth}px cannot fit the track floors (${floorSum}px)`);
  assert.ok(padding > 0, "row padding must be declared for the track sum to be meaningful");
});

// ---------------------------------------------- 7. 排序持久化只 PATCH 一个字段

test("saveModelOrder patches only model_order", async () => {
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  const saved = await panel.context.saveModelOrder(["qmodel", "auto"]);
  assert.equal(saved, true);
  assert.deepEqual(panel.state.patches, [["/plugins/qoder/config", { model_order: ["qmodel", "auto"] }]]);
  assert.deepEqual(
    Object.keys(panel.state.patches[0][1]),
    ["model_order"],
    "model_order is the only field this save may touch",
  );
  assert.equal(panelState(panel, "JSON.stringify(lastModels.map(m=>m.id))"), JSON.stringify(["qmodel", "auto"]));
});

test("saveModelOrder rolls back the local order when the response reports an error", async () => {
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  const toasts = [];
  panel.context.toast = (title, kind, detail) => { toasts.push([title, kind, detail]); };
  panel.context.managementAPI = async () => ({ error: "rejected" });
  const saved = await panel.context.saveModelOrder(["qmodel", "auto"]);
  assert.equal(saved, false);
  assert.equal(panelState(panel, "JSON.stringify(lastModels.map(m=>m.id))"), JSON.stringify(["auto", "qmodel"]));
  assert.equal(toasts.length, 1);
  assert.equal(toasts[0][1], "err");
});

test("saveModelOrder refuses a partial or unchanged order", async () => {
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  assert.equal(await panel.context.saveModelOrder(["auto"]), false, "a partial list must not be saved");
  assert.equal(await panel.context.saveModelOrder(["auto", "qmodel"]), false, "an unchanged order is a no-op");
  assert.equal(await panel.context.saveModelOrder(["auto", "ghost"]), false, "unknown ids must not be saved");
  assert.equal(panel.state.patches.length, 0);
});

test("a save in flight blocks a concurrent save and a stale catalog reload", async () => {
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  let release;
  const passthrough = panel.context.managementAPI;
  panel.context.managementAPI = async (p, o) => {
    await new Promise(resolve => { release = resolve; });
    return passthrough(p, o);
  };
  panel.context.toast = () => {};
  const inFlight = panel.context.saveModelOrder(["qmodel", "auto"]);
  // While the PATCH is in flight the local order is already the new one, and any
  // other writer (a second drag, or a late loadModels response) must be refused
  // rather than racing the pending write.
  assert.equal(panelState(panel, "modelOrderSaving"), true);
  assert.equal(await panel.context.saveModelOrder(["auto", "qmodel"]), false);
  assert.equal(await panel.context.loadModels(false), false);
  release();
  assert.equal(await inFlight, true);
  assert.equal(panelState(panel, "modelOrderSaving"), false);
  assert.equal(panelState(panel, "JSON.stringify(lastModels.map(m=>m.id))"), JSON.stringify(["qmodel", "auto"]));
});

test("moveModel swaps neighbours and persists through the plugin config", async () => {
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  panel.context.toast = () => {};
  assert.equal(await panel.context.moveModel("qmodel", -1), true);
  assert.deepEqual(panel.state.patches, [["/plugins/qoder/config", { model_order: ["qmodel", "auto"] }]]);
  // Off the ends is a no-op, not an error.
  assert.equal(await panel.context.moveModel("qmodel", -1), false);
});

// ------------------------------------------------------------- 8. 降级与安全

test("a catalog without region_models degrades to the flat table instead of throwing", async () => {
  const panel = loadPanel();
  panel.context.api = async () => ({
    source: "dynamic",
    persistent: true,
    models: [
      { id: "auto", name: "Auto", hidden: false, custom: false, coolingAccounts: 1 },
      { id: "gone", name: "Gone", hidden: true },
    ],
  });
  assert.equal(await panel.context.loadModels(false), true);
  const html = panel.elements.get("modelList").innerHTML;
  assert.doesNotMatch(html, /未加载/, "no region catalog means no region cells at all");
  assert.match(html, /model-table-flat/);
  assert.match(html, /auto/);
  assert.match(html, /冷却 1/, "the Qoder-specific cooling badge survives");
  assert.match(html, /已隐藏/);
  assert.match(html, /data-model-action="restore" data-model-id="gone"/);
  const hint = panel.elements.get("modelHint").textContent;
  assert.match(hint, /排序保存到插件配置/);
  assert.doesNotMatch(hint, /仅保存在内存中/);
});

test("models only present in region_models are still listed exactly once", async () => {
  const panel = loadPanelWithCatalog({
    ...CATALOG,
    models: [{ id: "auto", name: "Auto", hidden: false }],
    region_models: [
      CATALOG.region_models[0],
      { id: "intl-only", name: "Intl only", cn: { status: "not_loaded", present: null }, intl: { status: "present", present: true, price_factor: 1.4 } },
    ],
  });
  await panel.context.loadModels(false);
  const html = panel.elements.get("modelList").innerHTML;
  assert.match(html, /intl-only/);
  assert.match(html, /1\.4x/);
  // Count rows, not data-model-id attributes: each row carries the id on the
  // row, its drag handle and its action button, so only rows indicate duplication.
  assert.equal((html.match(/class="model-row"/g) || []).length, 2, "a model in both catalogs must not be duplicated");
  assert.equal((html.match(/>auto</g) || []).length, 1);
});

test("region facts escape dynamic values and never use inline handlers", async () => {
  const panel = loadPanel();
  const hostile = "<img src=x onerror=1>";
  const cell = panel.context.regionCell({
    cn: {
      status: "present",
      present: true,
      price_factor: 0.34,
      thinking: { status: "supported", default: hostile, levels: [hostile] },
      context_tiers: [{ label: hostile, tokens: 300000, is_default: true }],
    },
  }, "cn");
  assert.doesNotMatch(cell, /<img/);
  assert.match(cell, /&lt;/);
  assert.doesNotMatch(cell, /onclick=/);

  const panel2 = loadPanel();
  panel2.context.api = async () => ({
    source: "dynamic",
    models: [{ id: "model-\"quote\"", name: "M", hidden: false }],
    region_models: [{
      id: "model-\"quote\"",
      cn: {
        status: "present",
        present: true,
        price_factor: 0.34,
        thinking: { status: "supported", default: "high", levels: ["low", "high"] },
        context_tiers: [{ label: "600K", tokens: 600000, is_default: true }],
      },
      intl: { status: "absent", present: false },
    }],
  });
  await panel2.context.loadModels(false);
  const rendered = panel2.elements.get("modelList").innerHTML;
  assert.match(rendered, /倍率 0\.34x/);
  assert.match(rendered, /思考 默认 high（low\/high）/);
  assert.match(rendered, /<b class="ctx-default" title="当前默认档位">600K<\/b>/);
  assert.match(rendered, /data-model-action="toggle"/);
  assert.doesNotMatch(rendered, /onclick=/);
  assert.doesNotMatch(rendered, /aria-label="上移/);
});

test("region status is reported in the hint when the backend sends it", async () => {
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  const hint = panel.elements.get("modelHint").textContent;
  assert.match(hint, /CN：已加载/);
  assert.match(hint, /Intl：未加载/);

  // An older backend without region_status must not gain a fabricated one.
  const legacy = loadPanel();
  legacy.context.api = async () => ({ source: "dynamic", models: [{ id: "auto", hidden: false }], region_models: CATALOG.region_models });
  await legacy.context.loadModels(false);
  assert.doesNotMatch(legacy.elements.get("modelHint").textContent, /：未加载/);
});

// ------------------------------------------- 9. dataset 绑定 / 拖拽 / 键盘排序

// domNode is a minimal DOM node with a real event registry, so the panel's
// bindModelActions() can be driven for real: the earlier tests only prove what
// HTML is emitted, not that the data-* wiring on it works.
// settle drains the microtask queue so a fire-and-forget handler chain
// (moveModel -> saveModelOrder -> managementAPI -> loadModels) finishes before
// the assertions read the state it writes.
async function settle() {
  for (let i = 0; i < 20; i += 1) await new Promise(resolve => setImmediate(resolve));
}

function domNode(dataset) {
  const handlers = new Map();
  return {
    dataset,
    _bound: false,
    disabled: false,
    draggable: false,
    addEventListener(type, fn) {
      if (!handlers.has(type)) handlers.set(type, []);
      handlers.get(type).push(fn);
    },
    fire(type, event = {}) {
      for (const fn of handlers.get(type) || []) fn(event);
      return event;
    },
    hasHandler(type) { return handlers.has(type); },
  };
}

// wireRenderedRows reads the rows back out of the rendered table and builds the
// node graph bindModelActions() expects: one button per data-model-action, each
// pointing at its owning .model-row via closest().
function wireRenderedRows(panel, html) {
  // The row tag carries an extra class in the degraded table
  // (`class="model-row model-row-flat"`), so split before the attribute's
  // closing quote rather than on it.
  const chunks = html.split('<div class="model-row').slice(1);
  const nodes = [];
  for (const chunk of chunks) {
    const rowId = (chunk.match(/data-model-id="([^"]*)"/) || [])[1];
    const row = domNode({ modelId: rowId });
    nodes.push(row);
    for (const match of chunk.matchAll(/data-model-action="([^"]*)"/g)) {
      const button = domNode({ modelAction: match[1], modelId: rowId });
      button.closest = selector => (selector === ".model-row" ? row : null);
      nodes.push(button);
    }
  }
  panel.document.querySelectorAll = () => nodes;
  return nodes;
}

function fireDrag(panel, nodes, draggedId, targetId, dataTransfer) {
  const handle = nodes.find(n => n.dataset.modelAction === "drag" && n.dataset.modelId === draggedId);
  const target = nodes.find(n => n.dataset.modelId === targetId);
  assert.ok(handle && target, `missing drag handle for ${draggedId} or row ${targetId}`);
  handle.fire("dragstart", { dataTransfer, preventDefault() {} });
  target.fire("drop", { preventDefault() {} });
}

test("rendered rows bind through data-* attributes and drag reorders persistently", async () => {
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  panel.context.toast = () => {};
  const nodes = wireRenderedRows(panel, panel.elements.get("modelList").innerHTML);
  panel.context.bindModelActions();

  // Every rendered control is bound, and nothing looks up an inline handler.
  const drag = nodes.filter(n => n.dataset.modelAction === "drag");
  assert.equal(drag.length, 2);
  assert.ok(drag.every(node => node.hasHandler("dragstart") && node.hasHandler("keydown")));
  assert.ok(nodes.filter(n => n.dataset.modelAction === "toggle").every(node => node.hasHandler("click")));

  // Drag the second row onto the first: the drop must persist, not just repaint.
  const transfer = { effectAllowed: "", setData(t, v) { this.value = v; } };
  fireDrag(panel, nodes, "qmodel", "auto", transfer);
  assert.equal(transfer.value, "qmodel", "the drag payload carries the model id");
  await settle();
  assert.deepEqual(panel.state.patches, [["/plugins/qoder/config", { model_order: ["qmodel", "auto"] }]]);
  assert.equal(panelState(panel, "JSON.stringify(lastModels.map(m=>m.id))"), JSON.stringify(["qmodel", "auto"]));

  // Re-bind against the repainted table and reorder by keyboard instead.
  const next = wireRenderedRows(panel, panel.elements.get("modelList").innerHTML);
  panel.context.bindModelActions();
  const handle = next.find(n => n.dataset.modelAction === "drag" && n.dataset.modelId === "auto");
  assert.ok(handle, "the repainted table must still expose a drag handle for auto");
  let prevented = false;
  handle.fire("keydown", { key: "ArrowUp", preventDefault() { prevented = true; } });
  assert.equal(prevented, true, "the handle must claim the arrow key so the page does not scroll");
  await settle();
  assert.equal(panel.state.patches.length, 2);
  assert.deepEqual(panel.state.patches[1], ["/plugins/qoder/config", { model_order: ["auto", "qmodel"] }]);

  // Arrow keys are the only ordering gesture left, so non-arrow keys stay inert.
  const before = panelState(panel, "modelOrderVersion");
  handle.fire("keydown", { key: "ArrowLeft", preventDefault() {} });
  await settle();
  assert.equal(panelState(panel, "modelOrderVersion"), before);
  assert.equal(panel.state.patches.length, 2);
});

test("a hidden row has no drag handle and its click handler restores it", async () => {
  const panel = loadPanel();
  panel.context.api = async path => {
    if (path === "/models/action") return { overlay: { hide: [] } };
    return { source: "dynamic", models: [{ id: "gone", name: "Gone", hidden: true, custom: false }] };
  };
  const patched = [];
  panel.context.managementAPI = async (p, o) => { patched.push([p, JSON.parse(o.body)]); return { ok: true }; };
  panel.context.toast = () => {};
  await panel.context.loadModels(false);
  const nodes = wireRenderedRows(panel, panel.elements.get("modelList").innerHTML);
  panel.context.bindModelActions();
  assert.equal(nodes.filter(n => n.dataset.modelAction === "drag").length, 0, "a hidden model is not orderable");
  const restore = nodes.find(n => n.dataset.modelAction === "restore");
  assert.ok(restore, "a hidden model must offer 恢复");
  restore.fire("click", {});
  await settle();
  assert.deepEqual(patched, [["/plugins/qoder/config", { hidden_models: [] }]]);
});

test("hiding a model patches hidden_models and never model_order", async () => {
  const panel = loadPanel();
  panel.context.api = async path => {
    if (path === "/models/action") return { overlay: { hide: ["auto"] } };
    return { source: "dynamic", models: [{ id: "auto", name: "Auto", hidden: false, custom: false }] };
  };
  const patched = [];
  panel.context.managementAPI = async (p, o) => { patched.push([p, JSON.parse(o.body)]); return { ok: true }; };
  panel.context.toast = () => {};
  await panel.context.loadModels(false);
  await panel.context.toggleModel("auto");
  await settle();
  assert.deepEqual(patched, [["/plugins/qoder/config", { hidden_models: ["auto"] }]]);
  assert.ok(patched.every(entry => !("model_order" in entry[1])), "hiding must not rewrite the order");
});

test("a catalog response that lands after a save cannot overwrite the new order", async () => {
  const panel = loadPanel();
  const preSave = {
    source: "dynamic",
    models: [{ id: "auto", name: "Auto", hidden: false }, { id: "qmodel", name: "Q", hidden: false }],
    region_models: CATALOG.region_models,
  };
  const postSave = { ...preSave, models: [preSave.models[1], preSave.models[0]] };
  // Read #1 paints the catalog. Read #2 is the stale one: it starts before the
  // save and holds the pre-save order. Later reads answer with the saved order.
  let calls = 0;
  let releaseStale;
  const gate = new Promise(resolve => { releaseStale = resolve; });
  panel.context.api = async () => {
    calls += 1;
    if (calls === 1) return preSave;
    if (calls === 2) { await gate; return preSave; }
    if (calls === 3) return postSave;
    // A later independent read advertises yet another order; the guard must not
    // be a blanket "reloads never apply".
    return preSave;
  };
  await panel.context.loadModels(false);
  assert.equal(panelState(panel, "JSON.stringify(lastModels.map(m=>m.id))"), JSON.stringify(["auto", "qmodel"]));

  panel.context.toast = () => {};
  const patched = [];
  panel.context.managementAPI = async (p, o) => { patched.push(JSON.parse(o.body)); return { ok: true }; };

  const staleRead = panel.context.loadModels(false);
  await settle();
  assert.equal(calls, 2, "the stale read must be outstanding when the save happens");

  await panel.context.saveModelOrder(["qmodel", "auto"]);
  assert.deepEqual(patched, [{ model_order: ["qmodel", "auto"] }]);
  assert.equal(panelState(panel, "JSON.stringify(lastModels.map(m=>m.id))"), JSON.stringify(["qmodel", "auto"]));

  // Now let the stale read land. Applying it would silently resurrect the order
  // the operator just replaced, so the version guard must drop it.
  releaseStale();
  assert.equal(await staleRead, false);
  assert.equal(
    panelState(panel, "JSON.stringify(lastModels.map(m=>m.id))"),
    JSON.stringify(["qmodel", "auto"]),
    "the late pre-save catalog must not resurrect the old order",
  );
  assert.equal(
    panelState(panel, "JSON.stringify(lastRegionModels.map(m=>m.id))"),
    JSON.stringify(["auto", "qmodel"]),
    "the dropped response must not touch the region table either",
  );

  // The guard keys on a save, not on reads in general: a fresh read still applies.
  assert.equal(await panel.context.loadModels(false), true);
  assert.equal(panelState(panel, "JSON.stringify(lastModels.map(m=>m.id))"), JSON.stringify(["auto", "qmodel"]));
  assert.equal(panelState(panel, "modelOrderVersion"), 2, "one bump on save, one on completion");
});

test("a save in flight makes an arriving catalog reload a no-op", async () => {
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  panel.context.toast = () => {};
  let release;
  const passthrough = panel.context.managementAPI;
  panel.context.managementAPI = async (p, o) => {
    await new Promise(resolve => { release = resolve; });
    return passthrough(p, o);
  };
  const save = panel.context.saveModelOrder(["qmodel", "auto"]);
  await settle();
  assert.equal(panelState(panel, "modelOrderSaving"), true);
  // A reload landing while the PATCH is in flight must be dropped, not merged.
  const readsBefore = panelState(panel, "JSON.stringify(lastModels.map(m=>m.id))");
  assert.equal(await panel.context.loadModels(false), false);
  assert.equal(panelState(panel, "JSON.stringify(lastModels.map(m=>m.id))"), readsBefore);
  release();
  await save;
  assert.deepEqual(panel.state.patches, [["/plugins/qoder/config", { model_order: ["qmodel", "auto"] }]]);
});

test("every row occupies the same five grid tracks, including hidden rows", async () => {
  // A hidden model has no drag handle (it is not orderable), but dropping the
  // handle outright shifted that row's remaining cells one column to the left,
  // so its facts no longer lined up with every other row. A placeholder keeps
  // the sorting track occupied instead.
  const panel = loadPanelWithCatalog({
    ...CATALOG,
    models: [
      { id: "auto", name: "Auto", hidden: false, custom: false },
      { id: "gone", name: "Gone", hidden: true, custom: false },
    ],
    region_models: [
      CATALOG.region_models[0],
      { id: "gone", name: "Gone", cn: { status: "present", present: true, price_factor: 1.4 }, intl: { status: "absent", present: false } },
    ],
  });
  await panel.context.loadModels(false);
  const html = panel.elements.get("modelList").innerHTML;
  const rows = html.split('<div class="model-row').slice(1);
  assert.equal(rows.length, 2, `expected two rows, got ${rows.length}`);
  for (const row of rows) {
    const cells = [...row.matchAll(/<(span|button)\b[^>]*class="([^"]*)"/g)].map(m => m[2]);
    assert.equal(
      (row.match(/class="(drag-handle|drag-gap)[^"]*"/g) || []).length,
      1,
      `every row needs exactly one sorting-track occupant, got: ${cells.join(" | ")}`,
    );
  }
  // The hidden row still advertises no drag affordance.
  const hiddenRow = rows.find(r => r.includes('data-model-id="gone"'));
  assert.doesNotMatch(hiddenRow, /data-model-action="drag"/);
  assert.match(hiddenRow, /class="drag-gap"/);
  assert.match(hiddenRow, /class="realm-cell"/, "the facts still land in the CN / Intl tracks");
});

test("hidden rows keep their region facts in the CN / Intl cells", async () => {
  const panel = loadPanelWithCatalog({
    ...CATALOG,
    models: [{ id: "auto", name: "Auto", hidden: true, custom: false }],
    region_models: [{
      id: "auto",
      cn: { status: "present", present: true, price_factor: 0.5, context_length: 200000 },
      intl: { status: "absent", present: false },
    }],
  });
  await panel.context.loadModels(false);
  const row = panel.elements.get("modelList").innerHTML.split('<div class="model-row')[1];
  // Cells must appear in order: sorting track, id, CN, Intl, actions.
  const order = [...row.matchAll(/class="(drag-handle|drag-gap|model-id|realm-cell|model-actions)[^"]*"/g)].map(m => m[1]);
  assert.deepEqual(order, ["drag-gap", "model-id", "realm-cell", "realm-cell", "model-actions"]);
});

test("the degraded flat row declares one grid track per cell it emits", async () => {
  // The flat table emits 排序 / ID / 名称 / 操作 cells. Declaring fewer tracks
  // than cells pushes the extras into implicit rows, which wrapped the action
  // buttons onto a second line.
  const panel = loadPanel();
  panel.context.api = async () => ({
    source: "dynamic",
    models: [{ id: "auto", name: "Auto", hidden: false, custom: false, coolingAccounts: 2 }],
  });
  await panel.context.loadModels(false);
  const row = panel.elements.get("modelList").innerHTML.split('<div class="model-row')[1];
  const cells = [...row.matchAll(/class="(drag-handle|drag-gap|model-id|model-name|model-actions)[^"]*"/g)].map(m => m[1]);
  assert.deepEqual(cells, ["drag-handle", "model-id", "model-name", "model-actions"]);

  const html = fs.readFileSync(path.join(__dirname, "panel.html"), "utf8");
  const rule = html.match(/(?:^|[}\n])\.model-row-flat\{([^}]*)\}/);
  assert.ok(rule, "missing .model-row-flat rule");
  // The declaration may be last in the block, so it need not end in a semicolon.
  const tracks = rule[1].match(/grid-template-columns:([^;]+)(?:;|$)/);
  assert.ok(tracks, "the flat row must declare its tracks");
  const columns = tracks[1].trim().split(/\s+(?![^()]*\))/);
  assert.equal(
    columns.length,
    cells.length,
    `flat row emits ${cells.length} cells but declares ${columns.length} tracks: ${tracks[1]}`,
  );
  // The action track follows the same rule as the region table: a content-sized
  // floor so buttons never get squeezed onto a second line.
  assert.match(columns[columns.length - 1], /^minmax\(\s*\d+px\s*,\s*max-content\s*\)$/);
  assert.doesNotMatch(columns[columns.length - 1], /1fr|fit-content/);
});

test("the region table declares one track per cell too", async () => {
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  const row = panel.elements.get("modelList").innerHTML.split('<div class="model-row')[1];
  const cells = [...row.matchAll(/class="(drag-handle|drag-gap|model-id|realm-cell|model-actions)[^"]*"/g)].map(m => m[1]);
  assert.deepEqual(cells, ["drag-handle", "model-id", "realm-cell", "realm-cell", "model-actions"]);
  const html = fs.readFileSync(path.join(__dirname, "panel.html"), "utf8");
  const tracks = html.match(/(?:^|[}\n])\.model-head,\.model-row\{([^}]*)\}/)[1].match(/grid-template-columns:([^;]+);/)[1];
  assert.equal(tracks.trim().split(/\s+(?![^()]*\))/).length, cells.length);
});

test("a silently dropped model_order is reported instead of claimed as saved", async () => {
  // The backend may accept the PATCH with HTTP 200 while discarding a field it
  // does not know. The reload then serves the old order; saying "已保存" here
  // would be a false report, so the panel must warn.
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  const toasts = [];
  panel.context.toast = (title, kind, detail) => { toasts.push([title, kind, detail]); };
  // A backend that ignores model_order: patches succeed, order never changes.
  panel.context.managementAPI = async () => ({ ok: true });
  assert.equal(await panel.context.saveModelOrder(["qmodel", "auto"]), true);
  assert.equal(panelState(panel, "JSON.stringify(lastModels.map(m=>m.id))"), JSON.stringify(["auto", "qmodel"]));
  assert.equal(toasts.length, 2);
  // A success toast passes no detail, so the stub sees undefined rather than "".
  assert.deepEqual(toasts[0].slice(0, 2), ["模型顺序已保存到插件配置", "ok"]);
  assert.ok(toasts[0][2] === undefined || toasts[0][2] === "");
  assert.equal(toasts[1][0], "顺序未生效");
  assert.equal(toasts[1][1], "warn");
  assert.match(toasts[1][2], /model_order/);
});

test("orderKept ignores models added or removed by the upstream catalog", () => {
  const panel = loadPanelWithCatalog();
  // A normal catalog change during the reload must not look like a failed save.
  panelState(panel, `lastModels=[{id:"b"},{id:"a"},{id:"brand-new"}];`);
  assert.equal(panel.context.orderKept(["b", "a"]), true);
  panelState(panel, `lastModels=[{id:"a"},{id:"b"},{id:"brand-new"}];`);
  assert.equal(panel.context.orderKept(["b", "a"]), false);
  // A removed model simply drops out of the comparison.
  panelState(panel, `lastModels=[{id:"a"}];`);
  assert.equal(panel.context.orderKept(["b", "a"]), true);
  panelState(panel, `lastModels=[];`);
  assert.equal(panel.context.orderKept(["b", "a"]), true);
});

test("an unchanged, still-persisted save reports success without a warning", async () => {
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  const toasts = [];
  panel.context.toast = (title, kind, detail) => { toasts.push([title, kind, detail]); };
  assert.equal(await panel.context.saveModelOrder(["qmodel", "auto"]), true);
  assert.equal(toasts.length, 1, `expected only the success toast, got ${JSON.stringify(toasts)}`);
  assert.equal(toasts[0][1], "ok");
});
