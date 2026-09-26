const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { createRequire } = require('node:module');
const { pathToFileURL } = require('node:url');

const root = path.resolve(__dirname, '..');
const { chromium } = createRequire(path.join(root, '.context/browser-tests/package.json'))('playwright');
const source = fs.readFileSync(['src/pharos/ui.py', 'internal/archive/assets/ui.py'].map(name => path.join(root, name)).find(file => fs.existsSync(file)), 'utf8');
const html = source.slice(source.indexOf("r'''") + 4, source.lastIndexOf("'''"));

const day = offset => { const d = new Date(); d.setDate(d.getDate() - offset); return `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`; };
// The usage dataset's current day, week (Monday), and month keys.
const periodKey = period => { const d = new Date(); if (period === 'week') d.setDate(d.getDate() - ((d.getDay() + 6) % 7)); const key = `${d.getFullYear()}-${String(d.getMonth() + 1).padStart(2, '0')}-${String(d.getDate()).padStart(2, '0')}`; return period === 'month' ? key.slice(0, 7) : key; };
const counts = (typed, pasted, harness) => ({ typed_words: typed, typed_chars: typed * 6, pasted_words: pasted, pasted_chars: pasted * 6, harness_words: harness, harness_chars: harness * 6 });

test('Usage toggles between tokens and writing, remembers the choice, and charts follow table filters', async () => {
  const { encodeQuery, EMPTY_QUERY } = await import(pathToFileURL(path.join(root, 'web/node_modules/@pythia-software/query-table-core/dist/index.js')).href);
  const chrome = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
  const browser = await chromium.launch({ headless: true, ...(fs.existsSync(chrome) ? { executablePath: chrome } : {}) });
  try {
    const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
    const page = await context.newPage();
    const errors = [], series = [], aggregations = [];
    let pricing = { prompt: 'Update missing model prices', unpriced_models: [], unpriced_tokens: 0, confirmed_changes: 0, proposed_changes: 0 };
    page.on('pageerror', error => errors.push(error.message));
    await page.route('http://usage-ui.test/**', route => {
      const url = new URL(route.request().url());
      if (!url.pathname.startsWith('/api/') && !url.pathname.startsWith('/assets/')) return route.fulfill({ contentType: 'text/html', body: html });
      if (url.pathname === '/assets/query-tables.js') return route.fulfill({ contentType: 'application/javascript', body: fs.readFileSync(path.join(root, 'internal/archive/assets/query-tables.js')) });
      if (url.pathname === '/assets/query-tables.css') return route.fulfill({ contentType: 'text/css', body: fs.readFileSync(path.join(root, 'internal/archive/assets/query-tables.css')) });
      const json = body => route.fulfill({ contentType: 'application/json', body: JSON.stringify(body) });
      if (url.pathname === '/api/authorship') return json({ built_at: '2026-09-24T12:00:00Z', running: false, stale: false, error: null, totals: { typed_words: 1234, typed_messages: 20, messages: 40 }, last_30_days: { typed_words: 300 } });
      if (url.pathname === '/api/query/writing/series') {
        const body = route.request().postDataJSON();
        series.push(body);
        return json({ works: 2, daily: [{ day: day(1), typed_messages: 3, messages: 5, ...counts(120, 40, 900) }, { day: day(9), typed_messages: 2, messages: 2, ...counts(80, 0, 0) }], totals: { messages: 7, typed_messages: 5, first_day: day(9), ...counts(200, 40, 900) } });
      }
      if (url.pathname === '/api/query/usage/aggregations') {
        const body = route.request().postDataJSON();
        aggregations.push(body);
        return json({ metrics: body.aggregations.map((aggregation, index) => ({ id: aggregation.id, buckets: aggregation.groupBy?.[1] === 'model_family'
          ? ['m1', 'm2', 'm3', 'm4', 'm5', 'small-a', 'small-b'].map((model, rank) => ({ keys: [periodKey(aggregation.groupBy[0]), model], value: 700 - 100 * rank, count: 1 }))
          : [{ keys: [periodKey(aggregation.groupBy?.[0] ?? 'day'), ...((aggregation.groupBy?.length ?? 0) > 1 ? ['claude'] : [])], value: aggregation.field === 'cost_usd' ? 1_250_000_000_000 : 2_500_000_000 * (index + 1), count: 1 }] })) });
      }
      if (url.pathname.endsWith('/distinct')) return json({ values: [], hasMore: false });
      if (url.pathname.endsWith('/field-stats')) return json({});
      if (url.pathname.startsWith('/api/query/') && url.pathname.endsWith('/aggregations')) return json({ metrics: [] });
      if (url.pathname === '/api/query/writing') return json({ rows: [{ id: 'w1', workspace_id: 'w1', title: 'Deep work', repository_name: 'example/repo', typed_words: 200, typed_turns: 5, typed_share: 17.4, user_words: 1140 }], total: 1 });
      if (url.pathname === '/api/query/usage') return json({ rows: [{ id: 'u1', workspace_id: 'w1', total_tokens: 2_500_000_000_000, uncached_input_tokens: 1_200_000_000, cache_read_input_tokens: 0, cache_creation_input_tokens: 0, output_tokens: 0, cost_usd: 123_000_000_000 }], total: 1 });
      if (url.pathname === '/api/health/pricing') return json({ priced_tokens: 10, total_tokens: 2_500_000_000_000, cost_usd: 123_000_000_000, recent_tokens: 400_000_000_000, recent_cost_usd: 3 });
      if (url.pathname === '/api/pricing') return json(pricing);
      return route.fulfill({ status: 404, contentType: 'application/json', body: JSON.stringify({ error: 'not stubbed' }) });
    });

    // Tokens is the default, with its chart split by token type.
    await page.goto('http://usage-ui.test/usage');
    await page.locator('#usage .usage-chart h3', { hasText: 'Tokens per week by token type' }).waitFor();
    await page.locator('.token-summary .mcp-metric').first().waitFor();
    assert.deepEqual(await page.locator('.token-summary .mcp-metric > span').allTextContents(), ['Total tokens', 'API price equivalent', 'Cache hit rate', 'Output tokens', 'Agent sessions']);
    assert.equal(await page.locator('.token-summary .mcp-metric').count(), 5);
    assert.equal(await page.getByRole('group', { name: 'Usage view' }).getByRole('button', { name: 'Machine Tokens' }).getAttribute('aria-pressed'), 'true');
    assert.equal(await page.locator('.refresh-prices-notice').count(), 0);
    pricing = { ...pricing, unpriced_models: [{ model: 'new-model', provider: 'test', tokens: 50, first_day: day(1), last_day: day(1) }] };
    await page.evaluate(() => window.dispatchEvent(new Event('pharos:usage-refresh')));
    await page.locator('.refresh-prices-notice').waitFor();
    assert.match(await page.locator('.refresh-prices-notice').innerText(), /A new model has no price definition/);
    assert.equal(await page.locator('.refresh-prices').evaluate(button => getComputedStyle(button).color), await page.locator('.refresh-prices-notice p').evaluate(blurb => getComputedStyle(blurb).color));
    assert.ok((await page.locator('.refresh-prices').evaluate(button => getComputedStyle(button).fontWeight)) >= 700);
    const toggle = await page.locator('.usage-heading-actions .library-view-toggle').boundingBox();
    const priceNotice = await page.locator('.refresh-prices-notice').boundingBox();
    assert.ok(priceNotice.y >= toggle.y + toggle.height);
    await page.setViewportSize({ width: 1177, height: 780 });
    await page.screenshot({ path: path.join(root, '.context/usage-pricing-alert.png') });
    await page.setViewportSize({ width: 1280, height: 900 });
    await page.getByRole('button', { name: /Copy refresh prices prompt/ }).click();
    await page.locator('.refresh-prices-notice').waitFor({ state: 'detached' });
    assert.deepEqual(aggregations.at(-1).aggregations.map(a => a.field), ['uncached_input_tokens', 'cache_read_input_tokens', 'cache_creation_input_tokens', 'output_tokens', 'unclassified_tokens']);
    assert.match(await page.locator('.usage-chart-bars > span').last().getAttribute('aria-label'), /2\.5B/);
    await page.locator('#usage .qt-row', { hasText: '2.5T' }).waitFor();
    assert.equal(await page.locator('#usage .qt-row [title="2,500,000,000,000"]').innerText(), '2.5T');

    // Cost can't split by token type, so the split falls back to provider.
    await page.locator('.usage-chart').getByRole('button', { name: 'Cost', exact: true }).click();
    await page.locator('#usage .usage-chart h3', { hasText: 'API cost per week by provider' }).waitFor();
    assert.equal(await page.locator('.usage-chart').getByRole('button', { name: 'Token type' }).isDisabled(), true);
    assert.deepEqual(aggregations.at(-1).aggregations[0].groupBy, ['week', 'provider']);
    await page.waitForFunction(() => document.querySelector('.usage-chart-bars > span:last-child')?.getAttribute('aria-label')?.includes('$1.3T'));
    assert.match(await page.locator('.usage-chart-grid').first().innerText(), /\$\d+(?:\.\d+)?[BT]/);
    await page.locator('.usage-chart').getByRole('button', { name: 'Month', exact: true }).click();
    await page.locator('.usage-chart-legend', { hasText: 'Claude' }).waitFor();

    // Range sets how many columns show; gridlines replace the peak label.
    await page.locator('.usage-chart').getByRole('button', { name: 'Day', exact: true }).click();
    await page.locator('.usage-chart').getByRole('button', { name: '30 days' }).click();
    await page.waitForFunction(() => document.querySelectorAll('.usage-chart-bars > span').length === 30);
    assert.ok(await page.locator('.usage-chart-grid').count() >= 1);
    assert.doesNotMatch(await page.locator('.usage-chart-axis').innerText(), /peak/);

    // The hover key lists each series with its color, and Other names what it holds.
    await page.locator('.usage-chart').getByRole('button', { name: 'Model', exact: true }).click();
    await page.locator('.usage-chart-legend', { hasText: 'Other (2)' }).waitFor();
    await page.locator('.usage-chart-bars > span').last().hover();
    const tip = page.locator('.usage-chart-tip');
    await tip.waitFor();
    assert.equal(await tip.locator(':scope > ul > li').count(), 6);
    assert.equal(await tip.locator(':scope > ul > li .usage-swatch').count(), 6);
    assert.match(await tip.locator('.usage-chart-tip-parts').innerText(), /small-a[\s\S]*small-b/);
    assert.match(await tip.innerText(), /Total/);
    await page.locator('.usage-chart').getByRole('button', { name: 'Month', exact: true }).click();
    await page.locator('.usage-chart').getByRole('button', { name: '6 months' }).click();
    await page.locator('.usage-chart').getByRole('button', { name: 'Provider', exact: true }).click();

    // Switching to writing updates the URI, and the choice survives a plain /usage visit.
    await page.getByRole('group', { name: 'Usage view' }).getByRole('button', { name: 'Human Words' }).click();
    await page.locator('.usage-chart h3', { hasText: 'Typed words per week' }).waitFor();
    assert.equal(new URL(page.url()).searchParams.get('usage'), 'writing');
    await page.locator('.writing-table td.num', { hasText: '40' }).first().waitFor();
    assert.match(await page.locator('.usage-cards').innerText(), /200[\s\S]*Up to 240/);
    await page.locator('#usage .qt-row', { hasText: 'Deep work' }).waitFor();
    await page.goto('http://usage-ui.test/usage');
    await page.locator('.usage-chart h3', { hasText: 'Typed words per week' }).waitFor();

    // All input stacks every category, and the series request carries the table filters.
    await page.getByRole('button', { name: 'All input' }).click();
    await page.locator('.usage-chart h3', { hasText: 'Words of user input per week' }).waitFor();
    assert.equal(await page.locator('.usage-chart-legend > span').count(), 5);
    const filtered = encodeQuery({ ...EMPTY_QUERY, where: [{ field: 'repository_name', op: '=', value: 'example/repo' }] });
    series.length = 0;
    await page.goto(`http://usage-ui.test/usage?usage=writing&q_writing=${encodeURIComponent(filtered)}`);
    await page.locator('.usage-chart h3', { hasText: 'Words of user input per week' }).waitFor();
    await page.waitForFunction(() => document.querySelector('.writing-table'));
    assert.ok(series.some(body => JSON.stringify(body.where) === JSON.stringify([{ field: 'repository_name', op: '=', value: 'example/repo' }])), JSON.stringify(series));

    // The Settings cards open each Usage view.
    await page.goto('http://usage-ui.test/settings');
    const tokensCard = page.locator('#healthCards .health-link', { hasText: 'Tokens used' });
    await tokensCard.locator('.big', { hasText: '2.5T' }).waitFor();
    assert.match(await tokensCard.innerText(), /\$123B API cost[\s\S]*400B in the last 30 days/);
    await tokensCard.click();
    await page.locator('#usage.view.active .usage-chart h3', { hasText: 'API cost per month by provider' }).waitFor();
    assert.equal(new URL(page.url()).searchParams.get('usage'), 'tokens');
    await page.goto('http://usage-ui.test/settings');
    const pricingCard = page.locator('#healthCards > .panel', { hasText: 'Known Pricing' });
    await pricingCard.getByRole('button', { name: 'Open in Usage' }).click();
    await page.locator('#usage.view.active .usage-chart h3', { hasText: 'API cost per month by provider' }).waitFor();
    assert.equal(new URL(page.url()).searchParams.get('usage'), 'tokens');
    await page.goto('http://usage-ui.test/settings');
    await page.locator('#healthCards .health-link', { hasText: 'Words you wrote' }).locator('.big', { hasText: '1,234' }).waitFor();
    await page.locator('#healthCards .health-link', { hasText: 'Words you wrote' }).click();
    await page.locator('#usage.view.active .usage-chart h3', { hasText: 'Words of user input per week' }).waitFor();
    // Settings panels this test does not stub reject with "not stubbed".
    assert.deepEqual(errors.filter(message => message !== 'not stubbed'), []);
  } finally {
    await browser.close();
  }
});
