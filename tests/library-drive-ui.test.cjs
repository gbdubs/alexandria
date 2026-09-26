// Drives the library, drive and captures UI (internal/archive/assets/library.js
// and onboarding.js) against a synthetic portable library on a disk image this
// test creates and detaches, served by a freshly built alexandria on a free
// port. Two Macs are simulated with PHAROS_HOST_ID: "MacBook Air" captures
// first from the CLI; "Mac Studio" is the Mac running the service, onboarding
// for the first time. Nothing outside the temporary directory and the disk
// image is written: HOME and PHAROS_SUPPORT_DIR point into the temporary
// directory for every process.
//
//   node --test tests/library-drive-ui.test.cjs
//
// PHAROS_UI_SCREENSHOTS=DIR saves screenshots (ui-*.png) there. The
// Playwright package is looked up in .context/browser-tests above this
// checkout, or PHAROS_BROWSER_TESTS.
const { describe, it, before, after } = require('node:test');
const assert = require('node:assert/strict');
const fs = require('node:fs');
const os = require('node:os');
const net = require('node:net');
const path = require('node:path');
const { spawn, execFileSync } = require('node:child_process');
const { createRequire } = require('node:module');

const root = path.resolve(__dirname, '..');
function browserTests() {
  if (process.env.PHAROS_BROWSER_TESTS) return path.join(process.env.PHAROS_BROWSER_TESTS, 'package.json');
  for (let dir = root; ; dir = path.dirname(dir)) {
    const candidate = path.join(dir, '.context/browser-tests/package.json');
    if (fs.existsSync(candidate)) return candidate;
    if (path.dirname(dir) === dir) return null;
  }
}
const packageJSON = browserTests();
const skip = process.platform !== 'darwin' ? 'needs macOS (hdiutil, diskutil)' : !packageJSON ? 'Playwright not found (.context/browser-tests)' : false;
const screenshots = process.env.PHAROS_UI_SCREENSHOTS;

const work = fs.mkdtempSync(path.join(os.tmpdir(), 'pharos-ui-'));
const volume = `PharosUI-${process.pid}`;
let mount, device, library, token, port, service, browser, base;
const binary = path.join(work, 'alexandria');

const hostA = { id: 'host-a', label: 'Mac Studio', home: path.join(work, 'home-a'), support: path.join(work, 'support-a') };
const hostB = { id: 'host-b', label: 'MacBook Air', home: path.join(work, 'home-b'), support: path.join(work, 'support-b') };
const envFor = host => ({ ...process.env, HOME: host.home, PHAROS_SUPPORT_DIR: host.support, PHAROS_HOST_ID: host.id, PHAROS_HOST_LABEL: host.label, PHAROS_PROBE_SHELL: '0' });
const cli = (host, ...args) => execFileSync(binary, ['--config', path.join(library, 'library.toml'), ...args], { env: envFor(host), encoding: 'utf8', stdio: ['ignore', 'pipe', 'pipe'] });

