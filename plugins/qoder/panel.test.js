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
    // Attributes are stored, not swallowed: the tier buttons read their own
    // aria-pressed back to decide whether a click is a no-op, so a stub that
    // drops writes would make that path untestable (and would silently pass a
    // click handler that never guards against re-clicking the live tier).
    attributes: {},
    setAttribute(name, value) { this.attributes[name] = String(value); },
    getAttribute(name) {
      return Object.prototype.hasOwnProperty.call(this.attributes, name) ? this.attributes[name] : null;
    },
    removeAttribute(name) { delete this.attributes[name]; },
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
  //
  // It remembers model_context for the same reason: setContextTier() posts the
  // override, persists it, then reloads and checks the served context_length
  // actually changed. A stub that forgot the override would make that read-back
  // fail and turn every tier switch into a spurious "档位未生效" warning.
  const state = { order: null, context: {}, patches: [], contextCalls: [] };
  panel.state = state;
  const served = () => {
    const models = state.order
      ? state.order.map(id => catalog.models.find(m => m.id === id)).filter(Boolean)
      : catalog.models;
    // Apply the stored overrides to the served region cells, the way the real
    // backend does in buildPanelRegionModels().
    const regionModels = (catalog.region_models || []).map(row => {
      const override = state.context[row.id];
      if (!override) return row;
      const next = { ...row };
      for (const region of ["cn", "intl"]) {
        if (next[region] && next[region].present === true) {
          next[region] = { ...next[region], context_length: override, context_override: true };
        }
      }
      return next;
    });
    return { ...catalog, models, region_models: regionModels };
  };
  panel.context.api = async (path, opts) => {
    if (path === "/models/context") {
      const body = JSON.parse(opts.body);
      state.contextCalls.push(body);
      const tiers = (catalog.region_models || [])
        .find(r => r.id === body.id);
      const options = tiers
        ? [...new Set([tiers.cn, tiers.intl]
            .filter(c => c && Array.isArray(c.context_tiers))
            .flatMap(c => c.context_tiers.map(t => t.tokens)))].sort((a, b) => a - b)
        : [];
      if (body.context_length == null) delete state.context[body.id];
      else state.context[body.id] = body.context_length;
      return { success: true, id: body.id, context_length: body.context_length ?? null, model_context: { ...state.context }, context_options: options };
    }
    return served();
  };
  panel.context.managementAPI = async (p, o) => {
    const body = JSON.parse(o.body);
    state.patches.push([p, body]);
    if (body.model_order) state.order = body.model_order;
    if (body.model_context) state.context = { ...body.model_context };
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
  // Six-column header: 排序 / 模型 / 模型名 / CN / Intl / 操作.
  assert.match(html, /<span>排序<\/span><span>模型<\/span><span>模型名<\/span><span>CN<\/span><span>Intl<\/span><span>操作<\/span>/);
  // The display-name column turns the upstream key into something readable: the
  // fixture's qmodel carries upstream display_name "Q".
  assert.match(html, /class="model-display"[^>]*>Q</);
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

test("the effective context tier is bolded, for tiers and both legacy key shapes", () => {
  const { context } = loadPanel();
  // No model id → the tiers stay plain text. This is the fallback for a caller
  // that has no id to key a click on; it must still mark the live tier.
  const line = facts => {
    const html = context.regionCell({ cn: { status: "present", present: true, ...facts } }, "cn");
    const match = html.match(/上下文 ([\s\S]*?)<\/span><\/div>$/);
    assert.ok(match, `no 上下文 fact in ${html}`);
    return match[1];
  };
  const bolded = text => `<b class="ctx-default" title="当前生效档位 — CPA 与客户端看到的就是这一档">${text}</b>`;
  const sep = '<span class="ctx-sep">/</span>';
  // context_tiers is the current key: objects with label/tokens/is_default.
  const tiers = line({
    context_length: 200000,
    context_tiers: [
      { label: "200K", tokens: 200000, is_default: true },
      { label: "400K", tokens: 400000 },
      { label: "1M", tokens: 1000000 },
    ],
  });
  assert.equal(tiers, `${bolded("200K")}${sep}400K${sep}1M`);
  // The bold must follow context_length, NOT is_default. The backend advertises
  // a 1M-capable model at 1M even when the provider's own default tier is 200K
  // (models.go: effectiveCtx only falls back to the marked default when
  // max_input_tokens is absent), so bolding is_default would label a tier
  // nobody is served as the live one — the exact misreport this row exists to
  // prevent. This fixture is that disagreement, stated explicitly.
  const disagrees = line({
    context_length: 1000000,
    context_tiers: [
      { label: "200K", tokens: 200000, is_default: true },
      { label: "1M", tokens: 1000000 },
    ],
  });
  assert.equal(disagrees, `200K${sep}${bolded("1M")}`);
  assert.doesNotMatch(disagrees, /<b[^>]*>200K<\/b>/);
  // context_options is the legacy key; supported_context_lengths the older one.
  const options = line({ context_length: 600000, context_options: [300000, 600000, 1000000], default_context_length: 600000 });
  assert.equal(options, `300K${sep}${bolded("600K")}${sep}1M`);
  const legacy = line({ context_length: 300000, supported_context_lengths: [300000, 600000], default_context_length: 300000 });
  assert.equal(legacy, `${bolded("300K")}${sep}600K`);
  // context_tiers wins when both are present.
  assert.equal(
    line({ context_length: 128000, context_tiers: [{ label: "128K", tokens: 128000 }], context_options: [300000] }),
    bolded("128K"),
  );
  // No advertised value at all: the list renders unadorned rather than guessing
  // one of the tiers. Unknowable must stay visibly unknown.
  const noEffective = line({ context_options: [300000, 600000] });
  assert.equal(noEffective, `300K${sep}600K`);
  assert.doesNotMatch(noEffective, /ctx-default/);
  // An effective value outside the offered tiers is appended instead of being
  // dropped: dropping it would leave the row with no bold at all, which reads
  // as "no tier is live" — the one thing this row must never imply.
  const foreign = line({ context_length: 750000, context_options: [300000, 600000], default_context_length: 750000 });
  assert.equal(foreign, `300K${sep}600K${sep}${bolded("750K")}`);
  // Single-tier fallback when no option list is reported.
  assert.equal(line({ context_length: 128000 }), bolded("128K"));
  assert.equal(line({}), "未上报");
});

// 档位必须可点：点哪一档就切到哪一档。这里守的是"按钮形状 + 生效标记"两件事——
// 生效标记不能只靠加粗（视觉信号对读屏不可见），所以另发一个 aria-pressed。
test("each context tier is a clickable button carrying its own model id and tokens", () => {
  const { context } = loadPanel();
  const html = context.regionCell(
    {
      cn: {
        status: "present",
        present: true,
        context_length: 200000,
        context_tiers: [
          { label: "200K", tokens: 200000, is_default: true },
          { label: "400K", tokens: 400000 },
          { label: "1M", tokens: 1000000 },
        ],
      },
    },
    "cn",
    "qmodel_latest",
  );
  // Every tier is a real <button>, so it is focusable and keyboard-reachable.
  assert.equal((html.match(/<button type="button" class="ctx-tier/g) || []).length, 3);
  // Each carries the model id and its own token count — the click handler reads
  // both from the DOM, so losing either silently breaks switching.
  for (const tokens of [200000, 400000, 1000000]) {
    assert.match(html, new RegExp(`data-ctx-tier="qmodel_latest" data-ctx-tokens="${tokens}"`));
  }
  // Exactly one tier is marked live, and it is the advertised one.
  assert.equal((html.match(/aria-pressed="true"/g) || []).length, 1);
  assert.match(html, /class="ctx-tier ctx-tier-on"[^>]*data-ctx-tokens="200000"[^>]*aria-pressed="true"/);
  // The live tier must not be rendered as a bare <b>: a <b> is not clickable, so
  // a regression back to the old shape would leave no way to change the tier.
  assert.doesNotMatch(html, /<b class="ctx-default"/);
  // No inline handlers (the panel's CSP forbids them and the rest of the panel
  // binds through data-* attributes).
  assert.doesNotMatch(html, /onclick=/);
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
  // .ctx-default 是"没有档位表可点"时的兜底加粗文本；.ctx-tier-on 是可点按钮里
  // 生效的那一档。两者都得带上主色与加粗，否则"当前用的是哪一档"就没有视觉落点。
  assert.match(rule(".realm-facts .ctx-default"), /font-weight:700/);
  assert.match(rule(".realm-facts .ctx-default"), /color:var\(--acc\)/);
  assert.match(rule(".ctx-tier-on"), /font-weight:700/);
  assert.match(rule(".ctx-tier-on"), /color:var\(--acc\)/);
  // 档位按钮所在的 span 必须放行溢出：上面 .realm-facts>span 的 overflow:hidden 会
  // 把按钮裁掉半个，半个按钮点不到、看着还像渲染坏了。
  assert.match(rule(".realm-facts>span.realm-facts-ctx"), /overflow:visible/);
  assert.match(rule(".realm-facts>span.realm-facts-ctx"), /white-space:normal/);

  const tracks = rule(".model-head,.model-row").match(/grid-template-columns:([^;]+);/);
  assert.ok(tracks, "grid-template-columns must be declared for the model table");
  const columns = tracks[1].trim().split(/\s+(?![^()]*\))/);
  // 排序 / ID / 模型名 / CN / Intl / 操作
  assert.equal(columns.length, 6, `expected 6 tracks, got ${columns.join(" | ")}`);
  const last = columns[5];
  assert.doesNotMatch(last, /1fr|fit-content/);
  // The action track is sized to its content, with no px floor. A floor sounds
  // safer but is measured against the *theoretical* widest set (移除/恢复 +
  // 自定义 + 冷却 999 + 已隐藏 = 235.4px in headless Chrome), which a real
  // account rarely hits; the common widest is 174px. The unused pixels are
  // subtracted from CN / Intl, which is what truncated the thinking-tier list.
  // max-content takes the widest row actually present: it never over-reserves,
  // and it still grows for a four-tag row instead of squeezing the buttons.
  assert.equal(last, "max-content", `the action track must be max-content, got ${last}`);
  // The ID and display-name tracks are fixed-width on purpose: both wrap through
  // overflow-wrap:anywhere, so nothing is clipped, and every leftover pixel goes
  // to the CN / Intl fact columns — which do NOT wrap and therefore need the
  // room. A minmax(...,1fr) here would compete for exactly that space and
  // truncate the thinking-tier list.
  assert.match(columns[1], /^\d+px$/, "the model-ID track must be a fixed width");
  assert.match(columns[2], /^\d+px$/, "the model-name track must be a fixed width");
  const minWidth = Number((rule(".model-table").match(/min-width:(\d+)px/) || [])[1] || 0);
  const padding = Number((rule(".model-head,.model-row").match(/padding:\d+px (\d+)px/) || [])[1] || 0);
  const floorSum = columns.reduce((sum, track) => {
    const value = track.match(/(\d+)px/);
    return sum + (value ? Number(value[1]) : 0);
  }, 0);
  assert.ok(padding > 0, "row padding must be declared for the track sum to be meaningful");
  assert.ok(
    minWidth >= floorSum,
    `.model-table min-width ${minWidth}px cannot fit the declared track floors (${floorSum}px)`,
  );
  // The action column has no declared floor, so it needs its own worst case.
  const WIDEST_ACTION_SET_PX = 236;

  // The table must also fit the panel it lives in. Checking only
  // `min-width >= floorSum` (above) is exactly what let a 1090px table ship into
  // a 1034px container: the track floors agreed with each other while the whole
  // table overflowed its parent, so .model-table-wrap's overflow-x:auto produced
  // a horizontal scrollbar at every window width.
  //
  // The binding constraint is the narrowest desktop window we support, 1024px,
  // not .wrap's 1100px max-width. At 1024px the wrap is narrower than its cap, so
  // the cap tells us nothing. Every value below is read from the shipped CSS so
  // this guard keeps working when the padding changes.
  const MIN_DESKTOP_VIEWPORT = 1024;
  const wrapMax = Number((rule(".wrap").match(/max-width:(\d+)px/) || [])[1] || 0);
  const wrapPad = Number((rule(".wrap").match(/padding:\d+px (\d+)px/) || [])[1] || 0);
  const cardPad = Number((rule(".card").match(/padding:(\d+)px/) || [])[1] || 0);
  assert.ok(wrapMax && wrapPad, "could not derive the panel's available width from CSS");
  // A 1024px *window* is not a 1024px *viewport*: the page scrolls, so the
  // browser's vertical scrollbar takes ~15px of layout width. Omitting it is how
  // a 948px table came to scroll inside a container this test called 958px wide —
  // the guard passed while the real window showed a horizontal scrollbar.
  const SCROLLBAR_PX = 15;
  const wrapWidth = Math.min(wrapMax, MIN_DESKTOP_VIEWPORT - SCROLLBAR_PX);
  const available = wrapWidth - wrapPad * 2 - cardPad * 2 - 2;
  // Grid gaps and the row's own horizontal padding sit inside the table too.
  const gap = Number((rule(".model-head,.model-row").match(/gap:(\d+)px/) || [])[1] || 0);
  const inlinePad = Number((rule(".model-head,.model-row").match(/padding:\d+px (\d+)px/) || [])[1] || 0);
  const needed = floorSum + WIDEST_ACTION_SET_PX + gap * (columns.length - 1) + inlinePad * 2;
  assert.ok(
    needed <= available,
    `the model table needs ${needed}px (floors ${floorSum} + widest action column ${WIDEST_ACTION_SET_PX} `
      + `+ gaps ${gap * (columns.length - 1)} + padding ${inlinePad * 2}) `
      + `but only ${available}px is available at a ${MIN_DESKTOP_VIEWPORT}px viewport, so it will always show a horizontal scrollbar`,
  );
  // min-width must not exceed what that same viewport offers either: a table
  // wider than its container scrolls even when its tracks would have fit. It
  // also has to cover the same worst case, or the action column would push the
  // table past min-width at exactly the moment it is needed most.
  assert.equal(
    minWidth,
    needed,
    `.model-table min-width ${minWidth}px should equal the worst case ${needed}px `
      + `(fixed floors + widest action column + gaps + padding)`,
  );

  // The CN / Intl columns should have room for the longest fact line they render
  // at an ordinary desktop width, because that text does not wrap: under pressure
  // it ellipsizes (see .realm-facts>span), which quietly hides tier detail. This
  // asserts the facts are shown *in full* at 1280px; at 1024px they may ellipsize,
  // which is the deliberate trade for not having a scrollbar there.
  // Widest real value, measured in headless Chrome against this stylesheet:
  // "思考 默认 medium（low/medium/xhigh）" = 217.4px. The requirement is set 2px
  // above that on purpose: sizing the tracks to land exactly on 217px left the
  // line 0.4px short, which was enough for text-overflow to fire and print an
  // ellipsis on text that had room to fit. Sub-pixel shortfalls are real, so the
  // guard has to demand a margin rather than the bare measured width.
  const WIDEST_FACT_LINE_PX = 220;
  const COMFORTABLE_VIEWPORT = 1280;
  const wrapAtComfort = Math.min(wrapMax, COMFORTABLE_VIEWPORT);
  const availableAtComfort = wrapAtComfort - wrapPad * 2 - cardPad * 2 - 2;
  const leftoverAtComfort = availableAtComfort - floorSum - gap * (columns.length - 1) - inlinePad * 2;
  const factFloors = columns.slice(3, 5).map(track => Number(track.match(/minmax\(\s*(\d+)px/)[1]));
  const factWidth = Math.min(...factFloors) + leftoverAtComfort / 2;
  assert.ok(
    factWidth >= WIDEST_FACT_LINE_PX,
    `CN/Intl tracks reach only ${factWidth.toFixed(0)}px at a ${COMFORTABLE_VIEWPORT}px viewport, `
      + `but the longest fact line needs ${WIDEST_FACT_LINE_PX}px and cannot wrap, so it would ellipsize`,
  );
});

// ---------------------------------------------- 7. 排序持久化只 PATCH 一个字段

// 保存一次排序要发两个请求：PUT /models 让插件内存里的 overlay 立刻生效，
// PATCH config 把顺序落盘以便重启后恢复。只做 PATCH 的话，顺序要等 CPA
// 下一次重载插件才生效——而它不一定会重载，操作者就会看到"保存成功但没变"。
test("saveModelOrder applies immediately and persists", async () => {
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  const saved = await panel.context.saveModelOrder(["qmodel", "auto"]);
  assert.equal(saved, true);

  // Immediately applied through the model overlay endpoint.
  assert.equal(panel.state.patches[0][0], "/plugins/qoder/models");
  assert.deepEqual(panel.state.patches[0][1], { order: ["qmodel", "auto"] });

  // Persisted through config so the order survives a restart.
  const patch = panel.state.patches.find((p) => p[0] === "/plugins/qoder/config");
  assert.ok(patch, "config must be patched for the order to persist");
  assert.deepEqual(Object.keys(patch[1]), ["model_order"], "model_order is the only config field to touch");
  assert.deepEqual(patch[1].model_order, ["qmodel", "auto"]);
  assert.equal(panelState(panel, "JSON.stringify(lastModels.map(m=>m.id))"), JSON.stringify(["qmodel", "auto"]));
});

// 内存里应用成功了、但落盘失败，也必须回滚：重启后顺序会丢，
// 面板不能在这时候报告"已保存"。
test("saveModelOrder rolls back when persistence fails", async () => {
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  const toasts = [];
  panel.context.toast = (title, kind, detail) => { toasts.push([title, kind, detail]); };
  panel.context.managementAPI = async (path) => {
    if (path === "/plugins/qoder/models") return { ok: true };
    return { error: "rejected" };
  };
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
  // A save issues two requests now (PUT the overlay, PATCH the config), so every
  // call needs its own gate — releasing only the first would park the second one
  // forever and turn a concurrency test into a deadlock.
  // A save issues two requests in sequence (PUT the overlay, then PATCH the
  // config). The gate must therefore stay open once released: releasing only the
  // requests parked at that moment would leave the PATCH that follows parked
  // forever, turning a concurrency test into a deadlock.
  let open = false;
  const waiting = [];
  const passthrough = panel.context.managementAPI;
  panel.context.managementAPI = async (p, o) => {
    if (!open) await new Promise(resolve => { waiting.push(resolve); });
    return passthrough(p, o);
  };
  const release = () => { open = true; waiting.splice(0).forEach(fn => fn()); };
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
  // A move now persists the same way a drag does: apply the overlay, then the config.
  const moved = panel.state.patches.find((p) => p[0] === "/plugins/qoder/config");
  assert.deepEqual(moved[1], { model_order: ["qmodel", "auto"] });
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
        context_length: 600000,
        context_tiers: [{ label: "600K", tokens: 600000, is_default: true }],
      },
      intl: { status: "absent", present: false },
    }],
  });
  await panel2.context.loadModels(false);
  const rendered = panel2.elements.get("modelList").innerHTML;
  assert.match(rendered, /倍率 0\.34x/);
  assert.match(rendered, /思考 默认 high（low\/high）/);
  // The live tier is a clickable button here (the row carries a model id), not a
  // bare <b>; it must still be marked as the live one.
  assert.match(rendered, /class="ctx-tier ctx-tier-on"[^>]*data-ctx-tokens="600000"[^>]*aria-pressed="true"/);
  // The model id in the button must be HTML-escaped, same as everywhere else —
  // this fixture's id contains a quote, which would break out of the attribute.
  assert.match(rendered, /data-ctx-tier="model-&quot;quote&quot;"/);
  assert.match(rendered, /data-model-action="toggle"/);
  assert.doesNotMatch(rendered, /onclick=/);
  assert.doesNotMatch(rendered, /aria-label="上移/);
});

