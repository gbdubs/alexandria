// Pharos library, drive and captures. GET /api/library/status says what holds
// or writes the library and how to disconnect its drive; the header shows it,
// with Eject. Eject goes through the app (the pharosLibrary message handler),
// which releases the library before it ejects the drive; in a plain browser
// the page says how to eject in Finder instead. Settings → Sources gains the
// Macs whose captures are in the library, with Capture and Index; Settings →
// Health gains the drive's checks and backups.
(() => {
  'use strict';
  const CSS = `
.pharos-drive{display:flex;align-items:center;gap:7px;min-width:0;max-width:190px;height:32px;box-sizing:border-box;padding:0 10px;border:1px solid #f3ebdc30;border-radius:4px;background:#ffffff0f;color:var(--mast-ink,#f3ebdc);font-size:var(--fs-md,13px);line-height:1;white-space:nowrap}
.pharos-drive:hover,.pharos-drive[aria-expanded="true"]{border-color:var(--mast-brass,#d4a857);color:#f1d690}
.pharos-drive-name{flex:0 1 auto;min-width:2.5em;font-weight:650;overflow:hidden;text-overflow:ellipsis}
.pharos-drive-state{flex:none;color:var(--mast-muted,#c3cebc)}
.pharos-drive-state:empty{display:none}
.pharos-dot{flex:none;display:inline-block;width:8px;height:8px;border-radius:50%;background:var(--sea,#315845)}
.pharos-drive .pharos-dot{background:#8cc6a0}
.pharos-dot.busy{background:var(--mast-brass,#d4a857);box-shadow:0 0 0 3px color-mix(in srgb,var(--mast-brass,#d4a857) 30%,transparent)}
.pharos-dot.warn{background:var(--warn,#98601d)}.pharos-dot.bad{background:var(--bad,#9c3d36)}.pharos-dot.idle{background:var(--muted,#756c5f)}
.pharos-drive-panel{position:fixed;z-index:8000;width:min(430px,calc(100vw - 24px));max-height:calc(100vh - 100px);overflow:auto;padding:16px 18px;background:var(--panel,#fff);color:var(--ink,#222);border:1px solid var(--line,#ccc);border-radius:14px;box-shadow:0 18px 50px #0005;font-size:13px}
.pharos-drive-panel h2{margin:0;font:700 20px/1.2 var(--serif,serif)}
.pharos-drive-panel h3{margin:14px 0 6px;font-size:12px;letter-spacing:.06em;text-transform:uppercase;color:var(--muted,#666)}
.pharos-drive-panel p{margin:8px 0}
.pharos-sub{color:var(--muted,#666);overflow-wrap:anywhere}
.pharos-activity{padding:8px 0;border-bottom:1px solid var(--line,#ccc)}
.pharos-activity:last-child{border-bottom:0}
.pharos-activity-head{display:flex;align-items:center;gap:8px;font-weight:650}
.pharos-activity .pharos-sub{margin-top:2px}
.pharos-bar{height:6px;margin:7px 0 4px;overflow:hidden;border-radius:6px;background:var(--line,#ccc)}
.pharos-bar>span{display:block;height:100%;min-width:6px;background:var(--accent,#315845);transition:width .25s}
.pharos-bar.indeterminate>span{width:35%;animation:pharos-slide 1.6s ease-in-out infinite}
@keyframes pharos-slide{from{margin-left:-35%}to{margin-left:100%}}
@media(prefers-reduced-motion:reduce){.pharos-bar.indeterminate>span{animation:none;width:100%;opacity:.5}}
.pharos-advice{padding:10px 12px;border-radius:10px;background:color-mix(in srgb,var(--bg,#eee) 70%,transparent)}
.pharos-advice.unsafe{background:color-mix(in srgb,var(--warn,#98601d) 12%,transparent)}
.pharos-row{display:flex;flex-wrap:wrap;align-items:center;gap:8px;margin-top:12px}
.pharos-row .pharos-note{flex:1 1 100%}
.pharos-button{border:1px solid var(--line,#ccc);background:var(--panel,#fff);color:var(--ink,#222)}
.pharos-button.primary{background:var(--accent,#315845);border-color:var(--accent,#315845);color:var(--panel,#fff)}
.pharos-button:disabled{opacity:.55;cursor:default}
.pharos-code{display:flex;align-items:center;gap:8px;margin:6px 0}
.pharos-code code{flex:1;min-width:0;padding:6px 9px;border-radius:7px;background:color-mix(in srgb,var(--bg,#eee) 72%,transparent);font:12px/1.4 ui-monospace,SFMono-Regular,monospace;overflow-wrap:anywhere}
.pharos-code button{flex:none;padding:5px 9px;font-size:12px}
.pharos-error{color:var(--bad,#9c3d36)}.pharos-warn{color:var(--warn,#98601d)}.pharos-ok{color:var(--sea,#315845)}
.pharos-section{margin-top:26px}
.pharos-section-head{display:flex;flex-wrap:wrap;justify-content:space-between;align-items:end;gap:10px 20px;margin-bottom:12px}
.pharos-section-head h3{margin:0 0 4px}
.pharos-section-head p{margin:0;max-width:720px}
.pharos-section-head .pharos-actions{display:flex;flex-wrap:wrap;gap:8px}
.pharos-host{background:var(--panel,#fff);border:1px solid var(--line,#ccc);border-radius:11px;padding:14px 17px;margin-bottom:12px}
.pharos-host-head{display:flex;flex-wrap:wrap;align-items:center;gap:6px 10px}
.pharos-host-head h4{margin:0;font:700 17px/1.25 var(--serif,serif)}
.pharos-host-head .pharos-button{margin-left:auto}
.pharos-chip{border:1px solid var(--line,#ccc);border-radius:20px;padding:1px 8px;font-size:12px;color:var(--muted,#666)}
.pharos-chip.warn{color:var(--warn,#98601d);border-color:color-mix(in srgb,var(--warn,#98601d) 50%,var(--line,#ccc))}
.pharos-table{width:100%;margin-top:10px;border-collapse:collapse;font-size:13px}
.pharos-table th{text-align:left;font-weight:500;color:var(--muted,#666);padding:5px 10px 5px 0;border-bottom:1px solid var(--line,#ccc)}
.pharos-table td{padding:6px 10px 6px 0;border-bottom:1px solid var(--line,#ccc);vertical-align:top}
.pharos-table tr:last-child td{border-bottom:0}
.pharos-run{margin:0 0 12px;padding:10px 14px;border:1px dashed var(--line,#ccc);border-radius:11px}
.pharos-card-capture{margin-top:9px;font-size:12px}
.pharos-checks{display:grid;gap:0;background:var(--panel,#fff);border:1px solid var(--line,#ccc);border-radius:14px;padding:4px 18px}
.pharos-check{display:grid;grid-template-columns:18px 1fr;gap:2px 8px;padding:12px 0;border-bottom:1px solid var(--line,#ccc)}
.pharos-check:last-child{border-bottom:0}
.pharos-check>.pharos-dot{margin-top:6px}
.pharos-check-title{font-weight:650}
.pharos-check>div{grid-column:2}
.pharos-check>div{font-size:13px}
.pharos-backup{background:var(--panel,#fff);border:1px solid var(--line,#ccc);border-radius:14px;padding:16px 18px;margin-top:14px}
.pharos-backup h3{margin:0 0 8px}
.pharos-backup form{display:flex;flex-wrap:wrap;gap:8px;align-items:center;margin-top:10px}
.pharos-backup label{color:var(--muted,#666)}
.pharos-backup input{flex:1 1 320px;min-width:0;padding:7px 10px;border:1px solid var(--line,#ccc);border-radius:8px;background:var(--bg,#fff);color:var(--ink,#222);font:12px ui-monospace,SFMono-Regular,monospace}
.pharos-backup ul{margin:6px 0;padding-left:18px}`;

  // ---- helpers
  const node = (tag, cls, text) => {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined && text !== null) n.textContent = text;
    return n;
  };
  const button = (text, cls = '', onclick = null) => {
    const b = node('button', 'pharos-button ' + cls, text);
    b.type = 'button';
    if (onclick) b.onclick = onclick;
    return b;
  };
  const call = async (path, options = {}) => {
    const response = await fetch(path, {...options, headers: {'Content-Type': 'application/json'}});
    const body = await response.json().catch(() => ({}));
    if (!response.ok) {
      const error = new Error(body.error || response.statusText);
      error.status = response.status;
      throw error;
    }
    return body;
  };
  const post = (path, body = {}) => call(path, {method: 'POST', body: JSON.stringify(body)});
  const toast = message => { if (typeof window.feedbackToast === 'function') window.feedbackToast(message); };
  const plural = (count, unit) => `${Number(count || 0).toLocaleString()} ${unit}${count === 1 ? '' : 's'}`;
  const size = value => {
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let amount = Math.max(0, Number(value) || 0), power = 0;
    while (amount >= 1000 && power < units.length - 1) { amount /= 1000; power++; }
    return `${amount.toFixed(amount < 10 && power ? 1 : 0)} ${units[power]}`;
  };
  const ago = value => {
    if (!value) return 'never';
    const seconds = Math.max(0, (Date.now() - new Date(value).getTime()) / 1000);
    if (!Number.isFinite(seconds)) return 'unknown';
    if (seconds < 60) return 'just now';
    if (seconds < 3600) return `${Math.floor(seconds / 60)} min ago`;
    if (seconds < 172800) return `${Math.floor(seconds / 3600)} h ago`;
    return `${Math.floor(seconds / 86400)} days ago`;
  };
  const when = value => {
    const span = node('span', '', ago(value));
    if (value) span.title = new Date(value).toLocaleString();
    return span;
  };
  const progressBar = value => {
    const bar = node('div', 'pharos-bar' + (value == null ? ' indeterminate' : '')), fill = node('span');
    if (value != null) fill.style.width = `${Math.round(value * 100)}%`;
    bar.append(fill);
    return bar;
  };
  const copy = async text => {
    try {
      if (typeof window.alexandriaCopyText === 'function') await window.alexandriaCopyText(text);
      else await navigator.clipboard.writeText(text);
      toast('Copied');
    } catch { toast('Could not copy'); }
  };
  const codeLine = text => {
    const line = node('div', 'pharos-code');
    line.append(node('code', '', text), button('Copy', '', () => copy(text)));
    return line;
  };
  const shellWord = value => /^[\w@%+=:,./-]+$/.test(value) ? value : `'${value.replaceAll("'", `'\\''`)}'`;
  const nativeLibrary = () => window.webkit?.messageHandlers?.pharosLibrary;
  const sleep = ms => new Promise(resolve => setTimeout(resolve, ms));

  // ---- library status (header chip and panel)
  let status = null, statusError = null, statusTimer = null, statusRequest = null;
  const listeners = new Set();
  const driveName = () => status?.drive?.name || 'the drive';

  async function refreshStatus() {
    clearTimeout(statusTimer);
    if (!statusRequest) {
      statusRequest = call('/api/library/status').then(value => { status = value; statusError = null; }, error => { statusError = error; })
        .finally(() => { statusRequest = null; });
    }
    await statusRequest;
    renderChip();
    renderPanel();
    listeners.forEach(listener => listener(status));
    const busy = status && !status.idle;
    if (!document.hidden) statusTimer = setTimeout(refreshStatus, busy || statusError ? 2000 : 10000);
    return status;
  }
  document.addEventListener('visibilitychange', () => { if (!document.hidden) refreshStatus(); });

  let chip = null, panel = null, ejectState = null;
  function ensureChip() {
    if (chip?.isConnected) return chip;
    const icons = document.querySelector('body>header .header-icons');
    if (!icons) return null;
    chip = node('button', 'pharos-drive');
    chip.id = 'pharosDrive';
    chip.type = 'button';
    chip.setAttribute('aria-haspopup', 'dialog');
    chip.setAttribute('aria-expanded', 'false');
    chip.onclick = () => (panel ? closePanel() : openPanel());
    icons.prepend(chip);
    return chip;
  }

  function activitySummary(activity) {
    const verb = {sync: 'Syncing', index: 'Indexing', capture: 'Capturing', 'capture-other': 'Capturing', backup: 'Backing up', git: 'Checking Git', maintenance: 'Updating'}[activity.kind] || activity.label;
    return activity.progress ? `${verb} ${Math.round(activity.progress * 100)}%` : verb;
  }

  function renderChip() {
    const target = ensureChip();
    if (!target) return;
    const dot = node('span', 'pharos-dot'), name = node('span', 'pharos-drive-name'), state = node('span', 'pharos-drive-state');
    if (!status) {
      dot.classList.add(statusError?.status === 503 ? 'warn' : 'idle');
      name.textContent = 'Library';
      state.textContent = statusError?.status === 503 ? 'Stopping' : 'Unavailable';
    } else {
      name.textContent = status.drive?.ejectable ? status.drive.name : status.portable ? 'Library' : 'This Mac';
      const activities = status.activities || [];
      if (activities.length) {
        dot.classList.add('busy');
        state.textContent = activitySummary(activities[0]) + (activities.length > 1 ? ` +${activities.length - 1}` : '');
      }
    }
    target.replaceChildren(dot, name, state);
    target.title = status ? `${status.drive?.name || 'Library'}: ${status.idle ? 'nothing running' : status.activities.map(item => item.label).join(', ')}` : 'Library status unavailable';
    target.setAttribute('aria-label', `Library drive: ${target.title}`);
  }

  function placePanel() {
    if (!panel || !chip) return;
    const box = chip.getBoundingClientRect(), header = document.querySelector('body>header')?.getBoundingClientRect();
    panel.style.top = `${Math.round((header?.bottom ?? box.bottom) + 8)}px`;
    panel.style.left = `${Math.round(Math.max(12, Math.min(box.left, window.innerWidth - panel.offsetWidth - 12)))}px`;
  }

  function openPanel() {
    if (!ensureChip()) return;
    panel = node('section', 'pharos-drive-panel');
    panel.id = 'pharosDrivePanel';
    panel.setAttribute('role', 'dialog');
    panel.setAttribute('aria-label', 'Library drive');
    document.body.append(panel);
    chip.setAttribute('aria-expanded', 'true');
    renderPanel();
    refreshStatus();
  }

  function closePanel() {
    panel?.remove();
    panel = null;
    ejectState = null;
    chip?.setAttribute('aria-expanded', 'false');
  }
  document.addEventListener('mousedown', event => {
    if (panel && !panel.contains(event.target) && !chip?.contains(event.target)) closePanel();
  });
  document.addEventListener('keydown', event => {
    if (panel && event.key === 'Escape') { closePanel(); chip?.focus(); }
  });
  window.addEventListener('resize', placePanel);

  function renderPanel() {
    if (!panel) return;
    panel.replaceChildren();
    if (!status) {
      panel.append(node('h2', '', 'Library'), node('p', 'pharos-error', statusError?.status === 503 ? 'Pharos is stopping; it has released or is releasing the library.' : `Could not read the library's status${statusError ? `: ${statusError.message}` : ''}.`));
      placePanel();
      return;
    }
    const drive = status.drive || {};
    panel.append(node('h2', '', drive.ejectable ? drive.name : status.portable ? 'Library' : 'This Mac'));
    const where = [{external: 'External drive', 'disk-image': 'Disk image', internal: "This Mac's internal disk"}[drive.location] || 'Drive', status.library_dir].filter(Boolean).join(' · ');
    panel.append(node('div', 'pharos-sub', where));

    panel.append(node('h3', '', 'Running now'));
    const activities = status.activities || [];
    if (!activities.length) panel.append(node('p', 'pharos-sub', 'Nothing is running.'));
    activities.forEach(activity => {
      const item = node('div', 'pharos-activity'), head = node('div', 'pharos-activity-head');
      head.append(node('span', 'pharos-dot busy'), node('span', '', activity.label));
      item.append(head);
      if (activity.detail) item.append(node('div', 'pharos-sub', activity.detail));
      if (activity.kind !== 'maintenance' && activity.kind !== 'capture-other') item.append(progressBar(activity.progress));
      panel.append(item);
    });

    const unplug = status.unplug || {};
    if (unplug.action === 'eject') {
      panel.append(node('h3', '', 'Disconnecting'));
      panel.append(node('p', 'pharos-advice' + (unplug.without_eject === 'unsafe' ? ' unsafe' : ''), unplug.summary));
      panel.append(ejectControls(drive, activities));
    } else if (unplug.summary) {
      panel.append(node('p', 'pharos-sub', unplug.summary));
    }
    placePanel();
  }

  function ejectControls(drive, activities) {
    const row = node('div', 'pharos-row');
    row.id = 'pharosEject';
    if (!nativeLibrary()) {
      // A plain browser has no way to eject: say how.
      const note = node('div', 'pharos-note');
      note.append(node('p', '', `Eject ${drive.name} in Finder: click ⏏ beside it in the sidebar. Or in Terminal:`),
        codeLine(`diskutil eject ${shellWord(drive.mount_point)}`),
        node('p', 'pharos-sub', `With the Pharos app running, it lets go of the library before ${drive.name} unmounts, or says why it can't yet. If Pharos runs without its app (alexandria serve), stop that first.`));
      row.append(note);
      return row;
    }
    if (ejectState?.phase === 'confirm') {
      const stopping = activities.filter(activity => activity.kind !== 'capture-other').map(activity => activity.label);
      const other = activities.some(activity => activity.kind === 'capture-other');
      const note = node('p', 'pharos-note', stopping.length ? `Ejecting stops ${stopping.join(', ')} at a safe point; ${stopping.length === 1 ? 'it resumes' : 'each resumes'} the next time it runs.` : '');
      row.append(note);
      if (other) row.append(node('p', 'pharos-note pharos-warn', 'Pharos cannot stop a capture by another process; the drive stays busy until it finishes.'));
      row.append(button(`Stop and eject ${drive.name}`, 'primary', () => eject(drive)), button('Cancel', '', () => { ejectState = null; renderPanel(); }));
      return row;
    }
    const eject_ = button(ejectState?.phase === 'ejecting' ? ejectState.message : `Eject ${drive.name}`, 'primary', () => {
      if (activities.length) { ejectState = {phase: 'confirm'}; renderPanel(); } else eject(drive);
    });
    eject_.id = 'pharosEjectButton';
    eject_.disabled = ejectState?.phase === 'ejecting';
    row.append(eject_);
    if (ejectState?.phase === 'error') row.append(node('p', 'pharos-note pharos-error', ejectState.message));
    return row;
  }

  async function eject(drive) {
    const handler = nativeLibrary();
    if (!handler) return;
    ejectState = {phase: 'ejecting', message: 'Releasing the library…'};
    renderPanel();
    try {
      await handler.postMessage({action: 'eject'});
      // The app now shows the library as released and reports the eject.
      ejectState = {phase: 'ejecting', message: `Ejecting ${drive.name}…`};
    } catch (error) {
      ejectState = {phase: 'error', message: error?.message || String(error)};
    }
    renderPanel();
  }

  // ---- runs, shared with onboarding. An index is watched through
  // /api/activity, which lists runs only: /api/index also reads every
  // capture's manifest.
  async function watch(path, runID, onUpdate, every = 700) {
    for (let failures = 0; ;) {
      let body;
      try { body = await call(path); failures = 0; } catch (error) {
        if (error.status === 503) throw new Error('Pharos stopped: the library was released or its drive went away. The next run resumes where this one stopped.');
        if (++failures > 20) throw error;
        await sleep(every * 2);
        continue;
      }
      const run = (body.runs || []).find(item => item.id === runID) || null;
      onUpdate?.(run, body);
      if (!run || run.state !== 'running') return run;
      await sleep(every);
    }
  }
  const runs = {
    async capture(sources, onUpdate) {
      const started = await post('/api/capture', sources ? {sources} : {});
      refreshStatus();
      onUpdate?.(started.run);
      const run = await watch('/api/capture', started.run.id, onUpdate);
      refreshStatus();
      return run;
    },
    async index(body, onUpdate) {
      const started = await post('/api/index', body);
      refreshStatus();
      onUpdate?.(started.run);
      const run = await watch('/api/activity', started.run.id, onUpdate);
      refreshStatus();
      return run;
    },
  };

  // ---- Settings → Sources: Macs and their captures
  let hostsData = null, hostsBusy = null, hostsError = null;

  async function loadHosts() {
    const [sources, index, capture] = await Promise.all([call('/api/sources'), call('/api/index'), call('/api/capture')]);
    hostsData = {sources, index, capture};
    renderHosts();
    annotateCards();
    const running = index.active || index.sync_active || capture.active;
    return running;
  }

  function hostRows(host, current) {
    const rows = new Map();
    for (const source of host?.sources || []) rows.set(source.name, {name: source.name, kind: source.kind, captured: source});
    if (current) {
      for (const source of hostsData.sources.items || []) {
        const row = rows.get(source.name) || {name: source.name, kind: source.kind};
        row.configured = source;
        rows.set(source.name, row);
      }
    }
    return [...rows.values()];
  }

  function rowState(row) {
    const captured = row.captured, configured = row.configured;
    if (captured?.error) return ['Failed: ' + captured.error, 'pharos-error'];
    if (captured?.needs_index) return ['Needs index', 'pharos-warn'];
    if (captured) return ['Indexed', 'pharos-ok'];
    if (configured && !configured.enabled) return ['Paused', 'pharos-sub'];
    return ['Not captured yet', 'pharos-sub'];
  }

  function renderHosts() {
    const grid = document.getElementById('sourceGrid');
    if (!grid || !hostsData) return;
    let section = document.getElementById('pharosHosts');
    if (!section) {
      section = node('section', 'pharos-section');
      section.id = 'pharosHosts';
      grid.after(section);
    }
    const {sources, index, capture} = hostsData;
    const thisHost = sources.host || index.host || {};
    const captured = index.hosts || [];
    const busy = Boolean(hostsBusy) || index.active || index.sync_active;
    const drive = status?.drive?.ejectable ? status.drive.name : 'the library';
    const head = node('div', 'pharos-section-head'), intro = node('div'), actions = node('div', 'pharos-actions');
    intro.append(node('h3', '', 'Macs and captures'),
      node('p', 'muted', `A capture copies a Mac's conversation files onto ${drive} in seconds to minutes; indexing parses them into the library and can run later, on any Mac, whether or not the Mac that captured them is here.`));
    const captureButton = button(capture.active ? 'Capturing…' : `Capture ${thisHost.label || 'this Mac'}`, '', () => startCapture());
    captureButton.id = 'pharosCaptureHost';
    captureButton.disabled = capture.active || !sources.enabled || hostsBusy === 'capture';
    const needing = captured.filter(host => host.sources.some(source => source.needs_index));
    const indexAll = button('Index all Macs', 'primary', () => startIndex({all_hosts: true}, 'every Mac'));
    indexAll.id = 'pharosIndexAll';
    indexAll.disabled = busy || !needing.length;
    indexAll.title = index.sync_active ? 'A sync or index is running' : needing.length ? `Index ${needing.map(host => host.label).join(', ')}` : 'Everything captured is indexed';
    actions.append(captureButton, indexAll);
    head.append(intro, actions);
    section.replaceChildren(head);

    const line = runLine(capture, index);
    if (line) section.append(line);
    if (hostsError) section.append(node('p', 'pharos-error', hostsError));
    if (index.error && !captured.length) section.append(node('p', 'pharos-sub', `No captures yet (${index.error}).`));

    const ordered = [captured.find(host => host.current) || {id: thisHost.id, label: thisHost.label, user: thisHost.user, current: true, sources: []},
      ...captured.filter(host => !host.current)];
    for (const host of ordered) {
      const card = node('article', 'pharos-host'), top = node('div', 'pharos-host-head');
      card.dataset.host = host.id;
      top.append(node('h4', '', host.label || host.id));
      if (host.current) top.append(node('span', 'pharos-chip', 'this Mac'));
      if (host.user) top.append(node('span', 'pharos-chip', host.user));
      const waiting = host.sources.filter(source => source.needs_index).length;
      if (waiting) top.append(node('span', 'pharos-chip warn', `${plural(waiting, 'source')} to index`));
      const indexHost = button(`Index ${host.label || host.id}`, host.current ? '' : 'primary', () => startIndex({host: host.id}, host.label || host.id));
      indexHost.disabled = busy || !host.sources.length;
      indexHost.title = !host.sources.length ? 'Nothing captured from this Mac yet' : index.sync_active ? 'A sync or index is running' : '';
      top.append(indexHost);
      card.append(top);
      const rows = hostRows(host, host.current);
      if (!rows.length) {
        card.append(node('p', 'pharos-sub', host.current ? 'No sources on this Mac yet.' : 'Nothing captured.'));
      } else {
        const table = node('table', 'pharos-table'), header = node('tr');
        ['Source', 'Captured', 'Indexed', 'State'].forEach(name => header.append(node('th', '', name)));
        table.append(header);
        for (const row of rows) {
          const tr = node('tr'), [state, cls] = rowState(row);
          const name = node('td');
          name.append(node('strong', '', row.name));
          if (row.kind && row.kind !== row.name) name.append(node('span', 'pharos-sub', ` · ${row.kind}`));
          const capturedCell = node('td');
          if (row.captured?.captured_at) {
            capturedCell.append(when(row.captured.captured_at));
            if (!row.captured.capture_finished) capturedCell.append(node('span', 'pharos-warn', ' · interrupted'));
          } else capturedCell.append(node('span', 'pharos-sub', 'not captured'));
          const indexedCell = node('td');
          indexedCell.append(when(row.captured?.indexed_at || row.configured?.last_success_at));
          tr.append(name, capturedCell, indexedCell, node('td', cls, state));
          tr.dataset.source = row.name;
          table.append(tr);
        }
        card.append(table);
      }
      section.append(card);
    }
  }

  // Runs report progress by source; a lone source shows as work in progress.
  const runProgress = run => (run.total_sources > 1 ? run.completed_sources / run.total_sources : null);

  function runLine(capture, index) {
    const box = node('div', 'pharos-run');
    const captureRun = capture.run, indexRun = index.run;
    if (capture.active && captureRun?.state !== 'running') {
      box.append(node('strong', '', 'A capture by another process is running'),
        node('div', 'pharos-sub', 'Another process on this Mac, such as alexandria capture, holds the capture lock. Capture waits until it finishes.'), progressBar(null));
      return box;
    }
    if (capture.active && captureRun) {
      box.append(node('strong', '', `Capturing ${capture.host?.label || 'this Mac'}`),
        node('div', 'pharos-sub', `${captureRun.current_source || 'starting'} · ${captureRun.completed_sources} of ${captureRun.total_sources} sources · ${size(captureRun.bytes_copied)} copied`),
        progressBar(runProgress(captureRun)));
      return box;
    }
    if (indexRun?.state === 'running') {
      box.append(node('strong', '', 'Indexing captures'),
        node('div', 'pharos-sub', `${indexRun.current_source || 'starting'} · ${indexRun.completed_sources} of ${indexRun.total_sources} sources · ${plural(indexRun.conversations, 'conversation')} written`),
        progressBar(runProgress(indexRun)));
      return box;
    }
    const recent = [captureRun, indexRun].filter(Boolean).sort((a, b) => String(b.completed_at || '').localeCompare(String(a.completed_at || '')))[0];
    if (!recent || !recent.completed_at || Date.now() - new Date(recent.completed_at).getTime() > 3600_000) return null;
    const failed = recent.state !== 'complete';
    if (recent.kind === 'capture') {
      box.append(node('span', failed ? 'pharos-error' : '', failed ? `The last capture ${recent.state === 'failed' ? 'finished with errors' : 'stopped'}: ${(recent.errors || []).map(error => error.error).slice(0, 2).join('; ') || 'see the sources below'}.` : `Captured ${size(recent.bytes_copied)} ${ago(recent.completed_at)}.`));
    } else {
      box.append(node('span', failed ? 'pharos-error' : '', failed ? `The last index ${recent.state === 'interrupted' ? 'was interrupted; the next one resumes it' : `failed: ${recent.error || 'see the sources below'}`}.` : `Indexed ${plural(recent.conversations, 'conversation')} ${ago(recent.completed_at)}.`));
    }
    return box;
  }

  async function startCapture() {
    hostsBusy = 'capture';
    hostsError = null;
    renderHosts();
    try {
      const run = await runs.capture(null, update => {
        if (!update || !hostsData) return;
        hostsData.capture = {...hostsData.capture, run: update, active: update.state === 'running'};
        renderHosts();
      });
      toast(run?.state === 'complete' ? `Captured ${size(run.bytes_copied)}` : 'The capture finished with errors');
    } catch (error) {
      hostsError = error.status === 409 ? 'A capture is already running.' : error.message;
    } finally {
      hostsBusy = null;
      await loadHosts().catch(() => {});
    }
  }

  async function startIndex(body, label) {
    hostsBusy = 'index';
    hostsError = null;
    renderHosts();
    try {
      const run = await runs.index(body, update => {
        if (!update || !hostsData) return;
        hostsData.index = {...hostsData.index, run: update, active: update.state === 'running'};
        renderHosts();
      });
      toast(run?.state === 'complete' ? `Indexed ${label}` : `Indexing ${label} ${run?.state === 'interrupted' ? 'was interrupted' : 'finished with errors'}`);
      window.loadSources?.();
    } catch (error) {
      hostsError = error.status === 409 ? 'A sync or index is already running; try again when it finishes.' : error.message;
    } finally {
      hostsBusy = null;
      await loadHosts().catch(() => {});
    }
  }

  // Each of this Mac's source cards says when it was captured.
  function annotateCards() {
    const grid = document.getElementById('sourceGrid');
    if (!grid || !hostsData) return;
    const current = (hostsData.index.hosts || []).find(host => host.current);
    for (const card of grid.querySelectorAll('.source-card')) {
      const name = card.querySelector('h2')?.textContent;
      const source = current?.sources.find(item => item.name === name);
      let line = card.querySelector('.pharos-card-capture');
      if (!line) {
        line = node('div', 'pharos-card-capture');
        const controls = card.querySelector('.source-controls');
        if (controls) controls.before(line); else card.append(line);
      }
      line.replaceChildren();
      if (!source) {
        line.append(node('span', 'pharos-sub', `Not captured to ${driveName() === 'the drive' ? 'the library' : driveName()} yet`));
        continue;
      }
      line.append(node('span', 'pharos-sub', 'Captured '), when(source.captured_at));
      line.append(node('span', source.needs_index ? 'pharos-warn' : 'pharos-sub', source.needs_index ? ' · needs index' : ' · indexed'));
    }
  }

  // ---- Settings → Health: the drive's checks and backups
  const CHECK_TITLES = {encryption: 'Encryption', spotlight: 'Spotlight', filesystem: 'File system', location: 'Location', free_space: 'Free space', volume_pin: 'Volume pin', backup: 'Backup'};
  const STATUS_DOT = {ok: '', info: 'idle', unknown: 'idle', warning: 'warn', problem: 'bad'};
  let backupDraft = null, backupError = null, backupBusy = false;

  async function loadDriveHealth() {
    const [drive, backup] = await Promise.all([call('/api/health/drive'), call('/api/backup')]);
    renderDriveHealth(drive, backup);
    return drive.status === 'checking' || backup.active;
  }

  function renderDriveHealth(drive, backup) {
    const cards = document.getElementById('healthCards');
    // Leave the form alone while someone is typing a destination.
    if (!cards || document.activeElement?.id === 'pharosBackupDestination') return;
    let section = document.getElementById('pharosDriveHealth');
    if (!section) {
      section = node('section', 'pharos-section');
      section.id = 'pharosDriveHealth';
      cards.after(section);
    }
    const volume = drive.volume || {};
    const name = volume.name || status?.drive?.name || 'Library drive';
    const head = node('div', 'pharos-section-head'), intro = node('div');
    const title = node('h3', '', drive.scope === 'archive_root' ? `Archive drive · ${name}` : `Library drive · ${name}`);
    intro.append(title);
    const facts = [];
    if (volume.location) facts.push({external: 'external drive', 'disk-image': 'disk image', internal: "this Mac's internal disk"}[volume.location] || volume.location);
    if (volume.filesystem) facts.push(volume.filesystem.toUpperCase());
    if (drive.free_bytes != null) facts.push(`${size(drive.free_bytes)} free`);
    if (drive.sizes?.library_bytes) facts.push(`library ${size(drive.sizes.library_bytes)}`);
    if (drive.checked_at) facts.push(`checked ${ago(drive.checked_at)}`);
    intro.append(node('p', 'muted', facts.join(' · ') || drive.path || ''));
    const overall = node('span', 'pharos-chip' + (drive.status === 'warning' || drive.status === 'problem' ? ' warn' : ''),
      {ok: 'All checks pass', info: 'All checks pass', warning: 'Needs attention', problem: 'Needs attention', checking: 'Checking…', disconnected: 'Disconnected'}[drive.status] || drive.status || 'Unknown');
    head.append(intro, overall);
    section.replaceChildren(head);

    const list = node('div', 'pharos-checks');
    list.id = 'pharosDriveChecks';
    if (drive.status === 'checking') list.append(node('p', 'pharos-sub', 'Checking the drive (diskutil, Spotlight status, the library’s size)…'));
    for (const check of drive.checks || []) {
      const item = node('div', 'pharos-check');
      item.dataset.check = check.id;
      item.append(node('span', 'pharos-dot ' + (STATUS_DOT[check.status] ?? 'idle')));
      const titleLine = node('div', 'pharos-check-title', CHECK_TITLES[check.id] || check.id);
      if (check.status !== 'ok') titleLine.append(node('span', check.status === 'problem' ? 'pharos-error' : check.status === 'warning' ? 'pharos-warn' : 'pharos-sub', ` · ${check.status}`));
      item.append(titleLine, node('div', 'pharos-sub', check.summary));
      if (check.recommendation) item.append(node('div', '', check.recommendation));
      if (check.action) item.append(node('div', 'pharos-sub', check.action));
      if (check.command) { const line = codeLine(check.command); item.append(line); }
      list.append(item);
    }
    section.append(list);
    section.append(backupPanel(backup, name));
  }

  function backupPanel(backup, driveLabel) {
    const box = node('div', 'pharos-backup');
    box.id = 'pharosBackup';
    box.append(node('h3', '', 'Backups'));
    const last = backup.last_success;
    if (last) {
      const line = node('p', '');
      line.append(node('span', '', 'Last backup '), when(last.finished_at), node('span', '', ` to ${last.destination} · ${size(last.bytes_total)} (${size(last.bytes_copied)} copied)`));
      box.append(line);
    } else {
      box.append(node('p', 'pharos-warn', `No backup yet: ${driveLabel} holds the only copy of the library.`));
    }
    const run = backup.run;
    if (backup.active && run) {
      box.append(node('div', '', `Backing up to ${run.destination} · ${run.phase || 'starting'} · ${plural(run.files_copied, 'file')} copied, ${plural(run.files_unchanged, 'file')} unchanged · ${size(run.bytes_copied)} copied`), progressBar(null));
      box.append(button('Cancel backup', '', async () => { try { await post('/api/backup/cancel'); } catch (error) { toast(error.message); } refreshSettings(); }));
    } else if (run && run.completed_at) {
      const done = run.state === 'complete', partial = run.state === 'partial';
      const copied = `${plural(run.files_total, 'file')} (${size(run.bytes_total)}; ${size(run.bytes_copied)} copied) to ${run.destination} in ${Math.max(1, Math.round(run.duration_seconds))} s`;
      box.append(node('p', done ? 'pharos-ok' : partial ? 'pharos-warn' : 'pharos-error', done
        ? `Backed up ${copied}.`
        : partial
          ? `Backed up ${copied}, without ${(run.missing || []).join(', ') || 'parts of the library'}, which could not be read; the backup keeps its earlier copy of those.`
          : `The backup to ${run.destination} ${run.state === 'cancelled' ? 'was stopped' : 'failed'}; the next one carries on from what was copied.`));
      const problems = node('ul');
      (run.errors || []).slice(0, 5).forEach(error => problems.append(node('li', 'pharos-error', error.path ? `${error.path}: ${error.error}` : error.error)));
      (run.warnings || []).forEach(warning => problems.append(node('li', 'pharos-warn', warning)));
      if (problems.childElementCount) box.append(problems);
    }
    const form = node('form'), label = node('label', '', 'Back up to'), input = node('input'), go = node('button', 'pharos-button primary', 'Back up now');
    label.htmlFor = input.id = 'pharosBackupDestination';
    input.value = backupDraft ?? backup.suggested_destination ?? '';
    input.placeholder = '/Volumes/Other Drive/Pharos Backup';
    input.spellcheck = false;
    input.oninput = () => { backupDraft = input.value; };
    go.type = 'submit';
    go.disabled = backup.active || backupBusy;
    form.onsubmit = async event => {
      event.preventDefault();
      backupBusy = true;
      backupError = null;
      go.disabled = true;
      try {
        await post('/api/backup', {destination: input.value.trim()});
        backupDraft = null;
      } catch (error) {
        backupError = error.message;
      } finally {
        backupBusy = false;
        // Polls quickly again while the backup runs.
        refreshSettings();
      }
    };
    form.append(label, input, go);
    box.append(form);
    if (backupError) {
      const error = node('p', 'pharos-error', backupError);
      error.id = 'pharosBackupError';
      error.setAttribute('role', 'alert');
      box.append(error);
    }
    box.append(node('p', 'pharos-sub', 'Choose a folder on another drive, or on a Mac with FileVault on. The library’s own drive is refused: a backup there would be lost with it. Later backups copy only what changed.'));
    return box;
  }

  // ---- Settings visibility
  let settingsTimer = null, settingsRefresh = null, settingsAgain = false;
  const settingsVisible = () => document.getElementById('settings')?.classList.contains('active') && !document.hidden;
  // One refresh at a time; a request meanwhile runs once more after it.
  async function refreshSettings() {
    clearTimeout(settingsTimer);
    if (!settingsVisible()) return;
    if (settingsRefresh) { settingsAgain = true; return; }
    let busy = false;
    settingsRefresh = (async () => {
      do {
        settingsAgain = false;
        const results = await Promise.allSettled([loadHosts(), loadDriveHealth()]);
        busy = results.some(result => result.status === 'fulfilled' && result.value);
      } while (settingsAgain && settingsVisible());
    })();
    await settingsRefresh;
    settingsRefresh = null;
    clearTimeout(settingsTimer);
    // Captures are listed from their manifests on the drive; poll gently.
    if (settingsVisible()) settingsTimer = setTimeout(refreshSettings, busy ? 2500 : 20000);
  }

  function watchSettings() {
    const settings = document.getElementById('settings');
    if (settings) new MutationObserver(() => { if (settingsVisible()) refreshSettings(); }).observe(settings, {attributes: true, attributeFilter: ['class']});
    // Sources rebuilds its cards; mark each with its capture again.
    const grid = document.getElementById('sourceGrid');
    if (grid) new MutationObserver(annotateCards).observe(grid, {childList: true});
    document.addEventListener('visibilitychange', () => { if (settingsVisible()) refreshSettings(); });
    if (settingsVisible()) refreshSettings();
  }

  window.pharosLibrary = {refresh: refreshStatus, status: () => status, runs, refreshSettings, onStatus: listener => listeners.add(listener)};
  const sheet = node('style');
  sheet.textContent = CSS;
  document.head.append(sheet);
  watchSettings();
  refreshStatus();
})();