function write(file, text) {
  fs.mkdirSync(path.dirname(file), { recursive: true });
  fs.writeFileSync(file, text);
}
function claudeSession(home, project, session, turns) {
  const lines = [];
  for (let i = 0; i < turns; i++) {
    const time = `2026-09-0${1 + (i % 9)}T10:${String(i).padStart(2, '0')}:00Z`;
    lines.push(JSON.stringify({ type: 'user', uuid: `${session}-u${i}`, sessionId: session, cwd: `/Users/test/${project}`, timestamp: time, message: { role: 'user', content: `Question ${i} about ${project}` } }));
    lines.push(JSON.stringify({ type: 'assistant', uuid: `${session}-a${i}`, parentUuid: `${session}-u${i}`, sessionId: session, cwd: `/Users/test/${project}`, timestamp: time, message: { role: 'assistant', model: 'claude-test', content: [{ type: 'text', text: `Answer ${i} for ${project}. `.repeat(20) }] } }));
  }
  write(path.join(home, '.claude/projects', `-Users-test-${project}`, `${session}.jsonl`), lines.join('\n') + '\n');
}
function codexSession(home, id, messages) {
  const lines = [JSON.stringify({ type: 'session_meta', timestamp: '2026-09-02T09:00:00Z', payload: { id, cwd: '/Users/test/codex-project' } })];
  for (let i = 0; i < messages; i++) lines.push(JSON.stringify({ type: 'event_msg', timestamp: `2026-09-02T09:${String(i + 1).padStart(2, '0')}:00Z`, payload: { type: 'user_message', message: `codex message ${i}` } }));
  write(path.join(home, '.codex/sessions/2026/09/02', `rollout-2026-09-02T09-00-00-${id}.jsonl`), lines.join('\n') + '\n');
}
const freePort = () => new Promise((resolve, reject) => {
  const server = net.createServer();
  server.listen(0, '127.0.0.1', () => { const { port } = server.address(); server.close(() => resolve(port)); });
  server.on('error', reject);
});
async function waitFor(check, timeout = 30_000, every = 200) {
  const deadline = Date.now() + timeout;
  for (;;) {
    const value = await check().catch(() => null);
    if (value) return value;
    if (Date.now() > deadline) throw new Error('timed out');
    await new Promise(resolve => setTimeout(resolve, every));
  }
}
const api = async (pathname, options = {}) => {
  const response = await fetch(`${base}${pathname}`, { ...options, headers: { Authorization: `Bearer ${token}`, 'Content-Type': 'application/json' } });
  return response.json();
};
// A page, or with a locator just that element (a full-page shot of a scrolled
// page repeats the sticky header mid-page).
async function shoot(target, name) {
  if (!screenshots) return;
  fs.mkdirSync(screenshots, { recursive: true });
  await target.screenshot({ path: path.join(screenshots, `ui-${name}.png`) });
}
// Only the onboarding test sees onboarding. The tests run in order: the
// second expects the Mac the first onboarded; the others also run alone.
// Waits until nothing holds the library (a slow index from an earlier test).
const idle = () => waitFor(async () => (await api('/api/library/status')).idle, 180_000, 500);

async function openPage(init, { onboarding = false } = {}) {
  const context = await browser.newContext({ viewport: { width: 1280, height: 900 } });
  if (!onboarding) await context.addInitScript(id => sessionStorage.setItem(`pharos-onboarding-dismissed:${id}`, '1'), hostA.id);
  if (init) await context.addInitScript(init);
  const page = await context.newPage();
  const errors = [];
  page.on('pageerror', error => errors.push(error.message));
  await page.goto(`${base}/?token=${encodeURIComponent(token)}`);
  return { page, context, errors };
}

