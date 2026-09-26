const { test } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { createRequire } = require('node:module');

const root = path.resolve(__dirname, '..');
const { chromium } = createRequire(path.join(root, '.context/browser-tests/package.json'))('playwright');
const source = fs.readFileSync(['src/ai_work_archive/ui.py', 'internal/archive/assets/ui.py'].map(name => path.join(root, name)).find(file => fs.existsSync(file)), 'utf8');
const html = source.slice(source.indexOf("r'''") + 4, source.lastIndexOf("'''"));

test('Library table uses page scroll and conversation cards expose stacked Markdown and work facts', async () => {
  const chrome = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
  const browser = await chromium.launch({ headless: true, ...(fs.existsSync(chrome) ? { executablePath: chrome } : {}) });
  try {
    const page = await browser.newPage({ viewport: { width: 1280, height: 720 } });
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    const rows = Array.from({ length: 35 }, (_, index) => ({
      id: `work-${index}`, title: `Work ${index}`, repository_name: 'example/repo', source_kind: 'codex',
      activity_at: '2026-09-23T12:00:00Z', turn_count: 10, tool_use_count: 23,
      changed_file_count: 4, file_edit_count: 4, token_count: 12000, pr_count: 1,
      cost_usd: 1.5, price_status: 'priced', models: 'gpt-5-codex',
      pr_details: JSON.stringify([{ number: 42, title: 'Useful fix', url: 'https://github.com/example/repo/pull/42', host: 'github.com' }]),
      first_input: '<system_instruction>\nWork in the example workspace.\n</system_instruction>\n**Bold ask** stays literal',
      last_response: 'Latest **answer** with a [safe link](https://example.com).',
    }));
    await page.route('http://library-ui.test/**', route => {
      const url = new URL(route.request().url());
      if (url.pathname === '/' || url.pathname === '/library') return route.fulfill({ contentType: 'text/html', body: html });
      if (url.pathname === '/assets/query-tables.js') return route.fulfill({ contentType: 'application/javascript', body: fs.readFileSync(path.join(root, 'internal/archive/assets/query-tables.js')) });
      if (url.pathname === '/assets/query-tables.css') return route.fulfill({ contentType: 'text/css', body: fs.readFileSync(path.join(root, 'internal/archive/assets/query-tables.css')) });
      if (url.pathname.endsWith('/distinct')) return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ values: [], hasMore: false }) });
      if (url.pathname.startsWith('/api/query/') && route.request().method() === 'POST') return route.fulfill({ contentType: 'application/json', body: JSON.stringify(url.pathname.endsWith('/aggregations') ? { metrics: [] } : { rows, total: rows.length }) });
      if (url.pathname === '/api/work/work-0') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ conversations: [{ provider: 'codex', messages: [
        { native_id: 'u1', role: 'user', kind: 'message', text: '<environment_context>\n<cwd>/tmp</cwd>\n</environment_context>\nFix the bug' },
        { native_id: 'a1', role: 'assistant', kind: 'message', text: 'Fixed **it**.' },
      ] }] }) });
      // Other pages' panels handle an unavailable API; a wrong-shaped stub would crash them.
      if (url.pathname.startsWith('/api/')) return route.fulfill({ status: 404, contentType: 'application/json', body: JSON.stringify({ error: 'not stubbed' }) });
      return route.fulfill({ status: 404 });
    });
    await page.goto('http://library-ui.test/library');
    const table = page.locator('#queryTableLibrary .qt-table-wrap');
    await table.waitFor();
    await page.waitForFunction(() => document.querySelectorAll('#queryTableLibrary .qt-row').length >= 35);
    const layout = await page.evaluate(() => {
      const wrap = document.querySelector('#queryTableLibrary .qt-table-wrap');
      const qb = document.querySelector('#queryTableLibrary .qt-qb');
      return {
        wrapOverflow: getComputedStyle(wrap).overflow,
        wrapRadius: getComputedStyle(wrap).borderRadius,
        tableRadius: getComputedStyle(wrap.querySelector('.qt-table')).borderRadius,
        gap: Math.round(wrap.getBoundingClientRect().top - qb.getBoundingClientRect().bottom),
        documentWidth: document.documentElement.scrollWidth,
        documentHeight: document.documentElement.scrollHeight,
        viewportWidth: innerWidth, viewportHeight: innerHeight,
      };
    });
    assert.equal(layout.wrapOverflow, 'visible');
    assert.equal(layout.wrapRadius, '0px');
    assert.equal(layout.tableRadius, '0px');
    assert.equal(layout.gap, 0);
    assert.ok(layout.documentWidth > layout.viewportWidth, JSON.stringify(layout));
    assert.ok(layout.documentHeight > layout.viewportHeight, JSON.stringify(layout));
    fs.mkdirSync(path.join(root, '.context'), { recursive: true });
    await page.screenshot({ path: path.join(root, '.context/library-table.png') });

    await page.getByRole('button', { name: 'Conversations', exact: true }).click();
    const card = page.locator('.conversation-result-card').first();
    await card.waitFor();
    assert.match(await card.locator('.conversation-result-meta').innerText(), /example\/repo · codex · gpt-5-codex · /);
    assert.match(await card.locator('.conversation-result-facts').innerText(), /10 turns.*23 tool uses.*4 files edited.*\$1\.50 API cost.*#42/s);
    // Actions stack vertically in the card's top-right corner, beside the title and chips.
    const title = await card.locator('.conversation-result-title').boundingBox();
    const facts = await card.locator('.conversation-result-facts').boundingBox();
    const open = await card.getByRole('button', { name: 'Open full conversation' }).boundingBox();
    const browse = await card.getByRole('button', { name: 'Browse turns' }).boundingBox();
    assert.ok(open.x > facts.x + facts.width && Math.abs(open.y - title.y) < 8, JSON.stringify({ title, open }));
    assert.ok(browse.y > open.y && Math.abs(browse.x - open.x) < 2, JSON.stringify({ open, browse }));
    assert.equal(await card.locator('.conversation-result-preview .result-message').count(), 2);
    const first = card.locator('.conversation-result-preview .result-message').first();
    const second = card.locator('.conversation-result-preview .result-message').nth(1);
    assert.ok((await first.boundingBox()).y < (await second.boundingBox()).y);
    // Messages use the conversation view's renderer: XML blocks fold into chips,
    // human text stays literal, and agent text is Markdown.
    const chip = first.locator('.xml-chip');
    assert.equal(await chip.locator(':scope > summary').innerText(), 'system_instruction');
    assert.equal(await chip.getAttribute('open'), null);
    assert.match(await first.locator('.message-prose').innerText(), /\*\*Bold ask\*\* stays literal/);
    await chip.locator(':scope > summary').click();
    assert.match(await chip.locator('.xml-chip-body').innerText(), /Work in the example workspace\./);
    assert.equal(await second.locator('strong').innerText(), 'answer');
    await card.getByRole('button', { name: 'Browse turns' }).click();
    const exchange = card.locator('.result-turn-browser .result-exchange');
    await exchange.waitFor();
    assert.equal(await exchange.locator('.result-message.human .xml-chip > summary').innerText(), 'environment_context');
    assert.match(await exchange.locator('.result-message.human .message-prose').innerText(), /Fix the bug/);
    assert.equal(await exchange.locator('.result-message.assistant strong').innerText(), 'it');
    await page.evaluate(() => window.scrollTo(0, document.querySelector('.conversation-result-card').getBoundingClientRect().top + scrollY - 85));
    await page.screenshot({ path: path.join(root, '.context/library-conversations.png') });
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
});

