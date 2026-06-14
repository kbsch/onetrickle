'use strict';

/* onetrickle SPA — vanilla JS (ES2020), hash-routed, no frameworks.
 * Talks to the REST API of SPEC §10. JSON field names are assumed
 * lowerCamelCase; Go-default capitalized keys are tolerated where the
 * SPEC does not pin the wire shape. */

/* ------------------------------------------------------------------ utils */

const $ = (sel, root = document) => root.querySelector(sel);
const enc = encodeURIComponent;

// el('div', {class: 'x', onclick: fn}, child, [children...]) — tiny DOM builder.
function el(tag, attrs = {}, ...children) {
  const node = document.createElement(tag);
  for (const [k, v] of Object.entries(attrs || {})) {
    if (v == null || v === false) continue;
    if (k.startsWith('on') && typeof v === 'function') node.addEventListener(k.slice(2), v);
    else if (k === 'class') node.className = v;
    else if (k === 'value') node.value = v;
    else if (k === 'checked') node.checked = !!v;
    else if (k === 'disabled') node.disabled = !!v;
    else if (v === true) node.setAttribute(k, '');
    else node.setAttribute(k, String(v));
  }
  for (const c of children.flat(Infinity)) {
    if (c == null || c === false) continue;
    node.append(c instanceof Node ? c : document.createTextNode(String(c)));
  }
  return node;
}

// selectEl({onchange}, ['A', ['val','Label'], ...], selectedValue)
function selectEl(attrs, options, selected) {
  const s = el('select', attrs);
  for (const o of options) {
    const [val, label] = Array.isArray(o) ? o : [o, o];
    s.append(el('option', { value: val, selected: val === selected }, label));
  }
  return s;
}

const frow = (label, ctrl) =>
  el('label', { class: 'frow' }, el('span', { class: 'flabel' }, label), ctrl);

const numFmt = new Intl.NumberFormat('en-US', {
  minimumFractionDigits: 2,
  maximumFractionDigits: 2,
});

// Thousands separators, 2 decimals; zero (and non-numbers) render as "-".
function fmtNum(v) {
  const n = Number(v);
  if (v == null || v === '' || !isFinite(n) || Math.abs(n) < 1e-9) return '-';
  return numFmt.format(n);
}

const numOr = (s, d) => {
  const t = String(s == null ? '' : s).trim();
  if (t === '') return d;
  const n = Number(t);
  return isFinite(n) ? n : d;
};

function toast(msg, kind = 'error') {
  const box = $('#toasts');
  if (!box) return;
  const t = el('div', { class: 'toast ' + kind }, String(msg));
  box.append(t);
  setTimeout(() => {
    t.classList.add('gone');
    setTimeout(() => t.remove(), 350);
  }, 5000);
}

const loading = (msg = 'Loading…') => el('div', { class: 'loading' }, msg);
const emptyState = (msg) => el('div', { class: 'empty' }, msg);

// fetch wrapper: JSON in/out, FormData passthrough, {"error": "..."} → throw.
async function api(path, opts = {}) {
  const init = { method: opts.method || 'GET', headers: {} };
  if (opts.body !== undefined) {
    if (opts.body instanceof FormData) {
      init.body = opts.body; // browser sets the multipart boundary
    } else {
      init.headers['Content-Type'] = 'application/json';
      init.body = JSON.stringify(opts.body);
    }
  }
  let res;
  try {
    res = await fetch(path, init);
  } catch (err) {
    throw new Error('network error: ' + err.message);
  }
  const text = await res.text();
  let data = null;
  if (text !== '') {
    try {
      data = JSON.parse(text);
    } catch {
      data = text;
    }
  }
  if (!res.ok) {
    let msg = res.status + ' ' + res.statusText;
    if (data && typeof data === 'object' && typeof data.error === 'string' && data.error) msg = data.error;
    else if (typeof data === 'string' && data.trim()) msg = data.trim();
    throw new Error(msg);
  }
  if (data && typeof data === 'object' && !Array.isArray(data) &&
      typeof data.error === 'string' && data.error) {
    throw new Error(data.error);
  }
  return data;
}

/* ----------------------------------------------------------- shared state */

const POV_KEY = 'onetrickle.pov';
const state = {
  meta: { cubes: [], scenarios: [], years: [], currencies: [] },
  pov: { cube: '', scenario: '', time: '' },
};
try {
  Object.assign(state.pov, JSON.parse(localStorage.getItem(POV_KEY) || '{}') || {});
} catch { /* corrupt localStorage: keep defaults */ }

const savePOV = () => localStorage.setItem(POV_KEY, JSON.stringify(state.pov));

const nameList = (list) =>
  (list || []).map((x) => (x && typeof x === 'object' ? String(x.name ?? x.Name ?? '') : String(x))).filter(Boolean);

function monthsFromYears(years) {
  const out = [];
  for (const y of years || []) for (let m = 1; m <= 12; m++) out.push(`${y}M${m}`);
  return out;
}

async function loadMeta() {
  const m = (await api('/api/meta')) || {};
  state.meta = {
    cubes: nameList(m.cubes ?? m.Cubes),
    scenarios: nameList(m.scenarios ?? m.Scenarios),
    years: (m.years ?? m.Years ?? []).map(Number).filter((y) => !isNaN(y)).sort((a, b) => a - b),
    currencies: nameList(m.currencies ?? m.Currencies),
    latestDataTime: String(m.latestDataTime ?? m.LatestDataTime ?? ''),
  };
}

function ensurePOVDefaults() {
  const { cubes, scenarios, years, latestDataTime } = state.meta;
  const months = monthsFromYears(years);
  if (!cubes.includes(state.pov.cube)) state.pov.cube = cubes[0] || '';
  if (!scenarios.includes(state.pov.scenario)) state.pov.scenario = scenarios[0] || '';
  if (!months.includes(state.pov.time)) {
    // Prefer the latest month that actually has data (server hint), then
    // current-year January, then the first known month.
    const y = new Date().getFullYear();
    if (months.includes(latestDataTime)) state.pov.time = latestDataTime;
    else if (months.includes(`${y}M1`)) state.pov.time = `${y}M1`;
    else state.pov.time = months[0] || '';
  }
  savePOV();
}

function buildPOVBar() {
  const cube = $('#pov-cube'), scen = $('#pov-scenario'), time = $('#pov-time');
  cube.replaceChildren(...state.meta.cubes.map((c) => el('option', { value: c }, c)));
  scen.replaceChildren(...state.meta.scenarios.map((s) => el('option', { value: s }, s)));
  time.replaceChildren(...state.meta.years.map((y) =>
    el('optgroup', { label: String(y) },
      Array.from({ length: 12 }, (_, i) => el('option', { value: `${y}M${i + 1}` }, `${y}M${i + 1}`)))));
  cube.value = state.pov.cube;
  scen.value = state.pov.scenario;
  time.value = state.pov.time;
  const onChange = () => {
    state.pov = { cube: cube.value, scenario: scen.value, time: time.value };
    savePOV();
    gridState.norm = null; // grid results are POV-stale now
    renderRoute();
  };
  cube.onchange = onChange;
  scen.onchange = onChange;
  time.onchange = onChange;
}

/* -------------------------------------------------------- dimension cache */

const memberCache = new Map();

function dimMembers(dim) {
  if (!memberCache.has(dim)) {
    const p = api(`/api/dims/${enc(dim)}/members`)
      .then((r) => (Array.isArray(r) ? r : []))
      .catch((err) => {
        memberCache.delete(dim);
        throw err;
      });
    memberCache.set(dim, p);
  }
  return memberCache.get(dim);
}

function invalidateMembers(dim) {
  if (dim) memberCache.delete(dim);
  else memberCache.clear();
}

async function firstRoot(dim) {
  const ms = await dimMembers(dim);
  const r = ms.find((m) => !(m.parent ?? m.Parent)) || ms[0];
  return r ? String(r.name ?? r.Name ?? '') : '';
}

function isLeafMember(m, all) {
  if (typeof m.isLeaf === 'boolean') return m.isLeaf;
  if (typeof m.IsLeaf === 'boolean') return m.IsLeaf;
  return !all.some((x) => (x.parent ?? x.Parent) === m.name);
}

