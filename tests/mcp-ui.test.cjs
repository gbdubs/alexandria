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

test('MCP page explains connection, shows call history, and gates access', async () => {
  const chrome = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
  const browser = await chromium.launch({ headless: true, ...(fs.existsSync(chrome) ? { executablePath: chrome } : {}) });
  try {
    const page = await browser.newPage({ viewport: { width: 1360, height: 1600 } });
    const errors = [];
    let enabled = true;
    await page.addInitScript(() => { window.webkit = { messageHandlers: { alexandriaClipboard: { postMessage: async text => { window.__copiedText = text; } } } }; });
    page.on('pageerror', error => errors.push(error.message));
    await page.route('http://mcp.test/**', route => {
      const request = route.request();
      const url = new URL(request.url());
      if (url.pathname === '/mcp') return route.fulfill({ contentType: 'text/html', body: html });
      if (url.pathname === '/assets/query-tables.js') return route.fulfill({ contentType: 'application/javascript', body: fs.readFileSync(path.join(root, 'internal/archive/assets/query-tables.js')) });
      if (url.pathname === '/assets/query-tables.css') return route.fulfill({ contentType: 'text/css', body: fs.readFileSync(path.join(root, 'internal/archive/assets/query-tables.css')) });
      if (url.pathname === '/api/mcp' && request.method() === 'GET') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ enabled, transport: 'stdio', command: '/Applications/Alexandria.app/Contents/MacOS/alexandria', args: ['--config', '/tmp/archive.toml', 'mcp'], note: 'Agent clients launch this local command when they connect.' }) });
      if (url.pathname === '/api/mcp/calls') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ items: [{ id: 1, called_at: '2026-09-24T12:00:00Z', tool_name: 'search_conversations', arguments_json: '{"query":"parser"}', status: 'ok', error_text: null, duration_ms: 18, response_bytes: 900, estimated_output_tokens: 300, result_count: 3, truncated: 0 }], filtered_total: 1, tool_stats: [{ tool_name: 'search_conversations', calls: 1, errors: 0, average_output_tokens: 300, largest_output_tokens: 300, truncated_calls: 0, average_duration_ms: 18 }], stats: { total_calls: 1, failed_calls: 0, average_output_tokens: 300, largest_output_tokens: 300, truncated_calls: 0 } }) });
      if (url.pathname === '/api/mcp/enabled') { enabled = JSON.parse(request.postData()).enabled; return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ enabled }) }); }
      if (url.pathname === '/api/query/mcp_calls') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ rows: [{ id: 1, called_at: '2026-09-24T12:00:00Z', tool_name: 'search_conversations', arguments_json: '{"query":"parser"}', status: 'ok', error_text: null, duration_ms: 18, response_bytes: 900, estimated_output_tokens: 300, result_count: 3, truncated: false, day: '2026-09-24', week: '2026-09-21', month: '2026-09', call_count: 1, error_count: 0, truncated_count: 0 }], total: 1 }) });
      if (url.pathname === '/api/query/mcp_calls/aggregations') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ metrics: [] }) });
      if (url.pathname === '/api/query/mcp_calls/distinct') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ values: ['search_conversations'] }) });
      if (url.pathname.startsWith('/api/query/')) return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ rows: [], total: 0, metrics: [] }) });
      if (url.pathname.startsWith('/api/')) return route.fulfill({ contentType: 'application/json', body: '{}' });
      return route.fulfill({ status: 404 });
    });
    await page.goto('http://mcp.test/mcp');
    await page.getByRole('heading', { name: 'Agent access' }).waitFor();
    await page.locator('.mcp-history .qt-table').getByText('search_conversations', { exact: true }).waitFor();
    assert.equal(await page.locator('.mcp-history').evaluate(element => getComputedStyle(element).paddingTop), '0px');
    assert.equal(await page.locator('.mcp-history').evaluate(element => Boolean(element.compareDocumentPosition(document.querySelector('.mcp-setup-grid')) & Node.DOCUMENT_POSITION_FOLLOWING)), true);
    assert.match(await page.locator('.mcp-code').innerText(), /mcpServers/);
    assert.match(await page.locator('.mcp-prompt').innerText(), /get_conversation_messages/);
    assert.match(await page.locator('.mcp-history .qt-table').innerText(), /300/);
    await page.getByRole('button', { name: 'Details' }).click();
    await page.getByRole('dialog', { name: 'MCP call details' }).getByText('{"query":"parser"}').waitFor();
    await page.getByRole('dialog').getByRole('button', { name: 'Close' }).click();
    await page.getByRole('button', { name: 'By tool' }).click();
    await page.getByRole('button', { name: 'Clear metrics' }).click();
    await page.getByRole('button', { name: 'Copy JSON' }).click();
    assert.match(await page.evaluate(() => window.__copiedText), /mcpServers/);
    await page.getByRole('button', { name: 'Copy prompt' }).click();
    assert.match(await page.evaluate(() => window.__copiedText), /get_conversation_messages/);
    await page.getByRole('switch', { name: 'Enable MCP' }).click();
    await page.getByText('Off', { exact: true }).waitFor();
    assert.equal(enabled, false);
    assert.deepEqual(errors, []);
    await page.screenshot({ path: path.join(root, '.context/mcp-page.png'), fullPage: true });
  } finally { await browser.close(); }
});
