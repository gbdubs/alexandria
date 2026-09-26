// Pharos onboarding for portable libraries. When the library is opened on a
// Mac with no hosts/<id>.toml yet, it lists the conversation sources the
// service found in the usual places (GET /api/probe) and saves the user's
// choice for this Mac (POST /api/probe/accept). It then captures the added
// sources onto the library (POST /api/capture), which is quick, so the drive
// can be ejected, and offers to index them now (POST /api/index); indexing can
// also run later, on any Mac. Settings → Sources keeps the source discovery
// action with Capture and Index.
(() => {
  'use strict';
  const DISMISSED = 'pharos-onboarding-dismissed:';
  const KINDS = {claude: 'Claude Code', codex: 'Codex', conductor: 'Conductor', tl1: 'TL1', chatgpt: 'ChatGPT'};
  const CSS = `
.pharos-onboard-backdrop{position:fixed;inset:0;z-index:9000;display:grid;place-items:center;padding:24px;background:color-mix(in srgb,#000 42%,transparent)}
.pharos-onboard{width:min(780px,100%);max-height:calc(100vh - 48px);display:flex;flex-direction:column;background:var(--panel,#fff);color:var(--ink,#222);border:1px solid var(--line,#ccc);border-radius:16px;box-shadow:0 24px 70px #0006;overflow:hidden}
.pharos-onboard>h2{margin:0;padding:22px 24px 6px;font:700 24px/1.2 var(--serif,serif);letter-spacing:-.02em}
.pharos-onboard-intro{margin:0;padding:0 24px 16px;color:var(--muted,#666);font-size:14px;border-bottom:1px solid var(--line,#ccc)}
.pharos-onboard-body{overflow:auto;padding:4px 24px}
.pharos-onboard-body>p{margin:18px 0}
.pharos-candidate{display:grid;grid-template-columns:22px 1fr;gap:3px 10px;padding:14px 0;border-bottom:1px solid var(--line,#ccc)}
.pharos-candidate:last-child{border-bottom:0}
.pharos-candidate input[type=checkbox]{margin:4px 0 0;width:17px;height:17px;accent-color:var(--accent,#315845)}
.pharos-candidate-head{display:flex;flex-wrap:wrap;align-items:baseline;gap:4px 10px}
.pharos-candidate-head label{font-weight:650;font-size:16px;cursor:pointer}
.pharos-candidate-head code{font:12px ui-monospace,SFMono-Regular,monospace;color:var(--muted,#666);overflow-wrap:anywhere}
.pharos-candidate>div:not(.pharos-candidate-head){grid-column:2;font-size:13px}
.pharos-candidate .pharos-muted{color:var(--muted,#666)}
.pharos-candidate .pharos-warn{color:var(--warn,#98601d)}
.pharos-candidate-name{display:flex;align-items:center;gap:8px;color:var(--muted,#666)}
.pharos-candidate-name input{width:190px;padding:3px 8px;border:1px solid var(--line,#ccc);border-radius:7px;background:var(--bg,#fff);color:var(--ink,#222);font:12px ui-monospace,SFMono-Regular,monospace}
.pharos-chip{border:1px solid var(--line,#ccc);border-radius:20px;padding:1px 8px;font-size:12px;color:var(--muted,#666)}
.pharos-onboard-actions{display:flex;justify-content:flex-end;align-items:center;gap:10px;padding:14px 24px;border-top:1px solid var(--line,#ccc)}
.pharos-error{color:var(--bad,#9c3d36)}.pharos-onboard-actions .pharos-error{margin-right:auto;font-size:13px}
.pharos-onboard-actions button{border:1px solid var(--line,#ccc);background:var(--panel,#fff);color:var(--ink,#222)}
.pharos-onboard-actions button.primary{background:var(--accent,#315845);border-color:var(--accent,#315845);color:var(--panel,#fff)}
.pharos-onboard-actions button:disabled{opacity:.55;cursor:default}
.pharos-steps{margin:14px 0 18px;border-top:1px solid var(--line,#ccc)}
.pharos-step{display:grid;grid-template-columns:minmax(110px,1fr) 3fr;gap:4px 14px;padding:10px 0;border-bottom:1px solid var(--line,#ccc);font-size:13px}
.pharos-step strong{font-size:14px}
.pharos-step .pharos-meter{grid-column:2;height:6px;overflow:hidden;border-radius:6px;background:var(--line,#ccc)}
.pharos-step .pharos-meter>span{display:block;height:100%;min-width:6px;background:var(--accent,#315845);transition:width .25s}
.pharos-onboard-body .pharos-lead{font-size:16px;margin:18px 0 6px}`;

  const node = (tag, cls, text) => {
    const n = document.createElement(tag);
    if (cls) n.className = cls;
    if (text !== undefined) n.textContent = text;
    return n;
  };
  const call = async (path, options = {}) => {
    const response = await fetch(path, {...options, headers: {'Content-Type': 'application/json'}});
    const body = await response.json().catch(() => ({}));
    if (!response.ok) throw Object.assign(new Error(body.error || response.statusText), {status: response.status});
    return body;
  };
  const toast = message => { if (typeof window.feedbackToast === 'function') window.feedbackToast(message); };
  const plural = (count, unit) => `${Number(count).toLocaleString()} ${unit}${count === 1 ? '' : 's'}`;
  const size = value => {
    const units = ['B', 'KB', 'MB', 'GB', 'TB'];
    let amount = Number(value) || 0, power = 0;
    while (amount >= 1000 && power < units.length - 1) { amount /= 1000; power++; }
    return `${amount.toFixed(amount < 10 && power ? 1 : 0)} ${units[power]}`;
  };
  const day = value => new Date(value).toLocaleDateString(undefined, {year: 'numeric', month: 'short', day: 'numeric'});
  const hostLabel = status => status.host?.label || 'this Mac';

  function facts(candidate) {
    const parts = [];
    if (candidate.unit && candidate.status !== 'protected') parts.push((candidate.truncated ? 'at least ' : '') + plural(candidate.count, candidate.unit));
    if (candidate.bytes) parts.push(size(candidate.bytes));
    if (candidate.oldest && candidate.newest) parts.push(`${day(candidate.oldest)} – ${day(candidate.newest)}`);
    else if (candidate.newest) parts.push(`updated ${day(candidate.newest)}`);
    return parts.join(' · ');
  }

  function candidateRow(candidate, index, choices) {
    const row = node('div', 'pharos-candidate'), head = node('div', 'pharos-candidate-head');
    const id = `pharosCandidate${index}`, configured = candidate.configured;
    const title = node('label', '', KINDS[candidate.kind] || candidate.kind);
    title.htmlFor = id;
    head.append(title);
    if (candidate.display_path) head.append(node('code', '', candidate.display_path));
    if (configured) head.append(node('span', 'pharos-chip', `Added as ${configured.name}${configured.enabled ? '' : ' · paused'}`));
    else if (candidate.status !== 'found' && candidate.status !== 'manual') head.append(node('span', 'pharos-chip', candidate.status));
    if (candidate.acceptable) {
      const box = node('input');
      box.type = 'checkbox';
      box.id = id;
      box.checked = configured ? configured.enabled : candidate.recommended;
      // A paused source in this Mac's file can be switched on from here.
      box.disabled = Boolean(configured) && (configured.enabled || configured.scope !== 'host');
      row.append(box, head);
      const choice = {candidate, box};
      choices.push(choice);
      if (!configured) {
        const name = node('div', 'pharos-candidate-name'), input = node('input');
        input.value = candidate.name;
        input.setAttribute('aria-label', `Source name for ${candidate.display_path}`);
        input.spellcheck = false;
        choice.input = input;
        name.append(node('span', '', 'Source name'), input);
        choice.nameRow = name;
      }
    } else {
      row.append(node('span'), head);
    }
    const summary = facts(candidate);
    if (summary) row.append(node('div', 'pharos-muted', summary));
    if (candidate.retention) row.append(node('div', candidate.retention_days < 90 ? 'pharos-warn' : 'pharos-muted', candidate.retention));
    if (candidate.overlap?.summary) row.append(node('div', 'pharos-muted', candidate.overlap.summary));
    if (candidate.detail) row.append(node('div', 'pharos-muted', candidate.detail));
    if (candidate.found_by && candidate.found_by !== 'default location') row.append(node('div', 'pharos-muted', `Found by ${candidate.found_by}`));
    const choice = choices.find(item => item.candidate === candidate);
    if (choice?.nameRow) row.append(choice.nameRow);
    return row;
  }

  function close() { document.getElementById('pharosOnboarding')?.remove(); }

  function openPanel(status, onboarding) {
    close();
    const label = hostLabel(status);
    const backdrop = node('div', 'pharos-onboard-backdrop'), dialog = node('section', 'pharos-onboard');
    backdrop.id = 'pharosOnboarding';
    dialog.setAttribute('role', 'dialog');
    dialog.setAttribute('aria-modal', 'true');
    dialog.setAttribute('aria-labelledby', 'pharosOnboardTitle');
    const heading = onboarding ? (status.known_host ? `Set up sources for ${label}` : `New Mac detected: ${label}`) : `Conversation sources on ${label}`;
    const title = node('h2', '', heading);
    title.id = 'pharosOnboardTitle';
    const intro = node('p', 'pharos-onboard-intro', (onboarding ? `This library has not been set up on ${label} yet. ` : '') +
      'Pharos looked only in the usual places where Claude Code, Codex, Conductor, and TL1 keep conversations; it does not search the rest of your home folder, and nothing is indexed until you add it. ' +
      `Your choice is saved in ${status.host_file_display} inside the library and applies only to this Mac.`);
    const body = node('div', 'pharos-onboard-body'), actions = node('div', 'pharos-onboard-actions');
    body.append(node('p', 'pharos-muted', 'Looking for conversations on this Mac…'));
    const later = node('button', '', onboarding ? 'Not now' : 'Close');
    later.type = 'button';
    later.onclick = () => {
      if (onboarding) sessionStorage.setItem(DISMISSED + status.host?.id, '1');
      close();
    };
    actions.append(later);
    dialog.append(title, intro, body, actions);
    backdrop.append(dialog);
    backdrop.addEventListener('keydown', event => { if (event.key === 'Escape') later.click(); });
    document.body.append(backdrop);
    later.focus();
    call('/api/probe').then(report => renderReport(report, onboarding, body, actions, later))
      .catch(error => body.replaceChildren(node('p', 'pharos-error', `Could not look for sources: ${error.message}`)));
  }

  function renderReport(report, onboarding, body, actions, later) {
    const choices = [], list = node('div');
    report.candidates.forEach((candidate, index) => list.append(candidateRow(candidate, index, choices)));
    const found = report.candidates.some(candidate => candidate.acceptable && candidate.status === 'found');
    body.replaceChildren();
    if (!found) body.append(node('p', 'pharos-muted', 'No conversations were found in the usual places on this Mac. You can add a source by hand to the file named above.'));
    body.append(list);
    const error = node('span', 'pharos-error'), add = node('button', 'primary');
    add.type = 'button';
    const selected = () => choices.filter(choice => choice.box.checked && !choice.box.disabled);
    const update = () => {
      const count = selected().length;
      add.textContent = count ? `Add ${plural(count, 'source')}` : onboarding ? 'Continue without adding' : 'Add sources';
      add.disabled = !count && !onboarding;
    };
    choices.forEach(choice => choice.box.addEventListener('change', update));
    update();
    add.onclick = async () => {
      const accept = [], decline = [], names = {};
      for (const choice of choices) {
        if (choice.box.disabled) continue;
        const name = choice.candidate.name;
        if (choice.box.checked) {
          accept.push(name);
          const custom = choice.input?.value.trim();
          if (custom && custom !== name) names[name] = custom;
        } else if (onboarding && !choice.candidate.configured && choice.candidate.status === 'found') {
          // Remembered as paused sources, so this Mac is not asked again.
          decline.push(name);
        }
      }
      add.disabled = later.disabled = true;
      error.textContent = '';
      try {
        const result = await call('/api/probe/accept', {method: 'POST', body: JSON.stringify({accept, decline, names})});
        renderResult(result, body, actions, report);
        window.loadSources?.();
        refreshNote();
      } catch (failure) {
        error.textContent = failure.message;
        add.disabled = later.disabled = false;
      }
    };
    actions.replaceChildren(error, later, add);
  }

  function renderResult(result, body, actions, status) {
    const added = [...result.added, ...result.enabled];
    body.parentElement.querySelector('.pharos-onboard-intro')?.remove();
    body.replaceChildren();
    body.append(node('p', '', added.length ? `Added ${added.join(', ')} for this Mac.` : 'Saved. No sources were added.'));
    if (result.declined.length) body.append(node('p', 'pharos-muted', `Kept as paused sources: ${result.declined.join(', ')}. Turn them on in Settings → Sources whenever you like.`));
    result.skipped.forEach(note => body.append(node('p', 'pharos-muted', note)));
    const done = node('button', 'primary', 'Done');
    done.type = 'button';
    done.onclick = close;
    actions.replaceChildren(done);
    if (!added.length) return done.focus();
    // Copying the raw files is quick; once it is done the drive can go, and
    // indexing can follow now or later on any Mac.
    captureSources(added, status, body, actions);
  }

  const setTitle = (body, text) => { const title = body.parentElement?.querySelector('#pharosOnboardTitle'); if (title) title.textContent = text; };
  const button = (text, primary, onclick) => {
    const b = node('button', primary ? 'primary' : '', text);
    b.type = 'button';
    b.onclick = onclick;
    return b;
  };
  const meter = fraction => {
    const bar = node('div', 'pharos-meter'), fill = node('span');
    fill.style.width = `${Math.round(Math.min(Math.max(fraction, 0), 1) * 100)}%`;
    bar.append(fill);
    return bar;
  };
  const libraryRuns = () => window.pharosLibrary?.runs;
  async function driveOf() {
    try { return (await call('/api/library/status')).drive; } catch { return null; }
  }

  function captureRows(run) {
    const list = node('div', 'pharos-steps');
    for (const result of run?.results || []) {
      const row = node('div', 'pharos-step'), bytes = (result.bytes_copied || 0) + (result.snapshot_bytes || 0);
      const state = {pending: 'Waiting', running: 'Copying', complete: 'Captured', failed: 'Failed'}[result.state] || result.state;
      const facts = [state];
      if (result.files_copied || result.files_unchanged) facts.push(`${plural(result.files_copied || 0, 'file')} copied${result.files_unchanged ? `, ${plural(result.files_unchanged, 'file')} unchanged` : ''}`);
      if (result.snapshots_taken) facts.push(plural(result.snapshots_taken, 'database snapshot'));
      if (bytes) facts.push(size(bytes));
      row.append(node('strong', '', result.name), node('span', result.state === 'failed' ? 'pharos-error' : 'pharos-muted', facts.join(' · ')));
      if (result.state === 'running' && result.bytes_planned) row.append(meter(result.bytes_copied / result.bytes_planned));
      (result.errors || []).slice(0, 3).forEach(error => row.append(node('span', 'pharos-error', error.path ? `${error.path}: ${error.error}` : error.error)));
      list.append(row);
    }
    return list;
  }

  async function captureSources(sources, status, body, actions) {
    const label = hostLabel(status), runs = libraryRuns();
    const shown = () => document.body.contains(body);
    setTitle(body, `Capturing from ${label}`);
    const lead = node('p', 'pharos-lead', `Copying the conversation files of ${sources.join(', ')} into the library…`);
    const note = node('p', 'pharos-muted', 'This copies raw files and takes seconds to minutes. Nothing is parsed yet. You can close this; the capture carries on and the header shows its progress.');
    const rows = node('div');
    body.replaceChildren(lead, rows, note);
    actions.replaceChildren(button('Run in background', false, close));
    let run;
    try {
      if (!runs) throw new Error('Pharos could not load its library controls; reload the page.');
      run = await runs.capture(sources, update => { if (update && shown()) rows.replaceChildren(captureRows(update)); });
    } catch (failure) {
      if (!shown()) return toast(failure.message);
      setTitle(body, `Sources added for ${label}`);
      body.replaceChildren(node('p', 'pharos-error', failure.status === 409 ? 'A capture is already running on this Mac. Capture these sources from Settings → Sources once it finishes.' : `The capture did not start: ${failure.message}`));
      actions.replaceChildren(button('Close', true, close));
      return;
    }
    const drive = await driveOf();
    const bytes = run?.bytes_copied || 0;
    const incremental = (run?.results || []).some(item => item.files_unchanged || item.snapshots_unchanged);
    const headline = run?.state === 'complete'
      ? (incremental ? `Captured ${size(bytes)} of new and changed files from ${label}; the rest was already in the library.` : `Captured ${size(bytes)} from ${label}.`)
      : `The capture from ${label} finished with errors.`;
    if (!shown()) {
      toast(run?.state === 'complete' ? `${headline} Index it in Settings → Sources.` : headline);
      return;
    }
    setTitle(body, run?.state === 'complete' ? `Captured from ${label}` : `Capture from ${label} incomplete`);
    const next = drive?.ejectable
      ? `You can eject ${drive.name} now; indexing can finish now or later on any Mac.`
      : 'Indexing makes these conversations searchable; it can run now or later.';
    body.replaceChildren(node('p', 'pharos-lead', headline), captureRows(run), node('p', '', run?.state === 'complete' ? next : `What was captured is kept; capture again from Settings → Sources. ${next}`));
    const index = button('Index now', true, () => indexSources(sources, status, body, actions));
    actions.replaceChildren(button('Later', false, close), index);
    index.focus();
  }

  function indexRows(run) {
    const list = node('div', 'pharos-steps');
    for (const source of run?.sources || []) {
      const row = node('div', 'pharos-step'), done = (run.results || []).find(item => `${item.host}/${item.source}` === source || item.source === source.split('/').pop());
      const name = source.split('/').pop();
      let text = 'Waiting', cls = 'pharos-muted';
      if (done) {
        text = done.error ? done.error : `${plural(done.conversations, 'conversation')} and ${plural(done.messages, 'message')} written${done.unchanged ? `, ${plural(done.unchanged, 'part')} unchanged` : ''}`;
        cls = done.error ? 'pharos-error' : 'pharos-muted';
      } else if (run.current_source === source) {
        text = `Indexing · ${run.phase} · ${plural(run.conversations, 'conversation')} so far`;
      }
      row.append(node('strong', '', name), node('span', cls, text));
      list.append(row);
    }
    return list;
  }

  async function indexSources(sources, status, body, actions) {
    const label = hostLabel(status), runs = libraryRuns();
    const shown = () => document.body.contains(body);
    setTitle(body, `Indexing ${label}`);
    const rows = node('div');
    body.replaceChildren(node('p', 'pharos-lead', `Indexing what was captured from ${label}…`), rows,
      node('p', 'pharos-muted', 'Indexing parses the captured conversations into the library, so it can take a while the first time. It runs in the background: close this whenever you like. Ejecting the drive stops it safely, and the next index picks up where it stopped.'));
    actions.replaceChildren(button('Run in background', false, close));
    let run;
    try {
      run = await runs.index({sources, only_needed: true}, update => { if (update && shown()) rows.replaceChildren(indexRows(update)); });
    } catch (failure) {
      if (!shown()) return toast(failure.message);
      body.replaceChildren(node('p', 'pharos-error', failure.status === 409 ? 'Source indexing is already running. Index these captures from Settings → Sources once it finishes.' : `Indexing did not start: ${failure.message}`));
      actions.replaceChildren(button('Close', true, close));
      return;
    }
    window.loadSources?.();
    const alreadyCurrent = (run?.results || []).filter(result => result.skipped_unchanged).length;
    const parsedParts = (run?.results || []).reduce((count, result) => count + (result.parsed || 0), 0);
    const skippedParts = (run?.results || []).reduce((count, result) => count + (result.unchanged || 0), 0);
    const summary = run?.state === 'complete'
      ? !run.conversations && !parsedParts && !skippedParts && (alreadyCurrent || !run.results?.length) ? `${label} is already up to date; no conversations rewritten.`
        : `Indexed ${plural(run.conversations, 'conversation')} (${plural(run.messages, 'message')}) from ${label}.${parsedParts ? ` ${plural(parsedParts, 'source part')} parsed.` : ''}${skippedParts ? ` ${skippedParts.toLocaleString()} unchanged files or sessions skipped.` : ''}`
      : run?.state === 'interrupted' ? `Indexing ${label} was interrupted; the next index resumes it.` : `Indexing ${label} finished with errors.`;
    if (!shown()) return toast(summary);
    setTitle(body, run?.state === 'complete' ? `${label} is in the library` : `Indexing ${label}`);
    body.replaceChildren(node('p', 'pharos-lead', summary), indexRows(run));
    if (run?.error && run.state !== 'complete') body.append(node('p', 'pharos-error', run.error));
    const done = button('Done', true, close);
    actions.replaceChildren(done);
    done.focus();
  }

  async function refreshNote() {
    const slot = document.getElementById('sourceSetupAction');
    let status;
    try { status = await call('/api/probe/status'); } catch { return null; }
    if (!slot) return status;
    if (!status.library) { slot.replaceChildren(); return status; }
    const find = node('button', '', 'Find sources on this Mac…');
    find.type = 'button';
    find.id = 'pharosFindSources';
    find.title = `Look for conversation sources on ${hostLabel(status)}. Configuration is saved in ${status.host_file_display}.`;
    find.onclick = () => openPanel(status, !status.host_file_exists);
    slot.replaceChildren(find);
    return status;
  }

  const sheet = node('style');
  sheet.textContent = CSS;
  document.head.append(sheet);
  refreshNote().then(status => {
    if (status?.needs_onboarding && !sessionStorage.getItem(DISMISSED + status.host?.id)) openPanel(status, true);
  });
})();