// The bold is the one thing in this row an operator reads as "this is what I
// get". Guards the semantic against the tempting-but-wrong reading of
// is_default, which is only the provider's own preference.
test("the bolded context tier follows the advertised value, not is_default", async () => {
  const panel = loadPanel();
  panel.context.api = async () => ({
    source: "dynamic",
    models: [{ id: "qmodel_latest", name: "Qwen3.7-Max", hidden: false }],
    region_models: [{
      id: "qmodel_latest",
      cn: {
        status: "present",
        present: true,
        // Advertised 1M while the provider marks 200K as its default tier.
        context_length: 1000000,
        context_tiers: [
          { label: "200K", tokens: 200000, is_default: true },
          { label: "400K", tokens: 400000 },
          { label: "1M", tokens: 1000000 },
        ],
      },
      intl: { status: "absent", present: false },
    }],
  });
  await panel.context.loadModels(false);
  const rendered = panel.elements.get("modelList").innerHTML;
  // The live tier is marked as such...
  assert.match(rendered, /class="ctx-tier ctx-tier-on"[^>]*data-ctx-tokens="1000000"[^>]*aria-pressed="true"/);
  // ...and the provider's default is not, even though is_default says so.
  assert.doesNotMatch(rendered, /data-ctx-tokens="200000"[^>]*aria-pressed="true"/);
  // The tier list itself stays complete — the live marker shifts, it does not
  // truncate: all three tiers are still offered as buttons.
  assert.equal((rendered.match(/class="ctx-tier(?: ctx-tier-on)?"/g) || []).length, 3);
  for (const tokens of [200000, 400000, 1000000]) {
    assert.match(rendered, new RegExp(`data-ctx-tokens="${tokens}"`));
  }
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
  const attributes = {};
  return {
    dataset,
    _bound: false,
    disabled: false,
    draggable: false,
    // 档位按钮的点击处理器会回读自己的 aria-pressed 来判断"点的就是当前生效档"
    // （那种情况应当什么都不做）。替身必须真的存属性，否则这条分支测不到。
    setAttribute(name, value) { attributes[name] = String(value); },
    getAttribute(name) {
      return Object.prototype.hasOwnProperty.call(attributes, name) ? attributes[name] : null;
    },
    removeAttribute(name) { delete attributes[name]; },
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
// node graph bindModelActions() expects: one button per data-model-action and one
// per context-tier button, each pointing at its owning .model-row via closest().
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
    // Context-tier buttons are matched with their tokens and aria-pressed so the
    // click handler can read both back exactly as it does in the browser.
    for (const match of chunk.matchAll(
      /<button type="button" class="(ctx-tier(?: ctx-tier-on)?)" data-ctx-tier="([^"]*)" data-ctx-tokens="(\d+)" aria-pressed="(\w+)"/g,
    )) {
      const [, cls, tierId, tokens, pressed] = match;
      const button = domNode({ ctxTier: tierId, ctxTokens: tokens });
      button.className = cls;
      button.setAttribute("aria-pressed", pressed);
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

test("clicking a context tier switches the effective tier and persists it", async () => {
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  panel.context.toast = () => {};
  const nodes = wireRenderedRows(panel, panel.elements.get("modelList").innerHTML);
  panel.context.bindModelActions();

  const tiers = nodes.filter(n => n.dataset.ctxTier === "auto");
  assert.equal(tiers.length, 2, "auto should offer both of its tiers");
  const live = tiers.find(n => n.getAttribute("aria-pressed") === "true");
  const other = tiers.find(n => n.getAttribute("aria-pressed") === "false");
  assert.equal(live.dataset.ctxTokens, "200000", "the upstream default tier starts live");
  assert.equal(other.dataset.ctxTokens, "400000");

  // Click the non-live tier.
  other.fire("click");
  await settle();

  // The plugin was told to switch, and the change was persisted to config so it
  // survives a restart.
  assert.deepEqual(panel.state.contextCalls, [{ id: "auto", context_length: 400000 }]);
  const patch = panel.state.patches.find(p => p[0] === "/plugins/qoder/config");
  assert.ok(patch, "the tier must be persisted through the plugin config");
  assert.deepEqual(patch[1], { model_context: { auto: 400000 } });

  // After the reload the new tier is the live one — the read-back the panel uses
  // to tell a real switch from a silently dropped field.
  const after = wireRenderedRows(panel, panel.elements.get("modelList").innerHTML);
  const nowLive = after.filter(n => n.dataset.ctxTier === "auto" && n.getAttribute("aria-pressed") === "true");
  assert.equal(nowLive.length, 1, "exactly one tier is live");
  assert.equal(nowLive[0].dataset.ctxTokens, "400000", "the clicked tier became live");
});

// Clicking the tier that is already live must not write anything: a no-op write
// still produces a request, a config rewrite and a re-render, all of which the
// operator sees as a flicker for a click that changed nothing.
test("clicking the already-live tier is a no-op", async () => {
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  panel.context.toast = () => {};
  const nodes = wireRenderedRows(panel, panel.elements.get("modelList").innerHTML);
  panel.context.bindModelActions();

  const live = nodes.find(n => n.dataset.ctxTier === "auto" && n.getAttribute("aria-pressed") === "true");
  assert.ok(live, "auto must have a live tier");
  live.fire("click");
  await settle();

  assert.deepEqual(panel.state.contextCalls, [], "no request for a no-op click");
  assert.deepEqual(panel.state.patches, [], "no config write for a no-op click");
});

// A rejected switch must roll the UI back and say so. Silently keeping the old
// tier while the toast claims success is the failure mode worth guarding: the
// operator would believe a tier is in effect that the backend never accepted.
test("a rejected tier switch is reported and leaves the old tier live", async () => {
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  const toasts = [];
  panel.context.toast = (title, kind, detail) => toasts.push({ title, kind, detail });
  panel.context.api = async (path) => {
    if (path === "/models/context") {
      return { success: false, error: "context_length is not a supported tier for this model" };
    }
    return CATALOG;
  };
  const nodes = wireRenderedRows(panel, panel.elements.get("modelList").innerHTML);
  panel.context.bindModelActions();

  const other = nodes.find(n => n.dataset.ctxTier === "auto" && n.getAttribute("aria-pressed") === "false");
  other.fire("click");
  await settle();

  const failure = toasts.find(t => t.kind === "err");
  assert.ok(failure, "a rejected switch must surface an error toast");
  assert.match(failure.detail, /not a supported tier/);
  // Nothing was persisted, so the old tier is still the live one.
  assert.deepEqual(panel.state.patches, []);
  const live = wireRenderedRows(panel, panel.elements.get("modelList").innerHTML)
    .filter(n => n.dataset.ctxTier === "auto" && n.getAttribute("aria-pressed") === "true");
  assert.equal(live.length, 1);
  assert.equal(live[0].dataset.ctxTokens, "200000", "the previous tier stays live");
});

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
  const dragged = panel.state.patches.find((p) => p[0] === "/plugins/qoder/config");
  assert.deepEqual(dragged[1], { model_order: ["qmodel", "auto"] });
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
  // One drag = two writes: the drag above already wrote once, so the overview is
  // apply-then-persist twice over (see below).
  assert.equal(panel.state.patches.length, 4, "each save writes the overlay and the config");
  const keyed = panel.state.patches.filter((p) => p[0] === "/plugins/qoder/config");
  assert.deepEqual(keyed[1], ["/plugins/qoder/config", { model_order: ["auto", "qmodel"] }]);

  // Arrow keys are the only ordering gesture left, so non-arrow keys stay inert.
  const before = panelState(panel, "modelOrderVersion");
  handle.fire("keydown", { key: "ArrowLeft", preventDefault() {} });
  await settle();
  assert.equal(panelState(panel, "modelOrderVersion"), before);
  assert.equal(panel.state.patches.length, 4, "an inert key must not write anything");
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
  // Two writes per save: the overlay that applies immediately, then the config
  // that makes it survive a restart.
  assert.deepEqual(patched, [{ order: ["qmodel", "auto"] }, { model_order: ["qmodel", "auto"] }]);
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
  // The gate stays open once released: a save now performs two sequential
  // requests, so releasing only the parked one would leave the PATCH that
  // follows blocked forever.
  let open = false;
  const waiting = [];
  const passthrough = panel.context.managementAPI;
  panel.context.managementAPI = async (p, o) => {
    if (!open) await new Promise(resolve => { waiting.push(resolve); });
    return passthrough(p, o);
  };
  const release = () => { open = true; waiting.splice(0).forEach(fn => fn()); };
  const save = panel.context.saveModelOrder(["qmodel", "auto"]);
  await settle();
  assert.equal(panelState(panel, "modelOrderSaving"), true);
  // A reload landing while the PATCH is in flight must be dropped, not merged.
  const readsBefore = panelState(panel, "JSON.stringify(lastModels.map(m=>m.id))");
  assert.equal(await panel.context.loadModels(false), false);
  assert.equal(panelState(panel, "JSON.stringify(lastModels.map(m=>m.id))"), readsBefore);
  release();
  await save;
  const written = panel.state.patches.find((p) => p[0] === "/plugins/qoder/config");
  assert.deepEqual(written, ["/plugins/qoder/config", { model_order: ["qmodel", "auto"] }]);
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
  // The flat table emits 排序 / ID / 模型名 / 操作 cells. Declaring fewer tracks
  // than cells pushes the extras into implicit rows, which wrapped the action
  // buttons onto a second line.
  const panel = loadPanel();
  panel.context.api = async () => ({
    source: "dynamic",
    models: [{ id: "auto", name: "Auto", hidden: false, custom: false, coolingAccounts: 2 }],
  });
  await panel.context.loadModels(false);
  const row = panel.elements.get("modelList").innerHTML.split('<div class="model-row')[1];
  const cells = [...row.matchAll(/class="(drag-handle|drag-gap|model-id|model-display|model-actions)[^"]*"/g)].map(m => m[1]);
  assert.deepEqual(cells, ["drag-handle", "model-id", "model-display", "model-actions"]);

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
  const cells = [...row.matchAll(/class="(drag-handle|drag-gap|model-id|model-display|realm-cell|model-actions)[^"]*"/g)].map(m => m[1]);
  assert.deepEqual(cells, ["drag-handle", "model-id", "model-display", "realm-cell", "realm-cell", "model-actions"]);
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

// Qoder 对外的模型 ID 是上游内部 key（dmodel / qmodel / gmodel / kmodel …），
// 光看 ID 认不出是哪个模型。上游的 display_name 才是人类可读的名字，它在 discovery
// 时存进 ModelInfo.Name。这一列的存在意义就是把代号翻译成人能读的名字。
test("the model name column translates the upstream key into a readable name", async () => {
  const panel = loadPanelWithCatalog();
  await panel.context.loadModels(false);
  const html = panel.elements.get("modelList").innerHTML;

  // 每一行都必须有名称单元格，且与 ID 单元格相邻（ID 在前，名称在后）。
  assert.match(html, /<span class="model-id" title="qmodel">qmodel<\/span><span class="model-display" title="Q">Q<\/span>/,
    "the qmodel key must be shown next to its readable name");

  // 名称从 models 行的 name 取（即上游 display_name）。
  assert.equal(panel.context.modelDisplayLabel({ id: "dmodel", name: "DeepSeek V4.1 Flash" }, null), "DeepSeek V4.1 Flash");
  // region_models 里只有 name（没有 displayName），也要能用。
  assert.equal(panel.context.modelDisplayLabel({ id: "gm51model" }, { name: "GLM-5.1" }), "GLM-5.1");
  // models 的 name 优先于 displayName：displayName 是面板自定义显示名，通常为空，
  // 若它非空也不能盖掉上游真名。
  assert.equal(panel.context.modelDisplayLabel({ name: "上游真名", displayName: "自定义" }, null), "上游真名");
  // 全空时返回空串，由调用方渲染占位符 —— 不编造名字。
  assert.equal(panel.context.modelDisplayLabel({ id: "kmodel" }, {}), "");
  assert.equal(panel.context.modelDisplayLabel({}, null), "");
  assert.equal(panel.context.modelDisplayLabel({ name: "" }, null), "");
  // 非字符串会被 asText 字符串化（与面板其余地方处理这些字段的方式一致）。
  // Go 侧 name/displayName 都是 string，只可能缺席、不可能是别的类型，所以这个
  // 分支在真实数据里到不了；写成断言是为了钉住「沿用 asText」这个选择本身，
  // 免得以后有人以为这里做过类型过滤。
  assert.equal(panel.context.modelDisplayLabel({ name: 0 }, null), "0");
});

test("a model with no upstream name renders a placeholder instead of an empty cell", async () => {
  const panel = loadPanel();
  panel.context.api = async () => ({
    source: "dynamic",
    models: [{ id: "kmodel", hidden: false, custom: false, coolingAccounts: 0 }],
    region_models: [{ id: "kmodel", cn: { status: "present", present: true, price_factor: 1, context_length: 128000 } }],
  });
  await panel.context.loadModels(false);
  const html = panel.elements.get("modelList").innerHTML;
  // 空着分不清「没有名字」和「这一列没渲染」，所以要有明确占位符。
  assert.match(html, /class="model-display mut"[^>]*>未上报</);
  assert.match(html, /title="上游未上报 display_name"/);
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








