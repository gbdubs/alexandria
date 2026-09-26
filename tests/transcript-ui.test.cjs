// Run: npm install --prefix .context/browser-tests playwright --no-audit --no-fund
//      node --test tests/transcript-ui.test.cjs
// Uses the real embedded HTML and CSS, with network responses stubbed locally.
const { test, before, after } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const path = require('node:path');
const { createRequire } = require('node:module');
const root = path.resolve(__dirname, '..');
const localRequire = createRequire(path.join(root, '.context/browser-tests/package.json'));
const { chromium } = localRequire('playwright');
const source = fs.readFileSync(['src/pharos/ui.py', 'internal/archive/assets/ui.py'].map(name => path.join(root, name)).find(file => fs.existsSync(file)), 'utf8');
const html = source.slice(source.indexOf("r'''") + 4, source.lastIndexOf("'''"));
let browser;
before(async () => {
  const chrome = process.env.CHROME_PATH || '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
  browser = await chromium.launch({ headless: true, ...(fs.existsSync(chrome) ? { executablePath: chrome } : {}) });
});
after(async () => { await browser?.close(); });

function event(id, kind, text, seconds = 0, extra = {}) {
  return { native_id: id, role: kind === 'tool_result' ? 'tool' : 'assistant', kind,
    text: typeof text === 'string' ? text : JSON.stringify(text),
    created_at: new Date(Date.UTC(2026, 8, 20, 12, 0, seconds)).toISOString(), ...extra };
}
const human = (text = 'Inspect the rules and report the result.') => event('human', 'message', text, 0, { role: 'user' });

test('all source accounting formats reconcile against shared fixtures', async t => {
  const page=await fixture(t,[]);
  const cases=JSON.parse(fs.readFileSync(path.join(root,'tests/token-usage-fixtures.json'),'utf8'));
  for(const item of cases){const result=await page.evaluate(events=>conversationTokenReports(events.map((event,i)=>({native_id:String(i),text:JSON.stringify(event)}))).reduce((sum,report)=>sum+(report?.total||0),0),item.events);assert.equal(result,item.total,item.name)}
});

test('token usage is a top-level tab instead of a Health table', async t => {
  const page = await browser.newPage({ viewport: { width: 900, height: 800 } });
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.route('http://transcript.test/**', route => {
    const url = new URL(route.request().url());
    if (url.pathname === '/' || url.pathname === '/usage') return route.fulfill({ contentType: 'text/html', body: html });
    const health = {
      counts: { workspaces: 2, messages: 12 },
      storage: { volume_name: 'Archive', index_bytes: 1024, internal_free_bytes: 2048, free_bytes: 4096, path: '/archive', staging_bytes: 0, staging_cap_bytes: 1024, status: 'available' },
      freshness: { sources: [] },
    }[url.pathname.replace('/api/health/', '')];
    if (health) return route.fulfill({ contentType: 'application/json', body: JSON.stringify(health) });
    if (url.pathname.startsWith('/api/')) return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ items: [] }) });
    return route.fulfill({ body: '', contentType: 'application/javascript' });
  });
  await page.goto('http://transcript.test/');
  await page.getByRole('button', { name: 'Usage', exact: true }).click();
  assert.equal(new URL(page.url()).pathname, '/usage');
  assert.equal(await page.locator('#usage.view.active #queryTableUsage').count(), 1);
  await page.getByRole('button', { name: 'Settings', exact: true }).click();
  await page.locator('#healthCards .metric, #healthCards > *').first().waitFor();
  assert.equal(await page.locator('#health table').count(), 0);
  await page.close();
  assert.deepEqual(errors, [], 'No browser exceptions');
});