/* ----------------------------------------------------------------- router */

function routeName() {
  return location.hash.replace(/^#\/?/, '').split('?')[0];
}

async function renderRoute() {
  const name = routeName();
  const fn = ROUTES[name] || renderDashboard;
  const active = ROUTES[name] ? name : '';
  for (const a of document.querySelectorAll('#nav a')) {
    a.classList.toggle('active', (a.dataset.route || '') === active);
  }
  const view = $('#view');
  view.replaceChildren(loading());
  try {
    await fn(view);
  } catch (err) {
    view.replaceChildren(emptyState('Failed to load page: ' + err.message));
    toast(err.message);
  }
}

/* ----------------------------------------------------------- query shapes */

function normQueryResult(r) {
  r = r || {};
  return {
    rows: r.rowHeaders ?? r.RowHeaders ?? [],
    cols: r.colHeaders ?? r.ColHeaders ?? [],
    cells: r.cells ?? r.Cells ?? [],
    issues: (r.issues ?? r.Issues ?? []).map(String),
    rowPaths: normPaths(r.rowPaths ?? r.RowPaths),
    colPaths: normPaths(r.colPaths ?? r.ColPaths),
  };
}
const hName = (h) => String((h && (h.name ?? h.Name)) ?? '');
const hDepth = (h) => Number((h && (h.depth ?? h.Depth)) || 0);
const hIsLeaf = (h) => {
  const v = h && (h.isLeaf ?? h.IsLeaf);
  return v !== false;
};
const hDim = (h) => String((h && (h.dim ?? h.Dim)) ?? '');
// normPaths normalizes nested header tuples; null when the axis is flat.
const normPaths = (pp) =>
  Array.isArray(pp) && pp.length
    ? pp.map((tuple) => (tuple || []).map((p) =>
        ({ dim: hDim(p), name: hName(p), depth: hDepth(p), isLeaf: hIsLeaf(p) })))
    : null;

// effPaths normalizes an axis into per-position header tuples (outer→inner).
// A nested axis arrives as paths from the engine; a flat axis is synthesized
// from its single-level headers so the renderer/editor share one shape.
function effPaths(headers, paths, dims) {
  if (paths && paths.length) return paths;
  const dim = (dims && dims[0]) || '';
  return headers.map((h) => [{ dim, name: hName(h), depth: hDepth(h), isLeaf: hIsLeaf(h) }]);
}

// pivotTable renders a (possibly nested) grid: row-header tuples merge with
// rowspan, column-header tuples with colspan, so repeated outer members span
// their inner groups — a classic pivot layout. rowEff/colEff are effPaths().
function pivotTable(norm, rowEff, colEff, onDblClick) {
  const nrows = norm.rows.length, ncols = norm.cols.length;
  const rowLevels = rowEff.length ? rowEff[0].length : 1;
  const colLevels = colEff.length ? colEff[0].length : 1;
  const pad = (depth) => `padding-left:${depth * 16 + 10}px`;
  // Group key for a position at nesting level li: every level 0..li must match.
  const key = (path, li) => path.slice(0, li + 1).map((p) => p.name).join('');

  const thead = el('thead');
  for (let li = 0; li < colLevels; li++) {
    const tr = el('tr', {});
    if (li === 0) tr.append(el('th', { class: 'corner', colspan: rowLevels, rowspan: colLevels }, ''));
    for (let c = 0; c < ncols;) {
      const k = key(colEff[c], li);
      let span = 1;
      while (c + span < ncols && key(colEff[c + span], li) === k) span++;
      const part = colEff[c][li];
      // Innermost column level aligns over the numbers; outer levels center
      // across the columns they span.
      tr.append(el('th', {
        class: 'colhead ' + (li === colLevels - 1 ? 'num' : 'group') + (part.isLeaf ? '' : ' parent'),
        colspan: span, style: pad(part.depth),
      }, part.name));
      c += span;
    }
    thead.append(tr);
  }

  const tbody = el('tbody');
  for (let r = 0; r < nrows; r++) {
    const tr = el('tr', {});
    for (let li = 0; li < rowLevels; li++) {
      if (r > 0 && key(rowEff[r], li) === key(rowEff[r - 1], li)) continue; // covered by a rowspan above
      let span = 1;
      while (r + span < nrows && key(rowEff[r + span], li) === key(rowEff[r], li)) span++;
      const part = rowEff[r][li];
      tr.append(el('th', {
        class: 'rowhead' + (li === rowLevels - 1 ? '' : ' group') + (part.isLeaf ? '' : ' parent'),
        rowspan: span, style: pad(part.depth),
      }, part.name));
    }
    const rowParent = !rowEff[r][rowLevels - 1].isLeaf;
    for (let c = 0; c < ncols; c++) {
      const colParent = !colEff[c][colLevels - 1].isLeaf;
      tr.append(el('td', {
        class: 'num' + (rowParent || colParent ? ' parent' : ''),
        ondblclick: onDblClick ? () => onDblClick(r, c) : null,
      }, fmtNum((norm.cells[r] || [])[c])));
    }
    tbody.append(tr);
  }
  return el('table', { class: 'grid pivot' }, thead, tbody);
}

/* --------------------------------------------------------------- workflow */

const WF_STATUSES = ['NotStarted', 'Imported', 'Validated', 'Processed', 'Certified'];
const WF_ACTIONS = ['import', 'validate', 'process', 'certify', 'reopen'];
const workflowState = { issues: [], context: '' };

// legalActions mirrors the server's state machine (workflow.Actions):
// import from any non-certified state, validate from Imported, process from
// Validated, certify from Processed, reopen always.
function legalActions(status) {
  switch (status) {
    case 'Imported': return ['import', 'validate', 'reopen'];
    case 'Validated': return ['import', 'process', 'reopen'];
    case 'Processed': return ['import', 'certify', 'reopen'];
    case 'Certified': return ['reopen'];
    default: return ['import', 'reopen']; // NotStarted
  }
}

// Tolerates an array of entries or an object map; keys by entity name.
function workflowMap(resp) {
  const map = new Map();
  const put = (e) => {
    if (!e || typeof e !== 'object') return;
    const ent = e.entity ?? e.Entity;
    if (ent) map.set(String(ent), e);
  };
  if (Array.isArray(resp)) resp.forEach(put);
  else if (resp && typeof resp === 'object') {
    const list = Array.isArray(resp.entries ?? resp.Entries) ? (resp.entries ?? resp.Entries) : Object.values(resp);
    list.forEach(put);
  }
  return map;
}

const statusOf = (e) => String((e && (e.status ?? e.state ?? e.Status ?? e.State)) || 'NotStarted');

const statusChip = (st) => el('span', { class: 'chip st-' + st }, st);

async function doWorkflowAction(entity, action) {
  const { cube, scenario, time } = state.pov;
  try {
    const resp = await api('/api/workflow/action', {
      method: 'POST',
      body: { cube, entity, scenario, time, action },
    });
    if (action === 'process') {
      const issues = (resp && (resp.issues ?? resp.Issues ??
        (resp.result && (resp.result.issues ?? resp.result.Issues)))) || [];
      workflowState.issues = issues.map(String);
      workflowState.context = `${entity} · ${scenario} · ${time}`;
      toast(issues.length ? `Processed with ${issues.length} issue(s)` : 'Processed cleanly', issues.length ? 'info' : 'ok');
    } else {
      toast(`Action "${action}" applied to ${entity}`, 'ok');
    }
  } catch (err) {
    toast(err.message);
  }
  renderRoute();
}

async function renderWorkflow(view) {
  const { cube, scenario, time } = state.pov;
  const [entities, resp] = await Promise.all([
    dimMembers('Entity'),
    api(`/api/workflow?cube=${enc(cube)}&scenario=${enc(scenario)}&time=${enc(time)}`)
      .catch((err) => { toast(err.message); return []; }),
  ]);
  const leaves = entities.filter((m) => isLeafMember(m, entities));
  const wfMap = workflowMap(resp);
  const body = el('div', { class: 'page' },
    el('h1', {}, 'Workflow'),
    el('div', { class: 'muted' }, `${cube} · ${scenario} · ${time}`));
  if (!leaves.length) {
    body.append(emptyState('No leaf entities defined yet — add some under Metadata.'));
  } else {
    body.append(el('div', { class: 'panel table-wrap' },
      el('table', { class: 'grid wf' },
        el('thead', {}, el('tr', {},
          el('th', {}, 'Entity'), el('th', {}, 'Status'), el('th', {}, 'Actions'))),
        el('tbody', {}, leaves.map((m) => {
          const st = statusOf(wfMap.get(m.name));
          const legal = legalActions(st);
          return el('tr', {},
            el('th', { class: 'rowhead' }, m.name),
            el('td', {}, statusChip(st)),
            el('td', { class: 'actions' }, WF_ACTIONS.map((a) =>
              el('button', {
                class: 'btn small',
                disabled: !legal.includes(a),
                title: legal.includes(a) ? null : `"${a}" is not allowed from status ${st}`,
                onclick: () => doWorkflowAction(m.name, a),
              }, a))));
        })))));
  }
  if (workflowState.issues.length) {
    body.append(el('div', { class: 'panel' },
      el('span', { class: 'section-label' }, 'Process issues — ' + workflowState.context),
      el('ul', { class: 'issues' }, workflowState.issues.map((i) => el('li', {}, i)))));
  }
  view.replaceChildren(body);
}

/* -------------------------------------------------------------- dashboard */

const KPI_KEY = 'onetrickle.kpis';
const DEFAULT_KPIS = ['Sales', 'GrossProfit', 'NetIncome', 'GPMargin'];

// loadKPIs returns the configured KPI tile accounts (SPEC §11), persisted in
// localStorage; falls back to the defaults.
function loadKPIs() {
  try {
    const v = JSON.parse(localStorage.getItem(KPI_KEY) || 'null');
    if (Array.isArray(v) && v.length && v.every((x) => typeof x === 'string' && x.trim())) {
      return v.map((x) => x.trim());
    }
  } catch { /* corrupt localStorage: keep defaults */ }
  return DEFAULT_KPIS.slice();
}

async function renderDashboard(view) {
  const { cube, scenario, time } = state.pov;
  const page = el('div', { class: 'page' }, el('h1', {}, 'Dashboard'));
  if (!cube || !scenario || !time) {
    page.append(emptyState('No metadata yet — create a cube and scenario, or run "onetrickle seed".'));
    view.replaceChildren(page);
    return;
  }
  let entities = [];
  try {
    entities = await dimMembers('Entity');
  } catch (err) {
    toast(err.message);
  }
  const root = entities.find((m) => !(m.parent ?? m.Parent)) || entities[0] || null;
  const KPIS = loadKPIs();
  const values = {};
  if (root) {
    try {
      const res = await api('/api/query', {
        method: 'POST',
        body: {
          cube,
          pov: { cube, entity: root.name, scenario, time, view: 'YTD', stage: 'Consolidated' },
          rows: KPIS.map((a) => ({ dim: 'Account', member: a, expand: 'member' })),
          cols: [{ dim: 'Time', member: time, expand: 'member' }],
        },
      });
      const n = normQueryResult(res);
      n.rows.forEach((h, i) => { values[hName(h)] = (n.cells[i] || [])[0]; });
    } catch (err) {
      toast('KPI query failed: ' + err.message);
    }
  }
  page.append(el('div', { class: 'muted' },
    root ? `${root.name} · ${scenario} · ${time} · Consolidated · YTD` : 'No entities defined yet.'));
  page.append(el('div', { class: 'kpis' }, KPIS.map((a) => {
    let text = fmtNum(values[a]);
    if (a === 'GPMargin' && text !== '-') text += '%';
    return el('div', { class: 'tile' },
      el('span', { class: 'section-label' }, a),
      el('div', { class: 'kpi-value' }, text));
  })));

  // KPI tile configuration (comma-separated accounts, persisted locally).
  const kpiInput = el('input', { value: KPIS.join(', '), placeholder: DEFAULT_KPIS.join(', ') });
  page.append(el('div', { class: 'panel' },
    el('span', { class: 'section-label' }, 'KPI accounts'),
    el('div', { class: 'hrow' },
      kpiInput,
      el('button', {
        class: 'btn',
        onclick: () => {
          const list = kpiInput.value.split(',').map((x) => x.trim()).filter(Boolean);
          if (list.length) localStorage.setItem(KPI_KEY, JSON.stringify(list));
          else localStorage.removeItem(KPI_KEY);
          renderRoute();
        },
      }, 'Apply'),
      el('button', {
        class: 'btn',
        onclick: () => { localStorage.removeItem(KPI_KEY); renderRoute(); },
      }, 'Reset'))));

  // Workflow completion strip (count of leaf entities per status).
  let entries = [];
  try {
    entries = await api(`/api/workflow?cube=${enc(cube)}&scenario=${enc(scenario)}&time=${enc(time)}`);
  } catch (err) {
    toast('Workflow summary failed: ' + err.message);
  }
  const wfMap = workflowMap(entries);
  const leaves = entities.filter((m) => isLeafMember(m, entities));
  const counts = {};
  for (const s of WF_STATUSES) counts[s] = 0;
  for (const m of leaves) {
    const st = statusOf(wfMap.get(m.name));
    counts[st] = (counts[st] || 0) + 1;
  }
  page.append(el('div', { class: 'panel' },
    el('span', { class: 'section-label' }, `Workflow — ${leaves.length} leaf entit${leaves.length === 1 ? 'y' : 'ies'}`),
    el('div', { class: 'chips' }, WF_STATUSES.map((s) =>
      el('span', { class: 'chip st-' + s }, `${s} ${counts[s] || 0}`)))));
  view.replaceChildren(page);
}

/* ------------------------------------------------------------- quick view */

const EXPANDS = ['member', 'children', 'leaves', 'tree'];
const AXIS_DIMS = ['Account', 'Entity', 'Time', 'Scenario', 'Flow', 'Origin', 'UD1', 'UD2', 'UD3', 'UD4'];
const USER_ORIGINS = ['Import', 'Forms', 'Adj'];

// rows/cols are ordered lists of nesting levels (outer→inner); each level is
// one dimension slice {dim, member, expand}. A single-level axis renders flat;
// two or more levels render as a nested pivot.
const gridState = {
  rows: [{ dim: 'Account', member: '', expand: 'tree' }],
  cols: [{ dim: 'Time', member: '', expand: 'member' }],
  view: 'Periodic',
  stage: 'Consolidated',
  origin: '',
  entity: '',
  account: '',
  norm: null,
  resolved: { entity: '', account: '', rowDims: [], colDims: [] },
  rowEff: [],
  colEff: [],
};

async function buildQueryRequest() {
  const { cube, scenario, time } = state.pov;
  const entity = gridState.entity || (await firstRoot('Entity'));
  const account = gridState.account || (await firstRoot('Account'));
  const resolveLevel = async (ax) => ({
    dim: ax.dim,
    member: ax.member.trim() || (ax.dim === 'Time' ? time : await firstRoot(ax.dim)),
    expand: ax.expand,
  });
  const rows = [];
  for (const ax of gridState.rows) rows.push(await resolveLevel(ax));
  const cols = [];
  for (const ax of gridState.cols) cols.push(await resolveLevel(ax));
  gridState.resolved = {
    entity, account,
    rowDims: rows.map((s) => s.dim),
    colDims: cols.map((s) => s.dim),
  };
  return {
    cube,
    pov: {
      cube, entity, scenario, time,
      view: gridState.view, stage: gridState.stage,
      origin: gridState.origin, account,
    },
    // Each level is sent as its own one-spec nesting level; the engine crosses
    // them. A single level yields a flat axis (no paths returned).
    rowNest: rows.map((s) => [s]),
    colNest: cols.map((s) => [s]),
  };
}

async function runQuery(resultBox) {
  resultBox.replaceChildren(loading('Running query…'));
  try {
    const req = await buildQueryRequest();
    const res = await api('/api/query', { method: 'POST', body: req });
    gridState.norm = normQueryResult(res);
    drawGridResult(resultBox);
  } catch (err) {
    gridState.norm = null;
    resultBox.replaceChildren(emptyState('Query failed: ' + err.message));
    toast(err.message);
  }
}

// editableOrigin: cells may be edited only against a specific user origin —
// either the grid's Origin filter or an Origin member on any axis level.
function editableOriginActive() {
  return USER_ORIGINS.includes(gridState.origin) ||
    gridState.rows.some((a) => a.dim === 'Origin') ||
    gridState.cols.some((a) => a.dim === 'Origin');
}

function drawGridResult(resultBox) {
  const n = gridState.norm;
  if (!n) {
    resultBox.replaceChildren(emptyState('Run a query to see results.'));
    return;
  }
  if (!n.rows.length || !n.cols.length) {
    resultBox.replaceChildren(emptyState('The query returned no rows or columns.'));
    return;
  }
  // Effective header tuples drive both the pivot render and cell editing.
  gridState.rowEff = effPaths(n.rows, n.rowPaths, gridState.resolved.rowDims);
  gridState.colEff = effPaths(n.cols, n.colPaths, gridState.resolved.colDims);
  let hint;
  if (gridState.stage !== 'Local') {
    hint = 'Switch Stage to Local to edit cells.';
  } else if (!editableOriginActive()) {
    hint = 'To edit cells, set Origin to Import, Forms or Adj (the aggregated view is read-only).';
  } else {
    hint = 'Double-click a leaf cell to enter a value (replaces the value at that origin).';
  }
  const parts = [el('div', { class: 'hint muted' }, hint)];
  if (n.issues && n.issues.length) {
    parts.push(el('div', { class: 'panel' },
      el('span', { class: 'section-label' }, 'Query issues'),
      el('ul', { class: 'issues' }, n.issues.map((i) => el('li', {}, i)))));
  }
  parts.push(el('div', { class: 'panel table-wrap' },
    pivotTable(n, gridState.rowEff, gridState.colEff, (r, c) => editGridCell(r, c, resultBox))));
  resultBox.replaceChildren(...parts);
}

// Overlays an axis member onto a unit/coord per its dimension.
function overlayMember(dim, member, unit, coord) {
  switch (dim) {
    case 'Entity': unit.entity = member; break;
    case 'Scenario': unit.scenario = member; break;
    case 'Time': unit.time = member; break;
    case 'Account': coord.account = member; break;
    case 'Flow': coord.flow = member; break;
    case 'Origin': coord.origin = member; break;
    case 'UD1': coord.ud1 = member; break;
    case 'UD2': coord.ud2 = member; break;
    case 'UD3': coord.ud3 = member; break;
    case 'UD4': coord.ud4 = member; break;
  }
}

async function editGridCell(r, c, resultBox) {
  if (gridState.stage !== 'Local') {
    toast('Cells are editable only when Stage is Local.', 'info');
    return;
  }
  const n = gridState.norm;
  if (!n) return;
  const { cube, scenario, time } = state.pov;
  const rowTuple = gridState.rowEff[r] || [];
  const colTuple = gridState.colEff[c] || [];
  // Only fully-leaf cells are writable; any parent member on the axes means
  // this cell is an aggregate of several stored cells.
  if (![...rowTuple, ...colTuple].every((p) => p.isLeaf)) {
    toast('That cell aggregates several members — drill to leaf members on every axis to edit.', 'info');
    return;
  }
  const unit = { cube, entity: gridState.resolved.entity, scenario, time };
  const coord = {
    account: gridState.resolved.account,
    flow: '', origin: gridState.origin, ic: '',
    ud1: '', ud2: '', ud3: '', ud4: '',
  };
  for (const p of rowTuple) overlayMember(p.dim, p.name, unit, coord);
  for (const p of colTuple) overlayMember(p.dim, p.name, unit, coord);
  // Replace-what-you-see semantics: edits are only allowed against a specific
  // user origin, so the displayed value is the one being replaced. No silent
  // origin coercion (a write at another origin would ADD to the shown sum).
  if (!USER_ORIGINS.includes(coord.origin)) {
    toast('To edit cells, set Origin to Import, Forms or Adj — the aggregated view is read-only.', 'info');
    return;
  }
  if (!/^\d{1,4}M(?:1[0-2]|[1-9])$/.test(unit.time)) {
    toast('Writes need a month-level Time member (e.g. 2025M3).');
    return;
  }
  // Calculated accounts are engine-owned: refuse the edit up front.
  try {
    const accounts = await dimMembers('Account');
    const am = accounts.find((m) => (m.name ?? m.Name) === coord.account);
    if (am && (am.formula || am.Formula || am.dynamicCalc || am.DynamicCalc)) {
      toast(`Account "${coord.account}" is calculated by a formula and cannot be edited.`, 'info');
      return;
    }
  } catch { /* metadata unavailable: the server still validates */ }
  const cur = Number((n.cells[r] || [])[c]) || 0;
  const input = prompt(
    `New value for ${coord.account} @ ${unit.entity} / ${unit.scenario} / ${unit.time} (Origin=${coord.origin})`,
    cur === 0 ? '' : String(cur));
  if (input === null) return;
  const value = input.trim() === '' ? 0 : Number(input);
  if (!isFinite(value)) {
    toast('Not a number: ' + input);
    return;
  }
  try {
    await api('/api/data/cells', { method: 'POST', body: [{ unit, coord, value }] });
    toast('Cell saved', 'ok');
    await runQuery(resultBox);
  } catch (err) {
    toast(err.message);
  }
}

async function exportCSV() {
  const { cube, scenario, time } = state.pov;
  const stage = gridState.stage || 'Consolidated';
  const url = `/api/export?cube=${enc(cube)}&scenario=${enc(scenario)}&time=${enc(time)}&stage=${enc(stage)}`;
  try {
    const res = await fetch(url);
    if (!res.ok) {
      let msg = res.status + ' ' + res.statusText;
      try {
        const j = await res.json();
        if (j && j.error) msg = j.error;
      } catch { /* not JSON */ }
      throw new Error(msg);
    }
    const blob = await res.blob();
    const href = URL.createObjectURL(blob);
    const a = el('a', { href, download: `${cube}_${scenario}_${time}_${stage}.csv` });
    document.body.append(a);
    a.click();
    a.remove();
    setTimeout(() => URL.revokeObjectURL(href), 5000);
  } catch (err) {
    toast('Export failed: ' + err.message);
  }
}

async function renderGrid(view) {
  // Prefetch members for every dimension referenced by an axis level (for the
  // member datalists) plus Entity/Account (for the POV filters).
  const axisDims = [...new Set([...gridState.rows, ...gridState.cols].map((a) => a.dim))];
  const membersByDim = {};
  await Promise.all([...new Set([...axisDims, 'Entity', 'Account'])].map((d) =>
    dimMembers(d).then((ms) => { membersByDim[d] = ms; }).catch(() => { membersByDim[d] = []; })));
  const entities = membersByDim.Entity || [];
  const accounts = membersByDim.Account || [];
  const onAxis = new Set(axisDims);
  const resultBox = el('div', { class: 'result-box' });

  // One nesting level: dimension · member · expand · reorder/remove. Outer
  // levels sit above inner ones; the rendered axis is their cross product.
  const levelRow = (levels, i, dlPrefix) => {
    const ax = levels[i];
    const dlId = `${dlPrefix}-${i}`;
    const members = membersByDim[ax.dim] || [];
    return el('div', { class: 'level' },
      selectEl({
        onchange: (e) => { ax.dim = e.target.value; ax.member = ''; renderRoute(); },
        title: 'Dimension',
      }, AXIS_DIMS, ax.dim),
      el('input', {
        list: dlId, value: ax.member, placeholder: 'member (default: top)',
        oninput: (e) => { ax.member = e.target.value; },
      }),
      el('datalist', { id: dlId }, members.map((m) => el('option', { value: m.name }))),
      selectEl({ onchange: (e) => { ax.expand = e.target.value; }, title: 'Expand' }, EXPANDS, ax.expand),
      el('div', { class: 'level-btns' },
        el('button', {
          class: 'btn small', title: 'Move outward', disabled: i === 0,
          onclick: () => { [levels[i - 1], levels[i]] = [levels[i], levels[i - 1]]; renderRoute(); },
        }, '↑'),
        el('button', {
          class: 'btn small', title: 'Move inward', disabled: i === levels.length - 1,
          onclick: () => { [levels[i + 1], levels[i]] = [levels[i], levels[i + 1]]; renderRoute(); },
        }, '↓'),
        el('button', {
          class: 'btn small danger', title: 'Remove dimension', disabled: levels.length === 1,
          onclick: () => { levels.splice(i, 1); renderRoute(); },
        }, '✕')));
  };

  const axisControls = (label, levels, dlPrefix) => {
    const used = new Set(levels.map((a) => a.dim));
    const nextDim = AXIS_DIMS.find((d) => !used.has(d)) || AXIS_DIMS[0];
    return el('div', { class: 'control-group' },
      el('span', { class: 'section-label' }, `${label} — outer → inner`),
      el('div', { class: 'levels' }, levels.map((_, i) => levelRow(levels, i, dlPrefix))),
      el('button', {
        class: 'btn small', disabled: levels.length >= AXIS_DIMS.length,
        onclick: () => { levels.push({ dim: nextDim, member: '', expand: 'member' }); renderRoute(); },
      }, '+ Add dimension'));
  };

  // A POV member filter is disabled when its dimension is on an axis (the axis
  // member overrides it cell-by-cell).
  const povMemberFilter = (dim, list, key) =>
    frow(onAxis.has(dim) ? `${dim} (on axis)` : dim,
      selectEl({ disabled: onAxis.has(dim), onchange: (e) => { gridState[key] = e.target.value; } },
        [['', '(top)']].concat(list.map((m) => m.name)), gridState[key]));

  const povControls = el('div', { class: 'control-group' },
    el('span', { class: 'section-label' }, 'POV'),
    el('div', { class: 'hrow wrap' },
      frow('View', selectEl({ onchange: (e) => { gridState.view = e.target.value; } }, ['Periodic', 'YTD'], gridState.view)),
      frow('Stage', selectEl({ onchange: (e) => { gridState.stage = e.target.value; } },
        ['Local', 'Translated', 'Elimination', 'Consolidated'], gridState.stage)),
      frow('Origin', selectEl({ onchange: (e) => { gridState.origin = e.target.value; } },
        [['', '(all)'], 'Import', 'Forms', 'Adj', 'Calc', 'Elim'], gridState.origin)),
      povMemberFilter('Entity', entities, 'entity'),
      povMemberFilter('Account', accounts, 'account')));

  const buttons = el('div', { class: 'control-group buttons' },
    el('span', { class: 'section-label' }, 'Actions'),
    el('div', { class: 'hrow' },
      el('button', { class: 'btn primary', onclick: () => runQuery(resultBox) }, 'Run'),
      el('button', { class: 'btn', onclick: exportCSV }, 'Export CSV')));

  view.replaceChildren(el('div', { class: 'page wide' },
    el('h1', {}, 'Quick View'),
    el('div', { class: 'panel controls' },
      axisControls('Rows', gridState.rows, 'dl-row'),
      axisControls('Columns', gridState.cols, 'dl-col'),
      povControls,
      buttons),
    resultBox));
  drawGridResult(resultBox);
}

/* ---------------------------------------------------------------- metadata */

const META_DIMS = ['Entity', 'Account', 'Scenario', 'Flow', 'UD1', 'UD2', 'UD3', 'UD4'];
const ACCOUNT_TYPES = ['Revenue', 'Expense', 'Asset', 'Liability', 'Equity', 'Flow', 'NonFinancial'];
const metaState = { dim: 'Entity', selected: '', mode: 'view', addParent: '' };

function memberPropsSuffix(m) {
  const bits = [];
  if (m.currency) bits.push(m.currency);
  if (m.accountType) bits.push(m.accountType);
  if (m.weight != null && Number(m.weight) !== 1) bits.push('w=' + m.weight);
  if (m.isIC) bits.push('IC');
  if (m.dynamicCalc) bits.push('dynamic');
  if (m.ownershipPct != null && Number(m.ownershipPct) !== 100 && Number(m.ownershipPct) !== 0) {
    bits.push(m.ownershipPct + '%');
  }
  return bits.length ? el('span', { class: 'muted small' }, '  ' + bits.join(' · ')) : null;
}

async function saveMember(f, adding) {
  const dim = metaState.dim;
  const body = { weight: numOr(f.weight.value, 1) };
  if (dim === 'Account') {
    body.accountType = f.accountType.value;
    body.isIC = f.isIC.checked;
    body.dynamicCalc = f.dynamicCalc.checked;
    body.formula = f.formula.value.trim();
  }
  if (dim === 'Entity') {
    body.currency = f.currency.value.trim();
    body.ownershipPct = numOr(f.ownershipPct.value, 100);
  }
  try {
    if (adding) {
      body.name = f.name.value.trim();
      if (!body.name) {
        toast('Member name is required');
        return;
      }
      body.parent = metaState.addParent;
      await api(`/api/dims/${enc(dim)}/members`, { method: 'POST', body });
      metaState.selected = body.name;
    } else {
      await api(`/api/dims/${enc(dim)}/members/${enc(metaState.selected)}`, { method: 'PUT', body });
    }
    metaState.mode = 'view';
    invalidateMembers();
    toast('Member saved', 'ok');
    renderRoute();
  } catch (err) {
    toast(err.message);
  }
}

async function deleteMember() {
  const dim = metaState.dim, name = metaState.selected;
  if (!name) return;
  if (!confirm(`Delete "${name}" and all of its descendants?`)) return;
  try {
    await api(`/api/dims/${enc(dim)}/members/${enc(name)}?recursive=1`, { method: 'DELETE' });
    metaState.selected = '';
    metaState.mode = 'view';
    invalidateMembers();
    toast('Member deleted', 'ok');
    renderRoute();
  } catch (err) {
    toast(err.message);
  }
}

function memberFormPanel(members) {
  const dim = metaState.dim;
  const adding = metaState.mode === 'add';
  const m = adding ? null : members.find((x) => x.name === metaState.selected);
  if (!adding && !m) {
    return el('div', { class: 'panel form' },
      el('span', { class: 'section-label' }, 'Member properties'),
      emptyState('Select a member on the left, or add one.'));
  }
  const f = {
    name: el('input', { value: adding ? '' : m.name, disabled: !adding, placeholder: 'member name' }),
    weight: el('input', { type: 'number', step: 'any', value: String(adding ? 1 : (m.weight ?? 1)) }),
  };
  const rows = [
    frow('Name', f.name),
    frow('Parent', el('input', { value: adding ? (metaState.addParent || '(root)') : (m.parent || '(root)'), disabled: true })),
    frow('Weight', f.weight),
  ];
  if (dim === 'Account') {
    f.accountType = selectEl({}, [['', '(none)']].concat(ACCOUNT_TYPES), adding ? '' : (m.accountType || ''));
    f.isIC = el('input', { type: 'checkbox', checked: !adding && !!m.isIC });
    f.dynamicCalc = el('input', { type: 'checkbox', checked: !adding && !!m.dynamicCalc });
    f.formula = el('textarea', { rows: '3', placeholder: 'e.g. A#Sales - A#COGS' }, adding ? '' : (m.formula || ''));
    rows.push(
      frow('Account type', f.accountType),
      frow('Intercompany', f.isIC),
      frow('Dynamic calc', f.dynamicCalc),
      frow('Formula', f.formula));
  }
  if (dim === 'Entity') {
    f.currency = el('input', { value: adding ? '' : (m.currency || ''), placeholder: 'e.g. USD' });
    f.ownershipPct = el('input', { type: 'number', step: 'any', value: String(adding ? 100 : (m.ownershipPct ?? 100)) });
    rows.push(frow('Currency', f.currency), frow('Ownership %', f.ownershipPct));
  }
  return el('div', { class: 'panel form' },
    el('span', { class: 'section-label' },
      adding ? `New member under ${metaState.addParent || '(root)'}` : 'Member properties'),
    rows,
    el('div', { class: 'btnrow' },
      el('button', { class: 'btn primary', onclick: () => saveMember(f, adding) },
        adding ? 'Create member' : 'Save changes'),
      adding ? el('button', { class: 'btn', onclick: () => { metaState.mode = 'view'; renderRoute(); } }, 'Cancel') : null));
}

async function renderMetadata(view) {
  const members = await dimMembers(metaState.dim);
  if (metaState.selected && !members.some((m) => m.name === metaState.selected)) {
    metaState.selected = '';
  }
  const treePanel = el('div', { class: 'panel' },
    el('span', { class: 'section-label' }, metaState.dim + ' members'),
    members.length
      ? el('div', { class: 'tree' }, members.map((m) =>
          el('div', {
            class: 'tree-item' + (m.name === metaState.selected ? ' selected' : '') +
                   (isLeafMember(m, members) ? '' : ' parent'),
            style: `padding-left:${(m.depth || 0) * 16 + 8}px`,
            onclick: () => { metaState.selected = m.name; metaState.mode = 'view'; renderRoute(); },
          }, m.name, memberPropsSuffix(m))))
      : emptyState('No members in this dimension yet.'));

  view.replaceChildren(el('div', { class: 'page wide' },
    el('h1', {}, 'Metadata'),
    el('div', { class: 'hrow' },
      selectEl({
        onchange: (e) => {
          metaState.dim = e.target.value;
          metaState.selected = '';
          metaState.mode = 'view';
          renderRoute();
        },
      }, META_DIMS, metaState.dim),
      el('button', {
        class: 'btn',
        onclick: () => { metaState.mode = 'add'; metaState.addParent = ''; renderRoute(); },
      }, 'Add root'),
      el('button', {
        class: 'btn', disabled: !metaState.selected,
        onclick: () => { metaState.mode = 'add'; metaState.addParent = metaState.selected; renderRoute(); },
      }, 'Add child'),
      el('button', { class: 'btn danger', disabled: !metaState.selected, onclick: deleteMember }, 'Delete')),
    el('div', { class: 'cols' }, treePanel, memberFormPanel(members))));
}

/* ------------------------------------------------------------------- rates */

async function renderRates(view) {
  const { scenario, time } = state.pov;
  const resp = await api(`/api/rates?scenario=${enc(scenario)}&time=${enc(time)}`)
    .catch((err) => { toast(err.message); return []; });
  const have = new Map();
  for (const r of Array.isArray(resp) ? resp : []) {
    have.set(`${r.currency ?? r.Currency}|${r.type ?? r.Type}`, r.value ?? r.Value);
  }
  const currencies = state.meta.currencies;
  const page = el('div', { class: 'page' },
    el('h1', {}, 'Rates'),
    el('div', { class: 'muted' }, `${scenario} · ${time} — group-currency units per 1 unit of currency`));
  if (!currencies.length) {
    page.append(emptyState('No currencies known yet — define cubes and entity currencies first.'));
    view.replaceChildren(page);
    return;
  }
  const fields = [];
  const rateInput = (cur, type) => {
    const v = have.get(`${cur}|${type}`);
    const inp = el('input', {
      type: 'number', step: 'any', min: '0', class: 'short',
      value: v == null ? '' : String(v), placeholder: '—',
    });
    fields.push({ currency: cur, type, inp });
    return inp;
  };
  const tbl = el('table', { class: 'grid' },
    el('thead', {}, el('tr', {},
      el('th', {}, 'Currency'), el('th', {}, 'Average'), el('th', {}, 'Closing'))),
    el('tbody', {}, currencies.map((cur) => el('tr', {},
      el('th', { class: 'rowhead' }, cur),
      el('td', {}, rateInput(cur, 'Average')),
      el('td', {}, rateInput(cur, 'Closing'))))));
  const save = async () => {
    const out = [];
    for (const f of fields) {
      const raw = f.inp.value.trim();
      if (raw === '') continue;
      const v = Number(raw);
      if (!isFinite(v) || v <= 0) {
        toast(`Rate for ${f.currency} ${f.type} must be a positive number`);
        return;
      }
      out.push({ currency: f.currency, type: f.type, value: v });
    }
    try {
      await api(`/api/rates?scenario=${enc(scenario)}&time=${enc(time)}`, { method: 'PUT', body: out });
      toast('Rates saved', 'ok');
      renderRoute();
    } catch (err) {
      toast(err.message);
    }
  };
  page.append(
    el('div', { class: 'panel table-wrap' }, tbl),
    el('div', { class: 'btnrow' }, el('button', { class: 'btn primary', onclick: save }, 'Save rates')));
  view.replaceChildren(page);
}

/* ------------------------------------------------------------------ import */

const PROFILE_DIMS = ['Entity', 'Account', 'Scenario', 'Time', 'Flow', 'IC', 'UD1', 'UD2', 'UD3', 'UD4'];
const RULE_KINDS = ['exact', 'prefix', 'default'];
const importState = {
  selected: '', draft: null, editingExisting: false,
  file: null, preview: null, commit: null,
};

function normProfile(p) {
  const cols = {};
  for (const [k, v] of Object.entries(p.columns ?? p.Columns ?? {})) {
    if (v && typeof v === 'object') cols[k] = { col: Number(v.col ?? v.Col ?? -1), fixed: String(v.fixed ?? v.Fixed ?? '') };
  }
  return {
    name: String(p.name ?? p.Name ?? ''),
    cube: String(p.cube ?? p.Cube ?? ''),
    hasHeader: !!(p.hasHeader ?? p.HasHeader),
    delimiter: String(p.delimiter ?? p.Delimiter ?? ',') || ',',
    amountCol: Number(p.amountCol ?? p.AmountCol ?? 0),
    columns: cols,
    rules: (p.rules ?? p.Rules ?? []).map((r) => ({
      dim: String(r.dim ?? r.Dim ?? 'Account'),
      kind: String(r.kind ?? r.Kind ?? 'exact'),
      src: String(r.src ?? r.Src ?? ''),
      target: String(r.target ?? r.Target ?? ''),
    })),
  };
}

function normProfiles(resp) {
  let list = [];
  if (Array.isArray(resp)) list = resp;
  else if (resp && typeof resp === 'object') list = Object.values(resp);
  return list.filter((p) => p && typeof p === 'object').map(normProfile)
    .filter((p) => p.name).sort((a, b) => a.name.localeCompare(b.name));
}

function blankProfileDraft() {
  const cols = {};
  for (const d of PROFILE_DIMS) cols[d] = { col: '', fixed: '' };
  return {
    name: '', cube: state.pov.cube || '', hasHeader: true,
    delimiter: ',', amountCol: '0', columns: cols, rules: [],
  };
}

function draftFromProfile(p) {
  const d = blankProfileDraft();
  d.name = p.name;
  d.cube = p.cube;
  d.hasHeader = p.hasHeader;
  d.delimiter = p.delimiter;
  d.amountCol = String(p.amountCol);
  for (const [k, v] of Object.entries(p.columns)) {
    d.columns[k] = { col: v.col >= 0 ? String(v.col) : '', fixed: v.fixed || '' };
  }
  d.rules = p.rules.map((r) => ({ ...r }));
  return d;
}

async function saveProfile() {
  const d = importState.draft;
  const name = d.name.trim();
  if (!name) { toast('Profile name is required'); return; }
  if (!d.cube) { toast('Profile cube is required'); return; }
  const amountCol = Number(String(d.amountCol).trim());
  if (!Number.isInteger(amountCol) || amountCol < 0) {
    toast('Amount column must be a non-negative integer');
    return;
  }
  const columns = {};
  for (const [dim, c] of Object.entries(d.columns)) {
    const fixed = c.fixed.trim();
    const colRaw = String(c.col).trim();
    if (fixed !== '') {
      columns[dim] = { col: -1, fixed };
    } else if (colRaw !== '') {
      const n = Number(colRaw);
      if (!Number.isInteger(n) || n < 0) {
        toast(`Column index for ${dim} must be a non-negative integer`);
        return;
      }
      columns[dim] = { col: n, fixed: '' };
    }
  }
  const rules = d.rules.map((r) => ({
    dim: r.dim, kind: r.kind,
    src: r.kind === 'default' ? '*' : r.src.trim(),
    target: r.target.trim(),
  }));
  const body = { name, cube: d.cube, hasHeader: !!d.hasHeader, delimiter: d.delimiter || ',', columns, amountCol, rules };
  try {
    if (importState.editingExisting) {
      await api(`/api/profiles/${enc(name)}`, { method: 'PUT', body });
    } else {
      await api('/api/profiles', { method: 'POST', body });
    }
    importState.selected = name;
    importState.editingExisting = true;
    toast('Profile saved', 'ok');
    renderRoute();
  } catch (err) {
    toast(err.message);
  }
}

async function runImport(kind) {
  if (!importState.selected || !importState.file) return;
  const fd = new FormData();
  fd.append('profile', importState.selected);
  fd.append('file', importState.file, importState.file.name);
  try {
    const resp = await api(`/api/import/${kind}`, { method: 'POST', body: fd });
    if (kind === 'preview') {
      importState.preview = resp;
      importState.commit = null;
    } else {
      importState.commit = resp;
      toast('Import committed', 'ok');
    }
    renderRoute();
  } catch (err) {
    toast((kind === 'preview' ? 'Preview' : 'Commit') + ' failed: ' + err.message);
  }
}

const DIM_ORDER = PROFILE_DIMS;

function mappedRowDims(row) {
  const d = row.dims ?? row.Dims;
  if (d && typeof d === 'object') return d;
  const skip = new Set(['amount', 'value', 'line', 'sourceline', 'issues', 'source', 'raw']);
  const out = {};
  for (const [k, v] of Object.entries(row)) {
    if (typeof v === 'string' && !skip.has(k.toLowerCase())) out[k] = v;
  }
  return out;
}

function previewColumns(rows) {
  const keys = new Set();
  rows.forEach((r) => Object.keys(mappedRowDims(r)).forEach((k) => keys.add(k)));
  const ordered = [];
  for (const d of DIM_ORDER) {
    for (const k of keys) {
      if (k.toLowerCase() === d.toLowerCase()) {
        ordered.push(k);
        keys.delete(k);
        break;
      }
    }
  }
  return ordered.concat([...keys].sort());
}

function previewPanel() {
  const p = importState.preview;
  if (!p) return null;
  const rows = (p.rows ?? p.Rows ?? []).slice(0, 200);
  const issues = (p.issues ?? p.Issues ?? []).map(String);
  const cols = previewColumns(rows);
  const tbl = rows.length
    ? el('div', { class: 'table-wrap' }, el('table', { class: 'grid preview' },
        el('thead', {}, el('tr', {},
          el('th', {}, 'Line'),
          cols.map((c) => el('th', {}, c)),
          el('th', { class: 'num' }, 'Amount'),
          el('th', {}, 'Issues'))),
        el('tbody', {}, rows.map((r) => {
          const dims = mappedRowDims(r);
          const rIssues = (r.issues ?? r.Issues ?? []).map(String);
          return el('tr', { class: rIssues.length ? 'bad' : '' },
            el('td', { class: 'muted' }, String(r.line ?? r.Line ?? r.sourceLine ?? r.SourceLine ?? '')),
            cols.map((c) => el('td', {}, dims[c] ?? '')),
            el('td', { class: 'num' }, fmtNum(r.amount ?? r.Amount ?? r.value ?? r.Value)),
            el('td', { class: 'issues-cell' }, rIssues.join('; ')));
        }))))
    : emptyState('No rows were parsed from the file.');
  return el('div', { class: 'panel' },
    el('span', { class: 'section-label' }, `Preview — ${rows.length} row${rows.length === 1 ? '' : 's'}`),
    issues.length
      ? el('ul', { class: 'issues' }, issues.map((i) => el('li', {}, i)))
      : el('div', { class: 'muted hint' }, 'No global issues.'),
    tbl);
}

function commitPanel() {
  const c = importState.commit;
  if (!c) return null;
  const counts = Object.entries(typeof c === 'object' && c ? c : {})
    .filter(([, v]) => typeof v === 'number')
    .map(([k, v]) => `${k}: ${v}`);
  const issues = ((c && (c.issues ?? c.Issues)) || []).map(String);
  return el('div', { class: 'panel' },
    el('span', { class: 'section-label' }, 'Commit result'),
    el('div', {}, counts.length ? counts.join(' · ') : 'Committed.'),
    issues.length ? el('ul', { class: 'issues' }, issues.map((i) => el('li', {}, i))) : null);
}

function profileEditorPanel() {
  const d = importState.draft;
  if (!d) return null;
  const ruleTbody = el('tbody');
  const ruleRow = (r, i) => el('tr', {},
    el('td', {}, selectEl({ onchange: (e) => { r.dim = e.target.value; } }, PROFILE_DIMS, r.dim)),
    el('td', {}, selectEl({ onchange: (e) => { r.kind = e.target.value; } }, RULE_KINDS, r.kind)),
    el('td', {}, el('input', { value: r.src, placeholder: 'source (e.g. 41*)', oninput: (e) => { r.src = e.target.value; } })),
    el('td', {}, el('input', { value: r.target, placeholder: 'target member', oninput: (e) => { r.target = e.target.value; } })),
    el('td', {}, el('button', {
      class: 'btn small danger',
      onclick: () => { d.rules.splice(i, 1); refreshRules(); },
    }, 'Remove')));
  const refreshRules = () => ruleTbody.replaceChildren(...d.rules.map(ruleRow));
  refreshRules();

  return el('div', { class: 'panel form' },
    el('span', { class: 'section-label' },
      importState.editingExisting ? `Edit profile "${d.name}"` : 'New profile'),
    frow('Name', el('input', {
      value: d.name, disabled: importState.editingExisting,
      placeholder: 'profile name', oninput: (e) => { d.name = e.target.value; },
    })),
    frow('Cube', selectEl({ onchange: (e) => { d.cube = e.target.value; } },
      state.meta.cubes.length ? state.meta.cubes : [d.cube || ''], d.cube)),
    frow('Has header row', el('input', {
      type: 'checkbox', checked: d.hasHeader,
      onchange: (e) => { d.hasHeader = e.target.checked; },
    })),
    frow('Delimiter', el('input', {
      class: 'short', value: d.delimiter, maxlength: '3',
      oninput: (e) => { d.delimiter = e.target.value; },
    })),
    frow('Amount column', el('input', {
      class: 'short', type: 'number', min: '0', value: d.amountCol,
      oninput: (e) => { d.amountCol = e.target.value; },
    })),
    el('span', { class: 'section-label' }, 'Columns (index 0-based, or a fixed value)'),
    el('div', { class: 'table-wrap' }, el('table', { class: 'grid compact' },
      el('thead', {}, el('tr', {},
        el('th', {}, 'Dimension'), el('th', {}, 'Column'), el('th', {}, 'Fixed value'))),
      el('tbody', {}, PROFILE_DIMS.map((dim) => {
        const c = d.columns[dim];
        return el('tr', {},
          el('th', { class: 'rowhead' }, dim),
          el('td', {}, el('input', {
            class: 'short', type: 'number', min: '0', value: c.col, placeholder: '—',
            oninput: (e) => { c.col = e.target.value; },
          })),
          el('td', {}, el('input', {
            value: c.fixed, placeholder: 'fixed value',
            oninput: (e) => { c.fixed = e.target.value; },
          })));
      })))),
    el('span', { class: 'section-label' }, 'Transformation rules'),
    el('div', { class: 'table-wrap' }, el('table', { class: 'grid compact' },
      el('thead', {}, el('tr', {},
        el('th', {}, 'Dim'), el('th', {}, 'Kind'), el('th', {}, 'Source'), el('th', {}, 'Target'), el('th', {}, ''))),
      ruleTbody)),
    el('div', { class: 'btnrow' },
      el('button', {
        class: 'btn',
        onclick: () => { d.rules.push({ dim: 'Account', kind: 'exact', src: '', target: '' }); refreshRules(); },
      }, 'Add rule'),
      el('button', { class: 'btn primary', onclick: saveProfile },
        importState.editingExisting ? 'Save profile' : 'Create profile')));
}

async function renderImport(view) {
  const resp = await api('/api/profiles').catch((err) => { toast(err.message); return []; });
  const profiles = normProfiles(resp);
  if (importState.selected && !profiles.some((p) => p.name === importState.selected)) {
    importState.selected = '';
    importState.draft = null;
  }

  const previewBtn = el('button', { class: 'btn primary', disabled: true, onclick: () => runImport('preview') }, 'Preview');
  const commitBtn = el('button', { class: 'btn primary', disabled: true, onclick: () => runImport('commit') }, 'Commit');
  const updateBtns = () => {
    const ready = !!(importState.selected && importState.file);
    previewBtn.disabled = !ready;
    commitBtn.disabled = !ready;
  };

  const profileSel = selectEl({
    onchange: (e) => {
      importState.selected = e.target.value;
      const p = profiles.find((x) => x.name === importState.selected);
      importState.draft = p ? draftFromProfile(p) : null;
      importState.editingExisting = !!p;
      importState.preview = null;
      importState.commit = null;
      renderRoute();
    },
  }, [['', '(choose profile)']].concat(profiles.map((p) => p.name)), importState.selected);

  const fileInput = el('input', {
    type: 'file', accept: '.csv,.txt,text/csv',
    onchange: (e) => {
      importState.file = e.target.files[0] || null;
      fileLabel.textContent = importState.file ? importState.file.name : 'no file chosen';
      updateBtns();
    },
  });
  const fileLabel = el('span', { class: 'muted' }, importState.file ? importState.file.name : 'no file chosen');

  const deleteProfile = async () => {
    if (!importState.selected) return;
    if (!confirm(`Delete profile "${importState.selected}"?`)) return;
    try {
      await api(`/api/profiles/${enc(importState.selected)}`, { method: 'DELETE' });
      importState.selected = '';
      importState.draft = null;
      toast('Profile deleted', 'ok');
      renderRoute();
    } catch (err) {
      toast(err.message);
    }
  };

  const page = el('div', { class: 'page wide' },
    el('h1', {}, 'Import'),
    el('div', { class: 'panel' },
      el('span', { class: 'section-label' }, 'Profile'),
      el('div', { class: 'hrow' },
        profileSel,
        el('button', {
          class: 'btn',
          onclick: () => {
            importState.selected = '';
            importState.draft = blankProfileDraft();
            importState.editingExisting = false;
            renderRoute();
          },
        }, 'New profile'),
        el('button', { class: 'btn danger', disabled: !importState.selected, onclick: deleteProfile }, 'Delete')),
      profiles.length ? null : el('div', { class: 'muted hint' }, 'No profiles yet — create one to map CSV columns to dimensions.')),
    profileEditorPanel(),
    el('div', { class: 'panel' },
      el('span', { class: 'section-label' }, 'File'),
      el('div', { class: 'hrow' }, fileInput, fileLabel),
      el('div', { class: 'hrow', style: 'margin-top:12px' }, previewBtn, commitBtn),
      el('div', { class: 'muted hint' },
        'Preview transforms without writing; Commit replaces this unit\'s Origin=Import cells and moves workflow to Imported.')),
    previewPanel(),
    commitPanel());
  view.replaceChildren(page);
  updateBtns();
}

/* ---------------------------------------------------------------- formulas */

const formulaState = { account: '', formula: '', dynamic: false, error: '' };

function normFormulas(resp) {
  if (Array.isArray(resp)) {
    return resp.map((f) => ({
      account: String(f.account ?? f.Account ?? f.name ?? f.Name ?? ''),
      formula: String(f.formula ?? f.Formula ?? ''),
      dynamic: !!(f.dynamic ?? f.dynamicCalc ?? f.Dynamic ?? f.DynamicCalc),
    })).filter((f) => f.account);
  }
  if (resp && typeof resp === 'object') {
    return Object.entries(resp).map(([account, v]) => {
      if (typeof v === 'string') return { account, formula: v, dynamic: false };
      return {
        account,
        formula: String((v && (v.formula ?? v.Formula)) ?? ''),
        dynamic: !!(v && (v.dynamic ?? v.dynamicCalc ?? v.Dynamic ?? v.DynamicCalc)),
      };
    });
  }
  return [];
}

async function renderFormulas(view) {
  const { cube } = state.pov;
  const [resp, accounts] = await Promise.all([
    api(`/api/formulas?cube=${enc(cube)}`).catch((err) => { toast(err.message); return []; }),
    dimMembers('Account'),
  ]);
  const list = normFormulas(resp).sort((a, b) => a.account.localeCompare(b.account));

  const listPanel = el('div', { class: 'panel' },
    el('span', { class: 'section-label' }, 'Account formulas — ' + cube),
    list.length
      ? el('div', { class: 'table-wrap' }, el('table', { class: 'grid' },
          el('thead', {}, el('tr', {},
            el('th', {}, 'Account'), el('th', {}, 'Formula'), el('th', {}, 'Mode'), el('th', {}, ''))),
          el('tbody', {}, list.map((f) => el('tr', {},
            el('th', { class: 'rowhead' }, f.account),
            el('td', { class: 'mono' }, f.formula),
            el('td', {}, el('span', { class: 'chip ' + (f.dynamic ? 'st-Validated' : 'st-Imported') },
              f.dynamic ? 'dynamic' : 'stored')),
            el('td', {}, el('button', {
              class: 'btn small',
              onclick: () => {
                Object.assign(formulaState, { account: f.account, formula: f.formula, dynamic: f.dynamic, error: '' });
                renderRoute();
              },
            }, 'Edit')))))))
      : emptyState('No formulas defined for this cube.'));

  const errBox = el('div', { class: 'err-inline' }, formulaState.error);
  const formPanel = el('div', { class: 'panel form' },
    el('span', { class: 'section-label' }, 'Edit formula'),
    frow('Account', selectEl({ onchange: (e) => { formulaState.account = e.target.value; } },
      [['', '(choose account)']].concat(accounts.map((m) => m.name)), formulaState.account)),
    frow('Formula', el('textarea', {
      rows: '4', placeholder: 'e.g. IF(A#Sales == 0, 0, A#GrossProfit / A#Sales * 100)',
      oninput: (e) => { formulaState.formula = e.target.value; },
    }, formulaState.formula)),
    frow('Dynamic calc', el('input', {
      type: 'checkbox', checked: formulaState.dynamic,
      onchange: (e) => { formulaState.dynamic = e.target.checked; },
    })),
    errBox,
    el('div', { class: 'btnrow' }, el('button', {
      class: 'btn primary',
      onclick: async () => {
        if (!formulaState.account) {
          errBox.textContent = 'Choose an account first.';
          return;
        }
        try {
          await api(`/api/formulas/${enc(formulaState.account)}`, {
            method: 'PUT',
            body: { formula: formulaState.formula.trim(), dynamic: formulaState.dynamic },
          });
          formulaState.error = '';
          invalidateMembers('Account');
          toast('Formula saved', 'ok');
          renderRoute();
        } catch (err) {
          formulaState.error = err.message; // server-side parse errors shown inline
          errBox.textContent = err.message;
        }
      },
    }, 'Save formula')));

  view.replaceChildren(el('div', { class: 'page wide' },
    el('h1', {}, 'Formulas'),
    el('div', { class: 'cols' }, listPanel, formPanel)));
}

/* -------------------------------------------------------------------- init */

const ROUTES = {
  '': renderDashboard,
  'grid': renderGrid,
  'workflow': renderWorkflow,
  'metadata': renderMetadata,
  'rates': renderRates,
  'import': renderImport,
  'formulas': renderFormulas,
};

async function init() {
  try {
    await loadMeta();
  } catch (err) {
    toast('Failed to load /api/meta: ' + err.message);
  }
  ensurePOVDefaults();
  buildPOVBar();
  window.addEventListener('hashchange', renderRoute);
  renderRoute();
}

document.addEventListener('DOMContentLoaded', init);
