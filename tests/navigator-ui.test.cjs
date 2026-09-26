const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { createRequire } = require('node:module');

const root = path.resolve(__dirname, '..');
const requireLocal = createRequire(path.join(root, '.context/browser-tests/package.json'));
const { chromium } = requireLocal('playwright');
const source = fs.readFileSync(['src/ai_work_archive/ui.py', 'internal/archive/assets/ui.py'].map(name => path.join(root, name)).find(file => fs.existsSync(file)), 'utf8');
const html = source.slice(source.indexOf("r'''") + 4, source.lastIndexOf("'''"));

test('navigator restores routes, library search, view, and query filters', async () => {
  const chrome = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
  const browser = await chromium.launch({ headless: true, ...(fs.existsSync(chrome) ? { executablePath: chrome } : {}) });
  try {
    const page = await browser.newPage();
    const errors = [];
    const libraryQueries = [];
    page.on('pageerror', error => errors.push(error.stack));
    await page.route('http://navigator.test/**', route => {
      const url = new URL(route.request().url());
      if (['/', '/library', '/sources', '/activity', '/upcoming', '/health'].includes(url.pathname)) return route.fulfill({ contentType: 'text/html', body: html });
      if (url.pathname === '/assets/query-tables.js') return route.fulfill({ contentType: 'application/javascript', body: fs.readFileSync(path.join(root, 'internal/archive/assets/query-tables.js')) });
      if (url.pathname === '/assets/query-tables.css') return route.fulfill({ contentType: 'text/css', body: fs.readFileSync(path.join(root, 'internal/archive/assets/query-tables.css')) });
      if (url.pathname.endsWith('/distinct')) return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ values: [], hasMore: false }) });
      if (url.pathname.startsWith('/api/query/') && route.request().method() === 'POST') {
        if (url.pathname === '/api/query/library') libraryQueries.push(route.request().postDataJSON());
        return route.fulfill({ contentType: 'application/json', body: JSON.stringify(url.pathname.endsWith('/aggregations') ? { metrics: [] } : { rows: [], total: 0 }) });
      }
      if (url.pathname === '/api/sources') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ items: [], configured: 0, enabled: 0 }) });
      if (url.pathname.startsWith('/api/')) return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ items: [] }) });
      return route.fulfill({ status: 404 });
    });
    await page.goto('http://navigator.test/library');
    await page.getByRole('button', { name: 'Navigation bar' }).click();
    assert.match(await page.locator('#navigatorURI').inputValue(), /\/library/);
    await page.getByRole('searchbox', { name: 'Search text and related terms' }).fill('debugging');
    await page.getByRole('button', { name: 'Search library' }).and(page.getByText('Search library')).click();
    assert.equal(new URL(page.url()).searchParams.get('search'), 'debugging');
    await page.reload();
    assert.equal(await page.getByRole('searchbox', { name: 'Search text and related terms' }).inputValue(), 'debugging');
    await page.getByRole('button', { name: 'Conversations', exact: true }).click();
    await page.evaluate(() => window.alexandriaQueryTables.filterRepository('example/repo'));
    await page.waitForFunction(() => new URL(location.href).searchParams.has('q_library'));
    const saved = page.url();
    assert.match(saved, /view=conversation/);
    assert.match(saved, /q_library=/);
    await page.getByRole('button', { name: 'Sources', exact: true }).click();
    assert.equal(new URL(page.url()).pathname, '/sources');
    await page.goBack();
    assert.equal(new URL(page.url()).pathname, '/library');
    await page.goForward();
    assert.equal(new URL(page.url()).pathname, '/sources');
    await page.getByRole('button', { name: 'Navigation bar' }).click();
    await page.locator('#navigatorURI').fill(saved);
    await page.getByRole('button', { name: 'Go', exact: true }).click();
    await page.waitForURL(/\/library\?/);
    assert.equal(await page.getByRole('button', { name: 'Conversations', exact: true }).getAttribute('aria-pressed'), 'true');
    await page.waitForTimeout(250);
    assert.ok(libraryQueries.some(query => query.where?.some(clause => clause.value === 'example/repo')));
    assert.equal(page.url(), saved);
    await page.reload();
    assert.equal(await page.getByRole('button', { name: 'Conversations', exact: true }).getAttribute('aria-pressed'), 'true');
    await page.waitForTimeout(250);
    assert.ok(libraryQueries.some(query => query.where?.some(clause => clause.value === 'example/repo')));
    assert.equal(page.url(), saved);
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
});

test('Past Work shows linked GitHub PRs and origin/main merges', async () => {
  const chrome = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
  const browser = await chromium.launch({ headless: true, ...(fs.existsSync(chrome) ? { executablePath: chrome } : {}) });
  try {
    const page = await browser.newPage();
    await page.route('http://navigator.test/**', route => {
      const url = new URL(route.request().url());
      if (url.pathname === '/library') return route.fulfill({ contentType: 'text/html', body: html });
      if (url.pathname === '/assets/query-tables.js') return route.fulfill({ contentType: 'application/javascript', body: fs.readFileSync(path.join(root, 'internal/archive/assets/query-tables.js')) });
      if (url.pathname === '/assets/query-tables.css') return route.fulfill({ contentType: 'text/css', body: fs.readFileSync(path.join(root, 'internal/archive/assets/query-tables.css')) });
      if (url.pathname === '/api/query/library' && route.request().method() === 'POST') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ rows: [{
        id: 'work-1', title: 'Past work', repository_name: 'alexandria', source_kind: 'conductor',
        pr_count: 2, pr_numbers: '42,73', canonical_remote: 'git@github.com:acme/alexandria.git',
        main_merge_commit: 'abcdef1234567890', main_merge_title: 'Merge feature into main', main_merge_method: 'merge', main_merge_url: 'https://github.com/acme/alexandria/commit/abcdef1234567890',
        pr_details: JSON.stringify([{ number: 42, title: 'Improve archive search', url: 'https://github.com/acme/alexandria/pull/42', host: 'github.com' }, { number: 73, title: null, url: null, host: 'github.com' }]),
      }], total: 1 }) });
      if (url.pathname.startsWith('/api/query/') && route.request().method() === 'POST') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ metrics: [] }) });
      if (url.pathname === '/api/sources') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ items: [], configured: 0, enabled: 0 }) });
      if (url.pathname.startsWith('/api/')) return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ items: [] }) });
      return route.fulfill({ status: 404 });
    });
    await page.goto('http://navigator.test/library');
    const titled = page.getByRole('link', { name: 'Improve archive search · #42' });
    await titled.waitFor();
    assert.equal(await titled.getAttribute('href'), 'https://github.com/acme/alexandria/pull/42');
    assert.equal(await page.getByRole('link', { name: 'PR #73' }).getAttribute('href'), 'https://github.com/acme/alexandria/pull/73');
    assert.equal(await titled.getAttribute('target'), '_blank');
    assert.equal(await page.getByRole('link', { name: 'Merged: Merge feature into main' }).getAttribute('href'), 'https://github.com/acme/alexandria/commit/abcdef1234567890');
  } finally { await browser.close(); }
});
