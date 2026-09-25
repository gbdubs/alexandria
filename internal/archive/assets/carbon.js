// Pharos CO₂ calculator. Settings gains a Carbon footprint card: estimated
// inference energy and emissions for the usage in the library, all time or
// the last 30 days. GET /api/health/carbon supplies token totals by model tier
// and token type plus the cited factors (carbon/co2_factors.json); the card
// multiplies them out so the grid, data-center overhead (PUE), and estimate
// can be changed here without a round trip. Choices persist per browser.
(() => {
  'use strict';
  const CSS = `
.carbon-card{background:var(--panel,#fff);border:1px solid var(--line,#ccc);border-radius:6px;padding:18px 20px}
.carbon-head{display:flex;flex-wrap:wrap;justify-content:space-between;align-items:flex-start;gap:12px 24px}
.carbon-hero .big{margin:2px 0}
.carbon-range{color:var(--muted,#666);font-size:var(--fs-md,13px)}
.carbon-facts{display:flex;flex-wrap:wrap;gap:4px 18px;margin-top:8px;font-size:var(--fs-md,13px)}
.carbon-facts span{white-space:nowrap}
.carbon-facts b{font-weight:650;font-variant-numeric:tabular-nums}
.carbon-segmented{display:inline-flex;border:1px solid var(--line,#ccc);border-radius:5px;overflow:hidden}
.carbon-segmented button{border:0;border-radius:0;background:none;color:var(--muted,#666);padding:4px 10px}
.carbon-segmented button+button{border-left:1px solid var(--line,#ccc)}
.carbon-segmented button[aria-pressed="true"]{background:var(--accent,#315845);color:var(--panel,#fff)}
.carbon-controls{display:flex;flex-wrap:wrap;align-items:end;gap:12px 18px;margin:16px 0 4px;padding-top:14px;border-top:1px solid var(--line,#ccc)}
.carbon-controls label{display:flex;flex-direction:column;gap:4px;color:var(--muted,#666);font-size:var(--fs-sm,12px)}
.carbon-controls select{min-width:260px;width:auto;padding:5px 28px 5px 8px}
.carbon-controls input{width:92px;padding:5px 8px;border:1px solid var(--line,#ccc);border-radius:5px;background:var(--panel,#fff);color:var(--ink,#222);font-variant-numeric:tabular-nums}
.carbon-table{width:100%;margin-top:14px;border-collapse:collapse;font-size:var(--fs-md,13px);font-variant-numeric:tabular-nums}
.carbon-table caption{text-align:left;font-weight:650;padding-bottom:4px}
.carbon-table th,.carbon-table td{padding:6px 0 6px 14px;text-align:right;white-space:nowrap;border-bottom:1px solid color-mix(in srgb,var(--line,#ccc) 60%,transparent)}
.carbon-table th:first-child,.carbon-table td:first-child{padding-left:0;text-align:left}
.carbon-table thead th{color:var(--muted,#666);font-size:var(--fs-sm,12px);font-weight:600;border-bottom-color:var(--line,#ccc)}
.carbon-table tfoot td{font-weight:650;border-bottom:0}
.carbon-table td.carbon-share{width:30%;min-width:90px}
.carbon-bar{display:block;height:8px;border-radius:0 4px 4px 0;background:var(--accent,#315845);min-width:1px}
.carbon-sub{display:block;color:var(--muted,#666);font-size:var(--fs-xs,11px);font-weight:400}
.carbon-note{margin:10px 0 0;color:var(--muted,#666);font-size:var(--fs-sm,12px)}
.carbon-method{margin-top:14px;border-top:1px solid var(--line,#ccc);padding-top:10px;font-size:var(--fs-md,13px)}
.carbon-method summary{cursor:pointer;color:var(--accent,#315845)}
.carbon-method h4{margin:14px 0 4px;font-size:var(--fs-sm,12px);letter-spacing:.05em;text-transform:uppercase;color:var(--muted,#666)}
.carbon-method p,.carbon-method li{max-width:860px;line-height:1.45}
.carbon-method ul,.carbon-method ol{margin:4px 0;padding-left:20px}
.carbon-method code{font:12px ui-monospace,SFMono-Regular,monospace}
.carbon-method .carbon-table td{vertical-align:top}
.carbon-method .carbon-table td.carbon-basis{white-space:normal;text-align:left;min-width:280px}
.carbon-method .carbon-table td:first-child{white-space:normal}
.carbon-cite{font-size:var(--fs-xs,11px);vertical-align:super;margin-left:1px}
@media(max-width:760px){.carbon-table td.carbon-share{display:none}.carbon-controls select{min-width:0;width:100%}}`;

  const CATEGORY_LABELS = {
    uncached_input: ['Uncached input', 'Prompt tokens processed in full'],
    cache_write: ['Cache writes', 'Input processed and stored for reuse'],
    cache_read: ['Cache reads', 'Input served from a prompt cache'],
    output: ['Output', 'Generated tokens, including reasoning'],
  };
  const SCENARIOS = [['low', 'Low'], ['central', 'Central'], ['high', 'High']];
  const WINDOWS = [['all', 'All time'], ['30d', 'Last 30 days']];
  const STORE = 'alexandria-carbon-v1';

  const node = (tag, cls, text) => {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = text;
    return n;
  };
  const load = () => { try { return JSON.parse(localStorage.getItem(STORE)) || {}; } catch { return {}; } };
  const save = () => { try { localStorage.setItem(STORE, JSON.stringify(prefs)); } catch {} };
  const prefs = load();
  let data = null, error = '', loading = 0;

  // Three significant figures, with the unit stepped so the number stays short.
  const sig = value => {
    const n = Number(value) || 0;
    if (n === 0) return '0';
    const abs = Math.abs(n);
    return Number(n.toPrecision(abs >= 1 ? 3 : 2)).toLocaleString(undefined, {maximumFractionDigits: 3});
  };
  const mass = grams => {
    const g = Number(grams) || 0;
    if (g >= 1e6) return `${sig(g / 1e6)} t`;
    if (g >= 1e3) return `${sig(g / 1e3)} kg`;
    return `${sig(g)} g`;
  };
  const energy = wh => {
    const w = Number(wh) || 0;
    if (w >= 1e6) return `${sig(w / 1e6)} MWh`;
    if (w >= 1e3) return `${sig(w / 1e3)} kWh`;
    return `${sig(w)} Wh`;
  };
  const tokens = value => {
    const n = Number(value) || 0;
    for (const [size, unit] of [[1e12, 'T'], [1e9, 'B'], [1e6, 'M'], [1e3, 'k']]) if (n >= size) return `${sig(n / size)}${unit}`;
    return n.toLocaleString();
  };
  const pick = (range, scenario) => Number(range?.[scenario]) || 0;
  // Tiers are listed in match order; show them smallest first.
  const bySize = tiers => [...tiers].sort((a, b) => pick(a.output_wh_per_mtok, 'central') - pick(b.output_wh_per_mtok, 'central'));

  // The same arithmetic as carbon.go: tokens ÷ 1M × tier Wh per million
  // output tokens × the token type's ratio to output, then × PUE × grid.
  function calculate(factors, period, scenario, pue, grid) {
    const tiers = Object.fromEntries(factors.tiers.map(tier => [tier.key, tier]));
    const ratio = category => category === 'output' ? 1 : pick(factors.ratios[category], scenario);
    const byCategory = {}, byTier = {};
    let total = 0;
    for (const category of Object.keys(CATEGORY_LABELS)) byCategory[category] = {tokens: 0, wh: 0};
    for (const [tierKey, counts] of Object.entries(period.tokens || {})) {
      const perMillion = pick(tiers[tierKey]?.output_wh_per_mtok, scenario);
      const row = byTier[tierKey] = {tokens: 0, wh: 0};
      for (const category of Object.keys(CATEGORY_LABELS)) {
        const count = Number(counts[category]) || 0, wh = count / 1e6 * perMillion * ratio(category);
        byCategory[category].tokens += count;
        byCategory[category].wh += wh;
        row.tokens += count;
        row.wh += wh;
        total += wh;
      }
    }
    const grams = wh => wh * pue * grid / 1000;
    return {byCategory, byTier, pue, wh: total, facilityWh: total * pue, grams, co2: grams(total)};
  }

  function settings(factors) {
    const scenario = SCENARIOS.some(([key]) => key === prefs.scenario) ? prefs.scenario : 'central';
    const grid = factors.grids.find(item => item.key === prefs.grid) || (prefs.grid === 'custom' ? null : factors.grids.find(item => item.key === factors.default_grid));
    const gridValue = grid ? grid.g_co2e_per_kwh : Math.max(0, Number(prefs.customGrid) || 0);
    const customPue = Number(prefs.pue);
    const pue = customPue >= 1 ? customPue : pick(factors.pue, scenario);
    return {scenario, grid, gridValue, pue, customPue: customPue >= 1};
  }

  function segmented(label, options, current, onChange) {
    const group = node('div', 'carbon-segmented');
    group.setAttribute('role', 'group');
    group.setAttribute('aria-label', label);
    for (const [key, text] of options) {
      const choice = node('button', '', text);
      choice.type = 'button';
      choice.setAttribute('aria-pressed', String(key === current));
      choice.onclick = () => onChange(key);
      group.append(choice);
    }
    return group;
  }

  function cite(factors, ids) {
    const span = node('span', 'carbon-cite');
    for (const id of ids || []) {
      const index = factors.sources.findIndex(source => source.id === id);
      if (index < 0) continue;
      const link = node('a', '', `[${index + 1}]`);
      link.href = factors.sources[index].url;
      link.target = '_blank';
      link.rel = 'noreferrer';
      link.title = factors.sources[index].title;
      span.append(link);
    }
    return span;
  }

  function breakdownTable(caption, rows, result, labels) {
    const table = node('table', 'carbon-table'), head = node('tr'), body = node('tbody'), foot = node('tr');
    table.append(node('caption', '', caption));
    for (const text of ['', 'Tokens', 'Energy', 'CO₂e', 'Share']) head.append(node('th', '', text));
    table.createTHead().append(head);
    for (const [key, row] of rows) {
      const tr = node('tr'), name = node('td', '', labels[key]?.[0] || key), share = node('td', 'carbon-share'), bar = node('span', 'carbon-bar');
      if (labels[key]?.[1]) name.append(node('span', 'carbon-sub', labels[key][1]));
      const fraction = result.wh ? row.wh / result.wh : 0;
      bar.style.width = `${(fraction * 100).toFixed(1)}%`;
      share.title = `${(fraction * 100).toFixed(1)}% of estimated energy`;
      share.append(bar);
      tr.append(name, node('td', '', tokens(row.tokens)), node('td', '', energy(row.wh * result.pue)), node('td', '', mass(result.grams(row.wh))), share);
      body.append(tr);
    }
    table.append(body);
    const total = rows.reduce((sum, [, row]) => sum + row.tokens, 0);
    foot.append(node('td', '', 'Total'), node('td', '', tokens(total)), node('td', '', energy(result.facilityWh)), node('td', '', mass(result.co2)), node('td'));
    table.createTFoot().append(foot);
    return table;
  }

  function methodology(factors, result, setup) {
    const details = node('details', 'carbon-method');
    details.open = Boolean(prefs.methodOpen);
    details.ontoggle = () => { prefs.methodOpen = details.open; save(); };
    details.append(node('summary', '', 'How this is calculated, and sources'));
    const formula = node('p');
    formula.append('For each model tier and token type: ', node('code', '', 'tokens ÷ 1M × Wh per 1M output tokens × token-type ratio × PUE × grid g CO₂e/kWh'),
      '. Energy shown includes data-center overhead (PUE). The estimate switch uses every factor’s low, central, or high value together; the grid and PUE can be set separately.');
    details.append(node('h4', '', 'Formula'), formula);

    details.append(node('h4', '', 'Model tiers · server energy per 1M output tokens'));
    const tiers = node('table', 'carbon-table'), tierHead = node('tr'), tierBody = node('tbody');
    for (const text of ['Tier', 'Low', 'Central', 'High', 'Basis']) tierHead.append(node('th', '', text));
    tiers.createTHead().append(tierHead);
    for (const tier of bySize(factors.tiers)) {
      const tr = node('tr'), name = node('td', '', tier.label), basis = node('td', 'carbon-basis', tier.basis);
      name.append(node('span', 'carbon-sub', `Matches: ${tier.match.join(', ')}${tier.key === factors.default_tier ? ' · and unrecognized models' : ''}`));
      basis.append(cite(factors, tier.sources));
      tr.append(name, ...['low', 'central', 'high'].map(key => node('td', '', `${sig(tier.output_wh_per_mtok[key])} Wh`)), basis);
      tierBody.append(tr);
    }
    tiers.append(tierBody);
    details.append(tiers);

    details.append(node('h4', '', 'Token-type ratios · energy per token relative to output'));
    const ratios = node('table', 'carbon-table'), ratioHead = node('tr'), ratioBody = node('tbody');
    for (const text of ['Token type', 'Low', 'Central', 'High', 'Basis']) ratioHead.append(node('th', '', text));
    ratios.createTHead().append(ratioHead);
    const ratioRows = [...Object.keys(CATEGORY_LABELS).filter(key => factors.ratios[key]).map(key => [CATEGORY_LABELS[key][0], factors.ratios[key]]),
      ['PUE (data-center overhead)', factors.pue]];
    for (const [label, ratio] of ratioRows) {
      const tr = node('tr'), basis = node('td', 'carbon-basis', ratio.basis);
      basis.append(cite(factors, ratio.sources));
      tr.append(node('td', '', label), ...['low', 'central', 'high'].map(key => node('td', '', label.startsWith('PUE') ? sig(ratio[key]) : `${sig(ratio[key])}×`)), basis);
      ratioBody.append(tr);
    }
    ratios.append(ratioBody);
    details.append(ratios);

    details.append(node('h4', '', 'Grid carbon intensity'));
    const grids = node('ul');
    for (const grid of factors.grids) {
      const item = node('li');
      item.append(node('strong', '', `${grid.label}: ${sig(grid.g_co2e_per_kwh)} g CO₂e/kWh. `), grid.basis, cite(factors, grid.sources));
      grids.append(item);
    }
    details.append(grids);

    if (factors.caveats?.length) {
      details.append(node('h4', '', 'Caveats'));
      const list = node('ul');
      for (const caveat of factors.caveats) list.append(node('li', '', caveat));
      details.append(list);
    }

    details.append(node('h4', '', `Sources · retrieved ${factors.retrieved_at}`));
    const sources = node('ol');
    for (const source of factors.sources) {
      const item = node('li'), link = node('a', '', source.title);
      link.href = source.url;
      link.target = '_blank';
      link.rel = 'noreferrer';
      item.append(link, source.published ? ` (${source.published})` : '', `: ${source.finding}`);
      sources.append(item);
    }
    details.append(sources);
    return details;
  }

  function render() {
    const root = document.getElementById('carbonCard');
    if (!root) return;
    if (!data) {
      root.replaceChildren(node('div', 'meta', 'Estimated CO₂e'), node('div', 'big', error ? 'Unavailable' : '…'));
      if (error) root.append(node('div', 'meta health-detail', error));
      root.setAttribute('aria-busy', String(!error));
      return;
    }
    root.removeAttribute('aria-busy');
    // Keep focus on the control that was just used across the rebuild.
    const focused = document.activeElement && root.contains(document.activeElement) ? document.activeElement.dataset.carbonControl : null;
    const factors = data.factors, windowKey = WINDOWS.some(([key]) => key === prefs.window) ? prefs.window : 'all';
    const period = data.windows.find(item => item.key === windowKey) || data.windows[0];
    const setup = settings(factors);
    const result = calculate(factors, period, setup.scenario, setup.pue, setup.gridValue);
    const range = SCENARIOS.map(([key]) => {
      const pue = setup.customPue ? setup.pue : pick(factors.pue, key);
      return calculate(factors, period, key, pue, setup.gridValue).co2;
    });
    const update = change => { Object.assign(prefs, change); save(); render(); };

    const head = node('div', 'carbon-head'), hero = node('div', 'carbon-hero');
    hero.append(node('div', 'meta', `Estimated CO₂e · ${WINDOWS.find(([key]) => key === windowKey)[1].toLowerCase()}`),
      node('div', 'big', `${mass(result.co2)} CO₂e`),
      node('div', 'carbon-range', `Range ${mass(range[0])} – ${mass(range[2])} (low–high estimate)`));
    const facts = node('div', 'carbon-facts');
    const fact = (value, text) => { const span = node('span'); span.append(node('b', '', value), ` ${text}`); facts.append(span); };
    fact(energy(result.facilityWh), 'of electricity');
    for (const equivalent of factors.equivalents || []) fact(`≈ ${sig(result.co2 / equivalent.g_co2e_per_unit)}`, equivalent.label);
    hero.append(facts);
    head.append(hero, segmented('Period', WINDOWS, windowKey, key => update({window: key})));
    root.replaceChildren(head);

    const controls = node('div', 'carbon-controls');
    const gridLabel = node('label', '', 'Electricity grid'), gridSelect = node('select');
    gridSelect.dataset.carbonControl = 'grid';
    for (const grid of factors.grids) {
      const option = node('option', '', `${grid.label} · ${sig(grid.g_co2e_per_kwh)} g/kWh`);
      option.value = grid.key;
      gridSelect.append(option);
    }
    const custom = node('option', '', 'Custom…');
    custom.value = 'custom';
    gridSelect.append(custom);
    gridSelect.value = setup.grid ? setup.grid.key : 'custom';
    gridSelect.onchange = () => update({grid: gridSelect.value, customGrid: gridSelect.value === 'custom' ? (prefs.customGrid || setup.gridValue) : prefs.customGrid});
    gridLabel.append(gridSelect);
    controls.append(gridLabel);
    if (!setup.grid) {
      const customLabel = node('label', '', 'g CO₂e per kWh'), input = node('input');
      input.type = 'number';
      input.min = '0';
      input.step = '1';
      input.value = String(setup.gridValue);
      input.dataset.carbonControl = 'customGrid';
      input.onchange = () => update({customGrid: Math.max(0, Number(input.value) || 0)});
      customLabel.append(input);
      controls.append(customLabel);
    }
    const pueLabel = node('label', '', 'PUE'), pueInput = node('input');
    pueInput.type = 'number';
    pueInput.min = '1';
    pueInput.step = '0.01';
    pueInput.value = setup.pue.toFixed(2);
    pueInput.placeholder = pick(factors.pue, setup.scenario).toFixed(2);
    pueInput.title = setup.customPue ? 'Custom PUE; clear to follow the estimate' : 'Follows the estimate; type a value to fix it';
    pueInput.dataset.carbonControl = 'pue';
    pueInput.onchange = () => { const value = Number(pueInput.value); update({pue: value >= 1 ? value : null}); };
    pueLabel.append(pueInput);
    const scenarioLabel = node('label', '', 'Estimate');
    scenarioLabel.append(segmented('Estimate', SCENARIOS, setup.scenario, key => update({scenario: key})));
    controls.append(pueLabel, scenarioLabel);
    root.append(controls);

    const categoryRows = Object.keys(CATEGORY_LABELS).map(key => [key, result.byCategory[key]]);
    root.append(breakdownTable('By token type', categoryRows, result, CATEGORY_LABELS));
    const tierLabels = Object.fromEntries(factors.tiers.map(tier => [tier.key, [tier.label, `${sig(pick(tier.output_wh_per_mtok, setup.scenario))} Wh per 1M output tokens`]]));
    const tierRows = bySize(factors.tiers).filter(tier => result.byTier[tier.key]).map(tier => [tier.key, result.byTier[tier.key]]);
    if (tierRows.length) root.append(breakdownTable('By model tier', tierRows, result, tierLabels));

    const unmatched = data.default_tier_models || [];
    if (unmatched.length) {
      const tier = factors.tiers.find(item => item.key === factors.default_tier);
      root.append(node('p', 'carbon-note', `Counted as ${tier?.label || factors.default_tier}: ${unmatched.slice(0, 4).map(model => `${model.model} (${tokens(model.tokens)})`).join(', ')}${unmatched.length > 4 ? `, and ${unmatched.length - 4} more` : ''}.`));
    }
    root.append(node('p', 'carbon-note', 'An estimate of inference only, not a measurement: providers do not publish per-token energy. Training, embodied hardware emissions, water, and your own computer are not included.'));
    root.append(methodology(factors, result, setup));
    if (focused) root.querySelector(`[data-carbon-control="${focused}"]`)?.focus();
  }

  async function refresh() {
    const request = ++loading;
    try {
      const response = await fetch('/api/health/carbon', {headers: {'Content-Type': 'application/json'}});
      const body = await response.json().catch(() => ({}));
      if (!response.ok) throw new Error(body.error || response.statusText);
      if (request !== loading) return;
      data = body;
      error = '';
    } catch (failure) {
      if (request !== loading) return;
      if (!data) error = `Could not estimate emissions: ${failure.message}`;
    }
    render();
  }

  function mount() {
    const health = document.getElementById('health');
    if (!health || document.getElementById('carbon')) return;
    const section = node('section', 'settings-section');
    section.id = 'carbon';
    section.dataset.feedbackLabel = 'Carbon footprint';
    const heading = node('div', 'view-heading'), intro = node('div');
    intro.append(node('h2', '', 'Carbon footprint'), node('p', 'muted', 'Estimated electricity and CO₂e behind the tokens in this library, from published per-token energy measurements.'));
    heading.append(intro);
    const card = node('div', 'carbon-card');
    card.id = 'carbonCard';
    section.append(heading, card);
    health.after(section);
    render();
  }

  const visible = () => document.getElementById('settings')?.classList.contains('active');
  const sheet = node('style');
  sheet.textContent = CSS;
  document.head.append(sheet);
  mount();
  const settingsView = document.getElementById('settings');
  if (settingsView) new MutationObserver(() => { if (visible()) refresh(); }).observe(settingsView, {attributes: true, attributeFilter: ['class']});
  if (visible()) refresh();
  window.pharosCarbon = {refresh, calculate};
})();