describe('library, drive and captures UI', { skip }, () => {
  before(async () => {
    execFileSync('go', ['build', '-o', binary, './cmd/alexandria'], { cwd: root, env: { ...process.env, GOTOOLCHAIN: 'auto' } });
    // Sparse, so it takes only what is written; captures want 1 GB free.
    execFileSync('hdiutil', ['create', '-quiet', '-type', 'SPARSE', '-size', '3g', '-fs', 'APFS', '-volname', volume, path.join(work, 'library')]);
    const attached = execFileSync('hdiutil', ['attach', '-nobrowse', path.join(work, 'library.sparseimage')], { encoding: 'utf8' });
    device = attached.trim().split('\n')[0].split(/\s+/)[0];
    mount = attached.split('\n').map(line => line.split('\t').pop().trim()).find(value => value.startsWith('/Volumes/'));
    assert.ok(mount?.startsWith(`/Volumes/${volume}`), `mounted at ${mount}`);
    library = path.join(mount, 'Pharos');
    execFileSync(binary, ['init-library', library], { env: envFor(hostA), stdio: 'ignore' });
    port = await freePort();
    assert.ok(port !== 8765 && port !== 8766);
    const config = path.join(library, 'library.toml');
    fs.writeFileSync(config, fs.readFileSync(config, 'utf8').replace(/^port = \d+$/m, `port = ${port}`));
    token = fs.readFileSync(config, 'utf8').match(/^api_token = "(.*)"$/m)[1];
    base = `http://127.0.0.1:${port}`;

    // MacBook Air: sources found by the probe, captured from the CLI, not
    // indexed. Enough sessions that indexing them takes a while to watch.
    for (let i = 0; i < 600; i++) claudeSession(hostB.home, `air-${i % 40}`, `b${String(i).padStart(7, '0')}-0000-4000-8000-000000000000`, 3);
    cli(hostB, 'probe', '--accept-all');
    cli(hostB, 'capture');
    // Mac Studio: found by onboarding in the browser.
    claudeSession(hostA.home, 'studio-app', 'a0000000-0000-4000-8000-000000000001', 6);
    claudeSession(hostA.home, 'studio-app', 'a0000000-0000-4000-8000-000000000002', 5);
    claudeSession(hostA.home, 'studio-docs', 'a0000000-0000-4000-8000-000000000003', 2);
    codexSession(hostA.home, 'c0000000-0000-4000-8000-000000000001', 5);

    service = spawn(binary, ['--config', config, 'serve'], { env: envFor(hostA), stdio: ['ignore', 'ignore', 'pipe'] });
    let stderr = '';
    service.stderr.on('data', chunk => { stderr += chunk; });
    await waitFor(async () => (await fetch(`${base}/api/health`, { headers: { Authorization: `Bearer ${token}` } })).ok, 30_000).catch(() => { throw new Error(`service did not start: ${stderr}`); });
    const requireLocal = createRequire(packageJSON);
    const { chromium } = requireLocal('playwright');
    const chrome = '/Applications/Google Chrome.app/Contents/MacOS/Google Chrome';
    browser = await chromium.launch({ headless: true, ...(fs.existsSync(chrome) ? { executablePath: chrome } : {}) });
  });

  after(async () => {
    await browser?.close();
    if (service && service.exitCode === null) {
      service.kill('SIGTERM');
      await new Promise(resolve => { service.once('exit', resolve); setTimeout(resolve, 10_000); });
    }
    if (device) { try { execFileSync('hdiutil', ['detach', '-force', device], { stdio: 'ignore' }); } catch {} }
    fs.rmSync(work, { recursive: true, force: true });
  });

  it('onboards a new Mac: add sources, capture, then index', async () => {
    const { page, context, errors } = await openPage(null, { onboarding: true });
    await page.getByRole('dialog', { name: 'New Mac detected: Mac Studio' }).waitFor();
    // Its title follows the steps, so find it by id from here on.
    const dialog = page.locator('#pharosOnboarding [role=dialog]');
    await dialog.getByText('~/.claude/projects').waitFor();
    const add = dialog.getByRole('button', { name: 'Add 2 sources' });
    await add.click();
    await dialog.getByText(/^Captured .* from Mac Studio\.$/).waitFor({ timeout: 30_000 });
    const text = await dialog.textContent();
    assert.match(text, new RegExp(`You can eject ${volume} now; indexing can finish now or later on any Mac\\.`));
    assert.match(text, /claude.*Captured/s);
    await shoot(page, 'onboarding-captured');
    await dialog.getByRole('button', { name: 'Index now' }).click();
    await dialog.getByText(/^Indexed \d+ conversations? \(\d+ messages?\) from Mac Studio\.$/).waitFor({ timeout: 60_000 });
    await shoot(page, 'onboarding-indexed');
    const indexed = await api('/api/index');
    const studio = indexed.hosts.find(host => host.id === 'host-a');
    assert.deepEqual(studio.sources.map(source => [source.name, source.needs_index]).sort(), [['claude', false], ['codex', false]]);
    await dialog.getByRole('button', { name: 'Done' }).click();
    assert.equal(await page.locator('#pharosOnboarding').count(), 0);
    assert.deepEqual(errors, []);
    await context.close();
  });

  it("shows another Mac's captures as needing an index, and indexes them", async () => {
    const { page, context, errors } = await openPage();
    await page.goto(`${base}/settings`);
    const air = page.locator('#sourceGrid .source-card.remote[data-host="host-b"][data-source="claude"]');
    await air.waitFor();
    assert.match(await air.textContent(), /MacBook Air.*Last indexed.*Last index attempt.*Coverage.*Account.*Captured.*needs index/s);
    assert.equal(await air.locator('.source-state').count(), 0);
    const studio = page.locator('#sourceGrid .source-card:not(.remote)', { has: page.locator('h2', { hasText: /^codex$/ }) });
    assert.match(await studio.textContent(), /this Mac/);
    assert.equal(await studio.locator('.source-state').count(), 0);
    assert.match(await page.locator('#sourceGrid .source-card').first().locator('.pharos-card-capture').textContent(), /Captured .* · indexed/);
    await shoot(page.locator('#sourceGrid'), 'hosts-needs-index');
    assert.equal(await air.getByRole('button').count(), 0);
    await page.locator('#sourceToolbar').getByRole('button', { name: 'Index captured files' }).click();
    // While it runs: the run line, and the drive badge with its panel.
    await page.locator('#sourceActivity .pharos-run', { hasText: 'Indexing captures' }).waitFor({ timeout: 15_000 });
    await shoot(page.locator('#sources'), 'hosts-indexing');
    // The chip shows only the drive's name and a brass dot; what runs is in its tooltip.
    await page.waitForFunction(() => document.querySelector('#pharosDrive')?.dataset.state === 'busy' && /Indexing/.test(document.querySelector('#pharosDrive').title), null, { timeout: 15_000 });
    assert.equal(await page.locator('#pharosDrive').textContent(), volume);
    await page.evaluate(() => scrollTo(0, 0));
    await page.locator('#pharosDrive').click();
    const panel = page.getByRole('dialog', { name: 'Library drive' });
    await panel.getByText('Indexing captures', { exact: true }).waitFor();
    assert.match(await panel.textContent(), /host-b\/claude · 0 of 1 sources/);
    assert.match(await panel.textContent(), /Indexing captures(, [^:]+)?: writing to .* now; unplugging it would lose that work/);
    await shoot(page, 'drive-panel-indexing');
    await page.keyboard.press('Escape');
    await page.waitForFunction(() => /· indexed$/.test(document.querySelector('#sourceGrid .source-card.remote[data-host="host-b"][data-source="claude"] .pharos-card-capture')?.textContent.trim() || ''), null, { timeout: 180_000 });
    assert.doesNotMatch(await air.textContent(), /needs index/);
    assert.equal(await page.locator('#pharosIndexAll').isDisabled(), true);
    const status = await api('/api/index');
    const remote = status.hosts.find(host => host.id === 'host-b').sources[0];
    assert.equal(remote.needs_index, false);
    assert.equal(remote.coverage, 'complete');
    assert.ok(remote.path);
    assert.ok(remote.last_attempt_at);
    await shoot(page.locator('#sourceGrid'), 'hosts-indexed');
    await page.evaluate(() => scrollTo(0, 0));
    await shoot(page.locator('#sourceGrid .source-card').first(), 'source-card-capture');
    // The per-source switches live in card headers.
    const card = page.locator('#sourceGrid .source-card', { has: page.locator('h2', { hasText: /^codex$/ }) });
    assert.equal(await card.locator('.source-card-head [role="switch"]').count(), 1);
    assert.equal(await card.getByRole('button', { name: 'Index source' }).count(), 0);
    await card.getByRole('switch').click();
    await card.getByText('Paused', { exact: true }).first().waitFor();
    const sources = await api('/api/sources');
    assert.equal(sources.items.find(source => source.name === 'codex').enabled, false);
    await card.getByRole('switch').click();
    await page.waitForFunction(() => [...document.querySelectorAll('#sourceGrid .source-card')].some(card => card.querySelector('h2')?.textContent === 'codex' && card.querySelector('[role=switch]')?.getAttribute('aria-checked') === 'true'));
    assert.deepEqual(errors, []);
    await context.close();
  });

  it("renders the drive's checks with copyable commands", async () => {
    const { page, context, errors } = await openPage();
    await page.goto(`${base}/settings`);
    await page.locator('#healthCards > .panel', { hasText: 'Index size' }).locator('.big', { hasNotText: '…' }).waitFor();
    await page.locator('#healthCards .health-host-list').waitFor();
    await shoot(page.locator('#healthCards'), 'health-cards');
    const checks = page.locator('#pharosDriveChecks');
    await checks.locator('[data-check="encryption"]').waitFor({ timeout: 30_000 });
    const encryption = await checks.locator('[data-check="encryption"]').textContent();
    assert.match(encryption, /problem/);
    assert.match(encryption, /disk image that is not encrypted/);
    for (const id of ['spotlight', 'filesystem', 'location', 'free_space', 'volume_pin', 'backup']) assert.equal(await checks.locator(`[data-check="${id}"]`).count(), 1, id);
    const backupCheck = checks.locator('[data-check="backup"]');
    assert.match(await backupCheck.textContent(), /No backup recorded/);
    assert.match(await backupCheck.locator('code').textContent(), / backup /);
    assert.equal(await backupCheck.getByRole('button', { name: 'Copy' }).count(), 1);
    assert.match(await page.locator('#pharosDriveHealth h3').first().textContent(), new RegExp(`Library drive · ${volume}`));
    await page.locator('#pharosDriveHealth').getByRole('button', { name: 'Collapse' }).click();
    assert.equal(await checks.isVisible(), false);
    assert.ok(await page.locator('#pharosDriveHealth .pharos-health-attention .pharos-chip').count() >= 1);
    await shoot(page.locator('#pharosDriveHealth'), 'health-drive-collapsed');
    await page.locator('#pharosDriveHealth').getByRole('button', { name: 'Encryption' }).click();
    assert.equal(await checks.isVisible(), true);
    await shoot(page.locator('#pharosDriveHealth'), 'health-drive');
    assert.deepEqual(errors, []);
    await context.close();
  });

  it('shows disk space in the drive panel and keeps source actions together', async () => {
    const { page, context, errors } = await openPage();
    const headerAction = page.locator('#headerSync');
    await headerAction.waitFor();
    assert.equal(await headerAction.getAttribute('title'), 'Capture and Index');
    assert.equal(await headerAction.getAttribute('aria-label'), 'Capture and Index');
    await page.goto(`${base}/settings`);
    const actions = page.locator('#sourceToolbar');
    await actions.getByRole('button', { name: 'Find sources on this Mac…' }).waitFor();
    assert.equal(await actions.getByRole('button', { name: 'Capture this Mac' }).count(), 1);
    assert.equal(await actions.getByRole('button', { name: 'Index captured files' }).count(), 1);
    assert.equal(await page.locator('#sourceGrid .sync-source, #sourceGrid .remote button, #pharosHosts').count(), 0);
    await page.locator('#pharosDrive').click();
    const drivePanel = page.getByRole('dialog', { name: 'Library drive' });
    await drivePanel.getByText('Archive size').waitFor();
    await page.waitForFunction(() => [...document.querySelectorAll('#pharosDrivePanel dd')].every(item => /\d+(?:\.\d+)? (?:B|KB|MB|GB|TB)/.test(item.textContent)));
    assert.deepEqual(await drivePanel.locator('dt').allTextContents(), ['Free space', 'Archive size']);
    for (const value of await drivePanel.locator('dd').allTextContents()) assert.match(value, /^\d+(?:\.\d+)? (?:B|KB|MB|GB|TB)$/);
    await page.keyboard.press('Escape');

    assert.deepEqual(errors, []);
    await context.close();
  });

  it('backs up from Health, refusing the library drive with a clear reason', async () => {
    const { page, context, errors } = await openPage();
    await page.goto(`${base}/settings`);
    const input = page.locator('#pharosBackupDestination');
    await input.waitFor({ timeout: 30_000 });
    assert.equal(await input.inputValue(), path.join(hostA.home, 'Pharos Backup'));
    await input.fill(path.join(mount, 'Backup'));
    await page.getByRole('button', { name: 'Back up now' }).click();
    const refusal = page.locator('#pharosBackupError');
    await refusal.waitFor();
    assert.match(await refusal.textContent(), /on the same volume as .*a backup there would be lost with the library's drive/);
    await shoot(page.locator('#pharosBackup'), 'backup-refused');
    // The test library's disk image is stored on this Mac's disk, so a folder
    // there is on the same physical drive and must be refused too.
    const destination = path.join(work, 'backup-dest');
    await input.fill(destination);
    await page.getByRole('button', { name: 'Back up now' }).click();
    await page.locator('#pharosBackupError').getByText(/is on a disk image .* stored on the same drive as/).waitFor();
    assert.ok(!fs.existsSync(path.join(destination, 'pharos-backup.json')));
    // A real backup needs a second physical drive, so the finished states are
    // rendered from the service's status shape instead.
    const finished = (state, missing) => ({
      active: false, runs: [], suggested_destination: destination,
      last_success: { finished_at: new Date().toISOString(), destination, bytes_total: 4096, bytes_copied: 4096 },
      run: { state, phase: state, destination, completed_at: new Date().toISOString(), files_total: 12, bytes_total: 4096,
        bytes_copied: 4096, duration_seconds: 1, missing, errors: [], warnings: [] },
    });
    let status = finished('complete', []);
    await page.route('**/api/backup', route => route.request().method() === 'GET'
      ? route.fulfill({ json: status }) : route.fulfill({ status: 202, json: { ok: true, run: status.run } }));
    await page.getByRole('button', { name: 'Back up now' }).click();
    await page.locator('#pharosBackup').getByText(/^Backed up 12 files/).waitFor();
    assert.match(await page.locator('#pharosBackup').textContent(), /Last backup just now to /);
    await shoot(page.locator('#pharosBackup'), 'backup-done');
    status = finished('partial', ['hosts']);
    await page.getByRole('button', { name: 'Back up now' }).click();
    await page.locator('#pharosBackup').getByText(/^Backed up 12 files .*, without hosts, which could not be read/).waitFor();
    await page.unroute('**/api/backup');
    assert.deepEqual(errors, []);
    await context.close();
  });

  it('says how to eject in Finder in a plain browser', async () => {
    await idle();
    const { page, context, errors } = await openPage();
    const chip = page.locator('#pharosDrive');
    await chip.waitFor();
    await page.waitForFunction(name => document.querySelector('#pharosDrive')?.textContent.includes(name), volume);
    assert.equal(await chip.textContent(), volume);
    await chip.click();
    const panel = page.getByRole('dialog', { name: 'Library drive' });
    await panel.waitFor();
    const text = await panel.textContent();
    assert.match(text, new RegExp(`Eject ${volume} in Finder`));
    assert.match(text, /Nothing is running\./);
    assert.match(text, /Unplugging without ejecting now would probably lose nothing/);
    assert.equal(await panel.locator('code').textContent(), `diskutil eject ${mount}`);
    assert.equal(await panel.locator('#pharosEjectButton').count(), 0);
    await shoot(page, 'drive-panel-browser');
    await page.emulateMedia({ colorScheme: 'dark' });
    await shoot(page, 'drive-panel-browser-dark');
    await page.emulateMedia({ colorScheme: 'light' });
    assert.deepEqual(errors, []);
    await context.close();
  });

  it('shows what holds the library, and ejects through the app when it can', async () => {
    await idle();
    // A capture by another process holds this Mac's capture lock.
    const lock = path.join(library, 'captures', hostA.id, '.capture.lock');
    const holder = spawn('python3', ['-c', 'import fcntl,sys,time\nf=open(sys.argv[1],"a")\nfcntl.flock(f,fcntl.LOCK_EX)\nprint("locked",flush=True)\ntime.sleep(60)', lock], { stdio: ['ignore', 'pipe', 'ignore'] });
    await new Promise(resolve => holder.stdout.once('data', resolve));
    const messages = [];
    const { page, context, errors } = await openPage(() => {
      // Stands in for the app's pharosLibrary handler (WKScriptMessageHandlerWithReply).
      window.__ejectAnswer = 'refuse';
      window.webkit = { messageHandlers: { pharosLibrary: { postMessage: async message => {
        window.__ejectMessages = [...(window.__ejectMessages || []), message];
        if (window.__ejectAnswer === 'refuse') throw new Error('Pharos is finishing a write; try ejecting again in a moment.');
        return { released: true };
      } } } };
    });
    try {
      const chip = page.locator('#pharosDrive');
      await page.waitForFunction(() => /Capturing/.test(document.querySelector('#pharosDrive')?.title || ''), null, { timeout: 15_000 });
      assert.equal(await page.locator('#pharosDrive .pharos-dot.busy').count(), 1);
      await chip.click();
      const panel = page.getByRole('dialog', { name: 'Library drive' });
      await panel.getByText('Capture by another process', { exact: true }).waitFor();
      assert.match(await panel.textContent(), new RegExp(`Capture by another process: writing to ${volume} now; unplugging it would lose that work\\. Eject it instead.*Pharos cannot stop it\\.`));
      await shoot(page, 'drive-panel-busy');
      await panel.getByRole('button', { name: `Eject ${volume}` }).click();
      await panel.getByText('Pharos cannot stop a capture by another process').waitFor();
      await panel.getByRole('button', { name: `Stop and eject ${volume}` }).click();
      await panel.getByText('Pharos is finishing a write; try ejecting again in a moment.').waitFor();
      await shoot(page, 'drive-panel-eject-refused');
      holder.kill();
      await page.waitForFunction(() => document.querySelector('#pharosDrive')?.dataset.state === 'idle', null, { timeout: 15_000 });
      await page.evaluate(() => { window.__ejectAnswer = 'release'; });
      await panel.getByRole('button', { name: `Eject ${volume}` }).click();
      await panel.getByRole('button', { name: `Ejecting ${volume}…` }).waitFor();
      messages.push(...await page.evaluate(() => window.__ejectMessages));
      assert.deepEqual(messages, [{ action: 'eject' }, { action: 'eject' }]);
      assert.deepEqual(errors, []);
    } finally {
      holder.kill();
      await context.close();
    }
  });
});