test('Library filter chips and picker show static option labels while queries and URLs keep raw values', async () => {
  const chrome = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
  const browser = await chromium.launch({ headless: true, ...(fs.existsSync(chrome) ? { executablePath: chrome } : {}) });
  try {
    const page = await browser.newPage({ viewport: { width: 1280, height: 720 } });
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    const queries = [];
    await page.route('http://library-labels.test/**', route => {
      const url = new URL(route.request().url());
      if (url.pathname === '/' || url.pathname === '/library') return route.fulfill({ contentType: 'text/html', body: html });
      if (url.pathname === '/assets/query-tables.js') return route.fulfill({ contentType: 'application/javascript', body: fs.readFileSync(path.join(root, 'internal/archive/assets/query-tables.js')) });
      if (url.pathname === '/assets/query-tables.css') return route.fulfill({ contentType: 'text/css', body: fs.readFileSync(path.join(root, 'internal/archive/assets/query-tables.css')) });
      if (url.pathname.endsWith('/distinct')) return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ values: [], hasMore: false }) });
      if (url.pathname === '/api/query/library' && route.request().method() === 'POST') {
        const query = route.request().postDataJSON();
        queries.push(query);
        // The server rejects the second value so the failed state is visible.
        if (query.where?.some(clause => clause.value === 'tl1-export')) return route.fulfill({ status: 400, contentType: 'application/json', body: JSON.stringify({ error: 'export sources are unavailable' }) });
        return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ rows: [], total: 0 }) });
      }
      if (url.pathname.startsWith('/api/query/')) return route.fulfill({ contentType: 'application/json', body: JSON.stringify(url.pathname.endsWith('/aggregations') ? { metrics: [] } : { rows: [], total: 0 }) });
      // Other pages' panels handle an unavailable API; a wrong-shaped stub would crash them.
      return route.fulfill({ status: 404, contentType: 'application/json', body: JSON.stringify({ error: 'not stubbed' }) });
    });
    const sent = async value => {
      for (let attempt = 0; attempt < 50; attempt++) {
        if (queries.some(query => query.where?.some(clause => clause.field === 'source_kind' && clause.value === value))) return true;
        await page.waitForTimeout(50);
      }
      return false;
    };
    const token = Buffer.from(JSON.stringify({ w: [{ field: 'source_kind', op: '=', value: 'chatgpt' }], l: 50 })).toString('base64url');
    await page.goto(`http://library-labels.test/library?q_library=${token}`);
    const chip = page.getByRole('button', { name: 'Edit source_kind filter value' });
    await chip.waitFor();
    assert.equal(await chip.innerText(), 'ChatGPT');
    assert.ok(await sent('chatgpt'));

    await chip.click();
    await page.getByRole('textbox', { name: 'Search source_kind values' }).fill('export');
    const option = page.getByRole('button', { name: /^TL1 export/ });
    assert.equal((await option.locator('small').innerText()).trim(), 'tl1-export');
    await option.click();
    await page.waitForFunction(() => {
      const token = new URL(location.href).searchParams.get('q_library');
      return token && atob(token.replace(/-/g, '+').replace(/_/g, '/')).includes('"tl1-export"');
    });
    assert.equal(await chip.innerText(), 'TL1 export');
    assert.ok(await sent('tl1-export'));

    await page.locator('#queryTableLibrary .query-table-error').waitFor();
    assert.equal(await page.locator('#queryTableLibrary .query-table-error').innerText(), 'export sources are unavailable');
    assert.match(await page.locator('#queryTableLibrary .qt-table-wrap').innerText(), /the query failed/);
    assert.doesNotMatch(await page.locator('#queryTableLibrary .qt-table-wrap').innerText(), /No work matches/);
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
});

