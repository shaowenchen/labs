package api

import (
	"net/http"
)

// console is the landing page: a tab per kind of lab, each carrying the button
// that asks for one and the environments that kind can hand out. It is a static
// document carrying nothing — everything it shows is fetched from the API,
// which is where the limits are — so it is safe to serve to anyone.
//
// It is embedded as a string rather than a file so the binary is the whole
// product, and there is no build step and no static directory to keep in step
// with a deployment.
func (s *Server) console(w http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/" {
		// A path that is not a route and not the root is a real 404, not the
		// landing page, so a mistyped API path does not look like a success.
		writeJSON(w, http.StatusNotFound, errorBody{Error: "no such page: " + r.URL.Path})
		return
	}
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.WriteHeader(http.StatusOK)
	_, _ = w.Write([]byte(consoleHTML))
}

const consoleHTML = `<!doctype html>
<html lang="en">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width, initial-scale=1">
<title>labs</title>
<style>
  :root {
    color-scheme: light dark;
    --bg: #f7f6f3;
    --surface: #ffffff;
    --fg: #17171a;
    --muted: #6c6c74;
    --line: #e8e5df;
    --line-soft: #f0ede8;
    --accent: #4f46e5;
    --accent-soft: #eef0fe;
    --accent-fg: #ffffff;
    --ok: #15803d;
    --ok-soft: #e7f5ec;
    --warn: #b45309;
    --warn-soft: #fdf3e3;
    --err: #b91c1c;
    --err-soft: #fdeceb;
    --radius: 14px;
    --radius-sm: 9px;
    --shadow: 0 1px 2px rgba(20,20,30,.04), 0 6px 20px rgba(20,20,30,.05);
    --mono: ui-monospace, SFMono-Regular, "SF Mono", Menlo, Consolas, monospace;
  }
  @media (prefers-color-scheme: dark) {
    :root {
      --bg: #0f0f11;
      --surface: #17171a;
      --fg: #ececef;
      --muted: #9a9aa3;
      --line: #2a2a30;
      --line-soft: #212127;
      --accent: #8b86ff;
      --accent-soft: #1e1c33;
      --accent-fg: #12121a;
      --ok: #6ee7a0;
      --ok-soft: #12241a;
      --warn: #fbbf68;
      --warn-soft: #2a1f10;
      --err: #fca5a5;
      --err-soft: #2c1414;
      --shadow: 0 1px 2px rgba(0,0,0,.3), 0 6px 20px rgba(0,0,0,.25);
    }
  }
  * { box-sizing: border-box; }
  body {
    margin: 0;
    font: 16px/1.55 ui-sans-serif, system-ui, -apple-system, "Segoe UI", sans-serif;
    color: var(--fg);
    background: var(--bg);
    -webkit-font-smoothing: antialiased;
  }
  main { max-width: 720px; margin: 0 auto; padding: 64px 20px 80px; }

  header { margin-bottom: 36px; }
  h1 { font-size: 1.9rem; line-height: 1.1; letter-spacing: -.02em; margin: 0 0 8px; }
  h1 .mark {
    display: inline-block; width: 10px; height: 10px; border-radius: 3px;
    background: var(--accent); margin-right: 12px; vertical-align: middle;
    transform: translateY(-2px);
  }
  p.lead { color: var(--muted); margin: 0; font-size: 1.02rem; }

  h2 {
    font-size: .74rem; text-transform: uppercase; letter-spacing: .09em;
    color: var(--muted); font-weight: 600; margin: 34px 0 12px;
  }

  /* ── buttons ────────────────────────────────────────────────────────── */
  button {
    font: inherit; font-weight: 550;
    padding: 11px 20px;
    border: 1px solid transparent;
    background: var(--accent);
    color: var(--accent-fg);
    border-radius: var(--radius-sm);
    cursor: pointer;
    transition: transform .06s ease, filter .12s ease, background .12s ease;
  }
  button:hover:not(:disabled) { filter: brightness(1.08); }
  button:active:not(:disabled) { transform: translateY(1px); }
  button:disabled { opacity: .45; cursor: default; }
  button:focus-visible { outline: 2px solid var(--accent); outline-offset: 2px; }
  /* The one action a panel offers. It is a real button rather than a link so it
     keeps the pressed and disabled states that a request needs — a create that
     is in flight says so on the button itself. */
  .kind-head button { padding: 9px 18px; font-size: .93rem; white-space: nowrap; }
  button.small {
    padding: 4px 11px; font-size: .8rem; font-weight: 500;
    background: transparent; color: var(--muted);
    border-color: var(--line);
  }
  button.small:hover:not(:disabled) { background: var(--line-soft); filter: none; }

  /* ── cards ──────────────────────────────────────────────────────────── */
  #status:not(:empty) {
    background: var(--surface);
    border: 1px solid var(--line);
    border-radius: var(--radius);
    box-shadow: var(--shadow);
    overflow: hidden;
  }
  #status:not(:empty) { padding: 18px 20px; }
  /* A request's error, which sits in its kind's section rather than in a place
     of its own: it is about this kind, and it belongs under it. */
  .kind .row.err { text-align: left; }

  .row {
    display: grid; grid-template-columns: 6.5rem 1fr; gap: 14px;
    align-items: baseline; padding: 11px 0; border-top: 1px solid var(--line-soft);
  }
  .row:first-child { border-top: 0; padding-top: 0; }
  .row:last-child { padding-bottom: 0; }
  .row .k { color: var(--muted); font-size: .86rem; }
  .row.err { color: var(--err); }

  /* ── one tab per kind ───────────────────────────────────────────────── */
  /* The strip is the card's top edge: the tabs sit on the border itself, the
     active one painted in the surface colour and without its bottom edge, so
     it reads as part of the panel below it. The strip scrolls sideways rather
     than wrapping, which keeps every tab on one line.
     The card is drawn only once a kind exists — "has" is set from the script
     that draws them, because a container holding an empty strip is not
     :empty and would otherwise leave a bare outline on the page. */
  #kinds { display: none; }
  #kinds.has {
    display: block;
    margin-top: 22px;
    background: var(--surface);
    border: 1px solid var(--line);
    border-radius: var(--radius);
    box-shadow: var(--shadow);
    overflow: hidden;
  }
  /* The strip is the card's top edge. The tabs are plain labels on the card
     itself rather than boxes: the selected one is marked by the accent
     underline, which reads at a glance without drawing a second border inside
     the card. The strip scrolls sideways rather than wrapping, so a deployment
     with several kinds keeps every tab on one line. */
  #tabs {
    display: flex; gap: 24px;
    padding: 2px 20px 0;
    border-bottom: 1px solid var(--line);
    background: var(--surface);
    overflow-x: auto;
    scrollbar-width: none;
  }
  #tabs::-webkit-scrollbar { display: none; }
  #tabs:empty { display: none; }
  .tab {
    font: inherit; font-size: .93rem; font-weight: 550;
    display: flex; align-items: center; gap: 8px;
    padding: 13px 1px 11px;
    border: 0; border-bottom: 2px solid transparent;
    background: none; color: var(--muted); cursor: pointer;
    white-space: nowrap; margin-bottom: -1px;
    transition: color .12s ease, border-color .12s ease;
  }
  .tab:hover { color: var(--fg); }
  .tab[aria-selected="true"] { color: var(--fg); border-bottom-color: var(--accent); }
  .tab:focus-visible { outline: 2px solid var(--accent); outline-offset: -3px; border-radius: 5px; }
  .tick { width: 8px; height: 8px; border-radius: 50%; flex: none; background: var(--muted); }
  .tick.ok { background: var(--ok); }
  .tick.warn { background: var(--warn); }
  .tick.err { background: var(--err); }
  /* One panel is on screen at a time, so its contents need no box of their
     own — the card around the strip is the only border. */
  .kind { display: none; padding: 16px 20px 18px; }
  .kind.on { display: block; }
  /* Status on the left, the one action on the right: the panel says what state
     its kind is in and offers what can be done about it, on one line. */
  .kind-head {
    display: flex; align-items: center; justify-content: space-between;
    gap: 12px; flex-wrap: wrap; min-height: 40px;
  }
  .kind .card { background: none; border: 0; box-shadow: none; border-radius: 0; overflow: visible; }
  .kind .card:empty { display: none; }
  .kind h3 {
    font-size: .74rem; text-transform: uppercase; letter-spacing: .09em;
    color: var(--muted); font-weight: 600; margin: 18px 0 10px;
  }

  .env { padding: 11px 0; border-top: 1px solid var(--line-soft); }
  .env:first-child { border-top: 0; padding-top: 0; }
  .env .top { display: flex; align-items: center; gap: 9px; flex-wrap: wrap; }
  .env .name { font-weight: 600; }
  .env .meta { color: var(--muted); font-size: .85rem; }
  .env .msg {
    color: var(--muted); font-size: .86rem; margin-top: 7px;
    overflow-wrap: anywhere;
  }
  .env .msg a { color: var(--muted); }

  .dot { width: 8px; height: 8px; border-radius: 50%; flex: none; }
  .dot.ok { background: var(--ok); box-shadow: 0 0 0 3px var(--ok-soft); }
  .dot.warn { background: var(--warn); box-shadow: 0 0 0 3px var(--warn-soft); }
  .dot.err { background: var(--err); box-shadow: 0 0 0 3px var(--err-soft); }

  .ok { color: var(--ok); }
  .warn { color: var(--warn); }
  .err { color: var(--err); }

  /* A state, as a word with a dot before it rather than a pill: the pill drew a
     box around a single word, which on a page this quiet read as a button. */
  .tail {
    display: inline-flex; align-items: center; gap: 7px;
    font-size: .87rem; color: var(--muted);
  }
  .tail::before {
    content: ''; width: 8px; height: 8px; border-radius: 50%;
    background: currentColor; flex: none;
  }
  .tail.ok { color: var(--ok); }
  .tail.warn { color: var(--warn); }
  .tail.err { color: var(--err); }

  code {
    font: .84rem/1.45 var(--mono);
    background: var(--line-soft);
    padding: 2px 7px; border-radius: 6px;
    overflow-wrap: anywhere;
  }
  a { color: var(--accent); text-decoration: none; }
  a:hover { text-decoration: underline; }

  /* ── the hand-off, shown once, as a dialog ──────────────────────────── */
  dialog.key-modal {
    border: 1px solid var(--line);
    border-radius: var(--radius);
    background: var(--surface);
    color: var(--fg);
    padding: 0;
    width: min(560px, calc(100vw - 32px));
    box-shadow: 0 24px 60px rgba(10,10,20,.28);
  }
  dialog.key-modal::backdrop { background: rgba(12,12,18,.5); backdrop-filter: blur(2px); }
  .key-modal .head {
    padding: 22px 24px 18px;
    border-bottom: 1px solid var(--line-soft);
  }
  .key-modal .head .eyebrow {
    font-size: .72rem; font-weight: 600; letter-spacing: .09em;
    text-transform: uppercase; color: var(--ok);
  }
  .key-modal .head h3 { margin: 6px 0 0; font-size: 1.2rem; letter-spacing: -.01em; }
  .key-modal .body { padding: 8px 24px 20px; }
  .key-modal .body:empty { display: none; }
  .key-modal .row { grid-template-columns: 5.5rem 1fr; }
  .key-modal .warnbox {
    margin: 14px 24px 0; padding: 11px 13px; font-size: .85rem;
    border: 1px solid var(--warn); border-radius: var(--radius-sm);
    background: var(--warn-soft); color: var(--warn);
  }
  .key-modal .once {
    margin: 14px 24px 0; padding: 11px 13px; font-size: .85rem;
    border: 1px solid var(--line); border-radius: var(--radius-sm);
    background: var(--line-soft); color: var(--muted);
  }
  .key-modal .foot {
    display: flex; gap: 10px; justify-content: flex-end;
    padding: 16px 24px 20px;
  }
  /* The ask, before anything is created: a kind that offers a choice is asked
     which one here rather than in the page, so the panel shows only the lab. */
  .key-modal .field { padding: 14px 24px 0; }
  .key-modal .field label {
    display: block; font-size: .74rem; font-weight: 600;
    letter-spacing: .09em; text-transform: uppercase; color: var(--muted);
    margin-bottom: 7px;
  }
  .key-modal select { width: 100%; }
  .key-modal .hint { padding: 9px 24px 0; font-size: .84rem; color: var(--muted); }
  .key-modal .hint:empty { display: none; }
  .key-modal .keyline { display: flex; align-items: center; gap: 8px; flex-wrap: wrap; }
  .key-modal .keyline code { font-size: .9rem; padding: 5px 10px; }
  body.modal-open { overflow: hidden; }

  .banner {
    padding: 16px 18px; border: 1px solid var(--warn);
    border-radius: var(--radius); background: var(--warn-soft);
    margin-top: 18px;
  }
  .banner h2 {
    font-size: .95rem; text-transform: none; letter-spacing: 0;
    color: var(--fg); margin: 0 0 8px;
  }
  .banner ul { margin: 0; padding-left: 1.15rem; }
  .banner li { margin: 3px 0; font-size: .9rem; }

  footer {
    margin-top: 48px; padding-top: 16px; border-top: 1px solid var(--line);
    color: var(--muted); font: .74rem/1.6 var(--mono);
  }
  @media (max-width: 480px) {
    main { padding: 40px 16px 64px; }
    h1 { font-size: 1.6rem; }
    .row { grid-template-columns: 1fr; gap: 2px; }
    .row .k { font-size: .78rem; }
  }
</style>
</head>
<body>
<main>
  <header>
    <h1><span class="mark"></span>labs</h1>
    <p class="lead">Get a working environment for a couple of hours.</p>
  </header>
  <div id="status"></div>
  <div id="kinds"><div id="tabs" role="tablist" aria-label="kind of lab"></div></div>
  <footer id="build"></footer>
</main>
<dialog id="key-modal" class="key-modal">
  <div class="head">
    <div class="eyebrow" id="km-eyebrow">Ready</div>
    <h3 id="km-title">Your lab is ready</h3>
  </div>
  <div class="body" id="km-body"></div>
  <div class="field" id="km-field" hidden>
    <label for="km-template">template</label>
    <select id="km-template"></select>
  </div>
  <div class="hint" id="km-hint"></div>
  <div id="km-error"></div>
  <div class="foot">
    <button id="km-close" class="small">Cancel</button>
    <button id="km-confirm">Create</button>
  </div>
</dialog>
<script>
const status = document.getElementById('status');
const kindsBox = document.getElementById('kinds');
const tabsBox = document.getElementById('tabs');
const build = document.getElementById('build');
const km = document.getElementById('key-modal');
const kmTitle = document.getElementById('km-title');
const kmBody = document.getElementById('km-body');
const kmConfirm = document.getElementById('km-confirm');
const kmClose = document.getElementById('km-close');
const kmEyebrow = document.getElementById('km-eyebrow');
const kmField = document.getElementById('km-field');
const kmTemplate = document.getElementById('km-template');
const kmHint = document.getElementById('km-hint');
const kmError = document.getElementById('km-error');
// sections is one entry per kind, holding the elements its tab is drawn into.
// Keeping them across polls is what lets a button keep its disabled state while
// the page refreshes every five seconds.
const sections = new Map();
// activeKind is the tab on screen. It is a single name rather than a per-section
// flag because exactly one panel is shown; a flag per section could hold two.
let activeKind = '';
function row(k, v, cls) {
  const d = document.createElement('div'); d.className = 'row' + (cls ? ' ' + cls : '');
  const kk = document.createElement('div'); kk.className = 'k'; kk.textContent = k;
  const vv = document.createElement('div'); vv.className = 'v'; vv.innerHTML = v;
  d.append(kk, vv); return d;
}
// problems renders a list of what is wrong with the deployment, so the message
// that says "not configured" is followed by the actual missing variables.
function problems(list) {
  const box = document.createElement('div'); box.className = 'banner';
  const h = document.createElement('h2'); h.textContent = 'This deployment is not configured yet'; box.append(h);
  const ul = document.createElement('ul');
  (list || []).forEach(p => { const li = document.createElement('li'); li.textContent = p; ul.append(li); });
  box.append(ul);
  const hint = document.createElement('p'); hint.className = 'lead'; hint.style.margin = '.75rem 0 0';
  hint.textContent = 'Set these and redeploy. ';
  const a = document.createElement('a'); a.href = 'api/v1/config'; a.textContent = 'GET /api/v1/config';
  hint.append(a, document.createTextNode(' shows the full state.'));
  box.append(hint);
  return box;
}
// show switches to a kind's tab, and remembers the choice so a reload — or the
// next poll — comes back to the kind being read rather than to the first one.
// Until something is picked the page stays on whichever kind came first.
function show(kind) {
  if (!sections.has(kind)) return;
  activeKind = kind;
  for (const [k, s] of sections) {
    const on = k === kind;
    s.sec.classList.toggle('on', on);
    s.tab.setAttribute('aria-selected', on ? 'true' : 'false');
  }
  try { localStorage.setItem('labs.kind', kind); } catch (e) {}
}
// preferred is the kind the last visit was left on, so a reload returns to it.
// It is a read of browser storage, so it may be unavailable — a private window,
// or cleared data — and the page falls back to the first kind.
function preferred() {
  try { return localStorage.getItem('labs.kind') || ''; } catch (e) { return ''; }
}
// state prepares one kind's tab and panel, growing them the first time the kind
// is seen.
//
// The elements are kept and re-filled rather than rebuilt, so the poll does not
// replace the button under the pointer — which is what would make the page
// unusable at the moment someone is reaching for it.
function state(kind) {
  let s = sections.get(kind);
  if (s) return s;
  const sec = document.createElement('section'); sec.className = 'kind'; sec.id = 'panel-' + kind;
  // Status on the left, the one action on the right. The panel is what the tab
  // switched to, so the tab already says which kind this is; what the panel adds
  // is the state in words, which the tab cannot fit.
  const head = document.createElement('div'); head.className = 'kind-head';
  const st = document.createElement('span'); st.className = 'tail';
  const btn = document.createElement('button');
  btn.onclick = () => begin(kind);
  head.append(st, btn);
  // The strip's own dot, so a tab says whether its kind is up without being
  // opened — that is the thing someone wants from the tab they are not on.
  const tab = document.createElement('button');
  tab.className = 'tab'; tab.setAttribute('role', 'tab'); tab.setAttribute('aria-selected', 'false');
  tab.setAttribute('aria-controls', sec.id); tab.id = 'tab-' + kind;
  const tick = document.createElement('span'); tick.className = 'tick';
  const tabName = document.createElement('span'); tabName.textContent = kind;
  tab.append(tick, tabName);
  tab.onclick = () => show(kind);
  tabsBox.append(tab); kindsBox.append(sec);

  const errBox = document.createElement('div');
  const envBox = document.createElement('div'); envBox.className = 'card';
  const runningBox = document.createElement('div'); runningBox.className = 'card';
  const runningHead = document.createElement('h3'); runningHead.textContent = 'Running';
  runningHead.hidden = true;

  sec.append(head, errBox, envBox, runningHead, runningBox);

  s = { sec, tab, tick, tail: st, btn, errBox, envBox, runningBox, runningHead, up: null, error: null };
  sections.set(kind, s);
  return s;
}
// renderKinds draws one tab and one panel per kind the deployment serves, and
// only that kind's environments and labs under it — so the two are told apart
// at a glance instead of interleaved in one list.
//
// Templates are kept for the dialog rather than shown here: a kind that offers
// a choice is asked which one at the moment it is created, and nowhere else.
function renderKinds(list, labs, templates) {
  (list || []).forEach(e => {
    if (!e.kind) return;
    const s = state(e.kind);
    // How the action reads, and whether it is a "start" or a "get", depends on
    // whether anything of this kind is up. Same rule as before: a kind with
    // nothing running still gets a button, because pressing it is what starts
    // one — hiding it would leave the page saying "starting" with nothing that
    // could start.
    s.up = s.up === null ? !!e.ready : (s.up || !!e.ready);
    s.tail.textContent = e.ready ? 'ready' : (e.unauthorized ? 'key needed' : 'starting');
    s.tail.className = 'tail ' + (e.ready ? 'ok' : (e.unauthorized ? 'err' : 'warn'));
    s.tick.className = 'tick ' + (e.ready ? 'ok' : (e.unauthorized ? 'err' : 'warn'));
    s.btn.textContent = s.up ? 'Get a lab' : 'Start ' + e.kind;
    s.btn.dataset.ready = s.up ? 'up' : 'down';
    // Held for the dialog, which reads it when the button is pressed.
    s.templates = templates[e.kind] || [];
  });

  // Environments and labs, under their own kind. The environment rows are
  // rebuilt from scratch each poll — they are plain text, and there is nothing
  // in them worth preserving — so they are cleared once here rather than
  // accumulating a row per poll behind the panel.
  for (const [, s] of sections) s.envBox.replaceChildren();
  (list || []).forEach(e => { if (sections.has(e.kind)) renderEnvs(sections.get(e.kind), e); });
  (labs || []).forEach(l => { if (sections.has(l.kind)) renderRunning(sections.get(l.kind), l); });

  // A kind that has gone away — a deployment reconfigured under us — takes its
  // tab and panel with it rather than leaving a stale card on the page.
  const live = new Set((list || []).map(e => e.kind));
  for (const [kind, s] of sections) {
    if (!live.has(kind)) {
      const gone = kind === activeKind;
      s.sec.remove(); s.tab.remove(); sections.delete(kind);
      if (gone) activeKind = '';
    }
  }
  // Something has to be on screen: the kind being read if it is still served,
  // otherwise the first one. This is done here rather than only in show() so a
  // panel added after the last click is not left behind an empty strip.
  if (!activeKind || !sections.has(activeKind)) {
    const first = sections.keys().next();
    if (!first.done) show(first.value);
  }
  // A card is drawn only once there is a kind to put in it; otherwise an
  // unconfigured deployment shows an empty outline.
  kindsBox.classList.toggle('has', sections.size > 0);
}
// renderEnvs fills one kind's cluster status: one row per environment it runs,
// with how much of it is in use and, when it is down, why.
//
// The row does not repeat the state: the tab already carries the dot and the
// panel heading already says the state in words, so a third copy here would be
// the same fact three times. What is left is what those cannot say — the
// environment's own name, its usage, and the reason it is not usable.
function renderEnvs(s, e) {
  const box = document.createElement('div'); box.className = 'env';
  const top = document.createElement('div'); top.className = 'top';
  top.append(Object.assign(document.createElement('span'), { className: 'name', textContent: e.id }));
  box.append(top);
  // One line for every kind, so the two are read the same way. applab lends out
  // named application slots, and a lab holds one for as long as it lasts;
  // sandboxlab has no slots — a sandbox is created on demand — so its number is
  // what is running rather than what is left.
  if (e.capacity) {
    const what = e.kind === 'sandboxlab'
      ? ('sandboxes: ' + e.occupied + ' of ' + e.capacity + ' in use')
      : ('application slots: ' + e.occupied + ' of ' + e.capacity + ' in use');
    box.append(Object.assign(document.createElement('div'), { className: 'msg', textContent: what }));
  }
  // Only when it is down: "starting" is already in the heading, so repeating it
  // per environment would be noise, but the reason it is down is worth saying.
  if (!e.ready && e.message) box.append(Object.assign(document.createElement('div'), { className: 'msg', textContent: e.message }));
  s.envBox.append(box);
}
// setFooter renders the build identity and which kinds are in play.
//
// The time is the build's, not the moment of the last poll. An "updated" line
// was a freshness signal — this page polls every few seconds, so a stale one
// means it has stopped — but it read as a second, unexplained clock next to the
// commit and said nothing about the thing the footer is actually about: which
// build is running. The build time answers that, and it is what someone
// checking whether a deploy landed is looking for.
//
// The kind count is here because serving fewer kinds than expected is a silent
// mistake: nothing errors, there is just less than there should be. Saying how
// many kinds are configured turns "where is sandboxlab" into an answer.
function setFooter(b) {
  const parts = [];
  if (b.commit) parts.push('commit ' + b.commit);
  // Shown only when the build carried one. A build that was never told when it
  // happened reports "unknown", and a date formatted from that is not a date.
  if (b.build_time && b.build_time !== 'unknown') parts.push('built ' + new Date(b.build_time).toLocaleString());
  if (b.kinds) parts.push('kinds ' + b.kinds.length);
  build.textContent = parts.join(' · ');
}
// countdown is how long a lab has left, as "1h 57m" or "3m 12s". It is the
// product's promise, so it is shown counting down rather than as an end time
// the reader has to subtract from.
function countdown(expiresAt) {
  let s = Math.floor((new Date(expiresAt).getTime() - Date.now()) / 1000);
  if (s <= 0) return 'expired';
  const h = Math.floor(s / 3600), m = Math.floor((s % 3600) / 60);
  s = s % 60;
  if (h) return h + 'h ' + String(m).padStart(2, '0') + 'm';
  if (m) return m + 'm ' + String(s).padStart(2, '0') + 's';
  return s + 's';
}
// sinceFmt is how long ago a lab was created, as "12m ago".
function sinceFmt(createdAt) {
  let s = Math.floor((Date.now() - new Date(createdAt).getTime()) / 1000);
  if (s < 60) return 'just now';
  const m = Math.floor(s / 60), h = Math.floor(m / 60);
  if (h) return h + 'h ' + (m % 60) + 'm ago';
  return m + 'm ago';
}
let sessionTTL = 0; // seconds; from /config, for kinds that report no expiry
// renderRunning lists one kind's labs, with when each was created and how long
// it has left. It is read from the environments, so it is right even for a lab
// this service did not itself hand out — and it never carries a key: a key is
// shown once, at creation, and this list exists so it does not have to be shown
// again.
function renderRunning(s, l) {
  s.runningBox.replaceChildren();
  const items = l.labs || [];
  s.runningHead.hidden = items.length === 0;
  items.forEach(it => {
    const box = document.createElement('div'); box.className = 'env';
    const top = document.createElement('div'); top.className = 'top';
    const st = document.createElement('span'); st.className = 'tail'; st.textContent = it.state || 'running';
    const left = document.createElement('span'); left.className = 'meta'; left.dataset.expires = expiresOf(it);
    const at = document.createElement('span'); at.className = 'meta'; at.dataset.created = it.created_at || '';
    top.append(Object.assign(document.createElement('span'), { className: 'name', textContent: it.id }), st, left, at);
    if (it.template) top.append(Object.assign(document.createElement('span'), { className: 'meta', textContent: 'template ' + it.template }));
    box.append(top);
    s.runningBox.append(box);
  });
  tick();
}
// expiresOf is when a running instance ends. A sandboxlab sandbox reports its
// own expiry; an applab app has none, so the lab's clock is the session's —
// created plus the deployment's TTL.
function expiresOf(it) {
  if (it.expires_at) return it.expires_at;
  if (it.created_at && sessionTTL) return new Date(new Date(it.created_at).getTime() + sessionTTL * 1000).toISOString();
  return '';
}
// tick refreshes every countdown on the page from the current time. It runs on
// its own one-second timer and reads the data-expires values rather than
// re-rendering, so the lists do not flicker under the pointer.
function tick() {
  document.querySelectorAll('#kinds [data-expires]').forEach(e => {
    e.textContent = e.dataset.expires ? 'expires in ' + countdown(e.dataset.expires) : '';
  });
  document.querySelectorAll('#kinds [data-created]').forEach(e => {
    e.textContent = e.dataset.created ? 'created ' + sinceFmt(e.dataset.created) : '';
  });
  // The dialog's own countdown, so the hand-off keeps ticking while it is open.
  document.querySelectorAll('#km-body [data-expires]').forEach(e => {
    e.textContent = e.dataset.expires
      ? countdown(e.dataset.expires) + ' left · until ' + new Date(e.dataset.expires).toLocaleTimeString()
      : '';
  });
}
setInterval(tick, 1000);
let configured = true;
// fetchJSON fetches with a deadline, so a slow or hung endpoint cannot leave
// the page blank — it fails, and the caller shows why.
async function fetchJSON(url, opts) {
  const ctrl = new AbortController();
  const timer = setTimeout(() => ctrl.abort(), 15000);
  try {
    const res = await fetch(url, Object.assign({ signal: ctrl.signal }, opts || {}));
    const body = await res.json();
    return { ok: res.ok, status: res.status, body };
  } finally {
    clearTimeout(timer);
  }
}
async function loadStatus() {
  try {
    // /config carries both the state and the cluster status; it is what the
    // page shows, refreshed on a timer so an environment coming up turns into a
    // button without a reload. Nothing is started here — a lab is created, and
    // that is what brings an environment up.
    const { body } = await fetchJSON('api/v1/config');
    const cfg = body.data || {};
    setFooter(cfg);
    sessionTTL = cfg.session_ttl_seconds || 0;
    configured = cfg.configured !== false;
    // Come back to the tab that was left open, before the kinds are drawn, so
    // the first paint is already on the right one. renderKinds falls back to the
    // first kind when this names one that is no longer served.
    if (!activeKind) activeKind = preferred();
    renderKinds(configured ? cfg.environments : [], cfg.labs, cfg.templates || {});
    if (!configured && (cfg.problems || []).length) {
      status.replaceChildren(problems(cfg.problems));
    } else {
      status.replaceChildren();
    }
  } catch (e) {
    // Say so rather than showing nothing, which is what a silent failure looks
    // like: the cluster status simply never appears.
    status.replaceChildren(row('status', 'could not reach the service: ' + e.message, 'err'));
    renderKinds([], [], {});
  }
}
// begin is the button: it asks, when there is something to ask, and creates when
// there is not.
//
// A kind that offers a choice of templates is asked for one here, in a dialog,
// before anything is created — so a request the server would have refused is
// never sent, and the panel behind it holds only labs that exist. A kind with no
// choice goes straight to the create, with no dialog to click through.
function begin(kind) {
  const s = sections.get(kind);
  if (!s) return;
  s.error = null;
  if (!s.templates || s.templates.length === 0) { create(kind, '', s); return; }

  kmEyebrow.textContent = 'New lab';
  kmTitle.textContent = 'Create a ' + kind + ' lab';
  kmBody.replaceChildren();
  kmField.hidden = false;
  kmError.replaceChildren();
  kmHint.textContent = 'A sandbox is built from a template. Pick the one to start from.';
  // The options are built each time the dialog opens, so they are never stale
  // and there is no separate copy of them to keep in step with the poll.
  const keep = kmTemplate.value;
  kmTemplate.replaceChildren();
  s.templates.forEach(o => {
    const opt = document.createElement('option');
    opt.value = o.id;
    opt.textContent = o.title && o.title !== o.id ? o.title + ' — ' + o.id : o.id;
    if (o.description) opt.title = o.description;
    kmTemplate.append(opt);
  });
  if (s.templates.some(o => o.id === keep)) kmTemplate.value = keep;

  kmConfirm.textContent = 'Create';
  kmConfirm.disabled = false;
  kmClose.textContent = 'Cancel';
  openModal();
  kmConfirm.onclick = () => create(kind, kmTemplate.value, s);
}
// openModal shows the dialog, and closeModal hides it. Both go through the
// dialog element so the backdrop and Escape keep working, with the attribute as
// the fallback for a browser that has no showModal.
function openModal() {
  document.body.classList.add('modal-open');
  if (typeof km.showModal === 'function') { if (!km.open) km.showModal(); }
  else km.setAttribute('open', '');
  kmClose.onclick = () => closeModal();
  km.onclose = () => document.body.classList.remove('modal-open');
}
function closeModal() {
  if (typeof km.close === 'function') km.close(); else km.removeAttribute('open');
  document.body.classList.remove('modal-open');
}
// create asks for one lab of the named kind, from the named template when the
// kind offers a choice — the kind and template are what was picked, so a
// deployment serving both hands out exactly what was asked for rather than
// always the same one.
//
// A failure goes into that kind's own panel, so it appears under the thing it is
// about rather than in one place that has to be read against the buttons.
async function create(kind, template, s) {
  // Whether the dialog is mid-ask. The progress and the reset of the button
  // belong to the ask, not to the create: a create that succeeds replaces the
  // dialog with the hand-off, which owns the button from then on.
  const asking = !kmField.hidden;
  s.btn.disabled = true;
  if (asking) { kmConfirm.disabled = true; kmConfirm.textContent = 'Creating…'; kmError.replaceChildren(); }
  const req = { kind: kind };
  if (template) req.template = template;
  try {
    const { ok, status: code, body } = await fetchJSON('api/v1/labs', {
      method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify(req) });
    if (!ok) {
      showError(s, body, code);
      // While asking, the answer belongs in the dialog, in front of the person
      // who would act on it; the panel gets it too for when the dialog is gone.
      showModalError(body, code);
      loadStatus(); // show the environment the request just started
      return;
    }
    showLab(body.data, kind);
    loadStatus(); // the new lab appears in the running list on the next poll
  } catch (e) {
    s.error = 'could not reach the service: ' + e.message;
    renderError(s);
    showModalError({ error: s.error }, 0);
  } finally {
    s.btn.disabled = false;
    if (asking && kmConfirm.textContent === 'Creating…') { kmConfirm.disabled = false; kmConfirm.textContent = 'Create'; }
  }
}
// showModalError puts a failed create in the dialog, when the dialog is the
// thing that is open. A create that went straight through — no ask, no dialog —
// has nowhere to put it, and the panel is already saying it.
function showModalError(body, code) {
  if (kmField.hidden) return;
  kmError.replaceChildren();
  kmError.append(row('error', body.error || ('HTTP ' + code), 'err'));
}
// showError puts a failed request's message in its kind's section, with the
// retryable marker when the answer said so.
function showError(s, body, code) {
  s.error = body.error || ('HTTP ' + code);
  s.retryable = !!body.retryable;
  s.problems = (body.problems || []).length ? body.problems : null;
  renderError(s);
}
// renderError draws the section's current error, or clears it when there is
// none. It is called from both the request path and the poll, so an error is
// not wiped by a poll that happens to land while it is on screen.
function renderError(s) {
  s.errBox.replaceChildren();
  if (!s.error) return;
  const d = row('error', s.error, 'err');
  if (s.retryable) d.append(Object.assign(document.createElement('span'), { textContent: ' (retryable)' }));
  s.errBox.append(d);
  if (s.problems) s.errBox.append(problems(s.problems));
}
// showLab is the hand-off: the one moment a key is shown, in a dialog that says
// so. The key is not kept anywhere on the page afterwards — the running list
// below never carries it — so the dialog is explicit that closing it is the last
// chance to take it, and offers a copy button rather than a string to select by
// hand.
function showLab(d, kind) {
  kmEyebrow.textContent = 'Ready';
  kmTitle.textContent = 'Your ' + kind + ' lab is ready';
  kmBody.replaceChildren();
  // The ask, if it was open, is done: nothing more to fill in, and no error to
  // carry over from a previous attempt.
  kmField.hidden = true;
  kmHint.textContent = '';
  kmError.replaceChildren();

  kmBody.append(row('console', '<a href="' + d.console_url + '" target="_blank" rel="noopener">' + d.console_url + '</a>'));
  kmBody.append(keyRow(d.api_key));
  kmBody.append(row('expires', '<span data-expires="' + d.expires_at + '"></span>'));
  if (d.app) kmBody.append(row('app', '<code>' + d.app + '</code>'));
  if (d.template) kmBody.append(row('template', '<code>' + d.template + '</code>'));
  kmBody.append(row('how', kind === 'sandboxlab'
    ? 'Open the link — the key is already in it.'
    : 'Open the console and paste the key.'));

  if (d.warning) {
    const w = document.createElement('div'); w.className = 'warnbox'; w.textContent = d.warning;
    kmBody.append(w);
  }
  const once = document.createElement('div');
  once.className = 'once';
  once.textContent = 'This key is shown once, now. It is not shown again — copy it before closing.';
  kmBody.append(once);

  kmClose.textContent = 'Close';
  kmConfirm.disabled = false;
  kmConfirm.textContent = 'Open the lab';
  kmConfirm.onclick = () => window.open(d.console_url, '_blank', 'noopener');
  openModal();
  tick();
}
// keyRow renders the one-time key with a copy button. It is the only place a
// key ever appears, and it is not stored anywhere on the page afterwards.
function keyRow(key) {
  const r = row('api key', '', 'key');
  const v = r.querySelector('.v');
  v.className = 'v keyline';
  const code = document.createElement('code'); code.textContent = key || '(none)';
  const copy = document.createElement('button'); copy.className = 'small'; copy.textContent = 'Copy';
  copy.onclick = async () => {
    try { await navigator.clipboard.writeText(key); copy.textContent = 'Copied'; }
    catch (e) { copy.textContent = 'Select it'; }
    setTimeout(() => { copy.textContent = 'Copy'; }, 1500);
  };
  v.append(code, copy);
  return r;
}
loadStatus();
// Poll, so an environment that is booting becomes a lab without a reload. The
// interval is short enough to feel live and long enough not to hammer.
setInterval(() => { if (!document.hidden) loadStatus(); }, 5000);
</script>
</body>
</html>
`