test('token totals count increments once and stay visible on collapsed turns and events', async t => {
  const usage = (id,total) => event(id,'metadata',{type:'token_count',info:{total_token_usage:{total_tokens:total},last_token_usage:{input_tokens:total,output_tokens:0}}});
  const page = await fixture(t,[human(),usage('u1',12000),usage('duplicate',12000),
    event('h2','message','Continue',1,{role:'user'}),usage('u2',150000),
    event('compact','metadata',{type:'compacted',payload:{summary:'Context reduced'}})]);
  assert.deepEqual(await page.locator('.turn > summary .token-badge').allTextContents(),['12K','138K']);
  await page.getByRole('button',{name:'Close turns',exact:true}).click();
  assert.equal(await page.locator('.turn > summary .token-badge:visible').count(),2);
  assert.equal(await page.getByRole('button',{name:/Highest-spend turn/}).count(),0);
  await page.locator('.token-map .compaction-marker').click();
  assert.equal(await page.locator('.turn').nth(1).getAttribute('open'),'');
  assert.equal(await page.locator('.token-map .compaction-marker').count(),1);
  assert.equal(await page.locator('.turn-body .token-badge.token-high:visible').count(),1);
  assert.match(await page.locator('.turn-body .token-badge.token-high').first().getAttribute('style'),/--heat-color:\s*color-mix\(in oklch,\s*var\(--heat-2\),\s*var\(--heat-3\)/);
  await page.locator('.token-map .event-marker').first().click();
  assert.equal(await page.locator('.turn').first().getAttribute('open'),'');
});

test('split message usage includes cache once and reconciles result totals', async t => {
  const message={id:'request-1',usage:{input_tokens:100,output_tokens:20,cache_read_input_tokens:1000,cache_creation_input_tokens:200},content:[{type:'text',text:'Reply'},{type:'thinking',thinking:'Thought'}]};
  const page=await fixture(t,[human(),event('reply','message',{message}),event('retry','message',{message:{...message,usage:{...message.usage,output_tokens:30}}}),event('result','metadata',{type:'result',usage:{total_tokens:1330}})]);
  assert.deepEqual(await page.locator('.turn > summary .token-badge').allTextContents(),['1.3K']);
  assert.equal(await page.locator('.turn-body .context-badge').count(),1);
  assert.equal(await page.locator('.turn > summary .token-badge').getAttribute('data-tokens'),'1330');
  assert.equal(await page.evaluate(()=>tokenTotal({input_tokens:100,output_tokens:20,cached_input_tokens:80,reasoning_output_tokens:10})),120);
});

test('token badges sit in a fixed-width trailing column so Details and summaries stay aligned', async t => {
  const call=(id,tokens,s)=>event(id,'message',{message:{id:id+'-req',usage:{input_tokens:tokens,output_tokens:10},content:[{id:id+'-call',type:'tool_use',name:'Bash',input:{command:'echo '+id}}]}},s);
  const page=await fixture(t,[human(),call('small',1000,1),call('big',140000,2),call('tiny',1100,3)]);
  const rows=page.locator('.action-row .tool-event > summary');
  assert.equal(await rows.count(),3);
  const layout=await rows.evaluateAll(list=>list.map(summary=>{
    const children=[...summary.children],hint=summary.querySelector('.line-details-hint'),column=summary.querySelector(':scope > .token-column');
    return {last:children.at(-1)===column,afterHint:children.indexOf(column)>children.indexOf(hint),hint:hint.getBoundingClientRect().left,column:column.getBoundingClientRect().left,width:column.getBoundingClientRect().width};
  }));
  for(const row of layout){assert.ok(row.last&&row.afterHint,'Token column is the trailing column after Details')}
  assert.equal(new Set(layout.map(r=>r.hint)).size,1,'Details does not move with badge width');
  assert.equal(new Set(layout.map(r=>r.column)).size,1);
  assert.equal(new Set(layout.map(r=>r.width)).size,1);
  const turnColumn=page.locator('.turn > summary > .token-column');
  assert.equal(await turnColumn.locator('.overview-spend').count(),1);
  assert.equal(await turnColumn.evaluate(el=>el.getBoundingClientRect().width),layout[0].width);
});

test('conversations without usage do not reserve a token column', async t => {
  const page=await fixture(t,[human(),event('run','tool_call',{name:'Bash',input:{command:'ls'}},1,{call_id:'run'}),event('reply','message','Done',2)]);
  assert.ok(await page.locator('.token-column').count()>0);
  assert.equal(await page.locator('.token-column:visible').count(),0);
});

test('missing usage is unavailable rather than zero tokens', async t => {
  const page=await fixture(t,[human(),event('reply','message','Hello')],{width:390,height:844});
  assert.match(await page.locator('.token-legend').innerText(),/Token usage unavailable/);
  assert.equal(await page.locator('.token-badge').count(),0);
  assert.equal(await page.locator('.token-map').count(),0);
});

test('conversation header aligns stats and actions, and model searches Library', async t => {
  const page=await fixture(t,[human(),event('reply','message',{message:{id:'r',usage:{input_tokens:1200,output_tokens:100},content:[{type:'text',text:'Done'}]}})],{width:1177,height:780},{model:'claude-opus-5-5',coverage:'complete'});
  const head=page.locator('.conversation-head');
  assert.equal(await head.locator('.token-legend').innerText(),'1.3K tokens spent');
  assert.equal(await head.locator('.compaction-badge').count(),0);
  const positions=await head.evaluate(el=>Object.fromEntries(['.model-search','.chip:not(.model-search)','.token-legend','.source-details','.conversation-toolbar','.conversation-title-row'].map(selector=>{const box=el.querySelector(selector).getBoundingClientRect();return[selector,{x:box.x,y:box.y,right:box.right,bottom:box.bottom}]})));
  assert.ok(positions['.model-search'].x<positions['.chip:not(.model-search)'].x);
  assert.ok(Math.abs(positions['.token-legend'].y-positions['.chip:not(.model-search)'].y)<5);
  assert.ok(Math.abs(positions['.source-details'].y-positions['.chip:not(.model-search)'].y)<5);
  assert.ok(positions['.conversation-toolbar'].x>positions['.source-details'].right);
  assert.ok(positions['.conversation-toolbar'].bottom<=positions['.conversation-title-row'].bottom+1);
  await head.locator('.source-details > summary').click();
  assert.equal(await head.locator('.source-details').getAttribute('open'),'');
  await head.locator('.source-details > summary').click();
  if(process.env.HEADER_SCREENSHOT)await page.screenshot({path:path.join(root,'.context/conversation-header.png')});
  await page.evaluate(()=>{window.pharosQueryTables={filterModel:model=>window.selectedModel=model}});
  await head.locator('.model-search').click();
  assert.equal(new URL(page.url()).pathname,'/library');
  assert.equal(await page.evaluate(()=>window.selectedModel),'claude-opus-5-5');
});

test('deferred long turns retain profiles and spend before materialization', async t => {
  const request=event('usage','message',{message:{id:'long-request',usage:{input_tokens:12000,output_tokens:100},content:[{type:'text',text:'Final response'}]}});
  const page=await fixture(t,[human(),...Array.from({length:301},(_,i)=>event('filler-'+i,'message','Filler')),request]);
  assert.equal(await page.locator('.conversation-panel').getAttribute('data-deferred'),'true');
  assert.equal(await page.locator('.turn-body .timeline-entry').count(),0,'Bodies remain lazy');
  assert.equal(await page.locator('.turn > summary .overview-spend').getAttribute('data-tokens'),'12100');
  const marker=page.locator('.token-map .event-marker');
  assert.equal(await marker.getAttribute('data-context'),'12000');
  await marker.click();
  assert.equal(await page.locator('.context-badge:visible').count(),1);
  assert.equal(await page.locator('.turn > summary .overview-spend').count(),1);
  await page.getByRole('button',{name:'Close turns',exact:true}).click();
  assert.equal(await page.getByRole('button',{name:/Highest-spend turn/}).count(),0);
  await marker.click();
  assert.equal(await page.locator('.turn').getAttribute('open'),'');
  assert.equal(await marker.count(),1,'No duplicate profile after materialization');
});

test('many lazy turns keep all profiles, compactions, nested usage and highest-spend navigation', async t => {
  const messages=[];
  for(let i=0;i<45;i++)messages.push(event('h'+i,'message','Turn '+i,i,{role:'user'}),event('u'+i,'message',{message:{id:'r'+i,usage:{input_tokens:100+i,output_tokens:10},content:[{type:'text',text:'Reply'}]}},i));
  messages.push(event('agent','delegation',{tool:'Task',input:{description:'Review'}},46,{call_id:'child'}),event('child-request','message',{message:{id:'child-r',usage:{input_tokens:5000,output_tokens:100},content:[{type:'text',text:'Child reply'}]}},47,{parent_native_id:'child'}),event('compact','compaction',{type:'compacted'},48));
  const page=await fixture(t,messages);
  assert.equal(await page.locator('.turn > summary .overview-spend').count(),45);
  assert.equal(await page.locator('.token-map .event-marker').count(),46);
  assert.equal(await page.locator('.token-map .compaction-marker').count(),1);
  const last=page.locator('.turn').last();
  assert.equal(await last.locator('.timeline-entry').count(),0);
  assert.equal(await last.locator(':scope > summary .overview-spend').getAttribute('data-tokens'),'5254');
  assert.equal(await page.getByRole('button',{name:/Highest-spend turn/}).count(),0);
  await page.locator('.token-marker[data-stream="child"]').click();
  assert.equal(await last.getAttribute('open'),'');
  assert.equal(await last.locator('.subagent-run > summary .overview-spend').getAttribute('data-tokens'),'5100');
  assert.equal(await page.locator('.turn').nth(30).locator('.timeline-entry').count(),0,'Unrelated turns stay unmaterialized');
  await page.locator('.token-marker[data-stream="child"]').click();
  assert.equal(await last.locator('.subagent-run').getAttribute('open'),'');
  assert.equal(await page.locator('.token-map .event-marker').count(),46);
});

test('context profiles isolate streams, reset at compaction, and never use spend totals', async t => {
  const page=await fixture(t,[]);
  const result=await page.evaluate(()=>{
    const events=[
      {type:'assistant',message:{id:'a',usage:{input_tokens:100,cache_read_input_tokens:900,output_tokens:30}}},
      {type:'assistant',message:{id:'b',usage:{input_tokens:100,cache_read_input_tokens:1000,output_tokens:40}}},
      {type:'assistant',parent_tool_use_id:'child',message:{id:'c',usage:{input_tokens:50,output_tokens:20}}},
      {type:'assistant',message:{id:'d',usage:{input_tokens:100,cache_read_input_tokens:1200,output_tokens:50}}},
      {type:'compacted'},
      {type:'assistant',message:{id:'e',usage:{input_tokens:300,output_tokens:10}}},
      {type:'result',usage:{total_tokens:90000}},
      {type:'assistant',parent_tool_use_id:'child',message:{id:'f',usage:{input_tokens:150,output_tokens:20}}},
    ].map(e=>({text:JSON.stringify(e)}));
    const reports=contextTokenReports(events,conversationTokenReports(events));
    const target=document.createElement('div');attachTokenBadge(target,[{tokenReport:reports[1]},{tokenReport:reports[3]}]);
    return {samples:reports.map(r=>r?.contextSample||null),group:target.querySelector('.context-badge').textContent,tip:target.querySelector('.context-badge').title};
  });
  assert.deepEqual(result.samples.map(s=>s?.size??null),[1000,1100,50,1300,null,300,null,150]);
  assert.deepEqual(result.samples.map(s=>s?.delta??null),[null,100,null,200,null,null,null,100]);
  assert.match(result.group,/\+300 · 2 steps/);
  assert.doesNotMatch(result.group,/2\.4K/);
  assert.match(result.tip,/not an exact token count/);
});

test('context rail has linear widths, compressed collapsed profiles, and no turn spend markers', async t => {
  const request=(id,input)=>event(id,'message',{message:{id,usage:{input_tokens:input,output_tokens:10},content:[{type:'text',text:id}]}});
  const page=await fixture(t,[human(),request('first',1000),request('second',2000),event('compact','compaction',{type:'compacted'}),request('after',500)]);
  await page.waitForFunction(()=>document.querySelector('.token-marker')?.style.getPropertyValue('--context-profile'));
  const markers=page.locator('.token-map .event-marker');
  assert.deepEqual(await markers.evaluateAll(es=>es.map(e=>e.dataset.context)),['1000','2000','500']);
  assert.match(await markers.nth(0).evaluate(e=>e.style.getPropertyValue('--context-profile')),/50%/);
  assert.match(await markers.nth(1).evaluate(e=>e.style.getPropertyValue('--context-profile')),/100%/);
  assert.match(await markers.nth(2).evaluate(e=>e.style.getPropertyValue('--context-profile')),/25%/);
  assert.equal(await page.locator('.token-map .turn-marker').count(),0);
  assert.equal(await page.locator('.context-badge:visible').count(),3);
  if(process.env.CONTEXT_SCREENSHOT)await page.screenshot({path:path.join(root,'.context/context-growth.png')});
  await page.getByRole('button',{name:'Close turns',exact:true}).click();
  await page.waitForFunction(()=>document.querySelectorAll('.token-marker.collapsed-context').length===4);
  assert.equal(await markers.nth(0).evaluate(e=>getComputedStyle(e,'::before').backgroundImage),'none');
  assert.equal(await page.locator('.turn > summary .spend-badge:visible').count(),1);
  await markers.nth(2).click();
  assert.equal(await page.locator('.turn').getAttribute('open'),'');
});

test('context rail widths are shares of the context window and compactions show their trigger', async t => {
  const claude=(id,input,seconds)=>event(id,'message',{message:{id,model:'claude-opus-5-5',usage:{input_tokens:input,output_tokens:10},content:[{type:'text',text:id}]}},seconds);
  const page=await fixture(t,[human(),claude('first',50000,1),claude('second',100000,2),event('manual','metadata',{type:'system',subtype:'compact_boundary',compactMetadata:{trigger:'manual',preTokens:100000}},3),claude('third',20000,4),event('auto','compaction',{type:'compacted',compaction_trigger:'auto'},5)]);
  await page.waitForFunction(()=>document.querySelector('.token-marker')?.style.getPropertyValue('--context-profile'));
  const markers=page.locator('.token-map .event-marker');
  assert.match(await markers.nth(0).evaluate(e=>e.style.getPropertyValue('--context-profile')),/,25% 100%/);
  assert.match(await markers.nth(1).evaluate(e=>e.style.getPropertyValue('--context-profile')),/,50% 100%/);
  assert.match(await markers.nth(1).getAttribute('title'),/50% of 200K window/);
  assert.match(await page.locator('.token-map').getAttribute('title'),/200K-token context window/);
  assert.equal(await page.locator('.token-map .compaction-marker.manual-compaction').count(),1);
  assert.equal(await page.locator('.token-map .compaction-marker.auto-compaction').count(),1);
  const color=selector=>page.locator(selector).evaluate(e=>getComputedStyle(e,'::before').backgroundColor);
  assert.notEqual(await color('.token-map .manual-compaction'),await color('.token-map .auto-compaction'));
  const badge=page.locator('.timeline .compaction-badge.manual-compaction, .turn .compaction-badge.manual-compaction').first();
  assert.equal(await badge.getAttribute('data-label'),'Manual Compaction');
  assert.equal(await badge.innerText(),'');
  await badge.hover();
  assert.equal(await badge.evaluate(el=>getComputedStyle(el,'::after').content),'"Manual Compaction"');
});

test('context windows come from Codex checkpoints or grow to 1M once Claude exceeds 200K', async t => {
  const page=await fixture(t,[]);
  const result=await page.evaluate(()=>{
    const events=[
      {type:'event_msg',payload:{type:'token_count',info:{total_token_usage:{input_tokens:10},model_context_window:258400}}},
      {type:'token_usage_record',payload:{response_id:'r',usage:{input_tokens:1000,output_tokens:5},thread_token_usage:{input_tokens:1000,output_tokens:5}}},
    ].map(e=>({text:JSON.stringify(e)}));
    const codex=contextTokenReports(events,conversationTokenReports(events)).map(r=>r?.contextSample).filter(Boolean);
    return {codex:contextWindow(codex),claude:contextWindow([{model:'claude-sonnet-5',size:150000}]),large:contextWindow([{model:'claude-sonnet-5',size:150000},{model:'claude-sonnet-5',size:250000}]),other:contextWindow([{model:'local',size:5}])};
  });
  assert.deepEqual(result,{codex:258400,claude:200000,large:1000000,other:null});
});

test('nested subagent checkpoints count once and remain nested sessions', async t => {
  const messages = [human(),
    event('outer-call','delegation',{tool:'spawn_agent',input:{task:'outer'}},1,{call_id:'outer'}),
    event('outer-request','metadata',{type:'assistant',parent_tool_use_id:'outer',message:{id:'outer-request',usage:{total_tokens:120}}},2,{parent_native_id:'outer'}),
    event('inner-call','delegation',{tool:'spawn_agent',input:{task:'inner'}},3,{call_id:'inner',parent_native_id:'outer'}),
    event('inner-request','metadata',{type:'assistant',parent_tool_use_id:'inner',message:{id:'inner-request',usage:{total_tokens:60}}},4,{parent_native_id:'inner'}),
    event('inner-total','metadata',{type:'system',subtype:'task_notification',tool_use_id:'inner',usage:{total_tokens:80}},5,{parent_native_id:'inner'}),
    event('outer-total','metadata',{type:'system',subtype:'task_notification',tool_use_id:'outer',usage:{total_tokens:250}},6,{parent_native_id:'outer'}),
    event('result','result',{type:'result',usage:{total_tokens:300}},7),
  ];
  const page = await fixture(t,messages);
  assert.equal(await page.locator('.turn > summary .token-badge').getAttribute('data-tokens'),'300');
  assert.equal(await page.locator('.subagent-run').count(),2);
});
const read = (id = 'read', file = '/workspace/docs/RULES.md', seconds = 10) => event(id, 'message', {
  message: { id: 'provider-message-id', model: 'claude-opus-4-5', content: [
    { id: `${id}-call`, type: 'tool_use', name: 'Read', input: { file_path: file } },
  ] }, context_management: null,
}, seconds);

async function fixture(t, messages, viewport = { width: 1280, height: 900 }, conversation = {}) {
  const page = await browser.newPage({ viewport });
  page.setDefaultTimeout(5000);
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.route('http://transcript.test/**', route => {
    const url = new URL(route.request().url());
    if (url.pathname === '/') return route.fulfill({ contentType: 'text/html', body: html });
    if (url.pathname.startsWith('/api/')) return route.fulfill({ contentType: 'application/json', body: JSON.stringify({ items: [] }) });
    return route.fulfill({ body: '', contentType: 'application/javascript' });
  });
  await page.goto('http://transcript.test/');
  await page.addStyleTag({ content: 'body > :not(#transcript-test-root):not(.conversation-map):not(.conversation-find):not(.conversation-find-rail):not(.conversation-map-window) { display:none !important } #transcript-test-root { max-width:1100px;margin:20px auto; }' });
  await page.evaluate(({messages,conversation}) => {
    const root = document.createElement('main'); root.id = 'transcript-test-root';
    root.append(renderConversation({ provider: 'claude', native_id: 'fixture', repo_root: '/workspace', ...conversation, messages }, conversation.workspace_roots||[]));
    document.body.append(root);
  }, {messages,conversation});
  if (process.env.TRANSCRIPT_SCREENSHOTS) {
    const output = path.join(root, '.context/browser-tests/screenshots');
    fs.mkdirSync(output, { recursive: true });
    await page.screenshot({ path: path.join(output, `${t.name.replace(/[^a-z0-9]+/gi, '-').toLowerCase()}.png`), fullPage: true });
  }
  t.after(async () => {
    if (process.env.TRANSCRIPT_SCREENSHOTS && /file edit|delegation|failed Edit/i.test(t.name)) {
      await page.emulateMedia({ colorScheme: 'dark' });
      await page.locator('.tool-event, .subagent-run').evaluateAll(rows => rows.forEach(row => row.open = true));
      await page.screenshot({ path: path.join(root, '.context/browser-tests/screenshots', `${t.name.replace(/[^a-z0-9]+/gi, '-').toLowerCase()}-dark-expanded.png`), fullPage: true });
    }
    await page.close(); assert.deepEqual(errors, [], 'No browser exceptions');
  });
  return page;
}
const transcript = page => page.locator('#transcript-test-root');

async function workFixture(t, overrides = {}) {
  const page = await fixture(t, []);
  const work = { id:'work', title:'Inspect renderer', repository_name:'pharos', branch:'feature', location:'/workspace',
    summary:{initiation:'Duplicate initiation',outcome:'Duplicate outcome',failures:'Duplicate failures'},
    prs:[{number:42,title:'Improve archive search',state:'open',url:'https://example.com/pr/42'}],
    attempts:[{attempt_no:1,result:'Retained checkpoint'}], handoffs:[],
    changes:[{path:'/workspace/docs/RULES.md',classification:'source',status:'modified',complete:true}],
    metrics:[{name:'input_tokens',value:100,unit:'tokens',status:'observed'}],identity_links:[],
    receipt:{outcome:'preserved',package_bytes:1024,package_hash:'receipt-hash'},
    conversations:[{provider:'claude',native_id:'one',coverage:'complete',messages:[human(),read()]}], ...overrides };
  await page.route('http://transcript.test/api/work/work', route => route.fulfill({contentType:'application/json',body:JSON.stringify(work)}));
  await page.evaluate(async () => {
    await detail('work');
    document.querySelector('#transcript-test-root').replaceChildren(document.querySelector('#detailBody'));
  });
  return page;
}

test('workspace leads with transcript and keeps supporting information collapsed', async t => {
  const page = await workFixture(t);
  const text = await transcript(page).innerText();
  assert.match(text,/Improve archive search · #42/);
  assert.equal(await page.getByRole('link',{name:'Improve archive search · #42',exact:true}).getAttribute('href'),'https://example.com/pr/42');
  assert.doesNotMatch(text,/Duplicate|Retained checkpoint|receipt-hash|Input tokens/);
  assert.ok((await page.locator('.turn').boundingBox()).y < 450);
  assert.equal(await page.locator('.coverage-warning').count(),0);
  await page.locator('.file-index > summary').click();
  await page.locator('.file-index-link').click();
  assert.equal(await page.getByRole('searchbox',{name:'Find in conversation'}).inputValue(),'/workspace/docs/RULES.md');
  assert.equal(await page.locator('.file-index').getAttribute('open'),null);
  await page.locator('.archive-details > summary').click();
  assert.match(await transcript(page).innerText(),/Retained checkpoint/);
  assert.match(await transcript(page).innerText(),/receipt-hash/);
  await page.locator('#detailBody').getByText('Usage',{exact:true}).click();
  assert.match(await transcript(page).innerText(),/Input tokens/);
  if(process.env.TRANSCRIPT_SCREENSHOTS){await page.locator('.archive-details > summary').click();await page.locator('#detailBody').getByText('Usage',{exact:true}).click();await page.screenshot({path:path.join(root,'.context/browser-tests/screenshots/workspace-conversation-first.png'),fullPage:true})}
  await page.evaluate(()=>{window.filteredRepository='';window.pharosQueryTables={filterRepository:value=>{window.filteredRepository=value}}});
  await page.getByRole('button',{name:'pharos',exact:true}).click();
  assert.equal(await page.evaluate(()=>window.filteredRepository),'pharos');
  assert.equal(await page.locator('#library').getAttribute('class'),'view active');
});

test('workspace discovers GitHub pull-request links retained in conversation text', async t => {
  const page = await workFixture(t,{prs:[],canonical_remote:'git@github.com:acme/pharos.git',main_merge_commit:'abcdef1234567890',main_merge_title:'Merge feature into main',main_merge_method:'merge',main_merge_url:'https://github.com/acme/pharos/commit/abcdef1234567890',conversations:[{
    provider:'codex',native_id:'one',coverage:'complete',messages:[human('Ship the fix'),event('reply','message','Opened https://github.com/acme/pharos/pull/73 for review.',4)]
  }]});
  assert.equal(await page.getByRole('link',{name:'PR #73',exact:true}).getAttribute('href'),'https://github.com/acme/pharos/pull/73');
  assert.equal(await page.getByRole('link',{name:'Merged: Merge feature into main',exact:true}).getAttribute('href'),'https://github.com/acme/pharos/commit/abcdef1234567890');
});

test('workspace switches conversations, links files across conversations, and warns only for limited coverage', async t => {
  const page = await workFixture(t,{conversations:[
    {provider:'codex',native_id:'one',coverage:'codex-v1',messages:[human('First prompt')]},
    {provider:'claude',native_id:'two',coverage:'partial',messages:[human('Second prompt'),read()]}
  ]});
  assert.equal(await page.locator('.conversation-panel').count(),1);
  assert.match(await page.locator('.coverage-warning').innerText(),/claude · partial/);
  await page.locator('.file-index > summary').click();
  await page.locator('.file-index-link').click();
  assert.equal(await page.locator('.conversation-selector').inputValue(),'1');
  assert.match(await page.locator('.conversation-panel').innerText(),/Second prompt/);
  await page.locator('.conversation-selector').selectOption('0');
  assert.match(await page.locator('.conversation-panel').innerText(),/First prompt/);
  assert.equal(await page.locator('.conversation-toolbar input').inputValue(),'');
});

test('workspace conversation picker lives in the card, follows Library search, and survives navigation', async t => {
  const page = await workFixture(t,{conversations:[
    {id:'conversation-one',provider:'codex',native_id:'one',messages:[human('First prompt'),event('first-reply','message','No match here',1)]},
    {id:'conversation-two',provider:'claude',native_id:'two',messages:[human('Second prompt'),event('second-reply','message','The saffron answer',1)]}
  ]});
  await page.evaluate(async()=>{history.pushState(null,'','/library?search=saffron');await detail('work')});
  const picker=page.locator('.conversation-panel .conversation-selector');
  assert.equal(await picker.inputValue(),'1');
  assert.match(await page.locator('.conversation-choice label').innerText(),/Showing conversation #2 from this workspace/);
  if(process.env.PICKER_SCREENSHOT)await page.screenshot({path:path.join(root,'.context/conversation-picker.png')});
  assert.equal(new URL(page.url()).searchParams.get('conversation'),'conversation-two');
  await picker.selectOption('0');
  assert.equal(new URL(page.url()).searchParams.get('conversation'),'conversation-one');
  assert.match(await page.locator('.conversation-panel').innerText(),/First prompt/);
  await page.goBack();
  await page.locator('.conversation-selector').waitFor();
  assert.equal(await page.locator('.conversation-selector').inputValue(),'1');
  assert.match(await page.locator('.conversation-panel').innerText(),/The saffron answer/);
});

test('Ctrl+F searches across conversations and switches to the next matching card', async t => {
  const page = await workFixture(t,{conversations:[
    {id:'conversation-one',provider:'codex',native_id:'one',messages:[human('First prompt'),event('first-reply','message','No match here',1)]},
    {id:'conversation-two',provider:'claude',native_id:'two',messages:[human('Second prompt'),event('second-reply','message','The saffron answer',1)]}
  ]});
  await page.keyboard.press('Control+f');
  await page.getByRole('searchbox',{name:'Find in conversation'}).fill('saffron');
  assert.match(await page.locator('.conversation-find-workspace').innerText(),/1 of 2 conversations match/);
  await page.locator('.conversation-find-row button[title="Next match (Enter)"]').click();
  assert.equal(await page.locator('.conversation-selector').inputValue(),'1');
  assert.equal(new URL(page.url()).searchParams.get('conversation'),'conversation-two');
  assert.equal(await page.locator('.conversation-find-match').count(),1);
});

test('workspace without transcript retains archive context and unavailable file evidence', async t => {
  const page = await workFixture(t,{conversations:[],preservation_completeness:'incomplete'});
  assert.match(await transcript(page).innerText(),/No conversation was retained/);
  assert.match(await page.locator('.coverage-warning').innerText(),/Preservation · incomplete/);
  await page.locator('.file-index > summary').click();
  assert.equal(await page.locator('.file-index-link').isDisabled(),true);
  await page.locator('.archive-details > summary').click();
  assert.match(await transcript(page).innerText(),/Retained checkpoint/);
  assert.match(await transcript(page).innerText(),/Duplicate outcome/);
});

test('workspace failure navigation reveals failed action without claiming session failure', async t => {
  const page = await workFixture(t,{conversations:[{provider:'claude',messages:[human(),
    event('call','tool_call',{name:'Bash',input:{command:'git status'}},2,{call_id:'bad'}),
    event('result','tool_result',{is_error:true,content:'Permission denied'},3,{call_id:'bad'})]}]});
  await page.getByText('Close turns',{exact:true}).click();
  await page.getByText('Next failed action',{exact:true}).click();
  assert.equal(await page.locator('.turn').evaluate(el=>el.open),true);
  assert.equal(await page.locator('.tool-event.error').evaluate(el=>el.open),true);
  assert.match(await transcript(page).innerText(),/Permission denied/);
});

test('wrapped Claude Read leads with its action and file, never the raw envelope', async t => {
  const page = await fixture(t, [human(), read()]);
  const text = await transcript(page).innerText();
  assert.match(text, /Read/i);
  assert.match(text, /RULES\.md/);
  assert.doesNotMatch(text, /context_management|provider-message-id|"message"\s*:/);
  assert.equal(await page.locator('.tool-event').count(), 1);
  assert.equal(await page.locator('.tool-cluster').count(), 0, 'Targets must not require opening a generic cluster');
  assert.match(await page.locator('.tool-event > summary').innerText(), /RULES\.md/);
  assert.match(await page.locator('.timeline-time').allTextContents().then(x => x.join(' ')), /\+10s/);
  const alignment = await page.locator('.human-entry').evaluate(entry => {
    const time=entry.querySelector('.timeline-time').getBoundingClientRect(),mark=entry.querySelector('.message-kind').getBoundingClientRect();
    return Math.abs((time.top+time.height/2)-(mark.top+mark.height/2));
  });
  assert.ok(alignment <= 3, `Timestamp and speaker marker centers differ by ${alignment}px`);
  await page.locator('.tool-event > summary').click();
  assert.match(await transcript(page).innerText(), /RULES\.md/);
  assert.doesNotMatch(await transcript(page).innerText(), /context_management/);
});

test('mixed text and multiple tool blocks keep each action visible and pair wrapped results', async t => {
  const mixed = event('mixed', 'message', { message: { content: [
    { type: 'text', text: 'I will inspect both configuration files.' },
    { type: 'tool_use', id: 'r1', name: 'Read', input: { file_path: '/repo/a.conf' } },
    { type: 'tool_use', id: 'r2', name: 'Read', input: { file_path: '/repo/b.conf' } },
  ] } }, 2);
  const result = event('results', 'message', { message: { role: 'user', content: [
    { type: 'tool_result', tool_use_id: 'r1', content: 'alpha result text' },
    { type: 'tool_result', tool_use_id: 'r2', content: 'beta result text' },
  ] } }, 3, { role: 'user' });
  const page = await fixture(t, [human(), mixed, result]);
  assert.equal(await page.locator('.turn').count(), 1, 'Tool results from user envelopes are not human turns');
  assert.equal(await page.locator('.tool-event').count(), 1);
  const visible = await transcript(page).innerText();
  assert.match(visible, /I will inspect both configuration files/);
  assert.match(visible, /a\.conf/); assert.match(visible, /b\.conf/);
  assert.doesNotMatch(visible, /alpha result text|beta result text/);
  await page.getByRole('button', { name: 'Show output for /repo/a.conf', exact: true }).click();
  assert.match(await page.locator('.chip-preview').innerText(), /alpha result text/);
  assert.doesNotMatch(await page.locator('.chip-preview').innerText(), /beta result text/);
  assert.equal(await page.locator('.tool-event').evaluate(el => el.open), false);
});

test('JSON-string command arguments show the command and associate failures with it', async t => {
  const page = await fixture(t, [human(),
    event('cmd', 'tool_call', { name: 'exec_command', arguments: JSON.stringify({ cmd: 'npm test', workdir: '/repo' }) }, 10, { call_id: 'command-1' }),
    event('result', 'tool_result', { content: 'Tests failed: assertion mismatch', is_error: true }, 12, { call_id: 'command-1' }),
  ]);
  assert.equal(await page.locator('.tool-event').count(), 1);
  assert.match(await page.locator('.tool-event > summary').innerText(), /npm test/);
  assert.match(await page.locator('.tool-event').getAttribute('class'), /error/);
  await page.locator('.tool-event > summary').click();
  assert.match(await page.locator('.tool-event').innerText(), /assertion mismatch/);
});

test('file edit chips expose accurate deltas and open directly to the diff', async t => {
  const page = await fixture(t, [human(), event('edit', 'tool_call', {
    name: 'Edit', input: { file_path: '/repo/src/app.js', old_string: 'const old = 1;', new_string: 'const current = 2;\nrun(current);' },
  }, 14, { call_id: 'edit-1' })]);
  const summary = page.locator('.tool-event > summary');
  assert.match(await summary.innerText(), /app\.js/);
  assert.match(await summary.innerText(), /\+2/); assert.match(await summary.innerText(), /[−-]1/);
  await summary.click();
  assert.equal(await page.locator('.diff-view').isVisible(), true);
  assert.match(await page.locator('.diff-view').innerText(), /const current = 2/);
});

test('long prose expands without showing both a duplicate excerpt and full text; turns collapse', async t => {
  const sentence = 'A unique introduction followed by a detailed explanation. ';
  const page = await fixture(t, [human(), event('reply', 'message', sentence + 'Supporting details. '.repeat(100), 4)]);
  const reply = page.locator('[aria-label="Assistant message"]');
  assert.equal((await reply.innerText()).split('A unique introduction').length - 1, 1);
  const expand = reply.locator('summary, button').filter({ hasText: /more|expand/i }).first();
  await expand.click();
  assert.equal((await reply.innerText()).split('A unique introduction').length - 1, 1);
  assert.match(await reply.innerText(), /Supporting details\./);
  await page.locator('.turn > summary').click();
  assert.equal(await reply.isVisible(), false);
  assert.match(await page.locator('.turn > summary').innerText(), /Inspect the rules/);
});

test('harness XML blocks collapse into chips while surrounding text keeps its formatting', async t => {
  const ask = [
    '<environment_context>',
    '  <cwd>/workspace</cwd>',
    '  <shell>zsh</shell>',
    '</environment_context>',
    '<system_instruction>',
    'Save screenshots and embed them, for example `![Screenshot](</Users/me/shot.png>)`.',
    '</system_instruction>',
    'Please **tidy** the renderer.',
  ].join('\n');
  const reply = [
    '<system-reminder note="x">Keep going.</system-reminder>',
    'Done — see **the diff**.',
    '',
    '```xml',
    '<example>',
    '</example>',
    '```',
    '',
    '<div>unbalanced <b>markup</div>',
    'Use Array<string> here.',
  ].join('\n');
  const page = await fixture(t, [human(ask), event('reply', 'message', reply, 4)]);
  const user = page.locator('[aria-label="User message"]'), agent = page.locator('[aria-label="Assistant message"]');
  assert.deepEqual(await user.locator('.xml-chip > summary').allInnerTexts(), ['environment_context', 'system_instruction']);
  assert.equal(await user.locator('.xml-chip-body').first().isVisible(), false);
  assert.doesNotMatch(await user.innerText(), /<cwd>|Screenshot/);
  assert.match(await user.innerText(), /Please \*\*tidy\*\* the renderer\./);
  const envChip = user.locator('.xml-chip').first();
  await envChip.locator('> summary').click();
  assert.equal(await envChip.locator('> summary').isVisible(), false, 'the chip becomes the expanded view');
  assert.match(await envChip.locator('.xml-chip-body').innerText(), /<cwd>\/workspace<\/cwd>/);
  await envChip.locator('.xml-node > summary').first().click();
  await envChip.locator('> summary').waitFor({ state: 'visible' });
  assert.equal(await envChip.evaluate(element => element.open), false, 'folding the root restores the chip');
  await envChip.locator('> summary').click();
  assert.equal(await envChip.locator('.xml-node').first().evaluate(element => element.open), true, 'reopening shows the full tree again');
  await user.locator('.xml-chip > summary').nth(1).click();
  assert.match(await user.locator('.xml-chip-body').nth(1).innerText(), /<\/Users\/me\/shot\.png>/);
  assert.deepEqual(await agent.locator('.xml-chip > summary').allInnerTexts(), ['system-reminder']);
  assert.equal(await agent.locator('strong').innerText(), 'the diff');
  assert.match(await agent.locator('pre code').innerText(), /<example>/);
  assert.match(await agent.innerText(), /<div>unbalanced <b>markup<\/div>/);
  assert.match(await agent.innerText(), /Array<string>/);
  assert.match(await page.locator('.turn-summary-prompt').innerText(), /^Please \*\*tidy\*\* the renderer\.$/);
  assert.match(await page.locator('.turn-summary-outcome').innerText(), /^Done/);
});

test('messages that are only XML summarize by tag and reveal search matches inside chips', async t => {
  const page = await fixture(t, [human('<command-name>/review</command-name>\n<command-args>deep</command-args>')]);
  assert.equal(await page.locator('.turn-summary-prompt').innerText(), '<command-name> <command-args>');
  await page.evaluate(() => document.querySelector('.conversation-panel').openConversationSearch('deep'));
  const chip = page.locator('.xml-chip').nth(1);
  assert.equal(await chip.evaluate(element => element.open), true);
  assert.equal(await chip.locator('mark').innerText(), 'deep');
});

test('expanded XML chips fold nested elements independently', async t => {
  const page = await fixture(t, [human('<context>\n  <files>\n    <file path="a.go">one</file>\n    <file path="b.go">two\nlines</file>\n  </files>\n  <note>kept</note>\n</context>\nGo.')]);
  const chip = page.locator('.xml-chip');
  await chip.locator('> summary').click();
  const files = chip.locator('.xml-node').nth(1);
  assert.equal(await files.locator('> summary').innerText(), '<files>');
  await files.locator('> summary').click();
  assert.equal(await files.evaluate(element => element.open), false);
  assert.equal(await chip.evaluate(element => element.open), true, 'folding a child keeps the parent expanded');
  assert.equal(await files.locator('> summary').evaluate(element => getComputedStyle(element, '::after').content), '"…</files>"');
  assert.match(await chip.innerText(), /<note>kept<\/note>/);
  assert.doesNotMatch(await chip.innerText(), /a\.go/);
});

test('collapsed turns independently expand two-line ask and response summaries and report work counts', async t => {
  const ask='Review the experiment interface and explain every relevant alignment and disclosure choice. '.repeat(6);
  const response='The interface review is complete, with the requested hierarchy and spacing changes documented. '.repeat(6);
  const page=await fixture(t,[human(ask),
    event('command','tool_call',{name:'Bash',input:{command:'git diff'}},1,{call_id:'command'}),
    event('read','tool_call',{name:'Read',input:{file_path:'/repo/ui.py'}},2,{call_id:'read'}),
    event('delegate','delegation',{name:'Task',input:{description:'Check the rendered layout'}},3,{call_id:'delegate'}),
    event('reply','message',response,4)
  ],{width:1054,height:857});
  await page.getByText('Close turns',{exact:true}).click();
  const turn=page.locator('.turn'),rows=turn.locator('.turn-summary-row');
  if(process.env.TRANSCRIPT_SCREENSHOTS)await page.screenshot({path:path.join(root,'.context/browser-tests/screenshots/collapsed-turn-summaries.png'),fullPage:true});
  assert.equal(await rows.count(),2);
  assert.match(await turn.locator('.turn-summary-stats').innerText(),/1 reply.*3 activities.*2 tool calls.*1 sub-agent/s);
  for(const excerpt of [turn.locator('.turn-summary-prompt'),turn.locator('.turn-summary-outcome')]){
    const box=await excerpt.boundingBox(),lineHeight=await excerpt.evaluate(el=>parseFloat(getComputedStyle(el).lineHeight));
    assert.ok(box.height<=lineHeight*2+1,`Collapsed summary exceeds two lines: ${box.height}px`);
  }
  await turn.getByRole('button',{name:'Expand turn ask'}).click();
  assert.equal(await turn.evaluate(el=>el.open),false,'Expanding the ask must not open the turn');
  assert.equal(await turn.locator('.turn-summary-prompt').getAttribute('class'),'turn-summary-prompt summary-expanded');
  assert.doesNotMatch(await turn.locator('.turn-summary-outcome').getAttribute('class'),/summary-expanded/);
  await turn.getByRole('button',{name:'Expand turn response'}).click();
  assert.equal(await turn.evaluate(el=>el.open),false,'Expanding the response must not open the turn');
  await page.getByText('Open turns',{exact:true}).click();
  assert.equal(await turn.evaluate(el=>el.open),true);
  await page.waitForTimeout(250);
  if(process.env.TRANSCRIPT_SCREENSHOTS)await page.screenshot({path:path.join(root,'.context/browser-tests/screenshots/open-turn-summary.png'),fullPage:true});
  for(const row of await rows.all()){
    assert.ok(await row.isVisible(),'Opening a turn must keep its summary in place');
    assert.ok(Number(await row.evaluate(el=>getComputedStyle(el).opacity))<1,'An open turn summary should recede behind the transcript');
  }
});

test('message marker and operation content are vertically centered', async t => {
  const page=await fixture(t,[human(),event('command','tool_call',{name:'Bash',input:{command:'git diff'}},1),event('reply','message','First response line.\nSecond response line.',2)]);
  const markerOffset=await page.locator('.agent-entry').evaluate(entry=>{
    const marker=entry.querySelector('.message-kind').getBoundingClientRect(),prose=entry.querySelector('.message-prose'),text=prose.getBoundingClientRect(),lineHeight=parseFloat(getComputedStyle(prose).lineHeight);
    return Math.abs((marker.top+marker.height/2)-(text.top+lineHeight/2));
  });
  assert.ok(markerOffset<=1,`Message marker center differs from the first text line by ${markerOffset}px`);
  const operationOffset=await page.locator('.tool-event > summary').evaluate(summary=>{
    const row=summary.getBoundingClientRect(),icon=summary.querySelector('.kind-icon').getBoundingClientRect();
    return Math.abs((row.top+row.height/2)-(icon.top+icon.height/2));
  });
  assert.ok(operationOffset<=1,`Operation content center differs by ${operationOffset}px`);
});

test('delegation discloses child events instead of silently consuming them', async t => {
  const page = await fixture(t, [human(),
    event('delegate', 'delegation', { name: 'Task', input: { description: 'Audit configuration' } }, 5, { call_id: 'agent-1' }),
    event('child', 'tool_call', { name: 'Read', input: { file_path: '/repo/nested.toml' } }, 6, { call_id: 'nested-read', parent_native_id: 'agent-1' }),
    event('child-result', 'tool_result', { content: 'nested configuration contents' }, 7, { call_id: 'nested-read', parent_native_id: 'agent-1' }),
    event('delegate-result', 'delegation_result', { content: 'Audit complete' }, 8, { call_id: 'agent-1' }),
  ]);
  const run = page.locator('.subagent-run');
  assert.equal(await run.count(), 1);
  assert.match(await run.locator(':scope > summary').innerText(), /Audit configuration/);
  await run.locator(':scope > summary').click();
  assert.match(await run.innerText(), /nested\.toml/);
  const child = run.locator('.tool-event').filter({ hasText: 'nested.toml' });
  await child.locator(':scope > summary').click();
  assert.match(await child.innerText(), /nested configuration contents/);
});

test('nested delegations read like turns and appear as layers in the token rail', async t => {
  const usage = total => ({ type:'token_count', info:{ total_token_usage:{ total_tokens:total },last_token_usage:{input_tokens:total,output_tokens:0} } });
  const page=await fixture(t,[human(),
    event('outer','delegation',{name:'Task',input:{description:'Audit the interface',subagent_type:'Reviewer'}},1,{call_id:'outer-call'}),
    event('outer-usage','metadata',usage(12000),2,{parent_native_id:'outer-call'}),
    event('inner','delegation',{name:'Task',input:{description:'Check the token rail',subagent_type:'Analyst'}},3,{call_id:'inner-call',parent_native_id:'outer-call'}),
    event('inner-usage','metadata',usage(5000),4,{parent_native_id:'inner-call'}),
    event('inner-reply','message','Rail checked',5,{parent_native_id:'inner-call'}),
    event('inner-result','delegation_result',{content:'Rail complete'},6,{call_id:'inner-call'}),
    event('outer-result','delegation_result',{content:'Audit complete'},7,{call_id:'outer-call'}),
  ]);
  const runs=page.locator('.subagent-run');
  assert.equal(await runs.count(),2);
  assert.match(await runs.first().locator(':scope > summary').innerText(),/Reviewer.*Audit the interface.*Audit complete/s);
  assert.match(await runs.last().locator(':scope > summary').textContent(),/Analyst.*Check the token rail.*Rail checked/s);
  const markers=page.locator('.token-map .event-marker');
  assert.equal(await markers.count(),2);
  assert.match(await markers.nth(0).getAttribute('aria-label'),/Subagent outer-call · 12,000 input tokens/);
  assert.match(await markers.nth(1).getAttribute('aria-label'),/Subagent inner-call · 5,000 input tokens/);
  assert.notEqual(await markers.nth(0).evaluate(el=>el.style.getPropertyValue('--token-color')),await markers.nth(1).evaluate(el=>el.style.getPropertyValue('--token-color')));
  await markers.nth(1).click();
  assert.equal(await runs.last().evaluate(el=>el.open),true);
});

test('narrow screens retain readable action targets without horizontal overflow', async t => {
  const page = await fixture(t, [human(), read('read', '/Users/someone/projects/a-long-project-name/deeply/nested/folder/docs/RULES.md')], { width: 390, height: 844 });
  assert.match(await transcript(page).innerText(), /RULES\.md/);
  assert.equal(await page.evaluate(() => document.documentElement.scrollWidth <= innerWidth + 1), true);
  assert.equal(await page.locator('.file-target').evaluate(target => {
    const walker = document.createTreeWalker(target, NodeFilter.SHOW_TEXT);
    let text;
    while ((text = walker.nextNode())) {
      const index = text.textContent.indexOf('RULES.md');
      if (index < 0) continue;
      const range = document.createRange(); range.setStart(text, index); range.setEnd(text, index + 8);
      return [...range.getClientRects()].every(rect => rect.left >= 0 && rect.right <= innerWidth);
    }
    return false;
  }), true, 'The filename itself must be on screen, not merely present in clipped DOM text');
});

test('conversation map tracks the viewport and jumps to a human turn', async t => {
  const messages = Array.from({ length: 40 }, (_, index) => [
    event(`human-${index}`, 'message', `Review step ${index + 1}`, index * 20, { role: 'user' }),
    event(`reply-${index}`, 'message', `Completed review step ${index + 1}.`, index * 20 + 5),
  ]).flat();
  const page = await fixture(t, messages);
  await page.evaluate(() => { document.querySelector('#detail').classList.add('active'); window.scrollTo(0, 500); });
  await page.waitForFunction(() => document.querySelector('.conversation-map')?.classList.contains('visible'));
  assert.equal(await page.locator('.conversation-map-label').innerText(),'Turns');
  const mapBox = await page.locator('.conversation-map').boundingBox();
  assert.ok(mapBox.y <= 75 && mapBox.height >= 770, `Conversation map should span the viewport above its label: ${JSON.stringify(mapBox)}`);
  assert.equal(await page.locator('.conversation-map-turn.human').count(), 40);
  const before = await page.locator('.conversation-map-viewport').getAttribute('style');
  await page.evaluate(() => window.scrollTo(0, 2000));
  await page.waitForFunction(previous => document.querySelector('.conversation-map-viewport').getAttribute('style') !== previous, before);
  await page.locator('.conversation-map-turn').nth(15).click({ force: true });
  await page.waitForFunction(() => Math.abs(document.querySelectorAll('.turn')[15].getBoundingClientRect().top) < 3);
});

test('all results remain paired to one call and keyboard disclosure exposes them', async t => {
  const page = await fixture(t, [human(),
    event('call', 'tool_call', { name: 'Bash', input: { command: 'make test' } }, 2, { call_id: 'c' }),
    event('part-one', 'tool_result', { content: 'First output chunk' }, 3, { call_id: 'c' }),
    event('part-two', 'tool_result', { content: 'Second output chunk' }, 4, { call_id: 'c' }),
  ]);
  assert.equal(await page.locator('.tool-event').count(), 1);
  const summary = page.locator('.tool-event > summary'); await summary.focus(); await page.keyboard.press('Enter');
  const text = await page.locator('.tool-event').innerText();
  assert.match(text, /First output chunk/); assert.match(text, /Second output chunk/);
  assert.doesNotMatch(text, /"content"\s*:/);
});

test('orphaned child events and unknown envelopes remain accessible', async t => {
  const page = await fixture(t, [human(),
    event('orphan', 'tool_call', { name: 'Read', input: { file_path: '/repo/orphan.txt' } }, 4, { parent_native_id: 'missing-parent' }),
    event('unknown', 'metadata', { type: 'future_provider_event', status: 'paused', private_future_field: 'forensic-value' }, 5),
  ]);
  assert.match(await transcript(page).innerText(), /orphan\.txt/);
  assert.match(await transcript(page).innerText(), /future_provider_event|Future Provider Event/i);
  assert.doesNotMatch(await transcript(page).innerText(), /forensic-value/);
  const unknown = page.locator('.tool-event').filter({ hasText: 'forensic-value' });
  await unknown.locator(':scope > summary').click();
  await unknown.locator('.raw-event > summary').click();
  assert.match(await unknown.innerText(), /forensic-value/);
});

test('Write without prior contents does not invent line deltas', async t => {
  const page = await fixture(t, [human(), event('write', 'tool_call', {
    name: 'Write', input: { file_path: '/repo/overwritten.txt', content: 'Replacement line one\nReplacement line two' },
  }, 4)]);
  const summary = page.locator('.tool-event > summary');
  assert.match(await summary.innerText(), /overwritten\.txt/);
  assert.doesNotMatch(await summary.innerText(), /\+2|[−-]0/);
  await summary.click();
  assert.match(await page.locator('.tool-event').innerText(), /previous contents not retained|unknown|unavailable/i);
  assert.match(await page.locator('.tool-event').innerText(), /Replacement line one/);
});

test('normalized user-role text-block results remain paired and retain error status', async t => {
  const page = await fixture(t, [human(),
    event('call', 'tool_call', { tool: 'Read', input: { file_path: '/repo/missing.txt' } }, 1, { call_id: 'missing' }),
    event('result', 'tool_result', { tool: 'Read', content: [{ type: 'text', text: 'ENOENT missing file' }], is_error: true }, 2, { call_id: 'missing', role: 'user' }),
  ]);
  assert.equal(await page.locator('.turn').count(), 1);
  assert.equal(await page.locator('.tool-event').count(), 1);
  assert.match(await page.locator('.tool-event > summary').innerText(), /Failed/);
  await page.locator('.tool-event > summary').click();
  assert.match(await page.locator('.tool-event').innerText(), /ENOENT missing file/);
});

test('null blocks are tolerated and file text mentioning exit code does not signal failure', async t => {
  const page = await fixture(t, [human(),
    event('wrapped', 'message', { message: { content: [null, { type: 'text', text: 'Read the troubleshooting guide.' }] } }, 1),
    event('call', 'tool_call', { name: 'Read', input: { file_path: '/repo/guide.md' } }, 2, { call_id: 'guide' }),
    event('result', 'tool_result', { content: 'The guide says exit code 1 means an error.' }, 3, { call_id: 'guide' }),
  ]);
  assert.match(await transcript(page).innerText(), /Read the troubleshooting guide/);
  assert.doesNotMatch(await page.locator('.tool-event > summary').innerText(), /Failed/);
  assert.doesNotMatch(await page.locator('.tool-event').getAttribute('class'), /error/);
});

test('trailing aborted-by-user noise after a successful result is omitted', async t => {
  const page = await fixture(t, [human(),
    event('reply', 'message', 'The requested work is complete.', 5),
    event('success', 'result', { type: 'result', subtype: 'success' }, 10),
    event('late-abort', 'error', { type: 'error', content: 'aborted by user' }, 910),
  ]);
  const text = await transcript(page).innerText();
  assert.match(text, /Result · success/i);
  assert.doesNotMatch(text, /aborted by user|Run error/i);
  assert.equal(await page.locator('.tool-event.error').count(), 0);
});

test('other terminal abort errors remain visible', async t => {
  const page = await fixture(t, [human(),
    event('reply', 'message', 'Waiting for the operation.', 5),
    event('late-abort', 'error', { type: 'error', content: 'aborted by user' }, 910),
  ]);
  assert.match(await transcript(page).innerText(), /aborted by user/i);
  assert.equal(await page.locator('.tool-event.error').count(), 1);
});

test('failed Edit labels the requested diff without claiming it was applied', async t => {
  const page = await fixture(t, [human(),
    event('edit', 'tool_call', { name: 'Edit', input: { file_path: '/repo/app.js', old_string: 'old()', new_string: 'new()' } }, 2, { call_id: 'failed-edit' }),
    event('error', 'tool_result', { content: 'Could not locate old text', is_error: true }, 3, { call_id: 'failed-edit' }),
  ]);
  assert.match(await page.locator('.tool-event > summary').innerText(), /Failed/);
  assert.doesNotMatch(await page.locator('.tool-event > summary').innerText(), /Edited|Applied/);
  await page.locator('.tool-event > summary').click();
  assert.match(await page.locator('.tool-event').innerText(), /Requested change|Proposed change/i);
  assert.match(await page.locator('.tool-event').innerText(), /Could not locate old text/);
});

test('replace_all and deletion without retained lines use unknown delta counts', async t => {
  const page = await fixture(t, [human(),
    event('replace', 'tool_call', { name: 'Edit', input: { file_path: '/repo/all.txt', old_string: 'old', new_string: 'new', replace_all: true } }, 1),
    event('delete', 'tool_call', { name: 'apply_patch', input: '*** Begin Patch\n*** Delete File: /repo/deleted.txt\n*** End Patch' }, 2),
  ]);
  const summaries = await page.locator('.tool-event > summary').allTextContents();
  assert.equal(summaries.length, 1);
  for (const summary of summaries) { assert.match(summary, /unknown/i); assert.doesNotMatch(summary, /\+1|[−-]1|\+0|[−-]0/); }
});

test('search reveals matching output directly and does not open raw JSON', async t => {
  const page = await fixture(t, [human(),
    event('call', 'tool_call', { name: 'Read', input: { file_path: '/repo/config.json' } }, 1, { call_id: 'search-output' }),
    event('result', 'tool_result', { content: 'unique_output_needle is configured' }, 2, { call_id: 'search-output' }),
    event('second-human', 'message', 'An unrelated second task', 20, { role: 'user' }),
  ]);
  assert.doesNotMatch(await transcript(page).innerText(), /unique_output_needle/);
  await page.evaluate(()=>document.querySelector('#detail').classList.add('active'));
  await page.keyboard.press('Control+f');
  await page.getByRole('combobox',{name:'Search depth'}).selectOption('responses');
  await page.getByRole('searchbox',{name:'Find in conversation'}).fill('unique_output_needle');
  assert.match(await transcript(page).innerText(), /unique_output_needle is configured/);
  assert.equal(await page.locator('.raw-event[open]').count(), 0);
  assert.equal(await page.locator('.conversation-find-match').count(), 1);
});

test('file paths, copy and diff previews operate independently of full details', async t => {
  const page = await fixture(t, [human(), event('patch', 'tool_call', {name:'apply_patch', input:'*** Begin Patch\n*** Update File: /workspace/src/a.js\n-oldA\n+newA\n*** Update File: /workspace/src/b.js\n-oldB\n+newB\n*** End Patch'}, 2)]);
  const row=page.locator('.tool-event'), prefix=page.locator('.file-prefix').first();
  assert.equal(await prefix.innerText(),'…/');
  await prefix.click();
  assert.equal(await prefix.innerText(),'src/');
  assert.equal(await row.evaluate(el=>el.open),false);
  await page.evaluate(()=>{window.copiedPath='';Object.defineProperty(navigator,'clipboard',{value:{writeText:async value=>{window.copiedPath=value}},configurable:true})});
  await page.getByRole('button',{name:'Copy path /workspace/src/a.js',exact:true}).click();
  assert.equal(await page.evaluate(()=>window.copiedPath),'/workspace/src/a.js');
  await page.getByRole('button',{name:'Show diff for /workspace/src/b.js',exact:true}).click();
  const preview=page.locator('.chip-preview');
  assert.match(await preview.innerText(),/newB/);
  assert.doesNotMatch(await preview.innerText(),/newA|Raw event/);
  assert.equal(await row.evaluate(el=>el.open),false);
  await preview.getByRole('button',{name:'Close',exact:true}).click();
  assert.equal(await preview.isVisible(),false);
});

test('saved feedback and an unfinished annotation survive a page refresh', async t => {
  const page = await fixture(t, []);
  await page.evaluate(() => {
    const target = { id:'draft-target', note:'', view:{id:'library',title:'Library'}, label:'Search', tag:'input',
      selector:'#search', bounds:{x:1,y:2,width:100,height:30}, viewport:{width:1280,height:900,device_pixel_ratio:1},
      source:'src/pharos/ui.py (APP_HTML)', styles:{display:'block'} };
    localStorage.setItem('pharos-ui-feedback-v1', JSON.stringify([{...target,id:'saved',note:'Persist this annotation'}]));
    localStorage.setItem('pharos-ui-feedback-draft-v1', JSON.stringify({target,note:'Unfinished but important'}));
  });
  await page.reload();
  const result = await page.evaluate(() => {
    const state = { annotations:feedbackAnnotations.map(item=>item.note), draft:document.querySelector('#feedbackNote').value,
      composerOpen:document.querySelector('#feedbackComposer').classList.contains('open'), count:document.querySelector('#feedbackCount').textContent };
    localStorage.removeItem('pharos-ui-feedback-v1');
    localStorage.removeItem('pharos-ui-feedback-draft-v1');
    return state;
  });
  assert.deepEqual(result.annotations, ['Persist this annotation']);
  assert.equal(result.draft, 'Unfinished but important');
  assert.equal(result.composerOpen, true);
  assert.equal(result.count, '1');
});

test('feedback copy uses the macOS clipboard bridge and only clears after success', async t => {
  const page = await fixture(t, []);
  const result = await page.evaluate(async () => {
    window.nativeClipboardText = '';
    delete window.webkit;
    Object.defineProperty(window, 'webkit', { value: { messageHandlers: { pharosClipboard: {
      postMessage: async value => { window.nativeClipboardText = value; return true; },
    } } }, configurable: true });
    feedbackAnnotations = [{ note:'Make this clearer', view:{id:'library',title:'Library'}, label:'Search', tag:'input',
      selector:'#search', bounds:{x:1,y:2,width:100,height:30}, viewport:{width:1280,height:900,device_pixel_ratio:1},
      source:'src/pharos/ui.py (APP_HTML)', styles:{display:'block'} }];
    document.querySelector('#feedbackClearOnCopy').checked = true;
    await copyFeedback();
    return { text: window.nativeClipboardText, remaining: feedbackAnnotations.length, toast: document.querySelector('#feedbackToast').textContent };
  });
  assert.match(result.text, /# Pharos UI feedback/);
  assert.match(result.text, /Make this clearer/);
  assert.equal(result.remaining, 0);
  assert.match(result.toast, /Copied 1 annotation and cleared/);
});

test('feedback copy reports clipboard failure and preserves annotations', async t => {
  const page = await fixture(t, []);
  const result = await page.evaluate(async () => {
    Object.defineProperty(window, 'webkit', { value: undefined, configurable: true });
    Object.defineProperty(navigator, 'clipboard', { value: { writeText: async () => { throw new Error('denied'); } }, configurable: true });
    document.execCommand = () => false;
    feedbackAnnotations = [{ note:'Do not lose me', view:{id:'library',title:'Library'}, label:'Search', tag:'input',
      selector:'#search', bounds:{x:1,y:2,width:100,height:30}, viewport:{width:1280,height:900,device_pixel_ratio:1},
      source:'src/pharos/ui.py (APP_HTML)', styles:{display:'block'} }];
    document.querySelector('#feedbackClearOnCopy').checked = true;
    await copyFeedback();
    return { remaining: feedbackAnnotations.length, toast: document.querySelector('#feedbackToast').textContent,
      disabled: document.querySelector('#feedbackCopy').disabled };
  });
  assert.equal(result.remaining, 1);
  assert.match(result.toast, /Could not copy feedback/);
  assert.equal(result.disabled, false);
});

test('search groups preserve chronological boundaries and expose individual outputs', async t => {
  const page=await fixture(t,[human(),
    event('s1','tool_call',{name:'Grep',input:{pattern:'firstTerm',path:'/workspace/src'}},1,{call_id:'s1'}),
    event('r1','tool_result',{content:'first matches'},2,{call_id:'s1'}),
    event('s2','tool_call',{name:'Grep',input:{pattern:'secondTerm'}},3,{call_id:'s2'}),
    event('r2','tool_result',{content:'second matches'},4,{call_id:'s2'}),
    event('explain','message','Now I will edit the result.',5),
    read('after','/workspace/src/after.js',6)
  ]);
  assert.equal(await page.locator('.tool-event').count(),2);
  assert.equal(await page.locator('.query-chip').count(),2);
  await page.getByRole('button',{name:'secondTerm',exact:true}).click();
  assert.match(await page.locator('.chip-preview:visible').innerText(),/second matches/);
  assert.doesNotMatch(await page.locator('.chip-preview:visible').innerText(),/first matches/);
});

test('todo management is distinct from file writes and exposes statuses', async t => {
  const page=await fixture(t,[human(),
    event('todos','tool_call',{name:'TodoWrite',input:{todos:[{content:'Inspect parser',status:'completed'},{content:'Add tests',status:'in_progress'}]}},1),
    event('create','tool_call',{name:'TaskCreate',input:{subject:'Review changes'}},2),
    event('delete','tool_call',{name:'TaskUpdate',input:{taskId:'42',status:'deleted'}},3)
  ]);
  assert.equal(await page.locator('.file-chip').count(),0);
  const text=await transcript(page).innerText();
  assert.match(text,/Update todos/);assert.match(text,/Create todo/);assert.match(text,/Delete todo/);
  await page.getByRole('button',{name:'Set 2 todos',exact:true}).click();
  assert.match(await page.locator('.chip-preview:visible').innerText(),/completed/);
  assert.match(await page.locator('.chip-preview:visible').innerText(),/in progress/);
});

test('agent raw access is inline and message prose has stronger hierarchy', async t => {
  const page=await fixture(t,[human(),event('wrapped','message',{type:'agent_message',text:'A readable assistant response.'},1)]);
  const message=page.getByRole('article',{name:'Assistant message'});
  const raw=message.getByRole('button',{name:'Raw event',exact:true});
  assert.equal(await raw.evaluate(el=>getComputedStyle(el).position),'absolute');
  assert.equal(await message.locator('.raw-event').isVisible(),false);
  await raw.click();
  assert.match(await message.locator('.raw-json').innerText(),/agent_message/);
  await raw.click();
  assert.equal(await message.locator('.raw-event').isVisible(),false);
});

test('agent Markdown renders structure without activating embedded HTML or unsafe links', async t => {
  const markdown = '# Findings\n\n**Passed** with *one caveat* and \x60inlineCode\x60.\n\n- First item\n- Second item\n  - Nested item\n\n> A quoted observation\n\n| Name | Result |\n| --- | --- |\n| parser | pass |\n\n\x60\x60\x60python\nprint("ok")\n\x60\x60\x60\n\n[Docs](https://example.com/docs)\n\n<script>window.markdownExecuted=true</script>\n\n[bad](javascript:alert(1))';
  const page=await fixture(t,[human(),event('reply','message',markdown,1)]);
  const prose=page.locator('.rendered-markdown');
  assert.equal(await prose.locator('h1').innerText(),'Findings');
  assert.equal(await prose.locator('strong').innerText(),'Passed');
  assert.equal(await prose.locator('ul li').count(),3);
  assert.equal(await prose.locator('table tbody td').count(),2);
  assert.match(await prose.locator('pre code').innerText(),/print/);
  assert.equal(await prose.locator('a').count(),1);
  assert.equal(await prose.locator('script').count(),0);
  assert.equal(await page.evaluate(()=>window.markdownExecuted),undefined);
});

test('bash icons distinguish git python typechecking and tests', async t => {
  const commands=['git diff --stat','python3 tools/check.py','npm run typecheck','npm test'];
  const page=await fixture(t,[human(),...commands.map((command,index)=>event('cmd'+index,'tool_call',{name:'Bash',input:{command}},index+1))]);
  assert.deepEqual(await page.locator('.tool-event>summary>.kind-icon').evaluateAll(icons=>icons.map(icon=>icon.dataset.kind)),['git','python','typecheck','test']);
  const heights=await page.locator('.tool-event>summary').evaluateAll(rows=>rows.map(row=>row.getBoundingClientRect().height));
  assert.ok(heights.every(height=>height<=26),'Operation rows remain compact');
});

test('todo summaries describe only changes and preserve complete lists on expansion', async t => {
  const todos=(status)=>[{content:'Inspect parser',status},{content:'Write tests',status:'pending'}];
  const page=await fixture(t,[human(),
    event('initial','tool_call',{name:'TodoWrite',input:{todos:todos('pending')}},1),
    event('update','tool_call',{name:'TodoWrite',input:{todos:todos('completed')}},2),
    event('same','tool_call',{name:'TodoWrite',input:{todos:todos('completed')}},3)
  ]);
  const buttons=page.locator('.query-chip');
  assert.equal(await buttons.nth(0).innerText(),'Set 2 todos');
  assert.equal(await buttons.nth(1).innerText(),'Completed: Inspect parser');
  assert.equal(await buttons.nth(2).innerText(),'No todo changes');
  assert.doesNotMatch(await buttons.nth(1).innerText(),/Write tests/);
  await buttons.nth(1).click();
  assert.match(await page.locator('.chip-preview:visible').innerText(),/Write tests/);
});

test('repository worktree and recorded cwd paths collapse while external paths stay explicit', async t => {
  const page=await fixture(t,[human(),
    event('cwd','metadata',{type:'session_meta',cwd:'/checkout/task'},1),
    read('worktree','/checkout/task/src/main.py',2),
    read('repo','/source/project/docs/guide.md',3),
    read('outside','/source/project-other/secret.txt',4)
  ],{width:1280,height:900},{repo_root:null,workspace_roots:['/source/project']});
  const prefixes=page.locator('.file-prefix');
  assert.deepEqual(await prefixes.allTextContents(),['…/','…/','/source/project-other/']);
  assert.deepEqual(await prefixes.evaluateAll(nodes=>nodes.map(node=>node.getAttribute('aria-expanded'))),['false','false','true']);
  await prefixes.nth(0).click();
  assert.equal(await prefixes.nth(0).innerText(),'src/');
  assert.equal(await page.locator('.tool-event').last().evaluate(row=>row.open),false);
});

test('conversation find highlights messages and expands scope through thinking, calls, and responses', async t => {
  const page=await fixture(t,[
    human('Alpha alpha'),
    event('reply','message','alpha',1),
    event('thought','reasoning','alpha',2),
    event('call','tool_call',{name:'Bash',input:{command:'alpha'}},3,{call_id:'search-call'}),
    event('result','tool_result',{content:'alpha'},4,{call_id:'search-call'}),
  ]);
  await page.evaluate(()=>document.querySelector('#detail').classList.add('active'));
  await page.keyboard.press('Control+f');
  const search=page.getByRole('searchbox',{name:'Find in conversation'});
  await search.fill('al');
  assert.equal(await page.locator('.conversation-find-match').count(),0);
  assert.equal(await page.locator('.conversation-find-count').innerText(),'Type 3+');
  await page.getByRole('combobox',{name:'Search depth'}).selectOption('thinking');
  assert.equal(await page.locator('.conversation-find-match').count(),0);
  await page.getByRole('combobox',{name:'Search depth'}).selectOption('messages');
  await page.keyboard.press('Enter');
  assert.equal(await page.locator('.conversation-find-match').count(),3);
  await search.fill('a');
  assert.equal(await page.locator('.conversation-find-match').count(),0);
  await search.fill('alpha');
  assert.equal(await page.locator('.conversation-find-match').count(),3);
  assert.equal(await page.locator('.conversation-find-count').innerText(),'3 matches');
  await page.getByRole('combobox',{name:'Search depth'}).selectOption('thinking');
  assert.ok(await page.locator('.conversation-find-match').count()>=4);
  await page.getByRole('combobox',{name:'Search depth'}).selectOption('tools');
  const calls=await page.locator('.conversation-find-match').count();
  assert.ok(calls>=5);
  await page.getByRole('combobox',{name:'Search depth'}).selectOption('responses');
  assert.ok(await page.locator('.conversation-find-match').count()>calls);
  await page.locator('.conversation-find-options label').last().locator('input').check();
  await search.fill('Alpha');
  assert.equal(await page.locator('.conversation-find-match').count(),1);
  await page.locator('.conversation-find-options label').last().locator('input').uncheck();
  await page.locator('.conversation-find-options label').first().locator('input').check();
  await search.fill('alpha|Alpha');
  assert.ok(await page.locator('.conversation-find-match').count()>1);
  await page.keyboard.press('Enter');
  assert.match(await page.locator('.conversation-find-count').innerText(),/^1 \/ /);
  assert.equal(await page.locator('.conversation-find-marker').count(),await page.locator('.conversation-find-match').count());
  await page.locator('.conversation-find-track').click({position:{x:5,y:500}});
  assert.notEqual(await page.locator('.conversation-find-count').innerText(),`1 / ${await page.locator('.conversation-find-match').count()}`);
  await search.fill('(');
  assert.equal(await page.locator('.conversation-find-error').isVisible(),false);
  await page.keyboard.press('Enter');
  assert.match(await page.locator('.conversation-find-error').innerText(),/Invalid regular expression/);
  assert.equal(await page.locator('.conversation-find-match').count(),0);
  await page.keyboard.press('Escape');
  assert.equal(await page.locator('.conversation-find-match').count(),0);
});


test('turn, token, and search rails share conversation positions and one viewport window', async t => {
  const messages=[];
  for(let index=0;index<30;index++){
    messages.push(event(`turn-${index}`,'message',`Turn ${index} ${index===14?'needle':''} ${'extended transcript text '.repeat(12)}`,index,{role:'user'}));
    if(index===14)messages.push(event('usage','metadata',{type:'token_count',info:{total_token_usage:{total_tokens:12000},last_token_usage:{input_tokens:12000,output_tokens:0}}},index));
  }
  const page=await fixture(t,messages);
  await page.evaluate(()=>document.querySelector('#detail').classList.add('active'));
  await page.evaluate(()=>scrollTo(0,500));
  await page.waitForFunction(()=>document.querySelector('.conversation-map')?.classList.contains('visible'));
  await page.keyboard.press('Control+f');
  await page.getByRole('searchbox',{name:'Find in conversation'}).fill('needle');
  await page.waitForFunction(()=>document.querySelector('.conversation-find-rail')?.classList.contains('visible'));
  assert.equal(await page.locator('.token-map-label').innerText(),'Context');
  assert.equal(await page.locator('.conversation-find-label').innerText(),'Find');
  const labels=await page.evaluate(()=>{
    const bottom=selector=>document.querySelector(selector).getBoundingClientRect().bottom;
    const top=selector=>document.querySelector(selector).getBoundingClientRect().top;
    return {turn:top('.conversation-map-label')-bottom('.conversation-map'),context:top('.token-map-label')-bottom('.token-map')};
  });
  assert.ok(Math.abs(labels.turn)<=1);
  assert.ok(Math.abs(labels.context)<=1);
  if(process.env.RAIL_SCREENSHOT)await page.screenshot({path:path.join(root,'.context/conversation-rails.png')});
  const positions=await page.evaluate(()=>{
    const box=selector=>document.querySelector(selector).getBoundingClientRect();
    return {turn:box('.conversation-map-turn:nth-child(15)'),token:box('.token-map .event-marker'),search:box('.conversation-find-marker'),window:box('.conversation-map-window'),searchRail:box('.conversation-find-rail'),turnRail:box('.conversation-map'),turnTrack:box('.conversation-map-track'),tokenTrack:box('.token-map-track'),searchTrack:box('.conversation-find-track')};
  });
  const expectedTokenY=await page.evaluate(()=>{const track=document.querySelector('.token-map-track'),target=document.querySelector('.context-evidence').closest('.timeline-entry');return track.getBoundingClientRect().y+conversationRailFraction(conversationRailMetrics(document.querySelector('.turn-list')),target)*track.clientHeight});
  assert.ok(Math.abs(expectedTokenY-positions.token.y)<2,`Context marker offset: ${positions.token.y-expectedTokenY}`);
  assert.ok(Math.abs(positions.turn.y-positions.search.y)<30,`Search marker offset: ${positions.search.y-positions.turn.y}`);
  assert.ok(Math.abs(positions.turnTrack.y-positions.tokenTrack.y)<2);
  assert.ok(Math.abs(positions.turnTrack.y-positions.searchTrack.y)<2);
  assert.ok(positions.window.x<=positions.searchRail.x+1);
  assert.ok(positions.window.x+positions.window.width>=positions.turnRail.x+positions.turnRail.width-1);
  const before=await page.locator('.conversation-map-viewport').getAttribute('style');
  await page.evaluate(()=>scrollTo(0,1200));
  await page.waitForFunction(previous=>document.querySelector('.conversation-map-viewport').getAttribute('style')!==previous,before);
  await page.addStyleTag({content:'body > header {display:block!important}'});
  await page.evaluate(()=>setNavOpen(true));
  const railTops=await page.evaluate(()=>{
    const top=selector=>document.querySelector(selector).getBoundingClientRect().top;
    return {header:document.querySelector('body > header').getBoundingClientRect().bottom,turn:top('.conversation-map'),context:top('.token-map'),find:top('.conversation-find-rail'),window:top('.conversation-map-window')};
  });
  for(const rail of ['turn','context','find','window'])assert.ok(railTops[rail]>=railTops.header,`${rail} rail overlaps navigation`);
  assert.equal(railTops.turn,railTops.context);
  assert.equal(railTops.turn,railTops.find);
  assert.equal(railTops.turn,railTops.window);
  await page.evaluate(()=>scrollTo(0,document.body.scrollHeight));
  await page.waitForTimeout(100);
  const windowExtent=await page.locator('.conversation-map-viewport').evaluate(el=>parseFloat(el.style.top)+parseFloat(el.style.height));
  assert.ok(windowExtent<=100.001,`Viewport window extends below the rail: ${windowExtent}%`);
});

test('large conversations defer turn bodies and reveal later turns on demand', async t => {
  const messages=[];
  for(let index=0;index<55;index++){
    messages.push(event(`human-${index}`,'message',`Prompt ${index}`,index,{role:'user'}));
    messages.push(event(`reply-${index}`,'message',`Reply ${index}${index===48?' distant_needle':''}`,index));
  }
  const page=await fixture(t,messages);
  assert.equal(await page.locator('.turn').count(),55);
  assert.equal(await page.locator('.turn-body .timeline-entry').count(),4);
  assert.equal(await page.locator('.turn').nth(48).evaluate(element=>element.open),false);
  await page.locator('.turn').nth(48).locator('summary').first().click();
  await page.waitForFunction(()=>document.querySelectorAll('.turn')[48].querySelectorAll('.timeline-entry').length===2);
  assert.equal(await page.locator('.turn-body .timeline-entry').count(),6);
  await page.getByRole('button',{name:'Find',exact:true}).click();
  await page.getByRole('searchbox',{name:'Find in conversation'}).fill('distant_needle');
  assert.equal(await page.locator('.conversation-find-match').count(),1);
});

test('a single oversized turn also starts as a summary', async t => {
  const messages=[event('human-large','message','Large prompt',0,{role:'user'})];
  for(let index=0;index<350;index++)messages.push(event(`reply-large-${index}`,'message',`Response ${index}`,index));
  const page=await fixture(t,messages);
  assert.equal(await page.locator('.turn').count(),1);
  assert.equal(await page.locator('.turn-body .timeline-entry').count(),0);
  await page.locator('.turn > summary').click();
  await page.waitForFunction(()=>document.querySelectorAll('.turn-body .timeline-entry').length===351);
});