test('Library search asks for substring matches and explanations by default and shows why each row matched', async () => {
  const chrome = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
  const browser = await chromium.launch({ headless: true, ...(fs.existsSync(chrome) ? { executablePath: chrome } : {}) });
  try {
    const page = await browser.newPage({ viewport: { width: 1280, height: 800 } });
    const errors = [];
    page.on('pageerror', error => errors.push(error.message));
    const requests = [];
    const why = {
      score: 0.43, scan: 500,
      parts: [{ kind: 'text', value: 0.4, weight: 0.4 }, { kind: 'related', value: 0.03, weight: 0.6 }],
      substring: { rank: 1, count: 2, terms: ['log_que'], messages: [{ message_id: 'm1', conversation_id: 'c1', role: 'user', kind: 'message', segments: [{ text: 'Why is cata' }, { text: 'log_que', match: true }, { text: 'ry.go slow' }] }] },
      related: { cosine: 0.05, threshold: 0.05, weight: 0.6, exact: true, signal: 0.01, noise: 0.04, scattered: 0.002,
        terms: [{ query: 'bug', matched: ['error'], via: 'concept', concept: 'failure', value: 0.008, fields: ['Title'] }], fields: [{ label: 'Title', value: 0.008 }] },
    };
    const rows = [{ id: 'work-1', title: 'Speed up catalog queries', source_kind: 'codex', activity_at: '2026-09-23T12:00:00Z', why }];
    await page.route('http://library-why.test/**', route => {
      const url = new URL(route.request().url());
      if (url.pathname === '/' || url.pathname === '/library') return route.fulfill({ contentType: 'text/html', body: html });
      if (url.pathname === '/assets/query-tables.js') return route.fulfill({ contentType: 'application/javascript', body: fs.readFileSync(path.join(root, 'internal/archive/assets/query-tables.js')) });
      if (url.pathname === '/assets/query-tables.css') return route.fulfill({ contentType: 'text/css', body: fs.readFileSync(path.join(root, 'internal/archive/assets/query-tables.css')) });
      if (url.pathname === '/api/search/status') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ substring: { ready: false, done: 3, total: 10 } }) });
      if (url.pathname === '/api/query/library' && route.request().method() === 'POST') {
        requests.push(url.search);
        const explained = url.searchParams.get('explain') === '1';
        return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ rows: rows.map(row => explained ? row : { ...row, why: undefined }), total: rows.length }) });
      }
      if (url.pathname === '/api/query/library/aggregations') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ metrics: [] }) });
      if (url.pathname === '/api/query/library/distinct') return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ values: [], hasMore: false }) });
      if (url.pathname.startsWith('/api/')) return route.fulfill({ status: 404, contentType: 'application/json', body: JSON.stringify({ error: 'not stubbed' }) });
      return route.fulfill({ status: 404 });
    });
    await page.goto('http://library-why.test/library?search=log_que');
    const chips = page.locator('#queryTableLibrary .qt-work-why .why-chips');
    await chips.waitFor();
    assert.ok(requests.some(search => /substring=1/.test(search) && /explain=1/.test(search)), requests.join('\n'));
    assert.equal(await chips.innerText(), 'Inside words ×2\nbug ≈ error · weak');
    assert.match(await page.locator('.semantic-hint').innerText(), /3 of 10 conversations/);

    await chips.click();
    const dialog = page.getByRole('dialog', { name: 'Why this work matched' });
    await dialog.waitFor();
    assert.equal(await dialog.locator('mark').innerText(), 'log_que');
    assert.match(await dialog.innerText(), /Related concept “failure”/);
    assert.match(await dialog.innerText(), /Treat this match as weak/);
    await page.keyboard.press('Escape');
    await dialog.waitFor({ state: 'detached' });

    await page.getByLabel('Match inside words').uncheck();
    await page.getByLabel('Show why each result matched').uncheck();
    await page.waitForFunction(() => new URL(location.href).searchParams.get('substring') === '0' && new URL(location.href).searchParams.get('why') === '0');
    await page.waitForFunction(() => !document.querySelector('#queryTableLibrary .why-chips'));
    const last = requests[requests.length - 1];
    assert.doesNotMatch(last, /substring=|explain=/);
    assert.deepEqual(errors, []);
  } finally { await browser.close(); }
});
