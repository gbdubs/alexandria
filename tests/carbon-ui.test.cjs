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

test('Usage Carbon Impact estimates CO2e and recalculates from its controls', async () => {
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
      if (url.pathname === '/usage' || url.pathname === '/settings') return route.fulfill({ contentType: 'text/html', body: html });
      if (url.pathname.startsWith('/assets/')) {
        const file = path.join(root, 'internal/archive/assets', url.pathname.slice('/assets/'.length));
        if (fs.existsSync(file)) return route.fulfill({ contentType: file.endsWith('.css') ? 'text/css' : 'application/javascript', body: fs.readFileSync(file) });
      }
      if (url.pathname === '/api/health/carbon') return route.fulfill({ contentType: 'application/json', body: JSON.stringify(body) });
      if (url.pathname.startsWith('/api/query/') && route.request().method() === 'POST') return route.fulfill({ contentType: 'application/json', body: JSON.stringify(url.pathname.endsWith('/aggregations') ? { metrics: [] } : { rows: [], total: 0 }) });
      if (url.pathname.startsWith('/api/')) return route.fulfill({ contentType: 'application/json', body: '{}' });
      return route.fulfill({ status: 404 });
    });
    await page.goto('http://carbon.test/usage?usage=carbon');
    assert.equal(await page.getByRole('group', { name: 'Usage view' }).getByRole('button', { name: 'Carbon Impact' }).getAttribute('aria-pressed'), 'true');
    const card = page.locator('#carbonCard');
    const big = card.locator('.carbon-hero .big');
    // 1M output × 400 Wh × PUE 1.15 × 350 g/kWh = 161 g.
    await big.filter({ hasText: '161 g CO₂e' }).waitFor();
    assert.match(await card.locator('.carbon-facts').textContent(), /460 Wh of electricity/);
    assert.match(await card.textContent(), /Counted as Mid-size models: mystery-model/);
    const shares = card.locator('table.carbon-table').first().locator('tbody .carbon-share-percent');
    assert.deepEqual(await shares.allTextContents(), ['0.0%', '0.0%', '0.0%', '100.0%']);
    assert.equal(await card.locator('table.carbon-table').first().locator('tfoot .carbon-share').textContent(), '100%');
    assert.equal(await shares.last().evaluate(element => getComputedStyle(element).textAlign), 'right');
    const vehicle = card.locator('[data-carbon-control="vehicle"]');
    const from = card.locator('[data-carbon-control="flightFrom"]');
    const to = card.locator('[data-carbon-control="flightTo"]');
    assert.equal(await vehicle.inputValue(), 'car_miles');
    assert.equal(await from.inputValue(), 'jfk');
    assert.equal(await to.inputValue(), 'den');
    assert.equal(await card.locator('.carbon-facts > .carbon-comparison').count(), 2);
    assert.equal(await from.locator('option:checked').textContent(), 'NYC');
    assert.equal(await to.locator('option:checked').textContent(), 'DEN');
    assert.ok((await from.boundingBox()).width <= 52);
    const vehicleMiles = await card.locator('.carbon-comparison').first().locator('b').textContent();
    await vehicle.selectOption('car_hybrid');
    assert.notEqual(await card.locator('.carbon-comparison').first().locator('b').textContent(), vehicleMiles);
    await vehicle.selectOption('car_pickup');
    assert.notEqual(await card.locator('.carbon-comparison').first().locator('b').textContent(), vehicleMiles);
    await vehicle.selectOption('car_electric');
    assert.notEqual(await card.locator('.carbon-comparison').first().locator('b').textContent(), vehicleMiles);
    const flightCount = await card.locator('.carbon-comparison').nth(1).locator('b').textContent();
    await to.selectOption('sfo');
    const shortFlightCount = await card.locator('.carbon-comparison').nth(1).locator('b').textContent();
    assert.notEqual(shortFlightCount, flightCount);
    await from.selectOption('den');
    assert.equal(await from.inputValue(), 'den');
    assert.notEqual(await card.locator('.carbon-comparison').nth(1).locator('b').textContent(), shortFlightCount);
    assert.equal(await page.locator('#usage #carbonCard').count(), 1);
    assert.equal(await page.locator('#settings #carbonCard').count(), 0);

    await card.locator('[data-carbon-control="grid"]').selectOption('world');
    await big.filter({ hasText: '211 g CO₂e' }).waitFor();
    await card.locator('[data-carbon-control="grid"]').selectOption('custom');
    const custom = card.locator('input[data-carbon-control="customGrid"]');
    assert.equal(await card.locator('.carbon-info-button').count(), 9);
    await custom.fill('100');
    await custom.dispatchEvent('change');
    await big.filter({ hasText: '46 g CO₂e' }).waitFor();
    const pue = card.getByRole('group', { name: 'PUE' });
    const estimate = card.getByRole('group', { name: 'Estimate' });
    const prefillRatio = card.getByRole('group', { name: 'Prefill ratio' });
    assert.equal(await pue.getByRole('button', { name: 'Central' }).getAttribute('aria-pressed'), 'true');
    await pue.getByRole('button', { name: 'High' }).click();
    await big.filter({ hasText: '52 g CO₂e' }).waitFor();
    assert.equal(await pue.getByRole('button', { name: 'High' }).evaluate(button => button === document.activeElement), true);
    // Estimate changes model energy and ratios while PUE remains high.
    await estimate.getByRole('button', { name: 'High' }).click();
    await big.filter({ hasText: '156 g CO₂e' }).waitFor();
    assert.equal(await pue.getByRole('button', { name: 'High' }).getAttribute('aria-pressed'), 'true');
    await prefillRatio.getByRole('button', { name: 'Watershed' }).click();
    // Output is unchanged by the prefill ratio.
    await big.filter({ hasText: '156 g CO₂e' }).waitFor();

    const help = card.getByRole('button', { name: 'About Prefill ratio' });
    await help.click();
    const dialog = page.getByRole('dialog', { name: 'Prefill ratio' });
    assert.match(await dialog.textContent(), /Anthropic’s published input-to-output API price ratio/);
    assert.match(await dialog.textContent(), /0\.32 Wh per 1,000 input tokens and 0\.96 Wh per 1,000 output tokens/);
    assert.match(await dialog.textContent(), /Why both are useful/);
    assert.equal(await dialog.locator('a[href*="sanity.io"]').count(), 1);
    await page.keyboard.press('Escape');
    await dialog.waitFor({ state: 'detached' });
    assert.equal(await help.evaluate(button => button === document.activeElement), true);
    await card.getByRole('button', { name: 'About Electricity grid' }).click();
    await page.getByRole('dialog', { name: 'Electricity grid' }).getByRole('button', { name: 'Close information' }).click();
    assert.equal(await page.getByRole('dialog').count(), 0);

    // Last 30 days: 1M cache reads × 1,200 Wh × 0.08 × (5/3) × 1.3 × 100 g/kWh = 20.8 g.
    await card.getByRole('button', { name: 'Last 30 days' }).click();
    await big.filter({ hasText: '20.8 g CO₂e' }).waitFor();

    // Choices persist across reloads.
    await page.reload();
    await big.filter({ hasText: '20.8 g CO₂e' }).waitFor();
    assert.equal(await pue.getByRole('button', { name: 'High' }).getAttribute('aria-pressed'), 'true');
    assert.equal(await prefillRatio.getByRole('button', { name: 'Watershed' }).getAttribute('aria-pressed'), 'true');
    assert.equal(await vehicle.inputValue(), 'car_electric');
    assert.equal(await from.inputValue(), 'den');
    assert.equal(await to.inputValue(), 'sfo');
    await card.locator('summary', { hasText: 'How this is calculated' }).click();
    assert.equal(await card.locator('.carbon-method ol > li').count(), factors.sources.length);
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
});
