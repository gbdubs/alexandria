// Pharos CO₂ calculator for Usage → Carbon Impact: estimated
// inference energy and emissions for the usage in the library, all time or
// the last 30 days. GET /api/health/carbon supplies token totals by model tier
// and token type plus the cited factors (carbon/co2_factors.json); the card
// multiplies them out so the grid, data-center overhead (PUE), and estimate
// can be changed here without a round trip. Choices persist per browser.
(() => {
  'use strict';
  const CSS = `
.carbon-card{width:100%;box-sizing:border-box;background:var(--panel,#fff);border:1px solid var(--line,#ccc);border-radius:6px;padding:18px 20px}
.carbon-head{width:100%}
.carbon-hero{min-width:0}
.carbon-title-row{display:flex;flex-wrap:wrap;justify-content:space-between;align-items:center;gap:8px 24px}
.carbon-title{min-width:0}
.carbon-hero .big{margin:2px 0}
.carbon-facts{display:flex;flex-wrap:wrap;align-items:center;gap:4px 14px;margin-top:12px;font-size:var(--fs-md,13px)}
.carbon-facts span{white-space:nowrap}
.carbon-facts b{font-weight:650;font-variant-numeric:tabular-nums}
.carbon-comparison{display:inline-flex;align-items:center;gap:4px;white-space:nowrap}
.carbon-comparison b{font-weight:650;font-variant-numeric:tabular-nums}
.carbon-comparison select{width:50px;min-width:0;padding:0 15px 0 3px;border:1px solid var(--line,#ccc);border-radius:5px;background-color:var(--panel,#fff);background-position:calc(100% - 9px) center,calc(100% - 5px) center;background-size:4px 4px;color:var(--ink,#222);font:inherit}
.carbon-comparison select[data-carbon-control="vehicle"]{width:110px}
.carbon-comparison .carbon-info-button{margin-left:2px}
.carbon-segmented{display:inline-flex;border:1px solid var(--line,#ccc);border-radius:5px;overflow:hidden}
.carbon-segmented button{border:0;border-radius:0;background:none;color:var(--muted,#666);padding:4px 10px}
.carbon-segmented button+button{border-left:1px solid var(--line,#ccc)}
.carbon-segmented button[aria-pressed="true"]{background:var(--accent,#315845);color:var(--panel,#fff)}
.carbon-controls{display:flex;flex-wrap:wrap;align-items:end;gap:12px 18px;margin:16px 0 4px;padding-top:14px;border-top:1px solid var(--line,#ccc)}
.carbon-controls label,.carbon-control{display:flex;flex-direction:column;gap:4px;color:var(--muted,#666);font-size:var(--fs-sm,12px)}
.carbon-controls select{min-width:260px;width:auto;padding:5px 28px 5px 8px;border:1px solid var(--line,#ccc);background-color:var(--panel,#fff);color:var(--ink,#222)}
.carbon-controls input{width:92px;padding:5px 8px;border:1px solid var(--line,#ccc);border-radius:5px;background:var(--panel,#fff);color:var(--ink,#222);font-variant-numeric:tabular-nums}
.carbon-control-title{display:inline-flex;align-items:center;gap:5px;min-height:23px;font-weight:600;color:var(--ink,#222)}
.carbon-info-button{display:inline-flex;align-items:center;justify-content:center;flex:none;width:23px;height:23px;padding:2px;border:1px solid transparent;border-radius:50%;background:transparent;color:var(--muted,#666);cursor:pointer}
.carbon-info-button:hover{border-color:var(--line,#ccc);background:color-mix(in srgb,var(--accent,#315845) 9%,var(--panel,#fff));color:var(--accent,#315845)}
.carbon-info-button:focus-visible{outline:2px solid var(--accent,#315845);outline-offset:2px}
.carbon-info-button svg{width:16px;height:16px;fill:none;stroke:currentColor;stroke-width:1.7;stroke-linecap:round;stroke-linejoin:round}
.carbon-info-dialog{width:min(640px,calc(100vw - 32px));max-height:min(82vh,760px);padding:0;border:1px solid var(--line,#ccc);border-radius:12px;background:var(--panel,#fff);color:var(--ink,#222);box-shadow:0 20px 60px #0005;overflow:auto}
.carbon-info-dialog::backdrop{background:rgba(14,25,22,.55)}
.carbon-info-head{display:flex;align-items:flex-start;justify-content:space-between;gap:16px;padding:24px 26px 16px;border-bottom:1px solid var(--line,#ccc)}
.carbon-info-eyebrow{margin:0 0 5px;color:var(--accent,#315845);font-size:11px;font-weight:750;letter-spacing:.09em;text-transform:uppercase}
.carbon-info-head h2{margin:0;font-size:21px;line-height:1.2}
.carbon-info-close{flex:none;min-width:32px;min-height:32px;border:1px solid var(--line,#ccc);border-radius:6px;background:var(--panel,#fff);color:var(--ink,#222);font:inherit;font-size:23px;line-height:1;cursor:pointer}
.carbon-info-close:hover{color:var(--accent,#315845)}
.carbon-info-close:focus-visible{outline:2px solid var(--accent,#315845);outline-offset:2px}
.carbon-info-body{padding:20px 26px 26px;font-size:var(--fs-md,13px);line-height:1.55}
.carbon-info-lead{margin:0 0 18px;font-size:15px;line-height:1.5}
.carbon-info-section{margin-top:18px}
.carbon-info-section h3{margin:0 0 5px;font-size:13px}
.carbon-info-section p{margin:0;color:var(--ink,#222)}
.carbon-info-example{margin-top:18px;padding:13px 15px;border-left:3px solid var(--accent,#315845);border-radius:0 6px 6px 0;background:color-mix(in srgb,var(--accent,#315845) 7%,var(--panel,#fff));font-variant-numeric:tabular-nums}
.carbon-info-sources{margin-top:20px;padding-top:14px;border-top:1px solid var(--line,#ccc);color:var(--muted,#666)}
.carbon-info-sources a{color:var(--accent,#315845)}
.carbon-table{width:100%;margin-top:14px;border-collapse:collapse;font-size:var(--fs-md,13px);font-variant-numeric:tabular-nums}
.carbon-table caption{text-align:left;font-weight:650;padding-bottom:4px}
.carbon-table th,.carbon-table td{padding:6px 0 6px 14px;text-align:right;white-space:nowrap;border-bottom:1px solid color-mix(in srgb,var(--line,#ccc) 60%,transparent)}
.carbon-table th:first-child,.carbon-table td:first-child{padding-left:0;text-align:left}
.carbon-table thead th{color:var(--muted,#666);font-size:var(--fs-sm,12px);font-weight:600;border-bottom-color:var(--line,#ccc)}
.carbon-table tfoot td{font-weight:650;border-bottom:0}
.carbon-table .carbon-share{width:30%;min-width:120px}
.carbon-share-content{display:flex;align-items:center;gap:8px}
.carbon-share-track{flex:1;min-width:0}
.carbon-share-percent{min-width:3.5em;text-align:right}
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
@media(max-width:760px){.carbon-table .carbon-share{display:none}.carbon-controls select{min-width:0;width:100%}}`;

  const CATEGORY_LABELS = {
    uncached_input: ['Uncached input', 'Prompt tokens processed in full'],
    cache_write: ['Cache writes', 'Input processed and stored for reuse'],
    cache_read: ['Cache reads', 'Input served from a prompt cache'],
    output: ['Output', 'Generated tokens, including reasoning'],
  };
  const SCENARIOS = [['low', 'Low'], ['central', 'Central'], ['high', 'High']];
  const WINDOWS = [['all', 'All time'], ['30d', 'Last 30 days']];
  const INPUT_SENSITIVITIES = [['current', 'Labs'], ['third', 'Watershed']];
  const VEHICLES = [['car_miles', 'Gas car'], ['car_hybrid', 'Hybrid'], ['car_electric', 'Electric car'], ['car_pickup', 'Pickup']];
  // Representative airport locations, rounded to four decimals (FAA airport reference points).
  const CITIES = [
    {key: 'lax', label: 'LA · LAX', lat: 33.9425, lon: -118.4081},
    {key: 'sfo', label: 'SF · SFO', lat: 37.6188, lon: -122.3750},
    {key: 'den', label: 'Denver · DEN', lat: 39.8561, lon: -104.6737},
    {key: 'jfk', code: 'NYC', label: 'NYC · JFK', lat: 40.6413, lon: -73.7781},
    {key: 'dca', label: 'DC · DCA', lat: 38.8512, lon: -77.0402},
    {key: 'bos', label: 'Boston · BOS', lat: 42.3656, lon: -71.0096},
  ];
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
    return Number(n.toPrecision(abs >= 1 ? 3 : 2)).toLocaleString(undefined, {maximumFractionDigits: 6});
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
  const equivalent = (factors, key) => factors.equivalents?.find(item => item.key === key);
  function flightMiles(from, to) {
    const radians = degrees => degrees * Math.PI / 180;
    const a = radians(from.lat), b = radians(to.lat), dLat = b - a, dLon = radians(to.lon - from.lon);
    return 3958.8 * 2 * Math.asin(Math.sqrt(Math.sin(dLat / 2) ** 2 + Math.cos(a) * Math.cos(b) * Math.sin(dLon / 2) ** 2));
  }

  // The same arithmetic as carbon.go: tokens ÷ 1M × tier Wh per million
  // output tokens × the token type's ratio to output, then × PUE × grid.
  function calculate(factors, period, scenario, pue, grid, inputScale = 1) {
    const tiers = Object.fromEntries(factors.tiers.map(tier => [tier.key, tier]));
    const ratio = category => category === 'output' ? 1 : pick(factors.ratios[category], scenario) * inputScale;
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
    const pueScenario = SCENARIOS.some(([key]) => key === prefs.pueScenario) ? prefs.pueScenario : 'central';
    const inputSensitivity = INPUT_SENSITIVITIES.some(([key]) => key === prefs.inputSensitivity) ? prefs.inputSensitivity : 'current';
    const grid = factors.grids.find(item => item.key === prefs.grid) || (prefs.grid === 'custom' ? null : factors.grids.find(item => item.key === factors.default_grid));
    const gridValue = grid ? grid.g_co2e_per_kwh : Math.max(0, Number(prefs.customGrid) || 0);
    return {scenario, pueScenario, inputSensitivity, inputScale: inputSensitivity === 'third' ? 5 / 3 : 1,
      grid, gridValue, pue: pick(factors.pue, pueScenario)};
  }

  function segmented(label, options, current, onChange) {
    const group = node('div', 'carbon-segmented');
    group.setAttribute('role', 'group');
    group.setAttribute('aria-label', label);
    for (const [key, text] of options) {
      const choice = node('button', '', text);
      choice.type = 'button';
      choice.dataset.carbonControl = label.toLowerCase();
      choice.dataset.carbonValue = key;
      choice.setAttribute('aria-pressed', String(key === current));
      choice.onclick = () => onChange(key);
      group.append(choice);
    }
    return group;
  }

  const INFO = {
    period: {
      title: 'Which usage is counted?', lead: 'Choose the full usage history in this library or the most recent 30 local calendar days.',
      sections: [
        ['What changes', 'Only the token counts change. The model factors, grid, PUE, and prefill ratio stay as you selected them.'],
        ['What is included', 'The card uses reconciled usage from indexed agent sessions and removes linked mirror workspaces. Activity without reported token usage cannot be estimated.'],
      ],
    },
    grid: {
      title: 'Electricity grid', lead: 'The grid factor converts facility electricity into estimated operational CO₂e.',
      sections: [
        ['The calculation', 'Facility kWh × grid grams CO₂e per kWh gives grams CO₂e. Changing the grid changes emissions, not electricity use.'],
        ['Location and accounting', 'The default US average is a location-based proxy because the serving region is generally undisclosed. Regional factors describe the electricity mix in a place; the Google contracts option is a market-based example and should only be used for comparable procurement.'],
      ],
      example: 'At 350 g CO₂e/kWh, 1 kWh of facility electricity gives 350 g CO₂e.',
      sources: [['EPA eGRID summary data', 'https://www.epa.gov/egrid/summary-data'], ['Watershed on grid accounting', 'https://watershed.com/en-GB/blog/ai-emissions-by-region']],
    },
    customGrid: {
      title: 'Custom grid intensity', lead: 'Enter a carbon intensity in grams of CO₂e per kilowatt-hour of electricity.',
      sections: [
        ['Use a documented factor', 'A location-based figure should match the actual serving region where possible. A market-based figure should reflect the provider’s qualifying electricity contracts. This app cannot determine either from token logs.'],
      ],
      sources: [['EPA eGRID summary data', 'https://www.epa.gov/egrid/summary-data']],
    },
    pue: {
      title: 'Data-center overhead (PUE)', lead: 'Power usage effectiveness is facility electricity divided by IT equipment electricity.',
      sections: [
        ['Why it matters', 'A PUE of 1.15 adds 15% to the estimated server energy for cooling, power distribution, and other facility overhead. It is applied after token energy is calculated.'],
        ['The choices', 'Low, central, and high come from the cited data-center factors. This setting is independent of the estimate range so you can test overhead separately.'],
      ],
      example: '100 Wh of server electricity × 1.15 PUE = 115 Wh at the facility.',
      sources: [['Google data-center efficiency', 'https://datacenters.google/efficiency/']],
    },
    estimate: {
      title: 'Energy estimate', lead: 'Select the low, central, or high published-model assumptions used for token energy.',
      sections: [
        ['What changes', 'The switch changes model-tier energy and token-type ratios together.'],
        ['How to read the range', 'The low–high span combines the factor extremes. It is a sensitivity range, not a statistical confidence interval or a direct measurement of the provider’s servers.'],
      ],
    },
    inputSensitivity: {
      title: 'Prefill ratio',
      sections: [
        ['What it measures', 'Prefill processes fresh prompt tokens, often in parallel; decode generates output tokens sequentially. At the central estimate, Labs treats one output token as using 5× the energy of one fresh input token. Watershed uses 3×. The ratio can vary with model, context length, and serving conditions.'],
        ['Labs · 5:1', 'This estimate uses Anthropic’s published input-to-output API price ratio as an energy proxy. Prices can reflect different serving costs, but also include commercial choices; they are not direct electricity measurements.'],
        ['Watershed · 3:1', 'Watershed’s Activity Tier lists 0.32 Wh per 1,000 input tokens and 0.96 Wh per 1,000 output tokens. It derives its input factor from Google’s measured typical prompt energy, an assumed prompt length, and a 3:1 decode-to-prefill ratio. Google did not publish that prompt’s token split, so the per-token factors remain modeled estimates.'],
        ['Why both are useful', 'The Labs proxy follows a provider’s observed pricing; Watershed starts from published prompt electricity data and a phase-energy model. Neither directly measures per-token energy for the proprietary models in this card. Selecting either ratio keeps output energy and the relative cache discounts fixed; the low and high estimates retain their spread.'],
      ],
      example: 'In this card, a large model at Central counts fresh input at 180 Wh per million tokens with Labs or 300 Wh with Watershed. Cache reads are 18 or 30 Wh per million tokens, respectively; output remains 900 Wh.',
      sources: [['Watershed AI emissions framework, Table 2', 'https://cdn.sanity.io/files/3ogo9b9g/production/a5c1f64ca5864e61b47e6384ee4d0ed31bc861f4.pdf#page=24'], ['Anthropic pricing', 'https://platform.claude.com/docs/en/about-claude/pricing']],
    },
    vehicle: {
      title: 'Vehicle comparison', lead: 'This comparison divides the current CO₂e estimate by emissions per mile for the chosen vehicle.',
      sections: [
        ['What changes', 'Only the comparison in miles changes. It does not change the AI electricity or CO₂e estimate.'],
        ['Boundaries', 'Gasoline examples use stated miles-per-gallon assumptions. The electric-car example uses a fixed US-average grid rather than the grid selected for AI. Vehicle manufacture and upstream fuel emissions are excluded.'],
      ],
      sources: [['EPA greenhouse-gas equivalencies', 'https://www.epa.gov/energy/greenhouse-gas-equivalencies-calculator-calculations-and-references'], ['EPA electric-vehicle comparison', 'https://www.epa.gov/greenvehicles/comparison-your-car-vs-electric-vehicle']],
    },
    flight: {
      title: 'Flight comparison', lead: 'Choose airports to compare the current CO₂e estimate with one-way economy passenger flights.',
      sections: [
        ['How it works', 'The app measures approximate great-circle distance between airports and multiplies by an EPA passenger-mile combustion factor for short, medium, or long flights.'],
        ['Limits', 'The comparison does not change the AI estimate. It excludes actual routing, seat class, upstream fuel emissions, and non-CO₂ warming effects at altitude.'],
      ],
      sources: [['EPA 2025 emissions factors, Table 10', 'https://www.epa.gov/system/files/documents/2025-01/ghg-emission-factors-hub-2025.pdf']],
    },
  };

  function openInfo(topic, trigger) {
    const info = INFO[topic];
    if (!info) return;
    const dialog = node('dialog', 'carbon-info-dialog');
    dialog.setAttribute('aria-labelledby', 'carbon-info-title');
    const head = node('div', 'carbon-info-head'), titleBlock = node('div');
    titleBlock.append(node('p', 'carbon-info-eyebrow', 'Carbon footprint · guide'), node('h2', '', info.title));
    titleBlock.querySelector('h2').id = 'carbon-info-title';
    const close = node('button', 'carbon-info-close', '×');
    close.type = 'button';
    close.setAttribute('aria-label', 'Close information');
    close.onclick = () => dialog.close();
    head.append(titleBlock, close);
    const body = node('div', 'carbon-info-body');
    body.append(node('p', 'carbon-info-lead', info.lead));
    for (const [heading, copy] of info.sections) {
      const section = node('section', 'carbon-info-section');
      section.append(node('h3', '', heading), node('p', '', copy));
      body.append(section);
    }
    if (info.example) body.append(node('div', 'carbon-info-example', info.example));
    if (info.sources?.length) {
      const sources = node('div', 'carbon-info-sources');
      sources.append(node('strong', '', 'Read the sources: '));
      info.sources.forEach(([label, url], index) => {
        if (index) sources.append(' · ');
        const link = node('a', '', label);
        link.href = url;
        link.target = '_blank';
        link.rel = 'noopener noreferrer';
        sources.append(link);
      });
      body.append(sources);
    }
    dialog.append(head, body);
    dialog.addEventListener('click', event => { if (event.target === dialog) dialog.close(); });
    dialog.addEventListener('close', () => { dialog.remove(); if (trigger.isConnected) trigger.focus(); }, {once: true});
    document.body.append(dialog);
    dialog.showModal();
    close.focus();
  }

  function infoButton(topic, label) {
    const button = node('button', 'carbon-info-button');
    button.type = 'button';
    button.setAttribute('aria-label', `About ${label}`);
    button.title = `About ${label}`;
    const icon = document.createElementNS('http://www.w3.org/2000/svg', 'svg');
    icon.setAttribute('viewBox', '0 0 24 24');
    icon.setAttribute('aria-hidden', 'true');
    const use = document.createElementNS('http://www.w3.org/2000/svg', 'use');
    use.setAttribute('href', '#ph-icon-info');
    icon.append(use);
    button.append(icon);
    button.onclick = () => openInfo(topic, button);
    return button;
  }

  function controlTitle(label, topic) {
    const title = node('div', 'carbon-control-title');
    title.append(node('span', '', label), infoButton(topic, label));
    return title;
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
    for (const text of ['', 'Tokens', 'Energy', 'CO₂e', 'Share']) head.append(node('th', text === 'Share' ? 'carbon-share' : '', text));
    table.createTHead().append(head);
    for (const [key, row] of rows) {
      const tr = node('tr'), name = node('td', '', labels[key]?.[0] || key), share = node('td', 'carbon-share'), bar = node('span', 'carbon-bar');
      if (labels[key]?.[1]) name.append(node('span', 'carbon-sub', labels[key][1]));
      const fraction = result.wh ? row.wh / result.wh : 0;
      bar.style.width = `${(fraction * 100).toFixed(1)}%`;
      share.title = `${(fraction * 100).toFixed(1)}% of estimated energy`;
      const content = node('span', 'carbon-share-content'), track = node('span', 'carbon-share-track');
      track.append(bar);
      content.append(track, node('span', 'carbon-share-percent', `${(fraction * 100).toFixed(1)}%`));
      share.append(content);
      tr.append(name, node('td', '', tokens(row.tokens)), node('td', '', energy(row.wh * result.pue)), node('td', '', mass(result.grams(row.wh))), share);
      body.append(tr);
    }
    table.append(body);
    const total = rows.reduce((sum, [, row]) => sum + row.tokens, 0);
    foot.append(node('td', '', 'Total'), node('td', '', tokens(total)), node('td', '', energy(result.facilityWh)), node('td', '', mass(result.co2)), node('td', 'carbon-share', result.wh ? '100%' : '0%'));
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
      `. Energy shown includes data-center overhead (PUE). This estimates inference electricity emissions, not a direct measurement; providers do not publish per-token energy. Model training, embodied hardware emissions, water, networking, and your own computer are outside this estimate. The estimate switch selects the model energy and token-type ratios; the PUE and grid can be selected separately. Prefill ratio: ${setup.inputSensitivity === 'third' ? 'Watershed (3:1 at Central)' : 'Labs (5:1 at Central)'}.`);
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

    details.append(node('h4', '', 'Everyday comparisons'));
    const comparisons = node('p');
    comparisons.append('Vehicle miles divide the estimated CO₂e by the selected vehicle factor. The hybrid and pickup use 40 and 20 mpg examples; the electric car uses 0.39 kWh/mile on a fixed US-average 350 g CO₂e/kWh grid. Vehicle manufacture and fuel production are excluded. ', cite(factors, ['epa-equivalencies', 'epa-ev-comparison', 'egrid-2023']));
    details.append(comparisons);
    const flights = node('p');
    flights.append('Flights compare one economy passenger on a one-way, nonstop trip. Approximate great-circle airport distance is multiplied by EPA’s short, medium, or long-haul combustion factor; routing, seat class, upstream fuel emissions, and non-CO₂ effects at altitude are excluded. ', cite(factors, ['epa-flight-factors', 'faa-airports']));
    details.append(flights);

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
    const focused = document.activeElement && root.contains(document.activeElement)
      ? {control: document.activeElement.dataset.carbonControl, value: document.activeElement.dataset.carbonValue} : null;
    const factors = data.factors, windowKey = WINDOWS.some(([key]) => key === prefs.window) ? prefs.window : 'all';
    const period = data.windows.find(item => item.key === windowKey) || data.windows[0];
    const setup = settings(factors);
    const result = calculate(factors, period, setup.scenario, setup.pue, setup.gridValue, setup.inputScale);
    const update = change => { Object.assign(prefs, change); save(); render(); window.dispatchEvent(new Event('pharos:carbon-settings')); };

    const head = node('div', 'carbon-head'), hero = node('div', 'carbon-hero');
    const titleRow = node('div', 'carbon-title-row'), title = node('div', 'carbon-title');
    title.append(node('div', 'meta', `Estimated CO₂e · ${WINDOWS.find(([key]) => key === windowKey)[1].toLowerCase()}`),
      node('div', 'big', `${mass(result.co2)} CO₂e`));
    const facts = node('div', 'carbon-facts');
    const fact = (value, text) => { const span = node('span'); span.append(node('b', '', value), ` ${text}`); facts.append(span); };
    fact(energy(result.facilityWh), 'of electricity');
    const vehicleKey = VEHICLES.some(([key]) => key === prefs.vehicle && equivalent(factors, key)) ? prefs.vehicle : 'car_miles';
    const vehicle = equivalent(factors, vehicleKey);
    if (vehicle) {
      const row = node('div', 'carbon-comparison'), vehicleSelect = node('select');
      vehicleSelect.dataset.carbonControl = 'vehicle';
      vehicleSelect.setAttribute('aria-label', 'Vehicle comparison');
      for (const [key, label] of VEHICLES) if (equivalent(factors, key)) {
        const option = node('option', '', label); option.value = key; option.title = equivalent(factors, key).label; vehicleSelect.append(option);
      }
      vehicleSelect.value = vehicleKey;
      vehicleSelect.title = vehicle.label;
      vehicleSelect.onchange = () => update({vehicle: vehicleSelect.value});
      row.append(node('b', '', `≈ ${sig(result.co2 / vehicle.g_co2e_per_unit)} miles`), 'in', vehicleSelect, infoButton('vehicle', 'vehicle comparison'));
      facts.append(row);
    }
    const phone = equivalent(factors, 'phone_charges');
    if (phone) fact(`≈ ${sig(result.co2 / phone.g_co2e_per_unit)}`, phone.label);
    const from = CITIES.find(city => city.key === prefs.flightFrom) || CITIES.find(city => city.key === 'jfk');
    const to = CITIES.find(city => city.key === prefs.flightTo && city.key !== from.key)
      || CITIES.find(city => city.key === 'den' && city.key !== from.key)
      || CITIES.find(city => city.key !== from.key);
    const miles = flightMiles(from, to);
    const flight = equivalent(factors, miles < 300 ? 'flight_short' : miles < 2300 ? 'flight_medium' : 'flight_long');
    if (flight) {
      const row = node('div', 'carbon-comparison');
      const citySelect = (label, current, other, key) => {
        const select = node('select');
        select.dataset.carbonControl = key;
        select.setAttribute('aria-label', label);
        select.title = current.label;
        for (const city of CITIES) if (city.key !== other.key) {
          const option = node('option', '', city.code || city.key.toUpperCase()); option.value = city.key; option.title = city.label; select.append(option);
        }
        select.value = current.key;
        select.onchange = () => update({[key]: select.value});
        return select;
      };
      row.append(node('b', '', `≈ ${sig(result.co2 / (miles * flight.g_co2e_per_unit))} one-way flights`),
        citySelect('Flight from', from, to, 'flightFrom'), infoButton('flight', 'flight origin'),
        'to', citySelect('Flight to', to, from, 'flightTo'), infoButton('flight', 'flight destination'));
      facts.append(row);
    }
    const periodControl = node('div', 'carbon-control');
    periodControl.append(controlTitle('Period', 'period'), segmented('Period', WINDOWS, windowKey, key => update({window: key})));
    titleRow.append(title, periodControl);
    hero.append(titleRow, facts);
    head.append(hero);
    root.replaceChildren(head);

    const controls = node('div', 'carbon-controls');
    const gridLabel = node('div', 'carbon-control'), gridSelect = node('select');
    gridSelect.dataset.carbonControl = 'grid';
    gridSelect.setAttribute('aria-label', 'Electricity grid');
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
    gridLabel.append(controlTitle('Electricity grid', 'grid'), gridSelect);
    controls.append(gridLabel);
    if (!setup.grid) {
      const customLabel = node('div', 'carbon-control'), input = node('input');
      input.type = 'number';
      input.setAttribute('aria-label', 'Custom grid intensity in g CO₂e per kWh');
      input.min = '0';
      input.step = '1';
      input.value = String(setup.gridValue);
      input.dataset.carbonControl = 'customGrid';
      input.onchange = () => update({customGrid: Math.max(0, Number(input.value) || 0)});
      customLabel.append(controlTitle('g CO₂e per kWh', 'customGrid'), input);
      controls.append(customLabel);
    }
    const pueLabel = node('div', 'carbon-control');
    pueLabel.append(controlTitle(`PUE · ${sig(setup.pue)}`, 'pue'));
    pueLabel.append(segmented('PUE', SCENARIOS, setup.pueScenario, key => update({pueScenario: key})));
    const scenarioLabel = node('div', 'carbon-control');
    scenarioLabel.append(controlTitle('Estimate', 'estimate'));
    scenarioLabel.append(segmented('Estimate', SCENARIOS, setup.scenario, key => update({scenario: key})));
    const inputLabel = node('div', 'carbon-control');
    inputLabel.append(controlTitle('Prefill ratio', 'inputSensitivity'),
      segmented('Prefill ratio', INPUT_SENSITIVITIES, setup.inputSensitivity, key => update({inputSensitivity: key})));
    controls.append(pueLabel, scenarioLabel, inputLabel);
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
    root.append(methodology(factors, result, setup));
    if (focused?.control) [...root.querySelectorAll('[data-carbon-control]')]
      .find(element => element.dataset.carbonControl === focused.control && element.dataset.carbonValue === focused.value)?.focus();
  }

  async function refresh() {
    const request = ++loading;
    render();
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

  const sheet = node('style');
  sheet.textContent = CSS;
  document.head.append(sheet);
  window.pharosCarbon = {refresh, calculate};
})();
