package api

import (
	"net/http"
)

// console is the landing page: one panel, with a switch for the kind of lab to
// ask for and the count and list of what that kind is running under it. It is a
// static document carrying nothing — everything it shows is fetched from the
// API, which is where the limits are — so it is safe to serve to anyone.
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
  /* A request's error sits in the panel above the controls that made it: it is
     about this kind, and it belongs under the thing it is about. */

  .row {
    display: grid; grid-template-columns: 6.5rem 1fr; gap: 14px;
    align-items: baseline; padding: 11px 0; border-top: 1px solid var(--line-soft);
  }
  .row:first-child { border-top: 0; padding-top: 0; }
  .row:last-child { padding-bottom: 0; }
  .row .k { color: var(--muted); font-size: .86rem; }
  .row.err { color: var(--err); }

  /* ── the panel ──────────────────────────────────────────────────────── */
  /* One panel, not one per kind. The kind is a switch inside it, so changing
     what you are asking for does not change where you are looking — the page
     stays put and one word in it changes. The card is drawn only once a kind
     exists, because a card with nothing in it is a bare outline. */
  #panel { display: none; }
  #panel.has {
    display: block;
    margin-top: 22px;
    padding: 16px 20px 18px;
    background: var(--surface);
    border: 1px solid var(--line);
    border-radius: var(--radius);
    box-shadow: var(--shadow);
  }
  /* The state on the left; the controls that make a lab on the right, laid out
     in the order they are used — which kind, which template, then the button.
     Wrapping rather than scrolling keeps all of it on the page at any width. */
  .head {
    display: flex; align-items: center; justify-content: space-between;
    gap: 14px; flex-wrap: wrap;
  }
  .controls { display: flex; align-items: center; gap: 10px; flex-wrap: wrap; }
  /* The two pickers and the button are one row of things you use in sequence,
     so they are sized to each other rather than to their own content. */
  .controls select, .controls button {
    font-size: .9rem; line-height: 1.35; padding: 9px 14px;
    border-radius: var(--radius-sm); border: 1px solid var(--line);
  }
  .controls select {
    font-family: inherit; color: var(--fg); background: var(--surface);
    cursor: pointer; max-width: 16rem;
  }
  .controls select:hover { border-color: var(--muted); }
  .controls select:focus-visible { outline: 2px solid var(--accent); outline-offset: 1px; }
  .controls select:disabled { color: var(--muted); cursor: default; }
  .controls button { padding: 9px 18px; border-color: transparent; white-space: nowrap; }
  /* A state, as a word with a dot before it rather than a pill: the pill drew a
     box around a single word, which on a page this quiet read as a button. */
  .state {
    display: inline-flex; align-items: center; gap: 7px;
    font-size: .87rem; color: var(--muted);
  }
  .state::before {
    content: ''; width: 8px; height: 8px; border-radius: 50%;
    background: currentColor; flex: none;
  }
  .state.ok { color: var(--ok); }
  .state.warn { color: var(--warn); }
  .state.err { color: var(--err); }

  /* What the environment is carrying, then the labs in it — one list, with no
     heading over either half. The count and the rows below it are the same fact
     read from the same source; heading them separately only made the reader
     work out how the two related. */
  .usage { margin-top: 14px; font-size: .87rem; color: var(--muted); }
  .labs { margin-top: 16px; }
  .lab { padding: 11px 0; border-top: 1px solid var(--line-soft); }
  .lab:first-child { padding-top: 0; border-top: 0; }
  .lab .line { display: flex; align-items: baseline; gap: 10px; flex-wrap: wrap; }
  .lab .id { font: 600 .88rem/1.5 var(--mono); overflow-wrap: anywhere; }
  /* The instance's own state, undecorated: it is the environment's word, not a
     second opinion on whether the lab is any good, and the accent here would
     have been the third thing on the line competing for the eye. */
  .lab .st { font-size: .82rem; color: var(--muted); }
  .lab .meta { color: var(--muted); font-size: .85rem; }
  /* Why an environment is not up, in the environment's own words. It is the one
     thing the state word cannot say, so it is the only prose in the panel. */
  .reason {
    margin-top: 13px; font-size: .86rem; color: var(--muted);
    overflow-wrap: anywhere;
  }

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
  /* The hand-off is a dialog and nothing else: the ask is on the page now, so
     the dialog opens only when there is something to hand over, and closes
     when it has been. */
  .key-modal .foot {
    display: flex; gap: 10px; justify-content: flex-end;
    padding: 16px 24px 20px;
  }
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
  <div id="panel">
    <div class="head">
      <span class="state" id="state"></span>
      <div class="controls">
        <select id="kind" aria-label="kind of lab"></select>
        <select id="template" aria-label="template" hidden></select>
        <button id="create">Create a lab</button>
      </div>
    </div>
    <div id="error"></div>
    <div class="usage" id="usage"></div>
    <div class="reason" id="reason"></div>
    <div class="labs" id="labs"></div>
  </div>
  <footer id="build"></footer>
