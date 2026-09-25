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
      freshness: { sources: [{ source_name: 'Claude', capability: 'read', coverage: 'complete', last_success_at: '2026-09-24' }] },
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
      if (url.pathname === '/api/tools/status') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ backfill: { running: false, done: 0, total: 0 }, pending_conversations: 0, conversations: 0 }) });
      if (url.pathname.startsWith('/api/query/') && route.request().method() === 'POST') return route.fulfill({ contentType: 'application/json', body: JSON.stringify(url.pathname.endsWith('/aggregations') ? { metrics: [] } : { rows: [], total: 0 }) });
      if (url.pathname.startsWith('/api/')) return route.fulfill({ contentType: 'application/json', body: '{}' });
      return route.fulfill({ status: 404 });
    });
    await page.goto('http://health.test/settings');
    const card = name => page.locator('#healthCards > .panel', { has: page.locator('.meta', { hasText: new RegExp(`^${name}`) }) });
    // Storage and freshness render while counts are still outstanding.
    await card('Index size').locator('.big', { hasText: '2.0 KB' }).waitFor();
    await page.locator('#healthSources', { hasText: 'Claude' }).waitFor();
    assert.equal(await card('Messages').locator('.big').textContent(), '…');
    assert.equal(await card('Messages').getAttribute('aria-busy'), 'true');
    // A failed section says so without affecting the others.
    await card('Priced usage').locator('.big', { hasText: 'Unavailable' }).waitFor();
    held.counts();
    await card('Messages').locator('.big', { hasText: (3111202).toLocaleString('en-US') }).waitFor();
    assert.equal(await card('Messages').getAttribute('aria-busy'), null);
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
});
