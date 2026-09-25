const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { createRequire } = require('node:module');

const root = path.resolve(__dirname, '..');
const requireLocal = createRequire(path.join(root, '.context/browser-tests/package.json'));
const { chromium } = requireLocal('playwright');
const source = fs.readFileSync(path.join(root, 'internal/archive/assets/ui.py'), 'utf8');
const html = source.slice(source.indexOf("r'''") + 4, source.lastIndexOf("'''"));
const factors = JSON.parse(fs.readFileSync(path.join(root, 'carbon/co2_factors.json'), 'utf8'));

test('Carbon footprint card estimates CO2e and recalculates from its controls', async () => {
  const chrome = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
  const browser = await chromium.launch({ headless: true, ...(fs.existsSync(chrome) ? { executablePath: chrome } : {}) });
  try {
    const page = await browser.newPage();
    const errors = [];
    page.on('pageerror', error => { if ((error.stack || '').includes('/assets/carbon.js')) errors.push(error.message); });
    // 1M mid-size output tokens all time; 1M cache reads in the last 30 days.
    const body = {
      factors,
      windows: [
        { key: 'all', tokens: { medium: { uncached_input: 0, cache_write: 0, cache_read: 0, output: 1e6 } } },
        { key: '30d', tokens: { medium: { uncached_input: 0, cache_write: 0, cache_read: 1e6, output: 0 } } },
      ],
      default_tier_models: [{ model: 'mystery-model', tokens: 1234 }],
    };
    await page.route('http://carbon.test/**', route => {
      const url = new URL(route.request().url());
      if (url.pathname === '/settings') return route.fulfill({ contentType: 'text/html', body: html });
      if (url.pathname.startsWith('/assets/')) {
        const file = path.join(root, 'internal/archive/assets', url.pathname.slice('/assets/'.length));
        if (fs.existsSync(file)) return route.fulfill({ contentType: file.endsWith('.css') ? 'text/css' : 'application/javascript', body: fs.readFileSync(file) });
      }
      if (url.pathname === '/api/health/carbon') return route.fulfill({ contentType: 'application/json', body: JSON.stringify(body) });
      if (url.pathname.startsWith('/api/query/') && route.request().method() === 'POST') return route.fulfill({ contentType: 'application/json', body: JSON.stringify(url.pathname.endsWith('/aggregations') ? { metrics: [] } : { rows: [], total: 0 }) });
      if (url.pathname.startsWith('/api/')) return route.fulfill({ contentType: 'application/json', body: '{}' });
      return route.fulfill({ status: 404 });
    });
    await page.goto('http://carbon.test/settings');
    const card = page.locator('#carbonCard');
    const big = card.locator('.carbon-hero .big');
    // 1M output × 400 Wh × PUE 1.15 × 350 g/kWh = 161 g.
    await big.filter({ hasText: '161 g CO₂e' }).waitFor();
    assert.match(await card.locator('.carbon-facts').textContent(), /460 Wh of electricity/);
    assert.match(await card.textContent(), /Counted as Mid-size models: mystery-model/);
    // The section sits between Health and Sources.
    assert.equal(await page.evaluate(() => document.getElementById('carbon').previousElementSibling.id), 'health');

    await card.locator('select').selectOption('world');
    await big.filter({ hasText: '211 g CO₂e' }).waitFor();
    await card.locator('select').selectOption('custom');
    const custom = card.locator('input[data-carbon-control="customGrid"]');
    await custom.fill('100');
    await custom.dispatchEvent('change');
    await big.filter({ hasText: '46 g CO₂e' }).waitFor();
    const pue = card.locator('input[data-carbon-control="pue"]');
    await pue.fill('2');
    await pue.dispatchEvent('change');
    await big.filter({ hasText: '80 g CO₂e' }).waitFor();
    // High estimate keeps the fixed PUE: 1M × 1,200 Wh × 2 × 100 g/kWh = 240 g.
    await card.getByRole('button', { name: 'High' }).click();
    await big.filter({ hasText: '240 g CO₂e' }).waitFor();

    // Last 30 days: 1M cache reads × 1,200 Wh × 0.08 × 2 × 100 g/kWh = 19.2 g.
    await card.getByRole('button', { name: 'Last 30 days' }).click();
    await big.filter({ hasText: '19.2 g CO₂e' }).waitFor();

    // Choices persist across reloads.
    await page.reload();
    await big.filter({ hasText: '19.2 g CO₂e' }).waitFor();
    await card.locator('summary', { hasText: 'How this is calculated' }).click();
    assert.equal(await card.locator('.carbon-method ol > li').count(), factors.sources.length);
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
});