</main>
<dialog id="key-modal" class="key-modal">
  <div class="head">
    <div class="eyebrow" id="km-eyebrow">Ready</div>
    <h3 id="km-title">Your lab is ready</h3>
  </div>
  <div class="body" id="km-body"></div>
  <div id="km-error"></div>
  <div class="foot">
    <button id="km-close" class="small">Close</button>
    <button id="km-confirm">Open the lab</button>
  </div>
</dialog>
<script>
const status = document.getElementById('status');
const panel = document.getElementById('panel');
const build = document.getElementById('build');
const stateEl = document.getElementById('state');
const kindSel = document.getElementById('kind');
const tmplSel = document.getElementById('template');
const createBtn = document.getElementById('create');
const errBox = document.getElementById('error');
const usageBox = document.getElementById('usage');
const reasonBox = document.getElementById('reason');
const labsBox = document.getElementById('labs');
const km = document.getElementById('key-modal');
const kmTitle = document.getElementById('km-title');
const kmBody = document.getElementById('km-body');
const kmConfirm = document.getElementById('km-confirm');
const kmClose = document.getElementById('km-close');
const kmEyebrow = document.getElementById('km-eyebrow');
const kmError = document.getElementById('km-error');
// The page keeps one live copy of what it last read, and redraws from it.
// Provision changes what should be on screen before the next poll answers — a
// create that was refused can start an environment — so the redraws are driven
// by this rather than by the poll alone.
let cfg = {};
// kind is the switch's value. It is read from the element when a request is
// made rather than mirrored into a variable, so there is one place the answer
// lives and no copy to fall out of step with it.
function currentKind() { return kindSel.value; }
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
// render draws the whole page from one reading of /config.
//
// There is one panel and one kind on screen at a time, so this is a redraw of
// that panel rather than a reconciliation of several: the kind switch decides
// what the rest of it is about, and everything below the head belongs to the
// kind it names. Nothing here rebuilds the controls themselves — the poll runs
// every five seconds and a select replaced under a click is a select that
// cannot be used.
function render() {
  const list = cfg.configured === false ? [] : (cfg.environments || []);
  const labs = cfg.labs || [];
  const templates = cfg.templates || {};

  // A kind that has gone away — a deployment reconfigured under us — must not
  // be the one being read, or the panel below describes an environment that no
  // longer exists. The switch is refilled from what is served, and a request
  // for one that is not is not possible once it is off the list.
  const wanted = kindSel.value;
  let keep = '';
  kindSel.replaceChildren();
  list.forEach(e => {
    if (!e.kind) return;
    const o = document.createElement('option');
    o.value = e.kind; o.textContent = e.kind;
    kindSel.append(o);
    if (e.kind === wanted) keep = e.kind;
  });
  // The remembered kind, when the switch is being filled for the first time —
  // a reload should come back to what was being looked at. Browser storage can
  // be unavailable (a private window, cleared data), so this is a read that may
  // simply find nothing.
  let remembered = '';
  try { remembered = localStorage.getItem('labs.kind') || ''; } catch (e) {}
  const pick = (wanted && keep) ? keep
    : (list.some(e => e.kind === remembered) ? remembered : (list[0] && list[0].kind) || '');
  if (pick) kindSel.value = pick;

  // The template picker is shown for exactly the kinds that offer a choice, and
  // is filled from that kind's list each time — so it can never be stale, and
  // there is no second copy of the templates to keep in step with the poll.
  const kind = pick;
  const choices = templates[kind] || [];
  const keepT = tmplSel.value;
  tmplSel.replaceChildren();
  choices.forEach(o => {
    const opt = document.createElement('option');
    opt.value = o.id;
    opt.textContent = o.title && o.title !== o.id ? o.title + ' — ' + o.id : o.id;
    if (o.description) opt.title = o.description;
    tmplSel.append(opt);
  });
  if (choices.some(o => o.id === keepT)) tmplSel.value = keepT;
  tmplSel.hidden = choices.length === 0;

  // The state and how much is in use, for the kind being read. An environment
  // that is down is never asked how much it holds — there is nothing at the
  // other end to answer — so its count is zero by construction, and "0 of 8 in
  // use" under "starting" is a number nobody produced, read by someone deciding
  // whether to press the button. The reason line is the honest answer there.
  const env = list.find(e => e.kind === kind);
  const up = !!(env && env.ready);
  stateEl.textContent = !env ? '' : (env.ready ? 'ready' : (env.unauthorized ? 'key needed' : 'starting'));
  stateEl.className = 'state ' + (up ? 'ok' : (env && env.unauthorized ? 'err' : 'warn'));
  stateEl.hidden = !env;
  const capacity = env && env.capacity;
  usageBox.textContent = (up && capacity)
    ? (kind === 'sandboxlab'
      ? ('sandboxes: ' + env.occupied + ' of ' + capacity + ' in use')
      : ('application slots: ' + env.occupied + ' of ' + capacity + ' in use'))
    : '';
  reasonBox.textContent = (env && !env.ready && env.message) ? env.message : '';
  // The button's word follows the environment, not the request: a kind that is
  // up hands out a lab, and one that is not is what the press starts. It is
  // never hidden — a kind with nothing running still needs a way to ask, since
  // asking is what starts it.
  createBtn.textContent = up ? 'Create a lab' : 'Start a lab';
  createBtn.disabled = !kind;

  // What the kind is running. One row per lab, in the environment's own words
  // for its state — applab's "created" is an app record with nothing deployed
  // behind it, and any friendlier word would be this page's guess at what that
  // implies. The list is below the count because it is the same data the count
  // is: a number and the things it counts.
  labsBox.replaceChildren();
  const mine = labs.filter(l => l.kind === kind);
  mine.forEach(l => (l.labs || []).forEach(it => {
    const box = document.createElement('div'); box.className = 'lab';
    const line = document.createElement('div'); line.className = 'line';
    line.append(Object.assign(document.createElement('span'), { className: 'id', textContent: it.id }));
    if (it.state) {
      line.append(Object.assign(document.createElement('span'), {
        className: 'st', textContent: String(it.state).toLowerCase() }));
    }
    const left = document.createElement('span'); left.className = 'meta'; left.dataset.expires = expiresOf(it);
    const at = document.createElement('span'); at.className = 'meta'; at.dataset.created = it.created_at || '';
    line.append(left, at);
    if (it.template) line.append(Object.assign(document.createElement('span'), { className: 'meta', textContent: 'template ' + it.template }));
    box.append(line);
    labsBox.append(box);
  }));
  tick();

  // A panel is drawn only once there is a kind to put in it; otherwise an
  // unconfigured deployment shows an empty outline.
  panel.classList.toggle('has', !!kind);
  renderError();
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
  document.querySelectorAll('#panel [data-expires]').forEach(e => {
    e.textContent = e.dataset.expires ? 'expires in ' + countdown(e.dataset.expires) : '';
  });
  document.querySelectorAll('#panel [data-created]').forEach(e => {
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
    cfg = body.data || {};
    setFooter(cfg);
    sessionTTL = cfg.session_ttl_seconds || 0;
    render();
    if (cfg.configured === false && (cfg.problems || []).length) {
      status.replaceChildren(problems(cfg.problems));
    } else {
      status.replaceChildren();
    }
  } catch (e) {
    // Say so rather than showing nothing, which is what a silent failure looks
    // like: the cluster status simply never appears. What was last read stays
    // on the page, since it is still the most recent thing known.
    status.replaceChildren(row('status', 'could not reach the service: ' + e.message, 'err'));
    render();
  }
}
// create asks for one lab. The kind and template are read from the controls at
// the moment of the press, so what is sent is what is on screen.
//
// A failure goes into the panel above these controls, under the thing it is
// about, and the poll is asked for again immediately: a refused request is one
// that may have started an environment, and the page should show that rather
// than sit on a state it now knows is old.
async function create() {
  const kind = currentKind();
  if (!kind) return;
  const template = tmplSel.hidden ? '' : tmplSel.value;
  error = '';
  createBtn.disabled = true;
  createBtn.textContent = 'Creating…';
  renderError();
  const req = { kind: kind };
  if (template) req.template = template;
  try {
    const { ok, status: code, body } = await fetchJSON('api/v1/labs', {
      method: 'POST', headers: {'Content-Type':'application/json'}, body: JSON.stringify(req) });
    if (!ok) {
      showError(body, code);
      loadStatus(); // show the environment the request just started
      return;
    }
    showLab(body.data, kind);
    loadStatus(); // the new lab appears in the running list on the next poll
  } catch (e) {
    error = 'could not reach the service: ' + e.message;
    renderError();
  } finally {
    // Back to whatever the environment now warrants. render() is what decides
    // the button's word, so the progress text written above is always undone.
    render();
  }
}
// error is a failed request's message, held rather than drawn on the spot so
// the poll does not wipe it while it is on screen. Only a new request or a
// successful one clears it.
let error = '';
let retryable = false;
let problems_ = null;
// showError records a failed request's message, with the retryable marker when
// the answer said so.
function showError(body, code) {
  error = body.error || ('HTTP ' + code);
  retryable = !!body.retryable;
  problems_ = (body.problems || []).length ? body.problems : null;
  renderError();
}
// renderError draws the panel's current error, or clears it when there is none.
function renderError() {
  errBox.replaceChildren();
  if (!error) return;
  const d = row('error', error, 'err');
  if (retryable) d.append(Object.assign(document.createElement('span'), { textContent: ' (retryable)' }));
  errBox.append(d);
  if (problems_) errBox.append(problems(problems_));
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
// showLab is the hand-off: the one moment a key is shown, in a dialog that says
// so. The key is not kept anywhere on the page afterwards — the running list
// below never carries it — so the dialog is explicit that closing it is the last
// chance to take it, and offers a copy button rather than a string to select by
// hand.
function showLab(d, kind) {
  kmEyebrow.textContent = 'Ready';
  kmTitle.textContent = 'Your ' + kind + ' lab is ready';
  kmBody.replaceChildren();
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
// The controls. The switch is the one thing that changes what the page is
// about, so it is what redraws it — and it remembers the choice, so a reload
// comes back to the kind being read. Template is refilled by render() for
// whichever kind is chosen; the button reads both at the moment it is pressed.
kindSel.onchange = () => {
  try { localStorage.setItem('labs.kind', kindSel.value); } catch (e) {}
  // A refusal was about the kind that was on screen; it is not an answer to
  // the one just chosen, so it does not survive the switch.
  error = ''; renderError();
  render();
};
createBtn.onclick = create;
loadStatus();
// Poll, so an environment that is booting becomes a lab without a reload. The
// interval is short enough to feel live and long enough not to hammer.
setInterval(() => { if (!document.hidden) loadStatus(); }, 5000);
</script>
</body>
</html>
`
