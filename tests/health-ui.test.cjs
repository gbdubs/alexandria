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

test('Health sections render independently as each request finishes', async () => {
  const chrome = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
  const browser = await chromium.launch({ headless: true, ...(fs.existsSync(chrome) ? { executablePath: chrome } : {}) });
  try {
    const page = await browser.newPage();
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    const held = {};
    const bodies = {
      counts: { workspaces: 19643, messages: 3111202 },
      storage: { volume_name: 'Archive', index_bytes: 2000, internal_free_bytes: 0, free_bytes: 0, staging_bytes: 0, staging_cap_bytes: 1, path: '/archive', status: 'available' },
      pricing: { priced_tokens: 90, assumed_tokens: 0, partial_tokens: 0, unpriced_tokens: 10, confirmed_changes: 3, proposed_changes: 0 },
    };
    await page.route('http://health.test/**', route => {
      const url = new URL(route.request().url());
      if (url.pathname === '/settings') return route.fulfill({ contentType: 'text/html', body: html });
      if (url.pathname === '/assets/query-tables.js') return route.fulfill({ contentType: 'application/javascript', body: fs.readFileSync(path.join(root, 'internal/archive/assets/query-tables.js')) });
      if (url.pathname === '/assets/query-tables.css') return route.fulfill({ contentType: 'text/css', body: fs.readFileSync(path.join(root, 'internal/archive/assets/query-tables.css')) });
      const section = url.pathname.startsWith('/api/health/') && url.pathname.slice('/api/health/'.length);
      if (section === 'pricing') return route.fulfill({ status: 500, contentType: 'application/json', body: '{"error":"boom"}' });
      if (section in bodies) {
        // Hold counts until the test releases it; answer the rest at once.
        const reply = () => route.fulfill({ contentType: 'application/json', body: JSON.stringify(bodies[section]) });
        if (section === 'counts') { held.counts = reply; return; }
        return reply();
      }
      if (url.pathname === '/api/sources') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ items: [], configured: 0, enabled: 0 }) });
      if (url.pathname === '/api/index') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ hosts: [{ id: 'studio', label: 'Mac Studio', sources: [{ name: 'Claude', captured_at: new Date(Date.now() - 3 * 86400_000).toISOString(), indexed_at: new Date().toISOString() }] }, { id: 'air', label: 'MacBook Air', sources: [{ name: 'Codex', last_data_at: new Date(Date.now() - 125 * 60_000).toISOString() }] }, { id: 'mini', label: 'Mac mini', sources: [{ name: 'Claude', last_data_at: new Date(Date.now() - 8 * 60_000).toISOString() }] }, { id: 'pro', label: 'Mac Pro', sources: [{ name: 'Codex', last_data_at: new Date(Date.now() - 25_000).toISOString() }] }, { id: 'new', label: 'New Mac', sources: [] }] }) });
      if (url.pathname === '/api/query/library/aggregations') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ metrics: [{ id: 'conversations', buckets: [{ value: 24000 }] }] }) });
      if (url.pathname === '/api/authorship') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ built_at: '2026-09-26T00:00:00Z', totals: { typed_words: 1234, typed_messages: 9876 }, last_30_days: { typed_words: 500 } }) });
      if (url.pathname === '/api/tools/status') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ backfill: { running: false, done: 0, total: 0 }, pending_conversations: 0, conversations: 0 }) });
      if (url.pathname.startsWith('/api/query/') && route.request().method() === 'POST') return route.fulfill({ contentType: 'application/json', body: JSON.stringify(url.pathname.endsWith('/aggregations') ? { metrics: [] } : { rows: [], total: 0 }) });
      if (url.pathname.startsWith('/api/')) return route.fulfill({ contentType: 'application/json', body: '{}' });
      return route.fulfill({ status: 404 });
    });
    await page.goto('http://health.test/settings');
    const card = name => page.locator('#healthCards > .panel', { has: page.locator('.meta', { hasText: new RegExp(`^${name}`) }) });
    // Storage and capture ages render while counts are still outstanding.
    await card('Index size').locator('.big', { hasText: '2.0 KB' }).waitFor();
    assert.match(await card('Index size').textContent(), /0 B free on Archive/);
    for (const removed of ['Internal free', 'Archive free', 'Staging', 'Archive drive']) assert.equal(await card(removed).count(), 0);
    await card('Latest captured data by Mac').getByText('Mac Studio').waitFor();
    assert.equal(await card('Latest captured data by Mac').locator('.big').textContent(), '3d ago');
    assert.match(await card('Latest captured data by Mac').textContent(), /Mac Studio.*3d ago.*MacBook Air.*2h ago.*Mac mini.*8m ago.*Mac Pro.*\d+s ago.*New Mac.*No captured data/s);
    assert.doesNotMatch(await card('Latest captured data by Mac').textContent(), /Claude|Codex|Indexed|just now|2026/);
    if (process.env.PHAROS_UI_SCREENSHOTS) {
      fs.mkdirSync(process.env.PHAROS_UI_SCREENSHOTS, { recursive: true });
      await card('Latest captured data by Mac').screenshot({ path: path.join(process.env.PHAROS_UI_SCREENSHOTS, 'ui-health-capture-ages.png') });
    }
    assert.equal(await page.locator('#healthSources').count(), 0);
    assert.equal(await card('Messages').locator('.big').textContent(), '…');
    assert.equal(await card('Messages').getAttribute('aria-busy'), 'true');
    // A failed section says so without affecting the others.
    await card('Known Pricing').locator('.big', { hasText: 'Unavailable' }).waitFor();
    held.counts();
    await card('Messages').locator('.big', { hasText: (3111202).toLocaleString('en-US') }).waitFor();
    assert.match(await card('Workspaces').textContent(), /24,000 conversations/);
    await card('Messages').getByText('9,876 messages with typed text').waitFor();
    await card('Workspaces').getByText('24,000 conversations').waitFor();
    assert.equal(await card('Messages').getAttribute('aria-busy'), null);
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
});
